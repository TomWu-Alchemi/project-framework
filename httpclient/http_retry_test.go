package httpclient

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TomWu-Alchemi/project-framework/internal/logutil"
)

func TestGetWithRetry_501NoRetry(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNotImplemented)
	}))
	t.Cleanup(srv.Close)

	_, err := testClient(t, 5*time.Second).GetWithRetry(t.Context(), srv.URL, nil, nil, 5)
	if hits.Load() != 1 {
		t.Fatalf("hits=%d", hits.Load())
	}
	if !errors.Is(err, ErrFailedRequest) {
		t.Fatalf("err=%v", err)
	}
}

func TestGetWithRetry_503Retries(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	_, err := testClient(t, 5*time.Second).GetWithRetry(t.Context(), srv.URL, nil, nil, 2)
	if hits.Load() != 2 {
		t.Fatalf("hits=%d", hits.Load())
	}
	if !errors.Is(err, ErrFailedRequest) {
		t.Fatalf("err=%v", err)
	}
}

func TestCombineRetryWait_RetryAfter(t *testing.T) {
	if got := combineRetryWait(50*time.Millisecond, http.StatusTooManyRequests, "60"); got != maxRetryAfter {
		t.Fatalf("cap: got %v", got)
	}
	if got := combineRetryWait(10*time.Millisecond, http.StatusTooManyRequests, "1"); got != time.Second {
		t.Fatalf("take Retry-After: got %v", got)
	}
	if got := combineRetryWait(2*time.Second, http.StatusTooManyRequests, "1"); got != 2*time.Second {
		t.Fatalf("keep larger backoff: got %v", got)
	}
	if got := combineRetryWait(50*time.Millisecond, http.StatusInternalServerError, "60"); got != 50*time.Millisecond {
		t.Fatalf("500 ignores Retry-After: got %v", got)
	}
	if got := combineRetryWait(50*time.Millisecond, http.StatusServiceUnavailable, "not-a-date"); got != 50*time.Millisecond {
		t.Fatalf("parse fail keeps backoff: got %v", got)
	}
}

func TestParseRetryAfter_ExpiredDate(t *testing.T) {
	past := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
	d, ok := parseRetryAfter(past)
	if !ok || d != 0 {
		t.Fatalf("d=%v ok=%v", d, ok)
	}
}

func TestGetWithRetry_Non2xxBodyCapped(t *testing.T) {
	payload := strings.Repeat("x", logutil.MaxLogBytes+2048)
	var readBytes atomic.Int64
	c := NewDalHttpClient(DalHttpClientConf{Timeout: 5 * time.Second})
	c.httpClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body := &countingReadCloser{r: strings.NewReader(payload), n: &readBytes}
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Header:     make(http.Header),
			Body:       body,
			Request:    req,
		}, nil
	})
	_, err := c.GetWithRetry(t.Context(), "http://example.invalid/", nil, nil, 1)
	if !errors.Is(err, ErrFailedRequest) {
		t.Fatalf("err=%v", err)
	}
	if readBytes.Load() > int64(logutil.MaxLogBytes) {
		t.Fatalf("read %d bytes, want <= %d", readBytes.Load(), logutil.MaxLogBytes)
	}
}

type countingReadCloser struct {
	r io.Reader
	n *atomic.Int64
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}
func (c *countingReadCloser) Close() error { return nil }
