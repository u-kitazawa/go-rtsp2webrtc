package main

import (
	"bytes" // 追加
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
		// グローバルな spsNAL と ppsNAL を直接参照
		currentSps := spsNAL
		currentPps := ppsNAL
		webcodecClientsMutex.RUnlock()

		if !sent && currentSps != nil && currentPps != nil {
			config := map[string]interface{}{
				"type": "codec",
				"sps":  currentSps,
				"pps":  currentPps,
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

	// グローバルなSPS/PPSを更新する試み
	// この処理は sendNALsToWebCodecClients の前に実行して、
	// sendNALsToWebCodecClients が最新のSPS/PPSを参照できるようにする
	var newSps, newPps []byte
	for _, nalData := range nals {
		if len(nalData) > 0 {
			nalType := nalData[0] & 0x1F
			if nalType == 7 { // SPS
				newSps = nalData
			} else if nalType == 8 { // PPS
				newPps = nalData
			}
		}
	}

	if newSps != nil || newPps != nil {
		webcodecClientsMutex.Lock()
		if newSps != nil {
			if len(spsNAL) == 0 || !bytes.Equal(spsNAL, newSps) {
				spsNAL = make([]byte, len(newSps))
				copy(spsNAL, newSps)
				log.Println("SPSを保存/更新しました (from writeNALsToTracks)")
			}
		}
		if newPps != nil {
			if len(ppsNAL) == 0 || !bytes.Equal(ppsNAL, newPps) {
				ppsNAL = make([]byte, len(newPps))
				copy(ppsNAL, newPps)
				log.Println("PPSを保存/更新しました (from writeNALsToTracks)")
			}
		}
		webcodecClientsMutex.Unlock()
	}

	// WebCodecsクライアントへの送信
	sendNALsToWebCodecClients(nals, duration)
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

		// SPS と PPS をグローバル変数に保存 (初回のみ)
		nalType := nal.Data[0] & 0x1F
		webcodecClientsMutex.Lock()
		// グローバル変数を更新
		if nalType == 7 { // SPS
			if len(spsNAL) == 0 || !bytes.Equal(spsNAL, nal.Data) { // 変更があった場合のみ更新
				spsNAL = make([]byte, len(nal.Data))
				copy(spsNAL, nal.Data)
				log.Println("SPSを保存/更新しました")
			}
		} else if nalType == 8 { // PPS
			if len(ppsNAL) == 0 || !bytes.Equal(ppsNAL, nal.Data) { // 変更があった場合のみ更新
				ppsNAL = make([]byte, len(nal.Data))
				copy(ppsNAL, nal.Data)
				log.Println("PPSを保存/更新しました")
			}
		}
		webcodecClientsMutex.Unlock()

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
	clientsToSendConfig := make([]*websocket.Conn, 0)
	var currentGlobalSps, currentGlobalPps []byte // 送信に使用するSPS/PPSを一時的に保持

	webcodecClientsMutex.RLock()
	// RLock中にグローバル変数のSPS/PPSをコピー
	if spsNAL != nil {
		currentGlobalSps = make([]byte, len(spsNAL))
		copy(currentGlobalSps, spsNAL)
	}
	if ppsNAL != nil {
		currentGlobalPps = make([]byte, len(ppsNAL))
		copy(currentGlobalPps, ppsNAL)
	}

	for wsClient, sentConfig := range codecConfigSentToWebcodecClients {
		// グローバルなSPS/PPSが利用可能で、まだこのクライアントに送信していなければリストに追加
		if !sentConfig && currentGlobalSps != nil && currentGlobalPps != nil {
			clientsToSendConfig = append(clientsToSendConfig, wsClient)
		}
	}
	webcodecClientsMutex.RUnlock()

	// SPS/PPSを送信 (RLockの外で)
	if len(clientsToSendConfig) > 0 && currentGlobalSps != nil && currentGlobalPps != nil {
		config := map[string]interface{}{
			"type": "codec",
			"sps":  currentGlobalSps,
			"pps":  currentGlobalPps,
		}
		configMessage, err := json.Marshal(config)
		if err != nil {
			log.Printf("コーデック設定のJSONマーシャリングエラー: %v", err)
		} else {
			for _, wsClient := range clientsToSendConfig {
				if err := wsClient.WriteMessage(websocket.BinaryMessage, configMessage); err != nil {
					log.Printf("WebCodecsクライアント %s へのコーデック設定送信エラー: %v. クライアントを削除します。", wsClient.RemoteAddr(), err)
					webcodecClientsMutex.Lock()
					delete(webcodecClients, wsClient)
					delete(codecConfigSentToWebcodecClients, wsClient)
					webcodecClientsMutex.Unlock()
					wsClient.Close()
				} else {
					log.Printf("SPS/PPSをWebCodecsクライアント %s に送信しました", wsClient.RemoteAddr())
					webcodecClientsMutex.Lock()
					codecConfigSentToWebcodecClients[wsClient] = true
					webcodecClientsMutex.Unlock()
				}
			}
		}
	}

	// 実際のビデオNALユニットを送信
	if len(nals) == 0 {
		return
	}

	clientsToSendData := make([]*websocket.Conn, 0)
	webcodecClientsMutex.RLock()
	for client, sentConfig := range codecConfigSentToWebcodecClients {
		// SPS/PPSが送信済みのクライアントにのみビデオデータを送る
		if sentConfig {
			// webcodecClientsマップに存在するか確認（既に閉じられて削除された可能性があるため）
			if _, ok := webcodecClients[client]; ok {
				clientsToSendData = append(clientsToSendData, client)
			}
		}
	}
	webcodecClientsMutex.RUnlock()

	if len(clientsToSendData) == 0 {
		return
	}

	for _, nalData := range nals {
		if len(nalData) == 0 {
			log.Println("WebCodecs: Skipping empty nalData")
			continue
		}

		// Determine NALU type from the raw NAL data (nalData[0])
		// This is the NALU *before* the Annex B start code is prepended.
		naluHeaderByte := nalData[0]
		naluType := naluHeaderByte & 0x1F // Lower 5 bits

		// Filter for decodable NALU types for video frames.
		// SPS (7) and PPS (8) are handled by the 'codec' message.
		// We should only send video slice data (type 1 or 5) here.
		if naluType != 1 && naluType != 5 {
			// log.Printf("WebCodecs: Skipping NAL type %d (Header Byte: 0x%02x) for video data message", naluType, naluHeaderByte)
			continue // Skip NALUs that are not video slices
		}

		// Log details about the NALU being sent
		// log.Printf("WebCodecs: Sending NAL. Original Length: %d, Type: %d (Header Byte: 0x%02x)", len(nalData), naluType, naluHeaderByte)

		// Annex-Bスタートコードを付加
		annexBData := append([]byte{0x00, 0x00, 0x00, 0x01}, nalData...)

		videoDataMsg := map[string]interface{}{
			"type":     "video",
			"data":     annexBData, // Annex B 形式のデータ (JSONでBase64エンコードされる)
			"duration": duration.Microseconds(), // EncodedVideoChunkのdurationはマイクロ秒
		}
		message, err := json.Marshal(videoDataMsg)
		if err != nil {
			log.Printf("ビデオデータのJSONマーシャリングエラー: %v", err)
			continue
		}

		for _, wsClient := range clientsToSendData {
			// log.Printf("WebCodecsクライアント %s にAnnex B NALを送信中 (サイズ: %d)", wsClient.RemoteAddr(), len(annexBData))
			if err := wsClient.WriteMessage(websocket.BinaryMessage, message); err != nil {
				// log.Printf("WebCodecsクライアント %s へのNAL送信エラー: %v。クライアントを削除します。", wsClient.RemoteAddr(), err)
				// エラーが発生したクライアントは能動的に切断・削除する
				// このクライアントがclientsToSendDataに含まれている場合のみ処理
				isStillClient := false
				webcodecClientsMutex.RLock()
				_, isStillClient = webcodecClients[wsClient]
				webcodecClientsMutex.RUnlock()

				if isStillClient {
					webcodecClientsMutex.Lock()
					delete(webcodecClients, wsClient)
					delete(codecConfigSentToWebcodecClients, wsClient)
					webcodecClientsMutex.Unlock()
					wsClient.Close() // エラー発生時は接続を閉じる
				}
			}
		}
	}
}
