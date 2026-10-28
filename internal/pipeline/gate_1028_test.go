package pipeline

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// 把關（10/28）：補充 Task 1～3 與重複處理的邊界案例。

// Content-Length 剛好等於上限 → 成功（不可因 LimitReader 多讀 1 byte 的設計誤判）。
func TestGate1028_HTTPFetcher_ExactlyMaxBytesOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat("x", 10))) // 小 body：net/http 自動帶 Content-Length
	}))
	defer srv.Close()
	f := NewHTTPFetcher()
	f.MaxBytes = 10
	got, err := f.Fetch(context.Background(), srv.URL)
	if err != nil || len(got) != 10 {
		t.Errorf("剛好上限應成功，得到 %d bytes %v", len(got), err)
	}
}

// 轉址後以最終回應的 Content-Length 比對（第一跳的 Content-Length 不同也不可誤判）。
func TestGate1028_HTTPFetcher_RedirectUsesFinalContentLength(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/a", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/b", http.StatusFound) })
	mux.HandleFunc("/b", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(strings.Repeat("z", 777))) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	got, err := NewHTTPFetcher().Fetch(context.Background(), srv.URL+"/a")
	if err != nil || len(got) != 777 {
		t.Errorf("轉址後應成功，得到 %d bytes %v", len(got), err)
	}
}

// 伺服器宣告的 Content-Length 大於上限、實際也送超過上限 → ErrTooLarge（不是 ErrSizeMismatch）。
func TestGate1028_HTTPFetcher_DeclaredHugeIsTooLarge(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.Write([]byte(strings.Repeat("x", 1000)))
	}))
	defer srv.Close()
	f := NewHTTPFetcher()
	f.MaxBytes = 100
	if _, err := f.Fetch(context.Background(), srv.URL); !errors.Is(err, ErrTooLarge) || errors.Is(err, ErrSizeMismatch) {
		t.Errorf("應回 ErrTooLarge：%v", err)
	}
}

// 預覽網址回 Content-Type image/png 但內容不是圖片 → ErrNotImage、加 ❌、不上傳（預覽也必須通過解碼檢查）。
func TestGate1028_PreviewFallback_PreviewNotDecodable(t *testing.T) {
	srv := newPreviewServer(t)
	fakeImg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte("<svg xmlns='http://www.w3.org/2000/svg'></svg>"))
	}))
	defer fakeImg.Close()
	e := newLinkEnv(t)
	e.p.URLFetch = newURLFetcher(allowOnly(srv.Server, fakeImg))
	page := srv.URL + "/view/a.jpg"
	m := e.linkMessage(page, imagePreview(page, fakeImg.URL+"/p.png"))
	if err := e.p.Process(context.Background(), testGuild, m); !errors.Is(err, ErrNotImage) {
		t.Errorf("預覽內容不是圖片應回 ErrNotImage：%v", err)
	}
	e.failedWithX(t, m)
}

// 預覽網址（允許的伺服器）轉址到私有位址 → ErrBlockedAddress（每一跳都檢查，fallback 不可繞過）。
func TestGate1028_PreviewFallback_RedirectToPrivateBlocked(t *testing.T) {
	srv := newPreviewServer(t)
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://10.0.0.1/x.jpg", http.StatusFound)
	}))
	defer redir.Close()
	e := newLinkEnv(t)
	e.p.URLFetch = newURLFetcher(allowOnly(srv.Server, redir))
	page := srv.URL + "/view/a.jpg"
	m := e.linkMessage(page, imagePreview(page, redir.URL+"/r.jpg"))
	if err := e.p.Process(context.Background(), testGuild, m); !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("轉址到私有位址應回 ErrBlockedAddress：%v", err)
	}
	e.failedWithX(t, m)
}

// 等待後重讀遇非 404 錯誤（500）→ 只記 log，沿用原預覽（沒有）→ ErrNotImage、加 ❌。
func TestGate1028_PreviewFallback_RefetchServerErrorFails(t *testing.T) {
	srv := newPreviewServer(t)
	e := newPreviewEnv(t, srv)
	m := e.linkMessage(srv.URL + "/view/a.jpg")
	e.fake.FailOn["ChannelMessage"] = &discordgo.RESTError{Response: &http.Response{StatusCode: 500, Status: "500"}}
	if err := e.p.Process(context.Background(), testGuild, m); !errors.Is(err, ErrNotImage) {
		t.Errorf("應回 ErrNotImage：%v", err)
	}
	if n := e.fake.Calls["ChannelMessage"]; n != 1 {
		t.Errorf("應重讀 1 次：%d", n)
	}
	delete(e.fake.FailOn, "ChannelMessage") // failedWithX 以 ChannelMessage 確認原訊息仍在
	e.failedWithX(t, m)
}

// 預覽 fallback 等待期間（佔住佇列）收到 MESSAGE_UPDATE（預覽補上）與補處理：以訊息 ID 去重，
// 只發出一次、只刪除一次。
func TestGate1028_PreviewWait_NoDuplicateWithUpdateAndBackfill(t *testing.T) {
	srv := newPreviewServer(t)
	e := newPreviewEnv(t, srv)
	e.p.EmbedWait = 100 * time.Millisecond
	page := srv.URL + "/view/a.jpg"
	m := e.linkMessage(page)
	var wg sync.WaitGroup
	var once sync.Once
	srv.onView = func() {
		once.Do(func() {
			fresh := *m
			fresh.Embeds = []*discordgo.MessageEmbed{imagePreview(page, srv.URL+"/img/real.jpg")}
			e.fake.ReplaceMessage(&fresh)
			wg.Add(2)
			go func() { defer wg.Done(); e.p.HandleMessageUpdate(context.Background(), &fresh) }()
			go func() { defer wg.Done(); <-e.p.EnqueueBackfill(context.Background(), testGuild, &fresh) }()
		})
	}
	if err := e.p.Process(context.Background(), testGuild, m); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if len(e.fake.Sent) != 1 {
		t.Errorf("應只發出 1 則，得到 %d", len(e.fake.Sent))
	}
	if n := e.fake.Calls["ChannelMessageDelete"]; n != 1 {
		t.Errorf("應只刪除 1 次，得到 %d", n)
	}
}

// 即時處理完成（原訊息已刪）後才到的補處理／MESSAGE_UPDATE：輪到時重讀 404 → 略過，不重複發出。
func TestGate1028_LateBackfillAfterDone_NoDuplicate(t *testing.T) {
	e := newEnv(t)
	m := e.imageMessage(t, "a.png")
	stale := *m
	if err := e.p.Process(context.Background(), testGuild, m); err != nil {
		t.Fatal(err)
	}
	if err := <-e.p.EnqueueBackfill(context.Background(), testGuild, &stale); err != nil {
		t.Errorf("補處理已刪除的訊息應略過：%v", err)
	}
	e.p.HandleMessageUpdate(context.Background(), &stale)
	if len(e.fake.Sent) != 1 {
		t.Errorf("應只發出 1 則，得到 %d", len(e.fake.Sent))
	}
	if n := e.fake.Calls["ChannelMessageDelete"]; n != 1 {
		t.Errorf("應只刪除 1 次，得到 %d", n)
	}
}

// 記錄現況（非重複路徑的必要條件）：不重讀的 Enqueue（HandleMessage／Reprocess）若在同一則訊息處理完成後
// 才以舊的訊息內容排入，會再發一次，且刪除遇 10008 現在被當成成功、不留任何 log。
// 實際程式中：HandleMessage 每個 MESSAGE_CREATE 只呼叫一次且在任何網路呼叫前排入；Reprocess 先重讀並要求帶 ❌。
func TestGate1028_StaleNonRefetchEnqueue_DuplicatesSilently(t *testing.T) {
	e := newEnv(t)
	m := e.imageMessage(t, "a.png")
	stale := *m
	if err := e.p.Process(context.Background(), testGuild, m); err != nil {
		t.Fatal(err)
	}
	// 模擬第二個處理者（例如另一個 bot 行程）：附件網址仍可下載，原訊息已不在。
	e.fake.FailOn["ChannelMessageDelete"] = &discordgo.RESTError{
		Response: &http.Response{StatusCode: http.StatusNotFound},
		Message:  &discordgo.APIErrorMessage{Code: discordgo.ErrCodeUnknownMessage},
	}
	err := e.p.Process(context.Background(), testGuild, &stale)
	if err != nil {
		t.Logf("Process 回 %v", err)
	}
	t.Logf("Sent = %d（2 表示重複發出且回 nil）", len(e.fake.Sent))
	if len(e.fake.Sent) != 2 || err != nil {
		t.Errorf("現況假設改變：Sent %d err %v", len(e.fake.Sent), err)
	}
}
