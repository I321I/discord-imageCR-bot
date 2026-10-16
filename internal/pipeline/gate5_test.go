package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"image"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"discordbot/internal/discordtest"
	"discordbot/internal/imagemsg"
	"discordbot/internal/store"

	"github.com/bwmarrin/discordgo"
)

// 把關：每張都發送失敗（例如 10～25MB 超過伺服器上傳上限）→ 不刪原訊息、不進索引、只剩 ❌。
func TestGate5_AllSendsFail_KeepsOriginal(t *testing.T) {
	e := newEnv(t)
	e.fake.FailOn["ChannelMessageSendComplex"] = errors.New("HTTP 413 Request Entity Too Large")
	m := e.imageMessage(t, "a.png", "b.png")
	if err := e.p.Process(context.Background(), testGuild, m); err == nil {
		t.Error("全部發送失敗應回錯誤")
	}
	if !e.originalExists(m) || len(e.fake.Deleted) != 0 {
		t.Errorf("發送失敗不可刪原訊息：Deleted %+v", e.fake.Deleted)
	}
	if e.index.count() != 0 {
		t.Errorf("沒發出的不應進索引，得到 %d", e.index.count())
	}
	if got := e.fake.ReactionsOn(testSource, m.ID); !slices.Equal(got, []string{"❌"}) {
		t.Errorf("原訊息反應應只剩 ❌，得到 %q", got)
	}
}

// 把關：只有第 1 張發送失敗（最後一張成功）仍不可刪除原訊息。
func TestGate5_FirstSendFails_LastSucceeds_KeepsOriginal(t *testing.T) {
	e := newEnv(t)
	e.p.API = &failNthSend{Fake: e.fake, n: 1}
	m := e.imageMessage(t, "a.png", "b.png")
	if err := e.p.Process(context.Background(), testGuild, m); err == nil {
		t.Error("有一張失敗應回錯誤")
	}
	if !e.originalExists(m) {
		t.Error("第 1 張失敗時原訊息不可刪除")
	}
}

// 把關：移除 ⏳ 失敗時仍加 ❌、不刪原訊息。
func TestGate5_UnreactFails_StillMarksFailed(t *testing.T) {
	e := newEnv(t)
	e.fake.FailOn["MessageReactionRemove"] = discordtest.ErrInjected
	e.fake.FailOn["ChannelMessageSendComplex"] = discordtest.ErrInjected
	m := e.imageMessage(t, "a.png")
	if err := e.p.Process(context.Background(), testGuild, m); err == nil {
		t.Error("應回錯誤")
	}
	if !e.originalExists(m) {
		t.Error("原訊息不可刪除")
	}
	if got := e.fake.ReactionsOn(testSource, m.ID); !slices.Contains(got, "❌") {
		t.Errorf("移除 ⏳ 失敗仍應加 ❌，得到 %q", got)
	}
}

// 把關：刪除失敗且反應全部失敗（缺權限）時不 panic，圖片已發出、原訊息仍在。
func TestGate5_DeleteAndReactionsFail(t *testing.T) {
	e := newEnv(t)
	e.fake.FailOn["ChannelMessageDelete"] = discordtest.ErrInjected
	e.fake.FailOn["MessageReactionAdd"] = discordtest.ErrInjected
	e.fake.FailOn["MessageReactionRemove"] = discordtest.ErrInjected
	m := e.imageMessage(t, "a.png")
	if err := e.p.Process(context.Background(), testGuild, m); err == nil {
		t.Error("刪除失敗應回錯誤")
	}
	if len(e.fake.Sent) != 1 || !e.originalExists(m) {
		t.Errorf("Sent %d、原訊息存在 %v", len(e.fake.Sent), e.originalExists(m))
	}
}

// 把關：下載中 ctx 結束（關機）→ 不發、不刪、不加 ❌。
func TestGate5_CancelDuringDownload(t *testing.T) {
	e := newEnv(t)
	m := e.imageMessage(t, "a.png", "b.png")
	ctx, cancel := context.WithCancel(context.Background())
	e.p.Fetch = fetchFunc(func(ctx context.Context, url string) ([]byte, error) {
		cancel()
		return nil, ctx.Err()
	})
	if err := e.p.Process(ctx, testGuild, m); err == nil {
		t.Error("應回錯誤")
	}
	if len(e.fake.Sent) != 0 || !e.originalExists(m) {
		t.Errorf("關機中不發不刪：Sent %d", len(e.fake.Sent))
	}
	if got := e.fake.ReactionsOn(testSource, m.ID); slices.Contains(got, "❌") {
		t.Errorf("關機中斷不加 ❌，得到 %q", got)
	}
}

// 把關：排在 OCR 佇列中時關機 → 等待中的 Recognize 返回，不發出。
func TestGate5_CancelWhileQueued(t *testing.T) {
	e := newEnv(t)
	m := e.imageMessage(t, "a.png")
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	e.p.Text = textFunc(func(ctx context.Context, _ image.Image) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	})
	done := make(chan error, 1)
	go func() { done <- e.p.Process(ctx, testGuild, m) }()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("應回 context.Canceled，得到 %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("關機後 Process 沒有返回")
	}
	if len(e.fake.Sent) != 0 || !e.originalExists(m) {
		t.Errorf("關機中不發不刪：Sent %d", len(e.fake.Sent))
	}
}

// 把關（Q20）：原文含 @everyone、@here、角色與使用者 mention 時，AllowedMentions 仍不允許任何通知，
// 且序列化後 parse 不含任何類型、沒有 users/roles。
func TestGate5_MentionsInContentDoNotNotify(t *testing.T) {
	e := newEnv(t)
	m := e.imageMessage(t, "a.png")
	m.Content = "@everyone @here <@&999> <@123>"
	if err := e.p.Process(context.Background(), testGuild, m); err != nil {
		t.Fatal(err)
	}
	data := e.fake.Sent[0].Data
	if !strings.Contains(data.Content, "@everyone") {
		t.Fatalf("原文應進關鍵字：%q", data.Content)
	}
	b, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		AllowedMentions *struct {
			Parse       []string `json:"parse"`
			Users       []string `json:"users"`
			Roles       []string `json:"roles"`
			RepliedUser bool     `json:"replied_user"`
		} `json:"allowed_mentions"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	am := got.AllowedMentions
	if am == nil || len(am.Parse) != 0 || len(am.Users) != 0 || len(am.Roles) != 0 || am.RepliedUser {
		t.Errorf("allowed_mentions 應存在且不允許任何通知：%s", b)
	}
	// 使用者決定 6：與 Discord 文件一致，明確送出 "parse":[]。
	if !strings.Contains(string(b), `"allowed_mentions":{"parse":[]`) {
		t.Errorf("allowed_mentions 應序列化為 \"parse\":[]：%s", b)
	}
}

// 把關：不同伺服器的來源頻道，索引收到的伺服器 ID 各自正確（不混用）。
func TestGate5_GuildIDsNotMixed(t *testing.T) {
	e := newEnv(t)
	if err := e.p.Store.Update(context.Background(), func(d *store.Data) error {
		d.AddChannel("g2", "src2", "1")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	m1 := e.imageMessage(t, "a.png")
	m2 := e.imageMessage(t, "b.png")
	m2.ChannelID = "src2"
	m2.GuildID = "g2"
	e.fake.AddMessage(m2)
	e.p.HandleMessage(context.Background(), m1)
	e.p.HandleMessage(context.Background(), m2)
	if !slices.Equal(e.index.gids, []string{testGuild, "g2"}) {
		t.Errorf("伺服器 ID 應各自正確，得到 %v", e.index.gids)
	}
}

// 把關：多則訊息並行時，同時解碼的數量不超過 1（以解碼位置計）。
func TestGate5_DecodeSlotExclusive(t *testing.T) {
	p := &Pipeline{}
	var cur, peak atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rel := p.acquireDecode(context.Background())
			n := cur.Add(1)
			for {
				old := peak.Load()
				if n <= old || peak.CompareAndSwap(old, n) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			cur.Add(-1)
			rel()
		}()
	}
	wg.Wait()
	if peak.Load() != 1 {
		t.Errorf("同時解碼數峰值 %d，預期 1", peak.Load())
	}
}

type fetchFunc func(ctx context.Context, url string) ([]byte, error)

func (f fetchFunc) Fetch(ctx context.Context, url string) ([]byte, error) { return f(ctx, url) }

var _ = imagemsg.FailedMarker
var _ discordgo.Message
