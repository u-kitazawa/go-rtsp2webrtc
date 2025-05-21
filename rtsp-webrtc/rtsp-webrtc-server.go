package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"os/exec"
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
	rtspURL      string
	serverPort   string
	codec        string // "h264" or "h265"
)

func main() {
	flag.StringVar(&rtspURL, "rtsp-url", "", "RTSP URL for the camera")
	flag.StringVar(&serverPort, "port", "8080", "Server port")
	flag.StringVar(&codec, "codec", "h264", "Codec to use for RTSP input (h264 or h265)")
	flag.Parse()

	if rtspURL == "" {
		log.Fatal("RTSP URL must be provided")
	}
	log.Printf("Using RTSP URL: %s, Codec: %s", rtspURL, codec)

	switch codec {
	case "h264":
		go startFFmpegH264(rtspURL)
	case "h265":
		go startFFmpegH265ToH264NAL(rtspURL)
	default:
		log.Fatalf("Unsupported codec: %s", codec)
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
func startFFmpegH264(rtspURL string) {
	cmd := exec.Command("ffmpeg",
		"-rtsp_transport", "udp", "-max_delay", "0",
		"-analyzeduration", "0", "-avioflags", "direct",
		"-flags", "low_delay", "-fflags", "+igndts+nobuffer",
		"-i", rtspURL,
		"-c:v", "copy", "-an", "-fps_mode", "passthrough",
		"-bsf:v", "h264_metadata=aud=insert", "-flush_packets", "1",
		"-f", "h264", "pipe:1",
	)
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	go logFFmpegStderr(stderr)
	_ = cmd.Start()
	log.Println("FFmpeg (H264 passthrough) started.")

	go func() { _ = cmd.Wait() }()
	h264r, _ := h264reader.NewReader(bufio.NewReader(stdout))
	dur := time.Second / 30
	streamNAL(h264r, dur)
}

// --- H.265 to H.264 transcoding ---
func startFFmpegH265ToH264NAL(rtspURL string) {
	cmd := exec.Command("ffmpeg",
		"-rtsp_transport", "tcp", "-probesize", "250000", "-analyzeduration", "0",
		"-fflags", "nobuffer+flush_packets+genpts", "-flags", "low_delay", "-max_delay", "0",
		"-hwaccel", "cuda", "-hwaccel_output_format", "cuda",
		"-i", rtspURL, "-an",
		"-c:v", "h264_nvenc", "-preset", "p1", "-tune", "ll", "-delay", "0",
		"-rc:v", "cbr", "-b:v", "6M", "-g", "30", "-bf", "0",
		"-fps_mode", "passthrough", "-bsf:v", "h264_metadata=aud=insert",
		"-map", "0:v:0", "-f", "h264", "pipe:1",
	)
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()

	go logFFmpegStderr(stderr)
	_ = cmd.Start()
	log.Println("FFmpeg (H265 to H264 NAL) started.")

	go func() { _ = cmd.Wait() }()
	h264r, _ := h264reader.NewReader(bufio.NewReader(stdout))
	dur := time.Second / 30
	streamNAL(h264r, dur)
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
