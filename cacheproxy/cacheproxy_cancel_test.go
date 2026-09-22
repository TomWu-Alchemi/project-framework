package cacheproxy

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"golang.org/x/sync/singleflight"
)

func TestGetResource_PreCanceledSkipsGetter(t *testing.T) {
	p := &CacheProxy{getGroup: &singleflight.Group{}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var called atomic.Bool
	_, _, err := p.getResource(ctx, "k", SingleGetterFunc(func(context.Context, string) (string, bool, error) {
		called.Store(true)
		return "v", false, nil
	}))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
	if called.Load() {
		t.Fatal("getter must not be called when ctx is already canceled")
	}
}

func TestAwaitFlight_CanceledPrefersBufferedResult(t *testing.T) {
	// 结果已在缓冲通道中、同时 ctx 已取消：无论 select 先命中哪一侧，都应拿到数据。
	// 若命中 Done 分支，则覆盖「非阻塞再读结果通道」的新路径。
	ch := make(chan singleflight.Result, 1)
	ch <- singleflight.Result{Val: "payload"}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	got, fast, err := awaitFlight(ctx, ch)
	if err != nil || fast || got != "payload" {
		t.Fatalf("got=%q fast=%v err=%v", got, fast, err)
	}
}

func TestAwaitFlight_CanceledWithoutResult(t *testing.T) {
	ch := make(chan singleflight.Result, 1)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, _, err := awaitFlight(ctx, ch)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
}
