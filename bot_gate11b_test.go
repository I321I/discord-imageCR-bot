package main

import (
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// 把關者第二輪（階段 11）：READY 的伺服器清單只有 nil 項目時，視同清單為空，不清除來源頻道。
func TestGK11b_OnReadyNilGuildsSkipsPrune(t *testing.T) {
	f, st := newLoadedStore(t, "g1", "c1")
	sent := len(f.Sent)
	r := &recordScanner{done: make(chan struct{}, 1)}
	onReady(r, st)(nil, &discordgo.Ready{Guilds: []*discordgo.Guild{nil}})
	select {
	case <-r.done:
	case <-time.After(2 * time.Second):
		t.Fatal("沒有掃描")
	}
	if _, _, ok := st.Snapshot().SourceChannel("c1"); !ok {
		t.Error("清單只有 nil 時不應清除來源頻道")
	}
	if len(f.Sent) != sent {
		t.Error("不應寫入")
	}
}

// 把關者第二輪：預設目標（最後一個階段）必須是單純的 `FROM runtime`，其後不能有任何指令
// （避免把建置 context、原始碼或工具鏈帶進正式映像，也不改 runtime 的 USER/CMD/ENV）。
func TestGK11b_FinalStageIsBareRuntime(t *testing.T) {
	lines := strings.Split(readFile(t, "Dockerfile"), "\n")
	last := -1
	for i, l := range lines {
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(l)), "FROM ") {
			last = i
		}
	}
	if last < 0 {
		t.Fatal("沒有 FROM")
	}
	if got := strings.TrimSpace(lines[last]); got != "FROM runtime" {
		t.Errorf("最後一個階段應為 `FROM runtime`，得到 %q", got)
	}
	for _, l := range lines[last+1:] {
		s := strings.TrimSpace(l)
		if s != "" && !strings.HasPrefix(s, "#") {
			t.Errorf("最後一個階段後不應有指令：%q", s)
		}
	}
}

// 把關者第二輪：沒有 Ready.Shard 以外的分片設定（本 bot 不分片，READY 才會列出所有所在伺服器）。
func TestGK11b_SessionNotSharded(t *testing.T) {
	src := readFile(t, "bot.go")
	for _, kw := range []string{"ShardCount", "ShardID", "Identify.Shard"} {
		if strings.Contains(src, kw) {
			t.Errorf("bot.go 設定了分片（%s）：READY 只含本分片的伺服器，PruneMissingGuilds 會誤刪其他分片的來源頻道", kw)
		}
	}
}
