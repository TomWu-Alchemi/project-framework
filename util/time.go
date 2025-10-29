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
