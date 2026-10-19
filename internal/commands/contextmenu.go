package commands

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"discordbot/internal/imagemsg"
	"discordbot/internal/pipeline"
	"discordbot/internal/recognize"
	"discordbot/internal/store"

	"github.com/bwmarrin/discordgo"
)

// 右鍵指令與編輯表單的回覆文字（全部 ephemeral，階段 8 的使用者決定）。
const (
	msgNotImageMessage         = "只能對圖庫 bot 發的圖片訊息，或處理失敗（❌）的原訊息使用"
	msgNoPermission            = "你沒有權限使用此功能"
	msgUpdated                 = "已更新"
	msgNoFailedFields          = "沒有辨識失敗的欄位，不需要重新辨識"
	msgRerecognized            = "重新辨識完成"
	msgRerecognizedStillFailed = "重新辨識完成，仍有欄位辨識失敗"
	msgImageNotFound           = "找不到原圖，無法重新辨識"
	msgTooLong                 = "內容超過 Discord 2000 字上限，未修改"

	msgMessageGone       = "找不到這則圖片訊息，可能已被刪除"
	msgUpdateFailed      = "更新失敗，請稍後再試"
	msgNotFailedOriginal = "這則訊息沒有處理失敗的標記（❌），不需要重新處理"
	msgReprocessed       = "重新處理完成"
	msgReprocessFailed   = "重新處理失敗，已在原訊息加上 ❌"
	msgAlreadyQueued     = "這則訊息已在處理佇列中，請稍候"
	msgPermissionUnknown = "目前無法確認權限，請稍後再試"
	msgRestarting        = "機器人正在重新啟動，請稍後再試"
	msgNotSource         = "這個頻道已不是圖庫頻道"
	msgNoImages          = "這則訊息沒有可處理的圖片" // 帶 ❌ 但只剩 gifv 等新訊息不處理的內容（使用者決定）
)

const (
	// EditModalPrefix 是編輯表單 CustomID 的前綴（Router.Modals 的 key）。
	EditModalPrefix = "editimg"
	editModalTitle  = "編輯圖片訊息"
	// editOpenTimeout 是開啟編輯表單前檢查權限的時間上限：modal 不能 deferred，必須在 3 秒內回應。
	editOpenTimeout = 2 * time.Second
	// editFormTTL 是開表單時記下的欄位值保存多久（用來判斷送出時哪些欄位實際改過）。
	editFormTTL = 15 * time.Minute
	// maxFormRecords 是表單記錄的上限，超過時丟掉最舊的。
	maxFormRecords = 200
)

// editInputs 是編輯表單的四個欄位（依顯示順序）與 TextInput 的 CustomID。
var editInputs = []struct {
	id    string
	field imagemsg.Field
}{
	{"work", imagemsg.FieldWork},
	{"text", imagemsg.FieldText},
	{"characters", imagemsg.FieldCharacters},
	{"keywords", imagemsg.FieldKeywords},
}

// ContextMenuAPI 是右鍵指令需要的 Discord 方法，*discordgo.Session 直接滿足。
type ContextMenuAPI interface {
	API
	ChannelMessage(channelID, messageID string, options ...discordgo.RequestOption) (*discordgo.Message, error)
	ChannelMessageEditComplex(m *discordgo.MessageEdit, options ...discordgo.RequestOption) (*discordgo.Message, error)
}

// ChannelAccess 判定使用者能否使用來源頻道（*access.Resolver 直接滿足）。
// 無法確認（成員查詢失敗、逾時）時回錯誤，與「沒權限」（false, nil）區分。
type ChannelAccess interface {
	CheckView(ctx context.Context, userID, channelID string) (bool, error)
}

// ImageIndex 收錄修改後的圖片訊息（*index.Hooks 直接滿足：頻道已非來源時不寫入）。
type ImageIndex interface {
	PutImageMessage(guildID string, m *discordgo.Message)
}

// Reprocessor 是「重新辨識」需要的 pipeline 方法（*pipeline.Pipeline 直接滿足）。
type Reprocessor interface {
	RecognizeAttachment(ctx context.Context, a *discordgo.MessageAttachment, fields []imagemsg.Field) (imagemsg.Fields, error)
	Reprocess(ctx context.Context, guildID string, m *discordgo.Message) <-chan error
}

// ContextMenu 實作「編輯圖片訊息」「重新辨識」兩個右鍵指令與編輯表單。
// 權限一律為使用者能使用該來源頻道（Access.CanView）。
type ContextMenu struct {
	API      ContextMenuAPI
	Access   ChannelAccess
	Store    *store.Store
	Index    ImageIndex
	Pipeline Reprocessor
	// BotID 回傳 bot 自己的使用者 ID（連線後才知道）。
	BotID func() string
	// Ctx 是 bot 關機時才取消的 ctx，用於排入原訊息佇列的重新處理；nil 時用 handler 的 ctx。
	Ctx context.Context

	// Now 取得目前時間；nil 時用 time.Now。
	Now func() time.Time

	// locks 讓同一則圖片訊息的「重新取得最新內容 → 合併 → 寫回」互斥（編輯表單與重新辨識不互相覆蓋）。
	locks messageLocks

	// forms 記下開表單時的四欄值（key 為 modal CustomID），送出時只套用實際改過的欄位。
	// 最多 maxFormRecords 筆，超過時丟掉最舊的。
	formMu  sync.Mutex
	forms   map[string]formRecord
	formSeq uint64
}

type formRecord struct {
	userID string // 開表單的使用者：只有同一使用者送出時才取走
	fields imagemsg.Fields
	at     time.Time
	seq    uint64 // 寫入順序（找最舊的記錄用）
}

func (c *ContextMenu) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// saveForm 記下 userID 開啟表單 customID 時的欄位值，並清除過期記錄；超過 maxFormRecords 筆時丟掉最舊的。
func (c *ContextMenu) saveForm(customID, userID string, f imagemsg.Fields) {
	now := c.now()
	c.formMu.Lock()
	defer c.formMu.Unlock()
	for k, r := range c.forms {
		if now.Sub(r.at) >= editFormTTL {
			delete(c.forms, k)
		}
	}
	if c.forms == nil {
		c.forms = map[string]formRecord{}
	}
	for len(c.forms) >= maxFormRecords {
		oldest, first := "", true
		for k, r := range c.forms {
			if first || r.seq < c.forms[oldest].seq {
				oldest, first = k, false
			}
		}
		delete(c.forms, oldest)
	}
	c.formSeq++
	c.forms[customID] = formRecord{userID: userID, fields: f, at: now, seq: c.formSeq}
}

// takeForm 取出並刪除 userID 開啟的表單 customID 的記錄；沒有或已過期時 ok=false（過期記錄一併刪除）。
// 記錄屬於其他使用者時 ok=false，且不刪除（不影響開表單的人送出）。
func (c *ContextMenu) takeForm(customID, userID string) (imagemsg.Fields, bool) {
	c.formMu.Lock()
	defer c.formMu.Unlock()
	r, ok := c.forms[customID]
	if !ok {
		return imagemsg.Fields{}, false
	}
	expired := c.now().Sub(r.at) >= editFormTTL
	if !expired && r.userID != userID {
		return imagemsg.Fields{}, false
	}
	delete(c.forms, customID)
	if expired {
		return imagemsg.Fields{}, false
	}
	return r.fields, true
}

// permissionDenied 依權限檢查結果補上回覆，回傳 true 表示不可繼續：
// 無法確認 → 關機中為「正在重新啟動」，否則「目前無法確認權限」；沒權限 →「沒有權限」。
func (c *ContextMenu) permissionDenied(i *discordgo.InteractionCreate, ok bool, err error) bool {
	switch {
	case err != nil:
		log.Printf("interaction %s：無法確認權限：%v", i.ID, err)
		if c.shuttingDown() {
			editResponse(c.API, i, msgRestarting)
		} else {
			editResponse(c.API, i, msgPermissionUnknown)
		}
		return true
	case !ok:
		editResponse(c.API, i, msgNoPermission)
		return true
	}
	return false
}

// shuttingDown 回傳 bot 是否正在關機（c.Ctx 已結束）。
func (c *ContextMenu) shuttingDown() bool {
	return c.Ctx != nil && c.Ctx.Err() != nil
}

// targetMessage 回傳右鍵指令的目標訊息。
func targetMessage(i *discordgo.InteractionCreate) *discordgo.Message {
	data := i.ApplicationCommandData()
	if data.Resolved == nil {
		return nil
	}
	return data.Resolved.Messages[data.TargetID]
}

// imageMessage 回傳 m 是否為 bot 發的圖片訊息與解析結果。
func (c *ContextMenu) imageMessage(m *discordgo.Message) (imagemsg.Fields, string, bool) {
	if m == nil || m.Author == nil || m.Author.ID == "" || m.Author.ID != c.BotID() {
		return imagemsg.Fields{}, "", false
	}
	return imagemsg.Parse(m.Content)
}

// sourceGuild 回傳來源頻道所屬的伺服器。
func (c *ContextMenu) sourceGuild(channelID string) string {
	guildID, _, _ := c.Store.Snapshot().SourceChannel(channelID)
	return guildID
}

// Edit 是「編輯圖片訊息」的 handler：檢查後回覆預填目前值的表單（Q28：失敗欄位預填失敗標記）。
func (c *ContextMenu) Edit(ctx context.Context, i *discordgo.InteractionCreate) {
	m := targetMessage(i)
	f, _, ok := c.imageMessage(m)
	if !ok {
		respondEphemeral(c.API, i, msgNotImageMessage)
		return
	}
	switch ok, err := c.canOpen(ctx, i, m.ChannelID); {
	case err != nil:
		log.Printf("interaction %s：無法確認權限：%v", i.ID, err)
		respondEphemeral(c.API, i, msgPermissionUnknown)
		return
	case !ok:
		respondEphemeral(c.API, i, msgNoPermission)
		return
	}
	rows := make([]discordgo.MessageComponent, 0, len(editInputs))
	for _, in := range editInputs {
		style := discordgo.TextInputShort
		if in.field == imagemsg.FieldText {
			style = discordgo.TextInputParagraph
		}
		rows = append(rows, discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.TextInput{
			CustomID: in.id,
			Label:    imagemsg.Label(in.field),
			Style:    style,
			Value:    f.Get(in.field),
			Required: false,
		}}})
	}
	// CustomID 加上 interaction ID 當作唯一 token，對應這次開表單時的欄位值。
	customID := EditModalPrefix + ":" + m.ChannelID + ":" + m.ID + ":" + i.ID
	c.saveForm(customID, interactionUserID(i.Interaction), f)
	err := c.API.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseModal,
		Data: &discordgo.InteractionResponseData{
			CustomID:   customID,
			Title:      editModalTitle,
			Components: rows,
		},
	})
	if err != nil {
		log.Printf("回覆 interaction %s（編輯表單）失敗：%v", i.ID, err)
	}
}

// memberAccess 是可用 interaction 自帶的成員身分組判定權限的 ChannelAccess（*access.Resolver 滿足），不需 REST。
// gateway state 尚未載入伺服器或頻道時回錯誤（無法確認）。
type memberAccess interface {
	CanViewMember(guildID, userID, channelID string, roles []string) (bool, error)
}

// canOpen 判定開編輯表單的權限。modal 不能 deferred，必須在 3 秒內回應：
// interaction 帶有成員資料（伺服器內）且 Access 支援時，用它的身分組直接計算（快速路徑，不查 REST；
// gateway state 尚未載入伺服器或頻道時回錯誤＝無法確認）；
// 否則用 CheckView，最多等 editOpenTimeout（逾時回錯誤）。
func (c *ContextMenu) canOpen(ctx context.Context, i *discordgo.InteractionCreate, channelID string) (bool, error) {
	if ma, ok := c.Access.(memberAccess); ok && i.Member != nil && i.Member.User != nil && i.GuildID != "" {
		return ma.CanViewMember(i.GuildID, i.Member.User.ID, channelID, i.Member.Roles)
	}
	checkCtx, cancel := context.WithTimeout(ctx, editOpenTimeout)
	defer cancel()
	return c.Access.CheckView(checkCtx, interactionUserID(i.Interaction), channelID)
}

// parseEditID 解析編輯表單的 CustomID `editimg:<channelID>:<messageID>:<token>`（token 可省略）。
func parseEditID(customID string) (channelID, messageID string, ok bool) {
	parts := strings.Split(customID, ":")
	if len(parts) < 3 || len(parts) > 4 || parts[0] != EditModalPrefix || parts[1] == "" || parts[2] == "" {
		return "", "", false
	}
	if len(parts) == 4 && parts[3] == "" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

// submittedValues 取出表單送出的值（TextInput CustomID → 值）。
func submittedValues(data discordgo.ModalSubmitInteractionData) map[string]string {
	out := map[string]string{}
	add := func(c discordgo.MessageComponent) {
		switch ti := c.(type) {
		case *discordgo.TextInput:
			out[ti.CustomID] = ti.Value
		case discordgo.TextInput:
			out[ti.CustomID] = ti.Value
		}
	}
	for _, row := range data.Components {
		switch r := row.(type) {
		case *discordgo.ActionsRow:
			for _, c := range r.Components {
				add(c)
			}
		case discordgo.ActionsRow:
			for _, c := range r.Components {
				add(c)
			}
		}
	}
	return out
}

// EditSubmit 是編輯表單送出的 handler：
// 先送 deferred → 檢查權限 → 取走同一使用者開表單時的記錄 →（以下在該訊息的鎖內）重新取得最新內容並確認是 bot 的圖片訊息 →
// 套用表單中實際改過的欄位（空白 → 清空；仍是失敗標記 → 保留（Q29），最新內容已不是失敗標記時保留最新的值；
// 作品、角色、關鍵字以「、」正規化（Q22），圖片文字原樣）→ 保留上傳者 → 超過 2000 字回錯誤不修改（Q23）→
// 修改（mention 不通知）→ 更新索引 → 釋放鎖後回覆。
func (c *ContextMenu) EditSubmit(ctx context.Context, i *discordgo.InteractionCreate) {
	if !deferEphemeral(c.API, i) {
		return
	}
	customID := i.ModalSubmitData().CustomID
	channelID, messageID, ok := parseEditID(customID)
	if !ok {
		editResponse(c.API, i, msgNotImageMessage)
		return
	}
	userID := interactionUserID(i.Interaction)
	if ok, err := c.Access.CheckView(ctx, userID, channelID); c.permissionDenied(i, ok, err) {
		return
	}
	opened, hasOpened := c.takeForm(customID, userID)
	editResponse(c.API, i, c.applyEdit(channelID, messageID, submittedValues(i.ModalSubmitData()), opened, hasOpened))
}

// applyEdit 在訊息鎖內把表單送出的 values 套用到圖片訊息的最新內容並寫回、更新索引，回傳要回覆的文字。
// hasOpened 時只套用與開表單時（opened）不同的欄位。
func (c *ContextMenu) applyEdit(channelID, messageID string, values map[string]string, opened imagemsg.Fields, hasOpened bool) string {
	unlock := c.locks.lock(messageID)
	defer unlock()
	latest, err := c.API.ChannelMessage(channelID, messageID)
	if err != nil {
		log.Printf("編輯圖片訊息 %s/%s：重新取得訊息失敗：%v", channelID, messageID, err)
		return msgMessageGone
	}
	f, uploader, ok := c.imageMessage(latest)
	if !ok {
		return msgNotImageMessage
	}
	for _, in := range editInputs {
		v, ok := values[in.id]
		if !ok {
			continue // 沒送出的欄位保留最新的值
		}
		if hasOpened && sameInput(v, opened.Get(in.field)) {
			continue // 沒改過的欄位保留最新的值（別人期間的修改不被蓋掉）
		}
		f.Set(in.field, editedValue(in.field, v, f.Get(in.field)))
	}
	content, ok := imagemsg.FormatStrict(f, uploader)
	if !ok {
		return msgTooLong
	}
	if !c.editMessage(latest, content) {
		return msgUpdateFailed
	}
	return msgUpdated
}

// sameInput 回傳表單送出的值 a 與開表單時的值 b 是否相同：兩邊先修剪前後空白、統一換行為 \n 再比較。
func sameInput(a, b string) bool {
	norm := func(s string) string {
		s = strings.ReplaceAll(s, "\r\n", "\n")
		s = strings.ReplaceAll(s, "\r", "\n")
		return strings.TrimSpace(s)
	}
	return norm(a) == norm(b)
}

// editedValue 回傳表單送出 v 後欄位的新值；latest 是欄位目前（最新內容中）的值。
func editedValue(field imagemsg.Field, v, latest string) string {
	switch strings.TrimSpace(v) {
	case "":
		return ""
	case imagemsg.FailedMarker:
		if latest != imagemsg.FailedMarker && latest != "" {
			return latest // 期間已被重新辨識補上：不蓋回失敗標記
		}
		return imagemsg.FailedMarker
	}
	if field == imagemsg.FieldText {
		return v
	}
	return imagemsg.NormalizeList(v)
}

// editMessage 把圖片訊息 m 改成 content（mention 不通知）並更新索引；失敗時記 log 並回傳 false。
func (c *ContextMenu) editMessage(m *discordgo.Message, content string) bool {
	edited, err := c.API.ChannelMessageEditComplex(&discordgo.MessageEdit{
		ID:              m.ID,
		Channel:         m.ChannelID,
		Content:         &content,
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}},
	})
	if err != nil {
		log.Printf("修改圖片訊息 %s/%s 失敗：%v", m.ChannelID, m.ID, err)
		return false
	}
	if c.Index != nil {
		c.Index.PutImageMessage(c.sourceGuild(m.ChannelID), edited)
	}
	return true
}

// Rerecognize 是「重新辨識」的 handler。
//   - bot 的圖片訊息：只重跑標失敗的作品、圖片文字（沒有則提示）；
//   - 使用者自己發、帶 bot ❌ 的原訊息（圖片附件或圖片網址）：重新處理整則（同自動處理流程，網址圖重新下載）；
//   - 其他訊息：提示只能對圖片訊息使用。
func (c *ContextMenu) Rerecognize(ctx context.Context, i *discordgo.InteractionCreate) {
	m := targetMessage(i)
	userID := interactionUserID(i.Interaction)
	if f, _, ok := c.imageMessage(m); ok {
		c.rerecognizeImage(ctx, i, m, f, userID)
		return
	}
	if m != nil && m.Author != nil && m.Author.ID == userID && !m.Author.Bot && m.WebhookID == "" && recognize.Eligible(m) {
		c.reprocessOriginal(ctx, i, m, userID)
		return
	}
	respondEphemeral(c.API, i, msgNotImageMessage)
}

// recognizableFailed 回傳 f 中標失敗、且能重新辨識的欄位（作品、圖片文字）。
func recognizableFailed(f imagemsg.Fields) []imagemsg.Field {
	var out []imagemsg.Field
	for _, field := range f.Failed() {
		if field == imagemsg.FieldWork || field == imagemsg.FieldText {
			out = append(out, field)
		}
	}
	return out
}

// rerecognizeImage 重新辨識圖片訊息的失敗欄位：先送 deferred → 檢查權限 → 下載第一個附件、只辨識失敗欄位（優先排隊）→
// 重新取得最新內容，只覆寫當時仍是失敗標記的欄位 → 修改 → 更新索引 → 回報結果。
// 回報失敗（例如 token 已過期）只記 log。bot 關機（c.Ctx 結束）時取消。
func (c *ContextMenu) rerecognizeImage(ctx context.Context, i *discordgo.InteractionCreate, m *discordgo.Message, f imagemsg.Fields, userID string) {
	fields := recognizableFailed(f)
	if len(fields) == 0 {
		respondEphemeral(c.API, i, msgNoFailedFields)
		return
	}
	if !deferEphemeral(c.API, i) {
		return
	}
	ctx, cancel := c.withShutdown(ctx)
	defer cancel()
	if ok, err := c.Access.CheckView(ctx, userID, m.ChannelID); c.permissionDenied(i, ok, err) {
		return
	}
	if len(m.Attachments) == 0 || m.Attachments[0] == nil {
		editResponse(c.API, i, msgImageNotFound)
		return
	}
	got, err := c.Pipeline.RecognizeAttachment(ctx, m.Attachments[0], fields)
	if ctx.Err() != nil {
		// 關機中斷：被中斷的欄位結果不可信，不寫回。
		log.Printf("重新辨識 %s/%s 被中斷：%v", m.ChannelID, m.ID, ctx.Err())
		editResponse(c.API, i, msgRestarting)
		return
	}
	if err != nil {
		log.Printf("重新辨識 %s/%s：下載原圖失敗：%v", m.ChannelID, m.ID, err)
		editResponse(c.API, i, msgImageNotFound)
		return
	}
	editResponse(c.API, i, c.mergeRecognized(m, fields, got))
}

// mergeRecognized 在訊息鎖內重新取得圖片訊息 m 的最新內容，只把當時仍是失敗標記的欄位改成辨識結果 got，
// 寫回並更新索引，回傳要回覆的文字（辨識在鎖外；回覆在釋放鎖之後）。
func (c *ContextMenu) mergeRecognized(m *discordgo.Message, fields []imagemsg.Field, got imagemsg.Fields) string {
	unlock := c.locks.lock(m.ID)
	defer unlock()
	latest, err := c.API.ChannelMessage(m.ChannelID, m.ID)
	if err != nil {
		log.Printf("重新辨識 %s/%s：重新取得訊息失敗：%v", m.ChannelID, m.ID, err)
		return msgMessageGone
	}
	cur, uploader, ok := c.imageMessage(latest)
	if !ok {
		return msgMessageGone
	}
	merged := cur
	for _, field := range fields {
		if cur.Get(field) == imagemsg.FailedMarker {
			merged.Set(field, got.Get(field))
		}
	}
	if merged != cur && !c.editMessage(latest, imagemsg.Format(merged, uploader)) {
		return msgUpdateFailed
	}
	if len(merged.Failed()) > 0 {
		return msgRerecognizedStillFailed
	}
	return msgRerecognized
}

// withShutdown 回傳在 ctx 或 c.Ctx（bot 關機）任一結束時就結束的 ctx。
func (c *ContextMenu) withShutdown(ctx context.Context) (context.Context, context.CancelFunc) {
	return withShutdown(ctx, c.Ctx)
}

// withShutdown 回傳在 ctx 或 shutdown（bot 關機；nil 表示沒有）任一結束時就結束的 ctx。
func withShutdown(ctx, shutdown context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	if shutdown == nil {
		return ctx, cancel
	}
	if shutdown.Err() != nil {
		cancel() // 已在關機：立即取消（AfterFunc 是非同步的）
		return ctx, cancel
	}
	stop := context.AfterFunc(shutdown, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

// reprocessOriginal 重新處理使用者自己發、帶 bot ❌ 的原訊息：先送 deferred → 檢查權限 →
// 重新取得最新訊息並確認仍帶 ❌ → 移除 ❌ 後排入原訊息佇列（同自動處理）→ 等處理完回報結果。
func (c *ContextMenu) reprocessOriginal(ctx context.Context, i *discordgo.InteractionCreate, m *discordgo.Message, userID string) {
	if !deferEphemeral(c.API, i) {
		return
	}
	if ok, err := c.Access.CheckView(ctx, userID, m.ChannelID); c.permissionDenied(i, ok, err) {
		return
	}
	latest, err := c.API.ChannelMessage(m.ChannelID, m.ID)
	if err != nil {
		var rest *discordgo.RESTError
		if !errors.As(err, &rest) || rest.Response == nil || rest.Response.StatusCode != http.StatusNotFound {
			log.Printf("重新處理 %s/%s：重新取得訊息失敗：%v", m.ChannelID, m.ID, err)
		}
		editResponse(c.API, i, msgImageNotFound)
		return
	}
	if !pipeline.HasOwnReaction(latest, "❌") || !recognize.Eligible(latest) {
		editResponse(c.API, i, msgNotFailedOriginal)
		return
	}
	jobCtx := c.Ctx
	if jobCtx == nil {
		jobCtx = ctx
	}
	err = <-c.Pipeline.Reprocess(jobCtx, c.sourceGuild(latest.ChannelID), latest)
	switch {
	case err == nil:
	case errors.Is(err, pipeline.ErrAlreadyQueued):
		editResponse(c.API, i, msgAlreadyQueued)
		return
	case errors.Is(err, pipeline.ErrNoImages):
		editResponse(c.API, i, msgNoImages)
		return
	case errors.Is(err, pipeline.ErrNotSource):
		// 排隊期間頻道已不是來源（原訊息不補回 ❌）。
		editResponse(c.API, i, msgNotSource)
		return
	case jobCtx.Err() != nil:
		// 關機中斷：pipeline 不補回 ❌（下次啟動自動補處理），回覆不說已加 ❌。
		log.Printf("重新處理 %s/%s 被中斷：%v", m.ChannelID, m.ID, err)
		editResponse(c.API, i, msgRestarting)
		return
	default:
		log.Printf("重新處理 %s/%s 失敗：%v", m.ChannelID, m.ID, err)
		editResponse(c.API, i, msgReprocessFailed)
		return
	}
	editResponse(c.API, i, msgReprocessed)
}
