package cacheproxy

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"golang.org/x/sync/singleflight"
)

type memCache struct {
	mu               sync.Mutex
	store            map[string]StringView
	sets             int
	lastExpired      time.Duration
	lastEmptyExpired time.Duration
}

func newMemCache() *memCache {
	return &memCache{store: make(map[string]StringView)}
}

func (m *memCache) Get(_ context.Context, key string) (StringView, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.store[key]
	if !ok {
		return StringView{}, false, nil
	}
	return v, true, nil
}

func (m *memCache) Set(_ context.Context, key string, value StringView, expired time.Duration, emptyExpired time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.store[key] = value
	m.sets++
	m.lastExpired = expired
	m.lastEmptyExpired = emptyExpired
	return nil
}

func (m *memCache) Remove(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.store, key)
	return nil
}

func (m *memCache) MGet(_ context.Context, _ []string) ([]StringView, error) {
	return nil, errors.ErrUnsupported
}

func (m *memCache) MSet(_ context.Context, _ []string, _ []StringView, _ time.Duration, _ time.Duration) error {
	return errors.ErrUnsupported
}

func (m *memCache) snapshot(key string) (StringView, bool, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.store[key]
	return v, ok, m.sets
}

func TestGetResource_GetterError(t *testing.T) {
	p := &CacheProxy{getGroup: &singleflight.Group{}}
	wantErr := errors.New("upstream failed")

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("getResource panicked: %v", r)
		}
	}()

	_, _, err := p.getResource(t.Context(), "k1", SingleGetterFunc(func(context.Context, string) (string, bool, error) {
		return "", false, wantErr
	}))
	if !errors.Is(err, wantErr) {
		t.Fatalf("got err %v, want %v", err, wantErr)
	}
}

func TestGetHit_GetterError(t *testing.T) {
	p := &CacheProxy{
		cache:    newMemCache(),
		getGroup: &singleflight.Group{},
	}
	wantErr := errors.New("upstream failed")

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("GetHit panicked: %v", r)
		}
	}()

	_, _, err := p.GetHit(t.Context(), CacheContext{}, "k1", SingleGetterFunc(func(context.Context, string) (string, bool, error) {
		return "", false, wantErr
	}))
	if !errors.Is(err, wantErr) {
		t.Fatalf("got err %v, want %v", err, wantErr)
	}
}

func TestGetHit_RefreshFailureDoesNotOverwrite(t *testing.T) {
	const key = "user:1"
	const oldVal = "old-value"

	mc := newMemCache()
	mc.store[key] = StringView{
		Ctime: time.Now().Add(-time.Hour),
		Data:  oldVal,
	}
	p := &CacheProxy{
		cache:    mc,
		getGroup: &singleflight.Group{},
	}

	getterDone := make(chan struct{})
	got, hit, err := p.GetHit(t.Context(), CacheContext{
		NeedCacheRefresh: true,
		RefreshOffset:    time.Minute,
	}, key, SingleGetterFunc(func(context.Context, string) (string, bool, error) {
		defer close(getterDone)
		return "", false, errors.New("upstream failed")
	}))
	if err != nil {
		t.Fatalf("GetHit returned err: %v", err)
	}
	if !hit {
		t.Fatal("expected cache hit with stale value")
	}
	if got != oldVal {
		t.Fatalf("got %q, want stale %q", got, oldVal)
	}

	select {
	case <-getterDone:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh getter was not called")
	}

	deadline := time.Now().Add(200 * time.Millisecond)
	for {
		sv, ok, sets := mc.snapshot(key)
		if sets != 0 {
			t.Fatalf("refresh failure overwrote cache, sets=%d data=%q", sets, sv.Data)
		}
		if time.Now().After(deadline) {
			if !ok || sv.Data != oldVal {
				t.Fatalf("cache changed without Set: ok=%v data=%q", ok, sv.Data)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type failSetCache struct {
	*memCache
	err error
}

func (f *failSetCache) Set(context.Context, string, StringView, time.Duration, time.Duration) error {
	return f.err
}

func TestGetHit_EmptyKey(t *testing.T) {
	p := &CacheProxy{cache: newMemCache(), getGroup: &singleflight.Group{}}
	_, _, err := p.GetHit(t.Context(), CacheContext{}, "", SingleGetterFunc(func(context.Context, string) (string, bool, error) {
		t.Fatal("getter should not run")
		return "", false, nil
	}))
	if !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("err=%v", err)
	}
}

func TestGetHit_NilProxy(t *testing.T) {
	var p *CacheProxy
	_, _, err := p.GetHit(t.Context(), CacheContext{}, "k", nil)
	if !errors.Is(err, ErrNotInitialized) {
		t.Fatalf("err=%v", err)
	}
}

func TestSet_DefaultTTL(t *testing.T) {
	mc := newMemCache()
	p := &CacheProxy{cache: mc, getGroup: &singleflight.Group{}}
	if err := p.Set(t.Context(), CacheContext{}, "k", "v"); err != nil {
		t.Fatal(err)
	}
	if mc.lastExpired != defaultExpiredTime {
		t.Fatalf("expired=%s want %s", mc.lastExpired, defaultExpiredTime)
	}
	if err := p.Set(t.Context(), CacheContext{}, "empty", ""); err != nil {
		t.Fatal(err)
	}
	if mc.lastEmptyExpired != defaultEmptyExpiredTime {
		t.Fatalf("emptyExpired=%s want %s", mc.lastEmptyExpired, defaultEmptyExpiredTime)
	}
	if err := p.Set(t.Context(), CacheContext{ExpiredTime: time.Hour}, "k2", "v"); err != nil {
		t.Fatal(err)
	}
	if mc.lastExpired != time.Hour {
		t.Fatalf("explicit expired=%s", mc.lastExpired)
	}
	if err := p.Set(t.Context(), CacheContext{ExpiredTime: -1, EmptyExpiredTime: -time.Second}, "k3", "v"); err != nil {
		t.Fatal(err)
	}
	if mc.lastExpired != defaultExpiredTime {
		t.Fatalf("negative expired=%s want default", mc.lastExpired)
	}
	if err := p.Set(t.Context(), CacheContext{EmptyExpiredTime: -1}, "empty2", ""); err != nil {
		t.Fatal(err)
	}
	if mc.lastEmptyExpired != defaultEmptyExpiredTime {
		t.Fatalf("negative emptyExpired=%s want default", mc.lastEmptyExpired)
	}
}

func TestGetHit_DefaultRefreshOffsetDoesNotRefreshImmediately(t *testing.T) {
	const key = "k"
	mc := newMemCache()
	mc.store[key] = StringView{Ctime: time.Now(), Data: "v"}
	p := &CacheProxy{cache: mc, getGroup: &singleflight.Group{}}
	called := make(chan struct{}, 1)
	got, hit, err := p.GetHit(t.Context(), CacheContext{NeedCacheRefresh: true}, key, SingleGetterFunc(func(context.Context, string) (string, bool, error) {
		called <- struct{}{}
		return "new", false, nil
	}))
	if err != nil || !hit || got != "v" {
		t.Fatalf("got=%q hit=%v err=%v", got, hit, err)
	}
	select {
	case <-called:
		t.Fatal("zero RefreshOffset should use default, not refresh immediately")
	case <-time.After(80 * time.Millisecond):
	}
}

func TestGetHit_ZeroCtimeTriggersRefresh(t *testing.T) {
	const key = "k"
	mc := newMemCache()
	mc.store[key] = StringView{Data: "stale"}
	p := &CacheProxy{cache: mc, getGroup: &singleflight.Group{}}
	done := make(chan struct{})
	got, hit, err := p.GetHit(t.Context(), CacheContext{NeedCacheRefresh: true, RefreshOffset: time.Hour}, key, SingleGetterFunc(func(context.Context, string) (string, bool, error) {
		defer close(done)
		return "fresh", false, nil
	}))
	if err != nil || !hit || got != "stale" {
		t.Fatalf("got=%q hit=%v err=%v", got, hit, err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("zero ctime should refresh")
	}
}

func TestGetHit_ForceSetErrorStillReturnsData(t *testing.T) {
	p := &CacheProxy{
		cache:    &failSetCache{memCache: newMemCache(), err: errors.New("redis down")},
		getGroup: &singleflight.Group{},
	}
	got, hit, err := p.GetHit(t.Context(), CacheContext{NeedForceRefresh: true}, "k", SingleGetterFunc(func(context.Context, string) (string, bool, error) {
		return "fresh", false, nil
	}))
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if hit || got != "fresh" {
		t.Fatalf("got=%q hit=%v", got, hit)
	}
}

func TestGetResource_CallerCancelDoesNotFailGroup(t *testing.T) {
	p := &CacheProxy{cache: newMemCache(), getGroup: &singleflight.Group{}}
	started := make(chan struct{})
	release := make(chan struct{})
	var calls int
	var mu sync.Mutex
	getter := SingleGetterFunc(func(ctx context.Context, _ string) (string, bool, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		close(started)
		<-release
		if err := ctx.Err(); err != nil {
			return "", false, err
		}
		return "ok", false, nil
	})

	ctx1, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() {
		_, _, err := p.GetHit(ctx1, CacheContext{}, "k", getter)
		errCh <- err
	}()
	<-started
	cancel()

	type result struct {
		v   string
		err error
	}
	resCh := make(chan result, 1)
	go func() {
		v, _, err := p.GetHit(t.Context(), CacheContext{}, "k", getter)
		resCh <- result{v, err}
	}()
	time.Sleep(50 * time.Millisecond)
	close(release)

	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("first err=%v", err)
	}
	res := <-resCh
	if res.err != nil || res.v != "ok" {
		t.Fatalf("second got=%q err=%v", res.v, res.err)
	}
	mu.Lock()
	n := calls
	mu.Unlock()
	if n != 1 {
		t.Fatalf("getter calls=%d want 1", n)
	}
}
