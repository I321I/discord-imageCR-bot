package pipeline

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// 10/28 Task 3：看圖網頁網址（副檔名像圖片、實際回網頁）改下載 Discord 預覽的圖片。

// previewServer 是一個 httptest 伺服器：/view/ 回網頁、/img/ 回 JPEG、/html/ 回網頁、/gone/ 回 404、/big/ 回 2KB；
// 記錄各路徑的請求次數。onView 在回 /view/ 之前呼叫（模擬等待期間發生的事）。
type previewServer struct {
	*httptest.Server
	jpeg   []byte
	hits   map[string]*atomic.Int32
	onView func()
}

func newPreviewServer(t *testing.T) *previewServer {
	t.Helper()
	s := &previewServer{jpeg: jpegBytes(t, 64, 36), hits: map[string]*atomic.Int32{}}
	for _, p := range []string{"/view/", "/img/", "/html/", "/gone/", "/big/"} {
		s.hits[p] = &atomic.Int32{}
	}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for p, n := range s.hits {
			if strings.HasPrefix(r.URL.Path, p) {
				n.Add(1)
			}
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/view/"), strings.HasPrefix(r.URL.Path, "/html/"):
			if strings.HasPrefix(r.URL.Path, "/view/") && s.onView != nil {
				s.onView()
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte("<html><body><img src=x></body></html>"))
		case strings.HasPrefix(r.URL.Path, "/img/"):
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write(s.jpeg)
		case strings.HasPrefix(r.URL.Path, "/gone/"):
			http.NotFound(w, r)
		case strings.HasPrefix(r.URL.Path, "/big/"):
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write(bytes.Repeat([]byte{0xff}, 2048))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

// newPreviewEnv 同 newLinkEnv，但網址下載器為真正的 URLFetcher（只放行 srv）。
func newPreviewEnv(t *testing.T, srv *previewServer) *linkEnv {
	t.Helper()
	e := newLinkEnv(t)
	e.p.URLFetch = newURLFetcher(allowOnly(srv.Server))
	return e
}

func imagePreview(page, img string) *discordgo.MessageEmbed {
	return &discordgo.MessageEmbed{Type: discordgo.EmbedTypeImage, URL: page, Image: &discordgo.MessageEmbedImage{URL: img}}
}

func (e *linkEnv) failedWithX(t *testing.T, m *discordgo.Message) {
	t.Helper()
	if len(e.fake.Sent) != 0 || len(e.fake.Deleted) != 0 || !e.originalExists(m) {
		t.Errorf("不應發出或刪除：Sent %d Deleted %d", len(e.fake.Sent), len(e.fake.Deleted))
	}
	if got := e.fake.ReactionsOn(testSource, m.ID); !slices.Equal(got, []string{"❌"}) {
		t.Errorf("應只有 ❌：%q", got)
	}
}

func TestProcess_PreviewFallback_PostsPreviewImage(t *testing.T) {
	srv := newPreviewServer(t)
	e := newPreviewEnv(t, srv)
	page := srv.URL + "/view/a.jpg"
	m := e.linkMessage("看圖網頁 "+page, imagePreview(page, srv.URL+"/img/real.jpg"))
	if err := e.p.Process(context.Background(), testGuild, m); err != nil {
		t.Fatal(err)
	}
	if len(e.fake.Sent) != 1 {
		t.Fatalf("應發出 1 則，得到 %d", len(e.fake.Sent))
	}
	s := e.fake.Sent[0]
	if !bytes.Equal(s.Files[0], srv.jpeg) {
		t.Error("上傳內容應為預覽圖")
	}
	// Q5：檔名沿用原網址。
	if f := s.Data.Files[0]; f.Name != "a.jpg" || f.ContentType != "image/jpeg" {
		t.Errorf("檔名／類型：%s %s", f.Name, f.ContentType)
	}
	if !strings.Contains(s.Data.Content, "看圖網頁") || strings.Contains(s.Data.Content, page) {
		t.Errorf("關鍵字應去掉網址：%s", s.Data.Content)
	}
	if n := e.fake.Calls["ChannelMessage"]; n != 0 {
		t.Errorf("已有預覽時不應重讀訊息：%d 次", n)
	}
	if e.originalExists(m) {
		t.Error("成功應刪除原訊息")
	}
}

// 沒有預覽：等 EmbedWait 重讀（Q1 A）後仍沒有 → 照舊 ErrNotImage、加 ❌。
func TestProcess_PreviewFallback_NoPreviewFails(t *testing.T) {
	srv := newPreviewServer(t)
	e := newPreviewEnv(t, srv)
	m := e.linkMessage(srv.URL + "/view/a.jpg")
	err := e.p.Process(context.Background(), testGuild, m)
	if !errors.Is(err, ErrNotImage) {
		t.Errorf("應回 ErrNotImage：%v", err)
	}
	if n := e.fake.Calls["ChannelMessage"]; n != 1 {
		t.Errorf("應重讀訊息 1 次，得到 %d", n)
	}
	e.failedWithX(t, m)
}

func TestProcess_PreviewFallback_PreviewAlsoFails(t *testing.T) {
	srv := newPreviewServer(t)
	e := newPreviewEnv(t, srv)
	page, prev := srv.URL+"/view/a.jpg", srv.URL+"/html/b.jpg"
	m := e.linkMessage(page, imagePreview(page, prev))
	err := e.p.Process(context.Background(), testGuild, m)
	if !errors.Is(err, ErrNotImage) || !strings.Contains(err.Error(), page) || !strings.Contains(err.Error(), prev) {
		t.Errorf("錯誤應為 ErrNotImage 並含兩個網址：%v", err)
	}
	if srv.hits["/html/"].Load() != 1 {
		t.Error("應請求預覽網址一次")
	}
	e.failedWithX(t, m)
}

// 只在 ErrNotImage 時改抓預覽：404（ErrImageGone）、超過上限（ErrTooLarge）不請求預覽、不重讀訊息。
func TestProcess_PreviewFallback_OnlyOnNotImage(t *testing.T) {
	for _, c := range []struct {
		path string
		want error
	}{{"/gone/a.jpg", ErrImageGone}, {"/big/a.jpg", ErrTooLarge}} {
		t.Run(c.path, func(t *testing.T) {
			srv := newPreviewServer(t)
			e := newPreviewEnv(t, srv)
			e.p.URLFetch.(*URLFetcher).maxBytes = 1024
			page := srv.URL + c.path
			m := e.linkMessage(page, imagePreview(page, srv.URL+"/img/real.jpg"))
			if err := e.p.Process(context.Background(), testGuild, m); !errors.Is(err, c.want) {
				t.Errorf("應回 %v：%v", c.want, err)
			}
			if n := srv.hits["/img/"].Load(); n != 0 {
				t.Errorf("不應請求預覽網址：%d 次", n)
			}
			if n := e.fake.Calls["ChannelMessage"]; n != 0 {
				t.Errorf("不應重讀訊息：%d 次", n)
			}
			e.failedWithX(t, m)
		})
	}
}

// 預覽網址指向不允許的位址（私有位址、非允許的連接埠）→ ErrBlockedAddress，不可繞過 SSRF 檢查。
func TestProcess_PreviewFallback_BlockedAddressFails(t *testing.T) {
	srv := newPreviewServer(t)
	other := newPreviewServer(t) // 同在 127.0.0.1 但不在放行名單
	for _, prev := range []string{"http://10.0.0.1/x.jpg", other.URL + "/img/x.jpg"} {
		t.Run(prev, func(t *testing.T) {
			e := newPreviewEnv(t, srv)
			page := srv.URL + "/view/a.jpg"
			m := e.linkMessage(page, imagePreview(page, prev))
			if err := e.p.Process(context.Background(), testGuild, m); !errors.Is(err, ErrBlockedAddress) {
				t.Errorf("應回 ErrBlockedAddress：%v", err)
			}
			if n := other.hits["/img/"].Load(); n != 0 {
				t.Errorf("不應連到未放行的伺服器：%d 次", n)
			}
			e.failedWithX(t, m)
		})
	}
}

// 附件＋看圖網頁，fallback 成功 → 兩張都發（附件在前）。
func TestProcess_PreviewFallback_MixedWithAttachment(t *testing.T) {
	srv := newPreviewServer(t)
	e := newPreviewEnv(t, srv)
	m := e.imageMessage(t, "a.png")
	page := srv.URL + "/view/b.jpg"
	m.Content = "文字 " + page
	m.Embeds = []*discordgo.MessageEmbed{imagePreview(page, srv.URL+"/img/real.jpg")}
	e.fake.ReplaceMessage(m)
	if err := e.p.Process(context.Background(), testGuild, m); err != nil {
		t.Fatal(err)
	}
	if len(e.fake.Sent) != 2 {
		t.Fatalf("應發出 2 則，得到 %d", len(e.fake.Sent))
	}
	if e.fake.Sent[0].Data.Files[0].Name != "a.png" || e.fake.Sent[1].Data.Files[0].Name != "b.jpg" {
		t.Errorf("順序／檔名：%s %s", e.fake.Sent[0].Data.Files[0].Name, e.fake.Sent[1].Data.Files[0].Name)
	}
	if !bytes.Equal(e.fake.Sent[1].Files[0], srv.jpeg) {
		t.Error("第二張應為預覽圖")
	}
	if e.originalExists(m) {
		t.Error("成功應刪除原訊息")
	}
}

// Q1 A：第一次下載到網頁且尚無預覽 → 等 EmbedWait 重讀訊息，預覽已補上就改抓預覽圖。
func TestProcess_PreviewFallback_WaitsForPreview(t *testing.T) {
	srv := newPreviewServer(t)
	e := newPreviewEnv(t, srv)
	page := srv.URL + "/view/a.jpg"
	m := e.linkMessage(page)
	srv.onView = func() { // 處理中（等待期間之前）Discord 補上預覽
		fresh := *m
		fresh.Embeds = []*discordgo.MessageEmbed{imagePreview(page, srv.URL+"/img/real.jpg")}
		e.fake.ReplaceMessage(&fresh)
	}
	start := time.Now()
	if err := e.p.Process(context.Background(), testGuild, m); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < e.p.EmbedWait {
		t.Error("應等待 EmbedWait 才重讀")
	}
	if len(e.fake.Sent) != 1 || !bytes.Equal(e.fake.Sent[0].Files[0], srv.jpeg) {
		t.Fatalf("應發出預覽圖：Sent %d", len(e.fake.Sent))
	}
	if e.originalExists(m) {
		t.Error("成功應刪除原訊息")
	}
}

// 同一則有兩個看圖網頁、都沒有預覽 → 只等待、重讀一次。
func TestProcess_PreviewFallback_WaitsOncePerMessage(t *testing.T) {
	srv := newPreviewServer(t)
	e := newPreviewEnv(t, srv)
	a, b := srv.URL+"/view/a.jpg", srv.URL+"/view/b.jpg"
	m := e.linkMessage(a + " " + b)
	srv.onView = func() {
		fresh := *m
		fresh.Embeds = []*discordgo.MessageEmbed{imagePreview(a, srv.URL+"/img/a.jpg"), imagePreview(b, srv.URL+"/img/b.jpg")}
		e.fake.ReplaceMessage(&fresh)
	}
	if err := e.p.Process(context.Background(), testGuild, m); err != nil {
		t.Fatal(err)
	}
	if n := e.fake.Calls["ChannelMessage"]; n != 1 {
		t.Errorf("應只重讀 1 次，得到 %d", n)
	}
	if len(e.fake.Sent) != 2 {
		t.Errorf("應發出 2 則，得到 %d", len(e.fake.Sent))
	}
}

// Q1 A 子題：重讀時原訊息已刪除 → 比照 ErrNoImages：不發、不刪、不加 ❌，Process 回 nil。
func TestProcess_PreviewFallback_DeletedDuringWait(t *testing.T) {
	srv := newPreviewServer(t)
	e := newPreviewEnv(t, srv)
	m := e.linkMessage(srv.URL + "/view/a.jpg")
	srv.onView = func() { e.fake.ChannelMessageDelete(m.ChannelID, m.ID) }
	if err := e.p.Process(context.Background(), testGuild, m); err != nil {
		t.Errorf("原訊息已刪除應回 nil：%v", err)
	}
	if len(e.fake.Sent) != 0 {
		t.Errorf("不應發出：Sent %d", len(e.fake.Sent))
	}
	for _, c := range e.fake.ReactionCalls {
		if c.MessageID == m.ID && c.Emoji == "❌" {
			t.Error("不應加 ❌")
		}
	}
	// Reprocess（嚴格）收到 ErrNoImages。
	e2 := newPreviewEnv(t, srv)
	m2 := e2.linkMessage(srv.URL + "/view/a.jpg")
	srv.onView = func() { e2.fake.ChannelMessageDelete(m2.ChannelID, m2.ID) }
	if err := e2.p.process(context.Background(), testGuild, m2); !errors.Is(err, ErrNoImages) {
		t.Errorf("process 應回 ErrNoImages：%v", err)
	}
}

// 等待期間 ctx 結束（關機）→ 回 ctx 錯誤、不加 ❌。
func TestProcess_PreviewFallback_ShutdownDuringWait(t *testing.T) {
	srv := newPreviewServer(t)
	e := newPreviewEnv(t, srv)
	e.p.EmbedWait = 5 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := e.linkMessage(srv.URL + "/view/a.jpg")
	srv.onView = func() { time.AfterFunc(20*time.Millisecond, cancel) }
	start := time.Now()
	if err := e.p.Process(ctx, testGuild, m); !errors.Is(err, context.Canceled) {
		t.Errorf("應回 context.Canceled：%v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Error("ctx 結束應立刻放棄等待")
	}
	if n := e.fake.Calls["ChannelMessage"]; n != 0 {
		t.Errorf("ctx 結束不應重讀：%d", n)
	}
	if len(e.fake.Sent) != 0 || !e.originalExists(m) {
		t.Error("不應發出或刪除")
	}
	if got := e.fake.ReactionsOn(testSource, m.ID); slices.Contains(got, "❌") {
		t.Errorf("不應加 ❌：%q", got)
	}
}

// PM 決定：預覽的 Thumbnail.URL 就是原網址時，改抓 ProxyURL（同樣經 URLFetcher 的 SSRF 與解碼檢查）。
func TestProcess_PreviewFallback_UsesProxyURL(t *testing.T) {
	srv := newPreviewServer(t)
	e := newPreviewEnv(t, srv)
	page := srv.URL + "/view/a.jpg"
	m := e.linkMessage(page, &discordgo.MessageEmbed{Type: discordgo.EmbedTypeImage, URL: page,
		Thumbnail: &discordgo.MessageEmbedThumbnail{URL: page, ProxyURL: srv.URL + "/img/proxy.jpg"}})
	if err := e.p.Process(context.Background(), testGuild, m); err != nil {
		t.Fatal(err)
	}
	if len(e.fake.Sent) != 1 || !bytes.Equal(e.fake.Sent[0].Files[0], srv.jpeg) || e.fake.Sent[0].Data.Files[0].Name != "a.jpg" {
		t.Fatalf("應發出 ProxyURL 的圖、檔名沿用原網址：Sent %d", len(e.fake.Sent))
	}

	// ProxyURL 指向不允許的位址 → ErrBlockedAddress、加 ❌。
	e2 := newPreviewEnv(t, srv)
	m2 := e2.linkMessage(page, &discordgo.MessageEmbed{Type: discordgo.EmbedTypeImage, URL: page,
		Thumbnail: &discordgo.MessageEmbedThumbnail{URL: page, ProxyURL: "http://10.0.0.1/x.jpg"}})
	if err := e2.p.Process(context.Background(), testGuild, m2); !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("應回 ErrBlockedAddress：%v", err)
	}
	e2.failedWithX(t, m2)
}
