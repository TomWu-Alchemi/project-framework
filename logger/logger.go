package logger

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"time"

	"github.com/natefinch/lumberjack"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var log *zap.SugaredLogger

var accessLog *zap.Logger

var recoveryLog *zap.Logger

var dalLog *zap.Logger

func InitLogger() {
	var coreArr []zapcore.Core
	// 编码器
	encoderConfig := zap.NewProductionEncoderConfig()
	encoderConfig.EncodeTime = func(t time.Time, enc zapcore.PrimitiveArrayEncoder) {
		enc.AppendString(t.Format(time.DateTime))
	}
	encoderConfig.EncodeLevel = zapcore.CapitalLevelEncoder
	encoder := zapcore.NewConsoleEncoder(encoderConfig)

	lowPriority := zap.LevelEnablerFunc(func(lvl zapcore.Level) bool {
		return lvl < zap.WarnLevel
	})
	highPriority := zap.LevelEnablerFunc(func(lvl zapcore.Level) bool {
		return lvl >= zap.WarnLevel
	})

	infoLoggerWriter := &lumberjack.Logger{
		Filename:   getAbsPath("./log/info/info.log"),
		MaxSize:    30,
		MaxAge:     7,
		MaxBackups: 169,
		LocalTime:  true,
		Compress:   false,
	}
	infoFileWriteSyncer := zapcore.AddSync(infoLoggerWriter)
	infoFileCore := zapcore.NewCore(encoder, zapcore.NewMultiWriteSyncer(infoFileWriteSyncer, zapcore.AddSync(os.Stdout)), lowPriority)

	errorLoggerWriter := &lumberjack.Logger{
		Filename:   getAbsPath("./log/error/error.log"),
		MaxSize:    30,
		MaxAge:     14,
		MaxBackups: 420,
		LocalTime:  true,
		Compress:   false,
	}
	errorFileWriteSyncer := zapcore.AddSync(errorLoggerWriter)
	errorFileCore := zapcore.NewCore(encoder, zapcore.NewMultiWriteSyncer(errorFileWriteSyncer, zapcore.AddSync(os.Stdout)), highPriority)

	coreArr = append(coreArr, infoFileCore, errorFileCore)
	log = zap.New(zapcore.NewTee(coreArr...), zap.AddCaller()).Sugar()

	accessLoggerWriter := &lumberjack.Logger{
		Filename:   getAbsPath("./log/access/access.log"),
		MaxSize:    30,
		MaxAge:     7,
		MaxBackups: 169,
		LocalTime:  true,
		Compress:   false,
	}
	accessFileWriteSyncer := zapcore.AddSync(accessLoggerWriter)
	accessFileCore := zapcore.NewCore(encoder, zapcore.NewMultiWriteSyncer(accessFileWriteSyncer, zapcore.AddSync(os.Stdout)), zap.InfoLevel)
	accessLog = zap.New(accessFileCore)

	panicLoggerWriter := &lumberjack.Logger{
		Filename:   getAbsPath("./log/panic/panic.log"),
		MaxSize:    30,
		MaxAge:     14,
		MaxBackups: 420,
		LocalTime:  true,
		Compress:   false,
	}
	panicFileWriteSyncer := zapcore.AddSync(panicLoggerWriter)
	panicFileCore := zapcore.NewCore(encoder, zapcore.NewMultiWriteSyncer(panicFileWriteSyncer, zapcore.AddSync(os.Stdout)), zap.InfoLevel)
	recoveryLog = zap.New(panicFileCore)

	dataFileLoggerWriter := &lumberjack.Logger{
		Filename:   getAbsPath("./log/dal/dal.log"),
		MaxSize:    30,
		MaxAge:     7,
		MaxBackups: 169,
		LocalTime:  true,
		Compress:   false,
	}
	dataFileWriteSyncer := zapcore.AddSync(dataFileLoggerWriter)
	dataFileCore := zapcore.NewCore(encoder, zapcore.NewMultiWriteSyncer(dataFileWriteSyncer), zap.InfoLevel)
	dalLog = zap.New(dataFileCore)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				Error("panic in log rotating: %v, stack: %s", r, debug.Stack())
			}
		}()
		for {
			now := time.Now()
			next := now.Add(time.Hour)
			next = time.Date(next.Year(), next.Month(), next.Day(), next.Hour(), 0, 0, 0, next.Location())
			duration := next.Sub(now)

			timer := time.NewTimer(duration)
			<-timer.C

			// 强制切分所有日志文件
			rotateIfNotEmpty(infoLoggerWriter)
			rotateIfNotEmpty(errorLoggerWriter)
			rotateIfNotEmpty(accessLoggerWriter)
			rotateIfNotEmpty(panicLoggerWriter)
			rotateIfNotEmpty(dataFileLoggerWriter)
		}
	}()
}

func Info(args ...interface{}) {
	log.Info(args)
}

func Warn(args ...interface{}) {
	log.Warn(args)
}

func Error(args ...interface{}) {
	log.Error(args)
}

func StackedError(err error) {
	log.Error(fmt.Sprintf("[%+v]", err))
}

func GetAccessLog() *zap.Logger {
	return accessLog
}

func GetRecoveryLog() *zap.Logger {
	return recoveryLog
}

func GetDalLog() *zap.Logger {
	return dalLog
}

func rotateIfNotEmpty(writer *lumberjack.Logger) {
	// 检查文件是否存在且不为空
	if info, err := os.Stat(writer.Filename); err == nil && info.Size() > 0 {
		writer.Rotate()
	}
}

func getAbsPath(path string) string {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return absPath
}
