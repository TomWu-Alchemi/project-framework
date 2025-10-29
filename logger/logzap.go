package logger

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
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
			contentType := c.GetHeader("Content-Type")
			if c.Request.Method == http.MethodPost && contentType == "application/x-www-form-urlencoded" {
				// 打印请求时过滤敏感信息
				bodyStr = filterSensitiveData(bodyStr)
			}
			if c.Request.Method == http.MethodPost && contentType == "application/json" {
				// 打印请求时过滤敏感信息
				bodyStr = filterSensitiveDataForJson(bodyStr)
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

			fields := []zapcore.Field{
				zap.Int("status", c.Writer.Status()),
				zap.String("method", c.Request.Method),
				zap.String("path", path),
				zap.String("query", query),
				zap.String("ip", c.ClientIP()),
				zap.String("user-agent", c.Request.UserAgent()),
				zap.Int64("latency", latency.Milliseconds()),
				zap.Any("headers", filterSensitiveHeaders(c.Request.Header)),
			}
			if conf.TimeFormat != "" {
				fields = append(fields, zap.String("time", end.Format(conf.TimeFormat)))
			}
			if len(bodyStr) > 0 {
				fields = append(fields, zap.String("body", bodyStr))
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
	// 将 body 按照 & 分割成 key=value 形式的片段
	parts := strings.Split(body, "&")

	// 遍历每个片段，检查是否是 password 字段
	for i, part := range parts {
		if strings.HasPrefix(part, "password=") {
			// 将 password 的值替换为 ***
			parts[i] = "password=******"
		}
	}

	// 将所有片段重新组合成字符串并返回
	return strings.Join(parts, "&")
}

func filterSensitiveDataForJson(body string) string {
	var jsonData map[string]interface{}
	if err := sonic.UnmarshalString(body, &jsonData); err == nil {
		// 将密码字段替换为 ***
		if _, exists := jsonData["password"]; exists {
			jsonData["password"] = "******"
		}
		// 重新序列化 JSON
		filteredBytes, _ := sonic.Marshal(jsonData)
		return string(filteredBytes)
	}
	// 如果解析失败，返回原始内容
	return body
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
					// If the connection is dead, we can't write a status to it.
					c.Error(err.(error)) //nolint: errcheck
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
