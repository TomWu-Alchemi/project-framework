package httpclient

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCheckRedirect_CrossHostStripsXApiKey(t *testing.T) {
	var sawAPIKey, sawTrace atomic.Bool
	dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "" {
			sawAPIKey.Store(true)
		}
		if r.Header.Get("X-Trace-Id") == "tr-1" {
			sawTrace.Store(true)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(dst.Close)

	// httptest 默认 Host 是 127.0.0.1；把 Location 改成 localhost，Hostname 不同但通常仍能连上。
	dstURL, err := url.Parse(dst.URL)
	if err != nil {
		t.Fatal(err)
	}
	dstURL.Host = "localhost:" + dstURL.Port()
	crossHostLoc := dstURL.String()
	if u, _ := url.Parse(crossHostLoc); u.Hostname() == "127.0.0.1" {
		t.Fatal("cross-host Location must not use 127.0.0.1 as Hostname")
	}

	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, crossHostLoc, http.StatusFound)
	}))
	t.Cleanup(src.Close)

	c := testClient(t, 5*time.Second)
	if err := c.PostJson(t.Context(), src.URL, map[string]string{
		"X-Api-Key":  "secret-key",
		"X-Trace-Id": "tr-1",
	}, map[string]int{"n": 1}, nil); err != nil {
		t.Fatal(err)
	}
	if sawAPIKey.Load() {
		t.Fatal("cross-host hop must not receive X-Api-Key")
	}
	if !sawTrace.Load() {
		t.Fatal("X-Trace-Id must be forwarded")
	}
}

func TestCheckRedirect_SameHostKeepsAuthHeaders(t *testing.T) {
	var hops atomic.Int32
	var secondAPIKey, secondAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hops.Add(1)
		if n == 1 {
			http.Redirect(w, r, "/final", http.StatusFound)
			return
		}
		secondAPIKey = r.Header.Get("X-Api-Key")
		secondAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	c := testClient(t, 5*time.Second)
	if err := c.PostJson(t.Context(), srv.URL, map[string]string{
		"X-Api-Key":     "secret-key",
		"Authorization": "Bearer tok",
	}, map[string]int{"n": 1}, nil); err != nil {
		t.Fatal(err)
	}
	if secondAPIKey != "secret-key" || secondAuth != "Bearer tok" {
		t.Fatalf("same-host hop headers: api=%q auth=%q", secondAPIKey, secondAuth)
	}
}

func TestCheckRedirect_StopsAfter10(t *testing.T) {
	var hops atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hops.Add(1)
		http.Redirect(w, r, "/next", http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	_, err := testClient(t, 5*time.Second).GetWithRetry(t.Context(), srv.URL, nil, nil, 1)
	if err == nil {
		t.Fatal("expected redirect stop error")
	}
	if !strings.Contains(err.Error(), "stopped after 10 redirects") {
		t.Fatalf("err=%v", err)
	}
	if hops.Load() < 10 {
		t.Fatalf("hops=%d, want at least 10", hops.Load())
	}
}
