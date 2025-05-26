package main

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
	"github.com/pion/webrtc/v3/pkg/media/h264reader"
)

// --- WebSocketアップグレーダー ---
var upgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

// --- コーデック設定関数 ---
func setCurrentCodec(codec string) {
	currentCodec = codec
	log.Printf("WebRTC出力コーデックを %s に設定しました", codec)
}

// --- トラックリストとミューテックス (WebRTC用) ---
var (
	tracksSample     = make([]*webrtc.TrackLocalStaticSample, 0)
	tracksSampleH265 = make([]*webrtc.TrackLocalStaticSample, 0)
	trackMutex       sync.RWMutex
	currentCodec     string = "h264" // デフォルトはH.264
)

// --- WebSocketシグナリングハンドラー ---
func signalingHandler(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocketアップグレード失敗: %v", err)
		return
	}
	defer ws.Close()
	
	var pc *webrtc.PeerConnection
	var track *webrtc.TrackLocalStaticSample
	
	// 現在のコーデックに応じて適切なPeerConnectionとトラックを設定
	if currentCodec == "h265" {
		pc, track = setupPeerConnectionH265()
	} else {
		pc, track = setupPeerConnection()
	}
	
	if pc == nil || track == nil {
		return
	}
	
	defer func() {
		_ = pc.Close()
		if currentCodec == "h265" {
			unregisterTrackH265(track)
		} else {
			unregisterTrack(track)
		}
	}()

	log.Printf("WebSocket接続完了 (WebRTCモード - %s)", currentCodec)

	for {
		_, msg, err := ws.ReadMessage()
		if err != nil {
			log.Printf("WebSocket読み取りエラー: %v", err)
			break
		}
		var p map[string]interface{}
		if err := json.Unmarshal(msg, &p); err != nil {
			log.Printf("無効なWebSocketメッセージ: %v", err)
			continue
		}
		switch p["type"] {
		case "offer":
			offer := webrtc.SessionDescription{
				Type: webrtc.SDPTypeOffer,
				SDP:  p["sdp"].(string),
			}
			if err := pc.SetRemoteDescription(offer); err != nil {
				log.Printf("リモートディスクリプションの設定失敗: %v", err)
				continue
			}

			answer, err := pc.CreateAnswer(nil)
			if err != nil {
				log.Printf("アンサーの作成失敗: %v", err)
				continue
			}
			if err := pc.SetLocalDescription(answer); err != nil {
				log.Printf("ローカルディスクリプションの設定失敗: %v", err)
				continue
			}

			// ICE候補の収集が完了するのを待つ
			<-webrtc.GatheringCompletePromise(pc)

			// クライアントにアンサーを送信
			response := map[string]string{"type": "answer", "sdp": pc.LocalDescription().SDP}
			if err := ws.WriteJSON(response); err != nil {
				log.Printf("アンサーの送信失敗: %v", err)
			}

		case "candidate":
			candidateMap, ok := p["candidate"].(map[string]interface{})
			if !ok {
				log.Printf("無効な候補メッセージ")
				continue
			}
			sdpMid := candidateMap["sdpMid"].(string)
			candidate := webrtc.ICECandidateInit{
				Candidate:     candidateMap["candidate"].(string),
				SDPMid:        &sdpMid,
				SDPMLineIndex: func() *uint16 { u := uint16(candidateMap["sdpMLineIndex"].(float64)); return &u }(),
			}
			if err := pc.AddICECandidate(candidate); err != nil {
				log.Printf("ICE候補の追加失敗: %v", err)
			}
		}
	}
	log.Println("WebSocket切断")
}

// --- PeerConnectionとトラックのセットアップ (WebRTC用) ---
func setupPeerConnection() (*webrtc.PeerConnection, *webrtc.TrackLocalStaticSample) {
	m := &webrtc.MediaEngine{}
	_ = m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH264,
			ClockRate:   90000,
			SDPFmtpLine: "profile-level-id=42e01f;level-asymmetry-allowed=1;packetization-mode=1",
		},
		PayloadType: 96,
	}, webrtc.RTPCodecTypeVideo)
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m))

	pc, err := api.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}},
	})
	if err != nil {
		log.Printf("PeerConnection作成失敗: %v", err)
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

// --- PeerConnectionとH.265トラックのセットアップ (WebRTC用) ---
func setupPeerConnectionH265() (*webrtc.PeerConnection, *webrtc.TrackLocalStaticSample) {
	m := &webrtc.MediaEngine{}
	// H.265コーデックの登録
	_ = m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH265,
			ClockRate:   90000,
			SDPFmtpLine: "profile-id=1",
		},
		PayloadType: 97,
	}, webrtc.RTPCodecTypeVideo)
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m))

	pc, err := api.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}},
	})
	if err != nil {
		log.Printf("H.265 PeerConnection作成失敗: %v", err)
		return nil, nil
	}

	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH265, ClockRate: 90000}, "video", "pion")
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

	registerTrackH265(track)
	return pc, track
}

// --- トラック管理 (WebRTC用) ---
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

// --- H.265トラック管理 (WebRTC用) ---
func registerTrackH265(t *webrtc.TrackLocalStaticSample) {
	trackMutex.Lock()
	defer trackMutex.Unlock()
	tracksSampleH265 = append(tracksSampleH265, t)
}
func unregisterTrackH265(t *webrtc.TrackLocalStaticSample) {
	trackMutex.Lock()
	defer trackMutex.Unlock()
	for i, tr := range tracksSampleH265 {
		if tr == t {
			tracksSampleH265 = append(tracksSampleH265[:i], tracksSampleH265[i+1:]...)
			break
		}
	}
}

// NALユニット（[][]byteとして）をすべてのアクティブなWebRTCトラックに書き込む関数
// この関数はgortsplib_handlerから呼び出されます
func writeNALsToTracks(nals [][]byte, duration time.Duration) {
	// WebRTCクライアントへの送信
	trackMutex.RLock()
	activeWebRTCTracks := len(tracksSample) > 0
	trackMutex.RUnlock()

	if activeWebRTCTracks {
		for  _, nalData := range nals {
			if len(nalData) == 0 {
				continue // 空のNALユニットをスキップ
			}
			// 各NALユニットにAnnex-Bスタートコード（0x00000001）を付加
			sample := media.Sample{
				Data:     append([]byte{0x00, 0x00, 0x00, 0x01}, nalData...),
				Duration: duration,
			}

			trackMutex.RLock()
			for _, t := range tracksSample {
				if err := t.WriteSample(sample); err != nil {
					// エラー処理: log.Printf("WebRTCトラックへのサンプル書き込みエラー: %v", err)
				}
			}
			trackMutex.RUnlock()
		}
	}
}

// H.265 NALユニット（[][]byteとして）をすべてのアクティブなH.265 WebRTCトラックに書き込む関数
// この関数はgortsplib_handlerのH.265パススルー機能から呼び出されます
func writeNALsToTracksH265(nals [][]byte, duration time.Duration) {
	// H.265 WebRTCクライアントへの送信
	trackMutex.RLock()
	activeWebRTCTracks := len(tracksSampleH265) > 0
	trackMutex.RUnlock()

	if activeWebRTCTracks {
		for  _, nalData := range nals {
			if len(nalData) == 0 {
				continue // 空のNALユニットをスキップ
			}
			// 各NALユニットにAnnex-Bスタートコード（0x00000001）を付加
			sample := media.Sample{
				Data:     append([]byte{0x00, 0x00, 0x00, 0x01}, nalData...),
				Duration: duration,
			}

			trackMutex.RLock()
			for _, t := range tracksSampleH265 {
				if err := t.WriteSample(sample); err != nil {
					// エラー処理: log.Printf("H.265 WebRTCトラックへのサンプル書き込みエラー: %v", err)
				}
			}
			trackMutex.RUnlock()
		}
	}
}

// --- NALストリーミングループ (ffmpegベースのハンドラー用) ---
func streamNAL(h264r *h264reader.H264Reader, dur time.Duration) {
	for {
		nal, err := h264r.NextNAL()
		if err != nil {
			if err.Error() != "EOF" { // io.EOF から文字列比較に変更し、より広範な互換性を確保
				log.Printf("H264リーダーからのNAL読み取りエラー: %v", err)
			}
			break
		}

		sample := media.Sample{
			Data:     append([]byte{0x00, 0x00, 0x00, 0x01}, nal.Data...), // Annex-B
			Duration: dur,
		}
		trackMutex.RLock()
		if len(tracksSample) > 0 {
			for _, t := range tracksSample {
				if err := t.WriteSample(sample); err != nil {
					// log.Printf("WebRTCトラックへのサンプル書き込みエラー: %v", err)
				}
			}
		}
		trackMutex.RUnlock()
	}
}


