package httpclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	"go.uber.org/zap"
)

const (
	maxLogBytes      = 256 << 10
	maxResponseBytes = 10 << 20
	retryBaseDelay   = 50 * time.Millisecond
	retryMaxDelay    = 2 * time.Second
)

type DalHttpClient struct {
	httpClient *http.Client
	dalLog     *zap.Logger
}

type DalHttpClientConf struct {
	Timeout time.Duration
	DalLog  *zap.Logger
}

var ErrFailedRequest = errors.New("failed request")

func NewDalHttpClient(conf DalHttpClientConf) *DalHttpClient {
	t := http.DefaultTransport.(*http.Transport).Clone()
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

func truncateBytes(b []byte) (logged []byte, truncated bool, size int) {
	size = len(b)
	if size <= maxLogBytes {
		return b, false, size
	}
	return bytes.Clone(b[:maxLogBytes]), true, size
}

func truncateString(s string) (logged string, truncated bool, size int) {
	size = len(s)
	if size <= maxLogBytes {
		return s, false, size
	}
	return strings.Clone(s[:maxLogBytes]), true, size
}

func zapTruncatedBytes(key string, b []byte) []zap.Field {
	logged, truncated, size := truncateBytes(b)
	return []zap.Field{
		zap.ByteString(key, logged),
		zap.Bool(key+"_truncated", truncated),
		zap.Int(key+"_size", size),
	}
}

func zapTruncatedString(key string, s string) []zap.Field {
	logged, truncated, size := truncateString(s)
	return []zap.Field{
		zap.String(key, logged),
		zap.Bool(key+"_truncated", truncated),
		zap.Int(key+"_size", size),
	}
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
	logged, _, _ := truncateBytes(body)
	return fmt.Errorf("%w: status=%d body=%s", ErrFailedRequest, status, logged)
}

func wrapReadBody(err error) error {
	if maxErr, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return fmt.Errorf("response body exceeds size limit (%d bytes): %w", maxErr.Limit, maxErr)
	}
	return fmt.Errorf("failed to read response body: %w", err)
}

func (c *DalHttpClient) info(msg string, fields ...zap.Field) {
	if c.dalLog == nil {
		return
	}
	c.dalLog.Info(msg, fields...)
}

func (c *DalHttpClient) warn(msg string, fields ...zap.Field) {
	if c.dalLog == nil {
		return
	}
	c.dalLog.Warn(msg, fields...)
}

func (c *DalHttpClient) PostJson(ctx context.Context, rawURL string, headers map[string]string, data any, resp any) error {
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
		headerSb.WriteString(fmt.Sprintf("(%s:%s),", k, v))
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
	logFields = append(logFields, zapTruncatedString("path", rawURL)...)
	logFields = append(logFields, zapTruncatedBytes("data", jsonData)...)
	logFields = append(logFields, zapTruncatedString("header", headerSb.String())...)
	logFields = append(logFields, zapTruncatedBytes("response", bodyBytes)...)

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

func (c *DalHttpClient) GetWithRetry(ctx context.Context, baseUrl string, params map[string]string, headers map[string]string, maxRetries int) ([]byte, error) {
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
		headerSb.WriteString(fmt.Sprintf("(%s:%s),", k, v))
	}
	headerStr := headerSb.String()

	attempts := max(maxRetries, 1)
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
			if waitErr := waitRetry(ctx, i); waitErr != nil {
				return nil, waitErr
			}
			continue
		}

		bodyBytes, err := readLimitedBody(resp)
		if err != nil {
			if maxErr, ok := errors.AsType[*http.MaxBytesError](err); ok {
				return nil, fmt.Errorf("response body exceeds size limit (%d bytes): %w", maxErr.Limit, maxErr)
			}
			lastErr = err
			if i == attempts-1 {
				break
			}
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
		logFields = append(logFields, zapTruncatedString("path", fullURL)...)
		logFields = append(logFields, zapTruncatedString("header", headerStr)...)
		logFields = append(logFields, zapTruncatedBytes("response", bodyBytes)...)
		c.info("GetWithRetry", logFields...)

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
	return nil, fmt.Errorf("after %d retries: %w", attempts, lastErr)
}
