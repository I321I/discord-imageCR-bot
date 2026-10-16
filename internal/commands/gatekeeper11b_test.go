package commands

import (
	"context"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"discordbot/internal/store"
)

// 把關者第二輪（階段 11）：READY 清除已不在的伺服器時，使用者勾選中被移除的頻道也要一併拿掉，
// 仍在的伺服器的勾選保留。
func TestGK11b_PruneMissingGuildsCleansUserSelections(t *testing.T) {
	e := newChannelEnv(t)
	addSources(t, e.st, "g1", "c1")
	addSources(t, e.st, "g2", "c2")
	if err := e.st.Update(context.Background(), func(d *store.Data) error {
		d.SetUserChannels("u1", []string{"c1", "c2"})
		d.SetUserChannels("u2", []string{"c2"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	PruneMissingGuilds(context.Background(), e.st, e.hooks, []string{"g1"}, time.Now())
	d := e.st.Snapshot()
	if ids, _ := d.UserChannels("u1"); !slices.Equal(ids, []string{"c1"}) {
		t.Errorf("u1 的勾選應只剩 c1：%v", ids)
	}
	if _, chosen := d.UserChannels("u2"); chosen {
		t.Error("u2 的勾選全被移除後應視為沒選過")
	}
}

// 把關者第二輪：PruneMissingGuilds 與 READY 之後新增來源頻道（新加入的伺服器）並行時，
// 新增的頻道（setAt 晚於 receivedAt）不可被清除；-race 下不可有資料競爭。
func TestGK11b_PruneMissingGuildsConcurrentWithAdd(t *testing.T) {
	e := newChannelEnv(t)
	addSources(t, e.st, "gone", "old")
	received := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		id := "new" + strconv.Itoa(i)
		go func() {
			defer wg.Done()
			_ = e.st.Update(context.Background(), func(d *store.Data) error {
				d.AddChannel("joined", id, snowflakeAt(received.Add(time.Second)))
				return nil
			})
		}()
		go func() {
			defer wg.Done()
			PruneMissingGuilds(context.Background(), e.st, e.hooks, []string{"other"}, received)
		}()
	}
	wg.Wait()
	d := e.st.Snapshot()
	if _, _, ok := d.SourceChannel("old"); ok {
		t.Error("已不在的伺服器的舊頻道應被清除")
	}
	if n := len(d.Channels("joined")); n != 20 {
		t.Errorf("READY 之後新增的 20 個頻道都應保留，剩 %d 個", n)
	}
	if c := slices.Clone(e.hooks.removed); !slices.Equal(c, []string{"old"}) {
		t.Errorf("ChannelRemoved 應只對 old 呼叫一次：%v", c)
	}
}
