package recognize

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// 依真實 trace.moe /search?anilistInfo 回應格式（含 idMal、isAdult、filename、episode、from/to、video、image 等欄位）解析。
func TestGate4_TraceMoe_RealisticResponse(t *testing.T) {
	body := `{"frameCount":5890247,"error":"","result":[` +
		`{"anilist":{"id":21034,"idMal":31933,"title":{"native":"ご注文はうさぎですか？？","romaji":"Gochuumon wa Usagi desu ka??","english":"Is the Order a Rabbit??"},"synonyms":["Gochiusa 2","請問您今天要來點兔子嗎？？"],"isAdult":false},` +
		`"filename":"[Ohys-Raws] Gochuumon wa Usagi Desu ka 2 - 01.mp4","episode":1,"from":289.58,"to":292.67,"similarity":0.9440424588727485,` +
		`"video":"https://media.trace.moe/video/21034/x.mp4","image":"https://media.trace.moe/image/21034/x.jpg"},` +
		`{"anilist":{"id":1,"title":{"native":"別作品"},"synonyms":[],"isAdult":false},"filename":"y.mp4","episode":"1|2","from":1,"to":2,"similarity":0.5,"video":"","image":""}` +
		`]}`
	tm := newTraceMoeServer(t, respondJSON(body))
	name, err := tm.RecognizeWork(context.Background(), []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	// 10/28 Task 5（使用者決定，取代 Q14）：有中文只放一個中文名稱。
	// 原預期為「請問您今天要來點兔子嗎？？、ご注文はうさぎですか？？、Is the Order a Rabbit??、Gochuumon wa Usagi desu ka??」。
	want := "請問您今天要來點兔子嗎？？"
	if name != want {
		t.Errorf("得到 %q\n預期 %q", name, want)
	}
}

// 若 trace.moe 沒有展開 anilist（回傳數字 id），應回一般錯誤而非 ErrNotFound 或 panic。
func TestGate4_TraceMoe_AnilistAsNumberIsError(t *testing.T) {
	tm := newTraceMoeServer(t, respondJSON(`{"error":"","result":[{"anilist":21034,"similarity":0.99}]}`))
	_, err := tm.RecognizeWork(context.Background(), []byte("x"))
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("anilist 為數字應回解析錯誤，得到 %v", err)
	}
}

// 超大的回應本文（例如錯誤頁）不應整個讀入：在 1MB 截斷後解析失敗回錯誤。
func TestGate4_TraceMoe_HugeBodyIsBounded(t *testing.T) {
	big := `{"error":"","result":[],"pad":"` + strings.Repeat("a", 2<<20) + `"}`
	tm := newTraceMoeServer(t, respondJSON(big))
	_, err := tm.RecognizeWork(context.Background(), []byte("x"))
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("超過 1MB 的回應應被截斷而解析失敗，得到 %v", err)
	}
}

// Serialize(w, n) 的 n < 1 視為 1：呼叫照常執行（不會卡到 ctx 結束），且同時最多 1 個。
func TestGate4_SerializeNonPositiveActsAsOne(t *testing.T) {
	for _, n := range []int{0, -1} {
		rec := &concurrencyRecorder{}
		w := Serialize(rec, n)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		var wg sync.WaitGroup
		for i := 0; i < 5; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if got, err := w.RecognizeWork(ctx, []byte("x")); err != nil || got != "x" {
					t.Errorf("n=%d：應照常執行，得到 %q %v", n, got, err)
				}
			}()
		}
		wg.Wait()
		cancel()
		if rec.calls.Load() != 5 || rec.maxActive.Load() != 1 {
			t.Errorf("n=%d：應執行 5 次且同時最多 1 個，得到 %d 次、最多 %d", n, rec.calls.Load(), rec.maxActive.Load())
		}
	}
}

// 排隊中的多個呼叫在名額釋放後都能依序完成，且被取消的呼叫不佔名額。
func TestGate4_Serialize_CanceledWaiterDoesNotLeakSlot(t *testing.T) {
	b := &blockingRecognizer{started: make(chan struct{}, 1), release: make(chan struct{})}
	w := Serialize(b, 1)
	done := make(chan error, 1)
	go func() {
		_, err := w.RecognizeWork(context.Background(), nil)
		done <- err
	}()
	select {
	case <-b.started:
	case <-time.After(5 * time.Second):
		t.Fatal("第一個呼叫沒有開始")
	}
	for i := 0; i < 5; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
		w.RecognizeWork(ctx, nil)
		cancel()
	}
	close(b.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("第一個呼叫沒有結束")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if got, err := w.RecognizeWork(ctx, nil); err != nil || got != "done" {
		t.Errorf("被取消的等待者不應佔用名額，得到 %q %v", got, err)
	}
}
