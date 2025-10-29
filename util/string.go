package util

import "unicode"

// IsAlphanumeric 检测字符串是否只包含数字和字母
func IsAlphanumeric(s string) bool {
	if len(s) == 0 {
		return false // 空字符串可以根据需求决定返回true或false
	}

	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}
