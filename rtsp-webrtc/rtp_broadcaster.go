package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// SDPInfo はSDP情報を格納する構造体
type SDPInfo struct {
	Host       string
	Port       int
	CodecName  string
	PayloadType int
	ClockRate  int
	FmtpLine   string
}

// RTPClient はRTP接続を管理するクライアント構造体
type RTPClient struct {
	conn         *net.UDPConn
	sdpInfo      *SDPInfo
	isRunning    bool
	packetChan   chan []byte
	nalChan      chan [][]byte
	sdpReceived  bool
	waitingSDP   bool
}

// NewRTPClient は新しいRTPクライアントを作成
func NewRTPClient() *RTPClient {
	return &RTPClient{
		packetChan:  make(chan []byte, 100),
		nalChan:     make(chan [][]byte, 50),
		sdpReceived: false,
		waitingSDP:  true,
	}
}

// ParseSDP はSDP内容を解析してSDPInfo構造体を返す
func ParseSDP(sdpContent string) (*SDPInfo, error) {
	lines := strings.Split(strings.ReplaceAll(sdpContent, "\r\n", "\n"), "\n")
	
	info := &SDPInfo{
		PayloadType: 96, // デフォルト値
		ClockRate:   90000, // デフォルト値
	}
	
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		
		switch {
		case strings.HasPrefix(line, "c=IN IP4 "):
			info.Host = strings.TrimPrefix(line, "c=IN IP4 ")
		case strings.HasPrefix(line, "c=IN IP6 "):
			info.Host = strings.TrimPrefix(line, "c=IN IP6 ")
		case strings.HasPrefix(line, "m=video "):
			// m=video <port> RTP/AVP <payload_type>
			parts := strings.Fields(line)
			if len(parts) >= 4 {
				if port, err := strconv.Atoi(parts[1]); err == nil {
					info.Port = port
				}
				if pt, err := strconv.Atoi(parts[3]); err == nil {
					info.PayloadType = pt
				}
			}
		case strings.HasPrefix(line, "a=rtpmap:"):
			// a=rtpmap:96 H264/90000 または a=rtpmap:96 H265/90000
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				codecInfo := strings.Split(parts[1], "/")
				if len(codecInfo) >= 2 {
					info.CodecName = codecInfo[0]
					if clockRate, err := strconv.Atoi(codecInfo[1]); err == nil {
						info.ClockRate = clockRate
					}
				}
			}
		case strings.HasPrefix(line, "a=fmtp:"):
			// a=fmtp:96 packetization-mode=1 または a=fmtp:96 profile-id=1;level-id=93;tier-flag=0
			info.FmtpLine = strings.TrimPrefix(line, "a=fmtp:")
		}
	}
	
	if info.Host == "" || info.Port == 0 {
		return nil, fmt.Errorf("SDP解析エラー: ホストまたはポートが見つかりません")
	}
	
	log.Printf("RTP Client: SDP解析完了 - Host: %s, Port: %d, Codec: %s, PayloadType: %d", 
		info.Host, info.Port, info.CodecName, info.PayloadType)
		return info, nil
}

// ParseSDPFromRTPPacket は最初のRTPパケットからSDP情報を解析する
func (client *RTPClient) ParseSDPFromRTPPacket(packet []byte) (*SDPInfo, error) {
	if len(packet) < 12 {
		return nil, fmt.Errorf("パケットが小さすぎます")
	}
	
	// RTPヘッダーを解析
	version := (packet[0] >> 6) & 0x03
	if version != 2 {
		return nil, fmt.Errorf("無効なRTPバージョン: %d", version)
	}
	
	payloadType := packet[1] & 0x7F
	
	// ペイロードタイプからコーデックを推測
	codecName := "H264" // デフォルト
	clockRate := 90000  // デフォルト
	
	switch payloadType {
	case 96, 97:
		// H.264またはH.265の動的ペイロードタイプ
		// ペイロードを詳細に分析してコーデックを特定
		headerSize := 12 + int(packet[0]&0x0F)*4 // CSRC count
		if len(packet) > headerSize {
			payload := packet[headerSize:]
			if len(payload) > 0 {
				// H.264の場合、NALユニットタイプをチェック
				nalType := payload[0] & 0x1F
				if nalType >= 1 && nalType <= 23 {
					codecName = "H264"
				} else if len(payload) >= 2 {
					// H.265の場合、NALユニットタイプをチェック
					h265NalType := (payload[0] >> 1) & 0x3F
					if h265NalType >= 0 && h265NalType <= 63 {
						codecName = "H265"
					}
				}
			}
		}
	case 98, 99:
		codecName = "H265"
	default:
		// 標準的なペイロードタイプ
		codecName = "H264"
	}
	
	// デフォルトのSDP情報を作成
	info := &SDPInfo{
		Host:        "0.0.0.0", // デフォルト値、実際の接続で上書きされる
		Port:        0,         // デフォルト値、実際の接続で上書きされる
		CodecName:   codecName,
		PayloadType: int(payloadType),
		ClockRate:   clockRate,
		FmtpLine:    "", // 基本的なfmtp設定は後で追加
	}
	
	// コーデック固有のfmtp設定を追加
	switch codecName {
	case "H264":
		info.FmtpLine = fmt.Sprintf("%d packetization-mode=1", payloadType)
	case "H265":
		info.FmtpLine = fmt.Sprintf("%d profile-id=1;level-id=93;tier-flag=0", payloadType)
	}
	
	log.Printf("RTP Client: パケットからSDP情報を推測 - Codec: %s, PayloadType: %d", 
		codecName, payloadType)
	
	return info, nil
}

// Connect は指定されたSDP情報を使ってRTP接続を確立
func (client *RTPClient) Connect(sdpContent string) error {
	sdpInfo, err := ParseSDP(sdpContent)
	if err != nil {
		return fmt.Errorf("SDP解析エラー: %v", err)
	}
	
	client.sdpInfo = sdpInfo
	
	// UDPアドレスを解決
	udpAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", sdpInfo.Host, sdpInfo.Port))
	if err != nil {
		return fmt.Errorf("UDPアドレス解決エラー: %v", err)
	}
	
	// UDP接続を確立
	conn, err := net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		return fmt.Errorf("UDP接続エラー: %v", err)
	}
	
	client.conn = conn
	log.Printf("RTP Client: %s:%d に接続しました (Codec: %s)", sdpInfo.Host, sdpInfo.Port, sdpInfo.CodecName)
	log.Printf("RTP Client: ローカルアドレス: %s, リモートアドレス: %s", conn.LocalAddr(), conn.RemoteAddr())
	
	return nil
}

// ConnectAsServer はUDPサーバーとして動作し、最初のパケットでSDP情報を受信
func (client *RTPClient) ConnectAsServer(listenAddr string) error {
	// UDPアドレスを解決
	udpAddr, err := net.ResolveUDPAddr("udp", listenAddr)
	if err != nil {
		return fmt.Errorf("UDPアドレス解決エラー: %v", err)
	}
	
	// UDPサーバーを開始
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return fmt.Errorf("UDPサーバー開始エラー: %v", err)
	}
	
	client.conn = conn
	client.waitingSDP = true
	
	log.Printf("RTP Client: %s でUDPサーバーを開始しました（SDP情報を待機中）", listenAddr)
	
	return nil
}

// StartReceiving はRTPパケットの受信を開始
func (client *RTPClient) StartReceiving() error {
	if client.conn == nil {
		return fmt.Errorf("接続が確立されていません")
	}
	
	client.isRunning = true
	
	// RTPパケット受信ゴルーチン
	go client.receiveRTPPackets()
	
	// RTPパケット処理ゴルーチン
	go client.processRTPPackets()
	
	// WebRTC配信ゴルーチン
	go client.streamToWebRTC()
	
	log.Printf("RTP Client: パケット受信を開始しました")
	return nil
}

// receiveRTPPackets はUDPソケットからRTPパケットを受信
func (client *RTPClient) receiveRTPPackets() {
	buffer := make([]byte, 1500) // MTU考慮
	
	for client.isRunning {
		client.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		
		var n int
		var err error
		var remoteAddr net.Addr
				// サーバーモードかクライアントモードかで受信方法を変更
		if client.waitingSDP {
			// サーバーモードでの受信（送信者のアドレスを取得）
			n, remoteAddr, err = client.conn.ReadFromUDP(buffer)
		} else {
			// クライアントモードでの受信
			n, err = client.conn.Read(buffer)
		}
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				if client.sdpInfo != nil {
					log.Printf("RTP Client: 受信タイムアウト（5秒） - 送信側 %s からのパケットを待機中...", client.sdpInfo.Host)
				} else {
					log.Printf("RTP Client: 受信タイムアウト（5秒） - RTPパケットを待機中...")
				}
				continue // タイムアウトは無視
			}
			if !client.isRunning {
				break
			}
			log.Printf("RTP Client: パケット受信エラー: %v", err)
			continue
		}
		
		if n > 12 { // RTPヘッダーの最小サイズ
			// パケットをチャネルに送信（コピーを作成）
			packet := make([]byte, n)
			copy(packet, buffer[:n])
			
			// 最初のパケットでSDP情報を設定
			if client.waitingSDP && !client.sdpReceived {
				sdpInfo, err := client.ParseSDPFromRTPPacket(packet)
				if err != nil {
					log.Printf("RTP Client: パケットからのSDP解析エラー: %v", err)
					continue
				}
				
				// 送信者のアドレス情報を設定
				if remoteAddr != nil {
					if udpAddr, ok := remoteAddr.(*net.UDPAddr); ok {
						sdpInfo.Host = udpAddr.IP.String()
						sdpInfo.Port = udpAddr.Port
					}
				}
				
				client.sdpInfo = sdpInfo
				client.sdpReceived = true
				client.waitingSDP = false
				
				// 適切なコーデックモードを設定
				setCurrentCodec(strings.ToLower(client.sdpInfo.CodecName))
				
				log.Printf("RTP Client: SDP情報を受信しました - Host: %s, Port: %d, Codec: %s, PayloadType: %d", 
					sdpInfo.Host, sdpInfo.Port, sdpInfo.CodecName, sdpInfo.PayloadType)
			}
			
			// SDP情報が設定されている場合のみパケットを処理
			if client.sdpReceived {
				select {
				case client.packetChan <- packet:
				default:
					// チャネルが満杯の場合は古いパケットを破棄
					log.Printf("RTP Client: パケットチャネルが満杯です")
				}
			}
		}
	}
}

// processRTPPackets はRTPパケットを処理してNALユニットを抽出
func (client *RTPClient) processRTPPackets() {
	for client.isRunning {
		select {
		case packet := <-client.packetChan:
			nals := client.extractNALUnits(packet)
			if len(nals) > 0 {
				select {
				case client.nalChan <- nals:
				default:
					log.Printf("RTP Client: NALチャネルが満杯です")
				}
			}
		case <-time.After(1 * time.Second):
			// タイムアウトで継続
		}
	}
}

// extractNALUnits はRTPパケットからNALユニットを抽出
func (client *RTPClient) extractNALUnits(packet []byte) [][]byte {
	if len(packet) < 12 {
		return nil
	}
	
	// RTPヘッダーの解析
	// V(2) + P(1) + X(1) + CC(4) + M(1) + PT(7) + Sequence(16) + Timestamp(32) + SSRC(32)
	version := (packet[0] >> 6) & 0x03
	padding := (packet[0] >> 5) & 0x01
	extension := (packet[0] >> 4) & 0x01
	csrcCount := packet[0] & 0x0F
	marker := (packet[1] >> 7) & 0x01
	payloadType := packet[1] & 0x7F
		if version != 2 {
		log.Printf("RTP Client: 無効なRTPバージョン: %d", version)
		return nil
	}
	
	// sdpInfoがまだ設定されていない場合は、ペイロードタイプのチェックをスキップ
	if client.sdpInfo != nil && int(payloadType) != client.sdpInfo.PayloadType {
		log.Printf("RTP Client: 予期しないペイロードタイプ: %d (期待値: %d)", payloadType, client.sdpInfo.PayloadType)
		return nil
	}
	
	// ヘッダーサイズの計算
	headerSize := 12 + int(csrcCount)*4
	if extension == 1 && len(packet) > headerSize+4 {
		extensionLength := int(packet[headerSize+2])<<8 | int(packet[headerSize+3])
		headerSize += 4 + extensionLength*4
	}
	
	// パディングの処理
	payloadSize := len(packet) - headerSize
	if padding == 1 && payloadSize > 0 {
		paddingLength := int(packet[len(packet)-1])
		payloadSize -= paddingLength
	}
	
	if payloadSize <= 0 {
		return nil
	}
		payload := packet[headerSize : headerSize+payloadSize]
	
	// sdpInfoがまだ設定されていない場合は、ペイロードタイプから推測
	var codecName string
	if client.sdpInfo != nil {
		codecName = client.sdpInfo.CodecName
	} else {
		// ペイロードタイプから推測
		switch payloadType {
		case 96, 97:
			codecName = "H265" // デフォルトでH265と仮定
		case 98, 99:
			codecName = "H265"
		default:
			codecName = "H264"
		}
	}
	
	// コーデック別のNALユニット抽出
	switch codecName {
	case "H264":
		return client.extractH264NALs(payload, marker == 1)
	case "H265":
		return client.extractH265NALs(payload, marker == 1)
	default:
		log.Printf("RTP Client: サポートされていないコーデック: %s", codecName)
		return nil
	}
}

// extractH264NALs はH.264 RTPペイロードからNALユニットを抽出
func (client *RTPClient) extractH264NALs(payload []byte, isMarker bool) [][]byte {
	if len(payload) == 0 {
		return nil
	}
	
	nalType := payload[0] & 0x1F
	
	switch nalType {
	case 1, 5: // Single NAL Unit Packet (NON-IDR, IDR)
		nal := make([]byte, len(payload))
		copy(nal, payload)
		return [][]byte{nal}
		
	case 24: // STAP-A (Single Time Aggregation Packet)
		return client.parseSTAPA(payload[1:])
		
	case 28: // FU-A (Fragmentation Unit)
		return client.parseFUA(payload, isMarker)
		
	default:
		// その他のNALタイプも単一パケットとして処理
		nal := make([]byte, len(payload))
		copy(nal, payload)
		return [][]byte{nal}
	}
}

// extractH265NALs はH.265 RTPペイロードからNALユニットを抽出
func (client *RTPClient) extractH265NALs(payload []byte, isMarker bool) [][]byte {
	if len(payload) < 2 {
		return nil
	}
	
	nalType := (payload[0] >> 1) & 0x3F
	
	switch nalType {
	case 48: // Aggregation Packet (AP)
		return client.parseH265AP(payload[2:])
		
	case 49: // Fragmentation Unit (FU)
		return client.parseH265FU(payload, isMarker)
		
	default:
		// Single NAL Unit Packet
		nal := make([]byte, len(payload))
		copy(nal, payload)
		return [][]byte{nal}
	}
}

// parseSTAPA はSTAP-Aパケットを解析
func (client *RTPClient) parseSTAPA(payload []byte) [][]byte {
	var nals [][]byte
	offset := 0
	
	for offset < len(payload)-1 {
		if offset+2 > len(payload) {
			break
		}
		
		nalSize := int(payload[offset])<<8 | int(payload[offset+1])
		offset += 2
		
		if offset+nalSize > len(payload) {
			break
		}
		
		nal := make([]byte, nalSize)
		copy(nal, payload[offset:offset+nalSize])
		nals = append(nals, nal)
		
		offset += nalSize
	}
	
	return nals
}

// parseFUA はFU-Aパケットを解析（簡易実装）
func (client *RTPClient) parseFUA(payload []byte, isMarker bool) [][]byte {
	if len(payload) < 2 {
		return nil
	}
	
	fuIndicator := payload[0]
	fuHeader := payload[1]
	
	start := (fuHeader >> 7) & 0x01
	end := (fuHeader >> 6) & 0x01
	nalType := fuHeader & 0x1F
	
	if start == 1 {
		// フラグメントの開始
		nalHeader := (fuIndicator & 0xE0) | nalType
		nal := append([]byte{nalHeader}, payload[2:]...)
		
		if end == 1 || isMarker {
			// 単一フラグメント
			return [][]byte{nal}
		}
		// TODO: マルチフラグメント対応が必要
	}
	
	return nil
}

// parseH265AP はH.265 APパケットを解析
func (client *RTPClient) parseH265AP(payload []byte) [][]byte {
	var nals [][]byte
	offset := 0
	
	for offset < len(payload)-1 {
		if offset+2 > len(payload) {
			break
		}
		
		nalSize := int(payload[offset])<<8 | int(payload[offset+1])
		offset += 2
		
		if offset+nalSize > len(payload) {
			break
		}
		
		nal := make([]byte, nalSize)
		copy(nal, payload[offset:offset+nalSize])
		nals = append(nals, nal)
		
		offset += nalSize
	}
	
	return nals
}

// parseH265FU はH.265 FUパケットを解析（簡易実装）
func (client *RTPClient) parseH265FU(payload []byte, isMarker bool) [][]byte {
	if len(payload) < 3 {
		return nil
	}
	
	fuHeader := payload[2]
	start := (fuHeader >> 7) & 0x01
	end := (fuHeader >> 6) & 0x01
	
	if start == 1 {
		// フラグメントの開始
		nalType := fuHeader & 0x3F
		nalHeader := []byte{
			(payload[0] & 0x81) | (nalType << 1),
			payload[1],
		}
		nal := append(nalHeader, payload[3:]...)
		
		if end == 1 || isMarker {
			// 単一フラグメント
			return [][]byte{nal}
		}
		// TODO: マルチフラグメント対応が必要
	}
	
	return nil
}

// streamToWebRTC はNALユニットをWebRTCに配信
func (client *RTPClient) streamToWebRTC() {
	frameDuration := time.Second / 30 // デフォルト30FPS
	
	for client.isRunning {
		select {		case nals := <-client.nalChan:
			if len(nals) > 0 {
				// sdpInfoが設定されていない場合はスキップ
				if client.sdpInfo == nil {
					log.Printf("RTP Client: SDP情報が未設定のため、NALユニットをスキップします")
					continue
				}
				
				// 現在のコーデックに応じて適切な関数を呼び出し
				switch client.sdpInfo.CodecName {
				case "H264":
					writeNALsToTracks(nals, frameDuration)
				case "H265":
					writeNALsToTracksH265(nals, frameDuration)
				default:
					log.Printf("RTP Client: サポートされていないコーデック: %s", client.sdpInfo.CodecName)
				}
			}
		case <-time.After(1 * time.Second):
			// タイムアウトで継続
		}
	}
}

// Stop はRTPクライアントを停止
func (client *RTPClient) Stop() {
	client.isRunning = false
	
	if client.conn != nil {
		client.conn.Close()
		client.conn = nil
	}
	
	close(client.packetChan)
	close(client.nalChan)
	
	log.Printf("RTP Client: 停止しました")
}

// startRTPClient はRTP接続を開始する関数（既存のハンドラーと統合用）
func startRTPClient(inputURL string) {
	log.Printf("RTP Client: %s への接続を開始します", inputURL)
	
	// 接続テストを実行
	if strings.HasPrefix(inputURL, "rtp://") {
		address := strings.TrimPrefix(inputURL, "rtp://")
		log.Printf("RTP Client: 接続テストを実行中... (%s)", address)
		
		// 非ブロッキングでテストを実行
		go func() {
			if err := TestRTPConnection(address); err != nil {
				log.Printf("RTP Client: 接続テスト失敗: %v", err)
				log.Printf("RTP Client: 送信側が %s にRTPパケットを送信していることを確認してください", address)
			} else {
				log.Printf("RTP Client: 接続テスト成功！")
			}
		}()
	}
	
	client := NewRTPClient()
	defer client.Stop()
		// inputURLがSDPファイルパスの場合、ファイルから読み込み
	var sdpContent string
	if strings.HasSuffix(inputURL, ".sdp") {
		// SDPファイルから読み込み
		file, err := os.Open(inputURL)
		if err != nil {
			log.Printf("RTP Client: SDPファイル読み込みエラー: %v", err)
			return
		}
		defer file.Close()
		
		reader := bufio.NewReader(file)
		var lines []string
		for {
			line, err := reader.ReadString('\n')
			if err != nil && err != io.EOF {
				log.Printf("RTP Client: SDPファイル読み込みエラー: %v", err)
				return
			}
			if line != "" {
				lines = append(lines, strings.TrimSpace(line))
			}
			if err == io.EOF {
				break
			}
		}
		sdpContent = strings.Join(lines, "\r\n")
		log.Printf("RTP Client: SDPファイル読み込み完了: %s", inputURL)
	} else {
		// RTP URLから動的にSDP生成（既存のgenerateSDPContent関数を使用）
		codecName := "H264" // デフォルト
		if strings.Contains(strings.ToLower(inputURL), "h265") || strings.Contains(strings.ToLower(inputURL), "hevc") {
			codecName = "H265"
		}
				var err error
		sdpContent, err = generateSDPContent(inputURL, codecName)
		if err != nil {
			log.Printf("RTP Client: SDP生成エラー: %v", err)
			return
		}
		log.Printf("RTP Client: 動的SDP生成完了:\n%s", sdpContent)
	}
	
	// RTP接続を確立
	if err := client.Connect(sdpContent); err != nil {
		log.Printf("RTP Client: 接続エラー: %v", err)
		return
	}
	
	// 適切なコーデックモードを設定
	setCurrentCodec(strings.ToLower(client.sdpInfo.CodecName))
	
	// パケット受信を開始
	if err := client.StartReceiving(); err != nil {
		log.Printf("RTP Client: 受信開始エラー: %v", err)
		return
	}
		// サーバーが動作している間は待機
	select {}
}

// startRTPServer はRTPサーバーとして動作し、最初のパケットでSDP情報を受信する
func startRTPServer(listenAddr string) {
	log.Printf("RTP Server: %s でRTPサーバーを開始します（SDP情報を動的受信）", listenAddr)
	
	client := NewRTPClient()
	defer client.Stop()
	
	// UDPサーバーとして接続を確立
	if err := client.ConnectAsServer(listenAddr); err != nil {
		log.Printf("RTP Server: サーバー開始エラー: %v", err)
		return
	}
	
	// パケット受信を開始
	if err := client.StartReceiving(); err != nil {
		log.Printf("RTP Server: 受信開始エラー: %v", err)
		return
	}
	
	// サーバーが動作している間は待機
	select {}
}

// TestRTPConnection はRTP接続をテストする関数
func TestRTPConnection(address string) error {
	log.Printf("RTP Client: 接続テスト開始 - %s", address)
	
	// UDPアドレスを解決
	udpAddr, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		return fmt.Errorf("UDPアドレス解決エラー: %v", err)
	}
	
	// UDPサーバーとして待機
	conn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return fmt.Errorf("UDPサーバー開始エラー: %v", err)
	}
	defer conn.Close()
	
	log.Printf("RTP Client: %s でUDPパケット待機中...", address)
	
	// タイムアウト付きでパケット受信を試行
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	buffer := make([]byte, 1500)
	
	n, remoteAddr, err := conn.ReadFromUDP(buffer)
	if err != nil {
		if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
			return fmt.Errorf("30秒間パケットを受信できませんでした - 送信側が動作していない可能性があります")
		}
		return fmt.Errorf("パケット受信エラー: %v", err)
	}
	
	log.Printf("RTP Client: パケット受信成功！送信者: %s, サイズ: %d bytes", remoteAddr.String(), n)
	
	// RTPパケットの基本検証
	if n < 12 {
		return fmt.Errorf("受信したパケットが小さすぎます (RTPパケットではない可能性)")
	}
	
	version := (buffer[0] >> 6) & 0x03
	if version != 2 {
		return fmt.Errorf("無効なRTPバージョン: %d (期待値: 2)", version)
	}
	
	payloadType := buffer[1] & 0x7F
	log.Printf("RTP Client: 有効なRTPパケットを検出 - PayloadType: %d", payloadType)
	
	return nil
}