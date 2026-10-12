package store

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"discordbot/internal/discordtest"

	"github.com/bwmarrin/discordgo"
)

// 把關者第二輪：兩把鎖（writeMu、mu）與 generation 的並行正確性。

// chaosAPI 在每次呼叫時讓出排程，並以一定機率讓寫入失敗（模擬寫到一半失敗）。
type chaosAPI struct {
	*discordtest.Fake
	failRate float64
	disabled atomic.Bool
}

func (c *chaosAPI) ChannelMessages(ch string, limit int, before, after, around string, o ...discordgo.RequestOption) ([]*discordgo.Message, error) {
	runtime.Gosched()
	return c.Fake.ChannelMessages(ch, limit, before, after, around, o...)
}

func (c *chaosAPI) ChannelMessageSendComplex(ch string, d *discordgo.MessageSend, o ...discordgo.RequestOption) (*discordgo.Message, error) {
	runtime.Gosched()
	if !c.disabled.Load() && rand.Float64() < c.failRate {
		return nil, discordtest.ErrInjected
	}
	return c.Fake.ChannelMessageSendComplex(ch, d, o...)
}

// firstListBlocks 只讓第一次 ChannelMessages 卡住；之後的呼叫（例如第二個 Load）直接完成。
type firstListBlocks struct {
	*discordtest.Fake
	started, release chan struct{}
	n                atomic.Int32
}

func (b *firstListBlocks) ChannelMessages(ch string, limit int, before, after, around string, o ...discordgo.RequestOption) ([]*discordgo.Message, error) {
	msgs, err := b.Fake.ChannelMessages(ch, limit, before, after, around, o...)
	if b.n.Add(1) == 1 {
		close(b.started)
		<-b.release
	}
	return msgs, err
}

func runWithDeadline(t *testing.T, d time.Duration, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { f(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		t.Fatalf("疑似死結：%v 內沒有完成\n%s", d, buf[:n])
	}
}

// 大量並行 Update / Save / Load / Snapshot，並注入寫入失敗：
//   - 不死結
//   - Snapshot 從未看到寫入失敗的頻道（未寫入成功的資料）
//   - 最後記憶體 = 所有成功的 Update；重新讀取儲存頻道也得到相同結果（沒有遺失更新）
func TestGate2_Stress_NoDeadlockNoLostUpdateNoDirtyRead(t *testing.T) {
	for round := 0; round < 5; round++ {
		base := discordtest.NewFake()
		// 先放一個大的初始版本，讓每次寫入都是多則訊息，才會出現「寫到一半失敗」。
		init := NewData()
		for i := 0; i < 120; i++ {
			init.AddChannel("g", fmt.Sprintf("1%017d", i), fmt.Sprintf("2%017d", i))
		}
		addEncoded(t, base, init, "0")
		api := &chaosAPI{Fake: base, failRate: 0.15}
		s := New(api, storageCh)
		ctx := context.Background()
		if err := s.Load(ctx); err != nil {
			t.Fatal(err)
		}

		var (
			mu        sync.Mutex
			succeeded = map[string]bool{}
			failed    = map[string]bool{}
			observed  = map[string]bool{}
		)
		const writers = 40
		runWithDeadline(t, 60*time.Second, func() {
			var wg sync.WaitGroup
			stop := make(chan struct{})
			var readers sync.WaitGroup
			for r := 0; r < 4; r++ {
				readers.Add(1)
				go func() {
					defer readers.Done()
					for {
						select {
						case <-stop:
							return
						default:
						}
						for _, id := range s.Snapshot().Channels("w") {
							mu.Lock()
							observed[id] = true
							mu.Unlock()
						}
						runtime.Gosched()
					}
				}()
			}
			for i := 0; i < writers; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					id := fmt.Sprintf("w%03d", i)
					err := s.Update(ctx, func(d *Data) error {
						d.AddChannel("w", id, fmt.Sprint(i+1))
						return nil
					})
					mu.Lock()
					if err == nil {
						succeeded[id] = true
					} else {
						failed[id] = true
					}
					mu.Unlock()
				}(i)
			}
			for i := 0; i < 10; i++ {
				wg.Add(2)
				go func() { defer wg.Done(); _ = s.Load(ctx) }()
				go func() { defer wg.Done(); _ = s.Save(ctx) }()
			}
			wg.Wait()
			close(stop)
			readers.Wait()
		})

		got := map[string]bool{}
		for _, id := range s.Snapshot().Channels("w") {
			got[id] = true
		}
		for id := range observed {
			if failed[id] {
				t.Errorf("round %d：Snapshot 看到寫入失敗的頻道 %s", round, id)
			}
		}
		for id := range succeeded {
			if !got[id] {
				t.Errorf("round %d：成功的 Update %s 在記憶體中遺失", round, id)
			}
		}
		for id := range got {
			if !succeeded[id] {
				t.Errorf("round %d：記憶體中出現未成功的頻道 %s", round, id)
			}
		}
		if len(s.Snapshot().Channels("g")) != 120 {
			t.Errorf("round %d：初始資料遺失", round)
		}

		// 最後再 Load 一次（不注入失敗）：儲存頻道最新完整版本與記憶體一致。
		api.disabled.Store(true)
		if err := s.Load(ctx); err != nil {
			t.Fatal(err)
		}
		after := s.Snapshot().Channels("w")
		if len(after) != len(got) {
			t.Errorf("round %d：重新讀取後頻道數 %d，記憶體原本 %d（儲存頻道與記憶體不一致）", round, len(after), len(got))
		}
		t.Logf("round %d：成功 %d、失敗 %d", round, len(succeeded), len(failed))
	}
}

// Load 讀取期間有 Save 完成：Load 回傳成功、不覆蓋（Save 寫的就是目前資料）。
// Load 讀取期間有另一個 Load 完成：本次不覆蓋也不出錯。
func TestGate2_Load_DiscardedWhenGenerationChanged(t *testing.T) {
	for _, name := range []string{"Save", "Load", "UseEmpty"} {
		base := discordtest.NewFake()
		ctx := context.Background()
		d := NewData()
		d.AddChannel("g", "a", "1")
		addEncoded(t, base, d, "1")
		api := &firstListBlocks{Fake: base, started: make(chan struct{}), release: make(chan struct{})}
		s := New(api, storageCh)
		s.UseEmpty()
		done := make(chan error)
		go func() { done <- s.Load(ctx) }()
		<-api.started
		switch name {
		case "Save":
			if err := s.Save(ctx); err != nil { // 寫入空資料為最新版本
				t.Fatal(err)
			}
		case "Load":
			// 只有第一次讀取會卡住，第二個 Load 直接完成並讀到 "a"
			if err := s.Load(ctx); err != nil {
				t.Fatal(err)
			}
		case "UseEmpty":
			s.UseEmpty()
		}
		close(api.release)
		runWithDeadline(t, 5*time.Second, func() {
			if err := <-done; err != nil {
				t.Errorf("%s：%v", name, err)
			}
		})
		_, _, has := s.Snapshot().SourceChannel("a")
		switch name {
		case "Save":
			if has {
				t.Errorf("Save：Load 讀到的舊版本覆蓋了較新寫入（空資料）")
			}
		case "Load":
			if !has {
				t.Errorf("Load：第二個 Load 的結果遺失")
			}
		case "UseEmpty":
			if has {
				t.Errorf("UseEmpty：Load 讀到的資料覆蓋了之後的 UseEmpty")
			}
		}
	}
}

// Load 讀取期間有 Update 失敗（generation 不變）：Load 照常替換。
func TestGate2_Load_ReplacesWhenConcurrentUpdateFailed(t *testing.T) {
	base := discordtest.NewFake()
	ctx := context.Background()
	d := NewData()
	d.AddChannel("g", "a", "1")
	addEncoded(t, base, d, "1")
	api := &blockingList{Fake: base, started: make(chan struct{}), release: make(chan struct{})}
	s := New(api, storageCh)
	s.UseEmpty()
	done := make(chan error)
	go func() { done <- s.Load(ctx) }()
	<-api.started
	base.FailOn["ChannelMessageSendComplex"] = discordtest.ErrInjected
	err := s.Update(ctx, func(d *Data) error { d.AddChannel("g", "b", "2"); return nil })
	if !errors.Is(err, discordtest.ErrInjected) {
		t.Fatalf("預期寫入失敗，得到 %v", err)
	}
	close(api.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	snap := s.Snapshot()
	if _, _, ok := snap.SourceChannel("a"); !ok {
		t.Error("Update 失敗不應讓 Load 放棄替換")
	}
	if _, _, ok := snap.SourceChannel("b"); ok {
		t.Error("失敗的 Update 資料不應出現")
	}
}

// 寫入卡住期間：Snapshot 不阻擋；另一個 Update 會等待（依序），且不會死結；
// Load 會等到寫入結束才開始（讀 generation 需要 writeMu）。
func TestGate2_WriteBlocked_OthersQueueNoDeadlock(t *testing.T) {
	api := &blockingSend{Fake: discordtest.NewFake(), started: make(chan struct{}), release: make(chan struct{})}
	s := New(api, storageCh)
	s.UseEmpty()
	ctx := context.Background()
	first := make(chan error)
	go func() {
		first <- s.Update(ctx, func(d *Data) error { d.AddChannel("g", "a", "1"); return nil })
	}()
	<-api.started
	second := make(chan error)
	go func() {
		second <- s.Update(ctx, func(d *Data) error {
			if _, _, ok := d.SourceChannel("a"); !ok {
				return errors.New("第二個 Update 沒看到第一個的結果")
			}
			d.AddChannel("g", "b", "2")
			return nil
		})
	}()
	loadDone := make(chan error)
	go func() { loadDone <- s.Load(ctx) }()
	select {
	case <-second:
		t.Fatal("第一個寫入未完成時第二個 Update 不應完成")
	case <-time.After(50 * time.Millisecond):
	}
	for i := 0; i < 100; i++ {
		_ = s.Snapshot()
	}
	close(api.release)
	runWithDeadline(t, 5*time.Second, func() {
		if err := <-first; err != nil {
			t.Error(err)
		}
		if err := <-second; err != nil {
			t.Error(err)
		}
		if err := <-loadDone; err != nil {
			t.Error(err)
		}
	})
	snap := s.Snapshot()
	for _, id := range []string{"a", "b"} {
		if _, _, ok := snap.SourceChannel(id); !ok {
			t.Errorf("遺失 %s", id)
		}
	}
}

// fn 內呼叫 Snapshot 不死結（fn 執行時持有 writeMu，Snapshot 只取 mu）。
func TestGate2_SnapshotInsideUpdateFn(t *testing.T) {
	s := New(discordtest.NewFake(), storageCh)
	s.UseEmpty()
	runWithDeadline(t, 5*time.Second, func() {
		err := s.Update(context.Background(), func(d *Data) error {
			_ = s.Snapshot()
			d.AddChannel("g", "a", "1")
			return nil
		})
		if err != nil {
			t.Error(err)
		}
	})
}

// fn 修改的是複本：fn 回傳錯誤後，就算 fn 已經改了 d，也不影響記憶體。
func TestGate2_UpdateFnErrorAfterMutation(t *testing.T) {
	s := New(discordtest.NewFake(), storageCh)
	s.UseEmpty()
	err := s.Update(context.Background(), func(d *Data) error {
		d.AddChannel("g", "a", "1")
		return errors.New("x")
	})
	if err == nil {
		t.Fatal("預期錯誤")
	}
	if _, _, ok := s.Snapshot().SourceChannel("a"); ok {
		t.Error("fn 錯誤後記憶體不應改變")
	}
}

// Save 期間 Snapshot 看到的是目前資料，Save 失敗也不改變記憶體。
func TestGate2_SaveFailureKeepsData(t *testing.T) {
	f := discordtest.NewFake()
	s := New(f, storageCh)
	s.UseEmpty()
	ctx := context.Background()
	_ = s.Update(ctx, func(d *Data) error { d.AddChannel("g", "a", "1"); return nil })
	f.FailOn["ChannelMessageSendComplex"] = discordtest.ErrInjected
	if err := s.Save(ctx); err == nil {
		t.Fatal("預期錯誤")
	}
	if _, _, ok := s.Snapshot().SourceChannel("a"); !ok {
		t.Error("Save 失敗不應改變記憶體")
	}
}
