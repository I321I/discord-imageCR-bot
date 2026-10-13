"""OCR worker：由 Go bot 以子行程常駐執行（internal/recognize/rapidocr.go）。

協定（stdin/stdout，每行一個 JSON）：
- 啟動、模型載入完成後輸出 {"ready": true}
- 請求 {"id": <整數>, "png": "<base64 PNG>"}
- 回應 {"id": <同 id>, "items": [{"box": [[x, y] x4], "text": "<已簡轉繁>", "score": 0.97}]}
  或 {"id": <同 id 或 null>, "error": "<訊息>"}（單張失敗不結束 worker）
- stdout 只有協定輸出，其他記錄一律寫 stderr；讀到 stdin EOF 就結束。

rapidocr_onnxruntime、cv2、numpy、opencc 只在 create_engine、create_converter、decode_png 中 import，
讓單元測試不需安裝它們。
"""
import base64
import binascii
import contextlib
import json
import sys

# RapidOCR 參數（暫定，待 Pi 試驗定案）：偵測時長邊上限 960、不做方向分類、4 執行緒。
# 信心度過濾用 RapidOCR 內建的 text_score 0.5（Q66）。
ENGINE_PARAMS = {"det_limit_type": "max", "det_limit_side_len": 960, "intra_op_num_threads": 4}
USE_CLS = False
# 簡轉繁：台灣用字與慣用詞（Q58）。
OPENCC_CONFIG = "s2twp"


def create_engine():
    from rapidocr_onnxruntime import RapidOCR

    return RapidOCR(**ENGINE_PARAMS)


def create_converter():
    from opencc import OpenCC

    return OpenCC(OPENCC_CONFIG).convert


def decode_png(data):
    """PNG bytes → OpenCV BGR 影像；無法解碼時回 None。"""
    import cv2
    import numpy as np

    return cv2.imdecode(np.frombuffer(data, dtype=np.uint8), cv2.IMREAD_COLOR)


def log(msg):
    print(f"ocr_worker: {msg}", file=sys.stderr, flush=True)


def write(stdout, obj):
    stdout.write(json.dumps(obj) + "\n")
    stdout.flush()


def recognize(engine, convert, decode, req):
    """處理一個已解析的請求，回傳 items；失敗時丟出例外。"""
    png = req.get("png")
    if not isinstance(png, str):
        raise ValueError("缺少 png")
    try:
        data = base64.b64decode(png, validate=True)
    except (binascii.Error, ValueError) as e:
        raise ValueError(f"png 不是合法的 base64：{e}") from None
    img = decode(data)
    if img is None:
        raise ValueError("無法解碼圖片")
    result, _ = engine(img, use_cls=USE_CLS)
    items = []
    for box, text, score in result or []:
        items.append({
            "box": [[float(x), float(y)] for x, y in box],
            "text": convert(text),
            "score": float(score),
        })
    return items


def main(stdin, stdout, make_engine=create_engine, convert=None, decode=decode_png):
    # 引擎或套件若印到 stdout，改導到 stderr，避免混入協定輸出。
    with contextlib.redirect_stdout(sys.stderr):
        engine = make_engine()
        if convert is None:
            convert = create_converter()
    write(stdout, {"ready": True})
    while True:
        line = stdin.readline()
        if not line:
            return
        if not line.strip():
            continue
        try:
            req = json.loads(line)
        except ValueError as e:
            log(f"請求不是 JSON：{e}")
            write(stdout, {"id": None, "error": f"請求不是 JSON：{e}"})
            continue
        req_id = req.get("id") if isinstance(req, dict) else None
        try:
            if not isinstance(req, dict):
                raise ValueError("請求不是 JSON 物件")
            with contextlib.redirect_stdout(sys.stderr):
                items = recognize(engine, convert, decode, req)
        except Exception as e:  # 單張失敗不結束 worker
            log(f"請求 {req_id} 失敗：{e}")
            write(stdout, {"id": req_id, "error": str(e)})
            continue
        write(stdout, {"id": req_id, "items": items})


if __name__ == "__main__":
    main(sys.stdin, sys.stdout)
