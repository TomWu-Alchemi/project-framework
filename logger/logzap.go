package logger

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"runtime/debug"
	"strings"
	"time"

	"github.com/bytedance/sonic"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type Fn func(c *gin.Context) []zapcore.Field

// Skipper is a function to skip logs based on provided Context
type Skipper func(c *gin.Context) bool

// ZapLogger is the minimal logger interface compatible with zap.Logger
type ZapLogger interface {
	Info(msg string, fields ...zap.Field)
	Error(msg string, fields ...zap.Field)
}

// Config is config setting for Ginzap
type Config struct {
	TimeFormat      string
	UTC             bool
	SkipPaths       []string
	SkipPathRegexps []*regexp.Regexp
	Context         Fn
	DefaultLevel    zapcore.Level
	// skip is a Skipper that indicates which logs should not be written.
	// Optional.
	Skipper Skipper
}

const maxLogBytes = 256 << 10

var (
	sensitiveHeaders = map[string]struct{}{
		"Authorization":       {},
		"Cookie":              {},
		"Set-Cookie":          {},
		"X-API-Key":           {},
		"Proxy-Authorization": {},
		"WWW-Authenticate":    {},
	}
)

// Ginzap returns a gin.HandlerFunc (middleware) that logs requests using uber-go/zap.
//
// Requests with errors are logged using zap.Error().
// Requests without errors are logged using zap.Info().
//
// It receives:
//  1. A time package format string (e.g. time.RFC3339).
//  2. A boolean stating whether to use UTC time zone or local.
func Ginzap(logger ZapLogger, timeFormat string, utc bool) gin.HandlerFunc {
	return GinzapWithConfig(logger, &Config{TimeFormat: timeFormat, UTC: utc, DefaultLevel: zapcore.InfoLevel})
}

// GinzapWithConfig returns a gin.HandlerFunc using configs
func GinzapWithConfig(logger ZapLogger, conf *Config) gin.HandlerFunc {
	skipPaths := make(map[string]bool, len(conf.SkipPaths))
	for _, path := range conf.SkipPaths {
		skipPaths[path] = true
	}

	return func(c *gin.Context) {
		start := time.Now()
		// some evil middlewares modify this values
		path := c.Request.URL.Path
		query := c.Request.URL.RawQuery
		bodyStr := ""
		if c.Request.Body != nil {
			body, _ := io.ReadAll(c.Request.Body)
			bodyStr = string(body)
			c.Request.Body = io.NopCloser(bytes.NewBuffer(body))
			mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
			if err == nil {
				switch mediaType {
				case "application/x-www-form-urlencoded":
					bodyStr = filterSensitiveData(bodyStr)
				case "application/json":
					bodyStr = filterSensitiveDataForJson(bodyStr)
				}
			}
		}
		c.Next()
		track := true

		if _, ok := skipPaths[path]; ok || (conf.Skipper != nil && conf.Skipper(c)) {
			track = false
		}

		if track && len(conf.SkipPathRegexps) > 0 {
			for _, reg := range conf.SkipPathRegexps {
				if !reg.MatchString(path) {
					continue
				}

				track = false
				break
			}
		}

		if track {
			end := time.Now()
			latency := end.Sub(start)
			if conf.UTC {
				end = end.UTC()
			}

			loggedQuery, queryTruncated, querySize := truncateString(filterSensitiveQuery(query))
			fields := []zapcore.Field{
				zap.Int("status", c.Writer.Status()),
				zap.String("method", c.Request.Method),
				zap.String("path", path),
				zap.String("query", loggedQuery),
				zap.Bool("query_truncated", queryTruncated),
				zap.Int("query_size", querySize),
				zap.String("ip", c.ClientIP()),
				zap.String("user-agent", c.Request.UserAgent()),
				zap.Int64("latency", latency.Milliseconds()),
				zap.Any("headers", filterSensitiveHeaders(c.Request.Header)),
			}
			if conf.TimeFormat != "" {
				fields = append(fields, zap.String("time", end.Format(conf.TimeFormat)))
			}
			if len(bodyStr) > 0 {
				loggedBody, bodyTruncated, bodySize := truncateString(bodyStr)
				fields = append(fields,
					zap.String("body", loggedBody),
					zap.Bool("body_truncated", bodyTruncated),
					zap.Int("body_size", bodySize),
				)
			}

			if conf.Context != nil {
				fields = append(fields, conf.Context(c)...)
			}

			if len(c.Errors) > 0 {
				// Append error field if this is an erroneous request.
				for _, e := range c.Errors.Errors() {
					logger.Error(e, fields...)
				}
			} else {
				if zl, ok := logger.(*zap.Logger); ok {
					zl.Log(conf.DefaultLevel, "http", fields...)
				} else if conf.DefaultLevel == zapcore.InfoLevel {
					logger.Info(path, fields...)
				} else {
					logger.Error(path, fields...)
				}
			}
		}
	}
}

func filterSensitiveData(body string) string {
	var b strings.Builder
	first := true
	for part := range strings.SplitSeq(body, "&") {
		if !first {
			b.WriteByte('&')
		}
		first = false
		key, _, found := strings.Cut(part, "=")
		if !found {
			b.WriteString(part)
			continue
		}
		decoded, err := url.QueryUnescape(key)
		if err != nil {
			decoded = key
		}
		if strings.EqualFold(decoded, "password") {
			b.WriteString(key)
			b.WriteString("=******")
			continue
		}
		b.WriteString(part)
	}
	return b.String()
}

func filterSensitiveDataForJson(body string) string {
	var v any
	if err := sonic.UnmarshalString(body, &v); err != nil {
		return body
	}
	maskPasswordInJSON(v)
	filtered, err := sonic.Marshal(v)
	if err != nil {
		return body
	}
	return string(filtered)
}

func maskPasswordInJSON(v any) {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			if strings.EqualFold(k, "password") {
				x[k] = "******"
				continue
			}
			maskPasswordInJSON(child)
		}
	case []any:
		for _, child := range x {
			maskPasswordInJSON(child)
		}
	}
}

func filterSensitiveQuery(raw string) string {
	if raw == "" {
		return raw
	}
	values, err := url.ParseQuery(raw)
	if err != nil {
		return raw
	}
	changed := false
	for k, vs := range values {
		if !strings.EqualFold(k, "password") {
			continue
		}
		for i := range vs {
			vs[i] = "******"
		}
		changed = true
	}
	if !changed {
		return raw
	}
	return values.Encode()
}

func truncateString(s string) (logged string, truncated bool, size int) {
	size = len(s)
	if size <= maxLogBytes {
		return s, false, size
	}
	return strings.Clone(s[:maxLogBytes]), true, size
}

func defaultHandleRecovery(c *gin.Context, err interface{}) {
	c.AbortWithStatus(http.StatusInternalServerError)
}

// RecoveryWithZap returns a gin.HandlerFunc (middleware)
// that recovers from any panics and logs requests using uber-go/zap.
// All errors are logged using zap.Error().
// stack means whether output the stack info.
// The stack info is easy to find where the error occurs but the stack info is too large.
func RecoveryWithZap(logger ZapLogger, stack bool) gin.HandlerFunc {
	return CustomRecoveryWithZap(logger, stack, defaultHandleRecovery)
}

// CustomRecoveryWithZap returns a gin.HandlerFunc (middleware) with a custom recovery handler
// that recovers from any panics and logs requests using uber-go/zap.
// All errors are logged using zap.Error().
// stack means whether output the stack info.
// The stack info is easy to find where the error occurs but the stack info is too large.
func CustomRecoveryWithZap(logger ZapLogger, stack bool, recovery gin.RecoveryFunc) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if err := recover(); err != nil {
				// Check for a broken connection, as it is not really a
				// condition that warrants a panic stack trace.
				var brokenPipe bool
				if ne, ok := err.(*net.OpError); ok {
					if se, ok := ne.Err.(*os.SyscallError); ok {
						if strings.Contains(strings.ToLower(se.Error()), "broken pipe") ||
							strings.Contains(strings.ToLower(se.Error()), "connection reset by peer") {
							brokenPipe = true
						}
					}
				}

				httpRequest, _ := httputil.DumpRequest(c.Request, false)
				if brokenPipe {
					logger.Error(c.Request.URL.Path,
						zap.Any("error", err),
						zap.String("request", string(httpRequest)),
					)
					if e, ok := err.(error); ok {
						_ = c.Error(e)
					} else {
						_ = c.Error(fmt.Errorf("%v", err))
					}
					c.Abort()
					return
				}

				if stack {
					logger.Error("[Recovery from panic]",
						zap.Time("time", time.Now()),
						zap.Any("error", err),
						zap.String("request", string(httpRequest)),
						zap.String("stack", string(debug.Stack())),
					)
				} else {
					logger.Error("[Recovery from panic]",
						zap.Time("time", time.Now()),
						zap.Any("error", err),
						zap.String("request", string(httpRequest)),
					)
				}
				recovery(c, err)
			}
		}()
		c.Next()
	}
}

// 过滤敏感请求头
func filterSensitiveHeaders(headers http.Header) map[string][]string {
	filtered := make(map[string][]string)
	for k, v := range headers {
		if _, ok := sensitiveHeaders[k]; ok {
			filtered[k] = []string{"[FILTERED]"}
		} else {
			filtered[k] = v
		}
	}
	return filtered
}
