// Package snowflake 比較 Discord snowflake ID（數值比較，不是字串比較）。
package snowflake

import "strconv"

// Valid 回傳 s 是否為合法的 snowflake（十進位無號 64 位元整數）。
func Valid(s string) bool {
	_, err := strconv.ParseUint(s, 10, 64)
	return err == nil
}

// Compare 以數值比較 a、b，回傳 -1、0、1。
// 不合法的值（空字串、非數字）不做字串比較：一律排在所有合法值之後，不合法的值彼此視為相等。
// 需要區分不合法值的呼叫端（例如設定時間點）應先用 Valid 檢查。
func Compare(a, b string) int {
	x, errA := strconv.ParseUint(a, 10, 64)
	y, errB := strconv.ParseUint(b, 10, 64)
	switch {
	case errA != nil && errB != nil:
		return 0
	case errA != nil:
		return 1
	case errB != nil:
		return -1
	case x < y:
		return -1
	case x > y:
		return 1
	}
	return 0
}

// Less 回傳 a 是否早於 b。
func Less(a, b string) bool { return Compare(a, b) < 0 }
