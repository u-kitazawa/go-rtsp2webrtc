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
	cmdArgs, sdpContent, err := buildRTPCommand(inputURL, "H265",
		[]string{ // preInputArgs
			"-fflags", "nobuffer+genpts",
		},
		[]string{ // postInputArgs
			"-an",
			"-c:v", "libx264",
			"-preset", "ultrafast",
			"-tune", "zerolatency",
			"-x264-params", "nal-hrd=cbr",
			"-b:v", "6M",
			"-maxrate", "6M",
			"-bufsize", "6M",
			"-g", "30",
			"-bf", "0",
			"-fps_mode", "passthrough",
			// "-map", "0:v:0",
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

// --- RTP入力用のFFmpegコマンドを構築するヘルパー関数 ---
func buildRTPCommand(inputURL, sdpCodecName string, preInputArgs []string, postInputArgs []string) ([]string, string, error) {
	ffmpegInputArg := inputURL
	var sdpContent string
	isRTP := strings.HasPrefix(inputURL, "rtp://")
	protocolWhitelist := "file,udp,rtp"

	var cmdArgs []string
	cmdArgs = append(cmdArgs, "-loglevel", "error")

	if isRTP {
		var err error
		sdpContent, err = generateSDPContent(inputURL, sdpCodecName)
		if err != nil {
			return nil, "", fmt.Errorf("failed to generate SDP for %s: %w", inputURL, err)
		}
		ffmpegInputArg = "pipe:0"
		protocolWhitelist += ",pipe"

		cmdArgs = append(cmdArgs, "-f", "sdp")
		cmdArgs = append(cmdArgs, "-protocol_whitelist", protocolWhitelist)
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

	sdpContent := fmt.Sprintf(
		"v=0" +
			"o=- 0 0 IN %s %s" +
			"s=Dynamically Generated SDP" +
			"c=IN %s %s" +
			"t=0 0" +
			"m=video %s RTP/AVP 96"+ // Assuming payload type 96 from error log
			"a=rtpmap:96 %s/90000sprop-vps=QAEMAf//AWAAAAMAAAMAAAMAAAMAlqwJsprop-pps=RAHgdrAmQA==",
		ipVersion, host, ipVersion, host, port, sdpCodecName,
	)

	return sdpContent, nil
} // H265は特定のfmtp行が必要な場合があります（例：profile-tier-level-id）が、FFmpegがデマックスするために厳密に必要とされることは多くありません。
