//go:build integration

package store_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"data_service/internal/store"
	"data_service/internal/store/storetest"

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
//  4. Release 的条件删除:player_id 与 name_norm 都要对上,窗口外必须拒绝。

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
