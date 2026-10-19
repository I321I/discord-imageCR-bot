package store

import (
	"testing"
	"time"

	"discordbot/internal/snowflake"
)

// 把關重點 5（記錄）：url_since 以本機時間（收到 READY 的時間）產生。Pi 3B 沒有 RTC，開機時鐘若落後
// （NTP 尚未同步），url_since 會早於實際上線時間，期間之前的只含網址訊息會被當成新訊息補處理；
// 時鐘早於 Discord epoch 時 url_since 為 "0"，等於所有設定時間點之後的網址訊息都是新訊息。
func TestGate_URLSince_ClockBehind(t *testing.T) {
	at := snowflake.FromTime(time.Unix(0, 0))
	if at != "0" {
		t.Fatalf("1970 年的時間點：%q", at)
	}
	old := snowflake.FromTime(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) // 早就存在的網址訊息
	t.Logf("url_since=%q 時，2026-01 的網址訊息 IsNewMessage=%v（setAt=1）", at, IsNewMessage(old, "1", at, true))
	if !IsNewMessage(old, "1", at, true) {
		t.Error("記錄的行為改變了，請更新本測試說明")
	}
}
