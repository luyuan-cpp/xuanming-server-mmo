//go:build merge_integration

package main

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

// P7必须读取生产强TiDB连接的SQL成员原件。Redis回档/清空不能解除活队；旧缓存残影也不能恢复已离队成员。
func TestIT_TeamAuthority_SQLMembershipOverridesRedisHistory(t *testing.T) {
	requireIT(t)
	itReset(t)
	ctx := context.Background()
	itSeedPlayers(t, []uint64{12001}, true)
	db := itOpen(t)
	reader, err := openTeamAuthority(ctx, itDSN(), itDefaultDB)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.db.Close()
	if _, err := db.Exec("INSERT INTO team_membership(player_id,team_id,epoch) VALUES(12001,880001,1)"); err != nil {
		t.Fatal(err)
	}
	if n, err := reader.PlayersInTeams(ctx, []uint64{12001}); err != nil || n != 1 {
		t.Fatalf("原SQL活队 count=%d err=%v", n, err)
	}
	manifest := filepath.Join(t.TempDir(), "merge.json")
	args := []string{"-source-zone", "901", "-target-zone", "902", "-manifest-path", manifest, "-apply", "-player-rows-mode", "copy", "-expected-src-players", "1"}
	out, code := itRun(t, args...)
	if code == 0 || !strings.Contains(out, "P7") {
		t.Fatalf("Redis无缓存却跳过SQL活队 exit=%d\n%s", code, out)
	}
	itAssertNoFence(t, "SQL活队拒绝")
	if _, err := db.Exec("UPDATE team_membership SET team_id=0,epoch=2 WHERE player_id=12001"); err != nil {
		t.Fatal(err)
	}
	shared := itRedis(t, itSharedRD)
	if err := shared.Set(ctx, "team:player:12001", "880001", time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	if err := shared.Set(ctx, "team:rec:880001", `{"team_id":880001,"leader_id":12001}`, time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	if n, err := reader.PlayersInTeams(ctx, []uint64{12001}); err != nil || n != 0 {
		t.Fatalf("旧Redis复活SQL已离队 count=%d err=%v", n, err)
	}
	out, code = itRun(t, args...)
	if code != 0 {
		t.Fatalf("SQL已离队的原合区仍失败 exit=%d\n%s", code, out)
	}
	itAssertNoFence(t, "SQL无活队合区完成")
}

// 显式历史模式只能得到拒绝；不能靠清掉db_first水位把成功用例降回旧Redis写者。
func TestIT_TeamAuthority_RejectsLegacyRedisAndSplitSQLDomain(t *testing.T) {
	requireIT(t)
	itReset(t)
	itSeedPlayers(t, []uint64{12002}, true)
	manifest := filepath.Join(t.TempDir(), "legacy.json")
	args := []string{"-source-zone", "901", "-target-zone", "902", "-manifest-path", manifest, "-apply", "-player-rows-mode", "copy"}
	if err := itRedis(t, itMappingRD).Del(context.Background(), routeWatermarkKey).Err(); err != nil {
		t.Fatal(err)
	}
	out, code := itRun(t, append(append([]string{}, args...), "-route-source", routeSourceRedis, "-route-db-dsn", "")...)
	if code == 0 || !strings.Contains(out, "只允许-route-source=db") {
		t.Fatalf("旧Redis模式未在生产门禁拒绝 exit=%d\n%s", code, out)
	}
	itAssertNoFence(t, "旧Redis源拒绝")
	cfg, err := mysql.ParseDSN(itRouteDSN())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Addr = "127.0.0.1:1"
	out, code = itRun(t, append(append([]string{}, args...), "-route-db-dsn", cfg.FormatDSN())...)
	if code == 0 || !strings.Contains(out, "同一全球TiDB SQL入口") {
		t.Fatalf("分离SQL事务域未在连接前拒绝 exit=%d\n%s", code, out)
	}
	if n := itCount(t, itOpen(t), zoneDBName(itDstZone)+".player_database"); n != 0 {
		t.Fatalf("被拒合区改了目标行: %d", n)
	}
	itAssertNoFence(t, "分离SQL域拒绝")
}

// 复用真实Team路由前缀的参与者片主键锁：pid%%64=0，不是工具批读的63片。
// Team原生角色/名册事务另有GoMatch真实SQL回归；本例只证明同SQL域64片合区互斥接缝。
func TestIT_TeamAuthority_MemberShardLockConflictsWithAll64MergeFences(t *testing.T) {
	requireIT(t)
	itReset(t)
	rb := itFixtureBackend(t)
	db := itOpenRouteDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	itSeedSQLFence(t, itSrcZone, "it-seed-neutral")
	itReleaseSQLFences(t, itSrcZone)
	holder, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback()
	var shard uint32
	if err := holder.QueryRowContext(ctx, "SELECT shard FROM zone_merge_fence WHERE zone_id=? AND shard=0 FOR UPDATE", itSrcZone).Scan(&shard); err != nil {
		t.Fatal(err)
	}
	req := routeFenceAcquire{Zones: []uint32{itSrcZone, itDstZone}, RunID: "it-all64", Purpose: routeFencePurposeMerge, Src: itSrcZone, Dst: itDstZone, Holder: mergeInProgressValue{Operator: "merge-zone-it"}, TTL: time.Hour}
	_, err = rb.db.AcquireFence(ctx, req)
	var me *mysql.MySQLError
	if !errors.As(err, &me) || me.Number != 1205 {
		t.Fatalf("非63参与者片应阻止全部片获取，实际%v", err)
	}
	itAssertSQLFence(t, db, itSrcZone, false, "")
	itAssertSQLFence(t, db, itDstZone, false, "")
	if err := holder.Rollback(); err != nil {
		t.Fatal(err)
	}
	grant, err := rb.db.AcquireFence(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if grant.Epoch == 0 {
		t.Fatal("缺真实围栏世代")
	}
	for _, zone := range []uint32{itSrcZone, itDstZone} {
		itAssertSQLFence(t, db, zone, true, req.RunID)
	}
	itReleaseSQLFences(t, itSrcZone, itDstZone)
}

func TestIT_CurrentTiDB_AllPoolsOverrideHistoricalDSN(t *testing.T) {
	requireIT(t)
	itReset(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	poison := func(raw string) string {
		cfg, err := mysql.ParseDSN(raw)
		if err != nil {
			t.Fatal(err)
		}
		cfg.Params["TIDB_SNAPSHOT"] = "'123'"
		cfg.Params["tidb_read_staleness"] = "-60"
		cfg.Params["tidb_replica_read"] = "'follower'"
		cfg.Params["tidb_read_consistency"] = "'weak'"
		cfg.Params["tidb_low_resolution_tso"] = "ON"
		return cfg.FormatDSN()
	}
	game, err := openCurrentTiDB(ctx, poison(itDSN()))
	if err != nil {
		t.Fatalf("业务池未覆盖旧快照DSN: %v", err)
	}
	defer game.Close()
	routes, _, err := openMergeRouteStore(ctx, poison(itRouteDSN()), newMergeRouteWriter("merge-zone-it", "poisoned-dsn"))
	if err != nil {
		t.Fatalf("路由池未覆盖旧快照DSN: %v", err)
	}
	defer routes.Close()
	team, err := openTeamAuthority(ctx, poison(itDSN()), itDefaultDB)
	if err != nil {
		t.Fatalf("Team池未覆盖旧快照DSN: %v", err)
	}
	defer team.db.Close()
	for _, db := range []*sql.DB{game, routes.db, team.db} {
		if err := requireCurrentTiDB(ctx, db); err != nil {
			t.Fatal(err)
		}
	}
}
