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
				// bodyFiltered 标记上方分支是否已输出占位符；未占位时下方统一做
				// “未掩码 password 兜底断言”，堵住任何保原文路径把明文密码写进日志。
				bodyFiltered := false
				// F-20（fail closed）：非明文编码（gzip 等）、Content-Type 缺失/非法、
				// 截断的 form/json 都无法可靠脱敏，一律占位、禁止回显原文，与
				// filterSensitiveQuery 对不可解析 query 的 P3-10 取舍一致。
				if enc := c.GetHeader("Content-Encoding"); enc != "" && !strings.EqualFold(enc, "identity") {
					bodyStr = fmt.Sprintf("[filtered len=%d]", bodyOriginalLen)
					bodyFiltered = true
				} else if err != nil {
					// Content-Type 缺失或非法：没有媒体类型可用于选择脱敏策略，原文
					// 可能含 password 明文，一律占位（修复前这条路径会原样回显原文）。
					bodyStr = fmt.Sprintf("[filtered len=%d]", bodyOriginalLen)
					bodyFiltered = true
				} else {
					switch mediaType {
					case "application/x-www-form-urlencoded":
						if bodyTruncatedAtRead {
							// 截断点可能落在 password 值中间，值前缀仍会泄漏
							bodyStr = fmt.Sprintf("[filtered len=%d]", bodyOriginalLen)
							bodyFiltered = true
						} else {
							bodyStr = filterSensitiveData(bodyStr)
						}
					case "application/json":
						if bodyTruncatedAtRead {
							// 截断的 JSON 解析必然失败（scan 到截断点才报错），
							// 与 form 分支一致直接占位，省掉一次必然失败的解析（P-1.6）。
							bodyStr = fmt.Sprintf("[filtered len=%d]", bodyOriginalLen)
							bodyFiltered = true
						} else {
							bodyStr = filterSensitiveDataForJson(bodyStr)
						}
					default:
						// 未识别的媒体类型（multipart/form-data、text/*，以及 `json` 这类
						// ParseMediaType 成功但非合法 MIME 形态的值）保持原文以保住排障信息。
						// 明文安全由下方统一兜底断言保证：检测到未掩码 password 值即占位。
					}
				}
				// 统一兜底断言层（fail closed）：凡未走占位符的路径（尤其是保原文的未识别
				// 媒体类型、声明类型与实际 body 不符的路径）在写入日志前，必须再过一遍
				// “是否仍含未掩码 password 值”；命中即占位。策略单一，不做二次脱敏。
				if !bodyFiltered && hasUnmaskedPassword(bodyStr) {
					bodyStr = fmt.Sprintf("[filtered len=%d]", bodyOriginalLen)
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
				loggedBody, bodyTruncated, _ := logutil.TruncateString(bodyStr)
				if bodyTruncatedAtRead {
					bodyTruncated = true
				}
				// body_size 恒为“日志口径的原始 body 长度”，与 body 字段实际写出的文本
				// 长度解耦：占位符仅 20 余字节、脱敏后的 JSON/form 也可能比原文更长，若
				// 沿用 TruncateString 的 size（脱敏后/占位符长度）会与原文长度自相矛盾。
				// bodyOriginalLen 已在上方算好：截断时按 Content-Length 还原，未知时退化
				// 为已读下界。body_truncated 的语义与取值维持现状不变。
				fields = append(fields,
					zap.String("body", loggedBody),
					zap.Bool("body_truncated", bodyTruncated),
					zap.Int("body_size", bodyOriginalLen),
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

// maskedPasswordValue 是 password 掩码后写入日志的字面量：JSON / form / query 三条
// 脱敏路径统一产出它，hasUnmaskedPassword 也据此判定“已掩码”。
const maskedPasswordValue = "******"

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
			b.WriteString("=" + maskedPasswordValue)
			continue
		}
		b.WriteString(part)
	}
	return b.String()
}

// hasUnmaskedPassword 判定“最终要写入日志的 body 文本”中是否仍残留未掩码的 password 值，
// 作为未识别媒体类型（multipart/text/* 等）保留原文路径的兜底断言层：命中即改为占位符。
//
// 判据（大小写不敏感；“password” 前必须是标识符边界，避免 mypassword / new_password 误判）：
//   - 带引号 key：`"password" : "值"` / `'password': '值'`（JSON、Python 风格）——值非空
//     且不是 ****** 即泄漏；含转义与嵌套；仅出现键而无值、或 null 视为无值；
//   - 无引号 key：`password:"值"` / `password: 值`（对象字面量 / YAML / 头风格）——同上规则；
//   - form/query：`password=值`——值非空且不是 ****** 即泄漏（值以 '&' 收尾）；
//   - multipart 字段名：`name = "password"` / `name='password'` / `name=password`
//     （允许 '=' 两侧空白、单/双引号）——该路径不脱敏，字段值必然未掩码，直接判为泄漏。
//
// 不构成泄漏：文本中只有 password 一词、其后无 ':' 或 '='（如 “reset your password
// now”），无值可判定；取值恰为 ****** 或空串同样不占位；`name="password"` 之后不是
// 参数边界（如 HTML `<input name="password">`）或 name 前非词边界（filename=…）时
// 不按 multipart 处理，避免误占位。
//
// 输入为待写日志的 body 文本（上限 logutil.MaxLogBytes ≤256KiB）。实现选择手写单遍
// 大小写不敏感扫描而非 strings.ToLower 后 Index：后者会为每个访问请求多复制一份最多
// 256KiB 的临时字符串，前者零额外分配。
func hasUnmaskedPassword(s string) bool {
	const key = "password"
	for from := 0; from+len(key) <= len(s); {
		i := indexFold(s, from, key)
		if i < 0 {
			return false
		}
		end := i + len(key)
		if i > 0 && isTokenByte(s[i-1]) {
			// 词内子串（mypassword / new_password），并非独立字段名。
			from = end
			continue
		}
		if end < len(s) {
			// multipart 字段名（不脱敏，值必然未掩码）：优先于取值判定。
			if hasMultipartPasswordField(s, i, end) {
				return true
			}
			if val, ok := passwordValueAfter(s, end); ok && !isMaskedPasswordValue(val) {
				return true
			}
		}
		from = end
	}
	return false
}

// indexFold 返回 s[from:] 中首个大小写不敏感等于 key 的子串起始绝对下标，未命中返回 -1。
// 等价于 strings.Index(strings.ToLower(s[from:]), key) 但零分配。
func indexFold(s string, from int, key string) int {
	m := len(key)
	for i := from; i+m <= len(s); i++ {
		if equalFoldAt(s, i, key) {
			return i
		}
	}
	return -1
}

func equalFoldAt(s string, i int, key string) bool {
	for j := 0; j < len(key); j++ {
		if lowerASCII(s[i+j]) != lowerASCII(key[j]) {
			return false
		}
	}
	return true
}

// lowerASCII 仅折叠 ASCII 大写字母；password 为纯 ASCII，足够。
func lowerASCII(b byte) byte {
	if 'A' <= b && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}

// isTokenByte 报告 b 是否属于 [A-Za-z0-9_]（字段名可取字符），用于 password 单词边界判定。
func isTokenByte(b byte) bool {
	return b == '_' || '0' <= b && b <= '9' || 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z'
}

// isMaskedPasswordValue 报告 password 的取值是否无需占位：空串（无值）或掩码字面量。
func isMaskedPasswordValue(val string) bool {
	return val == "" || val == maskedPasswordValue
}

// hasMultipartPasswordField 判定 s[i:end]（i 为 "password" 起始、end 为其末尾）是否构成
// multipart Content-Disposition 里的 password 字段名声明（该路径不脱敏，字段值必然未掩码）：
//
//	name = "password"   /   name='password'   /   name=password
//
// 主判据（不依赖 `name=` 前缀文本匹配）：password 命中处后一字节为字段名闭合引号
// `"` 或 `'`，且其后（可跳过空白）为参数边界 CR / LF / ';' / 串尾 → 命中。因此
// `{"password":"x"}`（引号后是 ':'）与 `{"password"}`（引号后是 '}'）都不命中。
// 无引号 token 形态 `name=password` 另按 `name` '=' 前缀（word 边界）判定，避免误伤
// 普通文本里的 “name=password” 字样之外的场景。
func hasMultipartPasswordField(s string, i, end int) bool {
	if end < len(s) && (s[end] == '"' || s[end] == '\'') {
		// 带引号字段名：闭引号之后（跳过空白）须为参数边界。
		j := end + 1
		for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
			j++
		}
		return j >= len(s) || s[j] == ';' || s[j] == '\r' || s[j] == '\n'
	}
	// 无引号 token 形态：`name = password` —— 其后须为参数边界，且前方须为 name= 前缀。
	j := end
	for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
		j++
	}
	if j < len(s) && s[j] != ';' && s[j] != '\r' && s[j] != '\n' {
		return false
	}
	return hasMultipartNameEquals(s, i)
}

// hasMultipartNameEquals 报告 s[:i] 是否以 `name <ws>* = <ws>*` 结尾（大小写不敏感，
// name 前须为词边界，排除 filename=…）。用于无引号 token 形态 `name=password` 的认定。
func hasMultipartNameEquals(s string, i int) bool {
	k := skipSpaceBack(s, i)
	if k == 0 || s[k-1] != '=' {
		return false
	}
	k--
	k = skipSpaceBack(s, k)
	if k < 4 || !strings.EqualFold(s[k-4:k], "name") {
		return false
	}
	return k-4 == 0 || !isTokenByte(s[k-5])
}

// passwordValueAfter 从 pos（紧随 "password" 之后）尝试取出可判定的 password 取值。
// 返回的 val 为原样文本（可能含 JSON 转义或 form 编码）；ok 表示确实定位到一个值。
func passwordValueAfter(s string, pos int) (val string, ok bool) {
	i := pos
	switch {
	case i < len(s) && s[i] == '=':
		// form/query: password=VALUE，值以 '&' 收尾（不存在则到串尾）。
		start := i + 1
		i = start
		for i < len(s) && s[i] != '&' {
			i++
		}
		return s[start:i], true
	case i < len(s) && (s[i] == '"' || s[i] == '\''):
		// 带引号 key："password" / 'password' [ws] ':' VALUE
		i++
		i = skipSpaceForward(s, i)
		if i >= len(s) || s[i] != ':' {
			return "", false
		}
		return valueAfterColon(s, i+1)
	}
	// 无引号 key：password [ws] ':' VALUE（对象字面量 / YAML / 头风格）。
	i = skipSpaceForward(s, i)
	if i < len(s) && s[i] == ':' {
		return valueAfterColon(s, i+1)
	}
	return "", false
}

// valueAfterColon 取 ':' 之后的 password 取值：跳过空白；串尾视为无值；引号值取引号内
// 内容（保留转义，未闭合按“已取值”交由上层 fail closed）；null 视为无值；其余按无引号
// 值处理（到空白/结构分隔符为止），取空则视为无值。
func valueAfterColon(s string, i int) (string, bool) {
	i = skipSpaceForward(s, i)
	if i >= len(s) {
		return "", false
	}
	if s[i] == '"' || s[i] == '\'' {
		return quotedValue(s, i+1, s[i])
	}
	if strings.HasPrefix(s[i:], "null") {
		return "", false // null：无值
	}
	return unquotedValue(s, i), true
}

// quotedValue 从 start（开引号之后）扫描到闭合引号，返回不含引号的原始内容（保留转义）。
// 未闭合（截断/坏 JSON）时返回剩余内容并同样视为“取到值”——截断前缀可能仍是明文密码，
// 交由上层 fail closed 占位。
func quotedValue(s string, start int, quote byte) (string, bool) {
	for i := start; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++ // 跳过被转义字符，避免把 \" 误判为闭合引号
		case quote:
			return s[start:i], true
		}
	}
	return s[start:], true
}

// unquotedValue 取无引号取值：到空白或结构分隔符为止（取空则上层视为无值）。
func unquotedValue(s string, i int) string {
	start := i
	for i < len(s) && !isValueTerminator(s[i]) {
		i++
	}
	return s[start:i]
}

// isValueTerminator 报告 b 是否终止无引号取值：空白 / `,` `}` `&` `;`（串尾由循环条件处理）。
func isValueTerminator(b byte) bool {
	switch b {
	case ' ', '\t', '\r', '\n', ',', '}', '&', ';':
		return true
	}
	return false
}

// skipSpaceForward 跳过 ASCII 空白（JSON 结构空白：空格 / Tab / CR / LF）。
func skipSpaceForward(s string, i int) int {
	for i < len(s) && isJSONSpace(s[i]) {
		i++
	}
	return i
}

// skipSpaceBack 从 i 向前跳过空格 / Tab，返回新的下标（用于 multipart 参数内的空白）。
func skipSpaceBack(s string, i int) int {
	for i > 0 && (s[i-1] == ' ' || s[i-1] == '\t') {
		i--
	}
	return i
}

// isJSONSpace 报告 b 是否为 JSON 结构空白（RFC 8259）。
func isJSONSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
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
				x[k] = maskedPasswordValue
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
			vs[i] = maskedPasswordValue
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
