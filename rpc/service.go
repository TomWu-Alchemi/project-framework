package rpc

import (
	"bytes"
	"context"
	"runtime/debug"
	"strings"
	"time"

	"github.com/TomWu-Alchemi/project-framework/logger"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"
	errors2 "github.com/pkg/errors"
	"go.uber.org/zap"
)

const maxLogBytes = 256 << 10

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
			logger.Errorf("nats rpc disconnect error occur, err(%v)", err)
		}),
		nats.DrainTimeout(config.DrainTimeout))
	if err != nil {
		return nil, func() {}, errors2.WithStack(err)
	}

	srv, err := micro.AddService(nc, micro.Config{
		Name:    config.AppName,
		Version: config.Version,
		ErrorHandler: func(service micro.Service, natsError *micro.NATSError) {
			logger.Errorf("srv(%s) version(%s) error occurred, err(%v)", service.Info().Name, service.Info().Version, natsError.Error())
		},
	})
	if err != nil {
		nc.Close()
		return nil, func() {}, errors2.WithStack(err)
	}

	natsSrv := &NatsService{
		nc:  nc,
		srv: srv,
	}
	cleanup := func() {
		logger.Info("rpc service shutdown start.")
		// Stop unregisters the micro service; Drain waits for in-flight
		// requests and then closes the connection.
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
				if rec := logger.GetRecoveryLog(); rec != nil {
					fields := []zap.Field{
						zap.Time("time", time.Now()),
						zap.Any("error", r),
						zap.String("stack", string(debug.Stack())),
					}
					fields = append(fields, truncatedPayloadFields(rawReq)...)
					rec.Error("[Recovery from rpc panic]", fields...)
				}
				_ = rawReq.Error("500", "internal error", nil)
			}
		}()

		start := time.Now()
		fn(ctx, rawReq)
		if access := logger.GetAccessLog(); access != nil {
			fields := truncatedPayloadFields(rawReq)
			fields = append(fields, zap.Int64("latency_ms", time.Since(start).Milliseconds()))
			access.Info("nats-rpc", fields...)
		}
	}
}

func (s *NatsService) GetSrv() micro.Service {
	return s.srv
}

func (s *NatsService) GetClient() *nats.Conn {
	return s.nc
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

func truncatedPayloadFields(rawReq micro.Request) []zap.Field {
	loggedData, dataTrunc, dataSize := truncateBytes(rawReq.Data())
	loggedHeader, headerTrunc, headerSize := truncateString(headersToString(rawReq.Headers()))
	return []zap.Field{
		zap.String("path", rawReq.Subject()),
		zap.ByteString("data", loggedData),
		zap.Bool("data_truncated", dataTrunc),
		zap.Int("data_size", dataSize),
		zap.String("header", loggedHeader),
		zap.Bool("header_truncated", headerTrunc),
		zap.Int("header_size", headerSize),
	}
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
