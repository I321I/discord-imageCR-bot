package main

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"discordbot/internal/recognize"
)

// 階段 11：Dockerfile 中的路徑必須與程式的預設值一致。

func readFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// worker 的預設指令（NewRapidOCR）指向 Dockerfile 複製 worker 的位置。
func TestDockerfile_WorkerPathMatchesDefault(t *testing.T) {
	cmd := recognize.NewRapidOCR().Command
	if len(cmd) != 2 || cmd[0] != "python3" {
		t.Fatalf("預設指令 %v", cmd)
	}
	if df := readFile(t, "Dockerfile"); !strings.Contains(df, "COPY ocr/ocr_worker.py "+cmd[1]+"\n") {
		t.Errorf("Dockerfile 應把 ocr/ocr_worker.py 複製到 %s", cmd[1])
	}
}

// ocr_worker.py 的日文模型與字典路徑，就是 Dockerfile 下載並驗證 sha256 的位置。
func TestDockerfile_ModelPathsMatchWorker(t *testing.T) {
	worker := readFile(t, "ocr/ocr_worker.py")
	df := readFile(t, "Dockerfile")
	for _, name := range []string{"JAPAN_REC_MODEL", "JAPAN_REC_KEYS"} {
		m := regexp.MustCompile(`(?m)^` + name + ` = "([^"]+)"`).FindStringSubmatch(worker)
		if m == nil {
			t.Fatalf("ocr_worker.py 找不到 %s", name)
		}
		if n := strings.Count(df, m[1]); n < 2 {
			t.Errorf("Dockerfile 應下載並驗證 %s（%s），只出現 %d 次", name, m[1], n)
		}
	}
}

// .env 不能進建置 context；worker、requirements、測試圖片，以及本測試讀取的 Dockerfile 與 .dockerignore
// （docker build --target test 會在映像內執行本測試）要留著。
func TestDockerignore(t *testing.T) {
	lines := strings.Split(readFile(t, ".dockerignore"), "\n")
	for _, must := range []string{".env", ".env.*", "**/.env", "**/.env.*", ".git", "spike", "**/__pycache__"} {
		if !slices.Contains(lines, must) {
			t.Errorf(".dockerignore 應排除 %s", must)
		}
	}
	for _, keep := range []string{"ocr", "internal", "internal/recognize/testdata", "go.mod", "go.sum", "Dockerfile", ".dockerignore", "*"} {
		if slices.Contains(lines, keep) {
			t.Errorf(".dockerignore 不應排除 %s", keep)
		}
	}
}

// python:3.12-slim 是 Debian trixie，GLib 套件名稱為 libglib2.0-0t64（PM 決定）。
func TestDockerfile_GlibPackageName(t *testing.T) {
	df := readFile(t, "Dockerfile")
	if !regexp.MustCompile(`apt-get install [^\n]*\blibglib2\.0-0t64\b`).MatchString(df) {
		t.Error("runtime 應安裝 libglib2.0-0t64")
	}
	if regexp.MustCompile(`\blibglib2\.0-0\s`).MatchString(df) {
		t.Error("不應再寫 libglib2.0-0（trixie 中為虛擬套件）")
	}
}

// Dockerfile 的 golang 建置映像（gobuild 與 test 階段）與 go.mod 的 go 版本（主.次）一致（階段 11 決定 2：Go 1.27）。
func TestDockerfile_GoImageMatchesGoMod(t *testing.T) {
	m := regexp.MustCompile(`(?m)^go (\d+\.\d+)(?:\.\d+)?$`).FindStringSubmatch(readFile(t, "go.mod"))
	if m == nil {
		t.Fatal("go.mod 找不到 go 指令")
	}
	tags := regexp.MustCompile(`golang:(\S+)`).FindAllStringSubmatch(readFile(t, "Dockerfile"), -1)
	if len(tags) < 2 {
		t.Fatalf("Dockerfile 應有 gobuild 與 test 兩處 golang 映像，找到 %d 處", len(tags))
	}
	for _, tag := range tags {
		if want := m[1] + "-bookworm"; tag[1] != want {
			t.Errorf("golang:%s，預期 golang:%s（與 go.mod 的 go %s 一致）", tag[1], want, m[1])
		}
	}
}
