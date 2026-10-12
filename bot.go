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

	err = sess.Open()
	if err != nil {
		log.Fatalf("連線 Discord 失敗：%v", err)
	}
	defer sess.Close()

	ctx := context.Background()

	var devIDs []string
	app, err := sess.Application("@me")
	if err != nil {
		log.Printf("查詢 application 失敗，開發者指令將拒絕所有人：%v", err)
	} else {
		devIDs = commands.DeveloperIDs(app)
	}

	st := store.New(sess, cfg.StorageChannelID)
	loadStore(ctx, st)

	router := &commands.Router{
		Commands: map[string]commands.Handler{
			commands.CmdChannel: commands.NewChannelHandler(st, sess.State, sess, commands.NopIndexHooks{}),
			commands.CmdStorage: commands.NewDevHandler(st, devIDs, cfg.StorageChannelID, sess),
		},
	}
	sess.AddHandler(router.Handle)
	sess.AddHandler(onChannelDelete(st, commands.NopIndexHooks{}))

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
