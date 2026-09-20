package cacheproxy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bytedance/sonic"
	"github.com/redis/go-redis/v9"
)

type Cache interface {
	Get(ctx context.Context, key string) (StringView, bool, error)
	Set(ctx context.Context, key string, value StringView, expiredTime time.Duration, emptyExpiredTime time.Duration) error
	Remove(ctx context.Context, key string) error
	MGet(ctx context.Context, keys []string) ([]StringView, error)
	MSet(ctx context.Context, keys []string, values []StringView, expiredTime time.Duration, emptyExpiredTime time.Duration) error
}

var (
	ErrInvalidKey = errors.New("empty key")

	// ErrMismatchedPair 预留给 MGet / MSet 的"keys 与 values 数量不一致"场景。
	// 当前 MGet / MSet 直接返回 ErrUnsupported（见 S-5 决策），本变量暂无调用点，
	// 保留以供后续实现与调用方错误判断使用。
	//
	// P3-13：仅去掉错误消息的前导空格（原为 " keys and values mismatch"），
	// 变量本身保留（S-5），消息格式与其余两个错误对齐。
	ErrMismatchedPair = errors.New("keys and values mismatch")

	ErrNilRedis = errors.New("cacheproxy: empty redis client")

	// ErrCorruptedValue 表示缓存中存在值但不可解析（F-8：历史格式变更 / 其他系统
	// 共用实例 / 人工写入）。Cache 接口约定：Get 实现遇到该场景时应返回包装本
	// sentinel 的错误（附 key 与原始数据字节数，不携带原始内容，避免日志泄漏与膨胀），
	// 由 CacheProxy 按 miss 降级（回源重写自愈）并记 Warn 日志。
	// 适配层自身不做降级决策、不打日志。
	ErrCorruptedValue = errors.New("cacheproxy: corrupted cache value")
)

type RedisCache struct {
	rdb *redis.Client
}

func NewRedisAdaptor(rdb *redis.Client) *RedisCache {
	return &RedisCache{rdb: rdb}
}

// Get 读取并解析缓存值。
//
// 返回 (StringView, false, nil) 表示未命中（key 不存在，redis.Nil）。
//
// 值存在但不可解析（F-8：历史格式变更 / 其他系统共用实例 / 人工写入）时，返回包装
// ErrCorruptedValue 的错误（附 key 与原始数据字节数）。适配层不做降级决策、不打日志，
// 由上层 CacheProxy 按 miss 处理实现回源自愈并记 Warn 日志。
//
// 真错误（客户端不可用、网络错误、参数错误）仍然返回 error。
func (c *RedisCache) Get(ctx context.Context, key string) (StringView, bool, error) {
	res := StringView{}
	if err := c.ready(); err != nil {
		return res, false, err
	}
	if key == "" {
		return res, false, ErrInvalidKey
	}
	result, err := c.rdb.Get(ctx, key).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return StringView{IsNil: true}, false, nil
		}
		return res, false, err
	}
	sv, ok := decodeStringView(result)
	if !ok {
		// 值存在但不可解析：以 sentinel 包装 key/size 上抛，降级决策与日志归上层（F-8 / F-23）。
		return StringView{}, false, fmt.Errorf("%w, key(%s) size(%d)", ErrCorruptedValue, key, len(result))
	}
	return sv, true, nil
}

// decodeStringView 解析缓存中的原始值；ok=false 表示数据不可用（按 miss 处理，F-8）。
// 规则：空字符串 → 不可用；sonic 反序列化失败 → 不可用；成功 → 可用（允许字段缺失，得到零值字段）。
func decodeStringView(raw string) (StringView, bool) {
	var sv StringView
	if raw == "" {
		return StringView{}, false
	}
	if err := sonic.UnmarshalString(raw, &sv); err != nil {
		return StringView{}, false
	}
	return sv, true
}

func (c *RedisCache) Set(ctx context.Context, key string, value StringView, expiredTime time.Duration, emptyExpiredTime time.Duration) error {
	if err := c.ready(); err != nil {
		return err
	}
	if key == "" {
		return ErrInvalidKey
	}
	valStr, err := sonic.MarshalString(value)
	if err != nil {
		return err
	}
	expired := resolveExpiration(len(value.Data), expiredTime, emptyExpiredTime)
	_, err = c.rdb.Set(ctx, key, valStr, expired).Result()
	return err
}

// resolveExpiration 计算写缓存使用的 TTL：空数据（dataLen == 0）用 emptyExpiredTime，
// 否则用 expiredTime；对应值为 <= 0 时回退到默认常量（与 CacheProxy.setData 行为一致）。
func resolveExpiration(dataLen int, expiredTime, emptyExpiredTime time.Duration) time.Duration {
	if dataLen == 0 {
		return durationOrDefault(emptyExpiredTime, defaultEmptyExpiredTime)
	}
	return durationOrDefault(expiredTime, defaultExpiredTime)
}

func (c *RedisCache) Remove(ctx context.Context, key string) error {
	if err := c.ready(); err != nil {
		return err
	}
	if key == "" {
		return ErrInvalidKey
	}
	_, err := c.rdb.Del(ctx, key).Result()
	return err
}

func (c *RedisCache) MGet(_ context.Context, _ []string) ([]StringView, error) {
	return nil, fmtUnsupported("MGet")
}

func (c *RedisCache) MSet(_ context.Context, _ []string, _ []StringView, _ time.Duration, _ time.Duration) error {
	return fmtUnsupported("MSet")
}

func (c *RedisCache) ready() error {
	if c == nil || c.rdb == nil {
		return ErrNilRedis
	}
	return nil
}

func fmtUnsupported(op string) error {
	return fmt.Errorf("cacheproxy: %s: %w", op, errors.ErrUnsupported)
}
