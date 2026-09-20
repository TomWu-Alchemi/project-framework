package cacheproxy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sync/singleflight"
)

type memCache struct {
	mu               sync.Mutex
	store            map[string]StringView
	gets             int // Get 调用次数（P3-15 / F-17 的测试同步点，见 waitForGetCalls）
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
	m.gets++ // 在临界区内自增：其他 goroutine 观察到计数变化时，本次查找必然已发起
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

// getCalls 返回累计的 Get 调用次数（P3-15 / F-17 的测试同步点，见 waitForGetCalls）。
func (m *memCache) getCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.gets
}

// waitGetCallsBudget 是 waitForGetCalls 的轮询预算：远大于正常调度延迟，
// 仅在实现异常/严重卡顿时超时失败（失败即 t.Fatalf，不会挂死）。
const waitGetCallsBudget = 2 * time.Second

// waitForGetCalls 轮询等待 memCache 的 Get 调用次数达到 want。
//
// 存在理由（P3-15 的同步点说明）：singleflight 内部"等待者已注册"的状态无法从外部观测
// （Group.Do / DoChan 都不暴露等待队列），唯一可观测的前置状态是"调用方已通过缓存查找"——
// 未命中回源路径下，每个 GetHit 都先调用 cache.Get 再进入 getGroup.Do。
// 因此本函数把原先的"盲等固定时长"（time.Sleep(50ms)）替换为"轮询可观测状态"：
// 慢机器上不会再因为固定时长到点而提前放行（这正是原实现的 flaky 来源）。
//
// 注意：轮询命中后仍存在"Get 返回 → Do 注册等待者"的不可观测窗口，调用方
// 必须再补一小段兜底等待（见各用例内注释），本函数不负责该窗口。
func waitForGetCalls(t *testing.T, mc *memCache, want int) {
	t.Helper()
	deadline := time.Now().Add(waitGetCallsBudget)
	for {
		if n := mc.getCalls(); n >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待 memCache.Get 调用次数达到 %d 超时（预算 %v，当前 %d）",
				want, waitGetCallsBudget, mc.getCalls())
		}
		time.Sleep(time.Millisecond)
	}
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

func TestGetHit_ZeroValueNotInitialized(t *testing.T) {
	p := &CacheProxy{}
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

func TestGetHit_WithLogger_ObservesSetError(t *testing.T) {
	core, observed := observer.New(zap.ErrorLevel)
	p := newCacheProxy(&redis.Client{}, WithLogger(zap.New(core)))
	p.cache = &failSetCache{memCache: newMemCache(), err: errors.New("redis down")}

	got, hit, err := p.GetHit(t.Context(), CacheContext{NeedForceRefresh: true}, "k", SingleGetterFunc(
		func(context.Context, string) (string, bool, error) {
			return "fresh", false, nil
		}))
	if err != nil || hit || got != "fresh" {
		t.Fatalf("got=%q hit=%v err=%v", got, hit, err)
	}
	logs := observed.All()
	if len(logs) == 0 {
		t.Fatal("expected injected logger to observe setData error")
	}
	if !strings.Contains(logs[0].Message, "cacheProxy force setData err") {
		t.Fatalf("unexpected log message %q", logs[0].Message)
	}
}

func TestGetResource_CallerCancelDoesNotFailGroup(t *testing.T) {
	mc := newMemCache()
	// 本用例验证"首个调用者取消不传播"。显式给宽裕的回源超时（与批 2 的 F-10 集成用例一致），
	// 避免极慢机器上 getter 的 ctx deadline（默认 5s）先到期，把与本用例意图无关的
	// 超时错误混进断言。
	p := &CacheProxy{cache: mc, getGroup: &singleflight.Group{}, fetchTimeout: time.Minute}

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseGetter := func() { releaseOnce.Do(func() { close(release) }) }
	// 失败路径也必须放行 getter，否则会留下永久阻塞的 goroutine
	defer releaseGetter()

	var calls atomic.Int32
	getter := SingleGetterFunc(func(ctx context.Context, _ string) (string, bool, error) {
		calls.Add(1)
		select {
		case entered <- struct{}{}: // 容量 1：只记录首次进入；重复进入不阻塞，也不像 close 那样 panic
		default:
		}
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
	<-entered // 第一个调用者已成为 singleflight 的 leader，并在 getter 内阻塞
	cancel()  // 取消第一个调用者：必须立刻返回，且不得取消共享 getter / 其他等待者

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("first err=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled GetHit must return quickly without waiting for the shared fetch")
	}

	type result struct {
		v   string
		err error
	}
	resCh := make(chan result, 1)
	go func() {
		v, _, err := p.GetHit(t.Context(), CacheContext{}, "k", getter)
		resCh <- result{v, err}
	}()

	// ---- 同步点（P3-15 治理，替代原 time.Sleep(50 * time.Millisecond)）----
	// 期望状态：第二个调用者已进入 singleflight 的等待队列，且尚未放行 getter。
	// 可观测性限制：等待队列无法从外部观测（singleflight 不暴露等待者），本用例用
	// "memCache.Get 调用次数 == 2"（第一个 + 第二个调用者各查一次缓存）作为近似同步点，
	// 再补 10ms 兜底等待，覆盖 "Get 返回 → DoChan 注册等待者" 之间的窗口。
	// 必要性：若在第二个调用者进入 DoChan 之前就放行 getter，首个 flight 会先完成，
	// 第二个调用者随后将发起新 flight 并再次进入 getter（断言 calls==1 会失败）。
	waitForGetCalls(t, mc, 2)
	time.Sleep(10 * time.Millisecond) // 兜底等待：只覆盖不可观测的微小窗口，远小于原 50ms
	releaseGetter()

	res := <-resCh
	if res.err != nil || res.v != "ok" {
		t.Fatalf("second got=%q err=%v", res.v, res.err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("getter calls=%d want 1", n)
	}
}

// ---------- F-10：刷新防重 ----------

func TestTryStartRefresh_ExclusiveAndReusable(t *testing.T) {
	p := &CacheProxy{} // 零值：refreshing 为 nil，同时验证懒初始化
	const key = "refresh:key"

	if !p.tryStartRefresh(key) {
		t.Fatal("首次 tryStartRefresh 应获得刷新权")
	}
	if p.tryStartRefresh(key) {
		t.Fatal("已持有刷新权时 tryStartRefresh 应返回 false")
	}
	// 释放未持有的 key 是空操作，不应影响已持有的 key
	p.endRefresh("other:key")
	if p.tryStartRefresh(key) {
		t.Fatal("释放其他 key 后，原 key 的刷新权应仍然持有")
	}

	p.endRefresh(key)
	if !p.tryStartRefresh(key) {
		t.Fatal("endRefresh 后应可再次获得刷新权")
	}
	p.endRefresh(key)
	// 重复释放不应 panic，且不应破坏正常语义
	p.endRefresh(key)
	if !p.tryStartRefresh(key) {
		t.Fatal("重复 endRefresh 后仍应能正常获得刷新权")
	}
	p.endRefresh(key)

	// 不同 key 的刷新权互相独立
	if !p.tryStartRefresh("k2") || !p.tryStartRefresh("k3") {
		t.Fatal("不同 key 的刷新权应互相独立")
	}
	p.endRefresh("k2")
	p.endRefresh("k3")
}

func TestTryStartRefresh_ConcurrentOnlyOneWins(t *testing.T) {
	const (
		key         = "concurrent:key"
		concurrency = 64
	)
	p := &CacheProxy{}

	start := make(chan struct{})
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		acquired int
	)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if p.tryStartRefresh(key) {
				mu.Lock()
				acquired++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if acquired != 1 {
		t.Fatalf("并发 tryStartRefresh 获得刷新权次数=%d, want 1", acquired)
	}

	p.endRefresh(key)
	if !p.tryStartRefresh(key) {
		t.Fatal("endRefresh 后应可再次获得刷新权")
	}
	p.endRefresh(key)
}

// ---------- F-9：回源超时与选项归一化 ----------

func TestWithFetchTimeout_Normalization(t *testing.T) {
	tests := []struct {
		name string
		opt  Option
		want time.Duration
	}{
		{name: "未传选项回退默认值", opt: nil, want: defaultFetchTimeout},
		{name: "0 回退默认值", opt: WithFetchTimeout(0), want: defaultFetchTimeout},
		{name: "负数回退默认值", opt: WithFetchTimeout(-time.Second), want: defaultFetchTimeout},
		{name: "正值原样保留", opt: WithFetchTimeout(20 * time.Millisecond), want: 20 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// &redis.Client{} 只用于构造，不发起任何连接
			p := newCacheProxy(&redis.Client{}, tt.opt)
			if p.fetchTimeout != tt.want {
				t.Fatalf("fetchTimeout=%v, want %v", p.fetchTimeout, tt.want)
			}
		})
	}

	// 等价于既有调用 Init(rdb)：无选项时使用默认值
	p := newCacheProxy(&redis.Client{})
	if p.fetchTimeout != defaultFetchTimeout {
		t.Fatalf("无选项时 fetchTimeout=%v, want %v", p.fetchTimeout, defaultFetchTimeout)
	}
}

func TestGetResource_CarriesFetchTimeoutDeadline(t *testing.T) {
	tests := []struct {
		name      string
		proxy     *CacheProxy
		wantUpper time.Duration
	}{
		{
			name:      "显式配置的超时",
			proxy:     &CacheProxy{getGroup: &singleflight.Group{}, fetchTimeout: time.Second},
			wantUpper: time.Second,
		},
		{
			name:      "零值字段回退默认超时（直接构造的零值安全）",
			proxy:     &CacheProxy{getGroup: &singleflight.Group{}},
			wantUpper: defaultFetchTimeout,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			type deadlineInfo struct {
				hasDeadline bool
				remaining   time.Duration
			}
			infoCh := make(chan deadlineInfo, 1)
			_, _, err := tt.proxy.getResource(t.Context(), "k", SingleGetterFunc(
				func(ctx context.Context, _ string) (string, bool, error) {
					d, ok := ctx.Deadline()
					info := deadlineInfo{hasDeadline: ok}
					if ok {
						info.remaining = time.Until(d)
					}
					infoCh <- info
					return "v", false, nil
				}))
			if err != nil {
				t.Fatalf("getResource err=%v", err)
			}
			info := <-infoCh
			if !info.hasDeadline {
				t.Fatal("回源 ctx 必须携带 deadline")
			}
			if info.remaining <= 0 || info.remaining > tt.wantUpper {
				t.Fatalf("回源 ctx 剩余时间=%v, 期望区间 (0, %v]", info.remaining, tt.wantUpper)
			}
			if info.remaining < tt.wantUpper/2 {
				t.Fatalf("回源 ctx 剩余时间=%v, 明显小于期望值 %v（疑似未使用 proxy 级 fetchTimeout）",
					info.remaining, tt.wantUpper)
			}
		})
	}
}

func TestGetHit_FetchTimeout(t *testing.T) {
	const timeout = 20 * time.Millisecond

	mc := newMemCache() // 空缓存 → 走未命中回源路径
	p := newCacheProxy(&redis.Client{}, WithFetchTimeout(timeout))
	p.cache = mc // 注入内存缓存，避免真实 Redis 依赖

	deadlineSeen := make(chan struct{}, 1)
	getter := SingleGetterFunc(func(ctx context.Context, _ string) (string, bool, error) {
		if _, ok := ctx.Deadline(); ok {
			deadlineSeen <- struct{}{} // 容量 1，非阻塞
		}
		select {
		case <-ctx.Done(): // 尊重 ctx：等待超时
			return "", false, ctx.Err()
		case <-time.After(2 * time.Second): // 防御：ctx 无 deadline 时不挂死测试
			return "", false, errors.New("getter: 回源 ctx 未在预期时间内到期")
		}
	})

	start := time.Now()
	_, _, err := p.GetHit(t.Context(), CacheContext{}, "k", getter)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("GetHit err=%v, want context.DeadlineExceeded", err)
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("GetHit 耗时=%v, 期望 <200ms（应由 fetchTimeout 触发返回）", elapsed)
	}
	select {
	case <-deadlineSeen:
	default:
		t.Fatal("getter 收到的 ctx 未携带 deadline（应传播 proxy 级超时）")
	}
}

// ---------- F-10 集成：并发过期请求只触发一次刷新 ----------

func TestGetHit_RefreshDedup_ConcurrentStaleRequestsCallGetterOnce(t *testing.T) {
	const (
		key         = "dedup:key"
		oldVal      = "stale"
		newVal      = "fresh"
		concurrency = 64
	)

	mc := newMemCache()
	mc.store[key] = StringView{Ctime: time.Now().Add(-time.Hour), Data: oldVal}
	// 本用例只验证防重，故给回源一个宽裕超时，避免慢机器把 getter 判成超时
	p := &CacheProxy{cache: mc, getGroup: &singleflight.Group{}, fetchTimeout: time.Minute}

	var calls atomic.Int32
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	getter := SingleGetterFunc(func(ctx context.Context, _ string) (string, bool, error) {
		calls.Add(1)
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
			return newVal, false, nil
		case <-ctx.Done():
			return "", false, ctx.Err()
		}
	})

	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, hit, err := p.GetHit(t.Context(),
				CacheContext{NeedCacheRefresh: true, RefreshOffset: time.Minute}, key, getter)
			if err != nil || !hit || v != oldVal {
				t.Errorf("GetHit got=%q hit=%v err=%v, want stale value", v, hit, err)
			}
		}()
	}
	wg.Wait()

	// 此刻所有 GetHit 都已返回，而唯一合法的刷新 goroutine 仍阻塞在 getter 内
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("刷新 getter 未被调用")
	}
	// 暴露窗口：若防重失效，其他刷新 goroutine 会进入 getter 并在窗口内被计数
	time.Sleep(200 * time.Millisecond)
	if n := calls.Load(); n != 1 {
		t.Fatalf("刷新期间 getter 调用次数=%d, want 1（同一 key 只允许一个刷新 goroutine）", n)
	}

	close(release)

	// 等待刷新写回，确认刷新流程正常结束且调用次数未增长
	deadline := time.Now().Add(2 * time.Second)
	for {
		sv, ok, sets := mc.snapshot(key)
		if ok && sets > 0 && sv.Data == newVal {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("刷新未写回缓存: ok=%v sets=%d data=%q", ok, sets, sv.Data)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("刷新完成后 getter 调用次数=%d, want 1", n)
	}
}

// ---------- F-8 效果 / F-11 上下文：miss 回源与异步写 ----------

func TestGetHit_CacheMissSelfHeals(t *testing.T) {
	const (
		key    = "self:heal"
		newVal = "fresh"
	)
	mc := newMemCache() // 空缓存：等价于"Redis 中值不可解析 → adaptor 按 miss 返回"（F-8）
	p := &CacheProxy{cache: mc, getGroup: &singleflight.Group{}}

	var calls atomic.Int32
	got, hit, err := p.GetHit(t.Context(), CacheContext{}, key, SingleGetterFunc(
		func(context.Context, string) (string, bool, error) {
			calls.Add(1)
			return newVal, false, nil
		}))
	if err != nil || hit || got != newVal {
		t.Fatalf("got=%q hit=%v err=%v, want miss+回源值", got, hit, err)
	}

	// 未命中回源后异步重写缓存（自愈）
	deadline := time.Now().Add(2 * time.Second)
	for {
		sv, ok, sets := mc.snapshot(key)
		if ok && sets == 1 && sv.Data == newVal {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("缓存未被重写: ok=%v sets=%d data=%q", ok, sets, sv.Data)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("getter 调用次数=%d, want 1", n)
	}
}

// ---------- F-8 上移：坏值 ErrCorruptedValue 由本层降级并记日志 ----------

// errGetCache 让 cache.Get 固定返回指定错误（err == nil 时走 memCache 正常逻辑），
// 用于模拟 adaptor 层的坏值上抛（ErrCorruptedValue）与真错误（网络故障等）。
type errGetCache struct {
	*memCache
	err error
}

func (e *errGetCache) Get(ctx context.Context, key string) (StringView, bool, error) {
	if e.err != nil {
		e.mu.Lock()
		e.gets++
		e.mu.Unlock()
		return StringView{}, false, e.err
	}
	return e.memCache.Get(ctx, key)
}

func TestGetHit_CorruptedValueTreatedAsMiss(t *testing.T) {
	const newVal = "fresh"

	core, observed := observer.New(zap.WarnLevel)
	mc := newMemCache()
	p := &CacheProxy{
		cache:    &errGetCache{memCache: mc, err: fmt.Errorf("%w, key(k) size(5)", ErrCorruptedValue)},
		log:      zap.New(core),
		getGroup: &singleflight.Group{},
	}

	var calls atomic.Int32
	got, hit, err := p.GetHit(t.Context(), CacheContext{}, "k", SingleGetterFunc(
		func(context.Context, string) (string, bool, error) {
			calls.Add(1)
			return newVal, false, nil
		}))
	if err != nil || hit || got != newVal {
		t.Fatalf("got=%q hit=%v err=%v, want 坏值按 miss 回源", got, hit, err)
	}

	// 降级 Warn 已输出，且携带 adaptor 附带的 key/size 上下文
	logs := observed.All()
	if len(logs) != 1 {
		t.Fatalf("坏值 Warn 输出次数=%d, want 1", len(logs))
	}
	if !strings.Contains(logs[0].Message, "treated as miss") || !strings.Contains(logs[0].Message, "key(k) size(5)") {
		t.Fatalf("unexpected log message %q", logs[0].Message)
	}

	// 回源结果异步写回（自愈）
	deadline := time.Now().Add(2 * time.Second)
	for {
		sv, ok, sets := mc.snapshot("k")
		if ok && sets > 0 && sv.Data == newVal {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("缓存未被重写: ok=%v sets=%d data=%q", ok, sets, sv.Data)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("getter 调用次数=%d, want 1", n)
	}
}

func TestGetHit_CorruptedValueLogsEveryOccurrence(t *testing.T) {
	core, observed := observer.New(zap.WarnLevel)
	p := &CacheProxy{
		cache:    &errGetCache{memCache: newMemCache(), err: fmt.Errorf("%w, key(k)", ErrCorruptedValue)},
		log:      zap.New(core),
		getGroup: &singleflight.Group{},
	}
	getter := SingleGetterFunc(func(context.Context, string) (string, bool, error) {
		return "fresh", false, nil
	})

	// 不做限流：每次坏值降级都输出 Warn，保证排障信息不丢（日志组件侧已有 buffer 兜底）
	for i := 0; i < 2; i++ {
		if _, _, err := p.GetHit(t.Context(), CacheContext{}, "k", getter); err != nil {
			t.Fatalf("GetHit #%d err=%v", i, err)
		}
	}
	if logs := observed.All(); len(logs) != 2 {
		t.Fatalf("坏值 Warn 输出次数=%d, want 2（每次降级均记录）", len(logs))
	}
}

func TestGetHit_CacheGetErrorPropagates(t *testing.T) {
	wantErr := errors.New("redis down")
	p := &CacheProxy{
		cache:    &errGetCache{memCache: newMemCache(), err: wantErr},
		getGroup: &singleflight.Group{},
	}
	_, _, err := p.GetHit(t.Context(), CacheContext{}, "k", SingleGetterFunc(
		func(context.Context, string) (string, bool, error) {
			t.Fatal("getter should not run")
			return "", false, nil
		}))
	if !errors.Is(err, wantErr) {
		t.Fatalf("err=%v, want %v", err, wantErr)
	}
}

type testCtxKeyType string

const testCtxKey testCtxKeyType = "trace-id"

// ctxCaptureCache 记录异步写缓存时收到的 ctx，用于验证 F-11 的"values 保留、取消不传播"。
type ctxCaptureCache struct {
	*memCache
	setCtx chan context.Context
}

func newCtxCaptureCache() *ctxCaptureCache {
	return &ctxCaptureCache{memCache: newMemCache(), setCtx: make(chan context.Context, 1)}
}

func (c *ctxCaptureCache) Set(ctx context.Context, key string, value StringView, expired, emptyExpired time.Duration) error {
	select {
	case c.setCtx <- ctx:
	default:
	}
	return c.memCache.Set(ctx, key, value, expired, emptyExpired)
}

func TestGetHit_MissAsyncWriteKeepsValuesAndIgnoresCancel(t *testing.T) {
	mc := newCtxCaptureCache()
	p := &CacheProxy{cache: mc, getGroup: &singleflight.Group{}}

	parent, cancel := context.WithCancel(context.WithValue(t.Context(), testCtxKey, "trace-1"))
	got, _, err := p.GetHit(parent, CacheContext{}, "k", SingleGetterFunc(
		func(context.Context, string) (string, bool, error) {
			return "fresh", false, nil
		}))
	if err != nil || got != "fresh" {
		t.Fatalf("got=%q err=%v", got, err)
	}
	cancel() // 模拟请求结束/取消：异步写不应被中断，values 仍应保留

	select {
	case writeCtx := <-mc.setCtx:
		if writeCtx.Err() != nil {
			t.Fatalf("异步写 ctx 不应随请求取消: err=%v", writeCtx.Err())
		}
		if v, _ := writeCtx.Value(testCtxKey).(string); v != "trace-1" {
			t.Fatalf("异步写 ctx 丢失调用方 values: %v", writeCtx.Value(testCtxKey))
		}
		select {
		case <-writeCtx.Done():
			t.Fatal("异步写 ctx 不应被取消")
		default:
		}
	case <-time.After(2 * time.Second):
		t.Fatal("异步写未调用 cache.Set")
	}
}

// ---------- F-17：并发合并回源（P2-12 测试缺口） ----------

// TestGetHit_ConcurrentMissCallsGetterOnce 锁定 singleflight 的并发合并语义：
// 同一 key、缓存为空时，N 个并发 GetHit 只允许 getter 被执行一次，
// 且所有调用方都拿到同一次回源的结果（未命中，hit=false）。
func TestGetHit_ConcurrentMissCallsGetterOnce(t *testing.T) {
	const (
		key         = "concurrent:miss"
		newVal      = "fresh"
		concurrency = 64
	)

	mc := newMemCache() // 空缓存：所有调用方都走"未命中 → 回源"路径
	// 回源超时给足（同 F-10 集成用例的做法）：本用例只验证合并，
	// 避免慢机器把阻塞的 getter 判成超时。
	p := &CacheProxy{cache: mc, getGroup: &singleflight.Group{}, fetchTimeout: time.Minute}

	var calls atomic.Int32
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseGetter := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseGetter() // 失败路径也必须放行 getter，避免留下阻塞的 goroutine

	getter := SingleGetterFunc(func(ctx context.Context, _ string) (string, bool, error) {
		calls.Add(1)
		select {
		case entered <- struct{}{}: // 容量 1：只记录首次进入；若合并失效出现二次调用也不阻塞、不 panic
		default:
		}
		select {
		case <-release:
			return newVal, false, nil
		case <-ctx.Done(): // 尊重 ctx：防御性返回，避免异常时永久挂住
			return "", false, ctx.Err()
		}
	})

	type result struct {
		v   string
		hit bool
		err error
	}
	var wg sync.WaitGroup
	results := make([]result, concurrency) // 每个 goroutine 只写自己的下标，无数据竞争
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, hit, err := p.GetHit(t.Context(), CacheContext{}, key, getter)
			results[i] = result{v: v, hit: hit, err: err}
		}(i)
	}

	// ---- 同步点（singleflight 的等待者集合无法从外部观测）----
	// 1) 等 leader 进入 getter：此刻 flight 已建立且被 getter 阻塞；只要不放行 release，
	//    尚未进入 Do 的调用方仍有机会加入同一个 flight；
	// 2) 轮询 memCache.Get 调用次数到 N：未命中路径下每个 GetHit 都先查缓存再进 Do，
	//    该计数是"调用方已通过缓存查找"的唯一可观测前置状态；
	// 3) 50ms 兜底等待：覆盖 "Get 返回 → Do 注册等待者" 的不可观测窗口。
	// 失败判定方向：若某调用者晚于放行才进入 Do，它会发起新 flight 并再次调用 getter，
	// 断言 calls==1 必然失败 —— 本用例对"合并失效"宁可误报，不可漏报。
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("getter 未被调用：并发未命中回源未进入 singleflight")
	}
	waitForGetCalls(t, mc, concurrency)
	time.Sleep(50 * time.Millisecond)
	releaseGetter()

	wg.Wait()

	if n := calls.Load(); n != 1 {
		t.Fatalf("并发未命中回源 getter 调用次数=%d, want 1（singleflight 应把同一 key 的并发回源合并为一次）", n)
	}
	for i, r := range results {
		if r.err != nil || r.hit || r.v != newVal {
			t.Fatalf("调用方#%d got=%q hit=%v err=%v, want 未命中且值为 %q", i, r.v, r.hit, r.err, newVal)
		}
	}

	// 未命中回源后缓存应被异步写回。注意：每个调用方各自发起一次异步写（现状语义），
	// 因此只断言"至少成功写回一次"，不做 sets 精确计数；用轮询容忍异步写延迟。
	deadline := time.Now().Add(2 * time.Second)
	for {
		sv, ok, sets := mc.snapshot(key)
		if ok && sets > 0 && sv.Data == newVal {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("回源结果未异步写回缓存: ok=%v sets=%d data=%q", ok, sets, sv.Data)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
