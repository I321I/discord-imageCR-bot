package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"discordbot/internal/access"
	"discordbot/internal/commands"
	"discordbot/internal/config"
	"discordbot/internal/index"
	"discordbot/internal/pipeline"
	"discordbot/internal/recognize"
	"discordbot/internal/snowflake"
	"discordbot/internal/store"

	"github.com/bwmarrin/discordgo"
	"github.com/joho/godotenv"
)

func main() {
	loadDotEnv(".env")
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		log.Fatal(err)
	}

	sess, err := discordgo.New("Bot " + cfg.Token)
	if err != nil {
		log.Fatal(err)
	}

	// 需要 Guilds（gateway state 中的伺服器與頻道）、Guild Messages 與 Message Content；不開 Server Members。
	sess.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMessages | discordgo.IntentsMessageContent

	// 關機時取消：進行中的自動處理不再發出圖片訊息（留給下次啟動補處理）。
	ctx, cancel := context.WithCancel(context.Background())

	// 連線前先查開發者、載入設定並註冊 handler，避免連線後到這些步驟完成前的 interaction 與事件
	// （包含啟動時的 GUILD_CREATE）被漏掉或在設定載入前處理。
	var devIDs []string
	app, err := sess.Application("@me")
	if err != nil {
		log.Printf("查詢 application 失敗，開發者指令將拒絕所有人：%v", err)
	} else {
		devIDs = commands.DeveloperIDs(app)
	}

	st := store.New(sess, cfg.StorageChannelID)
	storeLoaded := loadStore(ctx, st)

	// 來源頻道自動處理：trace.moe 與 OCR 各自排隊（並行數 1）；原訊息一次只處理一則（佇列）。
	ocr := recognize.NewRapidOCR()
	idx := index.New()
	p := &pipeline.Pipeline{
		API:   sess,
		Fetch: pipeline.NewHTTPFetcher(),
		Work:  recognize.Serialize(recognize.NewTraceMoe(cfg.TraceMoeKey, cfg.TraceMoeMinSimilarity), 1),
		Text:  recognize.SerializeText(ocr),
		Store: st,
		// 圖片網址訊息：網址圖以防 SSRF 的下載器下載（只連公開位址的 80、443）。
		URLFetch: pipeline.NewURLFetcher(),
	}
	// 索引：頻道新增時掃描、移除時清除；訊息刪除與編輯時更新（Q31）。
	// pipeline 經由 hooks 寫入索引，頻道移除後不再寫入。
	hooks := index.NewHooks(ctx, sess, idx, p, st)
	p.Index = hooks

	// 可用頻道判定（成員快取 MEMBER_CACHE_TTL）：右鍵指令的權限檢查與 /search 的搜尋範圍。
	res := access.NewResolver(sess, sess.State, st, cfg.MemberCacheTTL)
	cm := &commands.ContextMenu{
		API:      sess,
		Access:   res,
		Store:    st,
		Index:    hooks,
		Pipeline: p,
		BotID:    hooks.BotID,
		Ctx:      ctx,
	}
	// /search：搜尋使用者要搜尋的頻道（可用頻道 ∩ 勾選）中的圖，貼圖時重新抓訊息並重新上傳；
	// 網址圖先以防 SSRF 的下載器確認圖片還在，再貼出網址。
	search := &commands.Search{
		API:       sess,
		Access:    res,
		Index:     idx,
		Fetch:     pipeline.NewHTTPFetcher(),
		LinkCheck: pipeline.NewURLFetcher(),
		Ctx:       ctx,
	}
	// /sources：使用者勾選 /search 要搜尋的頻道（存在儲存頻道）。
	sources := &commands.Sources{API: sess, Access: res, Store: st}
	router := newRouter(
		commands.NewChannelHandler(st, sess.State, sess, hooks),
		commands.NewDevHandler(st, devIDs, cfg.StorageChannelID, sess),
		cm,
		search,
		sources,
	)
	sess.AddHandler(router.Handle)
	sess.AddHandler(onChannelDelete(st, hooks))
	sess.AddHandler(onGuildDelete(st, hooks))
	sess.AddHandler(onGuildCreate(st, hooks, sess.State))
	sess.AddHandler(onMessageDelete(hooks))
	sess.AddHandler(onMessageDeleteBulk(hooks))
	sess.AddHandler(onMessageUpdate(ctx, hooks, p))
	sess.AddHandler(onMessageCreate(ctx, p))
	// 啟動掃描由 READY 觸發（首次連線與斷線後重新 IDENTIFY 都會收到；RESUME 不會），不在 Open 之後另外掃描。
	sess.AddHandler(onReady(hooks, st, storeLoaded))

	err = sess.Open()
	if err != nil {
		log.Fatalf("連線 Discord 失敗：%v", err)
	}
	defer sess.Close()

	// 啟動時用 bulk overwrite 同步指令；關機時不刪除（全域指令重新傳播需要時間）。
	registerCommands(sess, sess.State.User.ID, cfg.DevGuildID)

	fmt.Println("機器人已成功啟動！按下 CTRL-C 退出。")

	// 等待中斷訊號以優雅關閉
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-sc
	log.Print("關閉機器人")
	shutdown(cancel, ocr)
}

// newRouter 組出所有 interaction 的分派表：/channel、/storage、兩個右鍵指令與編輯表單、/search 與其 autocomplete、
// /sources 與其選單。
func newRouter(channel, dev commands.Handler, cm *commands.ContextMenu, search *commands.Search, sources *commands.Sources) *commands.Router {
	return &commands.Router{
		Commands: map[string]commands.Handler{
			commands.CmdChannel:     channel,
			commands.CmdStorage:     dev,
			commands.CmdEditImage:   cm.Edit,
			commands.CmdRerecognize: cm.Rerecognize,
			commands.CmdSearch:      search.Submit,
			commands.CmdSources:     sources.Command,
		},
		Autocomplete: map[string]commands.Handler{
			commands.CmdSearch: search.Autocomplete,
		},
		Components: map[string]commands.Handler{
			commands.SourcesMenuPrefix: sources.Component,
		},
		Modals: map[string]commands.Handler{
			commands.EditModalPrefix: cm.EditSubmit,
		},
	}
}

// shutdown 先取消 ctx（進行中的處理以錯誤結束、不發出圖片訊息），再結束 OCR worker。
func shutdown(cancel context.CancelFunc, ocr io.Closer) {
	cancel()
	if err := ocr.Close(); err != nil {
		log.Printf("關閉 OCR worker 失敗：%v", err)
	}
}

// messageHandler 處理新訊息（*pipeline.Pipeline 直接滿足）。
type messageHandler interface {
	HandleMessage(ctx context.Context, m *discordgo.Message)
}

// onMessageCreate 回傳新訊息事件的 handler：交給 pipeline 在背景處理（辨識可能需要數十秒）。
func onMessageCreate(ctx context.Context, h messageHandler) func(*discordgo.Session, *discordgo.MessageCreate) {
	return func(_ *discordgo.Session, e *discordgo.MessageCreate) {
		if e.Message == nil {
			return
		}
		go h.HandleMessage(ctx, e.Message)
	}
}

// messageIndexer 依訊息刪除與編輯更新索引（*index.Hooks 直接滿足）。
type messageIndexer interface {
	MessageDeleted(channelID, messageID string)
	MessagesDeleted(channelID string, messageIDs []string)
	MessageUpdated(m *discordgo.Message)
}

// onMessageDelete 回傳訊息刪除事件的 handler：移除索引項目（Q31）。
func onMessageDelete(x messageIndexer) func(*discordgo.Session, *discordgo.MessageDelete) {
	return func(_ *discordgo.Session, e *discordgo.MessageDelete) {
		if e.Message == nil {
			return
		}
		x.MessageDeleted(e.ChannelID, e.ID)
	}
}

// onMessageDeleteBulk 回傳批次刪除事件的 handler：移除索引項目（Q31）。
func onMessageDeleteBulk(x messageIndexer) func(*discordgo.Session, *discordgo.MessageDeleteBulk) {
	return func(_ *discordgo.Session, e *discordgo.MessageDeleteBulk) {
		x.MessagesDeleted(e.ChannelID, e.Messages)
	}
}

// messageUpdateHandler 處理訊息更新（*pipeline.Pipeline 直接滿足）。
type messageUpdateHandler interface {
	HandleMessageUpdate(ctx context.Context, m *discordgo.Message)
}

// onMessageUpdate 回傳訊息編輯事件的 handler：更新索引（Q31），並在背景交給 pipeline
// （預覽補上或編輯加上圖片網址的新訊息要處理）。
func onMessageUpdate(ctx context.Context, x messageIndexer, h messageUpdateHandler) func(*discordgo.Session, *discordgo.MessageUpdate) {
	return func(_ *discordgo.Session, e *discordgo.MessageUpdate) {
		if e.Message == nil {
			return
		}
		x.MessageUpdated(e.Message)
		go h.HandleMessageUpdate(ctx, e.Message)
	}
}

// readyScanner 是 READY 時清除與重新掃描需要的方法（*index.Hooks 直接滿足）。
type readyScanner interface {
	commands.IndexHooks
	SetBotID(id string)
	ScanAll(d *store.Data)
}

// onReady 回傳 READY 事件的 handler：設定 bot ID 後在背景先確定圖片網址功能上線時間點、清除，再掃描所有來源頻道。
// 上線時間點（U1）：ensureURLSince（設定已成功讀取）且設定中還沒有時，以收到 READY 的時間寫入並存回；
// 寫入失敗只記 log（只含圖片網址的訊息當舊圖片訊息）。設定讀取失敗、以空資料啟動時不寫入，
// 避免自動以空資料蓋掉儲存頻道中的設定。
// 清除：比對 READY 中 bot 所在的伺服器（暫時無法使用的也算仍在），移除已不在的伺服器的來源頻道（離線期間被踢出的殘留，
// 階段 11 決定 1）；沒有變動時不寫入；伺服器清單為空時不清除（PM 決定，比照 GUILD_CREATE）。
// 掃描（用清除後的設定）：讀各來源頻道歷史建立索引，設定時間點之後未處理的訊息（含斷線期間的訊息）依舊到新交給佇列補處理；
// 掃描期間 /search 回傳已建好的部分（Q52）。同一頻道重複掃描時舊的會被取消，已在佇列中的訊息不會重複排入。
func onReady(h readyScanner, st *store.Store, ensureURLSince bool) func(*discordgo.Session, *discordgo.Ready) {
	return func(_ *discordgo.Session, e *discordgo.Ready) {
		receivedAt := time.Now()
		if e.User != nil {
			h.SetBotID(e.User.ID)
		}
		present := make([]string, 0, len(e.Guilds))
		for _, g := range e.Guilds {
			if g != nil {
				present = append(present, g.ID)
			}
		}
		go func() {
			if ensureURLSince {
				if at, err := st.EnsureURLSince(context.Background(), snowflake.FromTime(receivedAt)); err != nil {
					log.Printf("寫入圖片網址功能上線時間點失敗，只含圖片網址的訊息暫時當舊圖片訊息：%v", err)
				} else {
					log.Printf("圖片網址功能上線時間點：%s", at)
				}
			}
			if len(present) == 0 {
				// 比照 GUILD_CREATE 頻道清單為空就跳過：清單為空可能是異常的 READY，不清除任何來源頻道。
				log.Print("READY 的伺服器清單為空，不清除來源頻道")
			} else {
				commands.PruneMissingGuilds(context.Background(), st, h, present, receivedAt)
			}
			h.ScanAll(st.Snapshot())
		}()
	}
}

// loadStore 讀取儲存頻道；讀取失敗或找不到完整版本時記 log，並以空資料啟動。回傳是否成功讀取（不是以空資料啟動）。
func loadStore(ctx context.Context, st *store.Store) bool {
	if err := st.Load(ctx); err != nil {
		log.Printf("讀取儲存頻道失敗，以空資料啟動：%v", err)
		st.UseEmpty()
		return false
	}
	return true
}

// onChannelDelete 回傳頻道刪除事件的 handler：被刪除的來源頻道自動從設定移除並寫回。
func onChannelDelete(st *store.Store, hooks commands.IndexHooks) func(*discordgo.Session, *discordgo.ChannelDelete) {
	return func(_ *discordgo.Session, e *discordgo.ChannelDelete) {
		if e.Channel == nil {
			return
		}
		commands.ChannelDeleted(context.Background(), st, hooks, e.GuildID, e.ID)
	}
}

// loadDotEnv 載入 path（.env）中的環境變數（不覆蓋已設定的）。
// 檔案不存在是正常情況（Docker 中以 --env-file 傳入，映像內沒有 .env），只記一般 log；
// 檔案存在但載入失敗才印警告。
func loadDotEnv(path string) {
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		log.Printf("沒有 %s 檔案，直接讀取環境變數", path)
		return
	}
	if err := godotenv.Load(path); err != nil {
		log.Printf("警告：無法載入 %s 檔案，將直接讀取環境變數：%v", path, err)
	}
}

// onGuildDelete 回傳 bot 離開伺服器事件的 handler：被踢出或伺服器被刪除時移除該伺服器所有來源頻道並寫回。
// 伺服器暫時無法使用（Unavailable）時不動。
func onGuildDelete(st *store.Store, hooks commands.IndexHooks) func(*discordgo.Session, *discordgo.GuildDelete) {
	return func(_ *discordgo.Session, e *discordgo.GuildDelete) {
		if e.Guild == nil || e.Unavailable {
			return
		}
		commands.GuildRemoved(context.Background(), st, hooks, e.ID)
	}
}

// stateRLocker 是 gateway state 的讀鎖，*discordgo.State 直接滿足。
type stateRLocker interface {
	RLock()
	RUnlock()
}

// onGuildCreate 回傳 GUILD_CREATE 事件（含啟動時）的 handler：移除設定中已不存在的來源頻道並寫回。
// 伺服器無法使用（Unavailable，事件中沒有頻道資料）或頻道清單為空時不動。
// 事件中的 Guild 與 state 共用，state 會在鎖內原地改寫頻道陣列，所以要在 state 讀鎖下複製頻道 ID。
// 只清除收到事件之前設定的來源頻道（見 commands.PruneMissingChannels）。
// 已知限制：receivedAt 是本機時鐘，setAt 是 Discord 的 snowflake 時間，兩者有時鐘誤差。
// 本機時鐘比 Discord 快時，事件之後才設定、但 snowflake 時間仍早於 receivedAt 的頻道可能被誤刪；
// 比 Discord 慢時，事件之前就設定的已刪除頻道可能這次不清除（下次 GUILD_CREATE 再清）。
func onGuildCreate(st *store.Store, hooks commands.IndexHooks, state stateRLocker) func(*discordgo.Session, *discordgo.GuildCreate) {
	return func(_ *discordgo.Session, e *discordgo.GuildCreate) {
		receivedAt := time.Now()
		if e.Guild == nil || e.Unavailable {
			return
		}
		state.RLock()
		ids := make([]string, 0, len(e.Channels))
		for _, ch := range e.Channels {
			if ch != nil {
				ids = append(ids, ch.ID)
			}
		}
		state.RUnlock()
		if len(ids) == 0 {
			return
		}
		commands.PruneMissingChannels(context.Background(), st, hooks, e.ID, ids, receivedAt)
	}
}

type commandRegistrar interface {
	ApplicationCommandBulkOverwrite(appID string, guildID string, commands []*discordgo.ApplicationCommand, options ...discordgo.RequestOption) ([]*discordgo.ApplicationCommand, error)
}

// registerCommands 全域註冊一般指令；開發者指令只註冊在 devGuildID（SERVER_ID），未設定則不註冊。
func registerCommands(r commandRegistrar, appID, devGuildID string) {
	global, dev := commands.Definitions()
	if _, err := r.ApplicationCommandBulkOverwrite(appID, "", global); err != nil {
		log.Printf("註冊全域指令失敗：%v", err)
	}
	if devGuildID == "" {
		log.Print("未設定 SERVER_ID，不註冊開發者指令")
		return
	}
	if _, err := r.ApplicationCommandBulkOverwrite(appID, devGuildID, dev); err != nil {
		log.Printf("註冊開發者指令失敗：%v", err)
	}
}
