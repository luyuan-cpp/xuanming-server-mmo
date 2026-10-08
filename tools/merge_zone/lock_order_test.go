package main

// 2026-09-21 死锁审计 #17 的纯单测(不连库):RC 会话 DSN、锁冲突判定、有界重试、点更新 SQL 形状、
// 围栏保留文案。执行计划与真实交错(整区 UPDATE 与 DisbandGuild 反序成环)在 integration_test.go 的
// TestIT_ZonePointUpdatesArePrimaryKeyPointLookups / TestIT_MigrateGuildZone_ConcurrentDisbandDoesNotDeadlock。
//
// 2026-09-28(player-storage-placement.md §12 A15)起每个 id 一个显式短事务:SELECT … FOR UPDATE →
// 带 zone 复核的 UPDATE → COMMIT。本文件末尾用内存替身(fakeZoneTable)钉住事务的口径:升序、一个 id
// 一个事务、先锁再改、不在 from 就只回滚、锁冲突重试整个事务、硬错误立即停且回滚。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

func TestReadCommittedDSN_SetsIsolationAndKeepsOtherParams(t *testing.T) {
	// 与集成测试 / main.go 默认值同形:密码带 #,带 parseTime / loc / multiStatements / charset。
	in := "root:Mmorpg#2026db@tcp(127.0.0.1:3306)/merge_zone_it_db?charset=utf8mb4&parseTime=true&loc=Local&multiStatements=true"
	out, err := readCommittedDSN(in)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := mysql.ParseDSN(out)
	if err != nil {
		t.Fatalf("the rewritten DSN no longer parses: %v", err)
	}
	if got := cfg.Params[mysqlIsolationParam]; got != "'READ-COMMITTED'" {
		t.Errorf("%s = %q, want 'READ-COMMITTED' (quoted: the value is spliced into SET verbatim)", mysqlIsolationParam, got)
	}
	if cfg.User != "root" || cfg.Passwd != "Mmorpg#2026db" || cfg.Addr != "127.0.0.1:3306" || cfg.DBName != "merge_zone_it_db" {
		t.Errorf("connection target changed: user=%q addr=%q db=%q", cfg.User, cfg.Addr, cfg.DBName)
	}
	if !cfg.ParseTime || !cfg.MultiStatements || cfg.Loc != time.Local {
		t.Errorf("existing params lost: parseTime=%v multiStatements=%v loc=%v", cfg.ParseTime, cfg.MultiStatements, cfg.Loc)
	}
	// 幂等:再过一遍结果不变。
	again, err := readCommittedDSN(out)
	if err != nil || again != out {
		t.Errorf("not idempotent: %q → %q (err=%v)", out, again, err)
	}
}

func TestReadCommittedDSN_OverridesExplicitIsolationAndDropsLegacyName(t *testing.T) {
	// 运维显式写了 RR,或写了旧名 tx_isolation:一律改成 RC,旧名删掉 —— 两个名字同时在时驱动按 map
	// 顺序拼进同一条 SET,最后生效哪个不确定。
	in := "u:p@tcp(db:3306)/mmorpg?transaction_isolation=%27REPEATABLE-READ%27&tx_isolation=%27SERIALIZABLE%27"
	out, err := readCommittedDSN(in)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := mysql.ParseDSN(out)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Params[mysqlIsolationParam]; got != mysqlReadCommitted {
		t.Errorf("%s = %q, want %s", mysqlIsolationParam, got, mysqlReadCommitted)
	}
	if _, ok := cfg.Params[mysqlLegacyIsolationParam]; ok {
		t.Errorf("legacy %s survived: %q", mysqlLegacyIsolationParam, out)
	}
}

func TestReadCommittedDSN_RejectsMalformedDSN(t *testing.T) {
	if _, err := readCommittedDSN("not a dsn"); err == nil {
		t.Fatal("a malformed -mysql-dsn must be refused before opening anything")
	}
}

func TestIsRetryableLockConflict(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{&mysql.MySQLError{Number: 1213}, true},
		{&mysql.MySQLError{Number: 1205}, true},
		{&mysql.MySQLError{Number: 9007}, true},
		{fmt.Errorf("wrapped: %w", &mysql.MySQLError{Number: 1213}), true},
		{&mysql.MySQLError{Number: 1062}, false}, // 重复键不是锁冲突
		{&mysql.MySQLError{Number: 1146}, false}, // 表不存在:重试不会好
		{errors.New("driver: bad connection"), false},
		{context.DeadlineExceeded, false},
		{nil, false},
	} {
		if got := isRetryableLockConflict(c.err); got != c.want {
			t.Errorf("isRetryableLockConflict(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

// recordingSleep 记录每次退避时长,不真的等(单测不依赖墙钟)。
type recordingSleep struct{ waits []time.Duration }

func (r *recordingSleep) sleep(_ context.Context, d time.Duration) error {
	r.waits = append(r.waits, d)
	return nil
}

func TestLockRetryPolicy_RetriesLockConflictsWithCappedBackoff(t *testing.T) {
	rec := &recordingSleep{}
	p := lockRetryPolicy{attempts: 5, baseBackoff: 100 * time.Millisecond, maxBackoff: 300 * time.Millisecond, sleep: rec.sleep}
	calls := 0
	err := p.do(context.Background(), "guild guild_id=12", func() error {
		calls++
		if calls < 4 {
			return &mysql.MySQLError{Number: 1213}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("should succeed on the 4th attempt: %v", err)
	}
	if calls != 4 {
		t.Errorf("calls = %d, want 4", calls)
	}
	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 300 * time.Millisecond}
	if fmt.Sprint(rec.waits) != fmt.Sprint(want) {
		t.Errorf("backoff = %v, want %v (doubling, capped at maxBackoff)", rec.waits, want)
	}
}

func TestLockRetryPolicy_GivesUpAfterAttemptsAndKeepsTheCause(t *testing.T) {
	rec := &recordingSleep{}
	p := lockRetryPolicy{attempts: 3, baseBackoff: time.Millisecond, maxBackoff: time.Second, sleep: rec.sleep}
	calls := 0
	err := p.do(context.Background(), "guild guild_id=12", func() error {
		calls++
		return &mysql.MySQLError{Number: 1205}
	})
	if calls != 3 {
		t.Errorf("calls = %d, want exactly 3 (bounded)", calls)
	}
	if len(rec.waits) != 2 {
		t.Errorf("slept %d times, want 2 (no sleep after the last attempt)", len(rec.waits))
	}
	var me *mysql.MySQLError
	if err == nil || !errors.As(err, &me) || me.Number != 1205 {
		t.Fatalf("the final error must wrap the last lock error: %v", err)
	}
	if !strings.Contains(err.Error(), "guild guild_id=12") {
		t.Errorf("the final error must name what was being retried: %v", err)
	}
}

func TestLockRetryPolicy_DoesNotRetryOtherErrors(t *testing.T) {
	rec := &recordingSleep{}
	p := lockRetryPolicy{attempts: 5, baseBackoff: time.Millisecond, maxBackoff: time.Second, sleep: rec.sleep}
	calls := 0
	boom := &mysql.MySQLError{Number: 1146}
	err := p.do(context.Background(), "x", func() error {
		calls++
		return boom
	})
	if calls != 1 || len(rec.waits) != 0 || !errors.Is(err, boom) {
		t.Errorf("a non-lock error must be returned as-is on the first attempt: calls=%d waits=%v err=%v", calls, rec.waits, err)
	}
}

func TestLockRetryPolicy_StopsWhenContextIsDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	p := lockRetryPolicy{attempts: 5, baseBackoff: time.Millisecond, maxBackoff: time.Second,
		sleep: func(context.Context, time.Duration) error {
			cancel() // 退避期间维护窗口到期
			return context.Canceled
		}}
	err := p.do(ctx, "x", func() error {
		calls++
		return &mysql.MySQLError{Number: 1213}
	})
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (cancellation during backoff stops the loop)", calls)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error must carry the cancellation: %v", err)
	}
	var me *mysql.MySQLError
	if !errors.As(err, &me) || me.Number != 1213 {
		t.Errorf("error must still carry the lock conflict that triggered the retry: %v", err)
	}
	// 进来时 ctx 已取消:一次都不执行。
	calls = 0
	if err := p.do(ctx, "x", func() error { calls++; return nil }); calls != 0 || !errors.Is(err, context.Canceled) {
		t.Errorf("an already-cancelled ctx must not run op: calls=%d err=%v", calls, err)
	}
}

func TestZoneRewriteRetry_IsBounded(t *testing.T) {
	// 生产参数:次数有上限、退避封顶,且全部退避加起来远小于 -timeout 默认 2h。
	p := zoneRewriteRetry
	if p.attempts < 2 || p.attempts > 10 {
		t.Fatalf("attempts = %d, want a small bounded number", p.attempts)
	}
	var total time.Duration
	for i := 0; i < p.attempts-1; i++ {
		total += p.backoff(i)
	}
	if total <= 0 || total > time.Minute {
		t.Errorf("total backoff = %s, want (0, 1m]", total)
	}
}

func TestZonePointUpdateSQL_IsAPrimaryKeyEqualityWithZoneGuard(t *testing.T) {
	// 形状即锁序:WHERE 首项是完整主键等值(const 访问,先锁主键),zone 只作旧值守卫(重跑幂等)。
	// 改回 `WHERE zone = ?` 整区或 `zone = ? AND pk IN (...)`,优化器就可能沿 zone 二级索引先锁二级项(审计 #17)。
	got := zonePointUpdateSQL(guildQualified(defaultGuildSchema, guildTable), guildPKColumn, guildZoneColumn)
	if want := "UPDATE mmorpg_guild.guild SET zone_id = ? WHERE guild_id = ? AND zone_id = ?"; got != want {
		t.Errorf("guild point update = %q, want %q", got, want)
	}
	got = zonePointUpdateSQL(tradeListingQualified(defaultTradeSchema), tradeListingPKColumn, tradeMarketZoneColumn)
	if want := "UPDATE mmorpg_trade.trade_listing SET market_zone = ? WHERE listing_id = ? AND market_zone = ?"; got != want {
		t.Errorf("trade point update = %q, want %q", got, want)
	}
	if strings.Contains(got, " IN (") {
		t.Error("the point update must not use IN lists")
	}
}

func TestZoneLockSelectSQL_LocksOnlyThePrimaryKeyRow(t *testing.T) {
	// 单行事务第一句:WHERE 只有完整主键等值 + FOR UPDATE —— 执行期就拿到主键锁(TiDB 悲观模式同样),
	// 不带 zone 条件,也就不会让优化器去看 zone 二级索引。zone 的复核交给随后那条带旧值守卫的 UPDATE。
	for _, c := range []struct{ got, want string }{
		{zoneLockSelectSQL(guildQualified(defaultGuildSchema, guildTable), guildPKColumn, guildZoneColumn),
			"SELECT zone_id FROM mmorpg_guild.guild WHERE guild_id = ? FOR UPDATE"},
		{zoneLockSelectSQL(tradeListingQualified(defaultTradeSchema), tradeListingPKColumn, tradeMarketZoneColumn),
			"SELECT market_zone FROM mmorpg_trade.trade_listing WHERE listing_id = ? FOR UPDATE"},
	} {
		if c.got != c.want {
			t.Errorf("lock select = %q, want %q", c.got, c.want)
		}
		if strings.Contains(c.got, " IN (") || strings.Count(c.got, "?") != 1 {
			t.Errorf("the lock select must be a single primary-key equality: %q", c.got)
		}
	}
}

// fakeZoneTable 是 zoneRowTx 的内存替身:一张「主键 → zone」表、按次序注入的错误与调用日志。
// 未提交的改写只存在事务里,提交才落表 —— 与真库一样,回滚的事务什么也没留下。
type fakeZoneTable struct {
	zones      map[uint64]uint32
	lockErrs   map[uint64][]error // lockZone 第 n 次调用返回第 n 个错误(nil = 正常)
	updateErrs map[uint64][]error
	log        []string
}

func (f *fakeZoneTable) begin(context.Context) (zoneRowTx, error) {
	f.log = append(f.log, "begin")
	return &fakeZoneTx{f: f}, nil
}

// popErr 取出 id 的下一个注入错误。
func popErr(m map[uint64][]error, id uint64) error {
	errs := m[id]
	if len(errs) == 0 {
		return nil
	}
	m[id] = errs[1:]
	return errs[0]
}

type fakeZoneTx struct {
	f       *fakeZoneTable
	pending map[uint64]uint32
}

func (t *fakeZoneTx) lockZone(_ context.Context, id uint64) (uint32, bool, error) {
	t.f.log = append(t.f.log, fmt.Sprintf("lock %d", id))
	if err := popErr(t.f.lockErrs, id); err != nil {
		return 0, false, err
	}
	zone, ok := t.f.zones[id]
	return zone, ok, nil
}

func (t *fakeZoneTx) rewriteZone(_ context.Context, id uint64, from, to uint32) (int64, error) {
	t.f.log = append(t.f.log, fmt.Sprintf("update %d", id))
	if err := popErr(t.f.updateErrs, id); err != nil {
		return 0, err
	}
	if zone, ok := t.f.zones[id]; !ok || zone != from {
		return 0, nil
	}
	if t.pending == nil {
		t.pending = map[uint64]uint32{}
	}
	t.pending[id] = to
	return 1, nil
}

func (t *fakeZoneTx) commit() error {
	t.f.log = append(t.f.log, "commit")
	for id, zone := range t.pending {
		t.f.zones[id] = zone
	}
	return nil
}

func (t *fakeZoneTx) rollback() error {
	t.f.log = append(t.f.log, "rollback")
	t.pending = nil
	return nil
}

func newFakeZoneTable(zones map[uint64]uint32) *fakeZoneTable {
	return &fakeZoneTable{zones: zones, lockErrs: map[uint64][]error{}, updateErrs: map[uint64][]error{}}
}

var testZoneTarget = zoneRewriteTarget{table: "mmorpg_guild.guild", pkCol: guildPKColumn, zoneCol: guildZoneColumn}

func TestRewriteZoneRows_OneTransactionPerIDInAscendingOrder(t *testing.T) {
	// 11 / 12 / 13 在源区;21 已在目标区(续跑或原住民);99 已被删除。输入故意乱序且有重复。
	f := newFakeZoneTable(map[uint64]uint32{11: 901, 12: 901, 13: 901, 21: 902})
	rec := &recordingSleep{}
	retry := lockRetryPolicy{attempts: 3, baseBackoff: time.Millisecond, maxBackoff: time.Second, sleep: rec.sleep}
	n, err := rewriteZoneRows(context.Background(), f.begin, retry, testZoneTarget, []uint64{13, 99, 11, 21, 12, 11}, 901, 902)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("rewrote %d rows, want 3", n)
	}
	want := []string{
		"begin", "lock 11", "update 11", "commit",
		"begin", "lock 12", "update 12", "commit",
		"begin", "lock 13", "update 13", "commit",
		"begin", "lock 21", "rollback", // 已不在 from:只锁、不写、回滚放锁
		"begin", "lock 99", "rollback", // 行已不存在:同上
	}
	if fmt.Sprint(f.log) != fmt.Sprint(want) {
		t.Errorf("transaction log =\n%v\nwant\n%v", f.log, want)
	}
	for _, id := range []uint64{11, 12, 13, 21} {
		if f.zones[id] != 902 {
			t.Errorf("guild %d zone = %d, want 902", id, f.zones[id])
		}
	}
	if len(rec.waits) != 0 {
		t.Errorf("no lock conflict, but slept %v", rec.waits)
	}
}

func TestRewriteZoneRows_RetriesTheWholeTransactionOnLockConflict(t *testing.T) {
	f := newFakeZoneTable(map[uint64]uint32{11: 901, 12: 901})
	// 12 的 UPDATE 先撞 1205(锁等待超时只回滚语句、不回滚事务),再在锁读上撞 1213,第三次才成。
	f.updateErrs[12] = []error{&mysql.MySQLError{Number: 1205}}
	f.lockErrs[12] = []error{nil, &mysql.MySQLError{Number: 1213}}
	rec := &recordingSleep{}
	retry := lockRetryPolicy{attempts: 5, baseBackoff: 10 * time.Millisecond, maxBackoff: time.Second, sleep: rec.sleep}
	n, err := rewriteZoneRows(context.Background(), f.begin, retry, testZoneTarget, []uint64{11, 12}, 901, 902)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("rewrote %d rows, want 2", n)
	}
	want := []string{
		"begin", "lock 11", "update 11", "commit",
		"begin", "lock 12", "update 12", "rollback", // 1205:整个事务回滚,不带着锁重试
		"begin", "lock 12", "rollback", // 1213
		"begin", "lock 12", "update 12", "commit",
	}
	if fmt.Sprint(f.log) != fmt.Sprint(want) {
		t.Errorf("transaction log =\n%v\nwant\n%v", f.log, want)
	}
	if len(rec.waits) != 2 {
		t.Errorf("slept %d times, want 2 (one per retried conflict)", len(rec.waits))
	}
}

func TestRewriteZoneRows_HardErrorStopsAndReportsProgress(t *testing.T) {
	f := newFakeZoneTable(map[uint64]uint32{11: 901, 12: 901, 13: 901})
	f.updateErrs[12] = []error{&mysql.MySQLError{Number: 1146}} // 表不存在:重试不会好
	rec := &recordingSleep{}
	retry := lockRetryPolicy{attempts: 5, baseBackoff: time.Millisecond, maxBackoff: time.Second, sleep: rec.sleep}
	n, err := rewriteZoneRows(context.Background(), f.begin, retry, testZoneTarget, []uint64{11, 12, 13}, 901, 902)
	if n != 1 {
		t.Errorf("rewrote %d rows before the failure, want 1", n)
	}
	var me *mysql.MySQLError
	if err == nil || !errors.As(err, &me) || me.Number != 1146 {
		t.Fatalf("the hard error must surface: %v", err)
	}
	for _, want := range []string{"mmorpg_guild.guild.zone_id 901 → 902", "guild_id=12", "此前已改 1 行"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
	if last := f.log[len(f.log)-1]; last != "rollback" {
		t.Errorf("the failed transaction must be rolled back, log ends with %q", last)
	}
	if f.zones[12] != 901 || f.zones[13] != 901 {
		t.Errorf("nothing after the failure may change: 12=%d 13=%d", f.zones[12], f.zones[13])
	}
	if len(rec.waits) != 0 {
		t.Errorf("a non-lock error must not be retried, slept %v", rec.waits)
	}
}

func TestFenceKeptAbortMessage_KeepsFenceAndSaysHowToClearIt(t *testing.T) {
	// 合服步骤 3 / 3b 与撤销 3' / 3b' 的写失败、缓存失效失败都走 log.Fatal,defer 的 fence.release 不执行。
	// 两条路径共有的契约:文案带本次 run_id、两把键名、mapping Redis 位置与 DEL 指引,否则运维重跑撞上 SETNX 被拒。
	// 各自的契约:保留围栏的理由与重跑语义按路径写 —— 「续跑按清单走、不重扫玩家」只对合服成立,
	// 撤销没有步骤进度,重跑靠每一步幂等。
	const runID = "901-902-20260921T000000Z-abcd1234"
	for _, c := range []struct {
		name    string
		op      fencedOp
		cause   string
		want    []string
		notWant []string
	}{
		{
			name:    "merge",
			op:      fencedOpMerge,
			cause:   "guild MySQL:改写 zone_id 中途失败,步骤 guild_mysql 未标记完成:boom",
			want:    []string{"guild MySQL:改写 zone_id 中途失败", "续跑按清单走、不重扫玩家", "合服读清单续跑", "源区残留对象"},
			notWant: []string{"撤销"},
		},
		{
			name:    "unmerge",
			op:      fencedOpUnmerge,
			cause:   "restore guild zone_id:已改回 3 行,但 guild:v2 缓存失效失败:boom",
			want:    []string{"restore guild zone_id:已改回 3 行", "半撤销", "每一步都按对象当前值过滤、幂等"},
			notWant: []string{"不重扫玩家", "读清单续跑", "源区残留对象"},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			msg := fenceKeptAbortMessage(c.cause, c.op, 901, 902, runID, "10.0.0.5:6379", 0)
			shared := []string{
				"LEFT IN PLACE",
				"仍归 run_id=" + runID,
				"(10.0.0.5:6379 db=0)上 GET merge:in_progress:901,核对其 run_id 是 " + runID,
				"DEL merge:in_progress:901 merge:in_progress:902",
				"another merge is already fencing zone", // 与 acquireMergeFence 的拒绝文案一致
				"SHOW ENGINE INNODB STATUS",
			}
			for _, want := range append(shared, c.want...) {
				if !strings.Contains(msg, want) {
					t.Errorf("abort message is missing %q:\n%s", want, msg)
				}
			}
			for _, bad := range c.notWant {
				if strings.Contains(msg, bad) {
					t.Errorf("abort message for %s must not say %q (wrong path's wording):\n%s", c.name, bad, msg)
				}
			}
			// 顺序也是契约:先 GET 核对,再 DEL,最后重跑。
			get, del, rerun := strings.Index(msg, "GET "), strings.Index(msg, "DEL "), strings.Index(msg, "立即用原命令重跑")
			if get < 0 || del < 0 || rerun < 0 || !(get < del && del < rerun) {
				t.Errorf("resume steps out of order (GET=%d DEL=%d re-run=%d):\n%s", get, del, rerun, msg)
			}
		})
	}
}
