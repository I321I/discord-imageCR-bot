package commands

import (
	"context"
	"slices"
	"strconv"
	"testing"
	"time"

	"discordbot/internal/discordtest"
	"discordbot/internal/store"
)

// addSources 在 store 中加入來源頻道（不經過 state 檢查）。
func addSources(t *testing.T, st *store.Store, guildID string, channelIDs ...string) {
	t.Helper()
	if err := st.Update(context.Background(), func(d *store.Data) error {
		for i, id := range channelIDs {
			d.AddChannel(guildID, id, string(rune('1'+i)))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestGuildRemoved_RemovesAllGuildChannelsAndSaves(t *testing.T) {
	e := newChannelEnv(t)
	addSources(t, e.st, "g1", "c1", "c2")
	addSources(t, e.st, "g2", "c3")
	sent := len(e.fake.Sent)
	GuildRemoved(context.Background(), e.st, e.hooks, "g1")
	d := e.st.Snapshot()
	if n := len(d.Channels("g1")); n != 0 {
		t.Errorf("伺服器 g1 的來源頻道應全部移除，剩 %d 個", n)
	}
	if _, _, ok := d.SourceChannel("c3"); !ok {
		t.Error("其他伺服器的來源頻道不應被動")
	}
	if len(e.fake.Sent) != sent+1 {
		t.Errorf("應寫回儲存頻道一次，得到 %d 次", len(e.fake.Sent)-sent)
	}
	removed := slices.Clone(e.hooks.removed)
	slices.Sort(removed)
	if !slices.Equal(removed, []string{"c1", "c2"}) {
		t.Errorf("應對每個被移除的頻道呼叫 ChannelRemoved：%v", e.hooks.removed)
	}
}

func TestGuildRemoved_NoSourcesDoesNothing(t *testing.T) {
	e := newChannelEnv(t)
	addSources(t, e.st, "g2", "c3")
	sent := len(e.fake.Sent)
	GuildRemoved(context.Background(), e.st, e.hooks, "g1")
	if len(e.fake.Sent) != sent || len(e.hooks.removed) != 0 {
		t.Error("沒有來源頻道時不應寫入或呼叫 hook")
	}
}

func TestGuildRemoved_WriteFailureKeepsData(t *testing.T) {
	e := newChannelEnv(t)
	addSources(t, e.st, "g1", "c1")
	e.fake.FailOn["ChannelMessageSendComplex"] = discordtest.ErrInjected
	GuildRemoved(context.Background(), e.st, e.hooks, "g1")
	if _, _, ok := e.st.Snapshot().SourceChannel("c1"); !ok {
		t.Error("寫入失敗時記憶體不變")
	}
	if len(e.hooks.removed) != 0 {
		t.Error("寫入失敗不應呼叫 hook")
	}
}

func TestPruneMissingChannels_RemovesOnlyMissing(t *testing.T) {
	e := newChannelEnv(t)
	addSources(t, e.st, "g1", "c1", "gone1", "gone2")
	addSources(t, e.st, "g2", "c3")
	sent := len(e.fake.Sent)
	PruneMissingChannels(context.Background(), e.st, e.hooks, "g1", []string{"c1", "c2"}, time.Now())
	d := e.st.Snapshot()
	if got := d.Channels("g1"); !slices.Equal(got, []string{"c1"}) {
		t.Errorf("應只留下仍存在的 c1，得到 %v", got)
	}
	if _, _, ok := d.SourceChannel("c3"); !ok {
		t.Error("其他伺服器的來源頻道不應被動")
	}
	if len(e.fake.Sent) != sent+1 {
		t.Errorf("應寫回儲存頻道一次，得到 %d 次", len(e.fake.Sent)-sent)
	}
	removed := slices.Clone(e.hooks.removed)
	slices.Sort(removed)
	if !slices.Equal(removed, []string{"gone1", "gone2"}) {
		t.Errorf("應對每個被移除的頻道呼叫 ChannelRemoved：%v", e.hooks.removed)
	}
}

func TestPruneMissingChannels_NoChangeDoesNotWrite(t *testing.T) {
	e := newChannelEnv(t)
	addSources(t, e.st, "g1", "c1")
	sent := len(e.fake.Sent)
	PruneMissingChannels(context.Background(), e.st, e.hooks, "g1", []string{"c1"}, time.Now())
	if len(e.fake.Sent) != sent || len(e.hooks.removed) != 0 {
		t.Error("沒有變動時不應寫入或呼叫 hook")
	}
}

func TestPruneMissingChannels_WriteFailureKeepsData(t *testing.T) {
	e := newChannelEnv(t)
	addSources(t, e.st, "g1", "gone")
	e.fake.FailOn["ChannelMessageSendComplex"] = discordtest.ErrInjected
	PruneMissingChannels(context.Background(), e.st, e.hooks, "g1", nil, time.Now())
	if _, _, ok := e.st.Snapshot().SourceChannel("gone"); !ok {
		t.Error("寫入失敗時記憶體不變")
	}
	if len(e.hooks.removed) != 0 {
		t.Error("寫入失敗不應呼叫 hook")
	}
}

// snowflakeAt 回傳時間點 t 對應的 snowflake（worker/increment 為 0）。
func snowflakeAt(t time.Time) string {
	ms := uint64(t.UnixMilli() - 1420070400000)
	return strconv.FormatUint(ms<<22, 10)
}

// GUILD_CREATE 收到之後才設定的來源頻道（事件中的頻道清單可能還沒有它）不可被清除。
func TestPruneMissingChannels_KeepsChannelsSetAfterReceived(t *testing.T) {
	e := newChannelEnv(t)
	received := time.Now()
	if err := e.st.Update(context.Background(), func(d *store.Data) error {
		d.AddChannel("g1", "old", snowflakeAt(received.Add(-time.Second)))
		d.AddChannel("g1", "new", snowflakeAt(received.Add(time.Second)))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	PruneMissingChannels(context.Background(), e.st, e.hooks, "g1", []string{"c1"}, received)
	d := e.st.Snapshot()
	if _, _, ok := d.SourceChannel("new"); !ok {
		t.Error("收到 GUILD_CREATE 之後才設定的來源頻道不應被清除")
	}
	if _, _, ok := d.SourceChannel("old"); ok {
		t.Error("收到 GUILD_CREATE 之前設定、且已不存在的來源頻道應被清除")
	}
	if !slices.Equal(e.hooks.removed, []string{"old"}) {
		t.Errorf("只應對 old 呼叫 ChannelRemoved：%v", e.hooks.removed)
	}
}

// setAt 無法解析成 snowflake 時無法判斷先後，保守起見不清除。
func TestPruneMissingChannels_UnparsableSetAtKept(t *testing.T) {
	e := newChannelEnv(t)
	if err := e.st.Update(context.Background(), func(d *store.Data) error {
		d.AddChannel("g1", "weird", "not-a-snowflake")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	PruneMissingChannels(context.Background(), e.st, e.hooks, "g1", nil, time.Now())
	if _, _, ok := e.st.Snapshot().SourceChannel("weird"); !ok {
		t.Error("setAt 無法解析時不應清除")
	}
}
