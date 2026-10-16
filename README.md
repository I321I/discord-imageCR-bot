# discord-imageCR-bot

圖庫 Discord bot：在設定為「圖庫頻道」的頻道中，自動辨識使用者貼的圖片（trace.moe 查作品、RapidOCR 讀圖片文字，支援中文與日文），重新發成帶有辨識結果的圖片訊息；使用者可用 `/search` 搜尋並貼出圖庫中的圖片。

用 Go 與 [discordgo](https://github.com/bwmarrin/discordgo) 撰寫，OCR 由同一容器內常駐的 Python 子行程（`ocr/ocr_worker.py`）執行。部署方式為單一 Docker 映像、單一容器，目標機器是 Raspberry Pi 3B（arm64、Debian 13、1GB RAM）。

## 指令

| 指令 | 說明 |
|---|---|
| `/channel add \| remove \| list` | 設定此伺服器的圖庫頻道（需要「管理伺服器」權限） |
| `/search` | 搜尋圖庫圖片並貼出（伺服器、私訊皆可用） |
| `/sources` | 勾選 `/search` 要搜尋的圖庫頻道 |
| 右鍵訊息 →「編輯圖片訊息」 | 修改圖片訊息的作品、圖片文字、角色、關鍵字 |
| 右鍵訊息 →「重新辨識」 | 重新辨識失敗的欄位，或重新處理帶 ❌ 的原訊息 |
| `/storage show \| reload \| save` | 儲存頻道管理（開發者專用，只註冊在 `SERVER_ID` 伺服器） |

## 環境變數

在 Docker 中以 `docker run --env-file .env` 傳入；`.env` 不會、也不可以放進映像（`.dockerignore` 已排除），所以映像內沒有 `.env`，bot 啟動時記一行「沒有 .env 檔案，直接讀取環境變數」屬正常。直接在主機執行 bot（不用 Docker）時，bot 會讀取工作目錄的 `.env`；檔案存在但無法載入時才會印警告。

| 變數 | 必填 | 預設 | 用途 |
|---|---|---|---|
| `TOKEN` | 是 | | Discord bot token。 |
| `STORAGE_CHANNEL_ID` | 是 | | 儲存頻道 ID。bot 把設定（圖庫頻道、各使用者勾選的搜尋頻道）以訊息寫在這個頻道，重啟後從這裡讀回。建議用只有 bot 與開發者看得到的私人頻道。 |
| `SERVER_ID` | 否 | | 開發者伺服器 ID，只用來註冊開發者指令 `/storage`；未設定則不註冊。 |
| `TRACEMOE_KEY` | 否 | （不帶 key） | [trace.moe](https://trace.moe) API key，用來提高查詢額度。 |
| `TRACEMOE_MIN_SIMILARITY` | 否 | `0.90` | trace.moe 結果的最低相似度（0～1），低於此值視為找不到作品。 |
| `MEMBER_CACHE_TTL` | 否 | `5m` | 成員資格（是否在伺服器、身分組）的快取時間，Go duration 格式（例如 `30s`、`5m`、`1h`）；`0` 表示不快取。 |

`.env` 範例：

```
TOKEN=你的 bot token
STORAGE_CHANNEL_ID=123456789012345678
SERVER_ID=123456789012345678
```

注意：`docker run --env-file` 不會去掉引號，值請**不要**加引號（`TOKEN="xxx"` 會把引號當成 token 的一部分）。

映像內已設定、通常不需要改的變數：`GOMEMLIMIT=150MiB`（讓 Go GC 在小記憶體機器上積極回收）。

## Discord 開發者後台設定

在 [Discord Developer Portal](https://discord.com/developers/applications) 的應用程式中：

1. **Bot** 頁：開啟 **Message Content Intent**（必須，否則讀不到訊息內容與附件）。不需要 Server Members Intent 與 Presence Intent。
2. **Installation** 頁：**同時啟用 Guild Install（伺服器安裝）與 User Install（使用者安裝）**。
   - 伺服器安裝：scopes `bot`、`applications.commands`，權限勾選：
     - 檢視頻道（View Channels）
     - 傳送訊息（Send Messages）
     - 附加檔案（Attach Files）
     - 讀取訊息歷史（Read Message History）
     - 管理訊息（Manage Messages）
     - 新增反應（Add Reactions）
   - 使用者安裝：scope `applications.commands`（讓使用者在任何地方用 `/search`、`/sources`）。
3. 用 Installation 頁的連結把 bot 加入伺服器；使用者另外以「使用者安裝」把 bot 加到自己的帳號。
4. 儲存頻道（`STORAGE_CHANNEL_ID`）也要讓 bot 有檢視頻道、傳送訊息、讀取訊息歷史權限。

## 建置與執行（在 Raspberry Pi 上直接建置）

需要 Docker 與 BuildKit（`docker buildx`）。用官方安裝腳本（`curl -fsSL https://get.docker.com | sh`）裝的 Docker 已包含；用 Debian 套件 `docker.io` 安裝的話，另外安裝 `docker-buildx`。舊式建置器（沒有 BuildKit）會連 `test` 階段一起建、在 Pi 上跑完整測試，請勿使用。

```sh
git clone <此 repo> imagebot && cd imagebot
docker build -t imagebot .
```

不加 `--target` 時建的是正式映像（Dockerfile 最後一個階段即正式映像），不會建 `test` 階段、也不會執行測試。

Go 程式在建置階段以交叉編譯產生（在 Pi 上即為原生編譯）；Python 套件（onnxruntime、OpenCV 等）都有 arm64 預編譯版本，不需要在 Pi 上編譯。建置時會下載 RapidOCR 日文辨識模型與字典並驗證 sha256，校驗不符時建置失敗。映像約 500MB 以上。

執行：

```sh
docker run -d --name imagebot \
  --env-file .env \
  --restart unless-stopped \
  imagebot
```

- 看 log：`docker logs -f imagebot`
- 看記憶體：`docker stats imagebot`
- 停止：`docker stop imagebot`（bot 收到 SIGTERM 會正常關閉 OCR worker）
- 更新：`git pull && docker build -t imagebot . && docker rm -f imagebot`，再執行上面的 `docker run`。

確認映像內沒有 `.env`：

```sh
docker run --rm --entrypoint ls imagebot -a /app /app/ocr /app/ocr/models
```

確認 OCR 套件可載入：

```sh
docker run --rm --entrypoint python3 imagebot -c "import rapidocr_onnxruntime, cv2, opencc"
```

## 另一種建置方式：在 PC 建 arm64 映像再傳到 Pi

Pi 上建置較慢時，可在 PC（x86_64）用 buildx 建 arm64 映像。Go 部分在 PC 原生交叉編譯；Python 階段在 QEMU 模擬下執行，較慢但只需做一次。

```sh
# PC 上（第一次需要安裝 QEMU 模擬器）
docker run --privileged --rm tonistiigi/binfmt --install arm64
docker buildx build --platform linux/arm64 -t imagebot --load .

# 傳到 Pi（pi 為 ssh 主機名稱）
docker save imagebot | gzip | ssh pi 'gunzip | docker load'
```

之後在 Pi 上照上一節的 `docker run` 執行。

## 測試

本機（不需 Docker）：

```sh
go vet ./... && go vet -tags rapidocr ./...
go test -timeout 120s -count=1 ./...
CGO_ENABLED=1 go test -timeout 900s -race -count=1 ./...
python3 -m unittest ocr/test_ocr_worker.py ocr/test_gate_ocr_worker.py
```

完整測試（含 `-race`、Python 單元測試，以及以 RapidOCR 實際辨識真實截圖的整合測試）在 Docker 的 `test` 階段執行，建議在 PC 上跑（不必是 arm64）：

```sh
docker build --target test --progress=plain .
```

整合測試使用的真實截圖（`internal/recognize/testdata/sub_*`、`manga_*`、`tv_*`）有版權，不在 git 中；只要放在本機的該目錄就會一起送進建置，缺圖時該項測試略過。整合測試的辨識結果與耗時會印在建置輸出中（`--progress=plain` 才看得到完整輸出）。

## Raspberry Pi 記憶體建議

Pi 3B 只有 1GB RAM，OCR worker 辨識時峰值約 600MB。

- 開啟 zram（Debian 的 `zram-tools` 或 `systemd-zram-generator`）即足夠；目前環境為 905MB RAM + 905MB zram。
- 想多一層保險可另加 swapfile（例如 1GB）：

  ```sh
  sudo fallocate -l 1G /swapfile && sudo chmod 600 /swapfile
  sudo mkswap /swapfile && sudo swapon /swapfile
  echo '/swapfile none swap sw 0 0' | sudo tee -a /etc/fstab
  ```

- OCR worker 閒置 5 分鐘後會自動結束以釋放記憶體，下一張圖來時再啟動（多約 3 秒）。
