package commands

import (
	"context"
	"strings"
	"sync"
	"testing"

	"discordbot/internal/discordtest"
	"discordbot/internal/store"

	"github.com/bwmarrin/discordgo"
)

// 把關者補充：兩個伺服器同時新增、移除頻道，資料不互相干擾，每個 interaction 都有回應。
func TestGate_ChannelConcurrentAcrossGuilds(t *testing.T) {
	e := newChannelEnv(t)
	h := NewChannelHandler(e.st, e.state, e.fake, NopIndexHooks{})
	var is []*discordgo.InteractionCreate
	for k := 0; k < 20; k++ {
		is = append(is,
			channelInteraction("g1", "c1", "add"),
			channelInteraction("g1", "c2", "add"),
			channelInteraction("g2", "c3", "add"),
			channelInteraction("g1", "c1", "list"),
			channelInteraction("g2", "c3", "list"),
		)
	}
	var wg sync.WaitGroup
	for _, i := range is {
		wg.Add(1)
		go func(i *discordgo.InteractionCreate) {
			defer wg.Done()
			h(context.Background(), i)
		}(i)
	}
	wg.Wait()
	d := e.st.Snapshot()
	if got := d.Channels("g1"); len(got) != 2 {
		t.Errorf("g1 應有 2 個頻道，得到 %v", got)
	}
	if got := d.Channels("g2"); len(got) != 1 || got[0] != "c3" {
		t.Errorf("g2 應只有 c3，得到 %v", got)
	}
	if len(e.fake.Responses) != len(is) {
		t.Errorf("每個 interaction 都應回應：%d/%d", len(e.fake.Responses), len(is))
	}
	if n := 3 * 20; len(e.fake.ResponseEdits) != n {
		t.Errorf("每個 add 都應在 deferred 後補上內容：%d/%d", len(e.fake.ResponseEdits), n)
	}
	if len(e.fake.Sent) != 3 {
		t.Errorf("應只寫入 3 次（重複新增不寫入），得到 %d", len(e.fake.Sent))
	}
	for _, r := range e.fake.Responses {
		if r.Interaction.GuildID == "g2" && strings.Contains(r.Response.Data.Content, "伺服器一") {
			t.Errorf("g2 的回覆混入 g1 資料：%q", r.Response.Data.Content)
		}
	}
	for _, r := range e.fake.ResponseEdits {
		if r.Interaction.GuildID == "g2" && strings.Contains(*r.Edit.Content, "伺服器一") {
			t.Errorf("g2 的回覆混入 g1 資料：%q", *r.Edit.Content)
		}
	}
}

// 把關者補充：remove 從其他伺服器的頻道下指令時不影響原伺服器的同名 ID（以 guild 隔離）。
func TestGate_ChannelRemove_FromWrongGuildDoesNothing(t *testing.T) {
	e := newChannelEnv(t)
	e.run(channelInteraction("g1", "c1", "add"))
	// 偽造：g2 的 interaction 帶 c1（例如 state 不一致）
	e.run(channelInteraction("g2", "c1", "remove"))
	if _, _, ok := e.st.Snapshot().SourceChannel("c1"); !ok {
		t.Error("g2 不應能移除 g1 的來源頻道")
	}
}

// 把關者補充：新增成功後若列表中有已不在 state 的頻道，該頻道顯示 ID，回覆仍看得出已新增成功。
func TestGate_ChannelAdd_SuccessButListHasDeletedChannel(t *testing.T) {
	e := newChannelEnv(t)
	_ = e.st.Update(context.Background(), func(d *store.Data) error {
		d.AddChannel("g1", "gone", "1")
		return nil
	})
	e.run(channelInteraction("g1", "c1", "add"))
	got := lastEdit(t, e.fake)
	if _, _, ok := e.st.Snapshot().SourceChannel("c1"); !ok {
		t.Fatal("c1 應已新增")
	}
	if want := "成功新增頻道！新增後頻道列表：\n伺服器：伺服器一\n[gone、一般]\n"; got != want {
		t.Errorf("回覆 %q，預期 %q", got, want)
	}
}

// 把關者補充：Router 並行處理不同類型 interaction 不 panic。
func TestGate_RouterConcurrent(t *testing.T) {
	var mu sync.Mutex
	count := 0
	h := func(ctx context.Context, i *discordgo.InteractionCreate) {
		mu.Lock()
		count++
		mu.Unlock()
	}
	r := &Router{Commands: map[string]Handler{"x": h}, Components: map[string]Handler{"p": h}}
	var wg sync.WaitGroup
	for k := 0; k < 100; k++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			r.Handle(nil, &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{Type: discordgo.InteractionApplicationCommand, Data: discordgo.ApplicationCommandInteractionData{Name: "x"}}})
		}()
		go func() {
			defer wg.Done()
			r.Handle(nil, &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{Type: discordgo.InteractionMessageComponent, Data: discordgo.MessageComponentInteractionData{CustomID: "p:1"}}})
		}()
	}
	wg.Wait()
	if count != 200 {
		t.Errorf("預期 200 次，得到 %d", count)
	}
}

// 把關者補充：開發者 ID 清單為空（Application 查詢失敗）時拒絕所有人且有回應。
func TestGate_DevEmptyIDsRejects(t *testing.T) {
	f := discordtest.NewFake()
	st := store.New(f, "storage")
	h := NewDevHandler(st, nil, "storage", f)
	h(context.Background(), &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		Type: discordgo.InteractionApplicationCommand,
		User: &discordgo.User{ID: ""},
		Data: discordgo.ApplicationCommandInteractionData{Name: CmdStorage, Options: []*discordgo.ApplicationCommandInteractionDataOption{{Name: "save"}}},
	}})
	if len(f.Responses) != 1 || f.Responses[0].Response.Data.Content != msgDevOnly {
		t.Errorf("應拒絕：%+v", f.Responses)
	}
	if f.CallCount("ChannelMessageSendComplex") != 0 {
		t.Error("不應寫入")
	}
}
