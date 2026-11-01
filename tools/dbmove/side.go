package main

// 两侧逻辑库上的 SQL。闸门走 authoritysql(维护身份,和 route_tool、maintenance_phase 同一条路);
// 建库、删库走管理员连接,校验和走只读的原始连接(checksum.go):运行池的语句白名单不放 SET、ADMIN、CHECKSUM,这几条不该进运行池。
//
// 错误信息不回显 DSN:ParseDSN 的错误不往外传,驱动的错误只有地址和账号名。

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	databasepb "proto/common/database"
	"shared/authoritysql"
)

var identifierRe = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)

func quoteIdentifier(name string) string { return "`" + strings.ReplaceAll(name, "`", "``") + "`" }

// sqlGate 实现 gateSide。
type sqlGate struct {
	schema  string
	db      *sql.DB
	catalog *authoritysql.Catalog
}

// gateConnMax:工具串行跑,一个事务一条连接,留一条给事务外的读。
const gateConnMax = 2

// openGate 用维护身份连逻辑库。generation 读库里的:Catalog 只拿它比对,目标上的值是复制过去的。
func openGate(ctx context.Context, dsn, schema string, backend authoritysql.Backend, principal string, physical []byte, permission authoritysql.Permission) (*sqlGate, error) {
	schemaDSN, err := authoritysql.DSNForSchema(dsn, schema)
	if err != nil {
		return nil, err
	}
	db, err := authoritysql.Open(ctx, schemaDSN, schema, backend, gateConnMax, permission)
	if err != nil {
		return nil, err
	}
	var generation string
	if err := db.QueryRowContext(ctx, "SELECT generation FROM route_meta WHERE id=1").Scan(&generation); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("read route_meta of %s: %w", schema, err)
	}
	catalog, err := authoritysql.NewCatalog(db, authoritysql.CatalogConfig{
		AuthoritySchema: schema, Generation: generation, ProtectedSchemas: []string{schema}, PhysicalAuthoritySHA256: physical, Principal: principal,
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return &sqlGate{schema: schema, db: db, catalog: catalog}, nil
}

func (g *sqlGate) Close() error { return g.db.Close() }

func (g *sqlGate) CurrentMeta(ctx context.Context) (*databasepb.RouteMeta, error) {
	return g.catalog.CurrentMeta(ctx)
}

func (g *sqlGate) FreezeWriters(ctx context.Context, runID []byte) (*databasepb.RouteMeta, error) {
	return g.catalog.FreezeWriters(ctx, runID)
}

func (g *sqlGate) ReleaseWriters(ctx context.Context, runID []byte) (*databasepb.RouteMeta, error) {
	return g.catalog.ReleaseWriters(ctx, runID)
}

func sentinelInsertSQL() string {
	return "INSERT INTO " + authoritysql.MoveSentinelTable + "(nonce,run_id,writer_epoch,written_ms) VALUES(?,?,?,?)"
}

func (g *sqlGate) WriteSentinel(ctx context.Context, nonce, runID []byte, epoch uint64) error {
	_, err := g.db.ExecContext(ctx, sentinelInsertSQL(), nonce, runID, epoch, uint64(time.Now().UnixMilli()))
	return err
}

func (g *sqlGate) HasSentinel(ctx context.Context, nonce []byte) (bool, error) {
	var count int
	err := g.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+authoritysql.MoveSentinelTable+" WHERE nonce=?", nonce).Scan(&count)
	return count == 1, err
}

// ProbeControl 见 gateSide。全部回滚:只验权限,不留痕。
func (g *sqlGate) ProbeControl(ctx context.Context) error {
	tx, err := g.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	var writable uint32
	if err := tx.QueryRowContext(ctx, "SELECT writable FROM route_meta WHERE id=1 FOR UPDATE").Scan(&writable); err != nil {
		return fmt.Errorf("lock route_meta: %w", err)
	}
	var key []byte
	if err := tx.QueryRowContext(ctx, "SELECT writer_key FROM writer_slot LIMIT 1 FOR UPDATE").Scan(&key); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("lock writer_slot: %w", err)
	}
	if _, err := tx.ExecContext(ctx, sentinelInsertSQL(), nonce, nonce, 0, 0); err != nil {
		return fmt.Errorf("write %s: %w", authoritysql.MoveSentinelTable, err)
	}
	return nil
}

func (g *sqlGate) ActivePrincipals(ctx context.Context) ([]string, error) {
	rows, err := g.db.QueryContext(ctx, "SELECT sql_principal FROM writer_registry WHERE state=1")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var principal []byte
		if err := rows.Scan(&principal); err != nil {
			return nil, err
		}
		out = append(out, string(principal))
	}
	return out, rows.Err()
}

func (g *sqlGate) SchemaCharset(ctx context.Context) (string, string, error) {
	var charset, collation string
	err := g.db.QueryRowContext(ctx, "SELECT default_character_set_name,default_collation_name FROM information_schema.schemata WHERE schema_name=?", g.schema).Scan(&charset, &collation)
	return charset, collation, err
}

// openRaw 建不经 authoritysql 的连接:不带默认库,没有读写超时(校验和和 DROP 合法地跑很久,时限由 ctx 管)。
func openRaw(rawDSN string, maxOpen int) (*sql.DB, error) {
	cfg, err := mysql.ParseDSN(rawDSN)
	if err != nil {
		return nil, errors.New("cannot parse a database DSN (its value is not echoed)")
	}
	cfg.DBName = ""
	cfg.ReadTimeout, cfg.WriteTimeout = 0, 0
	cfg.MultiStatements = false
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, errors.New("cannot use a database DSN (its value is not echoed)")
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(maxOpen)
	return db, nil
}

// sqlAdmin 实现 adminSide。
type sqlAdmin struct {
	db *sql.DB
}

func openAdmin(rawDSN string) (*sqlAdmin, error) {
	db, err := openRaw(rawDSN, 2)
	if err != nil {
		return nil, err
	}
	return &sqlAdmin{db: db}, nil
}

func (a *sqlAdmin) Close() error { return a.db.Close() }

func (a *sqlAdmin) ServerBackend(ctx context.Context) (string, error) {
	return readServerBackend(ctx, a.db)
}

// readServerBackend 判据和 authoritysql 的 mysql 档相同:VERSION() 和 @@version_comment 两个都看。
func readServerBackend(ctx context.Context, db *sql.DB) (string, error) {
	var version string
	var comment sql.NullString
	if err := db.QueryRowContext(ctx, "SELECT VERSION(),@@version_comment").Scan(&version, &comment); err != nil {
		return "", err
	}
	if strings.Contains(strings.ToLower(version+" "+comment.String), "tidb") {
		return string(authoritysql.BackendTiDB), nil
	}
	return string(authoritysql.BackendMySQL), nil
}

func (a *sqlAdmin) SchemaExists(ctx context.Context, schema string) (bool, error) {
	var count int
	err := a.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name=?", schema).Scan(&count)
	return count != 0, err
}

func (a *sqlAdmin) CreateSchema(ctx context.Context, schema, charset, collation string) error {
	if !identifierRe.MatchString(schema) || !identifierRe.MatchString(charset) || !identifierRe.MatchString(collation) {
		return errors.New("bad database name, charset or collation")
	}
	_, err := a.db.ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS "+quoteIdentifier(schema)+" CHARACTER SET "+charset+" COLLATE "+collation)
	return err
}

func (a *sqlAdmin) DropSchema(ctx context.Context, schema string) error {
	if !identifierRe.MatchString(schema) {
		return errors.New("bad database name")
	}
	_, err := a.db.ExecContext(ctx, "DROP DATABASE IF EXISTS "+quoteIdentifier(schema))
	return err
}

// AccountExists 按 CURRENT_USER() 的形状 user@host 比。
func (a *sqlAdmin) AccountExists(ctx context.Context, principal string) (bool, error) {
	var count int
	err := a.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mysql.user WHERE CONCAT(User,'@',Host)=?", principal).Scan(&count)
	return count != 0, err
}

// GateState 见 adminSide。库或表不在说明这个库还没有闸门,不是读失败。
func (a *sqlAdmin) GateState(ctx context.Context, schema string) (gateState, error) {
	if !identifierRe.MatchString(schema) {
		return gateState{}, errors.New("bad database name")
	}
	var writable uint32
	var epoch uint64
	err := a.db.QueryRowContext(ctx, "SELECT writable,writer_epoch FROM "+quoteIdentifier(schema)+".route_meta WHERE id=1").Scan(&writable, &epoch)
	if errors.Is(err, sql.ErrNoRows) || isMissingSchemaObject(err) {
		return gateState{}, nil
	}
	if err != nil {
		return gateState{}, err
	}
	return gateState{Found: true, Writable: writable == 1, WriterEpoch: epoch}, nil
}

func (a *sqlAdmin) GateFrozenBy(ctx context.Context, schema string, runID []byte) (bool, error) {
	if !identifierRe.MatchString(schema) {
		return false, errors.New("bad database name")
	}
	var writable uint32
	var freezeRun []byte
	err := a.db.QueryRowContext(ctx, "SELECT writable,freeze_run_id FROM "+quoteIdentifier(schema)+".route_meta WHERE id=1").Scan(&writable, &freezeRun)
	if err != nil {
		return false, err
	}
	return writable == 0 && string(freezeRun) == string(runID), nil
}

// 账号的形状和 deploy/sql 的渲染器一样。ALTER USER、SHOW GRANTS 不能参数化,这里是唯一拼进语句的地方。
var (
	accountUserRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	accountHostRe = regexp.MustCompile(`^[A-Za-z0-9_.%:-]{1,255}$`)
)

// quoteAccount 把 CURRENT_USER() 形状的 user@host 变成语句里的 'user'@'host'。形状不对就拒绝,错误里没有别的东西可泄露:主体不是秘密。
func quoteAccount(principal string) (string, error) {
	at := strings.LastIndexByte(principal, '@')
	if at < 1 || !accountUserRe.MatchString(principal[:at]) || !accountHostRe.MatchString(principal[at+1:]) {
		return "", fmt.Errorf("bad account name %q: want user@host", principal)
	}
	return "'" + principal[:at] + "'@'" + principal[at+1:] + "'", nil
}

// SetAccountLocked 见 adminSide。
func (a *sqlAdmin) SetAccountLocked(ctx context.Context, principal string, locked bool) error {
	account, err := quoteAccount(principal)
	if err != nil {
		return err
	}
	action := "UNLOCK"
	if locked {
		action = "LOCK"
	}
	_, err = a.db.ExecContext(ctx, "ALTER USER "+account+" ACCOUNT "+action)
	return err
}

// AccountLocked 见 adminSide。
func (a *sqlAdmin) AccountLocked(ctx context.Context, principal string) (bool, error) {
	var locked string
	err := a.db.QueryRowContext(ctx, "SELECT account_locked FROM mysql.user WHERE CONCAT(User,'@',Host)=?", principal).Scan(&locked)
	if err != nil {
		return false, err
	}
	return strings.EqualFold(locked, "Y"), nil
}

// AccountGrants 见 adminSide。
func (a *sqlAdmin) AccountGrants(ctx context.Context, principal string) ([]string, error) {
	account, err := quoteAccount(principal)
	if err != nil {
		return nil, err
	}
	return a.grantLines(ctx, "SHOW GRANTS FOR "+account)
}

func (a *sqlAdmin) grantLines(ctx context.Context, statement string) ([]string, error) {
	rows, err := a.db.QueryContext(ctx, statement)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return nil, err
		}
		out = append(out, line)
	}
	return out, rows.Err()
}

// CheckSeesAllConnections 见 adminSide。MySQL 读 performance_schema.threads,有 SELECT 就够。
// TiDB 的 cluster_processlist 没有 PROCESS 时只回自己的连接,不报错,查连接这道保护就静默失效了。
func (a *sqlAdmin) CheckSeesAllConnections(ctx context.Context) error {
	backend, err := a.ServerBackend(ctx)
	if err != nil {
		return err
	}
	if backend != string(authoritysql.BackendTiDB) {
		return nil
	}
	grants, err := a.grantLines(ctx, "SHOW GRANTS")
	if err != nil {
		return err
	}
	if !hasGlobalPrivilege(grants, "PROCESS") {
		return errors.New("the admin account has no global PROCESS privilege, information_schema.cluster_processlist would only show its own connections")
	}
	return nil
}

// hasGlobalPrivilege 看 SHOW GRANTS 的原文里有没有这条全局权限。
func hasGlobalPrivilege(grants []string, privilege string) bool {
	for _, line := range grants {
		grant := parseGrant(line)
		if grant.object == "*.*" && (grant.privs[privilege] || grant.privs["ALL PRIVILEGES"]) {
			return true
		}
	}
	return false
}

// Connections 见 adminSide。TiDB 的 information_schema.processlist 只有当前这台 tidb-server,要看全部实例得读 cluster_processlist。
// MySQL 用 performance_schema.threads:只要 SELECT,不要 PROCESS。
func (a *sqlAdmin) Connections(ctx context.Context) ([]accountConnection, error) {
	backend, err := a.ServerBackend(ctx)
	if err != nil {
		return nil, err
	}
	query := "SELECT PROCESSLIST_USER,COALESCE(PROCESSLIST_DB,''),COUNT(*) FROM performance_schema.threads WHERE TYPE='FOREGROUND' AND PROCESSLIST_USER IS NOT NULL GROUP BY PROCESSLIST_USER,PROCESSLIST_DB"
	if backend == string(authoritysql.BackendTiDB) {
		query = "SELECT COALESCE(USER,''),COALESCE(DB,''),COUNT(*) FROM information_schema.cluster_processlist GROUP BY USER,DB"
	}
	rows, err := a.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []accountConnection
	for rows.Next() {
		var conn accountConnection
		if err := rows.Scan(&conn.User, &conn.Database, &conn.Count); err != nil {
			return nil, err
		}
		out = append(out, conn)
	}
	return out, rows.Err()
}

const (
	mysqlErrUnknownDatabase uint16 = 1049
	mysqlErrNoSuchTable     uint16 = 1146
)

// HasFlipMarker 见 adminSide。库或表不在说明目标从来没有打开过这个库,不是读失败。
func (a *sqlAdmin) HasFlipMarker(ctx context.Context, schema string, nonce []byte) (bool, error) {
	if !identifierRe.MatchString(schema) {
		return false, errors.New("bad database name")
	}
	var count int
	err := a.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+quoteIdentifier(schema)+"."+authoritysql.MoveSentinelTable+" WHERE nonce=?", nonce).Scan(&count)
	if isMissingSchemaObject(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return count != 0, nil
}

func isMissingSchemaObject(err error) bool {
	var server *mysql.MySQLError
	return errors.As(err, &server) && (server.Number == mysqlErrUnknownDatabase || server.Number == mysqlErrNoSuchTable)
}

// grantLine 是 SHOW GRANTS 的一行拆出来的样子。MySQL 和 TiDB 的大小写、引号、空格都不一样,所以比之前要先拆。
type grantLine struct {
	object string          // 规范化之后的对象:*.*、db.*、db.table。角色之类没有 ON 的行是整行原文
	schema string          // 对象所在的库,*.* 时为 *
	privs  map[string]bool // 规范化之后的权限
}

var grantLineRe = regexp.MustCompile(`(?is)^GRANT\s+(.+?)\s+ON\s+(?:TABLE\s+|FUNCTION\s+|PROCEDURE\s+)?(\S+)\s+TO\s+`)

// parseGrant 拆一行 SHOW GRANTS。WITH GRANT OPTION 不比:模板不给这个选项,它也不影响业务能不能写。
func parseGrant(line string) grantLine {
	match := grantLineRe.FindStringSubmatch(strings.TrimSpace(line))
	if match == nil {
		text := strings.Join(strings.Fields(line), " ")
		return grantLine{object: text, privs: map[string]bool{text: true}}
	}
	object := strings.ReplaceAll(match[2], "`", "")
	schema, _, _ := strings.Cut(object, ".")
	return grantLine{object: object, schema: schema, privs: parsePrivileges(match[1])}
}

// parsePrivileges 把 "SELECT (`b`, `a`), UPDATE" 拆成 SELECT(A,B) 和 UPDATE:按括号外的逗号切,列名排序,两边的打印顺序不用相同。
func parsePrivileges(list string) map[string]bool {
	out := map[string]bool{}
	var current strings.Builder
	depth := 0
	flush := func() {
		if priv := normalizePrivilege(current.String()); priv != "" {
			out[priv] = true
		}
		current.Reset()
	}
	for _, ch := range list {
		switch ch {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				flush()
				continue
			}
		}
		current.WriteRune(ch)
	}
	flush()
	return out
}

func normalizePrivilege(raw string) string {
	text := strings.ToUpper(strings.Join(strings.Fields(strings.ReplaceAll(raw, "`", "")), " "))
	open := strings.IndexByte(text, '(')
	if open < 0 || !strings.HasSuffix(text, ")") {
		return text
	}
	columns := strings.Split(text[open+1:len(text)-1], ",")
	for i := range columns {
		columns[i] = strings.TrimSpace(columns[i])
	}
	slices.Sort(columns)
	return strings.TrimSpace(text[:open]) + "(" + strings.Join(columns, ",") + ")"
}

// 不算业务库的系统库:账号在这些库上有授权不代表它在写别的逻辑库。
func isSystemSchema(schema string) bool {
	switch strings.ToLower(schema) {
	case "mysql", "sys", "information_schema", "performance_schema", "metrics_schema", "tidb_cdc":
		return true
	}
	return false
}

// otherSchemasOf 列账号在 db 之外的业务授权。全局授权算 *:ACCOUNT LOCK 会连带影响它能碰的所有库。
func otherSchemasOf(grants []string, db string) []string {
	var out []string
	for _, line := range grants {
		grant := parseGrant(line)
		if grant.schema == "" || grant.schema == db || isSystemSchema(grant.schema) || grant.privs["USAGE"] && len(grant.privs) == 1 {
			continue
		}
		if !slices.Contains(out, grant.schema) {
			out = append(out, grant.schema)
		}
	}
	slices.Sort(out)
	return out
}

// missingGrants 列源库有、目标没有的授权,只看这个库和全局。别的库的授权两边本来就不同。
func missingGrants(source, target []string, db string) []string {
	have := map[string]map[string]bool{}
	for _, line := range target {
		grant := parseGrant(line)
		if have[grant.object] == nil {
			have[grant.object] = map[string]bool{}
		}
		for priv := range grant.privs {
			have[grant.object][priv] = true
		}
	}
	covered := func(grant grantLine, priv string) bool {
		// 目标上更宽的授权也算:库级、全局。
		for _, object := range []string{grant.object, grant.schema + ".*", "*.*"} {
			if have[object][priv] || have[object]["ALL PRIVILEGES"] {
				return true
			}
		}
		return false
	}
	var missing []string
	for _, line := range source {
		grant := parseGrant(line)
		if grant.privs["USAGE"] && len(grant.privs) == 1 {
			continue
		}
		if grant.schema != "" && grant.schema != db && grant.schema != "*" {
			continue
		}
		for priv := range grant.privs {
			if !covered(grant, priv) {
				missing = append(missing, priv+" on "+grant.object)
			}
		}
	}
	slices.Sort(missing)
	return missing
}
