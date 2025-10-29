package cacheproxy

import (
	"context"
	"errors"
	"github.com/bytedance/sonic"
	"github.com/redis/go-redis/v9"
	"time"
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
)

type RedisCache struct {
	rdb *redis.Client
}

func NewRedisAdaptor(rdb *redis.Client) *RedisCache {
	return &RedisCache{rdb: rdb}
}

func (c *RedisCache) Get(ctx context.Context, key string) (StringView, bool, error) {
	if c.rdb == nil {
		panic("empty redis client")
	}
	res := StringView{}
	if len(key) < 0 {
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
	if c.rdb == nil {
		panic("empty redis client")
	}
	if len(key) <= 0 {
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
	if c.rdb == nil {
		panic("empty redis client")
	}
	if len(key) <= 0 {
		return ErrInvalidKey
	}
	_, err := c.rdb.Del(ctx, key).Result()
	return err
}

func (c *RedisCache) MGet(ctx context.Context, keys []string) ([]StringView, error) {
	//TODO implement me
	return nil, nil
}

func (c *RedisCache) MSet(ctx context.Context, keys []string, values []StringView, expiredTime time.Duration, emptyExpiredTime time.Duration) error {
	//TODO implement me
	return nil
}
