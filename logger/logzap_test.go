package logger

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/TomWu-Alchemi/project-framework/internal/logutil"
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
	body := strings.Repeat("x", logutil.MaxLogBytes+2048)
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

// recordingLogger implements ZapLogger only (Info/Error) and records which
// entry points were called, so tests can assert the fallback level mapping.
type recordingLogger struct {
	infos  []string
	errors []string
}

func (l *recordingLogger) Info(msg string, _ ...zap.Field)  { l.infos = append(l.infos, msg) }
func (l *recordingLogger) Error(msg string, _ ...zap.Field) { l.errors = append(l.errors, msg) }

// serveGinzapRequest runs one GET /x through GinzapWithConfig with the given
// logger and DefaultLevel.
func serveGinzapRequest(t *testing.T, logger ZapLogger, level zapcore.Level) {
	t.Helper()
	r := gin.New()
	r.Use(GinzapWithConfig(logger, &Config{DefaultLevel: level}))
	r.GET("/x", func(c *gin.Context) { c.Status(http.StatusOK) })
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestGinzapWithConfig_DebugLevelUsesInfoFallback(t *testing.T) {
	l := &recordingLogger{}
	serveGinzapRequest(t, l, zapcore.DebugLevel)
	// Debug is <= Info: must go through Info (not dropped), never through Error.
	if len(l.infos) != 1 {
		t.Fatalf("DefaultLevel=Debug must call Info exactly once, infos=%v errors=%v", l.infos, l.errors)
	}
	if len(l.errors) != 0 {
		t.Fatalf("DefaultLevel=Debug must not call Error, errors=%v", l.errors)
	}
}

func TestGinzapWithConfig_WarnLevelUsesErrorFallback(t *testing.T) {
	l := &recordingLogger{}
	serveGinzapRequest(t, l, zapcore.WarnLevel)
	// Warn is > Info: conservative fallback goes through Error.
	if len(l.errors) != 1 {
		t.Fatalf("DefaultLevel=Warn must call Error exactly once, infos=%v errors=%v", l.infos, l.errors)
	}
	if len(l.infos) != 0 {
		t.Fatalf("DefaultLevel=Warn must not call Info, infos=%v", l.infos)
	}
}

func TestGinzapWithConfig_ZapLoggerKeepsExactLevel(t *testing.T) {
	zl, buf := ginZapBuffer(t) // buffer core with DebugLevel enabled
	serveGinzapRequest(t, zl, zapcore.WarnLevel)

	out := buf.String()
	if !strings.Contains(out, `"level":"warn"`) {
		t.Fatalf("expected exact warn level from *zap.Logger, got: %s", out)
	}
	if !strings.Contains(out, `"msg":"http"`) {
		t.Fatalf("expected Ginzap message %q, got: %s", "http", out)
	}
}

func TestGinzap_FilterSensitiveHeadersCanonicalForm(t *testing.T) {
	zl, buf := ginZapBuffer(t)
	r := gin.New()
	r.Use(Ginzap(zl, "", false))
	r.GET("/x", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	// Header.Set canonicalizes the key exactly like the wire format does:
	// "X-API-Key" -> "X-Api-Key", "WWW-Authenticate" -> "Www-Authenticate".
	req.Header.Set("X-API-Key", "secret-key")
	req.Header.Set("WWW-Authenticate", "Basic x")
	req.Header.Set("X-Trace-Id", "trace-keep") // positive control: must stay plaintext
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	out := buf.String()
	for _, want := range []string{
		`"X-Api-Key":["[FILTERED]"]`,
		`"Www-Authenticate":["[FILTERED]"]`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("sensitive header not filtered, missing %s in log: %s", want, out)
		}
	}
	if strings.Contains(out, "secret-key") {
		t.Fatalf("X-API-Key value leaked into log: %s", out)
	}
	if strings.Contains(out, "Basic x") {
		t.Fatalf("WWW-Authenticate value leaked into log: %s", out)
	}
	if !strings.Contains(out, "trace-keep") {
		t.Fatalf("non-sensitive header must stay plaintext: %s", out)
	}
}

// F-15: 超过 2^53 的大整数在 JSON 脱敏后必须逐字保真、且不带引号。
func TestFilterSensitiveDataForJson_BigIntegerPreserved(t *testing.T) {
	body := `{"id":9007199254740993,"password":"top-secret","user":{"uid":18446744073709551615,"password":"nested-secret"}}`
	got := filterSensitiveDataForJson(body)

	if !strings.Contains(got, `"id":9007199254740993`) {
		t.Fatalf("big integer id lost precision or got quoted: %s", got)
	}
	if strings.Contains(got, `9007199254740992`) {
		t.Fatalf("big integer id was rounded to ...992: %s", got)
	}
	if !strings.Contains(got, `"uid":18446744073709551615`) {
		t.Fatalf("nested uint64 lost precision or got quoted: %s", got)
	}
	if strings.Contains(got, "top-secret") || strings.Contains(got, "nested-secret") {
		t.Fatalf("password leaked: %s", got)
	}
	if n := strings.Count(got, `"******"`); n != 2 {
		t.Fatalf("both passwords must be masked, got %d: %s", n, got)
	}
}

// F-20（行为变化）：非法 JSON 不再原样返回（旧断言的"原样返回"正是泄漏路径），
// 改为占位符 [filtered len=N]；空串无脱敏意义、保持原样。
func TestFilterSensitiveDataForJson_InvalidJSONFailClosed(t *testing.T) {
	body := `{"password":"SECRET123","broken":`
	got := filterSensitiveDataForJson(body)
	if strings.Contains(got, "SECRET123") {
		t.Fatalf("invalid JSON must not be echoed back with plaintext password: %s", got)
	}
	if want := fmt.Sprintf("[filtered len=%d]", len(body)); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := filterSensitiveDataForJson(""); got != "" {
		t.Fatalf("empty body must be returned as-is: %q", got)
	}
}

// P3-10: 不可解析 query 输出 [filtered len=N]，不得回显原文。
func TestFilterSensitiveQuery_ParseErrorDowngrades(t *testing.T) {
	raw := "password=%zz&token=q-tok" // 非法转义导致 url.ParseQuery 失败
	got := filterSensitiveQuery(raw)
	if strings.Contains(got, "%zz") || strings.Contains(got, "q-tok") {
		t.Fatalf("unparseable query must not be echoed back: %s", got)
	}
	if want := fmt.Sprintf("[filtered len=%d]", len(raw)); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// P3-10 端到端：中间件日志中不可解析 query 只出现占位符。
func TestGinzap_UnparseableQueryIsRedacted(t *testing.T) {
	zl, buf := ginZapBuffer(t)
	r := gin.New()
	r.Use(Ginzap(zl, "", false))
	r.GET("/q", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/q?password=%zzsecret", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	out := buf.String()
	if strings.Contains(out, "zzsecret") {
		t.Fatalf("unparseable query password leaked: %s", out)
	}
	if !strings.Contains(out, "[filtered len=") {
		t.Fatalf("expected placeholder in query field: %s", out)
	}
}

// P3-11: nil conf 给默认值（TimeFormat 空、DefaultLevel=Info），照常记录。
func TestGinzapWithConfig_NilConfDefaultsToInfo(t *testing.T) {
	zl, buf := ginZapBuffer(t)
	r := gin.New()
	r.Use(GinzapWithConfig(zl, nil))
	r.GET("/x", func(c *gin.Context) { c.Status(http.StatusOK) })

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}

	out := buf.String()
	if !strings.Contains(out, `"level":"info"`) {
		t.Fatalf("nil conf must default to Info level: %s", out)
	}
	if !strings.Contains(out, `"msg":"http"`) {
		t.Fatalf("nil conf must still log the request: %s", out)
	}
}

func TestGinzap_NilLoggerNoPanic(t *testing.T) {
	r := gin.New()
	r.Use(Ginzap(nil, "", false))
	r.GET("/x", func(c *gin.Context) { c.Status(http.StatusOK) })
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
}

func TestCustomRecoveryWithZap_NilLoggerNoPanic(t *testing.T) {
	r := gin.New()
	r.Use(CustomRecoveryWithZap(nil, false, defaultHandleRecovery))
	r.GET("/p", func(c *gin.Context) { panic("boom") })
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/p", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// 批 2 P-1.3 / P-1.4 测试
// ---------------------------------------------------------------------------

// productionEncoderCfg 提供一个无时间字段的稳定 encoder 配置，供新旧编码器
// 逐字节对照使用（两次调用必须得到等价配置）。
func productionEncoderCfg() zapcore.EncoderConfig {
	cfg := zap.NewProductionEncoderConfig()
	cfg.TimeKey = ""
	return cfg
}

// encodeOneField 用给定的 encoder 编码单条日志并返回完整文本（含行尾换行）。
func encodeOneField(t *testing.T, enc zapcore.Encoder, f zap.Field) string {
	t.Helper()
	buf := &bytes.Buffer{}
	l := zap.New(zapcore.NewCore(enc, zapcore.AddSync(buf), zapcore.DebugLevel))
	l.Info("m", f)
	return buf.String()
}

// cloneHeader 深拷贝一份 header，用于"编码不得修改原 header"的对照。
func cloneHeader(h http.Header) http.Header {
	c := make(http.Header, len(h))
	for k, vs := range h {
		c[k] = append([]string(nil), vs...)
	}
	return c
}

// stripJSONWhitespaceOutsideStrings 删除 JSON 文本中字符串字面量之外的空白，
// 用来证明新旧 Console 输出只差 zap 渲染引入的空白字符（见下方 Console 断言说明）。
func stripJSONWhitespaceOutsideStrings(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inStr, escaped := false, false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if inStr {
			b.WriteByte(ch)
			switch {
			case escaped:
				escaped = false
			case ch == '\\':
				escaped = true
			case ch == '"':
				inStr = false
			}
			continue
		}
		switch ch {
		case '"':
			inStr = true
			b.WriteByte(ch)
		case ' ', '\t', '\n', '\r':
			// 字符串外的空白：丢弃
		default:
			b.WriteByte(ch)
		}
	}
	return b.String()
}

// consoleJSONPayload 取出 Console 行中最后一个制表符之后的 JSON 字段对象。
func consoleJSONPayload(line string) string {
	if i := strings.LastIndexByte(line, '\t'); i >= 0 {
		return line[i+1:]
	}
	return line
}

// P-1.3：jsonLogAPI 是包级冻结的 sonic API，必须可被多 goroutine 并发复用。
// 8 个 goroutine 同时脱敏含大整数（>2^53）与嵌套 password 的 body，断言每个输出
// 都正确（大整数逐字保真、两处 password 均已掩码）且不 panic；不断言时序。
//
// -race 兼容：本机 go test -race 不可用（进程退出码 0xc0000139），本用例不依赖
// 竞态检测即可跑通；CI/其它环境可 `go test -race -run TestFilterSensitiveDataForJson_Concurrent ./logger/` 复跑。
func TestFilterSensitiveDataForJson_Concurrent(t *testing.T) {
	const (
		goroutines = 8
		iterations = 50
	)
	errs := make(chan string, goroutines*iterations)
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			body := fmt.Sprintf(
				`{"g":%d,"id":9007199254740993,"password":"top-secret-%d","user":{"uid":18446744073709551615,"password":"nest-secret-%d"}}`,
				id, id, id,
			)
			for i := 0; i < iterations; i++ {
				got := filterSensitiveDataForJson(body)
				switch {
				case !strings.Contains(got, `"id":9007199254740993`):
					errs <- fmt.Sprintf("g%d: big int lost precision: %s", id, got)
				case strings.Contains(got, `9007199254740992`):
					errs <- fmt.Sprintf("g%d: big int rounded: %s", id, got)
				case !strings.Contains(got, `"uid":18446744073709551615`):
					errs <- fmt.Sprintf("g%d: nested uint64 lost: %s", id, got)
				case strings.Contains(got, "top-secret-") || strings.Contains(got, "nest-secret-"):
					errs <- fmt.Sprintf("g%d: password leaked: %s", id, got)
				default:
					if n := strings.Count(got, `"******"`); n != 2 {
						errs <- fmt.Sprintf("g%d: want 2 masked fields, got %d: %s", id, n, got)
					}
				}
			}
		}(g)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// wideHeader33 返回恰好 33 个键的 header（> maxStackHeaderKeys=32，覆盖
// "栈缓冲 → 堆分配" 的阈值边界）。键集刻意混合前导数字/大小写/前缀关系，
// 以暴露排序是否为 json.Marshal 的字节序。其中一个键是敏感头 Authorization。
//
// 注：这里直接写 map（绕过 http.Header.Set 的 canonical 化），正是为了构造
// 大小写混合的键来验证字节序排序——两条路径都遍历同一份 map，语义一致。
func wideHeader33() http.Header {
	h := http.Header{}
	keys := []string{
		"1-alpha", "2-beta",
		"A", "B", "C", "D", "E", "F", "G", "H", "I", "J", "K", "L", "M",
		"N", "O", "P", "Q", "R", "S", "T", "U", "V", "W", "X", "Y", "Z",
		"a", "b", "Beta", "beta", "Authorization",
	}
	for i, k := range keys {
		if k == "Authorization" {
			h[k] = []string{"Bearer secret"}
			continue
		}
		h[k] = []string{fmt.Sprintf("v%02d", i)}
	}
	return h
}

// headerEncodeCases 提供新旧路径对照所需的头数据覆盖：
// 空 / 无敏感 / 单个敏感 / 多个敏感 / 多值 / 含特殊字符 / 33 键（>栈缓冲阈值）。
func headerEncodeCases() []struct {
	name string
	hdr  http.Header
} {
	newHeader := func(kv ...string) http.Header {
		h := http.Header{}
		for i := 0; i < len(kv); i += 2 {
			h.Add(kv[i], kv[i+1])
		}
		return h
	}
	return []struct {
		name string
		hdr  http.Header
	}{
		{"empty", http.Header{}},
		{"no-sensitive", newHeader("Accept", "application/json", "X-Trace-Id", "trace-keep")},
		{"one-sensitive", newHeader("Authorization", "Bearer secret", "X-Trace-Id", "trace-keep")},
		{"multi-sensitive", newHeader(
			"Authorization", "Bearer secret",
			"Cookie", "sid=secret",
			"X-Api-Key", "secret-key",
			"Set-Cookie", "sid=secret",
			"X-Trace-Id", "trace-keep",
		)},
		{"multi-value", newHeader(
			"X-Forwarded-For", "203.0.113.7",
			"X-Forwarded-For", "198.51.100.9",
			"Authorization", "Bearer secret",
		)},
		{"special-chars", newHeader("Referer", "https://x.com/?a=1&b=2", "X-Note", "a<b>c")},
		{"many-keys-33-over-stack-threshold", wideHeader33()},
	}
}

// P-1.4（a）+（b）：同一份 header 分别经旧路径（filterSensitiveHeaders + zap.Any）
// 与新路径（headerLogObject + zap.Object）编码，JSON 与 Console 两种 encoder 各一轮；
// 同时断言编码前后原 header 的键、值切片完全不变。
//
// 【Console 无法逐字节一致——机制说明（已被主管确认接受，属 zap 渲染层固有行为）】
//   - NewConsoleEncoder(cfg) == consoleEncoder{newJSONEncoder(cfg, true)}，其内部 JSON
//     子编码器 spaced=true，故对象/数组渲染为 `"k": v`、`[a, b]`（键值间 ": "、元素间 ", "）；
//   - 旧路径 zap.Any(map) 是 ReflectType，经 AddReflected 直接嵌入 json.Marshal 的原始
//     紧凑字节（且键为字典序），不受 spaced 影响；
//   - 因此两条路径在 Console 下的差异**仅限空白**；任何 ObjectMarshaler 实现都无法逐字节
//     复刻 AddReflected 的原始字节（除非放弃 zap.Object，即放弃 P-1.4 的实现约束）。
//   - 附带说明：Console 下同行其它字段本就是 spaced 风格，headers 旧形态反而是该行唯一的
//     紧凑字段，改造后风格更一致。
//
// 因此 Console 断言为：
//  1) 剥离字符串字面量外空白后逐字节一致（证明差异仅限于空白）；
//  2) json.Unmarshal 后结构 DeepEqual（证明对象/数组形态与取值完全一致）。
// JSON encoder 则直接断言逐字节一致。
func TestHeaderLogObject_MatchesLegacyEncoding(t *testing.T) {
	encoders := []struct {
		name string
		make func() zapcore.Encoder
	}{
		{"json", func() zapcore.Encoder { return zapcore.NewJSONEncoder(productionEncoderCfg()) }},
		{"console", func() zapcore.Encoder { return zapcore.NewConsoleEncoder(productionEncoderCfg()) }},
	}
	for _, ec := range encoders {
		for _, tc := range headerEncodeCases() {
			t.Run(ec.name+"/"+tc.name, func(t *testing.T) {
				before := cloneHeader(tc.hdr)

				oldOut := encodeOneField(t, ec.make(), zap.Any("headers", filterSensitiveHeaders(tc.hdr)))
				newOut := encodeOneField(t, ec.make(), zap.Object("headers", headerLogObject(tc.hdr)))

				if !reflect.DeepEqual(tc.hdr, before) {
					t.Fatalf("encoding must not mutate the source header:\n got %#v\nwant %#v", tc.hdr, before)
				}

				if ec.name == "json" {
					if oldOut != newOut {
						t.Fatalf("JSON output must be byte-identical\nold=%q\nnew=%q", oldOut, newOut)
					}
					return
				}

				if stripJSONWhitespaceOutsideStrings(oldOut) != stripJSONWhitespaceOutsideStrings(newOut) {
					t.Fatalf("console output must differ by whitespace only\nold=%q\nnew=%q", oldOut, newOut)
				}
				var oldV, newV map[string]any
				if err := json.Unmarshal([]byte(consoleJSONPayload(oldOut)), &oldV); err != nil {
					t.Fatalf("legacy console output is not valid JSON: %v\n%s", err, oldOut)
				}
				if err := json.Unmarshal([]byte(consoleJSONPayload(newOut)), &newV); err != nil {
					t.Fatalf("new console output is not valid JSON: %v\n%s", err, newOut)
				}
				if !reflect.DeepEqual(oldV, newV) {
					t.Fatalf("console structural mismatch\nold=%#v\nnew=%#v", oldV, newV)
				}
			})
		}
	}
}

// P-1.4（b）：显式断言编码不修改原 header（含敏感头与多值头），且重复编码结果稳定
// （filteredHeaderValue 为包级只读占位，不会被写入）。
func TestHeaderLogObject_DoesNotMutateHeader(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer secret")
	h.Add("X-Forwarded-For", "203.0.113.7")
	h.Add("X-Forwarded-For", "198.51.100.9")
	h.Set("X-Trace-Id", "trace-keep")
	before := cloneHeader(h)

	first := encodeOneField(t, zapcore.NewJSONEncoder(productionEncoderCfg()), zap.Object("headers", headerLogObject(h)))
	if !reflect.DeepEqual(h, before) {
		t.Fatalf("first encode mutated header:\n got %#v\nwant %#v", h, before)
	}
	second := encodeOneField(t, zapcore.NewJSONEncoder(productionEncoderCfg()), zap.Object("headers", headerLogObject(h)))
	if !reflect.DeepEqual(h, before) {
		t.Fatalf("second encode mutated header:\n got %#v\nwant %#v", h, before)
	}
	if first != second {
		t.Fatalf("repeated encoding must be stable\nfirst=%q\nsecond=%q", first, second)
	}
	if !strings.Contains(first, `"Authorization":["[FILTERED]"]`) {
		t.Fatalf("sensitive header must be masked as [FILTERED]: %s", first)
	}
	if !strings.Contains(first, `"X-Forwarded-For":["203.0.113.7","198.51.100.9"]`) {
		t.Fatalf("multi-value header must stay intact: %s", first)
	}
}

// orderedJSONKeys 按出现顺序返回 payload 中 "headers" 对象的键序列。
// JSON 对象解码进 map 会丢序，故走 json.Decoder 的 token 流。
func orderedJSONKeys(t *testing.T, payload string) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(payload))
	for {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("scan for \"headers\" key: %v (payload=%s)", err, payload)
		}
		s, ok := tok.(string)
		if !ok || s != "headers" {
			continue
		}
		open, err := dec.Token()
		if err != nil || open != json.Delim('{') {
			t.Fatalf("headers value must be a JSON object: tok=%v err=%v (payload=%s)", open, err, payload)
		}
		var keys []string
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				t.Fatalf("read header key: %v", err)
			}
			keys = append(keys, kt.(string))
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				t.Fatalf("skip header value: %v", err)
			}
		}
		return keys
	}
}

// P-1.4：验证键排序语义与旧路径 json.Marshal 一致（字符串字节序比较），
// 并覆盖 >maxStackHeaderKeys(32) 的 33 键用例（"栈缓冲 → 堆分配"阈值边界）。
func TestHeaderLogObject_SortMatchesJSONMarshalByteOrder(t *testing.T) {
	h := wideHeader33()
	// 字节序期望（独立于实现的硬编码，不与 slices.Sort 共用逻辑）：
	// 前导数字 < 大写 < 小写；前缀键排在其扩展键之前；"Authorization" 紧跟 "A"。
	want := []string{
		"1-alpha", "2-beta",
		"A", "Authorization", "B", "Beta", "C", "D", "E", "F", "G", "H", "I",
		"J", "K", "L", "M", "N", "O", "P", "Q", "R", "S", "T", "U", "V", "W",
		"X", "Y", "Z", "a", "b", "beta",
	}

	oldOut := encodeOneField(t, zapcore.NewJSONEncoder(productionEncoderCfg()), zap.Any("headers", filterSensitiveHeaders(h)))
	newOut := encodeOneField(t, zapcore.NewJSONEncoder(productionEncoderCfg()), zap.Object("headers", headerLogObject(h)))

	if oldOut != newOut {
		t.Fatalf("33-key JSON output must be byte-identical\nold=%s\nnew=%s", oldOut, newOut)
	}
	got := orderedJSONKeys(t, newOut)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("key order must match json.Marshal byte order\ngot  (%d) %v\nwant (%d) %v", len(got), got, len(want), want)
	}
	if strings.Contains(newOut, "Bearer secret") {
		t.Fatalf("sensitive header leaked in 33-key payload: %s", newOut)
	}
	if !strings.Contains(newOut, `"Authorization":["[FILTERED]"]`) {
		t.Fatalf("Authorization must be masked in 33-key payload: %s", newOut)
	}
}

// P-1.4：isSensitiveHeader 只认 sensitiveHeaders 集合中的 canonical 拼写，
// 与旧 filterSensitiveHeaders 的判定保持一致（非 canonical 拼写不命中）。
func TestIsSensitiveHeader_CanonicalOnly(t *testing.T) {
	for _, name := range []string{"Authorization", "Cookie", "Set-Cookie", "X-Api-Key", "Proxy-Authorization", "Www-Authenticate"} {
		if !isSensitiveHeader(name) {
			t.Errorf("%q must be sensitive", name)
		}
	}
	for _, name := range []string{"X-API-Key", "WWW-Authenticate", "authorization", "X-Trace-Id", ""} {
		if isSensitiveHeader(name) {
			t.Errorf("%q must NOT be sensitive (non-canonical/plain)", name)
		}
	}
}
