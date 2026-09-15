package cacheproxy

import (
	"errors"
	"testing"

	"github.com/redis/go-redis/v9"
)

func TestRedisCache_EmptyKey(t *testing.T) {
	c := &RedisCache{rdb: &redis.Client{}}
	_, _, err := c.Get(t.Context(), "")
	if !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("Get err=%v", err)
	}
	if err := c.Set(t.Context(), "", StringView{Data: "v"}, 0, 0); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("Set err=%v", err)
	}
	if err := c.Remove(t.Context(), ""); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("Remove err=%v", err)
	}
}

func TestRedisCache_NilClient(t *testing.T) {
	var c *RedisCache
	_, _, err := c.Get(t.Context(), "k")
	if !errors.Is(err, ErrNilRedis) {
		t.Fatalf("err=%v", err)
	}
	c = &RedisCache{}
	if err := c.Set(t.Context(), "k", StringView{}, 0, 0); !errors.Is(err, ErrNilRedis) {
		t.Fatalf("err=%v", err)
	}
}

func TestRedisCache_MGetMSetUnsupported(t *testing.T) {
	c := &RedisCache{rdb: &redis.Client{}}
	_, err := c.MGet(t.Context(), []string{"a"})
	if !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("MGet err=%v", err)
	}
	err = c.MSet(t.Context(), []string{"a"}, []StringView{{}}, 0, 0)
	if !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("MSet err=%v", err)
	}
}
