package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/pion/webrtc/v3/pkg/media/h264reader"
)

// --- H.264 RTSP パススルー ---
func startFFmpegH264RTSP(inputURL string) {
	cmd := exec.Command("ffmpeg",
		"-loglevel", "error", // FFmpegのログ出力をエラーのみに抑制
		"-rtsp_transport", "udp", "-max_delay", "0",
		"-analyzeduration", "0", "-avioflags", "direct",
		"-flags", "low_delay", "-fflags", "+igndts+nobuffer",
		"-i", inputURL,
		"-c:v", "copy", "-an", "-fps_mode", "passthrough",
		"-flush_packets", "1",
		"-f", "h264", "pipe:1",
	)
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	go logFFmpegStderr(stderr)
	_ = cmd.Start()
	log.Println("FFmpeg (H264 RTSP パススルー) 開始")

	go func() { _ = cmd.Wait() }()
	h264r, _ := h264reader.NewReader(bufio.NewReader(stdout))
	dur := time.Second / 30
	streamNAL(h264r, dur)
}

// --- H.264 RTP パススルー ---
func startFFmpegH264RTP(inputURL string) {
	cmdArgs, sdpContent, err := buildRTPCommand(inputURL, "H264",
		[]string{}, // preInputArgs
		[]string{ // postInputArgs
			"-c:v", "copy", "-an", "-fps_mode", "passthrough",
			"-flush_packets", "1",
			"-f", "h264", "pipe:1",
		})
	if err != nil {
		log.Printf("Error building H264 RTP command: %v", err)
		return
	}

	log.Printf("FFmpeg H264 RTP コマンド: ffmpeg %s", strings.Join(cmdArgs, " "))
	cmd := exec.Command("ffmpeg", cmdArgs...)

	if sdpContent != "" {
		stdin, pipeErr := cmd.StdinPipe()
		if pipeErr != nil {
			log.Printf("Failed to get stdin pipe for FFmpeg: %v", pipeErr)
			return
		}
		go func() {
			defer stdin.Close()
			if _, writeErr := io.WriteString(stdin, sdpContent); writeErr != nil {
				log.Printf("Error writing SDP to FFmpeg stdin: %v", writeErr)
			}
		}()
	}

	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	go logFFmpegStderr(stderr)
	if err := cmd.Start(); err != nil {
		log.Printf("Failed to start FFmpeg (H264 RTP パススルー): %v", err)
		return
	}
	log.Println("FFmpeg (H264 RTP パススルー) 開始")

	go func() {
		if err := cmd.Wait(); err != nil {
			log.Printf("FFmpeg (H264 RTP パススルー) exited with error: %v", err)
		}
	}()
	h264r, _ := h264reader.NewReader(bufio.NewReader(stdout))
	dur := time.Second / 30
	streamNAL(h264r, dur)
}

// --- H.265 から H.264 へのトランスコーディング (GPU, RTSP) ---
func startFFmpegH265ToH264NALGPURTSP(inputURL string) {
	cmd := exec.Command("ffmpeg",
		"-loglevel", "error", // FFmpegのログ出力をエラーのみに抑制
		"-rtsp_transport", "tcp", "-probesize", "250000", "-analyzeduration", "0",
		"-fflags", "nobuffer+flush_packets+genpts", "-flags", "low_delay", "-max_delay", "0",
		"-hwaccel", "cuda", "-hwaccel_output_format", "cuda",
		"-i", inputURL, "-an",
		"-c:v", "h264_nvenc", "-preset", "p1", "-tune", "ll", "-delay", "0",
		"-rc:v", "cbr", "-b:v", "6M", "-g", "30", "-bf", "0",
		"-fps_mode", "passthrough",
		"-map", "0:v:0", "-f", "h264", "pipe:1",
	)
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	go logFFmpegStderr(stderr)
	_ = cmd.Start()
	log.Println("FFmpeg (H265 から H264 NAL - GPU, RTSP) 開始")

	go func() { _ = cmd.Wait() }()
	h264r, _ := h264reader.NewReader(bufio.NewReader(stdout))
	dur := time.Second / 30
	streamNAL(h264r, dur)
}

// --- H.265 から H.264 へのトランスコーディング (GPU, RTP) ---
func startFFmpegH265ToH264NALGPURTP(inputURL string) {
	cmdArgs, sdpContent, err := buildRTPCommand(inputURL, "H265",
		[]string{ // preInputArgs
			"-fflags", "genpts",
			"-hwaccel", "cuda", "-hwaccel_output_format", "cuda",
		},
		[]string{ // postInputArgs
			"-an",
			"-c:v", "h264_nvenc", "-preset", "p1", "-tune", "ll", "-delay", "0",
			"-rc:v", "cbr", "-b:v", "6M", "-g", "30", "-bf", "0",
			"-fps_mode", "passthrough",
			"-map", "0:v:0", "-f", "h264", "pipe:1",
		})
	if err != nil {
		log.Printf("Error building H265 to H264 GPU RTP command: %v", err)
		return
	}

	log.Printf("FFmpeg H265 から H264 GPU RTP コマンド: ffmpeg %s", strings.Join(cmdArgs, " "))
	cmd := exec.Command("ffmpeg", cmdArgs...)

	if sdpContent != "" {
		stdin, pipeErr := cmd.StdinPipe()
		if pipeErr != nil {
			log.Printf("Failed to get stdin pipe for FFmpeg: %v", pipeErr)
			return
		}
		go func() {
			defer stdin.Close()
			if _, writeErr := io.WriteString(stdin, sdpContent); writeErr != nil {
				log.Printf("Error writing SDP to FFmpeg stdin: %v", writeErr)
			}
		}()
	}

	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	go logFFmpegStderr(stderr)
	if err := cmd.Start(); err != nil {
		log.Printf("Failed to start FFmpeg (H265 to H264 NAL - GPU, RTP): %v", err)
		return
	}
	log.Println("FFmpeg (H265 to H264 NAL - GPU, RTP) 開始")

	go func() {
		if err := cmd.Wait(); err != nil {
			log.Printf("FFmpeg (H265 to H264 NAL - GPU, RTP) exited with error: %v", err)
		}
	}()
	h264r, _ := h264reader.NewReader(bufio.NewReader(stdout))
	dur := time.Second / 30
	streamNAL(h264r, dur)
}

// --- H.265 から H.264 へのトランスコーディング (CPU, RTSP) ---
func startFFmpegH265ToH264NALCPURTSP(inputURL string) {
	cmd := exec.Command("ffmpeg",
		"-loglevel", "error", // FFmpegのログ出力をエラーのみに抑制
		// 入力設定
		"-rtsp_transport", "udp",
		"-probesize", "250000",
		"-analyzeduration", "50000", // CPU版ではより大きな値を設定することがあります
		"-fflags", "nobuffer+genpts", // flush_packets はCPUエンコードでは問題を起こすことがあるため削除
		"-flags", "low_delay",
		"-max_delay", "500", // CPU版では少し余裕を持たせることがあります

		// 入力ストリーム
		"-i", inputURL,
		"-an", // 音声なし

		// --- エンコード設定 (CPU, libx264) ---
		"-c:v", "libx264",
		"-preset", "ultrafast",        // エンコード速度優先
		"-tune", "zerolatency",        // 低レイテンシ
		"-x264-params", "nal-hrd=cbr", // CBR に必要
		"-b:v", "6M",                  // ビットレート
		"-maxrate", "6M",              // 最大ビットレート
		"-bufsize", "6M",              // バッファサイズ

		"-g", "30", // GOP 長
		"-bf", "0", // B-frames 無効

		// フレームレートとメタデータ
		"-fps_mode", "passthrough", // 入力フレームレートを維持
		"-map", "0:v:0",
		"-f", "h264",
		"pipe:1",
	)
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	go logFFmpegStderr(stderr)
	_ = cmd.Start()
	log.Println("FFmpeg (H265 to H264 NAL - CPU, RTSP) 開始")

	go func() { _ = cmd.Wait() }()
	h264r, _ := h264reader.NewReader(bufio.NewReader(stdout))
	dur := time.Second / 30 // フレームレートに応じて調整
	streamNAL(h264r, dur)
}

// --- H.265 から H.264 へのトランスコーディング (CPU, RTP) ---
func startFFmpegH265ToH264NALCPURTP(inputURL string) {
	// H.265デコーディング用の強化されたパラメータ（RTP専用）	cmdArgs, sdpContent, err := buildRTPCommand(inputURL, "H265",
		cmdArgs, sdpContent, err := buildRTPCommand(inputURL, "H265",
			[]string{ // preInputArgs
				"-strict", "experimental",
				"-err_detect", "ignore_err",
				"-fflags", "+genpts+igndts+nobuffer",
				"-flags", "low_delay",
			},
			[]string{ // postInputArgs
				"-an",
				"-c:v", "libx264",
				"-preset", "ultrafast",
				"-tune", "zerolatency",
				"-x264-params", "nal-hrd=cbr:force-cfr=1",
				"-b:v", "2M",
				"-maxrate", "2M", 
				"-bufsize", "1M",
				"-fps_mode", "cfr",
				"-r", "30",
				"-g", "30",
				"-bf", "0",
				"-pix_fmt", "yuv420p",
				"-vf", "scale=trunc(iw/2)*2:trunc(ih/2)*2", // 解像度を偶数に調整
				"-map", "0:v:0", // 明示的にビデオストリームをマップ
				"-avoid_negative_ts", "make_zero",
				"-f", "h264",
				"pipe:1",
			})
		if err != nil {
			log.Printf("Error building H265 to H264 CPU RTP command: %v", err)
			return
		}

		log.Printf("FFmpeg H265 to H264 CPU RTP command: ffmpeg %s", strings.Join(cmdArgs, " "))
		cmd := exec.Command("ffmpeg", cmdArgs...)

	if sdpContent != "" {
		stdin, pipeErr := cmd.StdinPipe()
		if pipeErr != nil {
			log.Printf("Failed to get stdin pipe for FFmpeg: %v", pipeErr)
			return
		}
		go func() {
			defer stdin.Close()
			if _, writeErr := io.WriteString(stdin, sdpContent); writeErr != nil {
				log.Printf("Error writing SDP to FFmpeg stdin: %v", writeErr)
			}
		}()
	}

	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	go logFFmpegStderr(stderr)
	if err := cmd.Start(); err != nil {
		log.Printf("Failed to start FFmpeg (H265 to H264 NAL - CPU, RTP): %v", err)
		return
	}
	log.Println("FFmpeg (H265 to H264 NAL - CPU, RTP) 開始")

	go func() {
		if err := cmd.Wait(); err != nil {
			log.Printf("FFmpeg (H265 to H264 NAL - CPU, RTP) exited with error: %v", err)
		}
	}()
	h264r, _ := h264reader.NewReader(bufio.NewReader(stdout))
	dur := time.Second / 30 // フレームレートに応じて調整
	streamNAL(h264r, dur)
}

// --- H.265 RTP パススルー ---
func startFFmpegH265RTP(inputURL string) {
	cmdArgs, sdpContent, err := buildRTPCommand(inputURL, "H265",
		[]string{}, // preInputArgs
		[]string{ // postInputArgs
			"-c:v", "copy", "-an", "-fps_mode", "passthrough",
			"-flush_packets", "1",
			"-f", "hevc", "pipe:1", // H.265の場合はhevcフォーマットを使用
		})
	if err != nil {
		log.Printf("Error building H265 RTP command: %v", err)
		return
	}

	log.Printf("FFmpeg H265 RTP コマンド: ffmpeg %s", strings.Join(cmdArgs, " "))
	cmd := exec.Command("ffmpeg", cmdArgs...)

	if sdpContent != "" {
		stdin, pipeErr := cmd.StdinPipe()
		if pipeErr != nil {
			log.Printf("Failed to get stdin pipe for FFmpeg: %v", pipeErr)
			return
		}
		go func() {
			defer stdin.Close()
			if _, writeErr := io.WriteString(stdin, sdpContent); writeErr != nil {
				log.Printf("Error writing SDP to FFmpeg stdin: %v", writeErr)
			}
		}()
	}

	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	go logFFmpegStderr(stderr)
	if err := cmd.Start(); err != nil {
		log.Printf("Failed to start FFmpeg (H265 RTP パススルー): %v", err)
		return
	}
	log.Println("FFmpeg (H265 RTP パススルー) 開始")

	go func() {
		if err := cmd.Wait(); err != nil {
			log.Printf("FFmpeg (H265 RTP パススルー) exited with error: %v", err)
		}
	}()
	
	// H.265ストリームの処理
	streamH265NAL(bufio.NewReader(stdout), time.Second/30)
}

// streamH265NAL はH.265 NALユニットを処理してWebRTCに送信する関数
func streamH265NAL(reader *bufio.Reader, duration time.Duration) {
	buffer := make([]byte, 4096)
	nalBuffer := make([]byte, 0, 1024*1024) // 1MBのバッファ
	
	for {
		n, err := reader.Read(buffer)
		if err != nil {
			if err != io.EOF {
				log.Printf("H.265 NAL読み込みエラー: %v", err)
			}
			break
		}
		
		if n > 0 {
			nalBuffer = append(nalBuffer, buffer[:n]...)
			
			// H.265 NALユニットの検出と送信
			for {
				nalStart := findNextNALUnit(nalBuffer)
				if nalStart == -1 {
					break
				}
				
				nextNALStart := findNextNALUnit(nalBuffer[nalStart+4:])
				if nextNALStart == -1 {
					// 次のNALユニットが見つからない場合、バッファの最後まで読む
					break
				}
				
				nextNALStart += nalStart + 4
				nalUnit := nalBuffer[nalStart:nextNALStart]
				
				// NALユニットをWebRTCトラックに送信
				writeNALsToTracksH265([][]byte{nalUnit}, duration)
				
				// バッファから処理済みのデータを削除
				nalBuffer = nalBuffer[nextNALStart:]
			}
		}
	}
}

// findNextNALUnit はバッファ内の次のNALユニットの開始位置を検出する
func findNextNALUnit(buffer []byte) int {
	if len(buffer) < 4 {
		return -1
	}
	
	for i := 0; i <= len(buffer)-4; i++ {
		if buffer[i] == 0x00 && buffer[i+1] == 0x00 && buffer[i+2] == 0x00 && buffer[i+3] == 0x01 {
			return i
		}
		if i <= len(buffer)-3 && buffer[i] == 0x00 && buffer[i+1] == 0x00 && buffer[i+2] == 0x01 {
			return i
		}
	}
	return -1
}

// --- RTP入力用のFFmpegコマンドを構築するヘルパー関数 ---
func buildRTPCommand(inputURL, sdpCodecName string, preInputArgs []string, postInputArgs []string) ([]string, string, error) {	ffmpegInputArg := inputURL
	var sdpContent string
	isRTP := strings.HasPrefix(inputURL, "rtp://")
	protocolWhitelist := "file,udp,rtp"
	
	var cmdArgs []string
	cmdArgs = append(cmdArgs, "-loglevel", "debug") // より詳細なデバッグ情報

	if isRTP {
		var err error
		sdpContent, err = generateSDPContent(inputURL, sdpCodecName)
		if err != nil {
			return nil, "", fmt.Errorf("failed to generate SDP for %s: %w", inputURL, err)
		}
		ffmpegInputArg = "pipe:0"
		protocolWhitelist += ",pipe"
		cmdArgs = append(cmdArgs, "-f", "sdp")
		cmdArgs = append(cmdArgs, "-protocol_whitelist", protocolWhitelist)		// H.265の場合はプローブサイズと解析時間を増やす
		if sdpCodecName == "H265" {
			cmdArgs = append(cmdArgs, 
				"-probesize", "500M", 
				"-analyzeduration", "120000000",
				"-fflags", "+discardcorrupt+genpts",
				"-c:v", "hevc", // H.265デコーダーを明示的に指定
				"-avoid_negative_ts", "make_zero",
				"-max_delay", "0",
				"-reorder_queue_size", "0",
			)
		} else {
			// H.264の場合のプローブ設定
			cmdArgs = append(cmdArgs, 
				"-probesize", "32M", 
				"-analyzeduration", "5000000",
			)
		}
		log.Printf("Generated SDP content for %s, using pipe:0 as input.", inputURL)
	} else {
		cmdArgs = append(cmdArgs, "-protocol_whitelist", protocolWhitelist)
	}

	cmdArgs = append(cmdArgs, preInputArgs...)
	cmdArgs = append(cmdArgs, "-i", ffmpegInputArg)
	cmdArgs = append(cmdArgs, postInputArgs...)

	return cmdArgs, sdpContent, nil
}

// --- FFmpeg stderr ロガー ---
func logFFmpegStderr(stderr io.ReadCloser) {
	scanner := bufio.NewScanner(stderr)
	for scanner.Scan() {
		log.Printf("FFmpeg: %s", scanner.Text())
	}
}

// --- rtp:// URL用の一時的なSDPファイルを作成するヘルパー関数 ---
func generateSDPContent(rtpURL, sdpCodecName string) (string, error) {
	u, err := url.Parse(rtpURL)
	if err != nil {
		return "", fmt.Errorf("parsing RTP URL %s: %w", rtpURL, err)
	}

	host := u.Hostname()
	port := u.Port()
	if port == "" {
		return "", fmt.Errorf("RTP URL must specify a port: %s", rtpURL)
	}

	// SDP 'c=' 行のIPバージョンを決定
	ipVersion := "IP4"
	// A simple check for IPv6 literal format; net.ParseIP would be more robust
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") && !strings.HasSuffix(host, "]") {
		// This basic check might not cover all IPv6 cases, e.g. if host is a name resolving to IPv6.
		// For literal IPv6 addresses in URLs, they are typically enclosed in [].
		// If host is an IPv6 literal without brackets (unlikely from url.Parse), this might be incorrect.
		// However, url.Hostname() should return it correctly bracketed if it was in the original URL.
		// If it's a bare IPv6, IN IP6 is needed. For simplicity, assuming common IPv4 or bracketed IPv6.
		// A proper check would involve net.ParseIP.
		parsedIP := net.ParseIP(host)
		if parsedIP != nil && parsedIP.To4() == nil { // It's an IPv6 address
			ipVersion = "IP6"
		}
	}
	// Construct SDP content line by line
	sdpLines := []string{
		"v=0",
		fmt.Sprintf("o=- 0 0 IN %s %s", ipVersion, host),
		"s=Dynamically Generated SDP",
		fmt.Sprintf("c=IN %s %s", ipVersion, host),
		"t=0 0",
		fmt.Sprintf("m=video %s RTP/AVP 96", port), // Assuming payload type 96
	}	// コーデックに応じたrtpmapとfmtp行を追加
	if sdpCodecName == "H265" {
		// RFC 7798で定義されているH.265のRTPマップ
		sdpLines = append(sdpLines, "a=rtpmap:96 H265/90000")
		// H.265の詳細なfmtpパラメータ（プロファイル情報を含む）
		sdpLines = append(sdpLines, "a=fmtp:96 profile-tier-level-id=1;profile-id=1;tier-id=0;level-id=93;profile-space=0;profile-compatibility-indicator=96;progressive-source-flag=1;interlaced-source-flag=0;non-packed-constraint-flag=1;frame-only-constraint-flag=1")
		// パケット化モードを追加
		sdpLines = append(sdpLines, "a=recvonly")
	} else if sdpCodecName == "H264" {
		sdpLines = append(sdpLines, "a=rtpmap:96 H264/90000")
		// H.264用のfmtp行を追加（必要に応じて）
		sdpLines = append(sdpLines, "a=fmtp:96 packetization-mode=1")
		sdpLines = append(sdpLines, "a=recvonly")
	} else {
		// フォールバック
		sdpLines = append(sdpLines, fmt.Sprintf("a=rtpmap:96 %s/90000", sdpCodecName))
		sdpLines = append(sdpLines, "a=recvonly")
	}

	// 正しいCRLF行末文字を使用
	sdpContent := strings.Join(sdpLines, "\r\n") + "\r\n"

	log.Printf("Generated SDP content:\n%s", sdpContent)
	return sdpContent, nil
} // H265は特定のfmtp行が必要な場合があります（例：profile-tier-level-id）が、FFmpegがデマックスするために厳密に必要とされることは多くありません。

// --- RTP接続テスト関数 ---
func testRTPConnection(rtpURL string) error {
	u, err := url.Parse(rtpURL)
	if err != nil {
		return fmt.Errorf("RTP URL解析エラー: %v", err)
	}

	host := u.Hostname()
	port := u.Port()
	
	log.Printf("RTP接続テスト: %s:%s に接続を試行中...", host, port)
	
	// UDP接続テスト
	conn, err := net.DialTimeout("udp", net.JoinHostPort(host, port), 5*time.Second)
	if err != nil {
		return fmt.Errorf("RTP接続失敗: %v", err)
	}
	defer conn.Close()
	
	log.Printf("RTP接続テスト成功: %s:%s", host, port)
	return nil
}
