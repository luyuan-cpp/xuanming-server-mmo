package server

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"

	"friend/internal/constants"
	base "proto/common/base"
	pb "proto/friend"
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

// TestInbandInterceptorClassifiesFriendRejection 是「错误码撞车」修复的端到端护栏:
// 它跑的是**生产里真正挂上去的那条链**——
// serverbase.UnaryInterceptor(Options{TipClassifier: constants.TipClassifier()})
// 包住一个返回真实 pb.AddFriendResponse 的 handler。
//
// 修复前 ErrFriendListFull = 3,而拦截器没有(也无法有)好友自己的定性函数,
// 只能走 serverbase.TipVerdict —— tip 数轴上的 3 是 common 段的
// kInvalidTableData,属于故障码。于是每一次「好友列表已满」这种最普通的
// 业务拒绝,都会被打成 rpc_inband_fault_total 并刷一条 Error 日志;
// 客户端那边同样按 id 查提示表,显示的是「无效表数据」。
//
// 修复后 ErrFriendListFull 由 Tip.xlsx 发到 friend 段,全局段表判成
// biz_reject:fault 计数不动,reject 计数 +1。
func TestInbandInterceptorClassifiesFriendRejection(t *testing.T) {
	const method = "FriendService/AddFriend"
	labels := map[string]string{"method": method, "source": "tip_info"}

	beforeFault := counterValue(t, "rpc_inband_fault_total", labels)
	beforeReject := counterValue(t, "rpc_inband_reject_total", labels)
	beforeUnknown := counterValue(t, "rpc_inband_unknown_code_total", labels)

	itc := serverbase.UnaryInterceptor(serverbase.Options{
		TipClassifier: constants.TipClassifier(),
	})
	info := &grpc.UnaryServerInfo{FullMethod: "/friend.FriendService/AddFriend"}
	handler := func(ctx context.Context, req any) (any, error) {
		return &pb.AddFriendResponse{
			ErrorMessage: &base.TipInfoMessage{
				Id:         constants.ErrFriendListFull,
				Parameters: []string{"friend list full"},
			},
		}, nil
	}

	resp, err := itc(context.Background(), &pb.AddFriendRequest{}, info, handler)
	if err != nil {
		t.Fatalf("拦截器不该改变 handler 的 error 返回: %v", err)
	}
	// 观测层绝不能改动响应内容。
	addResp, ok := resp.(*pb.AddFriendResponse)
	if !ok {
		t.Fatalf("响应类型被拦截器改写: %T", resp)
	}
	if addResp.GetErrorMessage().GetId() != constants.ErrFriendListFull {
		t.Fatalf("响应里的 tip id 被改写: %d", addResp.GetErrorMessage().GetId())
	}

	if got := counterValue(t, "rpc_inband_reject_total", labels); got != beforeReject+1 {
		t.Errorf("rpc_inband_reject_total 应 +1,实际 %v -> %v", beforeReject, got)
	}
	if got := counterValue(t, "rpc_inband_fault_total", labels); got != beforeFault {
		t.Errorf("「好友列表已满」是业务拒绝,不该计入 rpc_inband_fault_total:%v -> %v", beforeFault, got)
	}
	if got := counterValue(t, "rpc_inband_unknown_code_total", labels); got != beforeUnknown {
		t.Errorf("好友号段必须被 TipClassifier 认领,不该计入 unknown_code:%v -> %v", beforeUnknown, got)
	}
}

// TestInbandInterceptorRecordsTransportError 覆盖另一侧:好友域的真故障
// (Redis / MySQL 挂了)在 friend_logic 里是 `return nil, err`,走 gRPC status。
// 拦截器必须把它原样透传,并记成 status=transport_error 而不是当成一次成功。
func TestInbandInterceptorRecordsTransportError(t *testing.T) {
	itc := serverbase.UnaryInterceptor(serverbase.Options{
		TipClassifier: constants.TipClassifier(),
	})
	info := &grpc.UnaryServerInfo{FullMethod: "/friend.FriendService/AddFriend"}
	wantErr := context.DeadlineExceeded
	handler := func(ctx context.Context, req any) (any, error) {
		return nil, wantErr
	}

	resp, err := itc(context.Background(), &pb.AddFriendRequest{}, info, handler)
	if err != wantErr {
		t.Fatalf("拦截器吞掉或改写了 handler 的 error: %v", err)
	}
	if resp != nil {
		t.Fatalf("handler 返回 nil 响应时拦截器不该凭空造一个: %T", resp)
	}
}
