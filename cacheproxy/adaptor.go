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
	ErrInvalidKey     = errors.New("empty key")
	ErrMismatchedPair = errors.New(" keys and values mismatch")
	ErrNilRedis       = errors.New("cacheproxy: empty redis client")
)

type RedisCache struct {
	rdb *redis.Client
}

func NewRedisAdaptor(rdb *redis.Client) *RedisCache {
	return &RedisCache{rdb: rdb}
}

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
	err = sonic.UnmarshalString(result, &res)
	if err != nil {
		return StringView{IsNil: true}, false, err
	}
	return res, true, nil
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
	expired := expiredTime
	if len(value.Data) == 0 {
		expired = emptyExpiredTime
	}
	_, err = c.rdb.Set(ctx, key, valStr, expired).Result()
	return err
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
