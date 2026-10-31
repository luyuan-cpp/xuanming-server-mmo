//go:build integration

package logic

// 帮会经济端到端流程:真 MySQL + miniredis + 假 scene(assetop.Applier)+ 真 data.GuildAssetStore
// + 真 assetop.Loop(05-economy.md §5.39 集成用例,按 90 清单与 D2 覆盖块订正)。
//
// 运行(Codex):`go test -tags integration ./internal/logic -run EconomyFlow -count=1 -v`。DSN 为空时整体 Skip。
//
// 库:GUILD_TEST_MYSQL_DSN **只借用它的账号**(需 CREATE / DROP DATABASE 权限),库名被忽略 —— 每个用例自建
// guild_it_<pid>_<n> 一次性库、结束 DROP(同 data/rank_zone_integration_test.go 的 newRepoOnThrowawayDB)。
// 不用 DSN 里的 guild_test:data 包的真库用例在那个库里 DROP 重建全部表,`go test -tags integration ./...`
// 按包并行跑时两个测试进程会互删对方正在用的表。
//
// 配表:本包其它用例依赖"未加载配表"的状态(见 economy_config_test.go 文件头)。table 包没有卸载接口,
// 所以 loadEconomyFlowTables 不往现有实例里加载,而是换上新建的管理器实例再加载、用例结束换回原实例
// (见该函数注释;它定义在 economy_logic_test.go,单测与本文件共用一份)。因此不带 `-run` 整包跑
// `-tags integration` 也不会污染同进程的其它用例。
//
// 数值:一律以真实配表 generated/tables/*.json 为准,用到的每个数在用例旁注明出处;升级用例另由
// requireUpgradeTable 在开头核对配表,配表改了就明确失败,而不是让断言悄悄变味。
//
// 时钟:业务时钟(EconomyDeps.Now)与重投循环共用一只可拨动的假钟,起点取真实时间 ——
// 离帮事务的提前截止用的是 nowMs()(真实时钟)。需要越过它的用例从库里读出截止时刻再把假钟拨过去,
// 不按固定时长拨(那会让结果依赖建环境到离帮之间的真实耗时)。
//
// logic 包拿不到 data 包的测试助手(_test.go 不跨包导出),所以建表 / 删表在本文件里写了最小的一份:
// 同一条 schemamigrate 建表路径,删表清单与 data/guild_repo_test.go 的 guildTestDropTables 一致。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"regexp"
	"shared/authoritysql/sqltest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"guild/internal/constants"
	"guild/internal/data"
	assetpb "proto/common/asset"
	pb "proto/guild"
	scenepb "proto/scene"
	"schemamigrate"
	"shared/assetop"
	"shared/assetop/assetoptest"
	"shared/gameday"
	tablepb "shared/generated/pb/table"
)

// ── 建表 / 删表(最小助手)────────────────────────────────────

func TestEconomyFlowExpectedGuildRejectsBeforeReservation(t *testing.T) {
	env := newEconomyFlowEnv(t, 1)
	env.seedGuild(t, flowGuildID, flowLeader)
	env.setContribution(t, flowGuildID, flowLeader, 1000)
	ctx := clientCtx(flowLeader)
	donation, err := env.logic.DonateToGuild(ctx, &pb.DonateToGuildRequest{DonateId: 1, ClientRequestId: 100, ExpectedGuildId: flowGuildID + 1})
	require.NoError(t, err)
	require.Equal(t, constants.ErrNotInGuild, donation.GetErrorMessage().GetId())
	order, err := env.logic.BuyGuildShopGoods(ctx, &pb.BuyGuildShopGoodsRequest{GoodsId: 101, Count: 1, ClientRequestId: 101, ExpectedGuildId: flowGuildID + 1})
	require.NoError(t, err)
	require.Equal(t, constants.ErrNotInGuild, order.GetErrorMessage().GetId())
	require.Zero(t, env.opCountOf(t, flowLeader))
	require.Zero(t, env.clientRequestCount(t))
	require.Zero(t, env.scene.totalCalls())
	_, balance, found := env.contribution(t, flowGuildID, flowLeader)
	require.True(t, found)
	require.Equal(t, uint64(1000), balance, "错误帮会请求不能扣帮贡")
}

func TestEconomyFlowReplayCannotChangeExpectedGuild(t *testing.T) {
	env := newEconomyFlowEnv(t, 1)
	env.seedGuild(t, flowGuildID, flowLeader)
	ctx := clientCtx(flowLeader)
	req := &pb.DonateToGuildRequest{DonateId: 1, ClientRequestId: 102, ExpectedGuildId: flowGuildID}
	first, err := env.logic.DonateToGuild(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, first.GetDonation())
	req.ExpectedGuildId++
	replayed, err := env.logic.DonateToGuild(ctx, req)
	require.NoError(t, err)
	require.Equal(t, uint32(tablepb.CommonError_kInvalidParameter), replayed.GetErrorMessage().GetId())
	require.Nil(t, replayed.GetDonation())
	require.Equal(t, 1, env.opCountOf(t, flowLeader))
}

// economyFlowDropTables:本服务全部表 + schemamigrate 台账,逆锁序。新增表时同步追加
// (resetEconomyFlowSchema 按 len(data.Tables())+1 守数量,漏加即红)。
var economyFlowDropTables = []string{
	"guild_client_request",                          // 客户端幂等键去重记录:全序末尾 R,逆锁序排最前
	"guild_trial_reward_owed", "guild_trial_battle", // B6b 同道历练两表 W、T
	"guild_activity_progress", // B6a 活动进度 P
	"guild_daily_counter", "guild_asset_op", "guild_player_op_seq",
	"guild_application", "guild_member", "guild_player_state", "guild_trial_launch_role", "guild_trial_launch", "guild_trial_lobby_slot", "guild_trial_lobby", "guild", "schema_migrations",
}

// economyFlowDBNamePattern:只许碰一次性测试库。appuser 对别的库也有 ALL 权限,黑名单挡不住 DSN 指错。
var economyFlowDBNamePattern = regexp.MustCompile(`^(guild_test|guild_it_\d+_\d+)$`)

func resetEconomyFlowSchema(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	require.Len(t, economyFlowDropTables, len(data.Tables())+1, "新增表后同步追加 economyFlowDropTables")
	var dbName string
	require.NoError(t, db.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&dbName))
	if !economyFlowDBNamePattern.MatchString(dbName) {
		t.Fatalf("测试 DSN 指向库 %q:只允许 guild_test 或 guild_it_<pid>_<n>", dbName)
	}
	for _, name := range economyFlowDropTables {
		_, err := db.ExecContext(ctx, "DROP TABLE IF EXISTS `"+name+"`")
		require.NoError(t, err, name)
	}
	report, err := schemamigrate.Up(ctx, db, schemamigrate.Options{Database: dbName, Tables: data.Tables(), Logf: t.Logf})
	require.NoError(t, err)
	require.Empty(t, report.Manual, "新建库不应出现需人工项")
}

// economyFlowDBCounter 给一次性库编号:同一测试进程里每个用例一个库。
var economyFlowDBCounter atomic.Uint32

// openEconomyFlowDB 用 rawDSN 的账号建一次性库 guild_it_<pid>_<n>、建表,返回连到它的连接池;用例结束 DROP。
// rawDSN 里的库名只被忽略、从不被碰(理由见文件头"库")。
//
// 账号建不了库时直接失败而不是 Skip:DSN 是显式设的,悄悄跳过等于把端到端覆盖静默丢掉。
func openEconomyFlowDB(t *testing.T, rawDSN string) *sql.DB {
	t.Helper()
	name := fmt.Sprintf("guild_it_%d_%d", os.Getpid(), economyFlowDBCounter.Add(1))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dsn, err := data.WithLockWaitTimeout(rawDSN)
	require.NoError(t, err)
	opts, err := sqltest.OptionsFromEnv(dsn, name+"_writer", name)
	require.NoError(t, err)
	// 该既有测试 MySQL 与 TiDB 端点都能跑,档位按真实引擎选；不把 MySQL 冒充 TiDB 锁资格验收。
	opts.AllowMySQL = true
	fixture, err := sqltest.New(ctx, t, opts)
	require.NoError(t, err)
	installDB, err := fixture.OpenSchema(ctx, name)
	require.NoError(t, err)
	resetEconomyFlowSchema(t, ctx, installDB)
	require.NoError(t, fixture.GrantProtoTables(ctx, name, data.Tables()))
	db, err := fixture.Open(ctx, name, 20)
	require.NoError(t, err)

	return db
}

func TestEconomyFlowTiDBConnectionsKeepStrongSession(t *testing.T) {
	env := newEconomyFlowEnv(t, 1)
	var version string
	require.NoError(t, env.db.QueryRowContext(context.Background(), "SELECT VERSION()").Scan(&version))
	if !strings.Contains(strings.ToLower(version), "tidb") {
		t.Skip("该断言只适用于显式TiDB夹具，旧MySQL兼容另测")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var connections []*sql.Conn
	defer func() {
		for _, conn := range connections {
			conn.Close()
		}
	}()
	for i := 0; i < 3; i++ {
		conn, err := env.db.Conn(ctx)
		require.NoError(t, err)
		connections = append(connections, conn)
		var mode, isolation, replica, consistency, snapshot string
		var stale, low, lockWait int64
		require.NoError(t, conn.QueryRowContext(ctx, "SELECT @@tidb_txn_mode,@@transaction_isolation,@@tidb_read_staleness,@@tidb_low_resolution_tso,@@tidb_replica_read,@@tidb_read_consistency,@@tidb_snapshot,@@innodb_lock_wait_timeout").Scan(&mode, &isolation, &stale, &low, &replica, &consistency, &snapshot, &lockWait))
		require.Equal(t, "pessimistic", mode)
		require.Equal(t, "READ-COMMITTED", isolation)
		require.Zero(t, stale)
		require.Zero(t, low)
		require.Equal(t, "leader", replica)
		require.Equal(t, "strict", consistency)
		require.Empty(t, snapshot)
		require.EqualValues(t, 1, lockWait)
	}
}

// ── 替身 ──────────────────────────────────────────────────────

// flowClock 是业务与重投循环共用的假钟。
type flowClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *flowClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *flowClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// AdvanceTo 把假钟拨到 at;at 不晚于当前时刻时不动(假钟不往回拨)。
func (c *flowClock) AdvanceTo(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if at.After(c.now) {
		c.now = at
	}
}

// sceneCall 记一次投递:哪个 RPC、哪条指令(correlation_id = op_id)。
type sceneCall struct {
	rpc  assetop.RPC
	opID uint64
}

// scriptedScene 实现 assetop.Applier:按脚本作答并记下每次调用。加锁:重投循环的 worker 并发调用。
type scriptedScene struct {
	assetoptest.CommittedLedger

	mu      sync.Mutex
	respond func(rpc assetop.RPC, req *assetpb.AssetOpRequest) assetop.Result
	calls   []sceneCall
}

var _ assetop.Applier = (*scriptedScene)(nil)

func (s *scriptedScene) Do(_ context.Context, rpc assetop.RPC, req *assetpb.AssetOpRequest) (assetop.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, sceneCall{rpc: rpc, opID: req.GetCorrelationId()})
	if s.respond == nil {
		return assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_RETRY}, nil
	}
	res := s.respond(rpc, req)
	// 此替身模拟scene将资产与账本一起提交；Loop仍经独立读取面核对，
	// 不向回包填SQLCommitted。真实玩家SQL提交由db/assetop集成测试覆盖。
	if err := s.Commit(req, res); err != nil {
		return assetop.Result{}, err
	}
	return res, nil
}

func (s *scriptedScene) answer(fn func(assetop.RPC, *assetpb.AssetOpRequest) assetop.Result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.respond = fn
}

func (s *scriptedScene) callsFor(opID uint64) []assetop.RPC {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []assetop.RPC
	for _, c := range s.calls {
		if c.opID == opID {
			out = append(out, c.rpc)
		}
	}
	return out
}

func (s *scriptedScene) totalCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func always(res assetop.Result) func(assetop.RPC, *assetpb.AssetOpRequest) assetop.Result {
	return func(assetop.RPC, *assetpb.AssetOpRequest) assetop.Result { return res }
}

// scene 的典型答复:已应用且落盘 / 战斗中暂不结算 / 永久拒绝且落盘 / 已应用但未落盘 /
// 背包满暂不发放 / 中止占位(Abort 打在 scene 从未见过的 seq 上:REJECTED、reason=0、落盘)。
var (
	flowApplied           = assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED, Durable: true}
	flowInBattle          = assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_RETRY, Reason: assetop.ReasonInBattle}
	flowBlocked           = assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_REJECTED, Reason: assetop.ReasonBlocked, Durable: true}
	flowAppliedNotDurable = assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED}
	flowBagFull           = assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_RETRY, Reason: assetop.ReasonBagFull}
	flowAbortPlaceholder  = assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_REJECTED, Durable: true}
)

// sequenceMinter 是 op_id 发号器替身:单调递增、并发安全。
type sequenceMinter struct{ next atomic.Uint64 }

func (m *sequenceMinter) Mint(context.Context) (uint64, error) { return m.next.Add(1), nil }

// ── 环境 ──────────────────────────────────────────────────────

const (
	flowZone    uint32 = 1
	flowGuildID uint64 = 5001
	flowLeader  uint64 = 6001
)

type economyFlowEnv struct {
	db       *sql.DB
	store    *data.GuildAssetStore
	scene    *scriptedScene
	clock    *flowClock
	notifier *recordingNotifier
	// syncBumps 记录落地恢复摘要的 bump(recordingSyncBumper 定义在 sync_digest_test.go)。
	syncBumps *recordingSyncBumper
	loopCfg   assetop.LoopConfig
	loop      *assetop.Loop
	logic     *GuildLogic
}

func newEconomyFlowEnv(t *testing.T, workers int) *economyFlowEnv {
	t.Helper()
	dsn := os.Getenv("GUILD_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("GUILD_TEST_MYSQL_DSN 未设置,跳过帮会经济端到端用例")
	}
	db := openEconomyFlowDB(t, dsn)
	loadEconomyFlowTables(t)

	mr := miniredis.RunT(t)
	rdb := goredis.NewClient(&goredis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { rdb.Close() })
	repo := data.NewGuildRepo(rdb, db, time.Minute)
	economyRepo, err := data.NewEconomyRepo(repo)
	require.NoError(t, err)
	store, err := data.NewGuildAssetStore(repo)
	require.NoError(t, err)

	clock := &flowClock{now: time.Now()}
	scene := &scriptedScene{}
	cfg := assetop.DefaultLoopConfig()
	cfg.Workers = workers
	cfg.Lease = 10 * time.Second
	cfg.BaseBackoff = AssetOpRetryBase()
	cfg.Streams = []assetpb.AssetOpStream{
		assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT,
		assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_CREDIT,
	}
	loop, err := assetop.NewLoop(cfg, store, scene, nil, clock.Now)
	require.NoError(t, err)
	loop.Ledger = scene

	notifier := &recordingNotifier{}
	syncBumps := &recordingSyncBumper{}
	minter := &sequenceMinter{}
	minter.next.Store(900_000)
	l := NewGuildLogic(repo, nil, nil, &fakeHomeZones{zone: flowZone},
		WithNotifier(notifier),
		WithSyncBumper(syncBumps),
		WithEconomy(EconomyDeps{
			Repo:       economyRepo,
			Loop:       loop,
			OpIDs:      minter,
			Now:        clock.Now,
			SyncBudget: 2500 * time.Millisecond,
			Lease:      cfg.Lease,
		}))
	// 与 guild.go 同序:推送回调在循环与 RPC 开始之前接上,之后不再改。
	store.OnFinalized = l.OnAssetFinalized

	return &economyFlowEnv{
		db:        db,
		store:     store,
		scene:     scene,
		clock:     clock,
		notifier:  notifier,
		syncBumps: syncBumps,
		loopCfg:   cfg,
		loop:      loop,
		logic:     l,
	}
}

// seedGuild 直接写 MySQL:1 级帮会、帮主 + 若干成员,帮贡与资金全 0。
func (e *economyFlowEnv) seedGuild(t *testing.T, guildID, leaderID uint64, members ...uint64) {
	t.Helper()
	ctx := context.Background()
	name := fmt.Sprintf("flow-%d", guildID)
	_, err := e.db.ExecContext(ctx, `INSERT INTO guild
		(guild_id, name, name_norm, leader_id, level, announcement, create_time_ms, max_members, zone_id, score, funds)
		VALUES (?, ?, ?, ?, ?, '', 1, 30, ?, 0, 0)`,
		guildID, name, name, leaderID, constants.DefaultInitLevel, flowZone)
	require.NoError(t, err)
	insert := func(playerID uint64, role uint32) {
		_, err := e.db.ExecContext(ctx, `INSERT INTO guild_member
			(guild_id, player_id, role, join_time_ms, last_active_ms, contribution_total, contribution_balance)
			VALUES (?, ?, ?, 1, 1, 0, 0)`, guildID, playerID, role)
		require.NoError(t, err)
	}
	insert(leaderID, constants.RoleLeader)
	for _, id := range members {
		insert(id, constants.RoleMember)
	}
}

func (e *economyFlowEnv) funds(t *testing.T, guildID uint64) uint64 {
	t.Helper()
	var funds uint64
	require.NoError(t, e.db.QueryRowContext(context.Background(),
		"SELECT funds FROM guild WHERE guild_id = ?", guildID).Scan(&funds))
	return funds
}

func (e *economyFlowEnv) contribution(t *testing.T, guildID, playerID uint64) (total, balance uint64, found bool) {
	t.Helper()
	err := e.db.QueryRowContext(context.Background(),
		"SELECT contribution_total, contribution_balance FROM guild_member WHERE guild_id = ? AND player_id = ?",
		guildID, playerID).Scan(&total, &balance)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, false
	}
	require.NoError(t, err)
	return total, balance, true
}

func (e *economyFlowEnv) opStatus(t *testing.T, opID uint64) (pb.GuildAssetOpStatus, uint32) {
	t.Helper()
	var (
		st        int32
		reasonTip uint32
	)
	require.NoError(t, e.db.QueryRowContext(context.Background(),
		"SELECT status, reason_tip_id FROM guild_asset_op WHERE op_id = ?", opID).Scan(&st, &reasonTip))
	return pb.GuildAssetOpStatus(st), reasonTip
}

// usedCount 读计数行;行不存在按 0。counter_kind 绑定生成常量,不写数字。
func (e *economyFlowEnv) usedCount(t *testing.T, playerID uint64, kind pb.GuildDailyCounterKind, refID, periodKey uint32) uint32 {
	t.Helper()
	var used uint32
	err := e.db.QueryRowContext(context.Background(),
		"SELECT used_count FROM guild_daily_counter WHERE player_id = ? AND counter_kind = ? AND ref_id = ? AND period_key = ?",
		playerID, uint32(kind), refID, periodKey).Scan(&used)
	if errors.Is(err, sql.ErrNoRows) {
		return 0
	}
	require.NoError(t, err)
	return used
}

func (e *economyFlowEnv) donate(t *testing.T, playerID uint64, donateID uint32) *pb.DonateToGuildResponse {
	t.Helper()
	resp, err := e.logic.DonateToGuild(clientCtx(playerID), &pb.DonateToGuildRequest{DonateId: donateID})
	require.NoError(t, err)
	require.NotNil(t, resp.GetDonation(), "预留成功就必须回订单视图(tip=%v)", resp.GetErrorMessage())
	return resp
}

// setFunds / setRole / setContribution 直接改 MySQL。必须在该帮会第一次被任何 RPC 读进缓存**之前**调用:
// 之后再改库,缓存快照在 TTL 内看不到(测试里没有走失效路径)。
func (e *economyFlowEnv) setFunds(t *testing.T, guildID, funds uint64) {
	t.Helper()
	_, err := e.db.ExecContext(context.Background(), "UPDATE guild SET funds = ? WHERE guild_id = ?", funds, guildID)
	require.NoError(t, err)
}

func (e *economyFlowEnv) setRole(t *testing.T, guildID, playerID uint64, role uint32) {
	t.Helper()
	_, err := e.db.ExecContext(context.Background(),
		"UPDATE guild_member SET role = ? WHERE guild_id = ? AND player_id = ?", role, guildID, playerID)
	require.NoError(t, err)
}

func (e *economyFlowEnv) setContribution(t *testing.T, guildID, playerID, amount uint64) {
	t.Helper()
	_, err := e.db.ExecContext(context.Background(),
		"UPDATE guild_member SET contribution_total = ?, contribution_balance = ? WHERE guild_id = ? AND player_id = ?",
		amount, amount, guildID, playerID)
	require.NoError(t, err)
}

// seedApplication 写一条有效的待审申请。过期时刻取**真实时钟** + 1 小时:
// GuildInfo 的待审数由 CountLiveApplications(ctx, guildID, nowMs()) 现算,用的是真实时钟而不是假钟。
func (e *economyFlowEnv) seedApplication(t *testing.T, guildID, playerID uint64) {
	t.Helper()
	applyMs := uint64(time.Now().UnixMilli())
	_, err := e.db.ExecContext(context.Background(),
		"INSERT INTO guild_application (guild_id, player_id, apply_ms, expire_ms) VALUES (?, ?, ?, ?)",
		guildID, playerID, applyMs, applyMs+durationMs(time.Hour))
	require.NoError(t, err)
}

// guildLevelRow 读升级用例关心的三列:等级、资金、成员上限。
func (e *economyFlowEnv) guildLevelRow(t *testing.T, guildID uint64) (level uint32, funds uint64, maxMembers uint32) {
	t.Helper()
	require.NoError(t, e.db.QueryRowContext(context.Background(),
		"SELECT level, funds, max_members FROM guild WHERE guild_id = ?", guildID).Scan(&level, &funds, &maxMembers))
	return level, funds, maxMembers
}

// flowOpRow 是一行指令里同步投递 / 重投循环会改写的列(opStatus 只读 status 与 reason_tip_id,不够判重排)。
type flowOpRow struct {
	status        pb.GuildAssetOpStatus
	durable       uint32
	attempts      uint32
	nextAttemptMs uint64
	deadlineMs    uint64
	leaseUntilMs  uint64
	lastReason    uint32
	reasonTipID   uint32
}

func (e *economyFlowEnv) opRow(t *testing.T, opID uint64) flowOpRow {
	t.Helper()
	var (
		row flowOpRow
		st  int32
	)
	require.NoError(t, e.db.QueryRowContext(context.Background(),
		"SELECT status, durable, attempts, next_attempt_ms, deadline_ms, lease_until_ms, last_reason, reason_tip_id"+
			" FROM guild_asset_op WHERE op_id = ?", opID).Scan(
		&st, &row.durable, &row.attempts, &row.nextAttemptMs, &row.deadlineMs, &row.leaseUntilMs,
		&row.lastReason, &row.reasonTipID))
	row.status = pb.GuildAssetOpStatus(st)
	return row
}

// 升级用例的数值,出处 generated/tables/guildlevel.json:
// id=1 的 upgrade_cost_funds(1 级升 2 级的花费)、id=2 的 upgrade_cost_funds(2 级升 3 级)与 max_members(2 级成员上限)。
const (
	flowUpgradeCostL1 uint64 = 20000
	flowUpgradeCostL2 uint64 = 50000
	flowMaxMembersL2  uint32 = 35
)

// requireUpgradeTable 用生产同一个查表函数核对上面三个数。配表改了就在这里明确失败,
// 而不是让"资金刚好够 / 差 1"之类的构造悄悄失去意义。须在 newEconomyFlowEnv(它加载配表)之后调用。
func requireUpgradeTable(t *testing.T) {
	t.Helper()
	cost1, _, ok := upgradeLevelLookup(1)
	require.True(t, ok, "guildlevel.json 缺 id=1")
	cost2, maxMembers2, ok := upgradeLevelLookup(2)
	require.True(t, ok, "guildlevel.json 缺 id=2")
	require.Equal(t, flowUpgradeCostL1, cost1, "guildlevel.json 变了:同步本文件的升级常量")
	require.Equal(t, flowUpgradeCostL2, cost2, "guildlevel.json 变了:同步本文件的升级常量")
	require.Equal(t, flowMaxMembersL2, maxMembers2, "guildlevel.json 变了:同步本文件的升级常量")
}

// ── 用例 ──────────────────────────────────────────────────────

// TestEconomyFlowDonateAppliedSynchronously:同步投递拿到 APPLIED + durable → 当场入资金与帮贡、
// 今日次数保留;同步路径上终结不推送(调用方手上就是回包)。
func TestEconomyFlowDonateAppliedSynchronously(t *testing.T) {
	env := newEconomyFlowEnv(t, 8)
	env.seedGuild(t, flowGuildID, flowLeader)
	env.scene.answer(always(flowApplied))

	resp := env.donate(t, flowLeader, 1)

	assert.Nil(t, resp.GetErrorMessage())
	donation := resp.GetDonation()
	assert.Equal(t, pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_APPLIED, donation.GetStatus())
	assert.Equal(t, uint32(0), donation.GetCurrencyType())
	assert.Equal(t, uint64(10000), donation.GetCostAmount())
	assert.Equal(t, uint64(1000), resp.GetGuild().GetFunds(), "回包里的 GuildInfo 必须是入账之后的")

	assert.Equal(t, uint64(1000), env.funds(t, flowGuildID))
	total, balance, found := env.contribution(t, flowGuildID, flowLeader)
	require.True(t, found)
	assert.Equal(t, uint64(10), total)
	assert.Equal(t, uint64(10), balance)
	st, _ := env.opStatus(t, donation.GetOpId())
	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED, st)
	assert.Equal(t, uint32(1), env.usedCount(t, flowLeader, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_DONATE,
		1, gameday.DayKey(env.clock.Now())), "APPLIED 保留今日次数")
	assert.Equal(t, []assetop.RPC{assetop.RPCDebit}, env.scene.callsFor(donation.GetOpId()))
	assert.Empty(t, env.notifier.pushesOf(pb.GuildChangeKind_GUILD_CHANGE_KIND_FUNDS_CHANGED),
		"同步投递里终结的不推送:标记要穿过 assetop 的 settleContext 到达 OnFinalized")
}

// TestEconomyFlowPendingThenFinalizedByTick:同步投递遇到战斗中 → 回 PENDING(原因 assetop.ReasonInBattle,不进 error_message);
// 之后重投循环领取并终结,入账一次,推 FUNDS_CHANGED 给本人。
func TestEconomyFlowPendingThenFinalizedByTick(t *testing.T) {
	env := newEconomyFlowEnv(t, 8)
	env.seedGuild(t, flowGuildID, flowLeader)
	env.scene.answer(always(flowInBattle))

	resp := env.donate(t, flowLeader, 2)

	assert.Nil(t, resp.GetErrorMessage(), "PENDING 不是错误,只能用视图表达")
	opID := resp.GetDonation().GetOpId()
	assert.Equal(t, pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_PENDING, resp.GetDonation().GetStatus())
	assert.Equal(t, assetop.ReasonInBattle, resp.GetDonation().GetReasonTipId())
	assert.Zero(t, env.funds(t, flowGuildID))

	options, err := env.logic.GetGuildDonateOptions(clientCtx(flowLeader), &pb.GetGuildDonateOptionsRequest{})
	require.NoError(t, err)
	require.Len(t, options.GetPendingDonations(), 1)
	assert.Equal(t, opID, options.GetPendingDonations()[0].GetOpId())
	assert.Equal(t, uint64(100000), options.GetPendingDonations()[0].GetCostAmount(), "待结算视图的数额从 payload 解出")

	env.scene.answer(always(flowApplied))
	env.clock.Advance(5 * time.Second) // 越过退避(attempts=0 时 1× 重投基准)
	assert.Equal(t, 1, env.loop.Tick(context.Background()))

	st, _ := env.opStatus(t, opID)
	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED, st)
	assert.Equal(t, uint64(12000), env.funds(t, flowGuildID))
	total, _, _ := env.contribution(t, flowGuildID, flowLeader)
	assert.Equal(t, uint64(120), total)
	pushes := env.notifier.pushesOf(pb.GuildChangeKind_GUILD_CHANGE_KIND_FUNDS_CHANGED)
	require.Len(t, pushes, 1)
	assert.Equal(t, []uint64{flowLeader}, pushes[0].recipients)
	assert.Equal(t, flowGuildID, pushes[0].change.GetGuildId())

	assert.Zero(t, env.loop.Tick(context.Background()), "已终结的行不会再被领取")

	after, err := env.logic.GetGuildDonateOptions(clientCtx(flowLeader), &pb.GetGuildDonateOptionsRequest{})
	require.NoError(t, err)
	assert.Empty(t, after.GetPendingDonations())
	require.Len(t, after.GetRecentResults(), 1)
	assert.Equal(t, pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_APPLIED, after.GetRecentResults()[0].GetStatus())
}

// TestEconomyFlowLeaveAcceleratesAbortThatTurnsOutApplied(D2 覆盖块第 2、4 条):
// 未结算时离帮 → 截止提前到离帮时刻 → 循环改发中止 → scene 说"中止前已经扣了"(APPLIED)→
// 资金照记到发起时绑定的帮会(op.guild_id),成员行已不在所以帮贡跳过,不插任何退款行。
func TestEconomyFlowLeaveAcceleratesAbortThatTurnsOutApplied(t *testing.T) {
	const member uint64 = 6002
	env := newEconomyFlowEnv(t, 8)
	env.seedGuild(t, flowGuildID, flowLeader, member)
	env.scene.answer(always(flowInBattle))

	resp := env.donate(t, member, 1)
	opID := resp.GetDonation().GetOpId()
	require.Equal(t, pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_PENDING, resp.GetDonation().GetStatus())

	leave, err := env.logic.LeaveGuild(clientCtx(member), &pb.LeaveGuildRequest{})
	require.NoError(t, err)
	require.Nil(t, leave.GetErrorMessage())

	// 提前截止写的是离帮时刻(真实时钟 nowMs()),从库里读出来再把循环的假钟拨过去 ——
	// 按固定时长拨会让结果依赖"建环境 → 离帮"的真实耗时(慢机器上超过那个时长,循环就会继续发扣款)。
	deadlineAfterMs, ok := assetOpDeadlineMs()
	require.True(t, ok)
	var acceleratedMs uint64
	require.NoError(t, env.db.QueryRowContext(context.Background(),
		"SELECT deadline_ms FROM guild_asset_op WHERE op_id = ?", opID).Scan(&acceleratedMs))
	require.Less(t, acceleratedMs, resp.GetDonation().GetCreatedMs()+deadlineAfterMs, "离帮必须把截止提前到离帮时刻")
	env.clock.AdvanceTo(time.UnixMilli(int64(acceleratedMs) + 1))
	env.scene.answer(func(rpc assetop.RPC, _ *assetpb.AssetOpRequest) assetop.Result {
		if rpc == assetop.RPCAbort {
			return flowApplied
		}
		return flowInBattle
	})
	assert.Equal(t, 1, env.loop.Tick(context.Background()))

	calls := env.scene.callsFor(opID)
	require.NotEmpty(t, calls)
	assert.Equal(t, assetop.RPCAbort, calls[len(calls)-1], "离帮之后必须改发中止,而不是继续扣款")
	st, _ := env.opStatus(t, opID)
	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED, st)
	assert.Equal(t, uint64(1000), env.funds(t, flowGuildID), "资金照记到 op.guild_id")
	_, _, found := env.contribution(t, flowGuildID, member)
	assert.False(t, found, "成员行已删,帮贡无处可记(只计 orphan,不补偿)")

	var ops int
	require.NoError(t, env.db.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM guild_asset_op WHERE player_id = ?", member).Scan(&ops))
	assert.Equal(t, 1, ops, "D2:不再插退款指令")
	require.Len(t, env.notifier.pushesOf(pb.GuildChangeKind_GUILD_CHANGE_KIND_FUNDS_CHANGED), 1)
}

// TestEconomyFlowShopRejectedRefundsContributionAndLimit:兑换被 scene 永久拒绝 → 退帮贡、退限购,
// 回包 error_message = kGuildAssetRejected,余额是退回之后直读 MySQL 的值。
func TestEconomyFlowShopRejectedRefundsContributionAndLimit(t *testing.T) {
	env := newEconomyFlowEnv(t, 8)
	env.seedGuild(t, flowGuildID, flowLeader)
	// 在任何缓存读之前改库:economyCaller 的帮贡预判读的是之后才装进缓存的快照。
	_, err := env.db.ExecContext(context.Background(),
		"UPDATE guild_member SET contribution_total = ?, contribution_balance = ? WHERE guild_id = ? AND player_id = ?",
		1000, 1000, flowGuildID, flowLeader)
	require.NoError(t, err)
	env.scene.answer(always(flowBlocked))

	resp, err := env.logic.BuyGuildShopGoods(clientCtx(flowLeader), &pb.BuyGuildShopGoodsRequest{GoodsId: 101, Count: 2})

	require.NoError(t, err)
	assert.Equal(t, constants.ErrAssetRejected, resp.GetErrorMessage().GetId())
	order := resp.GetOrder()
	require.NotNil(t, order)
	assert.Equal(t, pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_REJECTED, order.GetStatus())
	assert.Equal(t, uint32(2), order.GetCount())
	assert.Equal(t, uint64(60), order.GetCostContribution())
	assert.Equal(t, assetop.ReasonBlocked, order.GetReasonTipId())
	assert.Equal(t, uint64(1000), resp.GetContributionBalance(), "拒绝后余额必须是退回之后的")

	_, balance, found := env.contribution(t, flowGuildID, flowLeader)
	require.True(t, found)
	assert.Equal(t, uint64(1000), balance)
	assert.Zero(t, env.usedCount(t, flowLeader, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_SHOP,
		101, gameday.DayKey(env.clock.Now())), "限购份数按 ref_count 全部退回")
	st, reasonTip := env.opStatus(t, order.GetOpId())
	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_REJECTED, st)
	assert.Equal(t, assetop.ReasonBlocked, reasonTip)
	assert.Equal(t, []assetop.RPC{assetop.RPCCredit}, env.scene.callsFor(order.GetOpId()))
}

// TestEconomyFlowWorkersFinalizeEachOpExactlyOnce:两个副本(各一个 Loop,Lease 10s、Workers 4)
// 同时 Tick 同一批到期行 —— Claim 的主键 CAS 保证每行只被一个副本投递、只终结一次,资金不多记。
func TestEconomyFlowWorkersFinalizeEachOpExactlyOnce(t *testing.T) {
	const players = 12
	env := newEconomyFlowEnv(t, 4)
	members := make([]uint64, 0, players-1)
	for i := uint64(1); i < players; i++ {
		members = append(members, flowLeader+i)
	}
	env.seedGuild(t, flowGuildID, flowLeader, members...)
	env.scene.answer(always(flowInBattle))

	opIDs := make([]uint64, 0, players)
	for _, playerID := range append([]uint64{flowLeader}, members...) {
		resp := env.donate(t, playerID, 1)
		require.Equal(t, pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_PENDING, resp.GetDonation().GetStatus())
		opIDs = append(opIDs, resp.GetDonation().GetOpId())
	}

	env.scene.answer(always(flowApplied))
	env.clock.Advance(5 * time.Second)
	replica, err := assetop.NewLoop(env.loopCfg, env.store, env.scene, nil, env.clock.Now)
	require.NoError(t, err)
	replica.Ledger = env.scene
	callsBefore := env.scene.totalCalls()

	var (
		wg        sync.WaitGroup
		processed [2]int
	)
	for i, loop := range []*assetop.Loop{env.loop, replica} {
		wg.Add(1)
		go func(i int, loop *assetop.Loop) {
			defer wg.Done()
			processed[i] = loop.Tick(context.Background())
		}(i, loop)
	}
	wg.Wait()

	assert.Equal(t, players, processed[0]+processed[1], "每行恰好被一个副本领取")
	assert.Equal(t, players, env.scene.totalCalls()-callsBefore, "每行只投递一次")
	for _, opID := range opIDs {
		st, _ := env.opStatus(t, opID)
		assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED, st, "op %d", opID)
	}
	assert.Equal(t, uint64(players*1000), env.funds(t, flowGuildID), "资金恰好记 N 次")
	assert.Len(t, env.notifier.pushesOf(pb.GuildChangeKind_GUILD_CHANGE_KIND_FUNDS_CHANGED), players)
}

// ── 捐献与兑换的非典型结局(完整性审查 finding 4)──────────────────
//
// 捐献 1 的数值出处 generated/tables/guilddonate.json id=1:扣 10000 银两、帮贡 +10、资金 +1000、每日 5 次。
// 商品 101 的数值出处 generated/tables/guildshop.json id=101:每份 30 帮贡、每日限 10 份(limit_period=1)。

// TestEconomyFlowDonateBudgetShortSkipsSyncDelivery(裁决 K):请求只剩 900ms,扣掉 1000ms 尾巴后预算为负
// → 不做同步投递:假 scene 调用 0 次,回包视图 PENDING(原因 0)、不进 error_message。
// 行带着插行时写下的租约(next_attempt_ms = lease_until_ms = 发起 + Lease),租约到期前循环不领,
// 到期后由循环正常扣款入账。
//
// 前提:900ms 内能完成前置 + 预留事务(本地库在几十毫秒量级)。慢库上会以预留超时失败,而不是误判为通过。
func TestEconomyFlowDonateBudgetShortSkipsSyncDelivery(t *testing.T) {
	env := newEconomyFlowEnv(t, 8)
	env.seedGuild(t, flowGuildID, flowLeader)
	env.scene.answer(always(flowApplied))

	ctx, cancel := context.WithTimeout(clientCtx(flowLeader), 900*time.Millisecond)
	defer cancel()
	resp, err := env.logic.DonateToGuild(ctx, &pb.DonateToGuildRequest{DonateId: 1})

	require.NoError(t, err)
	assert.Nil(t, resp.GetErrorMessage(), "预算不足不是错误,只能用视图表达")
	donation := resp.GetDonation()
	require.NotNil(t, donation, "预留已提交就必须回订单视图")
	assert.Equal(t, pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_PENDING, donation.GetStatus())
	assert.Zero(t, donation.GetReasonTipId(), "从未投递过,没有暂时原因")
	assert.Zero(t, env.scene.totalCalls(), "预算不足必须跳过同步投递")

	opID := donation.GetOpId()
	row := env.opRow(t, opID)
	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING, row.status)
	assert.Zero(t, row.attempts, "一次都没投过")
	assert.Equal(t, donation.GetCreatedMs()+durationMs(env.loopCfg.Lease), row.leaseUntilMs, "租约由插行写下")
	assert.Equal(t, row.leaseUntilMs, row.nextAttemptMs)
	assert.Zero(t, env.funds(t, flowGuildID))

	assert.Zero(t, env.loop.Tick(context.Background()), "租约未到期,循环不领")
	env.clock.AdvanceTo(time.UnixMilli(int64(row.leaseUntilMs) + 1))
	assert.Equal(t, 1, env.loop.Tick(context.Background()))

	assert.Equal(t, []assetop.RPC{assetop.RPCDebit}, env.scene.callsFor(opID), "租约到期后由循环按扣款投递一次")
	st, _ := env.opStatus(t, opID)
	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED, st)
	assert.Equal(t, uint64(1000), env.funds(t, flowGuildID))
	pushes := env.notifier.pushesOf(pb.GuildChangeKind_GUILD_CHANGE_KIND_FUNDS_CHANGED)
	require.Len(t, pushes, 1, "循环里终结的要推本人")
	assert.Equal(t, []uint64{flowLeader}, pushes[0].recipients)
}

// TestEconomyFlowDonateDeadlineAbortsAndRefundsCount(finding 4 ②):scene 一直回战斗中,捐献拖到截止
// → 循环改发中止 → scene 回 REJECTED、reason=0、durable(中止占位)→ 行终结为 ABORTED:
// 今日次数退回、资金与帮贡都不动、reason_tip_id=0,推 FUNDS_CHANGED 给本人(客户端据此重拉捐献页,看到次数已退)。
func TestEconomyFlowDonateDeadlineAbortsAndRefundsCount(t *testing.T) {
	env := newEconomyFlowEnv(t, 8)
	env.seedGuild(t, flowGuildID, flowLeader)
	env.scene.answer(always(flowInBattle))
	// 周期键在发起时取定:之后假钟要拨过 10 分钟,不能再按拨过之后的时刻算(可能跨游戏日切点)。
	dayKey := gameday.DayKey(env.clock.Now())

	resp := env.donate(t, flowLeader, 1)

	opID := resp.GetDonation().GetOpId()
	require.Equal(t, pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_PENDING, resp.GetDonation().GetStatus())
	require.Equal(t, uint32(1), env.usedCount(t, flowLeader, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_DONATE, 1, dayKey),
		"预留即占今日次数")
	deadlineAfterMs, ok := assetOpDeadlineMs()
	require.True(t, ok)
	row := env.opRow(t, opID)
	require.Equal(t, resp.GetDonation().GetCreatedMs()+deadlineAfterMs, row.deadlineMs,
		"截止 = 发起 + GuildRule.asset_op_deadline_seconds(generated/tables/guildrule.json 为 600s)")

	env.clock.AdvanceTo(time.UnixMilli(int64(row.deadlineMs)))
	env.scene.answer(func(rpc assetop.RPC, _ *assetpb.AssetOpRequest) assetop.Result {
		if rpc == assetop.RPCAbort {
			return flowAbortPlaceholder
		}
		return flowInBattle
	})
	assert.Equal(t, 1, env.loop.Tick(context.Background()))

	assert.Equal(t, []assetop.RPC{assetop.RPCDebit, assetop.RPCAbort}, env.scene.callsFor(opID),
		"同步投递扣款一次;到截止改发中止,不再扣款")
	st, reasonTip := env.opStatus(t, opID)
	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_ABORTED, st, "Abort + REJECTED + reason=0 = 中止占位")
	assert.Zero(t, reasonTip, "ABORTED 的 reason_tip_id 恒 0")
	assert.Zero(t, env.usedCount(t, flowLeader, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_DONATE, 1, dayKey),
		"中止退回今日次数")
	assert.Zero(t, env.funds(t, flowGuildID), "中止不记资金")
	total, balance, found := env.contribution(t, flowGuildID, flowLeader)
	require.True(t, found)
	assert.Zero(t, total, "中止不记帮贡")
	assert.Zero(t, balance)
	pushes := env.notifier.pushesOf(pb.GuildChangeKind_GUILD_CHANGE_KIND_FUNDS_CHANGED)
	require.Len(t, pushes, 1, "中止也推 FUNDS_CHANGED:客户端要重拉捐献页看到次数已退")
	assert.Equal(t, []uint64{flowLeader}, pushes[0].recipients)
	assert.Zero(t, env.loop.Tick(context.Background()), "已终结的行不会再被领取")
}

// TestEconomyFlowDonateAppliedNotDurableWaitsThenCreditsOnce(finding 4 ③):同步投递拿到 APPLIED 但未落盘 →
// 不能终结(I4:未落盘的结局可能被 scene 崩溃抹掉):回包 PENDING、不进 error_message,行按 AwaitDurableDelay
// 重排(契约的 now+500ms;假钟在同步投递期间不走,所以恰好相等)、资金与帮贡不动;下一轮拿到 durable 的 APPLIED
// 才入账,且只入一次。
func TestEconomyFlowDonateAppliedNotDurableWaitsThenCreditsOnce(t *testing.T) {
	env := newEconomyFlowEnv(t, 8)
	env.seedGuild(t, flowGuildID, flowLeader)
	env.scene.answer(always(flowAppliedNotDurable))

	resp := env.donate(t, flowLeader, 1)

	assert.Nil(t, resp.GetErrorMessage(), "未落盘的结局不是错误")
	opID := resp.GetDonation().GetOpId()
	assert.Equal(t, pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_PENDING, resp.GetDonation().GetStatus())
	row := env.opRow(t, opID)
	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING, row.status)
	assert.Zero(t, row.durable)
	assert.Equal(t, resp.GetDonation().GetCreatedMs()+durationMs(env.loopCfg.AwaitDurableDelay), row.nextAttemptMs,
		"未落盘按 AwaitDurableDelay 短延迟重排,不走指数退避")
	assert.Zero(t, row.leaseUntilMs, "重排释放租约,循环到点即可领取")
	assert.Zero(t, env.funds(t, flowGuildID), "未落盘不入账")
	total, _, found := env.contribution(t, flowGuildID, flowLeader)
	require.True(t, found)
	assert.Zero(t, total)

	env.scene.answer(always(flowApplied))
	env.clock.Advance(env.loopCfg.AwaitDurableDelay)
	assert.Equal(t, 1, env.loop.Tick(context.Background()))

	st, _ := env.opStatus(t, opID)
	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_APPLIED, st)
	assert.Equal(t, uint64(1000), env.funds(t, flowGuildID), "落盘之后入账")
	total, balance, _ := env.contribution(t, flowGuildID, flowLeader)
	assert.Equal(t, uint64(10), total)
	assert.Equal(t, uint64(10), balance)

	env.clock.Advance(env.loopCfg.MaxBackoff)
	assert.Zero(t, env.loop.Tick(context.Background()), "已终结的行不会再被领取")
	assert.Equal(t, uint64(1000), env.funds(t, flowGuildID), "只入一次")
	assert.Equal(t, []assetop.RPC{assetop.RPCDebit, assetop.RPCDebit}, env.scene.callsFor(opID))
	require.Len(t, env.notifier.pushesOf(pb.GuildChangeKind_GUILD_CHANGE_KIND_FUNDS_CHANGED), 1,
		"终结发生在循环里,推本人一次")
}

// TestEconomyFlowShopBagFullStaysPending(finding 4 ④):兑换永不中止(deadline_ms=0,05 R7)——
// scene 持续回 RETRY + 背包满,行就一直 PENDING:帮贡与限购都不退,视图原因是背包满,
// GetGuildShop 的 pending_orders 看得到它;无论重投多少轮都只发 Credit,绝不改发中止,也不推送。
func TestEconomyFlowShopBagFullStaysPending(t *testing.T) {
	const (
		startBalance uint64 = 1000
		count        uint32 = 2
		rounds              = 3
	)
	env := newEconomyFlowEnv(t, 8)
	env.seedGuild(t, flowGuildID, flowLeader)
	env.setContribution(t, flowGuildID, flowLeader, startBalance)
	env.scene.answer(always(flowBagFull))
	dayKey := gameday.DayKey(env.clock.Now())
	// 101 每份 30 帮贡(guildshop.json),2 份扣 60。
	afterBalance := startBalance - 30*uint64(count)

	resp, err := env.logic.BuyGuildShopGoods(clientCtx(flowLeader), &pb.BuyGuildShopGoodsRequest{GoodsId: 101, Count: count})

	require.NoError(t, err)
	assert.Nil(t, resp.GetErrorMessage(), "PENDING 不是错误,只能用视图表达")
	order := resp.GetOrder()
	require.NotNil(t, order)
	opID := order.GetOpId()
	assert.Equal(t, pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_PENDING, order.GetStatus())
	assert.Equal(t, assetop.ReasonBagFull, order.GetReasonTipId())
	assert.Equal(t, afterBalance, resp.GetContributionBalance(), "帮贡在预留事务里已扣")
	assert.Zero(t, env.opRow(t, opID).deadlineMs, "兑换永不中止")

	shop, err := env.logic.GetGuildShop(clientCtx(flowLeader), &pb.GetGuildShopRequest{})
	require.NoError(t, err)
	assert.Nil(t, shop.GetErrorMessage())
	require.Len(t, shop.GetPendingOrders(), 1)
	pending := shop.GetPendingOrders()[0]
	assert.Equal(t, opID, pending.GetOpId())
	assert.Equal(t, uint32(101), pending.GetGoodsId())
	assert.Equal(t, count, pending.GetCount())
	assert.Equal(t, pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_PENDING, pending.GetStatus())
	assert.Equal(t, assetop.ReasonBagFull, pending.GetReasonTipId(), "待结算视图展示最近一次暂时原因")
	assert.Equal(t, afterBalance, shop.GetContributionBalance())
	assert.Empty(t, shop.GetRecentOrders(), "未终结的行不进最近结果")

	// 每轮拨过退避封顶(指数退避不会超过 MaxBackoff),保证每一轮都到期、都被领取。
	for i := 0; i < rounds; i++ {
		env.clock.Advance(env.loopCfg.MaxBackoff + time.Second)
		require.Equal(t, 1, env.loop.Tick(context.Background()), "第 %d 轮重投", i+1)
	}

	st, _ := env.opStatus(t, opID)
	assert.Equal(t, pb.GuildAssetOpStatus_GUILD_ASSET_OP_STATUS_PENDING, st, "背包满只会一直等,不会终结")
	_, balance, found := env.contribution(t, flowGuildID, flowLeader)
	require.True(t, found)
	assert.Equal(t, afterBalance, balance, "PENDING 期间不退帮贡")
	assert.Equal(t, count, env.usedCount(t, flowLeader, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_SHOP, 101, dayKey),
		"PENDING 期间不退限购")
	calls := env.scene.callsFor(opID)
	require.Len(t, calls, 1+rounds, "同步投递一次 + 每轮重投一次")
	for _, rpc := range calls {
		assert.Equal(t, assetop.RPCCredit, rpc, "兑换永不改发中止")
	}
	assert.Zero(t, env.notifier.total(), "未终结不推送")

	after, err := env.logic.GetGuildShop(clientCtx(flowLeader), &pb.GetGuildShopRequest{})
	require.NoError(t, err)
	require.Len(t, after.GetPendingOrders(), 1, "重投多轮之后仍在待结算列表里")
	assert.Equal(t, opID, after.GetPendingOrders()[0].GetOpId())
}

// ── 升级(完整性审查 finding 3)────────────────────────────────

// TestEconomyFlowUpgradeByOfficerPushesLevelUpToOthers:长老升级成功 → 扣当前等级行的花费、成员上限取下一级行;
// LEVEL_UP 推给除操作者外的全员(B2 §14,操作者手上的回包就是最新状态);回包 GuildInfo 是升级之后的,
// 且长老看得到待审数(pending_application_count 只对长老 / 帮主非 0)。升级不走资产通道。
func TestEconomyFlowUpgradeByOfficerPushesLevelUpToOthers(t *testing.T) {
	const (
		officer   uint64 = 6002
		member    uint64 = 6003
		applicant uint64 = 6100
	)
	env := newEconomyFlowEnv(t, 8)
	requireUpgradeTable(t)
	env.seedGuild(t, flowGuildID, flowLeader, officer, member)
	env.setRole(t, flowGuildID, officer, constants.RoleOfficer)
	env.setFunds(t, flowGuildID, flowUpgradeCostL1)
	env.seedApplication(t, flowGuildID, applicant)

	resp, err := env.logic.UpgradeGuild(clientCtx(officer), &pb.UpgradeGuildRequest{ExpectedLevel: constants.DefaultInitLevel})

	require.NoError(t, err)
	assert.Nil(t, resp.GetErrorMessage())
	info := resp.GetGuild()
	require.NotNil(t, info, "成功回包必须带升级之后的 GuildInfo")
	assert.Equal(t, constants.DefaultInitLevel+1, info.GetLevel())
	assert.Zero(t, info.GetFunds(), "资金恰好够一级,扣完为 0")
	assert.Equal(t, flowMaxMembersL2, info.GetMaxMembers(), "成员上限取下一级行")
	assert.Equal(t, uint32(1), info.GetPendingApplicationCount(), "长老回包必须带待审数")

	level, funds, maxMembers := env.guildLevelRow(t, flowGuildID)
	assert.Equal(t, constants.DefaultInitLevel+1, level)
	assert.Zero(t, funds)
	assert.Equal(t, flowMaxMembersL2, maxMembers)

	pushes := env.notifier.pushesOf(pb.GuildChangeKind_GUILD_CHANGE_KIND_LEVEL_UP)
	require.Len(t, pushes, 1)
	assert.Equal(t, 1, env.notifier.total(), "升级只推 LEVEL_UP 一条")
	assert.ElementsMatch(t, []uint64{flowLeader, member}, pushes[0].recipients, "推给除操作者外的全员")
	assert.Equal(t, officer, pushes[0].change.GetActorPlayerId())
	assert.Zero(t, pushes[0].change.GetTargetPlayerId())
	assert.Equal(t, flowGuildID, pushes[0].change.GetGuildId())
	assert.Zero(t, env.scene.totalCalls(), "升级只动 guild 库,不走资产通道")
}

// TestEconomyFlowUpgradeStaleExpectedLevelIsNoop:重复点击 / 回执丢失后的重发(expected_level 与库里不符)
// 回成功 + 最新 GuildInfo,但不扣第二次、不连升两级、不再推 LEVEL_UP。资金按两级的花费备足:
// 若 expected_level 的判定失效,第二次会真的升到 3 级并把资金扣光,断言立刻变红。
func TestEconomyFlowUpgradeStaleExpectedLevelIsNoop(t *testing.T) {
	const member uint64 = 6002
	env := newEconomyFlowEnv(t, 8)
	requireUpgradeTable(t)
	env.seedGuild(t, flowGuildID, flowLeader, member)
	env.setFunds(t, flowGuildID, flowUpgradeCostL1+flowUpgradeCostL2)

	first, err := env.logic.UpgradeGuild(clientCtx(flowLeader), &pb.UpgradeGuildRequest{ExpectedLevel: constants.DefaultInitLevel})
	require.NoError(t, err)
	require.Nil(t, first.GetErrorMessage())
	require.Equal(t, constants.DefaultInitLevel+1, first.GetGuild().GetLevel())
	require.Len(t, env.notifier.pushesOf(pb.GuildChangeKind_GUILD_CHANGE_KIND_LEVEL_UP), 1)

	again, err := env.logic.UpgradeGuild(clientCtx(flowLeader), &pb.UpgradeGuildRequest{ExpectedLevel: constants.DefaultInitLevel})

	require.NoError(t, err)
	assert.Nil(t, again.GetErrorMessage(), "expected_level 不符按成功答复,不是错误")
	require.NotNil(t, again.GetGuild(), "无改动也回最新 GuildInfo")
	assert.Equal(t, constants.DefaultInitLevel+1, again.GetGuild().GetLevel())
	assert.Equal(t, flowUpgradeCostL2, again.GetGuild().GetFunds())
	level, funds, _ := env.guildLevelRow(t, flowGuildID)
	assert.Equal(t, constants.DefaultInitLevel+1, level, "不连升两级")
	assert.Equal(t, flowUpgradeCostL2, funds, "不扣第二次")
	assert.Len(t, env.notifier.pushesOf(pb.GuildChangeKind_GUILD_CHANGE_KIND_LEVEL_UP), 1, "无改动不推")
	assert.Equal(t, 1, env.notifier.total())
}

// TestEconomyFlowUpgradeRejectionsStillCarryGuild:资金不足 / 职位不足是业务拒绝(tip + nil error),
// 回包仍带最新 GuildInfo(客户端顺带刷新资金显示);库里等级与资金不动,不推送。
// 普通成员的回包待审数恒 0(管理侧信息不外泄),哪怕本帮确有待审申请。
// 两个帮会共用一个环境(各自独立的 guild / member 行),省一次建库。
func TestEconomyFlowUpgradeRejectionsStillCarryGuild(t *testing.T) {
	const (
		poorGuildID uint64 = 5002
		poorLeader  uint64 = 7001
		member      uint64 = 6002
		applicant   uint64 = 6100
	)
	env := newEconomyFlowEnv(t, 8)
	requireUpgradeTable(t)
	env.seedGuild(t, poorGuildID, poorLeader)
	env.setFunds(t, poorGuildID, flowUpgradeCostL1-1)
	env.seedGuild(t, flowGuildID, flowLeader, member)
	env.setFunds(t, flowGuildID, flowUpgradeCostL1)
	env.seedApplication(t, flowGuildID, applicant)

	t.Run("资金差 1", func(t *testing.T) {
		resp, err := env.logic.UpgradeGuild(clientCtx(poorLeader), &pb.UpgradeGuildRequest{ExpectedLevel: constants.DefaultInitLevel})

		require.NoError(t, err, "资金不足是业务拒绝,不是故障")
		assert.Equal(t, constants.ErrFundsInsufficient, resp.GetErrorMessage().GetId())
		require.NotNil(t, resp.GetGuild(), "资金不足回包仍带 GuildInfo")
		assert.Equal(t, flowUpgradeCostL1-1, resp.GetGuild().GetFunds())
		assert.Equal(t, constants.DefaultInitLevel, resp.GetGuild().GetLevel())
		level, funds, _ := env.guildLevelRow(t, poorGuildID)
		assert.Equal(t, constants.DefaultInitLevel, level)
		assert.Equal(t, flowUpgradeCostL1-1, funds)
	})
	t.Run("普通成员职位不足", func(t *testing.T) {
		resp, err := env.logic.UpgradeGuild(clientCtx(member), &pb.UpgradeGuildRequest{ExpectedLevel: constants.DefaultInitLevel})

		require.NoError(t, err, "职位不足是业务拒绝,不是故障")
		assert.Equal(t, constants.ErrRankTooLow, resp.GetErrorMessage().GetId())
		require.NotNil(t, resp.GetGuild(), "职位不足回包仍带 GuildInfo")
		assert.Equal(t, flowUpgradeCostL1, resp.GetGuild().GetFunds())
		assert.Zero(t, resp.GetGuild().GetPendingApplicationCount(), "普通成员看不到待审数")
		level, funds, _ := env.guildLevelRow(t, flowGuildID)
		assert.Equal(t, constants.DefaultInitLevel, level)
		assert.Equal(t, flowUpgradeCostL1, funds, "资金够也不许普通成员升级")
	})

	assert.Zero(t, env.notifier.total(), "拒绝不推送")
}

// ── 落地恢复摘要 bump(docs/design/landing-recovery-sync-digest.md §3.3)────────

// TestEconomyFlowSyncDigestBumps(§3.3 表第 6、8、9 行):捐献预留提交后 bump 本人 GUILD_ECONOMY,同步投递当场终结时
// 再 bump 一次(推送在同步投递路径上被压掉,摘要不压:回包可能在断线时丢);APPLIED 终结给本人记了帮贡,
// 那一次同时带 GUILD,但资金是帮会级数值,捐献不扇出给其他成员。
// 升级成功给全帮(含操作者本人)bump GUILD,并顺带 GUILD_ECONOMY / GUILD_ACTIVITY(捐献档位、商品、活动的等级门槛都随等级变)。
// 资金在任何 RPC 读帮会之前备足(setFunds 的约定)。
func TestEconomyFlowSyncDigestBumps(t *testing.T) {
	const member uint64 = 6002
	env := newEconomyFlowEnv(t, 8)
	requireUpgradeTable(t)
	env.seedGuild(t, flowGuildID, flowLeader, member)
	env.setFunds(t, flowGuildID, flowUpgradeCostL1)
	env.scene.answer(always(flowApplied))

	resp := env.donate(t, flowLeader, 1)

	require.Equal(t, pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_APPLIED, resp.GetDonation().GetStatus())
	assert.Equal(t, []uint64{flowLeader}, env.syncBumps.playersOf(syncEconomy), "只 bump 捐献者本人")
	assert.Equal(t, 2, env.syncBumps.countOf(syncEconomy), "预留一次 + 同步投递终结一次")
	assert.Equal(t, []uint64{flowLeader}, env.syncBumps.playersOf(syncGuild),
		"只有捐献者本人 bump GUILD(他的帮贡变了);资金是帮会级数值,不扇出给 member")
	assert.Equal(t, 1, env.syncBumps.countOf(syncGuild), "GUILD 只在终结那一次:预留不改 GuildInfo")
	assert.Empty(t, env.notifier.pushesOf(pb.GuildChangeKind_GUILD_CHANGE_KIND_FUNDS_CHANGED), "推送仍按同步投递标记压掉")

	env.syncBumps.reset()
	upgraded, err := env.logic.UpgradeGuild(clientCtx(flowLeader), &pb.UpgradeGuildRequest{ExpectedLevel: constants.DefaultInitLevel})

	require.NoError(t, err)
	require.Nil(t, upgraded.GetErrorMessage())
	assert.Equal(t, []uint64{flowLeader, member}, env.syncBumps.playersOf(syncGuild), "升级给全帮,含操作者本人")
	assert.Equal(t, []uint64{flowLeader, member}, env.syncBumps.playersOf(syncEconomy), "捐献档位与商品的解锁随等级变")
	assert.Equal(t, []uint64{flowLeader, member}, env.syncBumps.playersOf(syncActivity), "活动的等级门槛随等级变")
	assert.Equal(t, 1, env.syncBumps.count(), "三个域搭同一次扇出,不多一批")

	env.syncBumps.reset()
	again, err := env.logic.UpgradeGuild(clientCtx(flowLeader), &pb.UpgradeGuildRequest{ExpectedLevel: constants.DefaultInitLevel})

	require.NoError(t, err)
	require.Nil(t, again.GetErrorMessage())
	assert.Zero(t, env.syncBumps.count(), "expected_level 已过期 = 库里没变,不 bump")
}

// TestEconomyFlowSyncDigestBumpOnBackgroundFinalize:同步投递留下 PENDING,之后由重投循环终结 ——
// 终结发生在 RPC 之外(玩家可能已经下线),摘要靠 OnAssetFinalized 这一处覆盖。
func TestEconomyFlowSyncDigestBumpOnBackgroundFinalize(t *testing.T) {
	env := newEconomyFlowEnv(t, 8)
	env.seedGuild(t, flowGuildID, flowLeader)
	env.scene.answer(always(flowInBattle))

	resp := env.donate(t, flowLeader, 2)

	require.Equal(t, pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_PENDING, resp.GetDonation().GetStatus())
	require.Equal(t, 1, env.syncBumps.countOf(syncEconomy), "预留提交后 bump 一次;PENDING 不是终结")

	env.scene.answer(always(flowApplied))
	env.clock.Advance(5 * time.Second)
	require.Equal(t, 1, env.loop.Tick(context.Background()))

	assert.Equal(t, 2, env.syncBumps.countOf(syncEconomy), "后台终结再 bump 一次")
	assert.Equal(t, []uint64{flowLeader}, env.syncBumps.playersOf(syncEconomy))
}

// TestEconomyFlowShopSyncDigestBumps(§3.3 表第 6、8 行):兑换预留提交即扣帮贡 —— 本人 GuildInfo.members[] 的
// contribution_balance 当场就变了,所以预留这一次 bump 带上 GUILD,不等终结;只给兑换者本人,不扇出。
// 行停在 PENDING(背包满)时没有终结,也就只有这一次。
func TestEconomyFlowShopSyncDigestBumps(t *testing.T) {
	const member uint64 = 6002
	env := newEconomyFlowEnv(t, 8)
	env.seedGuild(t, flowGuildID, flowLeader, member)
	env.setContribution(t, flowGuildID, flowLeader, 1000)
	env.scene.answer(always(flowBagFull))

	resp, err := env.logic.BuyGuildShopGoods(clientCtx(flowLeader), &pb.BuyGuildShopGoodsRequest{GoodsId: 101, Count: 1})

	require.NoError(t, err)
	require.Equal(t, pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_PENDING, resp.GetOrder().GetStatus())
	assert.Equal(t, 1, env.syncBumps.count(), "只有预留这一次:PENDING 不是终结")
	assert.Equal(t, []uint64{flowLeader}, env.syncBumps.playersOf(syncEconomy))
	assert.Equal(t, []uint64{flowLeader}, env.syncBumps.playersOf(syncGuild), "帮贡已扣:只 bump 兑换者本人")
}

// TestManagementFlowSyncDigestBumps(§3.3 表第 6、7 行):管理类写在提交之后给"拉取结果会变的所有人"bump GUILD ——
// 含操作者本人与已不在快照里的当事人(被踢者、离帮者);申请 / 撤回 / 审批给申请人 bump GUILD_APPLICATION,
// 新申请与撤回都给审批人 bump GUILD(待审数在他们的 GuildInfo 里),审批通过给全帮(含新成员)bump GUILD。
func TestManagementFlowSyncDigestBumps(t *testing.T) {
	const (
		officer    uint64 = 6002
		member     uint64 = 6003
		leaver     uint64 = 6004
		applicantA uint64 = 6100
		applicantB uint64 = 6101
	)
	env := newEconomyFlowEnv(t, 1)
	env.seedGuild(t, flowGuildID, flowLeader, officer, member, leaver)
	env.setRole(t, flowGuildID, officer, constants.RoleOfficer)
	// 审批通过会并行查审批人与申请人的归属 zone:换成加锁的 perPlayerZones,单值的 fakeHomeZones 在 -race 下会报竞争。
	// 在任何 RPC 之前替换,之后只读。
	zones := map[uint64]uint32{}
	for _, id := range []uint64{flowLeader, officer, member, leaver, applicantA, applicantB} {
		zones[id] = flowZone
	}
	env.logic.homeZones = &perPlayerZones{zones: zones}

	kicked, err := env.logic.KickGuildMember(clientCtx(flowLeader), &pb.KickGuildMemberRequest{TargetPlayerId: member})
	require.NoError(t, err)
	require.Nil(t, kicked.GetErrorMessage())
	assert.Equal(t, []uint64{flowLeader, officer, member, leaver}, env.syncBumps.playersOf(syncGuild),
		"踢人:快照全体(含操作者)+ 被踢者")

	env.syncBumps.reset()
	left, err := env.logic.LeaveGuild(clientCtx(leaver), &pb.LeaveGuildRequest{})
	require.NoError(t, err)
	require.Nil(t, left.GetErrorMessage())
	assert.Equal(t, []uint64{flowLeader, officer, leaver}, env.syncBumps.playersOf(syncGuild), "离帮:剩余全体 + 离帮者本人")

	env.syncBumps.reset()
	applied, err := env.logic.ApplyJoinGuild(clientCtx(applicantA), &pb.ApplyJoinGuildRequest{GuildId: flowGuildID})
	require.NoError(t, err)
	require.Nil(t, applied.GetErrorMessage())
	assert.Equal(t, []uint64{applicantA}, env.syncBumps.playersOf(syncApplication))
	assert.Equal(t, []uint64{flowLeader, officer}, env.syncBumps.playersOf(syncGuild), "新申请:审批人的待审数变了")

	env.syncBumps.reset()
	cancelled, err := env.logic.CancelGuildApplication(clientCtx(applicantA), &pb.CancelGuildApplicationRequest{GuildId: flowGuildID})
	require.NoError(t, err)
	require.Nil(t, cancelled.GetErrorMessage())
	assert.Equal(t, []uint64{applicantA}, env.syncBumps.playersOf(syncApplication))
	assert.Equal(t, []uint64{flowLeader, officer}, env.syncBumps.playersOf(syncGuild), "撤回:审批人的待审数少了一条")
	assert.Equal(t, []scenepb.SyncDomain{syncApplication}, env.syncBumps.domainsOf(applicantA), "申请人不在帮,GetPlayerGuild 没变")

	applyB, err := env.logic.ApplyJoinGuild(clientCtx(applicantB), &pb.ApplyJoinGuildRequest{GuildId: flowGuildID})
	require.NoError(t, err)
	require.Nil(t, applyB.GetErrorMessage())

	env.syncBumps.reset()
	reviewed, err := env.logic.ReviewGuildApplication(clientCtx(officer),
		&pb.ReviewGuildApplicationRequest{ApplicantPlayerId: applicantB, Approve: true})
	require.NoError(t, err)
	require.Nil(t, reviewed.GetErrorMessage())
	assert.Equal(t, []uint64{applicantB}, env.syncBumps.playersOf(syncApplication))
	assert.Equal(t, []uint64{flowLeader, officer, applicantB}, env.syncBumps.playersOf(syncGuild), "通过:全帮含新成员与审批人")
}

// ── 客户端幂等键(05 顶部「客户端幂等键」覆盖块)──────────────────────

// opCountOf 数某玩家名下的指令行(不分流、不分状态)。
func (e *economyFlowEnv) opCountOf(t *testing.T, playerID uint64) int {
	t.Helper()
	var n int
	require.NoError(t, e.db.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM guild_asset_op WHERE player_id = ?", playerID).Scan(&n))
	return n
}

// clientRequestCount 数去重记录行(全表)。
func (e *economyFlowEnv) clientRequestCount(t *testing.T) int {
	t.Helper()
	var n int
	require.NoError(t, e.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM guild_client_request").Scan(&n))
	return n
}

// flowInvalidParameter:同一个键换了参数时的答复(common 段 kInvalidParameter)。
var flowInvalidParameter = uint32(tablepb.CommonError_kInvalidParameter)

// TestEconomyFlowClientRequestDonateReplaysCurrentView:首次捐献停在 PENDING(战斗中)→ 重投循环终结为 APPLIED →
// 带同一个键重发:回同一个 op_id、指令的**当前**状态 APPLIED,不发号、不再投递,资金与今日次数只记一次。
func TestEconomyFlowClientRequestDonateReplaysCurrentView(t *testing.T) {
	const key uint64 = 0xD0_0001
	env := newEconomyFlowEnv(t, 8)
	env.seedGuild(t, flowGuildID, flowLeader)
	env.scene.answer(always(flowInBattle))
	req := &pb.DonateToGuildRequest{DonateId: 1, ClientRequestId: key}

	first, err := env.logic.DonateToGuild(clientCtx(flowLeader), req)
	require.NoError(t, err)
	opID := first.GetDonation().GetOpId()
	require.NotZero(t, opID)
	require.Equal(t, pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_PENDING, first.GetDonation().GetStatus())

	env.scene.answer(always(flowApplied))
	env.clock.Advance(5 * time.Second) // 越过退避(attempts=0 时 1× 重投基准)
	require.Equal(t, 1, env.loop.Tick(context.Background()))
	callsBefore := env.scene.totalCalls()

	retry, err := env.logic.DonateToGuild(clientCtx(flowLeader), req)
	require.NoError(t, err)
	assert.Nil(t, retry.GetErrorMessage())
	donation := retry.GetDonation()
	require.NotNil(t, donation, "重发必须带订单视图:客户端据此认定这一笔已经发生")
	assert.Equal(t, opID, donation.GetOpId(), "重发回首次那笔指令")
	assert.Equal(t, pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_APPLIED, donation.GetStatus(), "视图是指令的当前状态")
	assert.Equal(t, uint32(1), donation.GetDonateId())
	assert.Equal(t, uint64(10000), donation.GetCostAmount(), "数额从行上的 payload 解出,与首次回包一致")
	assert.Equal(t, first.GetDonation().GetCreatedMs(), donation.GetCreatedMs())
	require.NotNil(t, retry.GetGuild(), "本人仍在帮:GuildInfo 必填")
	assert.Equal(t, uint64(1000), retry.GetGuild().GetFunds())

	assert.Equal(t, callsBefore, env.scene.totalCalls(), "重发不再投递")
	assert.Equal(t, 1, env.opCountOf(t, flowLeader), "只有一行指令")
	assert.Equal(t, 1, env.clientRequestCount(t))
	assert.Equal(t, uint64(1000), env.funds(t, flowGuildID), "资金只记一次")
	dayKey := gameday.DayKey(time.UnixMilli(int64(first.GetDonation().GetCreatedMs())))
	assert.Equal(t, uint32(1), env.usedCount(t, flowLeader, pb.GuildDailyCounterKind_GUILD_DAILY_COUNTER_KIND_DONATE, 1, dayKey),
		"今日次数只占一次")
}

// TestEconomyFlowClientRequestShopDebitsOnce:帮贡恰好够一份,兑换 APPLIED 后带同一个键重发 —— 不能回"帮贡不足",
// 也不能再扣:回同一单、余额 0。count=0(老客户端不填份数)与 count=1 是同一个请求。
func TestEconomyFlowClientRequestShopDebitsOnce(t *testing.T) {
	const key uint64 = 0x5E_0001
	env := newEconomyFlowEnv(t, 8)
	env.seedGuild(t, flowGuildID, flowLeader)
	env.setContribution(t, flowGuildID, flowLeader, 30) // GuildShop 101 每份 30 帮贡(见 TestEconomyFlowShopRejectedRefundsContributionAndLimit)
	env.scene.answer(always(flowApplied))
	req := &pb.BuyGuildShopGoodsRequest{GoodsId: 101, Count: 1, ClientRequestId: key}

	first, err := env.logic.BuyGuildShopGoods(clientCtx(flowLeader), req)
	require.NoError(t, err)
	require.Nil(t, first.GetErrorMessage())
	opID := first.GetOrder().GetOpId()
	require.Equal(t, pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_APPLIED, first.GetOrder().GetStatus())
	require.Zero(t, first.GetContributionBalance())

	for name, retryReq := range map[string]*pb.BuyGuildShopGoodsRequest{
		"同一个请求原样重发":      req,
		"count=0 视为 1 份": {GoodsId: 101, ClientRequestId: key},
	} {
		retry, err := env.logic.BuyGuildShopGoods(clientCtx(flowLeader), retryReq)
		require.NoError(t, err, name)
		assert.Nil(t, retry.GetErrorMessage(), "%s:不能回帮贡不足", name)
		require.NotNil(t, retry.GetOrder(), name)
		assert.Equal(t, opID, retry.GetOrder().GetOpId(), name)
		assert.Equal(t, pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_APPLIED, retry.GetOrder().GetStatus(), name)
		assert.Equal(t, uint32(1), retry.GetOrder().GetCount(), name)
		assert.Equal(t, uint64(30), retry.GetOrder().GetCostContribution(), name)
		assert.Zero(t, retry.GetContributionBalance(), "%s:余额直读 MySQL", name)
	}

	_, balance, found := env.contribution(t, flowGuildID, flowLeader)
	require.True(t, found)
	assert.Zero(t, balance, "帮贡只扣一次")
	assert.Equal(t, 1, env.opCountOf(t, flowLeader))
	assert.Len(t, env.scene.callsFor(opID), 1, "重发不再投递")
}

// TestEconomyFlowClientRequestConflictRefusedWithoutWriting:同一个键换了捐献选项、拿去兑换、换了份数 → kInvalidParameter,
// 不带订单视图、一行都不写、帮贡不扣;首次那笔不受影响。
func TestEconomyFlowClientRequestConflictRefusedWithoutWriting(t *testing.T) {
	const (
		donateKey uint64 = 0xC0_0001
		shopKey   uint64 = 0xC0_0002
	)
	env := newEconomyFlowEnv(t, 8)
	env.seedGuild(t, flowGuildID, flowLeader)
	env.setContribution(t, flowGuildID, flowLeader, 1000)
	env.scene.answer(always(flowApplied))
	ctx := clientCtx(flowLeader)

	donated, err := env.logic.DonateToGuild(ctx, &pb.DonateToGuildRequest{DonateId: 1, ClientRequestId: donateKey})
	require.NoError(t, err)
	require.NotNil(t, donated.GetDonation())
	bought, err := env.logic.BuyGuildShopGoods(ctx, &pb.BuyGuildShopGoodsRequest{GoodsId: 101, Count: 1, ClientRequestId: shopKey})
	require.NoError(t, err)
	require.NotNil(t, bought.GetOrder())
	_, balanceBefore, _ := env.contribution(t, flowGuildID, flowLeader)

	type outcome struct {
		tipID     uint32
		wroteView bool
	}
	for name, call := range map[string]func() (outcome, error){
		"换了捐献选项": func() (outcome, error) {
			resp, err := env.logic.DonateToGuild(ctx, &pb.DonateToGuildRequest{DonateId: 2, ClientRequestId: donateKey})
			return outcome{resp.GetErrorMessage().GetId(), resp.GetDonation() != nil}, err
		},
		"捐献的键拿去兑换": func() (outcome, error) {
			resp, err := env.logic.BuyGuildShopGoods(ctx, &pb.BuyGuildShopGoodsRequest{GoodsId: 101, Count: 1, ClientRequestId: donateKey})
			return outcome{resp.GetErrorMessage().GetId(), resp.GetOrder() != nil}, err
		},
		"兑换换了份数": func() (outcome, error) {
			resp, err := env.logic.BuyGuildShopGoods(ctx, &pb.BuyGuildShopGoodsRequest{GoodsId: 101, Count: 2, ClientRequestId: shopKey})
			return outcome{resp.GetErrorMessage().GetId(), resp.GetOrder() != nil}, err
		},
	} {
		got, err := call()
		require.NoError(t, err, name)
		assert.Equal(t, flowInvalidParameter, got.tipID, name)
		assert.False(t, got.wroteView, "%s:冲突不带订单视图,客户端不能把它当成第二笔已成功", name)
	}

	assert.Equal(t, 2, env.opCountOf(t, flowLeader), "冲突一行都不写")
	assert.Equal(t, 2, env.clientRequestCount(t))
	_, balanceAfter, _ := env.contribution(t, flowGuildID, flowLeader)
	assert.Equal(t, balanceBefore, balanceAfter, "冲突不扣帮贡")
}

// TestEconomyFlowClientRequestZeroKeyIsNotDeduplicated:旧客户端(键 0)两次请求就是两笔 —— 与加字段之前完全相同,也不写去重记录。
func TestEconomyFlowClientRequestZeroKeyIsNotDeduplicated(t *testing.T) {
	env := newEconomyFlowEnv(t, 8)
	env.seedGuild(t, flowGuildID, flowLeader)
	env.scene.answer(always(flowApplied))

	first := env.donate(t, flowLeader, 1)
	second := env.donate(t, flowLeader, 1)

	assert.NotEqual(t, first.GetDonation().GetOpId(), second.GetDonation().GetOpId())
	assert.Equal(t, 2, env.opCountOf(t, flowLeader))
	assert.Equal(t, uint64(2000), env.funds(t, flowGuildID))
	assert.Zero(t, env.clientRequestCount(t), "键 0 不写去重记录")
}

// TestEconomyFlowClientRequestReplayAfterLeavingGuild:首次捐献已 APPLIED,玩家随后离帮;带同一个键重发仍回首次那笔
// (第 1 道在前置之前查,不回"未入帮"),GuildInfo 不填(本人已不在该帮)。
func TestEconomyFlowClientRequestReplayAfterLeavingGuild(t *testing.T) {
	const (
		member uint64 = 6002
		key    uint64 = 0x1E_0001
	)
	env := newEconomyFlowEnv(t, 8)
	env.seedGuild(t, flowGuildID, flowLeader, member)
	env.scene.answer(always(flowApplied))
	req := &pb.DonateToGuildRequest{DonateId: 1, ClientRequestId: key}

	first, err := env.logic.DonateToGuild(clientCtx(member), req)
	require.NoError(t, err)
	require.Equal(t, pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_APPLIED, first.GetDonation().GetStatus())

	left, err := env.logic.LeaveGuild(clientCtx(member), &pb.LeaveGuildRequest{})
	require.NoError(t, err)
	require.Nil(t, left.GetErrorMessage())

	retry, err := env.logic.DonateToGuild(clientCtx(member), req)
	require.NoError(t, err)
	assert.Nil(t, retry.GetErrorMessage(), "那笔捐献确实发生了,不能回未入帮")
	assert.Equal(t, first.GetDonation().GetOpId(), retry.GetDonation().GetOpId())
	assert.Equal(t, pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_APPLIED, retry.GetDonation().GetStatus())
	assert.Nil(t, retry.GetGuild(), "本人已不在该帮:GuildInfo 不填")
	assert.Equal(t, 1, env.opCountOf(t, member))
}

// TestEconomyFlowClientRequestRejectionIsNotRecorded:帮贡不足被拒的兑换不留去重记录(被拒的请求没有副作用);
// 补足帮贡之后带同一个键重发会真正执行一次。
func TestEconomyFlowClientRequestRejectionIsNotRecorded(t *testing.T) {
	const key uint64 = 0x2E_0001
	env := newEconomyFlowEnv(t, 8)
	env.seedGuild(t, flowGuildID, flowLeader)
	env.scene.answer(always(flowApplied))
	req := &pb.BuyGuildShopGoodsRequest{GoodsId: 101, Count: 1, ClientRequestId: key}

	rejected, err := env.logic.BuyGuildShopGoods(clientCtx(flowLeader), req)
	require.NoError(t, err)
	assert.Equal(t, constants.ErrContributionInsufficient, rejected.GetErrorMessage().GetId())
	assert.Nil(t, rejected.GetOrder())
	assert.Zero(t, env.clientRequestCount(t), "被拒的请求不留去重记录")

	// 缓存快照里帮贡仍是 0:预判会直读 MySQL 确认(contributionPrecheck),事务按锁内余额判,所以直接改库即可。
	env.setContribution(t, flowGuildID, flowLeader, 30)
	retry, err := env.logic.BuyGuildShopGoods(clientCtx(flowLeader), req)
	require.NoError(t, err)
	require.Nil(t, retry.GetErrorMessage())
	require.NotNil(t, retry.GetOrder())
	assert.Equal(t, pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_APPLIED, retry.GetOrder().GetStatus())
	assert.Equal(t, 1, env.opCountOf(t, flowLeader))
	assert.Equal(t, 1, env.clientRequestCount(t))
}

// TestEconomyFlowClientRequestConcurrentRetriesShareOneOp:同一个键 6 路并发(首次仍在途时客户端超时重发)→ 全部回同一个 op_id,
// 只写一行、资金只记一次、只投递一次。输家可能在第 1 道、事务内(第 2 道)或被拒之后(第 3 道)拿到首次结果,
// 三条路都必须收敛到同一笔。goroutine 里只收集结果,断言全部在 Wait 之后的主 goroutine 里做。
func TestEconomyFlowClientRequestConcurrentRetriesShareOneOp(t *testing.T) {
	const (
		key     uint64 = 0x3E_0001
		workers        = 6
	)
	env := newEconomyFlowEnv(t, 8)
	env.seedGuild(t, flowGuildID, flowLeader)
	env.scene.answer(always(flowApplied))
	req := &pb.DonateToGuildRequest{DonateId: 1, ClientRequestId: key}

	resps := make([]*pb.DonateToGuildResponse, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resps[i], errs[i] = env.logic.DonateToGuild(clientCtx(flowLeader), req)
		}()
	}
	wg.Wait()

	var opID uint64
	for i := range workers {
		require.NoError(t, errs[i], "第 %d 路", i)
		require.NotNil(t, resps[i].GetDonation(), "第 %d 路必须拿到订单视图(tip=%v)", i, resps[i].GetErrorMessage())
		if opID == 0 {
			opID = resps[i].GetDonation().GetOpId()
		}
		assert.Equal(t, opID, resps[i].GetDonation().GetOpId(), "第 %d 路", i)
	}
	assert.Equal(t, 1, env.opCountOf(t, flowLeader), "同一个键只写一行")
	assert.Equal(t, uint64(1000), env.funds(t, flowGuildID), "资金只记一次")
	assert.Len(t, env.scene.callsFor(opID), 1, "只投递一次")
}
