package response

import (
	"github.com/TomWu-Alchemi/project-framework/metrics"
	"github.com/gin-gonic/gin"
)

type CommonResponse struct {
	ResponseStatus ResponseStatus `json:"response_status"`
	Data           any            `json:"data"`
}

type ResponseStatus struct {
	Code      int    `json:"code"`
	Msg       string `json:"msg"`
	Extension []Pair `json:"extension"`
}

type Pair struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

func Success(c *gin.Context, data any, msg string, ext []Pair) CommonResponse {
	c.Set(metrics.ResponseCodeMetricKey, 200)
	return CommonResponse{
		ResponseStatus: successResponseStatus(msg, ext),
		Data:           data,
	}
}

func Failed(c *gin.Context, code int, msg string, ext []Pair) CommonResponse {
	c.Set(metrics.ResponseCodeMetricKey, code)
	return CommonResponse{
		ResponseStatus: failedResponseStatus(code, msg, ext),
		Data:           nil,
	}
}

func Success2(endpoint string, data any, msg string, ext []Pair) CommonResponse {
	metrics.ResponseCodeMetric(endpoint, 200)
	return CommonResponse{
		ResponseStatus: successResponseStatus(msg, ext),
		Data:           data,
	}
}

func Failed2(endpoint string, code int, msg string, ext []Pair) CommonResponse {
	metrics.ResponseCodeMetric(endpoint, code)
	return CommonResponse{
		ResponseStatus: failedResponseStatus(code, msg, ext),
		Data:           nil,
	}
}

func successResponseStatus(msg string, ext []Pair) ResponseStatus {
	return ResponseStatus{
		Code:      200,
		Msg:       msg,
		Extension: ext,
	}
}

func failedResponseStatus(code int, msg string, ext []Pair) ResponseStatus {
	return ResponseStatus{
		Code:      code,
		Msg:       msg,
		Extension: ext,
	}
}
