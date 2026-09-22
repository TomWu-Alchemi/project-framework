package logger

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
)

// defaultLogDir 是 InitLogger 使用的默认日志根目录（保持既有文件布局）。
const (
	defaultLogDir = "./log"
	logTimeLayout = "2006-01-02 15:04:05.000"
	// defaultBufferFlushInterval 是 BufferSize>0 且 BufferFlushInterval<=0 时的回退刷新周期。
	// 注意：100ms 是本库自定义语义；zapcore.BufferedWriteSyncer 自身的零值默认是 30s。
	defaultBufferFlushInterval = 100 * time.Millisecond
)

// 全局 logger 用 atomic.Pointer 发布：InitLogger 与业务 goroutine 的
// Info/Warn/Error 之间不再有数据竞争。Load() 为 nil 表示尚未初始化，
// 所有入口静默丢弃（与既有语义一致，不 panic）。
var (
	logPtr         atomic.Pointer[zap.SugaredLogger]
	baseLogPtr     atomic.Pointer[zap.Logger] // 无 Skip 的 *zap.Logger，供 GetLogger / 热路径结构化打点
	accessLogPtr   atomic.Pointer[zap.Logger]
	recoveryLogPtr atomic.Pointer[zap.Logger]
	dalLogPtr      atomic.Pointer[zap.Logger]

	// runtimePtr 指向最近一次 initLoggerWithConfig 创建的运行期资源。
	runtimePtr atomic.Pointer[loggerRuntime]

	// initOnce 保证 InitLogger / InitLoggerWithConfig 幂等（先到者生效）。
	initOnce sync.Once
)

// loggerRuntime 聚合单次初始化需要回收的全部资源。
type loggerRuntime struct {
	writers []*lumberjack.Logger
	// rotateOne 默认 nil => rotateIfNotEmpty；测试可替换。
	rotateOne func(*lumberjack.Logger)
	// sleepAfterPanic 默认 nil => time.Sleep；测试可替换以免满睡 1s。
	sleepAfterPanic func(time.Duration)
	// untilNextRotate 默认 nil => 等到下一整点；测试可返回极短时长。
	untilNextRotate func() time.Duration
	// buffers 汇总本次初始化创建的全部文件侧缓冲（info/error/access/panic/dal 各自独立一个）。
	// BufferSize<=0（含 InitLogger 默认路径）时为空切片。stop() 必须严格「先 Stop buffers、
	// 后 Close writers」，否则未 flush 的数据会写向已关闭的 writer。
	buffers      []*zapcore.BufferedWriteSyncer
	rotateDone   chan struct{} // close 之后要求轮转 goroutine 退出
	rotateExited chan struct{} // 轮转 goroutine 返回时 close

	stopOnce sync.Once // 单个生命周期内 Shutdown 幂等
}

// stop 刷新全局 logger、停止轮转 goroutine、Stop 全部文件侧缓冲并关闭全部 lumberjack writer。
// 可重复调用。
func (rt *loggerRuntime) stop() {
	rt.stopOnce.Do(func() {
		if l := logPtr.Load(); l != nil {
			_ = l.Sync()
		}
		close(rt.rotateDone)
		<-rt.rotateExited // 等 goroutine 真正退出，测试可确定性断言

		// 先 Stop 全部缓冲：Stop 内部会 Flush 未落盘数据并停掉 flushLoop/ticker。
		// 必须早于下面的 writer.Close()，顺序反了会把未 flush 的数据写向已关闭的 writer。
		// Stop 自身幂等（未初始化或已停止时直接返回），因此二次 Shutdown 安全。
		//
		// 语义边界：Stop 只停刷新循环、并不关闭缓冲写入——之后再有写入仍会进内存，而
		// flushLoop 已退出，这些数据可能滞留到进程退出。与既有 F-29「Shutdown 后进程应
		// 尽快退出」的语义一致。
		for _, b := range rt.buffers {
			if err := b.Stop(); err != nil {
				Warnf("logger: stop log buffer failed: %v", err)
			}
		}
		for _, w := range rt.writers {
			if err := w.Close(); err != nil {
				Warnf("logger: close %s failed: %v", w.Filename, err)
			}
		}
	})
}

// rotateLoop 每小时整点强制切分所有日志文件；Shutdown 通过 rotateDone 停止它。
func (rt *loggerRuntime) rotateLoop() {
	defer close(rt.rotateExited)
	for {
		if rt.rotateOnce() {
			return
		}
	}
}

func (rt *loggerRuntime) rotateOnce() (stop bool) {
	defer func() {
		if r := recover(); r != nil {
			Errorf("panic in log rotating: %v, stack: %s", r, debug.Stack())
			sleep := rt.sleepAfterPanic
			if sleep == nil {
				sleep = time.Sleep
			}
			sleep(time.Second)
		}
	}()

	var wait time.Duration
	if rt.untilNextRotate != nil {
		wait = rt.untilNextRotate()
	} else {
		now := time.Now()
		next := now.Add(time.Hour)
		next = time.Date(next.Year(), next.Month(), next.Day(), next.Hour(), 0, 0, 0, next.Location())
		wait = time.Until(next)
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-rt.rotateDone:
		return true
	case <-timer.C:
	}

	rotate := rt.rotateOne
	if rotate == nil {
		rotate = rotateIfNotEmpty
	}
	for _, w := range rt.writers {
		rotate(w)
	}
	return false
}

// LoggerConfig 控制 InitLoggerWithConfig 的编码、目录、stdout 与压缩。
type LoggerConfig struct {
	Dir        string // empty => defaultLogDir (./log)
	JSON       bool   // false => ConsoleEncoder（当前 InitLogger）；true => JSONEncoder
	AlsoStdout bool   // if true, info/error/access/panic also write stdout（当前 InitLogger 行为）。dal 始终只写文件。
	Compress   bool   // lumberjack Compress
	// DisableCaller 为 true 时关闭 AddCaller（不走栈、日志无文件:行号）。
	// 零值 false 保持当前 InitLogger 行为。热路径可改用 GetLogger() 打结构化字段。
	DisableCaller bool
	// BufferSize 为文件侧 WriteSyncer 的内存缓冲字节数；<=0 表示关闭缓冲（当前行为）。
	// 仅缓冲文件侧（lumberjack），stdout 始终同步写。
	BufferSize int
	// BufferFlushInterval 为缓冲刷新周期；BufferSize>0 且本字段 <=0 时回退 100ms。
	// 注意：100ms 是本库自定义语义（zapcore.BufferedWriteSyncer 默认 30s）。
	BufferFlushInterval time.Duration
}

// InitLogger 初始化各全局 logger。幂等：重复调用为 no-op，文件句柄与
// 轮转 goroutine 每进程至多创建一次。行为等价于 InitLoggerWithConfig 使用
// 控制台编码、AlsoStdout true、Compress false、目录 ./log。
func InitLogger() {
	InitLoggerWithConfig(LoggerConfig{
		Dir:        defaultLogDir,
		JSON:       false,
		AlsoStdout: true,
		Compress:   false,
	})
}

// InitLoggerWithConfig 按 cfg 初始化各全局 logger，与 InitLogger 共享 initOnce（先到者生效）。
// 空 Dir 回退 defaultLogDir。
func InitLoggerWithConfig(cfg LoggerConfig) {
	initOnce.Do(func() {
		initLoggerWithConfig(cfg)
	})
}

// Shutdown 刷新落盘、停止轮转 goroutine 并关闭 InitLogger 创建的全部
// lumberjack writer。仅用于进程退出阶段的最后一步调用；重复调用安全（幂等）。
//
// 语义边界（F-29）：lumberjack 的 Close 只关闭当前句柄（file 置 nil），之后的
// Write 会经 openExistingOrNew 重新打开文件并写入成功——该句柄不再受本函数
// 管理（轮转、最终关闭均失效）。因此"返回后日志不再落盘"并不成立，实际语义是
// "后续日志仍会写入、但不再受控"。Shutdown 之后进程应尽快退出，不要依赖日志
// 系统的轮转 / 关闭行为。
//
// 缓冲语义（BufferSize>0）：Shutdown 会 Stop 全部文件侧缓冲并 Flush；因 zapcore.
// BufferedWriteSyncer.Stop 不会关闭缓冲写入，之后再写日志只会进内存（flushLoop 已停），
// 直到进程退出都不会落盘。
func Shutdown() {
	if rt := runtimePtr.Load(); rt != nil {
		rt.stop()
	}
}

func combineStdout(file zapcore.WriteSyncer, alsoStdout bool) zapcore.WriteSyncer {
	if !alsoStdout {
		return file
	}
	return zapcore.NewMultiWriteSyncer(file, zapcore.AddSync(os.Stdout))
}

// newFileWriteSyncer 返回文件侧的 WriteSyncer：BufferSize>0 时套一层内存缓冲，
// 否则直接返回 AddSync(w)（与既有行为逐字节相等）；返回的缓冲追加到 buffers。
//
// 「关闭缓冲」必须在配置层短路：zapcore.BufferedWriteSyncer 会把 Size 零值替换成
// 256KiB 默认值，因此不能靠「置 Size=0」关闭，只能不包这层。stdout 不进缓冲：
// 组合顺序恒为 combineStdout(bufferedFile, cfg.AlsoStdout)，stdout 始终同步写。
//
// 缓冲收益边界：bufio 对「本次写入大于缓冲可用空间且缓冲为空」的写直通底层，所以
// 大 payload（访问日志 body）不过缓冲，收益集中在「小日志高频」场景。
//
// 已知取舍：整点 rotateLoop 的 Rotate() 与缓冲叠加时，尚未 flush 的数据会在下一次
// flush 时落入 Rotate 之后的新文件（文件归属错位）——已确认接受。
func newFileWriteSyncer(w *lumberjack.Logger, cfg LoggerConfig, buffers *[]*zapcore.BufferedWriteSyncer) zapcore.WriteSyncer {
	file := zapcore.AddSync(w)
	if cfg.BufferSize <= 0 {
		return file
	}
	interval := cfg.BufferFlushInterval
	if interval <= 0 {
		interval = defaultBufferFlushInterval
	}
	buf := &zapcore.BufferedWriteSyncer{
		WS:            file,
		Size:          cfg.BufferSize,
		FlushInterval: interval,
	}
	*buffers = append(*buffers, buf)
	return buf
}

// initLoggerWithConfig 在 cfg.Dir 下构建全部 logger 并原子发布。
func initLoggerWithConfig(cfg LoggerConfig) {
	if cfg.Dir == "" {
		cfg.Dir = defaultLogDir
	}
	dir := cfg.Dir

	// 防御：若存在上一轮生命周期的 runtime（重复 init 场景），先停止它，
	// 避免旧的 rotate goroutine 与文件句柄泄漏。InitLogger 的 initOnce 保证
	// 生产路径只初始化一次；本防御服务于测试与非常规重复初始化。
	if old := runtimePtr.Load(); old != nil {
		old.stop()
	}

	var coreArr []zapcore.Core
	// 编码器
	encoderConfig := zap.NewProductionEncoderConfig()
	// TimeEncoderOfLayout：JSON encoder 走 AppendTimeLayout（写入内部 buf，避免 t.Format 分配）；
	// ConsoleEncoder 仍回退 Format，版式与原来一致（F-31 毫秒）。
	encoderConfig.EncodeTime = zapcore.TimeEncoderOfLayout(logTimeLayout)
	encoderConfig.EncodeLevel = zapcore.CapitalLevelEncoder
	// 硬性约定：zap 的 Encoder 非并发安全，每个 zapcore.NewCore 必须持有独立的
	// encoder.Clone()，禁止把同一个 encoder 实例跨 Core 共享（依据 zap 官方约定：
	// Encoder 不可并发使用，Core 需各自持有 Clone）。encoder 本身仅作 Clone 源，
	// 构造完成后不再被任何 Core 持有。
	var encoder zapcore.Encoder
	if cfg.JSON {
		encoder = zapcore.NewJSONEncoder(encoderConfig)
	} else {
		encoder = zapcore.NewConsoleEncoder(encoderConfig)
	}

	lowPriority := zap.LevelEnablerFunc(func(lvl zapcore.Level) bool {
		return lvl < zap.WarnLevel
	})
	highPriority := zap.LevelEnablerFunc(func(lvl zapcore.Level) bool {
		return lvl >= zap.WarnLevel
	})

	// buffers 收集本次初始化创建的全部文件侧缓冲，交给 loggerRuntime.Stop 统一回收。
	var buffers []*zapcore.BufferedWriteSyncer

	infoLoggerWriter := &lumberjack.Logger{
		Filename:   getAbsPath(filepath.Join(dir, "info", "info.log")),
		MaxSize:    30,
		MaxAge:     7,
		MaxBackups: 169,
		LocalTime:  true,
		Compress:   cfg.Compress,
	}
	infoFileWriteSyncer := newFileWriteSyncer(infoLoggerWriter, cfg, &buffers)
	infoFileCore := zapcore.NewCore(encoder.Clone(), combineStdout(infoFileWriteSyncer, cfg.AlsoStdout), lowPriority)

	errorLoggerWriter := &lumberjack.Logger{
		Filename:   getAbsPath(filepath.Join(dir, "error", "error.log")),
		MaxSize:    30,
		MaxAge:     14,
		MaxBackups: 420,
		LocalTime:  true,
		Compress:   cfg.Compress,
	}
	errorFileWriteSyncer := newFileWriteSyncer(errorLoggerWriter, cfg, &buffers)
	errorFileCore := zapcore.NewCore(encoder.Clone(), combineStdout(errorFileWriteSyncer, cfg.AlsoStdout), highPriority)

	coreArr = append(coreArr, infoFileCore, errorFileCore)
	baseOpts := make([]zap.Option, 0, 1)
	if !cfg.DisableCaller {
		baseOpts = append(baseOpts, zap.AddCaller())
	}
	base := zap.New(zapcore.NewTee(coreArr...), baseOpts...)
	baseLogPtr.Store(base)
	// Info/Warn/Error 包一层函数，Sugar 需要 Skip(1) 才能指到业务行号；
	// GetLogger() 返回的 base 不加 Skip，避免业务直接 Info 时多跳一帧。
	if cfg.DisableCaller {
		logPtr.Store(base.Sugar())
	} else {
		logPtr.Store(base.WithOptions(zap.AddCallerSkip(1)).Sugar())
	}

	accessLoggerWriter := &lumberjack.Logger{
		Filename:   getAbsPath(filepath.Join(dir, "access", "access.log")),
		MaxSize:    30,
		MaxAge:     7,
		MaxBackups: 169,
		LocalTime:  true,
		Compress:   cfg.Compress,
	}
	accessFileWriteSyncer := newFileWriteSyncer(accessLoggerWriter, cfg, &buffers)
	accessFileCore := zapcore.NewCore(encoder.Clone(), combineStdout(accessFileWriteSyncer, cfg.AlsoStdout), zap.InfoLevel)
	accessLogPtr.Store(zap.New(accessFileCore))

	panicLoggerWriter := &lumberjack.Logger{
		Filename:   getAbsPath(filepath.Join(dir, "panic", "panic.log")),
		MaxSize:    30,
		MaxAge:     14,
		MaxBackups: 420,
		LocalTime:  true,
		Compress:   cfg.Compress,
	}
	panicFileWriteSyncer := newFileWriteSyncer(panicLoggerWriter, cfg, &buffers)
	panicFileCore := zapcore.NewCore(encoder.Clone(), combineStdout(panicFileWriteSyncer, cfg.AlsoStdout), zap.InfoLevel)
	recoveryLogPtr.Store(zap.New(panicFileCore))

	dataFileLoggerWriter := &lumberjack.Logger{
		Filename:   getAbsPath(filepath.Join(dir, "dal", "dal.log")),
		MaxSize:    30,
		MaxAge:     7,
		MaxBackups: 169,
		LocalTime:  true,
		Compress:   cfg.Compress,
	}
	dataFileWriteSyncer := newFileWriteSyncer(dataFileLoggerWriter, cfg, &buffers)
	dataFileCore := zapcore.NewCore(encoder.Clone(), zapcore.NewMultiWriteSyncer(dataFileWriteSyncer), zap.InfoLevel)
	dalLogPtr.Store(zap.New(dataFileCore))

	rt := &loggerRuntime{
		writers: []*lumberjack.Logger{
			infoLoggerWriter,
			errorLoggerWriter,
			accessLoggerWriter,
			panicLoggerWriter,
			dataFileLoggerWriter,
		},
		buffers:      buffers,
		rotateDone:   make(chan struct{}),
		rotateExited: make(chan struct{}),
	}
	runtimePtr.Store(rt)
	go rt.rotateLoop()
}

func Info(args ...any) {
	if l := logPtr.Load(); l != nil {
		l.Info(args...)
	}
}

func Warn(args ...any) {
	if l := logPtr.Load(); l != nil {
		l.Warn(args...)
	}
}

func Error(args ...any) {
	if l := logPtr.Load(); l != nil {
		l.Error(args...)
	}
}

func Infof(template string, args ...any) {
	if l := logPtr.Load(); l != nil {
		l.Infof(template, args...)
	}
}

func Warnf(template string, args ...any) {
	if l := logPtr.Load(); l != nil {
		l.Warnf(template, args...)
	}
}

func Errorf(template string, args ...any) {
	if l := logPtr.Load(); l != nil {
		l.Errorf(template, args...)
	}
}

// StackedError 打印带堆栈的错误；err 为 nil 时静默丢弃（P3-16）。
func StackedError(err error) {
	if err == nil {
		return
	}
	if l := logPtr.Load(); l != nil {
		l.Error(fmt.Sprintf("[%+v]", err))
	}
}

// GetLogger 返回未加 CallerSkip 的 *zap.Logger，供热路径用结构化字段打点。
// 未 Init 时返回 nil。包级 Info/Infof 仍走 SugaredLogger。
func GetLogger() *zap.Logger {
	return baseLogPtr.Load()
}

func GetAccessLog() *zap.Logger {
	return accessLogPtr.Load()
}

func GetRecoveryLog() *zap.Logger {
	return recoveryLogPtr.Load()
}

func GetDalLog() *zap.Logger {
	return dalLogPtr.Load()
}

func rotateIfNotEmpty(writer *lumberjack.Logger) {
	// 检查文件是否存在且不为空（不存在/为空静默跳过，行为与现状一致）
	info, err := os.Stat(writer.Filename)
	if err != nil || info.Size() == 0 {
		return
	}
	if err := writer.Rotate(); err != nil {
		Warnf("logger: rotate %s failed: %v", writer.Filename, err)
	}
}

func getAbsPath(path string) string {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return absPath
}
