package logger

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func attachBufferLogger(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	encCfg := zap.NewProductionEncoderConfig()
	encCfg.TimeKey = ""
	core := zapcore.NewCore(zapcore.NewJSONEncoder(encCfg), zapcore.AddSync(buf), zapcore.DebugLevel)
	prev := log
	log = zap.New(core, zap.AddCaller(), zap.AddCallerSkip(1)).Sugar()
	t.Cleanup(func() { log = prev })
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
	prev := log
	log = nil
	t.Cleanup(func() { log = prev })

	Info("hello")
	Warn("warn")
	Error("err")
	Infof("%s", "x")
	Warnf("%s", "y")
	Errorf("%s", "z")
	StackedError(errors.New("stacked"))
}
