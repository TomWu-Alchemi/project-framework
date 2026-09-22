package logger

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestGinzap_MultipleErrorsSingleLog(t *testing.T) {
	zl, buf := ginZapBuffer(t)
	r := gin.New()
	r.Use(Ginzap(zl, "", false))
	r.GET("/x", func(c *gin.Context) {
		_ = c.Error(errors.New("first-err"))
		_ = c.Error(errors.New("second-err"))
		c.Status(http.StatusOK)
	})

	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/x", nil))
	out := buf.String()
	lines := 0
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if line != "" {
			lines++
		}
	}
	if lines != 1 {
		t.Fatalf("log lines=%d, want 1; out=%s", lines, out)
	}
	var entry struct {
		Msg    string   `json:"msg"`
		Errors []string `json:"errors"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &entry); err != nil {
		t.Fatal(err)
	}
	if entry.Msg != "first-err" {
		t.Fatalf("msg=%q", entry.Msg)
	}
	if len(entry.Errors) != 2 {
		t.Fatalf("errors=%v", entry.Errors)
	}
}
