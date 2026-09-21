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
// (见该函数注释)。因此不带 `-run` 整包跑 `-tags integration` 也不会污染同进程的其它用例。
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
	"path/filepath"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	mysqldriver "github.com/go-sql-driver/mysql"
	goredis "github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"guild/internal/constants"
	"guild/internal/data"
	assetpb "proto/common/asset"
	pb "proto/guild"
	"schemamigrate"
	"shared/assetop"
	"shared/gameday"
	"shared/generated/table"
)

// ── 建表 / 删表(最小助手)────────────────────────────────────

// economyFlowDropTables:本服务全部表 + schemamigrate 台账,逆锁序。新增表时同步追加
// (resetEconomyFlowSchema 按 len(data.Tables())+1 守数量,漏加即红)。
var economyFlowDropTables = []string{
	"guild_daily_counter", "guild_asset_op", "guild_player_op_seq",
	"guild_application", "guild_member", "guild_player_state", "guild", "schema_migrations",
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
	cfg, err := mysqldriver.ParseDSN(rawDSN)
	require.NoError(t, err, "解析 GUILD_TEST_MYSQL_DSN")
	cfg.DBName = ""
	admin, err := sql.Open("mysql", cfg.FormatDSN())
	require.NoError(t, err)
	t.Cleanup(func() { admin.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, admin.PingContext(ctx))
	name := fmt.Sprintf("guild_it_%d_%d", os.Getpid(), economyFlowDBCounter.Add(1))
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE `"+name+"` DEFAULT CHARACTER SET utf8mb4"); err != nil {
		t.Fatalf("用 GUILD_TEST_MYSQL_DSN 的账号建一次性库 %s 失败(该账号需要 CREATE / DROP DATABASE 权限): %v", name, err)
	}
	// Cleanup 后进先出:业务连接先关(下面注册)→ DROP → admin 关(上面注册)。
	t.Cleanup(func() {
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dropCancel()
		if _, err := admin.ExecContext(dropCtx, "DROP DATABASE IF EXISTS `"+name+"`"); err != nil {
			t.Errorf("DROP DATABASE %s 失败(请手工清理): %v", name, err)
		}
	})

	// 与生产同一条 DSN 改写:innodb_lock_wait_timeout=1 且 ClientFoundRows=false ——
	// 计数器带上限 upsert 的 RowsAffected(1 新插 / 2 累加 / 0 达上限)依赖后者。
	cfg.DBName = name
	dsn, err := data.WithLockWaitTimeout(cfg.FormatDSN())
	require.NoError(t, err)
	db, err := sql.Open("mysql", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	require.NoError(t, db.PingContext(ctx))
	resetEconomyFlowSchema(t, ctx, db)
	return db
}

// loadEconomyFlowTables 加载真实导表产物(仓库根 generated/tables),并跑一遍启动校验:
// 用例的数值(捐献 1 = 10000 银两 / 帮贡 10 / 资金 1000;商品 101 每份 30 帮贡、每日限 10)以它为准。
//
// 隔离:各 *TableManagerInstance 是包级变量,生产代码每次都经它现查、不缓存指针。所以这里换上新建的
// 管理器再加载,t.Cleanup 换回原实例,还原"未加载配表"的状态。换指针是普通赋值而非原子操作,成立的前提是:
// 本包用例都不调 t.Parallel,且本文件的用例结束时没有仍在读配表的 goroutine(Tick / ProcessOne 同步返回,
// 推送替身同步记录)。日后给本包用例加 t.Parallel 之前,先改掉这里。
func loadEconomyFlowTables(t *testing.T) {
	t.Helper()
	origRule := table.GuildRuleTableManagerInstance
	origLevel := table.GuildLevelTableManagerInstance
	origDonate := table.GuildDonateTableManagerInstance
	origShop := table.GuildShopTableManagerInstance
	origItem := table.ItemTableManagerInstance
	t.Cleanup(func() {
		table.GuildRuleTableManagerInstance = origRule
		table.GuildLevelTableManagerInstance = origLevel
		table.GuildDonateTableManagerInstance = origDonate
		table.GuildShopTableManagerInstance = origShop
		table.ItemTableManagerInstance = origItem
	})
	table.GuildRuleTableManagerInstance = table.NewGuildRuleTableManager()
	table.GuildLevelTableManagerInstance = table.NewGuildLevelTableManager()
	table.GuildDonateTableManagerInstance = table.NewGuildDonateTableManager()
	table.GuildShopTableManagerInstance = table.NewGuildShopTableManager()
	table.ItemTableManagerInstance = table.NewItemTableManager()

	// 下面的方法值在换指针**之后**求值,绑定的是新实例。
	dir := filepath.Join("..", "..", "..", "..", "generated", "tables")
	loaders := []struct {
		name string
		load func(configDir string, useBinary bool) error
	}{
		{"GuildRule", table.GuildRuleTableManagerInstance.Load},
		{"GuildLevel", table.GuildLevelTableManagerInstance.Load},
		{"GuildDonate", table.GuildDonateTableManagerInstance.Load},
		{"GuildShop", table.GuildShopTableManagerInstance.Load},
		{"Item", table.ItemTableManagerInstance.Load},
	}
	for _, l := range loaders {
		require.NoError(t, l.load(dir, false), l.name)
	}
	require.NoError(t, ValidateEconomyTables())
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
	return s.respond(rpc, req), nil
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

// scene 的三种典型答复:已应用且落盘 / 战斗中暂不结算 / 永久拒绝且落盘。
var (
	flowApplied  = assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_APPLIED, Durable: true}
	flowInBattle = assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_RETRY, Reason: assetop.ReasonInBattle}
	flowBlocked  = assetop.Result{Outcome: assetpb.AssetOpOutcome_ASSET_OP_OUTCOME_REJECTED, Reason: assetop.ReasonBlocked, Durable: true}
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
	loopCfg  assetop.LoopConfig
	loop     *assetop.Loop
	logic    *GuildLogic
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

	notifier := &recordingNotifier{}
	minter := &sequenceMinter{}
	minter.next.Store(900_000)
	l := NewGuildLogic(repo, nil, nil, nil, &fakeHomeZones{zone: flowZone},
		WithNotifier(notifier),
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
		db:       db,
		store:    store,
		scene:    scene,
		clock:    clock,
		notifier: notifier,
		loopCfg:  cfg,
		loop:     loop,
		logic:    l,
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
