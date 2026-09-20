package logger

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap/zapcore"
)

// P-3（日志缓冲写）测试。
//
// -race 说明：本机 `go test -race` 不可用（进程退出码 0xc0000139），本文件用例保持
// -race 兼容（不依赖时序断言，并发段用 WaitGroup 收敛、落盘断言只在 Shutdown 之后），
// 可在 CI / 其它环境用 `go test -race ./logger/` 复跑；本机验收改用
// `go test -count=20 -run 'TestP3' ./logger/`。
//
// initOnce 说明：InitLoggerWithConfig 走包级 initOnce（先到者生效），因此每个用例调用前
// 重置 initOnce 以模拟「新进程首次初始化」。本文件用例不得加 t.Parallel，否则会与其它
// 用例的 initOnce 重置互相干扰。
func initLoggerWithBufferConfig(t *testing.T, cfg LoggerConfig) string {
	t.Helper()
	dir := t.TempDir()
	cfg.Dir = dir
	initOnce = sync.Once{}
	InitLoggerWithConfig(cfg)
	t.Cleanup(Shutdown) // 兜底：先 flush / 停句柄，Windows 上 t.TempDir 才能删除文件
	return dir
}

func readLogFile(t *testing.T, dir, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

// 用例 a：BufferSize<=0 关闭缓冲 → 与既有同步语义一致，写入后无需 Shutdown 即可读到；
// 且不创建任何 BufferedWriteSyncer（配置层短路）。
func TestP3_BufferDisabled_WritesSynchronously(t *testing.T) {
	for _, size := range []int{0, -1} {
		dir := initLoggerWithBufferConfig(t, LoggerConfig{
			JSON:       true,
			AlsoStdout: false,
			BufferSize: size,
		})
		Info("sync-line")

		if out := readLogFile(t, dir, "info/info.log"); !strings.Contains(out, "sync-line") {
			t.Fatalf("BufferSize=%d: 关闭缓冲必须同步落盘, got: %q", size, out)
		}
		rt := runtimePtr.Load()
		if rt == nil {
			t.Fatal("runtime not published")
		}
		if len(rt.buffers) != 0 {
			t.Fatalf("BufferSize=%d: 关闭缓冲时不应创建任何缓冲, got %d", size, len(rt.buffers))
		}
	}
}

// 用例 b：BufferSize>0 + 少量写入 + FlushInterval=1h + 不 Shutdown →
// 数据仍在内存缓冲，文件不存在或为空（缓冲生效的直接证据；lumberjack 懒创建文件，
// 故两种结果都接受）。
func TestP3_BufferEnabled_NotOnDiskBeforeShutdown(t *testing.T) {
	dir := initLoggerWithBufferConfig(t, LoggerConfig{
		JSON:                true,
		AlsoStdout:          false,
		BufferSize:          64 * 1024,
		BufferFlushInterval: time.Hour, // 排除 ticker 干扰
	})
	Info("buffered-only-line")

	data, err := os.ReadFile(filepath.Join(dir, "info", "info.log"))
	if err == nil && len(strings.TrimSpace(string(data))) != 0 {
		t.Fatalf("缓冲写入在 flush 之前不应落盘, got: %q", data)
	}
	// 之后由 t.Cleanup(Shutdown) 兜底 flush 并关闭句柄。
}

// 用例 c：BufferSize>0 + 写入 + Shutdown() → Stop 的 flush 保证数据完整落盘。
func TestP3_BufferEnabled_ShutdownFlushesToDisk(t *testing.T) {
	dir := initLoggerWithBufferConfig(t, LoggerConfig{
		JSON:                true,
		AlsoStdout:          false,
		BufferSize:          64 * 1024,
		BufferFlushInterval: time.Hour,
	})
	Info("flush-on-shutdown-line")

	Shutdown()
	if out := readLogFile(t, dir, "info/info.log"); !strings.Contains(out, "flush-on-shutdown-line") {
		t.Fatalf("Shutdown 必须 flush 缓冲, got: %q", out)
	}
}

// 用例 d：BufferFlushInterval 生效 → 10ms 周期 + 不 Shutdown，数据也能在后台刷到文件。
func TestP3_BufferFlushInterval_FlushesInBackground(t *testing.T) {
	dir := initLoggerWithBufferConfig(t, LoggerConfig{
		JSON:                true,
		AlsoStdout:          false,
		BufferSize:          64 * 1024,
		BufferFlushInterval: 10 * time.Millisecond,
	})
	Info("interval-flushed-line")

	path := filepath.Join(dir, "info", "info.log")
	deadline := time.Now().Add(3 * time.Second)
	for {
		if data, err := os.ReadFile(path); err == nil && strings.Contains(string(data), "interval-flushed-line") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("BufferFlushInterval 未在 3s 内把数据刷到文件: %s", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// 用例 e：并发写（多 goroutine）+ 缓冲开启 + Shutdown → 数据完整（条数精确）。
func TestP3_BufferEnabled_ConcurrentWritesAllFlushedOnShutdown(t *testing.T) {
	dir := initLoggerWithBufferConfig(t, LoggerConfig{
		JSON:                true,
		AlsoStdout:          false,
		BufferSize:          8 * 1024,
		BufferFlushInterval: time.Hour,
	})

	const (
		goroutines   = 16
		perGoroutine = 64
	)
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				Info("concurrent-buffered-line")
			}
		}()
	}
	wg.Wait()

	Shutdown()

	out := readLogFile(t, dir, "info/info.log")
	if got, want := strings.Count(out, "concurrent-buffered-line"), goroutines*perGoroutine; got != want {
		t.Fatalf("Shutdown 后应完整落盘 %d 条, got %d", want, got)
	}
}

// 用例 f：5 个文件 writer 各自独立一个 buffer；Shutdown 后五路日志均落盘；
// 二次 Shutdown 幂等（buffers.Stop 幂等）不 panic。
func TestP3_BufferEnabled_FiveIndependentBuffersAndDoubleShutdown(t *testing.T) {
	dir := initLoggerWithBufferConfig(t, LoggerConfig{
		JSON:                true,
		AlsoStdout:          false,
		BufferSize:          64 * 1024,
		BufferFlushInterval: time.Hour,
	})

	Info("info-buf-line")
	Warn("error-buf-line")
	if l := GetAccessLog(); l != nil {
		l.Info("access-buf-line")
	}
	if l := GetRecoveryLog(); l != nil {
		l.Info("panic-buf-line")
	}
	if l := GetDalLog(); l != nil {
		l.Info("dal-buf-line")
	}

	rt := runtimePtr.Load()
	if rt == nil {
		t.Fatal("runtime not published")
	}
	if len(rt.buffers) != 5 {
		t.Fatalf("应为 info/error/access/panic/dal 各建一个缓冲, want 5 got %d", len(rt.buffers))
	}
	seen := make(map[*zapcore.BufferedWriteSyncer]bool, len(rt.buffers))
	for _, b := range rt.buffers {
		if seen[b] {
			t.Fatal("5 个文件 writer 必须各自独立一个 buffer，不能共用")
		}
		seen[b] = true
	}

	Shutdown()

	cases := map[string]string{
		"info/info.log":     "info-buf-line",
		"error/error.log":   "error-buf-line",
		"access/access.log": "access-buf-line",
		"panic/panic.log":   "panic-buf-line",
		"dal/dal.log":       "dal-buf-line",
	}
	for rel, want := range cases {
		if out := readLogFile(t, dir, rel); !strings.Contains(out, want) {
			t.Fatalf("%s 缺少 %q: %q", rel, want, out)
		}
	}

	Shutdown() // 二次调用必须幂等、不 panic
}
