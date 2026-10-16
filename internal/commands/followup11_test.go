package commands

import (
	"context"
	"slices"
	"testing"
	"time"

	"discordbot/internal/discordtest"
	"discordbot/internal/store"
)

// 階段 11 的使用者決定 1：READY 時比對 bot 所在的伺服器清單，清除已不在的伺服器的來源頻道（離線期間被踢出的殘留）。

func TestPruneMissingGuilds_RemovesAbsentGuildsInOneWrite(t *testing.T) {
	e := newChannelEnv(t)
	addSources(t, e.st, "g1", "c1", "c2")
	addSources(t, e.st, "g2", "c3")
	addSources(t, e.st, "g3", "c4")
	sent := len(e.fake.Sent)
	PruneMissingGuilds(context.Background(), e.st, e.hooks, []string{"g2"}, time.Now())
	d := e.st.Snapshot()
	for _, c := range []string{"c1", "c2", "c4"} {
		if _, _, ok := d.SourceChannel(c); ok {
			t.Errorf("已不在的伺服器的來源頻道 %s 應被移除", c)
		}
	}
	if _, _, ok := d.SourceChannel("c3"); !ok {
		t.Error("仍在的伺服器的來源頻道不應被動")
	}
	if len(e.fake.Sent) != sent+1 {
		t.Errorf("應寫回儲存頻道一次，得到 %d 次", len(e.fake.Sent)-sent)
	}
	removed := slices.Clone(e.hooks.removed)
	slices.Sort(removed)
	if !slices.Equal(removed, []string{"c1", "c2", "c4"}) {
		t.Errorf("應對每個被移除的頻道呼叫 ChannelRemoved：%v", e.hooks.removed)
	}
}

func TestPruneMissingGuilds_NoChangeDoesNotWrite(t *testing.T) {
	e := newChannelEnv(t)
	addSources(t, e.st, "g1", "c1")
	sent := len(e.fake.Sent)
	PruneMissingGuilds(context.Background(), e.st, e.hooks, []string{"g1", "g9"}, time.Now())
	if len(e.fake.Sent) != sent || len(e.hooks.removed) != 0 {
		t.Error("沒有變動時不應寫入或呼叫 hook")
	}
}

func TestPruneMissingGuilds_WriteFailureKeepsData(t *testing.T) {
	e := newChannelEnv(t)
	addSources(t, e.st, "g1", "c1")
	e.fake.FailOn["ChannelMessageSendComplex"] = discordtest.ErrInjected
	PruneMissingGuilds(context.Background(), e.st, e.hooks, nil, time.Now())
	if _, _, ok := e.st.Snapshot().SourceChannel("c1"); !ok {
		t.Error("寫入失敗時記憶體不變")
	}
	if len(e.hooks.removed) != 0 {
		t.Error("寫入失敗不應呼叫 hook")
	}
}

// 收到 READY 之後才設定的來源頻道（READY 之後才加入的伺服器）不可被清除；setAt 無法解析時也不清除（同 PruneMissingChannels）。
func TestPruneMissingGuilds_KeepsChannelsSetAfterReceived(t *testing.T) {
	e := newChannelEnv(t)
	received := time.Now()
	if err := e.st.Update(context.Background(), func(d *store.Data) error {
		d.AddChannel("g1", "old", snowflakeAt(received.Add(-time.Second)))
		d.AddChannel("g2", "new", snowflakeAt(received.Add(time.Second)))
		d.AddChannel("g3", "weird", "not-a-snowflake")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	PruneMissingGuilds(context.Background(), e.st, e.hooks, nil, received)
	d := e.st.Snapshot()
	if _, _, ok := d.SourceChannel("old"); ok {
		t.Error("READY 之前設定、伺服器已不在的來源頻道應被清除")
	}
	for _, c := range []string{"new", "weird"} {
		if _, _, ok := d.SourceChannel(c); !ok {
			t.Errorf("%s 不應被清除", c)
		}
	}
	if !slices.Equal(e.hooks.removed, []string{"old"}) {
		t.Errorf("只應對 old 呼叫 ChannelRemoved：%v", e.hooks.removed)
	}
}

// 階段 11 的使用者決定 3：/sources 沒有可用頻道時，結果不完整回「目前無法確認權限，請稍後再試」，
// 完整才回「目前沒有可用的圖庫頻道」。
func TestSources_NoChannelsIncompleteAsksRetry(t *testing.T) {
	for _, c := range []struct {
		incomplete bool
		want       string
	}{{true, msgPermissionUnknown}, {false, msgSourcesNone}} {
		e := newPKEnv(t)
		e.access.incomplete = c.incomplete
		e.s.Command(context.Background(), sourcesCommand(pkUser, false))
		ed := lastSourcesEdit(t, e.fake)
		if *ed.Content != c.want {
			t.Errorf("incomplete=%v：內容 %q，預期 %q", c.incomplete, *ed.Content, c.want)
		}
		if ed.Components == nil || len(*ed.Components) != 0 {
			t.Errorf("incomplete=%v：不應有選單", c.incomplete)
		}
	}
}
