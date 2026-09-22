package cacheproxy

import (
	"errors"
	"testing"

	"github.com/redis/go-redis/v9"
)

func TestInit_NilThenValid(t *testing.T) {
	resetInit()
	t.Cleanup(resetInit)

	if err := Init(nil); !errors.Is(err, ErrNilRedis) {
		t.Fatalf("Init(nil) err=%v, want ErrNilRedis", err)
	}
	if GetInstance() != nil {
		t.Fatal("nil Init must not publish a proxy")
	}

	rdb := &redis.Client{}
	if err := Init(rdb); err != nil {
		t.Fatalf("valid Init after nil: %v", err)
	}
	if GetInstance() == nil {
		t.Fatal("expected proxy after valid Init")
	}
}

func TestInit_SecondReturnsAlreadyInitialized(t *testing.T) {
	resetInit()
	t.Cleanup(resetInit)

	rdb1 := &redis.Client{}
	if err := Init(rdb1); err != nil {
		t.Fatal(err)
	}
	first := GetInstance()

	rdb2 := &redis.Client{}
	if err := Init(rdb2); !errors.Is(err, ErrAlreadyInitialized) {
		t.Fatalf("second Init err=%v, want ErrAlreadyInitialized", err)
	}
	if GetInstance() != first {
		t.Fatal("GetInstance must still be the first proxy")
	}
}
