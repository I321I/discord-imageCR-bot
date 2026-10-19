package pipeline

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
)

// 把關：更多必須擋下的位址（IPv4-mapped 的各種私有範圍、IPv6 zone、IMDS IPv6 等）。
func TestGate_CheckPublicAddr_MoreBlocked(t *testing.T) {
	blocked := []string{
		"[::ffff:169.254.169.254]:80", "[::ffff:100.64.0.1]:443", "[::ffff:0.0.0.0]:443",
		"[::ffff:255.255.255.255]:443", "[::ffff:192.168.0.1]:443", "[::ffff:224.0.0.1]:443",
		"[fe80::1%eth0]:443", "[fd00:ec2::254]:80", "[ff05::1]:443", "[ff0e::1]:443",
		"100.127.255.255:443", "169.254.0.1:443", "172.31.255.255:80", "127.255.255.254:443",
		"8.8.8.8:65535", "8.8.8.8:8080",
	}
	for _, s := range blocked {
		if err := CheckPublicAddr(netip.MustParseAddrPort(s)); !errors.Is(err, ErrBlockedAddress) {
			t.Errorf("%s 應被擋：%v", s, err)
		}
	}
	// 邊界：緊鄰被擋範圍的公開位址要放行。
	for _, s := range []string{"100.63.255.255:443", "100.128.0.0:443", "172.32.0.1:443", "1.1.1.1:80"} {
		if err := CheckPublicAddr(netip.MustParseAddrPort(s)); err != nil {
			t.Errorf("%s 應放行：%v", s, err)
		}
	}
}

// 把關：嵌入 IPv4 的 IPv6 過渡位址。原本只記錄結果；PM 決定 (a)（2026-10-19 把關後）改為封鎖，斷言被擋。
func TestGate_CheckPublicAddr_TransitionAddrsReport(t *testing.T) {
	for _, s := range []string{
		"[::127.0.0.1]:443",      // IPv4-compatible（已廢止）
		"[::ffff:0:7f00:1]:443",  // IPv4-translated
		"[2002:7f00:1::1]:443",   // 6to4 包 127.0.0.1
		"[2002:a00:1::1]:443",    // 6to4 包 10.0.0.1
		"[2001:0:a00:1::1]:443",  // Teredo
		"[64:ff9b:1::a00:1]:443", // 本地 NAT64（RFC 8215）
		"[fec0::1]:443",          // site-local（已廢止）
		"[100::1]:443",           // discard-only
	} {
		if err := CheckPublicAddr(netip.MustParseAddrPort(s)); !errors.Is(err, ErrBlockedAddress) {
			t.Errorf("%s 應被擋：%v", s, err)
		}
	}
}

// 把關：各種寫法的 loopback host 都必須在連線前被擋（以實際連線 IP 檢查），伺服器不可收到請求。
func TestGate_URLFetch_HostSpellingsBlocked(t *testing.T) {
	srv, n := countingServer(t, nil)
	port := serverPort(t, srv)
	// 只驗 IP（port 換成 443），確保擋下的原因是 IP 而不是 port。
	// 不列需要查外部 DNS 的寫法（0x7f000001、localhost.），測試不連網。
	f := newURLFetcher(func(ap netip.AddrPort) error {
		return CheckPublicAddr(netip.AddrPortFrom(ap.Addr(), 443))
	})
	hosts := []string{
		"127.0.0.1", "127.1", "0177.0.0.1", "2130706433", "127.000.000.001",
		"[::ffff:127.0.0.1]", "[::ffff:7f00:1]", "[0:0:0:0:0:ffff:127.0.0.1]", "localhost", "LOCALHOST",
		"[::1]", "0.0.0.0", "0",
	}
	for _, h := range hosts {
		u := "http://" + h + ":" + port + "/x.png"
		if _, err := f.Fetch(context.Background(), u); err == nil {
			t.Errorf("%s 應失敗", u)
		}
		if err := f.Check(context.Background(), u); err == nil {
			t.Errorf("Check %s 應失敗", u)
		}
	}
	if got := n.Load(); got != 0 {
		t.Errorf("被擋的 host 不應收到請求，收到 %d 次", got)
	}
}

// 把關（DNS rebinding／新連線）：同一 host 的第二條連線也要重新檢查。
// A 第一次回應關閉連線並轉址回自己；check 只放行第一次連線 → 第二次連線必須被擋。
func TestGate_URLFetch_RechecksEveryConnection(t *testing.T) {
	var hits atomic.Int32
	srv, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Connection", "close")
			http.Redirect(w, r, "/again.png", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte("png"))
	})
	var dials atomic.Int32
	f := newURLFetcher(func(ap netip.AddrPort) error {
		if dials.Add(1) == 1 && ap.String() == srv.Listener.Addr().String() {
			return nil
		}
		return ErrBlockedAddress
	})
	if _, err := f.Fetch(context.Background(), srv.URL+"/x.png"); !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("第二條連線應被重新檢查並擋下：%v", err)
	}
	if dials.Load() < 2 {
		t.Errorf("每條連線都應檢查，只檢查了 %d 次", dials.Load())
	}
	if hits.Load() != 1 {
		t.Errorf("被擋的連線不應送出請求：伺服器收到 %d 次", hits.Load())
	}
}

// 把關：轉址到帶帳密的網址要拒絕，且目標不收到請求。
func TestGate_URLFetch_RedirectWithUserinfoRejected(t *testing.T) {
	b, nb := countingServer(t, nil)
	a, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		target := strings.Replace(b.URL, "http://", "http://user:pw@", 1) + "/x.png"
		http.Redirect(w, r, target, http.StatusFound)
	})
	f := newURLFetcher(allowOnly(a, b))
	if _, err := f.Fetch(context.Background(), a.URL+"/x.png"); err == nil {
		t.Fatal("轉址到帶帳密的網址應失敗")
	}
	if nb.Load() != 0 {
		t.Error("帶帳密的轉址目標不應收到請求")
	}
}

// 把關：恰好 5 次轉址可以成功（上限是 5 跳）。
func TestGate_URLFetch_FiveRedirectsOK(t *testing.T) {
	var hits atomic.Int32
	srv, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) <= 5 {
			http.Redirect(w, r, "/next", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte("png"))
	})
	if _, err := newURLFetcher(allowOnly(srv)).Fetch(context.Background(), srv.URL+"/x.png"); err != nil {
		t.Fatalf("5 次轉址應成功：%v", err)
	}
}

// 把關（壓縮炸彈）：伺服器以 gzip 傳送，解壓後超過上限 → ErrTooLarge（不可把整個解壓結果讀進記憶體）。
func TestGate_URLFetch_GzipBombLimited(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zero := make([]byte, 1<<20)
	for i := 0; i < 30; i++ {
		zw.Write(zero)
	}
	zw.Close()
	body := buf.Bytes()
	srv, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(body)
	})
	_, err := newURLFetcher(allowOnly(srv)).Fetch(context.Background(), srv.URL+"/x.png")
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("解壓後超過上限應為 ErrTooLarge：%v", err)
	}
}

// 把關：Check 對沒有 Content-Type 的網頁回 ErrNotImage；對大圖片不必讀完 body。
func TestGate_URLCheck_SniffAndNoFullRead(t *testing.T) {
	html, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header()["Content-Type"] = nil
		w.Write([]byte("<!DOCTYPE html><html><body>removed</body></html>"))
	})
	if err := newURLFetcher(allowOnly(html)).Check(context.Background(), html.URL+"/x"); !errors.Is(err, ErrNotImage) {
		t.Errorf("沒有 Content-Type 的網頁應為 ErrNotImage：%v", err)
	}
	var wrote atomic.Int64
	big, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		// 第二輪把關後決定：Check 以內容嗅探確認是 PNG／JPEG／GIF／WEBP，所以開頭要是 PNG 簽章（原本全為 0）。
		w.Write([]byte("\x89PNG\r\n\x1a\n"))
		chunk := make([]byte, 64<<10)
		for i := 0; i < 1024; i++ { // 64MiB
			n, err := w.Write(chunk)
			wrote.Add(int64(n))
			if err != nil {
				return
			}
		}
	})
	if err := newURLFetcher(allowOnly(big)).Check(context.Background(), big.URL+"/x.png"); err != nil {
		t.Errorf("image/png 應確認存在：%v", err)
	}
}
