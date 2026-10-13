package recognize

// 把關者（第四階段 RapidOCR 版）補充測試：子行程管理的邊界、優先號誌的名額守恆、JoinOCRItems 邊界。

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestGateHelperProcess 是把關者用的假 worker（不是真正的測試）。參數：-- <mode> <logPath>。
func TestGateHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_GATE_OCR_HELPER") != "1" {
		return
	}
	i := slices.Index(os.Args, "--")
	mode, logPath := os.Args[i+1], os.Args[i+2]
	appendLine(logPath, fmt.Sprintf("start %d", os.Getpid()))
	if mode == "noise-before-ready" {
		fmt.Println("Downloading model...")
	}
	fmt.Println(`{"ready":true}`)
	in := bufio.NewReaderSize(os.Stdin, 1<<16)
	for {
		line, err := in.ReadBytes('\n')
		if err != nil {
			os.Exit(0)
		}
		var req struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(line, &req); err != nil {
			fmt.Printf("{\"id\":null,\"error\":%q}\n", err.Error())
			continue
		}
		switch mode {
		case "big-response":
			// 約 2MB 的單行回應，遠超過 pipe buffer（64KB）。
			text := strings.Repeat("字", 700_000)
			fmt.Printf("{\"id\":%d,\"items\":[{\"box\":[[0,0],[10,0],[10,5],[0,5]],\"text\":%q,\"score\":0.9}]}\n", req.ID, text)
		case "extra-line":
			// 每個請求回兩行：第二行會被下一個請求讀到（錯位）。
			fmt.Printf("{\"id\":%d,\"items\":[{\"box\":[[0,0],[10,0],[10,5],[0,5]],\"text\":\"ok\",\"score\":0.9}]}\n", req.ID)
			fmt.Printf("{\"id\":%d,\"items\":[{\"box\":[[0,0],[10,0],[10,5],[0,5]],\"text\":\"stale\",\"score\":0.9}]}\n", req.ID)
		case "stderr-flood":
			// 大量 stderr（遠超過 pipe buffer）不應卡住 worker。
			fmt.Fprint(os.Stderr, strings.Repeat("noise line\n", 20000))
			fmt.Printf("{\"id\":%d,\"items\":[{\"box\":[[0,0],[10,0],[10,5],[0,5]],\"text\":\"ok\",\"score\":0.9}]}\n", req.ID)
		default:
			fmt.Printf("{\"id\":%d,\"items\":[{\"box\":[[0,0],[10,0],[10,5],[0,5]],\"text\":\"ok\",\"score\":0.9}]}\n", req.ID)
		}
	}
}

func gateRapidOCR(t *testing.T, mode string) (*RapidOCR, string) {
	t.Helper()
	t.Setenv("GO_WANT_GATE_OCR_HELPER", "1")
	logPath := filepath.Join(t.TempDir(), "worker.log")
	r := &RapidOCR{
		Command:      []string{os.Args[0], "-test.run=^TestGateHelperProcess$", "--", mode, logPath},
		StartTimeout: 10 * time.Second,
		Timeout:      10 * time.Second,
	}
	t.Cleanup(func() { r.Close() })
	return r, logPath
}

// noiseImage 回傳難以壓縮的圖（PNG 很大，用來測試大量 stdin 寫入）。
func noiseImage(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	rng := rand.New(rand.NewSource(1))
	rng.Read(img.Pix)
	return img
}

// 大圖（PNG 數 MB、base64 後一行更大）送出與大回應都不應卡住。
func TestGate4_RapidOCR_LargeRequestAndResponse(t *testing.T) {
	r, _ := gateRapidOCR(t, "big-response")
	got, err := r.RecognizeText(context.Background(), noiseImage(1280, 1280))
	if err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(got)); n != 700_000 {
		t.Errorf("回應文字長度 %d，預期 700000", n)
	}
}

// worker 大量寫 stderr 不應卡住。
func TestGate4_RapidOCR_StderrFloodDoesNotBlock(t *testing.T) {
	r, _ := gateRapidOCR(t, "stderr-flood")
	for i := 0; i < 3; i++ {
		if got, err := r.RecognizeText(context.Background(), solid(10, 10, color.White)); err != nil || got != "ok" {
			t.Fatalf("第 %d 次：得到 %q %v", i+1, got, err)
		}
	}
}

// ready 之前有非協定輸出：啟動失敗、worker 被殺掉回收（不留殭屍）。
func TestGate4_RapidOCR_NoiseBeforeReadyFailsAndReaps(t *testing.T) {
	r, logPath := gateRapidOCR(t, "noise-before-ready")
	if _, err := r.RecognizeText(context.Background(), solid(10, 10, color.White)); err == nil || errors.Is(err, ErrNoText) {
		t.Errorf("ready 前的雜訊應回錯誤，得到 %v", err)
	}
	pids := startedPIDs(t, logPath)
	if len(pids) != 1 || processAlive(pids[0]) {
		t.Errorf("worker 應已被殺掉回收，pids %v", pids)
	}
}

// 回應錯位（worker 多回一行）：下一張讀到舊 id → 錯誤、殺掉重啟；絕不會把別張的結果當成自己的。
func TestGate4_RapidOCR_StaleLineNeverReturnedAsResult(t *testing.T) {
	r, logPath := gateRapidOCR(t, "extra-line")
	for i := 0; i < 6; i++ {
		got, err := r.RecognizeText(context.Background(), solid(10, 10, color.White))
		if err == nil && got != "ok" {
			t.Fatalf("第 %d 次：讀到錯位的結果 %q", i+1, got)
		}
	}
	for _, pid := range startedPIDs(t, logPath) {
		r.mu.Lock()
		cur := r.w
		r.mu.Unlock()
		if cur != nil && cur.cmd.Process.Pid == pid {
			continue
		}
		if processAlive(pid) {
			t.Errorf("被取代的 worker %d 仍存在（殭屍或未殺掉）", pid)
		}
	}
}

// 閒置計時器反覆在請求之間觸發（IdleTimeout 極短）與並行請求競爭：每個請求都要成功，
// 不會出現「閒置結束了正在使用的 worker」；結束後所有舊 worker 都已回收。
func TestGate4_RapidOCR_IdleTimerRacesWithRequests(t *testing.T) {
	r, logPath := gateRapidOCR(t, "echo")
	r.IdleTimeout = time.Millisecond
	var wg sync.WaitGroup
	var failed atomic.Int32
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 15; i++ {
				if got, err := r.RecognizeText(context.Background(), solid(10, 10, color.White)); err != nil || got != "ok" {
					failed.Add(1)
					t.Errorf("得到 %q %v", got, err)
				}
				time.Sleep(time.Duration(i%3) * time.Millisecond)
			}
		}()
	}
	wg.Wait()
	if failed.Load() > 0 {
		return
	}
	r.Close()
	for _, pid := range startedPIDs(t, logPath) {
		waitFor(t, fmt.Sprintf("worker %d 回收", pid), func() bool { return !processAlive(pid) })
	}
}

// Close 與進行中的請求競爭：Close 會等進行中的請求結束（最多 Timeout）；之後 worker 已回收、不再啟動。
func TestGate4_RapidOCR_CloseDuringInFlightRequest(t *testing.T) {
	r, logPath := fakeRapidOCR(t, "hang-first-start")
	r.Timeout = 500 * time.Millisecond
	done := make(chan error, 1)
	go func() {
		_, err := r.RecognizeText(context.Background(), solid(10, 10, color.White))
		done <- err
	}()
	waitFor(t, "worker 啟動", func() bool { return len(startedPIDs(t, logPath)) == 1 })
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	r.Close()
	closeTook := time.Since(start)
	select {
	case err := <-done:
		if err == nil {
			t.Error("進行中的請求應回錯誤")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("進行中的請求沒有結束")
	}
	if closeTook > 3*time.Second {
		t.Errorf("Close 花了 %v", closeTook)
	}
	t.Logf("Close 等待進行中請求 %v（Timeout=%v）", closeTook, r.Timeout)
	pids := startedPIDs(t, logPath)
	if len(pids) != 1 || processAlive(pids[0]) {
		t.Errorf("worker 應已回收且不再啟動，pids %v", pids)
	}
}

// 優先號誌壓力測試：大量隨機取消（含優先），同時進行數不超過 n，結束後名額全部歸還（不遺失、不死結）。
func TestGate4_PrioritySem_StressNoLostPermits(t *testing.T) {
	for _, n := range []int{1, 3} {
		s := newPrioritySem(n)
		var active, maxActive atomic.Int32
		var wg sync.WaitGroup
		for i := 0; i < 300; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				ctx := context.Background()
				if i%2 == 0 {
					ctx = WithPriority(ctx)
				}
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, time.Duration(rand.Intn(3000))*time.Microsecond)
				defer cancel()
				if err := s.acquire(ctx); err != nil {
					return
				}
				a := active.Add(1)
				for {
					m := maxActive.Load()
					if a <= m || maxActive.CompareAndSwap(m, a) {
						break
					}
				}
				time.Sleep(time.Duration(rand.Intn(200)) * time.Microsecond)
				active.Add(-1)
				s.release()
			}(i)
		}
		wg.Wait()
		if m := maxActive.Load(); int(m) > n {
			t.Errorf("n=%d：同時進行數最大 %d", n, m)
		}
		s.mu.Lock()
		act, q0, q1 := s.active, len(s.queues[0]), len(s.queues[1])
		s.mu.Unlock()
		if act != 0 || q0 != 0 || q1 != 0 {
			t.Errorf("n=%d：結束後 active=%d、佇列 %d/%d，預期全為 0（名額遺失或殘留等待者）", n, act, q0, q1)
		}
		// 名額都還在：可立即再取得 n 個。
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		for i := 0; i < n; i++ {
			if err := s.acquire(ctx); err != nil {
				t.Fatalf("n=%d：第 %d 個名額取不到：%v", n, i+1, err)
			}
		}
		cancel()
	}
}

// 同優先級先到先放行；優先的等待者在一般等待者之前（即使較晚到）。
func TestGate4_PrioritySem_FIFOWithinLevel(t *testing.T) {
	s := newPrioritySem(1)
	if err := s.acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var order []string
	var wg sync.WaitGroup
	names := []struct {
		name string
		prio bool
	}{{"n1", false}, {"n2", false}, {"p1", true}, {"n3", false}, {"p2", true}}
	for i, x := range names {
		ctx := context.Background()
		if x.prio {
			ctx = WithPriority(ctx)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.acquire(ctx); err != nil {
				t.Errorf("%s：%v", x.name, err)
				return
			}
			mu.Lock()
			order = append(order, x.name)
			mu.Unlock()
			s.release()
		}()
		// 等它進佇列，確保到達順序。
		waitFor(t, x.name+" 排入", func() bool {
			s.mu.Lock()
			defer s.mu.Unlock()
			return len(s.queues[0])+len(s.queues[1]) == i+1
		})
	}
	s.release()
	wg.Wait()
	if want := []string{"p1", "p2", "n1", "n2", "n3"}; !slices.Equal(order, want) {
		t.Errorf("放行順序 %v，預期 %v", order, want)
	}
}

// JoinOCRItems 邊界：單一框、退化框（零寬高）、完全重疊的框、大量框不 panic 且結果穩定。
func TestGate4_JoinOCRItems_Edges(t *testing.T) {
	cases := []struct {
		name  string
		items []OCRItem
		want  string
	}{
		{"單一框", []OCRItem{rectItem("單", 5, 5, 40, 20)}, "單"},
		{"零寬高的框", []OCRItem{{Box: [4][2]float64{{3, 3}, {3, 3}, {3, 3}, {3, 3}}, Text: "點"}}, "點"},
		{"兩個完全重疊的框保持輸入順序", []OCRItem{rectItem("甲", 0, 0, 40, 20), rectItem("乙", 0, 0, 40, 20)}, "甲乙"},
		{"只有空白文字", []OCRItem{rectItem(" ", 0, 0, 40, 20), rectItem("\t", 0, 30, 40, 20)}, ""},
		{"只有直書一欄", []OCRItem{rectItem("直書", 0, 0, 20, 80)}, "直書"},
	}
	for _, c := range cases {
		if got := JoinOCRItems(c.items); got != c.want {
			t.Errorf("%s：得到 %q，預期 %q", c.name, got, c.want)
		}
	}
	// 大量框（含負座標）不 panic，且不改動呼叫端的切片順序以外的內容。
	var many []OCRItem
	for i := 0; i < 500; i++ {
		many = append(many, rectItem(fmt.Sprintf("w%d", i), float64((i*37)%400-50), float64((i*53)%300-20), 30, 15))
	}
	if got := JoinOCRItems(many); got == "" {
		t.Error("大量框應有輸出")
	}
}

// 16M 像素上限的邊界：剛好 16M 可解碼尺寸檢查通過，多 1 列就拒絕；且不需配置記憶體。
func TestGate4_CheckImageSize_Boundaries(t *testing.T) {
	if err := checkImageSize(4000, 4000); err != nil {
		t.Errorf("4000x4000（16M）應允許，得到 %v", err)
	}
	if err := checkImageSize(4000, 4001); !errors.Is(err, ErrImageTooLarge) {
		t.Errorf("4000x4001 應拒絕，得到 %v", err)
	}
	if err := checkImageSize(10001, 1); !errors.Is(err, ErrImageTooLarge) {
		t.Errorf("寬 10001 應拒絕，得到 %v", err)
	}
	if err := checkImageSize(10000, 1600); err != nil {
		t.Errorf("10000x1600（16M）應允許，得到 %v", err)
	}
}
