package access

import (
	"context"
	"slices"
	"testing"
	"time"

	"discordbot/internal/discordtest"
)

// 階段 10 收尾（決定 A）：AvailableComplete 回報 Available 的結果是否完整。

func availableComplete(t *testing.T, r *Resolver) ([]string, bool) {
	t.Helper()
	refs, complete, err := r.AvailableComplete(context.Background(), "u")
	if err != nil {
		t.Fatal(err)
	}
	return ids(refs), complete
}

// 所有伺服器都查到（成員或 404 非成員）→ 完整。
func TestAvailableComplete_AllResolved(t *testing.T) {
	e := newEnv(t)
	e.members.set("g1")
	got, complete := availableComplete(t, e.r)
	if !complete || !slices.Equal(got, []string{"c1"}) {
		t.Errorf("得到 %v complete=%v，預期 [c1] true", got, complete)
	}
	// 第二次全部來自快取，仍完整。
	if _, complete := availableComplete(t, e.r); !complete {
		t.Error("快取命中時應完整")
	}
}

// 成員查詢失敗（404 以外）→ 不完整。
func TestAvailableComplete_LookupFailed(t *testing.T) {
	e := newEnv(t)
	e.members.set("g1")
	e.members.set("g2")
	e.members.errs["g2"] = discordtest.ErrInjected
	got, complete := availableComplete(t, e.r)
	if complete || !slices.Equal(got, []string{"c1"}) {
		t.Errorf("得到 %v complete=%v，預期 [c1] false", got, complete)
	}
}

// 超過截止時間仍未查到 → 不完整。
func TestAvailableComplete_Deadline(t *testing.T) {
	e := newEnv(t)
	e.r.Deadline = 50 * time.Millisecond
	e.members.set("g1")
	e.members.set("g2")
	e.members.delay["g2"] = 300 * time.Millisecond
	got, complete := availableComplete(t, e.r)
	if complete || !slices.Equal(got, []string{"c1"}) {
		t.Errorf("得到 %v complete=%v，預期 [c1] false", got, complete)
	}
	time.Sleep(400 * time.Millisecond) // 等背景查詢結束，避免影響其他測試
}

// 來源伺服器暫時不在 state → 不完整；只是某個頻道不在 state（伺服器在）不影響完整性。
func TestAvailableComplete_GuildMissingFromState(t *testing.T) {
	e := newEnv(t)
	e.members.set("g1")
	addSource(t, e.store, "g1", "c1x", "130") // state 中沒有 c1x（伺服器在）
	if _, complete := availableComplete(t, e.r); !complete {
		t.Error("只缺頻道時應完整")
	}
	addSource(t, e.store, "g9", "c9", "131") // state 中沒有 g9
	got, complete := availableComplete(t, e.r)
	if complete || !slices.Equal(got, []string{"c1"}) {
		t.Errorf("得到 %v complete=%v，預期 [c1] false", got, complete)
	}
}

// Available 的結果與 AvailableComplete 相同（/search 用法不變）。
func TestAvailableComplete_SameRefsAsAvailable(t *testing.T) {
	e := newEnv(t)
	e.members.set("g1")
	e.members.set("g3")
	got, _ := availableComplete(t, e.r)
	if want := available(t, e.r); !slices.Equal(got, want) {
		t.Errorf("AvailableComplete %v，Available %v", got, want)
	}
}
