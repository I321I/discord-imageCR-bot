package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
	"unicode"
)

var (
	// ErrBlockedAddress 表示連線目標不是公開位址，或連接埠不是 80、443（U10）。
	ErrBlockedAddress = errors.New("blocked address")
	// ErrNotImage 表示網址的內容不是圖片（Content-Type 不是圖片，或無法解碼成支援的格式）。
	ErrNotImage = errors.New("not an image")
	// ErrImageGone 表示圖床回應 404 或 410（圖片已被刪除）。
	ErrImageGone = errors.New("image gone")
)

const (
	// urlFetchUserAgent 是網址下載的固定 User-Agent。
	urlFetchUserAgent = "discord-imageCR-bot"
	// urlMaxRedirects 是網址下載最多跟隨的轉址次數。
	urlMaxRedirects = 5
	// urlSniffBytes 是 Check 最多讀取的內容長度（Content-Type 不明時判斷是否為網頁）。
	urlSniffBytes = 512
)

// blockedPrefixes 是 netip 的 Is* 方法沒有涵蓋、但不可連線的位址範圍。
var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"), // 含 255.255.255.255
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("2001:db8::/32"),
	// IPv6 過渡位址（內嵌 IPv4，可能繞過 IPv4 檢查；PM 決定整段封鎖）。
	netip.MustParsePrefix("::/96"),           // IPv4-compatible（已廢止）
	netip.MustParsePrefix("::ffff:0:0:0/96"), // IPv4-translated
	netip.MustParsePrefix("2002::/16"),       // 6to4
	netip.MustParsePrefix("2001::/32"),       // Teredo
	netip.MustParsePrefix("64:ff9b:1::/48"),  // 本地 NAT64
	netip.MustParsePrefix("fec0::/10"),       // site-local（已廢止）
	netip.MustParsePrefix("100::/64"),        // discard-only
}

// CheckPublicAddr 回傳 ap 是否可連線：只允許公開位址的 80、443 連接埠；不可時回包含 ErrBlockedAddress 的錯誤。
// IPv4-mapped IPv6 先轉回 IPv4 再檢查。
func CheckPublicAddr(ap netip.AddrPort) error {
	if p := ap.Port(); p != 80 && p != 443 {
		return fmt.Errorf("%w：連接埠 %d", ErrBlockedAddress, p)
	}
	a := ap.Addr().Unmap()
	if !a.IsValid() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsInterfaceLocalMulticast() || a.IsMulticast() || a.IsUnspecified() {
		return fmt.Errorf("%w：%s", ErrBlockedAddress, a)
	}
	for _, p := range blockedPrefixes {
		if p.Contains(a) {
			return fmt.Errorf("%w：%s", ErrBlockedAddress, a)
		}
	}
	return nil
}

// URLFetcher 下載使用者貼的圖片網址（防 SSRF）；滿足 Fetcher。
// 每次實際連線（含轉址與新連線）都以連線當下的 IP 檢查（防 DNS rebinding），不使用代理伺服器。
type URLFetcher struct {
	client   *http.Client
	maxBytes int64
	gone     func(u *url.URL) bool // 最終網址（轉址後）代表圖片已被刪除時回 true（測試可替換）
}

// NewURLFetcher 回傳正式用的 URLFetcher：只連公開位址的 80、443，上限 25MiB，逾時 2 分鐘。
func NewURLFetcher() *URLFetcher {
	return newURLFetcher(CheckPublicAddr)
}

// newURLFetcher 以 check 檢查每次連線的位址（測試用：可放行 httptest 的 127.0.0.1:port）。
func newURLFetcher(check func(netip.AddrPort) error) *URLFetcher {
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return fmt.Errorf("%w：無法解析位址 %q", ErrBlockedAddress, address)
			}
			return check(ap)
		},
	}
	tr := &http.Transport{
		Proxy:                  nil, // 不用代理：否則檢查到的是代理伺服器的 IP
		DialContext:            dialer.DialContext,
		TLSHandshakeTimeout:    10 * time.Second,
		ResponseHeaderTimeout:  30 * time.Second,
		MaxResponseHeaderBytes: 64 << 10,
		IdleConnTimeout:        30 * time.Second,
		ForceAttemptHTTP2:      true,
	}
	client := &http.Client{
		Transport: tr,
		Timeout:   2 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > urlMaxRedirects {
				return fmt.Errorf("轉址超過 %d 次", urlMaxRedirects)
			}
			return checkWebURL(req.URL)
		},
	}
	return &URLFetcher{client: client, maxBytes: MaxAttachmentSize, gone: isImgurRemoved}
}

// isImgurRemoved 回傳 u 是否為 imgur 已刪除圖片的替代圖（i.imgur.com/removed.png，不分大小寫，忽略 query）：
// imgur 對已刪除的圖會轉址到這張圖並回 200（PM 決定）。
func isImgurRemoved(u *url.URL) bool {
	return strings.EqualFold(u.Hostname(), "i.imgur.com") && strings.EqualFold(u.Path, "/removed.png")
}

// checkWebURL 確認網址是 http/https、有 host、沒有帳密。
func checkWebURL(u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("不支援的網址：%q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("網址沒有 host")
	}
	if u.User != nil {
		return errors.New("網址不可帶帳號密碼")
	}
	return nil
}

// get 送出 GET 並檢查狀態碼與 Content-Type（全部值，見 contentType）；成功時呼叫者負責關閉 body。
// 404、410，或最終網址為已刪除圖片的替代圖（imgur removed.png）→ ErrImageGone；其他非 200 → 錯誤；Content-Type 有值且不是 image/*、也不是 application/octet-stream → ErrNotImage。
func (f *URLFetcher) get(ctx context.Context, rawURL string) (*http.Response, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("網址不合法：%w", err)
	}
	if err := checkWebURL(u); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", urlFetchUserAgent)
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		resp.Body.Close()
		return nil, fmt.Errorf("%w：HTTP %d", ErrImageGone, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		resp.Body.Close()
		return nil, fmt.Errorf("下載網址失敗：HTTP %d", resp.StatusCode)
	case f.gone != nil && resp.Request != nil && f.gone(resp.Request.URL):
		resp.Body.Close()
		return nil, fmt.Errorf("%w：轉址到 %s", ErrImageGone, resp.Request.URL)
	}
	if _, err := contentType(resp.Header.Values("Content-Type")); err != nil {
		resp.Body.Close()
		return nil, err
	}
	return resp, nil
}

// isImageLikeType 回傳媒體類型是否可能是圖片（image/* 或 application/octet-stream）。
func isImageLikeType(mediaType string) bool {
	return strings.HasPrefix(mediaType, "image/") || mediaType == "application/octet-stream"
}

// contentType 檢查回應的所有 Content-Type 值，回傳唯一的媒體類型（沒有 Content-Type 時為空字串）。
// 只有一個值（單一標頭、沒有以逗號列出多個）且能解析時才採用：不是圖片類 → ErrNotImage。
// 多個值（Go 取第一個、瀏覽器取最後一個，可用來繞過檢查）或無法解析時不確定瀏覽器會怎麼開：
// 任一值是圖片類或無法解析 → ErrUnconfirmedImage（不當成已刪除）；全部是非圖片類 → ErrNotImage。
func contentType(values []string) (string, error) {
	var types []string
	for _, v := range values {
		for _, t := range strings.Split(v, ",") {
			if t = strings.TrimSpace(t); t != "" {
				types = append(types, t)
			}
		}
	}
	if len(types) == 0 {
		return "", nil
	}
	imageLike, unparsable := false, false
	var mediaType string
	for _, t := range types {
		mt, _, err := mime.ParseMediaType(t)
		if err != nil {
			unparsable = true
			continue
		}
		mediaType = mt
		imageLike = imageLike || isImageLikeType(mt)
	}
	switch {
	case len(types) == 1 && !unparsable && imageLike:
		return mediaType, nil
	case !imageLike && !unparsable:
		return "", fmt.Errorf("%w：Content-Type %q", ErrNotImage, types)
	}
	return "", fmt.Errorf("%w：Content-Type %q 不只一個或無法解析", ErrUnconfirmedImage, types)
}

// Fetch 下載 rawURL：Content-Length 超過上限時不讀 body，回 ErrTooLarge；內容超過上限也回 ErrTooLarge。
// 是否能解碼成支援的圖片格式由呼叫端檢查。
func (f *URLFetcher) Fetch(ctx context.Context, rawURL string) ([]byte, error) {
	resp, err := f.get(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.ContentLength > f.maxBytes {
		return nil, fmt.Errorf("%w：%d bytes", ErrTooLarge, resp.ContentLength)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, f.maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("下載網址失敗：%w", err)
	}
	if int64(len(data)) > f.maxBytes {
		return nil, fmt.Errorf("%w：超過 %d bytes", ErrTooLarge, f.maxBytes)
	}
	return data, nil
}

// rasterSniffTypes 是 Check 視為「確定是圖片」的內容嗅探結果（http.DetectContentType）：與辨識支援的格式相同。
var rasterSniffTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
}

// ErrUnconfirmedImage 表示無法確認網址的內容是 PNG／JPEG／GIF／WEBP（例如 SVG、PDF，或宣稱是圖片但內容不符）：
// 不是「確定已刪除」也不是「確定不是圖片」，/search 歸為無法確認。
var ErrUnconfirmedImage = errors.New("content is not a confirmed raster image")

// looksLikeSVG 回傳內容開頭（去掉 BOM 與前導空白）是否為 SVG：以 <svg 開頭，或以 <?xml 開頭且含 <svg（不分大小寫）。
func looksLikeSVG(head []byte) bool {
	t := strings.TrimLeftFunc(strings.TrimPrefix(string(head), "\ufeff"), unicode.IsSpace)
	lower := strings.ToLower(t)
	return strings.HasPrefix(lower, "<svg") || (strings.HasPrefix(lower, "<?xml") && strings.Contains(lower, "<svg"))
}

// Check 確認 rawURL 上的圖片還在（/search 貼網址前）：讀回應標頭與前 512 bytes（不讀完整內容），以內容嗅探判斷格式，
// 不只信 Content-Type。
//   - 404、410、轉址到 imgur removed.png → ErrImageGone；
//   - Content-Type 不是圖片（網頁等），或 Content-Type 為 octet-stream／沒有而內容是 SVG 以外的文字 → ErrNotImage；
//   - Content-Type 有多個值或無法解析（見 contentType）→ ErrUnconfirmedImage，或全部為非圖片類時 ErrNotImage；
//   - 內容是 PNG／JPEG／GIF／WEBP → nil；
//   - 其他（SVG、BMP、PDF、宣稱 image/* 但內容不符等）→ ErrUnconfirmedImage（一般錯誤，無法確認）；
//   - 其他失敗回一般錯誤。
func (f *URLFetcher) Check(ctx context.Context, rawURL string) error {
	resp, err := f.get(ctx, rawURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	head, err := io.ReadAll(io.LimitReader(resp.Body, urlSniffBytes))
	if err != nil {
		return fmt.Errorf("讀取網址內容失敗：%w", err)
	}
	sniffed, _, _ := strings.Cut(http.DetectContentType(head), ";")
	if rasterSniffTypes[sniffed] {
		return nil
	}
	// get 已確認 Content-Type 只有一個值（圖片類）或沒有。
	mediaType, _ := contentType(resp.Header.Values("Content-Type"))
	if !strings.HasPrefix(mediaType, "image/") && strings.HasPrefix(sniffed, "text/") && !looksLikeSVG(head) {
		return fmt.Errorf("%w：內容是 %s", ErrNotImage, sniffed)
	}
	return fmt.Errorf("%w：Content-Type %q，內容是 %s", ErrUnconfirmedImage, mediaType, sniffed)
}
