# RTP Client Usage Guide

このドキュメントでは、新しく実装されたRTPクライアント機能の使用方法について説明します。

## 概要

RTPクライアント機能は、SDP（Session Description Protocol）情報を使用してRTPストリームに接続し、H.264/H.265ビデオストリームをWebRTCに配信する機能です。

## 使用方法

### 1. SDPファイルを使用した接続

```bash
# H.264 RTPストリームの場合
./rtsp2webrtc.exe -input-url="stream.sdp" -input-type="rtp" -codec="h264" -use-gortsplib="false"

# H.265 RTPストリームの場合
./rtsp2webrtc.exe -input-url="stream.sdp" -input-type="rtp" -codec="h265" -use-gortsplib="false"
```

### 2. RTP URLを使用した接続

```bash
# H.264 RTPストリーム
./rtsp2webrtc.exe -input-url="rtp://192.168.1.100:5004" -input-type="rtp" -codec="h264" -use-gortsplib="false"

# H.265 RTPストリーム
./rtsp2webrtc.exe -input-url="rtp://192.168.1.100:5004" -input-type="rtp" -codec="h265" -use-gortsplib="false"
```

## SDP形式

### H.264の場合

```
v=0
o=- 0 0 IN IP4 192.168.1.100
s=H264 RTP Stream
c=IN IP4 192.168.1.100
t=0 0
m=video 5004 RTP/AVP 96
a=rtpmap:96 H264/90000
a=fmtp:96 packetization-mode=1
```

### H.265の場合

```
v=0
o=- 0 0 IN IP4 192.168.1.100
s=H265 RTP Stream
c=IN IP4 192.168.1.100
t=0 0
m=video 5004 RTP/AVP 96
a=rtpmap:96 H265/90000
a=fmtp:96 profile-id=1;level-id=93;tier-flag=0
```

## クライアント実装仕様

### 主要コンポーネント

1. **SDPParser**: SDP情報を解析してメディア情報を抽出
2. **RTPReceiver**: UDPソケットからRTPパケットを受信
3. **NALExtractor**: RTPペイロードからNALユニットを抽出
4. **WebRTCStreamer**: NALユニットをWebRTCトラックに配信

### サポートされている機能

- **H.264 Single NAL Unit Packet**: 単一NALユニットパケット
- **H.264 STAP-A**: 複数NALユニット集約パケット
- **H.264 FU-A**: フラグメンテーションユニット（基本対応）
- **H.265 Single NAL Unit Packet**: 単一NALユニットパケット
- **H.265 AP**: 集約パケット
- **H.265 FU**: フラグメンテーションユニット（基本対応）

### 制限事項

1. **フラグメンテーション**: 複数パケットにまたがるフラグメンテーションは基本実装のみ
2. **エラー処理**: パケットロスやエラー回復は最小限
3. **タイミング**: 固定30FPSでの配信（SDPからの動的検出は未実装）

## Android端末からの送信例

Android端末から本RTPクライアントに接続する場合：

```kotlin
// UDPソケットでSDPを送信
val socket = DatagramSocket()
val sdpContent = """
v=0
o=android 123456 123456 IN IP4 192.168.1.50
s=Android RTP Stream
c=IN IP4 192.168.1.100
t=0 0
m=video 5004 RTP/AVP 96
a=rtpmap:96 H265/90000
a=fmtp:96 profile-id=1;level-id=93;tier-flag=0
a=sendonly
""".trimIndent()

// RTPパケットの送信
val rtpPacket = buildRTPPacket(nalUnit, sequenceNumber, timestamp)
socket.send(DatagramPacket(rtpPacket, rtpPacket.size, 
    InetAddress.getByName("192.168.1.100"), 5004))
```

## トラブルシューティング

### よくある問題

1. **接続エラー**: UDPポートが開いているか確認
2. **パケット受信なし**: ファイアウォール設定を確認
3. **映像が表示されない**: NALユニット抽出エラーをログで確認

### デバッグログ

```
RTP Client: SDP解析完了 - Host: 192.168.1.100, Port: 5004, Codec: H264, PayloadType: 96
RTP Client: 192.168.1.100:5004 に接続しました (Codec: H264)
RTP Client: パケット受信を開始しました
```

## 今後の改善予定

1. **高度なフラグメンテーション処理**
2. **動的フレームレート検出**
3. **パケットロス対応**
4. **多重ストリーム対応**
5. **音声ストリーム対応**
