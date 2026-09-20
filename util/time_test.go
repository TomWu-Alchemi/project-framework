package util

import (
	"strconv"
	"testing"
	"time"
)

// 时间断言统一用 time.Unix(...) 构造期望值并调用 time.Time.Equal 比较；
// 字符串断言用 time.Unix(...).Format(time.DateTime) 动态生成，避免硬编码时区结果。

// assertTimestamp 校验单个时间戳字符串在三个解析入口下均等于期望时间。
func assertTimestamp(t *testing.T, ts string, want time.Time) {
	t.Helper()

	v, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		t.Fatalf("ParseInt(%q) 出错: %v", ts, err)
	}

	got, err := ParseTimestamp(v)
	if err != nil {
		t.Fatalf("ParseTimestamp(%q) 出错: %v", ts, err)
	}
	if !got.Equal(want) {
		t.Fatalf("ParseTimestamp(%q) = %v, 期望 %v", ts, got, want)
	}

	gotTime, err := ParseTimestampToTime(ts)
	if err != nil {
		t.Fatalf("ParseTimestampToTime(%q) 出错: %v", ts, err)
	}
	if !gotTime.Equal(want) {
		t.Fatalf("ParseTimestampToTime(%q) = %v, 期望 %v", ts, gotTime, want)
	}

	gotStr, err := ParseTimestampToStr(ts)
	if err != nil {
		t.Fatalf("ParseTimestampToStr(%q) 出错: %v", ts, err)
	}
	if wantStr := want.Format(time.DateTime); gotStr != wantStr {
		t.Fatalf("ParseTimestampToStr(%q) = %q, 期望 %q", ts, gotStr, wantStr)
	}
}

// TestParseTimestamp_FourLevels 覆盖秒/毫秒/微秒/纳秒四档输入，
// 三个解析入口结果必须一致，且等于 time.Unix(1700000000, 0)。
func TestParseTimestamp_FourLevels(t *testing.T) {
	want := time.Unix(1700000000, 0)

	cases := []struct {
		name string
		ts   string
	}{
		{"秒级", "1700000000"},
		{"毫秒级", "1700000000000"},
		{"微秒级", "1700000000000000"},
		{"纳秒级", "1700000000000000000"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertTimestamp(t, tc.ts, want)
		})
	}
}

// TestParseTimestamp_Boundaries 覆盖零值、负值、负微秒、分级阈值与 int64 极值。
func TestParseTimestamp_Boundaries(t *testing.T) {
	cases := []struct {
		name string
		ts   string
		want time.Time
	}{
		{"零值", "0", time.Unix(0, 0)},
		{"负一秒", "-1", time.Unix(-1, 0)},
		{"负微秒", "-1700000000000000", time.Unix(-1700000000, 0)},
		// 分级阈值：|ts| 达到 1eN 即进入下一档；1e10 毫秒 = 1e13 微秒 = 1e16 纳秒 = 1e7 秒
		{"1e10 属毫秒档", "10000000000", time.Unix(10000000, 0)},
		{"-1e10 属毫秒档", "-10000000000", time.Unix(-10000000, 0)},
		{"1e13 属微秒档", "10000000000000", time.Unix(10000000, 0)},
		{"1e16 属纳秒档", "10000000000000000", time.Unix(10000000, 0)},
		// 1e16-1 在 float64 中会四舍五入为 1e16（旧 Log10 实现会误判纳秒档），整数阈值应判微秒档
		{"1e16-1 属微秒档", "9999999999999999", time.Unix(9999999999, 999999000)},
		// int64 最小值为纳秒档：-9223372036.854775808s 的规范化表示
		{"int64 最小值", "-9223372036854775808", time.Unix(-9223372037, 145224192)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertTimestamp(t, tc.ts, tc.want)
		})
	}
}

// TestParseTimestamp_SubSecond 覆盖毫秒/微秒/纳秒三档的亚秒余量（含负值）。
func TestParseTimestamp_SubSecond(t *testing.T) {
	cases := []struct {
		name string
		ts   string
		want time.Time
	}{
		{"毫秒含余量", "1700000000123", time.Unix(1700000000, 123000000)},
		{"微秒含余量", "1700000000000123", time.Unix(1700000000, 123000)},
		{"纳秒含余量", "1700000000000000123", time.Unix(1700000000, 123)},
		// -1700000000.123s（负值取模为负，由 time.Unix 规范化）
		{"负毫秒含余量", "-1700000000123", time.Unix(-1700000001, 877000000)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertTimestamp(t, tc.ts, tc.want)
		})
	}
}

// TestParseTimestampToStr_InvalidString 覆盖超长/非法字符串输入：
// int64 最多 19 位，20 位字符串与 19 位但超出 int64 上限的输入均由 strconv.ParseInt 报错；
// ParseTimestamp 入参为 int64，不存在 20 位取值（旧「>19 位」分支已删除）。
func TestParseTimestampToStr_InvalidString(t *testing.T) {
	cases := []struct {
		name string
		ts   string
	}{
		{"20 位字符串", "17000000000000000000"},
		{"19 位但超出 int64 上限", "9999999999999999999"},
		{"空字符串", ""},
		{"非数字", "not-a-timestamp"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := ParseTimestampToStr(tc.ts); err == nil {
				t.Fatalf("ParseTimestampToStr(%q) 期望报错，实际返回 %q", tc.ts, got)
			}
			if got, err := ParseTimestampToTime(tc.ts); err == nil {
				t.Fatalf("ParseTimestampToTime(%q) 期望报错，实际返回 %v", tc.ts, got)
			}
		})
	}
}

// TestParseLocalTime_LocalZone 锁定 A6-4：ParseLocalTime 等价于以 time.Local
// 解析（旧实现 LoadLocation("Local") 后忽略 error），且结果等于直接构造的本地时间。
func TestParseLocalTime_LocalZone(t *testing.T) {
	const layout = "2006-01-02 15:04:05"
	const in = "2024-03-01 08:30:00"

	got, err := ParseLocalTime(layout, in)
	if err != nil {
		t.Fatalf("ParseLocalTime(%q, %q) 出错: %v", layout, in, err)
	}
	want := time.Date(2024, 3, 1, 8, 30, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Fatalf("ParseLocalTime = %v, 期望 %v", got, want)
	}
	// 核心不变量：与显式 time.ParseInLocation(time.Local) 一致（含时区）。
	alt, err := time.ParseInLocation(layout, in, time.Local)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(alt) {
		t.Fatalf("与 ParseInLocation(time.Local) 不一致: got=%v alt=%v", got, alt)
	}
	if _, err := ParseLocalTime(layout, "not-a-time"); err == nil {
		t.Fatal("非法时间串应返回错误")
	}
}
