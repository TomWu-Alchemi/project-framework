package logger

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
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
	l.Trace(context.Background(), time.Now(), func() (string, int64) { return "select 1", 1 }, nil)
	out := buf.String()
	if !strings.Contains(out, `"level":"info"`) {
		t.Fatalf("want info trace, got %s", out)
	}
	if strings.Contains(out, `"level":"debug"`) {
		t.Fatalf("trace should not use debug: %s", out)
	}
}
