package rpc

import (
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
)

// applyNatsOptions mirrors what nats.Connect does internally: start from the
// library defaults and then apply the given options in order.
func applyNatsOptions(t *testing.T, target *nats.Options, options []nats.Option) {
	t.Helper()
	for _, apply := range options {
		if err := apply(target); err != nil {
			t.Fatalf("apply option: %v", err)
		}
	}
}

func TestConnectOptions_DrainTimeout(t *testing.T) {
	tests := []struct {
		name     string
		config   ServiceConfig
		expected time.Duration
	}{
		{
			name:     "unset keeps nats default",
			config:   ServiceConfig{},
			expected: nats.DefaultDrainTimeout,
		},
		{
			name:     "negative keeps nats default",
			config:   ServiceConfig{DrainTimeout: -time.Second},
			expected: nats.DefaultDrainTimeout,
		},
		{
			name:     "explicit value is applied",
			config:   ServiceConfig{DrainTimeout: 5 * time.Second},
			expected: 5 * time.Second,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			options := nats.GetDefaultOptions()
			applyNatsOptions(t, &options, connectOptions(tt.config))
			if options.DrainTimeout != tt.expected {
				t.Fatalf("DrainTimeout=%v, want %v", options.DrainTimeout, tt.expected)
			}
			if options.DisconnectedErrCB == nil {
				t.Fatal("DisconnectedErrCB must not be nil")
			}
		})
	}
}

func TestConnectOptions_UserInfo(t *testing.T) {
	options := nats.GetDefaultOptions()
	applyNatsOptions(t, &options, connectOptions(ServiceConfig{
		Username: "user",
		Password: "pass",
	}))
	if options.User != "user" || options.Password != "pass" {
		t.Fatalf("User=%q Password=%q", options.User, options.Password)
	}
}

// F-32：空账号不追加 UserInfo（保持 nats 默认 / URL 内嵌凭据语义）。
func TestConnectOptions_EmptyCredentialsSkipUserInfo(t *testing.T) {
	options := nats.GetDefaultOptions()
	applyNatsOptions(t, &options, connectOptions(ServiceConfig{}))
	if options.User != "" || options.Password != "" {
		t.Fatalf("User=%q Password=%q, want empty (option must be skipped)", options.User, options.Password)
	}
}

// F-32：空 Url 在框架层提前报错，不进入 nats 连接逻辑。
func TestNewNatsService_EmptyUrlRejected(t *testing.T) {
	for _, url := range []string{"", "   "} {
		svc, cleanup, err := NewNatsService(ServiceConfig{Url: url, AppName: "a", Version: "0.0.1"})
		if err == nil {
			t.Fatalf("url=%q must be rejected", url)
		}
		if !strings.Contains(err.Error(), "nats url is empty") {
			t.Fatalf("url=%q unexpected error: %v", url, err)
		}
		if svc != nil {
			t.Fatalf("url=%q svc=%v, want nil", url, svc)
		}
		if cleanup == nil {
			t.Fatalf("url=%q cleanup must not be nil", url)
		}
		cleanup() // 空 cleanup 可安全调用
	}
}

func TestDrainWaitBudget(t *testing.T) {
	// Contract from the F-2 review item: budget = effective drain timeout + 6s
	// (5s hardcoded FlushTimeout inside nats drainConnection + 1s slack).
	// The grace is pinned as a literal so that changing the constant in
	// service.go requires a deliberate test update.
	const grace = 6 * time.Second

	tests := []struct {
		name       string
		configured time.Duration
		expected   time.Duration
	}{
		{
			name:       "zero falls back to nats default",
			configured: 0,
			expected:   nats.DefaultDrainTimeout + grace,
		},
		{
			name:       "negative falls back to nats default",
			configured: -time.Second,
			expected:   nats.DefaultDrainTimeout + grace,
		},
		{
			name:       "positive value adds grace",
			configured: 5 * time.Second,
			expected:   5*time.Second + grace,
		},
		{
			name:       "nats default adds grace",
			configured: nats.DefaultDrainTimeout,
			expected:   nats.DefaultDrainTimeout + grace,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := drainWaitBudget(tt.configured); got != tt.expected {
				t.Fatalf("drainWaitBudget(%v)=%v, want %v", tt.configured, got, tt.expected)
			}
		})
	}
}

// Note: the success-path cleanup (sync.Once around srv.Stop + nc.Drain +
// closed-channel wait) cannot be exercised here: nats.Conn is a concrete type
// with no interface to stub, and nats.Connect requires a reachable server.
// Per the F-2 test strategy it is covered by code review plus a manual e2e run
// (local nats-server, single start/stop, verifying "rpc service shutdown end."
// is logged only after the connection has actually closed). What is covered
// below is the only cleanup reachable without a server: the no-op cleanup
// returned by the connect-failure path, called twice.
func TestNewNatsService_ConnectFail_CleanupTwice(t *testing.T) {
	svc, cleanup, err := NewNatsService(ServiceConfig{
		Url:     "nats://127.0.0.1:1",
		AppName: "test-app",
		Version: "0.0.1",
	})
	if cleanup == nil {
		t.Fatal("cleanup must not be nil on connect failure")
	}
	if err == nil {
		t.Fatal("expected connect error")
	}
	if svc != nil {
		t.Fatal("expected nil service on connect error")
	}

	// Idempotency: a second call must not panic (the failure path returns an
	// empty cleanup, so this does not exercise the success-path sync.Once).
	cleanup()
	cleanup()
}
