package pipeline

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 把關第四輪：再嘗試以 Content-Type 變形繞過「只有確定是點陣圖才回 nil」。
// 內容一律以 GIF 簽章開頭（嗅探會得到 image/gif），所以只有 Content-Type 檢查能擋下。
// 會讓瀏覽器以 HTML／XHTML 解析的宣告都不可回 nil。
func TestGateR4_URLCheck_ContentTypeVariantsNotConfirmed(t *testing.T) {
	const body = "GIF89a<html><script>location='https://phish.example/'</script></html>"
	cases := []struct {
		name string
		cts  []string // 每個元素是一個 Content-Type 標頭
	}{
		{"大寫 TEXT/HTML", []string{"TEXT/HTML"}},
		{"前後空白", []string{"  text/html  "}},
		{"單標頭逗號 image 在前", []string{"image/gif;charset=utf-8, text/html"}},
		{"單標頭逗號 html 在前", []string{"text/html, image/gif"}},
		{"引號內逗號", []string{`image/gif; a="x,y", text/html`}},
		{"多標頭 + 參數", []string{"image/gif; charset=x", "text/html; charset=utf-8"}},
		{"xhtml 大寫", []string{"Application/XHTML+XML"}},
		{"image/gif 後接空白與 html", []string{"image/gif text/html"}},
		{"分號後 html", []string{"image/gif;text/html"}},
		{"*/* （瀏覽器會嗅探）", []string{"*/*"}},
		{"unknown/unknown", []string{"unknown/unknown"}},
		{"application/unknown", []string{"application/unknown"}},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header()["Content-Type"] = c.cts
			w.Write([]byte(body))
		}))
		err := newURLFetcher(allowOnly(srv)).Check(context.Background(), srv.URL+"/cute.gif")
		if err == nil {
			t.Errorf("%s（%q）：Check 回 nil", c.name, c.cts)
		}
		srv.Close()
	}
}

// 加上 X-Content-Type-Options、Content-Disposition inline 等其他標頭不影響判斷：
// image/gif + GIF 簽章仍回 nil（瀏覽器依 image/gif 顯示圖片，不執行 script）。
func TestGateR4_URLCheck_OtherHeadersRasterStillOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/gif")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Disposition", "inline")
		w.Write([]byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;"))
	}))
	defer srv.Close()
	if err := newURLFetcher(allowOnly(srv)).Check(context.Background(), srv.URL+"/a.gif"); err != nil {
		t.Errorf("image/gif + 其他標頭應回 nil：%v", err)
	}
}

// 轉址鏈中途的 Content-Type（text/html）不影響：只看最終回應；最終為 image/gif → nil。
// 最終為 text/html → 不可為 nil。
func TestGateR4_URLCheck_RedirectChainUsesFinal(t *testing.T) {
	for _, final := range []string{"image/gif", "text/html"} {
		mux := http.NewServeMux()
		mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			http.Redirect(w, r, "/end", http.StatusFound)
		})
		mux.HandleFunc("/end", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", final)
			w.Write([]byte("GIF89a<script>alert(1)</script>"))
		})
		srv := httptest.NewServer(mux)
		err := newURLFetcher(allowOnly(srv)).Check(context.Background(), srv.URL+"/start")
		if final == "image/gif" && err != nil {
			t.Errorf("最終 image/gif 應回 nil：%v", err)
		}
		if final == "text/html" && err == nil {
			t.Errorf("最終 text/html 不可回 nil")
		}
		srv.Close()
	}
}

// looksLikeSVG：octet-stream 下，大小寫與 XML 宣告變形歸為無法確認（使用者決定 (b)）；
// HTML 仍為確定不是圖片。
func TestGateR4_URLCheck_SVGVariants(t *testing.T) {
	unconfirmed := map[string]string{
		"大寫 <SVG":         `<SVG xmlns="http://www.w3.org/2000/svg"></SVG>`,
		"UTF-8 BOM":       "\ufeff<svg xmlns=\"http://www.w3.org/2000/svg\"></svg>",
		"前導換行":            "\n\n  <svg></svg>",
		"<?XML 大寫":        `<?XML version="1.0"?><svg></svg>`,
		"XML 宣告後有註解再 svg": `<?xml version="1.0"?><!-- c --><svg></svg>`,
	}
	for name, body := range unconfirmed {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write([]byte(body))
		}))
		err := newURLFetcher(allowOnly(srv)).Check(context.Background(), srv.URL+"/a.svg")
		if !errors.Is(err, ErrUnconfirmedImage) {
			t.Errorf("SVG %s：應為 ErrUnconfirmedImage，得到 %v", name, err)
		}
		srv.Close()
	}
	notImage := map[string]string{
		"HTML":         "<!doctype html><html><body>phish</body></html>",
		"HTML 內含 <svg": "<html><svg></svg></html>",
	}
	for name, body := range notImage {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header()["Content-Type"] = nil
			w.Write([]byte(body))
		}))
		err := newURLFetcher(allowOnly(srv)).Check(context.Background(), srv.URL+"/a.gif")
		if !errors.Is(err, ErrNotImage) {
			t.Errorf("%s：應為 ErrNotImage，得到 %v", name, err)
		}
		srv.Close()
	}
}

// 記錄現況（非必修，供 PM 判斷）：以下情況 Check 回 nil，/search 會不看預覽直接公開貼出。
// 這些都不會讓瀏覽器在該網址執行 script，但可能被用於釣魚（見回報的待決定問題）。
func TestGateR4_URLCheck_DocumentedNilCases(t *testing.T) {
	cases := []struct {
		name string
		hdr  map[string]string
	}{
		{"Refresh 標頭轉到釣魚頁", map[string]string{"Content-Type": "image/gif", "Refresh": "0; url=https://phish.example/"}},
		{"Content-Disposition attachment 檔名 .html", map[string]string{"Content-Type": "image/gif", "Content-Disposition": `attachment; filename="login.html"`}},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for k, v := range c.hdr {
				w.Header().Set(k, v)
			}
			w.Write([]byte("GIF89a<html><script>alert(1)</script></html>"))
		}))
		err := newURLFetcher(allowOnly(srv)).Check(context.Background(), srv.URL+"/a.gif")
		t.Logf("%s：Check = %v", c.name, err)
		srv.Close()
	}
}
