package pipeline

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestHTTPFetcher_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/a.png" {
			t.Errorf("請求 %s %s", r.Method, r.URL.Path)
		}
		w.Write([]byte("data"))
	}))
	defer srv.Close()
	got, err := NewHTTPFetcher().Fetch(context.Background(), srv.URL+"/a.png")
	if err != nil || string(got) != "data" {
		t.Errorf("得到 %q %v", got, err)
	}
}

func TestHTTPFetcher_Non200IsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer srv.Close()
	if _, err := NewHTTPFetcher().Fetch(context.Background(), srv.URL); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("非 200 應回含狀態碼的錯誤，得到 %v", err)
	}
}

// 下載內容超過上限（附件 > 25MB）→ ErrTooLarge，不把整個內容讀進記憶體。
func TestHTTPFetcher_TooLarge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat("x", 11)))
	}))
	defer srv.Close()
	f := NewHTTPFetcher()
	f.MaxBytes = 10
	if _, err := f.Fetch(context.Background(), srv.URL); !errors.Is(err, ErrTooLarge) {
		t.Errorf("超過上限應回 ErrTooLarge，得到 %v", err)
	}
	f.MaxBytes = 11
	if got, err := f.Fetch(context.Background(), srv.URL); err != nil || len(got) != 11 {
		t.Errorf("剛好等於上限應成功，得到 %d bytes %v", len(got), err)
	}
}

func TestHTTPFetcher_ContextCanceled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := NewHTTPFetcher().Fetch(ctx, srv.URL); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("ctx 逾時應回 DeadlineExceeded，得到 %v", err)
	}
}

func TestNewHTTPFetcher_Defaults(t *testing.T) {
	f := NewHTTPFetcher()
	if f.MaxBytes != MaxAttachmentSize || MaxAttachmentSize != 25<<20 {
		t.Errorf("MaxBytes %d，預期 25MB", f.MaxBytes)
	}
	if f.Client == nil || f.Client.Timeout != 2*time.Minute {
		t.Errorf("Client 逾時應為 2 分鐘：%+v", f.Client)
	}
}

// 10/28 Task 1：以 HTTP Content-Length 偵測截斷——宣告 100 bytes 只收到 50 → 錯誤。
func TestHTTPFetcher_ShortBodyIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.Write([]byte(strings.Repeat("x", 50)))
	}))
	defer srv.Close()
	_, err := NewHTTPFetcher().Fetch(context.Background(), srv.URL)
	if err == nil || !(errors.Is(err, ErrSizeMismatch) || errors.Is(err, io.ErrUnexpectedEOF)) {
		t.Errorf("內容短於 Content-Length 應回錯誤，得到 %v", err)
	}
}

// 10/28 Task 1：沒有 Content-Length（chunked）時不比對長度，照常成功。
func TestHTTPFetcher_NoContentLengthOK(t *testing.T) {
	const n = 315747
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		chunk := []byte(strings.Repeat("x", 1000))
		for left := n; left > 0; left -= len(chunk) {
			if left < len(chunk) {
				chunk = chunk[:left]
			}
			w.Write(chunk)
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()
	got, err := NewHTTPFetcher().Fetch(context.Background(), srv.URL)
	if err != nil || len(got) != n {
		t.Errorf("chunked 應成功、%d bytes，得到 %d bytes %v", n, len(got), err)
	}
}

// 10/28 Task 1：伺服器回 gzip、Transport 自動解壓（ContentLength 變 -1）時不可誤判截斷。
func TestHTTPFetcher_GzipAutoDecompressOK(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write([]byte(strings.Repeat("y", 5000)))
	zw.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
		w.Write(buf.Bytes())
	}))
	defer srv.Close()
	got, err := NewHTTPFetcher().Fetch(context.Background(), srv.URL)
	if err != nil || len(got) != 5000 {
		t.Errorf("gzip 自動解壓應成功、5000 bytes，得到 %d bytes %v", len(got), err)
	}
}

// 超過上限時回 ErrTooLarge 而不是 ErrSizeMismatch（Content-Length 與截到上限的長度必然不符）。
func TestHTTPFetcher_TooLargeWithContentLength(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "20")
		w.Write([]byte(strings.Repeat("x", 20)))
	}))
	defer srv.Close()
	f := NewHTTPFetcher()
	f.MaxBytes = 10
	if _, err := f.Fetch(context.Background(), srv.URL); !errors.Is(err, ErrTooLarge) || errors.Is(err, ErrSizeMismatch) {
		t.Errorf("超過上限應回 ErrTooLarge，得到 %v", err)
	}
}
