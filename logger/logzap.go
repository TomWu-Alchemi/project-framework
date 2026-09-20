package logger

import (
	"bytes"
	"errors"
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
	"slices"
	"strings"
	"time"

	"github.com/bytedance/sonic"

	"github.com/TomWu-Alchemi/project-framework/internal/logutil"
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

// levelLogger is an optional capability probed by GinzapWithConfig: loggers
// implementing it can emit at an arbitrary zapcore.Level, so conf.DefaultLevel
// is honored exactly instead of being mapped onto Info/Error.
//
// *zap.Logger satisfies this interface natively (its Log method has exactly
// this signature; zap.Field is an alias of zapcore.Field).
type levelLogger interface {
	Log(lvl zapcore.Level, msg string, fields ...zap.Field)
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

// sensitiveHeaders lists header names whose values must be masked in logs.
//
// Keys MUST be spelled in canonical MIME header form (textproto.CanonicalMIMEHeaderKey:
// first letter and every letter after '-' upper-cased, all others lower-cased).
// net/http canonicalizes incoming header keys to this form, so any other
// spelling here silently never matches and the value leaks into the log
// (see docs/code-review.md, F-19 / appendix A-1: "X-API-Key" and
// "WWW-Authenticate" used to be written in non-canonical form).
var (
	sensitiveHeaders = map[string]struct{}{
		"Authorization":       {},
		"Cookie":              {},
		"Set-Cookie":          {},
		"X-Api-Key":           {},
		"Proxy-Authorization": {},
		"Www-Authenticate":    {},
	}
)

// Ginzap returns a gin.HandlerFunc (middleware) that logs requests using uber-go/zap.
//
// Requests with errors are logged using zap.Error().
// Requests without errors are logged using conf.DefaultLevel (InfoLevel by default).
//
// It receives:
//  1. A time package format string (e.g. time.RFC3339).
//  2. A boolean stating whether to use UTC time zone or local.
func Ginzap(logger ZapLogger, timeFormat string, utc bool) gin.HandlerFunc {
	return GinzapWithConfig(logger, &Config{TimeFormat: timeFormat, UTC: utc, DefaultLevel: zapcore.InfoLevel})
}

// GinzapWithConfig returns a gin.HandlerFunc using configs
func GinzapWithConfig(logger ZapLogger, conf *Config) gin.HandlerFunc {
	if logger == nil {
		logger = zap.NewNop()
	}
	if conf == nil {
		// P3-11: 防御 nil conf；TimeFormat 空、DefaultLevel=Info 与旧默认一致。
		conf = &Config{DefaultLevel: zapcore.InfoLevel}
	}
	skipPaths := make(map[string]bool, len(conf.SkipPaths))
	for _, path := range conf.SkipPaths {
		skipPaths[path] = true
	}

	return func(c *gin.Context) {
		start := time.Now()
		// some evil middlewares modify this values
		path := c.Request.URL.Path
		query := c.Request.URL.RawQuery
		skipByPath := skipPaths[path]
		if !skipByPath {
			for _, reg := range conf.SkipPathRegexps {
				if reg.MatchString(path) {
					skipByPath = true
					break
				}
			}
		}

		bodyStr := ""
		var bodyReadErr error
		var bodyTruncatedAtRead bool
		// bodyOriginalLen 是"日志口径的原始 body 长度"，供占位符 N 与下方 body_size 复用。
		// 未截断时即 len(bodyStr)；截断时 bodyStr 只剩已读前缀（恒为 MaxLogBytes），
		// 需要用 ContentLength 尽量还原真实长度；ContentLength 不可用（未知为 -1，
		// 或不大于已读前缀）时退化为已读到的下界。必须在本块内、bodyStr 被占位/脱敏
		// 替换之前算出——替换后 len(bodyStr) 不再代表原始长度。
		bodyOriginalLen := 0
		if !skipByPath {
			var logged []byte
			logged, bodyTruncatedAtRead, bodyReadErr = snapshotRequestBody(c.Request)
			bodyStr = string(logged)
			bodyOriginalLen = len(bodyStr)
			if bodyTruncatedAtRead {
				if cl := c.Request.ContentLength; cl > int64(bodyOriginalLen) {
					bodyOriginalLen = int(cl)
				}
			}
			// 空 body（如未带 Content-Type 的 GET）不占位、不脱敏，保持空串：
			// 否则会凭空多出 "body":"[filtered len=0]" 字段，污染全量访问日志。
			if bodyStr != "" {
				mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
				// F-20（fail closed）：非明文编码（gzip 等）、Content-Type 缺失/非法、
				// 截断的 form/json 都无法可靠脱敏，一律占位、禁止回显原文，与
				// filterSensitiveQuery 对不可解析 query 的 P3-10 取舍一致。
				if enc := c.GetHeader("Content-Encoding"); enc != "" && !strings.EqualFold(enc, "identity") {
					bodyStr = fmt.Sprintf("[filtered len=%d]", bodyOriginalLen)
				} else if err != nil {
					// Content-Type 缺失或非法：没有媒体类型可用于选择脱敏策略，原文
					// 可能含 password 明文，一律占位（修复前这条路径会原样回显原文）。
					bodyStr = fmt.Sprintf("[filtered len=%d]", bodyOriginalLen)
				} else {
					switch mediaType {
					case "application/x-www-form-urlencoded":
						if bodyTruncatedAtRead {
							// 截断点可能落在 password 值中间，值前缀仍会泄漏
							bodyStr = fmt.Sprintf("[filtered len=%d]", bodyOriginalLen)
						} else {
							bodyStr = filterSensitiveData(bodyStr)
						}
					case "application/json":
						if bodyTruncatedAtRead {
							// 截断的 JSON 解析必然失败（scan 到截断点才报错），
							// 与 form 分支一致直接占位，省掉一次必然失败的解析（P-1.6）。
							bodyStr = fmt.Sprintf("[filtered len=%d]", bodyOriginalLen)
						} else {
							bodyStr = filterSensitiveDataForJson(bodyStr)
						}
					default:
						// 有意保留原文：未识别的媒体类型（multipart/form-data、text/*，以及
						// `json` 这类 ParseMediaType 成功但非合法 MIME 形态的值）不做占位，
						// 保住非结构化 body 的排障信息。已知取舍：multipart 正常表单提交的
						// password 字段仍会明文入库，作为后续独立事项评估；本处不要改为占位。
					}
				}
			}
		}
		c.Next()
		track := !skipByPath
		if track && conf.Skipper != nil && conf.Skipper(c) {
			track = false
		}

		if track {
			end := time.Now()
			latency := end.Sub(start)
			if conf.UTC {
				end = end.UTC()
			}

			loggedQuery, queryTruncated, querySize := logutil.TruncateString(filterSensitiveQuery(query))
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
				zap.Object("headers", headerLogObject(c.Request.Header)),
			}
			if conf.TimeFormat != "" {
				fields = append(fields, zap.String("time", end.Format(conf.TimeFormat)))
			}
			if bodyReadErr != nil {
				fields = append(fields, zap.Error(bodyReadErr))
			}
			if len(bodyStr) > 0 || bodyTruncatedAtRead {
				loggedBody, bodyTruncated, bodySize := logutil.TruncateString(bodyStr)
				if bodyTruncatedAtRead {
					bodyTruncated = true
					// 截断时 bodyStr 可能是占位符（20 余字节），TruncateString 给出的
					// bodySize 会取到占位符自身长度，与 body_truncated=true 自相矛盾；
					// 统一改用日志口径的原始长度（已在上方按 ContentLength 修正，
					// 长度未知时退化为已读到的下界）。
					bodySize = bodyOriginalLen
				}
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
			} else if ll, ok := logger.(levelLogger); ok {
				// Exact level: *zap.Logger satisfies levelLogger natively, so
				// this branch is byte-for-byte the old *zap.Logger behavior.
				ll.Log(conf.DefaultLevel, "http", fields...)
			} else if conf.DefaultLevel <= zapcore.InfoLevel {
				// Fallback logger exposes only Info/Error: Debug/Info must not
				// be dropped, so they are emitted through Info.
				logger.Info(path, fields...)
			} else {
				// Warn and above are conservatively emitted through Error.
				logger.Error(path, fields...)
			}
		}
	}
}

type bodyRestore struct {
	io.Reader
	c io.Closer
}

func (b bodyRestore) Close() error {
	if b.c == nil {
		return nil
	}
	return b.c.Close()
}

// snapshotRequestBody copies at most logutil.MaxLogBytes+1 from the request for
// logging, then restores the full body (prefix + remainder) for downstream handlers.
func snapshotRequestBody(r *http.Request) ([]byte, bool, error) {
	if r == nil || r.Body == nil {
		return nil, false, nil
	}
	orig := r.Body
	buf, err := io.ReadAll(io.LimitReader(orig, int64(logutil.MaxLogBytes)+1))
	r.Body = bodyRestore{Reader: io.MultiReader(bytes.NewReader(buf), orig), c: orig}
	if err != nil {
		return buf, false, err
	}
	logged, truncated, _ := logutil.TruncateBytes(buf)
	return logged, truncated, nil
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

// jsonLogAPI 是包级冻结一次的 sonic 解码 API：UseNumber 让大于 2^53 的整数
// round-trip 不丢精度（F-15）。sonic 没有包级 NewDecoder，必须从 frozen Config
// 取 decoder。旧写法在每个 JSON body 上都重复 Config{...}.Froze() 一次（P-1.3），
// 该冻结成本与 body 无关、随 QPS 线性放大；冻结后的 API 只读、并发安全可复用。
var jsonLogAPI = sonic.Config{UseNumber: true}.Froze()

// filterSensitiveDataForJson masks password fields in a JSON body without
// corrupting big integers: UseNumber decodes numbers as json.Number, so values
// beyond 2^53 survive the round-trip unchanged (F-15). sonic has no top-level
// NewDecoder, the decoder must be obtained from a frozen Config.
//
// F-20（fail closed）：解析/序列化失败（截断、坏 JSON、压缩体）不再回显原文，
// 改为占位符——与 filterSensitiveQuery 对不可解析 query 的 P3-10 取舍一致：
// 宁可损失排障信息，不放行可能含明文密码的原文。
func filterSensitiveDataForJson(body string) string {
	if body == "" {
		return body
	}
	dec := jsonLogAPI.NewDecoder(strings.NewReader(body))
	var v any
	if err := dec.Decode(&v); err != nil {
		return fmt.Sprintf("[filtered len=%d]", len(body))
	}
	maskPasswordInJSON(v)
	filtered, err := sonic.Marshal(v)
	if err != nil {
		return fmt.Sprintf("[filtered len=%d]", len(body))
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
		// P3-10: ParseQuery 失败时部分键值仍可能含 password，禁止回显原文。
		return fmt.Sprintf("[filtered len=%d]", len(raw))
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

func defaultHandleRecovery(c *gin.Context, err any) {
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
	if logger == nil {
		logger = zap.NewNop()
	}
	return func(c *gin.Context) {
		defer func() {
			if err := recover(); err != nil {
				// Check for a broken connection, as it is not really a
				// condition that warrants a panic stack trace.
				// F-30：errors.As 识别包装过的 *net.OpError（直接断言会漏判）；
				// panic 值可能不是 error，先做类型断言再 As。
				var brokenPipe bool
				if e, ok := err.(error); ok {
					var ne *net.OpError
					if errors.As(e, &ne) {
						if se, ok := ne.Err.(*os.SyscallError); ok {
							msg := strings.ToLower(se.Error())
							brokenPipe = strings.Contains(msg, "broken pipe") ||
								strings.Contains(msg, "connection reset by peer")
						}
					}
				}

				// F-21：dump 前对克隆请求脱敏（敏感头 + query），避免 Authorization /
				// X-Api-Key / ?password= 明文进入 panic 日志。Clone 对 Header 与 URL
				// 均为深拷贝、不消费 Body，不影响原请求；DumpRequest 对服务端请求优先
				// 输出 RequestURI，因此一并重写，否则 query 仍会从请求行泄漏。
				reqCopy := c.Request.Clone(c.Request.Context())
				reqCopy.Header = filterSensitiveHeaders(reqCopy.Header)
				reqCopy.URL.RawQuery = filterSensitiveQuery(reqCopy.URL.RawQuery)
				reqCopy.RequestURI = reqCopy.URL.RequestURI()
				httpRequest, _ := httputil.DumpRequest(reqCopy, false)
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

// isSensitiveHeader 判定头名是否为敏感头。只认 sensitiveHeaders 中的 canonical
// 拼写（http.Header 的键已被 net/http 规范化为该形式）。filterSensitiveHeaders
// （Recovery 路径）与 headerLogObject 编码器共用这一份判定，避免规则漂移（P-1.4）。
func isSensitiveHeader(name string) bool {
	_, ok := sensitiveHeaders[name]
	return ok
}

// 过滤敏感请求头。仅 Recovery 路径使用：DumpRequest 仍需要 map 形态。
func filterSensitiveHeaders(headers http.Header) map[string][]string {
	filtered := make(map[string][]string)
	for k, v := range headers {
		if isSensitiveHeader(k) {
			filtered[k] = []string{"[FILTERED]"}
		} else {
			filtered[k] = v
		}
	}
	return filtered
}

// maxStackHeaderKeys 是栈上排序缓冲可容纳的头数量：常见请求头 10~30 个，落在
// 该阈值内时键切片不逃逸到堆；超出才退化为一次堆分配。
const maxStackHeaderKeys = 32

// headerLogObject 是 http.Header 的日志编码视图：zap.Object 直接编码原 header，
// 边遍历边脱敏——不构造中间过滤 map、不走反射（P-1.4 方案 B）。
//
// 使用前提：编码发生在 GinzapWithConfig 中 c.Next() 之后的访问日志组装阶段，
// 与旧实现 filterSensitiveHeaders 遍历请求头的时点一致；此处只读、不得与对
// c.Request.Header 的并发修改重叠（中间件链在此处本就不写 header），不新增风险。
//
// 键序：旧路径 zap.Any(map) 经 encoding/json 编码、键按字典序升序输出；为保持
// 访问日志输出逐字节一致，这里同样对键排序后写出（与 encoding/json 的字节序一致）。
type headerLogObject http.Header

// headerValues 把 []string 适配为 zapcore.ArrayMarshaler，逐元素 AppendString，
// 保持编码形态为字符串数组（不收成单个 string）。
type headerValues []string

func (v headerValues) MarshalLogArray(enc zapcore.ArrayEncoder) error {
	for _, s := range v {
		enc.AppendString(s)
	}
	return nil
}

// filteredHeaderValue 是敏感头的占位值：包级只读、仅被读取，避免每次分配。
var filteredHeaderValue = []string{"[FILTERED]"}

// MarshalLogObject 逐个头写出，命中敏感头时改用只读占位值。空 header 自然输出 {}。
func (h headerLogObject) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	var stack [maxStackHeaderKeys]string
	keys := stack[:0]
	if len(h) > len(stack) {
		keys = make([]string, 0, len(h))
	}
	for k := range h {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		vs := h[k]
		if isSensitiveHeader(k) {
			vs = filteredHeaderValue
		}
		if err := enc.AddArray(k, headerValues(vs)); err != nil {
			return err
		}
	}
	return nil
}
