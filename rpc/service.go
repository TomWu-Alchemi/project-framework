package rpc

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/TomWu-Alchemi/project-framework/internal/logutil"
	"github.com/TomWu-Alchemi/project-framework/logger"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/micro"
	errors2 "github.com/pkg/errors"
	"go.uber.org/zap"
)

const (
	// drainWaitGrace is added on top of the effective drain timeout when
	// cleanup waits for the connection's closed callback. It covers the
	// part of nats.go's drainConnection() that DrainTimeout does not bound:
	// the final publish flush uses a hardcoded 5s FlushTimeout (nats.go
	// drainConnection), plus 1s slack for scheduling and the dispatch of
	// the async closed callback.
	drainWaitGrace = 6 * time.Second
)

// connectOptions builds the NATS connection options derived from config.
//
// nats.DrainTimeout assigns unconditionally (nats.go), so passing the zero
// value would override the library default (nats.DefaultDrainTimeout, 30s)
// and make Drain() skip waiting for in-flight requests entirely. The option
// is therefore only appended when explicitly configured (> 0).
func connectOptions(config ServiceConfig) []nats.Option {
	log := config.Log
	options := []nats.Option{
		nats.DisconnectErrHandler(func(conn *nats.Conn, err error) {
			logErrorf(log, "nats rpc disconnect error occur, err(%v)", err)
		}),
		nats.ErrorHandler(asyncErrorHandler(log)),
	}
	// F-32：空账号不显式传 UserInfo，避免空值覆盖 URL 内嵌凭据的语义歧义
	if config.Username != "" || config.Password != "" {
		options = append(options, nats.UserInfo(config.Username, config.Password))
	}
	if config.DrainTimeout > 0 {
		options = append(options, nats.DrainTimeout(config.DrainTimeout))
	}
	return options
}

func asyncErrorHandler(log *zap.Logger) nats.ErrHandler {
	return func(_ *nats.Conn, sub *nats.Subscription, err error) {
		subject := ""
		if sub != nil {
			subject = sub.Subject
		}
		logErrorf(log, "nats async error, subject(%s) err(%v)", subject, err)
	}
}

// drainWaitBudget returns the maximum time cleanup waits for the connection's
// closed callback after Drain() has been initiated.
//
// DrainTimeout bounds how long nats.go waits for subscriptions to drain;
// drainConnection() then flushes pending publishes with a hardcoded 5s
// FlushTimeout before closing the connection. A configured value <= 0 means
// "unset": the effective timeout is nats.DefaultDrainTimeout, matching the
// library default that nats.DrainTimeout(0) used to override.
func drainWaitBudget(configured time.Duration) time.Duration {
	effective := configured
	if effective <= 0 {
		effective = nats.DefaultDrainTimeout
	}
	return effective + drainWaitGrace
}

type NatsService struct {
	nc  *nats.Conn
	srv micro.Service
	log *zap.Logger // nil => 回退全局 logger
}

type ServiceConfig struct {
	Url          string        `json:"url"`
	Username     string        `json:"username,omitempty"`
	Password     string        `json:"password,omitempty"`
	AppName      string        `json:"app_name"`
	Version      string        `json:"version"`
	DrainTimeout time.Duration `json:"drain_timeout"`
	Log          *zap.Logger   `json:"-"`
}

func NewNatsService(config ServiceConfig) (*NatsService, func(), error) {
	// F-32：空 Url 在框架层提前报错，错误信息优于 nats 内部文案（排障友好）
	if strings.TrimSpace(config.Url) == "" {
		return nil, func() {}, errors2.WithStack(errors.New("rpc: nats url is empty"))
	}

	// closedCh is signalled by the ClosedHandler once the connection has
	// moved to closed (both on an explicit Close and at the end of a drain).
	// The buffered channel plus the non-blocking send keep a repeated
	// callback from blocking nats' async callback loop.
	closedCh := make(chan struct{}, 1)

	options := connectOptions(config)
	// Registered here instead of inside connectOptions because it needs the
	// channel shared with cleanup, which cannot be derived from config.
	options = append(options, nats.ClosedHandler(func(*nats.Conn) {
		select {
		case closedCh <- struct{}{}:
		default:
		}
	}))

	nc, err := nats.Connect(config.Url, options...)
	if err != nil {
		return nil, func() {}, errors2.WithStack(err)
	}

	srv, err := micro.AddService(nc, micro.Config{
		Name:    config.AppName,
		Version: config.Version,
		ErrorHandler: func(service micro.Service, natsError *micro.NATSError) {
			logErrorf(config.Log, "srv(%s) version(%s) error occurred, err(%v)", service.Info().Name, service.Info().Version, natsError.Error())
		},
	})
	if err != nil {
		nc.Close()
		return nil, func() {}, errors2.WithStack(err)
	}

	natsSrv := &NatsService{
		nc:  nc,
		srv: srv,
		log: config.Log,
	}

	// shutdownOnce makes cleanup idempotent: repeated calls must not run
	// Stop/Drain twice.
	var shutdownOnce sync.Once
	cleanup := func() {
		shutdownOnce.Do(func() {
			log := natsSrv.log
			logInfo(log, "rpc service shutdown start.")
			// Stop unregisters the micro service; Drain waits for in-flight
			// requests and then closes the connection.
			if err := srv.Stop(); err != nil {
				logStackedError(log, err)
			}
			if err := nc.Drain(); err != nil {
				// Already closed / already draining are expected races (e.g.
				// the server closed the connection before cleanup ran): only
				// in-flight requests may be lost, the shutdown itself is fine.
				if errors.Is(err, nats.ErrConnectionClosed) || errors.Is(err, nats.ErrConnectionDraining) {
					logInfof(log, "nats rpc connection already closed or draining, err(%v)", err)
				} else {
					logStackedError(log, err)
				}
			}
			// Drain() returns as soon as drainConnection() has been started
			// in a goroutine, so wait for the closed callback before
			// reporting the shutdown as finished.
			budget := drainWaitBudget(config.DrainTimeout)
			select {
			case <-closedCh:
				logInfo(log, "rpc service shutdown end.")
			case <-time.After(budget):
				logWarnf(log, "rpc service shutdown timed out after %s, connection may not be fully closed", budget)
			}
		})
	}
	return natsSrv, cleanup, nil
}

func NatsRpcAccessLog(fn func(context.Context, micro.Request)) func(context.Context, micro.Request) {
	return func(ctx context.Context, rawReq micro.Request) {
		defer func() {
			if r := recover(); r != nil {
				rec := logger.GetRecoveryLog()
				if rec != nil {
					fields := []zap.Field{
						zap.Time("time", time.Now()),
						zap.Any("error", r),
						zap.String("stack", string(debug.Stack())),
					}
					fields = append(fields, truncatedPayloadFields(rawReq)...)
					rec.Error("[Recovery from rpc panic]", fields...)
				} else {
					// recovery log 未注入时回退全局 logger（对齐本文件 logStackedError
					// 的「nil 回退全局」模式），保留 error(panic 值) / path / stack
					// 三个排障关键信息；全局 logger 未初始化时该调用静默。
					logger.Errorf("[Recovery from rpc panic] error(%v) path(%s) stack(%s)",
						r, rawReq.Subject(), string(debug.Stack()))
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

func truncatedPayloadFields(rawReq micro.Request) []zap.Field {
	fields := []zap.Field{
		zap.String("path", rawReq.Subject()),
	}
	fields = append(fields, logutil.ZapTruncatedBytes("data", rawReq.Data())...)
	fields = append(fields, logutil.ZapTruncatedString("header", headersToString(rawReq.Headers()))...)
	return fields
}

func headersToString(m micro.Headers) string {
	if len(m) == 0 {
		return "{}"
	}

	b := strings.Builder{}
	// Grow 预估容量：每对 "key:[v]" 含分隔符按平均 24 字节估算，减少扩容拷贝。
	b.Grow(len(m) * 24)
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

func logInfo(l *zap.Logger, args ...any) {
	if l != nil {
		l.Sugar().Info(args...)
		return
	}
	logger.Info(args...)
}

func logInfof(l *zap.Logger, template string, args ...any) {
	if l != nil {
		l.Sugar().Infof(template, args...)
		return
	}
	logger.Infof(template, args...)
}

func logErrorf(l *zap.Logger, template string, args ...any) {
	if l != nil {
		l.Sugar().Errorf(template, args...)
		return
	}
	logger.Errorf(template, args...)
}

func logWarnf(l *zap.Logger, template string, args ...any) {
	if l != nil {
		l.Sugar().Warnf(template, args...)
		return
	}
	logger.Warnf(template, args...)
}

func logStackedError(l *zap.Logger, err error) {
	if err == nil {
		return
	}
	if l != nil {
		l.Sugar().Error(fmt.Sprintf("[%+v]", err))
		return
	}
	logger.StackedError(err)
}
