package httpclient

// A6-1 回归测试：header 日志字段由 fmt.Sprintf 改为 strings.Builder 直写后，
// 输出必须逐字一致。
//
// header 是 map，遍历顺序随机，因此：
//   - 单键场景整串精确断言；
//   - 多键场景把 header 串规范化为「(k:v), token 集合排序拼接」后断言，
//     禁止整串相等比较（顺序不稳定会 flaky）。

import (
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// canonicalHeaderTokens 把 "(k1:v1),(k2:v2)," 解析为排序后的 token 拼接；
// 单键时输出与输入逐字一致。
func canonicalHeaderTokens(s string) string {
	toks := make([]string, 0, 4)
	rest := s
	for len(rest) > 0 {
		i := strings.Index(rest, "),")
		if i < 0 {
			toks = append(toks, rest)
			break
		}
		toks = append(toks, rest[:i+2])
		rest = rest[i+2:]
	}
	sort.Strings(toks)
	return strings.Join(toks, "")
}

func captureDalHeaderField(t *testing.T, call func(c *DalHttpClient) error) string {
	t.Helper()
	core, observed := observer.New(zap.InfoLevel)
	c := NewDalHttpClient(DalHttpClientConf{Timeout: 5 * time.Second, DalLog: zap.New(core)})
	if err := call(c); err != nil {
		t.Fatal(err)
	}
	logs := observed.All()
	if len(logs) != 1 {
		t.Fatalf("log entries = %d, want 1", len(logs))
	}
	hf, _ := logs[0].ContextMap()["header"].(string)
	return hf
}

func TestDalHeaderLog_CanonicalOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	single := map[string]string{"X-Solo": "v1"}
	multi := map[string]string{"X-A": "1", "X-B": "2", "X-C": "3"}

	cases := []struct {
		name string
		hdrs map[string]string
		do   func(c *DalHttpClient, hdrs map[string]string) error
		want string
	}{
		{
			name: "PostJson 单键逐字",
			hdrs: single,
			do: func(c *DalHttpClient, hdrs map[string]string) error {
				return c.PostJson(t.Context(), srv.URL, hdrs, map[string]int{"n": 1}, nil)
			},
			want: "(X-Solo:v1),",
		},
		{
			name: "PostJson 多键集合",
			hdrs: multi,
			do: func(c *DalHttpClient, hdrs map[string]string) error {
				return c.PostJson(t.Context(), srv.URL, hdrs, map[string]int{"n": 1}, nil)
			},
			want: "(X-A:1),(X-B:2),(X-C:3),",
		},
		{
			name: "GetWithRetry 单键逐字",
			hdrs: single,
			do: func(c *DalHttpClient, hdrs map[string]string) error {
				_, err := c.GetWithRetry(t.Context(), srv.URL, nil, hdrs, 1)
				return err
			},
			want: "(X-Solo:v1),",
		},
		{
			name: "GetWithRetry 多键集合",
			hdrs: multi,
			do: func(c *DalHttpClient, hdrs map[string]string) error {
				_, err := c.GetWithRetry(t.Context(), srv.URL, nil, hdrs, 1)
				return err
			},
			want: "(X-A:1),(X-B:2),(X-C:3),",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			call := func(c *DalHttpClient) error { return tc.do(c, tc.hdrs) }
			got := canonicalHeaderTokens(captureDalHeaderField(t, call))
			if got != tc.want {
				t.Fatalf("header canonical = %q, want %q", got, tc.want)
			}
		})
	}
}
