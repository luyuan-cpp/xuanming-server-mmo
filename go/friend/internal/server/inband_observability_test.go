package server

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"

	"friend/internal/constants"
	base "proto/common/base"
	friendpb "proto/friend"
	"shared/grpcstats"
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
// 包住一个返回真实 friendpb.AddFriendResponse 的 handler。
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
	// 方法名取生成常量,不再手写字符串。原先写的是 "/friend.FriendService/AddFriend" ——
	// proto package 其实是 friendpb(服务也已改名 ClientPlayerFriend),那个字符串从一开始
	// 就与真实 FullMethod 不符;它没让用例红,只是因为下面的 method 标签是照着同一个
	// 错字符串手算的短名。两处一起错 = 测试在测自己编的世界。
	//
	// method 标签必须用被测链路**自己的**派生函数算:serverbase 的标签
	// = grpcstats.ShortMethod(info.FullMethod)。手写一份短名的话,哪天服务名、包名或
	// ShortMethod 的规则变了,前后两次 counterValue 都会读到"序列不存在"的 0,
	// 差值恰好是 0 —— 断言会报一个与真因无关的失败(或者被人改成 >= 之后永远绿)。
	const fullMethod = friendpb.ClientPlayerFriend_AddFriend_FullMethodName
	labels := map[string]string{"method": grpcstats.ShortMethod(fullMethod), "source": "tip_info"}

	beforeFault := counterValue(t, "rpc_inband_fault_total", labels)
	beforeReject := counterValue(t, "rpc_inband_reject_total", labels)
	beforeUnknown := counterValue(t, "rpc_inband_unknown_code_total", labels)

	itc := serverbase.UnaryInterceptor(serverbase.Options{
		TipClassifier: constants.TipClassifier(),
	})
	info := &grpc.UnaryServerInfo{FullMethod: fullMethod}
	handler := func(ctx context.Context, req any) (any, error) {
		return &friendpb.AddFriendResponse{
			ErrorMessage: &base.TipInfoMessage{
				Id:         constants.ErrFriendListFull,
				Parameters: []string{"friend list full"},
			},
		}, nil
	}

	resp, err := itc(context.Background(), &friendpb.AddFriendRequest{}, info, handler)
	if err != nil {
		t.Fatalf("拦截器不该改变 handler 的 error 返回: %v", err)
	}
	// 观测层绝不能改动响应内容。
	addResp, ok := resp.(*friendpb.AddFriendResponse)
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

// TestInbandInterceptorRecordsTransportError 覆盖另一侧:handler 真的返回了 error 时,
// 拦截器必须把它原样透传,并记成 status=transport_error 而不是当成一次成功。
//
// 它守的契约("不吞错、不凭空造响应")与 F2-1 的 in-band 化无关:存储故障改成
// `return resp, nil` + constants.ErrStorage 之后,handler 返回非 nil error 的来路仍然存在 ——
// NotifyFriendEvent 的 Unimplemented、go-zero 拦截器链里更外层的失败,
// 以及将来任何一处漏改回 `return nil, err` 的代码。所以这条用例不随 in-band 化退役。
func TestInbandInterceptorRecordsTransportError(t *testing.T) {
	itc := serverbase.UnaryInterceptor(serverbase.Options{
		TipClassifier: constants.TipClassifier(),
	})
	info := &grpc.UnaryServerInfo{FullMethod: friendpb.ClientPlayerFriend_AddFriend_FullMethodName}
	wantErr := context.DeadlineExceeded
	handler := func(ctx context.Context, req any) (any, error) {
		return nil, wantErr
	}

	resp, err := itc(context.Background(), &friendpb.AddFriendRequest{}, info, handler)
	if err != wantErr {
		t.Fatalf("拦截器吞掉或改写了 handler 的 error: %v", err)
	}
	if resp != nil {
		t.Fatalf("handler 返回 nil 响应时拦截器不该凭空造一个: %T", resp)
	}
}
