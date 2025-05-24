package main

import (
	"bufio"
	"encoding/base64"
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
	"github.com/pion/webrtc/v3/pkg/media/h264reader"
)

// WebSocketアップグレーダー
var upgrader = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

var (
	currentVideoDataChannel *webrtc.DataChannel
	dataChannelMutex        sync.RWMutex
	rtspURL	string
	serverPort	string
)


func main() {
	flag.StringVar(&rtspURL, "rtsp-url", "rtsp://admin:admin@192.168.40.118:1935", "カメラのRTSP URL")
	flag.StringVar(&serverPort, "port", "8080", "サーバーポート")
	flag.Parse()

	if rtspURL == "" {
		log.Fatal("RTSP URLは -rtsp-url フラグで指定する必要があります")
	}

	log.Printf("RTSP URLを使用: %s", rtspURL)
	go startFFmpeg(rtspURL)

	http.HandleFunc("/ws", signalingHandler)
	log.Printf("サーバーが :%s で起動しました", serverPort)
	log.Fatal(http.ListenAndServe(":"+serverPort, nil))
}

func signalingHandler(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocketへのアップグレードに失敗しました: %v", err)
		return
	}
	defer func() {
		log.Println("deferでWebSocket接続を閉じています。")
		ws.Close()
	}()

	// PeerConnectionのセットアップ
	m := &webrtc.MediaEngine{}
	m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000, SDPFmtpLine: "profile-level-id=42e01f;level-asymmetry-allowed=1;packetization-mode=1"},
		PayloadType:        96,
	}, webrtc.RTPCodecTypeVideo)
	api := webrtc.NewAPI(webrtc.WithMediaEngine(m))
	pc, err := api.NewPeerConnection(webrtc.Configuration{ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}}})
	if err != nil {
		log.Printf("PeerConnectionの作成に失敗しました: %v", err)
		return
	}
	defer func() {
		log.Println("deferでPeerConnectionを閉じています。")
		if pc != nil {
			if err := pc.Close(); err != nil {
				log.Printf("PeerConnectionを閉じる際にエラーが発生しました: %v", err)
			}
		}
	}()

	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Printf("PeerConnection ICE接続状態が変更されました: %s\\n", state.String())
		if state == webrtc.ICEConnectionStateFailed || state == webrtc.ICEConnectionStateDisconnected || state == webrtc.ICEConnectionStateClosed {
			log.Printf("ICE接続状態が%sのため、WebSocketは閉じるか、閉じられる可能性があります。", state.String())
		}
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("PeerConnection状態が変更されました: %s\\n", state.String())
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateDisconnected || state == webrtc.PeerConnectionStateClosed {
			log.Printf("PeerConnection状態が%sのため、WebSocketは閉じるか、閉じられる可能性があります。", state.String())
		}
	})

	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		log.Printf("新しいDataChannelが検出されました: %s - ID: %d\\n", dc.Label(), *dc.ID())

		if dc.Label() != "videoNaluChannel" {
			log.Printf("DataChannel %s は 'videoNaluChannel' ではないため、無視されます。", dc.Label())
			return
		}

		dc.OnOpen(func() {
			log.Printf("DataChannel '%s'-'%d' が開きました。ビデオデータの送信準備ができました。\\n", dc.Label(), *dc.ID())
			dataChannelMutex.Lock()
			currentVideoDataChannel = dc
			dataChannelMutex.Unlock()
		})

		// チャネルクローズ処理の登録
		dc.OnClose(func() {
			log.Printf("DataChannel '%s'-'%d' が閉じました。\\n", dc.Label(), *dc.ID())
			dataChannelMutex.Lock()
			// currentVideoDataChannelがこのチャネルである場合はクリア
			if currentVideoDataChannel != nil && currentVideoDataChannel.ID() != nil && dc.ID() != nil &&
				*currentVideoDataChannel.ID() == *dc.ID() {
				currentVideoDataChannel = nil
				log.Printf("currentVideoDataChannelが%s-%dのためnilに設定されました", dc.Label(), *dc.ID())
			}
			dataChannelMutex.Unlock()
		})

		dc.OnError(func(err error) {
			log.Printf("DataChannel '%s'-'%d' エラー: %v\\n", dc.Label(), *dc.ID(), err)
		})

		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			// クライアントからのメッセージは想定していません
			log.Printf("DataChannelからメッセージを受信しました: %s", string(msg.Data))
		})
	})


	// WebSocketメッセージループ
	for {
		_, msg, err := ws.ReadMessage()
		if err != nil {
			log.Printf("WebSocketからのメッセージ読み取り中にエラーが発生しました: %v。WebSocketを閉じます。", err)
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure, websocket.CloseNormalClosure) {
				log.Printf("WebSocketが予期せず閉じられました: %v", err)
			}
			break
		}
		var p map[string]interface{}
		if err := json.Unmarshal(msg, &p); err != nil {
			log.Printf("メッセージのデシリアライズ中にエラーが発生しました: %v。メッセージ: %s", err, string(msg))
			continue 
		}

		switch p["type"] {
		case "offer":
			sdp, ok := p["sdp"].(string)
			if !ok {
				log.Printf("無効なオファー形式: sdpが文字列ではありません。ペイロード: %v", p)
				continue
			}
			log.Println("オファーを受信しました")
			if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdp}); err != nil {
				log.Printf("リモート記述の設定に失敗しました（オファー）: %v", err)
				continue
			}
			
			answer, err := pc.CreateAnswer(nil)
			if err != nil {
				log.Printf("アンサーの作成に失敗しました: %v", err)
				continue
			}
			
			if err := pc.SetLocalDescription(answer); err != nil {
				log.Printf("ローカル記述の設定に失敗しました（アンサー）: %v", err)
				continue
			}
			
			<-webrtc.GatheringCompletePromise(pc)
			log.Println("ICE収集完了。アンサーを送信します。") 

			localDesc := pc.LocalDescription()
			if localDesc == nil {
				log.Println("ICE収集後にローカル記述がnilです。")
				continue
			}
			
			resp := map[string]string{"type": "answer", "sdp": localDesc.SDP}
			b, err := json.Marshal(resp)
			if err != nil {
				log.Printf("アンサーのシリアライズ中にエラーが発生しました: %v", err)
				continue
			}
			
			if err := ws.WriteMessage(websocket.TextMessage, b); err != nil {
				log.Printf("WebSocketへのアンサー書き込み中にエラーが発生しました: %v", err)
				break 
			}
		case "candidate":
			candidateStr, ok := p["candidate"].(string)
			if !ok {
				log.Printf("無効な候補形式: candidateが文字列ではありません。ペイロード: %v", p)
				continue
			}
			log.Println("ICE候補を受信しました") 
			if err := pc.AddICECandidate(webrtc.ICECandidateInit{Candidate: candidateStr}); err != nil {
				log.Printf("ICE候補の追加に失敗しました: %v", err)
			}
		default:
			log.Printf("未知のメッセージタイプを受信しました: %s", p["type"])
		}
	}
	log.Println("signalingHandlerを終了します。")
}

func startFFmpeg(rtspURL string) {
	log.Printf("RTSP URLに対してFFmpegを開始します: %s", rtspURL)
	cmd := exec.Command("ffmpeg",
		"-hide_banner", // オプション: FFmpeg起動ログをクリーンアップします
		"-avioflags", "direct",
		"-flags", "low_delay",
		"-fflags", "+igndts+nobuffer",
		"-vsync", "0", // 必要に応じて -vsync passthrough または -vsync cfr/vfr も可
		"-rtsp_transport", "tcp",
		"-i", rtspURL,
		"-tune", "zerolatency",
		// 重要: -c:v copy は、RTSPソースが既にクライアントのVideoDecoderと互換性のあるプロファイル（例: Constrained Baseline@L3.1の場合はprofile-level-id=42e01f）のH.264であることを前提としています。
		// ソースが異なるプロファイル（例: MainまたはHigh）の場合、VideoDecoderが失敗する可能性があります。
		// 互換性のあるプロファイルに強制的に再エンコードする場合（遅延とCPU負荷が増加します）:
		// "-c:v", "libx264", "-profile:v", "baseline", "-level:v", "3.1", "-preset", "ultrafast", "-tune", "zerolatency",
		// "-pix_fmt", "yuv420p", // H.264でしばしば必要
		"-c:v", "copy",
		"-an", // 音声なし
		"-f", "h264", // 生のH.264（Annex B形式）を出力
		"pipe:1",     // 標準出力に出力
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("FFmpegの標準出力パイプ作成中にエラーが発生しました: %v", err)
		return
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		log.Printf("FFmpegの標準エラーパイプ作成中にエラーが発生しました: %v", err)
		return
	}

	if err := cmd.Start(); err != nil {
		log.Printf("FFmpegの起動中にエラーが発生しました: %v", err)
		return
	}
	log.Println("FFmpegプロセスが開始されました。")

	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			log.Printf("FFmpeg stderr: %s", scanner.Text())
		}
	}()

	h264BufReader := bufio.NewReaderSize(stdout, 1024*64) // バッファサイズを大きくする
	rdr, err := h264reader.NewReader(h264BufReader)
	if err != nil {
		log.Printf("H264リーダー作成中にエラーが発生しました: %v", err)
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		cmd.Wait() 
		return
	}

	log.Println("FFmpegからH.264 NALUを読み込んでいます...")
	for {
		nal, err := rdr.NextNAL()
		if err != nil {
			if err == io.EOF {
				log.Println("FFmpegストリームが終了しました（EOF）。")
			} else {
				log.Printf("H264リーダーからNAL読み込み中にエラーが発生しました: %v\\n", err)
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
			log.Printf("NALUペイロードのシリアライズ中にエラーが発生しました: %v", err)
			continue
		}

		dataChannelMutex.RLock()
		dc := currentVideoDataChannel
		dataChannelMutex.RUnlock()

		if dc != nil && dc.ReadyState() == webrtc.DataChannelStateOpen {

		
			err := dc.SendText(string(jsonData))
			if err != nil {
				log.Printf("DataChannelにNALUデータを書き込む際にエラーが発生しました（タイプ: %s）: %v", naluType, err)
			}
		} else {
			time.Sleep(2 * time.Millisecond)
		}
	}

	log.Println("H.264 NALUの読み込みを停止しました。")
	if cmd.Process != nil {
		log.Println("FFmpegプロセスの終了を待っています...")
		if err := cmd.Wait(); err != nil { 
			log.Printf("FFmpegプロセスがエラーで終了しました: %v", err)
		} else {
			log.Println("FFmpegプロセスが正常に終了しました。")
		}
	} else {
		log.Println("FFmpegプロセスは開始されていないか、既にクリーンアップされています。")
	}
}
