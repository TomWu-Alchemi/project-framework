package httpclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestMaxConnsPerHost_ConfiguredLimit(t *testing.T) {
	var accepted atomic.Int32
	entered := make(chan struct{})
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accepted.Add(1)
		select {
		case <-entered:
		default:
			close(entered)
		}
		<-block
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := NewDalHttpClient(DalHttpClientConf{
		Timeout:         5 * time.Second,
		MaxConnsPerHost: 1,
	})
	if tr := c.httpClient.Transport.(*http.Transport); tr.MaxConnsPerHost != 1 {
		t.Fatalf("MaxConnsPerHost=%d", tr.MaxConnsPerHost)
	}

	// 第一个请求进入 handler 并堵住，占用唯一连接。
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- c.PostJson(t.Context(), srv.URL, nil, map[string]int{"n": 1}, nil)
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first request did not enter handler")
	}

	// 第一个仍堵住时发第二个短超时请求：不应再建立新连接到该主机。
	ctx2, cancel2 := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel2()
	secondErr := c.PostJson(ctx2, srv.URL, nil, map[string]int{"n": 2}, nil)
	if secondErr == nil {
		t.Fatal("second request expected error while MaxConnsPerHost=1 is held")
	}
	if got := accepted.Load(); got != 1 {
		t.Fatalf("accepted=%d, want 1 while first still blocked", got)
	}

	close(block)
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("first request after unblock: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first request did not finish after unblock")
	}
}
