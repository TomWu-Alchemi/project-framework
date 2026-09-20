package util

import "unicode"

// IsAlphanumeric 检测字符串是否只包含数字和字母（Unicode 语义，见下）。
//
// 语义说明（P3-7，仅补注释、逻辑不变）：
//   - 字母/数字判定使用 unicode.IsLetter / unicode.IsDigit，按 Unicode 类别判断，
//     **不是 ASCII 白名单**：中文（"你好"）、希腊字母（"αβ"）、全角字母数字（"Ａ１"）
//     等非 ASCII 字母/数字都会被放行；
//   - 下划线、连字符、空格、标点、emoji 等非字母数字字符返回 false；
//   - 空字符串返回 false（保持现状）。
//
// 如需严格的 ASCII 校验（仅 [A-Za-z0-9]），请另提需求新增函数，
// 不要就地修改本函数（会静默改变现有调用方的语义）。
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
