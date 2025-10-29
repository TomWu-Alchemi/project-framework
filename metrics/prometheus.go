package metrics

import (
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"net/http"
	"slices"
	"strconv"
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

		// 记录请求大小
		httpRequestSize.WithLabelValues(endpoint).Observe(float64(contentLength))

		// 增加当前处理的请求数
		httpRequestsInFlight.WithLabelValues(endpoint).Inc()
		defer httpRequestsInFlight.WithLabelValues(endpoint).Dec()

		// 记录开始时间
		startTime := time.Now()

		// 处理请求
		c.Next()

		// 计算请求处理时间（毫秒）
		elapsedTime := float64(time.Since(startTime).Milliseconds())

		// 获取响应状态码
		status := strconv.Itoa(c.Writer.Status())

		// 记录请求计数
		httpRequestsTotal.WithLabelValues(endpoint, status).Inc()

		// 记录请求处理时间
		httpRequestDuration.WithLabelValues(endpoint).Observe(elapsedTime)

		// 记录响应大小
		httpResponseSize.WithLabelValues(endpoint).Observe(float64(c.Writer.Size()))

		// 记录业务响应情况
		responseCode, exist := c.Get(ResponseCodeMetricKey)
		if exist {
			code := responseCode.(int)
			responseCounterTotal.WithLabelValues(endpoint, strconv.Itoa(code)).Inc()
		}
	}
}

func MetricWhitelist(ipList []string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if len(ipList) == 0 {
			c.Next()
		} else {
			if !slices.Contains(ipList, c.ClientIP()) {
				c.JSON(http.StatusNotFound, nil)
				c.Abort()
				return
			}
			c.Next()
		}
	}
}

func ResponseCodeMetric(endpoint string, code int) {
	responseCounterTotal.WithLabelValues(endpoint, strconv.Itoa(code)).Inc()
}
