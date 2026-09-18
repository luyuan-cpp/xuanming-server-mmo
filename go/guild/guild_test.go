package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"guild/internal/config"
	"guild/internal/data"
	"guild/internal/logic"
	"guild/internal/session"
	base "proto/common/base"
	pb "proto/guild"
	"schemamigrate"
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

	chain := buildUnaryInterceptors(ks, time.Second)

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
	h := chainUnary(buildUnaryInterceptors(ks, time.Second), info, func(ctx context.Context, req any) (any, error) {
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

// TestSessionGateWiredIntoUnaryChain 证明会话准入**确实挂在**拦截器链上:带客户端会话调用
// 内部方法 UpdateGuildScore 必须在 handler 之前被拒。与 killswitch 那条同理 —— 删掉
// buildUnaryInterceptors 里 session 那一行,其余测试与联调全绿,客户端却能任意改公会积分。
func TestSessionGateWiredIntoUnaryChain(t *testing.T) {
	raw, err := proto.Marshal(&base.SessionDetails{SessionId: 1, PlayerId: 42})
	if err != nil {
		t.Fatal(err)
	}
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(session.MetadataKey, base64.StdEncoding.EncodeToString(raw)))

	handlerCalled := false
	info := &grpc.UnaryServerInfo{FullMethod: pb.GuildService_UpdateGuildScore_FullMethodName}
	h := chainUnary(buildUnaryInterceptors(killswitch.New(killswitch.Config{}), time.Second), info,
		func(ctx context.Context, req any) (any, error) {
			handlerCalled = true
			return &pb.UpdateGuildScoreResponse{}, nil
		})

	_, err = h(ctx, &pb.UpdateGuildScoreRequest{GuildId: 1, Score: 999999})
	if handlerCalled {
		t.Fatal("客户端会话调用 UpdateGuildScore 走到了 handler:会话准入没有挂上拦截器链")
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("期望 PermissionDenied,得到 %v", err)
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
	h := chainUnary(buildUnaryInterceptors(killswitch.New(killswitch.Config{}), 300*time.Millisecond), info,
		func(ctx context.Context, req any) (any, error) {
			deadline, hasDeadline = ctx.Deadline()
			return &pb.GetGuildResponse{}, nil
		})

	// 不带会话 metadata:internal/session 规定无会话即内部调用,放行到 handler。
	if _, err := h(context.Background(), &pb.GetGuildRequest{}); err != nil {
		t.Fatalf("内部调用不该报错: %v", err)
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
	missingIndex := schemamigrate.Report{Warnings: []string{"缺索引(不会自动建):guild 上没有 proto 声明的索引 idx_guild_1"}}
	extraColumn := schemamigrate.Report{Warnings: []string{"多余列(不会删除):guild.legacy_col"}}
	manual := schemamigrate.Report{Manual: []string{"列类型漂移:guild.funds"}}
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
