package main

import (
	"regexp"
	"slices"
	"strings"
	"testing"
)

// 把關者（階段 11）：Dockerfile 結構檢查。本機沒有 docker，以文字檢查防止日後改壞。

// dockerStages 依 FROM 把 Dockerfile 切成各階段（key 為 AS 名稱），內容不含註解行。
func dockerStages(t *testing.T) (order []string, stages map[string][]string) {
	t.Helper()
	stages = map[string][]string{}
	from := regexp.MustCompile(`(?i)^FROM\s+(?:--platform=\S+\s+)?\S+\s+AS\s+(\S+)$`)
	cur := ""
	for _, line := range strings.Split(readFile(t, "Dockerfile"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if m := from.FindStringSubmatch(line); m != nil {
			cur = m[1]
			order = append(order, cur)
			continue
		}
		if strings.HasPrefix(strings.ToUpper(line), "FROM ") {
			// 沒有 AS 名稱的階段（例如最後的 FROM runtime）以整行為 key。
			cur = line
			order = append(order, cur)
			continue
		}
		stages[cur] = append(stages[cur], line)
	}
	return order, stages
}

// 正式映像（runtime）不得 COPY 整個 context（testdata 版權圖片、原始碼不能進映像），只能複製指定檔案。
func TestGK11_RuntimeDoesNotCopyWholeContext(t *testing.T) {
	_, st := dockerStages(t)
	rt, ok := st["runtime"]
	if !ok {
		t.Fatal("找不到 runtime 階段")
	}
	allowed := []string{
		"COPY ocr/requirements.txt /app/ocr/requirements.txt",
		"COPY ocr/ocr_worker.py /app/ocr/ocr_worker.py",
		"COPY --from=gobuild /out/bot /app/bot",
	}
	for _, l := range rt {
		up := strings.ToUpper(l)
		if strings.HasPrefix(up, "COPY ") || strings.HasPrefix(up, "ADD ") {
			if !slices.Contains(allowed, l) {
				t.Errorf("runtime 階段出現未預期的複製：%s", l)
			}
		}
	}
	// runtime 以非 root 執行，且是最後一個 USER。
	var lastUser string
	for _, l := range rt {
		if strings.HasPrefix(l, "USER ") {
			lastUser = l
		}
	}
	if lastUser != "USER bot" {
		t.Errorf("runtime 最後的 USER 為 %q，預期 USER bot", lastUser)
	}
}

// 交叉編譯：gobuild 以 BUILDPLATFORM 執行，TARGETOS/TARGETARCH 必須在該階段內宣告（FROM 之後）才有值。
func TestGK11_GoBuildCrossCompileArgs(t *testing.T) {
	order, st := dockerStages(t)
	if len(order) == 0 || order[0] != "gobuild" {
		t.Fatalf("階段順序 %v，預期 gobuild 在最前", order)
	}
	gb := st["gobuild"]
	for _, a := range []string{"ARG TARGETOS", "ARG TARGETARCH"} {
		if !slices.Contains(gb, a) {
			t.Errorf("gobuild 階段缺少 %s", a)
		}
	}
	if !strings.Contains(readFile(t, "Dockerfile"), "FROM --platform=$BUILDPLATFORM golang:") {
		t.Error("gobuild 應以 --platform=$BUILDPLATFORM 執行")
	}
	build := ""
	for _, l := range gb {
		if strings.Contains(l, "go build") {
			build = l
		}
	}
	for _, want := range []string{"CGO_ENABLED=0", "GOOS=$TARGETOS", "GOARCH=$TARGETARCH", "-o /out/bot ."} {
		if !strings.Contains(build, want) {
			t.Errorf("go build 指令缺少 %q：%s", want, build)
		}
	}
}

// test 階段以 runtime 為基礎（測試的就是正式映像的 Python 環境）。
func TestGK11_TestStageBasedOnRuntime(t *testing.T) {
	if !strings.Contains(readFile(t, "Dockerfile"), "\nFROM runtime AS test\n") {
		t.Error("test 階段應 FROM runtime")
	}
}

// 沒有 --target 時 docker build 建的是「最後一個」階段。README 的正式建置指令
// `docker build -t imagebot .` 必須得到正式映像，所以最後一個階段不能是 test
// （否則正式映像以 root 執行、內含原始碼與本機的版權測試圖片、Go 工具鏈，並在 Pi 上跑 -race 與 20 分鐘整合測試）。
func TestGK11_DefaultTargetIsNotTest(t *testing.T) {
	df := readFile(t, "Dockerfile")
	var froms []string
	for _, line := range strings.Split(df, "\n") {
		if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(line)), "FROM ") {
			froms = append(froms, strings.TrimSpace(line))
		}
	}
	if len(froms) == 0 {
		t.Fatal("Dockerfile 沒有 FROM")
	}
	last := froms[len(froms)-1]
	if strings.HasSuffix(strings.ToLower(last), " as test") {
		t.Errorf("最後一個階段是 test（%s）：docker build -t imagebot . 會建出測試映像而非正式映像", last)
	}
	if !strings.HasPrefix(last, "FROM runtime") && !strings.HasPrefix(last, "FROM python:") {
		t.Errorf("最後一個階段 %q 應是正式映像（runtime）", last)
	}
}

// .env 的各種變體都不能進建置 context。
func TestGK11_DockerignoreEnvVariants(t *testing.T) {
	lines := strings.Split(readFile(t, ".dockerignore"), "\n")
	for _, must := range []string{".env", ".env.*", ".claude"} {
		if !slices.Contains(lines, must) {
			t.Errorf(".dockerignore 應排除 %s", must)
		}
	}
}
