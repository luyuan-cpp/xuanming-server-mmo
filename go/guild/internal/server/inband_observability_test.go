package server

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"

	"guild/internal/constants"
	base "proto/common/base"
	pb "proto/guild"
	"shared/serverbase"
)

// counterValue 读取默认 registry 里某个 counter 在给定 label 组合下的当前值。
// series 还没被创建时返回 0(counter 是 lazy 创建的)。
func counterValue(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather 指标失败: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
	metricLoop:
		for _, m := range mf.GetMetric() {
			got := make(map[string]string, len(m.GetLabel()))
			for _, l := range m.GetLabel() {
				got[l.GetName()] = l.GetValue()
			}
			for k, v := range labels {
				if got[k] != v {
					continue metricLoop
				}
			}
			return m.GetCounter().GetValue()
		}
	}
	return 0
}

// TestInbandInterceptorClassifiesGuildRejection 是「错误码撞车」修复的端到端护栏:
// 它跑的是**生产里真正挂上去的那条链**——
// serverbase.UnaryInterceptor(Options{TipClassifier: constants.TipClassifier()})
// 包住一个返回真实 pb.JoinGuildResponse 的 handler。
//
// 修复前 ErrGuildFull = 4,而拦截器没有(也无法有)公会自己的定性函数,
// 只能走 serverbase.TipVerdict —— tip 数轴上的 4 是 common 段的
// kServiceUnavailable,属于故障码。于是每一次「公会人数已满」这种最普通的
// 业务拒绝,都会被打成 rpc_inband_fault_total 并刷一条 Error 日志。
// 那正是撞车的实际代价:监控与告警读到的语义是错的。
//
// 修复后 ErrGuildFull 落在 200-219 私有段,由 constants.TipClassifier() 判成
// biz_reject:fault 计数不动,reject 计数 +1。
func TestInbandInterceptorClassifiesGuildRejection(t *testing.T) {
	const method = "GuildService/JoinGuild"
	faultLabels := map[string]string{"method": method, "source": "tip_info"}
	rejectLabels := map[string]string{"method": method, "source": "tip_info"}
	unknownLabels := map[string]string{"method": method, "source": "tip_info"}

	beforeFault := counterValue(t, "rpc_inband_fault_total", faultLabels)
	beforeReject := counterValue(t, "rpc_inband_reject_total", rejectLabels)
	beforeUnknown := counterValue(t, "rpc_inband_unknown_code_total", unknownLabels)

	itc := serverbase.UnaryInterceptor(serverbase.Options{
		TipClassifier: constants.TipClassifier(),
	})
	info := &grpc.UnaryServerInfo{FullMethod: "/guild.GuildService/JoinGuild"}
	handler := func(ctx context.Context, req any) (any, error) {
		return &pb.JoinGuildResponse{
			ErrorMessage: &base.TipInfoMessage{Id: constants.ErrGuildFull, Parameters: []string{"guild is full"}},
		}, nil
	}

	resp, err := itc(context.Background(), &pb.JoinGuildRequest{}, info, handler)
	if err != nil {
		t.Fatalf("拦截器不该改变 handler 的 error 返回: %v", err)
	}
	// 观测层绝不能改动响应内容。
	joinResp, ok := resp.(*pb.JoinGuildResponse)
	if !ok {
		t.Fatalf("响应类型被拦截器改写: %T", resp)
	}
	if joinResp.GetErrorMessage().GetId() != constants.ErrGuildFull {
		t.Fatalf("响应里的 tip id 被改写: %d", joinResp.GetErrorMessage().GetId())
	}

	if got := counterValue(t, "rpc_inband_reject_total", rejectLabels); got != beforeReject+1 {
		t.Errorf("rpc_inband_reject_total 应 +1,实际 %v -> %v", beforeReject, got)
	}
	if got := counterValue(t, "rpc_inband_fault_total", faultLabels); got != beforeFault {
		t.Errorf("「公会已满」是业务拒绝,不该计入 rpc_inband_fault_total:%v -> %v", beforeFault, got)
	}
	if got := counterValue(t, "rpc_inband_unknown_code_total", unknownLabels); got != beforeUnknown {
		t.Errorf("公会号段必须被 TipClassifier 认领,不该计入 unknown_code:%v -> %v", beforeUnknown, got)
	}
}

// TestInbandInterceptorClassifiesGuildFault 覆盖另一侧:发号器被 fence 是真故障,
// 必须进 rpc_inband_fault_total(label 带上具体码,便于直接配告警)。
func TestInbandInterceptorClassifiesGuildFault(t *testing.T) {
	const method = "GuildService/CreateGuild"
	faultLabels := map[string]string{
		"method": method,
		"source": "tip_info",
		"code":   "208", // constants.ErrIDGenUnavailable
	}
	beforeFault := counterValue(t, "rpc_inband_fault_total", faultLabels)

	itc := serverbase.UnaryInterceptor(serverbase.Options{
		TipClassifier: constants.TipClassifier(),
		// 故障日志在这条用例里是预期噪音,压掉只留指标。
		SuppressFaultLog: true,
	})
	info := &grpc.UnaryServerInfo{FullMethod: "/guild.GuildService/CreateGuild"}
	handler := func(ctx context.Context, req any) (any, error) {
		return &pb.CreateGuildResponse{
			ErrorMessage: &base.TipInfoMessage{Id: constants.ErrIDGenUnavailable},
		}, nil
	}

	if _, err := itc(context.Background(), &pb.CreateGuildRequest{}, info, handler); err != nil {
		t.Fatalf("拦截器不该改变 handler 的 error 返回: %v", err)
	}

	if got := counterValue(t, "rpc_inband_fault_total", faultLabels); got != beforeFault+1 {
		t.Errorf("ErrIDGenUnavailable 应计入 rpc_inband_fault_total{code=208}:%v -> %v", beforeFault, got)
	}
}
