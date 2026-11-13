package auditoutbox

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zeromicro/go-zero/core/logx"

	storepb "proto/player_store"
)

// 发件箱的读写语句(表声明在 proto/common/database/mysql_database_table.proto 的 player_audit_outbox)。
// 主键是 (shard, kind, created_ms, player_id, record_id),三条语句都只走主键:探测和读是主键前缀上的区间扫描,
// 删除是整主键的点删。
const (
	sqlOldest = "SELECT created_ms FROM player_audit_outbox WHERE shard=? AND kind=? AND created_ms>=?" +
		" ORDER BY created_ms,player_id,record_id LIMIT 1"
	sqlRead = "SELECT created_ms,player_id,record_id,payload FROM player_audit_outbox WHERE shard=? AND kind=? AND created_ms>=?" +
		" ORDER BY created_ms,player_id,record_id LIMIT ?"
	sqlDeleteHead = "DELETE FROM player_audit_outbox WHERE shard=? AND kind=? AND (created_ms,player_id,record_id) IN ("
	sqlDeleteRow  = "(?,?,?)"

	// 会话级命名锁(与 data_service 审计清理、schema 迁移同一套机制)。1 = 拿到,0 = 别的会话持有,NULL = 服务端出错。
	// 超时传 0:MySQL 上是不等待;TiDB 的下限是 1 秒(0 被抬到 1),即撞锁时最多等 1 秒。转发先探测、只在分工切换时才撞锁,
	// 所以这 1 秒只拖慢那一轮,不影响正确性。锁跟着会话走,在整个 TiDB 集群上生效:连接断开、进程崩溃,服务端都会放掉,不会卡死。
	sqlGetLock     = "SELECT GET_LOCK(?, 0)"
	sqlReleaseLock = "SELECT RELEASE_LOCK(?)"
	// lockNamePrefix 是转发锁名的前缀,完整名字见 unitLockName。
	lockNamePrefix = "db_audit_outbox."
	// maxLockNameBytes 是 MySQL / TiDB 命名锁名字的上限。
	maxLockNameBytes = 64
	// releaseTimeout 是放锁那条语句的上限;放不掉就丢掉连接,服务端随会话结束放锁。
	releaseTimeout = 5 * time.Second
)

// SQLStore 是 Store 的真库实现。db 是这个存档库的写者闸门连接池(proto_sql.GameDB.DB):
// 读走闸门的读路径(冻结时回 authoritysql.ErrWriterFrozen),删除是带闸门的自动提交写,
// 所以只有持有这个库写资格的 go/db 能删,搬库冻结窗口里删不动(行留着,解冻后接着投)。
type SQLStore struct {
	db     *sql.DB
	dbName string
}

// NewSQLStore 包一个存档库连接池。dbName 是库名(player_store_<id>_db),进锁名:命名锁在整台服务器上共享,
// 同一集群上的几个存档库要各自一套锁。
func NewSQLStore(db *sql.DB, dbName string) *SQLStore {
	return &SQLStore{db: db, dbName: dbName}
}

// Oldest 实现 Store。
func (s *SQLStore) Oldest(ctx context.Context, shard uint32, kind storepb.PlayerAuditKind, from uint64) (uint64, bool, error) {
	var createdMs uint64
	err := s.db.QueryRowContext(ctx, sqlOldest, shard, int32(kind), from).Scan(&createdMs)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("probe %s shard %d kind %d: %w", s.dbName, shard, kind, err)
	}
	return createdMs, true, nil
}

// LockUnit 实现 Store:钉住一条连接,在它上面取这一片的锁(MySQL 不等待,TiDB 最多等 1 秒)。读和删都在这条连接上做。
func (s *SQLStore) LockUnit(ctx context.Context, shard uint32) (UnitLock, bool, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("pin a connection of %s for the relay lock: %w", s.dbName, err)
	}
	name := unitLockName(s.dbName, shard)
	var got sql.NullInt64
	if err := conn.QueryRowContext(ctx, sqlGetLock, name).Scan(&got); err != nil {
		// 结局不明(比如语句发出去之后超时):这个会话说不定已经拿到了锁,不能把它还回池里让锁挂在池里的连接上。
		discard(conn)
		return nil, false, fmt.Errorf("acquire relay lock %q: %w", name, err)
	}
	if !got.Valid {
		discard(conn)
		return nil, false, fmt.Errorf("acquire relay lock %q: GET_LOCK returned NULL (server error or session killed)", name)
	}
	if got.Int64 != 1 {
		_ = conn.Close()
		return nil, false, nil
	}
	return &sqlUnitLock{conn: conn, name: name, shard: shard, dbName: s.dbName}, true, nil
}

// unitLockName 是 (库, 片) 的转发锁名。库名放最后,超长截断(地域存档库名远短于上限)。
func unitLockName(dbName string, shard uint32) string {
	name := fmt.Sprintf("%s%02d.%s", lockNamePrefix, shard, dbName)
	if len(name) > maxLockNameBytes {
		name = name[:maxLockNameBytes]
	}
	return name
}

// sqlUnitLock 是 UnitLock 的真库实现:持锁的那条连接。
type sqlUnitLock struct {
	conn   *sql.Conn
	name   string
	shard  uint32
	dbName string
}

// Read 实现 UnitLock。
func (l *sqlUnitLock) Read(ctx context.Context, kind storepb.PlayerAuditKind, from uint64, limit int) ([]Row, error) {
	rows, err := l.conn.QueryContext(ctx, sqlRead, l.shard, int32(kind), from, limit)
	if err != nil {
		return nil, fmt.Errorf("read %s shard %d kind %d: %w", l.dbName, l.shard, kind, err)
	}
	defer rows.Close()
	out := make([]Row, 0, limit)
	for rows.Next() {
		var row Row
		if err := rows.Scan(&row.CreatedMs, &row.PlayerID, &row.RecordID, &row.Payload); err != nil {
			return nil, fmt.Errorf("scan %s shard %d kind %d: %w", l.dbName, l.shard, kind, err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read %s shard %d kind %d: %w", l.dbName, l.shard, kind, err)
	}
	return out, nil
}

// Delete 实现 UnitLock:一条语句按整主键删这些行。
func (l *sqlUnitLock) Delete(ctx context.Context, kind storepb.PlayerAuditKind, rows []Row) error {
	if len(rows) == 0 {
		return nil
	}
	query, args := buildDelete(l.shard, kind, rows)
	if _, err := l.conn.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("delete %d row(s) from %s shard %d kind %d: %w", len(rows), l.dbName, l.shard, kind, err)
	}
	return nil
}

// buildDelete 拼按整主键删除的语句。一批至多 txlogBatch 行,占位符 2 + 3×500 个,远低于预编译语句的上限。
func buildDelete(shard uint32, kind storepb.PlayerAuditKind, rows []Row) (string, []any) {
	var query strings.Builder
	query.WriteString(sqlDeleteHead)
	args := make([]any, 0, 2+3*len(rows))
	args = append(args, shard, int32(kind))
	for i, row := range rows {
		if i > 0 {
			query.WriteByte(',')
		}
		query.WriteString(sqlDeleteRow)
		args = append(args, row.CreatedMs, row.PlayerID, row.RecordID)
	}
	query.WriteByte(')')
	return query.String(), args
}

// Release 实现 UnitLock。RELEASE_LOCK 回 1 才把连接还回池里;语句失败、回 0(锁已不属于这个会话)或 NULL,
// 都丢掉这条物理连接:还回去的话,一条可能仍持锁的会话会挂在池里被存盘反复借用,锁就永远放不掉。
func (l *sqlUnitLock) Release() {
	ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer cancel()
	var released sql.NullInt64
	err := l.conn.QueryRowContext(ctx, sqlReleaseLock, l.name).Scan(&released)
	if err == nil && released.Valid && released.Int64 == 1 {
		_ = l.conn.Close()
		return
	}
	if err != nil {
		logx.Errorf("[audit_outbox] Release relay lock %s failed; dropping the connection so the server frees the lock: %v", l.name, err)
	} else {
		logx.Errorf("[audit_outbox] RELEASE_LOCK(%s) returned %d (valid=%t); dropping the connection so the server frees the lock",
			l.name, released.Int64, released.Valid)
	}
	discard(l.conn)
}

// discard 关掉这条物理连接,不还回池里:会话一断,服务端就放掉它持有的命名锁。
// database/sql 的约定:Raw 的回调回 driver.ErrBadConn,这条连接就被关掉而不是放回池里。
func discard(conn *sql.Conn) {
	_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	_ = conn.Close()
}
