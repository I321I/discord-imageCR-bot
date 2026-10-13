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
)

// errOCRClosed 表示 RapidOCR 已 Close，不再啟動 worker。
var errOCRClosed = errors.New("rapidocr closed")

// RapidOCR 透過常駐的 Python 子行程（ocr/ocr_worker.py，RapidOCR + OpenCC）辨識文字（Q54）。
// 以 stdin/stdout 一行一個 JSON 溝通；第一張圖才啟動 worker，閒置 IdleTimeout 後結束（Q55）。
// 同時只送一張；逾時、worker 結束、輸出非 JSON 或 id 不符時殺掉 worker，下一張重新啟動。
type RapidOCR struct {
	Command      []string      // worker 指令，預設 {"python3", "/app/ocr/ocr_worker.py"}
	StartTimeout time.Duration // 等待 ready 的上限（0 = 不限）
	Timeout      time.Duration // 每張辨識上限，不含排隊（0 = 不限；Q61）
	IdleTimeout  time.Duration // 閒置多久結束 worker（0 = 不結束；Q55）

	mu      sync.Mutex
	w       *ocrWorker
	nextID  int64
	idle    *time.Timer
	idleGen int // 每次排程或取消閒置計時就遞增；計時器觸發時比對，避免結束新的 worker
	closed  bool
}

// NewRapidOCR 回傳預設設定：啟動上限 60 秒、每張 90 秒（Q61）、閒置 5 分鐘結束（Q55）。
func NewRapidOCR() *RapidOCR {
	return &RapidOCR{
		Command:      []string{"python3", "/app/ocr/ocr_worker.py"},
		StartTimeout: 60 * time.Second,
		Timeout:      90 * time.Second,
		IdleTimeout:  5 * time.Minute,
	}
}

// ocrWorker 是一個執行中的 worker 行程。
type ocrWorker struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  chan []byte   // stdout 的每一行（略過空行）；讀到 EOF 或錯誤時關閉
	exited chan struct{} // 行程結束並回收後關閉
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
	if b := img.Bounds(); b.Dx() < 2 || b.Dy() < 2 {
		return "", ErrNoText
	}
	data, err := EncodePNG(img)
	if err != nil {
		return "", err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return "", errOCRClosed
	}
	r.cancelIdle()
	defer r.scheduleIdle()

	if r.w == nil {
		w, err := r.start(ctx)
		if err != nil {
			return "", err
		}
		r.w = w
	}
	r.nextID++
	req, err := json.Marshal(struct {
		ID  int64  `json:"id"`
		PNG []byte `json:"png"` // base64
	}{r.nextID, data})
	if err != nil {
		return "", err
	}
	msg, err := r.exchange(ctx, append(req, '\n'), r.nextID)
	if err != nil {
		r.w.kill()
		r.w = nil
		return "", err
	}
	if msg.Error != "" {
		return "", fmt.Errorf("OCR worker 辨識失敗：%s", msg.Error)
	}
	text := JoinOCRItems(msg.Items)
	if text == "" {
		return "", ErrNoText
	}
	return text, nil
}

// exchange 送出一行請求並等待對應 id 的回應。回傳錯誤時 worker 已不可再用（呼叫端要殺掉）。
func (r *RapidOCR) exchange(ctx context.Context, req []byte, id int64) (*workerMessage, error) {
	w := r.w
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

// start 啟動 worker 並等待 {"ready":true}；失敗時殺掉 worker。
func (r *RapidOCR) start(ctx context.Context) (*ocrWorker, error) {
	if len(r.Command) == 0 {
		return nil, errors.New("未設定 OCR worker 指令")
	}
	// 不用 CommandContext：worker 的生命週期由 RapidOCR 管，不跟著單一請求的 ctx。
	cmd := exec.Command(r.Command[0], r.Command[1:]...)
	cmd.Stderr = workerLogWriter{}
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
	w := &ocrWorker{cmd: cmd, stdin: stdin, lines: make(chan []byte), exited: make(chan struct{})}
	go func() {
		cmd.Wait()
		close(w.exited)
	}()
	go w.readLines(stdout)

	timeout, stop := timer(r.StartTimeout)
	defer stop()
	fail := func(err error) (*ocrWorker, error) {
		w.kill()
		return nil, err
	}
	select {
	case line, ok := <-w.lines:
		if !ok {
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

// readLines 把 stdout 的每一行（略過空行）送到 w.lines；讀到 EOF、錯誤或行程結束後停止。
func (w *ocrWorker) readLines(stdout io.Reader) {
	defer close(w.lines)
	br := bufio.NewReader(stdout)
	for {
		line, err := br.ReadBytes('\n')
		if line = bytes.TrimSpace(line); len(line) > 0 {
			select {
			case w.lines <- line:
			case <-w.exited:
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// kill 強制結束 worker 並等待回收。
func (w *ocrWorker) kill() {
	w.cmd.Process.Kill()
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
func (r *RapidOCR) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	r.cancelIdle()
	if r.w != nil {
		r.w.stop()
		r.w = nil
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
	if r.IdleTimeout <= 0 || r.w == nil {
		return
	}
	r.idleGen++
	gen := r.idleGen
	r.idle = time.AfterFunc(r.IdleTimeout, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if gen != r.idleGen || r.w == nil {
			return
		}
		log.Printf("OCR worker 閒置超過 %v，結束 worker", r.IdleTimeout)
		r.w.stop()
		r.w = nil
		r.idle = nil
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

// workerLogWriter 把 worker 的 stderr 逐行寫到 log。
type workerLogWriter struct{}

func (workerLogWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(string(p), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			log.Printf("OCR worker：%s", line)
		}
	}
	return len(p), nil
}
