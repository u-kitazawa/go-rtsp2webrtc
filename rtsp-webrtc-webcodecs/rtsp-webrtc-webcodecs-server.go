package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"flag" // 追加
	"io"
	"log"
	"net/http"
	"os/exec"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media/h264reader"
)

// WebSocket upgrader
var upgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

var (
	currentVideoDataChannel *webrtc.DataChannel
	dataChannelMutex        sync.RWMutex
	rtspURL                 string
	serverPort              string // 追加
)


func main() {
	flag.StringVar(&rtspURL, "rtsp-url", "rtsp://admin:admin@192.168.40.118:1935", "RTSP URL for the camera")
	flag.StringVar(&serverPort, "port", "8080", "Server port") // 追加
	flag.Parse()

	if rtspURL == "" {
		log.Fatal("RTSP URL must be provided via the -rtsp-url flag")
	}

	log.Printf("Using RTSP URL: %s", rtspURL)
	go startFFmpeg(rtspURL)

	http.HandleFunc("/ws", signalingHandler)
	log.Printf("Server started on :%s", serverPort) // 変更
	log.Fatal(http.ListenAndServe(":"+serverPort, nil)) // 変更
}

func signalingHandler(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Failed to upgrade to WebSocket: %v", err)
		return
	}
	defer func() {
		log.Println("Closing WebSocket connection in defer.")
		ws.Close()
	}()

	// setup PeerConnection
	m := &webrtc.MediaEngine{}
	m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: "profile-level-id=42e01f;level-asymmetry-allowed=1;packetization-mode=1"},
		PayloadType:        96,
	}, webrtc.RTPCodecTypeVideo)
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m))
	pc, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}}})
	if err != nil {
		log.Printf("Failed to create PeerConnection: %v", err)
		return
	}
	defer func() {
		log.Println("Closing PeerConnection in defer.")
		if pc != nil {
			if err := pc.Close(); err != nil {
				log.Printf("Failed to close PeerConnection: %v", err)
			}
		}
	}()

	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Printf("PeerConnection ICE Connection State has changed: %s\\n", state.String())
		if state == webrtc.ICEConnectionStateFailed || state == webrtc.ICEConnectionStateDisconnected || state == webrtc.ICEConnectionStateClosed {
			log.Printf("ICE Connection State is %s, WebSocket might close or be closed.", state.String())
		}
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("PeerConnection State has changed: %s\\n", state.String())
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateDisconnected || state == webrtc.PeerConnectionStateClosed {
			log.Printf("PeerConnection State is %s, WebSocket might close or be closed.", state.String())
		}
	})

	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		log.Printf("New DataChannel detected: %s - ID: %d\\n", dc.Label(), *dc.ID())

		if dc.Label() != "videoNaluChannel" {
			log.Printf("DataChannel %s is not 'videoNaluChannel', ignoring.", dc.Label())
			return
		}

		dc.OnOpen(func() {
			log.Printf("Data channel '%s'-'%d' open. Ready to send video data.\\n", dc.Label(), *dc.ID())
			dataChannelMutex.Lock()
			currentVideoDataChannel = dc
			dataChannelMutex.Unlock()
		})

		// Register channel closing handling
		dc.OnClose(func() {
			log.Printf("Data channel '%s'-'%d' closed.\\n", dc.Label(), *dc.ID())
			dataChannelMutex.Lock()
			// Clear currentVideoDataChannel if it's this one
			if currentVideoDataChannel != nil && currentVideoDataChannel.ID() != nil && dc.ID() != nil &&
				*currentVideoDataChannel.ID() == *dc.ID() {
				currentVideoDataChannel = nil
				log.Printf("currentVideoDataChannel has been set to nil for %s-%d", dc.Label(), *dc.ID())
			}
			dataChannelMutex.Unlock()
		})

		dc.OnError(func(err error) {
			log.Printf("Data channel '%s'-'%d' error: %v\\n", dc.Label(), *dc.ID(), err)
		})

	})


	// WebSocket message loop
	for {
		_, msg, err := ws.ReadMessage()
		if err != nil {
			// Log the actual error that caused the ReadMessage loop to exit
			log.Printf("Error reading message from WebSocket: %v. Closing WebSocket.", err)
			// Check for specific WebSocket closure errors
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure, websocket.CloseNormalClosure) {
				log.Printf("WebSocket unexpectedly closed: %v", err)
			}
			break // Exit loop, defer will close ws and pc
		}
		var p map[string]interface{}
		if err := json.Unmarshal(msg, &p); err != nil { // Added error handling for unmarshal
			log.Printf("Error unmarshalling message: %v. Message: %s", err, string(msg))
			continue // Skip malformed message
		}

		switch p["type"] {
		case "offer":
			sdp, ok := p["sdp"].(string)
			if !ok {
				log.Printf("Invalid offer format: sdp is not a string. Payload: %v", p)
				continue
			}
			log.Println("Received offer") // Log offer receipt
			if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdp}); err != nil {
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
			
			// Wait for ICE Gathering to complete
			<-webrtc.GatheringCompletePromise(pc)
			log.Println("ICE Gathering complete. Sending answer.") 

			localDesc := pc.LocalDescription()
			if localDesc == nil {
				log.Println("Local description is nil after ICE gathering.")
				continue
			}
			
			resp := map[string]string{"type": "answer", "sdp": localDesc.SDP}
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
			log.Println("Received ICE candidate") 
			if err := pc.AddICECandidate(webrtc.ICECandidateInit{Candidate: candidateStr}); err != nil {
				log.Printf("Failed to add ICE candidate: %v", err)
			}
		default:
			log.Printf("Received unknown message type: %s", p["type"])
		}
	}
	log.Println("Exiting signalingHandler.") // Log when handler exits
}

func startFFmpeg(rtspURL string) {
	log.Printf("Starting FFmpeg for RTSP URL: %s\\n", rtspURL)
	cmd := exec.Command("ffmpeg",
		"-hide_banner", // Optional: cleans up FFmpeg startup logs
		"-avioflags", "direct",
		"-flags", "low_delay",
		"-fflags", "+igndts+nobuffer",
		"-vsync", "0", // Can also be -vsync passthrough or -vsync cfr/vfr depending on needs
		"-rtsp_transport", "tcp",
		"-i", rtspURL,
		"-tune", "zerolatency",
		// IMPORTANT: -c:v copy assumes the RTSP source is already H.264 with a profile compatible with client's VideoDecoder (e.g., profile-level-id=42e01f for Constrained Baseline@L3.1).
		// If the source has a different profile (e.g., Main or High), VideoDecoder might fail.
		// To force re-encoding to a compatible profile (adds latency and CPU load):
		// "-c:v", "libx264", "-profile:v", "baseline", "-level:v", "3.1", "-preset", "ultrafast", "-tune", "zerolatency",
		// "-pix_fmt", "yuv420p", // Often required for H.264
		"-c:v", "copy",
		"-an", // No audio
		"-f", "h264", // Output raw H.264 (Annex B format)
		"pipe:1",     // Output to stdout
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("Error creating stdout pipe for FFmpeg: %v\\n", err)
		return
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		log.Printf("Error creating stderr pipe for FFmpeg: %v\\n", err)
		return
	}

	if err := cmd.Start(); err != nil {
		log.Printf("Error starting FFmpeg: %v\\n", err)
		return
	}
	log.Println("FFmpeg process started.")

	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
		}
		if err := scanner.Err(); err != nil {
			log.Printf("Error reading FFmpeg stderr: %v\\n", err)
		}
		log.Println("FFmpeg stderr stream finished.")
	}()

	h264BufReader := bufio.NewReaderSize(stdout, 1024*64) 
	rdr, err := h264reader.NewReader(h264BufReader)
	if err != nil {
		log.Printf("Error creating H264 reader: %v\\n", err)
			if cmd.Process != nil {
			cmd.Process.Kill()
		}
		cmd.Wait() 
		return
	}

	log.Println("Reading H.264 NALUs from FFmpeg...")
	for {
		nal, err := rdr.NextNAL()
		if err != nil {
			if err == io.EOF {
				log.Println("FFmpeg stream ended (EOF).")
			} else {
				log.Printf("Error reading NAL from H264 reader: %v\\n", err)
			}
			break 
		}

		var naluType string
		switch nal.UnitType {
		case 5: 
			naluType = "key"
		case 7:
			naluType = "sps"
		case 8: 
			naluType = "pps"
		default:
			naluType = "delta"
		}
		encodedData := base64.StdEncoding.EncodeToString(nal.Data)
		payload := map[string]interface{}{
			"type":      naluType,
			"timestamp": time.Now().UnixNano() / int64(time.Millisecond),
			"data":      encodedData,
		}

		jsonData, err := json.Marshal(payload)
		if err != nil {
			log.Printf("Error marshalling NALU payload: %v\\n", err)
			continue
		}

		dataChannelMutex.RLock()
		dc := currentVideoDataChannel
		dataChannelMutex.RUnlock()

		if dc != nil && dc.ReadyState() == webrtc.DataChannelStateOpen {

			
			err := dc.SendText(string(jsonData))
			if err != nil {
				log.Printf("Error writing NALU data to DataChannel (type: %s): %v\\n", naluType, err)
			} 
		} else {
			time.Sleep(2 * time.Millisecond)
		}
	}

	log.Println("Stopped reading H.264 NALUs.")
	if cmd.Process != nil {
		log.Println("Waiting for FFmpeg process to exit...")
		if err := cmd.Wait(); err != nil { 
			log.Printf("FFmpeg process exited with error: %v\\n", err)
		} else {
			log.Println("FFmpeg process exited successfully.")
		}
	} else {
		log.Println("FFmpeg process was not started or already cleaned up.")
	}
}
