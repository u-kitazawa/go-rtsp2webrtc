package main

import (
	"log"
	"net/url"
	"os/exec" // 追加
	"strconv" // strconv をインポートに追加
	"strings"
	"time"

	"github.com/bluenviron/gortsplib/v4"
	"github.com/bluenviron/gortsplib/v4/pkg/base"
	"github.com/bluenviron/gortsplib/v4/pkg/format"
	"github.com/bluenviron/gortsplib/v4/pkg/format/rtph264"
	"github.com/bluenviron/gortsplib/v4/pkg/format/rtph265" // 追加
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3/pkg/media/h264reader" // 追加
	// "github.com/bluenviron/mediacommon/v2/pkg/codecs/h264" // パススルーでは使用しません
)

// sanitizeContentBase は、RTSPレスポンスヘッダーの Content-Base を正規化します。
func sanitizeContentBase(res *base.Response) {
	if cb, ok := res.Header["Content-Base"]; ok && len(cb) == 1 {
		v := strings.Trim(cb[0], "[]\"")      // 角括弧や引用符を除去
		if !strings.HasPrefix(v, "rtsp://") { // スキームが無ければ付与
			v = "rtsp://" + v
		}
		// url.Parse できる形なら上書き
		if pu, err := url.Parse(v); err == nil {
			res.Header["Content-Base"] = base.HeaderValue{pu.String()}
		}
	}
}

// --- H.264 RTSP パススルー (gortsplib 版・超低遅延) ---
// startGortsplibH264RTSP は、指定されたRTSP URLからH.264ストリームを取得し、
// WebRTCハンドラー (webrtc_handler.go の writeNALsToTracks) にNALユニットを渡します。
func startGortsplibH264RTSP(props props) {
	c := gortsplib.Client{
		// OnResponse は、サーバーからのレスポンス受信時に呼び出されます。
		// ここでは、Content-Base ヘッダーを正規化するために使用しています。
		OnResponse: func(res *base.Response) {
			if res.StatusCode == base.StatusOK {
				sanitizeContentBase(res)
			}
		},
	}

	u, err := base.ParseURL(props.inputURL)
	log.Println("gortsplib: 入力URLを解析中:", u.Host) // 初期化時のログはパフォーマンスに影響小
	if err != nil {
		log.Printf("入力URLの解析エラー: %v", err)
		return
	}

	// サーバーに接続
	err = c.Start(u.Scheme, u.Host)
	if err != nil {
		log.Printf("RTSPサーバーへの接続エラー: %v", err)
		return
	}
	defer c.Close()
	log.Println("gortsplib: RTSPサーバーに接続しました") // 初期化時のログ

	// 利用可能なメディアを検索 (DESCRIBEリクエスト)
	desc, _, err := c.Describe(u)
	if err != nil {
		log.Printf("RTSPストリームの記述エラー: %v", err)
		return
	}

	// H264メディアとフォーマットを検索
	var forma *format.H264
	medi := desc.FindFormat(&forma)
	if medi == nil {
		log.Println("gortsplib: H.264メディアが見つかりません")
		return
	}
	log.Println("gortsplib: H.264メディアが見つかりました") // 初期化時のログ

	// RTP -> H264デコーダーをセットアップ (gortsplibの場合、これはNALユニットエクストラクタとして機能)
	rtpDec, err := forma.CreateDecoder()
	if err != nil {
		log.Printf("H.264 RTPデコーダーの作成エラー: %v", err)
		return
	}

	// SPSとPPSがSDPに存在する場合、それらをWebRTCトラックに送信します。
	// これらはビデオストリームの開始前にクライアントに送信される必要があります。
	initialNALs := [][]byte{}
	if forma.SPS != nil {
		initialNALs = append(initialNALs, forma.SPS)
		log.Println("gortsplib: SPSをWebRTCトラックに送信中") // 初期化時のログ
	}
	if forma.PPS != nil {
		initialNALs = append(initialNALs, forma.PPS)
		log.Println("gortsplib: PPSをWebRTCトラックに送信中") // 初期化時のログ
	}
	if len(initialNALs) > 0 {
		// SPS/PPSのような設定NALの場合、期間は厳密には重要ではありません。
		// ここでは一般的なフレームレートを想定したデフォルト値を使用しています。
		writeNALsToTracks(initialNALs, time.Second/30) // デフォルトの期間として30 FPSを想定
	}

	// 単一メディアをセットアップ (SETUPリクエスト)
	log.Printf("gortsplib: RTSPメディア %v をセットアップ中", medi) // 初期化時のログ
	_, err = c.Setup(desc.BaseURL, medi, 0, 0)
	if err != nil {
		log.Printf("RTSPメディアのセットアップエラー: %v", err)
		return
	}
	log.Println("gortsplib: RTSPメディアのセットアップ完了") // 初期化時のログ

	// フレーム期間の決定。
	// デフォルトで30 FPSを想定
	frameDuration := time.Second / 30
	log.Printf("gortsplib: デフォルトのフレーム期間: %v (30 FPS相当)", frameDuration)

	// SDPのfmtp属性からフレームレートに関連する情報を解析する試み
	// format.H264 の FMTP() メソッドを使用
	fmtpMap := forma.FMTP()
	if fmtpMap != nil {
		if framerateVal, ok := fmtpMap["framerate"]; ok {
			fps, err := strconv.ParseFloat(framerateVal, 64)
			if err == nil && fps > 0 {
				frameDuration = time.Duration(float64(time.Second) / fps)
				log.Printf("gortsplib: SDP FMTPからフレームレート %s FPS を検出。新しいフレーム期間: %v", framerateVal, frameDuration)
			} else if err != nil {
				log.Printf("gortsplib: SDP FMTPのフレームレート値 '%s' の解析エラー: %v。デフォルトのフレーム期間を使用します。", framerateVal, err)
			} else {
				log.Printf("gortsplib: SDP FMTPのフレームレート値 '%s' が無効 (0以下)。デフォルトのフレーム期間を使用します。", framerateVal)
			}
		} else {
			log.Println("gortsplib: SDP FMTPに 'framerate' が見つかりません。デフォルトのフレーム期間を使用します。")
		}
	} else {
		log.Println("gortsplib: SDPにFMTP属性が見つかりません。デフォルトのフレーム期間を使用します。")
	}

	// RTPパケットのタイムスタンプ差から動的に計算する方法も考えられますが、
	// 現在はSDPのframerate属性（利用可能な場合）または固定値（30 FPS相当、フォールバック）を使用しています。
	// この値は writeNALsToTracks に渡され、WebRTC側でのサンプル期間として使用されます。

	// OnPacketRTP は、RTPパケット到着時に呼び出されるコールバックです。
	// このコールバック内の処理は、パケット受信ごとに行われるため、効率性が重要です。
	c.OnPacketRTP(medi, forma, func(pkt *rtp.Packet) {

		// RTPパケットからアクセスユニット（NALユニット群）を抽出します。
		// au は [][]byte 型で、1つ以上のNALユニットを含みます。
		au, err := rtpDec.Decode(pkt) // au は [][]byte です
		if err != nil {
			// ErrNonStartingPacketAndNoPrevious: フレームの先頭パケットではないが、前のパケットがない場合 (順序が乱れた場合など)
			// ErrMorePacketsNeeded: パケットが分割されており、アクセスユニットを完成させるにはさらにパケットが必要な場合
			// これらは必ずしも致命的なエラーではなく、ストリームの特性上発生しうるため、ログレベルを調整するか、特定の条件下では無視することも検討できます。
			if err != rtph264.ErrNonStartingPacketAndNoPrevious && err != rtph264.ErrMorePacketsNeeded {
				log.Printf("gortsplib: RTPデコードエラー: %v", err) // エラー発生時のみログ出力
			}
			return
		}

		// 抽出されたNALユニットが存在する場合のみ処理
		if len(au) > 0 {
			// writeNALsToTracks は、NALユニット群をWebRTCトラックに書き込みます。
			// この関数は webrtc_handler.go で定義されており、複数のWebRTCクライアントへの配信処理を含みます。
			// この呼び出しがボトルネックになる場合は、webrtc_handler.go側の最適化や、
			// 非同期処理（ただしNALの順序保証が必要）を検討する必要があります。
			writeNALsToTracks(au, frameDuration)
		}
	})

	// 再生開始 (PLAYリクエスト)
	_, err = c.Play(nil)
	if err != nil {
		log.Printf("RTSP再生の開始エラー: %v", err)
		return
	}
	log.Println("gortsplib: RTSP再生が開始されました。WebRTCにストリーミング中...") // 初期化時のログ

	// 致命的なエラーが発生するか、ストリームが終了するまで待機
	// c.Wait() は通常、エラーが発生した場合にそのエラーを返します。正常終了時は nil を返すこともあります。
	log.Printf("gortsplib: クライアント処理終了: %v", c.Wait()) // 終了時のログ
}

// --- H.265 RTSP -> H.264 WebRTC (gortsplib + ffmpeg) ---
func startGortsplibH265toH264RTSP(props props) {
    log.Println("gortsplib: H.265 to H.264 並列トランスコーディングを開始します")

    c := gortsplib.Client{
        OnResponse: func(res *base.Response) {
            if res.StatusCode == base.StatusOK {
                sanitizeContentBase(res)
            }
        },
    }

    u, err := base.ParseURL(props.inputURL)
    if err != nil {
        log.Printf("gortsplib: 入力URLの解析エラー: %v", err)
        return
    }

    err = c.Start(u.Scheme, u.Host)
    if err != nil {
        log.Printf("gortsplib: RTSPサーバーへの接続エラー: %v", err)
        return
    }
    defer c.Close()

    desc, _, err := c.Describe(u)
    if err != nil {
        log.Printf("gortsplib: RTSPストリームの記述エラー: %v", err)
        return
    }

    var formaH265 *format.H265
    medi := desc.FindFormat(&formaH265)
    if medi == nil {
        log.Println("gortsplib: H.265メディアが見つかりません")
        return
    }

    rtpDec, err := formaH265.CreateDecoder()
    if err != nil {
        log.Printf("gortsplib: H.265 RTPデコーダーの作成エラー: %v", err)
        return
    }

    // 並列処理用のチャネル（バッファサイズを調整して遅延を最小化）
    nalChan := make(chan []byte, 100) // バッファサイズ増加
    h264NALChan := make(chan []byte, 100)
    
    // FFmpeg設定（さらに最適化）
    ffmpegArgs := []string{
        "-hide_banner",
        "-loglevel", "panic", // ログを最小化
        "-f", "hevc",
        "-fflags", "+genpts+igndts+nobuffer",
        "-probesize", "512",
        "-analyzeduration", "0",
        "-i", "pipe:0",
        "-c:v", "libx264",
        "-preset", "ultrafast",
        "-tune", "zerolatency",
        "-x264-params", "nal-hrd=cbr:force-cfr=1:sync-lookahead=0:sliced-threads=1:rc-lookahead=0:bframes=0:keyint=10:refs=1",
        "-b:v", "2M", // ビットレート削減
        "-maxrate", "2M",
        "-bufsize", "200k",
        "-g", "10", // GOP短縮
        "-bf", "0",
        "-refs", "1",
        "-flush_packets", "1",
        "-real_time", "1",
        "-bsf:v", "h264_mp4toannexb",
        "-f", "h264",
        "pipe:1",
    }

    if props.processor == "gpu" {
        ffmpegArgs = []string{
            "-hide_banner",
            "-loglevel", "panic",
            "-hwaccel", "auto",
            "-c:v", "hevc_cuvid",
            "-i", "pipe:0",
            "-c:v", "h264_nvenc",
            "-preset", "p1",
            "-tune", "ll",
            "-delay", "0",
            "-rc", "cbr",
            "-b:v", "2M",
            "-maxrate", "2M",
            "-bufsize", "200k",
            "-g", "10",
            "-bf", "0",
            "-forced-idr", "1",
            "-bsf:v", "h264_mp4toannexb",
            "-f", "h264",
            "pipe:1",
        }
    }

    cmd := exec.Command("ffmpeg", ffmpegArgs...)
    ffmpegIn, _ := cmd.StdinPipe()
    ffmpegOut, _ := cmd.StdoutPipe()
    cmd.Start()

    // 1. H.265 NAL書き込み用ゴルーチン（高優先度）
    go func() {
        defer ffmpegIn.Close()
        
        // 初期パラメータ送信
        if formaH265.VPS != nil {
            ffmpegIn.Write(append([]byte{0x00, 0x00, 0x00, 0x01}, formaH265.VPS...))
        }
        if formaH265.SPS != nil {
            ffmpegIn.Write(append([]byte{0x00, 0x00, 0x00, 0x01}, formaH265.SPS...))
        }
        if formaH265.PPS != nil {
            ffmpegIn.Write(append([]byte{0x00, 0x00, 0x00, 0x01}, formaH265.PPS...))
        }

        // バッチ処理で効率化
        batch := make([]byte, 0, 1024*1024) // 1MBバッファ
        batchCount := 0
        
        for nal := range nalChan {
            nalWithStartCode := append([]byte{0x00, 0x00, 0x00, 0x01}, nal...)
            batch = append(batch, nalWithStartCode...)
            batchCount++
            
            // 小さなバッチサイズで即座に送信（遅延最小化）
            if batchCount >= 3 || len(batch) > 64*1024 {
                if _, err := ffmpegIn.Write(batch); err != nil {
                    return
                }
                batch = batch[:0] // バッファリセット
                batchCount = 0
            }
        }
        
        // 残りのデータを送信
        if len(batch) > 0 {
            ffmpegIn.Write(batch)
        }
    }()

    // 2. H.264 NAL読み取り用ゴルーチン（高優先度）
    go func() {
        defer close(h264NALChan)
        
        h264NALReader, err := h264reader.NewReader(ffmpegOut)
        if err != nil {
            return
        }

        for {
            nal, err := h264NALReader.NextNAL()
            if err != nil {
                break
            }
            if nal != nil && len(nal.Data) > 0 {
                // 非ブロッキング送信で遅延防止
                select {
                case h264NALChan <- nal.Data:
                default:
                    // チャネルが満杯の場合はスキップ（遅延防止）
                }
            }
        }
    }()

    // 3. WebRTC送信用ゴルーチン（最高優先度）
    go func() {
        frameDuration := time.Second / 30
        fmtpMap := formaH265.FMTP()
        if fmtpMap != nil {
            if framerateVal, ok := fmtpMap["framerate"]; ok {
                if fps, err := strconv.ParseFloat(framerateVal, 64); err == nil && fps > 0 {
                    frameDuration = time.Duration(float64(time.Second) / fps)
                }
            }
        }

        for h264NAL := range h264NALChan {
            writeNALsToTracks([][]byte{h264NAL}, frameDuration)
        }
    }()

    _, err = c.Setup(desc.BaseURL, medi, 0, 0)
    if err != nil {
        log.Printf("gortsplib: RTSPメディアのセットアップエラー: %v", err)
        return
    }

    // 4. RTPパケット受信（メインスレッド）- 非ブロッキング処理
    c.OnPacketRTP(medi, formaH265, func(pkt *rtp.Packet) {
        au, err := rtpDec.Decode(pkt)
        if err != nil {
            if err != rtph265.ErrNonStartingPacketAndNoPrevious && err != rtph265.ErrMorePacketsNeeded {
                // エラーログを削除して高速化
            }
            return
        }

        if len(au) > 0 {
            for _, nal := range au {
                if len(nal) == 0 {
                    continue
                }
                // 非ブロッキング送信
                select {
                case nalChan <- nal:
                default:
                    // チャネルが満杯の場合はスキップ（遅延防止）
                }
            }
        }
    })

    _, err = c.Play(nil)
    if err != nil {
        log.Printf("gortsplib: RTSP再生の開始エラー: %v", err)
        return
    }

    log.Println("gortsplib: 並列H.265→H.264変換開始。WebRTCにストリーミング中...")

    clientErr := c.Wait()
    close(nalChan) // チャネルを閉じてゴルーチンを終了
    
    log.Printf("gortsplib: 並列処理完了: %v", clientErr)
}

// --- H.265 RTSP パススルー (gortsplib 版・超低遅延) ---
// startGortsplibH265RTSP は、指定されたRTSP URLからH.265ストリームを取得し、
// WebRTCハンドラー (webrtc_handler.go の writeNALsToTracksH265) にNALユニットを渡します。
func startGortsplibH265RTSP(props props) {
	c := gortsplib.Client{
		// OnResponse は、サーバーからのレスポンス受信時に呼び出されます。
		// ここでは、Content-Base ヘッダーを正規化するために使用しています。
		OnResponse: func(res *base.Response) {
			if res.StatusCode == base.StatusOK {
				sanitizeContentBase(res)
			}
		},
	}

	u, err := base.ParseURL(props.inputURL)
	log.Println("gortsplib: H.265入力URLを解析中:", u.Host) // 初期化時のログはパフォーマンスに影響小
	if err != nil {
		log.Printf("入力URLの解析エラー: %v", err)
		return
	}

	// サーバーに接続
	err = c.Start(u.Scheme, u.Host)
	if err != nil {
		log.Printf("RTSPサーバーへの接続エラー: %v", err)
		return
	}
	defer c.Close()
	log.Println("gortsplib: RTSPサーバーに接続しました (H.265)") // 初期化時のログ

	// 利用可能なメディアを検索 (DESCRIBEリクエスト)
	desc, _, err := c.Describe(u)
	if err != nil {
		log.Printf("RTSPストリームの記述エラー: %v", err)
		return
	}

	// H265メディアとフォーマットを検索
	var forma *format.H265
	medi := desc.FindFormat(&forma)
	if medi == nil {
		log.Println("gortsplib: H.265メディアが見つかりません")
		return
	}
	log.Println("gortsplib: H.265メディアが見つかりました") // 初期化時のログ

	// RTP -> H265デコーダーをセットアップ (gortsplibの場合、これはNALユニットエクストラクタとして機能)
	rtpDec, err := forma.CreateDecoder()
	if err != nil {
		log.Printf("H.265 RTPデコーダーの作成エラー: %v", err)
		return
	}

	// VPS、SPS、PPSがSDPに存在する場合、それらをWebRTCトラックに送信します。
	// これらはビデオストリームの開始前にクライアントに送信される必要があります。
	initialNALs := [][]byte{}
	if forma.VPS != nil {
		initialNALs = append(initialNALs, forma.VPS)
		log.Println("gortsplib: VPSをWebRTCトラックに送信中") // 初期化時のログ
	}
	if forma.SPS != nil {
		initialNALs = append(initialNALs, forma.SPS)
		log.Println("gortsplib: SPSをWebRTCトラックに送信中") // 初期化時のログ
	}
	if forma.PPS != nil {
		initialNALs = append(initialNALs, forma.PPS)
		log.Println("gortsplib: PPSをWebRTCトラックに送信中") // 初期化時のログ
	}
	if len(initialNALs) > 0 {
		// VPS/SPS/PPSのような設定NALの場合、期間は厳密には重要ではありません。
		// ここでは一般的なフレームレートを想定したデフォルト値を使用しています。
		writeNALsToTracksH265(initialNALs, time.Second/30) // デフォルトの期間として30 FPSを想定
	}

	// 単一メディアをセットアップ (SETUPリクエスト)
	log.Printf("gortsplib: RTSP H.265メディア %v をセットアップ中", medi) // 初期化時のログ
	_, err = c.Setup(desc.BaseURL, medi, 0, 0)
	if err != nil {
		log.Printf("RTSP H.265メディアのセットアップエラー: %v", err)
		return
	}
	log.Println("gortsplib: RTSP H.265メディアのセットアップ完了") // 初期化時のログ

	// フレーム期間の決定。
	// デフォルトで30 FPSを想定
	frameDuration := time.Second / 30
	log.Printf("gortsplib: デフォルトのフレーム期間: %v (30 FPS相当)", frameDuration)

	// SDPのfmtp属性からフレームレートに関連する情報を解析する試み
	// format.H265 の FMTP() メソッドを使用
	fmtpMap := forma.FMTP()
	if fmtpMap != nil {
		if framerateVal, ok := fmtpMap["framerate"]; ok {
			fps, err := strconv.ParseFloat(framerateVal, 64)
			if err == nil && fps > 0 {
				frameDuration = time.Duration(float64(time.Second) / fps)
				log.Printf("gortsplib: SDP FMTPからフレームレート %s FPS を検出。新しいフレーム期間: %v", framerateVal, frameDuration)
			} else if err != nil {
				log.Printf("gortsplib: SDP FMTPのフレームレート値 '%s' の解析エラー: %v。デフォルトのフレーム期間を使用します。", framerateVal, err)
			} else {
				log.Printf("gortsplib: SDP FMTPのフレームレート値 '%s' が無効 (0以下)。デフォルトのフレーム期間を使用します。", framerateVal)
			}
		} else {
			log.Println("gortsplib: SDP FMTPに 'framerate' が見つかりません。デフォルトのフレーム期間を使用します。")
		}
	} else {
		log.Println("gortsplib: SDPにFMTP属性が見つかりません。デフォルトのフレーム期間を使用します。")
	}

	// OnPacketRTP は、RTPパケット到着時に呼び出されるコールバックです。
	// このコールバック内の処理は、パケット受信ごとに行われるため、効率性が重要です。
	c.OnPacketRTP(medi, forma, func(pkt *rtp.Packet) {

		// RTPパケットからアクセスユニット（NALユニット群）を抽出します。
		// au は [][]byte 型で、1つ以上のNALユニットを含みます。
		au, err := rtpDec.Decode(pkt) // au は [][]byte です
		if err != nil {
			// ErrNonStartingPacketAndNoPrevious: フレームの先頭パケットではないが、前のパケットがない場合 (順序が乱れた場合など)
			// ErrMorePacketsNeeded: パケットが分割されており、アクセスユニットを完成させるにはさらにパケットが必要な場合
			// これらは必ずしも致命的なエラーではなく、ストリームの特性上発生しうるため、ログレベルを調整するか、特定の条件下では無視することも検討できます。
			if err != rtph265.ErrNonStartingPacketAndNoPrevious && err != rtph265.ErrMorePacketsNeeded {
				log.Printf("gortsplib: H.265 RTPデコードエラー: %v", err) // エラー発生時のみログ出力
			}
			return
		}

		// 抽出されたNALユニットが存在する場合のみ処理
		if len(au) > 0 {
			// writeNALsToTracksH265 は、NALユニット群をH.265 WebRTCトラックに書き込みます。
			// この関数は webrtc_handler.go で定義されており、複数のWebRTCクライアントへの配信処理を含みます。
			// この呼び出しがボトルネックになる場合は、webrtc_handler.go側の最適化や、
			// 非同期処理（ただしNALの順序保証が必要）を検討する必要があります。
			writeNALsToTracksH265(au, frameDuration)
		}
	})

	// 再生開始 (PLAYリクエスト)
	_, err = c.Play(nil)
	if err != nil {
		log.Printf("RTSP再生の開始エラー: %v", err)
		return
	}
	log.Println("gortsplib: H.265 RTSP再生が開始されました。WebRTCにストリーミング中...") // 初期化時のログ

	// 致命的なエラーが発生するか、ストリームが終了するまで待機
	// c.Wait() は通常、エラーが発生した場合にそのエラーを返します。正常終了時は nil を返すこともあります。
	log.Printf("gortsplib: H.265クライアント処理終了: %v", c.Wait()) // 終了時のログ
}
