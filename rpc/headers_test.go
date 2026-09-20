package rpc

// A6-3 回归测试：headersToString 的 Builder 加 Grow 预估后输出逐字不变。
// 多键场景按 key 排序做集合断言（map 遍历顺序随机），单键/空场景整串精确断言。

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/nats-io/nats.go/micro"
)

var headersPairRe = regexp.MustCompile(`([^{},\[\]]+):\[([^\]]*)\]`)

func canonicalHeadersString(s string) string {
	pairs := headersPairRe.FindAllStringSubmatch(s, -1)
	out := make([]string, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, p[1]+":["+p[2]+"]")
	}
	sort.Strings(out)
	return "{" + strings.Join(out, ",") + "}"
}

func TestHeadersToString_CanonicalOutput(t *testing.T) {
	if got := headersToString(micro.Headers{}); got != "{}" {
		t.Fatalf("empty = %q, want {}", got)
	}
	if got := headersToString(micro.Headers{"X-Solo": {"v1"}}); got != "{X-Solo:[v1]}" {
		t.Fatalf("single = %q", got)
	}
	multi := micro.Headers{
		"X-A": {"1"},
		"X-B": {"2", "3"},
		"X-C": {"4"},
	}
	if got := canonicalHeadersString(headersToString(multi)); got != "{X-A:[1],X-B:[2,3],X-C:[4]}" {
		t.Fatalf("multi canonical = %q", got)
	}
}
