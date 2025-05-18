// main.go
package main

import (
	"bufio"
	"encoding/json"
	"flag"
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

// WebSocket upgrader
var upgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

// concurrent-safe track list
var (
	tracks     = make([]*webrtc.TrackLocalStaticSample, 0)
	trackMutex sync.RWMutex
	rtspURL                 string
	serverPort              string
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
		return
	}
	defer ws.Close()

	// setup PeerConnection
	m := &webrtc.MediaEngine{}
	m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: "profile-level-id=42e01f;level-asymmetry-allowed=1;packetization-mode=1"},
		PayloadType: 96,
	}, webrtc.RTPCodecTypeVideo)
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m))
	pc, _ := api.NewPeerConnection(webrtc.Configuration{ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}}})

	track, _ := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264}, "video", "pion")
	pc.AddTrack(track)

	// register
	trackMutex.Lock()
	tracks = append(tracks, track)
	trackMutex.Unlock()
	defer func() {
		trackMutex.Lock()
		for i, t := range tracks {
			if t == track {
				tracks = append(tracks[:i], tracks[i+1:]...)
				break
			}
		}
		trackMutex.Unlock()
	}()

	// WebSocket message loop
	for {
		_, msg, err := ws.ReadMessage()
		if err != nil {
			break
		}
		var p map[string]interface{}
		json.Unmarshal(msg, &p)
		switch p["type"] {
		case "offer":
			sdp := p["sdp"].(string)
			pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdp})
			answer, _ := pc.CreateAnswer(nil)
			pc.SetLocalDescription(answer)
			<-webrtc.GatheringCompletePromise(pc)
			resp := map[string]string{"type": "answer", "sdp": pc.LocalDescription().SDP}
			b, _ := json.Marshal(resp)
			ws.WriteMessage(websocket.TextMessage, b)
		case "candidate":
			pc.AddICECandidate(webrtc.ICECandidateInit{Candidate: p["candidate"].(string)})
		}
	}
}

func startFFmpeg(rtspURL string) {
	cmd := exec.Command("ffmpeg",
		"-avioflags", "direct",
		"-flags", "low_delay",
		"-fflags", "+igndts+nobuffer",
		"-vsync", "0",
		"-rtsp_transport", "tcp",
		"-i", rtspURL,
		"-c:v", "copy", "-an",
		"-f", "h264",
		"pipe:1",
	)
	stdout, _ := cmd.StdoutPipe()
	// if err != nil {
	// 	log.Fatal(err)
	// }
	// if  err :=
	  cmd.Start();
	//   err != nil {
	// 	log.Fatal(err)
	// }

	rdr, _ := h264reader.NewReader(bufio.NewReaderSize(stdout, 4096)) // バッファサイズを小さくする
	dur := time.Second / 30

	for {
		nal, err := rdr.NextNAL()
		if err != nil {
			break
		}
		data := nal.Data

		// broadcast without logging per NALU
		trackMutex.RLock()
		for _, t := range tracks {
			t.WriteSample(media.Sample{Data: data, Duration: dur})
		}
		trackMutex.RUnlock()
	}
}
