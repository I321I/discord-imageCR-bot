package pipeline

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 把關第三輪：Check 先看內容嗅探、只要開頭是點陣圖簽章就回 nil，完全不看 Content-Type。
// 伺服器以 Content-Type text/html（或 xhtml）送出「GIF89a／PNG 簽章 + HTML」的 polyglot 時，
// 瀏覽器依 Content-Type 以 HTML 解析並執行其中的 script（text/html 不做內容嗅探），
// 但 Check 會當成「確定是圖片」，/search 不看 Discord 預覽就公開貼出——繞過防釣魚規則。
// 預期：Content-Type 有值且不是 image/*、也不是 octet-stream 時，不可回 nil。
func TestGateR3_URLCheck_HTMLPolyglotNotConfirmed(t *testing.T) {
	const script = `<html><script>location="https://phish.example/login"</script></html>`
	cases := []struct {
		name, ct, body string
	}{
		{"text/html + GIF89a", "text/html; charset=utf-8", "GIF89a" + script},
		{"text/html + PNG 簽章", "text/html", "\x89PNG\r\n\x1a\n" + script},
		{"application/xhtml+xml + GIF87a", "application/xhtml+xml", "GIF87a" + script},
		{"text/html + WEBP 簽章", "text/html", "RIFF\x00\x00\x00\x00WEBPVP" + script},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", c.ct)
			w.Write([]byte(c.body))
		}))
		err := newURLFetcher(allowOnly(srv)).Check(context.Background(), srv.URL+"/cute.gif")
		if err == nil {
			t.Errorf("%s：Check 回 nil（/search 會不看預覽直接公開貼出可執行 script 的網頁）", c.name)
		}
		srv.Close()
	}
}

// 使用者決定：SVG（含 octet-stream／沒有 Content-Type 但內容不是四種點陣圖）歸為「無法確認」，不移除索引。
// 目前 SVG 內容被 DetectContentType 判為 text/xml 或 text/plain → ErrNotImage → /search 移除索引並回「已被刪除」。
func TestGateR3_URLCheck_SVGOctetStreamIsUnconfirmed(t *testing.T) {
	svgs := map[string]string{
		"有 XML 宣告": `<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"></svg>`,
		"直接 <svg":  `<svg xmlns="http://www.w3.org/2000/svg"><rect/></svg>`,
	}
	for name, body := range svgs {
		for _, ct := range []string{"application/octet-stream", ""} {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if ct == "" {
					w.Header()["Content-Type"] = nil
				} else {
					w.Header().Set("Content-Type", ct)
				}
				w.Write([]byte(body))
			}))
			err := newURLFetcher(allowOnly(srv)).Check(context.Background(), srv.URL+"/a.svg")
			if errors.Is(err, ErrNotImage) || errors.Is(err, ErrImageGone) || !errors.Is(err, ErrUnconfirmedImage) {
				t.Errorf("SVG %s、Content-Type %q：應為 ErrUnconfirmedImage（無法確認），得到 %v", name, ct, err)
			}
			srv.Close()
		}
	}
}

// 對照組：image/* 宣告下的 polyglot（瀏覽器當圖片顯示、不執行 script）與 image/svg+xml 皆不可回 nil 以外的「確定刪除」。
func TestGateR3_URLCheck_ImageCTPolyglotNotGone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Write([]byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`))
	}))
	defer srv.Close()
	err := newURLFetcher(allowOnly(srv)).Check(context.Background(), srv.URL+"/a.svg")
	if !errors.Is(err, ErrUnconfirmedImage) {
		t.Errorf("image/svg+xml 應為 ErrUnconfirmedImage：%v", err)
	}
}

// 重複的 Content-Type 標頭：Go 的 Header.Get 取第一個（image/png），但瀏覽器依 Fetch 標準
// （extract a MIME type）取最後一個有效值（text/html）並以 HTML 執行 script。
// 內容開頭放 GIF 簽章 → 嗅探為 image/gif → Check 回 nil → /search 不看預覽直接公開貼出。
// 預期：有多個 Content-Type、或任一個不是 image/*／octet-stream 時不可回 nil。
func TestGateR3_URLCheck_DuplicateContentTypeNotConfirmed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header()["Content-Type"] = []string{"image/gif", "text/html"}
		w.Write([]byte(`GIF89a<html><script>location="https://phish.example/login"</script></html>`))
	}))
	defer srv.Close()
	if err := newURLFetcher(allowOnly(srv)).Check(context.Background(), srv.URL+"/cute.gif"); err == nil {
		t.Errorf("重複 Content-Type（最後一個是 text/html）時 Check 回 nil：瀏覽器會以 HTML 開啟")
	}
}
