package cacheproxy

import (
	"testing"
	"time"
)

func TestIsExpire_ZeroCtime(t *testing.T) {
	if !((StringView{}).IsExpire(time.Hour, 0)) {
		t.Fatal("zero ctime should be treated as expired")
	}
}

func TestIsExpire_ZeroOffsetUsesDefault(t *testing.T) {
	fresh := StringView{Ctime: time.Now()}
	if fresh.IsExpire(0, 0) || fresh.IsExpire(-1, -time.Second) {
		t.Fatal("fresh value should not expire when offset defaults to 10m")
	}
	stale := StringView{Ctime: time.Now().Add(-defaultRefreshTime - time.Second)}
	if !stale.IsExpire(0, 0) {
		t.Fatal("value older than default refresh should expire")
	}
}

func TestIsExpire_FastOffset(t *testing.T) {
	v := StringView{Ctime: time.Now().Add(-2 * time.Second), NeedFastRequery: true}
	if v.IsExpire(time.Hour, 3*time.Second) {
		t.Fatal("should use fast offset and still be fresh")
	}
	if !v.IsExpire(time.Hour, time.Second) {
		t.Fatal("should expire under short fast offset")
	}
}
