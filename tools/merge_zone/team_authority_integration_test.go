//go:build merge_integration

package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"shared/authoritysql"
)

// P7必须读取生产强TiDB连接的SQL成员原件。Redis回档/清空不能解除活队；旧缓存残影也不能恢复已离队成员。
func TestIT_TeamAuthority_SQLMembershipOverridesRedisHistory(t *testing.T) {
	requireIT(t)
	itReset(t)
	ctx := context.Background()
	itSetRouteHome(t, 12001, itSrcZone)
	db := itOpen(t)
	reader, err := openTeamAuthority(ctx, itDSN(), itDefaultDB, authoritysql.BackendTiDB)
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
	args := []string{"-source-zone", "901", "-target-zone", "902", "-manifest-path", manifest, "-dry-run"}
	out, code := itRun(t, args...)
	if code == 0 || !strings.Contains(out, "P7") {
		t.Fatalf("Redis无缓存却跳过SQL活队 exit=%d\n%s", code, out)
	}
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
		t.Fatalf("SQL已离队的预演仍失败 exit=%d\n%s", code, out)
	}
}

// 业务库与路由库不能是两个 TiDB 集群:在连接之前拒绝。
func TestIT_TeamAuthority_RejectsSplitSQLDomain(t *testing.T) {
	requireIT(t)
	itReset(t)
	itSetRouteHome(t, 12002, itSrcZone)
	cfg, err := mysql.ParseDSN(itRouteDSN())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Addr = "127.0.0.1:1"
	manifest := filepath.Join(t.TempDir(), "split.json")
	out, code := itRun(t, "-source-zone", "901", "-target-zone", "902", "-manifest-path", manifest, "-dry-run", "-route-db-dsn", cfg.FormatDSN())
	if code == 0 || !strings.Contains(out, "同一全球TiDB SQL入口") {
		t.Fatalf("分离SQL事务域未在连接前拒绝 exit=%d\n%s", code, out)
	}
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
	game, err := openCurrentSession(ctx, poison(itDSN()), authoritysql.BackendTiDB)
	if err != nil {
		t.Fatalf("业务池未覆盖旧快照DSN: %v", err)
	}
	defer game.Close()
	routes, _, err := openMergeRouteStore(ctx, poison(itRouteDSN()), authoritysql.BackendTiDB)
	if err != nil {
		t.Fatalf("路由池未覆盖旧快照DSN: %v", err)
	}
	defer routes.Close()
	team, err := openTeamAuthority(ctx, poison(itDSN()), itDefaultDB, authoritysql.BackendTiDB)
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
