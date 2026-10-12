package discordtest

import (
	"errors"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestFake_SendThenFetch(t *testing.T) {
	f := NewFake()
	sent, err := f.ChannelMessageSendComplex("c1", &discordgo.MessageSend{Content: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.ChannelMessage("c1", sent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Content != "hello" || got.ChannelID != "c1" {
		t.Errorf("取回內容不符：%+v", got)
	}
	if got.Author == nil || got.Author.ID != f.BotID {
		t.Errorf("作者應為 bot：%+v", got.Author)
	}
	if len(f.Sent) != 1 || f.Sent[0].ChannelID != "c1" {
		t.Errorf("Sent 紀錄不符：%+v", f.Sent)
	}
}

func TestFake_FailOn(t *testing.T) {
	f := NewFake()
	m := f.AddMessage(&discordgo.Message{ChannelID: "c1", Content: "x"})
	boom := errors.New("boom")
	f.FailOn["ChannelMessageDelete"] = boom
	if err := f.ChannelMessageDelete("c1", m.ID); err != boom {
		t.Fatalf("應回注入的錯誤，得到 %v", err)
	}
	if _, err := f.ChannelMessage("c1", m.ID); err != nil {
		t.Errorf("失敗的刪除不應移除訊息：%v", err)
	}
}

func TestFake_ChannelMessagesNewestFirstWithBefore(t *testing.T) {
	f := NewFake()
	var ids []string
	for i := 0; i < 5; i++ {
		ids = append(ids, f.AddMessage(&discordgo.Message{ChannelID: "c"}).ID)
	}
	page, err := f.ChannelMessages("c", 2, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 || page[0].ID != ids[4] || page[1].ID != ids[3] {
		t.Fatalf("第一頁應為最新兩則：%v", idsOf(page))
	}
	page, _ = f.ChannelMessages("c", 2, ids[3], "", "")
	if len(page) != 2 || page[0].ID != ids[2] || page[1].ID != ids[1] {
		t.Fatalf("before 分頁錯誤：%v", idsOf(page))
	}
}

func TestFake_DeleteAndEdit(t *testing.T) {
	f := NewFake()
	m := f.AddMessage(&discordgo.Message{ChannelID: "c", Content: "a"})
	content := "b"
	if _, err := f.ChannelMessageEditComplex(&discordgo.MessageEdit{Channel: "c", ID: m.ID, Content: &content}); err != nil {
		t.Fatal(err)
	}
	got, _ := f.ChannelMessage("c", m.ID)
	if got.Content != "b" {
		t.Errorf("編輯後內容應為 b：%q", got.Content)
	}
	if err := f.ChannelMessageDelete("c", m.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ChannelMessage("c", m.ID); err == nil {
		t.Error("刪除後應取不到")
	}
	if len(f.Edited) != 1 || len(f.Deleted) != 1 {
		t.Errorf("紀錄不符：Edited=%d Deleted=%d", len(f.Edited), len(f.Deleted))
	}
}

func TestFake_InteractionRespondRecorded(t *testing.T) {
	f := NewFake()
	i := &discordgo.Interaction{ID: "1"}
	resp := &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource}
	if err := f.InteractionRespond(i, resp); err != nil {
		t.Fatal(err)
	}
	if len(f.Responses) != 1 || f.Responses[0].Response != resp || f.Responses[0].Interaction != i {
		t.Errorf("Responses 紀錄不符：%+v", f.Responses)
	}
}

func TestFake_GuildMember404WhenUnknown(t *testing.T) {
	f := NewFake()
	f.SetMember("g", &discordgo.Member{User: &discordgo.User{ID: "u"}, Roles: []string{"r"}})
	if m, err := f.GuildMember("g", "u"); err != nil || m.Roles[0] != "r" {
		t.Fatalf("應取得成員：%v %v", m, err)
	}
	_, err := f.GuildMember("g", "other")
	var re *discordgo.RESTError
	if !errors.As(err, &re) || re.Response == nil || re.Response.StatusCode != 404 {
		t.Errorf("非成員應回 404 RESTError，得到 %v", err)
	}
}

func idsOf(ms []*discordgo.Message) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.ID)
	}
	return out
}
