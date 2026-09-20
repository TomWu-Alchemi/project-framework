package util

import (
	"strconv"
	"time"
)

func ParseTimestampToStr(timestampStr string) (string, error) {
	timestamp, err := strconv.ParseInt(timestampStr, 10, 64)
	if err != nil {
		return "", err
	}

	// 复用 ParseTimestamp 的统一分级逻辑（秒/毫秒/微秒/纳秒）
	t, err := ParseTimestamp(timestamp)
	if err != nil {
		return "", err
	}

	return t.Format(time.DateTime), nil
}

func ParseTimestampToTime(timestampStr string) (time.Time, error) {
	timestamp, err := strconv.ParseInt(timestampStr, 10, 64)
	if err != nil {
		return time.Time{}, err
	}

	// 复用 ParseTimestamp 的统一分级逻辑（秒/毫秒/微秒/纳秒）
	return ParseTimestamp(timestamp)
}

// ParseLocalTime 将时间字符串解析为本地时间
func ParseLocalTime(layout, timeStr string) (time.Time, error) {
	// time.Local 即 LoadLocation("Local") 的语义，直接使用可省去查找与错误忽略。
	return time.ParseInLocation(layout, timeStr, time.Local)
}

// GetTodayMidnight 获取"当日 0 点"的时间对象。
//
// 参数语义（P3-9，仅补注释、逻辑不变）：变参 t 用于模拟"可选参数"（Go 无默认参数），
// 只读取第一个元素：
//   - 不传参（含显式传 nil）：以 time.Now() 为基准；
//   - 传 1 个参数（或 t[0] 有效）：以 t[0] 所在日期为基准；
//   - 传入多个参数：仅 t[0] 生效，其余被忽略（调用方不应传入多个）。
//
// 返回值保留基准时间的时区（time.Date 使用 now.Location()），不做 UTC 转换。
//
// API 备注：该变参写法属历史设计（API 异味）；本批只补注释，不改签名。
// 后续主版本可考虑拆为 GetTodayMidnight() 与 GetMidnightOf(t time.Time) 两个显式函数（P3-9）。
func GetTodayMidnight(t ...time.Time) time.Time {
	var now time.Time
	if len(t) > 0 {
		now = t[0]
	} else {
		now = time.Now()
	}

	year, month, day := now.Date()
	return time.Date(year, month, day, 0, 0, 0, 0, now.Location())
}

// ParseTimestamp 将 int64 时间戳按数值绝对值自动分级解析为 time.Time：
//
//	|ts| < 1e10             秒级（10 位及以内）
//	|ts| < 1e13             毫秒级（11-13 位）
//	|ts| < 1e16             微秒级（14-16 位）
//	其余（int64 上限 19 位）  纳秒级（17-19 位）
//
// 采用整数阈值比较而非 float64 math.Log10，避免 10^n 边界的浮点误差。
// int64 最多 19 位，不存在 20 位取值，故无需位数超限分支；超长字符串输入由
// strconv.ParseInt 天然报错。error 返回值仅用于保持既有函数签名，当前恒为 nil。
func ParseTimestamp(timestamp int64) (time.Time, error) {
	switch {
	case timestamp > -10000000000 && timestamp < 10000000000:
		// 秒级时间戳（10 位或更少）
		return time.Unix(timestamp, 0), nil
	case timestamp > -10000000000000 && timestamp < 10000000000000:
		// 毫秒级时间戳（11-13 位）
		return time.Unix(timestamp/1000, (timestamp%1000)*1000000), nil
	case timestamp > -10000000000000000 && timestamp < 10000000000000000:
		// 微秒级时间戳（14-16 位）
		return time.Unix(timestamp/1000000, (timestamp%1000000)*1000), nil
	default:
		// 纳秒级时间戳（17-19 位）
		return time.Unix(timestamp/1000000000, timestamp%1000000000), nil
	}
}
