package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"guild/internal/config"
	"guild/internal/data"
	"guild/internal/logic"
	pb "proto/guild"
	"schemamigrate"
	"shared/callerauth/callerauthtest"
	"shared/generated/clientrpc"
	"shared/kafkautil"
	"shared/killswitch"
	"shared/rolefence"
	"shared/roleidentity"
)

func TestNodeAdvertiseHostPinsPodIP(t *testing.T) {
	t.Setenv("POD_IP", "10.12.34.56")
	for _, listen := range []string{"0.0.0.0", "::", "127.0.0.1"} {
		host, err := nodeAdvertiseHost(listen)
		if err != nil || host != "10.12.34.56" {
			t.Fatalf("监听%s应注册显式PodIP,得到%s/%v", listen, host, err)
		}
	}
	for _, invalid := range []string{"", "0.0.0.0", "::", "not-an-ip"} {
		t.Setenv("POD_IP", invalid)
		if _, err := nodeAdvertiseHost("127.0.0.1"); err == nil {
			t.Fatalf("显式非法POD_IP=%q不得回退", invalid)
		}
	}
	if err := os.Unsetenv("POD_IP"); err != nil {
		t.Fatal(err)
	}
	if host, err := nodeAdvertiseHost("127.0.0.1"); err != nil || host != "127.0.0.1" {
		t.Fatalf("本地监听兼容失败: %s/%v", host, err)
	}
	for _, wildcard := range []string{"", "0.0.0.0", "::"} {
		if _, err := nodeAdvertiseHost(wildcard); err == nil {
			t.Fatalf("无POD_IP不能注册通配监听%s", wildcard)
		}
	}
}

func TestGlobalGuildTrialRequiresDurableTopicBeforeServing(t *testing.T) {
	c := config.Config{}
	c.Kafka.Brokers = []string{"broker:9092"}
	c.Activity.TrialResult.Enabled = true
	c.Activity.TrialResult.Topic = "match-results"
	c.Activity.TrialResult.GroupID = "guild-trial"
	c.Activity.TrialResult.Partitions = 3
	calls := 0
	blocked := errors.New("existing topic RF1")
	ensure := func(brokers []string, specs []kafkautil.TopicSpec) error {
		calls++
		// 结果 topic 与本组死信 topic(2026-11-12 起,有界重试后转死信)同一套严格耐久契约,注册前一起核。
		if len(brokers) != 1 || len(specs) != 2 || specs[0].Partitions != 3 || specs[1].Name != "guild-trial-dlq" {
			t.Fatalf("全局启动必须同时核验结果 topic 与死信 topic: %+v", specs)
		}
		for _, spec := range specs {
			if !spec.RequireDurability || spec.ReplicaFactor != 3 {
				t.Fatal("全局启动必须显式核验RF3严格策略")
			}
		}
		return blocked
	}
	if err := ensureTrialResultTopic(c, ensure); !errors.Is(err, blocked) || calls != 1 {
		t.Fatalf("耐久门禁失败不能转为后台放行: %v calls=%d", err, calls)
	}
	c.MySQL.Backend = "mysql"
	c.Mode = "dev"
	if err := ensureTrialResultTopic(c, ensure); err != nil || calls != 1 {
		t.Fatalf("本地 mysql 档不应同步连接Kafka: %v calls=%d", err, calls)
	}
	// 生产 mysql 档的结算同样要耐久:省钱档不能顺带丢掉 RF3 门禁。
	c.Mode = "pro"
	if err := ensureTrialResultTopic(c, ensure); !errors.Is(err, blocked) || calls != 2 {
		t.Fatalf("生产 mysql 档必须核验耐久: %v calls=%d", err, calls)
	}
	c.MySQL.Backend = ""
	c.Activity.TrialResult.Enabled = false
	if err := ensureTrialResultTopic(c, ensure); err != nil || calls != 2 {
		t.Fatalf("活动关闭不应创建结果topic: %v calls=%d", err, calls)
	}
}

// chainUnary 按 gRPC 的语义把一组一元拦截器串成一个 handler:
// 切片里第一个是最外层,最后一个紧贴业务 handler。
//
// grpc-go 自己的 chainUnaryInterceptors 是包内私有函数,拿不到,
// 所以这里复刻一份(语义与 grpc.ChainUnaryInterceptor 的文档一致:
// "The first interceptor will be the outer most")。
// 这个副本在 friend / player_locator / scene_manager 的同名测试里逐字相同 ——
// 四个服务是四个独立 go module,没有共同的测试工具包可放。
// guildEntry 是 guild.go 装配的同一个入口准入(GetPlayerGuild 登记为匿名只读例外)。
func guildEntry(t *testing.T) grpc.UnaryServerInterceptor {
	t.Helper()
	entry, err := roleidentity.EntryInterceptor(clientrpc.Callable, guildAnonymousReads...)
	if err != nil {
		t.Fatalf("入口准入装配失败: %v", err)
	}
	return entry
}

// guildSession 模拟 gate 发来、已过验签拦截器的客户端请求:会话带完整原身份,签名绑定 method。
func guildSession(t *testing.T, method string, playerID uint64) context.Context {
	t.Helper()
	return callerauthtest.Incoming(t, method, callerauthtest.OnlineSession("guild-account", playerID, 7))
}

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

	chain := buildUnaryInterceptors(ks, time.Second, guildEntry(t))

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

		if _, err := h(guildSession(t, info.FullMethod, 42), &pb.GetGuildRequest{}); err != nil {
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
	h := chainUnary(buildUnaryInterceptors(ks, time.Second, guildEntry(t)), info, func(ctx context.Context, req any) (any, error) {
		handlerCalled = true
		return &pb.CreateGuildResponse{}, nil
	})

	if _, err := h(guildSession(t, info.FullMethod, 42), &pb.CreateGuildRequest{}); err != nil {
		t.Fatalf("无规则时必须放行: %v", err)
	}
	if !handlerCalled {
		t.Fatal("无规则时 handler 必须被调用(fail-open)")
	}
}

// TestSessionGateWiredIntoUnaryChain 证明入口准入**确实挂在**拦截器链上:带客户端会话调用
// 内部方法 UpdateGuildScore(标了 CLIENT_RPC_SERVER_ONLY,不在生成的客户端方法集合里)必须在 handler 之前被拒。
// 与 killswitch 那条同理 —— 删掉 buildUnaryInterceptors 里入口准入那一行,其余测试与联调全绿,客户端却能任意改公会积分。
func TestSessionGateWiredIntoUnaryChain(t *testing.T) {
	handlerCalled := false
	info := &grpc.UnaryServerInfo{FullMethod: pb.GuildService_UpdateGuildScore_FullMethodName}
	h := chainUnary(buildUnaryInterceptors(killswitch.New(killswitch.Config{}), time.Second, guildEntry(t)), info,
		func(ctx context.Context, req any) (any, error) {
			handlerCalled = true
			return &pb.UpdateGuildScoreResponse{}, nil
		})

	_, err := h(guildSession(t, info.FullMethod, 42), &pb.UpdateGuildScoreRequest{GuildId: 1, Score: 999999})
	if handlerCalled {
		t.Fatal("客户端会话调用 UpdateGuildScore 走到了 handler:会话准入没有挂上拦截器链")
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("期望 PermissionDenied,得到 %v", err)
	}
}

// TestOwnershipPrecheckWiredIntoUnaryChain 证明原角色归属预检**确实挂在**拦截器链上,且在入口准入之后、handler 之前
// (docs/design/gate-online-identity.md §9.3)。它读本库护栏副本、不加锁,是 guild 读方法唯一的归属检查。
// 判定函数换成测试给的结果,拒绝判据用 guild.go 装配的同一个 guildOwnerRejected。
func TestOwnershipPrecheckWiredIntoUnaryChain(t *testing.T) {
	for _, tc := range []struct {
		name     string
		checkErr error
		code     codes.Code
	}{
		{"角色仍归原身份", nil, codes.OK},
		{"角色已换主、已删或被冻结", data.ErrWriteConflict, codes.PermissionDenied},
		{"目录里没有这名角色", &rolefence.RoleNotFoundError{Players: []uint64{42}}, codes.PermissionDenied},
		// 目录读不到、库故障:客户端要能重试,不能告诉它角色变了;只读、确定没写,也不回 Unavailable 免得打成排空 Hold。
		{"这一次判不了", errors.New("目录读不到"), codes.Aborted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checked, handlerCalled := false, false
			ownership, err := roleidentity.OwnershipInterceptor(func(ctx context.Context) error {
				checked = true
				original, ok := roleidentity.Original(ctx)
				if !ok || original.Role.GetPlayerId() != 42 || original.Account != "guild-account" || original.OwnerRevision != 7 {
					t.Errorf("归属预检要按入口准入捕获的原身份判,实际 %v(ok=%v)", original, ok)
				}
				return tc.checkErr
			}, guildOwnerRejected)
			if err != nil {
				t.Fatalf("归属预检装配失败: %v", err)
			}
			info := &grpc.UnaryServerInfo{FullMethod: pb.GuildService_GetGuild_FullMethodName}
			h := chainUnary(buildUnaryInterceptors(killswitch.New(killswitch.Config{}), time.Second, guildEntry(t), ownership), info,
				func(ctx context.Context, req any) (any, error) {
					handlerCalled = true
					return &pb.GetGuildResponse{}, nil
				})

			_, err = h(guildSession(t, info.FullMethod, 42), &pb.GetGuildRequest{})
			if !checked {
				t.Fatal("归属判定没有被调用:预检没有挂上拦截器链(或排在入口准入之前,拿不到原身份)")
			}
			if status.Code(err) != tc.code {
				t.Fatalf("期望 %v,得到 %v", tc.code, err)
			}
			if handlerCalled != (tc.code == codes.OK) {
				t.Fatalf("handler 调用与结果不符: called=%v code=%v", handlerCalled, tc.code)
			}
		})
	}
}

// TestOwnershipPrecheckSkipsCallsWithoutOriginal:不带会话的调用(内部调用、匿名只读例外 GetPlayerGuild)ctx 里没有原身份,
// 不做归属预检;入口准入就拒掉的身份(缺修订)也不进预检。
func TestOwnershipPrecheckSkipsCallsWithoutOriginal(t *testing.T) {
	checked := false
	ownership, err := roleidentity.OwnershipInterceptor(func(context.Context) error {
		checked = true
		return data.ErrWriteConflict
	}, guildOwnerRejected)
	if err != nil {
		t.Fatalf("归属预检装配失败: %v", err)
	}
	chain := buildUnaryInterceptors(killswitch.New(killswitch.Config{}), time.Second, guildEntry(t), ownership)

	handlerCalled := false
	anonymous := &grpc.UnaryServerInfo{FullMethod: pb.GuildService_GetPlayerGuild_FullMethodName}
	if _, err := chainUnary(chain, anonymous, func(context.Context, any) (any, error) {
		handlerCalled = true
		return &pb.GetPlayerGuildResponse{}, nil
	})(context.Background(), &pb.GetPlayerGuildRequest{}); err != nil {
		t.Fatalf("匿名只读例外必须放行: %v", err)
	}
	if checked || !handlerCalled {
		t.Fatalf("不带会话的调用不做归属预检且要走到 handler: checked=%v handlerCalled=%v", checked, handlerCalled)
	}

	incomplete := &grpc.UnaryServerInfo{FullMethod: pb.GuildService_GetGuild_FullMethodName}
	ctx := callerauthtest.Incoming(t, incomplete.FullMethod, callerauthtest.OnlineSession("guild-account", 42, 0))
	_, err = chainUnary(chain, incomplete, func(context.Context, any) (any, error) {
		t.Error("缺修订的身份不该走到 handler")
		return nil, nil
	})(ctx, &pb.GetGuildRequest{})
	if status.Code(err) != codes.PermissionDenied || checked {
		t.Fatalf("入口准入就拒掉的身份不进归属预检: code=%v checked=%v", status.Code(err), checked)
	}
}

// TestOnlyGetPlayerGuildAcceptsCallsWithoutSession 钉住 docs/design/gate-online-identity.md §7.5 的关闭:
// 不带会话的调用只剩只读的 GetPlayerGuild(mail 领附件在用)能进 handler;建帮、退帮、解散、改公告
// 以前会按请求体身份执行,现在在入口就被 PermissionDenied。
func TestOnlyGetPlayerGuildAcceptsCallsWithoutSession(t *testing.T) {
	for _, tc := range []struct {
		method string
		allow  bool
	}{
		{pb.GuildService_GetPlayerGuild_FullMethodName, true},
		{pb.GuildService_CreateGuild_FullMethodName, false},
		{pb.GuildService_LeaveGuild_FullMethodName, false},
		{pb.GuildService_DisbandGuild_FullMethodName, false},
		{pb.GuildService_SetAnnouncement_FullMethodName, false},
	} {
		t.Run(tc.method, func(t *testing.T) {
			handlerCalled := false
			info := &grpc.UnaryServerInfo{FullMethod: tc.method}
			h := chainUnary(buildUnaryInterceptors(killswitch.New(killswitch.Config{}), time.Second, guildEntry(t)), info,
				func(ctx context.Context, req any) (any, error) {
					handlerCalled = true
					return nil, nil
				})
			_, err := h(context.Background(), nil)
			if handlerCalled != tc.allow {
				t.Fatalf("无会话调用进 handler = %v,期望 %v", handlerCalled, tc.allow)
			}
			if !tc.allow && status.Code(err) != codes.PermissionDenied {
				t.Fatalf("期望 PermissionDenied,得到 %v", err)
			}
		})
	}
}

// ── 整请求预算 ────────────────────────────────────────────────

// TestRequestBudgetInterceptorSetsDeadline 证明业务预算真的套在 handler 的 ctx 上:
// 没有它,归属区查询 / 发号 / MySQL 可以一直等到 zrpc 服务端超时,客户端拿到的是
// DeadlineExceeded 而不是 in-band tip。
func TestRequestBudgetInterceptorSetsDeadline(t *testing.T) {
	info := &grpc.UnaryServerInfo{FullMethod: pb.GuildService_GetGuild_FullMethodName}
	var deadline time.Time
	var hasDeadline bool
	h := chainUnary(buildUnaryInterceptors(killswitch.New(killswitch.Config{}), 300*time.Millisecond, guildEntry(t)), info,
		func(ctx context.Context, req any) (any, error) {
			deadline, hasDeadline = ctx.Deadline()
			return &pb.GetGuildResponse{}, nil
		})

	// 带完整原身份的客户端请求:过了入口准入才到预算拦截器。
	if _, err := h(guildSession(t, info.FullMethod, 42), &pb.GetGuildRequest{}); err != nil {
		t.Fatalf("合法客户端请求不该报错: %v", err)
	}
	if !hasDeadline {
		t.Fatal("handler 的 ctx 没有截止时间:预算拦截器没挂上")
	}
	if remaining := time.Until(deadline); remaining <= 200*time.Millisecond || remaining > 300*time.Millisecond {
		t.Fatalf("剩余预算 %v,期望落在 (200ms, 300ms]", remaining)
	}
}

func TestRequestBudgetInterceptorExpires(t *testing.T) {
	info := &grpc.UnaryServerInfo{FullMethod: pb.GuildService_GetGuild_FullMethodName}
	var cause error
	_, err := requestBudgetInterceptor(50*time.Millisecond)(context.Background(), nil, info,
		func(ctx context.Context, req any) (any, error) {
			<-ctx.Done()
			cause = ctx.Err()
			return nil, nil
		})
	if err != nil {
		t.Fatalf("handler 返回 nil 错误时拦截器不该改写: %v", err)
	}
	if !errors.Is(cause, context.DeadlineExceeded) {
		t.Fatalf("ctx.Err() = %v,期望 DeadlineExceeded", cause)
	}
}

// TestHomeZoneBudgetMirrorsLogic:config 不能 import logic,只能镜像常量;两者分叉时预算校验会算错。
func TestHomeZoneBudgetMirrorsLogic(t *testing.T) {
	if got := time.Duration(config.HomeZoneLookupBudgetMs) * time.Millisecond; got != logic.DefaultHomeZoneLookupTimeout {
		t.Fatalf("config.HomeZoneLookupBudgetMs = %v,logic.DefaultHomeZoneLookupTimeout = %v",
			got, logic.DefaultHomeZoneLookupTimeout)
	}
}

// ── 启动期建表策略 ────────────────────────────────────────────

func TestSchemaOptionsTargetsGuildDatabase(t *testing.T) {
	opts := schemaOptions()
	if opts.Database != data.DatabaseName {
		t.Fatalf("Database = %q,期望 %q", opts.Database, data.DatabaseName)
	}
	if len(opts.Tables) != len(data.Tables()) {
		t.Fatalf("Tables 数 = %d,期望与 data.Tables() 一致(%d)", len(opts.Tables), len(data.Tables()))
	}
}

type fakeSchemaRunner struct {
	reports []schemamigrate.Report
	errs    []error
	calls   int
}

func (f *fakeSchemaRunner) run(ctx context.Context, db *sql.DB, opts schemamigrate.Options) (schemamigrate.Report, error) {
	i := f.calls
	f.calls++
	if i >= len(f.reports) {
		i = len(f.reports) - 1
	}
	var err error
	if f.calls-1 < len(f.errs) {
		err = f.errs[f.calls-1]
	} else if len(f.errs) > 0 {
		err = f.errs[len(f.errs)-1]
	}
	return f.reports[i], err
}

func autoMigrate(v bool) config.Config {
	c := config.Config{}
	c.Schema.AutoMigrate = &v
	return c
}

func TestEnsureSchemaStrategy(t *testing.T) {
	old := lockBusyRetryDelay
	lockBusyRetryDelay = 0
	t.Cleanup(func() { lockBusyRetryDelay = old })

	clean := schemamigrate.Report{}
	// 缺索引与多余列都只在 Warnings 里;guild 按 Findings 的结构化类别区分两者,不看文案。
	const missingIndexMsg = "missing index (not created automatically): table guild lacks index idx_guild_1 declared in proto"
	const extraColumnMsg = "extra column (never dropped): guild.legacy_col is not declared in proto"
	missingIndex := schemamigrate.Report{Warnings: []string{missingIndexMsg}, Findings: []schemamigrate.Finding{
		{Kind: schemamigrate.FindingMissingIndex, Table: "guild", Index: "idx_guild_1", Message: missingIndexMsg}}}
	extraColumn := schemamigrate.Report{Warnings: []string{extraColumnMsg}, Findings: []schemamigrate.Finding{
		{Kind: schemamigrate.FindingExtraColumn, Table: "guild", Column: "legacy_col", Message: extraColumnMsg}}}
	manual := schemamigrate.Report{Manual: []string{"column type drift (not modified automatically): guild.funds"}}
	pending := schemamigrate.Report{Statements: []string{"ALTER TABLE guild ADD COLUMN funds bigint unsigned"}}

	cases := []struct {
		name      string
		cfg       config.Config
		up, plan  *fakeSchemaRunner
		wantErr   string
		wantCalls int
	}{
		{name: "自动建表成功", cfg: autoMigrate(true),
			up: &fakeSchemaRunner{reports: []schemamigrate.Report{clean}}, wantCalls: 1},
		{name: "自动建表出错即拒启", cfg: autoMigrate(true),
			up:      &fakeSchemaRunner{reports: []schemamigrate.Report{clean}, errs: []error{errors.New("boom")}},
			wantErr: "启动期建表失败", wantCalls: 1},
		{name: "需人工项即拒启", cfg: autoMigrate(true),
			up:      &fakeSchemaRunner{reports: []schemamigrate.Report{manual}},
			wantErr: "需人工处理", wantCalls: 1},
		{name: "锁忙重试后成功", cfg: autoMigrate(true),
			up: &fakeSchemaRunner{reports: []schemamigrate.Report{clean},
				errs: []error{schemamigrate.ErrLockBusy, schemamigrate.ErrLockBusy, nil}}, wantCalls: 3},
		{name: "锁忙用尽重试", cfg: autoMigrate(true),
			up: &fakeSchemaRunner{reports: []schemamigrate.Report{clean},
				errs: []error{schemamigrate.ErrLockBusy}}, wantErr: "启动期建表失败", wantCalls: lockBusyAttempts},
		{name: "缺索引在 Up 模式也拒启", cfg: autoMigrate(true),
			up: &fakeSchemaRunner{reports: []schemamigrate.Report{missingIndex}}, wantErr: "缺 1 个", wantCalls: 1},
		{name: "多余列只是告警", cfg: autoMigrate(true),
			up: &fakeSchemaRunner{reports: []schemamigrate.Report{extraColumn}}, wantCalls: 1},
		{name: "只读核对通过", cfg: autoMigrate(false),
			plan: &fakeSchemaRunner{reports: []schemamigrate.Report{clean}}, wantCalls: 1},
		{name: "只读核对发现待执行语句", cfg: autoMigrate(false),
			plan: &fakeSchemaRunner{reports: []schemamigrate.Report{pending}}, wantErr: migrateRemedyCommand, wantCalls: 1},
		{name: "只读核对缺索引", cfg: autoMigrate(false),
			plan: &fakeSchemaRunner{reports: []schemamigrate.Report{missingIndex}}, wantErr: "缺 1 个", wantCalls: 1},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up, plan := tc.up, tc.plan
			if up == nil {
				up = &fakeSchemaRunner{reports: []schemamigrate.Report{clean}}
			}
			if plan == nil {
				plan = &fakeSchemaRunner{reports: []schemamigrate.Report{clean}}
			}
			err := ensureSchema(context.Background(), nil, tc.cfg, up.run, plan.run)
			if tc.cfg.ShouldAutoMigrate() {
				if err == nil || up.calls != 0 || plan.calls != 0 {
					t.Fatalf("常驻启动不得申请 DDL: err=%v up=%d plan=%d", err, up.calls, plan.calls)
				}
				return
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("期望通过,得到 %v", err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("错误 = %v,期望包含 %q", err, tc.wantErr)
				}
			}
			calls := up.calls
			if !tc.cfg.ShouldAutoMigrate() {
				calls = plan.calls
			}
			if calls != tc.wantCalls {
				t.Fatalf("runner 调用 %d 次,期望 %d 次", calls, tc.wantCalls)
			}
		})
	}
}

func TestReportLinesFormatsAllSections(t *testing.T) {
	lines := reportLines(schemamigrate.Report{
		Statements: []string{"CREATE TABLE guild"},
		Warnings:   []string{"多余列"},
		Manual:     []string{"缺主键"},
	})
	if len(lines) != 3 ||
		!strings.Contains(lines[0], "statement:") ||
		!strings.Contains(lines[1], "warning:") ||
		!strings.Contains(lines[2], "MANUAL:") {
		t.Fatalf("三段格式不符: %v", lines)
	}
}

// TestAdvertisedHost(2026-10-16,K8s 上线批):写进 NodeInfo 的地址必须对外可拨。K8s 上 ListenOn 是 0.0.0.0,
// 原样写进去路由服拨不通;清单经 downward API 注入 POD_IP,优先用它。
func TestAdvertisedHost(t *testing.T) {
	t.Setenv("POD_IP", "10.1.2.3")
	if got := advertisedHost("0.0.0.0"); got != "10.1.2.3" {
		t.Fatalf("有 POD_IP 时应通告它,得到 %q", got)
	}
	t.Setenv("POD_IP", "")
	if got := advertisedHost("127.0.0.1"); got != "127.0.0.1" {
		t.Fatalf("ListenOn 是具体地址时原样通告,得到 %q", got)
	}
	for _, unspecified := range []string{"0.0.0.0", "::", ""} {
		if got := advertisedHost(unspecified); isUnspecifiedHost(got) {
			t.Fatalf("ListenOn=%q 时不得通告「监听所有网卡」的地址,得到 %q", unspecified, got)
		}
	}
}
