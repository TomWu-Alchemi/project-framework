package cacheproxy

import (
	"ai-kaka.com/project-framework/logger"
	"context"
	"errors"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
	"sync"
	"time"
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
	defaultExpiredTime = 24 * time.Hour
	defaultRefreshTime = 10 * time.Minute
)

var (
	once         sync.Once
	defaultProxy *CacheProxy

	fastRequeryErr = errors.New("need fast requery")
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
	defaultProxy = newCacheProxy(rdb)
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
	if p == nil {
		panic("empty cacheProxy")
	}
	if len(key) == 0 {
		return "", false, nil
	}
	// 强制刷新，不查询缓存，只回源并对缓存赋值
	if c.NeedForceRefresh {
		data, needFastRequery, err := p.getResource(ctx, key, getter)
		if err != nil {
			return "", false, err
		}
		err = p.setData(context.Background(), c, key, data, needFastRequery)
		if err != nil {
			return "", false, err
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
			setErr := p.setData(context.Background(), c, key, data, needFastRequery)
			if setErr != nil {
				logger.Error("cacheProxy setErr:" + setErr.Error())
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
			newCtx := context.Background()
			data, needFastRequery, err2 := p.getResource(newCtx, key, getter)
			if err2 != nil {
				logger.Error("cacheProxy refresh getResource err:" + err2.Error())
			}
			err2 = p.setData(newCtx, c, key, data, needFastRequery)
			if err2 != nil {
				logger.Error("cacheProxy refresh setData err:" + err2.Error())
			}
		}()
	}

	return sv.String(), true, nil
}

func (p *CacheProxy) Set(ctx context.Context, c CacheContext, key string, value string) error {
	if p == nil {
		panic("empty cacheProxy")
	}
	return p.setData(ctx, c, key, value, false)
}

func (p *CacheProxy) Remove(ctx context.Context, c CacheContext, key string) error {
	if p == nil {
		panic("empty cacheProxy")
	}
	return p.cache.Remove(ctx, key)
}

func (p *CacheProxy) getResource(ctx context.Context, key string, getter SingleGetter) (string, bool, error) {
	val, err, _ := p.getGroup.Do(key, func() (interface{}, error) {
		var getErr error
		data, needFastRequery, getErr := getter.Get(ctx, key)
		if getErr != nil {
			return nil, getErr
		}
		if needFastRequery {
			return data, fastRequeryErr
		}
		return data, nil
	})
	res := val.(string)
	if err != nil {
		if errors.Is(err, fastRequeryErr) {
			// 需要快速回源
			return res, true, nil
		} else {
			return "", false, err
		}
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
	return p.cache.Set(ctx, key, sv, c.ExpiredTime, c.EmptyExpiredTime)
}
