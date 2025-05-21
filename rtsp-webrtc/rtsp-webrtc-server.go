package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
	"github.com/pion/webrtc/v3/pkg/media/h264reader"
)

// WebSocket upgrader
var upgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

// concurrent-safe track list
var (
	tracksSample = make([]*webrtc.TrackLocalStaticSample, 0) // Unified tracks list for H.264 samples
	trackMutex   sync.RWMutex
	rtspURL      string
	serverPort   string
	codec        string // "h264" or "h265"
)

func main() {
	flag.StringVar(&rtspURL, "rtsp-url", "rtsp://admin:admin@192.168.40.118:1935", "RTSP URL for the camera")
	flag.StringVar(&serverPort, "port", "8080", "Server port")
	flag.StringVar(&codec, "codec", "h264", "Codec to use for RTSP input (h264 or h265). H.265 will be transcoded to H.264 NALs.")
	flag.Parse()

	if rtspURL == "" {
		log.Fatal("RTSP URL must be provided via the -rtsp-url flag")
	}

	log.Printf("Using RTSP URL: %s, Codec: %s", rtspURL, codec)

	if codec == "h264" {
		go startFFmpegH264(rtspURL)
	} else if codec == "h265" {
		log.Printf("H.265 input selected. FFmpeg will transcode to H.264 NAL units.")
		go startFFmpegH265ToH264NAL(rtspURL)
	} else {
		log.Fatalf("Unsupported codec: %s. Choose 'h264' or 'h265'", codec)
	}

	http.HandleFunc("/ws", signalingHandler)
	log.Printf("Server started on :%s", serverPort)
	log.Fatal(http.ListenAndServe(":"+serverPort, nil))
}

func signalingHandler(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Failed to upgrade to WebSocket: %v", err)
		return
	}
	defer ws.Close()

	// setup PeerConnection
	m := &webrtc.MediaEngine{}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: "profile-level-id=42e01f;level-asymmetry-allowed=1;packetization-mode=1"},
		PayloadType:        96, 
	}, webrtc.RTPCodecTypeVideo); err != nil {
		log.Printf("Failed to register H264 codec: %v", err)
		return
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m))
	pc, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}}})
	if err != nil {
		log.Printf("Failed to create PeerConnection: %v", err)
		return
	}
	defer func() {
		if err := pc.Close(); err != nil {
			log.Printf("Failed to close PeerConnection: %v", err)
		}
	}()

	// Unified track creation for both H.264 and H.265 (transcoded to H.264 NALs)
	track, trackErr := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000}, "video", "pion")
	if trackErr != nil {
		log.Printf("Failed to create NewTrackLocalStaticSample: %v", trackErr)
		return
	}
	rtpSender, addTrackErr := pc.AddTrack(track) // rtpSender is needed for RTCP handling
	if addTrackErr != nil {
		log.Printf("Failed to add H.264 sample track to PeerConnection: %v", addTrackErr)
		return
	}

	// Goroutine to read RTCP packets from the rtpSender.
	go func() {
		rtcpBuf := make([]byte, 1500)
		for {
			if _, _, rtcpErr := rtpSender.Read(rtcpBuf); rtcpErr != nil {
				return
			}
		}
	}()

	// register track
	trackMutex.Lock()
	tracksSample = append(tracksSample, track) 
	trackMutex.Unlock()
	defer func() {
		trackMutex.Lock()
		for i, t := range tracksSample {
			if t == track {
				tracksSample = append(tracksSample[:i], tracksSample[i+1:]...)
				break
			}
		}
		trackMutex.Unlock()
	}()

	// WebSocket message loop
	for {
		_, msg, err := ws.ReadMessage()
		if err != nil {
			log.Printf("Error reading message from WebSocket: %v", err)
			break
		}
		var p map[string]interface{}
		if err := json.Unmarshal(msg, &p); err != nil {
			log.Printf("Error unmarshalling message: %v. Message: %s", err, string(msg))
			continue
		}

		switch p["type"] {
		case "offer":
			sdpStr, ok := p["sdp"].(string)
			if !ok {
				log.Printf("Invalid offer SDP format. Payload: %v", p)
				continue
			}
			offer := webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdpStr}
			if err := pc.SetRemoteDescription(offer); err != nil {
				log.Printf("Failed to set remote description (offer): %v", err)
				continue
			}
			answer, err := pc.CreateAnswer(nil)
			if err != nil {
				log.Printf("Failed to create answer: %v", err)
				continue
			}
			if err := pc.SetLocalDescription(answer); err != nil {
				log.Printf("Failed to set local description (answer): %v", err)
				continue
			}
			<-webrtc.GatheringCompletePromise(pc) // Wait for ICE gathering to complete
			resp := map[string]string{"type": "answer", "sdp": pc.LocalDescription().SDP}
			b, err := json.Marshal(resp)
			if err != nil {
				log.Printf("Error marshalling answer: %v", err)
				continue
			}
			if err := ws.WriteMessage(websocket.TextMessage, b); err != nil {
				log.Printf("Error writing answer to WebSocket: %v", err)
				break
			}
		case "candidate":
			candidateStr, ok := p["candidate"].(string)
			if !ok {
				log.Printf("Invalid candidate format: candidate is not a string. Payload: %v", p)
				continue
			}
			// Allow empty candidate string for end of candidates
			if candidateStr != "" {
				if err := pc.AddICECandidate(webrtc.ICECandidateInit{Candidate: candidateStr}); err != nil {
					log.Printf("Failed to add ICE candidate: %v", err)
				}
			}
		default:
			log.Printf("Received unknown message type: %s", p["type"])
		}
	}
}

// startFFmpegH264 handles direct H.264 RTSP streams
func startFFmpegH264(rtspURL string) {
	cmd := exec.Command("ffmpeg",
		"-rtsp_transport", "udp",
		"-max_delay", "0",
		"-analyzeduration", "0",
		"-avioflags", "direct",
		"-flags", "low_delay",
		"-fflags", "+igndts+nobuffer",
		"-i", rtspURL,
		// 出力
		"-c:v", "copy", "-an",
		
		"-fps_mode", "passthrough", 
		"-bsf:v", "h264_metadata=aud=insert",
		"-flush_packets", "1",
		"-f", "h264",
		"pipe:1",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatalf("Failed to get stdout pipe for FFmpeg (H.264): %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		log.Printf("Failed to get stderr pipe for FFmpeg (H.264): %v", err)
	} else {
		go func() {
			scanner := bufio.NewScanner(stderr)
			for scanner.Scan() {
				log.Printf("FFmpeg (H.264): %s", scanner.Text())
			}
		}()
	}

	if err := cmd.Start(); err != nil {
		log.Fatalf("Failed to start FFmpeg (H.264): %v", err)
	}
	log.Printf("FFmpeg (H.264) process started for URL: %s", rtspURL)

	go func() {
		if err := cmd.Wait(); err != nil {
			log.Printf("FFmpeg (H.264) command finished with error: %v", err)
		} else {
			log.Println("FFmpeg (H.264) command finished successfully.")
		}
	}()

	rdr, _ := h264reader.NewReader(bufio.NewReaderSize(stdout, 4096))
	dur := time.Second / 30 

	for {
		nal, err := rdr.NextNAL()
		if err != nil {
			log.Printf("Error reading NAL unit (H.264): %v", err)
			break
		}
		data := nal.Data

		trackMutex.RLock()
		for _, t := range tracksSample {
			if err := t.WriteSample(media.Sample{Data: data, Duration: dur}); err != nil {
			}
		}
		trackMutex.RUnlock()
	}
}

// startFFmpegH265ToH264NAL starts FFmpeg to transcode H.265 RTSP to H.264 NAL units
// and pipes them to the WebRTC tracks.
func startFFmpegH265ToH264NAL(rtspURL string) {
	cmdArgs := []string{
    // ── 取り込み ──
    "-rtsp_transport", "tcp",
    "-probesize", "250000",           // 既定 5000000 をかなり縮小（起動 0.1 秒台）
    "-analyzeduration", "0",
    "-fflags", "nobuffer+flush_packets+genpts",
    "-flags", "low_delay",
    "-max_delay", "0",

    // ── NVDEC: 0-copy でデコード ──
    "-hwaccel", "cuda",
    "-hwaccel_output_format", "cuda", // ★ メモリコピーを避ける
    "-i", rtspURL,
    "-an",
		
    // ── NVENC ──
    "-c:v", "h264_nvenc",
    "-preset", "p1",                  // 最速
    "-tune", "ll",                    // low-latency
    "-delay", "0",                    // CPB を空にする（追加遅延 0）
    "-rc:v", "cbr", "-b:v", "6M",     // レート一定（可変でも可）
    "-g", "30", "-bf", "0",
		
    // ── 時間情報 ──
    "-fps_mode", "passthrough",       // PTS/DTS は genpts の値をそのまま
		//  (vsync ではなく fps_mode を推奨)
		
    // ── フレーム境界を保証 ──
    "-bsf:v", "h264_metadata=aud=insert",
		
    "-map", "0:v:0",
    "-f", "h264", "pipe:1",
}


	cmd := exec.Command("ffmpeg", cmdArgs...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Fatalf("Failed to get stdout pipe for FFmpeg (H.265->H.264 NAL): %v", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		log.Printf("Warning: Failed to get stderr pipe for FFmpeg (H.265->H.264 NAL): %v. FFmpeg errors might not be logged.", err)
	} else {
		go func() {
			scanner := bufio.NewScanner(stderr)
			for scanner.Scan() {
				log.Printf("FFmpeg (H.265->H.264 NAL): %s", scanner.Text())
			}
			if scanErr := scanner.Err(); scanErr != nil {
				log.Printf("Error reading FFmpeg stderr (H.265->H.264 NAL): %v", scanErr)
			}
		}()
	}

	if err := cmd.Start(); err != nil {
		log.Fatalf("Failed to start FFmpeg (H.265->H.264 NAL): %v. Command: ffmpeg %s", err, strings.Join(cmdArgs, " "))
	}
	log.Printf("FFmpeg (H.265->H.264 NAL) process started. Transcoding from %s to H.264 NALs.", rtspURL)

	go func() {
		if err := cmd.Wait(); err != nil {
			log.Printf("FFmpeg (H.265->H.264 NAL) command finished with error: %v", err)
		} else {
			log.Println("FFmpeg (H.265->H.264 NAL) command finished successfully.")
		}
	}()

	// Use a buffered reader for stdout
	h264BufReader := bufio.NewReaderSize(stdout, 4096*2) 
	h264r, err := h264reader.NewReader(h264BufReader)
	if err != nil {
		log.Printf("Failed to create H264 reader for H.265->H.264 NAL stream: %v", err)
		return 
	}

	// Assuming 30 FPS for sample duration. Adjust if necessary or obtain dynamically.
	dur := time.Second / 30

	for {
		nal, err := h264r.NextNAL()
		if err == io.EOF {
			log.Println("FFmpeg (H.265->H.264 NAL) stdout EOF.")
			break
		}
		if err != nil {
			log.Printf("Error reading NAL unit (H.265->H.264 NAL): %v", err)
			break
		}

		sample := media.Sample{Data: nal.Data, Duration: dur}

		trackMutex.RLock()
		if len(tracksSample) == 0 {
			trackMutex.RUnlock()
			continue
		}
		for _, t := range tracksSample {
			if writeErr := t.WriteSample(sample); writeErr != nil {
		}
		}
		trackMutex.RUnlock()
	}
	log.Println("Stopped processing H.264 NALs from FFmpeg (H.265 input).")
}
