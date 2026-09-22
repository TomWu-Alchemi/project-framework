package httpclient

// A5 验收测试：GetWithRetry 在 readResponseBody 失败（非 MaxBytesError）的
// 重试路径上补一条 Warn。与 TestGetWithRetry_NetworkErrorWarnsBeforeRetry
// （Do 出错路径）区分：本用例的 Do 成功，仅 body 读取报错。

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// errReadCloser 的 Read 恒定返回非 MaxBytesError 的普通错误，
// 用于构造「响应已建立但 body 读取失败」的场景。无共享状态，-race 兼容。
type errReadCloser struct{ err error }

func (e errReadCloser) Read([]byte) (int, error) { return 0, e.err }
func (e errReadCloser) Close() error             { return nil }

func TestGetWithRetry_BodyReadErrorWarnsBeforeRetry(t *testing.T) {
	readErr := errors.New("simulated body read failure")

	c := NewDalHttpClient(DalHttpClientConf{Timeout: 5 * time.Second})
	core, observed := observer.New(zap.WarnLevel)
	c.dalLog = zap.New(core)
	// 直接替换 Transport（roundTripFunc 见 http_transport_test.go），绕开真实网络。
	c.httpClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       errReadCloser{err: readErr},
			Request:    req,
		}, nil
	})

	_, err := c.GetWithRetry(t.Context(), "http://example.invalid/", nil, nil, 2)
	if err == nil {
		t.Fatal("expected an error when body read fails on every attempt")
	}
	if !strings.HasPrefix(err.Error(), "after 2 attempts: ") {
		t.Fatalf("error = %v, want prefix %q", err, "after 2 attempts: ")
	}

	logs := observed.All()
	// 两次尝试中只有第一次会被重试（第二次是最后一次），故恰好 1 条 Warn；
	// 该断言同时证明 Warn 来自 body 读取失败路径（Do 未报错）。
	if len(logs) != 1 {
		t.Fatalf("warn entries = %d, want 1", len(logs))
	}
	entry := logs[0]
	if entry.Level != zap.WarnLevel || entry.Message != "GetWithRetry" {
		t.Fatalf("level=%v message=%q", entry.Level, entry.Message)
	}
	got := entry.ContextMap()
	if attempt := got["attempt"]; attempt != int64(1) {
		t.Fatalf("attempt = %v (%T), want 1", attempt, attempt)
	}
	if ms, ok := got["latency_ms"].(int64); !ok || ms < 0 {
		t.Fatalf("latency_ms = %v, want non-negative int64", got["latency_ms"])
	}
	if s := fmt.Sprint(got["error"]); s == "" {
		t.Fatalf("error field missing or empty: %v", got)
	}
}
