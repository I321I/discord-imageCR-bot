package recognize

import (
	"context"
	"errors"
	"fmt"
	"image"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// concurrencyRecorder 記錄同時進行中的最大呼叫數。
type concurrencyRecorder struct {
	active, maxActive atomic.Int32
	calls             atomic.Int32
}

func (c *concurrencyRecorder) RecognizeWork(ctx context.Context, png []byte) (string, error) {
	c.calls.Add(1)
	n := c.active.Add(1)
	for {
		m := c.maxActive.Load()
		if n <= m || c.maxActive.CompareAndSwap(m, n) {
			break
		}
	}
	time.Sleep(2 * time.Millisecond)
	c.active.Add(-1)
	return string(png), nil
}

func TestSerialize_MaxConcurrency(t *testing.T) {
	rec := &concurrencyRecorder{}
	w := Serialize(rec, 1)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got, err := w.RecognizeWork(context.Background(), []byte("x")); err != nil || got != "x" {
				t.Errorf("應轉傳結果，得到 %q %v", got, err)
			}
		}()
	}
	wg.Wait()
	if rec.calls.Load() != 10 {
		t.Errorf("應呼叫 10 次，得到 %d", rec.calls.Load())
	}
	if m := rec.maxActive.Load(); m != 1 {
		t.Errorf("同時進行數最大應為 1，得到 %d", m)
	}
}

// blockingRecognizer 在 release 關閉前不回傳。
type blockingRecognizer struct {
	started chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (b *blockingRecognizer) RecognizeWork(ctx context.Context, png []byte) (string, error) {
	b.calls.Add(1)
	select {
	case b.started <- struct{}{}:
	default:
	}
	select {
	case <-b.release:
		return "done", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestSerialize_ContextCancelWhileWaiting(t *testing.T) {
	b := &blockingRecognizer{started: make(chan struct{}, 1), release: make(chan struct{})}
	w := Serialize(b, 1)
	first := make(chan error, 1)
	go func() {
		_, err := w.RecognizeWork(context.Background(), nil)
		first <- err
	}()
	select {
	case <-b.started: // 第一個呼叫佔住唯一的名額
	case <-time.After(5 * time.Second):
		t.Fatal("第一個呼叫沒有開始")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := w.RecognizeWork(ctx, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("排隊中 ctx 逾時應回 context.DeadlineExceeded，得到 %v", err)
	}
	if b.calls.Load() != 1 {
		t.Errorf("被取消的呼叫不應執行，呼叫數 %d", b.calls.Load())
	}

	close(b.release)
	select {
	case err := <-first:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("第一個呼叫沒有結束")
	}
	// 名額已釋放，之後的呼叫可以執行。
	b.release = make(chan struct{})
	close(b.release)
	if got, err := w.RecognizeWork(context.Background(), nil); err != nil || got != "done" {
		t.Errorf("名額釋放後應可執行，得到 %q %v", got, err)
	}
}

func TestSerialize_AlreadyCanceledDoesNotRun(t *testing.T) {
	rec := &concurrencyRecorder{}
	w := Serialize(rec, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := w.RecognizeWork(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("ctx 已取消應回 context.Canceled，得到 %v", err)
	}
	if rec.calls.Load() != 0 {
		t.Error("ctx 已取消不應執行")
	}
}

// textConcurrencyRecorder 是 TextRecognizer 版的 concurrencyRecorder。
type textConcurrencyRecorder struct{ concurrencyRecorder }

func (c *textConcurrencyRecorder) RecognizeText(ctx context.Context, img image.Image) (string, error) {
	return c.RecognizeWork(ctx, []byte("x"))
}

func TestSerializeText_MaxConcurrency(t *testing.T) {
	rec := &textConcurrencyRecorder{}
	tr := SerializeText(rec)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got, err := tr.RecognizeText(context.Background(), nil); err != nil || got != "x" {
				t.Errorf("應轉傳結果，得到 %q %v", got, err)
			}
		}()
	}
	wg.Wait()
	if rec.calls.Load() != 10 || rec.maxActive.Load() != 1 {
		t.Errorf("應呼叫 10 次且同時最多 1 個，得到 %d 次、最多 %d", rec.calls.Load(), rec.maxActive.Load())
	}
}

func TestSerializeText_AlreadyCanceledDoesNotRun(t *testing.T) {
	rec := &textConcurrencyRecorder{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := SerializeText(rec).RecognizeText(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("ctx 已取消應回 context.Canceled，得到 %v", err)
	}
	if rec.calls.Load() != 0 {
		t.Error("ctx 已取消不應執行")
	}
}

// orderRecorder 記錄執行順序；gate 關閉前第一個呼叫不回傳。
type orderRecorder struct {
	mu      sync.Mutex
	order   []string
	started chan struct{}
	gate    chan struct{}
}

func newOrderRecorder() *orderRecorder {
	return &orderRecorder{started: make(chan struct{}, 1), gate: make(chan struct{})}
}

func (o *orderRecorder) RecognizeWork(ctx context.Context, png []byte) (string, error) {
	o.mu.Lock()
	o.order = append(o.order, string(png))
	o.mu.Unlock()
	select {
	case o.started <- struct{}{}:
	default:
	}
	<-o.gate
	return string(png), nil
}

func (o *orderRecorder) RecognizeText(ctx context.Context, img image.Image) (string, error) {
	return o.RecognizeWork(ctx, []byte(fmt.Sprint(img.Bounds().Dx())))
}

// waitQueued 等到 sem 中排隊的人數為 n。
func waitQueued(t *testing.T, sem *prioritySem, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		sem.mu.Lock()
		got := len(sem.queues[0]) + len(sem.queues[1])
		sem.mu.Unlock()
		if got == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("排隊人數應為 %d，得到 %d", n, got)
		}
		time.Sleep(time.Millisecond)
	}
}

// Q62：「重新辨識」優先。佔住名額後排入一般 a、b、優先 p1、一般 c、優先 p2，釋放後依 p1、p2、a、b、c 執行。
func TestSerialize_PriorityFirst(t *testing.T) {
	o := newOrderRecorder()
	w := Serialize(o, 1)
	sem := w.(*serialized).sem
	var wg sync.WaitGroup
	run := func(ctx context.Context, name string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got, err := w.RecognizeWork(ctx, []byte(name)); err != nil || got != name {
				t.Errorf("%s：得到 %q %v", name, got, err)
			}
		}()
	}
	run(context.Background(), "first")
	<-o.started
	for i, c := range []struct {
		name     string
		priority bool
	}{{"a", false}, {"b", false}, {"p1", true}, {"c", false}, {"p2", true}} {
		ctx := context.Background()
		if c.priority {
			ctx = WithPriority(ctx)
		}
		run(ctx, c.name)
		waitQueued(t, sem, i+1)
	}
	close(o.gate)
	wg.Wait()
	want := []string{"first", "p1", "p2", "a", "b", "c"}
	if !slices.Equal(o.order, want) {
		t.Errorf("執行順序 %v，預期 %v", o.order, want)
	}
}

func TestSerializeText_PriorityFirst(t *testing.T) {
	o := newOrderRecorder()
	tr := SerializeText(o)
	sem := tr.(*serializedText).sem
	var wg sync.WaitGroup
	run := func(ctx context.Context, width int) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := tr.RecognizeText(ctx, image.NewRGBA(image.Rect(0, 0, width, 1))); err != nil {
				t.Error(err)
			}
		}()
	}
	run(context.Background(), 1)
	<-o.started
	run(context.Background(), 2)
	waitQueued(t, sem, 1)
	run(context.Background(), 3)
	waitQueued(t, sem, 2)
	run(WithPriority(context.Background()), 4)
	waitQueued(t, sem, 3)
	close(o.gate)
	wg.Wait()
	if want := []string{"1", "4", "2", "3"}; !slices.Equal(o.order, want) {
		t.Errorf("執行順序 %v，預期 %v", o.order, want)
	}
}

// 排隊中被取消的優先等待者不佔名額，也不影響之後的一般等待者。
func TestSerialize_CanceledPriorityWaiterDoesNotBlock(t *testing.T) {
	o := newOrderRecorder()
	w := Serialize(o, 1)
	sem := w.(*serialized).sem
	done := make(chan struct{})
	go func() {
		w.RecognizeWork(context.Background(), []byte("first"))
		close(done)
	}()
	<-o.started
	ctx, cancel := context.WithCancel(WithPriority(context.Background()))
	canceled := make(chan error, 1)
	go func() {
		_, err := w.RecognizeWork(ctx, []byte("p"))
		canceled <- err
	}()
	waitQueued(t, sem, 1)
	cancel()
	if err := <-canceled; !errors.Is(err, context.Canceled) {
		t.Errorf("應回 context.Canceled，得到 %v", err)
	}
	waitQueued(t, sem, 0)
	close(o.gate)
	<-done
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	if got, err := w.RecognizeWork(ctx2, []byte("next")); err != nil || got != "next" {
		t.Errorf("之後的呼叫應可執行，得到 %q %v", got, err)
	}
	if want := []string{"first", "next"}; !slices.Equal(o.order, want) {
		t.Errorf("執行順序 %v，預期 %v", o.order, want)
	}
}
