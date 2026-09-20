package logger

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
)

// P-3（缓冲写）前后对照基准。
//
// 方案说明：InitLogger / InitLoggerWithConfig / initLoggerWithDir 共享包级 initOnce，
// 同一进程内无法用它们初始化「同步写」与「缓冲写」两套 logger，因此这里下沉到
// WriteSyncer 层直接对照：同一份 lumberjack 文件 writer，一组直接 AddSync（等价
// BufferSize=0 的旧路径），一组套 zapcore.BufferedWriteSyncer（BufferSize=256KiB）。
// 这正是 Core 的真实写路径（zap 的 ioCore 只调用 WriteSyncer.Write(编码后的整条日志)）。
//
// 输出目录统一用 b.TempDir()，不生成 log/ 目录。
//
// 运行方式（-benchtime 用于收敛临时文件体积）：
//
//	go test -run '^$' -bench 'BenchmarkWriteSyncer_SmallPayload' -benchmem -benchtime=200ms ./logger/
//	go test -run '^$' -bench 'BenchmarkBufferBoundary' -benchmem -benchtime=50000x ./logger/
const benchSmallPayloadLine = `{"level":"info","ts":"2026-09-17 12:00:00.000","caller":"bench/bench.go:42","logger":"app","msg":"small high-frequency log line","k":"v"}`

const benchFileBufferSize = 256 * 1024

// countingWriteSyncer 装饰底层 WriteSyncer，统计「底层 Write 被调用的次数」。
// inner 为 nil 时只计数、不落盘（用于隔离缓冲行为）。
// sink-write/op 指标直接量化「缓冲是否把多条日志合并成一次底层写」。
type countingWriteSyncer struct {
	inner zapcore.WriteSyncer // nil => 只计数
	calls int
}

func (c *countingWriteSyncer) Write(p []byte) (int, error) {
	c.calls++
	if c.inner != nil {
		return c.inner.Write(p)
	}
	return len(p), nil
}

func (c *countingWriteSyncer) Sync() error {
	if c.inner != nil {
		return c.inner.Sync()
	}
	return nil
}

// 小日志高频：缓冲收益的主战场（同步写 = 每条一次 write(2)）。
func benchFileWrite(b *testing.B, buffered bool, payload []byte, bufferSize int) {
	b.Helper()
	w := &lumberjack.Logger{
		Filename:  filepath.Join(b.TempDir(), "bench.log"),
		MaxSize:   4096, // MB，远大于基准写入量，避免轮转噪声
		LocalTime: true,
	}
	defer func() { _ = w.Close() }() // 先注册 → 后执行（在 buffer.Stop 之后关闭句柄）

	counter := &countingWriteSyncer{inner: zapcore.AddSync(w)}
	var ws zapcore.WriteSyncer = counter
	if buffered {
		bws := &zapcore.BufferedWriteSyncer{
			WS:            counter,
			Size:          bufferSize,
			FlushInterval: time.Hour, // 只观察「缓冲满才落盘」，排除 ticker 干扰
		}
		ws = bws
		defer func() { _ = bws.Stop() }() // 后注册 → 先执行：先 flush 再 Close
	}

	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ws.Write(payload); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	if err := ws.Sync(); err != nil {
		b.Fatal(err)
	}
	b.ReportMetric(float64(counter.calls)/float64(b.N), "sink-write/op")
}

func BenchmarkWriteSyncer_SmallPayload_Sync(b *testing.B) {
	benchFileWrite(b, false, []byte(benchSmallPayloadLine), 0)
}

func BenchmarkWriteSyncer_SmallPayload_Buffered(b *testing.B) {
	benchFileWrite(b, true, []byte(benchSmallPayloadLine), benchFileBufferSize)
}

// 收益边界：payload 大于缓冲可用空间且缓冲为空时，bufio 直写底层——大 payload
// （访问日志 body，256KiB 上限量级）不过缓冲，缓冲无法减少底层 Write 次数。
//
// 此处的 sink 只计数、无写成本，因此 ns/op 只反映缓冲包装自身的固定开销
// （互斥 + bufio 分支）；关键是看 sink-write/op：同步组与缓冲组都等于 1，
// 即大 payload 场景缓冲没有任何合并效果。生产中的瓶颈是底层 write(2)，
// 这笔成本两组相同、缓冲省不掉，故大 payload 不应期待收益。
//
// 必须配 -benchtime=100x（256KiB × b.N 会撑爆临时盘；本组用内存 sink，无磁盘占用）。
func benchLargePayload(b *testing.B, buffered bool) {
	payload := bytes.Repeat([]byte("x"), 256*1024)
	counter := &countingWriteSyncer{} // inner nil：只计数
	var ws zapcore.WriteSyncer = counter
	if buffered {
		bws := &zapcore.BufferedWriteSyncer{
			WS:            counter,
			Size:          8 * 1024, // 远小于 payload，强制走直写路径
			FlushInterval: time.Hour,
		}
		ws = bws
		defer func() { _ = bws.Stop() }()
	}

	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ws.Write(payload); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(counter.calls)/float64(b.N), "sink-write/op")
}

func BenchmarkBufferBoundary_LargePayload_Sync(b *testing.B) {
	benchLargePayload(b, false)
}

func BenchmarkBufferBoundary_LargePayload_Buffered(b *testing.B) {
	benchLargePayload(b, true)
}
