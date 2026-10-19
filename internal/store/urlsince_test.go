package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"discordbot/internal/discordtest"

	"github.com/bwmarrin/discordgo"
)

// U1 (b)：圖片網址功能上線時間點（url_since）。

func TestURLSince_RoundTripAndClone(t *testing.T) {
	d := sampleData()
	d.URLSince = "300"
	msgs, err := Encode(d, "v1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msgs[0], `"url_since":"300"`) {
		t.Errorf("編碼結果應含 url_since：%s", msgs[0])
	}
	got, err := DecodeLatest(toMessages(msgs))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, d) {
		t.Errorf("round trip 不一致：%+v", got)
	}
	if c := d.Clone(); c.URLSince != "300" {
		t.Errorf("Clone 應保留 URLSince：%q", c.URLSince)
	}
}

// 舊版資料（沒有 url_since 欄位）仍能讀取，URLSince 為空；格式版本不變。
func TestURLSince_LegacyDataWithoutField(t *testing.T) {
	old := "imgbot-store 1 1/1\n" + `{"format_version":1,"guilds":{"g1":{"c1":"100"}},"user_channels":{}}`
	got, err := DecodeLatest(toMessages([]string{old}))
	if err != nil {
		t.Fatalf("舊資料應能讀取：%v", err)
	}
	if got.URLSince != "" {
		t.Errorf("舊資料 URLSince 應為空：%q", got.URLSince)
	}
	if _, setAt, ok := got.SourceChannel("c1"); !ok || setAt != "100" {
		t.Errorf("舊資料的來源頻道應保留：%v %v", setAt, ok)
	}
	if FormatVersion != 1 {
		t.Errorf("格式版本不應改變：%d", FormatVersion)
	}
}

// 沒有 url_since 時編碼不輸出此欄位（舊版程式讀到的內容與以前相同）。
func TestURLSince_OmittedWhenEmpty(t *testing.T) {
	msgs, err := Encode(sampleData(), "v1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(msgs[0], "url_since") {
		t.Errorf("沒有 url_since 時不應輸出：%s", msgs[0])
	}
}

// 把關後決定 (b)：url_since 用 Discord 伺服器時間——寫入儲存頻道時 Discord 回傳的訊息 snowflake ID，
// 不用本機時鐘。先寫一次目前資料取得 ID，再寫入含 url_since 的版本。
func TestEnsureURLSince_WritesOnceWhenMissing(t *testing.T) {
	f := discordtest.NewFake()
	s := New(f, storageCh)
	addEncoded(t, f, sampleData(), "1")
	if err := s.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := s.EnsureURLSince(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Sent) != 2 {
		t.Fatalf("應寫入儲存頻道兩次（取得 ID、存回），得到 %d", len(f.Sent))
	}
	msgs := f.Messages(storageCh)
	firstID := msgs[len(msgs)-2].ID // 第一次寫入的訊息
	if got != firstID {
		t.Errorf("url_since 應為 Discord 回傳的訊息 ID %s，得到 %q", firstID, got)
	}
	if s.Snapshot().URLSince != got {
		t.Errorf("記憶體中應為 %s：%q", got, s.Snapshot().URLSince)
	}
	s2 := New(f, storageCh)
	if err := s2.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := s2.Snapshot(); d.URLSince != got || len(d.Guilds) != 2 {
		t.Errorf("存回的資料不正確：%+v", d)
	}

	// 已有值：不改、不寫入。
	again, err := s.EnsureURLSince(context.Background())
	if err != nil || again != got {
		t.Fatalf("已有值時應回原值：%q %v", again, err)
	}
	if len(f.Sent) != 2 {
		t.Errorf("已有值時不應寫入，寫入次數 %d", len(f.Sent))
	}
}

func TestEnsureURLSince_WriteFailureKeepsEmpty(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		f := discordtest.NewFake()
		s := New(f, storageCh)
		addEncoded(t, f, sampleData(), "1")
		if err := s.Load(context.Background()); err != nil {
			t.Fatal(err)
		}
		api := &failNthStoreSend{Fake: f, n: failAt}
		s.api = api
		if _, err := s.EnsureURLSince(context.Background()); !errors.Is(err, discordtest.ErrInjected) {
			t.Fatalf("第 %d 次寫入失敗應回錯誤：%v", failAt, err)
		}
		if s.Snapshot().URLSince != "" || len(s.Snapshot().Guilds) != 2 {
			t.Errorf("第 %d 次寫入失敗時記憶體不應改變：%+v", failAt, s.Snapshot())
		}
		// 儲存頻道中最新的完整版本仍是原設定（沒有 url_since）。
		s2 := New(f, storageCh)
		if err := s2.Load(context.Background()); err != nil || len(s2.Snapshot().Guilds) != 2 || s2.Snapshot().URLSince != "" {
			t.Errorf("第 %d 次寫入失敗後儲存頻道應保留原設定：%+v %v", failAt, s2.Snapshot(), err)
		}
	}
}

// failNthStoreSend 在第 n 次發送訊息時回錯誤。
type failNthStoreSend struct {
	*discordtest.Fake
	calls, n int
}

func (f *failNthStoreSend) ChannelMessageSendComplex(channelID string, data *discordgo.MessageSend, options ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.calls++
	if f.calls == f.n {
		return nil, discordtest.ErrInjected
	}
	return f.Fake.ChannelMessageSendComplex(channelID, data, options...)
}

func TestEnsureURLSince_NotLoaded(t *testing.T) {
	s := New(discordtest.NewFake(), storageCh)
	if _, err := s.EnsureURLSince(context.Background()); !errors.Is(err, ErrNotLoaded) {
		t.Errorf("尚未載入應回 ErrNotLoaded：%v", err)
	}
}

// IsNewMessage：附件訊息只看 setAt；只含網址的訊息看 max(setAt, urlSince)；不合法時一律不算新訊息。
func TestIsNewMessage(t *testing.T) {
	cases := []struct {
		id, setAt, urlSince string
		linkOnly            bool
		want                bool
	}{
		{"150", "100", "", false, true},
		{"50", "100", "", false, false},
		{"150", "100", "200", false, true}, // 附件訊息不看 urlSince
		{"150", "100", "200", true, false}, // 網址訊息早於 urlSince
		{"250", "100", "200", true, true},
		{"150", "200", "100", true, false}, // 網址訊息早於 setAt
		{"250", "200", "100", true, true},
		{"250", "100", "", true, false}, // urlSince 不合法：網址訊息一律不算新訊息
		{"250", "", "100", false, false},
		{"250", "", "100", true, false},
		{"100", "100", "100", true, true}, // 等於時點算新訊息（同 setAt 現行規則）
	}
	for _, c := range cases {
		if got := IsNewMessage(c.id, c.setAt, c.urlSince, c.linkOnly); got != c.want {
			t.Errorf("IsNewMessage(%q,%q,%q,%v) = %v，預期 %v", c.id, c.setAt, c.urlSince, c.linkOnly, got, c.want)
		}
	}
}
