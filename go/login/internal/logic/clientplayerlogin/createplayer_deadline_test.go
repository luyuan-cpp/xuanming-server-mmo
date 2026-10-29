package clientplayerloginlogic

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	"login/internal/constants"
	"login/internal/logic/pkg/homezone"
	dspb "proto/data_service"
	"shared/generated/pb/table"
	"shared/idsegment"
	"shared/playername"
)

// CreatePlayer 的截止语义(docs/design/client-request-ordering.md §6.1 / O11):请求 ctx 一旦截止 / 取消,
// 建角不再发出任何新的写,只做补偿。截止之后 go-zero 超时拦截器已经给路由服回了错,gate 随即放行同一会话的
// 下一条请求,这时再发出的写都可能晚于下一条落地。
//
// harness 沿用 createplayer_name_test.go(newNameHarness)与 createplayer_seed_test.go(setNewPlayerSeed)。
// 不能一开始就传已取消的 ctx:那样第 2 步 GetAccount 先失败,根本走不到持锁之后的那几次写;所以每个用例
// 都在某一步**返回之后**才取消,模拟截止恰好落在两步之间。
//
// 为什么不只看「账号 blob 没变」:假 data_service 客户端不看 ctx,而 go-redis 对已取消的 ctx 会在取连接
// 之前原地拒掉命令 —— 不显式查 ctx 时,data_service 那几步照样「发出去」,Redis 那几步也照样落不了地,
// 只看最终状态分不出两者。所以这里同时钉住:data_service 的调用序列,以及取消之后 login 有没有**尝试**
// 碰账号 blob / 反查映射 / 种子键(钩子在 go-redis 取连接之前执行,被拒掉的命令也看得见)。

// cancellingSegment 发出号之后立刻取消请求 ctx:模拟截止恰好落在发号返回的那一刻。
type cancellingSegment struct {
	id     uint64
	cancel context.CancelFunc
}

func (s *cancellingSegment) Next(context.Context) (uint64, error) {
	s.cancel()
	return s.id, nil
}

// registerHookClient 在 RegisterPlayerZone 成功返回之前跑一次 after,其余 RPC 原样交给 fakeRegisterClient
// (事件序列仍记在同一个 fake 上)。
type registerHookClient struct {
	*fakeRegisterClient
	after func()
}

func (c *registerHookClient) RegisterPlayerZone(ctx context.Context, in *dspb.RegisterPlayerZoneRequest, opts ...grpc.CallOption) (*emptypb.Empty, error) {
	resp, err := c.fakeRegisterClient.RegisterPlayerZone(ctx, in, opts...)
	c.after()
	return resp, err
}

// commandProbe 是挂在 harness Redis 客户端上的钩子:
//   - 请求 ctx 已结束之后经过的每一条命令(参数逐个转成字符串)都记进 afterDone。走 WithoutCancel 的
//     补偿(建角锁释放)同样会被记下,所以断言只针对具体的键;
//   - cancelWhen 非 nil 时,在第一条满足它、且执行成功的命令返回之后取消请求 ctx。
type commandProbe struct {
	req        context.Context
	cancel     context.CancelFunc
	cancelWhen func(redis.Cmder) bool
	afterDone  [][]string
}

func cmdArgs(cmd redis.Cmder) []string {
	out := make([]string, 0, len(cmd.Args()))
	for _, a := range cmd.Args() {
		out = append(out, fmt.Sprint(a))
	}
	return out
}

func (p *commandProbe) observe(cmd redis.Cmder) {
	if p.req.Err() != nil {
		p.afterDone = append(p.afterDone, cmdArgs(cmd))
	}
}

func (p *commandProbe) maybeCancel(cmd redis.Cmder) {
	if p.cancelWhen != nil && p.req.Err() == nil && cmd.Err() == nil && p.cancelWhen(cmd) {
		p.cancel()
	}
}

func (p *commandProbe) DialHook(next redis.DialHook) redis.DialHook { return next }

func (p *commandProbe) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		p.observe(cmd)
		err := next(ctx, cmd)
		// hook返回前cmd.Err尚未由外层赋值；NOSCRIPT不能被误当成成功写种子。
		if err == nil {
			p.maybeCancel(cmd)
		}
		return err
	}
}

func (p *commandProbe) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		for _, cmd := range cmds {
			p.observe(cmd)
		}
		return next(ctx, cmds)
	}
}

// 回归:请求 ctx 在SQL提交前结束(截止 / 取消)→ 不提交。无论落在哪两步之间:
//   - 账号 blob 一个字节都不变,围栏写连尝试都没有(也就不必回读);反查映射不写;
//   - 之后的 data_service 写(名字登记 / player:zone 登记)不再发出;
//   - 已确定登记的名字按「确定已登记」释放一次(没有延迟的第二次);发号之后就结束的,名字登记一个 RPC
//     都没发,也就不释放;
//   - 建角锁照常释放(走 WithoutCancel);
//   - 取消落在SQL提交前不写可信种子；种子现只在SQL提交后写。
//     原“种子写之后”场景单列于下方具名提交后回归，不得释放已提交角色的名字。
//
// 修复前:发号之后的那一例会照发 Reserve 与 RegisterPlayerZone;登记之后的几例会尝试围栏写、
// 被 go-redis 拒掉之后再回读一次,tip 是 kLoginRedisSetFailed。
// (2026-10-16 补注)6b' 新铸 id 探测落地后,「发号之后」那一例停在探测的 ctx 前置检查,钉住的是 6b' 的预检;
// tryReserveName 开头的预检改由 TestCreatePlayer_IDProbeClearThenDeadlineSendsNoReserve 钉住。
func TestCreatePlayer_RequestDoneMidCreateCommitsNothing(t *testing.T) {
	const requested = "云中君"
	accountKey := constants.GetAccountDataKey(hzAccount)
	reverseKey := constants.PlayerToAccountKey(hzPlayerID)
	seedKey := seedParentKeyFor(hzPlayerID)

	cancelOnRegister := func(n *nameHarness, cancel context.CancelFunc, _ *commandProbe) {
		n.l.svcCtx.HomeZone = homezone.New(&registerHookClient{fakeRegisterClient: n.fake, after: cancel}, 0, 0, 0)
	}

	cases := []struct {
		name       string
		seed       bool // NewPlayerSeed.Enabled
		arm        func(n *nameHarness, cancel context.CancelFunc, probe *commandProbe)
		wantEvents string // data_service 调用序列
		wantSeed   bool   // 结束时种子父键是否在
	}{
		{
			name: "发号之后:名字登记一个 RPC 都不发",
			seed: true,
			arm: func(n *nameHarness, cancel context.CancelFunc, _ *commandProbe) {
				n.l.svcCtx.PlayerIDMinter = &idsegment.Minter{Name: "player_id", Segment: &cancellingSegment{id: hzPlayerID, cancel: cancel}}
			},
			wantEvents: "",
		},
		{
			name: "名字登记之后:不登记 player:zone,名字释放一次",
			seed: true,
			arm: func(n *nameHarness, cancel context.CancelFunc, _ *commandProbe) {
				n.fake.reserve = func(int, *dspb.ReservePlayerNameRequest) (*dspb.ReservePlayerNameResponse, error) {
					cancel()
					return &dspb.ReservePlayerNameResponse{Result: playername.ReserveOK}, nil
				}
			},
			wantEvents: "reserve,release",
		},
		{
			name:       "player:zone 登记之后(种子关):不发围栏写",
			seed:       false,
			arm:        cancelOnRegister,
			wantEvents: "reserve,register,release",
		},
		{
			name:       "player:zone 登记之后(种子开):不写种子,也不发围栏写",
			seed:       true,
			arm:        cancelOnRegister,
			wantEvents: "reserve,register,release",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setNewPlayerSeed(t, tc.seed)
			n := newNameHarness(t)
			before, err := n.mr.Get(accountKey)
			if err != nil {
				t.Fatalf("read seeded account blob: %v", err)
			}
			ctx, cancel := context.WithCancel(n.l.ctx)
			defer cancel()
			n.l.ctx = ctx
			probe := &commandProbe{req: ctx, cancel: cancel}
			n.rdb.AddHook(probe)
			tc.arm(n, cancel, probe)

			resp := n.create(t, requested)

			// 先钉住前提:请求 ctx 确实在建角中途结束了。前提不成立时下面的断言说明不了问题。
			if ctx.Err() == nil {
				t.Fatal("scenario never ended the request ctx")
			}
			requireTip(t, resp, table.LoginError_kLoginDataSerializeFailed)
			if got := strings.Join(n.fake.events, ","); got != tc.wantEvents {
				t.Fatalf("data_service call order = %q, want %q", got, tc.wantEvents)
			}
			for i, req := range n.fake.releaseReqs {
				if req.GetPlayerId() != hzPlayerID || req.GetName() != requested {
					t.Fatalf("release #%d = %d/%q, want %d/%q", i+1, req.GetPlayerId(), req.GetName(), hzPlayerID, requested)
				}
			}
			if len(n.scheduled) != 0 {
				t.Fatalf("delayed releases scheduled = %v, want none (the name was definitely reserved or never sent)", n.scheduled)
			}
			for _, args := range probe.afterDone {
				if slices.Contains(args, accountKey) {
					t.Fatalf("command %q touched the account blob after the request ctx ended", args)
				}
				if slices.Contains(args, reverseKey) {
					t.Fatalf("command %q touched the reverse mapping after the request ctx ended", args)
				}
				if slices.Contains(args, seedKey) {
					t.Fatalf("command %q touched the new-player seed after the request ctx ended", args)
				}
			}
			// 下面直接读 miniredis,不经挂了钩子的客户端。
			if after, err := n.mr.Get(accountKey); err != nil || after != before {
				t.Fatalf("account blob changed (err=%v) although the request ctx ended before the fenced write", err)
			}
			if n.mr.Exists(reverseKey) {
				t.Fatal("reverse mapping written although the create was abandoned")
			}
			if n.mr.Exists(nameCreateKey) {
				t.Fatal("create lock still held after CreatePlayer returned on an ended request ctx")
			}
			if got := n.mr.Exists(seedKey); got != tc.wantSeed {
				t.Fatalf("seed parent key exists = %v, want %v", got, tc.wantSeed)
			}
		})
	}
}

// 原种子返回后取消场景迁到真实提交点之后：缓存不是SQL提交的回滚权。
func TestCreatePlayer_DeadlineAfterCommittedSeedKeepsSQLRoleAndName(t *testing.T) {
	const requested = "云中君"
	setNewPlayerSeed(t, true)
	n := newNameHarness(t)
	ctx, cancel := context.WithCancel(n.l.ctx)
	defer cancel()
	n.l.ctx = ctx
	seedKey := seedParentKeyFor(hzPlayerID)
	commits := 0
	fixtureDirectory(t, n.rdb).afterCommit = func() { commits++ }
	probe := &commandProbe{req: ctx, cancel: cancel}
	probe.cancelWhen = func(cmd redis.Cmder) bool {
		return commits == 1 && slices.Contains(cmdArgs(cmd), seedKey)
	}
	n.rdb.AddHook(probe)

	resp := n.create(t, requested)
	if commits != 1 || ctx.Err() == nil || !n.mr.Exists(seedKey) {
		t.Fatalf("提交后种子取消前提不成立: commits=%d canceled=%v seed=%v", commits, ctx.Err(), n.mr.Exists(seedKey))
	}
	requireCreated(t, resp, nil)
	owned, err := n.l.svcCtx.AccountDirectory.Lookup(context.Background(), hzPlayerID)
	if err != nil || owned == nil || owned.Account != hzAccount || owned.Role.GetName() != requested {
		t.Fatalf("已提交SQL角色/名字未保留: %v/%v", owned, err)
	}
	players := storedPlayers(t, context.Background(), n.rdb)
	if len(players) != 1 || players[0].GetPlayerId() != hzPlayerID || players[0].GetName() != requested {
		t.Fatalf("已提交SQL目录未保留: %v", players)
	}
	if got := strings.Join(n.fake.events, ","); got != "reserve,register" || len(n.fake.releaseReqs) != 0 || len(n.scheduled) != 0 {
		t.Fatalf("提交后错误释放名字或发送后续目录写: events=%q releases=%d delayed=%v", got, len(n.fake.releaseReqs), n.scheduled)
	}
	for _, args := range probe.afterDone {
		for _, key := range []string{constants.GetAccountDataKey(hzAccount), constants.PlayerToAccountKey(hzPlayerID), seedKey} {
			if slices.Contains(args, key) {
				t.Fatalf("取消后仍触碰账号/反查/种子缓存: %q", args)
			}
		}
	}
	if n.mr.Exists(nameCreateKey) {
		t.Fatal("提交后请求取消仍未释放建角锁")
	}
}
