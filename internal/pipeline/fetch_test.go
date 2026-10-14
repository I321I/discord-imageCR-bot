package pipeline

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
