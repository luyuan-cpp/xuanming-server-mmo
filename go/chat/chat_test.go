package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"chat/internal/constants"
	"chat/internal/session"

	chatpb "proto/chat"
	base "proto/common/base"

	"shared/generated/tip"
	"shared/killswitch"
	"shared/serverbase"

	"github.com/zeromicro/go-zero/core/logx/logtest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// chainUnary 按 gRPC 的语义把一组一元拦截器串成一个 handler:切片里第一个是最外层。
// grpc-go 的 chainUnaryInterceptors 是包内私有函数,这里复刻一份(与 friend / guild /
// player_locator / scene_manager 的同名测试逐字相同 —— 各服务是独立 module,没有共享测试包)。
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

// incomingSessionContext 模拟路由服透传过来的 x-session-detail-bin(base64(SessionDetails))。
func incomingSessionContext(t *testing.T, playerId uint64) context.Context {
	t.Helper()
	bin, err := proto.Marshal(&base.SessionDetails{PlayerId: playerId})
	if err != nil {
		t.Fatalf("序列化 SessionDetails 失败: %v", err)
	}
	md := metadata.Pairs(session.MetadataKey, base64.StdEncoding.EncodeToString(bin))
	return metadata.NewIncomingContext(context.Background(), md)
}

// TestBuildUnaryInterceptorsOrder 钉住链长与顺序:grpcstats → killswitch → session → serverbase。
//
// 函数值不能比较,拿符号名判定又会被跨包内联改名打败,所以按**可观测行为**逐个探测:
// 对每个位置单独调用那一个拦截器,handler 返回一个带 fault 码的响应,并带上合法会话头 +
// 命中的关停规则。于是:
//   - killswitch:唯一不调 handler、回 Unavailable 的;
//   - session:唯一让 handler 从 ctx 取到会话的;
//   - serverbase:唯一把 fault 码打成 rpc_inband_fault 日志的;
//   - grpcstats:未开启采集时完全透明(以上三件事都不做)—— 剩下的那个位置就是它。
func TestBuildUnaryInterceptorsOrder(t *testing.T) {
	ks := killswitch.New(killswitch.Config{})
	ks.SetRules(map[string]killswitch.Rule{
		strings.TrimPrefix(chatpb.ClientPlayerChat_SendChat_FullMethodName, "/"): {Deny: true, Reason: "单测:顺序探针"},
	})
	chain := buildUnaryInterceptors(ks)
	if len(chain) != 4 {
		t.Fatalf("拦截器链长度应为 4(grpcstats/killswitch/session/serverbase),实际 %d", len(chain))
	}

	type observed struct {
		handlerCalled bool
		blocked       bool
		sawSession    bool
		loggedFault   bool
	}
	want := []struct {
		name string
		obs  observed
	}{
		{"grpcstats", observed{handlerCalled: true}},
		{"killswitch", observed{blocked: true}},
		{"session", observed{handlerCalled: true, sawSession: true}},
		{"serverbase", observed{handlerCalled: true, loggedFault: true}},
	}

	info := &grpc.UnaryServerInfo{FullMethod: chatpb.ClientPlayerChat_SendChat_FullMethodName}
	faultResp := &chatpb.SendChatResponse{ErrorMessage: &base.TipInfoMessage{Id: constants.ErrStorage}}

	for i, w := range want {
		t.Run(w.name, func(t *testing.T) {
			logs := logtest.NewCollector(t)
			var got observed
			_, err := chain[i](incomingSessionContext(t, 42), &chatpb.SendChatRequest{}, info,
				func(ctx context.Context, req any) (any, error) {
					got.handlerCalled = true
					_, got.sawSession = session.GetSessionDetails(ctx)
					return faultResp, nil
				})
			if st, ok := status.FromError(err); err != nil && ok && st.Code() == codes.Unavailable {
				got.blocked = true
			}
			got.loggedFault = strings.Contains(logs.String(), serverbase.EventInbandFault)
			if got != w.obs {
				t.Fatalf("位置 %d 应是 %s,行为不符: 期望 %+v,实际 %+v", i, w.name, w.obs, got)
			}
		})
	}
}

// TestKillSwitchWiredIntoUnaryChain 证明热关停**确实挂在**本服务的拦截器链上(D-1 验收第四条)。
// 删掉 buildUnaryInterceptors 里的 ks.UnaryServerInterceptor(),这条测试立刻 FAIL。
// 覆盖边界:验证的是"链的组装正确",不是"main() 调了 AddUnaryInterceptors"(后者靠 review)。
func TestKillSwitchWiredIntoUnaryChain(t *testing.T) {
	ks := killswitch.New(killswitch.Config{})
	// key 是匹配模式(全限定服务名/方法名),从生成代码取:顺带钉住服务名是 chatpb.ClientPlayerChat,
	// 与 etc/chat.yaml 里给运维的 etcdctl 示例一致。
	blocked := strings.TrimPrefix(chatpb.ClientPlayerChat_SendChat_FullMethodName, "/")
	ks.SetRules(map[string]killswitch.Rule{
		blocked: {Deny: true, Reason: "单测:聊天刷屏"},
	})
	chain := buildUnaryInterceptors(ks)

	t.Run("命中规则的方法被短路", func(t *testing.T) {
		handlerCalled := false
		info := &grpc.UnaryServerInfo{FullMethod: chatpb.ClientPlayerChat_SendChat_FullMethodName}
		h := chainUnary(chain, info, func(ctx context.Context, req any) (any, error) {
			handlerCalled = true
			return &chatpb.SendChatResponse{}, nil
		})

		resp, err := h(incomingSessionContext(t, 42), &chatpb.SendChatRequest{})
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
		if !strings.Contains(st.Message(), "SendChat") || !strings.Contains(st.Message(), "聊天刷屏") {
			t.Fatalf("错误文本缺少方法名/原因: %q", st.Message())
		}
	})

	t.Run("未命中的方法照常放行", func(t *testing.T) {
		handlerCalled := false
		info := &grpc.UnaryServerInfo{FullMethod: chatpb.ClientPlayerChat_PullChatHistory_FullMethodName}
		h := chainUnary(chain, info, func(ctx context.Context, req any) (any, error) {
			handlerCalled = true
			if _, ok := session.GetSessionDetails(ctx); !ok {
				t.Error("整条链走完后 handler 应能取到会话")
			}
			return &chatpb.PullChatHistoryResponse{}, nil
		})
		if _, err := h(incomingSessionContext(t, 42), &chatpb.PullChatHistoryRequest{}); err != nil {
			t.Fatalf("未命中规则的方法不该报错: %v", err)
		}
		if !handlerCalled {
			t.Fatal("未命中规则的方法必须走到 handler")
		}
	})
}

// TestKillSwitchFailOpenWithoutRules:没有任何规则(etcd 没配 / 连不上 / 前缀下为空)时链必须完全透明。
func TestKillSwitchFailOpenWithoutRules(t *testing.T) {
	ks := killswitch.New(killswitch.Config{})
	ks.Start(context.Background(), nil) // 模拟 etcd 客户端为 nil 的部署

	handlerCalled := false
	info := &grpc.UnaryServerInfo{FullMethod: chatpb.ClientPlayerChat_SendChat_FullMethodName}
	h := chainUnary(buildUnaryInterceptors(ks), info, func(ctx context.Context, req any) (any, error) {
		handlerCalled = true
		return &chatpb.SendChatResponse{}, nil
	})
	if _, err := h(incomingSessionContext(t, 42), &chatpb.SendChatRequest{}); err != nil {
		t.Fatalf("无规则时必须放行: %v", err)
	}
	if !handlerCalled {
		t.Fatal("无规则时 handler 必须被调用(fail-open)")
	}
}

// TestSessionInterceptorFailOpen:缺头 / 坏头放行不拒(由逻辑层回 kInvalidParameter),合法头解出会话。
func TestSessionInterceptorFailOpen(t *testing.T) {
	logtest.Discard(t) // 坏头会打 Error 日志,这里只关心行为

	interceptor := session.UnaryServerInterceptor()
	info := &grpc.UnaryServerInfo{FullMethod: chatpb.ClientPlayerChat_SendChat_FullMethodName}

	cases := []struct {
		name       string
		ctx        context.Context
		wantPlayer uint64 // 0 = 期望 ctx 里没有会话
	}{
		{"无 metadata", context.Background(), 0},
		{"有 metadata 但缺会话头", metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-other", "1")), 0},
		{"base64 坏", metadata.NewIncomingContext(context.Background(), metadata.Pairs(session.MetadataKey, "%%%不是base64")), 0},
		{"proto 坏", metadata.NewIncomingContext(context.Background(),
			metadata.Pairs(session.MetadataKey, base64.StdEncoding.EncodeToString([]byte{0xff, 0xff, 0xff}))), 0},
		{"合法会话头", incomingSessionContext(t, 1001), 1001},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handlerCalled := false
			_, err := interceptor(tc.ctx, &chatpb.SendChatRequest{}, info, func(ctx context.Context, req any) (any, error) {
				handlerCalled = true
				detail, ok := session.GetSessionDetails(ctx)
				if tc.wantPlayer == 0 && ok {
					t.Errorf("不该解出会话,得到 %+v", detail)
				}
				if tc.wantPlayer != 0 && (!ok || detail.GetPlayerId() != tc.wantPlayer) {
					t.Errorf("应解出 player=%d,得到 ok=%v detail=%+v", tc.wantPlayer, ok, detail)
				}
				return &chatpb.SendChatResponse{}, nil
			})
			if err != nil {
				t.Fatalf("会话拦截器不许拒绝请求: %v", err)
			}
			if !handlerCalled {
				t.Fatal("会话拦截器必须放行到 handler")
			}
		})
	}
}

// tipCodes 是 internal/constants 的全量 Err 常量清单(TestNoHandWrittenTipCodes 双向核对)。
func tipCodes() map[string]uint32 {
	return map[string]uint32{
		"ErrInvalidParameter":   constants.ErrInvalidParameter,
		"ErrMessageTooLong":     constants.ErrMessageTooLong,
		"ErrRateLimited":        constants.ErrRateLimited,
		"ErrChannelUnavailable": constants.ErrChannelUnavailable,
		"ErrStorage":            constants.ErrStorage,
	}
}

// TestNoHandWrittenTipCodes:Err 常量必须直接写成 uint32(table.CommonError_kX)(AGENTS.md §7.5)。
// 与 match / friend / client_rpc_router 的同名测试同一机制;chat 开私有段后把前缀换成 ChatError_。
func TestNoHandWrittenTipCodes(t *testing.T) {
	const path = "internal/constants/constants.go"
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("解析 %s 失败: %v", path, err)
	}

	want := tipCodes()
	seen := make(map[string]struct{}, len(want))
	ast.Inspect(file, func(node ast.Node) bool {
		valueSpec, ok := node.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for index, name := range valueSpec.Names {
			if !strings.HasPrefix(name.Name, "Err") {
				continue
			}
			seen[name.Name] = struct{}{}
			if index >= len(valueSpec.Values) {
				t.Errorf("%s 必须显式写生成枚举引用,不能省略 RHS 继承上一常量", name.Name)
				continue
			}
			if !isGeneratedTipRef(valueSpec.Values[index], "CommonError_") {
				t.Errorf("%s 必须直接写成 uint32(table.CommonError_kX),不能手写数字、别名或引用其他码轴", name.Name)
			}
		}
		return true
	})
	for name := range want {
		if _, ok := seen[name]; !ok {
			t.Errorf("tipCodes 声明了 %s,但 constants.go 没有对应 Err 常量", name)
		}
	}
	for name := range seen {
		if _, ok := want[name]; !ok {
			t.Errorf("constants.go 新增了 %s,必须同步纳入 tipCodes 的全量护栏", name)
		}
	}
}

func isGeneratedTipRef(expr ast.Expr, enumPrefix string) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok || len(call.Args) != 1 {
		return false
	}
	convert, ok := call.Fun.(*ast.Ident)
	if !ok || convert.Name != "uint32" {
		return false
	}
	selector, ok := call.Args[0].(*ast.SelectorExpr)
	if !ok || !strings.HasPrefix(selector.Sel.Name, enumPrefix) {
		return false
	}
	pkg, ok := selector.X.(*ast.Ident)
	return ok && pkg.Name == "table"
}

// TestTipCodesVerdicts:复用的码都在 common 段内、互不重复,且定性符合契约 §9 ——
// 只有 ErrStorage 是故障(告警),其余都是业务拒绝。若 Tip.xlsx 的 fault 列被改,这里先红。
func TestTipCodesVerdicts(t *testing.T) {
	var common tip.Segment
	for _, segment := range tip.Segments {
		if segment.Domain == "common" {
			common = segment
		}
	}
	if common.Width == 0 {
		t.Fatal("生成的段表里没有 common 段")
	}

	seen := make(map[uint32]string)
	for name, code := range tipCodes() {
		if code < common.Base || code >= common.Base+common.Width {
			t.Errorf("%s = %d 落在 common 段 [%d,%d) 之外", name, code, common.Base, common.Base+common.Width)
		}
		if previous, exists := seen[code]; exists {
			t.Errorf("tip 码 %d 被 %s 与 %s 同时使用", code, previous, name)
		}
		seen[code] = name

		wantVerdict := serverbase.VerdictBizReject
		if name == "ErrStorage" {
			wantVerdict = serverbase.VerdictFault
		}
		if got := constants.TipClassifier()(code); got != wantVerdict {
			t.Errorf("%s = %d 应判 %v,实际 %v", name, code, wantVerdict, got)
		}
	}
}

// TestNodeInfoValueMatchesRegistryContract:BuildValue 产物必须通过 noderegistry 的注册前校验
// (契约 §2:nodeId==nodeID、zoneId==ZoneId、nodeUuid 非空、endpoint.port / grpcEndpoint.port 非 0、
// protocolType 为 PROTOCOL_GRPC)。这里按 protojson 的 JSON 字段名解到镜像 struct,
// 与 go/client_rpc_router/internal/discovery/node_watcher.go 的读法一致。
func TestNodeInfoValueMatchesRegistryContract(t *testing.T) {
	build := nodeInfoValueBuilder(2, "10.0.0.8", 50700, 1_700_000_000)
	raw, err := build(7, "uuid-chat-test")
	if err != nil {
		t.Fatalf("BuildValue 失败: %v", err)
	}

	var mirror struct {
		NodeId       uint32 `json:"nodeId"`
		NodeType     uint32 `json:"nodeType"`
		ZoneId       uint32 `json:"zoneId"`
		NodeUuid     string `json:"nodeUuid"`
		ProtocolType uint32 `json:"protocolType"`
		Endpoint     struct {
			Ip   string `json:"ip"`
			Port uint32 `json:"port"`
		} `json:"endpoint"`
		GrpcEndpoint struct {
			Ip   string `json:"ip"`
			Port uint32 `json:"port"`
		} `json:"grpcEndpoint"`
	}
	if err := json.Unmarshal(raw, &mirror); err != nil {
		t.Fatalf("NodeInfo protojson 无法按镜像 struct 解析: %v\n%s", err, raw)
	}
	if mirror.NodeId != 7 || mirror.ZoneId != 2 || mirror.NodeUuid != "uuid-chat-test" {
		t.Fatalf("身份字段不符: %+v", mirror)
	}
	if mirror.NodeType != uint32(base.ENodeType_ChatNodeService) {
		t.Fatalf("nodeType 应为 ChatNodeService(%d),实际 %d", base.ENodeType_ChatNodeService, mirror.NodeType)
	}
	if mirror.ProtocolType != uint32(base.ENodeProtocolType_PROTOCOL_GRPC) {
		t.Fatalf("protocolType 应为 PROTOCOL_GRPC,实际 %d", mirror.ProtocolType)
	}
	if mirror.Endpoint.Port != 50700 || mirror.GrpcEndpoint.Port != 50700 ||
		mirror.Endpoint.Ip != "10.0.0.8" || mirror.GrpcEndpoint.Ip != "10.0.0.8" {
		t.Fatalf("端点字段不符: %+v", mirror)
	}
}
