package util

import (
	"fmt"
	"math"
	"strconv"
	"time"
)

func ParseTimestampToStr(timestampStr string) (string, error) {
	timestamp, err := strconv.ParseInt(timestampStr, 10, 64)
	if err != nil {
		return "", err
	}

	// 判断是否为毫秒级时间戳
	if len(timestampStr) > 10 {
		timestamp = timestamp / 1000
	}

	return time.Unix(timestamp, 0).Format(time.DateTime), nil
}

func ParseTimestampToTime(timestampStr string) (time.Time, error) {
	timestamp, err := strconv.ParseInt(timestampStr, 10, 64)
	if err != nil {
		return time.Time{}, err
	}

	// 判断是否为毫秒级时间戳
	if len(timestampStr) > 10 {
		return time.Unix(0, timestamp*int64(time.Millisecond)), nil
	}

	return time.Unix(timestamp, 0), nil
}

// ParseLocalTime 将时间字符串解析为本地时间
func ParseLocalTime(layout, timeStr string) (time.Time, error) {
	loc, _ := time.LoadLocation("Local")
	return time.ParseInLocation(layout, timeStr, loc)
}

// GetTodayMidnight 获取当日0点的时间对象
// 如果传入参数t，则基于t所在日期的0点；如果不传入参数，则基于当前时间的0点
// 保留原始时间的时区信息
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

func ParseTimestamp(timestamp int64) (time.Time, error) {
	// 计算时间戳的位数（绝对值）
	digits := 0
	if timestamp == 0 {
		digits = 1
	} else {
		digits = int(math.Log10(math.Abs(float64(timestamp)))) + 1
	}

	switch {
	case digits <= 10:
		// 秒级时间戳（10位或更少）
		return time.Unix(timestamp, 0), nil
	case digits <= 13:
		// 毫秒级时间戳（11-13位）
		return time.Unix(timestamp/1000, (timestamp%1000)*1e6), nil
	case digits <= 16:
		// 微秒级时间戳（14-16位）
		return time.Unix(timestamp/1e6, (timestamp%1e6)*1e3), nil
	case digits <= 19:
		// 纳秒级时间戳（17-19位）
		return time.Unix(timestamp/1e9, timestamp%1e9), nil
	default:
		// 超出合理范围（>19位）
		return time.Time{}, fmt.Errorf("timestamp %d out of range (max 19 digits)", timestamp)
	}
}
