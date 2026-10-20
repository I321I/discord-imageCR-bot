package pipeline

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// 把關（最後一批）：Refresh／Content-Disposition 檢查的繞過嘗試，以及 Check 與 Fetch 分工。
func TestGateR5_URLCheck_HeaderBypasses(t *testing.T) {
	gif := []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;")
	cases := []struct {
		name string
		hdr  map[string][]string
		want error // nil 表示 Check 應成功
	}{
		{"filename=a.png.html", map[string][]string{"Content-Disposition": {"attachment;filename=a.png.html"}}, ErrUnconfirmedImage},
		{"filename 帶路徑 ../a.html", map[string][]string{"Content-Disposition": {`attachment; filename="../x.png/a.html"`}}, ErrUnconfirmedImage},
		{"filename 結尾為點 a.png.", map[string][]string{"Content-Disposition": {`attachment; filename="a.png."`}}, ErrUnconfirmedImage},
		{"重複 filename 參數", map[string][]string{"Content-Disposition": {`attachment; filename="a.png"; filename="a.html"`}}, ErrUnconfirmedImage},
		{"Attachment 混合大小寫", map[string][]string{"Content-Disposition": {`AtTaChMeNt; FILENAME="a.htm"`}}, ErrUnconfirmedImage},
		{"attachment 加引號（無法解析）", map[string][]string{"Content-Disposition": {`"attachment"; filename=a.png`}}, ErrUnconfirmedImage},
		{"第二個 Content-Disposition 才是 attachment", map[string][]string{"Content-Disposition": {`inline; filename="a.png"`, `attachment; filename="a.svg"`}}, ErrUnconfirmedImage},
		{"Refresh 多個值", map[string][]string{"Refresh": {"0;url=https://evil.example/", "1"}}, ErrUnconfirmedImage},
		{"Content-Encoding 多值含 identity 與 gzip（未解壓）", map[string][]string{"Content-Encoding": {"identity", "br"}}, ErrUnconfirmedImage},
		{"attachment filename=a.jpeg", map[string][]string{"Content-Disposition": {`attachment; filename=a.jpeg`}}, nil},
	}
	for _, c := range cases {
		srv, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/gif")
			for k, v := range c.hdr {
				w.Header()[k] = v
			}
			w.Write(gif)
		})
		err := newURLFetcher(allowOnly(srv)).Check(context.Background(), srv.URL+"/a.gif")
		if c.want == nil && err != nil || c.want != nil && !errors.Is(err, c.want) {
			t.Errorf("%s：Check = %v，預期 %v", c.name, err, c.want)
		}
	}
}

// 記錄現況（非必修）：Go 只解碼 UTF-8／US-ASCII 的 filename*；其他字元集時退回 filename，
// 瀏覽器可能採用 filename*。本體仍須嗅探為點陣圖才回 nil，所以只影響「下載時的檔名」。
func TestGateR5_URLCheck_NonUTF8FilenameStar(t *testing.T) {
	srv, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/gif")
		w.Header().Set("Content-Disposition", `attachment; filename="a.gif"; filename*=iso-8859-1''a.html`)
		w.Write([]byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;"))
	})
	// 原本只記錄（Go 不支援 iso-8859-1 的 filename*，會略過它而採用 filename="a.gif"）；
	// 最後一條決定：含 filename* 但無法解析時，不論 filename 是什麼都歸為無法確認，改為斷言。
	err := newURLFetcher(allowOnly(srv)).Check(context.Background(), srv.URL+"/a.gif")
	if !errors.Is(err, ErrUnconfirmedImage) {
		t.Errorf("filename*=iso-8859-1''a.html：應為 ErrUnconfirmedImage，得到 %v", err)
	}
}

// Refresh 搭配網頁內容：仍為 ErrNotImage（get 先擋，確定不是圖片）。
func TestGateR5_URLCheck_RefreshWithHTMLIsNotImage(t *testing.T) {
	srv, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Refresh", "0;url=https://evil.example/")
		w.Write([]byte("<html></html>"))
	})
	if err := newURLFetcher(allowOnly(srv)).Check(context.Background(), srv.URL+"/a.png"); !errors.Is(err, ErrNotImage) {
		t.Errorf("應為 ErrNotImage：%v", err)
	}
}

// Fetch 只放行 ErrUnconfirmedImage；Content-Type 確定不是圖片（text/html、image/svg+xml 以外的非圖片類）仍失敗，
// 且 Fetch 不受 Content-Encoding 規則影響（未解壓資料交給解碼，解碼失敗）。
func TestGateR5_URLFetch_NotImageStillFails(t *testing.T) {
	for _, ct := range [][]string{{"text/html"}, {"text/html", "application/json"}, {"text/html, text/plain"}} {
		srv, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header()["Content-Type"] = ct
			w.Write([]byte("\x89PNG\r\n\x1a\n"))
		})
		if _, err := newURLFetcher(allowOnly(srv)).Fetch(context.Background(), srv.URL+"/a.png"); !errors.Is(err, ErrNotImage) {
			t.Errorf("Content-Type %q：Fetch 應為 ErrNotImage：%v", ct, err)
		}
	}
}
