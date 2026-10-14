package recognize

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"log"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	workerWaitDelay  = 2 * time.Second // worker 結束後最多再等 stdout/stderr 關閉的時間（避免孫行程佔住管線）
	workerStopGrace  = 5 * time.Second // 關閉 stdin 後等 worker 自行結束的時間，逾時強制結束
	workerMaxLogLine = 200             // 錯誤訊息中引用 worker 輸出的最大長度

	workerMaxStderrLine = 4096     // worker stderr 單行寫到 log 的最大 bytes，超過截斷
	workerLogTruncated  = "…（已截斷）" // 截斷的行結尾加上的標示
)

// errOCRClosed 表示 RapidOCR 已 Close，不再啟動 worker。
var errOCRClosed = errors.New("rapidocr closed")

// RapidOCR 透過常駐的 Python 子行程（ocr/ocr_worker.py，RapidOCR + OpenCC）辨識文字（Q54）。
// 以 stdin/stdout 一行一個 JSON 溝通；第一張圖才啟動 worker，閒置 IdleTimeout 後結束（Q55）。
// 同時只送一張；逾時、worker 結束、輸出非 JSON 或 id 不符時殺掉 worker，下一張重新啟動。
// 送出前發現 worker 已結束（兩張之間意外結束）時直接重新啟動，該張不失敗。
type RapidOCR struct {
	Command      []string      // worker 指令，預設 {"python3", "/app/ocr/ocr_worker.py"}
	StartTimeout time.Duration // 等待 ready 的上限（0 = 不限）
	Timeout      time.Duration // 每張辨識上限，不含排隊（0 = 不限；Q61）
	IdleTimeout  time.Duration // 閒置多久結束 worker（0 = 不結束；Q55）

	reqMu  sync.Mutex // 同時只處理一張：整個請求期間持有
	nextID int64      // 由 reqMu 保護

	mu      sync.Mutex // 保護以下欄位；等待 worker 時不持有，讓 Close 能立即殺掉 worker
	w       *ocrWorker // 目前的 worker（含啟動中、尚未 ready 的）
	busy    bool       // 是否有請求進行中
	idle    *time.Timer
	idleGen int // 每次排程或取消閒置計時就遞增；計時器觸發時比對，避免結束新的 worker
	closed  bool
}

// NewRapidOCR 回傳預設設定：啟動上限 60 秒、每張 3 分鐘（Q61，修改後）、閒置 5 分鐘結束（Q55）。
func NewRapidOCR() *RapidOCR {
	return &RapidOCR{
		Command:      []string{"python3", "/app/ocr/ocr_worker.py"},
		StartTimeout: 60 * time.Second,
		Timeout:      3 * time.Minute,
		IdleTimeout:  5 * time.Minute,
	}
}

// ocrWorker 是一個執行中的 worker 行程。
type ocrWorker struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   io.Closer
	lines    chan []byte   // stdout 的每一行（略過空行）；讀到 EOF、錯誤或放棄讀取時關閉
	quit     chan struct{} // 關閉後不再需要 stdout：readLines 停止送出並結束
	quitOnce sync.Once
	exited   chan struct{} // stdout 讀完（或放棄讀取）且行程已回收（cmd.Wait 結束）後關閉
}

// workerMessage 是 worker 輸出的一行：ready、辨識結果或錯誤。
type workerMessage struct {
	Ready bool      `json:"ready"`
	ID    *int64    `json:"id"`
	Items []OCRItem `json:"items"`
	Error string    `json:"error"`
}

// RecognizeText 把已前處理的 img 送給 worker 辨識，回傳 JoinOCRItems 組好的文字。
// 寬或高 < 2px、沒有任何文字時回 ErrNoText。
func (r *RapidOCR) RecognizeText(ctx context.Context, img image.Image) (string, error) {
	items, err := r.RecognizeItems(ctx, img)
	if err != nil {
		return "", err
	}
	text := JoinOCRItems(items)
	if text == "" {
		return "", ErrNoText
	}
	return text, nil
}

// RecognizeItems 把已前處理的 img 送給 worker 辨識，回傳讀到的文字框（可能為空）。
// 寬或高 < 2px 時回空結果、不送 worker。
func (r *RapidOCR) RecognizeItems(ctx context.Context, img image.Image) ([]OCRItem, error) {
	if b := img.Bounds(); b.Dx() < 2 || b.Dy() < 2 {
		return nil, nil
	}
	data, err := EncodePNG(img)
	if err != nil {
		return nil, err
	}

	r.reqMu.Lock()
	defer r.reqMu.Unlock()
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, errOCRClosed
	}
	r.cancelIdle()
	r.busy = true
	w := r.w
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.busy = false
		r.scheduleIdle()
		r.mu.Unlock()
	}()

	if w != nil && !w.alive() {
		// 兩張之間 worker 意外結束（OOM 被殺等）：重新啟動，不讓這張失敗。
		log.Printf("OCR worker 已結束，重新啟動")
		r.discard(w)
		w = nil
	}
	if w == nil {
		if w, err = r.start(ctx); err != nil {
			return nil, err
		}
	}
	r.nextID++
	req, err := json.Marshal(struct {
		ID  int64  `json:"id"`
		PNG []byte `json:"png"` // base64
	}{r.nextID, data})
	if err != nil {
		return nil, err
	}
	msg, err := r.exchange(ctx, w, append(req, '\n'), r.nextID)
	if err != nil {
		r.discard(w)
		return nil, err
	}
	if msg.Error != "" {
		return nil, fmt.Errorf("OCR worker 辨識失敗：%s", msg.Error)
	}
	return msg.Items, nil
}

// exchange 送出一行請求並等待對應 id 的回應。回傳錯誤時 worker 已不可再用（呼叫端要殺掉）。
func (r *RapidOCR) exchange(ctx context.Context, w *ocrWorker, req []byte, id int64) (*workerMessage, error) {
	writeErr := make(chan error, 1)
	go func() {
		_, err := w.stdin.Write(req)
		writeErr <- err
	}()
	timeout, stop := timer(r.Timeout)
	defer stop()
	for {
		select {
		case err := <-writeErr:
			if err != nil {
				return nil, fmt.Errorf("送出 OCR 請求失敗：%w", err)
			}
			writeErr = nil
		case line, ok := <-w.lines:
			if !ok {
				return nil, errors.New("OCR worker 意外結束")
			}
			var msg workerMessage
			if err := json.Unmarshal(line, &msg); err != nil {
				return nil, fmt.Errorf("OCR worker 輸出不是 JSON：%q", truncate(line))
			}
			if msg.ID == nil || *msg.ID != id {
				return nil, fmt.Errorf("OCR worker 回應 id 不符（預期 %d）：%q", id, truncate(line))
			}
			return &msg, nil
		case <-timeout:
			return nil, fmt.Errorf("OCR 逾時（超過 %v）", r.Timeout)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// start 啟動 worker、登記為目前的 worker（Close 可隨時殺掉），再等待 {"ready":true}；失敗時殺掉 worker。
func (r *RapidOCR) start(ctx context.Context) (*ocrWorker, error) {
	if len(r.Command) == 0 {
		return nil, errors.New("未設定 OCR worker 指令")
	}
	// 不用 CommandContext：worker 的生命週期由 RapidOCR 管，不跟著單一請求的 ctx。
	cmd := exec.Command(r.Command[0], r.Command[1:]...)
	stderr := &workerLogWriter{}
	cmd.Stderr = stderr
	cmd.WaitDelay = workerWaitDelay
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("啟動 OCR worker 失敗：%w", err)
	}
	w := &ocrWorker{cmd: cmd, stdin: stdin, stdout: stdout, lines: make(chan []byte), quit: make(chan struct{}), exited: make(chan struct{})}
	go func() {
		// 依 exec 文件：stdout 讀完才呼叫 Wait（Wait 會關閉 stdout，先呼叫可能讓最後的輸出遺失）。
		w.readLines(stdout)
		cmd.Wait()
		stderr.flush()
		close(w.exited)
	}()

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		w.kill()
		return nil, errOCRClosed
	}
	r.w = w
	r.mu.Unlock()

	timeout, stop := timer(r.StartTimeout)
	defer stop()
	fail := func(err error) (*ocrWorker, error) {
		r.discard(w)
		return nil, err
	}
	select {
	case line, ok := <-w.lines:
		if !ok {
			r.mu.Lock()
			closed := r.closed
			r.mu.Unlock()
			if closed {
				return fail(errOCRClosed)
			}
			return fail(errors.New("OCR worker 啟動後意外結束"))
		}
		var msg workerMessage
		if err := json.Unmarshal(line, &msg); err != nil || !msg.Ready {
			return fail(fmt.Errorf("OCR worker 啟動回應不是 ready：%q", truncate(line)))
		}
		return w, nil
	case <-timeout:
		return fail(fmt.Errorf("OCR worker 啟動逾時（超過 %v）", r.StartTimeout))
	case <-ctx.Done():
		return fail(ctx.Err())
	}
}

// discard 殺掉 w；w 仍是目前的 worker 時清除，讓下一張重新啟動。
func (r *RapidOCR) discard(w *ocrWorker) {
	w.kill()
	r.mu.Lock()
	if r.w == w {
		r.w = nil
	}
	r.mu.Unlock()
}

// readLines 把 stdout 的每一行（略過空行）送到 w.lines；讀到 EOF、錯誤或 quit 關閉後停止。
func (w *ocrWorker) readLines(stdout io.Reader) {
	defer close(w.lines)
	br := bufio.NewReader(stdout)
	for {
		line, err := br.ReadBytes('\n')
		if line = bytes.TrimSpace(line); len(line) > 0 {
			select {
			case w.lines <- line:
			case <-w.quit:
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// alive 回傳 worker 行程是否仍在執行（尚未結束並被回收）。
func (w *ocrWorker) alive() bool {
	select {
	case <-w.exited:
		return false
	default:
		return true
	}
}

// kill 強制結束 worker、放棄讀取 stdout，並等待回收。可重複呼叫。
// 關閉 stdout 讀取端讓 readLines 立即結束（孫行程佔住 stdout 時也不會等不到 EOF）。
func (w *ocrWorker) kill() {
	w.cmd.Process.Kill()
	w.quitOnce.Do(func() { close(w.quit) })
	w.stdout.Close()
	<-w.exited
}

// stop 關閉 stdin 讓 worker 自行結束；workerStopGrace 內沒結束就強制結束。
func (w *ocrWorker) stop() {
	w.stdin.Close()
	select {
	case <-w.exited:
	case <-time.After(workerStopGrace):
		w.kill()
	}
}

// Close 結束 worker（bot 結束時呼叫）；之後的 RecognizeText 回錯誤、不再啟動 worker。
// 有請求進行中（含啟動中）時不等它完成：直接殺掉 worker，進行中的請求以錯誤返回；閒置時關閉 stdin 讓 worker 自行結束。
func (r *RapidOCR) Close() error {
	r.mu.Lock()
	r.closed = true
	r.cancelIdle()
	w, busy := r.w, r.busy
	r.w = nil
	r.mu.Unlock()
	if w == nil {
		return nil
	}
	if busy {
		w.kill()
	} else {
		w.stop()
	}
	return nil
}

// cancelIdle 取消閒置計時（呼叫時須持有 r.mu）。
func (r *RapidOCR) cancelIdle() {
	if r.idle != nil {
		r.idle.Stop()
		r.idle = nil
	}
	r.idleGen++
}

// scheduleIdle 在 IdleTimeout > 0 且 worker 執行中時開始閒置計時（呼叫時須持有 r.mu）。
func (r *RapidOCR) scheduleIdle() {
	if r.IdleTimeout <= 0 || r.w == nil || r.closed {
		return
	}
	r.idleGen++
	gen := r.idleGen
	r.idle = time.AfterFunc(r.IdleTimeout, func() {
		r.mu.Lock()
		if gen != r.idleGen || r.w == nil || r.busy {
			r.mu.Unlock()
			return
		}
		w := r.w
		r.w = nil
		r.idle = nil
		r.mu.Unlock()
		log.Printf("OCR worker 閒置超過 %v，結束 worker", r.IdleTimeout)
		w.stop()
	})
}

// timer 回傳 d 後觸發的 channel 與停止函式；d <= 0 時 channel 永不觸發。
func timer(d time.Duration) (<-chan time.Time, func()) {
	if d <= 0 {
		return nil, func() {}
	}
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}

// truncate 截斷過長的 worker 輸出，用於錯誤訊息。
func truncate(b []byte) string {
	if len(b) > workerMaxLogLine {
		return string(b[:workerMaxLogLine]) + "…"
	}
	return string(b)
}

// workerLogWriter 把 worker 的 stderr 逐行寫到 log：跨 Write 緩衝到換行才輸出，
// 單行超過 workerMaxStderrLine bytes 時截斷（其餘丟棄到換行為止）。只由 exec 的 stderr 複製 goroutine 呼叫。
type workerLogWriter struct {
	buf       []byte
	truncated bool
}

func (w *workerLogWriter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		chunk, rest, newline := bytes.Cut(p, []byte{'\n'})
		if room := workerMaxStderrLine - len(w.buf); len(chunk) > room {
			chunk = chunk[:room]
			w.truncated = true
		}
		w.buf = append(w.buf, chunk...)
		if !newline {
			break
		}
		w.flush()
		p = rest
	}
	return n, nil
}

// flush 輸出緩衝中的內容（空白行略過）並清空；worker 結束後呼叫，輸出最後一行沒有換行的內容。
func (w *workerLogWriter) flush() {
	line := strings.TrimSpace(strings.ToValidUTF8(string(w.buf), ""))
	if w.truncated {
		line += workerLogTruncated
	}
	if line != "" {
		log.Printf("OCR worker：%s", line)
	}
	w.buf, w.truncated = w.buf[:0], false
}
