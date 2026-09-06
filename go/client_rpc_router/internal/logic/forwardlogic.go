// Package logic 是路由服的转发实现(docs/design/client-rpc-router.md §3)。
package logic

import (
	"context"
	"strings"
	"sync"
	"time"

	"client_rpc_router/generated/pb/game"
	"client_rpc_router/internal/constants"
	"client_rpc_router/internal/discovery"
	"client_rpc_router/internal/metrics"
	"client_rpc_router/internal/rawcodec"
	"client_rpc_router/internal/svc"

	pb "proto/client_rpc_router"
	base "proto/common/base"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// SessionMetaKey 是 gate 附带的会话身份 metadata(base64(SessionDetails)),
// 与 C++ kSessionBinMetaKey / login callerauth.MetaSessionDetail 同一字符串。
// 路由服不解析它:原值透传给目标、原值回写响应头(gate 的回包桥接按它找会话)。
const SessionMetaKey = "x-session-detail-bin"

// forwardMetaPrefix:透传给目标的 metadata 键前缀。设计契约要求目标 Go 服务
// 「看到的 metadata 与 gate 直连时完全相同」,所以按前缀整体透传而不是只挑
// x-session-detail-bin 一个键。**当前 C++ gate 并不签 x-caller-*(login 的
// callerauth 验签头)** —— 那是 gate 侧的既有缺口,dev 宽松档遮住了它;将来 gate
// 接上签名后,这些键按同一前缀原样透传即可,不需要改本文件。注意签名的
// canonical 里绑的是**目标业务方法**(如 /loginpb.ClientPlayerLogin/EnterGame),
// 不是本跳的 Forward,所以原样透传是正确做法。
// 按前缀整体透传应用层键;content-type / user-agent / grpc-* 等传输层键由本跳
// 自己生成,不透传(gRPC 也禁止调用方设置 grpc- 前缀键)。
const forwardMetaPrefix = "x-"

// ForwardLogic 处理一次 Forward。
type ForwardLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// NewForwardLogic 构造一次请求的处理器(与 match 的 logic 构造模式一致)。
func NewForwardLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ForwardLogic {
	return &ForwardLogic{ctx: ctx, svcCtx: svcCtx}
}

// Forward 严格按设计文档 §3 的六步执行;除「缺会话元数据」外一律以 MessageContent
// 信封回业务错误(gate 原样下发给客户端),不用 gRPC 错误:gate 的回包桥接只认
// 带 x-session-detail-bin 响应头的成功响应,gRPC 错误在 gate 侧无法路由回会话。
// sampledErrorf 是客户端 / 目标状态驱动的失败分支专用日志:这些分支的触发速率
// 完全由公网流量或对端故障决定,逐条 Errorf 会把故障放大成日志 I/O 风暴
// (gate 侧对同类拒绝走的就是 1/1024 采样,client_message_processor.cpp)。
// 每个 key(outcome)首条必打,之后每 1024 条打一行并带累计数;真正的计数
// 权威是 client_rpc_router_forward_total{message_id,outcome} 指标,日志只留样本。
func sampledErrorf(key, format string, args ...any) {
	n := sampleCounters.Add(key)
	if (n-1)%logSampleEvery != 0 {
		return
	}
	logx.Errorf(format+" (采样 1/%d, %s 累计=%d)", append(append([]any{}, args...), logSampleEvery, key, n)...)
}

// logSampleEvery 与 gate 的拒绝日志采样同口径(每 1024 条一行)。
const logSampleEvery uint64 = 1024

// sampleCounters 按 outcome 分别计数:一类噪音(比如目标全下线)不会把另一类
// (比如路由表版本不一致)淹没掉。
var sampleCounters = newCounterSet()

type counterSet struct {
	mu sync.Mutex
	m  map[string]uint64
}

func newCounterSet() *counterSet { return &counterSet{m: make(map[string]uint64)} }

// Add 自增并返回自增后的值(从 1 开始)。
func (c *counterSet) Add(key string) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[key]++
	return c.m[key]
}

func (l *ForwardLogic) Forward(in *pb.ForwardRequest) (*base.MessageContent, error) {
	start := time.Now()

	// 1) 会话元数据:gate 一定会带;缺失说明不是 gate 打来的,直接 UNAUTHENTICATED。
	incoming, _ := metadata.FromIncomingContext(l.ctx)
	sessionValues := incoming.Get(SessionMetaKey)
	if len(sessionValues) == 0 {
		metrics.ObserveForward(metrics.MessageIdLabel(in.GetRequest().GetMessageId(), false),
			metrics.OutcomeMissingSession, time.Since(start))
		return nil, status.Error(codes.Unauthenticated, "缺少 "+SessionMetaKey+" 会话元数据")
	}
	// 6) 响应头回写:无论后面成败都要有,gate 靠它找回会话。SetHeader 只是登记,
	//    随首个响应帧一起发出,所以放在最前面一次登记即可。
	l.echoSessionHeader(sessionValues)

	request := in.GetRequest()
	if request == nil {
		metrics.ObserveForward(metrics.MessageIdLabel(0, false), metrics.OutcomeInvalidRequest, time.Since(start))
		return rejected(request, constants.ErrInvalidParameter), nil
	}

	// 2) 查生成路由表。
	route, known := game.LookupRoute(request.MessageId)
	messageIdLabel := metrics.MessageIdLabel(request.MessageId, known)
	finish := func(outcome string) {
		metrics.ObserveForward(messageIdLabel, outcome, time.Since(start))
	}
	if !known {
		finish(metrics.OutcomeUnknownMessage)
		sampledErrorf(metrics.OutcomeUnknownMessage,
			"[forward] 消息号 %d 不在路由表(gate 与路由服生成物版本不一致?) id=%d",
			request.MessageId, request.Id)
		return rejected(request, constants.ErrInvalidParameter), nil
	}
	if !route.ClientProtocol {
		finish(metrics.OutcomeNotClientProtocol)
		sampledErrorf(metrics.OutcomeNotClientProtocol,
			"[forward] 消息号 %d(%s)不是客户端协议,拒绝 id=%d",
			request.MessageId, route.FullMethod, request.Id)
		return rejected(request, constants.ErrInvalidParameter), nil
	}
	if route.NodeType == base.ENodeType_BattleNodeService {
		// D33:战斗只走直连;gate 路由模式下收到战斗消息即为收缩后的预期拒绝。
		finish(metrics.OutcomeBattleRejected)
		return rejected(request, constants.ErrServiceUnavailable), nil
	}

	// 3) 选实例。
	nodeTypeName := base.ENodeType_name[int32(route.NodeType)]
	watcher := l.svcCtx.Targets[route.NodeType]
	if watcher == nil {
		finish(metrics.OutcomeNoTarget)
		logx.Errorf("[forward] 目标类型 %s 没有发现镜像(路由表与 svc.TargetNodeTypes 不一致) message_id=%d",
			nodeTypeName, request.MessageId)
		return rejected(request, constants.ErrServiceUnavailable), nil
	}
	target, ok := l.pickTarget(watcher, route.NodeType, in.ZoneId)
	if !ok {
		finish(metrics.OutcomeNoTarget)
		sampledErrorf(metrics.OutcomeNoTarget,
			"[forward] 目标类型 %s 无可用实例 zone_scoped=%v zone_id=%d message_id=%d",
			nodeTypeName, l.svcCtx.IsZoneScoped(route.NodeType), in.ZoneId, request.MessageId)
		return rejected(request, constants.ErrServiceUnavailable), nil
	}

	// 4) 原始字节 Invoke,透传应用层 metadata,超时 ForwardTimeoutMs。
	conn, err := l.svcCtx.Dial(target.Endpoint)
	if err != nil {
		finish(metrics.OutcomeDialError)
		logx.Errorf("[forward] 拨号目标失败 %s(%s) message_id=%d: %v",
			nodeTypeName, target.Endpoint, request.MessageId, err)
		return rejected(request, constants.ErrServiceUnavailable), nil
	}
	callCtx, cancel := context.WithTimeout(l.ctx, l.svcCtx.ForwardTimeout)
	defer cancel()
	callCtx = metadata.NewOutgoingContext(callCtx, forwardableMetadata(incoming))

	var responseBytes []byte
	err = conn.Invoke(callCtx, route.FullMethod, request.Body, &responseBytes, grpc.ForceCodec(rawcodec.Codec{}))
	if err != nil {
		// 5) gRPC 错误 → kServiceUnavailable;code / 目标 / 消息号进日志,不进 label。
		code := status.Code(err)
		upstreamOutcome := metrics.OutcomeUpstreamError
		if code == codes.DeadlineExceeded {
			upstreamOutcome = metrics.OutcomeUpstreamTimeout
		}
		finish(upstreamOutcome)
		sampledErrorf(upstreamOutcome,
			"[forward] 目标返回 gRPC 错误 code=%s method=%s target=%s(%s zone=%d node=%d) message_id=%d id=%d: %v",
			code, route.FullMethod, target.Endpoint, nodeTypeName, target.ZoneId, target.NodeId,
			request.MessageId, request.Id, err)
		return rejected(request, constants.ErrServiceUnavailable), nil
	}

	finish(metrics.OutcomeOK)
	return &base.MessageContent{
		Id:                request.Id,
		MessageId:         request.MessageId,
		SerializedMessage: responseBytes,
	}, nil
}

// pickTarget 按 D33 选实例:zone-scoped 类型只挑与发起 gate 同 zone 的实例,
// 其余全局随机。
func (l *ForwardLogic) pickTarget(watcher *discovery.NodeWatcher, nodeType base.ENodeType, zoneId uint32) (discovery.NodeEntry, bool) {
	if l.svcCtx.IsZoneScoped(nodeType) {
		return watcher.PickRandomInZone(zoneId)
	}
	return watcher.PickRandom()
}

// echoSessionHeader 把 x-session-detail-bin 原值登记进响应头。
// 不在 gRPC 服务端流里(单测直接调 logic)时没有响应头可写,静默跳过。
func (l *ForwardLogic) echoSessionHeader(values []string) {
	if grpc.ServerTransportStreamFromContext(l.ctx) == nil {
		return
	}
	if err := grpc.SetHeader(l.ctx, metadata.MD{SessionMetaKey: values}); err != nil {
		logx.Errorf("[forward] 回写 %s 响应头失败(gate 将无法把回包送回会话): %v", SessionMetaKey, err)
	}
}

// forwardableMetadata 挑出要透传给目标的应用层 metadata(见 forwardMetaPrefix)。
func forwardableMetadata(incoming metadata.MD) metadata.MD {
	out := metadata.MD{}
	for key, values := range incoming {
		if strings.HasPrefix(key, forwardMetaPrefix) {
			out[key] = append([]string(nil), values...)
		}
	}
	return out
}

// rejected 组装带 tip 码的错误信封:id / message_id 照抄请求,gate 原样下发。
func rejected(request *base.ClientRequest, tipCode uint32) *base.MessageContent {
	return &base.MessageContent{
		Id:           request.GetId(),
		MessageId:    request.GetMessageId(),
		ErrorMessage: &base.TipInfoMessage{Id: tipCode},
	}
}
