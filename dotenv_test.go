package main

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureLog 回傳 fn 執行期間 log 的輸出。
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev, flags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() { log.SetOutput(prev); log.SetFlags(flags) }()
	fn()
	return buf.String()
}

// Docker 中以 --env-file 傳入、映像內沒有 .env：檔案不存在是正常情況，不印警告。
func TestLoadDotEnv_MissingFileNoWarning(t *testing.T) {
	out := captureLog(t, func() { loadDotEnv(filepath.Join(t.TempDir(), ".env")) })
	if strings.Contains(out, "警告") {
		t.Errorf("沒有 .env 時不應印警告：%q", out)
	}
}

// .env 存在且正確：載入其中的變數，不印警告。
func TestLoadDotEnv_LoadsExistingFile(t *testing.T) {
	const key = "IMAGEBOT_DOTENV_TEST"
	t.Cleanup(func() { os.Unsetenv(key) })
	p := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(p, []byte(key+"=ok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := captureLog(t, func() { loadDotEnv(p) })
	if got := os.Getenv(key); got != "ok" {
		t.Errorf("%s = %q，預期 ok", key, got)
	}
	if strings.Contains(out, "警告") {
		t.Errorf("載入成功不應印警告：%q", out)
	}
}

// .env 存在但載入失敗（例如是目錄）：印警告。
func TestLoadDotEnv_ExistingButBrokenWarns(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".env")
	if err := os.Mkdir(p, 0o700); err != nil {
		t.Fatal(err)
	}
	out := captureLog(t, func() { loadDotEnv(p) })
	if !strings.Contains(out, "警告") {
		t.Errorf(".env 存在但載入失敗時應印警告：%q", out)
	}
}
