package main

// 真库用例:闸门、哨兵、校验和、切换窗口的 SQL 在真实后端上跑一遍。etcd 和复制是替身:etcd 的 CAS 由 shared/dbplacement 的真 etcd 用例钉住,
// 复制由测试自己把闸门表、哨兵行和业务行抄过去(真复制要 BR、TiCDC 或 GTID 通道,手册 §10 的演练覆盖)。
//
// 运行:go test -count=1 -v ./...
//   - DBMOVE_IT_SOURCE_DSN、DBMOVE_IT_TARGET_DSN:两个实例的基址,数字 loopback TCP、无库名,账号要能建库、删库。必须是两个实例:两边的库同名。
//   - MMORPG_SQL_BACKEND:tidb(缺省)或 mysql,和实例对上,两档都要跑。
//   - DBMOVE_IT_REQUIRE=1:依赖缺失时 FAIL;默认 SKIP。验收必须设 1,SKIP 不能算通过。
//
// 每个用例在两边各建一个 dbmove_it_<随机> 库,结束时删掉,不碰别的库。

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"shared/authoritysql"
	"shared/dbplacement"
)

const (
	itRequireEnv   = "DBMOVE_IT_REQUIRE"
	itSourceDSNEnv = "DBMOVE_IT_SOURCE_DSN"
	itTargetDSNEnv = "DBMOVE_IT_TARGET_DSN"
	itTimeout      = 60 * time.Second
)

func requireIT(t *testing.T) {
	t.Helper()
	if os.Getenv(itSourceDSNEnv) != "" && os.Getenv(itTargetDSNEnv) != "" {
		return
	}
	message := itSourceDSNEnv + " and " + itTargetDSNEnv + " are not set"
	if os.Getenv(itRequireEnv) == "1" {
		t.Fatal(message)
	}
	t.Skip(message)
}

// itBase 校验基址:用例会建库删库,只接受数字 loopback。错误不回显 DSN。
func itBase(t *testing.T, env string) *mysql.Config {
	t.Helper()
	cfg, err := mysql.ParseDSN(os.Getenv(env))
	if err != nil {
		t.Fatalf("%s does not parse (value not echoed)", env)
	}
	host, _, err := net.SplitHostPort(cfg.Addr)
	if err != nil || cfg.Net != "tcp" || cfg.DBName != "" {
		t.Fatalf("%s must be a plain TCP base DSN with an explicit port and no database", env)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		t.Fatalf("%s must be a numeric loopback address", env)
	}
	return cfg
}

// itSide 是一侧的随机逻辑库,带正式闸门表和一张业务表。
type itSide struct {
	dsn       string
	admin     *sql.DB
	gate      *sqlGate
	schema    string
	principal string
	backend   authoritysql.Backend
}

func itCreateSide(ctx context.Context, t *testing.T, env, schema string, backend authoritysql.Backend) *itSide {
	t.Helper()
	base := itBase(t, env)
	admin, err := openRaw(base.FormatDSN(), 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+quoteIdentifier(schema)); err != nil {
		t.Fatalf("create the database through %s (the two DSNs must be two instances): %v", env, err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), itTimeout)
		defer cancel()
		_, _ = admin.ExecContext(cleanup, "DROP DATABASE IF EXISTS "+quoteIdentifier(schema))
	})
	var principal string
	if err := admin.QueryRowContext(ctx, "SELECT CURRENT_USER()").Scan(&principal); err != nil {
		t.Fatal(err)
	}
	cfg := base.Clone()
	cfg.DBName = schema
	schemaPool, err := authoritysql.OpenForMigration(ctx, cfg.FormatDSN(), schema, backend, authoritysql.Schema(principal))
	if err != nil {
		t.Fatal(err)
	}
	defer schemaPool.Close()
	if err := authoritysql.EnsureCatalogSchema(ctx, schemaPool, schema); err != nil {
		t.Fatal(err)
	}
	if _, err := schemaPool.ExecContext(ctx, "CREATE TABLE it_business(id BIGINT PRIMARY KEY,val VARCHAR(16) NULL,payload VARBINARY(32) NULL,KEY idx_val(val))"); err != nil {
		t.Fatal(err)
	}
	return &itSide{dsn: cfg.FormatDSN(), admin: admin, schema: schema, principal: principal, backend: backend}
}

// openGate 在复制之后调:Catalog 记下 route_meta 的世代,目标上的世代是复制过去的。
func (s *itSide) openGate(ctx context.Context, t *testing.T) {
	t.Helper()
	gate, err := openGate(ctx, s.dsn, s.schema, s.backend, s.principal, bytes.Repeat([]byte{7}, 32), authoritysql.Operator(s.principal))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gate.Close() })
	s.gate = gate
}

func (s *itSide) exec(ctx context.Context, t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := s.admin.ExecContext(ctx, strings.ReplaceAll(query, "{db}", quoteIdentifier(s.schema)), args...); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// itCopyTable 把一张表整张抄到目标:测试里的「复制」。
func itCopyTable(ctx context.Context, t *testing.T, from, to *itSide, table string, columns []string) {
	t.Helper()
	list := strings.Join(columns, ",")
	rows, err := from.admin.QueryContext(ctx, "SELECT "+list+" FROM "+quoteIdentifier(from.schema)+"."+table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	to.exec(ctx, t, "DELETE FROM {db}."+table)
	placeholders := strings.TrimRight(strings.Repeat("?,", len(columns)), ",")
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatal(err)
		}
		to.exec(ctx, t, "INSERT INTO {db}."+table+"("+list+") VALUES("+placeholders+")", values...)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func itReplicate(ctx context.Context, t *testing.T, from, to *itSide) {
	t.Helper()
	itCopyTable(ctx, t, from, to, "route_meta", []string{"id", "generation", "writable", "writer_epoch", "catalog_revision", "freeze_run_id"})
	itCopyTable(ctx, t, from, to, authoritysql.MoveSentinelTable, []string{"nonce", "run_id", "writer_epoch", "written_ms"})
	itCopyTable(ctx, t, from, to, "it_business", []string{"id", "val", "payload"})
}

// itRepl 实现 replication:CaughtUp 时抄一遍。Mark、TargetPoint 走真实语句,验证它们在对应后端上能跑。
type itRepl struct {
	t            *testing.T
	source, dest *itSide
}

func (r *itRepl) CheckLag(context.Context, time.Duration) error { return nil }

func itPoint(ctx context.Context, side *itSide) (string, error) {
	if side.backend == authoritysql.BackendTiDB {
		tso, err := readTiDBTSO(ctx, side.admin)
		return fmt.Sprint(tso), err
	}
	return readGTIDExecuted(ctx, side.admin)
}

func (r *itRepl) Mark(ctx context.Context) (string, error) { return itPoint(ctx, r.source) }

func (r *itRepl) CaughtUp(ctx context.Context, _ string) (bool, error) {
	itReplicate(ctx, r.t, r.source, r.dest)
	return true, nil
}

func (r *itRepl) TargetPoint(ctx context.Context) (string, error) { return itPoint(ctx, r.dest) }

func (r *itRepl) VerifyPoint(context.Context) (verifyPoint, error) {
	return verifyPoint{Detail: "copied by the test", Feed: itFeed}, nil
}

func (r *itRepl) RecheckVerifyPoint(context.Context, verifyPoint) error { return nil }

func (r *itRepl) Identity(context.Context) (dbplacement.FeedIdentity, error) { return itFeed, nil }

var itFeed = dbplacement.FeedIdentity{Changefeed: "copied-by-the-test", StartTs: 1, CreateTime: "now"}

func itSetup(t *testing.T) (context.Context, *itSide, *itSide, *mover, *fakeStore) {
	t.Helper()
	backend, err := authoritysql.LoadBackend(os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), itTimeout)
	t.Cleanup(cancel)
	suffix := make([]byte, 8)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	schema := "dbmove_it_" + hex.EncodeToString(suffix)
	source := itCreateSide(ctx, t, itSourceDSNEnv, schema, backend)
	target := itCreateSide(ctx, t, itTargetDSNEnv, schema, backend)
	source.exec(ctx, t, "INSERT INTO {db}.it_business(id,val,payload) VALUES(1,'a',X'00FF'),(2,NULL,NULL),(3,'',X'')")
	itReplicate(ctx, t, source, target)
	source.openGate(ctx, t)
	target.openGate(ctx, t)
	run, err := newRunID()
	if err != nil {
		t.Fatal(err)
	}
	runBytes, err := hex.DecodeString(run)
	if err != nil {
		t.Fatal(err)
	}
	feed := itFeed
	store := &fakeStore{log: &eventLog{}, rev: 1, defaultCluster: "a", records: map[string]dbplacement.Record{
		schema: {ModRevision: 1, Placement: dbplacement.Placement{Cluster: "a", State: dbplacement.StateMoving, Move: &dbplacement.Move{
			RunID: run, SourceCluster: "a", TargetCluster: "b", VerifiedMs: uint64(time.Now().UnixMilli()), VerifiedFeed: &feed,
		}}},
	}}
	m := &mover{
		o: moverOptions{
			db: schema, runID: run, runIDBytes: runBytes, operator: source.principal, backend: string(backend),
			config: dbplacement.Config{DefaultCluster: "a", EtcdHosts: []string{"e:2379"}, SqlClusters: map[string]dbplacement.SqlCluster{"a": {Hosts: "h:1"}, "b": {Hosts: "h:2"}}},
			maxLag: time.Second, budget: 20 * time.Second, compareBudget: 20 * time.Second, rollbackBudget: 10 * time.Second, preflight: 10 * time.Second,
			verifyMaxAge: time.Hour, retention: time.Hour, pollInterval: 50 * time.Millisecond,
		},
		store:       store,
		source:      source.gate,
		target:      target.gate,
		sourceAdmin: &sqlAdmin{db: source.admin},
		targetAdmin: &sqlAdmin{db: target.admin},
		repl:        &itRepl{t: t, source: source, dest: target},
		openView: func(ctx context.Context, role, snapshot string) (checksumView, error) {
			dsn := source.dsn
			if role == roleTarget {
				dsn = target.dsn
			}
			view, err := openChecksumView(ctx, dsn, schema, backend, snapshot)
			if err != nil {
				return nil, err
			}
			return view, nil
		},
		now:   time.Now,
		sleep: sleepContext,
		log:   &journal{out: &bytes.Buffer{}, errOut: &bytes.Buffer{}, now: time.Now},
	}
	return ctx, source, target, m, store
}

func TestITCutoverOnRealDatabases(t *testing.T) {
	requireIT(t)
	ctx, source, target, m, store := itSetup(t)
	before, err := source.gate.CurrentMeta(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.cutover(ctx); err != nil {
		t.Fatal(err)
	}
	p := store.records[m.o.db].Placement
	if p.Cluster != "b" || p.Version != 1 || p.State != dbplacement.StateActive {
		t.Fatalf("翻转结果: %+v", p)
	}
	if source.backend == authoritysql.BackendTiDB && p.Move.TargetPoint == "" {
		t.Fatal("TiDB 档要记下目标的 TSO")
	}
	frozen, err := source.gate.CurrentMeta(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if frozen.Writable != 0 || !bytes.Equal(frozen.FreezeRunId, m.o.runIDBytes) || frozen.WriterEpoch != before.WriterEpoch+1 {
		t.Fatalf("源库要保持冻结: %+v", frozen)
	}
	// 翻转之后目标上有标记,rollback 靠它认出 etcd 回档。
	marked, err := (&sqlAdmin{db: target.admin}).HasFlipMarker(ctx, target.schema, flipMarkerNonce(m.o.runID))
	if err != nil || !marked {
		t.Fatalf("翻转标记: %v %v", marked, err)
	}
	opened, err := target.gate.CurrentMeta(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if opened.Writable != 1 || len(opened.FreezeRunId) != 0 || opened.WriterEpoch != frozen.WriterEpoch+1 {
		t.Fatalf("目标闸门要打开: %+v", opened)
	}
	// 重放幂等。
	if err := m.cutover(ctx); err != nil {
		t.Fatalf("重跑: %v", err)
	}
}

func TestITOverBudgetRollsBackRealGate(t *testing.T) {
	requireIT(t)
	ctx, source, _, m, store := itSetup(t)
	m.o.budget = 300 * time.Millisecond
	m.repl = stuckRepl{m.repl}
	err := m.cutover(ctx)
	if err == nil || errors.Is(err, errUnknown) {
		t.Fatalf("超预算要回滚: %v", err)
	}
	meta, err := source.gate.CurrentMeta(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Writable != 1 || len(meta.FreezeRunId) != 0 {
		t.Fatalf("回滚之后源库要可写: %+v", meta)
	}
	if p := store.records[m.o.db].Placement; p.Cluster != "a" || p.State != dbplacement.StateMoving {
		t.Fatalf("映射不能翻转: %+v", p)
	}
}

type stuckRepl struct{ replication }

func (stuckRepl) CaughtUp(context.Context, string) (bool, error) { return false, nil }

func TestITChecksumMatchesAndCatchesDifference(t *testing.T) {
	requireIT(t)
	ctx, source, target, m, _ := itSetup(t)
	mismatches, err := m.compare(ctx, "verify", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(mismatches) != 0 {
		t.Fatalf("抄过去的数据要一致(含 NULL、空串、二进制): %v", mismatches)
	}
	// 同样的行数,一个值不同。
	target.exec(ctx, t, "UPDATE {db}.it_business SET val='b' WHERE id=1")
	mismatches, err = m.compare(ctx, "verify", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(mismatches) != 1 || !strings.Contains(mismatches[0], "it_business") {
		t.Fatalf("值不同要查出来: %v", mismatches)
	}
	// NULL 和空串不算一样。
	itReplicate(ctx, t, source, target)
	target.exec(ctx, t, "UPDATE {db}.it_business SET val='' WHERE id=2")
	mismatches, err = m.compare(ctx, "verify", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(mismatches) != 1 {
		t.Fatalf("NULL 和空串不同要查出来: %v", mismatches)
	}
	// 值里有逗号:('a,b', 'c') 和 ('a', 'b,c') 用分隔符直接连起来是同一串,逐行摘要要分得清。
	itReplicate(ctx, t, source, target)
	source.exec(ctx, t, "UPDATE {db}.it_business SET val='a,b',payload=X'63' WHERE id=1")
	target.exec(ctx, t, "UPDATE {db}.it_business SET val='a',payload=X'622C63' WHERE id=1")
	mismatches, err = m.compare(ctx, "verify", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(mismatches) != 1 || !strings.Contains(mismatches[0], "it_business") {
		t.Fatalf("逗号挪了位置的两行要查出来: %v", mismatches)
	}
	if target.backend != authoritysql.BackendTiDB {
		return
	}
	// TiDB:快照上读。改之前取一个 TSO,用它看到的是改之前的数据。
	tso, err := readTiDBTSO(ctx, target.admin)
	if err != nil {
		t.Fatal(err)
	}
	target.exec(ctx, t, "UPDATE {db}.it_business SET val='later' WHERE id=1")
	view, err := openChecksumView(ctx, target.dsn, target.schema, target.backend, fmt.Sprint(tso))
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	past, err := view.TableDigest(ctx, "it_business")
	if err != nil {
		t.Fatal(err)
	}
	current, err := openChecksumView(ctx, target.dsn, target.schema, target.backend, "")
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	now, err := current.TableDigest(ctx, "it_business")
	if err != nil {
		t.Fatal(err)
	}
	if past == now {
		t.Fatal("快照要看到改之前的数据")
	}
	if past.KVs == 0 {
		t.Fatal("ADMIN CHECKSUM TABLE 的 Total_kvs 没读到")
	}
}

// 账号锁、授权、连接列表在真实后端上的语句形状。账号用完删掉。
func TestITAccountLockGrantsAndConnections(t *testing.T) {
	requireIT(t)
	ctx, _, target, _, _ := itSetup(t)
	admin := &sqlAdmin{db: target.admin}
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	user := "dbmove_it_" + hex.EncodeToString(suffix)
	principal := user + "@%"
	target.exec(ctx, t, "CREATE USER '"+user+"'@'%' IDENTIFIED BY 'dbmove-it-only'")
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), itTimeout)
		defer cancel()
		_, _ = target.admin.ExecContext(cleanup, "DROP USER IF EXISTS '"+user+"'@'%'")
	})
	target.exec(ctx, t, "GRANT SELECT,INSERT ON {db}.it_business TO '"+user+"'@'%'")
	lockState := func() string {
		var state string
		if err := target.admin.QueryRowContext(ctx, "SELECT account_locked FROM mysql.user WHERE User=? AND Host='%'", user).Scan(&state); err != nil {
			t.Fatal(err)
		}
		return state
	}
	for _, locked := range []bool{true, true, false, false} {
		if err := admin.SetAccountLocked(ctx, principal, locked); err != nil {
			t.Fatalf("改锁(幂等) %v: %v", locked, err)
		}
		want := "N"
		if locked {
			want = "Y"
		}
		if got := lockState(); got != want {
			t.Fatalf("account_locked=%s, want %s", got, want)
		}
	}
	grants, err := admin.AccountGrants(ctx, principal)
	if err != nil {
		t.Fatal(err)
	}
	if got := missingGrants([]string{"GRANT SELECT, INSERT ON `" + target.schema + "`.`it_business` TO `" + user + "`@`%`"}, grants, target.schema); len(got) != 0 {
		t.Fatalf("解析不了真实的 SHOW GRANTS: %v, %v", got, grants)
	}
	if got := otherSchemasOf(grants, target.schema); len(got) != 0 {
		t.Fatalf("只有这个库的授权: %v, %v", got, grants)
	}
	connections, err := admin.Connections(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(connections) == 0 {
		t.Fatal("至少有测试自己这条连接")
	}
}

// 校验和用的连接不走 authoritysql:运行池的语句白名单不放这些语句,这里钉住理由还成立。
func TestITMaintenanceProbeOnRealDatabases(t *testing.T) {
	requireIT(t)
	ctx, source, target, _, _ := itSetup(t)
	for _, side := range []*itSide{source, target} {
		if err := side.gate.ProbeControl(ctx); err != nil {
			t.Fatalf("真库上的权限探测: %v", err)
		}
		var rows int
		if err := side.admin.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+quoteIdentifier(side.schema)+"."+authoritysql.MoveSentinelTable).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows != 0 {
			t.Fatalf("探测要回滚,不留行: %d", rows)
		}
		principals, err := side.gate.ActivePrincipals(ctx)
		if err != nil || len(principals) != 0 {
			t.Fatalf("没有注册写者: %v %v", principals, err)
		}
		charset, collation, err := side.gate.SchemaCharset(ctx)
		if err != nil || charset == "" || collation == "" {
			t.Fatalf("字符集: %s %s %v", charset, collation, err)
		}
	}
	admin := &sqlAdmin{db: target.admin}
	backend, err := admin.ServerBackend(ctx)
	if err != nil || backend != string(target.backend) {
		t.Fatalf("服务器类型: %s %v", backend, err)
	}
	exists, err := admin.SchemaExists(ctx, target.schema)
	if err != nil || !exists {
		t.Fatalf("库要在: %v %v", exists, err)
	}
	found, err := admin.AccountExists(ctx, target.principal)
	if err != nil || !found {
		t.Fatalf("当前账号要在 mysql.user 里: %v %v", found, err)
	}
	if found, err = admin.AccountExists(ctx, "no_such_user@nowhere"); err != nil || found {
		t.Fatalf("不存在的账号: %v %v", found, err)
	}
	frozen, err := admin.GateFrozenBy(ctx, target.schema, bytes.Repeat([]byte{1}, 16))
	if err != nil || frozen {
		t.Fatalf("没冻结: %v %v", frozen, err)
	}
	if marked, err := admin.HasFlipMarker(ctx, target.schema, flipMarkerNonce("no-such-run")); err != nil || marked {
		t.Fatalf("没有标记: %v %v", marked, err)
	}
	if marked, err := admin.HasFlipMarker(ctx, "dbmove_it_no_such_database", flipMarkerNonce("no-such-run")); err != nil || marked {
		t.Fatalf("库不在不是读失败: %v %v", marked, err)
	}
	if state, err := admin.GateState(ctx, "dbmove_it_no_such_database"); err != nil || state.Found {
		t.Fatalf("库不在不是读失败: %+v %v", state, err)
	}
	if state, err := admin.GateState(ctx, target.schema); err != nil || !state.Found {
		t.Fatalf("目标有闸门表: %+v %v", state, err)
	}
	if _, err := admin.AccountLocked(ctx, "no_such_user@nowhere"); err == nil {
		t.Fatal("账号不存在是错误")
	}
	if err := admin.CheckSeesAllConnections(ctx); err != nil {
		t.Fatalf("测试用的管理员账号看得到全部连接: %v", err)
	}
	if err := admin.CreateSchema(ctx, target.schema, "utf8mb4", "utf8mb4_bin"); err != nil {
		t.Fatalf("建库幂等: %v", err)
	}
	if err := admin.DropSchema(ctx, target.schema); err != nil {
		t.Fatal(err)
	}
	if exists, err = admin.SchemaExists(ctx, target.schema); err != nil || exists {
		t.Fatalf("库要没了: %v %v", exists, err)
	}
}

// itShapeColumns 是 it_shape 表在源库上的定义。排序规则写明:两个后端的缺省值不同。
const itShapeColumns = "id BIGINT PRIMARY KEY,name VARCHAR(16) COLLATE utf8mb4_bin NOT NULL,note VARCHAR(16) COLLATE utf8mb4_bin NULL,UNIQUE KEY uk_name(name),KEY idx_pair(name,note)"

// 表的定义在真实后端上读得出来、比得出差异:数据一样(都是空表),定义不一样也算不一致。没有主键的表拒绝。
func TestITTableDefinitionsOnRealDatabases(t *testing.T) {
	requireIT(t)
	ctx, source, target, m, _ := itSetup(t)
	source.exec(ctx, t, "CREATE TABLE {db}.it_shape("+itShapeColumns+")")
	recreate := func(columns string) {
		t.Helper()
		target.exec(ctx, t, "DROP TABLE IF EXISTS {db}.it_shape")
		target.exec(ctx, t, "CREATE TABLE {db}.it_shape("+columns+")")
	}
	recreate(itShapeColumns)
	mismatches, err := m.compare(ctx, "verify", "", "")
	if err != nil || len(mismatches) != 0 {
		t.Fatalf("同一份定义要一致(闸门表、业务表都在内): %v %v", mismatches, err)
	}
	view, err := openChecksumView(ctx, source.dsn, source.schema, source.backend, "")
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	digest, err := view.TableDigest(ctx, "it_shape")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`column 1 "id" type="bigint`, `column 2 "name" type="varchar(16)" collation="utf8mb4_bin" nullable=NO`, `column 3 "note" type="varchar(16)" collation="utf8mb4_bin" nullable=YES`,
		`primary key "PRIMARY"`, `unique index "uk_name"`, `index "idx_pair"`,
	} {
		if !strings.Contains(digest.Structure, want) {
			t.Fatalf("表定义里缺 %q:\n%s", want, digest.Structure)
		}
	}
	// 索引里列的先后按 SEQ_IN_INDEX。
	if name, note := strings.Index(digest.Structure, `("name" expression`), strings.Index(digest.Structure, `"note" expression`); name < 0 || note < name {
		t.Fatalf("idx_pair 的列序是 name、note:\n%s", digest.Structure)
	}

	tests := []struct {
		name    string
		columns string
		want    string
	}{
		{"唯一约束成了普通索引", "id BIGINT PRIMARY KEY,name VARCHAR(16) COLLATE utf8mb4_bin NOT NULL,note VARCHAR(16) COLLATE utf8mb4_bin NULL,KEY uk_name(name),KEY idx_pair(name,note)", `unique index "uk_name"`},
		{"列类型不同", "id BIGINT PRIMARY KEY,name VARCHAR(32) COLLATE utf8mb4_bin NOT NULL,note VARCHAR(16) COLLATE utf8mb4_bin NULL,UNIQUE KEY uk_name(name),KEY idx_pair(name,note)", `type="varchar(32)"`},
		{"排序规则不同", "id BIGINT PRIMARY KEY,name VARCHAR(16) COLLATE utf8mb4_general_ci NOT NULL,note VARCHAR(16) COLLATE utf8mb4_bin NULL,UNIQUE KEY uk_name(name),KEY idx_pair(name,note)", `collation="utf8mb4_general_ci"`},
		{"可空性不同", "id BIGINT PRIMARY KEY,name VARCHAR(16) COLLATE utf8mb4_bin NOT NULL,note VARCHAR(16) COLLATE utf8mb4_bin NOT NULL,UNIQUE KEY uk_name(name),KEY idx_pair(name,note)", `column 3 "note"`},
		{"缺省值不同", "id BIGINT PRIMARY KEY,name VARCHAR(16) COLLATE utf8mb4_bin NOT NULL,note VARCHAR(16) COLLATE utf8mb4_bin NULL DEFAULT 'x',UNIQUE KEY uk_name(name),KEY idx_pair(name,note)", `default="x"`},
		{"索引的列序不同", "id BIGINT PRIMARY KEY,name VARCHAR(16) COLLATE utf8mb4_bin NOT NULL,note VARCHAR(16) COLLATE utf8mb4_bin NULL,UNIQUE KEY uk_name(name),KEY idx_pair(note,name)", `index "idx_pair"`},
		{"少一个索引", "id BIGINT PRIMARY KEY,name VARCHAR(16) COLLATE utf8mb4_bin NOT NULL,note VARCHAR(16) COLLATE utf8mb4_bin NULL,UNIQUE KEY uk_name(name)", `index "idx_pair"`},
	}
	for _, test := range tests {
		recreate(test.columns)
		mismatches, err := m.compare(ctx, "verify", "", "")
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		if len(mismatches) != 1 || !strings.Contains(mismatches[0], "it_shape: the table definition differs") || !strings.Contains(mismatches[0], test.want) {
			t.Fatalf("%s: 定义不同要查出来并点名 %q: %v", test.name, test.want, mismatches)
		}
	}

	// 没有主键,只有一个非空唯一键:plan 用的清单点名它,比对直接拒绝。
	recreate("id BIGINT NOT NULL,name VARCHAR(16) COLLATE utf8mb4_bin NOT NULL,note VARCHAR(16) COLLATE utf8mb4_bin NULL,UNIQUE KEY uk_id(id)")
	missing, err := (&sqlAdmin{db: target.admin}).TablesWithoutPrimaryKey(ctx, target.schema)
	if err != nil || strings.Join(missing, ",") != "it_shape" {
		t.Fatalf("没有主键的表: %v %v", missing, err)
	}
	if missing, err = (&sqlAdmin{db: source.admin}).TablesWithoutPrimaryKey(ctx, source.schema); err != nil || len(missing) != 0 {
		t.Fatalf("源库的表都有主键(闸门表在内): %v %v", missing, err)
	}
	if _, err := m.compare(ctx, "verify", "", ""); !errors.Is(err, errRefused) || !strings.Contains(err.Error(), "table it_shape has no primary key") {
		t.Fatalf("没有主键的表要拒绝并点名: %v", err)
	}
}

// mysql 档读复制设置的语句在真实后端上的形状(verify、cutover 核跳过开关和通道过滤,cleanup 核通道撤掉了没有)。
// 测试实例上不配复制通道:过滤是空集合,通道查不到。通道真的在跑时的行为由手册 §10 的演练覆盖。
func TestITReplicaSettingsOnRealMySQL(t *testing.T) {
	requireIT(t)
	ctx, _, target, _, _ := itSetup(t)
	if target.backend != authoritysql.BackendMySQL {
		return
	}
	const channel = "dbmove_it_no_such_channel"
	repl := &gtidReplication{channel: channel, target: target.admin, schema: target.schema}
	digest, err := repl.channelConfig(ctx)
	if err != nil || digest != channelFilterDigest(nil) {
		t.Fatalf("测试实例不该开跳过开关,没有通道时过滤是空集合: %s %v", digest, err)
	}
	exists, _, err := repl.StillExists(ctx, dbplacement.FeedIdentity{Channel: channel})
	if err != nil || exists {
		t.Fatalf("没有配置过的通道: %v %v", exists, err)
	}
}
