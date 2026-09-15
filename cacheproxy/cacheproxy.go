package cacheproxy

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/TomWu-Alchemi/project-framework/logger"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
)

type SingleGetter interface {
	Get(ctx context.Context, key string) (string, bool, error)
}

type MissedGetter interface {
	Get(ctx context.Context, missedKey []string) (map[string]string, error)
}

type SingleGetterFunc func(ctx context.Context, key string) (string, bool, error)

func (f SingleGetterFunc) Get(ctx context.Context, key string) (string, bool, error) {
	return f(ctx, key)
}

type MissedGetterFunc func(ctx context.Context, missedKey []string) (map[string]string, error)

func (f MissedGetterFunc) Get(ctx context.Context, missedKey []string) (map[string]string, error) {
	return f(ctx, missedKey)
}

const (
	defaultExpiredTime      = 24 * time.Hour
	defaultEmptyExpiredTime = time.Minute
	defaultRefreshTime      = 10 * time.Minute
)

var (
	once         sync.Once
	defaultProxy *CacheProxy

	fastRequeryErr    = errors.New("need fast requery")
	ErrNotInitialized = errors.New("cacheproxy: not initialized, call Init first")
)

type CacheProxy struct {
	cache    Cache
	getGroup *singleflight.Group
}

type CacheContext struct {
	NeedForceRefresh  bool
	NeedCacheRefresh  bool
	RefreshOffset     time.Duration
	FastRefreshOffset time.Duration
	ExpiredTime       time.Duration
	EmptyExpiredTime  time.Duration
}

func Init(rdb *redis.Client) {
	once.Do(func() {
		defaultProxy = newCacheProxy(rdb)
	})
}

func GetInstance() *CacheProxy {
	return defaultProxy
}

func newCacheProxy(rdb *redis.Client) *CacheProxy {
	return &CacheProxy{
		cache:    NewRedisAdaptor(rdb),
		getGroup: &singleflight.Group{},
	}
}

// GetHit string：存储值，bool：是否在缓存中找到，error：错误
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
		if err = p.setData(context.Background(), c, key, data, needFastRequery); err != nil {
			logger.Errorf("cacheProxy force setData err: %v", err)
		}
		return data, false, nil
	}

	sv, exist, err := p.cache.Get(ctx, key)
	if err != nil {
		return "", false, err
	}
	if !exist {
		// 缓存未命中，回源并写入
		data, needFastRequery, err := p.getResource(ctx, key, getter)
		if err != nil {
			return "", false, err
		}
		// 异步写入
		go func() {
			defer func() {
				if r := recover(); r != nil {
					logger.Errorf("cacheProxy setData panic: %v", r)
				}
			}()
			setErr := p.setData(context.Background(), c, key, data, needFastRequery)
			if setErr != nil {
				logger.Errorf("cacheProxy setErr: %v", setErr)
			}
		}()
		return data, false, nil
	}

	if c.NeedCacheRefresh {
		if !sv.IsExpire(c.RefreshOffset, c.FastRefreshOffset) {
			return sv.String(), true, nil
		}
		// 过期刷新
		go func() {
			defer func() {
				if r := recover(); r != nil {
					logger.Errorf("cacheProxy refresh panic: %v", r)
				}
			}()
			newCtx := context.Background()
			data, needFastRequery, err2 := p.getResource(newCtx, key, getter)
			if err2 != nil {
				logger.Errorf("cacheProxy refresh getResource err: %v", err2)
				return
			}
			err2 = p.setData(newCtx, c, key, data, needFastRequery)
			if err2 != nil {
				logger.Errorf("cacheProxy refresh setData err: %v", err2)
			}
		}()
	}

	return sv.String(), true, nil
}

func (p *CacheProxy) Set(ctx context.Context, c CacheContext, key string, value string) error {
	if err := p.ready(); err != nil {
		return err
	}
	return p.setData(ctx, c, key, value, false)
}

func (p *CacheProxy) Remove(ctx context.Context, c CacheContext, key string) error {
	if err := p.ready(); err != nil {
		return err
	}
	return p.cache.Remove(ctx, key)
}

func (p *CacheProxy) ready() error {
	if p == nil {
		return ErrNotInitialized
	}
	return nil
}

func (p *CacheProxy) getResource(ctx context.Context, key string, getter SingleGetter) (string, bool, error) {
	flightCtx := context.WithoutCancel(ctx)
	val, err, _ := p.getGroup.Do(key, func() (any, error) {
		data, needFastRequery, getErr := getter.Get(flightCtx, key)
		if getErr != nil {
			return nil, getErr
		}
		if needFastRequery {
			return data, fastRequeryErr
		}
		return data, nil
	})
	if err != nil && !errors.Is(err, fastRequeryErr) {
		return "", false, err
	}
	if ctx.Err() != nil {
		return "", false, ctx.Err()
	}
	res, ok := val.(string)
	if !ok {
		return "", false, fmt.Errorf("cacheproxy: unexpected singleflight type %T", val)
	}
	if errors.Is(err, fastRequeryErr) {
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
