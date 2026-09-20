package metrics

import (
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// Define Prometheus metrics
var (
	// Request counter
	httpRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Subsystem: "http",
			Name:      "http_requests_total",
			Help:      "Total number of HTTP requests",
		},
		[]string{"endpoint", "status"},
	)

	// Request latency histogram
	httpRequestDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Subsystem: "http",
			Name:      "http_request_duration_milliseconds",
			Help:      "HTTP request processing time (milliseconds)",
			Buckets:   []float64{5, 10, 25, 50, 100, 250, 500, 800, 1000, 2000, 5000},
		},
		[]string{"endpoint"},
	)

	// Request size histogram
	httpRequestSize = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Subsystem: "http",
			Name:      "http_request_size_bytes",
			Help:      "HTTP request size (bytes)",
			Buckets:   []float64{1024, 10 * 1024, 100 * 1024, 512 * 1024, 1024 * 1024, 5 * 1024 * 1024, 10 * 1024 * 1024},
		},
		[]string{"endpoint"},
	)

	// Response size histogram
	httpResponseSize = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Subsystem: "http",
			Name:      "http_response_size_bytes",
			Help:      "HTTP response size (bytes)",
			Buckets:   []float64{1024, 10 * 1024, 100 * 1024, 512 * 1024, 1024 * 1024, 5 * 1024 * 1024, 10 * 1024 * 1024},
		},
		[]string{"endpoint"},
	)

	// Current active requests
	httpRequestsInFlight = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Subsystem: "http",
			Name:      "http_requests_in_flight",
			Help:      "Number of HTTP requests currently being processed",
		},
		[]string{"endpoint"},
	)

	responseCounterTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Subsystem: "response",
			Name:      "total",
			Help:      "Total result of response",
		},
		[]string{"endpoint", "code"},
	)
)

const (
	ResponseCodeMetricKey = "metric_responseCode"
)

// PrometheusGinMiddleware returns a Gin middleware for collecting Prometheus metrics on HTTP requests
func PrometheusGinMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.FullPath()
		if path == "" {
			path = "unknown"
		}

		method := c.Request.Method
		contentLength := c.Request.ContentLength

		// 将方法和路径通过下划线连接
		endpoint := method + "_" + path

		if contentLength >= 0 {
			httpRequestSize.WithLabelValues(endpoint).Observe(float64(contentLength))
		}

		// 增加当前处理的请求数
		httpRequestsInFlight.WithLabelValues(endpoint).Inc()
		defer httpRequestsInFlight.WithLabelValues(endpoint).Dec()

		// 记录开始时间
		startTime := time.Now()

		// 处理请求
		c.Next()

		// 计算请求处理时间（毫秒，保留亚毫秒精度：由微秒换算为浮点毫秒；P3-14）
		elapsedTime := float64(time.Since(startTime).Microseconds()) / 1000.0

		// 获取响应状态码
		status := strconv.Itoa(c.Writer.Status())

		// 记录请求计数
		httpRequestsTotal.WithLabelValues(endpoint, status).Inc()

		// 记录请求处理时间
		httpRequestDuration.WithLabelValues(endpoint).Observe(elapsedTime)

		// 记录响应大小（gin 未写 body 时 Size() 返回 -1，此时不记录，避免污染直方图）
		if size := c.Writer.Size(); size >= 0 {
			httpResponseSize.WithLabelValues(endpoint).Observe(float64(size))
		}

		// 记录业务响应情况
		responseCode, exist := c.Get(ResponseCodeMetricKey)
		if exist {
			if code, ok := responseCode.(int); ok {
				responseCounterTotal.WithLabelValues(endpoint, strconv.Itoa(code)).Inc()
			}
		}
	}
}

// MetricWhitelist 返回一个仅放行白名单来源 IP 的 gin 中间件。
//
// 白名单条目支持两种写法，均在构造时一次性预解析：
//   - 精确 IP：IPv4（如 "10.0.0.1"，等价 /32）或 IPv6（如 "2001:db8::1"，等价 /128）；
//   - CIDR 前缀：如 "10.0.0.0/8"、"2001:db8::/32"。
//
// 无法解析的条目会被忽略；列表为空（或全部条目均非法）时一律拒绝并返回 404，
// 与旧实现“空列表全拒绝”的契约一致。
//
// 客户端 IP 取自 gin 的 ClientIP()，无法解析为合法 IP 时一律拒绝。
// 注意：ClientIP() 的可信度取决于 gin 的 TrustedProxies 配置——未正确配置时，
// 来源 IP 可被 X-Forwarded-For 等请求头伪造，白名单会被绕过。
func MetricWhitelist(ipList []string) gin.HandlerFunc {
	prefixes := parseWhitelist(ipList)
	return func(c *gin.Context) {
		if len(prefixes) == 0 || !ipWhitelisted(prefixes, c.ClientIP()) {
			// F-28：gin.H{} 序列化为 {}；此前传 nil 会输出字面 "null"，对抓取方不友好
			c.JSON(http.StatusNotFound, gin.H{})
			c.Abort()
			return
		}
		c.Next()
	}
}

// parseWhitelist 在构造期把白名单条目解析为 netip.Prefix；非法条目直接忽略。
func parseWhitelist(ipList []string) []netip.Prefix {
	prefixes := make([]netip.Prefix, 0, len(ipList))
	for _, item := range ipList {
		if strings.Contains(item, "/") {
			prefix, err := netip.ParsePrefix(item)
			if err != nil {
				continue
			}
			prefixes = append(prefixes, prefix)
			continue
		}

		addr, err := netip.ParseAddr(item)
		if err != nil {
			continue
		}
		// 精确 IP 等价于“全位前缀”：IPv4 → /32，IPv6 → /128。
		// 构造期先 Unmap：IPv4-mapped 写法（"::ffff:10.0.0.1"）按 IPv4 处理，
		// 与匹配侧保持一致（否则该写法永远不会命中）。
		addr = addr.Unmap()
		bits := 128
		if addr.Is4() {
			bits = 32
		}
		prefixes = append(prefixes, netip.PrefixFrom(addr, bits))
	}
	return prefixes
}

// ipWhitelisted 判断 ipStr 是否落在任一白名单前缀内；ipStr 无法解析时返回 false。
func ipWhitelisted(prefixes []netip.Prefix, ipStr string) bool {
	addr, err := netip.ParseAddr(ipStr)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	for _, prefix := range prefixes {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func ResponseCodeMetric(endpoint string, code int) {
	responseCounterTotal.WithLabelValues(endpoint, strconv.Itoa(code)).Inc()
}
