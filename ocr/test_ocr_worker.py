"""ocr_worker 的單元測試（標準函式庫 unittest；引擎、OpenCC、解碼都用假的，不需安裝 RapidOCR）。

執行：python3 -m unittest ocr/test_ocr_worker.py（在專案根目錄）
"""
import base64
import io
import json
import os
import subprocess
import sys
import tempfile
import textwrap
import types
import unittest
from unittest import mock

OCR_DIR = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, OCR_DIR)

import ocr_worker  # noqa: E402

BOX = [[1.0, 2.0], [30.0, 2.0], [30.0, 12.0], [1.0, 12.0]]


class FakeEngine:
    """記錄呼叫；依 results 依序回傳（預設回傳一個文字框）。"""

    def __init__(self, results=None, prints=False):
        self.calls = []
        self.results = list(results or [])
        self.prints = prints

    def __call__(self, img, use_cls=True):
        self.calls.append((img, use_cls))
        if self.prints:
            print("引擎的雜訊輸出")
        if self.results:
            r = self.results.pop(0)
            if isinstance(r, Exception):
                raise r
            return r
        return [[BOX, "简体", 0.97]], [0.1, 0.0, 0.2]


def request(id_, png=b"PNGDATA"):
    return json.dumps({"id": id_, "png": base64.b64encode(png).decode()}) + "\n"


def run(lines, engine=None, convert=lambda s: "轉:" + s, decode=lambda b: ("decoded", b)):
    """以假依賴執行 main，回傳 (stdout 每行解析後的 JSON, engine)。"""
    engine = engine or FakeEngine()
    stdin = io.StringIO("".join(lines))
    stdout = io.StringIO()
    if isinstance(sys.stderr, io.StringIO):
        ocr_worker.main(stdin, stdout, make_engine=lambda c: engine, convert=convert, decode=decode)
    else:
        with mock.patch.object(sys, "stderr", io.StringIO()):  # 不讓 worker 的記錄干擾測試輸出
            ocr_worker.main(stdin, stdout, make_engine=lambda c: engine, convert=convert, decode=decode)
    out = stdout.getvalue()
    assert out.endswith("\n") or out == "", repr(out)
    return [json.loads(line) for line in out.splitlines()], engine


class MainTest(unittest.TestCase):
    def test_ready_first(self):
        msgs, _ = run([])
        self.assertEqual(msgs, [{"ready": True}])

    def test_engine_created_before_ready(self):
        order = []
        stdout = io.StringIO()

        def make_engine(convert):
            order.append(("engine", stdout.getvalue()))
            return FakeEngine()

        ocr_worker.main(io.StringIO(""), stdout, make_engine=make_engine, convert=str, decode=lambda b: b)
        self.assertEqual(order, [("engine", "")])
        self.assertEqual(json.loads(stdout.getvalue()), {"ready": True})

    def test_request_returns_engine_items(self):
        # 簡轉繁由引擎內的辨識函式（combine_rec）逐框決定，main 不再轉換。
        msgs, engine = run([request(7)])
        self.assertEqual(msgs[1], {"id": 7, "items": [{"box": BOX, "text": "简体", "score": 0.97}]})
        self.assertEqual(engine.calls, [(("decoded", b"PNGDATA"), False)])

    def test_make_engine_receives_convert(self):
        got = []

        def make_engine(convert):
            got.append(convert)
            return FakeEngine()

        conv = lambda s: s  # noqa: E731
        with mock.patch.object(sys, "stderr", io.StringIO()):
            ocr_worker.main(io.StringIO(""), io.StringIO(), make_engine=make_engine, convert=conv, decode=lambda b: b)
        self.assertEqual(got, [conv])

    def test_uses_configured_use_cls(self):
        with mock.patch.object(ocr_worker, "USE_CLS", True):
            _, engine = run([request(1)])
        self.assertEqual(engine.calls[0][1], True)

    def test_no_result_is_empty_items(self):
        msgs, _ = run([request(3)], engine=FakeEngine(results=[(None, None)]))
        self.assertEqual(msgs[1], {"id": 3, "items": []})

    def test_bad_base64_is_error_and_continues(self):
        bad = json.dumps({"id": 1, "png": "!!!不是base64"}) + "\n"
        msgs, engine = run([bad, request(2)])
        self.assertEqual(msgs[1]["id"], 1)
        self.assertIn("error", msgs[1])
        self.assertEqual(msgs[2]["id"], 2)
        self.assertIn("items", msgs[2])
        self.assertEqual(len(engine.calls), 1)

    def test_invalid_json_is_error_with_null_id(self):
        msgs, _ = run(["not json\n", request(2)])
        self.assertIsNone(msgs[1]["id"])
        self.assertIn("error", msgs[1])
        self.assertEqual(msgs[2]["id"], 2)

    def test_missing_png_is_error(self):
        msgs, _ = run([json.dumps({"id": 4}) + "\n"])
        self.assertEqual(msgs[1]["id"], 4)
        self.assertIn("error", msgs[1])

    def test_decode_failure_is_error(self):
        msgs, engine = run([request(5)], decode=lambda b: None)
        self.assertEqual(msgs[1]["id"], 5)
        self.assertIn("error", msgs[1])
        self.assertEqual(engine.calls, [])

    def test_engine_exception_is_error_and_continues(self):
        msgs, _ = run([request(1), request(2)], engine=FakeEngine(results=[RuntimeError("boom")]))
        self.assertEqual(msgs[1], {"id": 1, "error": "boom"})
        self.assertEqual(msgs[2]["id"], 2)
        self.assertIn("items", msgs[2])

    def test_blank_lines_ignored(self):
        msgs, _ = run(["\n", "   \n", request(1)])
        self.assertEqual(len(msgs), 2)

    def test_engine_stdout_noise_does_not_reach_protocol(self):
        stderr = io.StringIO()
        with mock.patch.object(sys, "stderr", stderr):
            msgs, _ = run([request(1)], engine=FakeEngine(prints=True))
        self.assertEqual(len(msgs), 2)
        self.assertIn("引擎的雜訊輸出", stderr.getvalue())

    def test_numpy_like_values_are_serialized(self):
        class Num:
            def __init__(self, v):
                self.v = v

            def __float__(self):
                return float(self.v)

        box = [[Num(1), Num(2)], [Num(3), Num(2)], [Num(3), Num(4)], [Num(1), Num(4)]]
        msgs, _ = run([request(1)], engine=FakeEngine(results=[([[box, "字", Num(0.5)]], None)]))
        self.assertEqual(msgs[1]["items"], [{"box": [[1.0, 2.0], [3.0, 2.0], [3.0, 4.0], [1.0, 4.0]], "text": "字", "score": 0.5}])

    def test_each_stdout_line_is_json(self):
        stdout = io.StringIO()
        lines = [request(1), "garbage\n", request(2)]
        with mock.patch.object(sys, "stderr", io.StringIO()):
            ocr_worker.main(io.StringIO("".join(lines)), stdout, make_engine=lambda c: FakeEngine(), convert=str, decode=lambda b: b)
        for line in stdout.getvalue().splitlines():
            json.loads(line)


class DefaultsTest(unittest.TestCase):
    def test_engine_params(self):
        self.assertEqual(
            ocr_worker.ENGINE_PARAMS,
            {"det_limit_type": "max", "det_limit_side_len": 960, "intra_op_num_threads": 4},
        )
        self.assertFalse(ocr_worker.USE_CLS)
        self.assertEqual(ocr_worker.OPENCC_CONFIG, "s2twp")

    def test_japanese_model_defaults(self):
        self.assertEqual(ocr_worker.JAPAN_REC_MODEL, "/app/ocr/models/japan_PP-OCRv4_rec_mobile.onnx")
        self.assertEqual(ocr_worker.JAPAN_REC_KEYS, "/app/ocr/models/japan_dict.txt")
        self.assertEqual(ocr_worker.MAX_DUAL_REC_BOXES, 100)

    def test_create_converter_uses_opencc_config(self):
        configs = []

        class FakeOpenCC:
            def __init__(self, config):
                configs.append(config)

            def convert(self, s):
                return "繁:" + s

        fake = types.ModuleType("opencc")
        fake.OpenCC = FakeOpenCC
        with mock.patch.dict(sys.modules, {"opencc": fake}):
            convert = ocr_worker.create_converter()
        self.assertEqual(configs, ["s2twp"])
        self.assertEqual(convert("软件"), "繁:软件")

    def test_decode_png_uses_cv2_imdecode(self):
        calls = []
        fake_np = types.ModuleType("numpy")
        fake_np.uint8 = "uint8"
        fake_np.frombuffer = lambda data, dtype: ("buf", data, dtype)
        fake_cv2 = types.ModuleType("cv2")
        fake_cv2.IMREAD_COLOR = 1
        fake_cv2.imdecode = lambda buf, flag: calls.append((buf, flag)) or "image"
        with mock.patch.dict(sys.modules, {"numpy": fake_np, "cv2": fake_cv2}):
            self.assertEqual(ocr_worker.decode_png(b"abc"), "image")
        self.assertEqual(calls, [(("buf", b"abc", "uint8"), 1)])


def fake_rapidocr_modules(created, jp_configs):
    """回傳假的 rapidocr_onnxruntime 與 rapidocr_onnxruntime.ch_ppocr_rec 模組。
    假 RapidOCR 的 text_rec 是中文模型（回傳「中:<輸入>」）；假日文 TextRecognizer 回傳「日:<輸入>」。"""

    class FakeRapidOCR:
        def __init__(self, **kwargs):
            created.update(kwargs)
            self.text_rec = lambda imgs, return_word_box=False: ([("中" + str(i), 0.9) for i in imgs], 1.0)

    class FakeTextRecognizer:
        def __init__(self, config):
            jp_configs.append(config)

        def __call__(self, imgs, return_word_box=False):
            return [("日" + str(i), 0.8) for i in imgs], 2.0

    pkg = types.ModuleType("rapidocr_onnxruntime")
    pkg.RapidOCR = FakeRapidOCR
    rec = types.ModuleType("rapidocr_onnxruntime.ch_ppocr_rec")
    rec.TextRecognizer = FakeTextRecognizer
    pkg.ch_ppocr_rec = rec
    return {"rapidocr_onnxruntime": pkg, "rapidocr_onnxruntime.ch_ppocr_rec": rec}


class CreateEngineTest(unittest.TestCase):
    def setUp(self):
        self.created, self.jp_configs = {}, []
        patcher = mock.patch.dict(sys.modules, fake_rapidocr_modules(self.created, self.jp_configs))
        patcher.start()
        self.addCleanup(patcher.stop)
        self.stderr = io.StringIO()
        patcher = mock.patch.object(sys, "stderr", self.stderr)
        patcher.start()
        self.addCleanup(patcher.stop)

    def model_files(self):
        d = tempfile.TemporaryDirectory()
        self.addCleanup(d.cleanup)
        model, keys = os.path.join(d.name, "japan.onnx"), os.path.join(d.name, "japan_dict.txt")
        for p in (model, keys):
            open(p, "w").close()
        return model, keys

    def test_passes_engine_params(self):
        with mock.patch.object(ocr_worker, "JAPAN_REC_MODEL", "/nonexistent/japan.onnx"):
            ocr_worker.create_engine(lambda s: s)
        self.assertEqual(self.created, ocr_worker.ENGINE_PARAMS)

    def test_japanese_recognizer_config(self):
        model, keys = self.model_files()
        with mock.patch.object(ocr_worker, "JAPAN_REC_MODEL", model), mock.patch.object(ocr_worker, "JAPAN_REC_KEYS", keys):
            ocr_worker.create_engine(lambda s: s)
        self.assertEqual(self.jp_configs, [{
            "intra_op_num_threads": 4, "inter_op_num_threads": -1, "use_cuda": False, "use_dml": False,
            "model_path": model, "rec_keys_path": keys, "rec_img_shape": [3, 48, 320], "rec_batch_num": 6,
        }])

    def test_text_rec_combines_chinese_and_japanese(self):
        model, keys = self.model_files()
        with mock.patch.object(ocr_worker, "JAPAN_REC_MODEL", model), mock.patch.object(ocr_worker, "JAPAN_REC_KEYS", keys):
            engine = ocr_worker.create_engine(lambda s: "繁" + s)
        with mock.patch.object(ocr_worker, "has_kana", lambda s: s == "日b"):
            res, elapse = engine.text_rec(["a", "b"], False)
        self.assertEqual(res, [("繁中a", 0.9), ("日b", 0.8)])
        self.assertEqual(elapse, 3.0)

    def test_missing_japanese_model_falls_back_to_chinese(self):
        _, keys = self.model_files()
        with mock.patch.object(ocr_worker, "JAPAN_REC_MODEL", "/nonexistent/japan.onnx"), mock.patch.object(ocr_worker, "JAPAN_REC_KEYS", keys):
            engine = ocr_worker.create_engine(lambda s: "繁" + s)
        self.assertEqual(self.jp_configs, [])
        self.assertIn("/nonexistent/japan.onnx", self.stderr.getvalue())
        res, _ = engine.text_rec(["あ"], False)
        self.assertEqual(res, [("繁中あ", 0.9)])

    def test_missing_japanese_dict_falls_back_to_chinese(self):
        model, _ = self.model_files()
        with mock.patch.object(ocr_worker, "JAPAN_REC_MODEL", model), mock.patch.object(ocr_worker, "JAPAN_REC_KEYS", "/nonexistent/japan_dict.txt"):
            ocr_worker.create_engine(lambda s: s)
        self.assertEqual(self.jp_configs, [])
        self.assertIn("/nonexistent/japan_dict.txt", self.stderr.getvalue())


class HasKanaTest(unittest.TestCase):
    def test_has_kana(self):
        for s, want in [
            ("わたしたち", True), ("カタカナ", True), ("ｶﾀｶﾅ", True), ("兄妹なんだよ", True), ("ゝゞ", True),
            ("漢字", False), ("日本語", False), ("ー", False), ("一ー一", False), ("・", False), ("", False), ("ABC", False),
        ]:
            self.assertEqual(ocr_worker.has_kana(s), want, s)


class CombineRecTest(unittest.TestCase):
    def make(self, ch_texts, jp_texts, jp=True):
        self.calls = []

        def ch_rec(imgs, return_word_box=False):
            self.calls.append(("ch", list(imgs), return_word_box))
            return [(t, 0.9, "ch-word") for t in ch_texts[: len(imgs)]], 1.5

        def jp_rec(imgs, return_word_box=False):
            self.calls.append(("jp", list(imgs), return_word_box))
            return [(t, 0.7, "jp-word") for t in jp_texts[: len(imgs)]], 2.5

        return ocr_worker.combine_rec(ch_rec, jp_rec if jp else None, lambda s: "繁:" + s)

    def test_kana_uses_japanese_without_conversion(self):
        rec = self.make(["我们"], ["わたしたち"])
        res, elapse = rec(["img"], True)
        self.assertEqual(res, [("わたしたち", 0.7, "jp-word")])
        self.assertEqual(elapse, 4.0)
        self.assertEqual(self.calls, [("ch", ["img"], True), ("jp", ["img"], True)])

    def test_no_kana_uses_converted_chinese(self):
        rec = self.make(["软件"], ["軟件"])
        res, _ = rec(["img"], False)
        self.assertEqual(res, [("繁:软件", 0.9, "ch-word")])

    def test_prolonged_sound_mark_alone_is_not_kana(self):
        rec = self.make(["一一"], ["ーー"])
        res, _ = rec(["img"], False)
        self.assertEqual(res, [("繁:一一", 0.9, "ch-word")])

    def test_each_box_decided_separately(self):
        rec = self.make(["中文", "这样"], ["中文", "そうだ"])
        res, _ = rec(["a", "b"], False)
        self.assertEqual([r[0] for r in res], ["繁:中文", "そうだ"])

    def test_two_tuple_results(self):
        def ch_rec(imgs, return_word_box=False):
            return [("这", 0.9) for _ in imgs], 1.0

        def jp_rec(imgs, return_word_box=False):
            return [("これ", 0.8) for _ in imgs], 1.0

        res, _ = ocr_worker.combine_rec(ch_rec, jp_rec, lambda s: "繁:" + s)(["a"], False)
        self.assertEqual(res, [("これ", 0.8)])
        res, _ = ocr_worker.combine_rec(ch_rec, None, lambda s: "繁:" + s)(["a"], False)
        self.assertEqual(res, [("繁:这", 0.9)])

    def test_over_100_boxes_uses_chinese_only(self):
        rec = self.make(["这"] * 101, ["これ"] * 101)
        res, elapse = rec(list(range(101)), False)
        self.assertEqual([c[0] for c in self.calls], ["ch"])
        self.assertEqual(len(res), 101)
        self.assertTrue(all(r[0] == "繁:这" for r in res))
        self.assertEqual(elapse, 1.5)

    def test_exactly_100_boxes_uses_both(self):
        rec = self.make(["这"] * 100, ["これ"] * 100)
        res, _ = rec(list(range(100)), False)
        self.assertEqual([c[0] for c in self.calls], ["ch", "jp"])
        self.assertTrue(all(r[0] == "これ" for r in res))

    def test_without_japanese_model_uses_chinese(self):
        rec = self.make(["这"], ["これ"], jp=False)
        res, elapse = rec(["img"], False)
        self.assertEqual(res, [("繁:这", 0.9, "ch-word")])
        self.assertEqual(elapse, 1.5)


class RequirementsTest(unittest.TestCase):
    def test_all_packages_pinned(self):
        with open(os.path.join(OCR_DIR, "requirements.txt"), encoding="utf-8") as f:
            lines = [ln.strip() for ln in f if ln.strip() and not ln.strip().startswith("#")]
        pins = dict(ln.split("==", 1) for ln in lines)
        self.assertEqual(len(pins), len(lines), "每行都要用 == 鎖定版本")
        self.assertEqual(pins, {
            "flatbuffers": "25.12.19", "numpy": "2.5.4", "onnxruntime": "1.31.0",
            "opencc-python-reimplemented": "0.1.7", "opencv-python": "4.14.0.94", "packaging": "26.3",
            "pillow": "12.3.0", "protobuf": "7.36.2", "pyclipper": "1.4.0", "PyYAML": "6.0.3",
            "rapidocr-onnxruntime": "1.4.4", "shapely": "2.2.0", "six": "1.17.0", "tqdm": "4.70.1",
        })


def run_child(code, stdin=""):
    """在獨立的 Python 子行程執行 code（可 import ocr_worker），回傳 (stdout, stderr)。"""
    prelude = f"import sys; sys.path.insert(0, {OCR_DIR!r})\n"
    p = subprocess.run([sys.executable, "-I", "-c", prelude + textwrap.dedent(code)], input=stdin.encode(),
                       capture_output=True, timeout=60)
    if p.returncode != 0:
        raise AssertionError(f"子行程失敗 {p.returncode}：{p.stderr.decode()}")
    return p.stdout.decode(), p.stderr.decode()


class ProtocolStdoutTest(unittest.TestCase):
    def test_fd1_output_goes_to_stderr(self):
        out, err = run_child("""
            import os, ocr_worker
            proto = ocr_worker.protocol_stdout()
            os.write(1, b"c-library noise\\n")  # C 函式庫直接寫 fd 1
            print("python noise", flush=True)
            proto.write('{"ready": true}\\n')
            proto.flush()
        """)
        self.assertEqual(out, '{"ready": true}\n')
        self.assertIn("c-library noise", err)
        self.assertIn("python noise", err)

    def test_script_end_to_end_with_fake_modules(self):
        # 以假的 rapidocr／opencc／cv2／numpy 執行 worker 腳本本身：引擎初始化與辨識時寫 fd 1 的雜訊不會混入協定輸出；
        # 日文模型檔不存在時退回只用中文（簡轉繁仍套用）。
        worker = os.path.join(OCR_DIR, "ocr_worker.py")
        out, err = run_child(f"""
            import os, runpy, sys, types

            class FakeRapidOCR:
                def __init__(self, **kwargs):
                    os.write(1, b"init noise\\n")
                    self.text_rec = lambda imgs, return_word_box=False: ([("简体", 0.9) for _ in imgs], 0.1)

                def __call__(self, img, use_cls=True):
                    os.write(1, b"call noise\\n")
                    res, _ = self.text_rec([img], False)
                    box = [[0, 0], [10, 0], [10, 5], [0, 5]]
                    return [[box, text, score] for text, score in res], [0.1]

            class FakeOpenCC:
                def __init__(self, config):
                    pass

                def convert(self, s):
                    return "繁體" if s == "简体" else s

            def module(name, **attrs):
                m = types.ModuleType(name)
                m.__dict__.update(attrs)
                sys.modules[name] = m
                return m

            rec = module("rapidocr_onnxruntime.ch_ppocr_rec", TextRecognizer=None)
            module("rapidocr_onnxruntime", RapidOCR=FakeRapidOCR, ch_ppocr_rec=rec)
            module("opencc", OpenCC=FakeOpenCC)
            module("numpy", uint8="uint8", frombuffer=lambda data, dtype: data)
            module("cv2", IMREAD_COLOR=1, imdecode=lambda buf, flag: "image")
            runpy.run_path({worker!r}, run_name="__main__")
        """, stdin=request(1))
        lines = out.splitlines()
        self.assertEqual(json.loads(lines[0]), {"ready": True})
        self.assertEqual(json.loads(lines[1]), {"id": 1, "items": [{"box": [[0.0, 0.0], [10.0, 0.0], [10.0, 5.0], [0.0, 5.0]], "text": "繁體", "score": 0.9}]})
        self.assertEqual(len(lines), 2, out)
        self.assertIn("init noise", err)
        self.assertIn("call noise", err)
        self.assertIn("japan", err)


if __name__ == "__main__":
    unittest.main()
