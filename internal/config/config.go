// Package config 讀取環境變數設定。
package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	defaultTraceMoeMinSimilarity = 0.90
	defaultMemberCacheTTL        = 5 * time.Minute
)

type Config struct {
	Token            string // TOKEN（必填）
	StorageChannelID string // STORAGE_CHANNEL_ID（必填）
	DevGuildID       string // SERVER_ID：只用來註冊開發者指令

	TraceMoeKey           string        // TRACEMOE_KEY，空 = 不帶 key
	TraceMoeMinSimilarity float64       // TRACEMOE_MIN_SIMILARITY，預設 0.90
	MemberCacheTTL        time.Duration // MEMBER_CACHE_TTL（Go duration 格式），預設 5m
}

// Load 用 getenv 讀取設定；缺必填或格式錯誤時回錯誤，錯誤訊息含變數名稱。
func Load(getenv func(string) string) (Config, error) {
	get := func(k string) string { return strings.TrimSpace(getenv(k)) }

	c := Config{
		Token:                 get("TOKEN"),
		StorageChannelID:      get("STORAGE_CHANNEL_ID"),
		DevGuildID:            get("SERVER_ID"),
		TraceMoeKey:           get("TRACEMOE_KEY"),
		TraceMoeMinSimilarity: defaultTraceMoeMinSimilarity,
		MemberCacheTTL:        defaultMemberCacheTTL,
	}
	for _, req := range []struct{ name, val string }{
		{"TOKEN", c.Token},
		{"STORAGE_CHANNEL_ID", c.StorageChannelID},
	} {
		if req.val == "" {
			return Config{}, fmt.Errorf("缺少必填環境變數 %s", req.name)
		}
	}

	if v := get("TRACEMOE_MIN_SIMILARITY"); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return Config{}, fmt.Errorf("環境變數 TRACEMOE_MIN_SIMILARITY 格式錯誤：%w", err)
		}
		c.TraceMoeMinSimilarity = f
	}
	if v := get("MEMBER_CACHE_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return Config{}, fmt.Errorf("環境變數 MEMBER_CACHE_TTL 格式錯誤：%w", err)
		}
		c.MemberCacheTTL = d
	}
	return c, nil
}
