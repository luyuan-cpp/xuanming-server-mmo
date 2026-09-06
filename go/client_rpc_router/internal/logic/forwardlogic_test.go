package logic_test

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"client_rpc_router/generated/pb/game"
	"client_rpc_router/internal/config"
	"client_rpc_router/internal/constants"
	"client_rpc_router/internal/discovery"
	"client_rpc_router/internal/logic"
	"client_rpc_router/internal/metrics"
	"client_rpc_router/internal/rawcodec"
	"client_rpc_router/internal/server"
	"client_rpc_router/internal/svc"

	pb "proto/client_rpc_router"
	base "proto/common/base"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// sessionValue 模拟 gate 附带的 base64(SessionDetails);路由服不解析,只要求原样往返。
const sessionValue = "CgQIARABEgIIAQ=="

// ---------------------------------------------------------------------------
// 假目标:bufconn 上的 gRPC 服务,用同一原始字节 codec 注册路由表里的方法。
// 记录收到的 body / metadata,把 body 前面拼上自己的标签原样返回,便于断言
// 「body 原样到达」「响应原样返回」以及 zone 过滤挑到了哪台。
// ---------------------------------------------------------------------------

type echoTarget struct {
	tag string
	lis *bufconn.Listener
	srv *grpc.Server

	// failWith 非 nil 时 handler 直接返回该 gRPC 错误;block 为 true 时阻塞到 ctx 结束。
	failWith error
	block    bool

	mu      sync.Mutex
	calls   int
	gotBody []byte
	gotMD   metadata.MD
}

func splitFullMethod(t *testing.T, fullMethod string) (service, method string) {
	t.Helper()
	trimmed := strings.TrimPrefix(fullMethod, "/")
	idx := strings.LastIndex(trimmed, "/")
	require.Greater(t, idx, 0, "非法 FullMethod: %s", fullMethod)
	return trimmed[:idx], trimmed[idx+1:]
}

func startEchoTarget(t *testing.T, tag string, fullMethod string) *echoTarget {
	t.Helper()
	service, method := splitFullMethod(t, fullMethod)
	et := &echoTarget{tag: tag, lis: bufconn.Listen(1 << 20)}
	et.srv = grpc.NewServer(grpc.ForceServerCodec(rawcodec.Codec{}))
	et.srv.RegisterService(&grpc.ServiceDesc{
		ServiceName: service,
		HandlerType: (*any)(nil),
		Methods:     []grpc.MethodDesc{{MethodName: method, Handler: et.handle}},
		Metadata:    "client_rpc_router_test",
	}, et)
	go func() { _ = et.srv.Serve(et.lis) }()
	t.Cleanup(func() {
		et.srv.Stop()
		_ = et.lis.Close()
	})
	return et
}

func (et *echoTarget) handle(_ any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
	var body []byte
	if err := dec(&body); err != nil {
		return nil, err
	}
	md, _ := metadata.FromIncomingContext(ctx)
	et.mu.Lock()
	et.calls++
	et.gotBody = append([]byte(nil), body...)
	et.gotMD = md.Copy()
	et.mu.Unlock()

	if et.failWith != nil {
		return nil, et.failWith
	}
	if et.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return append([]byte(et.tag+"|"), body...), nil
}

func (et *echoTarget) snapshot() (int, []byte, metadata.MD) {
	et.mu.Lock()
	defer et.mu.Unlock()
	return et.calls, et.gotBody, et.gotMD
}

// bufDialer 把 endpoint 名映射到 bufconn 监听器,替代 discovery.DialEndpoint。
type bufDialer struct {
	mu      sync.Mutex
	targets map[string]*bufconn.Listener
	conns   []*grpc.ClientConn
}

func newBufDialer(t *testing.T) *bufDialer {
	d := &bufDialer{targets: map[string]*bufconn.Listener{}}
	t.Cleanup(func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		for _, conn := range d.conns {
			_ = conn.Close()
		}
	})
	return d
}

func (d *bufDialer) add(endpoint string, target *echoTarget) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.targets[endpoint] = target.lis
}

func (d *bufDialer) dial(endpoint string) (*grpc.ClientConn, error) {
	d.mu.Lock()
	lis, ok := d.targets[endpoint]
	d.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("测试假目标不存在: %s", endpoint)
	}
	conn, err := grpc.NewClient("passthrough:///"+endpoint,
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.conns = append(d.conns, conn)
	d.mu.Unlock()
	return conn, nil
}

// newSvcCtx 组装不依赖 etcd 的上下文;forwardTimeoutMs 是向目标 Invoke 的超时。
func newSvcCtx(t *testing.T, dialer *bufDialer, forwardTimeoutMs int64) *svc.ServiceContext {
	t.Helper()
	c := config.Config{ForwardTimeoutMs: forwardTimeoutMs, ZoneScopedNodeTypes: []string{"LoginNodeService"}}
	sc, err := svc.Build(c, dialer.dial)
	require.NoError(t, err)
	return sc
}

func addNode(sc *svc.ServiceContext, nodeType base.ENodeType, nodeId, zoneId uint32, endpoint string) {
	key := fmt.Sprintf("%s.rpc/zone/%d/node_type/%d/node_id/%d", base.ENodeType_name[int32(nodeType)], zoneId, nodeType, nodeId)
	sc.Targets[nodeType].Upsert(key, discovery.NodeEntry{NodeId: nodeId, ZoneId: zoneId, Endpoint: endpoint})
}

// ctxWithSession 模拟 gate 打进来的 incoming metadata。
func ctxWithSession(extra ...string) context.Context {
	pairs := append([]string{logic.SessionMetaKey, sessionValue}, extra...)
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(pairs...))
}

func forwardReq(messageId uint32, id uint64, zoneId uint32, body []byte) *pb.ForwardRequest {
	return &pb.ForwardRequest{
		Request: &base.ClientRequest{Id: id, MessageId: messageId, Body: body},
		ZoneId:  zoneId,
	}
}

func requireEnvelopeError(t *testing.T, resp *base.MessageContent, wantId uint64, wantMessageId uint32, wantTip uint32) {
	t.Helper()
	require.NotNil(t, resp)
	require.Equal(t, wantId, resp.Id, "id 必须照抄请求")
	require.Equal(t, wantMessageId, resp.MessageId, "message_id 必须照抄请求")
	require.NotNil(t, resp.ErrorMessage)
	require.Equal(t, wantTip, resp.ErrorMessage.Id)
	require.Empty(t, resp.SerializedMessage)
}

// ---------------------------------------------------------------------------
// 错误信封:直接调 logic(无 gRPC 服务端流,响应头回写被静默跳过)。
// ---------------------------------------------------------------------------

// 缺 x-session-detail-bin:唯一走 gRPC 错误的分支(gate 侧本来也无法路由回会话)。
func TestForwardRejectsMissingSessionMetadata(t *testing.T) {
	sc := newSvcCtx(t, newBufDialer(t), 1000)
	resp, err := logic.NewForwardLogic(context.Background(), sc).
		Forward(forwardReq(game.MatchServiceJoinQueueMessageId, 1, 1, []byte("x")))
	require.Nil(t, resp)
	require.Equal(t, codes.Unauthenticated, status.Code(err))
}

func TestForwardUnknownMessageIdIsInvalidParameter(t *testing.T) {
	const unknownId = uint32(4_000_000_000)
	_, known := game.LookupRoute(unknownId)
	require.False(t, known, "测试前提:该消息号不在路由表")

	sc := newSvcCtx(t, newBufDialer(t), 1000)
	before := metrics.ForwardTotalValue(metrics.UnknownMessageIdLabel, metrics.OutcomeUnknownMessage)
	resp, err := logic.NewForwardLogic(ctxWithSession(), sc).Forward(forwardReq(unknownId, 7, 1, nil))
	require.NoError(t, err)
	requireEnvelopeError(t, resp, 7, unknownId, constants.ErrInvalidParameter)
	require.Equal(t, before+1, metrics.ForwardTotalValue(metrics.UnknownMessageIdLabel, metrics.OutcomeUnknownMessage),
		"未知消息号的 label 必须归一到 unknown,不能让任意数字进 label")
}

func TestForwardNilRequestIsInvalidParameter(t *testing.T) {
	sc := newSvcCtx(t, newBufDialer(t), 1000)
	resp, err := logic.NewForwardLogic(ctxWithSession(), sc).Forward(&pb.ForwardRequest{ZoneId: 1})
	require.NoError(t, err)
	requireEnvelopeError(t, resp, 0, 0, constants.ErrInvalidParameter)
}

// 路由表里有、但所属 service 不是客户端协议(内部 RPC):拒绝,不能让客户端借道打内部接口。
func TestForwardNonClientProtocolIsInvalidParameter(t *testing.T) {
	route, ok := game.LookupRoute(game.DataServiceLoadPlayerDataMessageId)
	require.True(t, ok)
	require.False(t, route.ClientProtocol, "测试前提:DataService 不是客户端协议")

	sc := newSvcCtx(t, newBufDialer(t), 1000)
	resp, err := logic.NewForwardLogic(ctxWithSession(), sc).
		Forward(forwardReq(game.DataServiceLoadPlayerDataMessageId, 8, 1, []byte("x")))
	require.NoError(t, err)
	requireEnvelopeError(t, resp, 8, game.DataServiceLoadPlayerDataMessageId, constants.ErrInvalidParameter)
}

// D33:战斗只走直连,目标为 Battle 的客户端协议消息回 kServiceUnavailable。
func TestForwardBattleTargetIsServiceUnavailable(t *testing.T) {
	route, ok := game.LookupRoute(game.BattleClientPlayerSubmitBattleActionMessageId)
	require.True(t, ok)
	require.True(t, route.ClientProtocol)
	require.Equal(t, base.ENodeType_BattleNodeService, route.NodeType)

	sc := newSvcCtx(t, newBufDialer(t), 1000)
	resp, err := logic.NewForwardLogic(ctxWithSession(), sc).
		Forward(forwardReq(game.BattleClientPlayerSubmitBattleActionMessageId, 9, 1, []byte("x")))
	require.NoError(t, err)
	requireEnvelopeError(t, resp, 9, game.BattleClientPlayerSubmitBattleActionMessageId, constants.ErrServiceUnavailable)
}

func TestForwardNoInstanceIsServiceUnavailable(t *testing.T) {
	sc := newSvcCtx(t, newBufDialer(t), 1000)
	resp, err := logic.NewForwardLogic(ctxWithSession(), sc).
		Forward(forwardReq(game.MatchServiceJoinQueueMessageId, 10, 1, []byte("x")))
	require.NoError(t, err)
	requireEnvelopeError(t, resp, 10, game.MatchServiceJoinQueueMessageId, constants.ErrServiceUnavailable)
}

// ---------------------------------------------------------------------------
// zone 过滤:login 是 zone-scoped,只挑与 ForwardRequest.zone_id 同 zone 的实例。
// ---------------------------------------------------------------------------

func TestForwardZoneScopedTargetPicksSameZoneOnly(t *testing.T) {
	route, _ := game.LookupRoute(game.ClientPlayerLoginEnterGameMessageId)
	require.Equal(t, base.ENodeType_LoginNodeService, route.NodeType)

	dialer := newBufDialer(t)
	loginZ1 := startEchoTarget(t, "login-z1", route.FullMethod)
	loginZ2 := startEchoTarget(t, "login-z2", route.FullMethod)
	dialer.add("login-z1", loginZ1)
	dialer.add("login-z2", loginZ2)

	sc := newSvcCtx(t, dialer, 2000)
	addNode(sc, base.ENodeType_LoginNodeService, 1, 1, "login-z1")
	addNode(sc, base.ENodeType_LoginNodeService, 2, 2, "login-z2")

	for i := 0; i < 20; i++ {
		resp, err := logic.NewForwardLogic(ctxWithSession(), sc).
			Forward(forwardReq(game.ClientPlayerLoginEnterGameMessageId, uint64(i), 2, []byte("enter")))
		require.NoError(t, err)
		require.Nil(t, resp.ErrorMessage)
		require.Equal(t, "login-z2|enter", string(resp.SerializedMessage), "zone 2 的请求只能落到 zone 2 的 login")
	}
	z1Calls, _, _ := loginZ1.snapshot()
	z2Calls, _, _ := loginZ2.snapshot()
	require.Equal(t, 0, z1Calls)
	require.Equal(t, 20, z2Calls)

	// 该 zone 没实例:不回退到别的 zone。
	resp, err := logic.NewForwardLogic(ctxWithSession(), sc).
		Forward(forwardReq(game.ClientPlayerLoginEnterGameMessageId, 99, 3, []byte("enter")))
	require.NoError(t, err)
	requireEnvelopeError(t, resp, 99, game.ClientPlayerLoginEnterGameMessageId, constants.ErrServiceUnavailable)
}

// 非 zone-scoped 类型(match)全局随机:两个 zone 的实例都会被挑到。
func TestForwardGlobalPoolTargetIgnoresZone(t *testing.T) {
	route, _ := game.LookupRoute(game.MatchServiceJoinQueueMessageId)
	dialer := newBufDialer(t)
	matchZ1 := startEchoTarget(t, "match-z1", route.FullMethod)
	matchZ2 := startEchoTarget(t, "match-z2", route.FullMethod)
	dialer.add("match-z1", matchZ1)
	dialer.add("match-z2", matchZ2)

	sc := newSvcCtx(t, dialer, 2000)
	addNode(sc, base.ENodeType_MatchNodeService, 1, 1, "match-z1")
	addNode(sc, base.ENodeType_MatchNodeService, 2, 2, "match-z2")

	for i := 0; i < 60; i++ {
		resp, err := logic.NewForwardLogic(ctxWithSession(), sc).
			Forward(forwardReq(game.MatchServiceJoinQueueMessageId, uint64(i), 1, []byte("q")))
		require.NoError(t, err)
		require.Nil(t, resp.ErrorMessage)
	}
	z1Calls, _, _ := matchZ1.snapshot()
	z2Calls, _, _ := matchZ2.snapshot()
	require.Equal(t, 60, z1Calls+z2Calls)
	require.Greater(t, z1Calls, 0, "全局随机应能挑到 zone 1 的 match")
	require.Greater(t, z2Calls, 0, "全局随机应能挑到 zone 2 的 match(不按 gate zone 过滤)")
}

// ---------------------------------------------------------------------------
// 目标出错 / 超时 → kServiceUnavailable。
// ---------------------------------------------------------------------------

func TestForwardUpstreamGrpcErrorIsServiceUnavailable(t *testing.T) {
	route, _ := game.LookupRoute(game.MatchServiceJoinQueueMessageId)
	dialer := newBufDialer(t)
	target := startEchoTarget(t, "match", route.FullMethod)
	target.failWith = status.Error(codes.Internal, "boom")
	dialer.add("match-1", target)

	sc := newSvcCtx(t, dialer, 2000)
	addNode(sc, base.ENodeType_MatchNodeService, 1, 1, "match-1")

	label := strconv.Itoa(int(game.MatchServiceJoinQueueMessageId))
	before := metrics.ForwardTotalValue(label, metrics.OutcomeUpstreamError)
	resp, err := logic.NewForwardLogic(ctxWithSession(), sc).
		Forward(forwardReq(game.MatchServiceJoinQueueMessageId, 11, 1, []byte("x")))
	require.NoError(t, err)
	requireEnvelopeError(t, resp, 11, game.MatchServiceJoinQueueMessageId, constants.ErrServiceUnavailable)
	require.Equal(t, before+1, metrics.ForwardTotalValue(label, metrics.OutcomeUpstreamError))
}

func TestForwardUpstreamTimeoutIsServiceUnavailable(t *testing.T) {
	route, _ := game.LookupRoute(game.MatchServiceJoinQueueMessageId)
	dialer := newBufDialer(t)
	target := startEchoTarget(t, "match", route.FullMethod)
	target.block = true
	dialer.add("match-1", target)

	sc := newSvcCtx(t, dialer, 200)
	addNode(sc, base.ENodeType_MatchNodeService, 1, 1, "match-1")

	label := strconv.Itoa(int(game.MatchServiceJoinQueueMessageId))
	before := metrics.ForwardTotalValue(label, metrics.OutcomeUpstreamTimeout)
	started := time.Now()
	resp, err := logic.NewForwardLogic(ctxWithSession(), sc).
		Forward(forwardReq(game.MatchServiceJoinQueueMessageId, 12, 1, []byte("x")))
	require.NoError(t, err)
	requireEnvelopeError(t, resp, 12, game.MatchServiceJoinQueueMessageId, constants.ErrServiceUnavailable)
	require.Less(t, time.Since(started), 5*time.Second, "必须由 ForwardTimeoutMs 截断,而不是等目标")
	require.Equal(t, before+1, metrics.ForwardTotalValue(label, metrics.OutcomeUpstreamTimeout))
}

func TestForwardDialErrorIsServiceUnavailable(t *testing.T) {
	dialer := newBufDialer(t) // 不登记任何目标 → dial 失败
	sc := newSvcCtx(t, dialer, 1000)
	addNode(sc, base.ENodeType_MatchNodeService, 1, 1, "match-missing")

	resp, err := logic.NewForwardLogic(ctxWithSession(), sc).
		Forward(forwardReq(game.MatchServiceJoinQueueMessageId, 13, 1, []byte("x")))
	require.NoError(t, err)
	requireEnvelopeError(t, resp, 13, game.MatchServiceJoinQueueMessageId, constants.ErrServiceUnavailable)
}

// ---------------------------------------------------------------------------
// 端到端:真 gRPC 服务端(bufconn)承载路由服 → 假目标。断言 body 原样到达、
// 响应原样返回、x-session-detail-bin 透传与响应头回写、x-caller-* 透传、
// 非 x- 键不透传、指标计数。
// ---------------------------------------------------------------------------

func startRouter(t *testing.T, sc *svc.ServiceContext) pb.ClientRpcRouterClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	pb.RegisterClientRpcRouterServer(srv, server.NewClientRpcRouterServer(sc))
	go func() { _ = srv.Serve(lis) }()

	conn, err := grpc.NewClient("passthrough:///router",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = conn.Close()
		srv.Stop()
		_ = lis.Close()
	})
	return pb.NewClientRpcRouterClient(conn)
}

func TestForwardEndToEndThroughGrpc(t *testing.T) {
	route, _ := game.LookupRoute(game.MatchServiceJoinQueueMessageId)
	dialer := newBufDialer(t)
	target := startEchoTarget(t, "match-1", route.FullMethod)
	dialer.add("match-1", target)

	sc := newSvcCtx(t, dialer, 2000)
	addNode(sc, base.ENodeType_MatchNodeService, 1, 1, "match-1")
	client := startRouter(t, sc)

	body := []byte{0x08, 0x01, 0x10, 0x02, 0x1a, 0x03, 'a', 'b', 'c'}
	label := strconv.Itoa(int(game.MatchServiceJoinQueueMessageId))
	okBefore := metrics.ForwardTotalValue(label, metrics.OutcomeOK)

	ctx := metadata.AppendToOutgoingContext(context.Background(),
		logic.SessionMetaKey, sessionValue,
		"x-caller-id", "gate",
		"x-caller-sig", "deadbeef",
		"authorization", "must-not-leak",
	)
	var header metadata.MD
	resp, err := client.Forward(ctx, forwardReq(game.MatchServiceJoinQueueMessageId, 42, 1, body), grpc.Header(&header))
	require.NoError(t, err)

	// 响应信封:id / message_id 照抄,响应字节 = 目标原样返回。
	require.Equal(t, uint64(42), resp.Id)
	require.Equal(t, uint32(game.MatchServiceJoinQueueMessageId), resp.MessageId)
	require.Nil(t, resp.ErrorMessage)
	require.Equal(t, append([]byte("match-1|"), body...), resp.SerializedMessage)

	// 响应头回写:gate 的回包桥接靠它找回会话。
	require.Equal(t, []string{sessionValue}, header.Get(logic.SessionMetaKey))

	// 目标侧:body 原样到达;x- 键透传;非 x- 键不透传。
	calls, gotBody, gotMD := target.snapshot()
	require.Equal(t, 1, calls)
	require.Equal(t, body, gotBody)
	require.Equal(t, []string{sessionValue}, gotMD.Get(logic.SessionMetaKey))
	require.Equal(t, []string{"gate"}, gotMD.Get("x-caller-id"))
	require.Equal(t, []string{"deadbeef"}, gotMD.Get("x-caller-sig"))
	require.Empty(t, gotMD.Get("authorization"), "非 x- 前缀的键不得透传")

	require.Equal(t, okBefore+1, metrics.ForwardTotalValue(label, metrics.OutcomeOK))
}

// 端到端的失败路径也要回写响应头:gate 只有拿到头才能把错误信封送回客户端。
func TestForwardEndToEndErrorEnvelopeStillEchoesHeader(t *testing.T) {
	sc := newSvcCtx(t, newBufDialer(t), 1000)
	client := startRouter(t, sc)

	ctx := metadata.AppendToOutgoingContext(context.Background(), logic.SessionMetaKey, sessionValue)
	var header metadata.MD
	resp, err := client.Forward(ctx, forwardReq(game.MatchServiceJoinQueueMessageId, 5, 1, []byte("x")), grpc.Header(&header))
	require.NoError(t, err)
	requireEnvelopeError(t, resp, 5, game.MatchServiceJoinQueueMessageId, constants.ErrServiceUnavailable)
	require.Equal(t, []string{sessionValue}, header.Get(logic.SessionMetaKey))
}

// 端到端缺会话元数据:gRPC UNAUTHENTICATED。
func TestForwardEndToEndMissingSessionIsUnauthenticated(t *testing.T) {
	sc := newSvcCtx(t, newBufDialer(t), 1000)
	client := startRouter(t, sc)
	_, err := client.Forward(context.Background(), forwardReq(game.MatchServiceJoinQueueMessageId, 5, 1, []byte("x")))
	require.Equal(t, codes.Unauthenticated, status.Code(err))
}

// 空 body 也要能原样转发(某些请求消息本身没有字段)。
func TestForwardEmptyBodyRoundTrips(t *testing.T) {
	route, _ := game.LookupRoute(game.MatchServiceGetQueueStatusMessageId)
	dialer := newBufDialer(t)
	target := startEchoTarget(t, "match-1", route.FullMethod)
	dialer.add("match-1", target)

	sc := newSvcCtx(t, dialer, 2000)
	addNode(sc, base.ENodeType_MatchNodeService, 1, 1, "match-1")

	resp, err := logic.NewForwardLogic(ctxWithSession(), sc).
		Forward(forwardReq(game.MatchServiceGetQueueStatusMessageId, 3, 1, nil))
	require.NoError(t, err)
	require.Nil(t, resp.ErrorMessage)
	require.Equal(t, "match-1|", string(resp.SerializedMessage))
	_, gotBody, _ := target.snapshot()
	require.Empty(t, gotBody)
}
