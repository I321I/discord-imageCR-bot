# discord-imageCR-bot

用 Go 與 [discordgo](https://github.com/bwmarrin/discordgo) 撰寫的 Discord bot。

## 專案概況

動畫截圖圖庫 bot：在「圖庫頻道」（來源頻道）自動辨識使用者貼的圖片（圖片附件或圖片網址；trace.moe 查作品、RapidOCR 讀圖片文字），重新發成「圖片＋固定格式文字」的圖片訊息並刪除原訊息；使用者可用 `/search` 在任何地方搜尋並貼出圖片。設計與規劃：`docs/superpowers/specs/2026-10-12-image-bot-design.md`、`docs/plans/2026-10-12-image-bot.md`；圖片網址支援：`docs/plans/2026-10-19-image-url.md`；實測修正：`docs/plans/2026-10-28-fixes.md`。

- 語言與依賴：Go 1.27（module `discordbot`）、discordgo v0.29.0、godotenv、`golang.org/x/image`；OCR worker 為 Python 3.12（`ocr/requirements.txt` 全部鎖定版本）。
- 全部免費：不得引入付費 API。

### 套件結構

| 位置 | 責任 |
|---|---|
| `bot.go` | `main`：讀設定、建 session、組裝各元件、註冊事件 handler 與指令、等待結束訊號 |
| `internal/config` | 讀環境變數 |
| `internal/store` | 設定資料模型、分段編碼、讀寫儲存頻道（mutex 保護） |
| `internal/imagemsg` | 圖片訊息的格式化、解析、分隔符號正規化、2000 字截斷 |
| `internal/recognize` | 附件判定與解碼、訊息中的圖片清單（附件＋圖片網址，`imageurl.go`）、trace.moe、OCR 前處理與組行、RapidOCR worker 管理、排隊（含優先） |
| `internal/pipeline` | 來源頻道新訊息的佇列與處理流程（下載、辨識、發出、刪原訊息、⏳／❌）、等待預覽與訊息更新觸發、防 SSRF 的網址下載器 `URLFetcher`（`urlfetch.go`） |
| `internal/index` | 記憶體搜尋索引（Key `c:m:N` 附件圖、`c:m:uN` 網址圖）、啟動掃描與補處理、訊息刪除／編輯時更新（部分更新會重讀完整訊息） |
| `internal/access` | 頻道權限計算（移植 discordgo）、成員快取、可用頻道判定 |
| `internal/commands` | interaction 分派與各指令 |
| `internal/snowflake` | snowflake 數值比較 |
| `internal/discordtest` | 測試用假 Discord |
| `ocr/ocr_worker.py` | Python OCR worker（RapidOCR 中文＋日文模型、OpenCC `s2twp`） |

外部依賴（Discord、HTTP、trace.moe、OCR）都經由各套件自己定義的小介面，`*discordgo.Session`／`*discordgo.State` 直接滿足；測試一律用假實作，`go test ./...` 不連網、不需要 Python。

### 指令

- `/channel add | remove | list`：設定此伺服器的圖庫頻道（伺服器安裝、需「管理伺服器」，回覆 ephemeral）
- `/search`：搜尋並公開貼出圖片（使用者安裝，伺服器與私訊皆可，autocomplete）；網址圖不重新上傳，見下方「圖片網址」
- `/sources`：勾選 `/search` 要搜尋的圖庫頻道（使用者安裝）
- 右鍵訊息「編輯圖片訊息」「重新辨識」（伺服器安裝；對帶 ❌ 的原訊息「重新辨識」會重新處理整則）
- `/storage show | reload | save`：開發者專用，只註冊在 `SERVER_ID` 伺服器
- 指令啟動時以 bulk overwrite 同步，關機時不刪除。

### 環境變數（`.env` 或 `docker run --env-file`）

- 必填：`TOKEN`、`STORAGE_CHANNEL_ID`
- 選填：`SERVER_ID`（只用來註冊 `/storage`）、`TRACEMOE_KEY`、`TRACEMOE_MIN_SIMILARITY`（預設 0.90）、`MEMBER_CACHE_TTL`（Go duration，預設 `5m`）
- 選填變數格式錯誤時停止啟動。
- Gateway intents：Guilds、Guild Messages、Message Content（不開 Server Members）。

### 資料存放

- 設定（各伺服器的來源頻道與設定時間點、各使用者勾選的頻道）以 JSON 存成**儲存頻道中的訊息**，每次變動發一則新的完整版本，超過 2000 字拆成多則；不得寫檔。啟動時讀最新完整版本，讀取失敗則記 log 以空資料啟動。
- 設定資料另有全域欄位 `url_since`（圖片網址功能上線時間點，snowflake）：設定成功讀取時於第一次 READY 後寫入，值為寫入儲存頻道時 Discord 回傳的訊息 ID（不用本機時鐘）；寫入失敗或以空資料啟動時不寫。
- 圖片資料就是頻道中的圖片訊息本身；搜尋索引只在記憶體，啟動（及重新 IDENTIFY）時讀頻道歷史重建，並補處理離線期間的圖片。

### 圖片網址

- 圖片網址：訊息文字中的 http/https 連結，路徑副檔名為 png/jpg/jpeg/webp/gif，或 Discord 預覽為圖片（type image/gifv，或預覽的 image/thumbnail 就是該網址）；`<網址>`、`||網址||`、`[文字](網址)` 都算，同一網址算一張；附件在前、網址在後一起編號。一般網頁連結不處理；含非圖片附件的訊息整則不處理。搜尋文字一律去掉圖片網址。
- 新舊區分（`store.IsNewMessage`）：有附件的訊息只看設定時間點；只含圖片網址的看 max(設定時間點, `url_since`)。舊的只收進搜尋。
- 新訊息：下載網址圖（gifv 不處理），須能解碼成 PNG/JPEG/GIF/WEBP，重新上傳；任一張失敗一張都不發、加 ❌。
- 看圖網頁（2026-10-28）：網址下載結果為 `ErrNotImage`（實際是網頁）時，若 Discord 對該網址有 type image 預覽（`recognize.PreviewImageURL`，不用 gifv），改經同一個 `URLFetcher` 下載預覽的 image（優先）或 thumbnail 網址；該網址就是原網址時改用其 `ProxyURL`（media.discordapp.net）。尚無預覽時等 5 秒重讀一次再找；重讀發現原訊息已刪除 → 同 `ErrNoImages`。檔名沿用原網址。沒有預覽或預覽也失敗 → 照舊加 ❌。其他錯誤（過大、已刪除、連線錯誤）不改抓預覽。`/search` 不受影響。含尚無預覽的連結時等 5 秒重讀；MESSAGE_UPDATE（預覽補上、編輯加網址）也會觸發。重讀後沒有圖片 → 不發、不刪、不加 ❌（`ErrNoImages`）。
- `URLFetcher` 安全規則（不可放寬）：只 http/https、不帶帳密；只連公開位址的 80、443，在 `net.Dialer.Control` 以實際連線 IP 檢查（含每一跳轉址，最多 5 跳；擋私有／保留／內嵌 IPv4 的 IPv6 過渡位址）；`Proxy: nil`；25MiB、逾時 2 分鐘。測試用 `newURLFetcher(check)` 放行 httptest。
- `/search` 網址圖：同附件排隊；重讀訊息確認網址仍在原位置，以 `URLFetcher.Check`（15 秒）檢查：確定已刪除（404/410/imgur removed.png）或確定不是圖片 → 回「圖床上的圖片已被刪除」並移除索引；內容確定為四種點陣格式 → 貼出網址（不重新上傳，mention 不通知）；其他（無法確認，`ErrUnconfirmedImage` 等）→ 只有 Discord 對該網址有圖片預覽才貼出，否則回「目前無法確認圖片」（防釣魚）。

### 處理細節（2026-10-28）

- 附件下載：只在 `Size` > 25MiB 時提早拒絕，不再比對下載長度與 `Size`（Discord 回報的 Size 可能與實際不同）；`HTTPFetcher` 改以 HTTP 回應的 Content-Length（≥ 0 時）偵測截斷（`ErrSizeMismatch`），自動解壓或 chunked（-1）不比對。自動處理與 `/search` 共用。
- 刪除原訊息遇 Unknown Message（code 10008）視為已刪除成功（記一行 log、不加 ❌）；加／移除反應遇 10008 只記一行短 log。只認 10008，其他錯誤（403 等）仍是處理失敗。
- OCR 改辨識整張的條件另加：字幕區有任何（非空白）文字框 `y.lo ≤ 2`（縮放後字幕區像素，`subtitleTopEdgePx`），表示切到畫面中的文字（如直書漫畫半欄）。
- 作品名稱（取代舊的多名稱組法）：有中文時只放一個——`title.chinese` 優先，其次 synonyms 中第一個中文別名；沒有中文時放 native、english（「、」連接、去重）；兩者都沒有才用 romaji。不做簡轉繁（`title.chinese` 可能是簡體，用「編輯圖片訊息」修正）。

### OCR worker

- Go（`internal/recognize/rapidocr.go`）以子行程常駐執行 `python3 /app/ocr/ocr_worker.py`，stdin/stdout 每行一個 JSON；第一張圖才啟動、閒置 5 分鐘結束、每張逾時 3 分鐘、出錯即殺掉並在下一張重啟。
- OCR 與 trace.moe 各自排隊（並行數 1，「重新辨識」優先）；原訊息一次只處理一則、同時只解碼一張原圖（Pi 記憶體小）。
- OCR 只在 Docker 映像內可用；本機（Alpine/musl）無法安裝 onnxruntime，容器外執行時圖片文字欄會標辨識失敗。

### Docker 部署

- 單一映像、單一容器，目標 Raspberry Pi 3B（linux/arm64）。`Dockerfile` 階段：`gobuild`（交叉編譯）、`runtime`（python:3.12-slim、日文模型 sha256 驗證、非 root、`GOMEMLIMIT=150MiB`）、`test`；預設目標為正式映像。
- 建置：`docker build -t imagebot .`；執行：`docker run -d --env-file .env --restart unless-stopped imagebot`。詳見 `README.md`。
- `.env`、`spike/`、`docs/` 不進建置 context。有版權的真實截圖（`internal/recognize/testdata/sub_*`、`manga_*`、`tv_*`）不進 git。

### 建置與測試

```sh
go build ./... && go vet ./... && go vet -tags rapidocr ./...
go test ./...
CGO_ENABLED=1 go test -race ./...                                    # 需要 gcc（本機 apk add gcc musl-dev）
python3 -m unittest ocr/test_ocr_worker.py ocr/test_gate_ocr_worker.py
docker build --target test --progress=plain .                        # 以上全部＋ -tags rapidocr 真實截圖整合測試
```

`go test -tags rapidocr ./internal/recognize/` 需要 Python 與 RapidOCR，只在 Docker `test` 階段執行；缺測試圖片時略過。

## 開發團隊流程

開發任務由主對話擔任 **PM**，搭配 `.claude/agents/` 中的三個角色：

| 角色 | agent | 負責 |
|---|---|---|
| PM | 主對話 | 與使用者溝通、派工、轉述結果、撰寫報告 |
| 規劃者 | `planner` | 產出規劃、列出待決定問題；最後 review；必要時更新本檔 |
| 實作者 | `implementer` | 依規劃以 TDD 撰寫程式 |
| 把關者 | `gatekeeper` | 先提出潛在問題，再寫測試、跑測試，決定通過或退回 |

### 步驟

1. **確認需求**：PM 使用 `superpowers:brainstorming` 與使用者確認需求。
2. **規劃**：派 `planner` 產出規劃與待決定問題清單。
3. **開跑前一次問完**：PM 把規劃者與自己無法決定的問題，**一次全部**問使用者。使用者回答完才開始實作。
4. **實作**：派 `implementer`。
5. **把關**：派 `gatekeeper`。被退回就交給 `implementer` 修正，再交給 `gatekeeper` 檢查。**最多來回 3 輪**，超過就停下來問使用者。
6. **最後 review**：派 `planner` 對照規劃 review，必要時更新 `CLAUDE.md`。
7. **報告並 commit**：PM 撰寫完整報告，連同改動一起 commit。

### 規則

- **不分大小事，團隊無法決定的一律問使用者**，不可替使用者做決定。開跑後冒出新問題也一樣，先停下來問。
- 角色之間的資訊都經由 PM 轉交。

### 完整報告

存到 `docs/reports/YYYY-MM-DD-<主題>.md` 並 commit，內容包含：

- 需求摘要
- 規劃（連結到 `docs/plans/` 的規劃檔）
- 改了哪些檔案、各做了什麼
- 把關者提出的問題與處理結果
- 測試指令與實際結果
- 規劃者的 review 意見、`CLAUDE.md` 是否有更新
- 使用者在過程中做了哪些決定
