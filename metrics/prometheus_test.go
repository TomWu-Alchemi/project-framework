package metrics

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
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
