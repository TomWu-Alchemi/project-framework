package httpclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type infiniteReader struct{}

func (infiniteReader) Read(p []byte) (int, error) {
	return len(p), nil
}

func testClient(t *testing.T, timeout time.Duration) *DalHttpClient {
	t.Helper()
	return NewDalHttpClient(DalHttpClientConf{Timeout: timeout})
}

func TestNewDalHttpClient_ClonesDefaultTransport(t *testing.T) {
	c := testClient(t, time.Second)
	tr, ok := c.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type %T", c.httpClient.Transport)
	}
	def := http.DefaultTransport.(*http.Transport)
	if tr == def {
		t.Fatal("must Clone DefaultTransport, not share it")
	}
	if !tr.ForceAttemptHTTP2 {
		t.Fatal("expected ForceAttemptHTTP2 from cloned DefaultTransport")
	}
	if tr.MaxIdleConnsPerHost != 100 {
		t.Fatalf("MaxIdleConnsPerHost=%d", tr.MaxIdleConnsPerHost)
	}
}

func TestNewDalHttpClient_DefaultTimeout(t *testing.T) {
	tests := []struct {
		name    string
		timeout time.Duration
		want    time.Duration
	}{
		{"零值回退为 10s", 0, 10 * time.Second},
		{"负数回退为 10s", -1, 10 * time.Second},
		{"正值保持", 3 * time.Second, 3 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewDalHttpClient(DalHttpClientConf{Timeout: tt.timeout})
			if got := c.httpClient.Timeout; got != tt.want {
				t.Fatalf("httpClient.Timeout = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPostJson_StatusOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	var out struct {
		OK bool `json:"ok"`
	}
	if err := testClient(t, 5*time.Second).PostJson(t.Context(), srv.URL, nil, map[string]int{"n": 1}, &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK {
		t.Fatalf("out=%+v", out)
	}
}

func TestPostJson_StatusCreated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	var out struct {
		OK bool `json:"ok"`
	}
	if err := testClient(t, 5*time.Second).PostJson(t.Context(), srv.URL, nil, map[string]int{"n": 1}, &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK {
		t.Fatalf("out=%+v", out)
	}
}

func TestPostJson_NoContentSkipsUnmarshal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	dest := struct {
		V int `json:"v"`
	}{V: 7}
	if err := testClient(t, 5*time.Second).PostJson(t.Context(), srv.URL, nil, map[string]int{"n": 1}, &dest); err != nil {
		t.Fatal(err)
	}
	if dest.V != 7 {
		t.Fatalf("unmarshaled into dest: %+v", dest)
	}
}

func TestPostJson_NilRespSkipsUnmarshal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)

	if err := testClient(t, 5*time.Second).PostJson(t.Context(), srv.URL, nil, map[string]int{"n": 1}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestPostJson_Non2xxWrapsErrFailedRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad-body", http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)

	err := testClient(t, 5*time.Second).PostJson(t.Context(), srv.URL, nil, map[string]int{"n": 1}, &struct{}{})
	if !errors.Is(err, ErrFailedRequest) {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "bad-body") {
		t.Fatalf("error missing status/body: %v", err)
	}
}

func TestPostJson_MaxBytesErrorAsType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, io.LimitReader(infiniteReader{}, 10<<20+1))
	}))
	t.Cleanup(srv.Close)

	err := testClient(t, 30*time.Second).PostJson(t.Context(), srv.URL, nil, map[string]int{"n": 1}, &struct{}{})
	maxErr, ok := errors.AsType[*http.MaxBytesError](err)
	if !ok {
		t.Fatalf("want MaxBytesError, got %T %v", err, err)
	}
	if maxErr.Limit != 10<<20 {
		t.Fatalf("Limit=%d", maxErr.Limit)
	}
	if !strings.Contains(err.Error(), "10485760") {
		t.Fatalf("error missing Limit: %v", err)
	}
}

func TestPostJson_NilDalLogNoPanic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	c := testClient(t, 5*time.Second)
	c.dalLog = nil
	if err := c.PostJson(t.Context(), srv.URL, nil, map[string]int{"n": 1}, &struct{}{}); err != nil {
		t.Fatal(err)
	}
}

func TestGetWithRetry_ContextCanceled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := testClient(t, 5*time.Second).GetWithRetry(ctx, srv.URL, nil, nil, 3)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestGetWithRetry_QueryMerge(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	_, err := testClient(t, 5*time.Second).GetWithRetry(
		t.Context(),
		srv.URL+"/p?a=1&c=old",
		map[string]string{"b": "2", "c": "new"},
		nil,
		1,
	)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(gotQuery, "?") {
		t.Fatalf("raw query still concatenated with ?: %q", gotQuery)
	}
	q, err := url.ParseQuery(gotQuery)
	if err != nil {
		t.Fatal(err)
	}
	if q.Get("a") != "1" || q.Get("b") != "2" || q.Get("c") != "new" {
		t.Fatalf("query=%q", gotQuery)
	}
}

func TestGetWithRetry_4xxNoRetry(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "nope", http.StatusBadRequest)
	}))
	t.Cleanup(srv.Close)

	_, err := testClient(t, 5*time.Second).GetWithRetry(t.Context(), srv.URL, nil, nil, 5)
	if hits.Load() != 1 {
		t.Fatalf("hits=%d, 4xx should not retry", hits.Load())
	}
	if !errors.Is(err, ErrFailedRequest) {
		t.Fatalf("got %v", err)
	}
}

func TestGetWithRetry_MaxAttemptsLessThanOneMeansOnce(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	if _, err := testClient(t, 5*time.Second).GetWithRetry(t.Context(), srv.URL, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Fatalf("hits=%d", hits.Load())
	}
}

func TestGetWithRetry_429Retries(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	_, err := testClient(t, 5*time.Second).GetWithRetry(t.Context(), srv.URL, nil, nil, 2)
	if hits.Load() != 2 {
		t.Fatalf("hits=%d", hits.Load())
	}
	if !errors.Is(err, ErrFailedRequest) {
		t.Fatalf("got %v", err)
	}
	// P3-3: the wording now counts attempts, not retries.
	if !strings.HasPrefix(err.Error(), "after 2 attempts: ") {
		t.Fatalf("error = %v, want prefix %q", err, "after 2 attempts: ")
	}
}

func TestGetWithRetry_NetworkErrorWarnsBeforeRetry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	// Closed immediately: every attempt fails with a network error.
	srv.Close()

	core, observed := observer.New(zap.WarnLevel)
	c := NewDalHttpClient(DalHttpClientConf{
		Timeout: 5 * time.Second,
		DalLog:  zap.New(core),
	})

	_, err := c.GetWithRetry(t.Context(), srv.URL, nil, nil, 2)
	if err == nil {
		t.Fatal("expected a network error from the closed server")
	}
	if !strings.HasPrefix(err.Error(), "after 2 attempts: ") {
		t.Fatalf("error = %v, want prefix %q", err, "after 2 attempts: ")
	}

	logs := observed.All()
	// Only the attempt that is actually retried is warned about; the final
	// failure is returned to the caller as the wrapped error.
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
	// zapcore.ErrorType rendering depends on zap internals: require the key
	// to be present with a non-empty rendered value.
	if s := fmt.Sprint(got["error"]); s == "" {
		t.Fatalf("error field missing or empty: %v", got)
	}
}

func TestPostJson_NilClient(t *testing.T) {
	err := (*DalHttpClient)(nil).PostJson(t.Context(), "http://example.com", nil, map[string]int{"n": 1}, nil)
	if !errors.Is(err, ErrNilClient) {
		t.Fatalf("nil receiver err=%v", err)
	}
	err = (&DalHttpClient{}).PostJson(t.Context(), "http://example.com", nil, map[string]int{"n": 1}, nil)
	if !errors.Is(err, ErrNilClient) {
		t.Fatalf("nil httpClient err=%v", err)
	}
}

func TestGetWithRetry_NilClient(t *testing.T) {
	_, err := (*DalHttpClient)(nil).GetWithRetry(t.Context(), "http://example.com", nil, nil, 1)
	if !errors.Is(err, ErrNilClient) {
		t.Fatalf("nil receiver err=%v", err)
	}
	_, err = (&DalHttpClient{}).GetWithRetry(t.Context(), "http://example.com", nil, nil, 1)
	if !errors.Is(err, ErrNilClient) {
		t.Fatalf("nil httpClient err=%v", err)
	}
}
