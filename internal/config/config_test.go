package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoad_RequiresTokenAndStorageChannel(t *testing.T) {
	cases := map[string]map[string]string{
		"TOKEN":              {"STORAGE_CHANNEL_ID": "1"},
		"STORAGE_CHANNEL_ID": {"TOKEN": "t"},
	}
	for missing, m := range cases {
		_, err := Load(env(m))
		if err == nil {
			t.Fatalf("缺 %s 應回錯誤", missing)
		}
		if !strings.Contains(err.Error(), missing) {
			t.Errorf("錯誤訊息應含 %s，得到 %q", missing, err)
		}
	}
}

func TestLoad_OK(t *testing.T) {
	c, err := Load(env(map[string]string{
		"TOKEN":                   "tok",
		"STORAGE_CHANNEL_ID":      "100",
		"SERVER_ID":               "200",
		"TRACEMOE_KEY":            "key",
		"TRACEMOE_MIN_SIMILARITY": "0.85",
		"MEMBER_CACHE_TTL":        "90s",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		Token:                 "tok",
		StorageChannelID:      "100",
		DevGuildID:            "200",
		TraceMoeKey:           "key",
		TraceMoeMinSimilarity: 0.85,
		MemberCacheTTL:        90 * time.Second,
	}
	if c != want {
		t.Errorf("得到 %+v，預期 %+v", c, want)
	}
}

func TestLoad_Defaults(t *testing.T) {
	c, err := Load(env(map[string]string{"TOKEN": "tok", "STORAGE_CHANNEL_ID": "100"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.DevGuildID != "" || c.TraceMoeKey != "" {
		t.Errorf("選填字串應為空：%+v", c)
	}
	if c.TraceMoeMinSimilarity != 0.90 {
		t.Errorf("TraceMoeMinSimilarity 預設應為 0.90，得到 %v", c.TraceMoeMinSimilarity)
	}
	if c.MemberCacheTTL != 5*time.Minute {
		t.Errorf("MemberCacheTTL 預設應為 5m，得到 %v", c.MemberCacheTTL)
	}
}

func TestLoad_TrimsSpaces(t *testing.T) {
	c, err := Load(env(map[string]string{"TOKEN": " tok ", "STORAGE_CHANNEL_ID": " 100\n"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Token != "tok" || c.StorageChannelID != "100" {
		t.Errorf("應去除前後空白：%+v", c)
	}
}

func TestLoad_InvalidOptionalValues(t *testing.T) {
	cases := map[string]string{
		"TRACEMOE_MIN_SIMILARITY": "abc",
		"MEMBER_CACHE_TTL":        "5 minutes",
	}
	for k, v := range cases {
		_, err := Load(env(map[string]string{"TOKEN": "t", "STORAGE_CHANNEL_ID": "1", k: v}))
		if err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("%s=%q 應回含變數名的錯誤，得到 %v", k, v, err)
		}
	}
}
