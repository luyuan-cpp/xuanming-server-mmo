package main

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "proto/guild"
	"shared/killswitch"
)

// chainUnary 按 gRPC 的语义把一组一元拦截器串成一个 handler:
// 切片里第一个是最外层,最后一个紧贴业务 handler。
//
// grpc-go 自己的 chainUnaryInterceptors 是包内私有函数,拿不到,
// 所以这里复刻一份(语义与 grpc.ChainUnaryInterceptor 的文档一致:
// "The first interceptor will be the outer most")。
// 这个副本在 friend / player_locator / scene_manager 的同名测试里逐字相同 ——
// 四个服务是四个独立 go module,没有共同的测试工具包可放。
func chainUnary(
	interceptors []grpc.UnaryServerInterceptor,
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) grpc.UnaryHandler {
	next := handler
	for i := len(interceptors) - 1; i >= 0; i-- {
		cur, downstream := interceptors[i], next
		next = func(ctx context.Context, req any) (any, error) {
			return cur(ctx, req, info, downstream)
		}
	}
	return next
}

// TestKillSwitchWiredIntoUnaryChain 证明热关停**确实挂在**本服务的拦截器链上。
//
// 为什么要有这条测试:killswitch 是"平时完全没有可观测行为"的组件 ——
// 没写规则时它和不存在一模一样。所以一旦哪次重构把 buildUnaryInterceptors 里
// 那一行删掉,所有既有测试、所有联调、所有压测都照样全绿,只有真出事那天、
// 运维敲下 etcdctl put 却发现流量纹丝不动时才会发现止血阀是假的。
// 这条测试就是那一行的唯一守卫:删掉 ks.UnaryServerInterceptor() 它立刻 FAIL。
//
// 覆盖边界(如实说明):它验证的是"链的组装正确",不是"main() 调了
// AddUnaryInterceptors"。后者是 main 里的一行、无法在单测中执行(需要真实
// etcd + 端口 + zrpc 配置),只能靠 review 保证。
func TestKillSwitchWiredIntoUnaryChain(t *testing.T) {
	ks := killswitch.New(killswitch.Config{})
	// key 是 killswitch 的**匹配模式**(全限定服务名/方法名),不是 etcd 的完整 key。
	// 这里刻意从生成代码里取方法名:模式写错(比如把 guildpb 写成 guild)
	// 是运维最容易犯的错,用常量拼能顺带钉住"服务名到底叫什么"。
	blocked := strings.TrimPrefix(pb.GuildService_CreateGuild_FullMethodName, "/")
	ks.SetRules(map[string]killswitch.Rule{
		blocked: {Deny: true, Reason: "单测:建帮把库打爆了"},
	})

	chain := buildUnaryInterceptors(ks)

	t.Run("命中规则的方法被短路", func(t *testing.T) {
		handlerCalled := false
		info := &grpc.UnaryServerInfo{FullMethod: pb.GuildService_CreateGuild_FullMethodName}
		h := chainUnary(chain, info, func(ctx context.Context, req any) (any, error) {
			handlerCalled = true
			return &pb.CreateGuildResponse{}, nil
		})

		resp, err := h(context.Background(), &pb.CreateGuildRequest{})
		if handlerCalled {
			t.Fatal("handler 被调用了:killswitch 没有挂上拦截器链(或不在 handler 上游)")
		}
		if resp != nil {
			t.Fatalf("被关停的调用不该有响应体,得到 %#v", resp)
		}
		st, ok := status.FromError(err)
		if !ok || st.Code() != codes.Unavailable {
			t.Fatalf("期望 gRPC Unavailable,得到 %v", err)
		}
		// 错误文本必须带方法名与原因:客户端日志里要能一眼看出
		// "是被关停了,不是超时"。
		if !strings.Contains(st.Message(), "CreateGuild") ||
			!strings.Contains(st.Message(), "建帮把库打爆了") {
			t.Fatalf("错误文本缺少方法名/原因: %q", st.Message())
		}
	})

	t.Run("未命中的方法照常放行", func(t *testing.T) {
		handlerCalled := false
		info := &grpc.UnaryServerInfo{FullMethod: pb.GuildService_GetGuild_FullMethodName}
		h := chainUnary(chain, info, func(ctx context.Context, req any) (any, error) {
			handlerCalled = true
			return &pb.GetGuildResponse{}, nil
		})

		if _, err := h(context.Background(), &pb.GetGuildRequest{}); err != nil {
			t.Fatalf("未命中规则的方法不该报错: %v", err)
		}
		if !handlerCalled {
			t.Fatal("未命中规则的方法必须走到 handler")
		}
	})
}

// TestKillSwitchFailOpenWithoutRules 钉住铁律 fail-open 的入口那一侧:
// 没有任何规则(= etcd 没配 / 连不上 / 前缀下是空的)时,链必须完全透明。
// 这条测试保证"挂上开关"这件事本身不会给正常流量带来任何行为变化。
func TestKillSwitchFailOpenWithoutRules(t *testing.T) {
	ks := killswitch.New(killswitch.Config{})
	// 刻意不调 SetRules,并用 nil 客户端 Start —— 模拟没接 etcd 的部署。
	ks.Start(context.Background(), nil)

	handlerCalled := false
	info := &grpc.UnaryServerInfo{FullMethod: pb.GuildService_CreateGuild_FullMethodName}
	h := chainUnary(buildUnaryInterceptors(ks), info, func(ctx context.Context, req any) (any, error) {
		handlerCalled = true
		return &pb.CreateGuildResponse{}, nil
	})

	if _, err := h(context.Background(), &pb.CreateGuildRequest{}); err != nil {
		t.Fatalf("无规则时必须放行: %v", err)
	}
	if !handlerCalled {
		t.Fatal("无规则时 handler 必须被调用(fail-open)")
	}
}
