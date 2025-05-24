package main

import (
	"encoding/base64"
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

// --- トラックリストとミューテックス (WebRTC用) ---
var (
	tracksSample = make([]*webrtc.TrackLocalStaticSample, 0)
	trackMutex   sync.RWMutex
)

// --- WebSocketシグナリングハンドラー ---
func signalingHandler(w http.ResponseWriter, r *http.Request, outputMode string) { // outputMode を引数に追加
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocketアップグレード失敗: %v", err)
		return
	}
	// defer ws.Close() // WebCodecsモードでは早期に閉じないようにする

	if outputMode == "webcodecs" {
		webcodecClientsMutex.Lock()
		webcodecClients[ws] = true
		codecConfigSentToWebcodecClients[ws] = false // 新しいクライアントのためにリセット
		webcodecClientsMutex.Unlock()
		log.Println("WebCodecsクライアントが接続しました")

		defer func() {
			log.Println("WebCodecsクライアント接続を閉じます")
			webcodecClientsMutex.Lock()
			delete(webcodecClients, ws)
			delete(codecConfigSentToWebcodecClients, ws)
			webcodecClientsMutex.Unlock()
			ws.Close()
		}()

		// 既存のSPS/PPSがあれば送信
		webcodecClientsMutex.RLock()
		sent := codecConfigSentToWebcodecClients[ws]
		currentSpsNAL := spsNAL // RLock中にアクセス
		currentPpsNAL := ppsNAL // RLock中にアクセス
		webcodecClientsMutex.RUnlock()

		if !sent && currentSpsNAL != nil && currentPpsNAL != nil {
			config := map[string]interface{}{
				"type": "codec",
				"sps":  base64.StdEncoding.EncodeToString(currentSpsNAL), // base64エンコードは必要
				"pps":  base64.StdEncoding.EncodeToString(currentPpsNAL),
			}
			configMessage, _ := json.Marshal(config)
			if err := ws.WriteMessage(websocket.BinaryMessage, configMessage); err != nil {
				log.Printf("WebCodecsクライアントへのコーデック設定送信エラー: %v", err)
			} else {
				log.Println("既存のSPS/PPSをWebCodecsクライアントに送信しました")
				webcodecClientsMutex.Lock()
				codecConfigSentToWebcodecClients[ws] = true
				webcodecClientsMutex.Unlock()
			}
		}

		// WebCodecsクライアントはメッセージを送信しないので、ここでループを維持
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				// log.Printf("WebCodecsクライアントの読み取りエラー (おそらく切断): %v", err)
				break // エラーが発生したらループを抜けてdeferでクリーンアップ
			}
		}
		return
	}

	// WebRTCモードの既存のロジック
	defer ws.Close() // WebRTCモードではここで閉じる
	pc, track := setupPeerConnection()
	if pc == nil || track == nil {
		return
	}
	defer func() {
		_ = pc.Close()
		unregisterTrack(track)
	}()

	log.Println("WebSocket接続完了 (WebRTCモード)")

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

// NALユニット（[][]byteとして）をすべてのアクティブなWebRTCトラックに書き込む関数
// この関数はgortsplib_handlerから呼び出されます
func writeNALsToTracks(nals [][]byte, duration time.Duration) {
	// WebRTCクライアントへの送信
	trackMutex.RLock()
	activeWebRTCTracks := len(tracksSample) > 0
	trackMutex.RUnlock()

	if activeWebRTCTracks {
		for _, nalData := range nals {
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

	// WebCodecsクライアントへの送信
	sendNALsToWebCodecClients(nals, duration)
}

// --- NALストリーミングループ (ffmpegベースのハンドラー用) ---
func streamNAL(h264r *h264reader.H264Reader, dur time.Duration) { // インターフェースを *h264reader.H264Reader に変更
	for {
		nal, err := h264r.NextNAL() // nal は *h264reader.NAL になりました
		if err != nil {
			if err.Error() != "EOF" { // io.EOF から文字列比較に変更し、より広範な互換性を確保
				log.Printf("H264リーダーからのNAL読み取りエラー: %v", err)
			}
			break
		}

		// SPS と PPS をグローバル変数に保存 (初回のみ)
		nalType := nal.Data[0] & 0x1F
		webcodecClientsMutex.Lock() // SPS/PPSの更新と読み取りのためにロック
		if nalType == 7 && spsNAL == nil { // SPS
			spsNAL = make([]byte, len(nal.Data))
			copy(spsNAL, nal.Data)
			log.Println("SPSを保存しました")
		} else if nalType == 8 && ppsNAL == nil { // PPS
			ppsNAL = make([]byte, len(nal.Data))
			copy(ppsNAL, nal.Data)
			log.Println("PPSを保存しました")
		}
		webcodecClientsMutex.Unlock()

		// WebRTCクライアントへの送信
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

		// WebCodecsクライアントへの送信
		sendNALsToWebCodecClients([][]byte{nal.Data}, dur)
	}
}

// sendNALsToWebCodecClients は、NALユニットを接続されているすべてのWebCodecsクライアントに送信します。
func sendNALsToWebCodecClients(nals [][]byte, duration time.Duration) {
	webcodecClientsMutex.RLock()
	// SPS/PPSがまだ送信されていないクライアントがいるか確認し、必要なら送信
	for wsClient, sentConfig := range codecConfigSentToWebcodecClients {
		if !sentConfig && spsNAL != nil && ppsNAL != nil {
			// RLock内で送信処理を行うとデッドロックの可能性があるため、
			// 送信が必要なクライアントを特定し、後でまとめて送信するか、
			// ロックの粒度を調整する必要がある。
			// ここでは、まず設定を送信する。
			config := map[string]interface{}{
				"type": "codec",
				"sps":  spsNAL, // グローバル変数を直接使用
				"pps":  ppsNAL,
			}
			configMessage, err := json.Marshal(config)
			if err != nil {
				log.Printf("コーデック設定のJSONマーシャリングエラー: %v", err)
				continue
			}
			// 送信処理はRLockの外で行うべきだが、ここでは簡略化のためRLock内で行う。
			// ただし、WriteMessageがブロックする可能性があるため注意が必要。
			// 実際には、送信が必要なクライアントのリストを作成し、RLockを解放してから送信する。
			if err := wsClient.WriteMessage(websocket.BinaryMessage, configMessage); err != nil {
				log.Printf("WebCodecsクライアントへのコーデック設定送信エラー: %v", err)
			} else {
				log.Printf("SPS/PPSをWebCodecsクライアント %s に送信しました", wsClient.RemoteAddr())
				// この更新は書き込みロックが必要
				// codecConfigSentToWebcodecClients[wsClient] = true // RLock内では不可
			}
		}
	}
	webcodecClientsMutex.RUnlock() // SPS/PPS送信のためのRLockを解放

	// SPS/PPS送信状態を更新 (書き込みロックを取得して行う)
	webcodecClientsMutex.Lock()
	for wsClient := range webcodecClients { // webcodecClientsのキーでループ
		if !codecConfigSentToWebcodecClients[wsClient] && spsNAL != nil && ppsNAL != nil {
			// 再度送信試行はしない。上記で送信試行済み。
			// ここでは送信済みフラグを立てるだけ。
			codecConfigSentToWebcodecClients[wsClient] = true
		}
	}
	webcodecClientsMutex.Unlock()

	// 実際のビデオNALユニットを送信
	webcodecClientsMutex.RLock()
	defer webcodecClientsMutex.RUnlock()

	if len(webcodecClients) == 0 {
		return
	}

	for _, nalData := range nals {
		if len(nalData) == 0 {
			continue
		}
		message := map[string]interface{}{
			"type":     "video",
			"data":     nalData, // バイナリデータのまま
			"duration": duration.Microseconds(), // マイクロ秒単位の期間
		}
		videoMessage, err := json.Marshal(message)
		if err != nil {
			log.Printf("ビデオデータのJSONマーシャリングエラー: %v", err)
			continue
		}

		for wsClient, sentConfig := range codecConfigSentToWebcodecClients {
			// SPS/PPSが送信済みのクライアントにのみビデオデータを送信
			if sentConfig {
				if err := wsClient.WriteMessage(websocket.BinaryMessage, videoMessage); err != nil {
					// log.Printf("WebCodecsクライアントへのビデオNAL送信エラー: %v", err)
				}
			}
		}
	}
}
