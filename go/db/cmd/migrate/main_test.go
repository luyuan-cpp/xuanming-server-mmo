package main

import (
	"errors"
	"reflect"
	"testing"

	"db/internal/dbguard"
	"db/internal/logic/pkg/proto_sql"

	"github.com/go-sql-driver/mysql"
)

func TestMigrationConfigCanReadLedgerTimesWithoutChangingSharedSettings(t *testing.T) {
	shared := proto_sql.NewMysqlConfig()
	migration := migrationMysqlConfig()
	parsed, err := mysql.ParseDSN(migration.FormatDSN())
	if err != nil {
		t.Fatalf("parse migration connection options: %v", err)
	}
	if !parsed.ParseTime {
		t.Fatal("migration connection must decode ledger DATETIME fields for time.Time and sql.NullTime scans")
	}

	// 时间解析以外的参数必须仍与业务服务一致,包括数据库目标和严格模式。
	migration.ParseTime = shared.ParseTime
	if !reflect.DeepEqual(migration, shared) {
		t.Fatal("migration changed shared connection settings beyond ledger time decoding")
	}
}

// -storage-id 为 0 时与加这个参数之前逐字节相同:库名按 ZoneId 推导,白名单原样交给 runner。
func TestMigrationTargetWithoutStorageIDKeepsZoneBehaviour(t *testing.T) {
	allow := dbguard.Allowlist{Names: []string{"zone_7_db"}, Source: "test"}
	target, runnerAllow, admittedBy, err := migrationTarget(7, 0, allow, true, false)
	if err != nil {
		t.Fatalf("zone target: %v", err)
	}
	if target != "zone_7_db" || admittedBy != "" || !reflect.DeepEqual(runnerAllow, allow) {
		t.Fatalf("got (%q, %+v, %q), want (zone_7_db, unchanged allow list, \"\")", target, runnerAllow, admittedBy)
	}
	if _, _, _, err := migrationTarget(0, 0, allow, true, false); err == nil {
		t.Fatal("ZoneId 0 without -storage-id names no database")
	}
}

// 家族放行的库不在外部清单里:只给 runner 一份并进了目标库的有效清单,原清单不被改写。
func TestMigrationTargetFamilyAdmissionExtendsOnlyTheRunnerAllowList(t *testing.T) {
	// 留出容量:实现若就地 append,会把目标库写进调用方底层数组的空位里。
	names := make([]string, 1, 4)
	names[0] = "zone_1_db"
	allow := dbguard.Allowlist{Names: names, Source: "env:DB_ALLOWED_DATABASES"}
	target, runnerAllow, admittedBy, err := migrationTarget(1, 1000000, allow, true, false)
	if err != nil {
		t.Fatalf("family target: %v", err)
	}
	if target != "player_store_1000000_db" || admittedBy != proto_sql.StoreAdmittedByFamily {
		t.Fatalf("got (%q, %q)", target, admittedBy)
	}
	if !runnerAllow.Contains("player_store_1000000_db") || !runnerAllow.Contains("zone_1_db") {
		t.Fatalf("runner allow list = %s, want the original names plus the admitted store", runnerAllow.String())
	}
	if spare := names[:cap(names)][1]; spare != "" || allow.Contains("player_store_1000000_db") {
		t.Fatalf("the injected allow list must not be mutated (spare slot = %q)", spare)
	}
}

func TestMigrationTargetAllowListAdmissionPassesThrough(t *testing.T) {
	allow := dbguard.Allowlist{Names: []string{"zone_1_db", "zone_102_db"}, Source: "test"}
	target, runnerAllow, admittedBy, err := migrationTarget(1, 102, allow, false, false)
	if err != nil {
		t.Fatalf("allow-listed target: %v", err)
	}
	if target != "zone_102_db" || admittedBy != proto_sql.StoreAdmittedByAllowlist || !reflect.DeepEqual(runnerAllow, allow) {
		t.Fatalf("got (%q, %+v, %q)", target, runnerAllow, admittedBy)
	}
}

func TestMigrationTargetRejections(t *testing.T) {
	allow := dbguard.Allowlist{Names: []string{"zone_1_db"}, Source: "test"}
	cases := []struct {
		name       string
		storageID  uint64
		allow      dbguard.Allowlist
		families   bool
		relaxEmpty bool
		wantErr    error
	}{
		{name: "families off and not in the allow list", storageID: 1000000, allow: allow, families: false,
			wantErr: dbguard.ErrDatabaseNotAllowed},
		{name: "empty allow list in strict mode is refused even for a family name", storageID: 1000000,
			allow: dbguard.Allowlist{Source: "none"}, families: true, wantErr: dbguard.ErrAllowlistMissing},
		{name: "storage id wider than uint32", storageID: 1 << 32, allow: allow, families: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, err := migrationTarget(1, tc.storageID, tc.allow, tc.families, tc.relaxEmpty)
			if err == nil {
				t.Fatal("must be rejected")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// dev 的 warn 档:空白名单原样交给 runner,由它打 WARN 放行 —— 「宽松」要看得见,不被家族放行悄悄盖掉。
func TestMigrationTargetRelaxedEmptyAllowListStaysVisible(t *testing.T) {
	empty := dbguard.Allowlist{Source: "none"}
	target, runnerAllow, admittedBy, err := migrationTarget(1, 1000000, empty, true, true)
	if err != nil {
		t.Fatalf("relaxed dev target: %v", err)
	}
	if target != "player_store_1000000_db" || admittedBy != proto_sql.StoreAdmittedByFamily || !runnerAllow.Empty() {
		t.Fatalf("got (%q, %+v, %q), want the empty allow list handed to the runner", target, runnerAllow, admittedBy)
	}
	if _, _, _, err := migrationTarget(1, 1000000, empty, false, true); !errors.Is(err, dbguard.ErrDatabaseNotAllowed) {
		t.Fatalf("with families off even dev must not admit an unlisted store, err = %v", err)
	}
}
