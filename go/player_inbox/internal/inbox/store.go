package inbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"shared/drainproof"
	"strings"
	"time"

	dspb "proto/data_service"
	inboxpb "proto/player_inbox"

	"shared/assetop"

	"github.com/go-sql-driver/mysql"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc/peer"
)

// Store 是收件箱的存储与协议核心(设计 §6.2、§7.8、§7.9、§9)。
//
// 并发与锁(设计 §10):
//   - 所有事务都是 READ COMMITTED,经 assetop.WithTxRetryConfig 执行,并**显式注入**可重试判定
//     (1213 / 1205 / 9007,IsRetryableTxError)—— IsRetryable 为 nil 时一次都不重试(assetop/seq.go)。
//     RC 的理由同 assetop/seq.go:TiDB 悲观事务没有间隙锁;锁内的普通读必须看得见前一个持锁者已提交的行。
//   - 锁顺序固定:原角色P→消费原admission→流行→条目主键→审计行插入。
//     条目行一律先普通读、再按主键条件写,**不做**二级索引上的加锁读(二级索引加锁读与主键 CAS 加锁顺序相反会成环)。
//   - 角色封存与所有域内变更共用同P；原账号/owner_revision持久在条目，旧空原件不修复。
//   - Redis 读只在事务外进行,不在持有行锁时等待 Redis。
//
// Store 构造后只读,可被多个 RPC 与后台循环并发使用。
type Store struct {
	// RelayAdmission 在启动前绑定，通知后的 SQL CAS 失败属于未决发布。
	RelayAdmission *drainproof.Session
	db             *sql.DB
	registry       *Registry
	allowed        map[inboxpb.PlayerInboxMessageType]struct{}
	players        PlayerDirectory
	epochs         EpochReader
	notifier       Notifier
	now            func() time.Time
	txCfg          assetop.TxRetryConfig
	roles          RoleAuthority
}

// PlayerDirectory 是入箱前的玩家存在预检；最终归属/冻结判定只由同事务SQL原P负责。
// (false, nil) = 映射里没有这名玩家;err 非 nil = 查询失败(故障,fail-closed 不入箱)。
type PlayerDirectory interface {
	PlayerExists(ctx context.Context, playerID uint64) (bool, error)
}

// EpochReader仅为STALE日志分类读取Redis镜像，不能推翻SQL原入场授权。键不存在返回0。
type EpochReader interface {
	OwnerEpoch(ctx context.Context, playerID uint64) (uint64, error)
}

// StoreDeps 是 Store 的显式依赖(AGENTS §11.2)。Notifier 可为 nil:不发入箱后的那次提醒,
// 全部交给补发循环(单测与 -migrate 之外的形态不应为 nil)。
type StoreDeps struct {
	DB       *sql.DB
	Registry *Registry
	// Allowed 是 PlayerInbox.EnqueueTypes 解析后的白名单;nil / 空 = 全部停用。
	Allowed  map[inboxpb.PlayerInboxMessageType]struct{}
	Players  PlayerDirectory
	Epochs   EpochReader
	Notifier Notifier
	Roles    RoleAuthority
	// Now 是时间源,生产为 time.Now;集成测试注入固定时钟验证「同一毫秒并发入箱 seq 连续」。
	Now func() time.Time
}

// NewStore 校验依赖并构造 Store。缺任何一项必需依赖都返回错误(启动致命)。
func NewStore(d StoreDeps) (*Store, error) {
	if d.DB == nil || d.Registry == nil || d.Players == nil || d.Epochs == nil || d.Roles == nil {
		return nil, errors.New("inbox: NewStore requires DB, Registry, Players, Epochs and SQL RoleAuthority")
	}
	if err := d.Registry.CheckAllowlist(d.Allowed); err != nil {
		return nil, err
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	allowed := make(map[inboxpb.PlayerInboxMessageType]struct{}, len(d.Allowed))
	for t := range d.Allowed {
		allowed[t] = struct{}{}
	}
	return &Store{
		db:       d.DB,
		registry: d.Registry,
		allowed:  allowed,
		players:  d.Players,
		epochs:   d.Epochs,
		notifier: d.Notifier,
		now:      now,
		txCfg:    TxRetryConfig(),
		roles:    d.Roles,
	}, nil
}

// Registry 返回 Store 使用的类型注册表。
func (s *Store) Registry() *Registry { return s.registry }

// TxRetryConfig 是本服务全部事务的参数:assetop 默认值(RC、3 次、10ms 起退避 + 抖动),再**注入**
// 可重试判定 IsRetryableTxError。单测钉住注入不为 nil 且覆盖 1213 / 1205 / 9007(设计 §10)。
func TxRetryConfig() assetop.TxRetryConfig {
	cfg := assetop.DefaultTxRetryConfig()
	cfg.IsRetryable = IsRetryableTxError
	return cfg
}

// IsRetryableTxError 判定「事务已整体回滚,重跑是安全的」:1213 死锁、1205 锁等待超时、9007 TiDB 写冲突。
// 9007 必须在内:迁到 TiDB 之后它会取代死锁成为最常见的一类,漏判就等于不重试(assetop/seq.go 的注释)。
func IsRetryableTxError(err error) bool {
	var myErr *mysql.MySQLError
	if !errors.As(err, &myErr) {
		return false
	}
	switch myErr.Number {
	case 1213, 1205, 9007:
		return true
	default:
		return false
	}
}

// isDuplicateKeyError 判定 1062(唯一键 / 主键冲突)。
func isDuplicateKeyError(err error) bool {
	var myErr *mysql.MySQLError
	return errors.As(err, &myErr) && myErr.Number == 1062
}

// nowMs 取一次时间转成 Unix 毫秒;早于 1970 时返回 0(seq 分配会据此拒绝)。
func (s *Store) nowMs() uint64 {
	ms := s.now().UnixMilli()
	if ms < 0 {
		return 0
	}
	return uint64(ms)
}

// ── SQL 片段 ─────────────────────────────────────────────────────────────
// 列名一律加反引号:operator / action / state 等与 SQL 关键字同名(player_inbox_table.proto 头注)。

const (
	streamTable = "player_inbox_stream"
	itemTable   = "player_inbox_item"
	auditTable  = "player_inbox_audit"

	streamColumns = "`shard`,`player_id`,`domain`,`next_seq`,`fence_epoch`,`open_count`,`notify_due_ms`,`created_ms`,`updated_ms`"
	itemColumns   = "`shard`,`player_id`,`domain`,`seq`,`msg_type`,`producer_kind`,`request_id`,`content_sha256`,`payload`," +
		"`state`,`directive`,`void_reason`,`delivery_count`,`last_delivered_epoch`,`last_delivered_ms`,`outcome_reason`," +
		"`finalized_by`,`decided_epoch`,`issued_at_ms`,`created_ms`,`updated_ms`,`operator_name`,`note`,`owner_account`,`owner_revision`"

	streamPK = "`shard` = ? AND `player_id` = ? AND `domain` = ?"
	itemPK   = "`shard` = ? AND `player_id` = ? AND `domain` = ? AND `seq` = ?"
)

// terminalStatesSQL 是终态集合的 SQL 字面量(APPLIED / REJECTED / VOIDED / SETTLED_UNKNOWN)。
// 由枚举值拼出而不是手写数字,改枚举时跟着变。
var terminalStatesSQL = fmt.Sprintf("(%d,%d,%d,%d)",
	int32(inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_APPLIED),
	int32(inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_REJECTED),
	int32(inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_VOIDED),
	int32(inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_SETTLED_UNKNOWN))

// sqlRunner 是 *sql.DB 与 *sql.Tx 的公共子集:同一条读在事务内外复用。
type sqlRunner interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanStream(sc rowScanner) (*inboxpb.PlayerInboxStream, error) {
	r := &inboxpb.PlayerInboxStream{}
	if err := sc.Scan(&r.Shard, &r.PlayerId, &r.Domain, &r.NextSeq, &r.FenceEpoch, &r.OpenCount,
		&r.NotifyDueMs, &r.CreatedMs, &r.UpdatedMs); err != nil {
		return nil, err
	}
	if err := checkRowShard(r.GetShard(), r.GetPlayerId()); err != nil {
		return nil, err
	}
	return r, nil
}

func scanItem(sc rowScanner) (*inboxpb.PlayerInboxItemRow, error) {
	r := &inboxpb.PlayerInboxItemRow{}
	if err := sc.Scan(&r.Shard, &r.PlayerId, &r.Domain, &r.Seq, &r.MsgType, &r.ProducerKind, &r.RequestId,
		&r.ContentSha256, &r.Payload, &r.State, &r.Directive, &r.VoidReason, &r.DeliveryCount,
		&r.LastDeliveredEpoch, &r.LastDeliveredMs, &r.OutcomeReason, &r.FinalizedBy, &r.DecidedEpoch,
		&r.IssuedAtMs, &r.CreatedMs, &r.UpdatedMs, &r.OperatorName, &r.Note, &r.OwnerAccount, &r.OwnerRevision); err != nil {
		return nil, err
	}
	if err := checkRowShard(r.GetShard(), r.GetPlayerId()); err != nil {
		return nil, err
	}
	return r, nil
}

// checkRowShard 是读出时的断言(设计 §4.2:TiDB 不执行 CHECK,shard 一致性靠唯一计算函数 + 读出断言 + 单测)。
//
// 它只拦得住**按分片扫描**读出来的行(DueNotifications 等)。按玩家的读写一律带 WHERE `shard` = RowShard(player_id),
// shard 写错的行根本查不出来,走不到这里 —— 那种行是静默不可见,不是 fail-closed(评审 R3);整库核对见 CheckRowShards。
func checkRowShard(shard uint32, playerID uint64) error {
	if want := RowShard(playerID); shard != want {
		return fmt.Errorf("inbox: corrupt row shard=%d for player_id=%d (want %d)", shard, playerID, want)
	}
	return nil
}

// rowShardTables 是 shard 列由 RowShard(player_id) 决定的全部表(收件箱三张表)。
var rowShardTables = []string{streamTable, itemTable, auditTable}

// CheckRowShards 逐表核对 (shard, player_id):只要有一行 shard ≠ RowShard(player_id) 就返回 error,
// 错误里给出表名、行数、第一行与处置(评审 R3)。
//
// 为什么要整库核对:shard 写错的行(典型是 L5 改公式之前按 player_id % 64 写出的开发库存量)对按玩家的读写静默不可见 ——
// 锁内判重读不到旧行,同一 request_id 重试会再入箱一次(IB-2 失效);旧的 PENDING 行再也拉不到;旧流行只要挂着
// notify_due_ms,DueNotifications 每轮都在 checkRowShard 上整片报错,该片的提醒补发全停。
// 所以应在接请求之前跑一次,发现不一致即拒绝启动(装配入口:player_inbox.go 的建表 / 查表之后与 -migrate)。
//
// 只读、不加锁;每张表一条 DISTINCT (shard, player_id) 扫描(两列是主键前缀,走主键有序读)。收件箱只承载 GM 域,
// 三张表是千到十万行的量级(设计 §4.2 量级假设),每次部署跑一次的代价可以接受。ctx 结束返回 ctx 的错误。
func CheckRowShards(ctx context.Context, db sqlRunner) error {
	for _, table := range rowShardTables {
		if err := checkTableRowShards(ctx, db, table); err != nil {
			return err
		}
	}
	return nil
}

// checkTableRowShards 核对一张表;返回的 error 已带表名。
func checkTableRowShards(ctx context.Context, db sqlRunner, table string) error {
	rows, err := db.QueryContext(ctx, "SELECT DISTINCT `shard`, `player_id` FROM "+table)
	if err != nil {
		return fmt.Errorf("inbox: scan %s for shard layout: %w", table, err)
	}
	defer rows.Close()
	bad := 0
	var firstShard uint32
	var firstPlayer uint64
	for rows.Next() {
		var shard uint32
		var playerID uint64
		if err := rows.Scan(&shard, &playerID); err != nil {
			return fmt.Errorf("inbox: scan %s for shard layout: %w", table, err)
		}
		if checkRowShard(shard, playerID) == nil {
			continue
		}
		if bad == 0 {
			firstShard, firstPlayer = shard, playerID
		}
		bad++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("inbox: scan %s for shard layout: %w", table, err)
	}
	if bad > 0 {
		return fmt.Errorf("inbox: %s has %d (shard, player_id) pair(s) whose shard is not RowShard(player_id) "+
			"(first: shard=%d player_id=%d, want %d); such rows are invisible to every per-player read and break idempotency. "+
			"Rows written by a build older than the 2026-10-16 shard formula change (player_id %% 64) cannot be carried over: "+
			"on a disposable dev/test database, empty player_inbox_stream / player_inbox_item / player_inbox_audit and start again; "+
			"anywhere else stop and escalate, do not delete rows (design §4.2, review L5)",
			table, bad, firstShard, firstPlayer, RowShard(firstPlayer))
	}
	return nil
}

// readStream 普通读流行(不加锁);不存在返回 (nil, nil)。
func readStream(ctx context.Context, q sqlRunner, shard uint32, playerID uint64, domain inboxpb.PlayerInboxDomain) (*inboxpb.PlayerInboxStream, error) {
	row := q.QueryRowContext(ctx, "SELECT "+streamColumns+" FROM "+streamTable+" WHERE "+streamPK,
		shard, playerID, uint32(domain))
	st, err := scanStream(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inbox: read stream player=%d domain=%d: %w", playerID, domain, err)
	}
	return st, nil
}

// lockStream 对流行加 FOR UPDATE(锁序第 1);不存在返回 (nil, nil)。
func lockStream(ctx context.Context, tx *sql.Tx, shard uint32, playerID uint64, domain inboxpb.PlayerInboxDomain) (*inboxpb.PlayerInboxStream, error) {
	row := tx.QueryRowContext(ctx, "SELECT "+streamColumns+" FROM "+streamTable+" WHERE "+streamPK+" FOR UPDATE",
		shard, playerID, uint32(domain))
	st, err := scanStream(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inbox: lock stream player=%d domain=%d: %w", playerID, domain, err)
	}
	return st, nil
}

// readItemByRequest 按唯一键 (shard, player_id, producer_kind, request_id) 普通读;不存在返回 (nil, nil)。
func readItemByRequest(ctx context.Context, q sqlRunner, shard uint32, playerID uint64,
	producer inboxpb.PlayerInboxProducerKind, requestID uint64) (*inboxpb.PlayerInboxItemRow, error) {
	row := q.QueryRowContext(ctx, "SELECT "+itemColumns+" FROM "+itemTable+
		" WHERE `shard` = ? AND `player_id` = ? AND `producer_kind` = ? AND `request_id` = ?",
		shard, playerID, uint32(producer), requestID)
	it, err := scanItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inbox: read item by request player=%d request_id=%d: %w", playerID, requestID, err)
	}
	return it, nil
}

// readItem 按主键普通读一行;不存在返回 (nil, nil)。
func readItem(ctx context.Context, q sqlRunner, shard uint32, playerID uint64, domain inboxpb.PlayerInboxDomain, seq uint64) (*inboxpb.PlayerInboxItemRow, error) {
	row := q.QueryRowContext(ctx, "SELECT "+itemColumns+" FROM "+itemTable+" WHERE "+itemPK,
		shard, playerID, uint32(domain), seq)
	it, err := scanItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inbox: read item player=%d domain=%d seq=%d: %w", playerID, domain, seq, err)
	}
	return it, nil
}

// readItemsBySeq 按主键批量普通读(seq IN (...)),返回 seq → 行。
func readItemsBySeq(ctx context.Context, q sqlRunner, shard uint32, playerID uint64, domain inboxpb.PlayerInboxDomain,
	seqs []uint64) (map[uint64]*inboxpb.PlayerInboxItemRow, error) {
	out := make(map[uint64]*inboxpb.PlayerInboxItemRow, len(seqs))
	if len(seqs) == 0 {
		return out, nil
	}
	args := make([]any, 0, 3+len(seqs))
	args = append(args, shard, playerID, uint32(domain))
	for _, seq := range seqs {
		args = append(args, seq)
	}
	query := "SELECT " + itemColumns + " FROM " + itemTable +
		" WHERE `shard` = ? AND `player_id` = ? AND `domain` = ? AND `seq` IN (" + placeholders(len(seqs)) + ")"
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("inbox: read items by seq player=%d domain=%d: %w", playerID, domain, err)
	}
	defer rows.Close()
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out[it.GetSeq()] = it
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// queryItems 跑一条返回完整条目行的查询。
func queryItems(ctx context.Context, q sqlRunner, query string, args ...any) ([]*inboxpb.PlayerInboxItemRow, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*inboxpb.PlayerInboxItemRow
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// requireOneRow 断言一条条件写恰好命中一行。所有条件写都在流行锁下进行,命中数不对只可能是缺陷或数据已矛盾:
// 返回错误让整个事务回滚(fail-closed),而不是带着半套状态提交。
func requireOneRow(res sql.Result, what string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("inbox: %s rows affected: %w", what, err)
	}
	if n != 1 {
		return fmt.Errorf("inbox: %s affected %d rows, want 1 (state changed under stream lock?)", what, n)
	}
	return nil
}

// InsertAudit 在调用方事务里写一行审计(INSERT IGNORE:每条目每种动作一行,重复调用不报错,设计 §4.1)。
// 调用方只给身份、动作、「谁、为什么」与 void_reason;「做了什么」(msg_type / request_id / issued_at_ms /
// content_sha256 / payload)由本函数在**同一事务里**按主键普通读条目行后原样复制(评审 L9,组装见 auditRowFor),
// shard 按 player_id 计算 —— 调用方传入的这些值一律被忽略。
//
// 用于条目行刚在本事务里写出的场景(GM 入箱的 inTx):RC 下普通读看得见本事务自己的写。多一次主键点读,
// 锁序不变(流行 → 条目行 → 审计行;普通读不加锁)。条目行读不到返回 error,整个事务回滚(fail-closed)。
// 手里已经有条目行的调用方(作废)用 insertAuditForItem,不再多读一次。
func InsertAudit(ctx context.Context, tx *sql.Tx, a *inboxpb.PlayerInboxAudit) error {
	if a == nil {
		return errors.New("inbox: nil audit row")
	}
	item, err := readItem(ctx, tx, RowShard(a.GetPlayerId()), a.GetPlayerId(), inboxpb.PlayerInboxDomain(a.GetDomain()), a.GetSeq())
	if err != nil {
		return err
	}
	if item == nil {
		return fmt.Errorf("inbox: audit for a missing item player=%d domain=%d seq=%d action=%d",
			a.GetPlayerId(), a.GetDomain(), a.GetSeq(), a.GetAction())
	}
	return insertAuditForItem(ctx, tx, item, a)
}

// auditColumns 是审计表的全部列,顺序与 insertAuditForItem 的实参一一对应。
const auditColumns = "`shard`,`player_id`,`domain`,`seq`,`action`,`operator`,`peer_addr`,`reason`,`created_ms`," +
	"`msg_type`,`request_id`,`issued_at_ms`,`content_sha256`,`payload`,`void_reason`"

// insertAuditForItem 按 auditRowFor 组出整行后 INSERT IGNORE。item 必须是调用方在同一事务里读到的条目行
// (内容列入箱后不再改,早于本事务内其它条件写读到的快照也可以用)。
func insertAuditForItem(ctx context.Context, tx *sql.Tx, item *inboxpb.PlayerInboxItemRow, a *inboxpb.PlayerInboxAudit) error {
	row, err := auditRowFor(item, a)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT IGNORE INTO "+auditTable+" ("+auditColumns+") VALUES ("+placeholders(15)+")",
		row.GetShard(), row.GetPlayerId(), row.GetDomain(), row.GetSeq(), row.GetAction(),
		row.GetOperator(), row.GetPeerAddr(), row.GetReason(), row.GetCreatedMs(),
		row.GetMsgType(), row.GetRequestId(), row.GetIssuedAtMs(), row.ContentSha256, row.Payload, row.GetVoidReason())
	if err != nil {
		return fmt.Errorf("inbox: insert audit player=%d seq=%d action=%d: %w", row.GetPlayerId(), row.GetSeq(), row.GetAction(), err)
	}
	return nil
}

// PeerAddr 是 gRPC 对端地址,审计里唯一不可伪造的部分(设计 §1.6);取不到时为 "unknown"。
// 运维面写审计、consumer 面的异常日志(评审 L10)共用这一份;server 包的拦截器与适配层也调它。
func PeerAddr(ctx context.Context) string {
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		return p.Addr.String()
	}
	return "unknown"
}

// ensureStream保留独立接缝，仍先持P。入箱主事务直接调用ensureStreamTx，
// 以中性INSERT IGNORE先于stream锁，不采用缺行FOR UPDATE后再插入的反向锁序。
func (s *Store) ensureStream(ctx context.Context, shard uint32, playerID uint64, domain inboxpb.PlayerInboxDomain, nowMs uint64) error {
	return s.withPlayerTx(ctx, playerID, func(tx *sql.Tx, original *dspb.AccountRoleOwnership) error {
		if err := requirePendingOriginalsTx(ctx, tx, original); err != nil {
			return err
		}
		return ensureStreamTx(ctx, tx, shard, playerID, domain, nowMs)
	})
}

func ensureStreamTx(ctx context.Context, tx *sql.Tx, shard uint32, playerID uint64, domain inboxpb.PlayerInboxDomain, nowMs uint64) error {
	_, err := tx.ExecContext(ctx, "INSERT IGNORE INTO "+streamTable+" ("+streamColumns+") VALUES (?,?,?,1,0,0,0,?,?)", shard, playerID, uint32(domain), nowMs, nowMs)
	return err
}

// ClearNotifyDue 在提醒成功后把 notify_due_ms 置 0,**以 next_seq 做 CAS 令牌**(设计 §6.3):
// 这期间只要又有新的入箱,next_seq 就变了,CAS 失败,notify_due_ms 留给补发循环 —— 最多多发一次提醒,不会丢。
// 不用毫秒值当令牌:同一毫秒两次入箱会让旧令牌碰巧相等(ABA)。返回是否真的清掉了。
func (s *Store) ClearNotifyDue(ctx context.Context, playerID uint64, domain inboxpb.PlayerInboxDomain, expectNextSeq uint64) (bool, error) {
	changed := false
	err := s.withPlayerTx(ctx, playerID, func(tx *sql.Tx, original *dspb.AccountRoleOwnership) error {
		changed = false
		if err := requirePendingOriginalsTx(ctx, tx, original); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, "UPDATE "+streamTable+" SET `notify_due_ms` = 0 WHERE "+streamPK+
			" AND `next_seq` = ? AND `notify_due_ms` <> 0", RowShard(playerID), playerID, uint32(domain), expectNextSeq)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		changed = n == 1
		return err
	})
	return changed && err == nil, err
}

// notifyAndClear 发一次在线提醒;提醒已落定(送达 / 离线 / 等待落点)时按 next_seq 做 CAS 清 notify_due_ms。
// 失败只记日志,不影响调用方(入箱的返回值、补发循环的下一行)。
func (s *Store) notifyAndClear(ctx context.Context, playerID uint64, domain inboxpb.PlayerInboxDomain, expectNextSeq uint64) NotifyResult {
	if s.notifier == nil {
		return NotifyError
	}
	result := s.notifier.Notify(ctx, playerID, domain)
	if !result.Settled() {
		return result
	}
	if _, err := s.ClearNotifyDue(ctx, playerID, domain, expectNextSeq); err != nil {
		if s.RelayAdmission != nil {
			s.RelayAdmission.Hold()
		}
		logx.WithContext(ctx).Errorf("[player_inbox] Notify settled but clearing notify_due failed player=%d domain=%d: %v "+
			"(resend loop will notify again; outcome remains unknown)", playerID, domain, err)
		return NotifyError
	}
	return result
}
