//go:build integration

package store_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"data_service/internal/store"
	"data_service/internal/store/storetest"

	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 玩家名字注册表(设计 docs/design/guild-phase2/03-names.md §3.3 / §3.4)在真实 InnoDB 上
// 的不变量。这几条只有真库能证明,fake store 一条都证不了:
//  1. bootstrap DDL 真的建成了 VARCHAR + utf8mb4_bin + UNIQUE(name_norm) ——
//     唯一键是"名字全服唯一"的全部依据,collation 决定了"库认为哪两个名字相同";
//  2. Reserve 幂等 / 撞名能回出 owner / 同 id 换名是 Conflict;
//  3. 32 个并发抢同一个名字,**恰好**一个成功 —— 应用层的"先查后插"在这里必漏,
//     所以这条测的其实是"我们没有退回先查后插";
//  4. Release 的条件删除:player_id 与 name_norm 都要对上,窗口外必须拒绝;
//  5. 锁模式(审计 #12):未提交的 Release 后面排着两个同名 Reserve,按"持锁现场 × player_id 顺序"逐一摆出
//     (文件末尾一节,红绿对照 + 产品路径):竞争者都大于原主人时不成环、零重试;反之可能成环(Reserve「残余 B」),
//     终局仍正确、至多一次重试,实际行为写进日志。以及 Release 先锁唯一键、后锁聚簇(EXPLAIN)。

// newPlayerNameStore 开一个指向一次性库的 store,用例结束自动关。
func newPlayerNameStore(t *testing.T, db *storetest.DB) *store.PlayerNameStore {
	t.Helper()
	st, err := store.NewPlayerNameStore(db.Cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// playerNameNowMs 取登记时刻(Unix 毫秒)。时间是 Reserve 的显式入参,用例里也显式传。
func playerNameNowMs() uint64 {
	return uint64(time.Now().UnixMilli())
}

// TestPlayerName_BootstrapDDLIsVarcharUniqueBin 守住 schema.go 的 bootstrap 例外。
// 一旦有人"顺手把 player_name 收口回 CreateOrUpdateTable",列会变回 MEDIUMTEXT,
// 唯一键建不出来(1170),而迁移可能只是报错、也可能在某些版本上静默少一个索引 ——
// 那时名字唯一性就只剩应用层的先查后插,并发建角必然重名且全程零报错。
func TestPlayerName_BootstrapDDLIsVarcharUniqueBin(t *testing.T) {
	db := storetest.NewMigratedDB(t)

	var normType, normCollation string
	require.NoError(t, db.Raw.QueryRow(
		`SELECT COLUMN_TYPE, COLLATION_NAME FROM INFORMATION_SCHEMA.COLUMNS
		  WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND COLUMN_NAME = 'name_norm'`,
		db.Name, store.PlayerNameTableName).Scan(&normType, &normCollation))
	assert.Equal(t, "varchar(191)", strings.ToLower(normType), "name_norm 必须是 VARCHAR,TEXT 上建不了无前缀唯一键")
	assert.Equal(t, "utf8mb4_bin", strings.ToLower(normCollation),
		"归一化在 Go 侧(shared/playername)做,库必须逐字节比较;ci collation 会造成'库说撞名、Go 说不撞'")

	var nameType string
	require.NoError(t, db.Raw.QueryRow(
		`SELECT COLUMN_TYPE FROM INFORMATION_SCHEMA.COLUMNS
		  WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND COLUMN_NAME = 'name'`,
		db.Name, store.PlayerNameTableName).Scan(&nameType))
	assert.Equal(t, "varchar(64)", strings.ToLower(nameType))

	var pk string
	require.NoError(t, db.Raw.QueryRow(
		`SELECT COLUMN_NAME FROM INFORMATION_SCHEMA.KEY_COLUMN_USAGE
		  WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND CONSTRAINT_NAME = 'PRIMARY'`,
		db.Name, store.PlayerNameTableName).Scan(&pk))
	assert.Equal(t, "player_id", pk, "主键必须是整数列 player_id(D-14 §2)")

	var nonUnique int
	var idxCol string
	require.NoError(t, db.Raw.QueryRow(
		`SELECT NON_UNIQUE, COLUMN_NAME FROM INFORMATION_SCHEMA.STATISTICS
		  WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND INDEX_NAME = 'uk_player_name' AND SEQ_IN_INDEX = 1`,
		db.Name, store.PlayerNameTableName).Scan(&nonUnique, &idxCol),
		"uk_player_name 必须存在:名字全服唯一只靠它")
	assert.Equal(t, 0, nonUnique, "uk_player_name 必须是唯一索引")
	assert.Equal(t, "name_norm", idxCol)

	// 二次迁移零变更:生产每次部署都会重复跑 -migrate,这张表不能每次被 MODIFY 一遍。
	before := db.ShowCreateTable(t, store.PlayerNameTableName)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	require.NoError(t, store.MigrateSchema(ctx, db.Cfg, store.MigrateOptions{}), "second migration must be a no-op")
	assert.Equal(t, before, db.ShowCreateTable(t, store.PlayerNameTableName), "player_name DDL changed on second migration")
}

// TestPlayerName_ReserveIdempotentAndTaken 覆盖 Reserve 的四种终局里的三种
// (并发抢同名的 Inserted/Taken 竞态在下一个用例)。
func TestPlayerName_ReserveIdempotentAndTaken(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	st := newPlayerNameStore(t, db)
	ctx := context.Background()
	now := playerNameNowMs()

	const (
		playerA uint64 = 1001
		playerB uint64 = 1002
	)

	outcome, owner, err := st.Reserve(ctx, playerA, "Ab12", "ab12", now)
	require.NoError(t, err)
	assert.Equal(t, store.ReserveInserted, outcome)
	assert.Zero(t, owner, "owner 只在 Taken 时有意义")

	// login 对超时会原样重发同一 (player_id, name):必须是成功,不是撞名。
	outcome, owner, err = st.Reserve(ctx, playerA, "Ab12", "ab12", now+1)
	require.NoError(t, err)
	assert.Equal(t, store.ReserveAlreadyOwned, outcome)
	assert.Zero(t, owner)
	// 重试不刷新 created_ms:释放窗口必须以**首次**登记时刻为准,否则反复重试能把
	// 一个早该过窗的登记一直续在窗口里。
	assert.Equal(t, int64(1), db.Count(t, store.PlayerNameTableName, "player_id = ? AND created_ms = ?", playerA, now))

	// 别人来占同一个 norm:Taken,且 owner 要能回出去(login 据此识别"自己上次建角的重试")。
	outcome, owner, err = st.Reserve(ctx, playerB, "AB12", "ab12", now)
	require.NoError(t, err)
	assert.Equal(t, store.ReserveTaken, outcome)
	assert.Equal(t, playerA, owner)
	assert.Equal(t, int64(1), db.Count(t, store.PlayerNameTableName, "name_norm = ?", "ab12"))

	// 同一个 player_id 换名字:v1 无改名,只能是 Conflict,而且一个字节都不许写。
	outcome, owner, err = st.Reserve(ctx, playerA, "Cd34", "cd34", now)
	require.NoError(t, err)
	assert.Equal(t, store.ReserveConflict, outcome)
	assert.Zero(t, owner)
	assert.Equal(t, int64(1), db.Count(t, store.PlayerNameTableName, "player_id = ?", playerA))
	assert.Equal(t, int64(0), db.Count(t, store.PlayerNameTableName, "name_norm = ?", "cd34"))
}

// TestPlayerName_ConcurrentSameName 是本文件里最重要的一条:32 个不同 player_id 同时抢
// 同一个 name_norm,必须恰好一个 Inserted、其余全部 Taken 且 owner 都指向那个赢家,表里一行。
//
// 它测的是"唯一性由唯一键保证"这条设计本身:换成应用层先查后插,这个用例会稳定地
// 插进多行 —— 而线上那会表现为两个玩家顶着同一个名字,没有任何报错。
func TestPlayerName_ConcurrentSameName(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	st := newPlayerNameStore(t, db)

	const (
		goroutines  = 32
		firstPlayer = uint64(2000)
		display     = "YunZhongJun"
		norm        = "yunzhongjun"
	)

	outcomes := make([]store.ReserveOutcome, goroutines)
	owners := make([]uint64, goroutines)
	errs := make([]error, goroutines)

	now := playerNameNowMs()
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // 一起放行,尽量把所有 INSERT 压进同一个锁窗口
			outcomes[i], owners[i], errs[i] = st.Reserve(context.Background(), firstPlayer+uint64(i), display, norm, now)
		}(i)
	}
	close(start)
	wg.Wait()

	inserted, taken := 0, 0
	var winner uint64
	for i := 0; i < goroutines; i++ {
		require.NoErrorf(t, errs[i], "goroutine %d", i)
		switch outcomes[i] {
		case store.ReserveInserted:
			inserted++
			winner = firstPlayer + uint64(i)
		case store.ReserveTaken:
			taken++
		default:
			t.Fatalf("goroutine %d got unexpected outcome %s", i, outcomes[i])
		}
	}
	assert.Equal(t, 1, inserted, "唯一键必须让且只让一个 INSERT 成功")
	assert.Equal(t, goroutines-1, taken)
	assert.Equal(t, int64(1), db.Count(t, store.PlayerNameTableName, "name_norm = ?", norm))

	for i := 0; i < goroutines; i++ {
		if outcomes[i] == store.ReserveTaken {
			assert.Equalf(t, winner, owners[i], "goroutine %d 拿到的 owner 必须是那个插入成功的 id", i)
		}
	}
}

// TestPlayerName_ReleaseConditional 覆盖条件删除的四个分支。
// 这道窗是"别删别人刚建的号"的唯一防线:少了它,任何能发 RPC 的内部调用方都能拿走
// 在役角色的名字,而名字一旦释放就可能立刻被别人占走,不可回滚。
func TestPlayerName_ReleaseConditional(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	st := newPlayerNameStore(t, db)
	ctx := context.Background()
	now := playerNameNowMs()

	const (
		playerA uint64 = 3001
		playerB uint64 = 3002
		display        = "Ef56"
		norm           = "ef56"
	)

	outcome, _, err := st.Reserve(ctx, playerA, display, norm, now)
	require.NoError(t, err)
	require.Equal(t, store.ReserveInserted, outcome)

	// name_norm 对不上:条件删除两列都要匹配,行不能被误删。
	rel, err := st.Release(ctx, playerA, "ef57", 0)
	require.NoError(t, err)
	assert.Equal(t, store.ReleaseAbsent, rel)
	assert.Equal(t, int64(1), db.Count(t, store.PlayerNameTableName, "player_id = ?", playerA))

	// player_id 对不上:拿着名字也删不掉别人的行。
	rel, err = st.Release(ctx, playerB, norm, 0)
	require.NoError(t, err)
	assert.Equal(t, store.ReleaseAbsent, rel)
	assert.Equal(t, int64(1), db.Count(t, store.PlayerNameTableName, "player_id = ?", playerA))

	// 窗口下界比登记时刻还新 = 这行太老,必须回 OutsideWindow(调用方据此要求 admin token),
	// 而不是当成"删成功"骗过上游。
	rel, err = st.Release(ctx, playerA, norm, now+uint64(time.Hour.Milliseconds()))
	require.NoError(t, err)
	assert.Equal(t, store.ReleaseOutsideWindow, rel)
	assert.Equal(t, int64(1), db.Count(t, store.PlayerNameTableName, "player_id = ?", playerA))

	// 窗口内:login 建角补偿的正常形态。
	rel, err = st.Release(ctx, playerA, norm, now-1)
	require.NoError(t, err)
	assert.Equal(t, store.ReleaseDeleted, rel)
	assert.Equal(t, int64(0), db.Count(t, store.PlayerNameTableName, "player_id = ?", playerA))

	// 幂等:login 的"立即 + 延迟 10s"两次释放,第二次必须不是错误(否则每次都记一条孤儿)。
	rel, err = st.Release(ctx, playerA, norm, now-1)
	require.NoError(t, err)
	assert.Equal(t, store.ReleaseAbsent, rel)

	// 不限时间(运维带 x-admin-token 的那条路径)对已删的行同样幂等。
	rel, err = st.Release(ctx, playerA, norm, 0)
	require.NoError(t, err)
	assert.Equal(t, store.ReleaseAbsent, rel)

	// 释放之后名字立刻可被别人占走:唯一键上没有"软删"残留。
	outcome, _, err = st.Reserve(ctx, playerB, display, norm, now+2)
	require.NoError(t, err)
	assert.Equal(t, store.ReserveInserted, outcome)
}

// TestPlayerName_BatchGetSkipsAbsentAndRejectsOversize:缺席的 id 不出现在结果里(不是错误),
// 返回的是**展示名**(保留大小写、保留汉字),超限的入参被拒。
func TestPlayerName_BatchGetSkipsAbsentAndRejectsOversize(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	st := newPlayerNameStore(t, db)
	ctx := context.Background()
	now := playerNameNowMs()

	const (
		playerA uint64 = 4001
		playerB uint64 = 4002
		absent  uint64 = 4003
	)

	_, _, err := st.Reserve(ctx, playerA, "云中君", "云中君", now)
	require.NoError(t, err)
	_, _, err = st.Reserve(ctx, playerB, "Ab12", "ab12", now)
	require.NoError(t, err)

	// 入参里混进 0(logic 正常会去掉)与一个没登记的 id:两者都只是"不出现",不是错误。
	got, err := st.BatchGet(ctx, []uint64{playerA, playerB, absent, 0})
	require.NoError(t, err)
	assert.Equal(t, map[uint64]string{playerA: "云中君", playerB: "Ab12"}, got,
		"返回展示名:大小写按登记时保留,汉字要完整过 utf8mb4")

	empty, err := st.BatchGet(ctx, nil)
	require.NoError(t, err)
	assert.Empty(t, empty)

	_, err = st.BatchGet(ctx, make([]uint64, store.PlayerNameBatchLimit+1))
	assert.ErrorIs(t, err, store.ErrPlayerNameBatchTooLarge)
}

// ── 审计 #12:Release 未提交时两个 Reserve 抢同一个名字(锁模式回归)──────────────────────────
//
// # 编排
//
// 与 go/trade 收藏、go/friend 场景 (g)(h) 同一套办法:靠 performance_schema **观察**两个排队者真的进了锁等待
// 队列再放锁,不靠并发度去撞、不靠 sleep 估时间。红绿两组走**同一个**编排函数,唯一的变量是写入语句本身:
// 绿 = 生产常量 store.PlayerNameReserveSQLForTest(ODKU),红 = 改写前的普通 INSERT(legacyPlainInsertPlayerNameSQL)。
//
// # 两个维度:持锁现场 × player_id 顺序(每个组合一个子用例、一个一次性库)
//
// 持锁方都是"未提交的 Release"(与生产 Release 同一个常量 store.PlayerNameReleaseSQLForTest,放进显式事务;
// 生产路径是自动提交,停不在提交之前),分两种现场(nameRaceHolderCases):
//   - 正删被抢的名字(审计 #12 原形):两个排队者都卡在被抢名字的唯一键项上。
//   - 被抢的名字已释放(删除标记、未 purge),正删唯一键上**紧挨在它后面**的名字:一个排队者卡在后继项上,
//     另一个卡在被抢名字的删除标记项上(排在前者身后)。
//
// uk_player_name 的二级索引项实际按 (name_norm, player_id) 排序,所以胜者的新项插在被抢名字的删除标记项
// ('sanfang', racePrevOwner) 之前还是之后,取决于竞争者与原主人 player_id 的大小(nameRaceOrderCases):
//   - (a) 两个竞争者都大于原主人:新项在删除标记项之后。
//   - (b) 两个竞争者都小于原主人:新项在删除标记项之前,插入意向锁要检查的后继记录正是删除标记项,而另一个
//     竞争者的 X 请求正排在上面 —— 即 Reserve 注释「残余 B」形状 1。生产里号段 id 抢存量角色释放的名字恒为此序。
//   - (c) 一大一小:谁先拿到删除标记项上的 X,由引擎的授予顺序决定。
//
// # 放锁之后的推演与判据
//
//   - 普通 INSERT(红对照):重复检查取 **S** next-key,彼此相容,放锁那一刻被**同时**授予。
//     第二种现场三种顺序都确定成环(新项插在删除标记项前或后,后继记录上都有对方已授予的 S);
//     第一种现场 (b) 确定成环(两个 S 同时授予在删除标记项上,新项都插在它前面),(a)(c) 取决于放锁后两个线程
//     谁先去锁后继记录,只记录、不强制。
//   - ODKU(绿):重复检查取 **X**,排队者一个一个过。
//     (a) 两种现场都必须零 1213,且后完成者在先完成者提交**之前**被观察到在排队 —— 这是强制判据,写法被改回
//     普通 INSERT 时第二种现场的 (a) 稳定变红。
//     (b)(c) 取决于引擎是否把"排队中的请求"算作插入意向锁的冲突(见 Reserve「残余 B」的正反两方面说明):算 →
//     (b) 恰好一个 1213,(c) 看授予顺序;不算 → 与 (a) 同形。用例对两种都只判定"终局正确、至多一个 1213",
//     并把本轮属于哪一种、MySQL 版本写进日志 —— 这是决定要不要上报改 schema 的依据,真库跑完必须抄回来。
//
// # 为什么排队者用会话默认隔离级,而不是显式 READ COMMITTED
//
// 生产 Reserve 是自动提交的单条语句,DSN 不设隔离级,跑在库的全局默认(RR)下。用例要复现的是生产的锁行为,
// 所以排队事务 BeginTx(ctx, nil) 取同一个默认;另外唯一二级索引重复检查在 RC 下是否还取 next-key 随版本有出入,
// 换成 RC 红对照可能失去复现能力。所以本节**所有**用例都要求全局 RR 与 innodb_deadlock_detect 开启(setupNameRace 里
// 检查,不满足就跳过、不代表通过):关了死锁检测时,成环会表现为 nameStepBudget 后的"没有返回",而不是指向 1213 的判据。
//
// # purge 挡板
//
// 一个 REPEATABLE READ 只读事务在任何删除提交**之前**做一次一致性读,它的 read view 让 purge 不能物理清掉删除标记
// 记录。不挡的话,purge 抢在排队者之前清掉记录时,锁会被继承成后继记录上的间隙锁,形状就变成 Reserve 注释「残余」A
// 那类 InnoDB 固有情形(由有界重试兜住),结论不再确定。
//
// (InnoDB 的取锁细节是按手册与 row_ins_scan_sec_index_for_duplicate / lock_rec_insert_check_and_lock 的已知行为推演的,
// 以真库上跑出来的结果为准。)

// legacyPlainInsertPlayerNameSQL 是 2026-09-21 之前 Reserve 用的普通 INSERT。生产代码里已经没有它;这里留一份
// **只**给红对照用:证明编排确实走到了会成环的形状,绿用例的"不成环"才有证明力。
const legacyPlainInsertPlayerNameSQL = `INSERT INTO player_name (player_id, name, name_norm, created_ms) VALUES (?, ?, ?, ?)`

const (
	// nameRaceBudget 是一次编排的总时间预算:真卡住时在这里超时判红,而不是挂住 go test。
	nameRaceBudget = 30 * time.Second
	// nameStepBudget 是每一步"等某件事发生"的上限(等待者到齐、语句返回、清理)。正常都是毫秒级。
	nameStepBudget = 10 * time.Second

	raceContestedDisplay = "SanFang"
	raceContestedNorm    = "sanfang"
	// raceSuccessorNorm 在 utf8mb4_bin 下紧跟在 raceContestedNorm 之后(PAD SPACE 比较:'sanfang' 补空格 < 'sanfangz'),
	// 一次性库里两者之间没有别的名字。
	raceSuccessorDisplay = "SanFangZ"
	raceSuccessorNorm    = "sanfangz"

	racePrevOwner      uint64 = 5001 // 被抢名字原先的主人(它的删除标记项就是排队的地方);竞争者 id 按它分大小,见 nameRaceOrderCases
	raceSuccessorOwner uint64 = 5009
)

// nameRaceHolder 是排在两个 Reserve 前面、持有 X 的第三方(都是未提交的 Release)。
type nameRaceHolder int

const (
	// holderReleasingContestedName:Release 正删被抢的名字、尚未提交。
	holderReleasingContestedName nameRaceHolder = iota
	// holderReleasingSuccessorName:被抢的名字已释放(删除标记、被 purge 挡板留住),Release 正删唯一键上的后继名字、尚未提交。
	holderReleasingSuccessorName
)

// nameRaceHolderCases 是红、绿、产品路径三组共用的持锁现场。
var nameRaceHolderCases = []struct {
	name   string
	holder nameRaceHolder
}{
	{"未提交的Release正删被抢的名字", holderReleasingContestedName},
	{"被抢名字已释放且未purge_未提交的Release正删后继名字", holderReleasingSuccessorName},
}

// nameRaceOrder 是两个竞争者的 player_id 相对被抢名字原主人(racePrevOwner)的大小关系。
type nameRaceOrder int

const (
	// orderBothAbove (a):都大于原主人,新项插在删除标记项之后。
	orderBothAbove nameRaceOrder = iota
	// orderBothBelow (b):都小于原主人,新项插在删除标记项之前(Reserve「残余 B」形状 1)。
	orderBothBelow
	// orderMixed (c):一大一小。
	orderMixed
)

// nameRaceOrderCase 是一种 player_id 顺序与对应的两个竞争者。
type nameRaceOrderCase struct {
	name       string
	order      nameRaceOrder
	contenders [2]uint64
}

// nameRaceOrderCases 是红、绿、产品路径三组共用的 player_id 顺序。
var nameRaceOrderCases = []nameRaceOrderCase{
	{"a_竞争者都大于原主人", orderBothAbove, [2]uint64{5002, 5003}},
	{"b_竞争者都小于原主人", orderBothBelow, [2]uint64{4001, 4002}},
	{"c_竞争者一大一小", orderMixed, [2]uint64{4001, 6001}},
}

// forEachNameRace 按"持锁现场 × player_id 顺序"逐个跑子用例。
func forEachNameRace(t *testing.T, run func(t *testing.T, holder nameRaceHolder, oc nameRaceOrderCase)) {
	t.Helper()
	for _, hc := range nameRaceHolderCases {
		t.Run(hc.name, func(t *testing.T) {
			for _, oc := range nameRaceOrderCases {
				t.Run(oc.name, func(t *testing.T) { run(t, hc.holder, oc) })
			}
		})
	}
}

// nameRaceScene 是摆到"持锁方即将放锁"那一刻的现场。清理挂在 t.Cleanup 上,成功、失败、Skip 三条路径都会走到。
type nameRaceScene struct {
	ctx    context.Context
	db     *storetest.DB
	st     *store.PlayerNameStore
	now    uint64
	holder *sql.Tx
	// mysqlVersion 只进日志:(b)(c) 子用例成不成环取决于引擎版本,结果要能对上版本号。
	mysqlVersion string
	// pending 是本现场起过的所有 goroutine 的结束信号;清理时先取消 ctx 再等它们退出。
	pending []<-chan struct{}
	// txs 是本现场开过的所有事务;清理时一律回滚(已提交的再回滚无害)。
	txs []*sql.Tx
}

// setupNameRace 建库、核对环境前提、造好持锁方,返回时持锁方的 DELETE 已执行、尚未提交。
// 前提(全局 RR、innodb_deadlock_detect 开启)不满足时跳过(不代表通过),理由见本节头注。
func setupNameRace(t *testing.T, holder nameRaceHolder) *nameRaceScene {
	t.Helper()
	db := storetest.NewMigratedDB(t)
	requireGlobalRepeatableRead(t, db)
	requireDeadlockDetect(t, db.Raw)
	// 一个现场同时占用:purge 挡板、持锁方、两个排队事务、轮询 performance_schema 的查询 —— 超过 storetest 默认的 4。
	db.Raw.SetMaxOpenConns(8)
	st := newPlayerNameStore(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), nameRaceBudget)
	sc := &nameRaceScene{ctx: ctx, db: db, st: st, now: playerNameNowMs()}
	t.Cleanup(func() {
		// 顺序要紧:先取消 ctx,仍卡在锁等待里的语句在客户端立刻返回(驱动关连接,服务端回滚);再等 goroutine 退出、
		// 回滚剩下的事务 —— 全部结束之后,storetest 才能 DROP 掉这个库(它的清理注册得更早,所以更晚执行)。
		cancel()
		for _, done := range sc.pending {
			select {
			case <-done:
			case <-time.After(nameStepBudget):
				t.Errorf("清理:取消 ctx 之后 %v 内仍有语句没有返回", nameStepBudget)
			}
		}
		for _, tx := range sc.txs {
			_ = tx.Rollback()
		}
	})
	require.NoError(t, db.Raw.QueryRowContext(ctx, `SELECT VERSION()`).Scan(&sc.mysqlVersion))

	outcome, _, err := st.Reserve(ctx, racePrevOwner, raceContestedDisplay, raceContestedNorm, sc.now)
	require.NoError(t, err)
	require.Equal(t, store.ReserveInserted, outcome, "夹具:被抢的名字先归原主人")
	if holder == holderReleasingSuccessorName {
		outcome, _, err = st.Reserve(ctx, raceSuccessorOwner, raceSuccessorDisplay, raceSuccessorNorm, sc.now)
		require.NoError(t, err)
		require.Equal(t, store.ReserveInserted, outcome, "夹具:后继名字")
	}

	// purge 挡板必须在任何删除提交**之前**建好 read view。
	purge, err := db.Raw.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	require.NoError(t, err, "夹具:开 purge 挡板事务失败")
	sc.txs = append(sc.txs, purge)
	var n int
	require.NoError(t, purge.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM player_name WHERE player_id = ?`, racePrevOwner).Scan(&n), "夹具:purge 挡板的一致性读失败")

	holderPlayer, holderNorm := racePrevOwner, raceContestedNorm
	if holder == holderReleasingSuccessorName {
		// 被抢的名字先经生产 Release 释放并提交:唯一键上留下删除标记记录(被挡板留住)。
		rel, err := st.Release(ctx, racePrevOwner, raceContestedNorm, 0)
		require.NoError(t, err)
		require.Equal(t, store.ReleaseDeleted, rel, "夹具:被抢的名字应已释放")
		holderPlayer, holderNorm = raceSuccessorOwner, raceSuccessorNorm
	}
	sc.holder, err = db.Raw.BeginTx(ctx, nil)
	require.NoError(t, err, "夹具:开持锁方事务失败")
	sc.txs = append(sc.txs, sc.holder)
	res, err := sc.holder.ExecContext(ctx, store.PlayerNameReleaseSQLForTest, holderPlayer, holderNorm, 0)
	require.NoError(t, err, "夹具:持锁方的 Release DELETE 失败")
	removed, err := res.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), removed, "夹具:持锁方应恰好删掉 %s 那一行(此时未提交、持有 X)", holderNorm)
	return sc
}

// awaitQueued 确认 want 个事务排进了 player_name 的锁等待队列;读不了 performance_schema 时跳过(不代表通过)。
func (sc *nameRaceScene) awaitQueued(t *testing.T, want int, expectation string) {
	t.Helper()
	if err := awaitLockWaiters(sc.ctx, sc.db.Raw, store.PlayerNameTableName, want, nameStepBudget); err != nil {
		skipOrFailOnLockWait(t, err, expectation)
	}
}

// releaseHolder 提交持锁方:成环(如果会成)的时刻就在这里。
func (sc *nameRaceScene) releaseHolder(t *testing.T) {
	t.Helper()
	require.NoError(t, sc.holder.Commit(), "夹具:提交持锁方失败")
}

// nameStmt 是在独立事务里执行的一条抢名字的写入语句(未提交,由用例决定何时提交)。
type nameStmt struct {
	label    string
	player   uint64
	tx       *sql.Tx
	finished chan struct{} // 语句返回(成功或失败)后关闭;关闭之后才能读 affected / err
	affected int64
	err      error
}

// startNameStmt 开事务、在 goroutine 里执行 stmt(参数与 playerNameReserveSQL 同序)。
// 隔离级取会话默认,与生产 Reserve 一致(见本节头注)。
func (sc *nameRaceScene) startNameStmt(t *testing.T, stmt, label string, player uint64) *nameStmt {
	t.Helper()
	tx, err := sc.db.Raw.BeginTx(sc.ctx, nil)
	require.NoError(t, err, "夹具:开 %s 的事务失败", label)
	sc.txs = append(sc.txs, tx)
	s := &nameStmt{label: label, player: player, tx: tx, finished: make(chan struct{})}
	sc.pending = append(sc.pending, s.finished)
	go func() {
		defer close(s.finished)
		res, err := tx.ExecContext(sc.ctx, stmt, player, raceContestedDisplay, raceContestedNorm, sc.now+1)
		if err == nil {
			s.affected, err = res.RowsAffected()
		}
		s.err = err
	}()
	return s
}

// startContenders 按 oc 的两个竞争者各起一条 stmt。
func (sc *nameRaceScene) startContenders(t *testing.T, stmt, labelPrefix string, oc nameRaceOrderCase) [2]*nameStmt {
	t.Helper()
	return [2]*nameStmt{
		sc.startNameStmt(t, stmt, fmt.Sprintf("%s#A(player=%d)", labelPrefix, oc.contenders[0]), oc.contenders[0]),
		sc.startNameStmt(t, stmt, fmt.Sprintf("%s#B(player=%d)", labelPrefix, oc.contenders[1]), oc.contenders[1]),
	}
}

// waitNameStmt 等 s 的语句返回(之后才能读它的结果),最多等 nameStepBudget。
func waitNameStmt(t *testing.T, s *nameStmt) {
	t.Helper()
	select {
	case <-s.finished:
	case <-time.After(nameStepBudget):
		t.Fatalf("%s 在 %v 内没有返回:挡在它前面的事务都已放锁,它仍卡着 —— 锁行为与本节推演不符", s.label, nameStepBudget)
	}
}

// settleNameRace 在持锁方放锁之后把两个排队语句走到终点。返回先完成者、后完成者,以及后完成者是否在先完成者
// **提交之前**被观察到卡在锁等待里(ODKU 的 X 排队就是这个形状;此时本函数会提交先完成者,放它过去)。
//
// 先完成者成功时,后完成者只可能是两种状态之一,靠观察区分而不是估时间:
//   - 已经(或马上)返回:它是 1213 的牺牲者,错误正在路上;
//   - 排在先完成者未提交的插入后面(锁等待队列里能看见它)。
//
// 成环时两条语句都不经本函数提交(queued=false):幸存者的事务由调用方决定提交与否。
func settleNameRace(t *testing.T, sc *nameRaceScene, w [2]*nameStmt) (first, second *nameStmt, secondQueuedBehindFirst bool) {
	t.Helper()
	select {
	case <-w[0].finished:
		first, second = w[0], w[1]
	case <-w[1].finished:
		first, second = w[1], w[0]
	case <-time.After(nameStepBudget):
		t.Fatalf("持锁方放锁后 %v 内两个排队语句都没有返回", nameStepBudget)
	}
	if first.err != nil {
		// 先完成者失败(1213 牺牲者,已整条回滚):另一方不再被挡,必须自己走完。
		waitNameStmt(t, second)
		return first, second, false
	}

	const pollEvery = 10 * time.Millisecond
	deadline := time.Now().Add(nameStepBudget)
	for {
		select {
		case <-second.finished:
			return first, second, false
		default:
		}
		var waiters int
		if err := sc.db.Raw.QueryRowContext(sc.ctx, lockWaitersQuery, store.PlayerNameTableName).Scan(&waiters); err != nil {
			skipOrFailOnLockWait(t, fmt.Errorf("%w: %v", errLockWaitsUnobservable, err), "观察后完成者是否排在先完成者后面")
		}
		if waiters >= 1 {
			// 持锁方已提交、先完成者的语句已结束、purge 挡板不持锁:此刻还在等锁的只能是后完成者,等的是先完成者。
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s 成功后 %v 内,%s 既没有返回也不在锁等待队列里 —— 编排与推演不符", first.label, nameStepBudget, second.label)
		}
		time.Sleep(pollEvery)
	}
	require.NoError(t, first.tx.Commit(), "提交 %s 失败", first.label)
	waitNameStmt(t, second)
	return first, second, true
}

// isMySQLErrNumber 只按错误号判定(errors.As 能穿透 %w 包装)。错误号引用 store 包的权威定义
// (store.MySQLErrDeadlockForTest / store.MySQLErrDupEntryForTest),本文件不再写字面量。
func isMySQLErrNumber(err error, number uint16) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == number
}

// nameOwner 读名字的当前占用者;名字没人占时返回 0。
func nameOwner(t *testing.T, sc *nameRaceScene) uint64 {
	t.Helper()
	var owner uint64
	err := sc.db.Raw.QueryRowContext(sc.ctx, `SELECT player_id FROM player_name WHERE name_norm = ?`, raceContestedNorm).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return 0
	}
	require.NoError(t, err)
	return owner
}

// TestPlayerName_ReleaseRacesTwoReservesConverges 是**绿**:生产的 ODKU 写入在未提交的 Release 后面排队,放锁之后:
//   - (a) 竞争者都大于原主人:必须零 1213、恰好一方插入(受影响 1 行)、另一方撞键(受影响 0 行,no-op),而且后者是
//     **在前者提交之前**被观察到卡在锁等待里的 —— ODKU 的重复检查取 X,两个排队者不可能并行。
//     playerNameReserveSQL 被改回普通 INSERT / INSERT IGNORE 时,第二种现场的 (a) 稳定变红(1213)。
//   - (b)(c):至多一个 1213;没有 1213 时判据同 (a);有 1213 时幸存者必须插入成功。两种情形都把结果与 MySQL 版本
//     写进日志(见本节头注「放锁之后的推演与判据」)。
//
// 三种顺序的终局都必须是:名字归插入成功的那一方、表里一行、另一方一个字节都没写进去。
func TestPlayerName_ReleaseRacesTwoReservesConverges(t *testing.T) {
	forEachNameRace(t, func(t *testing.T, holder nameRaceHolder, oc nameRaceOrderCase) {
		sc := setupNameRace(t, holder)
		w := sc.startContenders(t, store.PlayerNameReserveSQLForTest, "Reserve", oc)
		sc.awaitQueued(t, 2, "持锁方未提交,两个 Reserve 的重复检查都应排进锁等待队列")

		sc.releaseHolder(t)
		first, second, queued := settleNameRace(t, sc, w)

		var victim *nameStmt
		for _, s := range []*nameStmt{first, second} {
			if isMySQLErrNumber(s.err, store.MySQLErrDeadlockForTest) {
				require.Nilf(t, victim, "两条 ODKU 都收到 1213:%s 与 %s —— 一次环只会牺牲一方,锁行为与本节推演不符", first.label, second.label)
				victim = s
				continue
			}
			require.NoErrorf(t, s.err, "%s 出现非预期错误", s.label)
		}

		var winner, loser *nameStmt
		if victim == nil {
			// 排队形状:X 一个一个过。
			require.Equalf(t, int64(1), first.affected, "先完成者 %s 应插入成功(受影响 1 行)", first.label)
			require.Truef(t, queued, "%s 在 %s 提交之前就返回了:重复检查没有取 X、两者并行了 —— 写法被改了?", second.label, first.label)
			require.Equalf(t, int64(0), second.affected, "后完成者 %s 应撞键走 no-op(受影响 0 行),不许改任何数据", second.label)
			require.NoError(t, second.tx.Commit())
			winner, loser = first, second
			if oc.order != orderBothAbove {
				t.Logf("本轮未成环(MySQL %s):胜者 %s 插入时没有被排队中的 %s 挡住 —— 本版本不把被自己已授予锁挡住的排队请求"+
					"算作插入意向锁的冲突,Reserve「残余 B」形状 1 在此现场不出现", sc.mysqlVersion, first.label, second.label)
			}
		} else {
			require.NotEqualf(t, orderBothAbove, oc.order,
				"竞争者都大于原主人时 ODKU 不许成环,%s 却收到 1213:重复检查又在取 S(playerNameReserveSQL 被改回普通 INSERT 了?)"+
					"或本节推演有误。%v", victim.label, victim.err)
			winner = first
			if winner == victim {
				winner = second
			}
			loser = victim
			require.Equalf(t, int64(1), winner.affected, "幸存者 %s 应插入成功(受影响 1 行)", winner.label)
			require.NoError(t, winner.tx.Commit())
			t.Logf("本轮成环(MySQL %s):%s 收到 1213,%s 插入成功 —— Reserve「残余 B」形状 1 在本版本上存在,"+
				"生产路径由有界重试兜住,是否改 schema 须上报决策", sc.mysqlVersion, victim.label, winner.label)
		}

		assert.Equal(t, winner.player, nameOwner(t, sc), "名字归插入成功的一方")
		assert.Equal(t, int64(1), sc.db.Count(t, store.PlayerNameTableName, "name_norm = ?", raceContestedNorm))
		assert.Equal(t, int64(0), sc.db.Count(t, store.PlayerNameTableName, "player_id = ?", loser.player),
			"撞键方 / 牺牲方一个字节都不许写进去")
	})
}

// TestPlayerName_LegacyPlainInsertDeadlocksBehindPendingRelease 是**红对照**:同一编排换成改写前的普通 INSERT。
// 它不验产品代码,验的是"编排确实走到了会成环的形状";第二种现场的三种顺序与第一种现场的 (b) 必须恰好一个 1213、
// 一个成功,不红的时候,绿用例 (a) 的"不成环"就什么都没证明。第一种现场的 (a)(c) 成环依赖放锁后两个线程的先后
// (见本节头注),只记录不强制。
func TestPlayerName_LegacyPlainInsertDeadlocksBehindPendingRelease(t *testing.T) {
	forEachNameRace(t, func(t *testing.T, holder nameRaceHolder, oc nameRaceOrderCase) {
		sc := setupNameRace(t, holder)
		w := sc.startContenders(t, legacyPlainInsertPlayerNameSQL, "INSERT", oc)
		sc.awaitQueued(t, 2, "持锁方未提交,两个普通 INSERT 的重复检查都应排进锁等待队列")

		sc.releaseHolder(t)
		first, second, _ := settleNameRace(t, sc, w)

		var deadlocked, inserted, duplicate int
		for _, s := range []*nameStmt{first, second} {
			switch {
			case isMySQLErrNumber(s.err, store.MySQLErrDeadlockForTest):
				deadlocked++
			case isMySQLErrNumber(s.err, store.MySQLErrDupEntryForTest):
				duplicate++
			case s.err == nil && s.affected == 1:
				inserted++
			default:
				t.Fatalf("%s 出现非预期结果(既不是插入成功,也不是 1213 / 1062): affected=%d err=%v", s.label, s.affected, s.err)
			}
		}
		require.Equal(t, 1, inserted, "无论成没成环,恰好一方插入")

		if holder == holderReleasingSuccessorName || oc.order == orderBothBelow {
			require.Equalf(t, 1, deadlocked,
				"红对照没有复现死锁(1213=%d 1062=%d,MySQL %s):两个 S next-key 本应在放锁那一刻被同时授予,"+
					"新项的后继记录上有对方已授予的 S,再互等插入意向锁。编排没走到成环形状,"+
					"TestPlayerName_ReleaseRacesTwoReservesConverges 在这个 MySQL 版本上失去证明力(见本节头注)",
				deadlocked, duplicate, sc.mysqlVersion)
			return
		}
		if deadlocked == 1 {
			t.Logf("本轮复现了 1213(MySQL %s)", sc.mysqlVersion)
		} else {
			t.Logf("本轮未复现 1213(1062=%d,MySQL %s):先完成者抢在另一方锁后继记录之前插入了,属于本现场的已知竞态,不作判据",
				duplicate, sc.mysqlVersion)
		}
	})
}

// TestPlayerName_ReserveBehindPendingReleaseOutcome 走完整的产品路径 store.Reserve(自动提交,与生产同一隔离级):
// 所有现场与顺序下都必须恰好一个 Inserted、一个 Taken 且 owner 是赢家,返回值里不许出现 error。
// 锁冲突重试次数(LockRetriesForTest 的增量)按顺序分开判:
//   - (a) 必须为 0 —— 证明这里结果正确不是靠重试把 1213 吸收掉换来的;
//   - (b)(c) 至多 1(Reserve「残余 B」可能成环一次,由有界重试兜住,通常一次重来即收敛),实际次数写进日志。
func TestPlayerName_ReserveBehindPendingReleaseOutcome(t *testing.T) {
	forEachNameRace(t, func(t *testing.T, holder nameRaceHolder, oc nameRaceOrderCase) {
		sc := setupNameRace(t, holder)
		retriesBefore := sc.st.LockRetriesForTest()

		type result struct {
			player  uint64
			outcome store.ReserveOutcome
			owner   uint64
			err     error
		}
		results := make(chan result, 2)
		for _, p := range oc.contenders {
			done := make(chan struct{})
			sc.pending = append(sc.pending, done)
			go func() {
				defer close(done)
				o, owner, err := sc.st.Reserve(sc.ctx, p, raceContestedDisplay, raceContestedNorm, sc.now+1)
				results <- result{player: p, outcome: o, owner: owner, err: err}
			}()
		}
		sc.awaitQueued(t, 2, "持锁方未提交,两个 Reserve 都应排进锁等待队列")
		sc.releaseHolder(t)

		var got []result
		for len(got) < 2 {
			select {
			case r := <-results:
				got = append(got, r)
			case <-time.After(nameStepBudget):
				t.Fatalf("持锁方放锁后 %v 内只有 %d/2 个 Reserve 返回", nameStepBudget, len(got))
			}
		}
		var winner uint64
		inserted, taken := 0, 0
		for _, r := range got {
			require.NoErrorf(t, r.err, "player %d 的 Reserve 返回了错误(1213 应被有界重试吸收)", r.player)
			switch r.outcome {
			case store.ReserveInserted:
				inserted++
				winner = r.player
			case store.ReserveTaken:
				taken++
			default:
				t.Fatalf("player %d 得到非预期终局 %s", r.player, r.outcome)
			}
		}
		require.Equal(t, 1, inserted, "恰好一个 Reserve 占到名字")
		require.Equal(t, 1, taken)
		for _, r := range got {
			if r.outcome == store.ReserveTaken {
				assert.Equalf(t, winner, r.owner, "player %d 拿到的 owner 必须是赢家", r.player)
			}
		}

		retries := sc.st.LockRetriesForTest() - retriesBefore
		if oc.order == orderBothAbove {
			assert.Equal(t, uint64(0), retries,
				"竞争者都大于原主人时排队抢名字的路径上发生了锁冲突重试:ODKU 下这里不该成环(见 Reserve 的注释)")
		} else {
			assert.LessOrEqualf(t, retries, uint64(1),
				"锁冲突重试 %d 次:「残余 B」一次环只该让牺牲方重来一次,超过说明收敛论证不成立", retries)
			t.Logf("锁冲突重试 %d 次(MySQL %s;1 = 本版本上「残余 B」成环并被有界重试吸收,0 = 未成环)", retries, sc.mysqlVersion)
		}
		assert.Equal(t, winner, nameOwner(t, sc))
		assert.Equal(t, int64(1), sc.db.Count(t, store.PlayerNameTableName, "name_norm = ?", raceContestedNorm))
	})
}

// TestPlayerName_ReleaseStatementLocksUniqueKeyFirst 钉住 Release 的取锁顺序(见 Release 的「锁序」):
// Reserve 的 ODKU 撞唯一键时先锁 uk_player_name、后锁聚簇记录,Release 必须同序,否则两者在占用者那一行上
// 反序成环。锁序只取决于执行计划,并发用例只能按概率撞上,而 EXPLAIN 每次都答得出来。
// 对生产代码里**同一个 SQL 常量**做 EXPLAIN,断言 key = uk_player_name;有人去掉 FORCE INDEX 或改回单表按主键删,
// 本用例就会变红。先插一行:命中已存在的行时计划才显示真实的索引。
func TestPlayerName_ReleaseStatementLocksUniqueKeyFirst(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	st := newPlayerNameStore(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	const (
		player uint64 = 7001
		norm          = "explainname" // 只含 [a-z],可以直接内联成 SQL 字面量
	)
	outcome, _, err := st.Reserve(ctx, player, "ExplainName", norm, playerNameNowMs())
	require.NoError(t, err)
	require.Equal(t, store.ReserveInserted, outcome)

	stmt := store.PlayerNameReleaseSQLForTest
	for _, lit := range []string{fmt.Sprint(player), "'" + norm + "'", "0"} {
		require.Contains(t, stmt, "?", "占位符数量与参数不符")
		stmt = strings.Replace(stmt, "?", lit, 1)
	}
	require.NotContains(t, stmt, "?", "占位符数量与参数不符")

	rows, err := db.Raw.QueryContext(ctx, "EXPLAIN FORMAT=TRADITIONAL "+stmt)
	require.NoError(t, err, "EXPLAIN 失败: %s", stmt)
	defer rows.Close()
	cols, err := rows.Columns()
	require.NoError(t, err)
	require.True(t, rows.Next(), "EXPLAIN 没有返回任何行: %s", stmt)
	vals := make([]sql.NullString, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	require.NoError(t, rows.Scan(ptrs...))
	plan := make(map[string]string, len(cols))
	for i, c := range cols {
		plan[c] = vals[i].String
	}
	t.Logf("Release 的执行计划: %v", plan)
	assert.Equal(t, "uk_player_name", plan["key"],
		"Release 必须先锁唯一键项、后锁聚簇记录(与 Reserve 的 ODKU 同序):FORCE INDEX (uk_player_name) 被去掉或没生效")
}
