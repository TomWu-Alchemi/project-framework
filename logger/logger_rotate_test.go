package logger

import (
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
)

func TestRotateLoop_RecoversAndContinues(t *testing.T) {
	buf := attachBufferLogger(t)
	var calls atomic.Int32
	second := make(chan struct{})
	rt := &loggerRuntime{
		writers:         []*lumberjack.Logger{{Filename: filepath.Join(t.TempDir(), "x.log")}},
		rotateDone:      make(chan struct{}),
		rotateExited:    make(chan struct{}),
		untilNextRotate: func() time.Duration { return time.Millisecond },
		sleepAfterPanic: func(time.Duration) {},
		rotateOne: func(*lumberjack.Logger) {
			n := calls.Add(1)
			if n == 1 {
				panic("rotate boom")
			}
			select {
			case <-second:
			default:
				close(second)
			}
		},
	}
	go rt.rotateLoop()
	select {
	case <-second:
	case <-time.After(2 * time.Second):
		t.Fatal("second rotate not called after panic")
	}
	close(rt.rotateDone)
	select {
	case <-rt.rotateExited:
	case <-time.After(2 * time.Second):
		t.Fatal("rotateLoop did not exit after Shutdown signal")
	}
	if !strings.Contains(buf.String(), "panic in log rotating") {
		t.Fatalf("expected panic log, got %s", buf.String())
	}
}
