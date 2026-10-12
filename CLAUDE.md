# discord-imageCR-bot

用 Go 與 [discordgo](https://github.com/bwmarrin/discordgo) 撰寫的 Discord bot。

## 專案概況

- 主程式：`bot.go`（module 名稱 `discordbot`）
- 環境變數由 `.env` 載入：`TOKEN`（bot token）、`SERVER_ID`（伺服器 ID）
- Slash command：
  - `/channel add | remove | list | id`：設定 bot 要在哪些頻道回應
  - `/search`
- 已設定的頻道目前存在記憶體中的 `guilds_channels`（`map[guildID][]channelID`），重啟後會消失
- 建置與測試：`go build ./...`、`go test ./...`

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
