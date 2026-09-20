package response

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TomWu-Alchemi/project-framework/metrics"
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// uniqueEndpoint 生成进程内唯一的 endpoint label。
// response_total 是包级 promauto 注册、进程生命周期内不可清零的计数器；
// 只有让 endpoint label 唯一，才能精确断言“本次调用是否/如何递增了该计数器”，
// 不受其它测试或历史样本干扰（与 metrics 包测试中的 uniquePath 同一手法）。
func uniqueEndpoint(prefix string) string {
	return fmt.Sprintf("%s%d_%d", prefix, time.Now().UnixNano(), uniqueEndpointSeq.Add(1))
}

// uniqueEndpointSeq 保证同一时间戳下的 endpoint label 仍然唯一；仅用 UnixNano 时，
// 本机墙钟粒度较粗会让相邻调用取到同值，导致 label 撞车、测试假失败。
var uniqueEndpointSeq atomic.Uint64

// responseCounterValue 在全局注册表（DefaultGatherer）中查找 response_total
// 下 endpoint 与 code 两个 label 同时匹配的计数器当前值；未找到返回 0。
// 说明：不显式 import dto 包（类型由 Gather 返回值推断），避免 client_model
// 从 indirect 提升为 direct、产生 go.mod 变化。
func responseCounterValue(t *testing.T, endpoint, code string) float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("Gather default metrics: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "response_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			var endpointMatched, codeMatched bool
			for _, lp := range m.GetLabel() {
				switch lp.GetName() {
				case "endpoint":
					endpointMatched = lp.GetValue() == endpoint
				case "code":
					codeMatched = lp.GetValue() == code
				}
			}
			if endpointMatched && codeMatched {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// withRouteContext 在真实 gin 路由（真实 *gin.Context）中执行 fn。
func withRouteContext(t *testing.T, pattern string, fn func(c *gin.Context)) {
	t.Helper()
	r := gin.New()
	r.GET(pattern, func(c *gin.Context) {
		fn(c)
		c.Status(http.StatusOK)
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, pattern, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200", w.Code)
	}
}

// mustMarshal 返回 v 的 JSON 文本（等价于下游把 CommonResponse 交给 serializer 的输出）。
func mustMarshal(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal(%#v): %v", v, err)
	}
	return string(raw)
}

// ---------------------------------------------------------------------------
// F-17：Success / Failed 写入 metrics.ResponseCodeMetricKey
// ---------------------------------------------------------------------------

func TestSuccess_SetsMetricKeyAndFields(t *testing.T) {
	payload := map[string]any{"name": "tom"}
	var (
		resp        CommonResponse
		metricVal   any
		metricExist bool
	)
	withRouteContext(t, "/f17-success", func(c *gin.Context) {
		resp = Success(c, payload, "ok", nil)
		metricVal, metricExist = c.Get(metrics.ResponseCodeMetricKey)
	})

	if !metricExist {
		t.Fatalf("Success 未写入 c.Set(%q, ...)", metrics.ResponseCodeMetricKey)
	}
	if code, ok := metricVal.(int); !ok || code != http.StatusOK {
		t.Fatalf("c.Get(%q)=%#v, want 200(int)", metrics.ResponseCodeMetricKey, metricVal)
	}
	if resp.ResponseStatus.Code != http.StatusOK {
		t.Fatalf("code=%d, want 200", resp.ResponseStatus.Code)
	}
	if resp.ResponseStatus.Msg != "ok" {
		t.Fatalf("msg=%q, want %q", resp.ResponseStatus.Msg, "ok")
	}
	if !reflect.DeepEqual(resp.Data, payload) {
		t.Fatalf("data=%#v, want %#v", resp.Data, payload)
	}
}

func TestFailed_SetsMetricKeyAndFields(t *testing.T) {
	const businessCode = 10001
	var (
		resp        CommonResponse
		metricVal   any
		metricExist bool
	)
	withRouteContext(t, "/f17-failed", func(c *gin.Context) {
		resp = Failed(c, businessCode, "boom", nil)
		metricVal, metricExist = c.Get(metrics.ResponseCodeMetricKey)
	})

	if !metricExist {
		t.Fatalf("Failed 未写入 c.Set(%q, ...)", metrics.ResponseCodeMetricKey)
	}
	if code, ok := metricVal.(int); !ok || code != businessCode {
		t.Fatalf("c.Get(%q)=%#v, want %d(int)", metrics.ResponseCodeMetricKey, metricVal, businessCode)
	}
	if resp.ResponseStatus.Code != businessCode || resp.ResponseStatus.Msg != "boom" {
		t.Fatalf("response_status=%+v", resp.ResponseStatus)
	}
	if resp.Data != nil {
		t.Fatalf("data=%#v, want nil", resp.Data)
	}
}

// ---------------------------------------------------------------------------
// F-17：Success2 / Failed2 递增 response_total
// ---------------------------------------------------------------------------

func TestSuccess2_IncrementsResponseCounter(t *testing.T) {
	endpoint := uniqueEndpoint("f17-success2-")
	resp := Success2(endpoint, "payload", "ok", nil)
	if resp.ResponseStatus.Code != http.StatusOK {
		t.Fatalf("code=%d, want 200", resp.ResponseStatus.Code)
	}
	if got := responseCounterValue(t, endpoint, "200"); got != 1 {
		t.Fatalf("response_total{endpoint=%q,code=\"200\"}=%v, want 1", endpoint, got)
	}
}

func TestFailed2_IncrementsResponseCounter(t *testing.T) {
	const businessCode = 50001
	endpoint := uniqueEndpoint("f17-failed2-")
	codeLabel := strconv.Itoa(businessCode)

	resp := Failed2(endpoint, businessCode, "boom", nil)
	if resp.ResponseStatus.Code != businessCode {
		t.Fatalf("code=%d, want %d", resp.ResponseStatus.Code, businessCode)
	}
	if got := responseCounterValue(t, endpoint, codeLabel); got != 1 {
		t.Fatalf("response_total{endpoint=%q,code=%q}=%v, want 1", endpoint, codeLabel, got)
	}

	// 第二次调用：计数器应累加。
	Failed2(endpoint, businessCode, "boom", nil)
	if got := responseCounterValue(t, endpoint, codeLabel); got != 2 {
		t.Fatalf("response_total{endpoint=%q,code=%q}=%v, want 2（应累加）", endpoint, codeLabel, got)
	}
}

// ---------------------------------------------------------------------------
// P3-12：extension nil → []
// ---------------------------------------------------------------------------

func TestExtension_NilBecomesEmptyArray(t *testing.T) {
	var (
		successResp CommonResponse
		failedResp  CommonResponse
	)
	withRouteContext(t, "/f17-extension-nil", func(c *gin.Context) {
		successResp = Success(c, nil, "ok", nil)
		failedResp = Failed(c, 10001, "boom", nil)
	})

	cases := []struct {
		name string
		resp CommonResponse
	}{
		{name: "Success", resp: successResp},
		{name: "Failed", resp: failedResp},
		{name: "Success2", resp: Success2(uniqueEndpoint("f17-ext-nil-2-"), nil, "ok", nil)},
		{name: "Failed2", resp: Failed2(uniqueEndpoint("f17-ext-nil-2-"), 10001, "boom", nil)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if ext := tc.resp.ResponseStatus.Extension; ext == nil {
				t.Fatalf("Extension 为 nil：JSON 会输出 null，未满足 P3-12")
			} else if len(ext) != 0 {
				t.Fatalf("Extension=%v, want empty", ext)
			}
			raw := mustMarshal(t, tc.resp)
			if !strings.Contains(raw, `"extension":[]`) {
				t.Fatalf("JSON=%s, want 包含 \"extension\":[]", raw)
			}
		})
	}
}

// P3-12：extension 非 nil 时原样透传（值与顺序均不变）。
func TestExtension_PassedThroughUnchanged(t *testing.T) {
	ext := []Pair{{Key: "trace_id", Value: "abc"}, {Key: "span", Value: "1"}}
	var resp CommonResponse
	withRouteContext(t, "/f17-extension-pass", func(c *gin.Context) {
		resp = Success(c, nil, "ok", ext)
	})

	if !reflect.DeepEqual(resp.ResponseStatus.Extension, ext) {
		t.Fatalf("Extension=%v, want %v", resp.ResponseStatus.Extension, ext)
	}
	raw := mustMarshal(t, resp)
	for _, want := range []string{`"key":"trace_id"`, `"value":"abc"`, `"key":"span"`, `"value":"1"`} {
		if !strings.Contains(raw, want) {
			t.Fatalf("JSON=%s, want 包含 %s", raw, want)
		}
	}
}
