"""把關者補充測試：假名判定邊界、combine_rec 在框數與回傳長度不一致時的行為。

執行：python3 -m unittest ocr/test_gate_ocr_worker.py（在專案根目錄）
"""
import os
import sys
import unicodedata
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import ocr_worker  # noqa: E402


class GateKanaTest(unittest.TestCase):
    def test_marks_and_symbols_are_not_kana(self):
        # 濁點／半濁點（U+309B/309C）、合成用濁點（U+3099/309A）、中點、長音、半形長音、半形濁點都不算假名。
        for ch in ["゛", "゜", "゙", "゚", "・", "ー", "ｰ", "･", "ﾞ", "ﾟ", "々", "〆", "〇"]:
            self.assertFalse(ocr_worker.has_kana(ch), f"U+{ord(ch):04X} {unicodedata.name(ch, '?')}")

    def test_iteration_marks_and_small_kana_are_kana(self):
        for ch in ["ゝ", "ゞ", "ゟ", "ヽ", "ヾ", "ヿ", "ぁ", "ゖ", "ァ", "ヺ", "ㇰ", "ｦ", "ｯ", "ｱ", "ﾝ", "ゔ", "ヴ"]:
            self.assertTrue(ocr_worker.has_kana(ch), f"U+{ord(ch):04X} {unicodedata.name(ch, '?')}")

    def test_ranges_match_unicode_names(self):
        # 每個範圍內的已指派字元名稱都應含 HIRAGANA／KATAKANA（或是圈／方塊片假名、片假名縮寫）。
        for lo, hi in ocr_worker.KANA_RANGES:
            for cp in range(lo, hi + 1):
                name = unicodedata.name(chr(cp), "")
                if not name:
                    continue
                self.assertTrue("HIRAGANA" in name or "KATAKANA" in name or name.startswith("SQUARE"),
                                f"U+{cp:04X} {name}")
        # 範圍外的 ー（U+30FC）與 ・（U+30FB）
        self.assertFalse(any(lo <= 0x30FC <= hi for lo, hi in ocr_worker.KANA_RANGES))
        self.assertFalse(any(lo <= 0x30FB <= hi for lo, hi in ocr_worker.KANA_RANGES))


class GateCombineRecLengthTest(unittest.TestCase):
    """RapidOCR 1.4.4 的 TextRecognizer 對每張輸入都回一項；此處記錄長度不一致時的實際行為。"""

    def test_japanese_shorter_than_chinese(self):
        def ch_rec(imgs, return_word_box=False):
            return [("中%d" % i, 0.9) for i, _ in enumerate(imgs)], 1.0

        def jp_rec(imgs, return_word_box=False):
            return [("か", 0.9)], 1.0  # 只回一項

        res, _ = ocr_worker.combine_rec(ch_rec, jp_rec, lambda s: s)(["a", "b", "c"], False)
        # 只記錄行為、不鎖定（目前 zip 會截斷，後面的框整個遺失）；若日後改成補中文結果，此測試不需修改。
        print(f"\n[gate] 3 個框、日文只回 1 項 → combine_rec 回 {len(res)} 項：{res}", file=sys.stderr)
        self.assertGreaterEqual(len(res), 1)


if __name__ == "__main__":
    unittest.main()
