package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"trade/internal/config"
	"trade/internal/constants"
	"trade/internal/data"
	"trade/internal/session"

	base "proto/common/base"
	tradepb "proto/trade"

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

// chainUnary 按 gRPC 的语义把一组一元拦截器串成一个 handler:切片里第一个是最外层。
// grpc-go 的 chainUnaryInterceptors 是包内私有函数,这里复刻一份(与 chat / guild 的同名测试逐字相同 ——
// 各服务是独立 module,没有共享测试包)。
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
	bin, err := proto.Marshal(&base.SessionDetails{SessionId: 1, PlayerId: playerId})
	if err != nil {
		t.Fatalf("序列化 SessionDetails 失败: %v", err)
	}
	md := metadata.Pairs(session.MetadataKey, base64.StdEncoding.EncodeToString(bin))
	return metadata.NewIncomingContext(context.Background(), md)
}

// TestBuildUnaryInterceptorsOrder 钉住链长与顺序:grpcstats → killswitch → session → serverbase。
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
		strings.TrimPrefix(tradepb.ClientPlayerJubaozhai_BrowseListings_FullMethodName, "/"): {Deny: true, Reason: "单测:顺序探针"},
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

	info := &grpc.UnaryServerInfo{FullMethod: tradepb.ClientPlayerJubaozhai_BrowseListings_FullMethodName}
	faultResp := &tradepb.BrowseListingsResponse{ErrorMessage: &base.TipInfoMessage{Id: constants.ErrServiceUnavailable}}

	for i, w := range want {
		t.Run(w.name, func(t *testing.T) {
			logs := logtest.NewCollector(t)
			var got observed
			_, err := chain[i](incomingSessionContext(t, 42), &tradepb.BrowseListingsRequest{}, info,
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
// 删掉 buildUnaryInterceptors 里的 ks.UnaryServerInterceptor(),这条测试立刻 FAIL。
func TestKillSwitchWiredIntoUnaryChain(t *testing.T) {
	ks := killswitch.New(killswitch.Config{})
	// key 从生成代码取:顺带钉住服务名是 trade.ClientPlayerJubaozhai,与 etc/trade.yaml 的 etcdctl 示例一致。
	blocked := strings.TrimPrefix(tradepb.ClientPlayerJubaozhai_BrowseListings_FullMethodName, "/")
	ks.SetRules(map[string]killswitch.Rule{
		blocked: {Deny: true, Reason: "单测:浏览把库打爆了"},
	})
	chain := buildUnaryInterceptors(ks)

	t.Run("命中规则的方法被短路", func(t *testing.T) {
		handlerCalled := false
		info := &grpc.UnaryServerInfo{FullMethod: tradepb.ClientPlayerJubaozhai_BrowseListings_FullMethodName}
		h := chainUnary(chain, info, func(ctx context.Context, req any) (any, error) {
			handlerCalled = true
			return &tradepb.BrowseListingsResponse{}, nil
		})

		resp, err := h(incomingSessionContext(t, 42), &tradepb.BrowseListingsRequest{})
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
		if !strings.Contains(st.Message(), "BrowseListings") || !strings.Contains(st.Message(), "浏览把库打爆了") {
			t.Fatalf("错误文本缺少方法名/原因: %q", st.Message())
		}
	})

	t.Run("未命中的方法照常放行", func(t *testing.T) {
		handlerCalled := false
		info := &grpc.UnaryServerInfo{FullMethod: tradepb.ClientPlayerJubaozhai_GetMyShelf_FullMethodName}
		h := chainUnary(chain, info, func(ctx context.Context, req any) (any, error) {
			handlerCalled = true
			if _, ok := session.ClientPlayerID(ctx); !ok {
				t.Error("整条链走完后 handler 应能取到会话")
			}
			return &tradepb.GetMyShelfResponse{}, nil
		})
		if _, err := h(incomingSessionContext(t, 42), &tradepb.GetMyShelfRequest{}); err != nil {
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
	info := &grpc.UnaryServerInfo{FullMethod: tradepb.ClientPlayerJubaozhai_BrowseListings_FullMethodName}
	h := chainUnary(buildUnaryInterceptors(ks), info, func(ctx context.Context, req any) (any, error) {
		handlerCalled = true
		return &tradepb.BrowseListingsResponse{}, nil
	})
	if _, err := h(incomingSessionContext(t, 42), &tradepb.BrowseListingsRequest{}); err != nil {
		t.Fatalf("无规则时必须放行: %v", err)
	}
	if !handlerCalled {
		t.Fatal("无规则时 handler 必须被调用(fail-open)")
	}
}

// TestSessionGateRejectsClientCallToTradeAdmin 证明会话准入**确实挂在**链上:带客户端会话调内部方法
// TradeAdmin/SeedListing 必须在 handler 之前被 PermissionDenied。删掉 buildUnaryInterceptors 里 session
// 那一行,其余测试全绿,客户端却能凭空造商品。
func TestSessionGateRejectsClientCallToTradeAdmin(t *testing.T) {
	logtest.Discard(t) // 拒绝会打 Error 日志,这里只关心行为

	handlerCalled := false
	info := &grpc.UnaryServerInfo{FullMethod: tradepb.TradeAdmin_SeedListing_FullMethodName}
	h := chainUnary(buildUnaryInterceptors(killswitch.New(killswitch.Config{})), info,
		func(ctx context.Context, req any) (any, error) {
			handlerCalled = true
			return &tradepb.SeedListingResponse{}, nil
		})

	_, err := h(incomingSessionContext(t, 42), &tradepb.SeedListingRequest{SellerPlayerId: 42})
	if handlerCalled {
		t.Fatal("客户端会话调用 SeedListing 走到了 handler:会话准入没有挂上拦截器链")
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("期望 PermissionDenied,得到 %v", err)
	}
}

// TestInternalSeedListingWithoutSessionReachesHandler:不带会话 metadata 的内部调用(robot 直连播种)
// 必须穿过整条链到达 handler,且 handler 眼里没有客户端身份。
func TestInternalSeedListingWithoutSessionReachesHandler(t *testing.T) {
	handlerCalled := false
	info := &grpc.UnaryServerInfo{FullMethod: tradepb.TradeAdmin_SeedListing_FullMethodName}
	h := chainUnary(buildUnaryInterceptors(killswitch.New(killswitch.Config{})), info,
		func(ctx context.Context, req any) (any, error) {
			handlerCalled = true
			if _, ok := session.ClientPlayerID(ctx); ok {
				t.Error("内部调用不该被当成客户端")
			}
			return &tradepb.SeedListingResponse{}, nil
		})

	if _, err := h(context.Background(), &tradepb.SeedListingRequest{}); err != nil {
		t.Fatalf("无会话的内部调用必须放行: %v", err)
	}
	if !handlerCalled {
		t.Fatal("无会话的内部调用必须走到 handler")
	}
}

// TestBrokenSessionRejectedInChain:坏会话头在整条链里同样 fail-closed(Unauthenticated),
// 不能退化成"当内部调用处理"。
func TestBrokenSessionRejectedInChain(t *testing.T) {
	logtest.Discard(t)

	handlerCalled := false
	info := &grpc.UnaryServerInfo{FullMethod: tradepb.TradeAdmin_SeedListing_FullMethodName}
	h := chainUnary(buildUnaryInterceptors(killswitch.New(killswitch.Config{})), info,
		func(ctx context.Context, req any) (any, error) {
			handlerCalled = true
			return &tradepb.SeedListingResponse{}, nil
		})
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(session.MetadataKey, "%%%不是base64"))

	_, err := h(ctx, &tradepb.SeedListingRequest{})
	if handlerCalled {
		t.Fatal("坏会话头走到了 handler")
	}
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("期望 Unauthenticated,得到 %v", err)
	}
}

// TestNodeInfoValueMatchesRegistryContract:BuildValue 产物必须通过 noderegistry 的注册前校验
// (契约 §2:nodeId==nodeID、zoneId==ZoneId、nodeUuid 非空、两个 port 非 0、protocolType 为 PROTOCOL_GRPC),
// 且 nodeType 是 TradeNodeService(12)。
func TestNodeInfoValueMatchesRegistryContract(t *testing.T) {
	build := nodeInfoValueBuilder(2, "10.0.0.9", 50800, 1_700_000_000)
	raw, err := build(7, "uuid-trade-test")
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
	if mirror.NodeId != 7 || mirror.ZoneId != 2 || mirror.NodeUuid != "uuid-trade-test" {
		t.Fatalf("身份字段不符: %+v", mirror)
	}
	if uint32(base.ENodeType_TradeNodeService) != 12 {
		t.Fatalf("TradeNodeService 枚举值应为 12,实际 %d", base.ENodeType_TradeNodeService)
	}
	if mirror.NodeType != uint32(base.ENodeType_TradeNodeService) {
		t.Fatalf("nodeType 应为 TradeNodeService(12),实际 %d", mirror.NodeType)
	}
	if mirror.ProtocolType != uint32(base.ENodeProtocolType_PROTOCOL_GRPC) {
		t.Fatalf("protocolType 应为 PROTOCOL_GRPC,实际 %d", mirror.ProtocolType)
	}
	if mirror.Endpoint.Port != 50800 || mirror.GrpcEndpoint.Port != 50800 ||
		mirror.Endpoint.Ip != "10.0.0.9" || mirror.GrpcEndpoint.Ip != "10.0.0.9" {
		t.Fatalf("端点字段不符: %+v", mirror)
	}
	if got := base.ENodeType_name[int32(nodeType)] + ".rpc"; got != "TradeNodeService.rpc" {
		t.Fatalf("注册前缀应为 TradeNodeService.rpc,实际 %q", got)
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

// TestEnsureSchema 钉住启动期建表策略(D-14 第 4 条):AutoMigrate 没写 / true 跑 Up,false 只跑 Plan;
// 出错、需人工项、(Plan 下)有待执行语句都拒绝启动,Plan 拒绝时带补救命令。
func TestEnsureSchema(t *testing.T) {
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
		{name: "没写 Schema 段 = Up,建表成功", report: schemamigrate.Report{Statements: []string{"CREATE TABLE trade_listing ..."}}, wantUp: true},
		{name: "显式 true,库已是最新", autoMigrate: &yes, wantUp: true},
		{name: "Up 只有告警照常启动", autoMigrate: &yes, report: schemamigrate.Report{Warnings: []string{"extra column"}}, wantUp: true},
		{name: "Up 出错拒绝启动", autoMigrate: &yes, err: schemamigrate.ErrLockBusy, wantUp: true, wantErr: true},
		{name: "Up 有需人工项拒绝启动", autoMigrate: &yes, report: schemamigrate.Report{Manual: []string{"type drift"}}, wantUp: true, wantErr: true},
		{name: "false + 库干净", autoMigrate: &no},
		{name: "false + 只有告警", autoMigrate: &no, report: schemamigrate.Report{Warnings: []string{"extra column"}}},
		{name: "false + 有待执行语句", autoMigrate: &no, report: schemamigrate.Report{Statements: []string{"CREATE TABLE trade_favorite ..."}},
			wantErr: true, wantRemedy: true},
		{name: "false + 需人工", autoMigrate: &no, report: schemamigrate.Report{Manual: []string{"type drift"}},
			wantErr: true, wantRemedy: true},
		{name: "false + Plan 出错", autoMigrate: &no, err: errors.New("dial tcp: connection refused"),
			wantErr: true, wantRemedy: true},
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

func TestRunMigrationModifyAuthorization(t *testing.T) {
	for _, allowModify := range []bool{false, true} {
		name := "默认迁移保留类型漂移供人工处理"
		if allowModify {
			name = "显式授权传给迁移器"
		}
		t.Run(name, func(t *testing.T) {
			up := &fakeSchemaRunner{}
			wantCode := schemamigrate.ExitOK
			if !allowModify {
				up.report.Manual = []string{"列类型漂移"}
				wantCode = schemamigrate.ExitManual
			}
			openCalls := 0
			openMySQL := func(c config.MySQLConf) (*sql.DB, error) {
				openCalls++
				// sql.Open 只创建连接池;假 Up 不发查询,整个测试不连接数据库。
				return sql.Open("mysql", "test@tcp(127.0.0.1:1)/mmorpg_trade")
			}
			code := runMigration(config.Config{}, allowModify, openMySQL, up.run)
			if code != wantCode || openCalls != 1 || up.calls != 1 {
				t.Fatalf("迁移应执行一次并保留退出码: code=%d want=%d open=%d up=%d", code, wantCode, openCalls, up.calls)
			}
			if up.opts.AllowModifyColumn != allowModify {
				t.Fatalf("迁移授权未正确传递: got=%v want=%v", up.opts.AllowModifyColumn, allowModify)
			}
			if up.opts.Database != data.DatabaseName || len(up.opts.Tables) != len(data.Tables()) {
				t.Fatalf("迁移授权不得改变目标库或表清单: %+v", up.opts)
			}
		})
	}
}

func TestPrintReport(t *testing.T) {
	var buf bytes.Buffer
	printReport(&buf, schemamigrate.Report{
		Statements: []string{"CREATE TABLE a"},
		Warnings:   []string{"extra column b.x"},
		Manual:     []string{"type drift c.y"},
	})
	out := buf.String()
	for _, want := range []string{"statement: CREATE TABLE a", "warning:   extra column b.x", "MANUAL:    type drift c.y"} {
		if !strings.Contains(out, want) {
			t.Errorf("报告输出缺少 %q:\n%s", want, out)
		}
	}
}
