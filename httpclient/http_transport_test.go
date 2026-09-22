package httpclient

// F-26 / F-27 验收测试：DefaultTransport 被替换时构造不 panic；
// 重试类失败状态（429/5xx）的尝试日志为 Warn 级。

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// F-26：宿主替换 DefaultTransport 为非 *http.Transport 时，构造不 panic，
// 兜底 Transport 可用且连接池参数照常生效。
func TestNewDalHttpClient_ReplacedDefaultTransportNoPanic(t *testing.T) {
	old := http.DefaultTransport
	// 仅用于把 DefaultTransport 换成非 *http.Transport 类型；不会被真正调用
	// （客户端持有自己的兜底 Transport）。
	http.DefaultTransport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("replaced transport must not be used")
	})
	t.Cleanup(func() { http.DefaultTransport = old })

	c := NewDalHttpClient(DalHttpClientConf{Timeout: 5 * time.Second})
	tr, ok := c.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type %T", c.httpClient.Transport)
	}
	if tr.MaxIdleConns != 100 || tr.MaxIdleConnsPerHost != 100 || tr.IdleConnTimeout != 60*time.Second {
		t.Fatalf("connection pool tuning not applied: %+v", tr)
	}
	if tr.MaxConnsPerHost != defaultMaxConnsPerHost {
		t.Fatalf("MaxConnsPerHost=%d, want %d", tr.MaxConnsPerHost, defaultMaxConnsPerHost)
	}
	if tr.Proxy == nil {
		t.Fatal("fallback transport must honor proxy env")
	}
	// 兜底 transport 端到端可用（本地 httptest server，不依赖外部网络）
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	if err := c.PostJson(t.Context(), srv.URL, nil, map[string]int{"n": 1}, nil); err != nil {
		t.Fatalf("fallback transport unusable: %v", err)
	}
}

// F-27：429 的每次尝试日志为 Warn 级（与网络错误路径同级）。
func TestGetWithRetry_RetryableStatusLogsWarn(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	core, observed := observer.New(zap.InfoLevel) // Info 起步，Info/Warn 均可观测
	c := NewDalHttpClient(DalHttpClientConf{Timeout: 5 * time.Second, DalLog: zap.New(core)})

	if _, err := c.GetWithRetry(t.Context(), srv.URL, nil, nil, 2); err == nil {
		t.Fatal("expected failure after 2 attempts")
	}
	logs := observed.All()
	if len(logs) != 2 {
		t.Fatalf("entries=%d, want 2 (one per 429 attempt)", len(logs))
	}
	for _, e := range logs {
		if e.Level != zap.WarnLevel {
			t.Fatalf("429 attempt must log at Warn, got %v", e.Level)
		}
		if e.Message != "GetWithRetry" {
			t.Fatalf("message=%q", e.Message)
		}
	}
}

// F-27 对照组：成功请求保持 Info 级。
func TestGetWithRetry_SuccessStillLogsInfo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	core, observed := observer.New(zap.InfoLevel)
	c := NewDalHttpClient(DalHttpClientConf{Timeout: 5 * time.Second, DalLog: zap.New(core)})

	if _, err := c.GetWithRetry(t.Context(), srv.URL, nil, nil, 1); err != nil {
		t.Fatal(err)
	}
	logs := observed.All()
	if len(logs) != 1 {
		t.Fatalf("entries=%d, want 1", len(logs))
	}
	if logs[0].Level != zap.InfoLevel {
		t.Fatalf("success must log at Info, got %v", logs[0].Level)
	}
}
