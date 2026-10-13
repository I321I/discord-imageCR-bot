package recognize

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/color"
	"image/png"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestHelperProcess 不是真正的測試：rapidocr 測試用 Go 測試二進位自身當假 OCR worker（不需 Python）。
// 參數：-- <mode> <logPath>。啟動時在 logPath 記錄 "start <pid>"，讀到 stdin EOF 時記錄 "exit"。
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_OCR_HELPER") != "1" {
		return
	}
	args := os.Args
	i := slices.Index(args, "--")
	mode, logPath := args[i+1], args[i+2]
	prev, _ := os.ReadFile(logPath)
	firstStart := !strings.Contains(string(prev), "start")
	appendLine(logPath, fmt.Sprintf("start %d", os.Getpid()))

	if mode == "no-ready" {
		time.Sleep(time.Hour)
	}
	fmt.Println(`{"ready":true}`)
	in := bufio.NewReader(os.Stdin)
	for n := 1; ; n++ {
		line, err := in.ReadBytes('\n')
		if err != nil {
			if mode != "ignore-eof" {
				appendLine(logPath, "exit")
				os.Exit(0)
			}
			time.Sleep(time.Hour)
		}
		var req struct {
			ID  int64  `json:"id"`
			PNG []byte `json:"png"`
		}
		if err := json.Unmarshal(line, &req); err != nil {
			fmt.Printf("{\"id\":0,\"error\":%q}\n", err.Error())
			continue
		}
		switch {
		case mode == "empty":
			fmt.Printf("{\"id\":%d,\"items\":[]}\n", req.ID)
			continue
		case mode == "error-first" && n == 1:
			fmt.Printf("{\"id\":%d,\"error\":\"bad image\"}\n", req.ID)
			continue
		case mode == "hang-first-start" && firstStart:
			time.Sleep(time.Hour)
		case mode == "crash-first-start" && firstStart:
			os.Exit(3)
		case mode == "garbage":
			fmt.Println("not json")
			continue
		case mode == "wrong-id":
			req.ID += 100
		case mode == "stderr":
			fmt.Fprintln(os.Stderr, "worker 記錄一行")
		}
		img, err := png.Decode(bytes.NewReader(req.PNG))
		if err != nil {
			fmt.Printf("{\"id\":%d,\"error\":%q}\n", req.ID, err.Error())
			continue
		}
		b := img.Bounds()
		fmt.Printf("{\"id\":%d,\"items\":[{\"box\":[[0,0],[10,0],[10,5],[0,5]],\"text\":\"%dx%d\",\"score\":0.9}]}\n", req.ID, b.Dx(), b.Dy())
	}
}

func appendLine(path, line string) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	fmt.Fprintln(f, line)
}

// fakeRapidOCR 回傳以假 worker（mode）執行的 RapidOCR 與記錄檔路徑。
func fakeRapidOCR(t *testing.T, mode string) (*RapidOCR, string) {
	t.Helper()
	t.Setenv("GO_WANT_OCR_HELPER", "1")
	logPath := filepath.Join(t.TempDir(), "worker.log")
	r := &RapidOCR{
		Command:      []string{os.Args[0], "-test.run=^TestHelperProcess$", "--", mode, logPath},
		StartTimeout: 10 * time.Second,
		Timeout:      10 * time.Second,
	}
	t.Cleanup(func() { r.Close() })
	return r, logPath
}

// workerLog 回傳記錄檔中以 prefix 開頭的行。
func workerLog(t *testing.T, path, prefix string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(l, prefix) {
			out = append(out, l)
		}
	}
	return out
}

// waitFor 每 10ms 檢查 cond，5 秒內未成立就失敗。
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("等待逾時：%s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// processAlive 回傳 pid 是否仍存在（已被 Wait 回收的行程視為不存在）。
func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func startedPIDs(t *testing.T, logPath string) []int {
	t.Helper()
	var pids []int
	for _, l := range workerLog(t, logPath, "start ") {
		var pid int
		fmt.Sscanf(l, "start %d", &pid)
		pids = append(pids, pid)
	}
	return pids
}

var _ TextRecognizer = (*RapidOCR)(nil)

func TestNewRapidOCR_Defaults(t *testing.T) {
	r := NewRapidOCR()
	if want := []string{"python3", "/app/ocr/ocr_worker.py"}; !slices.Equal(r.Command, want) {
		t.Errorf("Command %q，預期 %q", r.Command, want)
	}
	if r.StartTimeout != 60*time.Second || r.Timeout != 90*time.Second || r.IdleTimeout != 5*time.Minute {
		t.Errorf("StartTimeout %v、Timeout %v、IdleTimeout %v，預期 60s、90s（Q61）、5m（Q55）", r.StartTimeout, r.Timeout, r.IdleTimeout)
	}
}

func TestRapidOCR_RoundTrip(t *testing.T) {
	r, logPath := fakeRapidOCR(t, "echo")
	if pids := startedPIDs(t, logPath); len(pids) != 0 {
		t.Fatalf("第一張圖之前不應啟動 worker（Q55），已啟動 %v", pids)
	}
	got, err := r.RecognizeText(context.Background(), solid(64, 12, color.White))
	if err != nil {
		t.Fatal(err)
	}
	if got != "64x12" {
		t.Errorf("worker 應收到 64x12 的 PNG 並回傳其文字，得到 %q", got)
	}
}

func TestRapidOCR_StartsOnceForManyCalls(t *testing.T) {
	r, logPath := fakeRapidOCR(t, "echo")
	for i := 1; i <= 3; i++ {
		if got, err := r.RecognizeText(context.Background(), solid(10*i, 5, color.White)); err != nil || got != fmt.Sprintf("%dx5", 10*i) {
			t.Fatalf("第 %d 次：得到 %q %v", i, got, err)
		}
	}
	if pids := startedPIDs(t, logPath); len(pids) != 1 {
		t.Errorf("worker 應只啟動一次，得到 %v", pids)
	}
}

func TestRapidOCR_EmptyItemsIsErrNoText(t *testing.T) {
	r, _ := fakeRapidOCR(t, "empty")
	if _, err := r.RecognizeText(context.Background(), solid(10, 10, color.White)); !errors.Is(err, ErrNoText) {
		t.Errorf("items 為空應回 ErrNoText，得到 %v", err)
	}
}

func TestRapidOCR_ErrorResponseKeepsWorker(t *testing.T) {
	r, logPath := fakeRapidOCR(t, "error-first")
	_, err := r.RecognizeText(context.Background(), solid(10, 10, color.White))
	if err == nil || errors.Is(err, ErrNoText) || !strings.Contains(err.Error(), "bad image") {
		t.Errorf("error 回應應回含訊息的錯誤，得到 %v", err)
	}
	if got, err := r.RecognizeText(context.Background(), solid(10, 10, color.White)); err != nil || got != "10x10" {
		t.Errorf("下一張應成功，得到 %q %v", got, err)
	}
	if pids := startedPIDs(t, logPath); len(pids) != 1 {
		t.Errorf("error 回應後不應重啟 worker，得到 %v", pids)
	}
}

func TestRapidOCR_TimeoutKillsAndRestarts(t *testing.T) {
	r, logPath := fakeRapidOCR(t, "hang-first-start")
	r.Timeout = 300 * time.Millisecond
	start := time.Now()
	_, err := r.RecognizeText(context.Background(), solid(10, 10, color.White))
	if err == nil || errors.Is(err, ErrNoText) {
		t.Errorf("逾時應回錯誤，得到 %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("逾時後應很快回傳，花了 %v", d)
	}
	pids := startedPIDs(t, logPath)
	if len(pids) != 1 || processAlive(pids[0]) {
		t.Errorf("逾時的 worker 應被殺掉並回收，pids %v", pids)
	}
	r.Timeout = 10 * time.Second
	if got, err := r.RecognizeText(context.Background(), solid(20, 10, color.White)); err != nil || got != "20x10" {
		t.Errorf("第二張應由新 worker 處理並讀到自己的回應，得到 %q %v", got, err)
	}
	if pids := startedPIDs(t, logPath); len(pids) != 2 {
		t.Errorf("應重新啟動 worker，得到 %v", pids)
	}
}

func TestRapidOCR_ContextCanceledKillsWorker(t *testing.T) {
	r, logPath := fakeRapidOCR(t, "hang-first-start")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := r.RecognizeText(ctx, solid(10, 10, color.White)); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("ctx 逾時應回 context.DeadlineExceeded，得到 %v", err)
	}
	if pids := startedPIDs(t, logPath); len(pids) != 1 || processAlive(pids[0]) {
		t.Errorf("ctx 結束時 worker 應被殺掉，pids %v", pids)
	}
	if got, err := r.RecognizeText(context.Background(), solid(10, 10, color.White)); err != nil || got != "10x10" {
		t.Errorf("下一張應成功，得到 %q %v", got, err)
	}
}

func TestRapidOCR_WorkerCrashRestarts(t *testing.T) {
	r, logPath := fakeRapidOCR(t, "crash-first-start")
	if _, err := r.RecognizeText(context.Background(), solid(10, 10, color.White)); err == nil || errors.Is(err, ErrNoText) {
		t.Errorf("worker 結束應回錯誤，得到 %v", err)
	}
	if got, err := r.RecognizeText(context.Background(), solid(10, 10, color.White)); err != nil || got != "10x10" {
		t.Errorf("下一張應重新啟動 worker 並成功，得到 %q %v", got, err)
	}
	if pids := startedPIDs(t, logPath); len(pids) != 2 {
		t.Errorf("應啟動兩次，得到 %v", pids)
	}
}

func TestRapidOCR_GarbageStdoutIsError(t *testing.T) {
	r, logPath := fakeRapidOCR(t, "garbage")
	for i := 0; i < 2; i++ {
		if _, err := r.RecognizeText(context.Background(), solid(10, 10, color.White)); err == nil || errors.Is(err, ErrNoText) {
			t.Errorf("第 %d 次：非 JSON 輸出應回錯誤，得到 %v", i+1, err)
		}
	}
	pids := startedPIDs(t, logPath)
	if len(pids) != 2 {
		t.Errorf("輸出非 JSON 後應殺掉 worker、下一張重新啟動，得到 %v", pids)
	}
	if processAlive(pids[0]) {
		t.Error("第一個 worker 應已結束")
	}
}

func TestRapidOCR_WrongIDIsError(t *testing.T) {
	r, logPath := fakeRapidOCR(t, "wrong-id")
	if _, err := r.RecognizeText(context.Background(), solid(10, 10, color.White)); err == nil || errors.Is(err, ErrNoText) {
		t.Errorf("id 不符應回錯誤，得到 %v", err)
	}
	if pids := startedPIDs(t, logPath); len(pids) != 1 || processAlive(pids[0]) {
		t.Errorf("id 不符時應殺掉 worker，pids %v", pids)
	}
}

func TestRapidOCR_StartTimeout(t *testing.T) {
	r, logPath := fakeRapidOCR(t, "no-ready")
	r.StartTimeout = 300 * time.Millisecond
	start := time.Now()
	if _, err := r.RecognizeText(context.Background(), solid(10, 10, color.White)); err == nil || errors.Is(err, ErrNoText) {
		t.Errorf("等不到 ready 應回錯誤，得到 %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("應在 StartTimeout 後很快回傳，花了 %v", d)
	}
	if pids := startedPIDs(t, logPath); len(pids) != 1 || processAlive(pids[0]) {
		t.Errorf("等不到 ready 的 worker 應被殺掉，pids %v", pids)
	}
}

func TestRapidOCR_CommandNotFound(t *testing.T) {
	r := &RapidOCR{Command: []string{"/nonexistent/ocr-worker"}}
	if _, err := r.RecognizeText(context.Background(), solid(10, 10, color.White)); err == nil || errors.Is(err, ErrNoText) {
		t.Errorf("指令不存在應回錯誤，得到 %v", err)
	}
}

func TestRapidOCR_TinyImageIsErrNoTextWithoutWorker(t *testing.T) {
	r, logPath := fakeRapidOCR(t, "echo")
	for _, size := range [][2]int{{1, 5}, {5, 1}, {0, 0}} {
		if _, err := r.RecognizeText(context.Background(), solid(size[0], size[1], color.White)); !errors.Is(err, ErrNoText) {
			t.Errorf("%v：寬或高 < 2px 應回 ErrNoText，得到 %v", size, err)
		}
	}
	if pids := startedPIDs(t, logPath); len(pids) != 0 {
		t.Errorf("不應送給 worker，得到 %v", pids)
	}
}

func TestRapidOCR_ConcurrentCalls(t *testing.T) {
	r, logPath := fakeRapidOCR(t, "echo")
	var wg sync.WaitGroup
	for i := 1; i <= 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			want := fmt.Sprintf("%dx3", i+1)
			if got, err := r.RecognizeText(context.Background(), solid(i+1, 3, color.White)); err != nil || got != want {
				t.Errorf("得到 %q %v，預期 %q", got, err, want)
			}
		}()
	}
	wg.Wait()
	if pids := startedPIDs(t, logPath); len(pids) != 1 {
		t.Errorf("同時呼叫也只應有一個 worker，得到 %v", pids)
	}
}

func TestRapidOCR_CloseStopsWorker(t *testing.T) {
	r, logPath := fakeRapidOCR(t, "echo")
	if _, err := r.RecognizeText(context.Background(), solid(10, 10, color.White)); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if len(workerLog(t, logPath, "exit")) != 1 {
		t.Error("Close 應關閉 stdin 讓 worker 自行結束")
	}
	if pids := startedPIDs(t, logPath); processAlive(pids[0]) {
		t.Error("Close 後 worker 應已結束並回收")
	}
	if _, err := r.RecognizeText(context.Background(), solid(10, 10, color.White)); err == nil || errors.Is(err, ErrNoText) {
		t.Errorf("Close 後應回錯誤，得到 %v", err)
	}
	if pids := startedPIDs(t, logPath); len(pids) != 1 {
		t.Errorf("Close 後不應再啟動 worker，得到 %v", pids)
	}
}

func TestRapidOCR_CloseWithoutWorker(t *testing.T) {
	r, _ := fakeRapidOCR(t, "echo")
	if err := r.Close(); err != nil {
		t.Errorf("沒有 worker 時 Close 應回 nil，得到 %v", err)
	}
}

// worker 不理會 stdin EOF 時，Close 在寬限時間後強制結束它。
func TestRapidOCR_CloseKillsStuckWorker(t *testing.T) {
	r, logPath := fakeRapidOCR(t, "ignore-eof")
	if _, err := r.RecognizeText(context.Background(), solid(10, 10, color.White)); err != nil {
		t.Fatal(err)
	}
	r.Close()
	if pids := startedPIDs(t, logPath); processAlive(pids[0]) {
		t.Error("Close 後 worker 應已被強制結束並回收")
	}
}

func TestRapidOCR_IdleTimeoutStopsWorker(t *testing.T) {
	r, logPath := fakeRapidOCR(t, "echo")
	r.IdleTimeout = 200 * time.Millisecond
	if _, err := r.RecognizeText(context.Background(), solid(10, 10, color.White)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "閒置後 worker 結束", func() bool { return len(workerLog(t, logPath, "exit")) == 1 })
	waitFor(t, "worker 被回收", func() bool { return !processAlive(startedPIDs(t, logPath)[0]) })
	if got, err := r.RecognizeText(context.Background(), solid(10, 10, color.White)); err != nil || got != "10x10" {
		t.Errorf("閒置結束後下一張應重新啟動並成功，得到 %q %v", got, err)
	}
	if pids := startedPIDs(t, logPath); len(pids) != 2 {
		t.Errorf("應啟動兩次，得到 %v", pids)
	}
}

func TestRapidOCR_CallsResetIdleTimer(t *testing.T) {
	r, logPath := fakeRapidOCR(t, "echo")
	r.IdleTimeout = 300 * time.Millisecond
	for i := 0; i < 5; i++ {
		if _, err := r.RecognizeText(context.Background(), solid(10, 10, color.White)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if pids := startedPIDs(t, logPath); len(pids) != 1 {
		t.Errorf("閒置未超過 IdleTimeout 時不應結束 worker，得到 %v", pids)
	}
}

// syncBuffer 是可同時寫入與讀取的 bytes.Buffer。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func TestRapidOCR_WorkerStderrGoesToLog(t *testing.T) {
	var buf syncBuffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	r, _ := fakeRapidOCR(t, "stderr")
	if _, err := r.RecognizeText(context.Background(), solid(10, 10, color.White)); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "stderr 寫入 log", func() bool { return strings.Contains(buf.String(), "worker 記錄一行") })
}
