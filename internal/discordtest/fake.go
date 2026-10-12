// Package discordtest 提供測試用的假 Discord API。
//
// Fake 的方法簽名與 *discordgo.Session 相同，可替換任何套件中定義的小介面。
package discordtest

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"sync"

	"github.com/bwmarrin/discordgo"
)

type SentMessage struct {
	ChannelID string
	Data      *discordgo.MessageSend
	Files     [][]byte // 每個附件的 bytes（依 Data.Files 順序）
}

type DeletedMessage struct {
	ChannelID, MessageID string
}

type Response struct {
	Interaction *discordgo.Interaction
	Response    *discordgo.InteractionResponse
}

type ResponseEdit struct {
	Interaction *discordgo.Interaction
	Edit        *discordgo.WebhookEdit
}

type Followup struct {
	Interaction *discordgo.Interaction
	Params      *discordgo.WebhookParams
}

// Fake 是記錄呼叫的假 Discord。所有方法都有上鎖，可並行呼叫。
type Fake struct {
	mu sync.Mutex

	BotID string
	// FailOn：方法名稱 → 要回傳的錯誤。設定後該方法直接回傳此錯誤，不做任何事。
	FailOn map[string]error

	Sent          []SentMessage
	Edited        []*discordgo.MessageEdit
	Deleted       []DeletedMessage
	Responses     []Response
	ResponseEdits []ResponseEdit
	Followups     []Followup
	Calls         map[string]int

	messages map[string][]*discordgo.Message // channelID → 訊息（依 ID 由舊到新）
	members  map[string]map[string]*discordgo.Member
	nextID   uint64
}

func NewFake() *Fake {
	return &Fake{
		BotID:    "bot",
		FailOn:   map[string]error{},
		Calls:    map[string]int{},
		messages: map[string][]*discordgo.Message{},
		members:  map[string]map[string]*discordgo.Member{},
		nextID:   1 << 40,
	}
}

// begin 記錄呼叫次數，回傳注入的錯誤。呼叫者必須持有鎖。
func (f *Fake) begin(method string) error {
	f.Calls[method]++
	return f.FailOn[method]
}

func (f *Fake) newID() string {
	f.nextID++
	return strconv.FormatUint(f.nextID, 10)
}

// CallCount 回傳某方法被呼叫的次數。
func (f *Fake) CallCount(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Calls[method]
}

// AddMessage 直接放一則訊息到頻道中（不記錄為 Sent）。ID 為空時自動產生遞增 ID。
func (f *Fake) AddMessage(m *discordgo.Message) *discordgo.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m.ID == "" {
		m.ID = f.newID()
	} else if n, err := strconv.ParseUint(m.ID, 10, 64); err == nil && n > f.nextID {
		f.nextID = n
	}
	f.insert(m)
	return m
}

func (f *Fake) insert(m *discordgo.Message) {
	arr := append(f.messages[m.ChannelID], m)
	sort.SliceStable(arr, func(a, b int) bool { return idLess(arr[a].ID, arr[b].ID) })
	f.messages[m.ChannelID] = arr
}

// Messages 回傳頻道中所有訊息（由舊到新）的複本。
func (f *Fake) Messages(channelID string) []*discordgo.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*discordgo.Message
	for _, m := range f.messages[channelID] {
		c := *m
		out = append(out, &c)
	}
	return out
}

// SetMember 設定 GuildMember 的回傳值；沒設定的成員回 404。
func (f *Fake) SetMember(guildID string, m *discordgo.Member) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.members[guildID] == nil {
		f.members[guildID] = map[string]*discordgo.Member{}
	}
	f.members[guildID][m.User.ID] = m
}

func idLess(a, b string) bool {
	x, errA := strconv.ParseUint(a, 10, 64)
	y, errB := strconv.ParseUint(b, 10, 64)
	if errA == nil && errB == nil {
		return x < y
	}
	return a < b
}

func (f *Fake) find(channelID, messageID string) (int, *discordgo.Message) {
	for i, m := range f.messages[channelID] {
		if m.ID == messageID {
			return i, m
		}
	}
	return -1, nil
}

func notFound(what string) error {
	return &discordgo.RESTError{
		Response: &http.Response{StatusCode: http.StatusNotFound, Status: "404 Not Found"},
		Message:  &discordgo.APIErrorMessage{Code: 10008, Message: "Unknown " + what},
	}
}

func (f *Fake) ChannelMessages(channelID string, limit int, beforeID, afterID, aroundID string, options ...discordgo.RequestOption) ([]*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("ChannelMessages"); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 100 {
		limit = 100
	}
	arr := f.messages[channelID]
	var out []*discordgo.Message
	for i := len(arr) - 1; i >= 0 && len(out) < limit; i-- {
		m := arr[i]
		if beforeID != "" && !idLess(m.ID, beforeID) {
			continue
		}
		if afterID != "" && !idLess(afterID, m.ID) {
			continue
		}
		c := *m
		out = append(out, &c)
	}
	return out, nil
}

func (f *Fake) ChannelMessage(channelID, messageID string, options ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("ChannelMessage"); err != nil {
		return nil, err
	}
	_, m := f.find(channelID, messageID)
	if m == nil {
		return nil, notFound("Message")
	}
	c := *m
	return &c, nil
}

func (f *Fake) ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend, options ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("ChannelMessageSendComplex"); err != nil {
		return nil, err
	}
	m := &discordgo.Message{
		ID:        f.newID(),
		ChannelID: channelID,
		Content:   data.Content,
		Author:    &discordgo.User{ID: f.BotID, Bot: true},
	}
	var files [][]byte
	for _, file := range data.Files {
		var b []byte
		if file.Reader != nil {
			var err error
			if b, err = io.ReadAll(file.Reader); err != nil {
				return nil, err
			}
		}
		files = append(files, b)
		m.Attachments = append(m.Attachments, &discordgo.MessageAttachment{
			ID:          f.newID(),
			Filename:    file.Name,
			ContentType: file.ContentType,
			URL:         fmt.Sprintf("https://cdn.fake/%s/%s/%s", channelID, m.ID, file.Name),
			Size:        len(b),
		})
	}
	f.insert(m)
	f.Sent = append(f.Sent, SentMessage{ChannelID: channelID, Data: data, Files: files})
	c := *m
	return &c, nil
}

func (f *Fake) ChannelMessageEditComplex(e *discordgo.MessageEdit, options ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("ChannelMessageEditComplex"); err != nil {
		return nil, err
	}
	_, m := f.find(e.Channel, e.ID)
	if m == nil {
		return nil, notFound("Message")
	}
	if e.Content != nil {
		m.Content = *e.Content
	}
	f.Edited = append(f.Edited, e)
	c := *m
	return &c, nil
}

func (f *Fake) ChannelMessageDelete(channelID, messageID string, options ...discordgo.RequestOption) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("ChannelMessageDelete"); err != nil {
		return err
	}
	i, m := f.find(channelID, messageID)
	if m == nil {
		return notFound("Message")
	}
	arr := f.messages[channelID]
	f.messages[channelID] = append(arr[:i:i], arr[i+1:]...)
	f.Deleted = append(f.Deleted, DeletedMessage{ChannelID: channelID, MessageID: messageID})
	return nil
}

func (f *Fake) GuildMember(guildID, userID string, options ...discordgo.RequestOption) (*discordgo.Member, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("GuildMember"); err != nil {
		return nil, err
	}
	m, ok := f.members[guildID][userID]
	if !ok {
		return nil, notFound("Member")
	}
	c := *m
	return &c, nil
}

func (f *Fake) InteractionRespond(interaction *discordgo.Interaction, resp *discordgo.InteractionResponse, options ...discordgo.RequestOption) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("InteractionRespond"); err != nil {
		return err
	}
	f.Responses = append(f.Responses, Response{Interaction: interaction, Response: resp})
	return nil
}

func (f *Fake) InteractionResponseEdit(interaction *discordgo.Interaction, newresp *discordgo.WebhookEdit, options ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("InteractionResponseEdit"); err != nil {
		return nil, err
	}
	f.ResponseEdits = append(f.ResponseEdits, ResponseEdit{Interaction: interaction, Edit: newresp})
	return &discordgo.Message{ID: f.newID()}, nil
}

func (f *Fake) FollowupMessageCreate(interaction *discordgo.Interaction, wait bool, data *discordgo.WebhookParams, options ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("FollowupMessageCreate"); err != nil {
		return nil, err
	}
	f.Followups = append(f.Followups, Followup{Interaction: interaction, Params: data})
	return &discordgo.Message{ID: f.newID()}, nil
}

// ErrInjected 是測試常用的注入錯誤。
var ErrInjected = errors.New("injected error")
