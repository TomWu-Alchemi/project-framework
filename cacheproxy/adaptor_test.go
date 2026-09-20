package cacheproxy

import (
	"errors"
	"testing"
	"time"

	"github.com/bytedance/sonic"
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

func TestResolveExpiration(t *testing.T) {
	tests := []struct {
		name             string
		dataLen          int
		expiredTime      time.Duration
		emptyExpiredTime time.Duration
		want             time.Duration
	}{
		{
			name:             "非空数据-正常值原样返回",
			dataLen:          5,
			expiredTime:      2 * time.Hour,
			emptyExpiredTime: 30 * time.Second,
			want:             2 * time.Hour,
		},
		{
			name:             "非空数据-expiredTime 为 0 回退 defaultExpiredTime",
			dataLen:          5,
			expiredTime:      0,
			emptyExpiredTime: 30 * time.Second,
			want:             defaultExpiredTime,
		},
		{
			name:             "非空数据-expiredTime 为负数回退 defaultExpiredTime",
			dataLen:          5,
			expiredTime:      -time.Second,
			emptyExpiredTime: 30 * time.Second,
			want:             defaultExpiredTime,
		},
		{
			name:             "空数据-使用 emptyExpiredTime",
			dataLen:          0,
			expiredTime:      2 * time.Hour,
			emptyExpiredTime: 30 * time.Second,
			want:             30 * time.Second,
		},
		{
			name:             "空数据-emptyExpiredTime 为 0 回退 defaultEmptyExpiredTime",
			dataLen:          0,
			expiredTime:      2 * time.Hour,
			emptyExpiredTime: 0,
			want:             defaultEmptyExpiredTime,
		},
		{
			name:             "空数据-emptyExpiredTime 为负数回退 defaultEmptyExpiredTime",
			dataLen:          0,
			expiredTime:      2 * time.Hour,
			emptyExpiredTime: -time.Second,
			want:             defaultEmptyExpiredTime,
		},
		{
			name:             "空数据-两个入参均为 0 回退 defaultEmptyExpiredTime",
			dataLen:          0,
			expiredTime:      0,
			emptyExpiredTime: 0,
			want:             defaultEmptyExpiredTime,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveExpiration(tt.dataLen, tt.expiredTime, tt.emptyExpiredTime)
			if got != tt.want {
				t.Fatalf("resolveExpiration(%d, %v, %v) = %v, want %v",
					tt.dataLen, tt.expiredTime, tt.emptyExpiredTime, got, tt.want)
			}
		})
	}
}

func TestDecodeStringView(t *testing.T) {
	fixedTime := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC)
	// 用 sonic 自身生成一份合法 JSON，避免测试耦合序列化细节（尤其是 Ctime 的编码格式）
	roundTripRaw, err := sonic.MarshalString(StringView{
		Ctime:           fixedTime,
		NeedFastRequery: true,
		IsNil:           false,
		Data:            "hello",
	})
	if err != nil {
		t.Fatalf("构造往返用例失败: %v", err)
	}

	tests := []struct {
		name       string
		raw        string
		wantOK     bool
		wantData   string
		wantFast   bool
		checkCtime bool
		wantCtime  time.Time
	}{
		{
			name:   "空串视为不可用",
			raw:    "",
			wantOK: false,
		},
		{
			name:   "非 JSON 视为不可用",
			raw:    "not-a-json",
			wantOK: false,
		},
		{
			name:       "合法 JSON（字面量）解析成功",
			raw:        `{"ctime":"2026-09-16T10:00:00Z","need_fast_requery":true,"is_nil":false,"data":"hello"}`,
			wantOK:     true,
			wantData:   "hello",
			wantFast:   true,
			checkCtime: true,
			wantCtime:  fixedTime,
		},
		{
			name:       "合法 JSON（sonic 序列化往返）解析成功",
			raw:        roundTripRaw,
			wantOK:     true,
			wantData:   "hello",
			wantFast:   true,
			checkCtime: true,
			wantCtime:  fixedTime,
		},
		{
			name:       "JSON 合法但字段缺失：ok=true 且字段为零值",
			raw:        `{}`,
			wantOK:     true,
			wantData:   "",
			wantFast:   false,
			checkCtime: true,
			wantCtime:  time.Time{},
		},
		{
			// 附加用例：字段类型与 StringView 不匹配，应视为不可用。
			// 若实测 sonic 对该输入不报错（宽松模式），删除本用例即可，不影响其余用例与实现语义。
			name:   "JSON 合法但字段类型不匹配：不可用",
			raw:    `{"data":123}`,
			wantOK: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sv, ok := decodeStringView(tt.raw)
			if ok != tt.wantOK {
				t.Fatalf("decodeStringView(%q) ok=%v, want %v", tt.raw, ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			if sv.Data != tt.wantData {
				t.Fatalf("Data=%q want %q", sv.Data, tt.wantData)
			}
			if sv.NeedFastRequery != tt.wantFast {
				t.Fatalf("NeedFastRequery=%v want %v", sv.NeedFastRequery, tt.wantFast)
			}
			if tt.checkCtime && !sv.Ctime.Equal(tt.wantCtime) {
				t.Fatalf("Ctime=%v want %v", sv.Ctime, tt.wantCtime)
			}
		})
	}
}
