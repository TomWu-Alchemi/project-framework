package logger

// F-20 / F-21 验收测试：脱敏路径 fail closed、panic 恢复日志脱敏。

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/TomWu-Alchemi/project-framework/internal/logutil"
	"github.com/gin-gonic/gin"
)

// wrappedOpError 构造一个外层包装过的 *net.OpError（broken pipe），
// 直接类型断言无法识别，errors.As 才能命中（F-30 的识别面）。
func wrappedOpError() error {
	return fmt.Errorf("wrapped broken pipe: %w", &net.OpError{
		Op:  "write",
		Err: &os.SyscallError{Syscall: "write", Err: errors.New("broken pipe")},
	})
}

// F-20 路径 B：JSON body 超过截断上限、password 在前 256KiB 内 →
// 截断后的 JSON 必然非法，日志不得回显含明文密码的原文前缀。
func TestGinzap_TruncatedJSONBodyWithEarlyPasswordFailClosed(t *testing.T) {
	zl, buf := ginZapBuffer(t)
	r := gin.New()
	r.Use(Ginzap(zl, "", false))
	r.POST("/x", func(c *gin.Context) { c.Status(http.StatusOK) })

	body := `{"password":"TRUNCATED-SECRET","padding":"` +
		strings.Repeat("x", logutil.MaxLogBytes+2048) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	out := buf.String()
	if strings.Contains(out, "TRUNCATED-SECRET") {
		t.Fatalf("truncated JSON body leaked plaintext password: %s", out)
	}
	if !strings.Contains(out, "[filtered len=") {
		t.Fatalf("expected placeholder for unparseable truncated body: %s", out)
	}
}

// F-20 路径 A：客户端发送坏 JSON（未截断，本身非法）→ 同样 fail closed。
func TestGinzap_InvalidJSONBodyFailClosed(t *testing.T) {
	zl, buf := ginZapBuffer(t)
	r := gin.New()
	r.Use(Ginzap(zl, "", false))
	r.POST("/x", func(c *gin.Context) { c.Status(http.StatusOK) })

	body := `{"user":"a","password":"BADJSON-SECRET","broken":`
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	out := buf.String()
	if strings.Contains(out, "BADJSON-SECRET") {
		t.Fatalf("invalid JSON body leaked plaintext password: %s", out)
	}
	if !strings.Contains(out, "[filtered len=") {
		t.Fatalf("expected placeholder for unparseable body: %s", out)
	}
}

// F-20：非明文 Content-Encoding（gzip 等）→ 压缩字节不落日志，占位。
func TestGinzap_ContentEncodingBodyFailClosed(t *testing.T) {
	zl, buf := ginZapBuffer(t)
	r := gin.New()
	r.Use(Ginzap(zl, "", false))
	r.POST("/x", func(c *gin.Context) { c.Status(http.StatusOK) })

	body := `{"password":"GZIP-SECRET"}`
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	out := buf.String()
	if strings.Contains(out, "GZIP-SECRET") {
		t.Fatalf("encoded body must not be logged in plaintext: %s", out)
	}
	if !strings.Contains(out, "[filtered len=") {
		t.Fatalf("expected placeholder for encoded body: %s", out)
	}
}

// F-20：截断的 form body（截断点可能在 password 值中间）→ 占位，不走前缀脱敏。
func TestGinzap_TruncatedFormBodyFailClosed(t *testing.T) {
	zl, buf := ginZapBuffer(t)
	r := gin.New()
	r.Use(Ginzap(zl, "", false))
	r.POST("/x", func(c *gin.Context) { c.Status(http.StatusOK) })

	body := "password=FORM-SECRET&padding=" + strings.Repeat("x", logutil.MaxLogBytes+2048)
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	out := buf.String()
	if strings.Contains(out, "FORM-SECRET") {
		t.Fatalf("truncated form body leaked plaintext password: %s", out)
	}
	if !strings.Contains(out, "[filtered len=") {
		t.Fatalf("expected placeholder for truncated form body: %s", out)
	}
}

// F-20 对照组：合法 JSON / 完整 form 的脱敏行为不受影响（既有用例已覆盖
// 合法路径，这里锁一条端到端正向用例，防止 fail closed 误伤正常请求日志）。
func TestGinzap_ValidJSONBodyStillLogged(t *testing.T) {
	zl, buf := ginZapBuffer(t)
	r := gin.New()
	r.Use(Ginzap(zl, "", false))
	r.POST("/x", func(c *gin.Context) { c.Status(http.StatusOK) })

	body := `{"password":"VALID-SECRET","user":"alice"}`
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	out := buf.String()
	if strings.Contains(out, "VALID-SECRET") {
		t.Fatalf("valid JSON password leaked: %s", out)
	}
	if !strings.Contains(out, "alice") {
		t.Fatalf("non-sensitive field must still be logged: %s", out)
	}
}

// F-21：panic 恢复日志的 request 字段不得包含敏感头与未脱敏 query。
func TestCustomRecoveryWithZap_SanitizedDump(t *testing.T) {
	zl, buf := ginZapBuffer(t)
	r := gin.New()
	r.Use(RecoveryWithZap(zl, false))
	r.GET("/p", func(c *gin.Context) { panic("kaboom") })

	req := httptest.NewRequest(http.MethodGet, "/p?password=PANIC-SECRET&token=panic-tok", nil)
	req.Header.Set("Authorization", "Bearer PANIC-AUTH")
	req.Header.Set("X-API-Key", "PANIC-API-KEY")
	req.Header.Set("X-Trace-Id", "trace-keep")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d", w.Code)
	}
	out := buf.String()
	for _, secret := range []string{"PANIC-AUTH", "PANIC-API-KEY", "PANIC-SECRET"} {
		if strings.Contains(out, secret) {
			t.Fatalf("panic log leaked %q: %s", secret, out)
		}
	}
	if !strings.Contains(out, "[FILTERED]") {
		t.Fatalf("sensitive headers must show [FILTERED]: %s", out)
	}
	// filterSensitiveQuery 经 url.Values.Encode() 输出，掩码 ****** 被编码为 %2A*
	if !strings.Contains(out, "password=%2A%2A") {
		t.Fatalf("query password must be masked: %s", out)
	}
	// 非敏感信息保持可见（排障价值不受影响）
	if !strings.Contains(out, "trace-keep") || !strings.Contains(out, "panic-tok") {
		t.Fatalf("non-sensitive header/query must stay visible: %s", out)
	}
}

// F-30：包装过的 *net.OpError（broken pipe）也能被识别。broken pipe 分支只
// Abort 不写状态码（连接已断，写状态无意义），因此断言落在"panic 被记录、
// 进程不崩"上，而非响应码。
func TestCustomRecoveryWithZap_WrappedBrokenPipe(t *testing.T) {
	zl, buf := ginZapBuffer(t)
	r := gin.New()
	r.Use(RecoveryWithZap(zl, false))
	r.GET("/p", func(c *gin.Context) {
		panic(wrappedOpError())
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/p", nil))
	if !strings.Contains(buf.String(), "wrapped broken pipe") {
		t.Fatalf("panic must be logged: %s", buf.String())
	}
}

// ---------------------------------------------------------------------------
// 批 2 P-1.6 测试：截断 JSON 直接占位（快速路径）
// ---------------------------------------------------------------------------

// P-1.6（a）可区分用例：构造 >256KiB 的 JSON body =「合法 JSON（含 password）+
// 大量尾部空白填充」。截断后的前 MaxLogBytes 字节仍是合法 JSON，因此：
//   - 旧实现：解析成功 → 输出脱敏后的 JSON（特征串 `"password":"******"`）；
//   - 新实现：bodyTruncatedAtRead 命中 → 直接输出 [filtered len=N]，不进入解析。
//
// 断言占位出现、旧特征串不出现，即为"快速路径确实生效"的证据。
func TestGinzap_TruncatedValidJSONFastPath(t *testing.T) {
	zl, buf := ginZapBuffer(t)
	r := gin.New()
	r.Use(Ginzap(zl, "", false))
	r.POST("/x", func(c *gin.Context) { c.Status(http.StatusOK) })

	head := `{"password":"FASTPATH-SECRET","note":"ok"}`
	body := head + strings.Repeat(" ", logutil.MaxLogBytes-len(head)+2048)
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	out := buf.String()
	if strings.Contains(out, "FASTPATH-SECRET") {
		t.Fatalf("truncated JSON leaked plaintext password: %s", out)
	}
	// 占位 N 是日志口径的原始长度：httptest.NewRequest 对 *strings.Reader 精确设置
	// ContentLength == len(body)，即 256KiB+2048，而不再是截断后的 256KiB 常量。
	if want := fmt.Sprintf("[filtered len=%d]", len(body)); !strings.Contains(out, want) {
		t.Fatalf("truncated JSON must emit %q without parsing, got: %s", want, out)
	}
	if strings.Contains(out, `"password":"******"`) {
		t.Fatalf("JSON branch took parse+mask path instead of the truncated fast path: %s", out)
	}
}

// P-1.6（c）：form 与 json 两条截断路径的占位格式逐字一致（[filtered len=N]）。
func TestGinzap_TruncatedPlaceholderFormatConsistent(t *testing.T) {
	const pad = 2048

	placeholderOf := func(out string) string {
		i := strings.Index(out, "[filtered len=")
		if i < 0 {
			return ""
		}
		j := strings.IndexByte(out[i:], ']')
		if j < 0 {
			return ""
		}
		return out[i : i+j+1]
	}
	run := func(t *testing.T, contentType, body string) string {
		t.Helper()
		zl, buf := ginZapBuffer(t)
		r := gin.New()
		r.Use(Ginzap(zl, "", false))
		r.POST("/x", func(c *gin.Context) { c.Status(http.StatusOK) })
		req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
		req.Header.Set("Content-Type", contentType)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return buf.String()
	}

	formHead := "password=CONSIST-SECRET&padding="
	formBody := formHead + strings.Repeat("x", logutil.MaxLogBytes-len(formHead)+pad)
	jsonHead := `{"password":"CONSIST-SECRET","pad":"`
	jsonBody := jsonHead + strings.Repeat("x", logutil.MaxLogBytes-len(jsonHead)+pad)

	// 占位 N 取各自 body 的原始长度（= MaxLogBytes+pad）：httptest 对 *strings.Reader
	// 设 ContentLength == len(body)，截断后 N 仍能还原，不再恒为 MaxLogBytes。
	wantForm := fmt.Sprintf("[filtered len=%d]", len(formBody))
	wantJSON := fmt.Sprintf("[filtered len=%d]", len(jsonBody))

	formOut := run(t, "application/x-www-form-urlencoded", formBody)
	if strings.Contains(formOut, "CONSIST-SECRET") {
		t.Fatalf("form body leaked password: %s", formOut)
	}
	if got := placeholderOf(formOut); got != wantForm {
		t.Fatalf("form placeholder = %q, want %q\n%s", got, wantForm, formOut)
	}

	jsonOut := run(t, "application/json", jsonBody)
	if strings.Contains(jsonOut, "CONSIST-SECRET") {
		t.Fatalf("json body leaked password: %s", jsonOut)
	}
	if got := placeholderOf(jsonOut); got != wantJSON {
		t.Fatalf("json placeholder = %q, want %q\n%s", got, wantJSON, jsonOut)
	}
	if placeholderOf(formOut) != placeholderOf(jsonOut) {
		t.Fatalf("form/json placeholder must be identical: form=%q json=%q", placeholderOf(formOut), placeholderOf(jsonOut))
	}
}

// ---------------------------------------------------------------------------
// F-20 批 3：Content-Type 缺失/非法 → 占位；未识别媒体类型 → 有意保留原文
// ---------------------------------------------------------------------------

// F-20：Content-Type 缺失时没有媒体类型可判定脱敏策略（ParseMediaType("") 返回
// "mime: no media type"）；修复前该路径既不占位也不脱敏，含 password 的原文直接入库。
func TestGinzap_MissingContentTypeBodyFailClosed(t *testing.T) {
	zl, buf := ginZapBuffer(t)
	r := gin.New()
	r.Use(Ginzap(zl, "", false))
	r.POST("/x", func(c *gin.Context) { c.Status(http.StatusOK) })

	body := `{"password":"NOCT-SECRET","user":"alice"}`
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
	// 刻意不设置 Content-Type
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	out := buf.String()
	if strings.Contains(out, "NOCT-SECRET") {
		t.Fatalf("body without Content-Type leaked plaintext password: %s", out)
	}
	if want := fmt.Sprintf("[filtered len=%d]", len(body)); !strings.Contains(out, want) {
		t.Fatalf("expected placeholder %q for body without Content-Type, got: %s", want, out)
	}
}

// F-20：Content-Type 非法（ParseMediaType 报错）同样占位。注意 "json" 这类漏斜杠的值
// err == nil、不属于本用例覆盖面（见下方边界锁定用例）。
func TestGinzap_InvalidContentTypeBodyFailClosed(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
	}{
		{"invalid-token", "not a media type"}, // expected slash after first token
		{"missing-subtype", "application/"},   // expected token after slash
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			zl, buf := ginZapBuffer(t)
			r := gin.New()
			r.Use(Ginzap(zl, "", false))
			r.POST("/x", func(c *gin.Context) { c.Status(http.StatusOK) })

			body := `{"password":"BADCT-SECRET","user":"alice"}`
			req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
			req.Header.Set("Content-Type", tc.contentType)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			out := buf.String()
			if strings.Contains(out, "BADCT-SECRET") {
				t.Fatalf("invalid Content-Type %q leaked plaintext password: %s", tc.contentType, out)
			}
			if want := fmt.Sprintf("[filtered len=%d]", len(body)); !strings.Contains(out, want) {
				t.Fatalf("expected placeholder %q for invalid Content-Type %q, got: %s", want, tc.contentType, out)
			}
		})
	}
}

// 【边界锁定 · 有意行为固化】未识别媒体类型保持原文记录：主管确认、项目负责人拍板的
// 方案 B 取舍——保留非结构化 body 的排障信息。已知代价：multipart 正常表单提交
// （Content-Type: multipart/form-data）中的 password 字段仍会明文入库，作为后续独立
// 事项评估。未来若改为占位，必须同步修改本用例并重新评审。
//
// visible 用不含 & " < > 的片段，避免编码器转义差异带来的断言噪音；断言"能看到
// password 明文"正是本取舍的表现，不是缺陷。
func TestGinzap_UnrecognizedMediaTypeKeepsPlaintext(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		body        string
		visible     string
	}{
		{"text-plain", "text/plain", "password=PLAIN-SECRET", "password=PLAIN-SECRET"},
		{"multipart-form-data", "multipart/form-data; boundary=XyZ123", "password=MULTIPART-SECRET", "password=MULTIPART-SECRET"},
		// 漏斜杠的 "json"：ParseMediaType 成功（err == nil）但媒体类型未识别 → 保持原文。
		{"no-slash-json", "json", `{"password":"NOSLASH-SECRET"}`, "NOSLASH-SECRET"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			zl, buf := ginZapBuffer(t)
			r := gin.New()
			r.Use(Ginzap(zl, "", false))
			r.POST("/x", func(c *gin.Context) { c.Status(http.StatusOK) })

			req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", tc.contentType)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			out := buf.String()
			if strings.Contains(out, "[filtered len=") {
				t.Fatalf("unrecognized media type %q must keep the body as-is: %s", tc.contentType, out)
			}
			if !strings.Contains(out, tc.visible) {
				t.Fatalf("unrecognized media type %q must keep original text %q visible: %s", tc.contentType, tc.visible, out)
			}
		})
	}
}

// F-20 回归：空 body（无 Content-Type 的 GET）不得凭空产生 body 字段或占位符。
func TestGinzap_EmptyBodyEmitsNoBodyField(t *testing.T) {
	zl, buf := ginZapBuffer(t)
	r := gin.New()
	r.Use(Ginzap(zl, "", false))
	r.GET("/x", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	out := buf.String()
	for _, forbidden := range []string{"[filtered len=", `"body":`, "body_truncated", "body_size"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("empty body must not produce %q: %s", forbidden, out)
		}
	}
}

// F-20 + body_size 修正：chunked（ContentLength 未知，显式 -1）+ 无 Content-Type +
// 大 body：占位 N 与 body_size 都必须是"已读到的下界"（MaxLogBytes），而不是占位符
// 自身长度——否则 body_truncated=true 与 body_size≈22 自相矛盾。
func TestGinzap_TruncatedUnknownLengthBodySizeIsBytesRead(t *testing.T) {
	zl, buf := ginZapBuffer(t)
	r := gin.New()
	r.Use(Ginzap(zl, "", false))
	r.POST("/x", func(c *gin.Context) { c.Status(http.StatusOK) })

	body := `{"password":"CHUNKED-SECRET","pad":"` + strings.Repeat("x", logutil.MaxLogBytes+2048) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
	req.ContentLength = -1 // 模拟 chunked：长度未知
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	out := buf.String()
	if strings.Contains(out, "CHUNKED-SECRET") {
		t.Fatalf("truncated body leaked plaintext password: %s", out)
	}
	if want := fmt.Sprintf("[filtered len=%d]", logutil.MaxLogBytes); !strings.Contains(out, want) {
		t.Fatalf("placeholder must use the bytes actually read (%d), got: %s", logutil.MaxLogBytes, out)
	}
	if !strings.Contains(out, `"body_truncated":true`) {
		t.Fatalf("expected body_truncated=true: %s", out)
	}
	if want := fmt.Sprintf(`"body_size":%d`, logutil.MaxLogBytes); !strings.Contains(out, want) {
		t.Fatalf("body_size must be the bytes actually read (%d), not the placeholder length: %s", logutil.MaxLogBytes, out)
	}
}
