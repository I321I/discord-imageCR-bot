"""ocr_worker 的單元測試（標準函式庫 unittest；引擎、OpenCC、解碼都用假的，不需安裝 RapidOCR）。

執行：python3 -m unittest ocr/test_ocr_worker.py（在專案根目錄）
"""
import base64
import io
import json
import os
import sys
import types
import unittest
from unittest import mock

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

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
        ocr_worker.main(stdin, stdout, make_engine=lambda: engine, convert=convert, decode=decode)
    else:
        with mock.patch.object(sys, "stderr", io.StringIO()):  # 不讓 worker 的記錄干擾測試輸出
            ocr_worker.main(stdin, stdout, make_engine=lambda: engine, convert=convert, decode=decode)
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

        def make_engine():
            order.append(("engine", stdout.getvalue()))
            return FakeEngine()

        ocr_worker.main(io.StringIO(""), stdout, make_engine=make_engine, convert=str, decode=lambda b: b)
        self.assertEqual(order, [("engine", "")])
        self.assertEqual(json.loads(stdout.getvalue()), {"ready": True})

    def test_request_returns_converted_items(self):
        msgs, engine = run([request(7)])
        self.assertEqual(msgs[1], {"id": 7, "items": [{"box": BOX, "text": "轉:简体", "score": 0.97}]})
        self.assertEqual(engine.calls, [(("decoded", b"PNGDATA"), False)])

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
        self.assertEqual(msgs[1]["items"], [{"box": [[1.0, 2.0], [3.0, 2.0], [3.0, 4.0], [1.0, 4.0]], "text": "轉:字", "score": 0.5}])

    def test_each_stdout_line_is_json(self):
        stdout = io.StringIO()
        lines = [request(1), "garbage\n", request(2)]
        with mock.patch.object(sys, "stderr", io.StringIO()):
            ocr_worker.main(io.StringIO("".join(lines)), stdout, make_engine=FakeEngine, convert=str, decode=lambda b: b)
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

    def test_create_engine_passes_params(self):
        created = {}

        class FakeRapidOCR:
            def __init__(self, **kwargs):
                created.update(kwargs)

        fake = types.ModuleType("rapidocr_onnxruntime")
        fake.RapidOCR = FakeRapidOCR
        with mock.patch.dict(sys.modules, {"rapidocr_onnxruntime": fake}):
            engine = ocr_worker.create_engine()
        self.assertIsInstance(engine, FakeRapidOCR)
        self.assertEqual(created, ocr_worker.ENGINE_PARAMS)

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


if __name__ == "__main__":
    unittest.main()
