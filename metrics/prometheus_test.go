package metrics

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func TestMetricWhitelist_EmptyListDenies(t *testing.T) {
	for _, ipList := range [][]string{nil, {}} {
		r := gin.New()
		r.GET("/metrics", MetricWhitelist(ipList), func(c *gin.Context) {
			t.Error("handler should not run")
			c.Status(http.StatusOK)
		})
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		if w.Code != http.StatusNotFound {
			t.Fatalf("ipList=%#v status=%d", ipList, w.Code)
		}
		// F-28：拒绝响应体为 {}，不再是字面 "null"
		if got := w.Body.String(); got != "{}" {
			t.Fatalf("ipList=%#v body=%q, want {}", ipList, got)
		}
	}
}

func TestPrometheusGinMiddleware_NegativeContentLength(t *testing.T) {
	r := gin.New()
	r.Use(PrometheusGinMiddleware())
	r.POST("/x", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	req.ContentLength = -1
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestPrometheusGinMiddleware_NonIntResponseCodeSkipped(t *testing.T) {
	r := gin.New()
	r.Use(PrometheusGinMiddleware())
	r.GET("/x", func(c *gin.Context) {
		c.Set(ResponseCodeMetricKey, "not-int")
		c.Status(http.StatusOK)
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
}

// uniquePath 生成进程内唯一的测试路由路径。
// 指标为包级 promauto 注册、进程生命周期内不可清零；只有让 endpoint label 唯一，
// 才能对"本次请求是否产生样本"做精确断言而不受其它测试/历史样本干扰。
func uniquePath(prefix string) string {
	return fmt.Sprintf("%s%d_%d", prefix, time.Now().UnixNano(), uniqueSeq.Add(1))
}

// uniqueSeq 保证同一时间戳下的 endpoint label 仍然唯一；仅用 UnixNano 时，
// 本机墙钟粒度较粗会让相邻调用取到同值，导致 label 撞车、测试假失败。
var uniqueSeq atomic.Uint64

// histogramSamples 在全局注册表（DefaultGatherer）中查找指标族 name 下
// endpoint label 匹配的直方图，返回样本数 count 与样本值之和 sum；未找到时返回 (0, 0)。
// 说明：不显式 import dto 包（类型由 Gather 返回值推断），避免 client_model 从 indirect 提升为 direct。
func histogramSamples(t *testing.T, name, endpoint string) (count uint64, sum float64) {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("Gather default metrics: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "endpoint" && lp.GetValue() == endpoint {
					h := m.GetHistogram()
					return h.GetSampleCount(), h.GetSampleSum()
				}
			}
		}
	}
	return 0, 0
}

// counterValue 在全局注册表中查找指标族 name 下 endpoint label 匹配的 Counter 值；未找到返回 0。
func counterValue(t *testing.T, name, endpoint string) float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("Gather default metrics: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "endpoint" && lp.GetValue() == endpoint {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

// F-3：204（未写 body，Size() == -1）不应产生响应尺寸样本。
func TestPrometheusGinMiddleware_NoResponseSizeSampleForNoBody(t *testing.T) {
	path := uniquePath("/f3-no-body-")
	r := gin.New()
	r.Use(PrometheusGinMiddleware())
	r.GET(path, func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	if w.Code != http.StatusNoContent {
		t.Fatalf("status=%d", w.Code)
	}

	endpoint := http.MethodGet + "_" + path

	// 先确认请求确实被 middleware 观测到（排除"路由未命中导致无样本"的假阴性）
	if got := counterValue(t, "http_http_requests_total", endpoint); got != 1 {
		t.Fatalf("http_http_requests_total{%s}=%v, want 1", endpoint, got)
	}
	if count, sum := histogramSamples(t, "http_http_response_size_bytes", endpoint); count != 0 || sum != 0 {
		t.Fatalf("response size samples count=%d sum=%v, want none (Size() == -1)", count, sum)
	}
}

// F-3：正常写 body 的请求应产生恰好一个样本，且样本和等于 body 长度。
func TestPrometheusGinMiddleware_ResponseSizeSampleEqualsBodyLength(t *testing.T) {
	path := uniquePath("/f3-body-")
	body := "f3-response-body"
	r := gin.New()
	r.Use(PrometheusGinMiddleware())
	r.GET(path, func(c *gin.Context) {
		_, _ = c.Writer.WriteString(body)
	})

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	if got := w.Body.String(); got != body {
		t.Fatalf("body=%q, want %q", got, body)
	}

	endpoint := http.MethodGet + "_" + path
	count, sum := histogramSamples(t, "http_http_response_size_bytes", endpoint)
	if count != 1 {
		t.Fatalf("response size sample count=%d, want 1", count)
	}
	if sum != float64(len(body)) {
		t.Fatalf("response size sample sum=%v, want %d", sum, len(body))
	}
}

// ---------------------------------------------------------------------------
// F-16：MetricWhitelist 支持 CIDR
// ---------------------------------------------------------------------------

// whitelistStatus 在真实 gin 路由上发起一次请求，返回 MetricWhitelist 放行/拒绝后的状态码。
// 显式 SetTrustedProxies(nil)：关闭代理信任，让 ClientIP() 直接取 RemoteAddr，
// 用例不依赖 gin 默认的代理策略。
func whitelistStatus(t *testing.T, ipList []string, remoteAddr string) int {
	t.Helper()
	r := gin.New()
	if err := r.SetTrustedProxies(nil); err != nil {
		t.Fatalf("SetTrustedProxies(nil): %v", err)
	}
	r.GET("/f16-metrics", MetricWhitelist(ipList), func(c *gin.Context) {
		c.Status(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/f16-metrics", nil)
	req.RemoteAddr = remoteAddr
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code
}

// F-16：CIDR / 精确 IP / IPv6 / 全放行前缀 的匹配矩阵。
// （空列表全拒绝已由既有 TestMetricWhitelist_EmptyListDenies 覆盖，不重复造用例。）
func TestMetricWhitelist_Matching(t *testing.T) {
	tests := []struct {
		name       string
		ipList     []string
		remoteAddr string
		want       int
	}{
		{name: "CIDR 命中", ipList: []string{"10.0.0.0/8"}, remoteAddr: "10.1.2.3:5000", want: http.StatusOK},
		{name: "CIDR 未命中", ipList: []string{"10.0.0.0/8"}, remoteAddr: "192.168.1.1:5000", want: http.StatusNotFound},
		{name: "精确 IPv4 命中", ipList: []string{"192.168.1.100"}, remoteAddr: "192.168.1.100:5000", want: http.StatusOK},
		{name: "精确 IPv4 未命中", ipList: []string{"192.168.1.100"}, remoteAddr: "192.168.1.101:5000", want: http.StatusNotFound},
		{name: "精确 IPv6 命中", ipList: []string{"2001:db8::1"}, remoteAddr: "[2001:db8::1]:5000", want: http.StatusOK},
		{name: "IPv6 CIDR 命中", ipList: []string{"2001:db8::/32"}, remoteAddr: "[2001:db8:1::5]:5000", want: http.StatusOK},
		{name: "IPv6 CIDR 未命中", ipList: []string{"2001:db9::/32"}, remoteAddr: "[2001:db8:1::5]:5000", want: http.StatusNotFound},
		{name: "IPv4 前缀不放行 IPv6 客户端", ipList: []string{"10.0.0.0/8"}, remoteAddr: "[2001:db8::1]:5000", want: http.StatusNotFound},
		{name: "精确 IP 与 CIDR 混合命中", ipList: []string{"192.168.1.100", "10.0.0.0/8"}, remoteAddr: "10.200.0.9:5000", want: http.StatusOK},
		{name: "0.0.0.0/0 放行任意 IPv4 客户端", ipList: []string{"0.0.0.0/0"}, remoteAddr: "203.0.113.7:5000", want: http.StatusOK},
		{name: "::/0 放行任意 IPv6 客户端", ipList: []string{"::/0"}, remoteAddr: "[2001:db8::1]:5000", want: http.StatusOK},
		{name: "0.0.0.0/0 不覆盖 IPv6 客户端（netip 字面语义）", ipList: []string{"0.0.0.0/0"}, remoteAddr: "[2001:db8::1]:5000", want: http.StatusNotFound},
		{name: "::/0 不覆盖 IPv4 客户端（netip 字面语义）", ipList: []string{"::/0"}, remoteAddr: "203.0.113.7:5000", want: http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := whitelistStatus(t, tt.ipList, tt.remoteAddr); got != tt.want {
				t.Fatalf("MetricWhitelist(%v) RemoteAddr=%q → status=%d, want %d", tt.ipList, tt.remoteAddr, got, tt.want)
			}
		})
	}
}

// F-16：非法条目被忽略；若列表非空但全部非法，等价空白名单（全拒绝）。
func TestMetricWhitelist_InvalidEntriesIgnored(t *testing.T) {
	// 含非法条目时，合法条目仍然生效。
	if got := whitelistStatus(t, []string{"not-an-ip", "10.0.0.0/33", "2001:db8::gg", "", "10.0.0.0/8"}, "10.1.2.3:5000"); got != http.StatusOK {
		t.Fatalf("status=%d, want 200（非法条目应被忽略、合法 CIDR 仍生效）", got)
	}
	// 全部条目非法：与空列表同语义，全拒绝。
	if got := whitelistStatus(t, []string{"not-an-ip", "10.0.0.0/33", ""}, "10.1.2.3:5000"); got != http.StatusNotFound {
		t.Fatalf("status=%d, want 404（全部条目非法应全拒绝）", got)
	}
}

// F-16：客户端 IP 无法解析时一律拒绝（即便白名单是全放行前缀）。
// RemoteAddr 选用无论 gin 回退为原串还是空串都必然解析失败的取值。
func TestMetricWhitelist_UnparseableClientIPDenied(t *testing.T) {
	if got := whitelistStatus(t, []string{"0.0.0.0/0", "::/0"}, "not-an-ip"); got != http.StatusNotFound {
		t.Fatalf("status=%d, want 404（无法解析的 ClientIP 必须拒绝）", got)
	}
}

// P3-14：duration 由微秒换算浮点毫秒，亚毫秒请求也必须产生非零样本和
// （旧实现 Milliseconds() 整数截断会把亚毫秒请求记成 0）。
func TestPrometheusGinMiddleware_DurationKeepsSubMillisecondPrecision(t *testing.T) {
	path := uniquePath("/p3-14-duration-")
	r := gin.New()
	r.Use(PrometheusGinMiddleware())
	r.GET(path, func(c *gin.Context) {
		// 注入可测耗时：本机时钟粒度较粗，零工作量 handler 的 delta 会恰为 0。
		// 固定 500µs，连同该 handler 的固定开销后总耗时仍须 < 1ms——这是本断言保持
		// 区分能力的前提（否则旧实现的毫秒整数截断也会记 ≥1，sum > 0 将失去意义）。
		time.Sleep(500 * time.Microsecond)
		c.Status(http.StatusOK)
	})

	wallStart := time.Now()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	wallMs := float64(time.Since(wallStart).Microseconds()) / 1000.0
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}

	endpoint := http.MethodGet + "_" + path
	count, sum := histogramSamples(t, "http_http_request_duration_milliseconds", endpoint)
	if count != 1 {
		t.Fatalf("duration sample count=%d, want 1", count)
	}
	if sum <= 0 {
		t.Fatalf("duration sum=%v, want > 0（毫秒整数截断会得到 0）", sum)
	}
	// 内部计时区间必被测试侧观测区间包含，用于发现时间口径异常（非精度断言）。
	if sum > wallMs {
		t.Fatalf("duration sum=%v 超过测试侧观测总耗时 %v，时间口径异常", sum, wallMs)
	}
}
