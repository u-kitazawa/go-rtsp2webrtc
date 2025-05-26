# RTSP Server モード トラブルシューティングガイド

## "could not write header (incorrect codec parameters?)" エラーについて

このエラーは主にサーバー側でH264コーデックパラメータを正しく検出できない場合に発生します。

### 主な原因

1. **H264フォーマットが見つからない**
   - SDPディスクリプション内にH264メディアが含まれていない
   - サポートされていないコーデック（H265、VP8など）を使用している

2. **コーデックパラメータが不正**
   - SPS/PPSヘッダーが不正または欠落している
   - パケタイゼーションモードが対応していない

3. **SDPフォーマットの問題**
   - 不正なSDPディスクリプション
   - メディアタイプの指定が不正

### デバッグ方法

1. **サーバーログの確認**
   ```
   RTSP server: 受信したSDP内容:
     メディア 0: タイプ=video
       フォーマット 0: *format.H264
   ```

2. **FFmpegでのテスト送信**
   ```bash
   # 正しい例（H264）
   ffmpeg -re -i input.mp4 -c:v libx264 -preset ultrafast -tune zerolatency -f rtsp rtsp://localhost:554/stream

   # プロファイルとレベルを明示的に指定
   ffmpeg -re -i input.mp4 -c:v libx264 -profile:v baseline -level 3.1 -preset ultrafast -f rtsp rtsp://localhost:554/stream
   ```

3. **GStreamerでのテスト送信**
   ```bash
   gst-launch-1.0 videotestsrc ! videoconvert ! x264enc tune=zerolatency ! h264parse ! rtph264pay ! udpsink host=localhost port=554
   ```

### よくある解決策

1. **H264コーデックの明示的指定**
   ```bash
   ffmpeg -i input.mp4 -c:v libx264 -c:a aac -f rtsp rtsp://localhost:554/stream
   ```

2. **SPS/PPSの強制生成**
   ```bash
   ffmpeg -i input.mp4 -c:v libx264 -x264-params keyint=30:min-keyint=30 -f rtsp rtsp://localhost:554/stream
   ```

3. **パケタイゼーションモードの指定**
   ```bash
   ffmpeg -i input.mp4 -c:v libx264 -rtsp_transport tcp -f rtsp rtsp://localhost:554/stream
   ```

### サポートされているコーデック

- **映像**: H264のみ
- **音声**: 現在未対応
- **コンテナ**: RTSP/RTP

### ログ解析

- `H264 media not found`: SDPにH264フォーマットが含まれていません
- `H264デコーダーの作成に失敗`: コーデックパラメータが不正です
- `RTPデコードエラー`: RTPパケットの復号化に失敗しました

### 確認項目チェックリスト

- [ ] 入力ストリームがH264エンコードされているか
- [ ] FFmpegのエンコード設定が正しいか
- [ ] サーバーがポート554でリッスンしているか
- [ ] ファイアウォールがポート554を許可しているか
- [ ] 複数のクライアントが同時接続していないか

### 追加のトラブルシューティング

#### RTSPクライアントツールでの確認
```bash
# VLCメディアプレーヤーでテスト
vlc rtsp://localhost:554/stream

# FFplayでテスト
ffplay rtsp://localhost:554/stream

# OpenRTSPでテスト
openRTSP rtsp://localhost:554/stream
```

#### Wiresharkでのパケットキャプチャ
1. Wiresharkを起動
2. localhost インターフェースを選択
3. フィルタ: `rtsp or rtp`
4. ANNOUNCEリクエストのSDPペイロードを確認

## その他のエラーパターン

### "Server returned 404 not found"
- 間違ったパスを指定している可能性があります
- 正しいパス: `rtsp://localhost:554/stream`

### "Connection refused"
- サーバーが起動していない
- ポート番号が間違っている（デフォルト: 554）

### "Timeout"
- ネットワーク接続の問題
- ファイアウォールの設定を確認
