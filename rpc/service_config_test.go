package rpc

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func TestAsyncErrorHandler_LogsSubject(t *testing.T) {
	buf := &bytes.Buffer{}
	encCfg := zap.NewProductionEncoderConfig()
	encCfg.TimeKey = ""
	core := zapcore.NewCore(zapcore.NewJSONEncoder(encCfg), zapcore.AddSync(buf), zapcore.DebugLevel)
	log := zap.New(core)

	h := asyncErrorHandler(log)
	h(nil, &nats.Subscription{Subject: "orders.create"}, errors.New("slow consumer"))
	out := buf.String()
	if !strings.Contains(out, "orders.create") || !strings.Contains(out, "slow consumer") {
		t.Fatalf("log=%s", out)
	}
}
