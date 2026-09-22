package cacheproxy

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/TomWu-Alchemi/project-framework/logger"
	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"
)

type SingleGetter interface {
	Get(ctx context.Context, key string) (string, bool, error)
}

// MissedGetter（F-34 标记 deprecated）：当前全仓库无引用，为 MGet / MSet 批量
// 接口预留（见第一轮 S-5 决策）；计划下个主版本随批量接口实现决策一并处理，
// 勿在新代码中依赖。
type MissedGetter interface {
	Get(ctx context.Context, missedKey []string) (map[string]string, error)
}

type SingleGetterFunc func(ctx context.Context, key string) (string, bool, error)

func (f SingleGetterFunc) Get(ctx context.Context, key string) (string, bool, error) {
	return f(ctx, key)
}

// MissedGetterFunc（F-34 标记 deprecated）：见 MissedGetter 说明。
type MissedGetterFunc func(ctx context.Context, missedKey []string) (map[string]string, error)

func (f MissedGetterFunc) Get(ctx context.Context, missedKey []string) (map[string]string, error) {
	return f(ctx, missedKey)
}

const (
	defaultExpiredTime      = 24 * time.Hour
	defaultEmptyExpiredTime = time.Minute
	defaultRefreshTime      = 10 * time.Minute

	// defaultFetchTimeout 是回源（singleflight 执行体）的默认超时上限（F-9）。
	// 2026-09-16 决策值：5s；<=0 的配置与此常量之外的取值一律回退到该值。
	defaultFetchTimeout = 5 * time.Second
)

var (
	once sync.Once

	// defaultProxyPtr 用 atomic.Pointer 发布（F-24）：Init 写入与业务 goroutine 的
	// GetInstance 读取之间不再有数据竞争（与 logger 包 F-12 的手法一致）。
	// Load() 为 nil 表示尚未 Init，ready() 对 nil 做防御。
	defaultProxyPtr atomic.Pointer[CacheProxy]

	fastRequeryErr        = errors.New("need fast requery")
	ErrNotInitialized     = errors.New("cacheproxy: not initialized, call Init first")
	ErrAlreadyInitialized = errors.New("cacheproxy: already initialized")
)

// options 是 CacheProxy 的构造选项聚合体（F-9）。新增选项时在 WithXxx 中归一化后再写入。
type options struct {
	fetchTimeout time.Duration
	log          *zap.Logger
}

// Option 是 CacheProxy 的构造选项。
type Option func(*options)

// WithLogger 注入 *zap.Logger。l == nil 时字段保持 nil，回退全局 logger（既有行为）。
func WithLogger(l *zap.Logger) Option {
	return func(o *options) {
		o.log = l
	}
}

// WithFetchTimeout 设置回源超时上限（proxy 级全局配置，F-9）。
//
// 为什么是 proxy 级而不是 CacheContext 字段：singleflight 下同 key 的并发调用只执行一次回源，
// 若超时值随调用方不同将产生"以谁的配置为准"的歧义；proxy 级配置无歧义。
//
// d <= 0 时回退 defaultFetchTimeout，与项目"<=0 用默认值"的一致约定相同。
func WithFetchTimeout(d time.Duration) Option {
	return func(o *options) {
		o.fetchTimeout = durationOrDefault(d, defaultFetchTimeout)
	}
}

// CacheProxy 是缓存代理：单飞回源 + 逻辑过期异步刷新 + 空值缓存。
// 内部含互斥量（refreshMu），只能以 *CacheProxy 使用，禁止值拷贝。
type CacheProxy struct {
	cache    Cache
	getGroup *singleflight.Group
	log      *zap.Logger // nil => 回退全局 logger.Errorf/Warnf

	// fetchTimeout 是回源（singleflight 执行体）的超时上限（F-9），proxy 级全局配置。
	// 通过 Init(rdb, WithFetchTimeout(d)) 设置；构造时与 getResource 读取时都会用
	// durationOrDefault 归一化，因此零值 CacheProxy（直接构造）同样安全。
	fetchTimeout time.Duration

	// refreshMu / refreshing 保证"同一 key 同时最多一个刷新 goroutine"（F-10），
	// 使刷新 goroutine 数量上限 = 活跃 key 数，而不是请求量。
	// refreshing 懒初始化：零值（nil map）下 tryStartRefresh/endRefresh 均安全。
	refreshMu  sync.Mutex
	refreshing map[string]struct{}
}

type CacheContext struct {
	NeedForceRefresh  bool
	NeedCacheRefresh  bool
	RefreshOffset     time.Duration
	FastRefreshOffset time.Duration
	ExpiredTime       time.Duration
	EmptyExpiredTime  time.Duration
}

// Init 初始化全局代理。rdb == nil 时返回 ErrNilRedis，且不消耗 once，随后仍可合法 Init。
// 合法第一次写入代理；之后若发现本次创建的对象未被 Store，返回 ErrAlreadyInitialized。
func Init(rdb *redis.Client, opts ...Option) error {
	if rdb == nil {
		return ErrNilRedis
	}
	proxy := newCacheProxy(rdb, opts...)
	once.Do(func() {
		defaultProxyPtr.Store(proxy)
	})
	if defaultProxyPtr.Load() != proxy {
		return ErrAlreadyInitialized
	}
	return nil
}

// resetInit 仅供包内测试重置全局 Init 状态。不要在生产路径调用。
func resetInit() {
	once = sync.Once{}
	defaultProxyPtr.Store(nil)
}

func GetInstance() *CacheProxy {
	return defaultProxyPtr.Load()
}

func newCacheProxy(rdb *redis.Client, opts ...Option) *CacheProxy {
	o := options{fetchTimeout: defaultFetchTimeout}
	for _, opt := range opts {
		if opt == nil { // 允许调用方传入 nil 选项
			continue
		}
		opt(&o)
	}
	rc := NewRedisAdaptor(rdb)
	return &CacheProxy{
		cache:    rc,
		getGroup: &singleflight.Group{},
		log:      o.log,
		// 双重归一化：WithFetchTimeout 内已归一化；此处兜住"未传选项"与自定义 Option 的写入。
		// 与 getResource 的读取侧归一化共用同一常量，不存在语义分叉。
		fetchTimeout: durationOrDefault(o.fetchTimeout, defaultFetchTimeout),
	}
}

// GetHit string：存储值，bool：是否在缓存中找到，error：错误
//
// 整合后的路径语义（批 2）：
//   - NeedForceRefresh：回源 + 同步写缓存，写失败仅记日志、不影响返回值（F-11）；
//   - 缓存未命中：回源 + 异步写缓存；异步写使用 context.WithoutCancel(ctx)，
//     保留 trace/values 且不随请求取消中断（F-11）；
//     Redis 中"存在但不可解析"的数据由 adaptor 以 ErrCorruptedValue 上抛（F-8），
//     本层按 miss 降级（回源重写自愈）并记 Warn 日志；
//   - 缓存命中且逻辑过期：异步刷新，同一 key 同时最多一个刷新 goroutine（F-10），
//     未抢到刷新权的请求直接返回旧值（语义同改动前）；
//   - 以上三条回源路径的超时上限统一由 getResource 的 fetchTimeout 控制（F-9）。
func (p *CacheProxy) GetHit(ctx context.Context, c CacheContext, key string, getter SingleGetter) (string, bool, error) {
	if err := p.ready(); err != nil {
		return "", false, err
	}
	if key == "" {
		return "", false, ErrInvalidKey
	}

	// 强制刷新，不查询缓存，只回源并对缓存赋值
	if c.NeedForceRefresh {
		data, needFastRequery, err := p.getResource(ctx, key, getter)
		if err != nil {
			return "", false, err
		}
		// WithoutCancel 与 miss 异步写 / 刷新路径对称（F-25）：保留调用方 values
		// （trace 等），且同步写不随请求取消中断。
		if err = p.setData(context.WithoutCancel(ctx), c, key, data, needFastRequery); err != nil {
			p.errorf("cacheProxy force setData err: %v", err)
		}
		return data, false, nil
	}

	sv, exist, err := p.cache.Get(ctx, key)
	if err != nil {
		// F-8：值存在但不可解析（历史格式变更 / 共用实例误写 / 人工写入）不是本层能
		// 修复的错误，adaptor 以 ErrCorruptedValue 包装 key/size 上抛；此处降级为 miss
		// 走回源重写自愈，并记 Warn 日志。其余真错误（客户端不可用 / 网络错误 / 参数
		// 错误）原样返回。
		if !errors.Is(err, ErrCorruptedValue) {
			return "", false, err
		}
		p.warnf("cacheProxy corrupted value treated as miss: %v", err)
		exist = false
	}
	if !exist {
		// 缓存未命中，回源并写入
		data, needFastRequery, err := p.getResource(ctx, key, getter)
		if err != nil {
			return "", false, err
		}
		// 异步写入：context.WithoutCancel 保留调用方 values（trace 等）且不随请求取消中断（F-11）；
		// 写超时由 redis 客户端自身配置兜底，这里不额外加 deadline。
		writeCtx := context.WithoutCancel(ctx)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					p.errorf("cacheProxy setData panic: %v", r)
				}
			}()
			if setErr := p.setData(writeCtx, c, key, data, needFastRequery); setErr != nil {
				p.errorf("cacheProxy setErr: %v", setErr)
			}
		}()
		return data, false, nil
	}

	if c.NeedCacheRefresh {
		if !sv.IsExpire(c.RefreshOffset, c.FastRefreshOffset) {
			return sv.String(), true, nil
		}
		// 过期刷新：同一 key 同时最多一个刷新 goroutine（F-10）
		if p.tryStartRefresh(key) {
			go func() {
				// defer 顺序（后注册先执行）：先 recover 捕获 panic，再 endRefresh 释放刷新权，
				// 因此正常返回与 panic 两条路径都会释放，不会把 key 永久锁在 refreshing 中。
				defer p.endRefresh(key)
				defer func() {
					if r := recover(); r != nil {
						p.errorf("cacheProxy refresh panic: %v", r)
					}
				}()
				// 刷新上下文用 WithoutCancel：保留调用方 values（trace 等）且不随请求取消中断（F-11）；
				// getResource 内部仍会剥离取消并施加 fetchTimeout 超时（F-9）。
				newCtx := context.WithoutCancel(ctx)
				data, needFastRequery, err2 := p.getResource(newCtx, key, getter)
				if err2 != nil {
					p.errorf("cacheProxy refresh getResource err: %v", err2)
					return
				}
				if err2 = p.setData(newCtx, c, key, data, needFastRequery); err2 != nil {
					p.errorf("cacheProxy refresh setData err: %v", err2)
				}
			}()
		}
	}

	return sv.String(), true, nil
}

// tryStartRefresh 尝试获取 key 的刷新权（F-10）：true 表示由当前调用方启动刷新 goroutine。
// refreshing 为 nil 时懒初始化，零值 CacheProxy（直接构造）同样安全。
//
// 降级语义（有意设计）：若刷新 goroutine 因 getter 不尊重 ctx 而长期不结束，
// 该 key 会暂时不再触发刷新（请求继续返回旧值，可用性不受影响）；这是防重与
// "避免 goroutine 堆积"之间的取舍，getter 应尊重 ctx（见 getResource 能力边界说明）。
func (p *CacheProxy) tryStartRefresh(key string) bool {
	p.refreshMu.Lock()
	defer p.refreshMu.Unlock()
	if _, ok := p.refreshing[key]; ok {
		return false
	}
	if p.refreshing == nil {
		p.refreshing = make(map[string]struct{})
	}
	p.refreshing[key] = struct{}{}
	return true
}

// endRefresh 释放 key 的刷新权。必须与返回 true 的 tryStartRefresh 成对（用 defer 保证 panic 路径也释放）。
// 未持有时调用是安全的空操作（delete(nil map, k) 合法）。
func (p *CacheProxy) endRefresh(key string) {
	p.refreshMu.Lock()
	defer p.refreshMu.Unlock()
	delete(p.refreshing, key)
}

func (p *CacheProxy) errorf(template string, args ...any) {
	if p != nil && p.log != nil {
		p.log.Sugar().Errorf(template, args...)
		return
	}
	logger.Errorf(template, args...)
}

func (p *CacheProxy) warnf(template string, args ...any) {
	if p != nil && p.log != nil {
		p.log.Sugar().Warnf(template, args...)
		return
	}
	logger.Warnf(template, args...)
}

func (p *CacheProxy) Set(ctx context.Context, c CacheContext, key string, value string) error {
	if err := p.ready(); err != nil {
		return err
	}
	return p.setData(ctx, c, key, value, false)
}

// Remove 删除指定 key 的缓存。
//
// 参数说明（P3-1）：c（CacheContext）在本方法中**未被使用** —— 删除操作不涉及
// TTL（ExpiredTime / EmptyExpiredTime）、刷新偏移（RefreshOffset / FastRefreshOffset）、
// 强制刷新（NeedForceRefresh）等任何 CacheContext 字段。
// 保留该参数是为了与 GetHit / Set 保持一致的调用形态、避免破坏既有调用方的编译兼容；
// 调用方直接传 CacheContext{} 即可。如后续确有"按上下文差异化删除"的扩展需求
// （例如按 key 前缀批量删除的配置项），再从该参数扩展，而不是现在移除它。
func (p *CacheProxy) Remove(ctx context.Context, c CacheContext, key string) error {
	if err := p.ready(); err != nil {
		return err
	}
	return p.cache.Remove(ctx, key)
}

func (p *CacheProxy) ready() error {
	if p == nil || p.cache == nil || p.getGroup == nil {
		return ErrNotInitialized
	}
	return nil
}

func (p *CacheProxy) getResource(ctx context.Context, key string, getter SingleGetter) (string, bool, error) {
	// 回源超时（F-9 / P2-1）：
	//  1. DoChan 让调用方可在自身 ctx 取消时立刻返回，不必等共享 getter 结束；
	//  2. 超时 ctx 必须在 DoChan 回调内创建并 defer cancel()。若把 WithTimeout 放在
	//     等待者一侧并在 ctx.Done() 时提前 return，defer cancel() 会取消共享 getter，
	//     导致同 key 其他等待者失败；
	//  3. context.WithoutCancel(ctx) 保留首个调用方 values（trace 等），且不把该调用方
	//     的取消传播进 getter；再套 fetchTimeout 作为共享回源上限；
	//  4. 读取侧归一化：fetchTimeout <= 0 回退 defaultFetchTimeout。
	//
	// 能力边界：Go 无法强制中断不尊重 ctx 的 goroutine。超时保证是
	// "flightCtx 携带 deadline 并传递到 getter"；只有 getter 尊重 ctx 时才会在超时后返回
	// context.DeadlineExceeded —— 该错误由 singleflight 共享给仍在等待的调用方。
	// 已因自身 ctx 取消而返回的调用方不受影响。取消等待者时不 Forget(key)，
	// 以免拆掉仍在执行的共享 flight。
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	timeout := durationOrDefault(p.fetchTimeout, defaultFetchTimeout)
	ch := p.getGroup.DoChan(key, func() (out any, retErr error) {
		// F-22：getter 是业务方注入的外部依赖，panic 在此收敛为 error。
		// x/sync 对 fn panic 的既有行为是领导者重抛、等待者同步 panic
		// （singleflight.go:101-103,163-170）；不收敛则一个 panic getter
		// 会让同 key 的全部在途请求一起失败。singleflight 在 doCall 结束后
		// 即从合并表删除该 key，无需额外 Forget。
		defer func() {
			if r := recover(); r != nil {
				out = nil
				retErr = fmt.Errorf("cacheproxy: getter panic, key(%s): %v", key, r)
				p.errorf("cacheproxy: getter panic recovered, key(%s) panic(%v) stack(%s)", key, r, debug.Stack())
			}
		}()
		flightCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
		defer cancel()
		data, needFastRequery, getErr := getter.Get(flightCtx, key)
		if getErr != nil {
			return nil, getErr
		}
		if needFastRequery {
			return data, fastRequeryErr
		}
		return data, nil
	})
	return awaitFlight(ctx, ch)
}

// awaitFlight 等待 singleflight 结果；调用方 ctx 取消时若结果通道已有缓冲结果则优先返回结果。
func awaitFlight(ctx context.Context, ch <-chan singleflight.Result) (string, bool, error) {
	select {
	case <-ctx.Done():
		select {
		case r := <-ch:
			return decodeFlightResult(r)
		default:
			return "", false, ctx.Err()
		}
	case r := <-ch:
		return decodeFlightResult(r)
	}
}

func decodeFlightResult(r singleflight.Result) (string, bool, error) {
	if r.Err != nil && !errors.Is(r.Err, fastRequeryErr) {
		return "", false, r.Err
	}
	res, ok := r.Val.(string)
	if !ok {
		return "", false, fmt.Errorf("cacheproxy: unexpected singleflight type %T", r.Val)
	}
	if errors.Is(r.Err, fastRequeryErr) {
		return res, true, nil
	}
	return res, false, nil
}

func (p *CacheProxy) setData(ctx context.Context, c CacheContext, key string, data string, needFastRequery bool) error {
	sv := StringView{
		Ctime:           time.Now(),
		NeedFastRequery: needFastRequery,
		IsNil:           false,
		Data:            data,
	}
	return p.cache.Set(ctx, key, sv,
		durationOrDefault(c.ExpiredTime, defaultExpiredTime),
		durationOrDefault(c.EmptyExpiredTime, defaultEmptyExpiredTime),
	)
}

func durationOrDefault(d, fallback time.Duration) time.Duration {
	if d <= 0 {
		return fallback
	}
	return d
}
