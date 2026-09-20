package rpc

import (
	"context"
	"testing"

	"github.com/nats-io/nats.go/micro"
)

func TestNewNatsService_ConnectFail_CleanupNonNil(t *testing.T) {
	svc, cleanup, err := NewNatsService(ServiceConfig{
		Url:     "nats://127.0.0.1:1",
		AppName: "test-app",
		Version: "0.0.1",
	})
	if cleanup == nil {
		t.Fatal("cleanup must not be nil on connect failure")
	}
	cleanup()
	if err == nil {
		t.Fatal("expected connect error")
	}
	if svc != nil {
		t.Fatal("expected nil service on connect error")
	}
}

type fakeRequest struct {
	subject   string
	data      []byte
	headers   micro.Headers
	errCode   string
	errDesc   string
	responded bool
}

func (f *fakeRequest) Respond([]byte, ...micro.RespondOpt) error {
	f.responded = true
	return nil
}
func (f *fakeRequest) RespondJSON(any, ...micro.RespondOpt) error {
	f.responded = true
	return nil
}
func (f *fakeRequest) Error(code, description string, data []byte, opts ...micro.RespondOpt) error {
	f.errCode = code
	f.errDesc = description
	f.responded = true
	return nil
}
func (f *fakeRequest) Data() []byte           { return f.data }
func (f *fakeRequest) Headers() micro.Headers { return f.headers }
func (f *fakeRequest) Subject() string        { return f.subject }
func (f *fakeRequest) Reply() string          { return "" }

var _ micro.Request = (*fakeRequest)(nil)

func TestNatsRpcAccessLog_PanicResponds(t *testing.T) {
	h := NatsRpcAccessLog(func(context.Context, micro.Request) {
		panic("boom")
	})
	req := &fakeRequest{subject: "svc.method", data: []byte("payload-keep")}
	h(t.Context(), req)
	if !req.responded || req.errCode != "500" || req.errDesc != "internal error" {
		t.Fatalf("code=%q desc=%q responded=%v", req.errCode, req.errDesc, req.responded)
	}
}

func TestNatsRpcAccessLog_NilLoggerNoPanic(t *testing.T) {
	called := false
	h := NatsRpcAccessLog(func(ctx context.Context, req micro.Request) {
		called = true
		_ = req.Respond(nil)
	})
	h(t.Context(), &fakeRequest{subject: "svc.ok"})
	if !called {
		t.Fatal("handler not called")
	}
}
