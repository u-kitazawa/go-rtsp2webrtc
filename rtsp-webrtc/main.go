package main

import (
	"flag"
	"log"
	"net/http"
)

// --- トラックリストとミューテックス ---
var (
	inputURL       string // RTSP URL または RTP SDP ファイルパス
	serverPort     string
	codec          string // "h264" または "h265" (入力コーデック)
	outputCodec    string // "h264" または "h265" (出力コーデック、H.265入力時のみ使用)
	processor      string // H.265 トランスコーディング用の "cpu" または "gpu"
	inputType      string // "rtsp" または "rtp" または "server" または "rtp-server"
	useGortsplib   string // gortsplib パススルー用の "true" または "false"
	rtpServerAddr  string // RTP サーバーのリスニングアドレス
)

type props struct {
	codec       string
	outputCodec string
	serverPort  string
	processor   string
	inputType   string
	inputURL    string
	fps         int // 追加: フレームレートを追加
}

func main() {
	flag.StringVar(&inputURL, "input-url", "", "カメラのRTSP URLまたはRTP SDPファイルパス (server モードでは不要)")
	flag.StringVar(&serverPort, "port", "8080", "サーバーポート")
	flag.StringVar(&codec, "codec", "h264", "入力に使用するコーデック (h264 または h265)")
	flag.StringVar(&outputCodec, "output-codec", "h264", "出力コーデック (h264 または h265) - H.265入力時のみ有効")
	flag.StringVar(&processor, "processor", "cpu", "H.265トランスコーディングに使用するプロセッサ (cpu または gpu)")
	flag.StringVar(&inputType, "input-type", "rtsp", "入力タイプ (rtsp, rtp, server, rtp-server)")	
	flag.StringVar(&useGortsplib, "use-gortsplib", "false", "RTSPパススルーにgortsplibを使用する (true または false)")
	flag.StringVar(&rtpServerAddr, "rtp-server-addr", ":5004", "RTPサーバーのリスニングアドレス (rtp-server モード時のみ)")
	flag.Parse()
	// サーバーモードの場合は入力URLチェックをスキップ
	if inputType != "server" && inputType != "rtp-server" && inputURL == "" {
		log.Fatal("入力URL（RTSPまたはRTP SDPファイル）を指定する必要があります。現在の入力タイプ: ", inputType)
	}
	
	// 出力コーデックの妥当性チェック
	if outputCodec != "h264" && outputCodec != "h265" {
		log.Fatalf("サポートされていない出力コーデック: %s。'h264' または 'h265' を使用してください。", outputCodec)
	}
	
	// H.264入力時に出力コーデックがH.265の場合は警告
	if codec == "h264" && outputCodec == "h265" {
		log.Printf("警告: H.264入力からH.265出力への変換は現在サポートされていません。出力をH.264に設定します。")
		outputCodec = "h264"
	}
		if inputType == "server" {
		log.Printf("入力タイプ: %s, コーデック: %s を使用します (RTSPサーバーモード)", inputType, codec)
	} else if inputType == "rtp-server" {
		log.Printf("入力タイプ: %s, コーデック: %s を使用します (RTPサーバーモード、リスニングアドレス: %s)", inputType, codec, rtpServerAddr)
	} else {
		log.Printf("入力URL: %s, 入力タイプ: %s, コーデック: %s を使用します", inputURL, inputType, codec)
	}
	
	if codec == "h265" {
		log.Printf("H.265入力 -> %s出力, プロセッサ: %s", outputCodec, processor)
	}
	
	props := props{
		codec:       codec,
		outputCodec: outputCodec,
		serverPort:  serverPort,
		processor:   processor,
		inputType:   inputType,
		inputURL:    inputURL,
		fps:         30, // デフォルトのフレームレートを設定 (必要に応じて変更可能)
	}
	
	if useGortsplib == "true" {
		log.Println("RTSPパススルーまたはトランスコーディングにgortsplibベースのハンドラーを使用します")
		switch inputType {
		case "server":
			log.Println("RTSPサーバーモードでgortsplibベースのサーバーを起動します")
			switch codec {
			case "h264":
				go startGortsplibH264RTSPServer(props)
			case "h265":
				go startGortsplibH265RTSPServer(props)
			default:
				log.Fatalf("サポートされていないコーデック: %s。'h264' または 'h265' を使用してください。", codec)
			}
		case "rtp-server":
			log.Println("RTPサーバーモードで動的SDP受信サーバーを起動します")
			go startRTPServer(rtpServerAddr)
		case "rtp":
			// RTP入力の場合は既存のRTPクライアントを使用
			log.Printf("RTP入力が検出されました。RTPクライアントを使用します (コーデック: %s)", codec)
			switch codec {
			case "h264":
				setCurrentCodec("h264")
				go startRTPClient(props.inputURL)
			case "h265":
				if outputCodec == "h264" {
					log.Println("H.265 RTP入力をH.264に変換してWebRTCにストリーミングします（注意: RTPクライアントは直接パススルーのみサポート）")
					setCurrentCodec("h265") // RTPクライアントはパススルーのみ
				} else {
					setCurrentCodec("h265")
				}				
				go startRTPClient(props.inputURL)
			default:
				log.Fatalf("RTPクライアントは現在H.264およびH.265のみをサポートしています。指定されたコーデック: %s", codec)
			}
		default:
			switch codec {
			case "h264":
				setCurrentCodec("h264")
				go startGortsplibH264RTSP(props)
			case "h265":
				// H.265入力時の出力コーデックに基づいて処理を分岐
				if outputCodec == "h264" {
					log.Println("gortsplibを使用してH.265をH.264にトランスコードし、WebRTCにストリーミングします")
					setCurrentCodec("h264")
					go startGortsplibH265toH264RTSP(props)
				} else {
					log.Println("gortsplibを使用してH.265をパススルーし、WebRTCにストリーミングします")
					setCurrentCodec("h265")
					go startGortsplibH265RTSP(props)
				}
			default:
				log.Fatalf("gortsplibは現在H.264およびH.265 (->H.264トランスコード) のみをサポートしています。指定されたコーデック: %s", codec)
			}
		}
	} else {
		// 既存のffmpegベースのロジック (useGortsplib が "false" の場合)		log.Println("従来のffmpegベースのハンドラーを使用します")
		switch inputType {
		case "server":
			log.Fatal("サーバーモードはgortsplibが必要です。-use-gortsplib=true を指定してください")
		case "rtp-server":
			log.Println("RTPサーバーモードで動的SDP受信サーバーを起動します")
			go startRTPServer(rtpServerAddr)
		case "rtsp":
			switch codec {			case "h264":
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
				setCurrentCodec("h264")
				go startFFmpegH264RTP(inputURL) // ffmpegベースのH.264 RTP処理
			case "h265":
				if outputCodec == "h264" {
					// H.265 -> H.264 トランスコーディング
					switch processor {
					case "gpu":
						go startFFmpegH265ToH264NALGPURTP(inputURL)
					case "cpu":
						go startFFmpegH265ToH264NALCPURTP(inputURL)
					default:
						log.Fatalf("H.265のサポートされていないプロセッサ: %s。'cpu' または 'gpu' を使用してください。", processor)
					}
					setCurrentCodec("h264") // 出力はH.264
				} else {
					// H.265パススルー
					setCurrentCodec("h265")
					go startFFmpegH265RTP(inputURL) // ffmpegベースのH.265 RTPパススルー
				}
			default:
				log.Fatalf("RTPのサポートされていないコーデック: %s", codec)
			}
		default:
			log.Fatalf("サポートされていない入力タイプ: %s。'rtsp', 'rtp', 'server', または 'rtp-server' を使用してください。", inputType)
		}
	}
	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		signalingHandler(w, r)
	})
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "prev.html")
	})
	// ローカル外部アクセスを許可するため、ListenAndServeのアドレスを 0.0.0.0 から指定IPに変更可能にします
	addr := "0.0.0.0:" + serverPort
	log.Printf("サーバーが %s で起動しました", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

