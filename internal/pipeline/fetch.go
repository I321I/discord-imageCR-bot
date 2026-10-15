package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// MaxAttachmentSize 是處理附件的大小上限（25MB）：超過就不下載，該則訊息視為處理失敗。
const MaxAttachmentSize = 25 << 20

// ErrTooLarge 表示附件超過 MaxAttachmentSize。
var ErrTooLarge = errors.New("attachment too large")

// ErrSizeMismatch 表示下載到的長度與附件的 Size 不符（下載不完整），視為下載失敗。
var ErrSizeMismatch = errors.New("downloaded size does not match attachment size")

// HTTPFetcher 以 HTTP GET 下載附件；內容超過 MaxBytes 時回 ErrTooLarge。
type HTTPFetcher struct {
	Client   *http.Client
	MaxBytes int64
}

// NewHTTPFetcher 回傳上限 25MB、單次下載逾時 2 分鐘的 HTTPFetcher。
func NewHTTPFetcher() *HTTPFetcher {
	return &HTTPFetcher{Client: &http.Client{Timeout: 2 * time.Minute}, MaxBytes: MaxAttachmentSize}
}

func (f *HTTPFetcher) Fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := f.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("下載附件失敗：HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, f.MaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("下載附件失敗：%w", err)
	}
	if int64(len(data)) > f.MaxBytes {
		return nil, fmt.Errorf("%w：超過 %d bytes", ErrTooLarge, f.MaxBytes)
	}
	return data, nil
}
