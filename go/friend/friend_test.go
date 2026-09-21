package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"friend/internal/config"
	"friend/internal/constants"
	"friend/internal/data"
	"friend/internal/session"

	base "proto/common/base"
	friendpb "proto/friend"

	"schemamigrate"

	"shared/killswitch"
	"shared/serverbase"

	"github.com/zeromicro/go-zero/core/logx/logtest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// chainUnary 按 gRPC 的语义把一组一元拦截器串成一个 handler:切片里第一个是最外层,
// 最后一个紧贴业务 handler(与 grpc.ChainUnaryInterceptor 的文档一致)。
//
// grpc-go 自己的 chainUnaryInterceptors 是包内私有函数,拿不到,所以这里复刻一份。
// 这个副本在 chat / guild / trade 的同名测试里逐字相同 —— 各服务是独立 go module,
// 没有共同的测试工具包可放。
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

// incomingSessionContext 模拟 gate 注入、路由服原样透传的 x-session-detail-bin
// (值 = base64(proto SessionDetails))。客户端请求体里没有 player_id(D-9),
// 身份只能从这里来,所以凡是"像客户端来的"测试都必须带上它。
func incomingSessionContext(t *testing.T, playerId uint64) context.Context {
	t.Helper()
	bin, err := proto.Marshal(&base.SessionDetails{SessionId: 1, PlayerId: playerId})
	if err != nil {
		t.Fatalf("序列化 SessionDetails 失败: %v", err)
	}
	md := metadata.Pairs(session.MetadataKey, base64.StdEncoding.EncodeToString(bin))
	return metadata.NewIncomingContext(context.Background(), md)
}

// TestBuildUnaryInterceptorsOrder 钉住链长与顺序:grpcstats → killswitch → session → serverbase。
//
// 顺序不是风格问题,每一层换位都有具体代价(契约 §3):统计层挪到里面会漏记被关停 / 被拒绝的请求;
// killswitch 挪到 session 之后,被关停的方法仍要解一次会话;session 挪到 serverbase 之后,
// 被拒绝的调用会算进业务耗时。
//
// 按**可观测行为**逐个探测(函数值不能比较):对每个位置单独调用那一个拦截器,handler 返回带 fault 码的
// 响应,并带上合法会话头 + 命中的关停规则。于是:
//   - killswitch:唯一不调 handler、回 Unavailable 的;
//   - session:唯一让 handler 从 ctx 取到会话 player_id 的;
//   - serverbase:唯一把 fault 码打成 rpc_inband_fault 日志的;
//   - grpcstats:未开启采集时完全透明 —— 剩下的那个位置就是它。
func TestBuildUnaryInterceptorsOrder(t *testing.T) {
	ks := killswitch.New(killswitch.Config{})
	ks.SetRules(map[string]killswitch.Rule{
		strings.TrimPrefix(friendpb.ClientPlayerFriend_AddFriend_FullMethodName, "/"): {Deny: true, Reason: "单测:顺序探针"},
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

	info := &grpc.UnaryServerInfo{FullMethod: friendpb.ClientPlayerFriend_AddFriend_FullMethodName}
	// ErrStorage = kServiceUnavailable,是 friend 唯一被 Tip.xlsx 标成 fault 的码:
	// 用它当探针才能看出 serverbase 那一层在不在(业务拒绝码不打 rpc_inband_fault)。
	faultResp := &friendpb.AddFriendResponse{ErrorMessage: &base.TipInfoMessage{Id: constants.ErrStorage}}

	for i, w := range want {
		t.Run(w.name, func(t *testing.T) {
			logs := logtest.NewCollector(t)
			var got observed
			_, err := chain[i](incomingSessionContext(t, 42), &friendpb.AddFriendRequest{}, info,
				func(ctx context.Context, req any) (any, error) {
					got.handlerCalled = true
					_, got.sawSession = session.ClientPlayerID(ctx)
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

// TestKillSwitchWiredIntoUnaryChain 证明热关停**确实挂在**本服务的拦截器链上。
//
// 为什么要有这条测试:killswitch 是"平时完全没有可观测行为"的组件 —— 没写规则时它和不存在一模一样。
// 所以一旦哪次重构把 buildUnaryInterceptors 里那一行删掉,所有既有测试、所有联调、所有压测都照样全绿,
// 只有真出事那天、运维敲下 etcdctl put 却发现流量纹丝不动时才会发现止血阀是假的。
// 这条测试就是那一行的唯一守卫:删掉 ks.UnaryServerInterceptor() 它立刻 FAIL。
//
// 覆盖边界(如实说明):它验证的是"链的组装正确",不是"main() 调了 AddUnaryInterceptors"。
// 后者是 main 里的一行、无法在单测中执行(需要真实 etcd + 端口 + zrpc 配置),只能靠 review 保证。
func TestKillSwitchWiredIntoUnaryChain(t *testing.T) {
	ks := killswitch.New(killswitch.Config{})
	// key 是 killswitch 的**匹配模式**(全限定服务名/方法名),不是 etcd 的完整 key。
	// 刻意从生成代码里取方法名:模式写错是运维最容易犯的错,用常量拼能顺带钉住
	// "服务名到底叫什么"—— 本期从 FriendService 改名成 ClientPlayerFriend,
	// etc/friend.yaml 里的 etcdctl 示例必须跟着改。
	blocked := strings.TrimPrefix(friendpb.ClientPlayerFriend_AddFriend_FullMethodName, "/")
	ks.SetRules(map[string]killswitch.Rule{
		blocked: {Deny: true, Reason: "单测:好友库过载"},
	})

	chain := buildUnaryInterceptors(ks)

	t.Run("命中规则的方法被短路", func(t *testing.T) {
		handlerCalled := false
		info := &grpc.UnaryServerInfo{FullMethod: friendpb.ClientPlayerFriend_AddFriend_FullMethodName}
		h := chainUnary(chain, info, func(ctx context.Context, req any) (any, error) {
			handlerCalled = true
			return &friendpb.AddFriendResponse{}, nil
		})

		resp, err := h(incomingSessionContext(t, 42), &friendpb.AddFriendRequest{})
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
		// 错误文本必须带方法名与原因:客户端日志里要能一眼看出"是被关停了,不是超时"。
		if !strings.Contains(st.Message(), "AddFriend") || !strings.Contains(st.Message(), "好友库过载") {
			t.Fatalf("错误文本缺少方法名/原因: %q", st.Message())
		}
	})

	t.Run("未命中的方法照常放行", func(t *testing.T) {
		handlerCalled := false
		info := &grpc.UnaryServerInfo{FullMethod: friendpb.ClientPlayerFriend_GetFriendList_FullMethodName}
		h := chainUnary(chain, info, func(ctx context.Context, req any) (any, error) {
			handlerCalled = true
			if _, ok := session.ClientPlayerID(ctx); !ok {
				t.Error("整条链走完后 handler 应能取到会话身份(D-9:身份只从会话来)")
			}
			return &friendpb.GetFriendListResponse{}, nil
		})

		if _, err := h(incomingSessionContext(t, 42), &friendpb.GetFriendListRequest{}); err != nil {
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
	// 刻意不调 SetRules —— 模拟 etcd 客户端为 nil 的部署。
	ks.Start(context.Background(), nil)

	handlerCalled := false
	info := &grpc.UnaryServerInfo{FullMethod: friendpb.ClientPlayerFriend_AddFriend_FullMethodName}
	h := chainUnary(buildUnaryInterceptors(ks), info, func(ctx context.Context, req any) (any, error) {
		handlerCalled = true
		return &friendpb.AddFriendResponse{}, nil
	})

	if _, err := h(incomingSessionContext(t, 42), &friendpb.AddFriendRequest{}); err != nil {
		t.Fatalf("无规则时必须放行: %v", err)
	}
	if !handlerCalled {
		t.Fatal("无规则时 handler 必须被调用(fail-open)")
	}
}

// TestSessionRejectsNotifyFriendEventFromClient 经**真链**验证 S2C 方法挡得住客户端伪造。
//
// NotifyFriendEvent 是服务端推给客户端的方向(rpc ... returns Empty),放在同一个
// ClientPlayerFriend 服务里只是为了让生成器出 Unity / robot 的推送 handler。
// 但它同时也就成了一个"客户端能拨的方法号":客户端若能调它,就能给自己伪造
// "某人通过了你的好友申请"。第一道防线是 session 白名单不收它,
// 第二道是 server 侧不实现它(继承 Unimplemented)。这条测试守第一道。
//
// 删掉 buildUnaryInterceptors 里 session 那一行,其余测试全绿,这条立刻 FAIL。
func TestSessionRejectsNotifyFriendEventFromClient(t *testing.T) {
	logtest.Discard(t) // 拒绝会打 Error 日志,这里只关心行为

	handlerCalled := false
	info := &grpc.UnaryServerInfo{FullMethod: friendpb.ClientPlayerFriend_NotifyFriendEvent_FullMethodName}
	h := chainUnary(buildUnaryInterceptors(killswitch.New(killswitch.Config{})), info,
		func(ctx context.Context, req any) (any, error) {
			handlerCalled = true
			return &base.Empty{}, nil
		})

	_, err := h(incomingSessionContext(t, 42), &friendpb.FriendEventS2C{})
	if handlerCalled {
		t.Fatal("客户端会话调用 NotifyFriendEvent 走到了 handler:S2C 方法没有被会话白名单挡住")
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("期望 PermissionDenied,得到 %v", err)
	}
}

// TestBrokenSessionRejectedInChain:坏会话头在整条链里同样 fail-closed(Unauthenticated),
// 不能退化成"当内部调用处理"—— 否则伪造一个解不开的头就绕过了整套准入。
func TestBrokenSessionRejectedInChain(t *testing.T) {
	logtest.Discard(t)

	handlerCalled := false
	info := &grpc.UnaryServerInfo{FullMethod: friendpb.ClientPlayerFriend_AddFriend_FullMethodName}
	h := chainUnary(buildUnaryInterceptors(killswitch.New(killswitch.Config{})), info,
		func(ctx context.Context, req any) (any, error) {
			handlerCalled = true
			return &friendpb.AddFriendResponse{}, nil
		})
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(session.MetadataKey, "%%%不是base64"))

	_, err := h(ctx, &friendpb.AddFriendRequest{})
	if handlerCalled {
		t.Fatal("坏会话头走到了 handler")
	}
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("期望 Unauthenticated,得到 %v", err)
	}
}

// TestNodeInfoValueMatchesRegistryContract:BuildValue 产物必须通过 noderegistry 的注册前校验
// (契约 §2:nodeId==nodeID、zoneId==ZoneId、nodeUuid 非空、两个 port 非 0、protocolType 为 PROTOCOL_GRPC),
// 且 nodeType 是 FriendNodeService(27)。
//
// 重点在"两个端点都填":只填 Endpoint 时 etcd 里的注册项看着完全正常,路由服却拨不通
// (guild 踩过,排障方向会被带到网络层去)。
func TestNodeInfoValueMatchesRegistryContract(t *testing.T) {
	build := nodeInfoValueBuilder(2, "10.0.0.9", 50400, 1_700_000_000)
	raw, err := build(7, "uuid-friend-test")
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
	if mirror.NodeId != 7 || mirror.ZoneId != 2 || mirror.NodeUuid != "uuid-friend-test" {
		t.Fatalf("身份字段不符: %+v", mirror)
	}
	if uint32(base.ENodeType_FriendNodeService) != 27 {
		t.Fatalf("FriendNodeService 枚举值应为 27,实际 %d", base.ENodeType_FriendNodeService)
	}
	if mirror.NodeType != uint32(base.ENodeType_FriendNodeService) {
		t.Fatalf("nodeType 应为 FriendNodeService(27),实际 %d", mirror.NodeType)
	}
	if mirror.ProtocolType != uint32(base.ENodeProtocolType_PROTOCOL_GRPC) {
		t.Fatalf("protocolType 应为 PROTOCOL_GRPC,实际 %d", mirror.ProtocolType)
	}
	if mirror.Endpoint.Port != 50400 || mirror.GrpcEndpoint.Port != 50400 ||
		mirror.Endpoint.Ip != "10.0.0.9" || mirror.GrpcEndpoint.Ip != "10.0.0.9" {
		t.Fatalf("端点字段不符(Endpoint 与 GrpcEndpoint 必须同时填): %+v", mirror)
	}
	if got := base.ENodeType_name[int32(nodeType)] + ".rpc"; got != "FriendNodeService.rpc" {
		t.Fatalf("注册前缀应为 FriendNodeService.rpc,实际 %q", got)
	}
}

// fakeSchemaRunner 记录调用并返回预设结果,替代 schemamigrate.Up / Plan。
type fakeSchemaRunner struct {
	report schemamigrate.Report
	err    error
	calls  int
	opts   schemamigrate.Options
}

func (f *fakeSchemaRunner) run(_ context.Context, _ *sql.DB, opts schemamigrate.Options) (schemamigrate.Report, error) {
	f.calls++
	f.opts = opts
	return f.report, f.err
}

// missingIndexWarningSample 是 schemamigrate 那条"缺索引"告警的**逐字样本**,抄自
// go/schemamigrate/plan.go 里生成它的那句 fmt.Sprintf。
//
// ⚠ 它与生产代码的联系只有一条前缀常量 missingIndexWarningPrefix(下面的用例断言样本以它开头),
// schemamigrate 那边改文案时**本测试不会自动变红** —— 真正的机械保障是交接文档 §2 第 7a 步的真库
// 核对("-migrate 的输出里不得出现 `缺索引(不会自动建)`")。这一点与 guild 的同类用例是同一个
// 取舍:不为了一条文案去起真 MySQL。
const missingIndexWarningSample = "缺索引(不会自动建):friend_capacity 上没有 proto 声明的索引 idx_friend_capacity_0"

// TestMissingIndexWarningIsBlocking 钉住"缺索引不是告警,是阻断"。
//
// 为什么值得单独一条用例:schemamigrate 对**已存在的表**只 ADD COLUMN,不补建 proto 新声明的
// 普通索引,而 Warning 不进 ExitCode。friend 这边最直接的受害者是 friend_capacity 的回收
// (依赖 (friend_count, created_ms) 复合索引),缺了它每轮候选读都是全表扫,**全程零报错**。
func TestMissingIndexWarningIsBlocking(t *testing.T) {
	if !strings.HasPrefix(missingIndexWarningSample, missingIndexWarningPrefix) {
		t.Fatalf("样本告警 %q 不以生产代码的前缀 %q 开头,missingIndexWarnings 会漏判",
			missingIndexWarningSample, missingIndexWarningPrefix)
	}

	// 只有"缺索引"那一条被挑出来:多余列之类的告警必须继续放行,否则一次无害的 schema 漂移
	// 就会把服务挡在启动之外。
	report := schemamigrate.Report{Warnings: []string{
		"extra column: friend.legacy_col 不在 proto 里",
		missingIndexWarningSample,
	}}
	got := missingIndexWarnings(report)
	if len(got) != 1 || got[0] != missingIndexWarningSample {
		t.Fatalf("应只挑出那一条缺索引告警,实际 %v", got)
	}

	err := missingIndexError(report)
	if err == nil {
		t.Fatal("缺索引必须返回错误(拒绝启动 / -migrate 返回非 0)")
	}
	// 错误里要带上原始告警:排查的人靠它知道是哪张表的哪条索引。
	if !strings.Contains(err.Error(), missingIndexWarningSample) {
		t.Errorf("错误应带上原始告警原文,实际: %v", err)
	}
	if !strings.Contains(err.Error(), data.DatabaseName) {
		t.Errorf("错误应点名库 %q,实际: %v", data.DatabaseName, err)
	}

	if missingIndexError(schemamigrate.Report{Warnings: []string{"extra column"}}) != nil {
		t.Error("只有多余列告警时不得拒绝启动")
	}
	if missingIndexError(schemamigrate.Report{}) != nil {
		t.Error("没有任何告警时不得拒绝启动")
	}
}

// TestEnsureSchemaRejectsStartupWhenPlanNotClean 钉住启动期建表策略(D-14 第 4 条):
// AutoMigrate 没写 / true 跑 Up,false 只跑只读 Plan;出错、需人工项、(Plan 下)有待执行语句
// 都拒绝启动,Plan 拒绝时带补救命令。
//
// 最要紧的一条是"false + 有待执行语句必须拒启":放它过去的话,服务会带着缺列的表对外服务,
// 每个写好友的请求都在 MySQL 报 unknown column,而启动日志里一切正常。
//
// "缺索引"两种模式都拒启:它落在 Warnings 里,而 Report.Clean() 不看 Warnings —— 两个分支
// 各自都要单独判一次,漏掉任一个都会让缺索引静默放行(见 missingIndexWarnings 的说明)。
func TestEnsureSchemaRejectsStartupWhenPlanNotClean(t *testing.T) {
	logtest.Discard(t)
	yes, no := true, false
	cases := []struct {
		name        string
		autoMigrate *bool
		report      schemamigrate.Report
		err         error
		wantUp      bool
		wantErr     bool
		wantRemedy  bool
	}{
		{name: "没写 Schema 段 = Up,建表成功", report: schemamigrate.Report{Statements: []string{"CREATE TABLE friend ..."}}, wantUp: true},
		{name: "显式 true,库已是最新", autoMigrate: &yes, wantUp: true},
		{name: "Up 只有告警照常启动", autoMigrate: &yes, report: schemamigrate.Report{Warnings: []string{"extra column"}}, wantUp: true},
		{name: "Up 出错拒绝启动", autoMigrate: &yes, err: schemamigrate.ErrLockBusy, wantUp: true, wantErr: true},
		{name: "Up 有需人工项拒绝启动", autoMigrate: &yes, report: schemamigrate.Report{Manual: []string{"type drift"}}, wantUp: true, wantErr: true},
		{name: "Up 缺索引拒绝启动", autoMigrate: &yes, report: schemamigrate.Report{Warnings: []string{missingIndexWarningSample}},
			wantUp: true, wantErr: true},
		{name: "false + 库干净", autoMigrate: &no},
		{name: "false + 只有告警", autoMigrate: &no, report: schemamigrate.Report{Warnings: []string{"extra column"}}},
		{name: "false + 有待执行语句", autoMigrate: &no, report: schemamigrate.Report{Statements: []string{"CREATE TABLE friend_block ..."}},
			wantErr: true, wantRemedy: true},
		{name: "false + 需人工", autoMigrate: &no, report: schemamigrate.Report{Manual: []string{"type drift"}},
			wantErr: true, wantRemedy: true},
		{name: "false + Plan 出错", autoMigrate: &no, err: errors.New("dial tcp: connection refused"),
			wantErr: true, wantRemedy: true},
		// 只读核对同样要拦住缺索引:Report.Clean() 只看 Statements 与 Manual,缺索引在 Warnings 里,
		// 所以这条走的不是上面 !Clean() 那个分支,而是它之后单独那一判。
		// 它**不带**补救命令(migrateRemedyCommand 是"去跑 -migrate",而 -migrate 同样不补建索引,
		// 指过去只会让人白跑一趟),所以这里 wantRemedy 为 false。
		{name: "false + 缺索引", autoMigrate: &no, report: schemamigrate.Report{Warnings: []string{missingIndexWarningSample}},
			wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := &fakeSchemaRunner{}
			plan := &fakeSchemaRunner{}
			active := plan
			if tc.wantUp {
				active = up
			}
			active.report, active.err = tc.report, tc.err
			c := config.Config{Schema: config.SchemaConf{AutoMigrate: tc.autoMigrate}}

			err := ensureSchema(context.Background(), nil, c, up.run, plan.run)

			if tc.wantUp && (up.calls != 1 || plan.calls != 0) {
				t.Fatalf("应只跑 Up:up=%d plan=%d", up.calls, plan.calls)
			}
			if !tc.wantUp && (plan.calls != 1 || up.calls != 0) {
				t.Fatalf("AutoMigrate=false 应只跑只读 Plan:up=%d plan=%d", up.calls, plan.calls)
			}
			if active.opts.Database != data.DatabaseName {
				t.Errorf("库名断言应为 %q,实际 %q", data.DatabaseName, active.opts.Database)
			}
			if len(active.opts.Tables) != len(data.Tables()) {
				t.Errorf("表清单应来自 data.Tables()(%d 张),实际 %d 张", len(data.Tables()), len(active.opts.Tables))
			}
			if active.opts.AllowModifyColumn {
				t.Error("常驻启动的 Up / Plan 不得获得 MODIFY COLUMN 授权")
			}
			if tc.wantErr != (err != nil) {
				t.Fatalf("wantErr=%v,实际 err=%v", tc.wantErr, err)
			}
			if tc.wantRemedy && !strings.Contains(err.Error(), migrateRemedyCommand) {
				t.Errorf("拒绝启动的错误应带补救命令 %q,实际: %v", migrateRemedyCommand, err)
			}
			if tc.err != nil && !errors.Is(err, tc.err) {
				t.Errorf("错误应包裹原始错误 %v,实际: %v", tc.err, err)
			}
		})
	}
}

// nopConnector 让 runMigration 拿到一个**非 nil** 的 *sql.DB(它会 defer db.Close()),
// 但这个池永远不会被真正连上:runMigration 只把 db 原样交给注入的 up,而 fakeSchemaRunner 不碰它。
// sql.OpenDB 是惰性的,不发任何连接;真有代码去连,会拿到下面这条错误而不是悄悄连上某个库。
type nopConnector struct{}

func (nopConnector) Connect(context.Context) (driver.Conn, error) {
	return nil, errors.New("测试里的 *sql.DB 不应被连接")
}

func (nopConnector) Driver() driver.Driver { return nopDriver{} }

type nopDriver struct{}

func (nopDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("测试里的 *sql.DB 不应被连接")
}

// TestRunMigrationExitCodes 钉住 `-migrate` 的退出码(D-14:0 成功 / 1 失败 / 3 锁忙 / 4 需人工)。
//
// K8s 的 friend-migrate Job 与部署脚本**只看退出码**,所以这张表就是它们的契约。最要紧的是
// "缺索引 → 4":schemamigrate 把缺索引记成 Warning、ExitCode 算出来是 0,不在 runMigration 里升级的话
// Job 会绿着结束,随后起来的 friend Pod 又被 ensureSchema 拦下 —— "迁移成功但服务起不来"。
func TestRunMigrationExitCodes(t *testing.T) {
	logtest.Discard(t)
	for _, tc := range []struct {
		name   string
		report schemamigrate.Report
		err    error
		want   int
	}{
		{name: "建表成功", report: schemamigrate.Report{Statements: []string{"CREATE TABLE friend ..."}}, want: schemamigrate.ExitOK},
		{name: "只有多余列告警照常成功", report: schemamigrate.Report{Warnings: []string{"extra column"}}, want: schemamigrate.ExitOK},
		{name: "缺索引升级成需人工", report: schemamigrate.Report{Warnings: []string{missingIndexWarningSample}}, want: schemamigrate.ExitManual},
		{name: "需人工项", report: schemamigrate.Report{Manual: []string{"type drift"}}, want: schemamigrate.ExitManual},
		{name: "锁忙可重试", err: schemamigrate.ErrLockBusy, want: schemamigrate.ExitLockBusy},
		{name: "迁移出错", err: errors.New("dial tcp: connection refused"), want: schemamigrate.ExitFailed},
		// 迁移本身已经失败时,缺索引告警不得把退出码"改善"成 4:失败(1)比需人工(4)更要紧,
		// Job 的重试策略也不同。runMigration 只在 ExitOK 时才做缺索引升级,这条钉住那个条件。
		{name: "出错且缺索引仍按出错", report: schemamigrate.Report{Warnings: []string{missingIndexWarningSample}},
			err: errors.New("ddl failed"), want: schemamigrate.ExitFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := &fakeSchemaRunner{report: tc.report, err: tc.err}
			openMySQL := func(config.MySQLConf) (*sql.DB, error) { return sql.OpenDB(nopConnector{}), nil }

			got := runMigration(config.Config{}, false, openMySQL, up.run)

			if got != tc.want {
				t.Fatalf("退出码应为 %d,实际 %d", tc.want, got)
			}
			if up.calls != 1 {
				t.Fatalf("应恰好跑一次 Up,实际 %d 次", up.calls)
			}
		})
	}

	t.Run("连库失败按失败退出且不跑迁移", func(t *testing.T) {
		up := &fakeSchemaRunner{}
		openMySQL := func(config.MySQLConf) (*sql.DB, error) { return nil, errors.New("unknown database") }

		if got := runMigration(config.Config{}, false, openMySQL, up.run); got != schemamigrate.ExitFailed {
			t.Fatalf("连不上库应退出 %d,实际 %d", schemamigrate.ExitFailed, got)
		}
		if up.calls != 0 {
			t.Fatalf("连不上库时不得跑迁移,实际 %d 次", up.calls)
		}
	})

	t.Run("-allow-modify 原样传给 Up", func(t *testing.T) {
		up := &fakeSchemaRunner{}
		openMySQL := func(config.MySQLConf) (*sql.DB, error) { return sql.OpenDB(nopConnector{}), nil }

		runMigration(config.Config{}, true, openMySQL, up.run)

		if !up.opts.AllowModifyColumn {
			t.Fatal("显式 -migrate -allow-modify 时,改列授权必须传到 Up")
		}
	})
}

// TestValidateMigrationFlags:`-allow-modify` 不带 `-migrate` 必须报错。
// 常驻启动继承了改列授权 = 一次滚动更新就可能在线上 MODIFY COLUMN 重建大表并锁表。
func TestValidateMigrationFlags(t *testing.T) {
	for _, tc := range []struct {
		name        string
		migrateOnly bool
		allowModify bool
		wantErr     bool
	}{
		{name: "常驻启动"},
		{name: "默认迁移", migrateOnly: true},
		{name: "显式迁移授权改列", migrateOnly: true, allowModify: true},
		{name: "常驻启动不得授权改列", allowModify: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateMigrationFlags(tc.migrateOnly, tc.allowModify)
			if (err != nil) != tc.wantErr {
				t.Fatalf("wantErr=%v,实际 err=%v", tc.wantErr, err)
			}
			if tc.wantErr && !strings.Contains(err.Error(), "-migrate") {
				t.Fatalf("拒绝信息必须说明缺少 -migrate: %v", err)
			}
		})
	}
}
