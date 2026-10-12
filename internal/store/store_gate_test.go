package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"unicode/utf8"

	"discordbot/internal/discordtest"

	"github.com/bwmarrin/discordgo"
)

// 把關者補充：資料大到需要 10 則以上（標頭位數變化）時仍每則 ≤ 2000 字並可還原，且含非 ASCII。
func TestGate_EncodeManyParts_RoundTrip(t *testing.T) {
	d := NewData()
	for i := 0; i < 1500; i++ {
		d.AddChannel(fmt.Sprintf("1%017d", i%7), fmt.Sprintf("2%017d", i), fmt.Sprintf("3%017d", i))
	}
	d.SetUserChannels("使用者", []string{"頻道一", "頻道二"})
	msgs, err := Encode(d, "1234567890123456789")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) < 10 {
		t.Fatalf("預期至少 10 則，得到 %d", len(msgs))
	}
	var dm []*discordgo.Message
	for i := len(msgs) - 1; i >= 0; i-- { // 由新到舊
		if n := utf8.RuneCountInString(msgs[i]); n > maxMessageLen {
			t.Errorf("第 %d 則 %d 字", i+1, n)
		}
		dm = append(dm, &discordgo.Message{Content: msgs[i]})
	}
	got, err := DecodeLatest(dm)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Guilds) != len(d.Guilds) || len(got.UserSelections) != 1 {
		t.Errorf("還原不一致")
	}
}

// 把關者補充：最新版本的多則訊息跨越分頁邊界時，仍應讀到最新版本。
func TestGate_Load_VersionSpansPageBoundary(t *testing.T) {
	f := discordtest.NewFake()
	old := NewData()
	old.AddChannel("g", "old", "1")
	addEncoded(t, f, old, "v1")
	addNoise(f, 150)
	big := NewData()
	for i := 0; i < 120; i++ {
		big.AddChannel("g", fmt.Sprintf("1%017d", i), fmt.Sprintf("2%017d", i))
	}
	msgs, _ := Encode(big, "v2")
	if len(msgs) < 2 {
		t.Fatalf("需要多則，得到 %d", len(msgs))
	}
	f.AddMessage(&discordgo.Message{ChannelID: storageCh, Content: msgs[0]})
	addNoise(f, 99) // 讓 1/n 落在第二頁
	for _, c := range msgs[1:] {
		f.AddMessage(&discordgo.Message{ChannelID: storageCh, Content: c})
	}
	s := New(f, storageCh)
	if err := s.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(s.Snapshot().Channels("g")); n != 120 {
		t.Errorf("應讀到最新版本 120 個頻道，得到 %d", n)
	}
}

// 把關者補充：寫入到一半失敗（第 2 則失敗），記憶體不變，重新讀取得到前一個完整版本。
type failNth struct {
	*discordtest.Fake
	mu    sync.Mutex
	n     int
	failN int
}

func (f *failNth) ChannelMessageSendComplex(ch string, d *discordgo.MessageSend, o ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.mu.Lock()
	f.n++
	n := f.n
	f.mu.Unlock()
	if n == f.failN {
		return nil, discordtest.ErrInjected
	}
	return f.Fake.ChannelMessageSendComplex(ch, d, o...)
}

func TestGate_Update_PartialWriteFailure(t *testing.T) {
	base := discordtest.NewFake()
	api := &failNth{Fake: base, failN: -1}
	s := New(api, storageCh)
	ctx := context.Background()
	if err := s.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(ctx, func(d *Data) error { d.AddChannel("g", "first", "1"); return nil }); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	api.failN = api.n + 2
	api.mu.Unlock()
	err := s.Update(ctx, func(d *Data) error {
		for i := 0; i < 120; i++ {
			d.AddChannel("g", fmt.Sprintf("1%017d", i), fmt.Sprintf("2%017d", i))
		}
		return nil
	})
	if !errors.Is(err, discordtest.ErrInjected) {
		t.Fatalf("預期寫入錯誤，得到 %v", err)
	}
	if n := len(s.Snapshot().Channels("g")); n != 1 {
		t.Errorf("記憶體應保持 1 個頻道，得到 %d", n)
	}
	s2 := New(base, storageCh)
	if err := s2.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(s2.Snapshot().Channels("g")); n != 1 {
		t.Errorf("重新讀取應得前一個完整版本（1 個頻道），得到 %d", n)
	}
}

// 把關者補充：Load（/storage reload）進行中有 Update 完成時，Update 的結果不應被舊資料蓋掉。
type blockingList struct {
	*discordtest.Fake
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingList) ChannelMessages(ch string, limit int, before, after, around string, o ...discordgo.RequestOption) ([]*discordgo.Message, error) {
	msgs, err := b.Fake.ChannelMessages(ch, limit, before, after, around, o...)
	b.once.Do(func() {
		close(b.started)
		<-b.release
	})
	return msgs, err
}

func TestGate_ReloadConcurrentWithUpdate_LosesUpdate(t *testing.T) {
	base := discordtest.NewFake()
	ctx := context.Background()
	s0 := New(base, storageCh)
	_ = s0.Load(ctx)
	_ = s0.Update(ctx, func(d *Data) error { d.AddChannel("g", "a", "1"); return nil })

	api := &blockingList{Fake: base, started: make(chan struct{}), release: make(chan struct{})}
	s := New(api, storageCh)
	s.UseEmpty()
	done := make(chan error)
	go func() { done <- s.Load(ctx) }()
	<-api.started
	if err := s.Update(ctx, func(d *Data) error { d.AddChannel("g", "b", "2"); return nil }); err != nil {
		t.Fatal(err)
	}
	close(api.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, _, ok := s.Snapshot().SourceChannel("b"); !ok {
		t.Error("reload 期間完成的 Update 被 Load 讀到的舊資料覆蓋，記憶體遺失頻道 b")
	}
}
