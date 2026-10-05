package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/bwmarrin/discordgo"
	"github.com/joho/godotenv"
)

func main() {
	err := godotenv.Load()
	if err != nil {
		log.Println("警告：無法載入 .env 檔案，將嘗試直接讀取系統環境變數")
	}
	token := os.Getenv("TOKEN")

	sess, err := discordgo.New("Bot " + token)
	if err != nil {
		log.Fatal(err)
	}
	// 2. 設定 Intents (必須開啟 Message Content Intent 才能讀取文字訊息內容)
	sess.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentsMessageContent

	// 3. 註冊事件監聽器
	sess.AddHandler(messageCreate)

	// 4. 開啟 WebSocket 連線
	err = sess.Open()
	if err != nil {
		fmt.Println("連線 Discord 失敗:", err)
		return
	}
	defer sess.Close()

	fmt.Println("機器人已成功啟動！按下 CTRL-C 退出。")

	// 5. 等待中斷訊號以優雅關閉
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-sc
}

// 當有人發送訊息時觸發
func messageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	// 忽略機器人自己發送的訊息，避免無限迴圈
	if m.Author.ID == s.State.User.ID {
		return
	}

	// 簡單的文字響應
	if m.Content == "ping" {
		s.ChannelMessageSend(m.ChannelID, "Pong!")
	}
}
