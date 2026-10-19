package store

import (
	"context"
	"testing"
	"time"

	"discordbot/internal/discordtest"
	"discordbot/internal/snowflake"
)

// 把關重點 5：url_since 原本以本機時間（收到 READY 的時間）產生，Pi 3B 沒有 RTC，開機時鐘落後時 url_since 會過早
// （1970 年時為 "0"），以前的只含網址訊息會被當成新訊息補處理。
// 把關後決定 (b) 改用 Discord 伺服器時間（寫入儲存頻道時回傳的訊息 ID）：本機時鐘落後也不影響。
func TestGate_URLSince_ClockBehind(t *testing.T) {
	f := discordtest.NewFake()
	s := New(f, "storage")
	s.now = func() time.Time { return time.Unix(0, 0) } // 本機時鐘停在 1970
	if err := s.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	at, err := s.EnsureURLSince(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	msgs := f.Messages("storage")
	if len(msgs) == 0 || at != msgs[0].ID {
		t.Fatalf("url_since 應為 Discord 回傳的訊息 ID：%q（訊息 %v）", at, msgs)
	}
	if at == "0" || at == snowflake.FromTime(time.Unix(0, 0)) {
		t.Errorf("url_since 不應來自本機時鐘：%q", at)
	}
	// 早於 url_since 的網址訊息不是新訊息。
	older := "1000"
	if IsNewMessage(older, "1", at, true) {
		t.Errorf("早於 url_since 的網址訊息不應是新訊息（url_since=%s）", at)
	}
}
