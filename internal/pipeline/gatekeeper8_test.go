package pipeline

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// 記錄現況（非斷言失敗）：關機時（ctx 已取消）重新處理 → 回錯誤，但不重新加 ❌，原訊息此時既無 ❌ 也無 ⏳。
// 指令端會回覆「重新處理失敗，已在原訊息加上 ❌」，與實際不符；下次啟動補處理會把它當成未處理訊息重新處理。
func TestGK8_Reprocess_CanceledCtx_NoFailedMark(t *testing.T) {
	e := newEnv(t)
	m := e.imageMessage(t, "a.png")
	m.Reactions = failedReaction()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := <-e.p.Reprocess(ctx, testGuild, m); err == nil {
		t.Fatal("ctx 已取消應回錯誤")
	}
	got := e.fake.ReactionsOn(testSource, m.ID)
	if slices.Contains(got, "❌") || slices.Contains(got, "⏳") {
		t.Errorf("現況應為沒有 ❌ 也沒有 ⏳，得到 %q（若行為已改，請更新此測試）", got)
	}
}

// 同一則訊息已在佇列中時再 Reprocess → 略過並立即收到 ErrAlreadyQueued（階段 8 的使用者決定 S4：指令端回「已在處理佇列中」），❌ 已被移除。
func TestGK8_Reprocess_AlreadyPending_ReturnsNilImmediately(t *testing.T) {
	e := newEnv(t)
	m := e.imageMessage(t, "a.png")
	m.Reactions = failedReaction()
	e.p.qmu.Lock()
	if e.p.pending == nil {
		e.p.pending = map[string]bool{}
	}
	e.p.pending[m.ID] = true
	e.p.qmu.Unlock()
	if err := <-e.p.Reprocess(context.Background(), testGuild, m); !errors.Is(err, ErrAlreadyQueued) {
		t.Fatalf("略過時應回 ErrAlreadyQueued：%v", err)
	}
	if got := e.fake.ReactionsOn(testSource, m.ID); slices.Contains(got, "❌") {
		t.Errorf("❌ 應已移除，得到 %q", got)
	}
	if len(e.fake.Sent) != 0 {
		t.Error("略過時不應發出")
	}
}
