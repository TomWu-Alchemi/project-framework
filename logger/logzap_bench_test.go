package logger

import (
	"io"
	"net/http"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// 本文件承载批 2（P-1.3 / P-1.4）的前后对照 benchmark。
//
// 约定：
//   - 所有 bench 输出到 io.Discard，避免任何 I/O 干扰编码本身的成本度量。
//   - bench 数据固定为同一份，保证 Old/New 可比。
//   - 编码器默认用 Console（库当前默认形态），与线上 access 日志一致。

// benchHeaders 返回一份"典型请求头"：15 个键、含 1 个敏感头（Authorization）。
func benchHeaders() http.Header {
	h := http.Header{}
	h.Set("Accept", "application/json")
	h.Set("Accept-Encoding", "gzip, deflate, br")
	h.Set("Accept-Language", "en-US,en;q=0.9")
	h.Set("Authorization", "Bearer bench-secret-token")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("Content-Length", "512")
	h.Set("Content-Type", "application/json")
	h.Set("Host", "example.com")
	h.Set("Postman-Token", "11111111-2222-3333-4444-555555555555")
	h.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64)")
	h.Set("X-Forwarded-For", "203.0.113.7, 198.51.100.9")
	h.Set("X-Real-Ip", "203.0.113.7")
	h.Set("X-Request-Id", "req-bench-0001")
	h.Set("X-Trace-Id", "trace-bench-0001")
	return h
}

// benchDiscardLogger 构造一个写往 io.Discard 的、级别足够的 logger。
func benchDiscardLogger(enc zapcore.Encoder) *zap.Logger {
	return zap.New(zapcore.NewCore(enc, zapcore.AddSync(io.Discard), zapcore.InfoLevel))
}

// BenchmarkFilterSensitiveDataForJson 度量 P-1.3：中等 JSON body（嵌套结构 +
// password + 大于 2^53 的大整数）的脱敏编码成本。
func BenchmarkFilterSensitiveDataForJson(b *testing.B) {
	const body = `{"user":{"id":9007199254740993,"name":"alice","contact":{"email":"alice@example.com","phone":"+1-555-0100"}},"password":"bench-secret","items":[{"sku":"A-1001","qty":3,"price":19.99},{"sku":"B-2002","qty":1,"price":99.5}],"note":"medium sized json body for the access log masking benchmark"}`
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = filterSensitiveDataForJson(body)
	}
}

// BenchmarkAccessLogHeaders_Old 度量 P-1.4 旧路径：每请求构造过滤 map
// （make + 遍历）后再经 zap.Any 反射/JSON 编码。
//
// 注意：改代码前仅存在本用例，基线数字在批 2 实施前采集；实施后新增
// BenchmarkAccessLogHeaders_New 对照。
func BenchmarkAccessLogHeaders_Old(b *testing.B) {
	h := benchHeaders()
	l := benchDiscardLogger(zapcore.NewConsoleEncoder(zap.NewProductionEncoderConfig()))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.Info("http", zap.Any("headers", filterSensitiveHeaders(h)))
	}
}

// BenchmarkAccessLogHeaders_New 度量 P-1.4 新路径：zap.Object 直接编码原 header，
// 无中间过滤 map、无反射。与 Old 使用同一份 header，便于逐项对照。
func BenchmarkAccessLogHeaders_New(b *testing.B) {
	h := benchHeaders()
	l := benchDiscardLogger(zapcore.NewConsoleEncoder(zap.NewProductionEncoderConfig()))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.Info("http", zap.Object("headers", headerLogObject(h)))
	}
}
