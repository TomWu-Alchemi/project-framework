package logger

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"
)

// P-2 并发基线：多个 goroutine 同时向包级 Info/Warn/Error（info + error Core）
// 以及 access / recovery / dal 三个独立 Core 写入，验证 5 个 Core 各自持有独立
// encoder.Clone() 后并发写不 panic、不产生坏输出。
//
// 断言范围：
//   - 不 panic；结束后 Shutdown 幂等安全。
//   - 输出完整性：5 个日志文件均非空，且每条记录独占一行、无空行；
//     **不断言行序、行数**（并发交错下二者本就不确定）。
//
// 关于 -race：本机 `go test -race` 不可用（进程退出码 0xc0000139）。本用例保持
// -race 兼容，CI/其它环境可用 `go test -race -run TestConcurrentLoggers_NoPanic ./logger/`
// 复跑；本机验收改用 `go test -count=20 -run TestConcurrentLoggers_NoPanic ./logger/`。
//
// 目录必须重定向到 t.TempDir()，不得在副本内生成 log/ 目录。
func TestConcurrentLoggers_NoPanic(t *testing.T) {
	dir := t.TempDir()
	initLoggerWithDir(dir)
	t.Cleanup(Shutdown) // 兜底：先关句柄，Windows 上 t.TempDir 才能删除文件

	const (
		goroutines   = 16
		perGoroutine = 32
	)

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(id int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				Info("info", id, i)
				Warn("warn", id, i)
				Error("error", id, i)

				if l := GetAccessLog(); l != nil {
					l.Info("access", zap.Int("g", id), zap.Int("i", i))
				}
				if l := GetRecoveryLog(); l != nil {
					l.Info("recovery", zap.Int("g", id), zap.Int("i", i))
				}
				if l := GetDalLog(); l != nil {
					l.Info("dal", zap.Int("g", id), zap.Int("i", i))
				}
			}
		}(g)
	}
	wg.Wait()

	Shutdown()

	for _, rel := range []string{
		"info/info.log",
		"error/error.log",
		"access/access.log",
		"panic/panic.log",
		"dal/dal.log",
	} {
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if len(data) == 0 {
			t.Fatalf("%s must not be empty", rel)
		}
		lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
		for i, line := range lines {
			if strings.TrimSpace(line) == "" {
				t.Fatalf("%s has an empty line at %d", rel, i+1)
			}
		}
	}
}
