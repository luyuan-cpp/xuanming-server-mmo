package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"player_inbox/internal/config"
	"player_inbox/internal/data"
	"player_inbox/internal/server"

	base "proto/common/base"
	inboxpb "proto/player_inbox"

	"schemamigrate"

	"shared/killswitch"

	"github.com/zeromicro/go-zero/core/logx/logtest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// chainUnary 按 gRPC 的语义把一组一元拦截器串成一个 handler:切片里第一个是最外层(与 trade 的同名测试相同)。
func chainUnary(interceptors []grpc.UnaryServerInterceptor, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) grpc.UnaryHandler {
	next := handler
	for i := len(interceptors) - 1; i >= 0; i-- {
		cur, downstream := interceptors[i], next
		next = func(ctx context.Context, req any) (any, error) {
			return cur(ctx, req, info, downstream)
		}
	}
	return next
}

// TestUnaryChainGatesAdminMethods:链长 4(grpcstats / killswitch / 运维鉴权 / serverbase);
// 运维方法没口令 → PermissionDenied 且 handler 不执行;带对口令放行;scene 的 Fetch 不受口令影响。
func TestUnaryChainGatesAdminMethods(t *testing.T) {
	logtest.Discard(t)
	const token = "0123456789abcdef-main-test"
	chain := buildUnaryInterceptors(killswitch.New(killswitch.Config{}), token)
	if len(chain) != 4 {
		t.Fatalf("拦截器链长度应为 4,实际 %d", len(chain))
	}
	call := func(method string, md metadata.MD) (bool, error) {
		called := false
		ctx := context.Background()
		if md != nil {
			ctx = metadata.NewIncomingContext(ctx, md)
		}
		h := chainUnary(chain, &grpc.UnaryServerInfo{FullMethod: method}, func(context.Context, any) (any, error) {
			called = true
			return &inboxpb.GmSanctionPlayerResponse{}, nil
		})
		_, err := h(ctx, nil)
		return called, err
	}
	if called, err := call(inboxpb.PlayerInboxAdmin_GmSanctionPlayer_FullMethodName, nil); called || status.Code(err) != codes.PermissionDenied {
		t.Fatalf("没口令: called=%v err=%v", called, err)
	}
	if called, err := call(inboxpb.PlayerInboxAdmin_GmSanctionPlayer_FullMethodName, metadata.Pairs(server.AdminTokenMetadataKey, token)); !called || err != nil {
		t.Fatalf("带对口令: called=%v err=%v", called, err)
	}
	if called, err := call(inboxpb.PlayerInbox_FetchItems_FullMethodName, nil); !called || err != nil {
		t.Fatalf("Fetch 不经口令: called=%v err=%v", called, err)
	}
}

// TestKillSwitchShortCircuitsBeforeAdminAuth:命中关停规则时直接 Unavailable,连鉴权都不做(killswitch 在鉴权之外层)。
func TestKillSwitchShortCircuitsBeforeAdminAuth(t *testing.T) {
	logtest.Discard(t)
	ks := killswitch.New(killswitch.Config{})
	ks.SetRules(map[string]killswitch.Rule{
		strings.TrimPrefix(inboxpb.PlayerInboxAdmin_VoidItem_FullMethodName, "/"): {Deny: true, Reason: "单测"},
	})
	chain := buildUnaryInterceptors(ks, "")
	h := chainUnary(chain, &grpc.UnaryServerInfo{FullMethod: inboxpb.PlayerInboxAdmin_VoidItem_FullMethodName},
		func(context.Context, any) (any, error) { return nil, errors.New("handler must not run") })
	if _, err := h(context.Background(), nil); status.Code(err) != codes.Unavailable {
		t.Fatalf("命中关停应 Unavailable(先于鉴权的 PermissionDenied),got %v", err)
	}
}

func TestNodeInfoValueMatchesRegistryContract(t *testing.T) {
	raw, err := nodeInfoValueBuilder(2, "10.0.0.9", 50950, 1_700_000_000)(7, "uuid-inbox-test")
	if err != nil {
		t.Fatalf("BuildValue 失败: %v", err)
	}
	var mirror struct {
		NodeId       uint32 `json:"nodeId"`
		NodeType     uint32 `json:"nodeType"`
		ZoneId       uint32 `json:"zoneId"`
		NodeUuid     string `json:"nodeUuid"`
		ProtocolType uint32 `json:"protocolType"`
		GrpcEndpoint struct {
			Ip   string `json:"ip"`
			Port uint32 `json:"port"`
		} `json:"grpcEndpoint"`
	}
	if err := json.Unmarshal(raw, &mirror); err != nil {
		t.Fatalf("NodeInfo protojson 无法解析: %v\n%s", err, raw)
	}
	if uint32(base.ENodeType_PlayerInboxNodeService) != 30 {
		t.Fatalf("PlayerInboxNodeService 枚举值应为 30,实际 %d", base.ENodeType_PlayerInboxNodeService)
	}
	if mirror.NodeId != 7 || mirror.ZoneId != 2 || mirror.NodeUuid != "uuid-inbox-test" || mirror.NodeType != 30 ||
		mirror.ProtocolType != uint32(base.ENodeProtocolType_PROTOCOL_GRPC) ||
		mirror.GrpcEndpoint.Ip != "10.0.0.9" || mirror.GrpcEndpoint.Port != 50950 {
		t.Fatalf("NodeInfo = %+v", mirror)
	}
	if got := base.ENodeType_name[int32(nodeType)] + ".rpc"; got != "PlayerInboxNodeService.rpc" {
		t.Fatalf("注册前缀应为 PlayerInboxNodeService.rpc(scene 按它发现),实际 %q", got)
	}
}

func TestValidateMigrationFlags(t *testing.T) {
	if err := validateMigrationFlags(false, true); err == nil {
		t.Error("-allow-modify 不带 -migrate 必须拒绝")
	}
	for _, ok := range [][2]bool{{false, false}, {true, false}, {true, true}} {
		if err := validateMigrationFlags(ok[0], ok[1]); err != nil {
			t.Errorf("validateMigrationFlags(%v, %v) = %v", ok[0], ok[1], err)
		}
	}
}

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

// fakeLayoutCheck 记录行布局核对是否被调用,并返回预设结果。
type fakeLayoutCheck struct {
	err   error
	calls int
}

func (f *fakeLayoutCheck) run(context.Context, *sql.DB) error {
	f.calls++
	return f.err
}

// TestEnsureSchema 钉住启动期建表策略(D-14 第 4 条,同 go/trade)。
func TestEnsureSchema(t *testing.T) {
	logtest.Discard(t)
	yes, no := true, false
	auto := config.Config{Schema: config.SchemaConf{AutoMigrate: &yes}}
	planOnly := config.Config{Schema: config.SchemaConf{AutoMigrate: &no}}

	up, plan, check := &fakeSchemaRunner{}, &fakeSchemaRunner{}, &fakeLayoutCheck{}
	if err := ensureSchema(context.Background(), nil, auto, up.run, plan.run, check.run); err == nil || up.calls != 0 || plan.calls != 0 {
		t.Fatalf("AutoMigrate=true 必须在 runner 前拒绝: err=%v up=%d plan=%d", err, up.calls, plan.calls)
	}
	if check.calls != 0 {
		t.Fatalf("自动 DDL 被拒绝后不得继续核验,实际 %d 次", check.calls)
	}
	if opts := schemaOptions(); opts.Database != data.DatabaseName || len(opts.Tables) != 3 {
		t.Fatalf("迁移选项 = %+v,应断言独占库并包含三张表", opts)
	}
	up, check = &fakeSchemaRunner{report: schemamigrate.Report{Manual: []string{"type drift"}}}, &fakeLayoutCheck{}
	if err := ensureSchema(context.Background(), nil, auto, up.run, plan.run, check.run); err == nil || check.calls != 0 {
		t.Fatalf("出现需人工项必须拒启,且不跑布局核对: err=%v check=%d", err, check.calls)
	}
	up, check = &fakeSchemaRunner{err: errors.New("lock busy")}, &fakeLayoutCheck{}
	if err := ensureSchema(context.Background(), nil, auto, up.run, plan.run, check.run); err == nil || check.calls != 0 {
		t.Fatalf("Up 失败必须拒启,且不跑布局核对: err=%v check=%d", err, check.calls)
	}
	plan = &fakeSchemaRunner{report: schemamigrate.Report{Statements: []string{"CREATE TABLE ..."}}}
	err := ensureSchema(context.Background(), nil, planOnly, up.run, plan.run, check.run)
	if err == nil || !strings.Contains(err.Error(), migrateRemedyCommand) {
		t.Fatalf("plan 不干净必须拒启并带补救命令: %v", err)
	}
	plan, check = &fakeSchemaRunner{}, &fakeLayoutCheck{}
	if err := ensureSchema(context.Background(), nil, planOnly, up.run, plan.run, check.run); err != nil || check.calls != 1 {
		t.Fatalf("plan 干净应通过并跑一次布局核对: err=%v check=%d", err, check.calls)
	}
}

// TestEnsureSchemaRefusesBadRowLayout:表结构对齐但行布局核对不过(旧分片公式写出的存量行)时,两条启动路径都必须拒启。
func TestEnsureSchemaRefusesBadRowLayout(t *testing.T) {
	logtest.Discard(t)
	yes, no := true, false
	bad := errors.New("player_inbox_stream has 1 (shard, player_id) pair(s) whose shard is not RowShard(player_id)")
	for _, c := range []config.Config{
		{Schema: config.SchemaConf{AutoMigrate: &yes}},
		{Schema: config.SchemaConf{AutoMigrate: &no}},
	} {
		check := &fakeLayoutCheck{err: bad}
		err := ensureSchema(context.Background(), nil, c, (&fakeSchemaRunner{}).run, (&fakeSchemaRunner{}).run, check.run)
		if c.ShouldAutoMigrate() {
			if err == nil || check.calls != 0 {
				t.Fatalf("自动 DDL 应先拒绝: err=%v check=%d", err, check.calls)
			}
			continue
		}
		if err == nil || !errors.Is(err, bad) || check.calls != 1 {
			t.Fatalf("AutoMigrate=%v: 布局核对失败必须拒启并包住原因: err=%v check=%d", *c.Schema.AutoMigrate, err, check.calls)
		}
	}
}
