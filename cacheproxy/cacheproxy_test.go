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
	mu    sync.Mutex
	store map[string]StringView
	sets  int
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

func (m *memCache) Set(_ context.Context, key string, value StringView, _ time.Duration, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.store[key] = value
	m.sets++
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
