package cacheproxy

// F-22 验收测试：getter panic 在 singleflight 执行体内收敛为 error，
// 领导者与等待者均不再 panic。

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sync/singleflight"
)

func panickingGetter() SingleGetter {
	return SingleGetterFunc(func(context.Context, string) (string, bool, error) {
		panic("getter exploded")
	})
}

func TestGetResource_GetterPanicReturnsError(t *testing.T) {
	p := &CacheProxy{getGroup: &singleflight.Group{}}

	// 本用例的存在意义就是"不 panic"：任何向上传播的 panic 都会直接 FAIL
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("getResource must not propagate getter panic: %v", r)
		}
	}()

	_, _, err := p.getResource(t.Context(), "k1", panickingGetter())
	if err == nil {
		t.Fatal("expected error from panicking getter")
	}
	if !strings.Contains(err.Error(), "getter panic") || !strings.Contains(err.Error(), "getter exploded") {
		t.Fatalf("error should identify the getter panic, got: %v", err)
	}
}

func TestGetHit_MissPathGetterPanicReturnsError(t *testing.T) {
	mc := newMemCache()
	p := &CacheProxy{cache: mc, getGroup: &singleflight.Group{}}

	_, _, err := p.GetHit(t.Context(), CacheContext{}, "k1", panickingGetter())
	if err == nil || !strings.Contains(err.Error(), "getter panic") {
		t.Fatalf("expected getter panic error, got: %v", err)
	}
	if _, ok, _ := mc.snapshot("k1"); ok {
		t.Fatal("cache must not be written when the getter panicked")
	}
}

func TestGetHit_ForcePathGetterPanicReturnsError(t *testing.T) {
	mc := newMemCache()
	p := &CacheProxy{cache: mc, getGroup: &singleflight.Group{}}

	_, _, err := p.GetHit(t.Context(), CacheContext{NeedForceRefresh: true}, "k1", panickingGetter())
	if err == nil || !strings.Contains(err.Error(), "getter panic") {
		t.Fatalf("expected getter panic error, got: %v", err)
	}
	if _, ok, _ := mc.snapshot("k1"); ok {
		t.Fatal("cache must not be written when the getter panicked")
	}
}

// F-22 核心场景：并发下同 key 的等待者与领导者一起拿到 error，
// 而不是一起 panic（x/sync 对 fn panic 的原生行为）。
func TestGetHit_ConcurrentGetterPanicAllWaitersGetError(t *testing.T) {
	mc := newMemCache()
	p := &CacheProxy{cache: mc, getGroup: &singleflight.Group{}}

	const n = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, n)

	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, _, err := p.GetHit(t.Context(), CacheContext{}, "k1", panickingGetter())
			errs[i] = err
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err == nil {
			t.Fatalf("goroutine %d: expected error, got nil", i)
		}
		if !strings.Contains(err.Error(), "getter panic") {
			t.Fatalf("goroutine %d: error should identify the getter panic, got: %v", i, err)
		}
	}
}

// 对照组：panic 收敛不影响正常 error 透传（既有行为回归保护）。
func TestGetHit_GetterErrorStillPropagates(t *testing.T) {
	mc := newMemCache()
	p := &CacheProxy{cache: mc, getGroup: &singleflight.Group{}}
	wantErr := errors.New("upstream failed")

	_, _, err := p.GetHit(t.Context(), CacheContext{}, "k1", SingleGetterFunc(func(context.Context, string) (string, bool, error) {
		return "", false, wantErr
	}))
	if !errors.Is(err, wantErr) {
		t.Fatalf("got %v, want %v", err, wantErr)
	}
}
