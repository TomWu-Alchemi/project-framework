package httpclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/TomWu-Alchemi/project-framework/internal/logutil"
	"github.com/bytedance/sonic"
	"go.uber.org/zap"
)

const (
	maxResponseBytes = 10 << 20
	retryBaseDelay   = 50 * time.Millisecond
	retryMaxDelay    = 2 * time.Second
	defaultTimeout   = 10 * time.Second
)

type DalHttpClient struct {
	httpClient *http.Client
	dalLog     *zap.Logger
}

type DalHttpClientConf struct {
	Timeout time.Duration
	// DalLog 为 DAL 日志器（记录本条请求的全量 path/header/data/response）。
	// nil 表示**不记录任何 DAL 日志**：这是显式 opt-in 设计——DAL 日志是全量
	// 请求/响应记录，未注入时既不产生日志，也不占用全局日志通道（本字段与
	// logger.GetDalLog() 相互独立，nil 不会回退全局 DAL 日志）。
	DalLog *zap.Logger
}

var (
	ErrFailedRequest = errors.New("failed request")
	ErrNilClient     = errors.New("httpclient: nil client")
)

func NewDalHttpClient(conf DalHttpClientConf) *DalHttpClient {
	if conf.Timeout <= 0 {
		conf.Timeout = defaultTimeout
	}
	// F-26：宿主可能替换 http.DefaultTransport（测试注入 / APM 仪器化的常见做法），
	// 硬类型断言会让构造直接 panic。ok 断言失败时兜底自建 Transport，
	// 其余字段对齐 http.DefaultTransport 的默认值。
	t, ok := http.DefaultTransport.(*http.Transport)
	if ok {
		t = t.Clone()
	} else {
		t = &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ForceAttemptHTTP2:     true,
			DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		}
	}
	t.MaxIdleConns = 100
	t.MaxIdleConnsPerHost = 100
	t.IdleConnTimeout = 60 * time.Second
	return &DalHttpClient{
		httpClient: &http.Client{Timeout: conf.Timeout, Transport: t},
		dalLog:     conf.DalLog,
	}
}

func isSuccessStatus(code int) bool {
	return code >= 200 && code < 300
}

func isRetryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code >= 500
}

func retryBackoff(attempt int) time.Duration {
	backoff := retryMaxDelay
	if attempt < 6 {
		backoff = min(retryBaseDelay<<attempt, retryMaxDelay)
	}
	j := backoff / 10
	if j > 0 {
		backoff += time.Duration(rand.Int64N(int64(2*j)+1)) - j
	}
	if backoff < 0 {
		backoff = 0
	}
	return backoff
}

func waitRetry(ctx context.Context, attempt int) error {
	timer := time.NewTimer(retryBackoff(attempt))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func readLimitedBody(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	resp.Body = http.MaxBytesReader(nil, resp.Body, maxResponseBytes)
	return io.ReadAll(resp.Body)
}

func failedRequest(status int, body []byte) error {
	logged, _, _ := logutil.TruncateBytes(body)
	return fmt.Errorf("%w: status=%d body=%s", ErrFailedRequest, status, logged)
}

func wrapReadBody(err error) error {
	if maxErr, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return fmt.Errorf("response body exceeds size limit (%d bytes): %w", maxErr.Limit, maxErr)
	}
	return fmt.Errorf("failed to read response body: %w", err)
}

// info 在 DalLog 非 nil 时打一条 Info 级 DAL 日志。
// DalLog == nil 表示**不记录任何 DAL 日志**（显式 opt-in，见 DalHttpClientConf.DalLog），
// 此情况下静默返回，不写日志、不占用全局日志通道。
func (c *DalHttpClient) info(msg string, fields ...zap.Field) {
	if c.dalLog == nil {
		return
	}
	c.dalLog.Info(msg, fields...)
}

// warn 在 DalLog 非 nil 时打一条 Warn 级 DAL 日志。
// DalLog == nil 表示**不记录任何 DAL 日志**（显式 opt-in，见 DalHttpClientConf.DalLog），
// 此情况下静默返回，不写日志、不占用全局日志通道。
func (c *DalHttpClient) warn(msg string, fields ...zap.Field) {
	if c.dalLog == nil {
		return
	}
	c.dalLog.Warn(msg, fields...)
}

// PostJson marshals data as JSON, POSTs it to rawURL and, unless resp is nil
// or the response is 204/empty, unmarshals the response body into resp.
//
// Design note (P3-5): PostJson never retries, unlike GetWithRetry. POST is not
// idempotent, so replaying it automatically could duplicate the server-side
// effect. Callers that need retries must implement them on top of PostJson
// and guarantee idempotency themselves (for example with an idempotency key).
func (c *DalHttpClient) PostJson(ctx context.Context, rawURL string, headers map[string]string, data any, resp any) error {
	if c == nil || c.httpClient == nil {
		return ErrNilClient
	}
	jsonData, err := sonic.Marshal(data)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewBuffer(jsonData))
	if err != nil {
		return err
	}
	headerSb := strings.Builder{}
	headerSb.Grow(len(headers) * 20)
	for k, v := range headers {
		req.Header.Set(k, v)
		headerSb.WriteByte('(')
		headerSb.WriteString(k)
		headerSb.WriteByte(':')
		headerSb.WriteString(v)
		headerSb.WriteString("),")
	}
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}

	start := time.Now()
	rawResponse, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}

	bodyBytes, err := readLimitedBody(rawResponse)
	if err != nil {
		return wrapReadBody(err)
	}

	logFields := []zap.Field{
		zap.Int("status", rawResponse.StatusCode),
		zap.String("method", http.MethodPost),
		zap.Int64("latency_ms", time.Since(start).Milliseconds()),
	}
	logFields = append(logFields, logutil.ZapTruncatedString("path", rawURL)...)
	logFields = append(logFields, logutil.ZapTruncatedBytes("data", jsonData)...)
	logFields = append(logFields, logutil.ZapTruncatedString("header", headerSb.String())...)
	logFields = append(logFields, logutil.ZapTruncatedBytes("response", bodyBytes)...)

	if !isSuccessStatus(rawResponse.StatusCode) {
		c.warn("PostJson", logFields...)
		return failedRequest(rawResponse.StatusCode, bodyBytes)
	}
	c.info("PostJson", logFields...)
	if resp == nil || rawResponse.StatusCode == http.StatusNoContent || len(bodyBytes) == 0 {
		return nil
	}
	return sonic.Unmarshal(bodyBytes, resp)
}

// GetWithRetry issues a GET request and retries on network errors and
// retryable statuses (429 / 5xx).
//
// maxAttempts is the total number of attempts, not the number of extra
// retries: values below 1 are treated as a single attempt. When all attempts
// are exhausted, the returned error is wrapped as "after %d attempts".
func (c *DalHttpClient) GetWithRetry(ctx context.Context, baseUrl string, params map[string]string, headers map[string]string, maxAttempts int) ([]byte, error) {
	if c == nil || c.httpClient == nil {
		return nil, ErrNilClient
	}
	fullURL := baseUrl
	if len(params) > 0 {
		u, err := url.Parse(baseUrl)
		if err != nil {
			return nil, err
		}
		q := u.Query()
		for k, v := range params {
			q.Set(k, v)
		}
		u.RawQuery = q.Encode()
		fullURL = u.String()
	}

	headerSb := strings.Builder{}
	headerSb.Grow(len(headers) * 20)
	for k, v := range headers {
		headerSb.WriteByte('(')
		headerSb.WriteString(k)
		headerSb.WriteByte(':')
		headerSb.WriteString(v)
		headerSb.WriteString("),")
	}
	headerStr := headerSb.String()

	attempts := max(maxAttempts, 1)
	var lastErr error
	for i := range attempts {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
		if err != nil {
			return nil, err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}

		start := time.Now()
		resp, err := c.httpClient.Do(req)
		latency := time.Since(start).Milliseconds()
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if i == attempts-1 {
				break
			}
			// The network-error retry path used to be completely silent
			// (P3-4): warn once per retried attempt so operators can see the
			// failed attempt (1-based), its latency and the underlying error.
			c.warn("GetWithRetry",
				zap.Int("attempt", i+1),
				zap.Int64("latency_ms", latency),
				zap.Error(err),
			)
			if waitErr := waitRetry(ctx, i); waitErr != nil {
				return nil, waitErr
			}
			continue
		}

		bodyBytes, err := readLimitedBody(resp)
		if err != nil {
			// MaxBytesError 不是瞬时故障：响应体已确定超限，重试也不会变小，
			// 因此保持「立即返回、不打日志」。
			if maxErr, ok := errors.AsType[*http.MaxBytesError](err); ok {
				return nil, fmt.Errorf("response body exceeds size limit (%d bytes): %w", maxErr.Limit, maxErr)
			}
			lastErr = err
			if i == attempts-1 {
				break
			}
			// 与网络错误重试路径（P3-4）同级：重试前 Warn 一次，字段同款
			// （attempt / latency_ms / error），避免这条 body 读取失败的重试路径静默。
			c.warn("GetWithRetry",
				zap.Int("attempt", i+1),
				zap.Int64("latency_ms", latency),
				zap.Error(err),
			)
			if waitErr := waitRetry(ctx, i); waitErr != nil {
				return nil, waitErr
			}
			continue
		}

		logFields := []zap.Field{
			zap.Int("status", resp.StatusCode),
			zap.String("method", http.MethodGet),
			zap.Int64("latency_ms", latency),
		}
		logFields = append(logFields, logutil.ZapTruncatedString("path", fullURL)...)
		logFields = append(logFields, logutil.ZapTruncatedString("header", headerStr)...)
		logFields = append(logFields, logutil.ZapTruncatedBytes("response", bodyBytes)...)
		// F-27：重试类失败状态（429/5xx）与网络错误路径（P3-4）同为 Warn 级，
		// 便于基于级别的告警捕捉重试风暴；成功与其余 4xx 保持 Info。
		if isRetryableStatus(resp.StatusCode) {
			c.warn("GetWithRetry", logFields...)
		} else {
			c.info("GetWithRetry", logFields...)
		}

		if isSuccessStatus(resp.StatusCode) {
			return bodyBytes, nil
		}

		lastErr = failedRequest(resp.StatusCode, bodyBytes)
		if !isRetryableStatus(resp.StatusCode) {
			return nil, lastErr
		}
		if i == attempts-1 {
			break
		}
		if waitErr := waitRetry(ctx, i); waitErr != nil {
			return nil, waitErr
		}
	}

	if lastErr == nil {
		lastErr = ErrFailedRequest
	}
	return nil, fmt.Errorf("after %d attempts: %w", attempts, lastErr)
}
