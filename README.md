# RTSP 2 WebRTC

## 概要

GO RTSP 2 WebRTC は、RTSP ストリームを WebRTC ストリームに変換するためのツールです。これにより、RTSP カメラや RTSP サーバーからの映像を WebRTC を介してブラウザで視聴できるようになります。
現在、このサーバー単体で 100ms 程度の遅延で映像を配信することが可能です。

## 種類

### `rtsp-webrtc`

RTSP ストリームを WebRTC ストリームに変換するサーバーです。標準的な WebRTC 機能を使用して映像を配信します。

### `rtsp-webrtc-webcodecs`

RTSP ストリームを WebRTC ストリームに変換するサーバーです。WebCodecs API を使用して、ブラウザ側でデコードします。
webCodecs は対応ブラウザが限られているため、注意が必要です。
また、webRTC の DataChannel を使用して、映像データを送信します。

### `webrtc-dc-rtsp-webrtc`（実装予定）

WebRTC の DataChannel を使用して、RTSP ストリームの映像データを受信し、WebRTC ストリームに変換するサーバーです。RTSP ストリームを Android 端末などで転送する必要があるときに使用します。

## 実行

### 環境

- Windows 10/11
- Go 1.20 以上
- FFmpeg

### FFmpeg のインストール

FFmpeg は公式サイトからダウンロードできます。
[FFmpeg の公式サイト](https://ffmpeg.org/download.html)

### ビルド済みバイナリの実行

コマンドラインから以下のコマンドを実行します。

```bash
./rtsp-webrtc/rtsp-webrtc-server.exe -input-url "rtsp://example.com/stream"
```

### 引数

- `-h` : ヘルプを表示します。
- `-input-url` : RTSP ストリームの URL または RTP SDP ファイルパスを指定します。例:`rtsp://example.com/stream`
- `-port`: サーバーのポート番号を指定します。デフォルトは`8080`です。
- `-codec`: 入力に使用するコーデックを指定します。`h264`または`h265`が指定可能です。デフォルトは`h264`です。
- `-processor`: H.265 トランスコーディングに使用するプロセッサを指定します。`cpu`または`gpu`が指定可能です。デフォルトは`gpu`です。
- `-input-type`: 入力タイプを指定します。`rtsp`、`rtp`、または`server`が指定可能です。デフォルトは`rtsp`です。
- `-use-gortsplib`: RTSP パススルーに gortsplib を使用するかどうかを指定します。`true`または`false`が指定可能です。デフォルトは`false`です。
- `-output-mode`: 出力モードを指定します。`webrtc`または`webcodecs`が指定可能です。デフォルトは`webrtc`です。

## 開発

### Go のインストール

Go をインストールするには、以下の手順に従ってください。

1. [Go の公式サイト](https://go.dev/dl/)から最新の Go をダウンロードします。
2. ダウンロードしたインストーラーを実行し、指示に従ってインストールします。
3. インストールが完了したら、コマンドラインで以下のコマンドを実行して、Go が正しくインストールされたことを確認します。

```bash
go --version
```

4. Go のバージョンが表示されれば、インストールは成功です。

### リポジトリのクローン

```bash
git clone

cd go-rtsp2webrtc
```

### 依存関係のインストール

```bash
# ビルドしたいディレクトリに移動
cd rtsp-webrtc

# 依存関係をインストール
go get

go mod tidy

# ビルド
go build -o rtsp-webrtc-server.exe .

# 実行例（基本的な使用法）
./rtsp-webrtc-server.exe -input-url "rtsp://example.com/stream"

# 実行例（すべてのオプションを指定）
./rtsp-webrtc-server.exe -input-url "rtsp://example.com/stream" -port 8080 -codec h264 -processor gpu -input-type rtsp -use-gortsplib false -output-mode webrtc

# 実行例（RTSPサーバーモード）
./rtsp-webrtc-server.exe -input-type server -use-gortsplib true
```
