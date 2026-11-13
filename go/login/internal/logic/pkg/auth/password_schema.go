package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// password_schema.go —— 口令凭据表的归属与形状守卫。
//
// # 归属
//
// 口令凭据按账号存,账号是全服的,所以凭据全服一份,放在 login 登记写者资格的那个逻辑库(PasswordAuthDatabase,
// 在全球权威 SQL 入口上)。建表语句的唯一来源是 go/login/model/password_auth_schema.sql,由 mmorpg 的建表 Job
// (writer_authority prepare-schema -ddl-file)执行;本机与集群内开发 MySQL 用 deploy/mysql-init/gateway_tables.sql 里的同一条语句。
// 存档库(zone 库)里同名的 user_accounts 是 db 服务的旧表(带 simple_players),不是口令权威:
// 那里每个区各有一份,一个账号的口令会分成几份;login 去读别的服务的库也违反逻辑库自治(AGENTS.md §7 第 6 条)。
//
// # 为什么在启动时核对形状
//
// 运行期的三条前提(账号不重复、账号不为 NULL 且不超过 191 个字符、口令列放得下 Argon2id PHC 串)原来由一段只能在 MySQL 上跑的
// 存储过程(migrations/20260803_password_auth.sql,2026-11-12 删除)在迁移存量表时检查。TiDB 不支持存储过程,项目也没有存量表要迁,
// 所以改成约束本身:主键、NOT NULL 与列宽由建表语句给出,login 与 password_admin 每次启动核对表确实是这个形状,不是就拒绝启动。
// 排序规则也核:账号匹配不区分大小写与重音、再以库里那一行的原值为准(LookupPasswordHash 返回的 canonicalAccount),
// 这个语义来自 account 列的排序规则。只写 CHARSET 时 MySQL 与 TiDB 取的缺省排序规则不同,同一个账号名在两边会一个登得上、一个登不上。

// PasswordAuthDatabase 是口令凭据表所在的逻辑库。生产(非 dev / test)配置的 PasswordAuth.Database 必须是它。
const PasswordAuthDatabase = "mmorpg"

const (
	passwordTable = "user_accounts"
	// passwordAccountCollation 决定账号怎么匹配,与 go/login/model/password_auth_schema.sql 里写的一致。
	passwordAccountCollation = "utf8mb4_unicode_ci"
	passwordHashColumnWidth  = 255
)

// passwordTableColumn 是 information_schema.COLUMNS 里与凭据表形状有关的几项。
type passwordTableColumn struct {
	name      string
	dataType  string // 小写,如 varchar
	width     int64  // CHARACTER_MAXIMUM_LENGTH;非字符列为 0
	nullable  bool
	collation string
}

// shapeQueryer 是形状核对需要的读能力;*sql.DB 与 *sql.Tx 都满足。
type shapeQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// VerifyPasswordTableShape 核对 database 里的口令凭据表是 password_auth_schema.sql 建出来的形状,不是就返回错误。
// 只读 information_schema,不碰任何账号行;login 启动与 password_admin 写入之前各调一次。
func VerifyPasswordTableShape(ctx context.Context, db shapeQueryer, database string) error {
	columns, err := readPasswordTableColumns(ctx, db, database)
	if err != nil {
		return err
	}
	primaryKey, err := readPasswordTablePrimaryKey(ctx, db, database)
	if err != nil {
		return err
	}
	if err := checkPasswordTableShape(columns, primaryKey); err != nil {
		return fmt.Errorf("password credential table %s.%s has an unexpected shape (create it from go/login/model/password_auth_schema.sql): %w",
			database, passwordTable, err)
	}
	return nil
}

func readPasswordTableColumns(ctx context.Context, db shapeQueryer, database string) ([]passwordTableColumn, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT COLUMN_NAME, DATA_TYPE, COALESCE(CHARACTER_MAXIMUM_LENGTH, 0), IS_NULLABLE, COALESCE(COLLATION_NAME, '')
		   FROM INFORMATION_SCHEMA.COLUMNS
		  WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?`, database, passwordTable)
	if err != nil {
		return nil, fmt.Errorf("read columns of %s.%s: %w", database, passwordTable, err)
	}
	defer rows.Close()

	var columns []passwordTableColumn
	for rows.Next() {
		var column passwordTableColumn
		var nullable string
		if err := rows.Scan(&column.name, &column.dataType, &column.width, &nullable, &column.collation); err != nil {
			return nil, fmt.Errorf("scan columns of %s.%s: %w", database, passwordTable, err)
		}
		column.dataType = strings.ToLower(column.dataType)
		column.nullable = strings.EqualFold(nullable, "YES")
		columns = append(columns, column)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate columns of %s.%s: %w", database, passwordTable, err)
	}
	return columns, nil
}

func readPasswordTablePrimaryKey(ctx context.Context, db shapeQueryer, database string) ([]string, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT COLUMN_NAME FROM INFORMATION_SCHEMA.STATISTICS
		  WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ? AND INDEX_NAME = 'PRIMARY'
		  ORDER BY SEQ_IN_INDEX`, database, passwordTable)
	if err != nil {
		return nil, fmt.Errorf("read primary key of %s.%s: %w", database, passwordTable, err)
	}
	defer rows.Close()

	var primaryKey []string
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			return nil, fmt.Errorf("scan primary key of %s.%s: %w", database, passwordTable, err)
		}
		primaryKey = append(primaryKey, column)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate primary key of %s.%s: %w", database, passwordTable, err)
	}
	return primaryKey, nil
}

// checkPasswordTableShape 是纯判定,每一条对应运行期的一条前提:
//   - 主键恰好是 account 一列:同一个账号只有一行(LookupPasswordHash 读到两行就是权威损坏);
//   - account 是 NOT NULL 的 VARCHAR(passwordAccountMaxRunes):与 ValidatePasswordAccount 的边界一致;
//   - account 的排序规则是 passwordAccountCollation:账号匹配语义在 MySQL 与 TiDB 上相同;
//   - password 是 VARCHAR(255):放得下 Argon2id PHC 串。可空,NULL 表示这个账号停用口令登录。
//
// 别的列不管:本包只读写这两列。
func checkPasswordTableShape(columns []passwordTableColumn, primaryKey []string) error {
	if len(columns) == 0 {
		return errors.New("table is missing")
	}
	if len(primaryKey) != 1 || !strings.EqualFold(primaryKey[0], "account") {
		return fmt.Errorf("primary key must be exactly (account), got (%s)", strings.Join(primaryKey, ", "))
	}
	account, ok := findPasswordTableColumn(columns, "account")
	if !ok {
		return errors.New("column account is missing")
	}
	if account.dataType != "varchar" || account.nullable || account.width != passwordAccountMaxRunes {
		return fmt.Errorf("column account must be VARCHAR(%d) NOT NULL, got %s(%d) nullable=%t",
			passwordAccountMaxRunes, account.dataType, account.width, account.nullable)
	}
	if !strings.EqualFold(account.collation, passwordAccountCollation) {
		return fmt.Errorf("column account must use collation %s, got %q", passwordAccountCollation, account.collation)
	}
	password, ok := findPasswordTableColumn(columns, "password")
	if !ok {
		return errors.New("column password is missing")
	}
	if password.dataType != "varchar" || password.width != passwordHashColumnWidth {
		return fmt.Errorf("column password must be VARCHAR(%d), got %s(%d)", passwordHashColumnWidth, password.dataType, password.width)
	}
	return nil
}

func findPasswordTableColumn(columns []passwordTableColumn, name string) (passwordTableColumn, bool) {
	for _, column := range columns {
		if strings.EqualFold(column.name, name) {
			return column, true
		}
	}
	return passwordTableColumn{}, false
}
