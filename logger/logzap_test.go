package logger

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func ginZapBuffer(t *testing.T) (*zap.Logger, *bytes.Buffer) {
	t.Helper()
	buf := &bytes.Buffer{}
	encCfg := zap.NewProductionEncoderConfig()
	encCfg.TimeKey = ""
	core := zapcore.NewCore(zapcore.NewJSONEncoder(encCfg), zapcore.AddSync(buf), zapcore.DebugLevel)
	return zap.New(core), buf
}

func TestGinzap_JSONCharsetNestedPassword(t *testing.T) {
	zl, buf := ginZapBuffer(t)
	r := gin.New()
	r.Use(Ginzap(zl, "", false))
	const body = `{"password":"top-secret","token":"tok-keep","secret":"sec-keep","user":{"password":"nest-secret","token":"tok2"},"arr":[{"password":"arr-secret"}]}`
	r.POST("/x", func(c *gin.Context) {
		got, _ := io.ReadAll(c.Request.Body)
		if !bytes.Contains(got, []byte("top-secret")) || !bytes.Contains(got, []byte("nest-secret")) || !bytes.Contains(got, []byte("arr-secret")) {
			t.Error("handler must see full unmasked body")
		}
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "/x?password=q-secret&token=q-tok", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	out := buf.String()
	for _, secret := range []string{"top-secret", "nest-secret", "arr-secret", "q-secret"} {
		if strings.Contains(out, secret) {
			t.Fatalf("password leaked %q in log: %s", secret, out)
		}
	}
	for _, keep := range []string{"tok-keep", "sec-keep", "tok2", "q-tok"} {
		if !strings.Contains(out, keep) {
			t.Fatalf("token/secret missing from log (%s): %s", keep, out)
		}
	}
}

func TestGinzap_FormPasswordCaseInsensitive(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "lower", body: "password=Secret1&foo=bar", want: "Secret1"},
		{name: "upper", body: "Password=Secret2&foo=bar", want: "Secret2"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			zl, buf := ginZapBuffer(t)
			r := gin.New()
			r.Use(Ginzap(zl, "", false))
			r.PUT("/x", func(c *gin.Context) {
				got, _ := io.ReadAll(c.Request.Body)
				if !bytes.Contains(got, []byte(tc.want)) {
					t.Error("handler must see full unmasked form body")
				}
				c.Status(http.StatusOK)
			})
			req := httptest.NewRequest(http.MethodPut, "/x", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			out := buf.String()
			if strings.Contains(out, tc.want) {
				t.Fatalf("form password leaked: %s", out)
			}
			if !strings.Contains(out, "foo=bar") {
				t.Fatalf("non-password field missing: %s", out)
			}
		})
	}
}

func TestGinzap_QueryPasswordMaskedTokenPlain(t *testing.T) {
	zl, buf := ginZapBuffer(t)
	r := gin.New()
	r.Use(Ginzap(zl, "", false))
	r.GET("/q", func(c *gin.Context) {
		if c.Query("token") != "q-tok" || c.Query("password") != "q-secret" {
			t.Error("handler must see original query")
		}
		c.Status(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/q?password=q-secret&token=q-tok", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	out := buf.String()
	if strings.Contains(out, "q-secret") {
		t.Fatalf("query password leaked: %s", out)
	}
	if !strings.Contains(out, "q-tok") {
		t.Fatalf("query token should remain plaintext: %s", out)
	}
}

func TestCustomRecoveryWithZap_PanicString(t *testing.T) {
	r := gin.New()
	r.Use(CustomRecoveryWithZap(zap.NewNop(), false, defaultHandleRecovery))
	r.GET("/p", func(c *gin.Context) { panic("boom") })
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/p", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestCustomRecoveryWithZap_BrokenPipe(t *testing.T) {
	r := gin.New()
	r.Use(CustomRecoveryWithZap(zap.NewNop(), false, defaultHandleRecovery))
	r.GET("/p", func(c *gin.Context) {
		panic(&net.OpError{Op: "write", Err: &os.SyscallError{Syscall: "write", Err: errors.New("broken pipe")}})
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/p", nil))
	_ = w.Code
}

func TestTruncateString(t *testing.T) {
	s := strings.Repeat("a", maxLogBytes+3)
	logged, trunc, size := truncateString(s)
	if !trunc || size != maxLogBytes+3 || len(logged) != maxLogBytes {
		t.Fatalf("len=%d trunc=%v size=%d", len(logged), trunc, size)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("read fail") }

func TestGinzap_SkipPathsDoesNotLogBody(t *testing.T) {
	zl, buf := ginZapBuffer(t)
	r := gin.New()
	r.Use(GinzapWithConfig(zl, &Config{SkipPaths: []string{"/health"}, DefaultLevel: zapcore.InfoLevel}))
	r.POST("/health", func(c *gin.Context) {
		got, _ := io.ReadAll(c.Request.Body)
		if string(got) != "ping-body" {
			t.Errorf("handler body=%q", got)
		}
		c.Status(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodPost, "/health", strings.NewReader("ping-body"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if buf.Len() != 0 {
		t.Fatalf("skip path should not log, got %s", buf.String())
	}
}

func TestGinzap_LargeBodyRestoredAndTruncated(t *testing.T) {
	zl, buf := ginZapBuffer(t)
	r := gin.New()
	r.Use(Ginzap(zl, "", false))
	body := strings.Repeat("x", maxLogBytes+2048)
	r.POST("/x", func(c *gin.Context) {
		got, err := io.ReadAll(c.Request.Body)
		if err != nil {
			t.Errorf("handler read: %v", err)
		}
		if len(got) != len(body) {
			t.Errorf("handler got %d bytes, want %d", len(got), len(body))
		}
		c.Status(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
	req.ContentLength = int64(len(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	out := buf.String()
	if !strings.Contains(out, `"body_truncated":true`) {
		t.Fatalf("expected truncated flag: %s", out)
	}
}

func TestSnapshotRequestBody_ReadErrorRestores(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	req.Body = io.NopCloser(errReader{})
	logged, truncated, err := snapshotRequestBody(req)
	if err == nil || err.Error() != "read fail" {
		t.Fatalf("err=%v", err)
	}
	if truncated || len(logged) != 0 {
		t.Fatalf("logged=%q truncated=%v", logged, truncated)
	}
	got, readErr := io.ReadAll(req.Body)
	if readErr == nil || readErr.Error() != "read fail" {
		t.Fatalf("restored read err=%v body=%q", readErr, got)
	}
}
