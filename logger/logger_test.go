package logger

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// attachBufferLogger 替换全局 logger 为内存 buffer（atomic.Pointer 版）。
func attachBufferLogger(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	encCfg := zap.NewProductionEncoderConfig()
	encCfg.TimeKey = ""
	core := zapcore.NewCore(zapcore.NewJSONEncoder(encCfg), zapcore.AddSync(buf), zapcore.DebugLevel)
	prev := logPtr.Load()
	logPtr.Store(zap.New(core, zap.AddCaller(), zap.AddCallerSkip(1)).Sugar())
	t.Cleanup(func() { logPtr.Store(prev) })
	return buf
}

func TestInfo_DoesNotLogArgsAsSingleSlice(t *testing.T) {
	buf := attachBufferLogger(t)
	Info("hello", 1)
	out := buf.String()
	if strings.Contains(out, "[hello 1]") {
		t.Fatalf("Info logged args as a single slice: %s", out)
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("Info output missing message: %s", out)
	}
	if !strings.Contains(out, "logger_test.go") {
		t.Fatalf("AddCallerSkip missing, caller not in test file: %s", out)
	}
}

func TestErrorf_Interpolates(t *testing.T) {
	buf := attachBufferLogger(t)
	Errorf("%s", "x")
	out := buf.String()
	if strings.Contains(out, "%s") {
		t.Fatalf("Errorf did not interpolate: %s", out)
	}
	if !strings.Contains(out, "x") {
		t.Fatalf("Errorf output missing interpolated value: %s", out)
	}
}

func TestLog_BeforeInitLogger_NoPanic(t *testing.T) {
	prev := logPtr.Load()
	logPtr.Store(nil)
	t.Cleanup(func() { logPtr.Store(prev) })

	Info("hello")
	Warn("warn")
	Error("err")
	Infof("%s", "x")
	Warnf("%s", "y")
	Errorf("%s", "z")
	StackedError(errors.New("stacked"))
}

// P3-16: nil error 不得产生任何输出。
func TestStackedError_NilErrorIsDropped(t *testing.T) {
	buf := attachBufferLogger(t)
	StackedError(nil)
	if buf.Len() != 0 {
		t.Fatalf("StackedError(nil) must log nothing, got: %s", buf.String())
	}
}

// F-12: InitLogger 幂等（第二次调用不替换 runtime、不重复建句柄）。
// 用 t.Chdir 把默认目录 ./log 隔离到临时目录，避免污染仓库。
//
// -count>1 语义：InitLogger 的 once 是进程级的。为让本用例在重复运行下每轮都
// 完整生效（等价于"模拟一次新进程的首次初始化"），轮首重置包级 initOnce；
// 重置后本轮 initLoggerWithDir 会走"先停旧 runtime"防御分支（回收上一轮句柄与
// 轮转 goroutine），该分支在生产路径因 once 而不可达，在此获得覆盖。
// 注意：重置包级 once 需独占，本用例禁止添加 t.Parallel。
func TestInitLogger_Idempotent(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Cleanup(Shutdown)

	initOnce = sync.Once{} // 模拟新进程：让本轮 InitLogger 真正执行一次初始化
	InitLogger()
	first := runtimePtr.Load()
	if first == nil {
		t.Fatal("InitLogger must publish a runtime")
	}
	Info("idempotent-line")

	InitLogger() // 第二次必须 no-op
	if got := runtimePtr.Load(); got != first {
		t.Fatalf("InitLogger must be idempotent: runtime %p replaced by %p", first, got)
	}
	if logPtr.Load() == nil {
		t.Fatal("global logger must stay initialized")
	}

	data, err := os.ReadFile(filepath.Join("log", "info", "info.log"))
	if err != nil {
		t.Fatalf("read info.log: %v", err)
	}
	if !strings.Contains(string(data), "idempotent-line") {
		t.Fatalf("info.log missing message: %s", data)
	}
}

// F-12 验收：initLoggerWithDir(t.TempDir()) → 各级别写日志 → 文件存在且内容正确。
func TestInitLoggerWithDir_WritesFiles(t *testing.T) {
	dir := t.TempDir()
	initLoggerWithDir(dir)
	t.Cleanup(Shutdown) // 先关闭句柄，Windows 上 t.TempDir 才能删除文件

	Info("info-line")
	Warn("warn-line")
	Error("error-line")

	access := GetAccessLog()
	if access == nil {
		t.Fatal("access logger not initialized")
	}
	access.Info("access-line")
	recovery := GetRecoveryLog()
	if recovery == nil {
		t.Fatal("recovery logger not initialized")
	}
	recovery.Info("recovery-line")
	dal := GetDalLog()
	if dal == nil {
		t.Fatal("dal logger not initialized")
	}
	dal.Info("dal-line")

	cases := []struct {
		rel  string
		want []string
	}{
		{"info/info.log", []string{"info-line"}},
		{"error/error.log", []string{"warn-line", "error-line"}},
		{"access/access.log", []string{"access-line"}},
		{"panic/panic.log", []string{"recovery-line"}},
		{"dal/dal.log", []string{"dal-line"}},
	}
	for _, tc := range cases {
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(tc.rel)))
		if err != nil {
			t.Fatalf("%s: %v", tc.rel, err)
		}
		for _, want := range tc.want {
			if !strings.Contains(string(data), want) {
				t.Fatalf("%s missing %q: %s", tc.rel, want, data)
			}
		}
	}

	// 低级日志不落 error.log，高级日志不落 info.log（级别分流不变）。
	infoData, _ := os.ReadFile(filepath.Join(dir, "info", "info.log"))
	if strings.Contains(string(infoData), "warn-line") {
		t.Fatalf("warn must not go to info.log: %s", infoData)
	}
}

// F-12 验收：Shutdown 幂等（连续两次不 panic、不重复关闭）。
func TestShutdown_Idempotent(t *testing.T) {
	initLoggerWithDir(t.TempDir())
	t.Cleanup(Shutdown)

	Info("before-shutdown")
	Shutdown()
	Shutdown() // 第二次必须是 no-op
}

// F-12 验收：Shutdown 后轮转 goroutine 退出。
func TestShutdown_StopsRotateGoroutine(t *testing.T) {
	initLoggerWithDir(t.TempDir())

	rt := runtimePtr.Load()
	if rt == nil {
		t.Fatal("initLoggerWithDir must publish a runtime")
	}

	Shutdown()

	select {
	case <-rt.rotateExited:
	case <-time.After(5 * time.Second):
		t.Fatal("rotation goroutine did not exit after Shutdown")
	}

	Shutdown() // 已停止后重复调用仍安全
}

func TestInitLoggerWithConfig_JSONNoStdoutCompress(t *testing.T) {
	dir := t.TempDir()
	initOnce = sync.Once{} // 模拟新进程：让本轮 InitLoggerWithConfig 真正执行
	InitLoggerWithConfig(LoggerConfig{
		Dir:        dir,
		JSON:       true,
		AlsoStdout: false,
		Compress:   true,
	})
	t.Cleanup(Shutdown)

	Info("json-line")
	if l := logPtr.Load(); l != nil {
		_ = l.Sync()
	}

	data, err := os.ReadFile(filepath.Join(dir, "info", "info.log"))
	if err != nil {
		t.Fatalf("read info.log: %v", err)
	}
	out := string(data)
	if !strings.Contains(out, `"level"`) {
		t.Fatalf("want JSON log containing %q, got: %s", `"level"`, out)
	}
	if !strings.Contains(out, "json-line") {
		t.Fatalf("info.log missing message: %s", out)
	}
	if GetLogger() == nil {
		t.Fatal("GetLogger must be published")
	}
	if !strings.Contains(out, `"caller"`) {
		t.Fatalf("default JSON log should include caller, got: %s", out)
	}

	rt := runtimePtr.Load()
	if rt == nil {
		t.Fatal("InitLoggerWithConfig must publish a runtime")
	}
	for _, w := range rt.writers {
		if !w.Compress {
			t.Fatalf("Compress not set on %s", w.Filename)
		}
	}
}

func TestInitLoggerWithConfig_DisableCaller(t *testing.T) {
	dir := t.TempDir()
	initOnce = sync.Once{}
	InitLoggerWithConfig(LoggerConfig{
		Dir:           dir,
		JSON:          true,
		AlsoStdout:    false,
		DisableCaller: true,
	})
	t.Cleanup(Shutdown)

	Info("no-caller-line")
	if l := logPtr.Load(); l != nil {
		_ = l.Sync()
	}
	data, err := os.ReadFile(filepath.Join(dir, "info", "info.log"))
	if err != nil {
		t.Fatalf("read info.log: %v", err)
	}
	out := string(data)
	if !strings.Contains(out, "no-caller-line") {
		t.Fatalf("missing message: %s", out)
	}
	if strings.Contains(out, `"caller"`) {
		t.Fatalf("DisableCaller must omit caller, got: %s", out)
	}
}
