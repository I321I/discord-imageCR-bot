# syntax=docker/dockerfile:1
#
# 圖庫 bot：單一映像、單一容器（Go bot + 常駐 Python OCR worker）。部署目標 Raspberry Pi 3B（linux/arm64）。
# 需要 BuildKit（docker buildx；Docker 23 以後的 docker build 預設即是）。
#
#   正式映像：docker build -t imagebot .（最後一個階段為 runtime，不加 --target 即得正式映像）
#   測試：    docker build --target test .
#
# 建置方式與執行方式見 README.md。

# ---- Go 建置：在建置機原生交叉編譯，不經 QEMU（在 Pi 上直接 build 時就是原生編譯）----
FROM --platform=$BUILDPLATFORM golang:1.27-bookworm AS gobuild
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/bot .

# ---- 執行階段 ----
FROM python:3.12-slim AS runtime
SHELL ["/bin/bash", "-o", "pipefail", "-c"]

# OpenCV 需要 libgl1、libglib2.0-0t64（python:3.12-slim 為 Debian trixie，GLib 套件名稱帶 t64）；ca-certificates 給 bot 連 Discord 與 trace.moe（HTTPS）。
# apt 套件不鎖版本（Debian 安全更新會讓舊版本無法安裝）；Python 套件全部鎖定。
# hadolint ignore=DL3008
RUN apt-get update \
    && apt-get install -y --no-install-recommends libgl1 libglib2.0-0t64 ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Python 依賴：版本全部鎖定（Pi 上實測的 pip freeze）。
COPY ocr/requirements.txt /app/ocr/requirements.txt
RUN pip install --no-cache-dir -r /app/ocr/requirements.txt

# 日文辨識模型與字典（ModelScope RapidAI/RapidOCR），下載後驗證 sha256，不符則建置失敗。
# 路徑須與 ocr/ocr_worker.py 的 JAPAN_REC_MODEL、JAPAN_REC_KEYS 一致。
ARG MODELSCOPE_BASE=https://www.modelscope.cn/models/RapidAI/RapidOCR/resolve/master
ARG JAPAN_REC_MODEL_SHA256=e1075a67dba758ecfc7ebc78a10ae61c95ac8fb66a9c86fab5541e33f085cb7a
ARG JAPAN_DICT_SHA256=1dcfcb41eec90576a945b3084f22ade11ced506e24f14879245b071698f308e8
RUN mkdir -p /app/ocr/models \
    && python3 -c "import sys, urllib.request; urllib.request.urlretrieve(sys.argv[1], sys.argv[2])" \
        "$MODELSCOPE_BASE/onnx/PP-OCRv4/rec/japan_PP-OCRv4_rec_mobile.onnx" /app/ocr/models/japan_PP-OCRv4_rec_mobile.onnx \
    && python3 -c "import sys, urllib.request; urllib.request.urlretrieve(sys.argv[1], sys.argv[2])" \
        "$MODELSCOPE_BASE/paddle/PP-OCRv4/rec/japan_PP-OCRv4_rec_mobile/japan_dict.txt" /app/ocr/models/japan_dict.txt \
    && printf '%s  %s\n' \
        "$JAPAN_REC_MODEL_SHA256" /app/ocr/models/japan_PP-OCRv4_rec_mobile.onnx \
        "$JAPAN_DICT_SHA256" /app/ocr/models/japan_dict.txt \
        | sha256sum -c - \
    && chmod 0644 /app/ocr/models/*

# OCR worker：路徑須與 internal/recognize/rapidocr.go 的預設指令（python3 /app/ocr/ocr_worker.py）一致。
COPY ocr/ocr_worker.py /app/ocr/ocr_worker.py
COPY --from=gobuild /out/bot /app/bot

# 非 root 使用者執行（映像內的檔案皆為 root 擁有、唯讀即可）。
RUN useradd --system --uid 10001 --user-group --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin bot

# GOMEMLIMIT：讓 Go GC 在小記憶體機器上積極回收。PYTHONUNBUFFERED：worker 的 log 立即寫出。
ENV GOMEMLIMIT=150MiB \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1

WORKDIR /app
USER bot
CMD ["/app/bot"]

# ---- 測試階段：docker build --target test .（不必是 arm64）----
# Go 測試（含 -race，需要 gcc）、Python 單元測試、-tags rapidocr 真實截圖整合測試（缺圖時略過）。
FROM runtime AS test
# 測試階段以 root 執行（需要 apt 與 Go 建置快取）；此階段不用於部署。
# hadolint ignore=DL3002
USER root
# hadolint ignore=DL3008
RUN apt-get update \
    && apt-get install -y --no-install-recommends gcc libc6-dev \
    && rm -rf /var/lib/apt/lists/*
COPY --from=golang:1.27-bookworm /usr/local/go /usr/local/go
# 測試不套用正式環境的 GOMEMLIMIT（-race 需要較多記憶體）。
ENV PATH=/usr/local/go/bin:$PATH \
    GOMEMLIMIT=off
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# 分開執行，失敗時容易看出是哪一步。
# hadolint ignore=DL3059
RUN python3 -m unittest ocr/test_ocr_worker.py ocr/test_gate_ocr_worker.py
# hadolint ignore=DL3059
RUN go vet ./... && go vet -tags rapidocr ./...
# hadolint ignore=DL3059
RUN CGO_ENABLED=1 go test -timeout 900s -race -count=1 ./...
# hadolint ignore=DL3059
RUN go test -tags rapidocr -timeout 20m -count=1 -v ./internal/recognize/

# ---- 正式映像（預設目標）----
# 不加 --target 時 docker build 建的是最後一個階段，所以最後回到 runtime：
# `docker build -t imagebot .` 得到正式映像，不會建 test 階段、也不會跑測試。
FROM runtime
