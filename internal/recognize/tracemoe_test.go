package recognize

import (
	"context"
	"errors"
	"image"
	"image/color"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

const frierenJSON = `{"frameCount":1,"error":"","result":[{"anilist":{"id":154587,"title":{"native":"葬送のフリーレン","romaji":"Sousou no Frieren","english":"Frieren: Beyond Journey's End"},"synonyms":["Frieren","葬送的芙莉蓮","フリーレン","葬送的芙莉莲"],"isAdult":false},"episode":1,"similarity":0.95}]}`

// newTraceMoeServer 回傳以 handler 回應的 trace.moe 假伺服器與指向它的 client。
func newTraceMoeServer(t *testing.T, handler http.HandlerFunc) *TraceMoe {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	tm := NewTraceMoe("", 0.9)
	tm.BaseURL = srv.URL
	tm.Client = srv.Client()
	return tm
}

func respondJSON(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}
}

func TestNewTraceMoe_Defaults(t *testing.T) {
	tm := NewTraceMoe("k", 0.8)
	if tm.BaseURL != "https://api.trace.moe" || tm.Key != "k" || tm.MinSimilarity != 0.8 || tm.Client == nil || tm.Timeout != 30*time.Second {
		t.Errorf("預設值不符：%+v", tm)
	}
}

func TestTraceMoe_Success(t *testing.T) {
	var gotMethod, gotPath, gotType, gotKey string
	var gotQuery map[string][]string
	var gotBody []byte
	tm := newTraceMoeServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotType = r.Method, r.URL.Path, r.Header.Get("Content-Type")
		gotKey = r.Header.Get("x-trace-key")
		gotQuery = r.URL.Query()
		gotBody, _ = io.ReadAll(r.Body)
		respondJSON(frierenJSON)(w, r)
	})
	name, err := tm.RecognizeWork(context.Background(), []byte("PNGDATA"))
	if err != nil {
		t.Fatal(err)
	}
	if gotMethod != http.MethodPost || gotPath != "/search" || gotType != "image/png" || string(gotBody) != "PNGDATA" {
		t.Errorf("請求不符：%s %s type=%q body=%q", gotMethod, gotPath, gotType, gotBody)
	}
	if _, ok := gotQuery["anilistInfo"]; !ok {
		t.Errorf("query 應含 anilistInfo：%v", gotQuery)
	}
	if _, ok := gotQuery["cutBorders"]; !ok {
		t.Errorf("query 應含 cutBorders：%v", gotQuery)
	}
	if gotKey != "" {
		t.Errorf("Key 為空時不應帶 x-trace-key，得到 %q", gotKey)
	}
	// Q14：含中文的別名放最前（依原順序），再接 native、english、romaji；去重，以「、」連接。
	want := "葬送的芙莉蓮、葬送的芙莉莲、葬送のフリーレン、Frieren: Beyond Journey's End、Sousou no Frieren"
	if name != want {
		t.Errorf("作品名稱\n得到 %q\n預期 %q", name, want)
	}
}

func TestTraceMoe_SendsKey(t *testing.T) {
	var gotKey string
	tm := newTraceMoeServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-trace-key")
		respondJSON(frierenJSON)(w, r)
	})
	tm.Key = "secret"
	if _, err := tm.RecognizeWork(context.Background(), []byte("x")); err != nil {
		t.Fatal(err)
	}
	if gotKey != "secret" {
		t.Errorf("應帶 x-trace-key: secret，得到 %q", gotKey)
	}
}

func TestTraceMoe_NamesDedupAndMissing(t *testing.T) {
	tm := newTraceMoeServer(t, respondJSON(`{"error":"","result":[{"anilist":{"title":{"native":"銀魂","romaji":"Gintama","english":"Gintama"},"synonyms":["銀魂","Gin Tama"]},"similarity":0.99}]}`))
	name, err := tm.RecognizeWork(context.Background(), []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if name != "銀魂、Gintama" {
		t.Errorf("應去重，得到 %q", name)
	}

	tm = newTraceMoeServer(t, respondJSON(`{"error":"","result":[{"anilist":{"title":{"native":null,"romaji":"Only Romaji","english":null},"synonyms":null},"similarity":0.99}]}`))
	name, err = tm.RecognizeWork(context.Background(), []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if name != "Only Romaji" {
		t.Errorf("缺少的名稱應略過，得到 %q", name)
	}
}

func TestTraceMoe_NoNames(t *testing.T) {
	tm := newTraceMoeServer(t, respondJSON(`{"error":"","result":[{"anilist":{"title":{}},"similarity":0.99}]}`))
	if _, err := tm.RecognizeWork(context.Background(), []byte("x")); !errors.Is(err, ErrNotFound) {
		t.Errorf("沒有任何名稱應回 ErrNotFound，得到 %v", err)
	}
}

func TestTraceMoe_LowSimilarity(t *testing.T) {
	tm := newTraceMoeServer(t, respondJSON(`{"error":"","result":[{"anilist":{"title":{"native":"A"}},"similarity":0.89}]}`))
	if _, err := tm.RecognizeWork(context.Background(), []byte("x")); !errors.Is(err, ErrNotFound) {
		t.Errorf("相似度低於門檻應回 ErrNotFound，得到 %v", err)
	}
	tm = newTraceMoeServer(t, respondJSON(`{"error":"","result":[{"anilist":{"title":{"native":"A"}},"similarity":0.9}]}`))
	if name, err := tm.RecognizeWork(context.Background(), []byte("x")); err != nil || name != "A" {
		t.Errorf("相似度等於門檻應成功，得到 %q %v", name, err)
	}
}

func TestTraceMoe_EmptyResult(t *testing.T) {
	tm := newTraceMoeServer(t, respondJSON(`{"frameCount":0,"error":"","result":[]}`))
	if _, err := tm.RecognizeWork(context.Background(), []byte("x")); !errors.Is(err, ErrNotFound) {
		t.Errorf("沒有結果應回 ErrNotFound，得到 %v", err)
	}
}

func TestTraceMoe_QuotaExceeded402(t *testing.T) {
	for _, code := range []int{http.StatusPaymentRequired, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable, http.StatusBadRequest} {
		tm := newTraceMoeServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			io.WriteString(w, `{"error":"Search quota depleted"}`)
		})
		_, err := tm.RecognizeWork(context.Background(), []byte("x"))
		if err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("HTTP %d 應回錯誤（非 ErrNotFound），得到 %v", code, err)
		}
	}
}

func TestTraceMoe_ErrorField(t *testing.T) {
	tm := newTraceMoeServer(t, respondJSON(`{"error":"Failed to process image","result":[]}`))
	_, err := tm.RecognizeWork(context.Background(), []byte("x"))
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("JSON error 非空應回錯誤（非 ErrNotFound），得到 %v", err)
	}
}

func TestTraceMoe_BadJSON(t *testing.T) {
	tm := newTraceMoeServer(t, respondJSON(`not json`))
	if _, err := tm.RecognizeWork(context.Background(), []byte("x")); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("JSON 格式錯誤應回錯誤，得到 %v", err)
	}
}

func TestTraceMoe_ContextTimeout(t *testing.T) {
	release := make(chan struct{})
	tm := newTraceMoeServer(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := tm.RecognizeWork(ctx, []byte("x"))
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("逾時應回 context.DeadlineExceeded，得到 %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ctx 逾時後沒有回傳")
	}
}

func TestPrepareForTraceMoe_ShrinksLongSideTo1000(t *testing.T) {
	for _, c := range []struct{ w, h, wantW, wantH int }{
		{1920, 1080, 1000, 562},
		{1080, 1920, 562, 1000},
		{1001, 10, 1000, 9},
	} {
		if b := PrepareForTraceMoe(solid(c.w, c.h, color.White)).Bounds(); b.Dx() != c.wantW || b.Dy() != c.wantH {
			t.Errorf("%dx%d 應縮為 %dx%d，得到 %v", c.w, c.h, c.wantW, c.wantH, b)
		}
	}
}

func TestPrepareForTraceMoe_SmallUnchanged(t *testing.T) {
	for _, src := range []image.Image{solid(1000, 500, color.White), solid(640, 360, color.White)} {
		if got := PrepareForTraceMoe(src); got != src {
			t.Errorf("%v 長邊不超過 1000 應原樣回傳", src.Bounds())
		}
	}
}

// trace.moe 的名稱不拆逗號：含「,」或「，」的名稱整個保留。
func TestTraceMoe_NamesNotSplitOnCommas(t *testing.T) {
	tm := newTraceMoeServer(t, respondJSON(`{"error":"","result":[{"anilist":{"title":{"native":"中二病でも恋がしたい!","romaji":"Chuunibyou demo Koi ga Shitai!","english":"Love, Chunibyo & Other Delusions!"},"synonyms":["中二病也想談戀愛，第一季"]},"similarity":0.99}]}`))
	name, err := tm.RecognizeWork(context.Background(), []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	want := "中二病也想談戀愛，第一季、中二病でも恋がしたい!、Love, Chunibyo & Other Delusions!、Chuunibyou demo Koi ga Shitai!"
	if name != want {
		t.Errorf("得到 %q\n預期 %q", name, want)
	}
}

// 每張圖的逾時（Timeout）：呼叫端的 ctx 沒有期限時，超過 Timeout 也要回 context.DeadlineExceeded。
func TestTraceMoe_PerRequestTimeout(t *testing.T) {
	release := make(chan struct{})
	tm := newTraceMoeServer(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	defer close(release)
	tm.Timeout = 50 * time.Millisecond
	done := make(chan error, 1)
	go func() {
		_, err := tm.RecognizeWork(context.Background(), []byte("x"))
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("逾時應回 context.DeadlineExceeded，得到 %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Timeout 沒有生效")
	}
}

// HTTP 非 2xx 時錯誤訊息只保留回應本文前 300 字。
func TestTraceMoe_HTTPErrorBodyTruncated(t *testing.T) {
	tm := newTraceMoeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		io.WriteString(w, strings.Repeat("錯", 300)+strings.Repeat("x", 5000))
	})
	_, err := tm.RecognizeWork(context.Background(), []byte("x"))
	if err == nil {
		t.Fatal("應回錯誤")
	}
	if msg := err.Error(); !strings.Contains(msg, strings.Repeat("錯", 300)) || strings.Contains(msg, "x") || utf8.RuneCountInString(msg) > 400 {
		t.Errorf("錯誤訊息應只含本文前 300 字，得到 %d 字：%.80q…", utf8.RuneCountInString(msg), msg)
	}

	short := newTraceMoeServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		io.WriteString(w, `{"error":"Search quota depleted"}`)
	})
	if _, err := short.RecognizeWork(context.Background(), []byte("x")); err == nil || !strings.Contains(err.Error(), "Search quota depleted") {
		t.Errorf("短本文應完整保留，得到 %v", err)
	}
}
