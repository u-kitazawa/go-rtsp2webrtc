package main

import (
	"flag"
	"log"
	"net/http"
	"sync" // 追加

	"github.com/gorilla/websocket" // 追加
)

// --- トラックリストとミューテックス ---
var (
	inputURL     string // RTSP URL または RTP SDP ファイルパス
	serverPort   string
	codec        string // "h264" または "h265"
	processor    string // H.265 トランスコーディング用の "cpu" または "gpu"
	inputType    string // "rtsp" または "rtp" または "server"
	useGortsplib string // gortsplib パススルー用の "true" または "false"
	outputMode   string // "webrtc" または "webcodecs" を追加

	webcodecClientsMutex sync.RWMutex // 追加
	webcodecClients      = make(map[*websocket.Conn]bool) // 追加

	// SPS/PPSを一度受信したら保存し、新しいwebcodecクライアントに送信するため
	spsNAL []byte // 追加
	ppsNAL []byte // 追加
	// codecConfigSentToWebcodecClients は、特定のクライアントに設定が送信されたかどうかを追跡します
	codecConfigSentToWebcodecClients = make(map[*websocket.Conn]bool) // 追加
)

type props struct {
	codec      string
	serverPort string
	processor  string
	inputType  string
	inputURL   string
	outputMode string // 追加
	fps 			int // 追加: フレームレートを追加
}

func main() {	flag.StringVar(&inputURL, "input-url", "", "カメラのRTSP URLまたはRTP SDPファイルパス (server モードでは不要)")
	flag.StringVar(&serverPort, "port", "8080", "サーバーポート")
	flag.StringVar(&codec, "codec", "h264", "入力に使用するコーデック (h264 または h265)")
	flag.StringVar(&processor, "processor", "gpu", "H.265トランスコーディングに使用するプロセッサ (cpu または gpu)")
	flag.StringVar(&inputType, "input-type", "rtsp", "入力タイプ (rtsp, rtp, server)")
	flag.StringVar(&useGortsplib, "use-gortsplib", "false", "RTSPパススルーにgortsplibを使用する (true または false)")
	flag.StringVar(&outputMode, "output-mode", "webrtc", "出力モード: webrtc または webcodecs") // 追加
	flag.Parse()

	// サーバーモードの場合は入力URLチェックをスキップ
	if inputType != "server" && inputURL == "" {
		log.Fatal("入力URL（RTSPまたはRTP SDPファイル）を指定する必要があります。現在の入力タイプ: ", inputType)
	}
	
	if inputType == "server" {
		log.Printf("入力タイプ: %s, コーデック: %s, 出力モード: %s を使用します (RTSPサーバーモード)", inputType, codec, outputMode)
	} else {
		log.Printf("入力URL: %s, 入力タイプ: %s, コーデック: %s, 出力モード: %s を使用します", inputURL, inputType, codec, outputMode)
	}
	if codec == "h265" {
		log.Printf("H.265トランスコーディングプロセッサ: %s", processor)
	}
	props := props{
		codec:      codec,
		serverPort: serverPort,
		processor:  processor,
		inputType:  inputType,
		inputURL:   inputURL,
		outputMode: outputMode, // 追加
		fps:        30, // デフォルトのフレームレートを設定 (必要に応じて変更可能)
	}
	
	if useGortsplib == "true" { // 修正: 文字列比較を正しく行う
		log.Println("RTSPパススルーまたはトランスコーディングにgortsplibベースのハンドラーを使用します")
		switch inputType {
		case "server":
			log.Println("RTSPサーバーモードでgortsplibベースのサーバーを起動します")
			switch codec {			case "h264":
				go startGortsplibH264RTSPServer(props)
			case "h265":
				go startGortsplibH265RTSPServer(props)
			default:
				log.Fatalf("サポートされていないコーデック: %s。'h264' または 'h265' を使用してください。", codec)
			}
		default:
			switch codec {
			case "h264":
				go startGortsplibH264RTSP(props)
			case "h265":
				log.Println("gortsplibを使用してH.265をH.264にトランスコードし、WebRTCにストリーミングします")
				go startGortsplibH265toH264RTSP(props) // H.265用gortsplibハンドラーを呼び出す
			default:
				log.Fatalf("gortsplibは現在H.264およびH.265 (->H.264トランスコード) のみをサポートしています。指定されたコーデック: %s", codec)
			}
		}
	} else {
		// 既存のffmpegベースのロジック (useGortsplib が "false" の場合)
		log.Println("従来のffmpegベースのハンドラーを使用します")
		switch inputType {
		case "server":
			log.Fatal("サーバーモードはgortsplibが必要です。-use-gortsplib=true を指定してください")
		case "rtsp":
			switch codec {
			case "h264":
				go startFFmpegH264RTSP(props.inputURL)
			case "h265":
				switch processor {
				case "gpu":
					go startFFmpegH265ToH264NALGPURTSP(props.inputURL)
				case "cpu":
					go startFFmpegH265ToH264NALCPURTSP(props.inputURL)
				default:
					log.Fatalf("H.265のサポートされていないプロセッサ: %s。'cpu' または 'gpu' を使用してください。", processor)
				}
			default:
				log.Fatalf("RTSPのサポートされていないコーデック: %s", codec)
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
					log.Fatalf("H.265のサポートされていないプロセッサ: %s。'cpu' または 'gpu' を使用してください。", processor)
				}
			default:
				log.Fatalf("RTPのサポートされていないコーデック: %s", codec)
			}
		default:
			log.Fatalf("サポートされていない入力タイプ: %s。'rtsp' または 'rtp' を使用してください。", inputType)
		}
	}

	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) { // outputModeをハンドラに渡すためにクロージャを使用
		signalingHandler(w, r, props.outputMode) // props.outputMode を使用
	})
	log.Printf("サーバーが :%s で起動しました", serverPort)
	log.Fatal(http.ListenAndServe(":"+serverPort, nil))
}

