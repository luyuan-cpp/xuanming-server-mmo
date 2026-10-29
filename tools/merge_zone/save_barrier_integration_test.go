//go:build merge_integration

package main

// verify:save_barrier 在真 MySQL / Redis 上的读口径(r2-mz#1,2026-10-20 修复)。判定与汇总逻辑在 player_rows_barrier_test.go
// 用替身覆盖;这里只证替身证不了的几件事:
//   - sqlRowFenceReader 的 SELECT 在真 MySQL 上按 id 取对两列围栏(含 uint64 上界),没有行的 id 不出现;缺围栏列时报错
//     (调用方据此报 INFRA,不当作「没有行」)。
//   - readSaveBarrierState 在真 Redis 上只 HMGET pending 与两表 ACK:hash 里 C++ 写的 schema_gen / schema_writer 不影响结果,
//     读完 hash 原样在(只读)。
//   - (2026-10-22)真 Redis 的 pipeline 里,类型错的键只让那一条命令回 WRONGTYPE,readSaveBarrierState 把它归到那名玩家,
//     不整体报错;前向 copy 合服的缓存失效让屏障 hash 逐字段原样留下,撤销的缓存失效把它删掉。
//
// 行围栏用一次性库 merge_zone_it_fence_db,本用例自建、自删:共享夹具 zone_901_db / zone_902_db 的玩家表没有围栏列,
// 给它们加列会改变其他用例的拷行列集。也因为这一点,端到端用例 TestIT_EndToEnd_MergeVerifyUnmerge 的夹具玩家没有
// owner_epoch 与屏障,verify:save_barrier 在那里只出 info;保留屏障这条路径由下面的真 Redis 用例与单测分段证明。

import (
	"context"
	"reflect"
	"strconv"
	"testing"
)

const itFenceDB = "merge_zone_it_fence_db"

func TestIT_SQLRowFenceReader_ReadsBothFenceColumnsAndFailsWithoutThem(t *testing.T) {
	requireIT(t)
	ctx := context.Background()
	db := itOpen(t)
	t.Cleanup(func() { _, _ = db.Exec("DROP DATABASE IF EXISTS " + itFenceDB) })
	for _, q := range []string{
		"DROP DATABASE IF EXISTS " + itFenceDB,
		"CREATE DATABASE " + itFenceDB,
		"CREATE TABLE " + itFenceDB + ".player_database (player_id BIGINT UNSIGNED NOT NULL, " +
			"fence_owner_epoch BIGINT UNSIGNED NOT NULL DEFAULT 0, fence_save_seq BIGINT UNSIGNED NOT NULL DEFAULT 0, " +
			"payload MEDIUMBLOB, PRIMARY KEY (player_id))",
		// uint64 上界用字面量写入,不经驱动的参数转换。
		"INSERT INTO " + itFenceDB + ".player_database (player_id, fence_owner_epoch, fence_save_seq, payload) VALUES " +
			"(1, 8, 90, 'a'), (2, 18446744073709551615, 1, 'b'), (4, 0, 0, 'c')",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	read := sqlRowFenceReader(db)
	got, err := read(ctx, itFenceDB, "player_database", []uint64{3, 2, 1, 4})
	if err != nil {
		t.Fatal(err)
	}
	want := map[uint64]string{
		1: saveBarrierVersion(8, 90),
		2: saveBarrierVersion(18446744073709551615, 1),
		4: saveBarrierVersion(0, 0), // 零围栏照实返回,judgeSaveBarrier 按 savebarrier.RowVersion 口径把它当「从未经围栏写入」
	}
	if len(got) != len(want) {
		t.Fatalf("fences = %v, want %v (id 3 has no row and must be absent)", got, want)
	}
	for id, v := range want {
		if got[id] != v {
			t.Errorf("player %d fence = %q, want %q", id, got[id], v)
		}
	}

	// 共享夹具表没有围栏列:必须报错,不能当成「这些人没有行」。
	if _, err := read(ctx, zoneDBName(itSrcZone), "player_database", []uint64{1}); err == nil {
		t.Fatal("a table without fence columns must make the read fail (the audit reports INFRA)")
	}
}

func TestIT_ReadSaveBarrierState_IgnoresSchemaGenAndWriterFields(t *testing.T) {
	requireIT(t)
	itReset(t)
	ctx := context.Background()
	shared := itRedis(t, itSharedRD)
	v := saveBarrierVersion(8, 90)
	if err := shared.Set(ctx, playerOwnerEpochKey(9501), "9", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if err := shared.Set(ctx, playerOwnerEpochKey(9502), "0", 0).Err(); err != nil {
		t.Fatal(err)
	}
	barrier := playerSaveBarrierKey("9501")
	if err := shared.HSet(ctx, barrier, saveBarrierPendingField, v, saveBarrierAckTables[0], v, saveBarrierAckTables[1], v,
		"schema_gen", "3", "schema_writer", "6f1c2a9e-4b7d-4e0a-9c3f-2d8b1e5a7c40").Err(); err != nil {
		t.Fatal(err)
	}

	obs, required, err := readSaveBarrierState(ctx, shared, []uint64{9501, 9502, 9503})
	if err != nil {
		t.Fatal(err)
	}
	if len(required) != 1 || required[0] != 9501 {
		t.Fatalf("required = %v, want only 9501 (9502 has owner_epoch 0, 9503 has none)", required)
	}
	o := obs[9501]
	if o.Pending != v || o.Acks != [2]string{v, v} || o.OwnerRaw != "9" {
		t.Fatalf("observation of 9501 = %+v", *o)
	}
	if obs[9502].OwnerRaw != "0" || obs[9503].OwnerRaw != "" || obs[9503].OwnerWrongType {
		t.Fatalf("owner observations: 9502=%+v 9503=%+v", *obs[9502], *obs[9503])
	}
	// 行围栏与屏障同版时判定通过:schema_gen / schema_writer 不是拒绝条件。
	o.RowFence, o.RowPresent = [2]string{v, v}, [2]bool{true, true}
	if verdict := judgeSaveBarrier(*o); verdict != barrierVerdictOK {
		t.Fatalf("judge = %q, want pass", verdict)
	}
	// 只读:hash 的五个字段都还在。
	if n, err := shared.HLen(ctx, barrier).Result(); err != nil || n != 5 {
		t.Fatalf("barrier hash fields after the read = %d (err %v), want 5", n, err)
	}
}

func TestIT_ReadSaveBarrierState_WrongTypeIsAPerPlayerObservation(t *testing.T) {
	requireIT(t)
	itReset(t)
	ctx := context.Background()
	shared := itRedis(t, itSharedRD)
	v := saveBarrierVersion(8, 90)
	// 9511:owner_epoch 键被写成了 hash;9512:屏障键是 string;9513:正常稳态。三人在同一条 pipeline 里。
	if err := shared.HSet(ctx, playerOwnerEpochKey(9511), "corrupt", "1").Err(); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uint64{9512, 9513} {
		if err := shared.Set(ctx, playerOwnerEpochKey(id), "9", 0).Err(); err != nil {
			t.Fatal(err)
		}
	}
	if err := shared.Set(ctx, playerSaveBarrierKey("9512"), "corrupt", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if err := shared.HSet(ctx, playerSaveBarrierKey("9513"), saveBarrierPendingField, v, saveBarrierAckTables[0], v,
		saveBarrierAckTables[1], v).Err(); err != nil {
		t.Fatal(err)
	}

	obs, required, err := readSaveBarrierState(ctx, shared, []uint64{9511, 9512, 9513})
	if err != nil {
		t.Fatalf("a WRONGTYPE reply belongs to one player and must not fail the whole read: %v", err)
	}
	if !reflect.DeepEqual(required, []uint64{9511, 9512, 9513}) {
		t.Fatalf("required = %v", required)
	}
	if !obs[9511].OwnerWrongType || obs[9512].OwnerWrongType || !obs[9512].BarrierWrongType || obs[9513].BarrierWrongType {
		t.Fatalf("observations: 9511=%+v 9512=%+v 9513=%+v", *obs[9511], *obs[9512], *obs[9513])
	}
	if got := judgeSaveBarrier(*obs[9511]); got != barrierVerdictOwnerMalformed {
		t.Errorf("9511 judge = %q", got)
	}
	if got := judgeSaveBarrier(*obs[9512]); got != barrierVerdictBarrierMalformed {
		t.Errorf("9512 judge = %q", got)
	}
	if o := obs[9513]; o.OwnerRaw != "9" || o.Pending != v || o.Acks != [2]string{v, v} {
		t.Errorf("9513 = %+v", *o)
	}
}

func TestIT_ForwardCopyKeepsTheBarrierHashFieldByField(t *testing.T) {
	requireIT(t)
	itReset(t)
	ctx := context.Background()
	shared := itRedis(t, itSharedRD)
	ids := []uint64{9521, 9522}
	tables := []string{"player_database", "player_database_1", "player_centre_database"}
	v := saveBarrierVersion(8, 90)
	want := map[uint64]map[string]string{}
	for _, id := range ids {
		for _, k := range playerCacheKeysKeepBarrier(id, tables) {
			if err := shared.Set(ctx, k, "stale-from-source-zone", 0).Err(); err != nil {
				t.Fatal(err)
			}
		}
		// 稳态屏障连同 C++ 写的 schema_gen / schema_writer 与 login 写的身份戳(savebarrier.VerifiedIdentityField):
		// 合服不该改动其中任何一个。
		fields := map[string]string{saveBarrierPendingField: v, saveBarrierAckTables[0]: v, saveBarrierAckTables[1]: v,
			"schema_gen": "3", "schema_writer": "6f1c2a9e-4b7d-4e0a-9c3f-2d8b1e5a7c40", "verified_identity": "run-before-merge"}
		if err := shared.HSet(ctx, playerSaveBarrierKey(strconv.FormatUint(id, 10)), fields).Err(); err != nil {
			t.Fatal(err)
		}
		want[id] = fields
	}

	// 前向 copy 合服:缓存与证据键删掉,屏障 hash 逐字段不变。
	if _, err := invalidatePlayerCachesKeepBarrier(ctx, shared, ids, tables, false); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		for _, k := range playerCacheKeysKeepBarrier(id, tables) {
			if n, err := shared.Exists(ctx, k).Result(); err != nil || n != 0 {
				t.Fatalf("forward copy merge left %s (n=%d err=%v)", k, n, err)
			}
		}
		got, err := shared.HGetAll(ctx, playerSaveBarrierKey(strconv.FormatUint(id, 10))).Result()
		if err != nil || !reflect.DeepEqual(got, want[id]) {
			t.Fatalf("barrier of %d after the forward copy merge = %v (err %v), want %v", id, got, err, want[id])
		}
	}

	// 撤销:屏障一起删。
	if _, err := invalidatePlayerCaches(ctx, shared, ids, tables, false); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if n, err := shared.Exists(ctx, playerSaveBarrierKey(strconv.FormatUint(id, 10))).Result(); err != nil || n != 0 {
			t.Fatalf("unmerge left the barrier of %d (n=%d err=%v)", id, n, err)
		}
	}
}
