// collation.go —— TiDB 最低版本校验与排序规则真实行为探针（移植自 xuanming-server/pkg/mysqlx/backend_check.go）。
//
// 为什么必须用行为探针而不是比对 collation 名：
//   - TiDB v7.4.0 起才支持 utf8mb4_0900_ai_ci，更早版本静默回退到 utf8mb4_bin 且不报错；
//   - 若集群首次 bootstrap 未启用 new_collations_enabled_on_first_bootstrap，即便语法接受 _ci 仍按二进制比较；
//   - NO PAD 与 PAD SPACE 退化会导致 "abc " 与 "abc" 唯一键语义在迁移或业务写时静默冲突/并号。
package authoritysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strconv"
)

const (
	// MinTiDBMajor / MinTiDBMinor 是权威 SQL 强依赖的 TiDB 最低版本（v7.4 起才真正支持 utf8mb4_0900_ai_ci，
	// 低版本静默回退到 utf8mb4_bin 且不报错）。
	MinTiDBMajor = 7
	MinTiDBMinor = 4
)

var (
	// 同时兼容 SELECT VERSION() 的 "-TiDB-v8.5.2" 与 SELECT tidb_version() 的 "Release Version: v8.5.2"。
	tidbSemverRe       = regexp.MustCompile(`(?:-TiDB-|Release Version:\s*)v(\d+)\.(\d+)\.(\d+)`)
	safeCollationIdent = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
)

// AssertTiDBVersionAtLeast 断言权威库为 TiDB 且版本不低于 major.minor。
// 优先读 SELECT tidb_version()（不受 TiDB server-version 伪装影响），失败时回退 SELECT VERSION()；
// 版本号无法解析时 fail-closed 拒绝启动。
func AssertTiDBVersionAtLeast(ctx context.Context, db *sql.DB, major, minor int) error {
	var version string
	if err := db.QueryRowContext(ctx, "SELECT tidb_version()").Scan(&version); err != nil {
		if fallbackErr := db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version); fallbackErr != nil {
			return fmt.Errorf("authoritysql: query TiDB version failed: %w", err)
		}
	}
	return tidbVersionAtLeast(version, major, minor)
}

func tidbVersionAtLeast(version string, major, minor int) error {
	m := tidbSemverRe.FindStringSubmatch(version)
	if m == nil {
		return fmt.Errorf("authoritysql: cannot parse TiDB version from %q (fail-closed)", version)
	}
	gotMajor, _ := strconv.Atoi(m[1])
	gotMinor, _ := strconv.Atoi(m[2])
	if gotMajor < major || (gotMajor == major && gotMinor < minor) {
		return fmt.Errorf("authoritysql: TiDB version v%d.%d is below required v%d.%d", gotMajor, gotMinor, major, minor)
	}
	return nil
}

// AssertColumnCollationSemantics 用实时行为探针断言 table.column 的排序规则语义。
// wantCaseInsensitive 要求 'A' = 'a'；wantNoPad 要求 'a ' != 'a'（NO PAD）。
// 任一维度不符或列不存在均 fail-closed 返回错误。
func AssertColumnCollationSemantics(
	ctx context.Context, db *sql.DB, table, column string, wantCaseInsensitive, wantNoPad bool,
) error {
	const q = `SELECT COLLATION_NAME FROM information_schema.COLUMNS
WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?`
	var collation sql.NullString
	if err := db.QueryRowContext(ctx, q, table, column).Scan(&collation); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("authoritysql: column %s.%s not found for collation check", table, column)
		}
		return fmt.Errorf("authoritysql: query collation for %s.%s failed: %w", table, column, err)
	}
	if !collation.Valid || collation.String == "" {
		return fmt.Errorf("authoritysql: column %s.%s has empty collation", table, column)
	}
	name := collation.String
	if !safeCollationIdent.MatchString(name) {
		return fmt.Errorf("authoritysql: column %s.%s has unsafe collation identifier %q", table, column, name)
	}

	var gotCaseInsensitive, gotPadSpace bool
	probe := fmt.Sprintf("SELECT 'A' COLLATE %s = 'a', 'a ' COLLATE %s = 'a'", name, name)
	if err := db.QueryRowContext(ctx, probe).Scan(&gotCaseInsensitive, &gotPadSpace); err != nil {
		return fmt.Errorf("authoritysql: collation %s behavior probe failed: %w", name, err)
	}

	if wantCaseInsensitive && !gotCaseInsensitive {
		return fmt.Errorf("authoritysql: column %s.%s collation %s is case-sensitive on this backend (expected case-insensitive)", table, column, name)
	}
	if !wantCaseInsensitive && gotCaseInsensitive {
		return fmt.Errorf("authoritysql: column %s.%s collation %s is case-insensitive on this backend (expected binary/case-sensitive)", table, column, name)
	}
	if wantNoPad && gotPadSpace {
		return fmt.Errorf("authoritysql: column %s.%s collation %s behaves as PAD SPACE (expected NO PAD)", table, column, name)
	}
	return nil
}
