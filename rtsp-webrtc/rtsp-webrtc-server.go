package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"log"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings" // Added for strings.Join
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/rtp" // Required for RTP packet handling for H.265 to H.264 transcoding
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
	"github.com/pion/webrtc/v3/pkg/media/h264reader"
)

// WebSocket upgrader
var upgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

// concurrent-safe track list
var (
	tracksSample     = make([]*webrtc.TrackLocalStaticSample, 0) // For H.264 direct
	tracksRTP        = make([]*webrtc.TrackLocalStaticRTP, 0)   // For H.265 to H.264 transcoded RTP
	trackMutex       sync.RWMutex
	rtspURL          string
	serverPort       string
	codec            string // "h264" or "h265"
	rtpPort          int    // Port for FFmpeg to send RTP to (used with "h265" codec)
)

func main() {
	flag.StringVar(&rtspURL, "rtsp-url", "rtsp://admin:admin@192.168.40.118:1935", "RTSP URL for the camera")
	flag.StringVar(&serverPort, "port", "8080", "Server port")
	flag.StringVar(&codec, "codec", "h264", "Codec to use for RTSP input (h264 or h265). H.265 will be transcoded to H.264.")
	flag.IntVar(&rtpPort, "rtp-port", 5004, "UDP port for receiving H.264 RTP from FFmpeg (used with h265 codec)")
	flag.Parse()

	if rtspURL == "" {
		log.Fatal("RTSP URL must be provided via the -rtsp-url flag")
	}

	log.Printf("Using RTSP URL: %s, Codec: %s", rtspURL, codec)

	if codec == "h264" {
		go startFFmpegH264(rtspURL)
	} else if codec == "h265" {
		log.Printf("H.265 input selected. FFmpeg will transcode to H.264 and send via RTP to port %d", rtpPort)
		go startFFmpegH265AndRTPListener(rtspURL, rtpPort)
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
	// FFmpeg will output H.264 in both cases (direct for H.264 input, transcoded for H.265 input)
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: "profile-level-id=42e01f;level-asymmetry-allowed=1;packetization-mode=1"},
		PayloadType:        96, // This should match the payload type used by FFmpeg
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

	if codec == "h264" {
		track, trackErr := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000}, "video", "pion")
		if trackErr != nil {
			log.Printf("Failed to create NewTrackLocalStaticSample: %v", trackErr)
			return
		}
		_, addTrackErr := pc.AddTrack(track)
		if addTrackErr != nil {
			log.Printf("Failed to add H.264 sample track to PeerConnection: %v", addTrackErr)
			return
		}
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
	} else if codec == "h265" {
		track, trackErr := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000}, "video", "pion")
		if trackErr != nil {
			log.Printf("Failed to create NewTrackLocalStaticRTP: %v", trackErr)
			return
		}
		rtpSender, addTrackErr := pc.AddTrack(track)
		if addTrackErr != nil {
			log.Printf("Failed to add H.264 RTP track to PeerConnection: %v", addTrackErr)
			return
		}
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
		tracksRTP = append(tracksRTP, track)
		trackMutex.Unlock()
		defer func() {
			trackMutex.Lock()
			for i, t := range tracksRTP {
				if t == track {
					tracksRTP = append(tracksRTP[:i], tracksRTP[i+1:]...)
					break
				}
			}
			trackMutex.Unlock()
		}()
	}

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
		"-c:v", "copy", "-an", // Copy H.264 video, no audio
		"-fps_mode", "passthrough", // Moved before -i
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
	dur := time.Second / 30 // Assuming 30 FPS, adjust if necessary

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
				// log.Printf("Error writing sample to H.264 track: %v", err) // Can be verbose
			}
		}
		trackMutex.RUnlock()
	}
}

// startFFmpegH265AndRTPListener starts FFmpeg to transcode H.265 RTSP to H.264 RTP
// and listens on a UDP port for these RTP packets, writing them to the WebRTC tracks.
func startFFmpegH265AndRTPListener(rtspURL string, rtpListenPort int) {
	cmdArgs := []string{
		"-rtsp_transport", "tcp", // Consider making this configurable or testing udp
		"-probesize", "32",       // May need adjustment based on stream
		"-analyzeduration", "500000", // May need adjustment
		"-fflags", "nobuffer",
		"-i", rtspURL, // Input H.265 RTSP stream
		"-an",         // No audio
		"-c:v", "libx264",
		"-preset", "ultrafast", // Balances CPU and latency. Consider "superfast" or "veryfast" for better quality if CPU allows.
		"-tune", "zerolatency", // Good for low latency
		"-threads", "auto", // Utilize available CPU cores for encoding
		"-pix_fmt", "yuv420p",
		"-payload_type", "96", // Must match WebRTC MediaEngine
		"-fps_mode", "passthrough",
		"-f", "rtp",

		"rtp://127.0.0.1:" + strconv.Itoa(rtpListenPort), // Consider making IP configurable
	}
	cmd := exec.Command("ffmpeg", cmdArgs...)

	stderr, err := cmd.StderrPipe()
	if err != nil {
		// If getting stderr pipe fails, we can still try to start FFmpeg,
		// but we won't get its error output directly.
		log.Printf("Warning: Failed to get stderr pipe for FFmpeg (H.265->H.264): %v. FFmpeg errors might not be logged.", err)
	} else {
		go func() {
			scanner := bufio.NewScanner(stderr)
			for scanner.Scan() {
				log.Printf("FFmpeg (H.265->H.264): %s", scanner.Text())
			}
			// Check for scanner errors, though rare for stderr.
			if scanErr := scanner.Err(); scanErr != nil {
				log.Printf("Error reading FFmpeg stderr: %v", scanErr)
			}
		}()
	}

	if err := cmd.Start(); err != nil {
		log.Fatalf("Failed to start FFmpeg (H.265->H.264): %v. Command: ffmpeg %s", err, strings.Join(cmdArgs, " ")) // Log the full command
	}
	log.Printf("FFmpeg (H.265->H.264) process started. Transcoding from %s to H.264 RTP on 127.0.0.1:%d", rtspURL, rtpListenPort)

	// Goroutine to wait for FFmpeg command to finish and log its exit status
	go func() {
		if err := cmd.Wait(); err != nil {
			log.Printf("FFmpeg (H.265->H.264) command finished with error: %v", err)
		} else {
			log.Println("FFmpeg (H.265->H.264) command finished successfully.")
		}
	}()

	listener, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: rtpListenPort})
	if err != nil {
		log.Fatalf("Failed to listen on UDP port %d for RTP: %v", rtpListenPort, err)
	}
	defer listener.Close()

	log.Printf("Listening for H.264 RTP packets (from H.265 transcode) on UDP 127.0.0.1:%d", rtpListenPort)

	rtpBuf := make([]byte, 2048) // Buffer for incoming RTP packets
	for {
		n, _, err := listener.ReadFromUDP(rtpBuf)
		if err != nil {
			// If the listener is closed, this error is expected, so we can break the loop.
			// For other errors, log and continue.
			if netErr, ok := err.(net.Error); ok && !netErr.Temporary() && !netErr.Timeout() {
				log.Printf("UDP listener error, closing RTP listener: %v", err)
				break // Exit loop if it's a permanent error (e.g. listener closed)
			}
			log.Printf("Error reading RTP from UDP: %v", err)
			continue
		}

		packet := &rtp.Packet{}
		if err := packet.Unmarshal(rtpBuf[:n]); err != nil {
			log.Printf("Error unmarshalling RTP packet: %v", err)
			continue
		}

		trackMutex.RLock()
		// Optimization: if there are no tracks, don't try to write.
		if len(tracksRTP) == 0 {
			trackMutex.RUnlock()
			// Optional: Add a small sleep here if this state is common and CPU usage is a concern.
			// time.Sleep(10 * time.Millisecond)
			continue
		}
		for _, t := range tracksRTP {
			if writeErr := t.WriteRTP(packet); writeErr != nil {
				// This log can be very verbose. Enable if debugging track writing issues.
				log.Printf("Error writing RTP to track: %v, Track: %s, Codec: %s", writeErr, t.ID(), t.Codec().MimeType)
			}
		}
		trackMutex.RUnlock()
	}
	log.Printf("Stopped listening for H.264 RTP packets on UDP 127.0.0.1:%d", rtpListenPort)
}
