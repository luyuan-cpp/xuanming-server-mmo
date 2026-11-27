// schema_check.go —— 启动期表/列/形状 fail-fast 守卫（移植自 xuanming-server/pkg/mysqlx/schema.go）。
//
// 既有 MySQL/TiDB volume 升级后不会自动重放 init SQL；服务启动期主动查 information_schema，
// 在进入流量前拦截缺表、缺列、列类型/可空性/键形状漂移，错误文本指向对应迁移 SQL。
package authoritysql

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// CheckTables verifies that all expected tables exist in the current DATABASE().
func CheckTables(ctx context.Context, db *sql.DB, migrationHint string, tables ...string) error {
	if len(tables) == 0 {
		return nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(tables)), ",")
	args := make([]any, len(tables))
	for i, t := range tables {
		args[i] = t
	}
	rows, err := db.QueryContext(ctx,
		`SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE() AND table_name IN (`+placeholders+`)`,
		args...)
	if err != nil {
		return fmt.Errorf("authoritysql: query information_schema.tables: %w", err)
	}
	defer func() { _ = rows.Close() }()

	present := make(map[string]struct{}, len(tables))
	for rows.Next() {
		var name string
		if serr := rows.Scan(&name); serr != nil {
			return fmt.Errorf("authoritysql: scan table_name: %w", serr)
		}
		present[strings.ToLower(name)] = struct{}{}
	}
	if rerr := rows.Err(); rerr != nil {
		return fmt.Errorf("authoritysql: iterate information_schema.tables: %w", rerr)
	}

	var missing []string
	for _, t := range tables {
		if _, ok := present[strings.ToLower(t)]; !ok {
			missing = append(missing, t)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("authoritysql: missing tables %v in current database; apply migration %s before startup", missing, migrationHint)
	}
	return nil
}

// CheckColumns verifies that table contains all required column names (case-insensitive).
func CheckColumns(ctx context.Context, db *sql.DB, migrationHint, table string, columns ...string) error {
	if len(columns) == 0 {
		return nil
	}
	rows, err := db.QueryContext(ctx,
		`SELECT column_name FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = ?`,
		table)
	if err != nil {
		return fmt.Errorf("authoritysql: query information_schema.columns for %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	present := make(map[string]struct{}, len(columns))
	for rows.Next() {
		var name string
		if serr := rows.Scan(&name); serr != nil {
			return fmt.Errorf("authoritysql: scan column_name: %w", serr)
		}
		present[strings.ToLower(name)] = struct{}{}
	}
	if rerr := rows.Err(); rerr != nil {
		return fmt.Errorf("authoritysql: iterate information_schema.columns: %w", rerr)
	}

	var missing []string
	for _, c := range columns {
		if _, ok := present[strings.ToLower(c)]; !ok {
			missing = append(missing, c)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("authoritysql: table %s missing columns %v; apply migration %s before startup", table, missing, migrationHint)
	}
	return nil
}

// ColumnSpec describes the expected shape of a column in information_schema.columns.
// Empty fields skip that dimension so callers can adopt checks incrementally.
type ColumnSpec struct {
	Name     string
	DataType string // lowercase DATA_TYPE, e.g. "bigint", "varchar", "tinyint"
	Nullable string // "NO" or "YES"
	Key      string // "PRI", "UNI", "MUL", or "" (skip)
}

// CheckColumnSpecs verifies that each specified column exists and matches DataType/Nullable/Key.
func CheckColumnSpecs(ctx context.Context, db *sql.DB, migrationHint, table string, specs ...ColumnSpec) error {
	if len(specs) == 0 {
		return nil
	}
	rows, err := db.QueryContext(ctx,
		`SELECT column_name, data_type, is_nullable, column_key FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = ?`,
		table)
	if err != nil {
		return fmt.Errorf("authoritysql: query information_schema.columns for %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()

	type colShape struct {
		dataType string
		nullable string
		key      string
	}
	present := make(map[string]colShape)
	for rows.Next() {
		var name, dataType, nullable, key string
		if serr := rows.Scan(&name, &dataType, &nullable, &key); serr != nil {
			return fmt.Errorf("authoritysql: scan column shape: %w", serr)
		}
		present[strings.ToLower(name)] = colShape{
			dataType: strings.ToLower(dataType),
			nullable: strings.ToUpper(nullable),
			key:      strings.ToUpper(key),
		}
	}
	if rerr := rows.Err(); rerr != nil {
		return fmt.Errorf("authoritysql: iterate information_schema.columns: %w", rerr)
	}

	var problems []string
	for _, spec := range specs {
		shape, ok := present[strings.ToLower(spec.Name)]
		if !ok {
			problems = append(problems, fmt.Sprintf("%s: missing column", spec.Name))
			continue
		}
		if spec.DataType != "" && shape.dataType != strings.ToLower(spec.DataType) {
			problems = append(problems, fmt.Sprintf("%s: data_type=%s want=%s", spec.Name, shape.dataType, strings.ToLower(spec.DataType)))
		}
		if spec.Nullable != "" && shape.nullable != strings.ToUpper(spec.Nullable) {
			problems = append(problems, fmt.Sprintf("%s: is_nullable=%s want=%s", spec.Name, shape.nullable, strings.ToUpper(spec.Nullable)))
		}
		if spec.Key != "" && shape.key != strings.ToUpper(spec.Key) {
			problems = append(problems, fmt.Sprintf("%s: column_key=%q want=%q", spec.Name, shape.key, strings.ToUpper(spec.Key)))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("authoritysql: table %s schema mismatch [%s]; reconcile against %s before startup",
			table, strings.Join(problems, "; "), migrationHint)
	}
	return nil
}
