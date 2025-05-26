package main

import (
	"fmt"
	"io"
	"log"
	"os/exec"
	"sync"
	"time"

	"github.com/pion/rtp"

	"github.com/bluenviron/gortsplib/v4"
	"github.com/bluenviron/gortsplib/v4/pkg/base"
	"github.com/bluenviron/gortsplib/v4/pkg/description"
	"github.com/bluenviron/gortsplib/v4/pkg/format"
	"github.com/bluenviron/gortsplib/v4/pkg/format/rtph264"
	"github.com/bluenviron/gortsplib/v4/pkg/format/rtph265"
	"github.com/pion/webrtc/v3/pkg/media/h264reader"
)

// RTSPサーバーハンドラー（RTSPクライアントからのPUSHを受けてWebRTC配信）
type serverHandler struct {
	server    *gortsplib.Server
	mutex     sync.Mutex
	publisher *gortsplib.ServerSession
	media     *description.Media
	
	// H.264用フィールド
	formatH264 *format.H264
	rtpDecH264 *rtph264.Decoder
	
	// H.265用フィールド
	formatH265   *format.H265
	rtpDecH265   *rtph265.Decoder
	ffmpegCmd    *exec.Cmd
	ffmpegStdin  io.WriteCloser
	h264Reader   *h264reader.H264Reader
	
	// コーデックタイプ
	codecType string // "h264" または "h265"
	props     props  // プロセッサ情報など
}

// 接続が開かれたときに呼び出される
func (sh *serverHandler) OnConnOpen(ctx *gortsplib.ServerHandlerOnConnOpenCtx) {
	log.Printf("RTSP server: 接続が開かれました")
}

// 接続が閉じられたときに呼び出される
func (sh *serverHandler) OnConnClose(ctx *gortsplib.ServerHandlerOnConnCloseCtx) {
	log.Printf("RTSP server: 接続が閉じられました (%v)", ctx.Error)
}

// セッションが開かれたときに呼び出される
func (sh *serverHandler) OnSessionOpen(ctx *gortsplib.ServerHandlerOnSessionOpenCtx) {
	log.Printf("RTSP server: セッションが開かれました")
}

// セッションが閉じられたときに呼び出される
func (sh *serverHandler) OnSessionClose(ctx *gortsplib.ServerHandlerOnSessionCloseCtx) {
	log.Printf("RTSP server: セッションが閉じられました")

	sh.mutex.Lock()
	defer sh.mutex.Unlock()

	// H.265トランスコーディングプロセスのクリーンアップ
	if sh.ffmpegStdin != nil {
		sh.ffmpegStdin.Close()
		sh.ffmpegStdin = nil
	}
	
	if sh.ffmpegCmd != nil && sh.ffmpegCmd.Process != nil {
		log.Printf("RTSP server: H.265トランスコーディングプロセスを終了中")
		sh.ffmpegCmd.Process.Kill()
		sh.ffmpegCmd.Wait()
		sh.ffmpegCmd = nil
	}

	if sh.h264Reader != nil {
		sh.h264Reader = nil
	}

	sh.publisher = nil
}

// ANNOUNCEリクエストを受信したときに呼び出される
func (sh *serverHandler) OnAnnounce(ctx *gortsplib.ServerHandlerOnAnnounceCtx) (*base.Response, error) {
	log.Printf("RTSP server: ANNOUNCEリクエストを受信 (期待されるコーデック: %s)", sh.codecType)

	sh.mutex.Lock()
	defer sh.mutex.Unlock()

	if sh.publisher != nil {
		sh.publisher.Close()
	}

	// 事前にわかっているコーデックに基づいて効率的に処理
	switch sh.codecType {
	case "h264":
		return sh.setupH264Stream(ctx)
	case "h265":
		return sh.setupH265Stream(ctx)
	default:
		return &base.Response{
			StatusCode: base.StatusBadRequest,
		}, fmt.Errorf("サポートされていないコーデック: %s", sh.codecType)
	}
}

// H.264ストリームのセットアップ（効率化版）
func (sh *serverHandler) setupH264Stream(ctx *gortsplib.ServerHandlerOnAnnounceCtx) (*base.Response, error) {
	var formatH264 *format.H264
	mediH264 := ctx.Description.FindFormat(&formatH264)
	
	if mediH264 == nil {
		return &base.Response{
			StatusCode: base.StatusBadRequest,
		}, fmt.Errorf("期待されたH.264メディアが見つかりません")
	}

	log.Printf("RTSP server: H.264メディアを設定中")
	
	rtpDec, err := formatH264.CreateDecoder()
	if err != nil {
		log.Printf("RTSP server: H264デコーダーの作成に失敗: %v", err)
		return &base.Response{
			StatusCode: base.StatusBadRequest,
		}, err
	}
	
	sh.publisher = ctx.Session
	sh.media = mediH264
	sh.formatH264 = formatH264
	sh.rtpDecH264 = rtpDec

	// SPS/PPSが利用可能な場合、WebRTCトラックに初期送信
	initialNALs := [][]byte{}
	if formatH264.SPS != nil {
		initialNALs = append(initialNALs, formatH264.SPS)
		log.Printf("RTSP server: SPSをWebRTCトラックに送信中")
	}
	if formatH264.PPS != nil {
		initialNALs = append(initialNALs, formatH264.PPS)
		log.Printf("RTSP server: PPSをWebRTCトラックに送信中")
	}
	if len(initialNALs) > 0 {
		writeNALsToTracks(initialNALs, time.Second/30)
	}

	log.Printf("RTSP server: H.264パブリッシャーのセットアップが完了")
	return &base.Response{StatusCode: base.StatusOK}, nil
}

// H.265ストリームのセットアップ（効率化版）
func (sh *serverHandler) setupH265Stream(ctx *gortsplib.ServerHandlerOnAnnounceCtx) (*base.Response, error) {
	var formatH265 *format.H265
	mediH265 := ctx.Description.FindFormat(&formatH265)
	
	if mediH265 == nil {
		return &base.Response{
			StatusCode: base.StatusBadRequest,
		}, fmt.Errorf("期待されたH.265メディアが見つかりません")
	}

	log.Printf("RTSP server: H.265メディアを設定中 - H.264へのトランスコーディングを開始")
	
	rtpDec, err := formatH265.CreateDecoder()
	if err != nil {
		log.Printf("RTSP server: H265デコーダーの作成に失敗: %v", err)
		return &base.Response{
			StatusCode: base.StatusBadRequest,
		}, err
	}
	
	sh.publisher = ctx.Session
	sh.media = mediH265
	sh.formatH265 = formatH265
	sh.rtpDecH265 = rtpDec
	
	// H.265 -> H.264 ffmpegプロセスをセットアップ
	err = sh.setupH265ToH264Transcoding()
	if err != nil {
		log.Printf("RTSP server: H.265トランスコーディングのセットアップに失敗: %v", err)
		return &base.Response{
			StatusCode: base.StatusBadRequest,
		}, err
	}

	log.Printf("RTSP server: H.265パブリッシャーのセットアップが完了")
	return &base.Response{StatusCode: base.StatusOK}, nil
}

// SETUPリクエストを受信したときに呼び出される
func (sh *serverHandler) OnSetup(ctx *gortsplib.ServerHandlerOnSetupCtx) (*base.Response, *gortsplib.ServerStream, error) {
	// リーダーからのアクセスを防止（パブリッシャーのみ許可）
	if ctx.Session.State() == gortsplib.ServerSessionStateInitial {
		return &base.Response{
			StatusCode: base.StatusNotImplemented,
		}, nil, nil
	}

	log.Printf("RTSP server: SETUPリクエストを受信")

	return &base.Response{
		StatusCode: base.StatusOK,
	}, nil, nil
}

// RECORDリクエストを受信したときに呼び出される
func (sh *serverHandler) OnRecord(ctx *gortsplib.ServerHandlerOnRecordCtx) (*base.Response, error) {
	log.Printf("RTSP server: RECORDリクエストを受信 - ストリーミング開始 (コーデック: %s)", sh.codecType)
	
	// 事前にわかっているコーデックに基づいて効率的に処理
	switch sh.codecType {
	case "h264":
		sh.setupH264PacketHandler(ctx)
	case "h265":
		sh.setupH265PacketHandler(ctx)
	}

	return &base.Response{
		StatusCode: base.StatusOK,
	}, nil
}

// H.264パケットハンドラーのセットアップ（効率化版）
func (sh *serverHandler) setupH264PacketHandler(ctx *gortsplib.ServerHandlerOnRecordCtx) {
	ctx.Session.OnPacketRTP(sh.media, sh.formatH264, func(pkt *rtp.Packet) {
		// パケットのタイムスタンプをデコード
		_, ok := ctx.Session.PacketPTS2(sh.media, pkt)
		if !ok {
			return
		}

		// RTPパケットからH264アクセスユニットをデコード
		au, err := sh.rtpDecH264.Decode(pkt)
		if err != nil {
			// 特定のエラーは無視、その他はログ出力
			if err != rtph264.ErrNonStartingPacketAndNoPrevious && err != rtph264.ErrMorePacketsNeeded {
				log.Printf("RTSP server: RTPデコードエラー: %v", err)
			}
			return
		}

		// アクセスユニットが有効な場合、WebRTCトラックに送信
		if len(au) > 0 {
			// フレームレートを30FPSと仮定してdurationを計算
			duration := time.Second / 30
			// writeNALsToTracks関数を呼び出してWebRTCクライアントに配信
			writeNALsToTracks(au, duration)
		}
	})
}

// H.265パケットハンドラーのセットアップ（効率化版）
func (sh *serverHandler) setupH265PacketHandler(ctx *gortsplib.ServerHandlerOnRecordCtx) {
	ctx.Session.OnPacketRTP(sh.media, sh.formatH265, func(pkt *rtp.Packet) {
		// パケットのタイムスタンプをデコード
		_, ok := ctx.Session.PacketPTS2(sh.media, pkt)
		if !ok {
			return
		}

		// RTPパケットからH265アクセスユニットをデコード
		au, err := sh.rtpDecH265.Decode(pkt)
		if err != nil {
			// 特定のエラーは無視、その他はログ出力
			if err != rtph265.ErrNonStartingPacketAndNoPrevious && err != rtph265.ErrMorePacketsNeeded {
				log.Printf("RTSP server: H265 RTPデコードエラー: %v", err)
			}
			return
		}

		// アクセスユニットが有効な場合、ffmpegプロセスに送信
		if len(au) > 0 {
			sh.writeH265ToTranscoder(au)
		}
	})
}

// H.264専用RTSPサーバーを起動する関数
func startGortsplibH264RTSPServer(props props) {
	log.Printf("RTSP server: H.264専用サーバーを起動中 (ポート: %s)", props.serverPort)
	
	// H.264専用サーバーハンドラーを設定
	h := &serverHandler{
		props:     props,
		codecType: "h264", // H.264固定
	}
	
	h.server = &gortsplib.Server{
		Handler:           h,
		RTSPAddress:       "0.0.0.0:554",
		UDPRTPAddress:     "0.0.0.0:8000",
		UDPRTCPAddress:    "0.0.0.0:8001",
		MulticastIPRange:  "224.1.0.0/16",
		MulticastRTPPort:  8002,
		MulticastRTCPPort: 8003,
	}

	log.Printf("RTSP server: H.264専用サーバーが %s で準備完了", h.server.RTSPAddress)
	log.Printf("RTSP server: クライアントは rtsp://0.0.0.0:554/stream でH.264ストリームをPUSHできます")
	
	// バックグラウンドでサーバーを実行
	go func() {
		if err := h.server.StartAndWait(); err != nil {
			log.Printf("RTSP server: H.264サーバーエラー: %v", err)
		}
	}()
}

// H.265専用RTSPサーバーを起動する関数
func startGortsplibH265RTSPServer(props props) {
	log.Printf("RTSP server: H.265専用サーバーを起動中 (ポート: %s, プロセッサ: %s)", props.serverPort, props.processor)
	
	// H.265専用サーバーハンドラーを設定
	h := &serverHandler{
		props:     props,
		codecType: "h265", // H.265固定
	}
	
	h.server = &gortsplib.Server{
		Handler:           h,
		RTSPAddress:       "0.0.0.0:554",
		UDPRTPAddress:     "0.0.0.0:8000",
		UDPRTCPAddress:    "0.0.0.0:8001",
		MulticastIPRange:  "224.1.0.0/16",
		MulticastRTPPort:  8002,
		MulticastRTCPPort: 8003,
	}

	log.Printf("RTSP server: H.265専用サーバーが %s で準備完了 (H.264トランスコーディング有効)", h.server.RTSPAddress)
	log.Printf("RTSP server: クライアントは rtsp://0.0.0.0:554/stream でH.265ストリームをPUSHできます")
	
	// バックグラウンドでサーバーを実行
	go func() {
		if err := h.server.StartAndWait(); err != nil {
			log.Printf("RTSP server: H.265サーバーエラー: %v", err)
		}
	}()
}

// setupH265ToH264Transcoding はH.265からH.264へのトランスコーディングを設定します
func (sh *serverHandler) setupH265ToH264Transcoding() error {
	log.Printf("RTSP server: H.265 -> H.264 トランスコーディングをセットアップ中 (プロセッサ: %s)", sh.props.processor)
	
	var args []string
	if sh.props.processor == "gpu" {
		// GPU トランスコーディング
		args = []string{
			"-hide_banner",
			"-loglevel", "error",
			"-f", "hevc",         // 入力フォーマット H.265
			"-i", "pipe:0",       // 標準入力から読み取り
			"-c:v", "h264_nvenc", // NVIDIA GPU エンコーダ
			"-preset", "p1",      // 最高速度プリセット
			"-tune", "ll",        // 低遅延チューニング
			"-rc:v", "cbr",       // 固定ビットレート
			"-b:v", "2M",         // ビットレート
			"-maxrate", "2M",
			"-bufsize", "4M",
			"-g", "30",           // GOPサイズ
			"-keyint_min", "30",
			"-bf", "0",           // Bフレームなし
			"-f", "h264",         // 出力フォーマット
			"-bsf:v", "h264_mp4toannexb", // Annex-B形式に変換
			"pipe:1",             // 標準出力へ書き込み
		}
	} else {
		// CPU トランスコーディング
		args = []string{
			"-hide_banner",
			"-loglevel", "error",
			"-probesize", "250000",
			"-analyzeduration", "50000", // CPU版ではより大きな値を設定することがあります
			"-fflags", "nobuffer+genpts", // flush_packets はCPUエンコードでは問題を起こすことがあるため削除
			"-flags", "low_delay",
			"-max_delay", "500", // CPU版では少し余裕を持たせることがあります
	
			"-f", "hevc",
			"-i", "pipe:0",

			"-an", // 音声なし

			// --- エンコード設定 (CPU, libx264) ---
			"-c:v", "libx264",
			"-preset", "ultrafast",        // エンコード速度優先
			// "-tune", "zerolatency",        // 低レイテンシ
			"-x264-params", "rc-lookahead=0:scenecut=0:vbv-maxrate=2000:vbv-bufsize=50", // レート制御パラメータ
			"-x264-params", "nal-hrd=cbr", // CBR に必要
			"-b:v", "3M",                  // ビットレート
			"-maxrate", "3M",              // 最大ビットレート
			"-bufsize", "5000",              // バッファサイズ

			"-g", "30", // GOP 長
			"-bf", "0", // B-frames 無効

			// フレームレートとメタデータ
			"-fps_mode", "passthrough", // 入力フレームレートを維持
			"-map", "0:v:0",
			"-f", "h264",
			"pipe:1",
		}
	}

	sh.ffmpegCmd = exec.Command("ffmpeg", args...)
	
	// ffmpegプロセスのパイプを設定
	var err error
	sh.ffmpegStdin, err = sh.ffmpegCmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("ffmpeg標準入力パイプの作成に失敗: %v", err)
	}
	
	stdout, err := sh.ffmpegCmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("ffmpeg標準出力パイプの作成に失敗: %v", err)
	}

	// ffmpegプロセスを開始
	if err := sh.ffmpegCmd.Start(); err != nil {
		return fmt.Errorf("ffmpegプロセスの開始に失敗: %v", err)
	}

	// H.264リーダーを設定
	sh.h264Reader, err = h264reader.NewReader(stdout)
	if err != nil {
		return fmt.Errorf("H264リーダーの作成に失敗: %v", err)
	}

	// H.264出力を読み取り、WebRTCに送信するゴルーチン
	go func() {
		defer stdout.Close()
		sh.streamTranscodedH264()
	}()

	log.Printf("RTSP server: H.265 -> H.264 トランスコーディングが準備完了")
	return nil
}

// writeH265ToTranscoder はH.265 NALユニットをffmpegトランスコーダーに書き込みます
func (sh *serverHandler) writeH265ToTranscoder(nals [][]byte) {
	if sh.ffmpegStdin == nil {
		return
	}

	for _, nal := range nals {
		// Annex-B形式でNALユニットを書き込み
		annexBData := append([]byte{0x00, 0x00, 0x00, 0x01}, nal...)
		if _, err := sh.ffmpegStdin.Write(annexBData); err != nil {
			log.Printf("RTSP server: ffmpegへのH.265データ書き込みエラー: %v", err)
			return
		}
	}
}

// streamTranscodedH264 はトランスコードされたH.264ストリームを処理します
func (sh *serverHandler) streamTranscodedH264() {
	for {
		nal, err := sh.h264Reader.NextNAL()
		if err != nil {
			if err.Error() != "EOF" {
				log.Printf("RTSP server: トランスコードされたH.264読み取りエラー: %v", err)
			}
			break
		}

		// NALユニットをWebRTCトラックに送信
		duration := time.Second / 30 // 30FPS想定
		writeNALsToTracks([][]byte{nal.Data}, duration)
	}
}
