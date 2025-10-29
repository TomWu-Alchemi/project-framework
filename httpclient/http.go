package httpclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/bytedance/sonic"
	errors2 "github.com/pkg/errors"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
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
	return &DalHttpClient{
		httpClient: &http.Client{Timeout: conf.Timeout, Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 100,
			IdleConnTimeout:     60 * time.Second,
			Proxy:               http.ProxyFromEnvironment,
		}},
		dalLog: conf.DalLog,
	}
}

func (c *DalHttpClient) PostJson(ctx context.Context, url string, headers map[string]string, data any, resp any) error {
	jsonData, err := sonic.Marshal(data)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(jsonData))
	if err != nil {
		return err
	}
	if _, exists := headers["Content-Type"]; !exists {
		req.Header.Set("Content-Type", "application/json")
	}
	headerSb := strings.Builder{}
	headerSb.Grow(len(headers) * 20)
	for k, v := range headers {
		req.Header.Set(k, v)
		headerSb.WriteString(fmt.Sprintf("(%s:%s),", k, v))
	}
	start := time.Now()
	rawResponse, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer rawResponse.Body.Close()

	// 限制响应体大小为 10MB
	rawResponse.Body = http.MaxBytesReader(nil, rawResponse.Body, 10<<20)

	bodyBytes, err := io.ReadAll(rawResponse.Body)
	if err != nil {
		if errors.Is(err, &http.MaxBytesError{}) {
			return errors2.New("response body exceeds size limit")
		}
		return errors2.Wrap(err, "failed to read response body")
	}
	logFields := []zapcore.Field{
		zap.Int("status", rawResponse.StatusCode),
		zap.String("method", http.MethodPost),
		zap.String("path", url),
		zap.ByteString("data", jsonData),
		zap.String("header", headerSb.String()),
		zap.Int64("latency_ms", time.Since(start).Milliseconds()),
		zap.ByteString("response", bodyBytes),
	}
	if rawResponse.StatusCode == http.StatusOK {
		c.dalLog.Info("PostJson", logFields...)
		err = sonic.Unmarshal(bodyBytes, resp)
		return err
	} else {
		c.dalLog.Warn("PostJson", logFields...)
		return ErrFailedRequest
	}
}

func (c *DalHttpClient) GetWithRetry(baseUrl string, params map[string]string, headers map[string]string, maxRetries int) ([]byte, error) {
	fullUrl := baseUrl
	if len(params) > 0 {
		urlParams := url.Values{}
		for k, v := range params {
			urlParams.Add(k, v)
		}
		fullUrl = baseUrl + "?" + urlParams.Encode()
	}
	req, err := http.NewRequest("GET", fullUrl, nil)
	if err != nil {
		return nil, err
	}

	// 构建请求头日志字符串
	headerSb := strings.Builder{}
	headerSb.Grow(len(headers) * 20)
	if len(headers) > 0 {
		for k, v := range headers {
			req.Header.Add(k, v)
			headerSb.WriteString(fmt.Sprintf("(%s:%s),", k, v))
		}
	}
	headerStr := headerSb.String()

	var lastErr error
	for i := 0; i < maxRetries; i++ {
		start := time.Now()
		resp, err := c.httpClient.Do(req)
		currentLatency := time.Since(start).Milliseconds()

		if err != nil {
			lastErr = err
			time.Sleep(time.Millisecond * time.Duration(i+1*50)) // 指数退避
			continue
		}

		// 读取响应体
		bodyBytes, err := io.ReadAll(resp.Body)
		resp.Body.Close() // 显式关闭body，避免循环中defer导致的资源泄露

		if err != nil {
			lastErr = errors2.WithStack(err)
			time.Sleep(time.Millisecond * time.Duration(i+1*50))
			continue
		}

		// 记录日志
		logFields := []zapcore.Field{
			zap.Int("status", resp.StatusCode),
			zap.String("method", "GET"),
			zap.String("path", fullUrl),
			zap.String("header", headerStr),
			zap.Int64("latency_ms", currentLatency),
			zap.ByteString("response", bodyBytes),
		}
		c.dalLog.Info("GetWithRetry", logFields...)
		if resp.StatusCode == http.StatusOK {
			return bodyBytes, nil
		}

		lastErr = fmt.Errorf("url:(%s) status code:%d", fullUrl, resp.StatusCode)
		time.Sleep(time.Millisecond * time.Duration(i+1*50))
	}

	return nil, errors2.WithStack(fmt.Errorf("after %d retries, last error: %v", maxRetries, lastErr))
}
