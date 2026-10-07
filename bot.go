package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"slices"
	"syscall"

	"github.com/bwmarrin/discordgo"
	"github.com/joho/godotenv"
)

func init() {
	err := godotenv.Load()
	if err != nil {
		log.Println("警告：無法載入 .env 檔案，將嘗試直接讀取系統環境變數")
	}
	token := os.Getenv("TOKEN")

	sess, err = discordgo.New("Bot " + token)
	if err != nil {
		log.Fatal(err)
	}
}

var (
	GuildId             = os.Getenv("SERVER_ID")
	RemoveCommands bool = true
)

var sess *discordgo.Session

var (
	guilds_channels = make(map[string]([]string))
	commands        = []*discordgo.ApplicationCommand{
		{
			Name:        "channel",
			Description: "頻道設定",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "add",
					Description: "設定為圖片存取處之一",
				},
				{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "remove",
					Description: "移除自圖片存取處",
				},
				{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "list",
					Description: "列出圖片存取處",
				},
			},
		},
	}
	commandHandlers = map[string]func(s *discordgo.Session, i *discordgo.InteractionCreate){
		"channel": func(s *discordgo.Session, i *discordgo.InteractionCreate) {
			options := i.ApplicationCommandData().Options
			if len(options) == 0 {
				return
			}
			switch options[0].Name {
			case "add":
				handdleChannelAdd(s, i)
			case "remove":
				handdleChannelRemove(s, i)
			case "list":
				handdleChannelList(s, i)
			}
		}}
)

func handdleChannelAdd(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if slices.Contains(GetChannels(), i.ChannelID) {
		content, err := guilds_channels_nameList(s, guilds_channels)
		if err != nil {
			s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
				Type: discordgo.InteractionResponseChannelMessageWithSource,
				Data: &discordgo.InteractionResponseData{
					Content: err.Error(),
				},
			})
			return
		}
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: fmt.Sprintf("頻道已存在！目前頻道列表：\n%v", content),
			},
		})
		return
	}
	guilds_channels[i.GuildID] = append(guilds_channels[i.GuildID], i.ChannelID)
	content, err := guilds_channels_nameList(s, guilds_channels)
	if err != nil {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: err.Error(),
			},
		})
		return
	}
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: fmt.Sprintf("成功新增頻道！新增後頻道列表：\n%v", content),
		},
	})
}

func handdleChannelRemove(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if !slices.Contains(GetChannels(), i.ChannelID) {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "此頻道不存在於頻道列表中",
			},
		})
		return
	}
	for GuildId, arr := range guilds_channels {
		if index := slices.Index(arr, i.ChannelID); index >= 0 {
			guilds_channels[GuildId] = slices.Delete(arr, index, index+1)
		}
	}
	content, err := guilds_channels_nameList(s, guilds_channels)
	if err != nil {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: err.Error(),
			},
		})
		return
	}
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: fmt.Sprintf("移除此頻道自頻道列表，目前頻道列表：\n%v", content),
		},
	})
}
func handdleChannelList(s *discordgo.Session, i *discordgo.InteractionCreate) {
	content, err := guilds_channels_nameList(s, guilds_channels)
	if err != nil {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: err.Error(),
			},
		})
		return
	}
	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: fmt.Sprintf("頻道列表：\n%v", content),
		},
	})
}

func init() {
	sess.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		if h, ok := commandHandlers[i.ApplicationCommandData().Name]; ok {
			h(s, i)
		}
	})
}

func main() {

	// 2. 設定 Intents (必須開啟 Message Content Intent 才能讀取文字訊息內容)
	sess.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentsMessageContent

	// 3. 註冊事件監聽器
	sess.AddHandler(messageCreate)

	// 4. 開啟 WebSocket 連線
	err := sess.Open()
	if err != nil {
		fmt.Println("連線 Discord 失敗:", err)
		return
	}
	registeredCommands := make([]*discordgo.ApplicationCommand, len(commands))
	for i, v := range commands {
		cmd, err := sess.ApplicationCommandCreate(sess.State.User.ID, GuildId, v)
		if err != nil {
			log.Print(err)
		}
		registeredCommands[i] = cmd
	}

	defer sess.Close()

	fmt.Println("機器人已成功啟動！按下 CTRL-C 退出。")

	// 5. 等待中斷訊號以優雅關閉
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT, syscall.SIGTERM, os.Interrupt)
	<-sc

	if RemoveCommands {
		log.Print("關閉機器人")
		for _, v := range registeredCommands {
			err := sess.ApplicationCommandDelete(sess.State.User.ID, GuildId, v.ID)
			if err != nil {
				log.Panicf("Cannot delete '%v' command: %v", v.Name, err)
			}
		}
		log.Print("刪除指令成功")
	}
}

// 當有人發送訊息時觸發
func messageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	// 忽略機器人自己發送的訊息，避免無限迴圈
	if m.Author.ID == s.State.User.ID {
		return
	}

	//確認是已登錄的channel
	if slices.Contains(GetChannels(), m.ChannelID) {

		// 簡單的文字響應
		if m.Content == "ping" {
			s.ChannelMessageSend(m.ChannelID, fmt.Sprintf("pong! %s", m.ChannelID))
		}
	}
}

func GetGuilds() []string {
	var guilds []string
	for guild, _ := range guilds_channels {
		guilds = append(guilds, guild)
	}
	return guilds
}

func GetChannels() []string {
	var channels []string
	for _, arr := range guilds_channels {
		for _, channel := range arr {
			channels = append(channels, channel)
		}
	}
	return channels
}

func guilds_channels_nameList(s *discordgo.Session, guilds_channels map[string][]string) (content string, err error) {
	var str string
	for guildId, arr := range guilds_channels {
		guild, err := s.Guild(guildId)
		if err != nil {
			return "", err
		}
		var guildName string = guild.Name
		//不顯示無Channel的Guild
		if len(guilds_channels[guildId]) > 0 {
			str = str + fmt.Sprintf("伺服器：%v\n", guildName)
		}
		for i, channelId := range arr {
			channel, err := s.Channel(channelId)
			if err != nil {
				return "", err
			}
			var channelName string = channel.Name
			if i == 0 && i == len(arr)-1 {
				str = str + fmt.Sprintf("[%v]\n", channelName)
			} else if i == 0 {
				str = str + fmt.Sprintf("[%v", channelName)
			} else if i == len(arr)-1 {
				str = str + fmt.Sprintf("、%v]\n", channelName)
			} else {
				str = str + fmt.Sprintf("、%v", channelName)
			}
		}
	}
	return str, err
}
