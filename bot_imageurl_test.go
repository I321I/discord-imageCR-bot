package main

import (
	"context"
	"slices"
	"testing"
	"time"

	"discordbot/internal/discordtest"
	"discordbot/internal/snowflake"
	"discordbot/internal/store"

	"github.com/bwmarrin/discordgo"
)

func waitScan(t *testing.T, r *recordScanner) {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(2 * time.Second):
		t.Fatal("沒有掃描")
	}
}

// U1 (b)：第一次啟動（沒有 url_since）寫入並存回，掃描用寫入後的設定。
// 把關後決定 (b)：值為寫入儲存頻道時 Discord 回傳的訊息 ID（伺服器時間），不用本機時鐘。
func TestOnReady_WritesURLSinceBeforeScan(t *testing.T) {
	f, st := newLoadedStore(t, "g", "c1")
	sent := len(f.Sent)
	r := &recordScanner{done: make(chan struct{}, 1)}
	onReady(r, st, true)(nil, &discordgo.Ready{Guilds: []*discordgo.Guild{{ID: "g"}}})
	waitScan(t, r)
	r.mu.Lock()
	defer r.mu.Unlock()
	got := r.data[0].URLSince
	if !snowflake.Valid(got) {
		t.Fatalf("掃描用的設定應有 url_since：%q", got)
	}
	ids := map[string]bool{}
	for _, m := range f.Messages("storage") {
		ids[m.ID] = true
	}
	if !ids[got] {
		t.Errorf("url_since 應為儲存頻道中 Discord 回傳的訊息 ID：%s", got)
	}
	if len(f.Sent) != sent+2 {
		t.Errorf("應寫入儲存頻道兩次（取得 ID、存回），得到 %d 次", len(f.Sent)-sent)
	}
	st2 := store.New(f, "storage")
	if err := st2.Load(context.Background()); err != nil || st2.Snapshot().URLSince != got {
		t.Errorf("應存回儲存頻道：%q %v", st2.Snapshot().URLSince, err)
	}
}

// 已有 url_since（之後的 READY、重新 IDENTIFY）：不改、不寫入。
func TestOnReady_KeepsExistingURLSince(t *testing.T) {
	f, st := newLoadedStore(t, "g", "c1")
	if err := st.Update(context.Background(), func(d *store.Data) error { d.URLSince = "12345"; return nil }); err != nil {
		t.Fatal(err)
	}
	sent := len(f.Sent)
	r := &recordScanner{done: make(chan struct{}, 1)}
	onReady(r, st, true)(nil, &discordgo.Ready{Guilds: []*discordgo.Guild{{ID: "g"}}})
	waitScan(t, r)
	if r.data[0].URLSince != "12345" || len(f.Sent) != sent {
		t.Errorf("不應改寫：%q，寫入 %d 次", r.data[0].URLSince, len(f.Sent)-sent)
	}
}

// 設定讀取失敗、以空資料啟動時不寫入 url_since（避免自動以空資料蓋掉儲存頻道中的設定）。
func TestOnReady_StoreNotLoadedDoesNotWriteURLSince(t *testing.T) {
	f, st := newLoadedStore(t, "g", "c1")
	sent := len(f.Sent)
	r := &recordScanner{done: make(chan struct{}, 1)}
	onReady(r, st, false)(nil, &discordgo.Ready{Guilds: []*discordgo.Guild{{ID: "g"}}})
	waitScan(t, r)
	if r.data[0].URLSince != "" || len(f.Sent) != sent {
		t.Errorf("不應寫入 url_since：%q，寫入 %d 次", r.data[0].URLSince, len(f.Sent)-sent)
	}
}

// 寫入失敗：記 log，仍照常掃描（只含網址的訊息當舊圖片訊息）。
func TestOnReady_URLSinceWriteFailureStillScans(t *testing.T) {
	f, st := newLoadedStore(t, "g", "c1")
	f.FailOn["ChannelMessageSendComplex"] = discordtest.ErrInjected
	r := &recordScanner{done: make(chan struct{}, 1)}
	onReady(r, st, true)(nil, &discordgo.Ready{Guilds: []*discordgo.Guild{{ID: "g"}}})
	waitScan(t, r)
	if r.data[0].URLSince != "" {
		t.Errorf("寫入失敗時記憶體不應有 url_since：%q", r.data[0].URLSince)
	}
}

func TestLoadStore_ReportsLoaded(t *testing.T) {
	f := discordtest.NewFake()
	if !loadStore(context.Background(), store.New(f, "storage")) {
		t.Error("儲存頻道為空（全新安裝）應回 true")
	}
	f.FailOn["ChannelMessages"] = discordtest.ErrInjected
	if loadStore(context.Background(), store.New(f, "storage")) {
		t.Error("讀取失敗應回 false")
	}
	f2 := discordtest.NewFake()
	f2.AddMessage(&discordgo.Message{ChannelID: "storage", Content: "imgbot-store v1 1/2\n{"})
	if loadStore(context.Background(), store.New(f2, "storage")) {
		t.Error("沒有完整版本應回 false")
	}
}

type recordUpdateHandler struct {
	got chan *discordgo.Message
	ctx chan context.Context
}

func (h *recordUpdateHandler) HandleMessageUpdate(ctx context.Context, m *discordgo.Message) {
	h.ctx <- ctx
	h.got <- m
}

// 訊息更新事件同時交給索引與 pipeline（預覽補上、編輯加上圖片網址）。
func TestOnMessageUpdate_CallsIndexAndPipeline(t *testing.T) {
	r := &recordIndexer{}
	h := &recordUpdateHandler{got: make(chan *discordgo.Message, 1), ctx: make(chan context.Context, 1)}
	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "v")
	m := &discordgo.Message{ID: "4", ChannelID: "c"}
	onMessageUpdate(ctx, r, h)(nil, &discordgo.MessageUpdate{Message: m})
	select {
	case got := <-h.got:
		if got != m || (<-h.ctx).Value(key{}) != "v" {
			t.Error("pipeline 應收到同一則訊息與 bot 的 ctx")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("沒有交給 pipeline")
	}
	if !slices.Equal(r.updated, []string{"4"}) {
		t.Errorf("索引應收到：%v", r.updated)
	}
	onMessageUpdate(ctx, r, h)(nil, &discordgo.MessageUpdate{})
	select {
	case <-h.got:
		t.Error("沒有訊息時不應交給 pipeline")
	case <-time.After(50 * time.Millisecond):
	}
	if len(r.updated) != 1 {
		t.Errorf("沒有訊息時不應交給索引：%v", r.updated)
	}
}
