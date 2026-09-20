package logger

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func TestNewGormLogger_IgnoreRecordNotFound(t *testing.T) {
	l := NewGormLogger(zap.NewNop())
	if !l.IgnoreRecordNotFoundError {
		t.Fatal("IgnoreRecordNotFoundError should default to true")
	}
}

func TestGormLogger_InfoUsesInfoLevel(t *testing.T) {
	buf := &bytes.Buffer{}
	encCfg := zap.NewProductionEncoderConfig()
	encCfg.TimeKey = ""
	core := zapcore.NewCore(zapcore.NewJSONEncoder(encCfg), zapcore.AddSync(buf), zapcore.DebugLevel)
	l := NewGormLogger(zap.New(core))
	l.LogLevel = gormlogger.Info
	l.Info(context.Background(), "hello %s", "world")
	out := buf.String()
	if !strings.Contains(out, `"level":"info"`) {
		t.Fatalf("want info log, got %s", out)
	}
	if !strings.Contains(out, "hello world") {
		t.Fatalf("missing message: %s", out)
	}
}

func TestGormLogger_TraceInfoUsesInfo(t *testing.T) {
	buf := &bytes.Buffer{}
	encCfg := zap.NewProductionEncoderConfig()
	encCfg.TimeKey = ""
	core := zapcore.NewCore(zapcore.NewJSONEncoder(encCfg), zapcore.AddSync(buf), zapcore.DebugLevel)
	l := NewGormLogger(zap.New(core))
	l.LogLevel = gormlogger.Info
	l.SlowThreshold = time.Hour
	l.Trace(t.Context(), time.Now(), func() (string, int64) { return "select 1", 1 }, nil)
	out := buf.String()
	if !strings.Contains(out, `"level":"info"`) {
		t.Fatalf("want info trace, got %s", out)
	}
	if strings.Contains(out, `"level":"debug"`) {
		t.Fatalf("trace should not use debug: %s", out)
	}
}

func TestNewGormLogger_NilZapNoPanic(t *testing.T) {
	NewGormLogger(nil).Info(t.Context(), "hello %s", "world")
}

// traceBufferLogger 构造一个把 JSON 日志写入 buf 的 Logger（默认 Warn /
// 100ms / IgnoreRecordNotFoundError=true），供 P-6 的 Trace 分支用例复用。
func traceBufferLogger(buf *bytes.Buffer) Logger {
	encCfg := zap.NewProductionEncoderConfig()
	encCfg.TimeKey = ""
	core := zapcore.NewCore(zapcore.NewJSONEncoder(encCfg), zapcore.AddSync(buf), zapcore.DebugLevel)
	return NewGormLogger(zap.New(core))
}

// P-6-a：默认 Warn 级别下快速成功 SQL 不打任何日志，且配置的 Context 回调
// 与 fc 都不应被调用。修复前 Trace 在 switch 之前无条件执行 l.logger(ctx)，
// 会触发 Context 回调并使本断言失败；修复后提前返回，二者均为零调用。
func TestGormLogger_TraceWarnFastSkipsLoggerAndContext(t *testing.T) {
	buf := &bytes.Buffer{}
	l := traceBufferLogger(buf)

	var contextCalls int32
	l.Context = func(context.Context) []zapcore.Field {
		atomic.AddInt32(&contextCalls, 1)
		return nil
	}
	fcCalled := false
	l.Trace(context.Background(), time.Now(), func() (string, int64) {
		fcCalled = true
		return "SELECT 1", 1
	}, nil)

	if out := buf.String(); out != "" {
		t.Fatalf("fast successful SQL at Warn must log nothing, got: %s", out)
	}
	if n := atomic.LoadInt32(&contextCalls); n != 0 {
		t.Fatalf("Context callback must not run when nothing is logged, got %d call(s)", n)
	}
	if fcCalled {
		t.Fatal("fc must not run when nothing is logged")
	}
}

// P-6-b：Warn 级别 + 慢查询（elapsed > SlowThreshold）→ 打 Warn 日志。
func TestGormLogger_TraceWarnSlowQueryLogs(t *testing.T) {
	buf := &bytes.Buffer{}
	l := traceBufferLogger(buf)
	l.SlowThreshold = time.Millisecond

	l.Trace(context.Background(), time.Now().Add(-time.Hour), func() (string, int64) {
		return "SELECT slow", 3
	}, nil)

	out := buf.String()
	if !strings.Contains(out, `"level":"warn"`) {
		t.Fatalf("want warn trace, got %s", out)
	}
	if !strings.Contains(out, "SELECT slow") {
		t.Fatalf("missing sql: %s", out)
	}
}

// P-6-c：Warn 级别 + gorm.ErrRecordNotFound → 忽略不打。
// 追加覆盖：Error 级别下 ErrRecordNotFound 仍被 IgnoreRecordNotFoundError
// 忽略（Warn 级别下 Error 分支本就不可达，此断言才真正经过该开关）。
func TestGormLogger_TraceRecordNotFoundIgnored(t *testing.T) {
	buf := &bytes.Buffer{}
	l := traceBufferLogger(buf)
	l.Trace(context.Background(), time.Now(), func() (string, int64) {
		return "SELECT 1", 0
	}, gorm.ErrRecordNotFound)
	if out := buf.String(); out != "" {
		t.Fatalf("ErrRecordNotFound at Warn must be ignored, got: %s", out)
	}

	buf.Reset()
	l.LogLevel = gormlogger.Error
	l.Trace(context.Background(), time.Now(), func() (string, int64) {
		return "SELECT 1", 0
	}, gorm.ErrRecordNotFound)
	if out := buf.String(); out != "" {
		t.Fatalf("ErrRecordNotFound must be ignored via IgnoreRecordNotFoundError, got: %s", out)
	}
}

// P-6-d：Error 级别 + 真实错误 → Error 日志。
func TestGormLogger_TraceErrorLevelRealErrorLogs(t *testing.T) {
	buf := &bytes.Buffer{}
	l := traceBufferLogger(buf)
	l.LogLevel = gormlogger.Error

	l.Trace(context.Background(), time.Now(), func() (string, int64) {
		return "UPDATE t", 0
	}, errors.New("boom"))

	out := buf.String()
	if !strings.Contains(out, `"level":"error"`) {
		t.Fatalf("want error trace, got %s", out)
	}
	if !strings.Contains(out, "boom") || !strings.Contains(out, "UPDATE t") {
		t.Fatalf("missing error/sql fields: %s", out)
	}
}
