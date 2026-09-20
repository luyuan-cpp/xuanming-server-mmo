package logic

// friend_logic_test.go —— 逻辑层的行为回归(假 store + miniredis)。
//
// # 这一层该被测什么
//
// logic 只负责四件事,每一件都不需要真 MySQL:
//
//	① 取身份(D-9:"我"只从会话取)与校参;
//	② 把 data 层的哨兵错误翻成**正确的** tip 码;
//	③ 业务失败一律 in-band(`return resp, nil`),不许漏成 gRPC error(F2-1);
//	④ 频控、推送这些旁路的失败不得影响主结果。
//
// 所以存储用假 store 注入(AGENTS.md §11.4 面向接口测试),Redis 用 miniredis
// (两条 Lua 真实执行,频控不是被 mock 掉的)。
//
// # ② 为什么值得一整张表
//
// friend_repo.go 顶部那张映射表写明:同一个"谁的好友列表满了",在 AddFriend 与 AcceptFriend
// 上对应**相反**的 tip 码(AddFriend 的 me 是申请人,AcceptFriend 的 me 是接受者)。
// 照抄另一条路径的 switch 会让两个码正好对调 —— 编译器看不出来,玩家看到的是
// "对方的好友列表满了",而实际上满的是自己。下面 TestAddFriendMapsStoreErrors /
// TestAcceptFriendMapsStoreErrors 两张表就是为了钉住这一处。
//
// # ④ 推送与频控怎么观测
//
// pushFriendEvent 没有返回值(push.go 刻意如此,从签名上堵住"顺手 return err"),
// 所以唯一的观测面是 **Prometheus 指标**:friend_push_total{reason,outcome}。
// 指标是懒注册的(只在 metrics.Start 里),测试用 ensureMetricsRegistered 触发一次,
// 之后经 prometheus.DefaultGatherer 读计数差值 —— 这正是线上判断"推了还是没推"的同一个面,
// 拿它当断言面比断言内部调用更贴近契约。

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus"
	kafkago "github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeromicro/go-zero/core/stores/redis"
	"google.golang.org/protobuf/proto"

	"friend/internal/config"
	"friend/internal/constants"
	"friend/internal/data"
	friendkafka "friend/internal/kafka"
	"friend/internal/metrics"
	"friend/internal/session"
	"friend/internal/svc"

	base "proto/common/base"
	pb "proto/friend"
	plpb "proto/player_locator"
)

// ── 假 store ────────────────────────────────────────────────────

// fakeFriendStore 实现 FriendStore。每个方法可以被指定返回一个错误,
// 用来驱动"哨兵 → tip 码"的映射表;不指定时返回预置数据。
//
// 不加锁:本包的用例都是串行的(没有 t.Parallel,也没有并发调用 logic)。
// 并发语义属于 data 层,由 internal/data 的真 InnoDB 用例覆盖 —— 在假 store 上
// 模拟并发只能复述我们自己的假设,测不出锁的行为。
type fakeFriendStore struct {
	// errs 按方法名给定要返回的错误。用 map 而不是每个方法一个字段,
	// 是为了让"这次要让哪个方法失败"在用例里只占一行。
	errs map[string]error

	friends []data.FriendEntry
	pending []data.FriendRequestEntry
	blocks  []data.BlockEntry
	mutual  []data.RecommendCandidate
	random  []data.RecommendCandidate

	// 记录最后一次调用的入参,用来断言限幅、排除集这些"传进去的值"。
	lastAddLimits     data.AddFriendLimits
	lastMutualExclude []uint64
	lastMutualLimit   uint32
	lastRandomExclude []uint64
	lastRandomLimit   uint32
	calls             map[string]int
}

func newFakeStore() *fakeFriendStore {
	return &fakeFriendStore{errs: map[string]error{}, calls: map[string]int{}}
}

func (f *fakeFriendStore) fail(method string, err error) *fakeFriendStore {
	f.errs[method] = err
	return f
}

func (f *fakeFriendStore) take(method string) error {
	f.calls[method]++
	return f.errs[method]
}

func (f *fakeFriendStore) AddFriendRequest(_ context.Context, _, _ uint64, lim data.AddFriendLimits) error {
	f.lastAddLimits = lim
	return f.take("AddFriendRequest")
}

func (f *fakeFriendStore) AcceptFriend(_ context.Context, _, _ uint64, _ uint32) error {
	return f.take("AcceptFriend")
}

func (f *fakeFriendStore) RejectFriend(_ context.Context, _, _ uint64) error {
	return f.take("RejectFriend")
}

func (f *fakeFriendStore) RemoveFriend(_ context.Context, _, _ uint64) error {
	return f.take("RemoveFriend")
}

func (f *fakeFriendStore) GetFriendList(_ context.Context, _ uint64) ([]data.FriendEntry, error) {
	if err := f.take("GetFriendList"); err != nil {
		return nil, err
	}
	return f.friends, nil
}

func (f *fakeFriendStore) GetPendingRequests(_ context.Context, _ uint64) ([]data.FriendRequestEntry, error) {
	if err := f.take("GetPendingRequests"); err != nil {
		return nil, err
	}
	return f.pending, nil
}

func (f *fakeFriendStore) Block(_ context.Context, _, _ uint64, _ uint32) error {
	return f.take("Block")
}

func (f *fakeFriendStore) Unblock(_ context.Context, _, _ uint64) error {
	return f.take("Unblock")
}

func (f *fakeFriendStore) ListBlocks(_ context.Context, _ uint64) ([]data.BlockEntry, error) {
	if err := f.take("ListBlocks"); err != nil {
		return nil, err
	}
	return f.blocks, nil
}

func (f *fakeFriendStore) RecommendByMutual(_ context.Context, _ uint64, exclude []uint64, limit uint32) ([]data.RecommendCandidate, error) {
	// exclude 是调用方拥有的切片、且策略链会往里 append —— 必须拷一份存,
	// 否则断言看到的是第二级跑完之后的样子(切片共享底层数组)。
	f.lastMutualExclude = append([]uint64(nil), exclude...)
	f.lastMutualLimit = limit
	if err := f.take("RecommendByMutual"); err != nil {
		return nil, err
	}
	return f.mutual, nil
}

func (f *fakeFriendStore) RecommendRandom(_ context.Context, _ uint64, exclude []uint64, limit uint32) ([]data.RecommendCandidate, error) {
	f.lastRandomExclude = append([]uint64(nil), exclude...)
	f.lastRandomLimit = limit
	if err := f.take("RecommendRandom"); err != nil {
		return nil, err
	}
	return f.random, nil
}

// fakeSessionStore 实现 SessionStore。states 为 nil 就等于"全部离线"——
// 这正是真实现读失败时的降级形态(接口没有 error,降级在实现内部完成)。
type fakeSessionStore struct {
	states map[uint64]data.OnlineStatus
	calls  int
}

func (f *fakeSessionStore) FillOnlineStatus(_ context.Context, _ []uint64) map[uint64]data.OnlineStatus {
	f.calls++
	return f.states
}

// ── 夹具 ────────────────────────────────────────────────────────

// testFriendConf 与 etc/friend.yaml 的默认阈值一致。
// 要改阈值的用例用 newFixture 的 tweak 改**自己那一份**,不要改这里 ——
// 改这里会让别的用例跟着变,而失败信息完全指不到原因。
func testFriendConf() config.FriendConf {
	return config.FriendConf{
		MaxFriends:            200,
		MaxPendingRequests:    50,
		MaxIncomingRequests:   200,
		MaxBlocks:             200,
		RecommendDefaultLimit: 10,
		RecommendMaxLimit:     20,
		RecommendMaxExclude:   64,
		RequestQuotaPerMinute: 10,
		ListReadHardLimit:     1000,
		CacheTTL:              30 * time.Minute,
		Sweep: config.SweepConf{
			Mode:          config.SweepModeReportOnly,
			Interval:      5 * time.Minute,
			RetentionDays: 7,
			BatchLimit:    1000,
		},
	}
}

func testConfig() config.Config {
	c := config.Config{ZoneId: 1, LeaseTTL: 60, Friend: testFriendConf()}
	// Timeout 显式给:RequestBudget() = Timeout 毫秒 − InBandReplyReserve(500ms),
	// 这是配置的真实形状(config.Validate 拒收 Timeout < MinRpcTimeoutMs)。
	// 留 0 会让预算变成 −500ms;现在全部 10 个 handler 都走 withRequestBudget,
	// 非正预算在那里被识别出来("不套预算" + 一条错误日志),所以留 0 不再让用例炸,
	// 但那样测的就不是生产路径上的 ctx 形状了。
	c.Timeout = config.MaxRpcTimeoutMs
	return c
}

// testNowMs 是固定时钟。推送的 ts_ms 取它,所以断言里可以直接比常量,
// 不依赖真实墙钟(AGENTS.md §11.4)。
const testNowMs int64 = 1_780_000_000_000

type fixture struct {
	deps     *Deps
	svcCtx   *svc.ServiceContext
	store    *fakeFriendStore
	sessions *fakeSessionStore
	friendMr *miniredis.Miniredis // FriendRedis:friend 私有 key(频控计数器)
	sharedMr *miniredis.Miniredis // SharedRedis:契约 key player:session:{id}
}

// newFixture 直接构造 ServiceContext 与 Deps,不走 svc.NewServiceContext / NewDeps:
// 前者要 etcd 且任何依赖建不出来就 panic(启动致命,照 trade),后者会装配真 repo。
func newFixture(t *testing.T, tweak func(*config.Config)) *fixture {
	t.Helper()
	cfg := testConfig()
	if tweak != nil {
		tweak(&cfg)
	}
	friendMr := miniredis.RunT(t)
	sharedMr := miniredis.RunT(t)
	sc := &svc.ServiceContext{
		Config:      cfg,
		FriendRedis: redis.MustNewRedis(redis.RedisConf{Host: friendMr.Addr(), Type: "node"}),
		SharedRedis: redis.MustNewRedis(redis.RedisConf{Host: sharedMr.Addr(), Type: "node"}),
		// KafkaWriter 默认 nil = 本地没配 Brokers 的常态(svc 的注释写明了)。
		// 要验"推没推"的用例自己调 withKafka 换成非 nil。
		KafkaWriter:        nil,
		GateCommandBuilder: friendkafka.NewGateCommandBuilder(),
	}
	store := newFakeStore()
	sessions := &fakeSessionStore{}
	return &fixture{
		deps: &Deps{
			SvcCtx:   sc,
			Repo:     store,
			Sessions: sessions,
			Now:      func() time.Time { return time.UnixMilli(testNowMs) },
		},
		svcCtx: sc, store: store, sessions: sessions,
		friendMr: friendMr, sharedMr: sharedMr,
	}
}

// withKafka 装一个指向不可达端口的 Writer。
//
// 它永远写不成功,但这正是我们要的:推送路径能不能**走到** kafkautil 的收口,
// 在指标上表现为 error 而不是 offline —— 两者的区别就是"在线闸放没放行"。
// 不需要真 broker,也就不需要 docker。
func (f *fixture) withKafka() *fixture {
	f.svcCtx.KafkaWriter = &kafkago.Writer{Addr: kafkago.TCP("127.0.0.1:1")}
	return f
}

func (f *fixture) logic() *FriendLogic { return NewFriendLogic(f.deps) }

// playerCtx 造一个"经 gate → 路由服转发"的客户端 ctx(D-9:身份只从会话取)。
func playerCtx(playerID uint64) context.Context {
	return session.WithDetails(context.Background(), &base.SessionDetails{PlayerId: playerID})
}

// testPlayerSessionKey —— 跨运行时契约 key(契约 §4),写者是 login / player_locator。
//
// 测试自己拼一遍、而不是复用 push.go 的 playerSessionKey:这条键名是 friend / login /
// player_locator / gate 几方的共同约定,改了就是契约破坏,而且全程零报错
// (GET 未命中在业务上长得像"玩家离线")。复用生产 helper 的话,生产端改键名、
// 测试跟着一起改,谁也发现不了。
func testPlayerSessionKey(playerID uint64) string {
	return "player:session:" + strconv.FormatUint(playerID, 10)
}

func (f *fixture) writeSession(t *testing.T, s *plpb.PlayerSession) {
	t.Helper()
	raw, err := proto.Marshal(s)
	require.NoError(t, err)
	require.NoError(t, f.sharedMr.Set(testPlayerSessionKey(s.GetPlayerId()), string(raw)))
}

// ── 指标观测面 ──────────────────────────────────────────────────

var metricsOnce sync.Once

// ensureMetricsRegistered 触发 metrics 包的懒注册(它只发生在 Start 里)。
//
// 刻意传一个**绑不上**的地址:端口 99999 超出范围,net.Listen 立刻返回错误、
// 不做 DNS、也不会留下监听套接字;而 register() 在起 goroutine **之前**已经执行。
// 我们要的只有注册,不要一个在测试进程里活到最后的 HTTP 端点。
func ensureMetricsRegistered(t *testing.T) {
	t.Helper()
	metricsOnce.Do(func() { metrics.Start("127.0.0.1:99999") })
}

// metricValue 读一条 Prometheus 序列的当前值;found=false 表示序列不存在。
//
// 序列"不存在"与"值为 0"必须分开:register() 会预建全部 label 组合的 0 值序列,
// 正是为了让告警规则不把"从未发生"和"指标缺失"看成一回事。
func metricValue(t *testing.T, name string, labels map[string]string) (float64, bool) {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			matched := true
			for _, lp := range m.GetLabel() {
				if want, ok := labels[lp.GetName()]; ok && want != lp.GetValue() {
					matched = false
					break
				}
			}
			if !matched || len(m.GetLabel()) != len(labels) {
				continue
			}
			if c := m.GetCounter(); c != nil {
				return c.GetValue(), true
			}
			if g := m.GetGauge(); g != nil {
				return g.GetValue(), true
			}
		}
	}
	return 0, false
}

func pushCount(t *testing.T, reason, outcome string) float64 {
	t.Helper()
	v, ok := metricValue(t, "friend_push_total", map[string]string{"reason": reason, "outcome": outcome})
	require.True(t, ok, "序列 friend_push_total{reason=%q,outcome=%q} 不存在:register() 应当预建全部组合", reason, outcome)
	return v
}

// ── ① 身份与校参 ────────────────────────────────────────────────

// rpcCall 把每个 RPC 收成同一个形状:(tip 码, gRPC error)。
// 这样"无会话""存储故障"这两条**跨全部方法**的规则可以用一张表覆盖;
// 逐个手写的话,新增 RPC 时一定会有人忘记补,而漏掉不会让任何用例变红。
type rpcCall struct {
	name string
	call func(l *FriendLogic, ctx context.Context) (uint32, error)
}

const peerID uint64 = 4242

func allClientRPCs() []rpcCall {
	return []rpcCall{
		{"AddFriend", func(l *FriendLogic, ctx context.Context) (uint32, error) {
			r, err := l.AddFriend(ctx, &pb.AddFriendRequest{TargetPlayerId: peerID})
			return r.GetErrorMessage().GetId(), err
		}},
		{"AcceptFriend", func(l *FriendLogic, ctx context.Context) (uint32, error) {
			r, err := l.AcceptFriend(ctx, &pb.AcceptFriendRequest{FromPlayerId: peerID})
			return r.GetErrorMessage().GetId(), err
		}},
		{"RejectFriend", func(l *FriendLogic, ctx context.Context) (uint32, error) {
			r, err := l.RejectFriend(ctx, &pb.RejectFriendRequest{FromPlayerId: peerID})
			return r.GetErrorMessage().GetId(), err
		}},
		{"RemoveFriend", func(l *FriendLogic, ctx context.Context) (uint32, error) {
			r, err := l.RemoveFriend(ctx, &pb.RemoveFriendRequest{TargetPlayerId: peerID})
			return r.GetErrorMessage().GetId(), err
		}},
		{"GetFriendList", func(l *FriendLogic, ctx context.Context) (uint32, error) {
			r, err := l.GetFriendList(ctx, &pb.GetFriendListRequest{})
			return r.GetErrorMessage().GetId(), err
		}},
		{"GetPendingRequests", func(l *FriendLogic, ctx context.Context) (uint32, error) {
			r, err := l.GetPendingRequests(ctx, &pb.GetPendingRequestsRequest{})
			return r.GetErrorMessage().GetId(), err
		}},
		{"Block", func(l *FriendLogic, ctx context.Context) (uint32, error) {
			r, err := l.Block(ctx, &pb.BlockRequest{TargetPlayerId: peerID})
			return r.GetErrorMessage().GetId(), err
		}},
		{"Unblock", func(l *FriendLogic, ctx context.Context) (uint32, error) {
			r, err := l.Unblock(ctx, &pb.UnblockRequest{TargetPlayerId: peerID})
			return r.GetErrorMessage().GetId(), err
		}},
		{"ListBlocks", func(l *FriendLogic, ctx context.Context) (uint32, error) {
			r, err := l.ListBlocks(ctx, &pb.ListBlocksRequest{})
			return r.GetErrorMessage().GetId(), err
		}},
		{"RecommendFriends", func(l *FriendLogic, ctx context.Context) (uint32, error) {
			r, err := l.RecommendFriends(ctx, &pb.RecommendFriendsRequest{})
			return r.GetErrorMessage().GetId(), err
		}},
	}
}

// TestNoSessionRejectedInBand:无会话(或会话 player_id=0)一律 in-band ErrInvalidParameter,
// 且**一次存储都不碰**。
//
// 为什么不是故障码:请求没带会话元数据属于客户端 / 链路问题,而 kPlayerNotFoundInSession
// 的 fault 列是 1,会把扫描器和旧客户端重放刷成服务端故障告警。
// 为什么必须 err==nil:回 gRPC error 会被路由服翻成信封级错误,客户端拿不到 TipInfoMessage。
func TestNoSessionRejectedInBand(t *testing.T) {
	cases := allClientRPCs()
	require.Len(t, cases, 10,
		"ClientPlayerFriend 有 10 个 C2S 方法;表里少一行 = 那个方法没被'无会话必须拒'与'存储故障必须 in-band'这两条全局规则覆盖")

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, nil)

			tip, err := c.call(f.logic(), context.Background()) // 完全没有会话
			require.NoError(t, err, "业务拒绝必须 in-band,不许走 gRPC error")
			assert.Equal(t, constants.ErrInvalidParameter, tip)

			// 有会话但 player_id=0:同样不许放行(0 号玩家不存在,放过去会给 0 建关系)。
			tip, err = c.call(f.logic(), playerCtx(0))
			require.NoError(t, err)
			assert.Equal(t, constants.ErrInvalidParameter, tip, "会话里 player_id=0 等于没有身份")

			assert.Empty(t, f.store.calls, "没有身份就不该碰存储:校验必须在任何 I/O 之前")
		})
	}
}

// TestAddFriendRejectsSelfAndZeroTarget。
//
// 两条分开验是有原因的:身份改从会话取之后 me 永远非 0,原来那条
// `req.PlayerId == req.TargetPlayerId` 不再顺带挡住"目标为 0"(以前 0==0 会命中自加好友分支)。
// 放过去就会给 player_id=0 建好友边,而且用的是"加自己"的码,排障时完全指错方向。
func TestAddFriendRejectsSelfAndZeroTarget(t *testing.T) {
	f := newFixture(t, nil)
	const me uint64 = 5001

	resp, err := f.logic().AddFriend(playerCtx(me), &pb.AddFriendRequest{TargetPlayerId: 0})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrInvalidParameter, resp.GetErrorMessage().GetId(),
		"目标为 0 是参数错,不是'加自己'")

	resp, err = f.logic().AddFriend(playerCtx(me), &pb.AddFriendRequest{TargetPlayerId: me})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrCannotAddSelf, resp.GetErrorMessage().GetId(),
		"加自己有专用码,别复用 ErrInvalidParameter(客户端要弹的文案不一样)")

	assert.Zero(t, f.store.calls["AddFriendRequest"], "两种都必须在开事务之前拒掉")
}

// TestZeroIdsRejected 是 F2-12 的回归。
//
// 这些方法以前没有零值校验:0 会被当成一个合法的对端 id 一路带到 SQL,
// 在库里留下以 0 为主键的行 —— 而 0 永远不会有对应的玩家,那些行谁也清不掉。
func TestZeroIdsRejected(t *testing.T) {
	const me uint64 = 5002
	cases := map[string]struct {
		store string
		call  func(l *FriendLogic, ctx context.Context) (uint32, error)
	}{
		"AcceptFriend": {"AcceptFriend", func(l *FriendLogic, ctx context.Context) (uint32, error) {
			r, err := l.AcceptFriend(ctx, &pb.AcceptFriendRequest{FromPlayerId: 0})
			return r.GetErrorMessage().GetId(), err
		}},
		"RejectFriend": {"RejectFriend", func(l *FriendLogic, ctx context.Context) (uint32, error) {
			r, err := l.RejectFriend(ctx, &pb.RejectFriendRequest{FromPlayerId: 0})
			return r.GetErrorMessage().GetId(), err
		}},
		"RemoveFriend": {"RemoveFriend", func(l *FriendLogic, ctx context.Context) (uint32, error) {
			r, err := l.RemoveFriend(ctx, &pb.RemoveFriendRequest{TargetPlayerId: 0})
			return r.GetErrorMessage().GetId(), err
		}},
		"Block": {"Block", func(l *FriendLogic, ctx context.Context) (uint32, error) {
			r, err := l.Block(ctx, &pb.BlockRequest{TargetPlayerId: 0})
			return r.GetErrorMessage().GetId(), err
		}},
		"Unblock": {"Unblock", func(l *FriendLogic, ctx context.Context) (uint32, error) {
			r, err := l.Unblock(ctx, &pb.UnblockRequest{TargetPlayerId: 0})
			return r.GetErrorMessage().GetId(), err
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, nil)
			tip, err := c.call(f.logic(), playerCtx(me))
			require.NoError(t, err)
			assert.Equal(t, constants.ErrInvalidParameter, tip, "请求体 id 为 0 必须在触库之前拒掉")
			assert.Zero(t, f.store.calls[c.store], "被拒的请求不得进存储")
		})
	}
}

// TestBlockRejectsSelf:拉黑自己没有语义,而且一旦落库会同时踩 data 层的两条不变量
// (自反的"既是好友又拉黑"、以及 §3.4 ⑤ 的自删边)。
func TestBlockRejectsSelf(t *testing.T) {
	f := newFixture(t, nil)
	const me uint64 = 5003
	resp, err := f.logic().Block(playerCtx(me), &pb.BlockRequest{TargetPlayerId: me})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrInvalidParameter, resp.GetErrorMessage().GetId())
	assert.Zero(t, f.store.calls["Block"])
}

// TestSelfTargetRejectedBeforeStorage:凡是"目标 = 自己"的请求都必须在**进 data 之前**被拒。
//
// 这不是洁癖:data 层对 from==to 一律回 errInvalidPlayerPair,而那是个**刻意的非哨兵**
// (data/friend_repo.go:66-72)—— 它会落到本层 switch 的 default,被定性成本域唯一的
// fault 码 ErrStorage,于是 serverbase 记一条 rpc_inband_fault。
// 也就是说:少挡一个入口,客户端就能用一个伪造的包按秒刷出"存储故障"告警,
// 把真故障淹在噪音里。AddFriend / AcceptFriend / Block 早就挡了,
// RejectFriend 与 Unblock 是后补的两个漏网入口。
func TestSelfTargetRejectedBeforeStorage(t *testing.T) {
	const me uint64 = 5004
	cases := map[string]struct {
		store string
		call  func(l *FriendLogic, ctx context.Context) (uint32, error)
	}{
		"RejectFriend": {"RejectFriend", func(l *FriendLogic, ctx context.Context) (uint32, error) {
			r, err := l.RejectFriend(ctx, &pb.RejectFriendRequest{FromPlayerId: me})
			return r.GetErrorMessage().GetId(), err
		}},
		"Unblock": {"Unblock", func(l *FriendLogic, ctx context.Context) (uint32, error) {
			r, err := l.Unblock(ctx, &pb.UnblockRequest{TargetPlayerId: me})
			return r.GetErrorMessage().GetId(), err
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, nil)
			tip, err := c.call(f.logic(), playerCtx(me))
			require.NoError(t, err)
			assert.Equal(t, constants.ErrInvalidParameter, tip,
				"目标是自己必须回参数错,不能变成 fault 码 ErrStorage")
			assert.Zero(t, f.store.calls[c.store],
				"必须在本层就拒掉,一次都不许进 data(进去就会刷出一条 rpc_inband_fault 假告警)")
		})
	}
}

// ── ② 哨兵 → tip 码的映射 ──────────────────────────────────────

// TestAddFriendMapsStoreErrors —— AddFriend 的全部哨兵映射。
//
// 其中 ErrBlocked 就是规格要求覆盖的"拉黑拒绝":客户端必须拿到一个**不区分方向**的码,
// 区分方向等于把对方的黑名单设置泄露给申请人。
func TestAddFriendMapsStoreErrors(t *testing.T) {
	cases := []struct {
		name     string
		storeErr error
		wantTip  uint32
	}{
		{"成功", nil, 0},
		{"任一方向拉黑", data.ErrBlocked, constants.ErrBlocked},
		{"已是好友", data.ErrAlreadyFriends, constants.ErrAlreadyFriends},
		{"重复申请", data.ErrRequestAlreadySent, constants.ErrRequestAlreadySent},
		{"我的出站满", data.ErrTooManyPending, constants.ErrTooManyPending},
		{"对方收件箱满", data.ErrTargetInboxFull, constants.ErrTargetInboxFull},
		// ⚠ 下面两条是最容易写反的:AddFriend 的操作者是**申请人**(Sender),
		// 所以 Sender = 我的列表满 → ErrFriendListFull;Acceptor = 对方满 → ErrTargetFriendListFull。
		// 照抄 AcceptFriend 的 switch 会让这两个码正好对调(那边 me 是 Acceptor,映射相反),
		// 而编译器和别的用例都看不出来。
		{"我的好友数满", data.ErrSenderFriendsFull, constants.ErrFriendListFull},
		{"对方好友数满", data.ErrAcceptorFriendsFull, constants.ErrTargetFriendListFull},
		{"未知错误 = 存储故障", errors.New("dial tcp: connection refused"), constants.ErrStorage},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, nil)
			f.store.fail("AddFriendRequest", c.storeErr)
			resp, err := f.logic().AddFriend(playerCtx(5010), &pb.AddFriendRequest{TargetPlayerId: peerID})
			require.NoError(t, err, "业务拒绝与存储故障都必须 in-band(F2-1)")
			assert.Equal(t, c.wantTip, resp.GetErrorMessage().GetId())
		})
	}
}

// TestAddFriendPassesConfiguredLimits:三个上限必须**原样**来自 config,且不许传反。
//
// 出站与入站同类型、方向相反,位置传反不会报错,只会让两条上限张冠李戴 ——
// 表现是"我明明没发几条却说我发太多了"。所以三个值刻意设成互不相同。
func TestAddFriendPassesConfiguredLimits(t *testing.T) {
	f := newFixture(t, func(c *config.Config) {
		c.Friend.MaxFriends = 111
		c.Friend.MaxPendingRequests = 222
		c.Friend.MaxIncomingRequests = 333
	})
	_, err := f.logic().AddFriend(playerCtx(5011), &pb.AddFriendRequest{TargetPlayerId: peerID})
	require.NoError(t, err)
	assert.Equal(t, uint32(111), f.store.lastAddLimits.MaxFriends)
	assert.Equal(t, uint32(222), f.store.lastAddLimits.MaxPendingRequests, "出站上限")
	assert.Equal(t, uint32(333), f.store.lastAddLimits.MaxIncomingRequests, "入站上限")
}

// TestAcceptFriendMapsStoreErrors —— 与上一张表配对看。
//
// AcceptFriend 的操作者是**接受者**,所以 Sender(申请人)满 → 对方的列表满,
// Acceptor(接受者)满 → 我的列表满 —— 与 AddFriend 那一对正好相反。
// 两张表放在一起,谁把 switch 照抄过去都会立刻红。
func TestAcceptFriendMapsStoreErrors(t *testing.T) {
	cases := []struct {
		name     string
		storeErr error
		wantTip  uint32
	}{
		{"成功", nil, 0},
		{"任一方向拉黑", data.ErrBlocked, constants.ErrBlocked},
		{"没有待处理申请", data.ErrRequestNotFound, constants.ErrNoPendingRequest},
		{"申请人的列表满(= 对方满)", data.ErrSenderFriendsFull, constants.ErrTargetFriendListFull},
		{"接受者的列表满(= 我满)", data.ErrAcceptorFriendsFull, constants.ErrFriendListFull},
		{"未知错误 = 存储故障", errors.New("i/o timeout"), constants.ErrStorage},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, nil)
			f.store.fail("AcceptFriend", c.storeErr)
			resp, err := f.logic().AcceptFriend(playerCtx(5020), &pb.AcceptFriendRequest{FromPlayerId: peerID})
			require.NoError(t, err)
			assert.Equal(t, c.wantTip, resp.GetErrorMessage().GetId())
		})
	}
}

// TestFriendsFullTipsAreNotInterchangeable 把上面两张表里最危险的一组单独钉一遍:
// **同一个哨兵**(ErrSenderFriendsFull)在两条路径上必须映射成相反的码 ——
// AddFriend 的 me 是 Sender(满的是我),AcceptFriend 的 me 是 Acceptor(满的是对方)。
//
// 单独写出来是因为它是"两处都对了才算对"的性质 —— 只看其中一张表时,把两个码都写成
// ErrFriendListFull 也能过。
func TestFriendsFullTipsAreNotInterchangeable(t *testing.T) {
	add := newFixture(t, nil)
	add.store.fail("AddFriendRequest", data.ErrSenderFriendsFull)
	addResp, err := add.logic().AddFriend(playerCtx(5030), &pb.AddFriendRequest{TargetPlayerId: peerID})
	require.NoError(t, err)

	acc := newFixture(t, nil)
	acc.store.fail("AcceptFriend", data.ErrSenderFriendsFull)
	accResp, err := acc.logic().AcceptFriend(playerCtx(5030), &pb.AcceptFriendRequest{FromPlayerId: peerID})
	require.NoError(t, err)

	assert.Equal(t, constants.ErrFriendListFull, addResp.GetErrorMessage().GetId(),
		"AddFriend 里 Sender = 操作者自己")
	assert.Equal(t, constants.ErrTargetFriendListFull, accResp.GetErrorMessage().GetId(),
		"AcceptFriend 里 Sender = 对方(申请人)")
	assert.NotEqual(t, addResp.GetErrorMessage().GetId(), accResp.GetErrorMessage().GetId(),
		"两条路径的'好友数满'必须分别映射;写成同一个码时,玩家会被告知满的是另一个人的列表")
}

// ── ③ 存储故障 in-band 化(F2-1) ──────────────────────────────

// TestStorageFaultIsInBand:任何方法的依赖故障都必须 `return resp, nil` + ErrStorage。
//
// 为什么要覆盖**全部**方法:`return nil, err` 与 `return resp, nil` 在代码里长得很像,
// 漏改一个方法没有任何静态信号。而漏改的后果是客户端在那一个方法上收到信封级错误、
// 拿不到 TipInfoMessage,界面上表现为"点了没反应"。
func TestStorageFaultIsInBand(t *testing.T) {
	// 方法名 → 假 store 里对应的 store 方法名(RecommendFriends 先打 mutual)。
	storeMethod := map[string]string{
		"AddFriend":          "AddFriendRequest",
		"AcceptFriend":       "AcceptFriend",
		"RejectFriend":       "RejectFriend",
		"RemoveFriend":       "RemoveFriend",
		"GetFriendList":      "GetFriendList",
		"GetPendingRequests": "GetPendingRequests",
		"Block":              "Block",
		"Unblock":            "Unblock",
		"ListBlocks":         "ListBlocks",
		"RecommendFriends":   "RecommendByMutual",
	}
	for _, c := range allClientRPCs() {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, nil)
			f.store.fail(storeMethod[c.name], errors.New("dial tcp 127.0.0.1:3306: connect: connection refused"))
			tip, err := c.call(f.logic(), playerCtx(5040))
			require.NoError(t, err,
				"存储故障也必须 in-band:return nil, err 会被路由服翻成信封错误,in-band 结果被丢掉")
			assert.Equal(t, constants.ErrStorage, tip,
				"MySQL 故障必须回本域唯一的 fault 码 ErrStorage(kServiceUnavailable)")
		})
	}
}

// ── 推荐的限幅(规格 §3.7) ────────────────────────────────────

// TestRecommendRejectsOversizedExclude:exclude 必须有上限。
//
// gate 的单包在 1KB 量级,而 exclude 是 repeated uint64 —— 客户端塞一万个 id 就能让服务端
// 拼出一条上万个占位符的 SQL,撞上 max_allowed_packet / 预处理参数上限时表现为
// "这个玩家每次点推荐都失败",而不是一条清晰的拒绝。
func TestRecommendRejectsOversizedExclude(t *testing.T) {
	f := newFixture(t, nil)
	ctx := playerCtx(5050)
	maxExclude := int(f.svcCtx.Config.Friend.RecommendMaxExclude)

	atLimit := make([]uint64, maxExclude)
	for i := range atLimit {
		atLimit[i] = uint64(9000 + i)
	}
	// 恰好等于上限必须放行 —— 否则证明拒的是"有 exclude",不是"超过上限"。
	resp, err := f.logic().RecommendFriends(ctx, &pb.RecommendFriendsRequest{ExcludePlayerIds: atLimit})
	require.NoError(t, err)
	require.Zero(t, resp.GetErrorMessage().GetId(), "恰好等于上限必须放行")
	require.Equal(t, 1, f.store.calls["RecommendByMutual"])

	resp, err = f.logic().RecommendFriends(ctx, &pb.RecommendFriendsRequest{
		ExcludePlayerIds: append(append([]uint64(nil), atLimit...), 9999),
	})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrInvalidParameter, resp.GetErrorMessage().GetId(),
		"exclude 超过 RecommendMaxExclude 必须回参数错")
	assert.Equal(t, 1, f.store.calls["RecommendByMutual"], "被拒的请求不得再打一次存储")
}

// TestRecommendClampsLimitInsteadOfRejecting:limit 越界是**限幅**,不是拒绝。
//
// 这两者容易写反。客户端传大 limit 通常只是"尽量多给几个",拒掉它会把一个能用的功能
// 变成一个报错;而不限幅则让一次请求拖回整表。所以:0 取默认、超上限取上限、都不报错。
func TestRecommendClampsLimitInsteadOfRejecting(t *testing.T) {
	cfg := testFriendConf()
	for _, tc := range []struct {
		requested uint32
		want      uint32
	}{
		{0, cfg.RecommendDefaultLimit},
		{3, 3},
		{cfg.RecommendMaxLimit, cfg.RecommendMaxLimit},
		{1 << 20, cfg.RecommendMaxLimit},
	} {
		f := newFixture(t, nil)
		resp, err := f.logic().RecommendFriends(playerCtx(5051), &pb.RecommendFriendsRequest{Limit: tc.requested})
		require.NoError(t, err)
		require.Zero(t, resp.GetErrorMessage().GetId(), "limit=%d 不该被判成参数错", tc.requested)
		assert.Equal(t, tc.want, f.store.lastMutualLimit,
			"limit=%d 应被限幅成 %d 再交给存储", tc.requested, tc.want)
	}
}

// TestRecommendFallsBackToRandomAndAccumulatesExclude 钉 §3.7 的策略链。
//
// 两件事:① mutual 不够时才补 random,且只补**缺口**(多查出来的行会被丢掉却照样花开销);
// ② 已选中的候选要进 exclude,否则同一个人会在一次响应里出现两遍。
func TestRecommendFallsBackToRandomAndAccumulatesExclude(t *testing.T) {
	f := newFixture(t, nil)
	const me uint64 = 5052
	f.store.mutual = []data.RecommendCandidate{{CandidatePlayerID: 7001, MutualFriends: 3}}
	f.store.random = []data.RecommendCandidate{{CandidatePlayerID: 7002}, {CandidatePlayerID: 7003}}

	resp, err := f.logic().RecommendFriends(playerCtx(me), &pb.RecommendFriendsRequest{Limit: 3})
	require.NoError(t, err)
	require.Zero(t, resp.GetErrorMessage().GetId())
	require.Len(t, resp.GetCandidates(), 3)

	assert.Equal(t, uint32(3), f.store.lastMutualLimit)
	assert.Equal(t, uint32(2), f.store.lastRandomLimit, "第二级只补缺口,不重新要一整个 limit")
	assert.Contains(t, f.store.lastMutualExclude, me, "自己必须进排除集,不能把玩家推荐给自己")
	assert.Contains(t, f.store.lastRandomExclude, uint64(7001),
		"第一级已选中的候选必须进排除集,否则同一个人会在一次响应里出现两遍")

	assert.Equal(t, uint64(7001), resp.GetCandidates()[0].GetCandidatePlayerId())
	assert.Equal(t, uint32(3), resp.GetCandidates()[0].GetMutualFriends())
}

// TestRecommendFillsOnlineFromSessions:推荐候选的 is_online / last_active_ms 必须来自
// SessionStore。这是 recommend.go 与 data/session_reader.go 的唯一接触点 ——
// F2 的两条 blocker(OnlineStatus 类型名、FillOnlineStatus 方法名)就发在这个接缝上,
// 没有断言时假 store 全绿而生产路径根本装配不起来。
func TestRecommendFillsOnlineFromSessions(t *testing.T) {
	f := newFixture(t, nil)
	const me uint64 = 5053
	f.store.mutual = []data.RecommendCandidate{{CandidatePlayerID: 7001, MutualFriends: 3}}
	f.store.random = []data.RecommendCandidate{{CandidatePlayerID: 7002}}
	// 只给第一个候选一条会话:第二个候选不在 map 里,验的是"缺席 = 离线(零值)"。
	f.sessions.states = map[uint64]data.OnlineStatus{7001: {Online: true, LastActiveMs: 42}}

	resp, err := f.logic().RecommendFriends(playerCtx(me), &pb.RecommendFriendsRequest{Limit: 2})
	require.NoError(t, err)
	require.Zero(t, resp.GetErrorMessage().GetId())
	require.Len(t, resp.GetCandidates(), 2)

	assert.True(t, resp.GetCandidates()[0].GetIsOnline())
	assert.EqualValues(t, 42, resp.GetCandidates()[0].GetLastActiveMs())
	assert.False(t, resp.GetCandidates()[1].GetIsOnline(), "不在 map 里 = 离线(零值)")
	assert.Zero(t, resp.GetCandidates()[1].GetLastActiveMs())
	assert.Equal(t, 1, f.sessions.calls, "一次请求只批量查一次,不许按候选逐个查")
}

// ── ④ 频控(规格 §3.6) ────────────────────────────────────────

// TestRequestQuotaRejectsAfterLimit。
//
// 配额挡的是"发一条、立刻撤一条"的高频刷:每一条都合法、任何时刻的挂起数都是 1,
// MaxPendingRequests 这类"同时挂多少"的上限一条都拦不住。
//
// 断言的语义:配额按**尝试**计数。第 limit 次恰好用完名额、仍然放行(脚本里是 `count > limit`),
// 第 limit+1 次才拒;而且拒发生在触存储之前。
func TestRequestQuotaRejectsAfterLimit(t *testing.T) {
	const quota = 3
	f := newFixture(t, func(c *config.Config) { c.Friend.RequestQuotaPerMinute = quota })
	ctx := playerCtx(5060)

	for i := 0; i < quota; i++ {
		resp, err := f.logic().AddFriend(ctx, &pb.AddFriendRequest{TargetPlayerId: uint64(6000 + i)})
		require.NoError(t, err)
		require.Zero(t, resp.GetErrorMessage().GetId(), "第 %d 次应当放行(count > limit 才拒)", i+1)
	}
	resp, err := f.logic().AddFriend(ctx, &pb.AddFriendRequest{TargetPlayerId: 6999})
	require.NoError(t, err)
	assert.Equal(t, constants.ErrRateLimited, resp.GetErrorMessage().GetId(),
		"超过 RequestQuotaPerMinute 必须回限流码")
	assert.Equal(t, quota, f.store.calls["AddFriendRequest"],
		"被配额拒掉的那次必须在**任何副作用之前**返回:放在事务后面等于'先写库再限流',写放大照旧")

	// 配额是**按玩家**的:一个刷子不能把别人的名额一起吃掉。
	resp, err = f.logic().AddFriend(playerCtx(5061), &pb.AddFriendRequest{TargetPlayerId: 6999})
	require.NoError(t, err)
	assert.Zero(t, resp.GetErrorMessage().GetId(), "配额 key 必须带 player_id,否则全服共用一个计数器")
}

// TestRequestQuotaFailsOpenWhenRedisDown 验规格 §3.6 明写的 fail-open。
//
// 这是一次**有意识的**取舍,按 AGENTS.md §11.3 记在案:配额是防刷不是防作弊,
// Redis 抖一下就让全服发不出好友申请,代价远大于那一小段时间内被刷。
// 代价同样写明:Redis 故障期间配额形同不存在,所以它不能当安全边界用 ——
// 真正的硬上限(好友数 / 收件箱 / 重复申请)在 MySQL 的权威事务里 fail-closed。
//
// 两段式才有鉴别力:先证明"Redis 在的时候配额确实会拒",再证明"Redis 断了就不拒"。
// 只写后半段的话,一个从来没接入过配额的实现也会通过。
func TestRequestQuotaFailsOpenWhenRedisDown(t *testing.T) {
	ensureMetricsRegistered(t)
	f := newFixture(t, func(c *config.Config) { c.Friend.RequestQuotaPerMinute = 1 })
	ctx := playerCtx(5070)

	resp, err := f.logic().AddFriend(ctx, &pb.AddFriendRequest{TargetPlayerId: 6001})
	require.NoError(t, err)
	require.Zero(t, resp.GetErrorMessage().GetId(), "第 1 次用掉唯一的名额")
	resp, err = f.logic().AddFriend(ctx, &pb.AddFriendRequest{TargetPlayerId: 6002})
	require.NoError(t, err)
	require.Equal(t, constants.ErrRateLimited, resp.GetErrorMessage().GetId(),
		"前置条件:Redis 正常时配额必须真的会拒(否则下面那半段没有意义)")

	before, _ := metricValue(t, "friend_rate_quota_total", map[string]string{"outcome": metrics.OutcomeError})
	f.friendMr.Close() // 打断 FriendRedis(频控计数器在它上面)

	resp, err = f.logic().AddFriend(ctx, &pb.AddFriendRequest{TargetPlayerId: 6003})
	require.NoError(t, err)
	assert.Zero(t, resp.GetErrorMessage().GetId(),
		"Redis 故障时配额必须 fail-open,不能把整个好友申请功能一起关掉")

	after, ok := metricValue(t, "friend_rate_quota_total", map[string]string{"outcome": metrics.OutcomeError})
	require.True(t, ok, "fail-open 之后 friend_rate_quota_total{outcome=\"error\"} 必须有值")
	assert.Equal(t, before+1, after, "fail-open 每放行一次都要计一次 error")

	// ⚠ 这条断言只能证明"出错时会 +1",**证明不了序列是预建的**:它在第一次 error 发生时
	// 才被创建。而 metrics.register() 的预建列表里 rateQuotaTotal 只有 allowed / rejected
	// 两个 outcome,没有 error —— 于是在真正出错之前,friend_rate_quota_total{outcome="error"}
	// 这条序列**根本不存在**,`rate(...{outcome="error"}[5m]) > 0` 既不报警也不报错,
	// 与"一切正常"长得一样。这正是 register() 自己的注释要防的那件事,也正是 rate_quota.go
	// 说"它必须配告警"的那条信号。
	// metrics.go 不在本批(B5)的文件清单里,所以这里只登记、不改;已在交付说明里报告。
}

// ── ⑤ S2C 推送(规格 §3.5) ───────────────────────────────────

// TestPushOnlyTargetsOnlineSessions 验"只推 ONLINE"。
//
// 观测面是 friend_push_total{reason,outcome} 的计数差值:
//   - offline:在线闸挡住了(键不存在 / DISCONNECTING);
//   - error:走到了推送收口才失败(这里是 kafkautil 对空 gate_instance_id 的防僵尸
//     fail-closed,以及会话解码失败)。
//
// 两个 outcome 分得开,就证明在线闸真的存在。DISCONNECTING 必须落在 offline ——
// 断线重连窗口内 gate 上已经没有可写的 TCP 连接,推过去只是白占一次 Kafka 写。
func TestPushOnlyTargetsOnlineSessions(t *testing.T) {
	ensureMetricsRegistered(t)
	const reason = metrics.ReasonRequestReceived

	newCase := func(t *testing.T) *fixture { return newFixture(t, nil).withKafka() }

	t.Run("键不存在 = 离线", func(t *testing.T) {
		f := newCase(t)
		before := pushCount(t, reason, metrics.OutcomeOffline)
		f.logic().pushFriendEvent(context.Background(), 7001, pb.FriendEventReason_FRIEND_EVENT_REASON_REQUEST_RECEIVED, 1)
		assert.Equal(t, before+1, pushCount(t, reason, metrics.OutcomeOffline),
			"没有会话的玩家不推,并且记成 offline(不是 error:没登录是最常见的正常分支)")
	})

	t.Run("DISCONNECTING 不是在线", func(t *testing.T) {
		f := newCase(t)
		f.writeSession(t, &plpb.PlayerSession{
			PlayerId: 7002, SessionId: 11, GateId: "1", GateInstanceId: "uuid-a",
			State: plpb.PlayerSessionState_SESSION_STATE_DISCONNECTING,
		})
		beforeOffline := pushCount(t, reason, metrics.OutcomeOffline)
		beforeError := pushCount(t, reason, metrics.OutcomeError)
		f.logic().pushFriendEvent(context.Background(), 7002, pb.FriendEventReason_FRIEND_EVENT_REASON_REQUEST_RECEIVED, 1)
		assert.Equal(t, beforeOffline+1, pushCount(t, reason, metrics.OutcomeOffline))
		assert.Equal(t, beforeError, pushCount(t, reason, metrics.OutcomeError),
			"DISCONNECTING 必须被在线闸挡住,而不是推出去再失败")
	})

	t.Run("ONLINE 会走到推送收口", func(t *testing.T) {
		f := newCase(t)
		// gate_instance_id 留空:kafkautil.PushToPlayer 的防僵尸 fail-closed 会在真正写 Kafka
		// **之前**拒绝。于是不需要 broker,也能证明"确实走过了在线闸"。
		f.writeSession(t, &plpb.PlayerSession{
			PlayerId: 7003, SessionId: 12, GateId: "1", GateInstanceId: "",
			State: plpb.PlayerSessionState_SESSION_STATE_ONLINE,
		})
		beforeOffline := pushCount(t, reason, metrics.OutcomeOffline)
		beforeError := pushCount(t, reason, metrics.OutcomeError)
		f.logic().pushFriendEvent(context.Background(), 7003, pb.FriendEventReason_FRIEND_EVENT_REASON_REQUEST_RECEIVED, 1)
		assert.Equal(t, beforeOffline, pushCount(t, reason, metrics.OutcomeOffline),
			"ONLINE 不许被记成 offline —— 那会让'在线闸把所有人都判成离线'这种缺陷完全无声")
		assert.Equal(t, beforeError+1, pushCount(t, reason, metrics.OutcomeError))
	})

	t.Run("会话解码失败记 error 而不是 offline", func(t *testing.T) {
		f := newCase(t)
		require.NoError(t, f.sharedMr.Set(testPlayerSessionKey(7004), "not-a-valid-proto\xff"))
		beforeOffline := pushCount(t, reason, metrics.OutcomeOffline)
		beforeError := pushCount(t, reason, metrics.OutcomeError)
		assert.NotPanics(t, func() {
			f.logic().pushFriendEvent(context.Background(), 7004, pb.FriendEventReason_FRIEND_EVENT_REASON_REQUEST_RECEIVED, 1)
		})
		assert.Equal(t, beforeOffline, pushCount(t, reason, metrics.OutcomeOffline),
			"解码失败记成 offline 会让一次协议事故表现为'全服都离线'且零异常指标")
		assert.Equal(t, beforeError+1, pushCount(t, reason, metrics.OutcomeError))
	})

	t.Run("共享库不可用记 error", func(t *testing.T) {
		f := newCase(t)
		f.sharedMr.Close()
		beforeError := pushCount(t, reason, metrics.OutcomeError)
		assert.NotPanics(t, func() {
			f.logic().pushFriendEvent(context.Background(), 7005, pb.FriendEventReason_FRIEND_EVENT_REASON_REQUEST_RECEIVED, 1)
		})
		assert.Equal(t, beforeError+1, pushCount(t, reason, metrics.OutcomeError))
	})
}

// TestPushWithoutKafkaWriterDoesNotPanic:没配 Brokers 时 KafkaWriter 是 nil
// (svc 的注释写明这是本地联调常态),而 nil Writer 上调 WriteMessages 会 panic,
// panic 会把整个 gRPC 进程带走。
//
// 同时钉住"不记 ok":推送整个关着必须在指标上看得见,否则线上漏配 Brokers 会表现为
// "功能正常但红点从不亮",没有任何告警。
func TestPushWithoutKafkaWriterDoesNotPanic(t *testing.T) {
	ensureMetricsRegistered(t)
	f := newFixture(t, nil) // KafkaWriter = nil
	const reason = metrics.ReasonRequestAccepted
	f.writeSession(t, &plpb.PlayerSession{
		PlayerId: 7010, SessionId: 13, GateId: "1", GateInstanceId: "uuid-b",
		State: plpb.PlayerSessionState_SESSION_STATE_ONLINE,
	})
	beforeOK := pushCount(t, reason, metrics.OutcomeOK)
	beforeError := pushCount(t, reason, metrics.OutcomeError)

	assert.NotPanics(t, func() {
		f.logic().pushFriendEvent(context.Background(), 7010,
			pb.FriendEventReason_FRIEND_EVENT_REASON_REQUEST_ACCEPTED, 1)
	})
	assert.Equal(t, beforeOK, pushCount(t, reason, metrics.OutcomeOK), "没发出去就不许记 ok")
	assert.Equal(t, beforeError+1, pushCount(t, reason, metrics.OutcomeError))
}

// TestPushFailureDoesNotAffectRpcResult:推送失败不得影响 RPC 结果(规格 §3.5 纪律 2)。
//
// 夹具里 KafkaWriter 是 nil,所以推送必然失败。若有人把推送的错误抬成 RPC 失败,
// 玩家的申请其实已经写进库了,客户端却收到失败 —— 他会再点一次,拿到"重复申请",
// 然后来报 bug。
func TestPushFailureDoesNotAffectRpcResult(t *testing.T) {
	ensureMetricsRegistered(t)
	f := newFixture(t, nil) // KafkaWriter = nil ⇒ 推送必然失败

	resp, err := f.logic().AddFriend(playerCtx(5080), &pb.AddFriendRequest{TargetPlayerId: peerID})
	require.NoError(t, err)
	assert.Zero(t, resp.GetErrorMessage().GetId(), "事务已提交,推送失败不许改写 RPC 结果")
	assert.Equal(t, 1, f.store.calls["AddFriendRequest"])

	acc := newFixture(t, nil)
	accResp, err := acc.logic().AcceptFriend(playerCtx(5081), &pb.AcceptFriendRequest{FromPlayerId: peerID})
	require.NoError(t, err)
	assert.Zero(t, accResp.GetErrorMessage().GetId())
}

// TestPushGoesToThePeerNotTheActor 钉规格 §3.5 纪律 3:不推给操作者本人。
//
// 观测方式:只给**操作者**写一条 ONLINE 会话,目标没有会话。
// 如果推送发错了人(推给了自己),就会走到推送收口并记 error;推对了人(目标没登录)
// 记的是 offline。两者分得开。
func TestPushGoesToThePeerNotTheActor(t *testing.T) {
	ensureMetricsRegistered(t)
	const me, target uint64 = 5090, 5091
	const reason = metrics.ReasonRequestReceived

	f := newFixture(t, nil).withKafka()
	f.writeSession(t, &plpb.PlayerSession{
		PlayerId: me, SessionId: 14, GateId: "1", GateInstanceId: "", // 走到收口就会失败 ⇒ error
		State: plpb.PlayerSessionState_SESSION_STATE_ONLINE,
	})
	beforeOffline := pushCount(t, reason, metrics.OutcomeOffline)
	beforeError := pushCount(t, reason, metrics.OutcomeError)

	_, err := f.logic().AddFriend(playerCtx(me), &pb.AddFriendRequest{TargetPlayerId: target})
	require.NoError(t, err)

	assert.Equal(t, beforeOffline+1, pushCount(t, reason, metrics.OutcomeOffline),
		"推送的收件人必须是 target(他没登录 ⇒ offline)")
	assert.Equal(t, beforeError, pushCount(t, reason, metrics.OutcomeError),
		"推给操作者自己会让客户端收到自己动作的回声,红点逻辑还要额外去重")
}

// ── 列表读与在线状态降级(规格 §3.9) ───────────────────────────

// TestGetFriendListFillsOnlineFromSessions:is_online / last_active_ms 的唯一事实源是
// 共享库的会话,不是 friend 表(friend 表里根本没有这两列,显示态不进持久化记录,
// AGENTS.md §11.6)。
func TestGetFriendListFillsOnlineFromSessions(t *testing.T) {
	f := newFixture(t, nil)
	f.store.friends = []data.FriendEntry{
		{FriendPlayerID: 8001, SinceMs: 111},
		{FriendPlayerID: 8002, SinceMs: 222},
	}
	f.sessions.states = map[uint64]data.OnlineStatus{
		8001: {Online: true, LastActiveMs: 999},
	}

	resp, err := f.logic().GetFriendList(playerCtx(5100), &pb.GetFriendListRequest{})
	require.NoError(t, err)
	require.Zero(t, resp.GetErrorMessage().GetId())
	require.Len(t, resp.GetFriends(), 2)

	assert.True(t, resp.GetFriends()[0].GetIsOnline())
	assert.EqualValues(t, 999, resp.GetFriends()[0].GetLastActiveMs())
	assert.EqualValues(t, 111, resp.GetFriends()[0].GetSinceMs(), "since_ms 来自 friend 表,不是会话")
	// map 里缺席 = 离线且无活跃记录(pb 零值恰好就是这个语义)。
	assert.False(t, resp.GetFriends()[1].GetIsOnline())
	assert.Zero(t, resp.GetFriends()[1].GetLastActiveMs())
}

// TestGetFriendListDegradesWhenSessionsUnavailable:共享 Redis 读不到时全部按离线返回,
// **不让好友列表整个失败**。抖一下就看不到好友,比看到一份"全部灰着"的列表更糟。
func TestGetFriendListDegradesWhenSessionsUnavailable(t *testing.T) {
	f := newFixture(t, nil)
	f.store.friends = []data.FriendEntry{{FriendPlayerID: 8003, SinceMs: 1}}
	f.sessions.states = nil // 真实现读失败时就是这个形态(接口没有 error)

	resp, err := f.logic().GetFriendList(playerCtx(5101), &pb.GetFriendListRequest{})
	require.NoError(t, err)
	assert.Zero(t, resp.GetErrorMessage().GetId(), "在线状态是展示字段,查不到不该让整个列表失败")
	require.Len(t, resp.GetFriends(), 1)
	assert.False(t, resp.GetFriends()[0].GetIsOnline())
}

// TestListBlocksCarriesNoOnlineState:黑名单刻意不带在线状态。
// 带上就成了"查某人是否在线"的旁路 —— 拉黑任意 id 即可探测,而拉黑不需要对方同意。
func TestListBlocksCarriesNoOnlineState(t *testing.T) {
	f := newFixture(t, nil)
	f.store.blocks = []data.BlockEntry{{BlockedPlayerID: 8004, SinceMs: 7}}
	f.sessions.states = map[uint64]data.OnlineStatus{8004: {Online: true, LastActiveMs: 5}}

	resp, err := f.logic().ListBlocks(playerCtx(5102), &pb.ListBlocksRequest{})
	require.NoError(t, err)
	require.Len(t, resp.GetBlocks(), 1)
	assert.EqualValues(t, 8004, resp.GetBlocks()[0].GetBlockedPlayerId())
	assert.EqualValues(t, 7, resp.GetBlocks()[0].GetSinceMs())
	assert.Zero(t, f.sessions.calls, "ListBlocks 不许查在线状态(会变成在线探测旁路)")
}

// TestGetPendingRequestsMapsStatus:status 是 int32 → proto enum 的显式转换,
// 转错了不会报错,只会让客户端把 accepted 显示成 pending。
func TestGetPendingRequestsMapsStatus(t *testing.T) {
	f := newFixture(t, nil)
	f.store.pending = []data.FriendRequestEntry{
		{FromPlayerID: 8005, ToPlayerID: 5103, RequestTimeMs: 42, Status: 1},
	}
	resp, err := f.logic().GetPendingRequests(playerCtx(5103), &pb.GetPendingRequestsRequest{})
	require.NoError(t, err)
	require.Len(t, resp.GetRequests(), 1)
	got := resp.GetRequests()[0]
	assert.EqualValues(t, 8005, got.GetFromPlayerId())
	assert.EqualValues(t, 42, got.GetRequestTimeMs())
	assert.EqualValues(t, 1, int32(got.GetStatus()), "status 的数值必须原样过渡(1=pending)")
}

// ── config 侧的字面量对齐 ──────────────────────────────────────

// TestSweepModeConstantsAreTheWireLiterals:sweep 的 mode 是字符串,拼错没有任何静态信号,
// 而后果是"delete 静默变成什么都不做"或反过来"report_only 真删"。
//
// internal/data 的测试里写的是字面量 "report_only" / "delete"(那边不能 import config ——
// config 要 import data 取 DatabaseName,包内测试反向 import 就是 import cycle),
// 所以两侧靠这一条对齐。yaml 的 options 标签也是这两个拼法。
//
// ⚠ 全仓其实有**三份**副本:config.SweepMode*、data.SweepMode*,以及
// internal/metrics/metrics.go:51-52 那对**不可导出**的 sweepModeReportOnly / sweepModeDelete
// (Gauge 预建用)。本断言够不到第三份 —— 它不可导出,而 metrics 刻意保持叶子包。
// 第三份漂移时的表现:预建的 0 值序列落在一个 label 上、SetSweepPendingRows 写的是另一个,
// "sweep 没在跑"这个唯一信号静默失效。已登记给 F3 处理。
func TestSweepModeConstantsAreTheWireLiterals(t *testing.T) {
	assert.Equal(t, "report_only", config.SweepModeReportOnly)
	assert.Equal(t, "delete", config.SweepModeDelete)
	// data 包不能 import config(config 为了 data.DatabaseName 已经反向 import 了 data),
	// 所以那两个常量只能在这里对齐 —— 这是全仓唯一能机械钉住它们的地方。
	// 拼错时的表现是 delete 模式静默退化成 report_only(data/sweep_repo.go 的判定点)。
	assert.Equal(t, config.SweepModeReportOnly, data.SweepModeReportOnly)
	assert.Equal(t, config.SweepModeDelete, data.SweepModeDelete)
	assert.Equal(t, config.SweepModeReportOnly, testFriendConf().Sweep.Mode,
		"默认模式必须是 report_only:updated_ms 的写入方是本批刚补的,delete 是有风险的选择")
}
