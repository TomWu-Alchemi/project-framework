package logger

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"
)

// BenchmarkGormTrace_NoLog 度量 GORM 最热路径：默认 Warn 级别下「快速成功的
// SQL」——既不慢查询、也无错误，因此不应产生任何日志。
//
// 该场景用于对照 P-6 修复前后的开销：修复前 Trace 在 switch 之前无条件调用
// l.logger(ctx)（runtime.Caller 扫描 + Context 回调）；修复后应提前返回。
// fc 只返回固定 SQL，不触发真实查询；begin 每次取当前时间，保证 elapsed 恒为
// 微秒级，稳定落在「快查询、不打日志」分支。
func BenchmarkGormTrace_NoLog(b *testing.B) {
	l := NewGormLogger(zap.NewNop())
	fc := func() (string, int64) { return "SELECT 1", 1 }
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.Trace(ctx, time.Now(), fc, nil)
	}
}
