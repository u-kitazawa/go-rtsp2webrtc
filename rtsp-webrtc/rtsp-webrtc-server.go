package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
	"github.com/pion/webrtc/v3/pkg/media/h264reader"
)

// --- WebSocket upgrader ---
var upgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

// --- Track list and mutex ---
var (
	tracksSample = make([]*webrtc.TrackLocalStaticSample, 0)
	trackMutex   sync.RWMutex
	inputURL     string // RTSP URL or RTP SDP file path
	serverPort   string
	codec        string // "h264" or "h265"
	processor    string // "cpu" or "gpu" for H.265 transcoding
	inputType    string // "rtsp" or "rtp"
)

func main() {
	flag.StringVar(&inputURL, "input-url", "", "RTSP URL or RTP SDP file path for the camera")
	flag.StringVar(&serverPort, "port", "8080", "Server port")
	flag.StringVar(&codec, "codec", "h264", "Codec to use for input (h264 or h265)")
	flag.StringVar(&processor, "processor", "gpu", "Processor to use for H.265 transcoding (cpu or gpu)")
	flag.StringVar(&inputType, "input-type", "rtsp", "Input type (rtsp or rtp)")
	flag.Parse()

	if inputURL == "" {
		log.Fatal("Input URL (RTSP or RTP SDP file) must be provided")
	}
	log.Printf("Using Input URL: %s, Input Type: %s, Codec: %s", inputURL, inputType, codec)
	if codec == "h265" {
		log.Printf("H.265 Transcoding Processor: %s", processor)
	}

	switch inputType {
	case "rtsp":
		switch codec {
		case "h264":
			go startFFmpegH264RTSP(inputURL)
		case "h265":
			switch processor {
			case "gpu":
				go startFFmpegH265ToH264NALGPURTSP(inputURL)
			case "cpu":
				go startFFmpegH265ToH264NALCPURTSP(inputURL)
			default:
				log.Fatalf("Unsupported processor for H.265: %s. Use 'cpu' or 'gpu'.", processor)
			}
		default:
			log.Fatalf("Unsupported codec for RTSP: %s", codec)
		}
	case "rtp":
		switch codec {
		case "h264":
			go startFFmpegH264RTP(inputURL)
		case "h265":
			switch processor {
			case "gpu":
				go startFFmpegH265ToH264NALGPURTP(inputURL)
			case "cpu":
				go startFFmpegH265ToH264NALCPURTP(inputURL)
			default:
				log.Fatalf("Unsupported processor for H.265: %s. Use 'cpu' or 'gpu'.", processor)
			}
		default:
			log.Fatalf("Unsupported codec for RTP: %s", codec)
		}
	default:
		log.Fatalf("Unsupported input type: %s. Use 'rtsp' or 'rtp'.", inputType)
	}

	http.HandleFunc("/ws", signalingHandler)
	log.Printf("Server started on :%s", serverPort)
	log.Fatal(http.ListenAndServe(":"+serverPort, nil))
}

// --- WebSocket signaling handler ---
func signalingHandler(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade failed: %v", err)
		return
	}
	defer ws.Close()

	pc, track := setupPeerConnection()
	if pc == nil || track == nil {
		return
	}
	defer func() {
		_ = pc.Close()
		unregisterTrack(track)
	}()

	log.Println("WebSocket connected")

	for {
		_, msg, err := ws.ReadMessage()
		if err != nil {
			log.Printf("WebSocket read error: %v", err)
			break
		}
		var p map[string]interface{}
		if err := json.Unmarshal(msg, &p); err != nil {
			log.Printf("Invalid WebSocket message: %v", err)
			continue
		}
		switch p["type"] {
		case "offer":
			offer := webrtc.SessionDescription{
				Type: webrtc.SDPTypeOffer,
				SDP:  p["sdp"].(string),
			}
			if err := pc.SetRemoteDescription(offer); err != nil {
				log.Printf("SetRemoteDescription error: %v", err)
				continue
			}
			answer, _ := pc.CreateAnswer(nil)
			_ = pc.SetLocalDescription(answer)
			<-webrtc.GatheringCompletePromise(pc)
			resp := map[string]string{"type": "answer", "sdp": pc.LocalDescription().SDP}
			b, _ := json.Marshal(resp)
			_ = ws.WriteMessage(websocket.TextMessage, b)
		case "candidate":
			if c, ok := p["candidate"].(string); ok && c != "" {
				pc.AddICECandidate(webrtc.ICECandidateInit{Candidate: c})
			}
		}
	}
	log.Println("WebSocket disconnected")
}

// --- PeerConnection and track setup ---
func setupPeerConnection() (*webrtc.PeerConnection, *webrtc.TrackLocalStaticSample) {
	m := &webrtc.MediaEngine{}
	_ = m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:     webrtc.MimeTypeH264,
			ClockRate:    90000,
			SDPFmtpLine:  "profile-level-id=42e01f;level-asymmetry-allowed=1;packetization-mode=1",
		},
		PayloadType: 96,
	}, webrtc.RTPCodecTypeVideo)
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m))

	pc, err := api.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}},
	})
	if err != nil {
		log.Printf("PeerConnection creation failed: %v", err)
		return nil, nil
	}

	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000}, "video", "pion")
	if err != nil {
		_ = pc.Close()
		return nil, nil
	}

	rtpSender, err := pc.AddTrack(track)
	if err != nil {
		_ = pc.Close()
		return nil, nil
	}
	go func() {
		rtcpBuf := make([]byte, 1500)
		for {
			if _, _, err := rtpSender.Read(rtcpBuf); err != nil {
				return
			}
		}
	}()

	registerTrack(track)
	return pc, track
}

// --- Track management ---
func registerTrack(t *webrtc.TrackLocalStaticSample) {
	trackMutex.Lock()
	defer trackMutex.Unlock()
	tracksSample = append(tracksSample, t)
}
func unregisterTrack(t *webrtc.TrackLocalStaticSample) {
	trackMutex.Lock()
	defer trackMutex.Unlock()
	for i, tr := range tracksSample {
		if tr == t {
			tracksSample = append(tracksSample[:i], tracksSample[i+1:]...)
			break
		}
	}
}

// --- H.264 RTSP passthrough ---
func startFFmpegH264RTSP(inputURL string) {
	cmd := exec.Command("ffmpeg",
		"-loglevel", "error", // FFmpegのログ出力をエラーのみに抑制
		"-rtsp_transport", "udp", "-max_delay", "0",
		"-analyzeduration", "0", "-avioflags", "direct",
		"-flags", "low_delay", "-fflags", "+igndts+nobuffer",
		"-i", inputURL,
		"-c:v", "copy", "-an", "-fps_mode", "passthrough",
		"-bsf:v", "h264_metadata=aud=insert", "-flush_packets", "1",
		"-f", "h264", "pipe:1",
	)
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	go logFFmpegStderr(stderr)
	_ = cmd.Start()
	log.Println("FFmpeg (H264 RTSP passthrough) started.")

	go func() { _ = cmd.Wait() }()
	h264r, _ := h264reader.NewReader(bufio.NewReader(stdout))
	dur := time.Second / 30
	streamNAL(h264r, dur)
}

// --- H.264 RTP passthrough ---
func startFFmpegH264RTP(inputURL string) {
	cmdArgs, sdpContent, err := buildRTPCommand(inputURL, "H264",
		[]string{}, // preInputArgs
		[]string{ // postInputArgs
			"-c:v", "copy", "-an", "-fps_mode", "passthrough",
			"-bsf:v", "h264_metadata=aud=insert", "-flush_packets", "1",
			"-f", "h264", "pipe:1",
		})
	if err != nil {
		log.Printf("Error building H264 RTP command: %v", err)
		return
	}

	log.Printf("FFmpeg H264 RTP command: ffmpeg %s", strings.Join(cmdArgs, " "))
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
		log.Printf("Failed to start FFmpeg (H264 RTP passthrough): %v", err)
		return
	}
	log.Println("FFmpeg (H264 RTP passthrough) started.")

	go func() {
		if err := cmd.Wait(); err != nil {
			log.Printf("FFmpeg (H264 RTP passthrough) exited with error: %v", err)
		}
	}()
	h264r, _ := h264reader.NewReader(bufio.NewReader(stdout))
	dur := time.Second / 30
	streamNAL(h264r, dur)
}

// --- H.265 to H.264 transcoding (GPU, RTSP) ---
func startFFmpegH265ToH264NALGPURTSP(inputURL string) {
	cmd := exec.Command("ffmpeg",
		"-loglevel", "error", // FFmpegのログ出力をエラーのみに抑制
		"-rtsp_transport", "tcp", "-probesize", "250000", "-analyzeduration", "0",
		"-fflags", "nobuffer+flush_packets+genpts", "-flags", "low_delay", "-max_delay", "0",
		"-hwaccel", "cuda", "-hwaccel_output_format", "cuda",
		"-i", inputURL, "-an",
		"-c:v", "h264_nvenc", "-preset", "p1", "-tune", "ll", "-delay", "0",
		"-rc:v", "cbr", "-b:v", "6M", "-g", "30", "-bf", "0",
		"-fps_mode", "passthrough", "-bsf:v", "h264_metadata=aud=insert",
		"-map", "0:v:0", "-f", "h264", "pipe:1",
	)
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	go logFFmpegStderr(stderr)
	_ = cmd.Start()
	log.Println("FFmpeg (H265 to H264 NAL - GPU, RTSP) started.")

	go func() { _ = cmd.Wait() }()
	h264r, _ := h264reader.NewReader(bufio.NewReader(stdout))
	dur := time.Second / 30
	streamNAL(h264r, dur)
}

// --- H.265 to H.264 transcoding (GPU, RTP) ---
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
			"-fps_mode", "passthrough", "-bsf:v", "h264_metadata=aud=insert",
			"-map", "0:v:0", "-f", "h264", "pipe:1",
		})
	if err != nil {
		log.Printf("Error building H265 to H264 GPU RTP command: %v", err)
		return
	}

	log.Printf("FFmpeg H265 to H264 GPU RTP command: ffmpeg %s", strings.Join(cmdArgs, " "))
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
	log.Println("FFmpeg (H265 to H264 NAL - GPU, RTP) started.")

	go func() {
		if err := cmd.Wait(); err != nil {
			log.Printf("FFmpeg (H265 to H264 NAL - GPU, RTP) exited with error: %v", err)
		}
	}()
	h264r, _ := h264reader.NewReader(bufio.NewReader(stdout))
	dur := time.Second / 30
	streamNAL(h264r, dur)
}

// --- H.265 to H.264 transcoding (CPU, RTSP) ---
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
		"-bsf:v", "h264_metadata=aud=insert",

		// 出力
		"-map", "0:v:0",
		"-f", "h264",
		"pipe:1",
	)
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	go logFFmpegStderr(stderr)
	_ = cmd.Start()
	log.Println("FFmpeg (H265 to H264 NAL - CPU, RTSP) started.")

	go func() { _ = cmd.Wait() }()
	h264r, _ := h264reader.NewReader(bufio.NewReader(stdout))
	dur := time.Second / 30 // フレームレートに応じて調整
	streamNAL(h264r, dur)
}

// --- H.265 to H.264 transcoding (CPU, RTP) ---
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
			"-bsf:v", "h264_metadata=aud=insert",
			"-map", "0:v:0",
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
	log.Println("FFmpeg (H265 to H264 NAL - CPU, RTP) started.")

	go func() {
		if err := cmd.Wait(); err != nil {
			log.Printf("FFmpeg (H265 to H264 NAL - CPU, RTP) exited with error: %v", err)
		}
	}()
	h264r, _ := h264reader.NewReader(bufio.NewReader(stdout))
	dur := time.Second / 30 // フレームレートに応じて調整
	streamNAL(h264r, dur)
}

// --- Helper function to build FFmpeg command for RTP input ---
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

// --- NAL streaming loop ---
func streamNAL(h264r *h264reader.H264Reader, dur time.Duration) {
	for {
		nal, err := h264r.NextNAL()
		if err != nil {
			if err != io.EOF {
				log.Printf("NAL read error: %v", err)
			}
			break
		}
		sample := media.Sample{
			Data:     append([]byte{0x00, 0x00, 0x00, 0x01}, nal.Data...), // Annex-B
			Duration: dur,
		}

		trackMutex.RLock()
		if len(tracksSample) == 0 {
			trackMutex.RUnlock()
			time.Sleep(10 * time.Millisecond)
			continue
		}
		for _, t := range tracksSample {
			if err := t.WriteSample(sample); err != nil {
				log.Printf("WriteSample error: %v", err)
			}
		}
		trackMutex.RUnlock()
	}
}

// --- FFmpeg stderr logger ---
func logFFmpegStderr(stderr io.ReadCloser) {
	scanner := bufio.NewScanner(stderr)
	for scanner.Scan() {
		log.Printf("FFmpeg: %s", scanner.Text())
	}
}

// --- Helper function to create temporary SDP file for rtp:// URLs ---
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

	// Determine IP version for SDP 'c=' line
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
		"v=0\r\n"+
			"o=- 0 0 IN %s %s\r\n"+
			"s=Dynamically Generated SDP\r\n"+
			"c=IN %s %s\r\n"+
			"t=0 0\r\n"+
			"m=video %s RTP/AVP 96\r\n"+ // Assuming payload type 96 from error log
			"a=rtpmap:96 %s/90000\r\nsprop-vps=QAEMAf//AWAAAAMAAAMAAAMAAAMAlqwJ\r sprop-sps=QgEBAWAAAAMAAAMAAAMAAAMAlqACgIAtFja5JMmuWcAgAAB9IAAOqcE=\r sprop-pps=RAHgdrAmQA==",

		ipVersion, host, ipVersion, host, port, sdpCodecName,
	)

	if sdpCodecName == "H264" {
		sdpContent += "a=fmtp:96 packetization-mode=1\r\n" // Common for H.264
	}
	// H265 might need specific fmtp lines (e.g., for profile-tier-level-id), but often not strictly required for FFmpeg to demux.

	return sdpContent, nil
}
