package pipeline

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCheckPublicAddr(t *testing.T) {
	blocked := []string{
		"127.0.0.1:443", "10.0.0.1:443", "172.16.0.1:443", "192.168.1.1:443", "169.254.169.254:80",
		"100.64.0.1:443", "0.0.0.0:443", "0.1.2.3:443", "192.0.0.8:443", "198.18.0.1:443", "240.0.0.1:443",
		"255.255.255.255:443", "[::1]:443", "[::ffff:127.0.0.1]:443", "[::ffff:10.0.0.1]:80", "[fe80::1]:443",
		"[fc00::1]:443", "[ff02::1]:443", "224.0.0.1:443", "[::]:443", "[64:ff9b::a00:1]:443", "[2001:db8::1]:443",
		"8.8.8.8:22", "8.8.8.8:8443", "8.8.8.8:0", // U10：只允許 80、443
	}
	allowed := []string{"8.8.8.8:443", "8.8.8.8:80", "[2606:4700::1]:443", "[::ffff:8.8.8.8]:443"}
	for _, s := range blocked {
		if err := CheckPublicAddr(netip.MustParseAddrPort(s)); !errors.Is(err, ErrBlockedAddress) {
			t.Errorf("%s 應被擋：%v", s, err)
		}
	}
	for _, s := range allowed {
		if err := CheckPublicAddr(netip.MustParseAddrPort(s)); err != nil {
			t.Errorf("%s 應放行：%v", s, err)
		}
	}
}

// countingServer 回傳記錄被呼叫次數的 httptest 伺服器。
func countingServer(t *testing.T, h http.HandlerFunc) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		if h != nil {
			h(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte("png"))
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func serverPort(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	_, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// allowOnly 只放行 srv 的位址（測試用：httptest 只在 127.0.0.1）。
func allowOnly(srvs ...*httptest.Server) func(netip.AddrPort) error {
	return func(ap netip.AddrPort) error {
		for _, s := range srvs {
			if ap.String() == s.Listener.Addr().String() {
				return nil
			}
		}
		return fmt.Errorf("%w：%s", ErrBlockedAddress, ap)
	}
}

func allowAll(netip.AddrPort) error { return nil }

func TestURLFetch_BlocksPrivateTargets(t *testing.T) {
	srv, n := countingServer(t, nil)
	port := serverPort(t, srv)
	f := NewURLFetcher()
	for _, u := range []string{"http://127.0.0.1:" + port + "/x.png", "http://[::1]:" + port + "/"} {
		if _, err := f.Fetch(context.Background(), u); !errors.Is(err, ErrBlockedAddress) {
			t.Errorf("%s 應被擋：%v", u, err)
		}
		if err := f.Check(context.Background(), u); !errors.Is(err, ErrBlockedAddress) {
			t.Errorf("Check %s 應被擋：%v", u, err)
		}
	}
	if got := n.Load(); got != 0 {
		t.Errorf("被擋的位址不應收到請求，收到 %d 次", got)
	}
}

// 正式設定下，允許的 port 也不能連到 loopback（以 Control 檢查實際連線 IP，不只看網址）。
func TestURLFetch_BlocksLoopbackOnAllowedPortCheck(t *testing.T) {
	if err := CheckPublicAddr(netip.MustParseAddrPort("127.0.0.1:80")); !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("127.0.0.1:80 應被擋：%v", err)
	}
}

func TestURLFetch_ChecksResolvedIP(t *testing.T) {
	srv, n := countingServer(t, nil)
	var mu sync.Mutex
	var checked []netip.AddrPort
	f := newURLFetcher(func(ap netip.AddrPort) error {
		mu.Lock()
		checked = append(checked, ap)
		mu.Unlock()
		return CheckPublicAddr(netip.AddrPortFrom(ap.Addr(), 443)) // 只驗 IP（port 放行），確認看到的是解析後的 IP
	})
	_, err := f.Fetch(context.Background(), "http://localhost:"+serverPort(t, srv)+"/x.png")
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("解析到 loopback 的網域名應被擋：%v", err)
	}
	if n.Load() != 0 {
		t.Error("被擋時不應收到請求")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(checked) == 0 || !checked[0].Addr().Unmap().IsLoopback() {
		t.Errorf("Control 應檢查解析後的 IP：%v", checked)
	}
}

func TestURLFetch_BlocksRedirectToPrivate(t *testing.T) {
	b, nb := countingServer(t, nil)
	a, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, b.URL+"/secret", http.StatusFound)
	})
	f := newURLFetcher(allowOnly(a))
	if _, err := f.Fetch(context.Background(), a.URL+"/x.png"); !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("轉址到不允許的位址應被擋：%v", err)
	}
	if nb.Load() != 0 {
		t.Error("轉址目標不應收到請求")
	}
	// 允許兩者時轉址可成功（確認上面是被位址檢查擋下）。
	if _, err := newURLFetcher(allowOnly(a, b)).Fetch(context.Background(), a.URL+"/x.png"); err != nil {
		t.Errorf("允許時轉址應成功：%v", err)
	}
}

func TestURLFetch_RedirectLimitAndScheme(t *testing.T) {
	var srv *httptest.Server
	srv, _ = countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/file" {
			w.Header().Set("Location", "file:///etc/passwd")
			w.WriteHeader(http.StatusFound)
			return
		}
		if r.URL.Path == "/cred" {
			w.Header().Set("Location", "http://user:pw@"+srv.Listener.Addr().String()+"/r0")
			w.WriteHeader(http.StatusFound)
			return
		}
		n, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/r"))
		if n > 0 {
			http.Redirect(w, r, fmt.Sprintf("/r%d", n-1), http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte("png"))
	})
	f := newURLFetcher(allowOnly(srv))
	if _, err := f.Fetch(context.Background(), srv.URL+"/r5"); err != nil {
		t.Errorf("5 次轉址應可以：%v", err)
	}
	if _, err := f.Fetch(context.Background(), srv.URL+"/r6"); err == nil {
		t.Error("6 次轉址應失敗")
	}
	if _, err := f.Fetch(context.Background(), srv.URL+"/file"); err == nil {
		t.Error("轉址到 file:// 應失敗")
	}
	if _, err := f.Fetch(context.Background(), srv.URL+"/cred"); err == nil {
		t.Error("轉址到帶帳密的網址應失敗")
	}
}

func TestURLFetch_RejectsBadURL(t *testing.T) {
	srv, n := countingServer(t, nil)
	addr := srv.Listener.Addr().String()
	f := newURLFetcher(allowAll)
	for _, u := range []string{"ftp://" + addr + "/x.png", "http://user:pw@" + addr + "/x.png", "http:///x.png", "javascript:alert(1)", "://bad"} {
		if _, err := f.Fetch(context.Background(), u); err == nil {
			t.Errorf("%s 應失敗", u)
		}
		if err := f.Check(context.Background(), u); err == nil {
			t.Errorf("Check %s 應失敗", u)
		}
	}
	if n.Load() != 0 {
		t.Errorf("不合法的網址不應發出請求，收到 %d 次", n.Load())
	}
}

func TestURLFetch_TooLarge(t *testing.T) {
	var written atomic.Int64
	srv, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		if r.URL.Path == "/declared" {
			w.Header().Set("Content-Length", strconv.Itoa(MaxAttachmentSize+1))
		}
		chunk := make([]byte, 1<<20)
		total := MaxAttachmentSize + 1
		for total > 0 {
			k := min(total, len(chunk))
			m, err := w.Write(chunk[:k])
			written.Add(int64(m))
			if err != nil {
				return
			}
			total -= k
		}
	})
	f := newURLFetcher(allowOnly(srv))
	if _, err := f.Fetch(context.Background(), srv.URL+"/declared"); !errors.Is(err, ErrTooLarge) {
		t.Errorf("Content-Length 超過上限應回 ErrTooLarge：%v", err)
	}
	if _, err := f.Fetch(context.Background(), srv.URL+"/stream"); !errors.Is(err, ErrTooLarge) {
		t.Errorf("串流超過上限應回 ErrTooLarge：%v", err)
	}
	srv.Close()
	// 第一個請求在讀 body 前就放棄：伺服器寫出的量（兩次合計）遠小於兩次完整 body。
	if got := written.Load(); got >= 2*int64(MaxAttachmentSize) {
		t.Errorf("Content-Length 超過時不應讀完 body：伺服器寫出 %d", got)
	}
}

func TestURLFetch_ExactlyMax(t *testing.T) {
	srv, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(make([]byte, MaxAttachmentSize))
	})
	data, err := newURLFetcher(allowOnly(srv)).Fetch(context.Background(), srv.URL)
	if err != nil || len(data) != MaxAttachmentSize {
		t.Errorf("剛好 25MiB 應可以：%d %v", len(data), err)
	}
}

func TestURLFetch_NonImageContentType(t *testing.T) {
	srv, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/html":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		case "/png":
			w.Header().Set("Content-Type", "image/png")
		case "/octet":
			w.Header().Set("Content-Type", "application/octet-stream")
		case "/none":
			w.Header()["Content-Type"] = nil // 不讓 net/http 自動判斷
		}
		w.Write([]byte("\x89PNG body"))
	})
	f := newURLFetcher(allowOnly(srv))
	if _, err := f.Fetch(context.Background(), srv.URL+"/html"); !errors.Is(err, ErrNotImage) {
		t.Errorf("text/html 應回 ErrNotImage：%v", err)
	}
	for _, p := range []string{"/png", "/octet", "/none"} {
		data, err := f.Fetch(context.Background(), srv.URL+p)
		if err != nil || string(data) != "\x89PNG body" {
			t.Errorf("%s 應回傳 body：%q %v", p, data, err)
		}
	}
}

func TestURLFetch_Non200(t *testing.T) {
	srv, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/404":
			w.WriteHeader(http.StatusNotFound)
		case "/410":
			w.WriteHeader(http.StatusGone)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	})
	f := newURLFetcher(allowOnly(srv))
	for _, p := range []string{"/404", "/410"} {
		if _, err := f.Fetch(context.Background(), srv.URL+p); !errors.Is(err, ErrImageGone) {
			t.Errorf("%s 應回 ErrImageGone：%v", p, err)
		}
	}
	if _, err := f.Fetch(context.Background(), srv.URL+"/500"); err == nil || errors.Is(err, ErrImageGone) {
		t.Errorf("500 應為一般錯誤：%v", err)
	}
}

func TestURLFetch_IgnoresProxyEnv(t *testing.T) {
	proxy, np := countingServer(t, nil)
	target, nt := countingServer(t, nil)
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("http_proxy", proxy.URL)
	f := newURLFetcher(allowOnly(target))
	if _, err := f.Fetch(context.Background(), target.URL+"/x.png"); err != nil {
		t.Fatal(err)
	}
	if np.Load() != 0 || nt.Load() != 1 {
		t.Errorf("不應經過代理：proxy %d target %d", np.Load(), nt.Load())
	}
	// 環境變數的代理設定會被 net/http 快取、且不套用到 127.0.0.1，所以另外確認 Transport 沒有代理。
	tr, ok := f.client.Transport.(*http.Transport)
	if !ok || tr.Proxy != nil {
		t.Errorf("Transport 的 Proxy 必須為 nil")
	}
	if f.client.Timeout <= 0 || tr.ResponseHeaderTimeout <= 0 {
		t.Errorf("應有逾時：client %v header %v", f.client.Timeout, tr.ResponseHeaderTimeout)
	}
}

func TestURLFetch_UserAgentAndContext(t *testing.T) {
	var ua atomic.Value
	srv, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		ua.Store(r.UserAgent())
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte("png"))
	})
	f := newURLFetcher(allowOnly(srv))
	if _, err := f.Fetch(context.Background(), srv.URL); err != nil {
		t.Fatal(err)
	}
	if ua.Load() != urlFetchUserAgent {
		t.Errorf("User-Agent = %v", ua.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.Fetch(ctx, srv.URL); !errors.Is(err, context.Canceled) {
		t.Errorf("ctx 取消應回錯誤：%v", err)
	}
}

// Check（/search 貼網址前確認圖片還在）：只讀標頭與少量內容。
func TestURLCheck(t *testing.T) {
	var written atomic.Int64
	srv, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/png":
			w.Header().Set("Content-Type", "image/png")
			chunk := make([]byte, 1<<20)
			for i := 0; i < 20; i++ {
				m, err := w.Write(chunk)
				written.Add(int64(m))
				if err != nil {
					return
				}
			}
		case "/404":
			w.WriteHeader(http.StatusNotFound)
		case "/410":
			w.WriteHeader(http.StatusGone)
		case "/500":
			w.WriteHeader(http.StatusInternalServerError)
		case "/html":
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("<html>removed</html>"))
		case "/octet-html":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write([]byte("<!DOCTYPE html><html>removed</html>"))
		case "/octet-png":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write([]byte("\x89PNG\r\n\x1a\n...."))
		}
	})
	f := newURLFetcher(allowOnly(srv))
	cases := map[string]error{
		"/png": nil, "/octet-png": nil,
		"/404": ErrImageGone, "/410": ErrImageGone,
		"/html": ErrNotImage, "/octet-html": ErrNotImage,
	}
	for p, want := range cases {
		err := f.Check(context.Background(), srv.URL+p)
		if want == nil && err != nil || want != nil && !errors.Is(err, want) {
			t.Errorf("Check %s = %v，預期 %v", p, err, want)
		}
	}
	if err := f.Check(context.Background(), srv.URL+"/500"); err == nil || errors.Is(err, ErrImageGone) || errors.Is(err, ErrNotImage) {
		t.Errorf("500 應為一般錯誤：%v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := written.Load(); got >= 20<<20 {
		t.Errorf("Check 不應讀完整個圖片：伺服器寫出 %d", got)
	}
}

// PM 決定：imgur 已刪除的圖會轉址到 i.imgur.com/removed.png（200），視為 ErrImageGone。
func TestIsImgurRemoved(t *testing.T) {
	yes := []string{"https://i.imgur.com/removed.png", "https://I.IMGUR.COM/Removed.PNG", "http://i.imgur.com/removed.png?x=1", "https://i.imgur.com:443/removed.png"}
	no := []string{"https://i.imgur.com/abc.png", "https://imgur.com/removed.png", "https://evil.com/removed.png", "https://i.imgur.com/a/removed.png"}
	for _, s := range yes {
		u, _ := url.Parse(s)
		if !isImgurRemoved(u) {
			t.Errorf("%s 應視為已刪除", s)
		}
	}
	for _, s := range no {
		u, _ := url.Parse(s)
		if isImgurRemoved(u) {
			t.Errorf("%s 不應視為已刪除", s)
		}
	}
	if NewURLFetcher().gone == nil {
		t.Error("正式下載器應檢查 imgur 已刪除的圖")
	}
}

// 轉址後的最終網址被判定為已刪除 → Fetch、Check 都回 ErrImageGone（測試以可注入的比對代替 i.imgur.com）。
func TestURLFetch_RedirectToRemovedImage(t *testing.T) {
	srv, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/removed.png" {
			w.Header().Set("Content-Type", "image/png")
			w.Write([]byte("\x89PNG removed"))
			return
		}
		http.Redirect(w, r, "/removed.png", http.StatusFound)
	})
	f := newURLFetcher(allowOnly(srv))
	f.gone = func(u *url.URL) bool {
		return u.Host == srv.Listener.Addr().String() && strings.EqualFold(u.Path, "/removed.png")
	}
	if _, err := f.Fetch(context.Background(), srv.URL+"/abc.jpg"); !errors.Is(err, ErrImageGone) {
		t.Errorf("Fetch 應回 ErrImageGone：%v", err)
	}
	if err := f.Check(context.Background(), srv.URL+"/abc.jpg"); !errors.Is(err, ErrImageGone) {
		t.Errorf("Check 應回 ErrImageGone：%v", err)
	}
}
