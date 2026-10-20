package pipeline

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 把關第二輪：/search 在 Check 成功時不看 Discord 預覽、直接公開貼出網址。
// Check 只要 Content-Type 是 image/* 就算成功，所以 image/svg+xml（瀏覽器直接開啟時會執行其中的 script、
// 可做成登入頁或轉址）會被當成「確認是圖片」；application/octet-stream 加上非文字內容（例如 PDF）也會。
// 這兩種都可偽裝成 .jpg 網址，繞過「只有 Discord 圖片預覽才貼出」的釣魚防護（使用者決定的目的）。
// 預期：Check 對非點陣圖（PNG／JPEG／GIF／WEBP 以外）不可回 nil。
func TestGateR2_URLCheck_NonRasterNotConfirmed(t *testing.T) {
	const svg = `<svg xmlns="http://www.w3.org/2000/svg"><script>location="https://phish.example/login"</script></svg>`
	cases := []struct {
		name, ct string
		body     []byte
	}{
		{"image/svg+xml 帶 script", "image/svg+xml", []byte(svg)},
		{"octet-stream 的 PDF", "application/octet-stream", []byte("%PDF-1.7\n1 0 obj\n<<>>\nendobj\n")},
		{"沒有 Content-Type 的 PDF", "", []byte("%PDF-1.7\n1 0 obj\n<<>>\nendobj\n")},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if c.ct == "" {
				w.Header()["Content-Type"] = nil // 不讓 net/http 自動補
			} else {
				w.Header().Set("Content-Type", c.ct)
			}
			w.Write(c.body)
		}))
		err := newURLFetcher(allowOnly(srv)).Check(context.Background(), srv.URL+"/cute.jpg")
		if err == nil {
			t.Errorf("%s：Check 回 nil（視為確認是圖片，/search 會直接公開貼出）", c.name)
		} else {
			t.Logf("%s：%v", c.name, err)
		}
		srv.Close()
	}
}

// 對照組：真正的點陣圖照常確認成功（修正時不可誤擋）。
func TestGateR2_URLCheck_RasterConfirmed(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	for _, ct := range []string{"image/png", "application/octet-stream", ""} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ct == "" {
				w.Header()["Content-Type"] = nil
			} else {
				w.Header().Set("Content-Type", ct)
			}
			w.Write(png)
		}))
		if err := newURLFetcher(allowOnly(srv)).Check(context.Background(), srv.URL+"/a.png"); err != nil {
			t.Errorf("Content-Type %q 的 PNG 應確認成功：%v", ct, err)
		}
		srv.Close()
	}
}
