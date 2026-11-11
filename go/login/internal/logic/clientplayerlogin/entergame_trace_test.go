package clientplayerloginlogic

import (
	"testing"

	base "proto/common/base"
	loginpb "proto/login"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace"
)

// EnterGame 的后台链保留请求 ctx 的值(docs/design/distributed-tracing.md §6.2):请求里的 trace 一路带到发给 gate 的
// Bind 命令,gate 处理绑定时接上同一次 EnterGame。链 ctx 若退回 context.Background(),本用例变红。
// 搭法同 enter_online_identity_test.go:完整走一遍异步入场,只把 Bind 换成记录 ctx 的假实现。
func TestEnterGameChainCarriesRequestTraceToBind(t *testing.T) {
	h := newEnterWiringHarness(t, []*base.AccountSimplePlayer{{PlayerId: identityPlayerID, ClassId: 3, Name: "云中君"}}, nil)
	h.l.svcCtx.PlayerLocatorClient = &kickLocator{events: &kickEventLog{}}
	binder := &bindRecorder{}
	h.l.binder = binder
	h.l.svcCtx.SceneManagerClient = &acceptedIdentityScene{}

	traceID, err := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	require.NoError(t, err)
	spanID, err := trace.SpanIDFromHex("00f067aa0ba902b7")
	require.NoError(t, err)
	h.l.ctx = trace.ContextWithSpanContext(h.l.ctx, trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled, Remote: true,
	}))

	resp, err := h.l.EnterGame(&loginpb.EnterGameRequest{PlayerId: identityPlayerID, RequestId: "trace"})
	require.NoError(t, err)
	require.Nil(t, resp.GetErrorMessage())
	h.waitChain(t)

	require.Equal(t, 1, binder.calls)
	require.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", binder.traceID,
		"the async chain must keep the request trace so the gate-side Bind continues the same trace")
}
