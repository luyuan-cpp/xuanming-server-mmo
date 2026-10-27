package kafka

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"db/internal/logic/pkg/proto_sql"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/stretchr/testify/require"
	"os"
	assetpb "proto/common/asset"
	"shared/assetop"
	"shared/ownerepoch"
	"shared/savebarrier"
	"sync"
	"testing"
	"time"

	db_config "db/internal/config"

	"github.com/go-sql-driver/mysql"
	"github.com/luyuancpp/proto2mysql"
	"google.golang.org/protobuf/proto"
	componentpb "proto/common/component"
	playerpb "proto/common/database"
	dbpb "proto/db"
	"shared/authoritysql"
)

// 只在一次性随机库验证；不借用已有 zone/player 库，不改共享 SQL/Redis 数据。
func openAssetFenceFixture(t *testing.T) *proto2mysql.DB {
	t.Helper()
	dsn := os.Getenv("ASSET_FENCE_TEST_DSN")
	if dsn == "" {
		t.Skip("未配置独立 ASSET_FENCE_TEST_DSN")
	}
	parsed, err := mysql.ParseDSN(dsn)
	if err != nil || parsed.DBName != "" {
		t.Fatal("fixture DSN 必须不指定数据库")
	}
	admin, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	var nonce [8]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	name := "asset_fence_" + hex.EncodeToString(nonce[:])
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err = admin.ExecContext(ctx, "CREATE DATABASE `"+name+"`"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec("DROP DATABASE `" + name + "`"); err != nil {
			t.Errorf("清理 fixture 库失败: %v", err)
		}
	})
	parsed.DBName = name
	handle, err := openFenceAdminPool(t, ctx, parsed.FormatDSN(), name, os.Getenv("ASSET_FENCE_REQUIRE_TIDB") == "1", 8, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = handle.Close() })
	model := proto2mysql.NewDB()
	if err = model.OpenDB(handle, name); err != nil {
		t.Fatal(err)
	}
	// 这些用例验证 dual 下的行内围栏；legacy 的更新保留字段由专项真库用例验证。
	if err := SetFencingMode(db_config.FencingModeDual); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = SetFencingMode(db_config.FencingModeLegacy) })
	model.RegisterTable(&playerpb.PlayerDatabase{})
	model.RegisterTable(&playerpb.PlayerDatabase_1{})
	if err = model.CreateOrUpdateTable(&playerpb.PlayerDatabase{}); err != nil {
		t.Fatal(err)
	}
	if err = model.CreateOrUpdateTable(&playerpb.PlayerDatabase_1{}); err != nil {
		t.Fatal(err)
	}
	return model
}

func fenceRow(balance uint64) *playerpb.PlayerDatabase {
	return &playerpb.PlayerDatabase{PlayerId: 42, Currency: &componentpb.CurrencyComp{Values: []uint64{0, balance}}}
}

var errPlayerRowStale = errors.New("测试期拒写结局适配")

func writeFence(t *testing.T, model *proto2mysql.DB, epoch, seq, balance uint64) error {
	t.Helper()
	handled, outcome, err := savePlayerRow(context.Background(), model, &dbpb.DBTask{Key: 42, OwnerEpoch: epoch, SaveSeq: seq}, fenceRow(balance))
	if !handled {
		t.Fatal("玩家行未进入 SQL 围栏")
	}
	if err == nil && !outcome.publishes() {
		return errPlayerRowStale
	}
	return err
}

func TestIntegrationPlayerSQLFenceRejectsLateAndConflictingWrites(t *testing.T) {
	model := openAssetFenceFixture(t)
	if err := writeFence(t, model, 10, 1, 100); err != nil {
		t.Fatal(err)
	}
	if err := writeFence(t, model, 10, 1, 100); err != nil {
		t.Fatalf("同版本同载荷应幂等: %v", err)
	}
	// 同 (epoch, seq) 按幂等生产者口径视为重放，不比内容（v0.4 §14 第 12 条），以先提交的 SQL 行为准。
	if err := writeFence(t, model, 10, 1, 99); err != nil {
		t.Fatalf("同版本重放不得报错: %v", err)
	}
	replayed := &playerpb.PlayerDatabase{PlayerId: 42}
	if err := model.FindOneByPK(replayed); err != nil {
		t.Fatal(err)
	}
	if replayed.Currency.Values[1] != 100 {
		t.Fatalf("同版本重放不得改写 SQL 行: %v", replayed)
	}
	if err := writeFence(t, model, 10, 2, 80); err != nil {
		t.Fatal(err)
	}
	for _, v := range [][2]uint64{{10, 1}, {0, 0}, {9, 999999}} {
		if err := writeFence(t, model, v[0], v[1], 999); !errors.Is(err, errPlayerRowStale) {
			t.Fatalf("旧存盘必须拒绝 %v: %v", v, err)
		}
	}
	if err := writeFence(t, model, 11, 1, 70); err != nil {
		t.Fatal(err)
	}
	if err := writeFence(t, model, 10, 999999, 999); !errors.Is(err, errPlayerRowStale) {
		t.Fatalf("旧 owner 的大 seq 不能反超: %v", err)
	}
	got := &playerpb.PlayerDatabase{PlayerId: 42}
	if err := model.FindOneByPK(got); err != nil {
		t.Fatal(err)
	}
	if got.FenceOwnerEpoch != 11 || got.FenceSaveSeq != 1 || got.Currency.Values[1] != 70 {
		t.Fatalf("SQL 状态被旧写覆盖: %v", got)
	}
}

func TestIntegrationPlayerSQLFenceSerializesConcurrentSaves(t *testing.T) {
	model := openAssetFenceFixture(t)
	if err := writeFence(t, model, 12, 1, 1); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	failures := make(chan error, 16)
	for seq := uint64(2); seq <= 17; seq++ {
		group.Add(1)
		go func(seq uint64) {
			defer group.Done()
			_, _, err := savePlayerRow(context.Background(), model, &dbpb.DBTask{Key: 42, OwnerEpoch: 12, SaveSeq: seq}, fenceRow(seq))
			if err != nil && !errors.Is(err, errPlayerRowStale) {
				failures <- err
			}
		}(seq)
	}
	group.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	got := &playerpb.PlayerDatabase{PlayerId: 42}
	if err := model.FindOneByPK(got); err != nil {
		t.Fatal(err)
	}
	if got.FenceSaveSeq != 17 || got.Currency.Values[1] != 17 {
		t.Fatalf("最终只能保留最大保存版本: %v", got)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	before := proto.Clone(got)
	if _, _, err := savePlayerRow(cancelled, model, &dbpb.DBTask{Key: 42, OwnerEpoch: 12, SaveSeq: 18}, fenceRow(18)); err == nil {
		t.Fatal("取消的写不得提交")
	}
	got = &playerpb.PlayerDatabase{PlayerId: 42}
	if err := model.FindOneByPK(got); err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(before, got) {
		t.Fatal("取消改变了 SQL 状态")
	}
}

func fenceRowFor(playerID, balance uint64) *playerpb.PlayerDatabase {
	return &playerpb.PlayerDatabase{PlayerId: playerID, Currency: &componentpb.CurrencyComp{Values: []uint64{0, balance}}}
}

// 不同新号并发首次存盘不得跨玩家死锁（RR 下锁定读未命中取间隙锁 → 两边插入意向锁互等 → 1213）。
// 空表（压测清库后的形态）与非空表各跑一轮；旧实现在两种形态下都能真库复现 1213。
func TestIntegrationPlayerSQLFenceConcurrentFirstSavesOfDifferentPlayersDoNotDeadlock(t *testing.T) {
	model := openAssetFenceFixture(t)
	runRound := func(base uint64) {
		const players = 16
		var group sync.WaitGroup
		start := make(chan struct{})
		failures := make(chan error, players)
		for i := uint64(0); i < players; i++ {
			group.Add(1)
			go func(playerID uint64) {
				defer group.Done()
				<-start
				_, _, err := savePlayerRow(context.Background(), model, &dbpb.DBTask{Key: playerID, OwnerEpoch: 7, SaveSeq: 1}, fenceRowFor(playerID, playerID))
				if err != nil {
					failures <- err
				}
			}(base + i)
		}
		close(start)
		group.Wait()
		close(failures)
		for err := range failures {
			t.Errorf("新号首次存盘失败（疑似跨玩家死锁）: %v", err)
		}
		for i := uint64(0); i < players; i++ {
			got := &playerpb.PlayerDatabase{PlayerId: base + i}
			if err := model.FindOneByPK(got); err != nil {
				t.Fatalf("player %d 未落库: %v", base+i, err)
			}
			if got.FenceOwnerEpoch != 7 || got.FenceSaveSeq != 1 || got.Currency.Values[1] != base+i {
				t.Fatalf("player %d 落库内容不对: %v", base+i, got)
			}
		}
	}
	runRound(1000) // 空表
	runRound(500)  // 非空表，新号落在已有行之间的间隙
}

// 同一新号的多笔首写并发：只有一笔走插入，其余撞 1062 后转锁定比较，最终保留最大版本。
func TestIntegrationPlayerSQLFenceConcurrentFirstSavesOfSamePlayerKeepMaxVersion(t *testing.T) {
	model := openAssetFenceFixture(t)
	const playerID = 77
	var group sync.WaitGroup
	start := make(chan struct{})
	failures := make(chan error, 8)
	for seq := uint64(1); seq <= 8; seq++ {
		group.Add(1)
		go func(seq uint64) {
			defer group.Done()
			<-start
			task := &dbpb.DBTask{Key: playerID, OwnerEpoch: 3, SaveSeq: seq}
			_, _, err := savePlayerRow(context.Background(), model, task, fenceRowFor(playerID, seq))
			// 已登记的残余窗口（v0.4 §4.8 先行修复注）：同一玩家首个自动提交 INSERT 回滚、后面排着 ≥2 笔时
			// 会 1213。生产上同一玩家的写由 key 分片加排序租约串行到不了；这里 8 路故意并发，按管线的重试吸收一次。
			var myErr *mysql.MySQLError
			if errors.As(err, &myErr) && myErr.Number == 1213 {
				_, _, err = savePlayerRow(context.Background(), model, task, fenceRowFor(playerID, seq))
			}
			if err != nil && !errors.Is(err, errPlayerRowStale) {
				failures <- err
			}
		}(seq)
	}
	close(start)
	group.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	got := &playerpb.PlayerDatabase{PlayerId: playerID}
	if err := model.FindOneByPK(got); err != nil {
		t.Fatal(err)
	}
	if got.FenceSaveSeq != 8 || got.Currency.Values[1] != 8 {
		t.Fatalf("最终只能保留最大保存版本: %v", got)
	}
}

// 真实 SQL 资产行先提交，再模拟令牌易主：没有缓存/证据/游标；重投 equal 后才一起发布。
// Redis 只用隔离 miniredis 执行真实 Lua，SQL 从未由 handler 替身冒充成功。
func TestIntegrationSQLCommitLeaseLossReplayAndOldEpochEvidence(t *testing.T) {
	model := openAssetFenceFixture(t)
	w, mr, _ := newTestWorker(t, 0)
	storesOf(t, w).stores[testZoneID] = &proto_sql.GameDB{SqlModel: model}
	dbOpHandlers[dbpb.DBOp_DB_OP_WRITE] = handleDBWriteOp
	ledger := &componentpb.PlayerAssetOpLedgerComp{Streams: []*componentpb.AssetOpStreamLedger{{StreamEpoch: 1, MaxSeq: 1, SeenBits: []uint64{1}, AppliedBits: []uint64{1}, Receipts: []*componentpb.AssetOpReceipt{{Seq: 1, RequestDigest: bytes.Repeat([]byte{0x31}, 32), CorrelationId: 91}}}}}
	row := fenceRow(100)
	row.AssetOpLedger = ledger
	makeTask := func(epoch, saveSeq, cursor, balance uint64) *workerTask {
		next := proto.Clone(row).(*playerpb.PlayerDatabase)
		next.Currency.Values[1] = balance
		raw, err := proto.Marshal(next)
		require.NoError(t, err)
		return &workerTask{dbTask: &dbpb.DBTask{Key: 42, Op: dbpb.DBOp_DB_OP_WRITE, MsgType: "player_database", TaskId: fmt.Sprintf("fence-e%d-s%d-c%d", epoch, saveSeq, cursor), OwnerEpoch: epoch, SaveSeq: saveSeq, Body: raw}, seq: cursor, originPartition: 0, hasOriginPartition: true}
	}
	require.NoError(t, w.redisClient.Set(w.ctx, ownerepoch.OwnerEpochKey(42), "7", 0).Err())
	first := makeTask(7, 9, 10, 100)
	// 保留真实 SQL 入口，仅在其返回后切换锁令牌，复现已提交但发布前失租。
	dbOpHandlers[dbpb.DBOp_DB_OP_WRITE] = func(ctx context.Context, store *proto2mysql.DB, task *dbpb.DBTask, msg proto.Message) (writeOutcome, error) {
		outcome, err := handleDBWriteOp(ctx, store, task, msg)
		if err == nil {
			require.NoError(t, mr.Set(orderingLockRedisKey(w, task), "successor"))
		}
		return outcome, err
	}
	firstSession := attachKafkaClaim(t, w, first)
	w.handleTask(first, false)
	require.Equal(t, []int64{9}, firstSession.marked(), "重试副本落稳后才能确认 Kafka")
	sqlRow := &playerpb.PlayerDatabase{PlayerId: 42}
	require.NoError(t, model.FindOneByPK(sqlRow))
	require.Equal(t, uint64(100), sqlRow.Currency.Values[1])
	require.Equal(t, uint64(7), sqlRow.FenceOwnerEpoch)
	require.Equal(t, uint64(9), sqlRow.FenceSaveSeq)
	require.True(t, proto.Equal(ledger, sqlRow.AssetOpLedger), "资产与操作收据必须处于同一 SQL 行")
	for _, key := range []string{buildCacheKey(first.dbTask), assetop.SQLCommitKey(42), savebarrier.Key(42), appliedSeqKey(w.topic, 42, "player_database"), appliedEpochKey(w.topic, 42, "player_database")} {
		require.False(t, mr.Exists(key), key)
	}
	require.Equal(t, int64(1), redisListLen(t, w, w.retryQueueKey))
	require.NoError(t, w.redisClient.Del(w.ctx, orderingLockRedisKey(w, first.dbTask)).Err())
	dbOpHandlers[dbpb.DBOp_DB_OP_WRITE] = handleDBWriteOp
	retry := claimRetry(t, w)
	require.NotNil(t, retry)
	// 同 (E,S) 的不同载荷仍判 equal，但发布内容只能来自先提交的 SQL 行。
	forged := proto.Clone(row).(*playerpb.PlayerDatabase)
	forged.Currency.Values[1] = 999
	forged.AssetOpLedger.Streams[0].Receipts[0].RequestDigest = bytes.Repeat([]byte{0x99}, 32)
	forgedRaw, err := proto.Marshal(forged)
	retry.dbTask.Body = forgedRaw
	require.NoError(t, err)
	w.handleTask(retry, false)
	require.Zero(t, redisListLen(t, w, w.retryProcessingKey))
	require.Zero(t, redisListLen(t, w, w.retryQueueKey))
	raw, err := w.redisClient.HGet(w.ctx, assetop.SQLCommitKey(42), "data").Bytes()
	require.NoError(t, err)
	commit := &assetpb.PlayerAssetSQLCommit{}
	require.NoError(t, proto.Unmarshal(raw, commit))
	ledgerRaw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(sqlRow.AssetOpLedger)
	require.NoError(t, err)
	require.Equal(t, uint64(7), commit.OwnerEpoch)
	require.Equal(t, uint64(9), commit.SaveSeq)
	require.Equal(t, ledgerRaw, commit.LedgerProto)
	require.Equal(t, savebarrier.Version(7, 9), mr.HGet(savebarrier.Key(42), "player_database"))
	cursorBefore, err := w.redisClient.Get(w.ctx, appliedSeqKey(w.topic, 42, "player_database")).Result()
	require.NoError(t, err)
	// SQL older_seq 不推进高 Kafka offset，也不改变收据；它仍需通过 placement + token 核验。
	older := makeTask(7, 8, 11, 999)
	session := attachKafkaClaim(t, w, older)
	w.handleTask(older, false)
	require.Equal(t, []int64{10}, session.marked())
	cursorAfter, err := w.redisClient.Get(w.ctx, appliedSeqKey(w.topic, 42, "player_database")).Result()
	require.NoError(t, err)
	require.Equal(t, cursorBefore, cursorAfter)
	// 新 owner 从较低 Kafka offset 兜底：override 原子改写 cursor，SQL 与证据进入新 epoch。
	fencingEpochOverride.Store(true)
	t.Cleanup(func() { fencingEpochOverride.Store(false) })
	require.NoError(t, w.redisClient.Set(w.ctx, ownerepoch.OwnerEpochKey(42), "8", 0).Err())
	newer := makeTask(8, 1, 5, 80)
	w.handleTask(newer, false)
	sqlRow = &playerpb.PlayerDatabase{PlayerId: 42}
	require.NoError(t, model.FindOneByPK(sqlRow))
	require.Equal(t, uint64(8), sqlRow.FenceOwnerEpoch)
	require.Equal(t, uint64(80), sqlRow.Currency.Values[1])
	cacheBefore, err := w.redisClient.Get(w.ctx, buildCacheKey(first.dbTask)).Bytes()
	require.NoError(t, err)
	proofBefore, err := w.redisClient.HGet(w.ctx, assetop.SQLCommitKey(42), "data").Bytes()
	require.NoError(t, err)
	stale := makeTask(7, 999, 12, 999)
	w.handleTask(stale, false)
	actual, err := w.redisClient.Get(w.ctx, buildCacheKey(first.dbTask)).Bytes()
	require.NoError(t, err)
	require.Equal(t, cacheBefore, actual)
	actual, err = w.redisClient.HGet(w.ctx, assetop.SQLCommitKey(42), "data").Bytes()
	require.NoError(t, err)
	require.Equal(t, proofBefore, actual)
	sqlRow = &playerpb.PlayerDatabase{PlayerId: 42}
	require.NoError(t, model.FindOneByPK(sqlRow))
	require.Equal(t, uint64(80), sqlRow.Currency.Values[1])
	require.Equal(t, uint64(8), sqlRow.FenceOwnerEpoch)
}

func TestIntegrationPlayerSQLFenceLegacyCompatibilityKeepsCommittedFence(t *testing.T) {
	model := openAssetFenceFixture(t)
	require.NoError(t, writeFence(t, model, 20, 6, 100))
	require.NoError(t, SetFencingMode("legacy"))
	input := fenceRow(90)
	input.FenceOwnerEpoch = 99
	input.FenceSaveSeq = 99
	handled, outcome, err := savePlayerRow(context.Background(), model, &dbpb.DBTask{Key: 42, OwnerEpoch: 21, SaveSeq: 1}, input)
	require.True(t, handled)
	require.NoError(t, err)
	require.True(t, outcome.publishes())
	got := &playerpb.PlayerDatabase{PlayerId: 42}
	require.NoError(t, model.FindOneByPK(got))
	require.Equal(t, uint64(20), got.FenceOwnerEpoch)
	require.Equal(t, uint64(6), got.FenceSaveSeq)
	require.Equal(t, uint64(90), got.Currency.Values[1])
	require.Zero(t, input.FenceOwnerEpoch)
	require.NoError(t, SetFencingMode("dual"))
	_, outcome, err = savePlayerRow(context.Background(), model, &dbpb.DBTask{Key: 42, OwnerEpoch: 20}, fenceRow(80))
	require.NoError(t, err)
	require.Equal(t, writeApplied, outcome.kind)
	got = &playerpb.PlayerDatabase{PlayerId: 42}
	require.NoError(t, model.FindOneByPK(got))
	require.Equal(t, uint64(6), got.FenceSaveSeq)
	require.Equal(t, uint64(80), got.Currency.Values[1])
	_, outcome, err = savePlayerRow(context.Background(), model, &dbpb.DBTask{Key: 42}, fenceRow(999))
	require.NoError(t, err)
	require.Equal(t, writeLegacyAfterSeq, outcome.kind)
	_, outcome, err = savePlayerRow(context.Background(), model, &dbpb.DBTask{Key: 42, OwnerEpoch: 19, SaveSeq: 99}, fenceRow(999))
	require.NoError(t, err)
	require.Equal(t, writeStaleOwner, outcome.kind)
	require.Equal(t, uint64(20), outcome.storedEpoch)
}

// C1b(persist_schema.go,docs/design/mixed-version-persist-field-protection.md §6.2):body 的持久 schema 世代低于已落库行时,
// dual 与 legacy 都在同一把行锁下拒写,行保持原样;同世代或更高世代照常写。未盖戳(0)按世代 1 比较。
func TestIntegrationPlayerSQLRejectsPersistSchemaRegression(t *testing.T) {
	for _, mode := range []string{db_config.FencingModeDual, db_config.FencingModeLegacy} {
		t.Run(mode, func(t *testing.T) {
			model := openAssetFenceFixture(t)
			require.NoError(t, SetFencingMode(mode))
			save := func(epoch, seq, balance uint64, gen uint32) string {
				t.Helper()
				row := fenceRow(balance)
				row.PersistSchemaGen = gen
				handled, outcome, err := savePlayerRow(context.Background(), model, &dbpb.DBTask{Key: 42, OwnerEpoch: epoch, SaveSeq: seq}, row)
				require.True(t, handled, "玩家行未进入 SQL 围栏")
				require.NoError(t, err)
				return outcome.kind
			}
			load := func() *playerpb.PlayerDatabase {
				t.Helper()
				got := &playerpb.PlayerDatabase{PlayerId: 42}
				require.NoError(t, model.FindOneByPK(got))
				return got
			}

			require.Equal(t, writeApplied, save(10, 1, 100, 3))
			require.Equal(t, writeSchemaRegression, save(10, 2, 50, 2), "低世代 body 不得盖高世代行")
			require.Equal(t, writeSchemaRegression, save(10, 3, 40, 0), "未盖戳按 1,同样低于 3")
			got := load()
			require.EqualValues(t, 3, got.PersistSchemaGen)
			require.EqualValues(t, 100, got.Currency.Values[1], "倒退写不得改动行")

			require.Equal(t, writeApplied, save(10, 4, 70, 3))
			require.Equal(t, writeApplied, save(10, 5, 60, 4), "更高世代照常写")
			got = load()
			require.EqualValues(t, 4, got.PersistSchemaGen)
			require.EqualValues(t, 60, got.Currency.Values[1])
		})
	}
}

// 本helper只装配原随机夹具的显式schema/operator连接；不冒充普通writer权限/slot验收。
func openFenceAdminPool(t *testing.T, ctx context.Context, dsn, schema string, requireTiDB bool, maxOpen int, migration bool) (*sql.DB, error) {
	t.Helper()
	probe, err := authoritysql.Open(ctx, dsn, schema, requireTiDB, 1, authoritysql.Reader())
	if err != nil {
		return nil, err
	}
	var principal string
	err = probe.QueryRowContext(ctx, "SELECT CURRENT_USER()").Scan(&principal)
	closeErr := probe.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if migration {
		return authoritysql.OpenForMigration(ctx, dsn, schema, requireTiDB, authoritysql.Schema(principal))
	}
	return authoritysql.Open(ctx, dsn, schema, requireTiDB, maxOpen, authoritysql.Operator(principal))
}
