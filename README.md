# GO RTSP 2 WebRTC

## 概要

GO RTSP 2 WebRTCは、RTSPストリームをWebRTCストリームに変換するためのツールです。これにより、RTSPカメラやRTSPサーバーからの映像をWebRTCを介してブラウザで視聴できるようになります。
現在、このサーバー単体で100ms程度の遅延で映像を配信することが可能です。

## 種類

### `rtsp-webrtc` 
RTSPストリームをWebRTCストリームに変換するサーバーです。

### `rtsp-webrtc-webcodecs` 
RTSPストリームをWebRTCストリームに変換するサーバーです。WebCodecs APIを使用して、ブラウザ側でデコードします。
webCodecsは対応ブラウザが限られているため、注意が必要です。
また、webRTCのDataChannelを使用して、映像データを送信します。

### `webrtc-dc-rtsp-webrtc`（実装予定）
WebRTCのDataChannelを使用して、RTSPストリームの映像データを受信し、WebRTCストリームに変換するサーバーです。RTSPストリームをAndroid端末などで転送する必要があるときに使用します。

## 実行

### 環境
- Windows 10/11
- Go 1.20以上
- FFmpeg

### FFmpegのインストール

FFmpegは公式サイトからダウンロードできます。
[FFmpegの公式サイト](https://ffmpeg.org/download.html)

### ビルド済みバイナリの実行

コマンドラインから以下のコマンドを実行します。

```bash
.rtsp-webrtc/rtsp-webrtc-server.exe 
```

### 引数

- `-h` : ヘルプを表示します。
- `-rtsp-url` : RTSPストリームのURLを指定します。例:`rtsp://example.com/stream`
- `-port`: サーバーのポート番号を指定します。デフォルトは`8080`です。

## 開発

### Goのインストール
Goをインストールするには、以下の手順に従ってください。

1. [Goの公式サイト](https://go.dev/dl/)から最新のGoをダウンロードします。
2. ダウンロードしたインストーラーを実行し、指示に従ってインストールします。
3. インストールが完了したら、コマンドラインで以下のコマンドを実行して、Goが正しくインストールされたことを確認します。

```bash
go --version
```
4. Goのバージョンが表示されれば、インストールは成功です。

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

# 実行
./rtsp-webrtc.exe
```
