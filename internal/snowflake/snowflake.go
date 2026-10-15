// Package snowflake 比較 Discord snowflake ID（數值比較，不是字串比較）。
package snowflake

import "strconv"

// Compare 以數值比較 a、b，回傳 -1、0、1；任一無法解析時退回字串比較。
func Compare(a, b string) int {
	x, errA := strconv.ParseUint(a, 10, 64)
	y, errB := strconv.ParseUint(b, 10, 64)
	if errA != nil || errB != nil {
		switch {
		case a < b:
			return -1
		case a > b:
			return 1
		}
		return 0
	}
	switch {
	case x < y:
		return -1
	case x > y:
		return 1
	}
	return 0
}

// Less 回傳 a 是否早於 b。
func Less(a, b string) bool { return Compare(a, b) < 0 }
