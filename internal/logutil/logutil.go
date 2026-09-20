// Package logutil 提供 module 内的日志载荷截断工具（F-14）：
// 由 httpclient / logger / rpc 三个包共用，语义与原先各自的私有实现逐字一致。
//
// 该包位于 internal/，仅对本 module 可见，不属对外公开 API。
package logutil

import (
	"bytes"
	"strings"

	"go.uber.org/zap"
)

// MaxLogBytes 是单条日志载荷（body / response / header 等）记录的最大字节数，
// 超过部分截断（三个调用方原私有常量 256KiB 的统一来源）。
const MaxLogBytes = 256 << 10

// TruncateBytes 返回可安全写入日志的字节切片：size <= MaxLogBytes 时原样返回
// （不克隆），否则返回前 MaxLogBytes 字节的克隆，避免与调用方后续修改共享底层数组。
// truncated 表示是否发生截断，size 恒为原始长度。
func TruncateBytes(b []byte) (logged []byte, truncated bool, size int) {
	size = len(b)
	if size <= MaxLogBytes {
		return b, false, size
	}
	return bytes.Clone(b[:MaxLogBytes]), true, size
}

// TruncateString 与 TruncateBytes 语义相同，针对字符串。
func TruncateString(s string) (logged string, truncated bool, size int) {
	size = len(s)
	if size <= MaxLogBytes {
		return s, false, size
	}
	return strings.Clone(s[:MaxLogBytes]), true, size
}

// ZapTruncatedBytes 返回三个 zap 字段：载荷 key、<key>_truncated、<key>_size。
func ZapTruncatedBytes(key string, b []byte) []zap.Field {
	logged, truncated, size := TruncateBytes(b)
	return []zap.Field{
		zap.ByteString(key, logged),
		zap.Bool(key+"_truncated", truncated),
		zap.Int(key+"_size", size),
	}
}

// ZapTruncatedString 返回三个 zap 字段：载荷 key、<key>_truncated、<key>_size。
func ZapTruncatedString(key string, s string) []zap.Field {
	logged, truncated, size := TruncateString(s)
	return []zap.Field{
		zap.String(key, logged),
		zap.Bool(key+"_truncated", truncated),
		zap.Int(key+"_size", size),
	}
}
