package logger

// F-20 / F-21 验收测试：脱敏路径 fail closed、panic 恢复日志脱敏。

import (
	"encoding/json"
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

// loggedBodyField 从单行访问日志中取出 body 字段的原始（已反序列化）文本，
// 供需要逐字断言脱敏结果的用例使用——直接对日志行做子串匹配会被 JSON 转义干扰。
func loggedBodyField(t *testing.T, out string) string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &m); err != nil {
		t.Fatalf("access log line is not valid JSON: %v\n%s", err, out)
	}
	s, _ := m["body"].(string)
	return s
}

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

// ---------------------------------------------------------------------------
// 【策略变更（已拍板）】未识别媒体类型保持原文，但写入日志前统一做“未掩码 password
// 兜底检测”，命中即占位。原先“未识别类型保持明文”的用例已推翻。
// ---------------------------------------------------------------------------

// 验收 1/2/3/4/8：未识别媒体类型（或声明类型与实际 body 不符）时，只要 body 里存在
// 未掩码的 password 值，就必须占位且不得出现明文；body_size 恒为原始 body 长度。
func TestGinzap_UnmaskedPasswordIsRedactedAcrossMediaTypes(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		body        string
		secret      string
	}{
		{
			// 验收 1：text/plain 声明 + JSON body。
			name:        "text-plain-json-body",
			contentType: "text/plain",
			body:        `{"password":"PLAINTEXT-SECRET","user":"alice"}`,
			secret:      "PLAINTEXT-SECRET",
		},
		{
			// 验收 2：multipart/form-data，含 name="password" 字段（该路径不脱敏）。
			name:        "multipart-form-data",
			contentType: "multipart/form-data; boundary=XyZ123",
			body:        "--XyZ123\r\nContent-Disposition: form-data; name=\"password\"\r\n\r\nMULTIPART-SECRET\r\n--XyZ123--\r\n",
			secret:      "MULTIPART-SECRET",
		},
		{
			// 验收 3：漏斜杠的 "json"——ParseMediaType 成功（err == nil）但媒体类型未识别。
			name:        "no-slash-json",
			contentType: "json",
			body:        `{"password":"NOSLASH-SECRET"}`,
			secret:      "NOSLASH-SECRET",
		},
		{
			// 验收 4：声明 form-urlencoded 但实际 body 是 JSON——filterSensitiveData
			// 切不出 k=v，会原样输出，靠兜底断言占位。
			name:        "form-declared-json-body",
			contentType: "application/x-www-form-urlencoded",
			body:        `{"password":"MISMATCH-SECRET","user":"alice"}`,
			secret:      "MISMATCH-SECRET",
		},
		{
			// 补充：未识别类型 + form 形态。
			name:        "octet-stream-form-body",
			contentType: "application/octet-stream",
			body:        "user=alice&password=OCTET-SECRET",
			secret:      "OCTET-SECRET",
		},
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
			if strings.Contains(out, tc.secret) {
				t.Fatalf("%s (%s) leaked plaintext password: %s", tc.name, tc.contentType, out)
			}
			if want := fmt.Sprintf("[filtered len=%d]", len(tc.body)); !strings.Contains(out, want) {
				t.Fatalf("%s (%s) must emit placeholder %q, got: %s", tc.name, tc.contentType, want, out)
			}
			// 验收 8：body_size 恒为原始 body 长度（占位场景等于占位符 N）。
			if want := fmt.Sprintf(`"body_size":%d`, len(tc.body)); !strings.Contains(out, want) {
				t.Fatalf("%s: body_size must be original body length (%d), got: %s", tc.name, len(tc.body), out)
			}
		})
	}
}

// 验收 5【“禁止按内容类型粗放占位”红线保护用例之一】：text/plain 不带未掩码 password
// 时必须逐字保留原文、不得占位；含 password 一词而下一位既非 '"' 也非 '='（无值可判定）
// 同样不得占位。断言取 body 字段的完整还原值 == 原始 body（而非片段包含）。
func TestGinzap_PasswordlessTextPlainKeepsPlaintext(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"plain-text", "hello world"},
		{"word-only-password", "please reset your password as soon as possible"},
		{"json-password-policy", `{"password_policy":"must-be-long"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			zl, buf := ginZapBuffer(t)
			r := gin.New()
			r.Use(Ginzap(zl, "", false))
			r.POST("/x", func(c *gin.Context) { c.Status(http.StatusOK) })

			req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "text/plain")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			out := buf.String()
			if strings.Contains(out, "[filtered len=") {
				t.Fatalf("%s: passwordless text/plain must NOT be placeholdered: %s", tc.name, out)
			}
			if logged := loggedBodyField(t, out); logged != tc.body {
				t.Fatalf("%s: body must be kept verbatim\n got %q\nwant %q", tc.name, logged, tc.body)
			}
		})
	}
}

// 验收 10【“禁止按内容类型粗放占位”红线保护用例之二】：multipart/form-data 不含 password
// 字段时必须逐字保留原文、不得占位。与 TestGinzap_UnmaskedPasswordIsRedactedAcrossMediaTypes
// 的 multipart-form-data 子用例配对——同样是 multipart，是否占位只取决于内容中是否存在
// 未掩码 password，与媒体类型无关（不存在“是 multipart 就占位”的粗放规则）。
func TestGinzap_PasswordlessMultipartKeepsPlaintext(t *testing.T) {
	const body = "------boundary\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a.xlsx\"\r\n\r\nDATA\r\n------boundary--\r\n"

	zl, buf := ginZapBuffer(t)
	r := gin.New()
	r.Use(Ginzap(zl, "", false))
	r.POST("/x", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=------boundary")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	out := buf.String()
	if strings.Contains(out, "[filtered len=") {
		t.Fatalf("passwordless multipart must NOT be placeholdered: %s", out)
	}
	if logged := loggedBodyField(t, out); logged != body {
		t.Fatalf("passwordless multipart body must be kept verbatim\n got %q\nwant %q", logged, body)
	}
	if !strings.Contains(out, "a.xlsx") {
		t.Fatalf("non-sensitive multipart content must stay visible: %s", out)
	}
}

// 主管审查（第一轮）补漏：四类此前漏判的未掩码形态，各补一条端到端用例。每例断言
// 不出现明文、出现占位符、且 body_size 为原始 body 长度。
func TestGinzap_ExtendedPasswordFormsAreRedacted(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		body        string
		secret      string
	}{
		{
			// 修 A-1：multipart 参数 '=' 两侧带空白（合法 MIME 参数语法，标准解析器
			// 能取出该字段，业务收到密码 → 必须占位）。
			name:        "multipart-name-equals-with-ws",
			contentType: "multipart/form-data; boundary=b",
			body:        "--b\r\nContent-Disposition: form-data; name = \"password\"\r\n\r\nSECRET\r\n--b--\r\n",
			secret:      "SECRET",
		},
		{
			// 修 A-2：multipart 参数名单引号（合法 MIME 参数语法）。
			name:        "multipart-name-single-quote",
			contentType: "multipart/form-data; boundary=b",
			body:        "--b\r\nContent-Disposition: form-data; name='password'\r\n\r\nSECRET\r\n--b--\r\n",
			secret:      "SECRET",
		},
		{
			// 修 B-1：对象字面量无引号 key（前端畸形 body 常见形态）。
			name:        "unquoted-key-object-literal",
			contentType: "text/plain",
			body:        `{password:"SECRET"}`,
			secret:      "SECRET",
		},
		{
			// 修 B-2：'password: 值' 冒号分隔（YAML / 配置 / 头风格）。
			name:        "colon-separated-plain",
			contentType: "text/plain",
			body:        "password: SECRET",
			secret:      "SECRET",
		},
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
			if strings.Contains(out, tc.secret) {
				t.Fatalf("%s leaked plaintext password: %s", tc.name, out)
			}
			if want := fmt.Sprintf("[filtered len=%d]", len(tc.body)); !strings.Contains(out, want) {
				t.Fatalf("%s must emit placeholder %q, got: %s", tc.name, want, out)
			}
			if want := fmt.Sprintf(`"body_size":%d`, len(tc.body)); !strings.Contains(out, want) {
				t.Fatalf("%s: body_size must be original body length (%d), got: %s", tc.name, len(tc.body), out)
			}
		})
	}
}

// 验收 6/7/8：正常 JSON / form 路径仍输出掩码字面量、不占位，body_size 为原始长度
// （而不是脱敏后长度）。
func TestGinzap_MaskedBodyStillLoggedWithoutPlaceholder(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		body        string
		masked      string
		keep        string
	}{
		{
			name:        "valid-json",
			contentType: "application/json",
			body:        `{"password":"VALID-SECRET","user":"alice"}`,
			masked:      `"password":"******"`,
			keep:        "alice",
		},
		{
			name:        "valid-form",
			contentType: "application/x-www-form-urlencoded",
			body:        "user=alice&password=FORM-SECRET",
			masked:      "password=******",
			keep:        "user=alice",
		},
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
				t.Fatalf("%s must NOT be placeholdered: %s", tc.name, out)
			}
			logged := loggedBodyField(t, out)
			if !strings.Contains(logged, tc.masked) {
				t.Fatalf("%s must contain masked literal %q, got body %q (log=%s)", tc.name, tc.masked, logged, out)
			}
			if !strings.Contains(logged, tc.keep) {
				t.Fatalf("%s must keep non-sensitive field %q, got body %q (log=%s)", tc.name, tc.keep, logged, out)
			}
			// 验收 8：body_size 为原始 body 长度（若沿用脱敏后长度会与 -- 不同）。
			if want := fmt.Sprintf(`"body_size":%d`, len(tc.body)); !strings.Contains(out, want) {
				t.Fatalf("%s: body_size must be original body length (%d), not masked length, got: %s", tc.name, len(tc.body), out)
			}
		})
	}
}

// hasUnmaskedPassword 判据单元测试：锁定“命中即占位、已掩码/无值不占位、无语义不误判”。
func TestHasUnmaskedPassword(t *testing.T) {
	leaked := []string{
		`{"password":"SECRET"}`,
		`{"Password":"SECRET"}`,
		`{"a":{"b":[{"password":"SECRET"}]}}`,
		`{"password":"a\"b"}`, // 值含转义引号
		`{"password":"SECRET`, // 截断未闭合
		`{"password":123456}`, // 非字符串值（fail closed）
		"password=SECRET",
		"user=alice&Password=SECRET",
		"password=SECRET&token=t",
		"--b\r\nContent-Disposition: form-data; name=\"password\"\r\n\r\nSECRET\r\n--b--\r\n",
		`name="password"`,
		// 修 A：multipart 参数 '=' 两侧空白 / 单引号。
		"--b\r\nContent-Disposition: form-data; name = \"password\"\r\n\r\nSECRET\r\n--b--\r\n",
		"--b\r\nContent-Disposition: form-data; name='password'\r\n\r\nSECRET\r\n--b--\r\n",
		`name = 'password'`,
		// 修 B：无引号 key 对象字面量 / ':' 分隔（含 ':' 前空白）。
		`{password:"SECRET"}`,
		"password: SECRET",
		"password : SECRET",
		// 同族加固：无引号 token 形态（MIME 合法）、单引号对象 key、非词边界的嵌套 key。
		"--b\r\nContent-Disposition: form-data; name=password\r\n\r\nSECRET\r\n--b--\r\n",
		`{'password': 'x'}`,
		"user.password=x", // 既有保守行为：'.' 非词边界仍按字段处理（主管裁定保留）
	}
	for _, s := range leaked {
		if !hasUnmaskedPassword(s) {
			t.Errorf("must be flagged as unmasked password: %q", s)
		}
	}
	safe := []string{
		"",
		"hello world",
		"please reset your password now",
		"please reset your password as soon as possible",
		`{"password":"******"}`,
		`{"password":""}`,
		`{"password":null}`,
		`{"password"}`, // 引号后是 '}'（非参数边界）→ 不占位
		`{password:""}`,
		`{'password':''}`,
		`{"password_policy":"x"}`,
		`{"mypassword":"x"}`,
		"password=******",
		"password=",
		"user=alice&password=******",
		"password", // 仅单词、无可判定值
		"password:",
		"password: ",
		`<input name="password">`, // name="password" 之后非参数边界 → 不按 multipart 占位
		`name="file"`,             // 无 password 字段
	}
	for _, s := range safe {
		if hasUnmaskedPassword(s) {
			t.Errorf("must NOT be flagged: %q", s)
		}
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
