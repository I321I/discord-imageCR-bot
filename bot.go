package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"discordbot/internal/commands"
	"discordbot/internal/config"
	"discordbot/internal/store"

	"github.com/bwmarrin/discordgo"
	"github.com/joho/godotenv"
)

func main() {
	err := godotenv.Load()
	if err != nil {
		log.Println("警告：無法載入 .env 檔案，將嘗試直接讀取系統環境變數")
	}
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

	ctx := context.Background()

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
	loadStore(ctx, st)

	hooks := commands.NopIndexHooks{}
	router := &commands.Router{
		Commands: map[string]commands.Handler{
			commands.CmdChannel: commands.NewChannelHandler(st, sess.State, sess, hooks),
			commands.CmdStorage: commands.NewDevHandler(st, devIDs, cfg.StorageChannelID, sess),
		},
	}
	sess.AddHandler(router.Handle)
	sess.AddHandler(onChannelDelete(st, hooks))
	sess.AddHandler(onGuildDelete(st, hooks))
	sess.AddHandler(onGuildCreate(st, hooks))

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
}

// loadStore 讀取儲存頻道；讀取失敗或找不到完整版本時記 log，並以空資料啟動。
func loadStore(ctx context.Context, st *store.Store) {
	if err := st.Load(ctx); err != nil {
		log.Printf("讀取儲存頻道失敗，以空資料啟動：%v", err)
		st.UseEmpty()
	}
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

// onGuildCreate 回傳 GUILD_CREATE 事件（含啟動時）的 handler：移除設定中已不存在的來源頻道並寫回。
// 伺服器無法使用（Unavailable，事件中沒有頻道資料）時不動。
func onGuildCreate(st *store.Store, hooks commands.IndexHooks) func(*discordgo.Session, *discordgo.GuildCreate) {
	return func(_ *discordgo.Session, e *discordgo.GuildCreate) {
		if e.Guild == nil || e.Unavailable {
			return
		}
		commands.PruneMissingChannels(context.Background(), st, hooks, e.ID, e.Channels)
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
