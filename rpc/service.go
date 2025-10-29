package rpc

import (
	"ai-kaka.com/project-framework/logger"
	"context"
	"fmt"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"
	errors2 "github.com/pkg/errors"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"runtime/debug"
	"strings"
	"time"
)

type NatsService struct {
	nc  *nats.Conn
	srv micro.Service
}

type ServiceConfig struct {
	Url          string        `json:"url"`
	Username     string        `json:"username,omitempty"`
	Password     string        `json:"password,omitempty"`
	AppName      string        `json:"app_name"`
	Version      string        `json:"version"`
	DrainTimeout time.Duration `json:"drain_timeout"`
}

func NewNatsService(config ServiceConfig) (*NatsService, func(), error) {
	nc, err := nats.Connect(config.Url,
		nats.UserInfo(config.Username, config.Password),
		nats.DisconnectErrHandler(func(conn *nats.Conn, err error) {
			logger.Error(fmt.Sprintf("nats rpc disconnect error occur, err(%v）", err))
		}),
		nats.DrainTimeout(config.DrainTimeout))
	if err != nil {
		return nil, func() {}, errors2.WithStack(err)
	}

	srv, err := micro.AddService(nc, micro.Config{
		Name:    config.AppName,
		Version: config.Version,
		ErrorHandler: func(service micro.Service, natsError *micro.NATSError) {
			logger.Error("srv(%s) version(%s) error occurred, err(%v)", service.Info().Name, service.Info().Version, natsError.Error())
		},
	})
	if err != nil {
		return nil, func() {}, errors2.WithStack(err)
	}

	natsSrv := &NatsService{
		nc:  nc,
		srv: srv,
	}
	cleanup := func() {
		logger.Info("rpc service shutdown start.")
		if err := srv.Stop(); err != nil {
			logger.StackedError(err)
		}
		if err := nc.Drain(); err != nil {
			logger.StackedError(err)
		}
		logger.Info("rpc service shutdown end.")
	}
	return natsSrv, cleanup, nil
}

func NatsRpcAccessLog(fn func(context.Context, micro.Request)) func(context.Context, micro.Request) {
	return func(ctx context.Context, rawReq micro.Request) {
		defer func() {
			if r := recover(); r != nil {
				logger.GetRecoveryLog().Error("[Recovery from rpc panic]",
					zap.Time("time", time.Now()),
					zap.Any("error", r),
					zap.String("path", rawReq.Subject()),
					zap.ByteString("data", rawReq.Data()),
					zap.String("header", headersToString(rawReq.Headers())),
					zap.String("stack", string(debug.Stack())))
			}
		}()

		start := time.Now()

		fn(ctx, rawReq)

		logFields := []zapcore.Field{
			zap.String("path", rawReq.Subject()),
			zap.ByteString("data", rawReq.Data()),
			zap.String("header", headersToString(rawReq.Headers())),
			zap.Int64("latency_ms", time.Since(start).Milliseconds()),
		}
		logger.GetAccessLog().Info("nats-rpc", logFields...)
	}
}

func (s *NatsService) GetSrv() micro.Service {
	return s.srv
}

func (s *NatsService) GetClient() *nats.Conn {
	return s.nc
}

func headersToString(m micro.Headers) string {
	if len(m) == 0 {
		return "{}"
	}

	b := strings.Builder{}
	b.WriteByte('{')

	i := 0
	for k, v := range m {
		if i > 0 {
			b.WriteString(",")
		}

		b.WriteString(k)
		b.WriteString(":[")
		b.WriteString(strings.Join(v, ","))
		b.WriteString("]")

		i++
	}

	b.WriteByte('}')
	return b.String()
}
