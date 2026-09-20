package rpc

import (
	"context"
	"testing"

	"github.com/TomWu-Alchemi/project-framework/logger"
	"github.com/nats-io/nats.go/micro"
)

// A2 验收测试：recovery log 未注入（GetRecoveryLog() == nil）时，panic 恢复分支
// 回退全局 logger，并照常回发 500。
//
// 限制说明：本库全局 logger 的 Errorf 在未初始化时静默（logger 包内 logPtr 为 nil
// 时直接返回），且 InitLogger 是进程级一次性初始化，测试进程不保证已初始化，因此
// **无法在本用例中断言全局 logger 的调用或输出**。本用例只覆盖可观测契约：nil
// recovery log 下 panic 被 recover、不向调用方传播，且 500 响应已发送；回退分支
// 携带的 error(panic 值)/path/stack 由代码评审确认。
//
// -race 兼容：单 goroutine、无共享状态（本机 -race 不可用：0xc0000139，无法实测）。
func TestNatsRpcAccessLog_NilRecoveryLogFallsBackAndResponds500(t *testing.T) {
	if logger.GetRecoveryLog() != nil {
		t.Skip("recovery log initialized by earlier test; nil-fallback path not exercised")
	}

	h := NatsRpcAccessLog(func(context.Context, micro.Request) {
		panic("recovery-fallback-boom")
	})
	req := &fakeRequest{subject: "svc.fallback", data: []byte("payload-keep")}

	// 若回退分支再 panic，此处会直接失败（panic 蔓延到测试）。
	h(t.Context(), req)

	if !req.responded || req.errCode != "500" || req.errDesc != "internal error" {
		t.Fatalf("code=%q desc=%q responded=%v", req.errCode, req.errDesc, req.responded)
	}
}
