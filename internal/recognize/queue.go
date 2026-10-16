package recognize

import (
	"context"
	"image"
	"slices"
	"sync"
)

// Serialize 包裝 w，限制同時最多 n 個請求（trace.moe 有並行限制，n=1）。
// n < 1 視為 1。排隊等待時 ctx 被取消或逾時，回傳 ctx.Err() 且不執行。
// 帶 WithPriority 標記的請求先於一般請求放行（Q62），同優先級先到先放行。
func Serialize(w WorkRecognizer, n int) WorkRecognizer {
	return &serialized{w: w, sem: newPrioritySem(n)}
}

type serialized struct {
	w   WorkRecognizer
	sem *prioritySem
}

func (s *serialized) RecognizeWork(ctx context.Context, png []byte) (string, error) {
	if err := s.sem.acquire(ctx); err != nil {
		return "", err
	}
	defer s.sem.release()
	return s.w.RecognizeWork(ctx, png)
}

// SerializeText 包裝 t，同時只執行一個 OCR（並行數 1）；排隊規則同 Serialize（含優先順序）。
func SerializeText(t TextRecognizer) TextRecognizer {
	return &serializedText{t: t, sem: newPrioritySem(1)}
}

type serializedText struct {
	t   TextRecognizer
	sem *prioritySem
}

func (s *serializedText) RecognizeText(ctx context.Context, img image.Image) (string, error) {
	if err := s.sem.acquire(ctx); err != nil {
		return "", err
	}
	defer s.sem.release()
	return s.t.RecognizeText(ctx, img)
}

// recognizeWithFallback 佔用一個名額執行整個 RecognizeWithFallback（整張重試接續處理、不重新排隊）。
func (s *serializedText) recognizeWithFallback(ctx context.Context, subtitle, whole image.Image) (string, error) {
	if err := s.sem.acquire(ctx); err != nil {
		return "", err
	}
	defer s.sem.release()
	return RecognizeWithFallback(ctx, s.t, subtitle, whole)
}

type priorityKey struct{}

// WithPriority 標記 ctx 為優先請求（「重新辨識」使用，Q62）：排隊時先於一般請求放行。
func WithPriority(ctx context.Context) context.Context {
	return context.WithValue(ctx, priorityKey{}, true)
}

// IsPriority 回傳 ctx 是否帶有 WithPriority 標記。
func IsPriority(ctx context.Context) bool {
	p, _ := ctx.Value(priorityKey{}).(bool)
	return p
}

// prioritySem 是有兩個等候佇列（優先、一般）的計數號誌；名額釋放時直接交給下一位等待者。
type prioritySem struct {
	mu     sync.Mutex
	n      int                // 名額
	active int                // 使用中的名額
	queues [2][]chan struct{} // [0] 優先、[1] 一般；放行時關閉該 channel
}

// newPrioritySem 建立 n 個名額的號誌；n < 1 視為 1。
func newPrioritySem(n int) *prioritySem {
	return &prioritySem{n: max(n, 1)}
}

// acquire 取得一個名額；ctx 已結束或排隊中結束時回 ctx.Err()（不佔名額）。
func (s *prioritySem) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	if s.active < s.n && len(s.queues[0]) == 0 && len(s.queues[1]) == 0 {
		s.active++
		s.mu.Unlock()
		return nil
	}
	q := 1
	if IsPriority(ctx) {
		q = 0
	}
	ch := make(chan struct{})
	s.queues[q] = append(s.queues[q], ch)
	s.mu.Unlock()

	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		if i := slices.Index(s.queues[q], ch); i >= 0 {
			s.queues[q] = slices.Delete(s.queues[q], i, i+1)
			s.mu.Unlock()
			return ctx.Err()
		}
		s.mu.Unlock()
		// 取消的同時已被放行：把名額交給下一位。
		s.release()
		return ctx.Err()
	}
}

// release 釋放名額：有人排隊時直接交給優先佇列的第一位，其次一般佇列的第一位。
func (s *prioritySem) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for q := range s.queues {
		if len(s.queues[q]) > 0 {
			ch := s.queues[q][0]
			s.queues[q] = s.queues[q][1:]
			close(ch)
			return
		}
	}
	s.active--
}
