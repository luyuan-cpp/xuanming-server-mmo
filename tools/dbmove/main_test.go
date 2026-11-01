package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"shared/authoritysql"
)

func flagsFor(command string, extra ...string) []string {
	args := []string{command, "-db", testDB}
	return append(args, extra...)
}

func TestParseOptionsAccepted(t *testing.T) {
	cases := [][]string{
		flagsFor(cmdPlan, "-target-cluster", "b"),
		flagsFor(cmdPlan, "-target-cluster", "b", "-roster", "a.json"),
		flagsFor(cmdPrepare, "-target-cluster", "b", "-run-id", testRun, "-roster", "a.json,b.json"),
		flagsFor(cmdVerify, "-run-id", testRun, "-sync", "ticdc:127.0.0.1:8300:feed-1"),
		flagsFor(cmdCutover, "-run-id", testRun, "-sync", "mysql-gtid", "-budget", "5s", "-roster", "a.json", "-metrics-targets", "a:1"),
		flagsFor(cmdRollback, "-run-id", testRun),
		flagsFor(cmdRollback, "-run-id", testRun, "-target-lost"),
		flagsFor(cmdAudit, "-run-id", testRun, "-metrics-targets", "a:1, b:2"),
		flagsFor(cmdCleanup, "-run-id", testRun, "-confirm-drop"),
	}
	for _, args := range cases {
		if _, err := parseOptions(args, io.Discard); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	o, err := parseOptions(flagsFor(cmdAudit, "-run-id", strings.ToUpper(testRun), "-metrics-targets", "a:1, b:2"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(o.metricsTargets) != 2 || o.metricsTargets[1] != "b:2" {
		t.Fatalf("metrics targets: %v", o.metricsTargets)
	}
	if o.runID != testRun {
		t.Fatalf("run id 统一小写,和映射值里的比: %s", o.runID)
	}
	if o.budget != 30*time.Second || o.retention != 7*24*time.Hour {
		t.Fatalf("缺省值: budget=%s retention=%s", o.budget, o.retention)
	}
}

func TestParseOptionsRefused(t *testing.T) {
	cases := map[string][]string{
		"没有子命令":         nil,
		"未知子命令":         {"move", "-db", testDB},
		"没有库名":          {cmdPlan, "-target-cluster", "b"},
		"库名非法":          {cmdPlan, "-db", "a`b", "-target-cluster", "b"},
		"plan 缺目标":      flagsFor(cmdPlan),
		"prepare 缺 run": flagsFor(cmdPrepare, "-target-cluster", "b"),
		"prepare 缺名册":   flagsFor(cmdPrepare, "-target-cluster", "b", "-run-id", testRun),
		"cutover 缺名册":   flagsFor(cmdCutover, "-run-id", testRun, "-sync", "mysql-gtid", "-metrics-targets", "a:1"),
		"cutover 缺进程":   flagsFor(cmdCutover, "-run-id", testRun, "-sync", "mysql-gtid", "-roster", "a.json"),
		"run id 形状不对":   flagsFor(cmdRollback, "-run-id", "abc"),
		"verify 缺 sync": flagsFor(cmdVerify, "-run-id", testRun),
		"sync 写错":       flagsFor(cmdCutover, "-run-id", testRun, "-sync", "dm"),
		"audit 缺进程":     flagsFor(cmdAudit, "-run-id", testRun),
		"多余参数":          flagsFor(cmdRollback, "-run-id", testRun, "secret"),
		"预算非正":          flagsFor(cmdCutover, "-run-id", testRun, "-sync", "mysql-gtid", "-budget", "0s"),
	}
	for name, args := range cases {
		if _, err := parseOptions(args, io.Discard); err == nil {
			t.Fatalf("%s 被接受了", name)
		}
	}
	// 写错位置的参数可能是口令,不回显。
	_, err := parseOptions(flagsFor(cmdRollback, "-run-id", testRun, "hunter2"), io.Discard)
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("多余参数不能回显: %v", err)
	}
}

func TestParseOptionsHelp(t *testing.T) {
	if _, err := parseOptions([]string{"-h"}, io.Discard); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("-h: %v", err)
	}
}

func TestParseSync(t *testing.T) {
	spec, err := parseSync("ticdc:127.0.0.1:8300:feed-1")
	if err != nil || spec.api != "http://127.0.0.1:8300" || spec.changefeed != "feed-1" || spec.backend != authoritysql.BackendTiDB {
		t.Fatalf("ticdc: %+v %v", spec, err)
	}
	spec, err = parseSync("ticdc:https://cdc.example:8300/:feed")
	if err != nil || spec.api != "https://cdc.example:8300" || spec.changefeed != "feed" {
		t.Fatalf("带协议的地址: %+v %v", spec, err)
	}
	spec, err = parseSync("mysql-gtid")
	if err != nil || spec.channel != defaultGTIDChannel || spec.backend != authoritysql.BackendMySQL {
		t.Fatalf("mysql-gtid: %+v %v", spec, err)
	}
	spec, err = parseSync("mysql-gtid:move_1")
	if err != nil || spec.channel != "move_1" {
		t.Fatalf("mysql-gtid 通道: %+v %v", spec, err)
	}
	for _, bad := range []string{"", "dm", "ticdc:feed", "ticdc:h:1:bad_feed!", "mysql-gtid:bad channel", "ticdc::feed"} {
		if _, err := parseSync(bad); err == nil {
			t.Fatalf("%q 被接受了", bad)
		}
	}
}

func TestDsnAddrDoesNotEchoSecret(t *testing.T) {
	addr, err := dsnAddr("X", "u:hunter2@tcp(10.0.0.1:4000)/db")
	if err != nil || addr != "10.0.0.1:4000" {
		t.Fatalf("地址: %s %v", addr, err)
	}
	_, err = dsnAddr("X", "u:hunter2 not a dsn")
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("连接串不能回显: %v", err)
	}
}

func TestParseVersionMetric(t *testing.T) {
	body := `# HELP db_placement_version x
# TYPE db_placement_version gauge
db_placement_version{logical_db="mmorpg_mail"} 9
db_placement_version{logical_db="mmorpg_guild"} 3
`
	version, found, err := parseVersionMetric(body, "mmorpg_guild")
	if err != nil || !found || version != 3 {
		t.Fatalf("解析: %d %v %v", version, found, err)
	}
	if _, found, err := parseVersionMetric(body, "mmorpg_trade"); err != nil || found {
		t.Fatalf("没有这个库: %v %v", found, err)
	}
	if _, _, err := parseVersionMetric(`db_placement_version{logical_db="x"} nope`, "x"); err == nil {
		t.Fatal("坏值要报错")
	}
	// 大数字的科学计数法。
	if version, _, err := parseVersionMetric(`db_placement_version{logical_db="x"} 1e+06`, "x"); err != nil || version != 1000000 {
		t.Fatalf("科学计数法: %d %v", version, err)
	}
}

func TestScrapeVersion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`db_placement_version{logical_db="mmorpg_guild"} 2` + "\n"))
	}))
	defer server.Close()
	version, found, err := scrapeVersion(context.Background(), server.URL, "mmorpg_guild")
	if err != nil || !found || version != 2 {
		t.Fatalf("抓取: %d %v %v", version, found, err)
	}
	server.Close()
	if _, _, err := scrapeVersion(context.Background(), server.URL, "mmorpg_guild"); err == nil {
		t.Fatal("连不上要报错")
	}
}

func TestTicdcDetail(t *testing.T) {
	var mu sync.Mutex
	var body string
	code := http.StatusOK
	respond := func(newCode int, newBody string) {
		mu.Lock()
		defer mu.Unlock()
		code, body = newCode, newBody
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/changefeeds/feed-1" {
			t.Errorf("路径: %s", r.URL.Path)
		}
		mu.Lock()
		defer mu.Unlock()
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	repl := &ticdcReplication{api: server.URL, changefeed: "feed-1", client: server.Client()}
	ctx := context.Background()

	respond(http.StatusOK, `{"id":"feed-1","state":"normal","checkpoint_ts":450000000000000000,"checkpoint_time":"x","resolved_ts":1}`)
	caught, err := repl.CaughtUp(ctx, "449999999999999999")
	if err != nil || !caught {
		t.Fatalf("checkpoint 过了冻结点: %v %v", caught, err)
	}
	caught, err = repl.CaughtUp(ctx, "450000000000000000")
	if err != nil || !caught {
		t.Fatalf("相等算追上: %v %v", caught, err)
	}
	caught, err = repl.CaughtUp(ctx, "450000000000000001")
	if err != nil || caught {
		t.Fatalf("没过冻结点: %v %v", caught, err)
	}
	if _, err := repl.CaughtUp(ctx, "not-a-number"); err == nil {
		t.Fatal("坏的冻结点要报错")
	}

	for _, state := range []string{"stopped", "failed", "error", "finished"} {
		respond(http.StatusOK, `{"state":"`+state+`","checkpoint_ts":1,"error":{"message":"boom"}}`)
		if _, err := repl.CaughtUp(ctx, "1"); !errors.Is(err, errReplicationBroken) {
			t.Fatalf("%s 算断开: %v", state, err)
		}
	}
	respond(http.StatusOK, `{"state":"warning","checkpoint_ts":5}`)
	if caught, err := repl.CaughtUp(ctx, "9"); err != nil || caught {
		t.Fatalf("warning 不算断开,只是还没追上: %v %v", caught, err)
	}

	respond(http.StatusNotFound, "")
	if _, err := repl.CaughtUp(ctx, "1"); !errors.Is(err, errReplicationBroken) {
		t.Fatalf("changefeed 不存在算断开: %v", err)
	}
	// TiCDC 实际回的是 400 加错误码。
	respond(http.StatusBadRequest, `{"error_msg":"changefeed not exists","error_code":"CDC:ErrChangeFeedNotExists"}`)
	if _, err := repl.CaughtUp(ctx, "1"); !errors.Is(err, errReplicationBroken) {
		t.Fatalf("changefeed 不存在算断开: %v", err)
	}
	respond(http.StatusBadRequest, `{"error_msg":"bad param","error_code":"CDC:ErrAPIInvalidParam"}`)
	if _, err := repl.CaughtUp(ctx, "1"); err == nil || errors.Is(err, errReplicationBroken) {
		t.Fatalf("别的 400 不算断开: %v", err)
	}
	respond(http.StatusInternalServerError, "")
	if _, err := repl.CaughtUp(ctx, "1"); err == nil || errors.Is(err, errReplicationBroken) {
		t.Fatalf("TiCDC 抖动不算断开: %v", err)
	}
}

func TestParseKnownClusterMetric(t *testing.T) {
	body := `# TYPE db_placement_known_cluster gauge
db_placement_known_cluster{cluster="a"} 1
db_placement_known_cluster{cluster="b"} 1
`
	if !parseKnownClusterMetric(body, "b") {
		t.Fatal("认识的集群")
	}
	if parseKnownClusterMetric(body, "c") {
		t.Fatal("没有这个序列就是不认识")
	}
	if parseKnownClusterMetric(`db_placement_known_cluster{cluster="b"} 0`, "b") {
		t.Fatal("值不是正数不算")
	}
}

func TestScrapeKnownCluster(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`db_placement_known_cluster{cluster="b"} 1` + "\n"))
	}))
	defer server.Close()
	if known, err := scrapeKnownCluster(context.Background(), server.URL, "b"); err != nil || !known {
		t.Fatalf("抓取: %v %v", known, err)
	}
	if known, err := scrapeKnownCluster(context.Background(), server.URL, "c"); err != nil || known {
		t.Fatalf("没有这个集群: %v %v", known, err)
	}
	server.Close()
	if _, err := scrapeKnownCluster(context.Background(), server.URL, "b"); err == nil {
		t.Fatal("连不上要报错")
	}
}

func TestChangefeedIdentity(t *testing.T) {
	var d changefeedDetail
	raw := `{"state":"normal","checkpoint_ts":7,"start_ts":5,"create_time":"2026-10-31 10:00:00.000","config":{"enable_sync_point":true,"sync_point_interval":"30s"}}`
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatal(err)
	}
	feed, err := changefeedIdentity("feed-1", d)
	if err != nil || feed.Changefeed != "feed-1" || feed.StartTs != 5 || feed.CreateTime != "2026-10-31 10:00:00.000" {
		t.Fatalf("身份: %+v %v", feed, err)
	}
	if interval, err := d.syncPointInterval(); err != nil || interval != 30*time.Second {
		t.Fatalf("间隔: %v %v", interval, err)
	}
	if _, err := changefeedIdentity("feed-1", changefeedDetail{CreateTime: "x"}); !errors.Is(err, errRefused) {
		t.Fatalf("没有 start_ts 分不出重建: %v", err)
	}
	if _, err := changefeedIdentity("feed-1", changefeedDetail{StartTs: 5}); !errors.Is(err, errRefused) {
		t.Fatalf("没有 create_time 分不出重建: %v", err)
	}
	for name, config := range map[string]string{
		"没有配置":     `{}`,
		"没开":       `{"config":{"enable_sync_point":false,"sync_point_interval":"30s"}}`,
		"没有间隔":     `{"config":{"enable_sync_point":true}}`,
		"间隔写坏了":    `{"config":{"sync_point_interval":"soon"}}`,
		"间隔是 0":    `{"config":{"sync_point_interval":0}}`,
		"间隔是 null": `{"config":{"sync_point_interval":null}}`,
	} {
		var bad changefeedDetail
		if err := json.Unmarshal([]byte(config), &bad); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := bad.syncPointInterval(); err == nil {
			t.Fatalf("%s 要报错", name)
		}
	}
	var old changefeedDetail
	if err := json.Unmarshal([]byte(`{"config":{"sync_point_interval":600000000000}}`), &old); err != nil {
		t.Fatal(err)
	}
	if interval, err := old.syncPointInterval(); err != nil || interval != 10*time.Minute {
		t.Fatalf("旧版的纳秒整数: %v %v", interval, err)
	}
}

func TestCheckSyncPoint(t *testing.T) {
	tso := func(ms int64) uint64 { return uint64(ms) << 18 }
	const nowMs = int64(1_800_000_000_000)
	interval := 30 * time.Second
	if err := checkSyncPoint("feed", tso(nowMs-60_000), tso(nowMs-3_600_000), tso(nowMs), interval); err != nil {
		t.Fatalf("新鲜的 syncpoint: %v", err)
	}
	// 同名的旧 changefeed 留下的行:primary_ts 早于新 changefeed 的 start_ts。
	err := checkSyncPoint("feed", tso(nowMs-60_000), tso(nowMs-30_000), tso(nowMs), interval)
	if !errors.Is(err, errRefused) || !strings.Contains(err.Error(), "earlier changefeed") {
		t.Fatalf("primary_ts 早于 start_ts 要拒绝: %v", err)
	}
	// 落后超过 N 个间隔。
	err = checkSyncPoint("feed", tso(nowMs-int64(verifySyncPointIntervals+1)*30_000), tso(1), tso(nowMs), interval)
	if !errors.Is(err, errRefused) || !strings.Contains(err.Error(), "sync point intervals") {
		t.Fatalf("太旧要拒绝: %v", err)
	}
	if err := checkSyncPoint("feed", tso(nowMs-int64(verifySyncPointIntervals)*30_000), tso(1), tso(nowMs), interval); err != nil {
		t.Fatalf("正好 N 个间隔还算新: %v", err)
	}
}

func TestChannelIdentity(t *testing.T) {
	feed, err := channelIdentity("dbmove", "1", "mysql-a", 3306, "3E11FA47-71CA-11E1-9E33-C80AA9429562")
	if err != nil || feed.Channel != "dbmove" || feed.SourceHost != "mysql-a" || feed.SourcePort != 3306 || feed.SourceUUID != "3e11fa47-71ca-11e1-9e33-c80aa9429562" {
		t.Fatalf("身份: %+v %v", feed, err)
	}
	if _, err := channelIdentity("dbmove", "0", "mysql-a", 3306, "uuid"); !errors.Is(err, errRefused) || !strings.Contains(err.Error(), "SOURCE_AUTO_POSITION") {
		t.Fatalf("AUTO_POSITION 没开要拒绝: %v", err)
	}
	if _, err := channelIdentity("dbmove", "1", "mysql-a", 3306, ""); !errors.Is(err, errRefused) {
		t.Fatalf("还没连上来源分不清身份: %v", err)
	}
}

func TestQuoteAccount(t *testing.T) {
	for principal, want := range map[string]string{
		"writer@%":               "'writer'@'%'",
		"normal_01@10.0.0.%":     "'normal_01'@'10.0.0.%'",
		"a.b-c@host-1.example":   "'a.b-c'@'host-1.example'",
		"odd@name@%":             "",
		"no_host":                "",
		"@%":                     "",
		"writer'; DROP USER x@%": "",
		"writer@%'":              "",
	} {
		got, err := quoteAccount(principal)
		if want == "" {
			if err == nil {
				t.Fatalf("%q 要拒绝: %s", principal, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Fatalf("%q: %s %v", principal, got, err)
		}
	}
}

func TestParseGrantShapes(t *testing.T) {
	tests := []struct {
		line   string
		object string
		schema string
		privs  string
	}{
		{"GRANT SELECT, INSERT ON `mmorpg_guild`.`route_meta` TO `w`@`%`", "mmorpg_guild.route_meta", "mmorpg_guild", "INSERT,SELECT"},
		{"GRANT Select,Insert ON mmorpg_guild.route_meta TO 'w'@'%'", "mmorpg_guild.route_meta", "mmorpg_guild", "INSERT,SELECT"},
		{"GRANT USAGE ON *.* TO 'w'@'%'", "*.*", "*", "USAGE"},
		{"GRANT ALL PRIVILEGES ON `db`.* TO 'w'@'%' WITH GRANT OPTION", "db.*", "db", "ALL PRIVILEGES"},
		{"GRANT SELECT (`b`, `a`), UPDATE (`lock_touch`) ON `db`.`writer_slot` TO 'w'@'%'", "db.writer_slot", "db", "SELECT(A,B),UPDATE(LOCK_TOUCH)"},
	}
	for _, test := range tests {
		grant := parseGrant(test.line)
		var privs []string
		for priv := range grant.privs {
			privs = append(privs, priv)
		}
		slices.Sort(privs)
		if grant.object != test.object || grant.schema != test.schema || strings.Join(privs, ",") != test.privs {
			t.Fatalf("%s: %+v", test.line, grant)
		}
	}
	// 没有 ON 的行(角色)按整行比。
	if grant := parseGrant("GRANT `r`@`%` TO `w`@`%`"); grant.schema != "" || !grant.privs["GRANT `r`@`%` TO `w`@`%`"] {
		t.Fatalf("角色行: %+v", grant)
	}
}

func TestOtherSchemasOf(t *testing.T) {
	got := otherSchemasOf([]string{
		"GRANT USAGE ON *.* TO 'w'@'%'",
		"GRANT SELECT ON `mmorpg_guild`.`route_meta` TO 'w'@'%'",
		"GRANT SELECT ON `mysql`.`user` TO 'w'@'%'",
		"GRANT SELECT ON `player_store_2_db`.`player_store` TO 'w'@'%'",
		"GRANT UPDATE ON `player_store_2_db`.`writer_slot` TO 'w'@'%'",
	}, "mmorpg_guild")
	if strings.Join(got, ",") != "player_store_2_db" {
		t.Fatalf("别的业务库: %v", got)
	}
	if got := otherSchemasOf([]string{"GRANT SELECT ON *.* TO 'w'@'%'"}, "mmorpg_guild"); strings.Join(got, ",") != "*" {
		t.Fatalf("全局授权锁了也会连带别的库: %v", got)
	}
}

func TestMissingGrants(t *testing.T) {
	source := []string{
		"GRANT USAGE ON *.* TO `w`@`%`",
		"GRANT SELECT, INSERT ON `db`.`t` TO `w`@`%`",
		"GRANT SELECT ON `other`.`t` TO `w`@`%`",
		"GRANT SELECT (`a`, `b`) ON `db`.`c` TO `w`@`%`",
	}
	same := []string{"GRANT Insert,Select ON db.t TO 'w'@'%'", "GRANT SELECT (`b`, `a`) ON db.c TO 'w'@'%'"}
	if got := missingGrants(source, same, "db"); len(got) != 0 {
		t.Fatalf("写法不同内容相同,别的库不比: %v", got)
	}
	if got := missingGrants(source, []string{"GRANT SELECT ON db.t TO 'w'@'%'"}, "db"); strings.Join(got, ";") != "INSERT on db.t;SELECT(A,B) on db.c" {
		t.Fatalf("缺的要列出来: %v", got)
	}
	for _, broader := range []string{"GRANT ALL PRIVILEGES ON `db`.* TO 'w'@'%'", "GRANT ALL PRIVILEGES ON *.* TO 'w'@'%'"} {
		if got := missingGrants(source, []string{broader}, "db"); len(got) != 0 {
			t.Fatalf("目标上更宽的授权也算: %s: %v", broader, got)
		}
	}
}

func TestLoadRosterPrincipals(t *testing.T) {
	writeRoster := func(name, body string) string {
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	principal := func(text string) string { return base64.StdEncoding.EncodeToString([]byte(text)) }
	request := func(schema, account string) string {
		return fmt.Sprintf(`{"authoritySchema":%q,"writer":{"sqlPrincipal":%q}}`, schema, principal(account))
	}
	roster := func(entries ...string) string {
		return `{"format":"` + rosterFormat + `","entries":[` + strings.Join(entries, ",") + `]}`
	}
	first := writeRoster("db.json", roster(`{"requests":[`+request(testDB, "db_01@%")+`,`+request("other_db", "db_01@%")+`]}`, `{"requests":[`+request(testDB, "db_02@%")+`]}`))
	second := writeRoster("ds.json", roster(`{"requests":[`+request(testDB, "ds_01@%")+`]}`, `{"requests":[`+request(testDB, "db_01@%")+`]}`))
	got, err := loadRosterPrincipals([]string{first, second}, testDB)
	if err != nil || strings.Join(got, ",") != "db_01@%,db_02@%,ds_01@%" {
		t.Fatalf("多个名册合并去重: %v %v", got, err)
	}
	if got, err := loadRosterPrincipals(nil, testDB); err != nil || len(got) != 0 {
		t.Fatalf("没给名册: %v %v", got, err)
	}
	if _, err := loadRosterPrincipals([]string{writeRoster("x.json", roster(`{"requests":[`+request("other_db", "a@%")+`]}`))}, testDB); err == nil || !strings.Contains(err.Error(), "wrong file") {
		t.Fatalf("文件里没有这个库: %v", err)
	}
	if _, err := loadRosterPrincipals([]string{writeRoster("f.json", `{"format":"OTHER","entries":[]}`)}, testDB); err == nil {
		t.Fatal("格式不对要拒绝")
	}
	if _, err := loadRosterPrincipals([]string{writeRoster("b.json", `not json`)}, testDB); err == nil {
		t.Fatal("坏文件要拒绝")
	}
	if _, err := loadRosterPrincipals([]string{filepath.Join(t.TempDir(), "missing.json")}, testDB); err == nil {
		t.Fatal("文件不在要拒绝")
	}
}

func TestTsoPhysicalMs(t *testing.T) {
	const physical = int64(1_700_000_000_000)
	tso := uint64(physical)<<18 | 12345
	if got := tsoPhysicalMs(tso); got != physical {
		t.Fatalf("物理时间: %d", got)
	}
}

func TestGTIDHelpers(t *testing.T) {
	const a, b = "3e11fa47-71ca-11e1-9e33-c80aa9429562", "4e11fa47-71ca-11e1-9e33-c80aa9429562"
	if got := normalizeGTIDSet(a + ":1-5,\n" + b + ":1-3\n"); got != a+":1-5,"+b+":1-3" {
		t.Fatalf("去空白: %s", got)
	}
	uuids := gtidUUIDs(a + ":1-5,\n" + strings.ToUpper(b) + ":1-3")
	if len(uuids) != 2 || !uuids[a] || !uuids[b] {
		t.Fatalf("UUID: %v", uuids)
	}
	if len(gtidUUIDs("")) != 0 {
		t.Fatal("空集合")
	}
	if err := checkGTIDAligned(a+":1-5", b, "", b+":1-3"); err != nil {
		t.Fatalf("目标自己 UUID 上多出来的不算: %v", err)
	}
	if err := checkGTIDAligned(a+":1-5", b, a+":5", ""); !errors.Is(err, errRefused) {
		t.Fatalf("目标落后: %v", err)
	}
	if err := checkGTIDAligned(a+":1-5", b, "", a+":6-7"); !errors.Is(err, errRefused) {
		t.Fatalf("目标超前: %v", err)
	}
	// 这个库以前是从目标搬过来的:目标的 UUID 也在源副本的集合里,目标自己之后又写过。
	if err := checkGTIDAligned(a+":1-5,"+b+":1-3", strings.ToUpper(b), "", b+":4-9"); err != nil {
		t.Fatalf("反向再搬时目标自己的 UUID 也不算: %v", err)
	}
}

func TestCheckVerifySource(t *testing.T) {
	const source, target, replica = "aaaaaaaa-0000-0000-0000-000000000001", "bbbbbbbb-0000-0000-0000-000000000002", "cccccccc-0000-0000-0000-000000000003"
	if err := checkVerifySource(replica, target, source, source+":1-5"); err != nil {
		t.Fatalf("源库的副本: %v", err)
	}
	if err := checkVerifySource(strings.ToUpper(target), target, source, source+":1-5"); !errors.Is(err, errRefused) {
		t.Fatalf("连接串指到目标自己,verify 等于没比: %v", err)
	}
	if err := checkVerifySource(replica, target, source, replica+":1-5"); !errors.Is(err, errRefused) {
		t.Fatalf("集合里没有复制来源的事务,不是源库的副本: %v", err)
	}
}

func TestHasGlobalPrivilege(t *testing.T) {
	for _, grants := range [][]string{
		{"GRANT PROCESS ON *.* TO 'admin'@'%'"},
		{"GRANT SELECT,PROCESS ON *.* TO 'admin'@'%'"},
		{"GRANT ALL PRIVILEGES ON *.* TO 'admin'@'%' WITH GRANT OPTION"},
	} {
		if !hasGlobalPrivilege(grants, "PROCESS") {
			t.Fatalf("有全局权限: %v", grants)
		}
	}
	for _, grants := range [][]string{
		nil,
		{"GRANT USAGE ON *.* TO 'admin'@'%'"},
		{"GRANT ALL PRIVILEGES ON `mmorpg_guild`.* TO 'admin'@'%'"},
	} {
		if hasGlobalPrivilege(grants, "PROCESS") {
			t.Fatalf("没有全局权限: %v", grants)
		}
	}
}

func TestChangefeedBroken(t *testing.T) {
	for state, want := range map[string]bool{"normal": false, "warning": false, "pending": false, "stopped": true, "error": true, "failed": true, "finished": true, "removed": true} {
		if changefeedBroken(state) != want {
			t.Fatalf("%s", state)
		}
	}
}

func TestChangefeedDetailKeepsDocumentedFieldName(t *testing.T) {
	var d changefeedDetail
	if err := json.Unmarshal([]byte(`{"state":"normal","checkpoint_ts":7}`), &d); err != nil || d.CheckpointTs != 7 {
		t.Fatalf("字段名是 OpenAPI v2 的 checkpoint_ts: %+v %v", d, err)
	}
}

func TestTiDBRowDigestSQL(t *testing.T) {
	got := tidbRowDigestSQL("`db`.`t`", []string{"id", "name"})
	for _, want := range []string{
		"BIT_XOR(", "MD5(", "CONCAT_WS(',',`id`,`name`,CONCAT(ISNULL(`id`),ISNULL(`name`)))", "FROM `db`.`t`", "SELECT COUNT(*),",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("缺 %q: %s", want, got)
		}
	}
}

type mapView struct {
	tables map[string]tableDigest
	err    error
}

func (v mapView) Tables(context.Context) ([]string, error) {
	var out []string
	for name := range v.tables {
		out = append(out, name)
	}
	return out, nil
}
func (v mapView) TableDigest(_ context.Context, table string) (tableDigest, error) {
	return v.tables[table], v.err
}
func (mapView) CheckPoint(context.Context, string) error   { return nil }
func (mapView) GateRows(context.Context) (gateRows, error) { return gateRows{}, nil }
func (mapView) Close() error                               { return nil }

func TestCompareViews(t *testing.T) {
	row := tableDigest{Columns: "id", Rows: 1, KVs: 2, Digest: "x"}
	source := mapView{tables: map[string]tableDigest{"a": row, "b": row, "c": row}}
	target := mapView{tables: map[string]tableDigest{"a": row, "b": {Columns: "id", Rows: 1, KVs: 3, Digest: "x"}, "d": row}}
	mismatches, err := compareViews(context.Background(), source, target, func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(mismatches, "\n")
	for _, want := range []string{"b: source", "c: missing on the target", "d: exists only on the target"} {
		if !strings.Contains(text, want) {
			t.Fatalf("缺 %q: %s", want, text)
		}
	}
	if len(mismatches) != 3 {
		t.Fatalf("a 是一致的: %v", mismatches)
	}
	// 只差列顺序也算不一致:同样的值摘要会不同,不能当成巧合放过。
	reordered := mapView{tables: map[string]tableDigest{"a": {Columns: "x,id", Rows: 1, KVs: 2, Digest: "x"}}}
	mismatches, err = compareViews(context.Background(), mapView{tables: map[string]tableDigest{"a": row}}, reordered, func(string, ...any) {})
	if err != nil || len(mismatches) != 1 {
		t.Fatalf("列不同: %v %v", mismatches, err)
	}
	if _, err := compareViews(context.Background(), mapView{}, mapView{}, func(string, ...any) {}); err == nil {
		t.Fatal("源库没有表要报错:指错库不能算一致")
	}
	broken := mapView{tables: map[string]tableDigest{"a": row}, err: errors.New("gc safe point")}
	if _, err := compareViews(context.Background(), broken, target, func(string, ...any) {}); err == nil {
		t.Fatal("没比完不能算一致")
	}
}

func TestJournalWritesFileWithoutSecrets(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested")
	var out bytes.Buffer
	j, err := openJournal(dir, testDB, testRun, &out, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	j.write("cutover", "source frozen")
	j.record("cutover", "ERROR: boom")
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "dbmove-"+testDB+"-"+testRun+".log"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("文件: %s", raw)
	}
	var entry struct{ Time, Step, Line string }
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil || entry.Step != "cutover" || entry.Line != "source frozen" || entry.Time == "" {
		t.Fatalf("行: %s %v", lines[0], err)
	}
	if !strings.Contains(out.String(), "[cutover] source frozen") || strings.Contains(out.String(), "boom") {
		t.Fatalf("屏幕: %s", out.String())
	}
	// 同一个 run 重跑是追加。
	j, err = openJournal(dir, testDB, testRun, io.Discard, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	j.write("audit", "ok")
	_ = j.Close()
	raw, _ = os.ReadFile(filepath.Join(dir, "dbmove-"+testDB+"-"+testRun+".log"))
	if len(strings.Split(strings.TrimSpace(string(raw)), "\n")) != 3 {
		t.Fatalf("要追加: %s", raw)
	}
}

// run 的入口判定:配置错的时候不连任何东西。
func TestRunRefusesBeforeConnecting(t *testing.T) {
	var errOut bytes.Buffer
	env := map[string]string{}
	getenv := func(name string) string { return env[name] }
	args := flagsFor(cmdRollback, "-run-id", testRun, "-log-dir", t.TempDir())
	if code := run(args, getenv, io.Discard, &errOut); code != exitRefused || !strings.Contains(errOut.String(), "MMORPG_DB_PLACEMENT") {
		t.Fatalf("没有映射配置: %d %s", code, errOut.String())
	}
	env["MMORPG_DB_PLACEMENT"] = `{"DefaultCluster":"a","SqlClusters":{"a":{"Hosts":"h:1"}},"EtcdHosts":["127.0.0.1:1"]}`
	errOut.Reset()
	if code := run(args, getenv, io.Discard, &errOut); code != exitRefused || !strings.Contains(errOut.String(), envPhysical) {
		t.Fatalf("没有物理摘要: %d %s", code, errOut.String())
	}
	env[envPhysical] = strings.Repeat("ab", 32)
	errOut.Reset()
	if code := run(args, getenv, io.Discard, &errOut); code != exitRefused || !strings.Contains(errOut.String(), envOperator) {
		t.Fatalf("没有维护身份: %d %s", code, errOut.String())
	}
	if code := run([]string{"nope"}, getenv, io.Discard, io.Discard); code != exitRefused {
		t.Fatalf("未知子命令: %d", code)
	}
	if code := run([]string{"-h"}, getenv, io.Discard, io.Discard); code != exitOK {
		t.Fatalf("-h: %d", code)
	}
	// -sync 和档位对不上。
	env[envOperator] = "maintenance@%"
	env[authoritysql.BackendEnv] = "mysql"
	errOut.Reset()
	syncArgs := flagsFor(cmdVerify, "-run-id", testRun, "-sync", "ticdc:h:1:feed", "-log-dir", t.TempDir())
	if code := run(syncArgs, getenv, io.Discard, &errOut); code != exitRefused || !strings.Contains(errOut.String(), "-sync") {
		t.Fatalf("-sync 和档位不符: %d %s", code, errOut.String())
	}
	// cutover 要核注册、schema 账号,没配主体在连任何东西之前拒绝。
	roster := filepath.Join(t.TempDir(), "roster.json")
	body := `{"format":"` + rosterFormat + `","entries":[{"requests":[{"authoritySchema":"` + testDB + `","writer":{"sqlPrincipal":"d0BA"}}]}]}`
	if err := os.WriteFile(roster, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	errOut.Reset()
	cutoverArgs := flagsFor(cmdCutover, "-run-id", testRun, "-sync", "mysql-gtid", "-roster", roster, "-metrics-targets", "a:1", "-log-dir", t.TempDir())
	if code := run(cutoverArgs, getenv, io.Discard, &errOut); code != exitRefused || !strings.Contains(errOut.String(), envRegistrar) {
		t.Fatalf("没有注册主体: %d %s", code, errOut.String())
	}
	env[envRegistrar] = "registrar@%"
	errOut.Reset()
	if code := run(cutoverArgs, getenv, io.Discard, &errOut); code != exitRefused || !strings.Contains(errOut.String(), envSchema) {
		t.Fatalf("没有 schema 主体: %d %s", code, errOut.String())
	}
	errOut.Reset()
	badRoster := flagsFor(cmdCutover, "-run-id", testRun, "-sync", "mysql-gtid", "-roster", filepath.Join(t.TempDir(), "missing.json"), "-metrics-targets", "a:1", "-log-dir", t.TempDir())
	if code := run(badRoster, getenv, io.Discard, &errOut); code != exitRefused || !strings.Contains(errOut.String(), "roster") {
		t.Fatalf("名册读不到: %d %s", code, errOut.String())
	}
}
