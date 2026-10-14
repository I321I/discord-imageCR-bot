"""OCR worker：由 Go bot 以子行程常駐執行（internal/recognize/rapidocr.go）。

協定（stdin/stdout，每行一個 JSON）：
- 啟動、模型載入完成後輸出 {"ready": true}
- 請求 {"id": <整數>, "png": "<base64 PNG>"}
- 回應 {"id": <同 id>, "items": [{"box": [[x, y] x4], "text": "<文字>", "score": 0.97}]}
  或 {"id": <同 id 或 null>, "error": "<訊息>"}（單張失敗不結束 worker）
- stdout 只有協定輸出，其他記錄一律寫 stderr；讀到 stdin EOF 就結束。
  以腳本執行時，協定輸出使用原本 fd 1 的複本，fd 1 改指向 stderr（C 函式庫直接寫 fd 1 的輸出也不會混入協定）。

辨識：偵測只做一次，每個文字框都用中文與日文模型辨識；日文結果含平假名或片假名時採用日文結果，
否則採用中文結果並簡轉繁（OpenCC）。文字框超過 MAX_DUAL_REC_BOXES 個、找不到或無法載入日文模型檔時只用中文模型。

rapidocr_onnxruntime、cv2、numpy、opencc 只在 create_engine、create_japanese_rec、create_converter、decode_png 中 import，
讓單元測試不需安裝它們。
"""
import base64
import binascii
import contextlib
import json
import os
import sys

# RapidOCR 參數（Pi 第三版試驗定案）：偵測時長邊上限 960、不做方向分類、4 執行緒。
# 信心度過濾用 RapidOCR 內建的 text_score 0.5（Q66）。
ENGINE_PARAMS = {"det_limit_type": "max", "det_limit_side_len": 960, "intra_op_num_threads": 4}
USE_CLS = False
# 簡轉繁：台灣用字與慣用詞（Q58）。只套用在採用中文結果的文字框。
OPENCC_CONFIG = "s2twp"
# 日文辨識模型（Pi 第四版試驗；檔案由 Docker 映像建置時下載）。
JAPAN_REC_MODEL = "/app/ocr/models/japan_PP-OCRv4_rec_mobile.onnx"
JAPAN_REC_KEYS = "/app/ocr/models/japan_dict.txt"
JAPAN_REC_PARAMS = {
    "intra_op_num_threads": 4, "inter_op_num_threads": -1, "use_cuda": False, "use_dml": False,
    "rec_img_shape": [3, 48, 320], "rec_batch_num": 6,
}
# 一張圖的文字框超過此數時只用中文模型（滿版漫畫頁用雙模型會超過 Go 端 3 分鐘逾時）。
MAX_DUAL_REC_BOXES = 100

# 平假名與片假名（Unicode Script，與 Go 的 unicode.Hiragana、unicode.Katakana 的 BMP 範圍相同）。
# 長音符「ー」、中點「・」不屬於這兩個 Script（中文模型讀成「一」的筆畫可能被日文模型讀成「ー」）。
KANA_RANGES = [
    (0x3041, 0x3096), (0x309D, 0x309F),  # 平假名
    (0x30A1, 0x30FA), (0x30FD, 0x30FF), (0x31F0, 0x31FF), (0x32D0, 0x32FE), (0x3300, 0x3357),
    (0xFF66, 0xFF6F), (0xFF71, 0xFF9D),  # 片假名（含半形）
]


def has_kana(text):
    """回傳 text 是否含平假名或片假名。"""
    return any(lo <= ord(ch) <= hi for ch in text for lo, hi in KANA_RANGES)


def combine_rec(ch_rec, jp_rec, convert):
    """回傳取代 RapidOCR engine.text_rec 的辨識函式 rec(img_list, return_word_box) -> (結果, 耗時)。

    每個文字框都用中文（ch_rec）與日文（jp_rec）模型辨識：日文結果含假名時採用日文結果，
    否則採用中文結果並以 convert 簡轉繁。jp_rec 為 None 或文字框超過 MAX_DUAL_REC_BOXES 個時只用中文；
    中文、日文回傳筆數不一致時全部改用中文結果。
    結果每項為 (text, score, ...)，其餘欄位跟著採用的模型。
    """

    def chinese(r):
        return (convert(r[0]), *r[1:])

    def rec(img_list, return_word_box=False):
        r_ch, e_ch = ch_rec(img_list, return_word_box)
        if jp_rec is None or len(img_list) > MAX_DUAL_REC_BOXES:
            return [chinese(r) for r in r_ch], e_ch
        r_jp, e_jp = jp_rec(img_list, return_word_box)
        if len(r_jp) != len(r_ch):
            log(f"中文、日文辨識結果筆數不一致（{len(r_ch)}、{len(r_jp)}），全部改用中文結果")
            return [chinese(r) for r in r_ch], e_ch + e_jp
        out = [tuple(b) if has_kana(b[0]) else chinese(a) for a, b in zip(r_ch, r_jp)]
        return out, e_ch + e_jp

    return rec


def create_japanese_rec():
    """建立日文辨識模型；模型或字典檔不存在、或載入失敗時記錄到 stderr 並回傳 None（只用中文）。"""
    missing = [p for p in (JAPAN_REC_MODEL, JAPAN_REC_KEYS) if not os.path.exists(p)]
    if missing:
        log(f"找不到日文模型檔 {', '.join(missing)}，只用中文模型")
        return None
    from rapidocr_onnxruntime.ch_ppocr_rec import TextRecognizer

    try:
        return TextRecognizer({**JAPAN_REC_PARAMS, "model_path": JAPAN_REC_MODEL, "rec_keys_path": JAPAN_REC_KEYS})
    except Exception as e:
        log(f"載入日文模型失敗：{e}，只用中文模型")
        return None


def create_engine(convert):
    """建立 RapidOCR 引擎，並把它的文字辨識換成中文＋日文（combine_rec）。"""
    from rapidocr_onnxruntime import RapidOCR

    engine = RapidOCR(**ENGINE_PARAMS)
    engine.text_rec = combine_rec(engine.text_rec, create_japanese_rec(), convert)
    return engine


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


def protocol_stdout():
    """保留原本的 fd 1 給協定輸出（回傳其檔案物件），再把 fd 1 指向 stderr：
    C 函式庫（onnxruntime 等）與 Python 的 print 寫到 fd 1 的輸出都會進 stderr，不會混入協定。"""
    sys.stdout.flush()
    proto = os.dup(1)
    os.dup2(2, 1)
    return os.fdopen(proto, "w", encoding="utf-8")


def recognize(engine, decode, req):
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
            "text": text,
            "score": float(score),
        })
    return items


def main(stdin, stdout, make_engine=create_engine, convert=None, decode=decode_png):
    # 引擎或套件若以 Python 印到 stdout，改導到 stderr，避免混入協定輸出。
    with contextlib.redirect_stdout(sys.stderr):
        if convert is None:
            convert = create_converter()
        engine = make_engine(convert)
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
                items = recognize(engine, decode, req)
        except Exception as e:  # 單張失敗不結束 worker
            log(f"請求 {req_id} 失敗：{e}")
            write(stdout, {"id": req_id, "error": str(e)})
            continue
        write(stdout, {"id": req_id, "items": items})


if __name__ == "__main__":
    main(sys.stdin, protocol_stdout())
