package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"discordbot/internal/discordtest"

	"github.com/bwmarrin/discordgo"
)

const storageCh = "storage"

func addEncoded(t *testing.T, f *discordtest.Fake, d *Data, version string) {
	t.Helper()
	msgs, err := Encode(d, version)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range msgs {
		f.AddMessage(&discordgo.Message{ChannelID: storageCh, Content: c})
	}
}

func addNoise(f *discordtest.Fake, n int) {
	for i := 0; i < n; i++ {
		f.AddMessage(&discordgo.Message{ChannelID: storageCh, Content: fmt.Sprint("noise ", i)})
	}
}

func TestLoad_PaginatesUntilComplete(t *testing.T) {
	f := discordtest.NewFake()
	want := sampleData()
	addEncoded(t, f, want, "v1")
	addNoise(f, 249) // 共 250 則，store 訊息在第 3 頁
	s := New(f, storageCh)
	if err := s.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s.Snapshot(), want) {
		t.Errorf("載入資料不符：%+v", s.Snapshot())
	}
	if n := f.CallCount("ChannelMessages"); n != 3 {
		t.Errorf("應讀 3 頁，實際 %d", n)
	}
}

func TestLoad_StopsAtFirstCompleteVersion(t *testing.T) {
	f := discordtest.NewFake()
	addNoise(f, 250)
	addEncoded(t, f, sampleData(), "v1")
	s := New(f, storageCh)
	if err := s.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := f.CallCount("ChannelMessages"); n != 1 {
		t.Errorf("最新頁就有完整版本時只應讀 1 頁，實際 %d", n)
	}
}

func TestLoad_EmptyChannel(t *testing.T) {
	f := discordtest.NewFake()
	s := New(f, storageCh)
	if err := s.Load(context.Background()); err != nil {
		t.Fatalf("空頻道應以空資料成功載入：%v", err)
	}
	if !reflect.DeepEqual(s.Snapshot(), NewData()) {
		t.Errorf("應為空資料：%+v", s.Snapshot())
	}
	if err := s.Save(context.Background()); err != nil {
		t.Fatalf("空資料應可寫入：%v", err)
	}
	if len(f.Sent) != 1 || f.Sent[0].ChannelID != storageCh {
		t.Errorf("應寫一則到儲存頻道：%+v", f.Sent)
	}
}

func TestLoad_NoCompleteVersion(t *testing.T) {
	f := discordtest.NewFake()
	msgs, _ := Encode(bigData(), "v1")
	f.AddMessage(&discordgo.Message{ChannelID: storageCh, Content: msgs[0]})
	s := New(f, storageCh)
	if err := s.Load(context.Background()); !errors.Is(err, ErrNoCompleteVersion) {
		t.Errorf("應回 ErrNoCompleteVersion，得到 %v", err)
	}
}

func TestLoad_ErrorDoesNotAllowSave(t *testing.T) {
	f := discordtest.NewFake()
	f.FailOn["ChannelMessages"] = discordtest.ErrInjected
	s := New(f, storageCh)
	if err := s.Load(context.Background()); err == nil {
		t.Fatal("API 錯誤應回錯誤")
	}
	if err := s.Save(context.Background()); !errors.Is(err, ErrNotLoaded) {
		t.Errorf("Save 應回 ErrNotLoaded，得到 %v", err)
	}
	err := s.Update(context.Background(), func(d *Data) error { d.AddChannel("g", "c", "1"); return nil })
	if !errors.Is(err, ErrNotLoaded) {
		t.Errorf("Update 應回 ErrNotLoaded，得到 %v", err)
	}
	if len(f.Sent) != 0 {
		t.Errorf("不應寫入任何訊息：%+v", f.Sent)
	}
}

func TestUseEmpty_AllowsSaveAfterFailedLoad(t *testing.T) {
	f := discordtest.NewFake()
	f.FailOn["ChannelMessages"] = discordtest.ErrInjected
	s := New(f, storageCh)
	_ = s.Load(context.Background())
	s.UseEmpty()
	if !reflect.DeepEqual(s.Snapshot(), NewData()) {
		t.Errorf("UseEmpty 後應為空資料")
	}
	if err := s.Save(context.Background()); err != nil {
		t.Errorf("UseEmpty 後應可寫入：%v", err)
	}
}

func TestLoad_FailureKeepsCurrentData(t *testing.T) {
	f := discordtest.NewFake()
	addEncoded(t, f, sampleData(), "v1")
	s := New(f, storageCh)
	if err := s.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.FailOn["ChannelMessages"] = discordtest.ErrInjected
	if err := s.Load(context.Background()); err == nil {
		t.Fatal("應回錯誤")
	}
	if !reflect.DeepEqual(s.Snapshot(), sampleData()) {
		t.Errorf("重新讀取失敗時應保留目前資料")
	}
	if err := s.Save(context.Background()); err != nil {
		t.Errorf("之前載入成功，重新讀取失敗後仍應可寫入：%v", err)
	}
}

func TestSave_ThenLoadRoundTrip(t *testing.T) {
	f := discordtest.NewFake()
	s := New(f, storageCh)
	_ = s.Load(context.Background())
	if err := s.Update(context.Background(), func(d *Data) error {
		for i := 0; i < 300; i++ {
			d.AddChannel("g", fmt.Sprintf("1%017d", i), fmt.Sprintf("9%017d", i))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(f.Sent) < 2 {
		t.Fatalf("大量資料應拆成多則，得到 %d", len(f.Sent))
	}
	s2 := New(f, storageCh)
	if err := s2.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s2.Snapshot(), s.Snapshot()) {
		t.Error("寫入後再讀取應一致")
	}
}

func TestUpdate_WriteFailure(t *testing.T) {
	f := discordtest.NewFake()
	s := New(f, storageCh)
	_ = s.Load(context.Background())
	f.FailOn["ChannelMessageSendComplex"] = discordtest.ErrInjected
	err := s.Update(context.Background(), func(d *Data) error { d.AddChannel("g", "c", "1"); return nil })
	if err == nil {
		t.Fatal("寫入失敗應回錯誤")
	}
	if _, _, ok := s.Snapshot().SourceChannel("c"); ok {
		t.Error("寫入失敗時記憶體中的變更應撤回")
	}
}

func TestUpdate_FnErrorSkipsSave(t *testing.T) {
	f := discordtest.NewFake()
	s := New(f, storageCh)
	_ = s.Load(context.Background())
	boom := errors.New("boom")
	err := s.Update(context.Background(), func(d *Data) error { d.AddChannel("g", "c", "1"); return boom })
	if err != boom {
		t.Errorf("應回 fn 的錯誤，得到 %v", err)
	}
	if len(f.Sent) != 0 {
		t.Error("fn 失敗不應寫入")
	}
	if _, _, ok := s.Snapshot().SourceChannel("c"); ok {
		t.Error("fn 失敗不應改變資料")
	}
}

func TestSnapshot_IsCopy(t *testing.T) {
	f := discordtest.NewFake()
	s := New(f, storageCh)
	_ = s.Load(context.Background())
	s.Snapshot().AddChannel("g", "c", "1")
	if _, _, ok := s.Snapshot().SourceChannel("c"); ok {
		t.Error("修改 Snapshot 不應影響 Store")
	}
}

func TestUpdate_Concurrent(t *testing.T) {
	f := discordtest.NewFake()
	s := New(f, storageCh)
	_ = s.Load(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = s.Update(context.Background(), func(d *Data) error {
				d.AddChannel("g", fmt.Sprint("c", i), fmt.Sprint(i))
				return nil
			})
			_ = s.Snapshot()
		}(i)
	}
	wg.Wait()
	if n := len(s.Snapshot().Channels("g")); n != 100 {
		t.Errorf("應有 100 個頻道，得到 %d", n)
	}
	s2 := New(f, storageCh)
	if err := s2.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(s2.Snapshot().Channels("g")); n != 100 {
		t.Errorf("儲存頻道最新版本應有 100 個頻道，得到 %d", n)
	}
}

// blockingSend 讓寫入儲存頻道卡住，直到 release 被關閉。
type blockingSend struct {
	*discordtest.Fake
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingSend) ChannelMessageSendComplex(ch string, d *discordgo.MessageSend, o ...discordgo.RequestOption) (*discordgo.Message, error) {
	b.once.Do(func() {
		close(b.started)
		<-b.release
	})
	return b.Fake.ChannelMessageSendComplex(ch, d, o...)
}

func TestSnapshot_NotBlockedByWrite(t *testing.T) {
	for _, name := range []string{"Update", "Save"} {
		api := &blockingSend{Fake: discordtest.NewFake(), started: make(chan struct{}), release: make(chan struct{})}
		s := New(api, storageCh)
		s.UseEmpty()
		ctx := context.Background()
		done := make(chan error)
		go func() {
			if name == "Save" {
				done <- s.Save(ctx)
				return
			}
			done <- s.Update(ctx, func(d *Data) error { d.AddChannel("g", "c", "1"); return nil })
		}()
		<-api.started
		got := make(chan *Data)
		go func() { got <- s.Snapshot() }()
		select {
		case d := <-got:
			if _, _, ok := d.SourceChannel("c"); ok {
				t.Errorf("%s：寫入完成前 Snapshot 不應看到新資料", name)
			}
		case <-time.After(time.Second):
			t.Errorf("%s：寫入儲存頻道期間 Snapshot 被阻擋", name)
		}
		close(api.release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if name == "Update" {
			if _, _, ok := s.Snapshot().SourceChannel("c"); !ok {
				t.Error("Update 完成後應看到新資料")
			}
		}
	}
}
