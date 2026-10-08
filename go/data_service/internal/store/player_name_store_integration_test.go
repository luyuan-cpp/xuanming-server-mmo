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
//  1. bootstrap DDL 真的建成了 VARCHAR + utf8mb4_bin + **PRIMARY KEY(name_norm)** + UNIQUE(player_id) ——
//     名字当聚簇主键是审计 #12 根治的全部前提,collation 决定了"库认为哪两个名字相同";
//  2. Reserve 幂等 / 撞名能回出 owner / 同 id 换名是 Conflict;
//  3. 32 个并发抢同一个名字,**恰好**一个成功 —— 应用层的"先查后插"在这里必漏,
//     所以这条测的其实是"我们没有退回先查后插";
//  4. Release 的条件删除:player_id 与 name_norm 都要对上,窗口外必须拒绝;
//  5. 锁模式(审计 #12 的根治):未提交的 Release 后面排着两个同名 Reserve,按"持锁现场 × player_id 顺序"
//     逐一摆出(文件末尾一节,红绿对照 + 产品路径):**三种顺序 × 两种现场一律零 1213、零重试**,
//     终局都正确。红对照在用例自建的**旧结构**临时表上跑普通 INSERT,证明这套编排确实能把环摆出来。
//     以及 Release 先锁聚簇、后锁二级索引项(EXPLAIN 钉 key = PRIMARY);
//  6. 换主键那次迁移本身:旧结构的存量表跑完 -migrate 之后形状对、数据一行不少、再跑一次零变更。

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
// 主键 / 唯一键建不出来(1170),而迁移可能只是报错、也可能在某些版本上静默少一个约束 ——
// 那时名字唯一性就只剩应用层的先查后插,并发建角必然重名且全程零报错。
// 同时钉住新结构的键形状:主键 name_norm(根治的前提)、player_id 上另有一条唯一键。
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

	ctxPK, cancelPK := context.WithTimeout(context.Background(), time.Minute)
	pkCols, err := store.PrimaryKeyColumnsForTest(ctxPK, db.Raw, db.Name, store.PlayerNameTableName)
	cancelPK()
	require.NoError(t, err)
	assert.Equal(t, []string{"name_norm"}, pkCols,
		"主键必须是 name_norm:名字抢注死锁的根治就是「被争抢的键做成聚簇主键」,"+
			"改回 player_id 会让生产常态那一路重新成环(真库实测 5/5)")

	var nonUnique int
	var idxCol string
	require.NoError(t, db.Raw.QueryRow(
		`SELECT NON_UNIQUE, COLUMN_NAME FROM INFORMATION_SCHEMA.STATISTICS
		  WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND INDEX_NAME = ? AND SEQ_IN_INDEX = 1`,
		db.Name, store.PlayerNameTableName, store.PlayerNameOwnerUniqueKey).Scan(&nonUnique, &idxCol),
		"%s 必须存在:Reserve 靠它把「这个 player_id 已经有别的名字」判成 Conflict", store.PlayerNameOwnerUniqueKey)
	assert.Equal(t, 0, nonUnique, "%s 必须是唯一索引", store.PlayerNameOwnerUniqueKey)
	assert.Equal(t, "player_id", idxCol)

	// name_norm 上**不能**再有二级唯一索引:ODKU 的重复扫描会连它一起取 X next-key,
	// 把改主键消掉的那条环原样带回来(而且是静默的 —— 功能全对,只是又开始死锁)。
	var normSecondary int
	require.NoError(t, db.Raw.QueryRow(
		`SELECT COUNT(*) FROM INFORMATION_SCHEMA.STATISTICS
		  WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND COLUMN_NAME = 'name_norm' AND INDEX_NAME <> 'PRIMARY'`,
		db.Name, store.PlayerNameTableName).Scan(&normSecondary))
	assert.Equal(t, 0, normSecondary,
		"name_norm 上出现了二级索引:主键已经覆盖它,多出来的那条只会让 ODKU 多取一把 X next-key,把环带回来")

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

// ── 审计 #12 根治之后的锁模式回归(名字是聚簇主键 + ODKU)──────────────────────────────
//
// # 这一节要证明什么
//
// 2026-09-29 的真库探针(独立小程序,不在本仓)给出了这张表:
//
//	表结构                                      写法          竞争者都小于原主人  都大于  一大一小
//	PRIMARY KEY(player_id) + UNIQUE(name_norm)   普通 INSERT   5/5 死锁          2/5    5/5
//	PRIMARY KEY(player_id) + UNIQUE(name_norm)   ODKU          5/5 死锁          0/5    2/5
//	PRIMARY KEY(name_norm) + UNIQUE(player_id)   普通 INSERT   5/5 死锁          5/5    5/5
//	PRIMARY KEY(name_norm) + UNIQUE(player_id)   **ODKU**      **0/5**           0/5    0/5
//
// 于是本节的分工是:
//   - **绿**(TestPlayerName_ReleaseRacesTwoReservesConverges / TestPlayerName_ReserveBehindPendingReleaseOutcome):
//     在生产表(已迁移 = 新结构)上跑生产语句,3 种 player_id 顺序 × 2 种持锁现场,**一律断言零 1213**。
//   - **红对照**(TestPlayerName_LegacyPrimaryKeyPlainInsertDeadlocks):在用例**自己建的**旧结构临时表上
//     跑普通 INSERT,断言能复现 1213 —— 证明这套编排确实能把环摆出来,绿用例的"零"才有证明力。
//     它刻意不碰生产表结构:红对照的前提是旧结构,而生产表已经不是旧结构了。
//
// # 编排
//
// 与 go/trade 收藏、go/friend 场景 (g)(h) 同一套办法:靠 performance_schema **观察**排队者真的进了锁等待
// 队列再放锁,不靠并发度去撞、不靠 sleep 估时间。
//
// 两个维度:
//
// 持锁现场(nameRaceHolderCases),持锁方都是"未提交的 Release"(放进显式事务;生产路径是自动提交,
// 停不在提交之前):
//   - **正删被抢的名字**(审计 #12 原形):两个竞争者都卡在被抢名字那条记录上,`queueBehindHolder = true`。
//   - **正删后继名字**(被抢的名字已释放、删除标记未 purge):`queueBehindHolder = false` ——
//     两个竞争者与持锁方**根本不相干**,这是探针实测的(旧结构上也只看到 0/2 个排队者),
//     所以这里**不能**等"两个都排进队列",等了必然在 awaitQueued 上超时判红。
//     翻过来断言更有意义:至少一个竞争者必须在持锁方**仍未提交**时就跑完 —— 真被挡住的话一个都跑不完。
//     这条同时是"跨名字耦合已经消失"的回归:谁要是把 name_norm 上的二级唯一索引加回来,
//     ODKU 的重复扫描又会连"第一条不相等的记录"一起取 X next-key,这条断言立刻变红。
//
// player_id 顺序(nameRaceOrderCases),相对被抢名字原主人 racePrevOwner:
//   - (a) 都大于、(b) 都小于、(c) 一大一小。
//     新结构下这个维度**不应该再有任何影响**:名字是聚簇主键,撞上同键的删除标记记录走的是原地改写
//     (row_ins_must_modify_rec),取 LOCK_X + LOCK_REC_NOT_GAP、不申请插入意向锁,所以"新项插在删除标记项
//     之前还是之后"这件事根本不存在。三种顺序都跑,正是为了把这条"不应该再有影响"钉成机械判据 ——
//     (b) 在旧结构上是生产常态且必然成环(实测 5/5),它要是又红了,说明主键被改回去了。
//
// # 为什么排队者用会话默认隔离级,而不是显式 READ COMMITTED
//
// 生产 Reserve 是自动提交的单条语句,DSN 不设隔离级,跑在库的全局默认(RR)下。用例要复现的是生产的锁行为,
// 所以排队事务 BeginTx(ctx, nil) 取同一个默认。本节**所有**用例都要求全局 RR 与 innodb_deadlock_detect 开启
// (setupNameRace 里检查,不满足就跳过、不代表通过):关了死锁检测时,成环会表现为 nameStepBudget 后的
// "没有返回",而不是指向 1213 的判据。
//
// # purge 挡板
//
// 一个 REPEATABLE READ 只读事务在任何删除提交**之前**做一次一致性读,它的 read view 让 purge 不能物理清掉
// 删除标记记录。不挡的话,purge 抢在排队者之前清掉记录时,锁会被继承成后继记录上的间隙锁,形状就变成
// Reserve 注释「残余 A」那类 InnoDB 固有情形(由有界重试兜住),本节的判据不再成立。

// legacyPlayerNamePKTable 是红对照**自己建**的旧结构表:PRIMARY KEY(player_id) + UNIQUE(name_norm)。
//
// 为什么不复用生产表:生产表跑完迁移已经是新结构了,而红对照要证明的恰恰是"旧结构 + 普通 INSERT 会成环"。
// 建一张一次性表比"先把生产表 ALTER 回去再 ALTER 回来"稳得多 —— 后者会让红对照和绿用例互相破坏前提。
const legacyPlayerNamePKTable = "player_name_legacy_pk_it"

// legacyPlayerNamePKTableDDL 与 2026-09-29 之前的 playerNameBootstrapDDL 同形(列、collation 都一样,
// 只把表名换掉),这样红对照复现的是**当时真实的**表结构,而不是一张另外设计的表。
const legacyPlayerNamePKTableDDL = "CREATE TABLE `" + legacyPlayerNamePKTable + "` (\n" +
	"  `player_id` bigint unsigned NOT NULL DEFAULT 0,\n" +
	"  `name` VARCHAR(64) NOT NULL DEFAULT '',\n" +
	"  `name_norm` VARCHAR(191) NOT NULL,\n" +
	"  `created_ms` bigint unsigned NOT NULL DEFAULT 0,\n" +
	"  PRIMARY KEY (`player_id`),\n" +
	"  UNIQUE KEY `uk_player_name` (`name_norm`)\n" +
	") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin"

// legacyPlainInsertPlayerNameSQL 是 2026-09-21 之前 Reserve 用的普通 INSERT(打在红对照那张临时表上)。
// 生产代码里已经没有它;这里留一份**只**给红对照用。
const legacyPlainInsertPlayerNameSQL = "INSERT INTO `" + legacyPlayerNamePKTable +
	"` (player_id, name, name_norm, created_ms) VALUES (?, ?, ?, ?)"

// legacyReleasePlayerNameSQL 是红对照里持锁方的 DELETE:与旧生产语句同形(FORCE INDEX 走名字那条二级唯一索引,
// 先锁唯一键项、后锁聚簇记录),只把表名换成临时表。探针的编排也是这么摆的。
const legacyReleasePlayerNameSQL = "DELETE `" + legacyPlayerNamePKTable + "` FROM `" + legacyPlayerNamePKTable +
	"` FORCE INDEX (`uk_player_name`) WHERE player_id = ? AND name_norm = ? AND created_ms >= ?"

// legacySeedPlayerNameSQL 给红对照那张临时表种一行(不走 store,store 只认生产表)。
const legacySeedPlayerNameSQL = "INSERT INTO `" + legacyPlayerNamePKTable +
	"` (player_id, name, name_norm, created_ms) VALUES (?, ?, ?, ?)"

const (
	// nameRaceBudget 是一次编排的总时间预算:真卡住时在这里超时判红,而不是挂住 go test。
	nameRaceBudget = 30 * time.Second
	// nameStepBudget 是每一步"等某件事发生"的上限(等待者到齐、语句返回、清理)。正常都是毫秒级。
	nameStepBudget = 10 * time.Second

	raceContestedDisplay = "SanFang"
	raceContestedNorm    = "sanfang"
	// raceSuccessorNorm 在 utf8mb4_bin 下紧跟在 raceContestedNorm 之后('sanfang' < 'sanfangz'),
	// 一次性库里两者之间没有别的名字。
	raceSuccessorDisplay = "SanFangZ"
	raceSuccessorNorm    = "sanfangz"

	racePrevOwner      uint64 = 5001 // 被抢名字原先的主人(它的删除标记记录就是排队的地方);竞争者 id 按它分大小
	raceSuccessorOwner uint64 = 5009
)

// nameRaceHolder 是排在两个竞争者前面、持有 X 的第三方(都是未提交的 Release)。
type nameRaceHolder int

const (
	// holderReleasingContestedName:Release 正删被抢的名字、尚未提交。
	holderReleasingContestedName nameRaceHolder = iota
	// holderReleasingSuccessorName:被抢的名字已释放(删除标记、被 purge 挡板留住),Release 正删后继名字、尚未提交。
	holderReleasingSuccessorName
)

// nameRaceHolderCase 是一种持锁现场,以及"两个竞争者会不会排在持锁方后面"这个**前提**。
type nameRaceHolderCase struct {
	name   string
	holder nameRaceHolder
	// queueBehindHolder = 两个竞争者**确定**会卡在持锁方持有的那条记录上,可以硬等(awaitQueued)。
	//
	// 只有"持锁方正删被抢的名字"那一路是 true:名字是聚簇主键,竞争者的重复检查必然落在同一条记录上,
	// 与时序无关。后继名字那一路是 false,而 false 的含义是"**不硬等,只观察**",不是"断言它们不排队" ——
	// 探针那份"两个竞争者根本不排队(0/2)"是在**旧结构**(名字只是二级唯一索引)上测的,不能外推到新结构;
	// 而按 player_name_store.go「与其它写者的交错」刚改准的结论,持锁方删的若是**未 purge 的删除标记记录**,
	// DELETE 的免间隙优化未必成立,退回 next-key 时竞争者**会**被挡住 —— 那是引擎的合法行为,不是缺陷。
	// 所以这一格的编排结果只进日志(见 observeSuccessorArrangement),强判据留给"零 1213"。
	queueBehindHolder bool
}

// nameRaceHolderCases 是红、绿、产品路径共用的持锁现场。
var nameRaceHolderCases = []nameRaceHolderCase{
	{name: "未提交的Release正删被抢的名字", holder: holderReleasingContestedName, queueBehindHolder: true},
	{name: "被抢名字已释放且未purge_未提交的Release正删后继名字", holder: holderReleasingSuccessorName, queueBehindHolder: false},
}

// nameRaceOrder 是两个竞争者的 player_id 相对被抢名字原主人(racePrevOwner)的大小关系。
type nameRaceOrder int

const (
	// orderBothAbove (a):都大于原主人。
	orderBothAbove nameRaceOrder = iota
	// orderBothBelow (b):都小于原主人。旧结构下这是生产常态,且必然成环(实测 5/5)。
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

// nameRaceOrderCases 是红、绿、产品路径共用的 player_id 顺序。
var nameRaceOrderCases = []nameRaceOrderCase{
	{"a_竞争者都大于原主人", orderBothAbove, [2]uint64{5002, 5003}},
	{"b_竞争者都小于原主人", orderBothBelow, [2]uint64{4001, 4002}},
	{"c_竞争者一大一小", orderMixed, [2]uint64{4001, 6001}},
}

// forEachNameRace 按"持锁现场 × player_id 顺序"逐个跑子用例。
func forEachNameRace(t *testing.T, run func(t *testing.T, hc nameRaceHolderCase, oc nameRaceOrderCase)) {
	t.Helper()
	for _, hc := range nameRaceHolderCases {
		t.Run(hc.name, func(t *testing.T) {
			for _, oc := range nameRaceOrderCases {
				t.Run(oc.name, func(t *testing.T) { run(t, hc, oc) })
			}
		})
	}
}

// nameRaceScene 是摆到"持锁方即将放锁"那一刻的现场。清理挂在 t.Cleanup 上,成功、失败、Skip 三条路径都会走到。
type nameRaceScene struct {
	ctx context.Context
	db  *storetest.DB
	st  *store.PlayerNameStore
	now uint64
	// table 是本现场操作的表:绿 / 产品路径 = 生产表(新结构),红对照 = 用例自建的旧结构临时表。
	table  string
	holder *sql.Tx
	// mysqlVersion 只进日志与失败信息。本节的判据**不随版本变**:
	// 新结构 + ODKU 的"零 1213"来自"聚簇索引上撞同键走原地改写、不申请插入意向锁"这条机制,
	// 旧结构那条环的"与版本无关"由 E4 的 ut_a(!conflicting.bypassed) 钉死。
	// 记版本号只是为了判据真跑不出来时,能指认是在哪个实例上重核的 —— 不是"按版本放宽断言"的借口。
	mysqlVersion string
	// arrangement 是 armContenders 观察到的编排现场(排队数 / 有没有竞争者先跑完),**只进日志与失败信息**。
	// 它存在的理由:强判据只剩"零 1213"之后,一条用例变红时必须能分辨两种完全不同的原因 ——
	// 「环真的成了」(出现 1213 / 重试计数 > 0)与「只是没排上队,编排根本没摆到位」(排队数为 0)。
	arrangement string
	// pending 是本现场起过的所有 goroutine 的结束信号;清理时先取消 ctx 再等它们退出。
	pending []<-chan struct{}
	// txs 是本现场开过的所有事务;清理时一律回滚(已提交的再回滚无害)。
	txs []*sql.Tx
}

// nameRaceFixture 描述一个现场用哪张表、哪两段语句。
type nameRaceFixture struct {
	// legacyTable = 在用例自建的旧结构临时表上跑(红对照);false = 在生产表上跑。
	legacyTable bool
	// seedSQL 种子行的写法(生产表走 store.Reserve,所以为空)。
	seedSQL string
	// releaseSQL 持锁方那条未提交的 DELETE。
	releaseSQL string
}

var (
	// productionNameRaceFixture 绿 / 产品路径:生产表 + 生产的 Release 语句。
	productionNameRaceFixture = nameRaceFixture{releaseSQL: store.PlayerNameReleaseSQLForTest}
	// legacyNameRaceFixture 红对照:自建旧结构表 + 与旧生产语句同形的 Release。
	legacyNameRaceFixture = nameRaceFixture{
		legacyTable: true,
		seedSQL:     legacySeedPlayerNameSQL,
		releaseSQL:  legacyReleasePlayerNameSQL,
	}
)

// setupNameRace 建库、核对环境前提、造好持锁方,返回时持锁方的 DELETE 已执行、尚未提交。
// 前提(全局 RR、innodb_deadlock_detect 开启)不满足时跳过(不代表通过),理由见本节头注。
func setupNameRace(t *testing.T, holder nameRaceHolder, fx nameRaceFixture) *nameRaceScene {
	t.Helper()
	db := storetest.NewMigratedDB(t)
	requireGlobalRepeatableRead(t, db)
	requireDeadlockDetect(t, db.Raw)
	// 一个现场同时占用:purge 挡板、持锁方、两个排队事务、轮询 performance_schema 的查询 —— 超过 storetest 默认的 4。
	db.Raw.SetMaxOpenConns(8)
	st := newPlayerNameStore(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), nameRaceBudget)
	sc := &nameRaceScene{ctx: ctx, db: db, st: st, now: playerNameNowMs(), table: store.PlayerNameTableName}
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

	if fx.legacyTable {
		sc.table = legacyPlayerNamePKTable
		_, err := db.Raw.ExecContext(ctx, legacyPlayerNamePKTableDDL)
		require.NoError(t, err, "夹具:建旧结构临时表失败")
	} else {
		// 绿 / 产品路径必须跑在**迁移后**的结构上,否则"零 1213"证明的是另一张表。
		requirePlayerNameKeyedByName(t, sc)
	}

	sc.seedName(t, fx, racePrevOwner, raceContestedDisplay, raceContestedNorm)
	if holder == holderReleasingSuccessorName {
		sc.seedName(t, fx, raceSuccessorOwner, raceSuccessorDisplay, raceSuccessorNorm)
	}

	// purge 挡板必须在任何删除提交**之前**建好 read view。
	purge, err := db.Raw.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	require.NoError(t, err, "夹具:开 purge 挡板事务失败")
	sc.txs = append(sc.txs, purge)
	var n int
	require.NoError(t, purge.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM `"+sc.table+"` WHERE player_id = ?", racePrevOwner).Scan(&n), "夹具:purge 挡板的一致性读失败")

	holderPlayer, holderNorm := racePrevOwner, raceContestedNorm
	if holder == holderReleasingSuccessorName {
		// 被抢的名字先经一次**已提交**的删除释放掉:留下删除标记记录(被挡板留住)。
		sc.deleteName(t, fx, racePrevOwner, raceContestedNorm)
		holderPlayer, holderNorm = raceSuccessorOwner, raceSuccessorNorm
	}
	sc.holder, err = db.Raw.BeginTx(ctx, nil)
	require.NoError(t, err, "夹具:开持锁方事务失败")
	sc.txs = append(sc.txs, sc.holder)
	res, err := sc.holder.ExecContext(ctx, fx.releaseSQL, holderPlayer, holderNorm, 0)
	require.NoError(t, err, "夹具:持锁方的 Release DELETE 失败")
	removed, err := res.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), removed, "夹具:持锁方应恰好删掉 %s 那一行(此时未提交、持有 X)", holderNorm)
	return sc
}

// seedName 给现场种一行。生产表走 store.Reserve(顺带证明生产路径本身能用),临时表走 fx.seedSQL。
func (sc *nameRaceScene) seedName(t *testing.T, fx nameRaceFixture, player uint64, display, norm string) {
	t.Helper()
	if fx.legacyTable {
		_, err := sc.db.Raw.ExecContext(sc.ctx, fx.seedSQL, player, display, norm, sc.now)
		require.NoErrorf(t, err, "夹具:给临时表种 %s 失败", norm)
		return
	}
	outcome, _, err := sc.st.Reserve(sc.ctx, player, display, norm, sc.now)
	require.NoError(t, err)
	require.Equalf(t, store.ReserveInserted, outcome, "夹具:%s 应先归 player %d", norm, player)
}

// deleteName 把一行删掉并提交(留下删除标记记录)。
func (sc *nameRaceScene) deleteName(t *testing.T, fx nameRaceFixture, player uint64, norm string) {
	t.Helper()
	if fx.legacyTable {
		res, err := sc.db.Raw.ExecContext(sc.ctx, fx.releaseSQL, player, norm, 0)
		require.NoError(t, err, "夹具:释放临时表上的 %s 失败", norm)
		n, err := res.RowsAffected()
		require.NoError(t, err)
		require.Equal(t, int64(1), n, "夹具:释放 %s 应删掉一行", norm)
		return
	}
	rel, err := sc.st.Release(sc.ctx, player, norm, 0)
	require.NoError(t, err)
	require.Equalf(t, store.ReleaseDeleted, rel, "夹具:%s 应已释放", norm)
}

// requirePlayerNameKeyedByName 确认生产表真的跑过了主键迁移。
// 这不是可选的:本节绿用例的全部结论都建立在"名字是聚簇主键"之上,跑在旧结构上会得到完全不同的结果
// (生产常态那一路 5/5 成环),而失败信息会把人引向"锁行为与推演不符"这个错误方向。
func requirePlayerNameKeyedByName(t *testing.T, sc *nameRaceScene) {
	t.Helper()
	pkCols, err := store.PrimaryKeyColumnsForTest(sc.ctx, sc.db.Raw, sc.db.Name, store.PlayerNameTableName)
	require.NoError(t, err)
	require.Truef(t, store.PlayerNamePKShapeIsByNameForTest(pkCols),
		"%s 的主键是 %v,不是 name_norm:MigrateSchema 没有把主键换过来,本节的判据不适用于这张表",
		store.PlayerNameTableName, pkCols)
	require.Falsef(t, sc.st.LegacyKeyedByPlayerIDForTest(),
		"store 探到的是旧主键形态,与 INFORMATION_SCHEMA 读出来的 %v 不一致 —— 探测逻辑坏了", pkCols)
}

// awaitQueued 确认 want 个事务排进了本现场那张表的锁等待队列;读不了 performance_schema 时跳过(不代表通过)。
func (sc *nameRaceScene) awaitQueued(t *testing.T, want int, expectation string) {
	t.Helper()
	if err := awaitLockWaiters(sc.ctx, sc.db.Raw, sc.table, want, nameStepBudget); err != nil {
		skipOrFailOnLockWait(t, err, expectation)
	}
}

// observeSuccessorArrangement 记录「后继名字」现场的实际编排:窗口内看到的最大排队事务数,以及持锁方提交前
// 有没有竞争者已经跑完。**只观察、不判红**,结果写进日志并存进 sc.arrangement。
//
// 这里原先是一条硬断言(要求持锁方提交前至少一个竞争者跑完,否则 Fatalf)。删掉它有两个理由:
//   - 它与 player_name_store.go「与其它写者的交错」里刚改准的结论**矛盾**:持锁方删的若是未 purge 的
//     删除标记记录,DELETE 的免间隙优化未必成立,退回 next-key 时竞争者被挡住是引擎的合法行为;
//     原断言会把这种情形报成产品缺陷,并把人引向"name_norm 上又冒出二级唯一索引"这个错误方向。
//   - 它本身**依赖时序**:竞争者跑没跑完取决于 goroutine 何时被调度,拿它判红只会换来偶发假红
//     (AGENTS.md §11.4 要求测试确定、可重复)。
//
// 观察窗口最长 nameStepBudget:有人跑完就立刻返回(常态,毫秒级),没人跑完就等满预算再记一句"都被挡住了"。
// 读锁等待失败也只记进 note —— 本函数不承担定性职责,真的编排坏了会在后面的硬断言上暴露,
// 而那些失败信息都带上了 sc.arrangement。
func (sc *nameRaceScene) observeSuccessorArrangement(t *testing.T, finished [2]<-chan struct{}) {
	t.Helper()
	const pollEvery = 10 * time.Millisecond
	deadline := time.Now().Add(nameStepBudget)
	maxWaiters, anyFinished := 0, false
	waitersNote := ""
	for {
		select {
		case <-finished[0]:
			anyFinished = true
		case <-finished[1]:
			anyFinished = true
		default:
		}
		if waitersNote == "" {
			var waiters int
			if err := sc.db.Raw.QueryRowContext(sc.ctx, lockWaitersQuery, sc.table).Scan(&waiters); err != nil {
				waitersNote = fmt.Sprintf("排队数未能观察(%v)", lockWaitsQueryError(sc.ctx, err))
			} else if waiters > maxWaiters {
				maxWaiters = waiters
			}
		}
		if anyFinished || time.Now().After(deadline) {
			break
		}
		time.Sleep(pollEvery)
	}
	if waitersNote == "" {
		waitersNote = fmt.Sprintf("窗口内 %s 上最多 %d 个事务在等锁", sc.table, maxWaiters)
	}
	outcome := fmt.Sprintf("%v 内两个竞争者一个都没跑完(被持锁方挡住了)", nameStepBudget)
	if anyFinished {
		outcome = "持锁方提交前已有竞争者跑完(与持锁方不相干)"
	}
	sc.arrangement = fmt.Sprintf("后继名字现场(MySQL %s,持锁方正删 %s、被抢的是 %s):%s;%s",
		sc.mysqlVersion, raceSuccessorNorm, raceContestedNorm, outcome, waitersNote)
	t.Logf("编排观察(不判红)—— %s", sc.arrangement)
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

// finishedChans 取两条 stmt 的结束信号,喂给 armContenders(它只认信号,不认载体)。
func finishedChans(w [2]*nameStmt) [2]<-chan struct{} {
	return [2]<-chan struct{}{w[0].finished, w[1].finished}
}

// startContenders 按 oc 的两个竞争者各起一条 stmt。
func (sc *nameRaceScene) startContenders(t *testing.T, stmt, labelPrefix string, oc nameRaceOrderCase) [2]*nameStmt {
	t.Helper()
	return [2]*nameStmt{
		sc.startNameStmt(t, stmt, fmt.Sprintf("%s#A(player=%d)", labelPrefix, oc.contenders[0]), oc.contenders[0]),
		sc.startNameStmt(t, stmt, fmt.Sprintf("%s#B(player=%d)", labelPrefix, oc.contenders[1]), oc.contenders[1]),
	}
}

// armContenders 把两个竞争者摆到"持锁方即将放锁"那一刻,按现场分叉(见 nameRaceHolderCase.queueBehindHolder):
// 确定会排队的那一格硬等到两个都进了锁等待队列(编排没摆到位就判红);另一格只观察、不判红。
// 两条路都把观察结果留在 sc.arrangement 里,供后面的失败信息区分"环成了"与"只是没排上队"。
//
// finished 传竞争者的结束信号,而不是具体的执行载体:语句路径用 nameStmt.finished,
// 产品路径(store.Reserve 跑在自己的 goroutine 里)用它自己的 done 通道,两边共用这一段编排。
func (sc *nameRaceScene) armContenders(t *testing.T, hc nameRaceHolderCase, finished [2]<-chan struct{}, expectation string) {
	t.Helper()
	if hc.queueBehindHolder {
		sc.awaitQueued(t, 2, expectation)
		sc.arrangement = fmt.Sprintf("被抢名字现场(MySQL %s):两个竞争者都排进了 %s 的锁等待队列(硬断言已通过)",
			sc.mysqlVersion, sc.table)
		return
	}
	sc.observeSuccessorArrangement(t, finished)
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
		if err := sc.db.Raw.QueryRowContext(sc.ctx, lockWaitersQuery, sc.table).Scan(&waiters); err != nil {
			skipOrFailOnLockWait(t, lockWaitsQueryError(sc.ctx, err), "观察后完成者是否排在先完成者后面")
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

// nameOwner 读本现场那张表里名字的当前占用者;名字没人占时返回 0。
func nameOwner(t *testing.T, sc *nameRaceScene) uint64 {
	t.Helper()
	var owner uint64
	err := sc.db.Raw.QueryRowContext(sc.ctx,
		"SELECT player_id FROM `"+sc.table+"` WHERE name_norm = ?", raceContestedNorm).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return 0
	}
	require.NoError(t, err)
	return owner
}

// TestPlayerName_ReleaseRacesTwoReservesConverges 是**绿**:生产的 ODKU 写入在未提交的 Release 后面抢同一个名字。
// 3 种 player_id 顺序 × 2 种持锁现场,**一律断言零 1213**,终局都是"恰好一方插入、另一方撞键 no-op"。
//
// 判据为什么是"零"而不是"看顺序分情况"(前几轮的写法):名字现在是**聚簇主键**,两个竞争者的重复检查必然
// 落在**同一条**聚簇记录上,被那把 X **串行化** —— 同一时刻只有一个事务往下走,凑不出"两个排队者各持间隙锁、
// 互挡插入意向"的形状,新项插在删除标记项之前还是之后也就不再是一件事。
// 注意**不能**说成"整条路上没有插入意向锁":第一个拿到 X 的竞争者要去**复活**那条删除标记的聚簇记录,
// 那会把 uk_player_name_owner 上的新 (player_id) 项**插**出来,是一次真插入(完整推导见 player_name_store.go
// 的「与其它写者的交错」)。2026-09-29 真库探针在这三种顺序上各跑 5 次,**0/15 未复现** —— 是"没复现",不是"不可能"。
// 任何一格变红,先查主键是不是被改回 player_id 了(红对照 TestPlayerName_LegacyPrimaryKeyPlainInsertDeadlocks
// 证明的正是那张表上会成环),而不是把断言放宽。
func TestPlayerName_ReleaseRacesTwoReservesConverges(t *testing.T) {
	forEachNameRace(t, func(t *testing.T, hc nameRaceHolderCase, oc nameRaceOrderCase) {
		sc := setupNameRace(t, hc.holder, productionNameRaceFixture)
		w := sc.startContenders(t, store.PlayerNameReserveSQLForTest, "Reserve", oc)
		sc.armContenders(t, hc, finishedChans(w), "持锁方未提交,两个 Reserve 的重复检查都应排在被抢名字那条记录上")

		sc.releaseHolder(t)
		first, second, queued := settleNameRace(t, sc, w)

		for _, s := range []*nameStmt{first, second} {
			require.Falsef(t, isMySQLErrNumber(s.err, store.MySQLErrDeadlockForTest),
				"%s 收到 1213(MySQL %s):名字是聚簇主键之后,抢同一个名字的 ODKU 会被那条聚簇记录的 X 串行化、"+
					"一个一个过(真库探针 3 种顺序各 5 次 = 0/15 未复现)。"+
					"先查两件事:(1) player_name 的主键是不是被改回 player_id 了;(2) 写法是不是被改回普通 INSERT 了"+
					"(那会退回 S→X 升级环,探针实测 15/15)。编排观察: %s。错误: %v",
				s.label, sc.mysqlVersion, sc.arrangement, s.err)
			require.NoErrorf(t, s.err, "%s 出现非预期错误(编排观察: %s)", s.label, sc.arrangement)
		}

		// 排队形状:X 一个一个过。
		require.Equalf(t, int64(1), first.affected, "先完成者 %s 应插入成功(受影响 1 行)", first.label)
		require.Truef(t, queued, "%s 在 %s 提交之前就返回了:重复检查没有取 X、两者并行了 —— 写法被改了?(编排观察: %s)",
			second.label, first.label, sc.arrangement)
		require.Equalf(t, int64(0), second.affected, "后完成者 %s 应撞键走 no-op(受影响 0 行),不许改任何数据", second.label)
		require.NoError(t, second.tx.Commit())

		assert.Equal(t, first.player, nameOwner(t, sc), "名字归插入成功的一方")
		assert.Equal(t, int64(1), sc.db.Count(t, store.PlayerNameTableName, "name_norm = ?", raceContestedNorm))
		assert.Equal(t, int64(0), sc.db.Count(t, store.PlayerNameTableName, "player_id = ?", second.player),
			"撞键方一个字节都不许写进去")
	})
}

// TestPlayerName_LegacyPrimaryKeyPlainInsertDeadlocks 是**红对照**:同一套编排,换成
// "旧结构(PRIMARY KEY(player_id) + UNIQUE(name_norm))+ 普通 INSERT",在用例自建的临时表上跑。
//
// 它不验产品代码,验的是"这套编排确实能把环摆出来" —— 不红的时候,绿用例的"零 1213"就什么都没证明。
//
// 只用**被抢名字**那个持锁现场:后继名字那一路探针实测两个竞争者根本不排队(0/2 排队者),
// 摆不出以持锁方为闸门的交错,放进红对照只会在 awaitQueued 上超时。
//
// 强制判据只给 (b) 竞争者都小于原主人:探针在这一格两轮都是 5/5,而且它正是生产常态
// (号段发的 player_id 小于存量角色的 snowflake id)。(a) 探针是 2/5、(c) 是 5/5 —— 5/5 是 5 次重复而不是证明,
// 硬断言会换来一个偶发假红,所以这两格只记日志。探针自己也写明"现行表结构下成环与否随时序波动"。
func TestPlayerName_LegacyPrimaryKeyPlainInsertDeadlocks(t *testing.T) {
	for _, oc := range nameRaceOrderCases {
		t.Run(oc.name, func(t *testing.T) {
			sc := setupNameRace(t, holderReleasingContestedName, legacyNameRaceFixture)
			w := sc.startContenders(t, legacyPlainInsertPlayerNameSQL, "INSERT", oc)
			sc.awaitQueued(t, 2, "持锁方未提交,两个普通 INSERT 的重复检查都应排进 uk_player_name 上的锁等待队列")

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

			if oc.order == orderBothBelow {
				require.Equalf(t, 1, deadlocked,
					"红对照没有复现死锁(1213=%d 1062=%d,MySQL %s):旧结构上两个竞争者都小于原主人 %d 时,"+
						"两个 S next-key 在放锁那一刻被同时授予,新项都要插在删除标记项之前,插入意向锁被对方"+
						"**排队中**的请求挡住 —— 真库探针在这一格两轮都是 5/5。编排没走到成环形状,"+
						"TestPlayerName_ReleaseRacesTwoReservesConverges 的「零 1213」就失去了对照",
					deadlocked, duplicate, racePrevOwner, sc.mysqlVersion)
				return
			}
			t.Logf("%s:1213=%d 1062=%d(MySQL %s)。这一格不设强制判据 —— 探针实测 (a) 2/5、(c) 5/5,"+
				"而 5/5 是 5 次重复不是证明,旧结构下成环与否随时序波动", oc.name, deadlocked, duplicate, sc.mysqlVersion)
		})
	}
}

// TestPlayerName_ReserveBehindPendingReleaseOutcome 走完整的产品路径 store.Reserve(自动提交,与生产同一隔离级):
// 所有现场与顺序下都必须恰好一个 Inserted、一个 Taken 且 owner 是赢家,返回值里不许出现 error。
//
// 锁冲突重试次数(LockRetriesForTest 的增量)**三种顺序一律断言 0**:名字是聚簇主键之后这条路上的环
// 真库实测 0/15 未复现,所以结果正确不是靠重试把 1213 吸收掉换来的。这正是用户口径"能消掉的环不许用重试兜底"
// 的机械证据 —— 判据要是变成 ">= 1",说明根治没生效,不是断言该放宽。
//
// 编排前提按现场分叉(见 armContenders):只有"持锁方正删被抢的名字"那一格硬等两个 Reserve 排进锁等待队列,
// 后继名字那一格只观察、不判红 —— 那一格排不排队依赖 DELETE 在删除标记记录上取的是不是 next-key,
// 拿它判红会与 player_name_store.go 里的保守结论打架。观察结果进日志,并带进本用例的失败信息。
func TestPlayerName_ReserveBehindPendingReleaseOutcome(t *testing.T) {
	forEachNameRace(t, func(t *testing.T, hc nameRaceHolderCase, oc nameRaceOrderCase) {
		sc := setupNameRace(t, hc.holder, productionNameRaceFixture)
		retriesBefore := sc.st.LockRetriesForTest()

		type result struct {
			player  uint64
			outcome store.ReserveOutcome
			owner   uint64
			err     error
		}
		results := make(chan result, 2)
		finished := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
		for i, p := range oc.contenders {
			done := finished[i]
			sc.pending = append(sc.pending, done)
			go func() {
				defer close(done)
				o, owner, err := sc.st.Reserve(sc.ctx, p, raceContestedDisplay, raceContestedNorm, sc.now+1)
				results <- result{player: p, outcome: o, owner: owner, err: err}
			}()
		}
		// 与语句路径共用同一段编排:确定排队的那一格硬等,后继名字那一格只观察(理由见 armContenders)。
		sc.armContenders(t, hc, [2]<-chan struct{}{finished[0], finished[1]},
			"持锁方未提交,两个 Reserve 都应排在被抢名字那条记录上")
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
			require.NoErrorf(t, r.err, "player %d 的 Reserve 返回了错误(编排观察: %s)", r.player, sc.arrangement)
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
		assert.Equalf(t, uint64(0), retries,
			"抢名字的路径上发生了 %d 次锁冲突重试(胜者 player=%d,原主人 %d,MySQL %s):"+
				"名字是聚簇主键之后这条路上不该再成环(真库探针 3 种顺序各 5 次 = 0/15 未复现),"+
				"重试只该兜锁继承这类 InnoDB 固有情形。先查主键有没有被改回 player_id、写法有没有被改回普通 INSERT,"+
				"不要把这条断言放宽成「至多 1 次」—— 那正是用户明确不接受的「用重试兜掉能消的环」。编排观察: %s",
			retries, winner, racePrevOwner, sc.mysqlVersion, sc.arrangement)

		assert.Equal(t, winner, nameOwner(t, sc))
		assert.Equal(t, int64(1), sc.db.Count(t, store.PlayerNameTableName, "name_norm = ?", raceContestedNorm))
	})
}

// TestPlayerName_ReleaseStatementLocksClusteredKeyFirst 钉住 Release 的取锁顺序(见 Release 的「锁序」):
// Reserve 的 ODKU 从聚簇索引开始,Release 必须同序,否则两者在同一行上反序成环。
// 锁序只取决于执行计划,并发用例只能按概率撞上,而 EXPLAIN 每次都答得出来。
//
// 对生产代码里**同一个 SQL 常量**做 EXPLAIN,断言 key = PRIMARY **且是单行点查**;有人去掉 FORCE INDEX
// (优化器可能改挑 uk_player_name_owner,那就是先二级、后聚簇 = 反序)、或把 name_norm 等值条件从 WHERE 里
// 拿掉(FORCE INDEX 会把它退化成全索引扫描,key 不变、锁覆盖面却从一条记录放大到整张聚簇索引),
// 本用例都会变红。
func TestPlayerName_ReleaseStatementLocksClusteredKeyFirst(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	st := newPlayerNameStore(t, db)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	require.Equalf(t, store.PlayerNameReleaseSQLForTest, st.ReleaseSQLForTest(),
		"迁移之后 store 应当选定新结构那条 Release 语句,实际选的是 %q —— 形态探测或迁移有一个没生效",
		st.ReleaseSQLForTest())

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
	assert.Equal(t, store.PlayerNameReleaseForcedIndexForTest, plan["key"],
		"Release 必须先锁聚簇记录、后锁二级索引项(与 Reserve 的 ODKU 同序):FORCE INDEX (PRIMARY) 被去掉或没生效")

	// 光看 key 不够:WHERE 里丢掉 name_norm 等值条件之后,FORCE INDEX 会退化成**全索引扫描**(type=index),
	// key 仍是 PRIMARY、上面那条断言照样绿,而运行期会对索引上每一项取 next-key ——
	// 锁覆盖面从一条记录变成整张聚簇索引,是比反序取锁严重得多的死锁面。所以访问类型也要钉住。
	//
	// 判据取**否定式**:直接拦住要防的退化形态,而不是枚举合法形态。MySQL 对被修改的表通常不做 const-table
	// 优化,多表 DELETE 的目标表很可能报 type=range(单点范围、rows=1),那是完全正确的单行点查,
	// 用白名单 {const, eq_ref, ref} 会把它判成假红(本用例未在真库跑过,不拿白名单去赌)。
	assert.NotContainsf(t, []string{"index", "ALL"}, plan["type"],
		"Release 的访问类型是 %q:index=全索引扫描 / ALL=全表扫描,都会把锁覆盖面从一条记录放大到整张索引,"+
			"检查 WHERE 是不是丢了 name_norm 等值条件。完整计划: %v", plan["type"], plan)
	// ref 与 rows 二选一即可:MySQL 对 type=const 的行在 ref 列填 "const"、rows 填 "1",
	// 但多表 DELETE 的计划在不同小版本上这两列的填法有出入,任一成立都说明落到了单行点查上。
	assert.Truef(t, plan["ref"] != "" || plan["rows"] == "1",
		"Release 的 EXPLAIN 既没有 ref、rows 也不是 1:等值条件没落到主键上,只是被 FORCE INDEX 拉着扫索引。完整计划: %v",
		plan)
}

// TestPlayerName_MigrationSwitchesPrimaryKeyFromPlayerID 是**迁移本身**的回归:
// 造一张旧结构的 player_name(带几行数据),跑 MigrateSchema,断言主键换成了 name_norm、
// player_id 上有了唯一键、数据一行不少,而且再跑一次是零变更(幂等)。
//
// 为什么必须有这一条:新建库走的是 bootstrap DDL,**永远不会**走到换主键那段代码;
// 存量库才会,而那正是唯一一次不可回头的结构变更。
func TestPlayerName_MigrationSwitchesPrimaryKeyFromPlayerID(t *testing.T) {
	db := storetest.NewEmptyDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// 先手工建出**旧结构**的生产表名,再跑迁移 —— CREATE TABLE IF NOT EXISTS 会跳过建表,
	// 于是迁移必须靠换主键那一步把它救回来。
	legacyDDL := strings.Replace(legacyPlayerNamePKTableDDL, legacyPlayerNamePKTable, store.PlayerNameTableName, 1)
	_, err := db.Raw.ExecContext(ctx, legacyDDL)
	require.NoError(t, err, "夹具:建旧结构的 player_name 失败")

	now := playerNameNowMs()
	seeded := map[uint64]string{4001: "laoyi", 4002: "laoer", 9001: "laosan"}
	for id, norm := range seeded {
		_, err := db.Raw.ExecContext(ctx,
			"INSERT INTO `"+store.PlayerNameTableName+"` (player_id, name, name_norm, created_ms) VALUES (?, ?, ?, ?)",
			id, strings.ToUpper(norm), norm, now)
		require.NoError(t, err, "夹具:种 %s 失败", norm)
	}

	require.NoError(t, store.MigrateSchema(ctx, db.Cfg, store.MigrateOptions{}), "迁移应当把旧结构换成新结构")

	pkCols, err := store.PrimaryKeyColumnsForTest(ctx, db.Raw, db.Name, store.PlayerNameTableName)
	require.NoError(t, err)
	assert.Equal(t, []string{"name_norm"}, pkCols, "迁移之后主键必须是 name_norm(审计 #12 根治的全部前提)")

	var nonUnique int
	var ownerCol string
	require.NoError(t, db.Raw.QueryRowContext(ctx,
		`SELECT NON_UNIQUE, COLUMN_NAME FROM INFORMATION_SCHEMA.STATISTICS
		  WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND INDEX_NAME = ? AND SEQ_IN_INDEX = 1`,
		db.Name, store.PlayerNameTableName, store.PlayerNameOwnerUniqueKey).Scan(&nonUnique, &ownerCol),
		"迁移之后必须建出 %s", store.PlayerNameOwnerUniqueKey)
	assert.Equal(t, 0, nonUnique)
	assert.Equal(t, "player_id", ownerCol)

	// 旧的二级唯一键必须**消失**:留着它,ODKU 的重复扫描会连它一起取 X next-key,把根治掉的环带回来。
	var legacyIdx int
	require.NoError(t, db.Raw.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM INFORMATION_SCHEMA.STATISTICS
		  WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND INDEX_NAME = ?`,
		db.Name, store.PlayerNameTableName, store.PlayerNameLegacyNormUniqueKey).Scan(&legacyIdx))
	assert.Equal(t, 0, legacyIdx, "%s 应当在换主键时被一并 DROP 掉", store.PlayerNameLegacyNormUniqueKey)

	// 数据一行不少(这条 ALTER 会重建整张表,丢行是最不能接受的失败形态)。
	for id, norm := range seeded {
		assert.Equalf(t, int64(1), db.Count(t, store.PlayerNameTableName, "player_id = ? AND name_norm = ?", id, norm),
			"迁移丢了 player %d 的名字 %s", id, norm)
	}
	assert.Equal(t, int64(len(seeded)), db.Count(t, store.PlayerNameTableName, ""))

	// 幂等:生产每次部署都会重复跑 -migrate,第二次必须零变更。
	before := db.ShowCreateTable(t, store.PlayerNameTableName)
	require.NoError(t, store.MigrateSchema(ctx, db.Cfg, store.MigrateOptions{}), "second migration must be a no-op")
	assert.Equal(t, before, db.ShowCreateTable(t, store.PlayerNameTableName), "player_name DDL changed on second migration")

	// 迁移之后 store 必须探到新形态,并选新结构那条 Release 语句。
	st, err := store.NewPlayerNameStore(db.Cfg)
	require.NoError(t, err)
	defer st.Close()
	assert.False(t, st.LegacyKeyedByPlayerIDForTest(), "迁移之后不该再被探成旧形态")
	assert.Equal(t, store.PlayerNameReleaseSQLForTest, st.ReleaseSQLForTest())

	// 迁移之后名字唯一性仍然由库兜住(换的是键的位置,不是有没有这条约束)。
	_, err = db.Raw.ExecContext(ctx,
		"INSERT INTO `"+store.PlayerNameTableName+"` (player_id, name, name_norm, created_ms) VALUES (?, ?, ?, ?)",
		7777, "LaoYi", "laoyi", now)
	require.Error(t, err, "迁移之后重名必须仍被库拒绝")
	assert.Truef(t, isMySQLErrNumber(err, store.MySQLErrDupEntryForTest), "重名应当是 1062,实际: %v", err)
}

// TestPlayerName_MigrationRefusesUnknownPrimaryKeyShape:主键既不是 name_norm 也不是 player_id 时,
// 迁移必须**停住**而不是猜着下发 ALTER。看不懂的形状上继续,要么删错索引,要么建出一张两种语句都不匹配的表。
func TestPlayerName_MigrationRefusesUnknownPrimaryKeyShape(t *testing.T) {
	db := storetest.NewEmptyDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// 复合主键 (player_id, name_norm):两条唯一性看起来都"在",但谁也不是单列唯一 —— 一个人能占多个名字。
	_, err := db.Raw.ExecContext(ctx, "CREATE TABLE `"+store.PlayerNameTableName+"` (\n"+
		"  `player_id` bigint unsigned NOT NULL DEFAULT 0,\n"+
		"  `name` VARCHAR(64) NOT NULL DEFAULT '',\n"+
		"  `name_norm` VARCHAR(191) NOT NULL,\n"+
		"  `created_ms` bigint unsigned NOT NULL DEFAULT 0,\n"+
		"  PRIMARY KEY (`player_id`, `name_norm`)\n"+
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin")
	require.NoError(t, err)

	err = store.MigrateSchema(ctx, db.Cfg, store.MigrateOptions{})
	require.Error(t, err, "认不出的主键形状上必须 fail-closed,不许猜着 ALTER")
	assert.Contains(t, err.Error(), store.PlayerNameTableName)

	// 顺带证明 store 也不会在这种表上建出来(两条判据同口径,免得一处拦一处放)。
	_, err = store.NewPlayerNameStore(db.Cfg)
	assert.Error(t, err, "形态认不出来时 NewPlayerNameStore 必须拒绝(CreatePlayer 当场 fail-closed)")
}

// TestPlayerName_LegacyShapeStorePicksLegacyRelease:迁移还没跑过的库上(主键仍是 player_id),
// store 必须探到旧形态并改用带 FORCE INDEX (uk_player_name) 的旧 Release 语句 ——
// 在旧表上发新语句 = 与 Reserve 反序取锁,凭空多出一类两方环。
//
// 这同时是"新二进制 + 旧表结构"这个并存窗口的行为证据:不拒启动、功能正确、锁序仍然一致。
func TestPlayerName_LegacyShapeStorePicksLegacyRelease(t *testing.T) {
	db := storetest.NewEmptyDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	legacyDDL := strings.Replace(legacyPlayerNamePKTableDDL, legacyPlayerNamePKTable, store.PlayerNameTableName, 1)
	_, err := db.Raw.ExecContext(ctx, legacyDDL)
	require.NoError(t, err, "夹具:建旧结构的 player_name 失败")

	st, err := store.NewPlayerNameStore(db.Cfg)
	require.NoError(t, err, "旧结构不该拒绝建 store:它功能正确,只是更容易成环(TiDB 上迁移会跳过,拒启动等于让 player_name 不可用)")
	defer st.Close()
	assert.True(t, st.LegacyKeyedByPlayerIDForTest())
	assert.Equal(t, store.PlayerNameReleaseLegacySQLForTest, st.ReleaseSQLForTest(),
		"旧结构上必须用带 FORCE INDEX (%s) 的那条 Release:发新语句会与 Reserve 反序取锁",
		store.PlayerNameLegacyNormUniqueKey)

	// 对外语义在旧结构上一个字都不变(Reserve 四种终局里的三种 + Release 幂等)。
	now := playerNameNowMs()
	outcome, owner, err := st.Reserve(ctx, 4001, "LaoYi", "laoyi", now)
	require.NoError(t, err)
	require.Equal(t, store.ReserveInserted, outcome)
	require.Zero(t, owner)

	outcome, owner, err = st.Reserve(ctx, 9001, "LAOYI", "laoyi", now)
	require.NoError(t, err)
	assert.Equal(t, store.ReserveTaken, outcome)
	assert.Equal(t, uint64(4001), owner)

	rel, err := st.Release(ctx, 4001, "laoyi", 0)
	require.NoError(t, err)
	assert.Equal(t, store.ReleaseDeleted, rel)
	rel, err = st.Release(ctx, 4001, "laoyi", 0)
	require.NoError(t, err)
	assert.Equal(t, store.ReleaseAbsent, rel, "幂等")
}

// TestPlayerName_MigrationConvergesLeftoverKeyShape:**"主键对了"不等于"形状对了"**。
//
// 造的是运维照注释里的反向 ALTER **只回滚了一半**(或手工只执行了 ADD PRIMARY KEY)之后的中间态:
// PRIMARY KEY(name_norm) 已经换好,但旧的 uk_player_name(name_norm) 还留着、uk_player_name_owner 还没建。
// 这个形态在本次改动之前会一路静默通过:迁移在"主键已是 name_norm"的快路径直接 return nil,
// assertUniqueColumns 只问"这一列上有没有单列唯一约束"(PRIMARY 就满足),NewPlayerNameStore 也只看主键形态 ——
// 全链路零报错,而多出来的那条二级唯一索引会把根治掉的死锁形状带回来,缺的那条键会让 Reserve 的
// ID 复用检测静默失效。所以迁移必须在这一支上**收敛**,而不是跳过。
func TestPlayerName_MigrationConvergesLeftoverKeyShape(t *testing.T) {
	db := storetest.NewEmptyDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// 先建旧结构,再执行"回滚做了一半"的那一半:换主键、但不 DROP 旧键、也不建 owner 键。
	legacyDDL := strings.Replace(legacyPlayerNamePKTableDDL, legacyPlayerNamePKTable, store.PlayerNameTableName, 1)
	_, err := db.Raw.ExecContext(ctx, legacyDDL)
	require.NoError(t, err, "夹具:建旧结构的 player_name 失败")

	now := playerNameNowMs()
	seeded := map[uint64]string{4101: "banjiu", 9101: "banshi"}
	for id, norm := range seeded {
		_, err := db.Raw.ExecContext(ctx,
			"INSERT INTO `"+store.PlayerNameTableName+"` (player_id, name, name_norm, created_ms) VALUES (?, ?, ?, ?)",
			id, strings.ToUpper(norm), norm, now)
		require.NoError(t, err, "夹具:种 %s 失败", norm)
	}

	_, err = db.Raw.ExecContext(ctx, "ALTER TABLE `"+store.PlayerNameTableName+
		"` DROP PRIMARY KEY, ADD PRIMARY KEY (`name_norm`)")
	require.NoError(t, err, "夹具:摆出「主键换了、旧键还在、owner 键没建」的中间态失败")

	// 夹具自检:确实是那个中间态,而不是被 MySQL 顺手整理过。
	var leftover int
	require.NoError(t, db.Raw.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM INFORMATION_SCHEMA.STATISTICS
		  WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND INDEX_NAME = ?`,
		db.Name, store.PlayerNameTableName, store.PlayerNameLegacyNormUniqueKey).Scan(&leftover))
	require.Equal(t, 1, leftover, "夹具:中间态里旧的 %s 必须还在", store.PlayerNameLegacyNormUniqueKey)

	require.NoError(t, store.MigrateSchema(ctx, db.Cfg, store.MigrateOptions{}),
		"主键已对但形状没收敛时,迁移必须把它收敛掉,而不是在快路径直接跳过")

	pkCols, err := store.PrimaryKeyColumnsForTest(ctx, db.Raw, db.Name, store.PlayerNameTableName)
	require.NoError(t, err)
	assert.Equal(t, []string{"name_norm"}, pkCols)

	require.NoError(t, db.Raw.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM INFORMATION_SCHEMA.STATISTICS
		  WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND COLUMN_NAME = 'name_norm' AND INDEX_NAME <> 'PRIMARY'`,
		db.Name, store.PlayerNameTableName).Scan(&leftover))
	assert.Equal(t, 0, leftover,
		"残留的 name_norm 二级唯一索引必须被 DROP 掉:留着它,Reserve 插入新名字时会多走一次二级唯一索引的插入路径,"+
			"把改主键消掉的死锁形状带回来(而且功能全对、零报错)")

	var nonUnique int
	var ownerCol string
	require.NoError(t, db.Raw.QueryRowContext(ctx,
		`SELECT NON_UNIQUE, COLUMN_NAME FROM INFORMATION_SCHEMA.STATISTICS
		  WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND INDEX_NAME = ? AND SEQ_IN_INDEX = 1`,
		db.Name, store.PlayerNameTableName, store.PlayerNameOwnerUniqueKey).Scan(&nonUnique, &ownerCol),
		"缺失的 %s 必须被补建", store.PlayerNameOwnerUniqueKey)
	assert.Equal(t, 0, nonUnique)
	assert.Equal(t, "player_id", ownerCol)

	for id, norm := range seeded {
		assert.Equalf(t, int64(1), db.Count(t, store.PlayerNameTableName, "player_id = ? AND name_norm = ?", id, norm),
			"收敛键形状丢了 player %d 的名字 %s", id, norm)
	}

	// 幂等:收敛过的表再跑一次必须零变更(收敛这一步每次启动都会走到)。
	before := db.ShowCreateTable(t, store.PlayerNameTableName)
	require.NoError(t, store.MigrateSchema(ctx, db.Cfg, store.MigrateOptions{}), "second migration must be a no-op")
	assert.Equal(t, before, db.ShowCreateTable(t, store.PlayerNameTableName), "player_name DDL changed on second migration")

	st, err := store.NewPlayerNameStore(db.Cfg)
	require.NoError(t, err)
	defer st.Close()
	assert.False(t, st.LegacyKeyedByPlayerIDForTest())
	assert.Equal(t, store.PlayerNameReleaseSQLForTest, st.ReleaseSQLForTest())
}

// TestPlayerName_MigrationCanSkipPrimaryKeySwitchByOption:换主键那条 ALTER 要重建整张表,
// 表太大 / 窗口不够 / 反复被 MDL 挡住时,运维必须能"先把其余迁移跑完、这一条稍后单独做" ——
// 否则唯一的选择是整条 -migrate 都不跑,连带停掉另外四张表的列同步与 id_segment 地板校验。
//
// 打开这个开关的代价写在选项注释里:本实例停在旧形态,名字抢注在生产常态那一路仍然成环(实测 5/5),
// 靠有界 1213 重试吸收。所以本用例同时钉住"退路是完整的":store 必须能在这张表上建出来并选**旧**语句。
func TestPlayerName_MigrationCanSkipPrimaryKeySwitchByOption(t *testing.T) {
	db := storetest.NewEmptyDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	legacyDDL := strings.Replace(legacyPlayerNamePKTableDDL, legacyPlayerNamePKTable, store.PlayerNameTableName, 1)
	_, err := db.Raw.ExecContext(ctx, legacyDDL)
	require.NoError(t, err, "夹具:建旧结构的 player_name 失败")

	require.NoError(t, store.MigrateSchema(ctx, db.Cfg, store.MigrateOptions{AllowLegacyPlayerNamePrimaryKey: true}),
		"开关打开时整条迁移必须跑完(其余四张表的列同步与地板校验是无关且必要的)")

	pkCols, err := store.PrimaryKeyColumnsForTest(ctx, db.Raw, db.Name, store.PlayerNameTableName)
	require.NoError(t, err)
	assert.Equal(t, []string{"player_id"}, pkCols, "开关打开时**不**换主键,表保持旧形态")

	st, err := store.NewPlayerNameStore(db.Cfg)
	require.NoError(t, err, "旧形态下 store 必须仍然建得出来,否则这个逃生口等于关掉了建角")
	defer st.Close()
	assert.True(t, st.LegacyKeyedByPlayerIDForTest())
	assert.Equal(t, store.PlayerNameReleaseLegacySQLForTest, st.ReleaseSQLForTest(),
		"旧形态下必须自动退回带 FORCE INDEX (%s) 的旧 Release 语句,锁序才与 Reserve 一致",
		store.PlayerNameLegacyNormUniqueKey)

	// 关掉开关重跑一次就该根治 —— 逃生口是"推迟",不是"永久跳过"。
	require.NoError(t, store.MigrateSchema(ctx, db.Cfg, store.MigrateOptions{}))
	pkCols, err = store.PrimaryKeyColumnsForTest(ctx, db.Raw, db.Name, store.PlayerNameTableName)
	require.NoError(t, err)
	assert.Equal(t, []string{"name_norm"}, pkCols)
}

// ── 「残余 B2」:Reserve(P, N2) × Release(P, N1) 这条**改主键新引入**的反序对 ──────────────
//
// 这一对是同一个 player_id 的**两个不同名字**,走的是 uk_player_name_owner 那条二级唯一索引。
// 2026-09-29 的 v3 探针已经把它在真库上跑过:两个写者直接对撞,现行主键与候选主键两种结构各 20 次,
// **0/20 未复现 1213**(同批还测了 B1"并发登记两个不同名字",同样 0/20)。所以下面的推演目前是
// "**推演成环、实测未复现**",本用例仍然只记录不断言 —— 20 次不是证明,换 MySQL 版本时拿它复核。
// 推演(完整版在 player_name_store.go 的「与其它写者的交错」):
//   - Reserve(P,N2):聚簇 N2 → 二级 (P) → 聚簇 N1(ODKU 顺着二级项定位到旧名字那一行做 no-op 更新);
//   - Release(P,N1):聚簇 N1 → 二级 (P)(删行时连带把 owner 项删除标记)。
//
// 在 (P) 与 N1 这两处恰好互为反序,两方即可成环。
//
// # 编排为什么要把 Release 切成两步
//
// 生产的 Release 是**一条**自动提交的 DELETE,停不在"已拿到聚簇 N1、还没去要二级 (P)"那一刻,
// 而那正是成环必需的交错。所以这里用 `SELECT ... FORCE INDEX (PRIMARY) ... FOR UPDATE` 先把聚簇 N1 的 X
// 拿到手(与 DELETE 在这一步取的锁同形:唯一索引 + 唯一等值条件 → X 记录锁),把两个竞争者摆好之后,
// 再用**生产那一段 SQL 文本**发真正的 DELETE。
// 也就是说:本用例证明的是"这两条语句的取锁顺序互为反序、能成环",**不是**"生产上多久会撞上一次" ——
// 真实并发下这个交错是概率事件,而正常路径根本到不了(可达性论证见 player_name_store.go,
// 要 ID 复用事故叠加 login 延迟约 10s 的第二次补偿 Release)。
//
// # 为什么**不设**成环与否的强制判据
//
// 两个方向的断言现在都会说谎:断言"必成环"——v3 探针 0/20 直接把它证伪了;断言"不成环"——20 次复现不出来
// 不等于不存在,把它写死就是拿样本当证明。所以本用例只做三件事:
//  1. require 编排确实摆到了预期交错(竞争者进了锁等待队列)—— 这一步失败说明取锁顺序与推演不符,是真信息;
//  2. require 两条语句都终结、且错误只可能是 1213(出现别的错误 = 编排坏了);
//  3. t.Logf 报出 1213 的计数,交给编排方回报。
//
// 拿到实测之后的两条路都已经写在 player_name_store.go 里:确认成环 → 候选根治是把 Release 也改成
// FORCE INDEX (uk_player_name_owner)(两者在 (P) 上同序),但那会让探针 0/15 那份证据失效,必须连
// "抢同一个名字"那组形状一起重测;确认不成环 → 把「残余 B2」从清单里降级并记上实测出处。
const (
	reverseOwner               uint64 = 8001
	reverseOldDisplay                 = "LaoJiu"
	reverseOldNorm                    = "laojiu" // N1:先登记好、随后被 Release 删的那个
	reverseNewDisplay                 = "LaoShi"
	reverseNewNorm                    = "laoshi" // N2:同一个 player_id 去登记的另一个名字
	reverseHoldClusteredKeySQL        = "SELECT player_id FROM `" + store.PlayerNameTableName +
		"` FORCE INDEX (PRIMARY) WHERE name_norm = ? FOR UPDATE"
)

func TestPlayerName_ReserveOtherNameRacesReleaseSameOwner(t *testing.T) {
	db := storetest.NewMigratedDB(t)
	requireGlobalRepeatableRead(t, db)
	requireDeadlockDetect(t, db.Raw)
	// purge 这里不需要挡板(两条语句碰的都是活行),但仍要给编排留出连接:两个事务 + 轮询锁等待。
	db.Raw.SetMaxOpenConns(8)
	st := newPlayerNameStore(t, db)

	ctx, cancel := context.WithTimeout(context.Background(), nameRaceBudget)
	defer cancel()
	var mysqlVersion string
	require.NoError(t, db.Raw.QueryRowContext(ctx, `SELECT VERSION()`).Scan(&mysqlVersion))
	requirePlayerNameKeyedByName(t, &nameRaceScene{ctx: ctx, db: db, st: st})

	now := playerNameNowMs()
	outcome, _, err := st.Reserve(ctx, reverseOwner, reverseOldDisplay, reverseOldNorm, now)
	require.NoError(t, err)
	require.Equal(t, store.ReserveInserted, outcome, "夹具:%s 应先归 player %d", reverseOldNorm, reverseOwner)

	// T1:Release 的第一步 —— 拿到聚簇 N1 的 X(与 DELETE 在这一步取的锁同形),停在这里。
	releaseTx, err := db.Raw.BeginTx(ctx, nil)
	require.NoError(t, err, "夹具:开 Release 事务失败")
	defer func() { _ = releaseTx.Rollback() }()
	var held uint64
	require.NoError(t, releaseTx.QueryRowContext(ctx, reverseHoldClusteredKeySQL, reverseOldNorm).Scan(&held),
		"夹具:锁住 %s 那条聚簇记录失败", reverseOldNorm)
	require.Equal(t, reverseOwner, held)

	// T2:同一个 player_id 登记另一个名字。它会先把 N2 插进聚簇索引,再在 uk_player_name_owner 的 (P) 项上
	// 取 X,然后顺着那条二级项去要**聚簇 N1** 的 X —— 正好撞在 T1 手里,于是卡住。
	reserveTx, err := db.Raw.BeginTx(ctx, nil)
	require.NoError(t, err, "夹具:开 Reserve 事务失败")
	defer func() { _ = reserveTx.Rollback() }()
	var reserveErr error
	var reserveAffected int64
	reserveDone := make(chan struct{})
	go func() {
		defer close(reserveDone)
		res, e := reserveTx.ExecContext(ctx, store.PlayerNameReserveSQLForTest,
			reverseOwner, reverseNewDisplay, reverseNewNorm, now+1)
		if e == nil {
			reserveAffected, e = res.RowsAffected()
		}
		reserveErr = e
	}()

	if waitErr := awaitLockWaiters(ctx, db.Raw, store.PlayerNameTableName, 1, nameStepBudget); waitErr != nil {
		skipOrFailOnLockWait(t, waitErr,
			"Reserve(P,N2) 应当持着 uk_player_name_owner 上的 (P) 项、卡在聚簇 N1 的 X 上;"+
				"它要是没卡住,说明 ODKU 定位重复行的取锁顺序与推演不符 —— 那本身就是要回报的结论")
	}

	// T1 的第二步:发**生产那一段** Release。它已持有聚簇 N1,接着要去删 (P) 那条二级项 —— 而那把 X 在 T2 手里。
	var releaseErr error
	var releaseAffected int64
	releaseDone := make(chan struct{})
	go func() {
		defer close(releaseDone)
		res, e := releaseTx.ExecContext(ctx, store.PlayerNameReleaseSQLForTest, reverseOwner, reverseOldNorm, uint64(0))
		if e == nil {
			releaseAffected, e = res.RowsAffected()
		}
		releaseErr = e
	}()

	// Release 两条路上都会返回:成环时它要么是牺牲者、要么在对方被牺牲后前进;不成环时它根本不必等。
	select {
	case <-releaseDone:
	case <-time.After(nameStepBudget):
		t.Fatalf("%v 内 Release 没有返回,而 Reserve 仍在等它 —— 两者互等却没有被 InnoDB 判成死锁?"+
			"先确认 innodb_deadlock_detect 真的是 ON(MySQL %s)", nameStepBudget, mysqlVersion)
	}
	// 放掉 Release 的锁,让仍在排队的 Reserve 走完(它若已是牺牲者,这两步都无害)。
	_ = releaseTx.Commit()
	select {
	case <-reserveDone:
	case <-time.After(nameStepBudget):
		t.Fatalf("Release 返回并提交之后 %v 内 Reserve 仍没有返回(MySQL %s)", nameStepBudget, mysqlVersion)
	}
	_ = reserveTx.Commit()

	deadlocks := 0
	for _, e := range []struct {
		label string
		err   error
	}{{"Release(P,N1)", releaseErr}, {"Reserve(P,N2)", reserveErr}} {
		switch {
		case e.err == nil:
		case isMySQLErrNumber(e.err, store.MySQLErrDeadlockForTest):
			deadlocks++
		default:
			t.Fatalf("%s 出现了既不是成功、也不是 1213 的错误,编排坏了: %v", e.label, e.err)
		}
	}

	// 这条是本用例唯一的"结论",而且刻意只进日志:编排方把它回报之后,才谈得上收紧或降级。
	t.Logf("【残余 B2 实测】Reserve(P,N2) × Release(P,N1):1213=%d(Release affected=%d err=%v;"+
		"Reserve affected=%d err=%v;MySQL %s)。1213>0 = 反序对确实成环,按 player_name_store.go"+
		"「残余」一节的候选方案(Release 改钉 %s)重测两组形状;1213=0 = 推演偏保守,把该条降级并记上本次出处。",
		deadlocks, releaseAffected, releaseErr, reserveAffected, reserveErr, mysqlVersion, store.PlayerNameOwnerUniqueKey)

	// 无论成没成环,库里都不许出现"一个 player_id 占两个名字"(那条唯一键就是拦它的)。
	assert.LessOrEqual(t, db.Count(t, store.PlayerNameTableName, "player_id = ?", reverseOwner), int64(1),
		"同一个 player_id 占了多个名字:%s 失效了", store.PlayerNameOwnerUniqueKey)
}
