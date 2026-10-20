package pipeline

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// 把關第 6 輪：validFilenameStar 對怪輸入不 panic。
func TestGateR6_ValidFilenameStar_NoPanic(t *testing.T) {
	inputs := []string{"", ";", ";;;;", "=", "filename*", "filename*=", "filename*='", "filename*=''", "filename*='''",
		"filename*=UTF-8''", "filename*=UTF-8''%", "filename*=UTF-8''%2", "\x00;\xff=\xfe", "attachment; filename*=é''x",
		strings.Repeat(";filename*=UTF-8''a", 10000), strings.Repeat("%", 100000), "filename*=UTF-8'" + strings.Repeat("'", 1000)}
	for _, in := range inputs {
		_ = validFilenameStar(in)
		_ = checkPageHeaders(&http.Response{Header: http.Header{"Content-Disposition": {in}}, Uncompressed: true})
	}
}

// 正常圖片不得被誤判。
func TestGateR6_NormalImagesConfirmed(t *testing.T) {
	gifBody := []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;")
	cds := []string{"-", `inline`, `inline; filename="a.gif"`, `inline; filename*=UTF-8''%E5%9C%96.gif`,
		`attachment; filename="a.GIF"`, `attachment; filename=a.jpeg`, `attachment; filename*=utf-8''%E5%9C%96.webp`,
		`attachment; filename="x.png"; filename*=UTF-8''x.png`}
	for _, cd := range cds {
		srv, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/gif")
			if cd != "-" {
				w.Header().Set("Content-Disposition", cd)
			}
			w.Write(gifBody)
		})
		if err := newURLFetcher(allowOnly(srv)).Check(context.Background(), srv.URL+"/a.gif"); err != nil {
			t.Errorf("%q：正常圖片被判為 %v", cd, err)
		}
		if data, err := newURLFetcher(allowOnly(srv)).Fetch(context.Background(), srv.URL+"/a.gif"); err != nil || len(data) == 0 {
			t.Errorf("%q：Fetch 應成功：%v", cd, err)
		}
	}
	// Fetch 不受 filename* 新規則影響。
	srv, _ := countingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/gif")
		w.Header().Set("Content-Disposition", `x-attachment; filename*=iso-8859-1''a.html`)
		w.Write(gifBody)
	})
	if _, err := newURLFetcher(allowOnly(srv)).Fetch(context.Background(), srv.URL+"/a.gif"); err != nil {
		t.Errorf("Fetch 不應受影響：%v", err)
	}
}
