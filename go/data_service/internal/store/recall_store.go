package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	assetpb "proto/common/asset"
	dbpb "proto/common/database"
	rollbackpb "proto/common/rollback"
	rfpb "proto/role_fence"
	"shared/assetop"
	"shared/authoritysql"
	"shared/idsegment"
	"shared/rolefence"
	"shared/workshard"

	"github.com/go-sql-driver/mysql"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

// GM 批量回收的 outbox(docs/design/exploit_loss_prevention_zh.md「BatchRecallItems 执行方案」)。
//
// 四张表都在流水库。状态机:PLANNED(计划已冻结)→ PENDING(已分配 seq)→ 终局,PLANNED 也能直接到 CANCELLED、ROLE_GONE。
// 确定没回收到东西的终局(REJECTED、ABORTED、CANCELLED、ROLE_GONE)把流水从 gm_recall_source 放回去,下一批还能回收。
//
// 锁序:
//   - 派发:role_fence → gm_recall_seq → gm_recall_op。
//   - 终结、取消:gm_recall_batch → gm_recall_op。
//   - 护栏 Prepare:role_fence → gm_recall_op。
//
// 护栏 Prepare 读 gm_recall_op 不加锁,见 PendingForRole。派发和终结都不反着拿,成不了环。
// 派发不碰批次行:一批上万行时批次行不是派发的瓶颈,计数只在终局时动。
//
// 人工终结只有一条写路径 ResolveManuallyIfQuiet:和重投循环的 Finalize 走同一个 settle(批次计数、来源放回、结束审计),
// 另带读证据时的 lease_until_ms、last_send_ms,settle 持行锁比对,读完之后有人领过、发过就不终结。证据由调用方
// (logic.ResolveRecallOp)读已提交账本判定,这里不判。
//
// needs_review 指令交人工处置的登记(RegisterReviewHandled)也写 gm_recall_source:op_id = 0(没有回收指令),request_id 是
// 同一事务写的审计行号,操作人和理由在那一行审计里。settle 按 op_id 放回、取消按批次的 op_id 放回,都碰不到 op_id = 0 的行,
// 所以人工登记的流水永远算已消费,以后的批次和预览都把那条指令剔除。登记只写这一张表和审计表,不碰批次行与 op 行。

const (
	recallStatusPending   = uint32(dbpb.GmRecallOpStatus_GM_RECALL_OP_PENDING)
	recallStatusApplied   = uint32(dbpb.GmRecallOpStatus_GM_RECALL_OP_APPLIED)
	recallStatusRejected  = uint32(dbpb.GmRecallOpStatus_GM_RECALL_OP_REJECTED)
	recallStatusAborted   = uint32(dbpb.GmRecallOpStatus_GM_RECALL_OP_ABORTED)
	recallStatusPartial   = uint32(dbpb.GmRecallOpStatus_GM_RECALL_OP_PARTIAL)
	recallStatusPlanned   = uint32(dbpb.GmRecallOpStatus_GM_RECALL_OP_PLANNED)
	recallStatusCancelled = uint32(dbpb.GmRecallOpStatus_GM_RECALL_OP_CANCELLED)
	recallStatusRoleGone  = uint32(dbpb.GmRecallOpStatus_GM_RECALL_OP_ROLE_GONE)

	recallBatchRunning  = uint32(dbpb.GmRecallBatchState_GM_RECALL_BATCH_RUNNING)
	recallBatchFinished = uint32(dbpb.GmRecallBatchState_GM_RECALL_BATCH_FINISHED)

	// RecallTxType 是回收指令的 tx_type,scene 的流水按它记。
	RecallTxType = uint32(rollbackpb.TransactionType_TX_BATCH_RECALL)

	// recallAuditRollbackType 是 rollback_audit_log.rollback_type 的取值:4 = 批量回收。
	recallAuditRollbackType uint32 = 4

	// recallInsertChunk 是一条多行 INSERT 的行数上限。一批最多一万行,不能拼成一条语句。
	recallInsertChunk = 200
	// recallSourceChunk 是按 tx_id 查已消费流水时一条 IN 的长度。
	recallSourceChunk = 500
	// recallManualSourceOpID 是人工处置登记的流水在 gm_recall_source.op_id 上的取值:没有回收指令。回收指令的 op_id 由发号器发,恒非 0。
	recallManualSourceOpID uint64 = 0
	// maxRecallReviewHandledSources 是一次人工处置登记的流水上限,和一批的同指令流水上限同值。
	maxRecallReviewHandledSources = 10000

	// mysqlErrTiDBWriteConflict 9007:TiDB 乐观写冲突。迁库之后它是最常见的一类,不判等于不重试。
	mysqlErrTiDBWriteConflict = 9007
)

var (
	// ErrRecallBatchExists:这个 request_id 已经有批次。调用方读出来比摘要。
	ErrRecallBatchExists = errors.New("recall batch already exists")
	// ErrRecallSourceConsumed:计划里的流水已经被另一批回收了。事务已回滚,调用方重新预览。
	ErrRecallSourceConsumed = errors.New("recall source transaction already consumed")
	// ErrRecallBatchNotFound:没有这个 request_id。
	ErrRecallBatchNotFound = errors.New("recall batch not found")
	// ErrRecallOpNotFound:没有这个 op_id。
	ErrRecallOpNotFound = errors.New("recall op not found")
	// ErrRecallFinalInvalid:人工终结只收拒绝、中止、部分。全额结果只能来自 scene 的明细。
	ErrRecallFinalInvalid = errors.New("recall manual final status not allowed")

	// errRecallFenced 是派发撞上冻结或已删除的角色。只在本文件流转。
	errRecallFenced = errors.New("recall role fenced")
	// errRecallOpLost 是派发的 CAS 落空:这一行已被取消。事务回滚,seq 不消耗。
	errRecallOpLost = errors.New("recall op no longer planned")

	recallSeqTables = assetop.SeqTables{SeqTable: "gm_recall_seq", OpTable: "gm_recall_op", PendingStatus: recallStatusPending}
)

// RecallBatchRow 是 gm_recall_batch 的一行。
type RecallBatchRow struct {
	RequestID       uint64
	RequestDigest   []byte
	PlanDigest      []byte
	Kind            uint32
	ItemConfigID    uint32
	CurrencyType    uint32
	TimeStart       uint64
	TimeEnd         uint64
	Operator        string
	Reason          string
	PeerAddr        string
	State           uint32
	PlayerCount     uint32
	OpCount         uint32
	OpenOps         uint32
	SourceRows      uint32
	OpsComplete     uint32
	OpsShortfall    uint32
	OpsFailed       uint32
	AmountRequested uint64
	AmountTaken     uint64
	AmountDebt      uint64
	AmountShortfall uint64
	CancelledMs     uint64
	CreatedMs       uint64
	FinishedMs      uint64
}

// Finished 报告批次是否已经全部有结局。
func (b *RecallBatchRow) Finished() bool { return b.State == recallBatchFinished }

// RecallOpPlan 是冻结计划时写入的一行。
type RecallOpPlan struct {
	OpID         uint64
	PlayerID     uint64
	Kind         uint32
	CurrencyType uint32
	ItemConfigID uint32
	Requested    uint64
	Payload      []byte   // AssetBundle 序列化
	SourceTxIDs  []uint64 // 这一行消费的流水
}

// RecallOpRow 是 gm_recall_op 给运维看的那部分列。
type RecallOpRow struct {
	OpID         uint64
	PlayerID     uint64
	Status       uint32
	CurrencyType uint32
	ItemConfigID uint32
	Requested    uint64
	Taken        uint64
	Debt         uint64
	Shortfall    uint64
	LastReason   uint32
	Attempts     uint32
}

// IsOpen 报告这一行还没有结局。
func (o *RecallOpRow) IsOpen() bool {
	return o.Status == recallStatusPlanned || o.Status == recallStatusPending
}

// IsApplied 报告 scene 已记账。扣到多少看 Taken、Debt、Shortfall。
func (o *RecallOpRow) IsApplied() bool { return o.Status == recallStatusApplied }

// IsPartial 报告应用到一半内部出错。Taken 只是确认过的下限,实际扣除要按流水核对。
func (o *RecallOpRow) IsPartial() bool { return o.Status == recallStatusPartial }

// RecallOpState 是按 recallOpColumns 读出的一行:还原出的请求,加上行上的状态与租约。
// Claim 下发与人工终结读证据(ReadOpState)都经 scanRecallOp 得到它,两边看到的是同一份请求。
type RecallOpState struct {
	// Status 是 GmRecallOpStatus。
	Status uint32
	// Op 是这一行在资产通道里的请求:correlation_id = op_id,载荷是冻结计划时写下的 AssetBundle。行上的 last_reason、
	// last_send_ms、created_ms、attempts、deadline_ms 也在里面,是同一次扫出的列值;LeaseToken 只属于领取方,这里为 0。
	Op assetop.Op
	// OpErr 非 nil:载荷解不开,Op.Bundle 为 nil。Claim 把这一行当毒行推迟;人工终结核不了收据,按证据不足处理。
	OpErr error
	// LeaseUntilMs 是读行这一刻的租约到期时刻:判「此刻有没有人持租约」与人工终结的见证都要它。
	LeaseUntilMs uint64
	// LeaseToken 是读行这一刻行上的领取令牌(lease_token 列),0 = 没人领过。Op.LeaseToken 不填它:那是领取方自己的令牌。
	// Claim 回读拿它核对这一行是不是自己刚领到的(heldBy)。终结不清这一列,所以光看令牌不够,还要看 status。
	LeaseToken uint64
}

// IsPending 报告这一行已分配 seq、还在投递。只有它能被领取,也只有它能人工终结。
func (s RecallOpState) IsPending() bool { return s.Status == recallStatusPending }

// heldBy 报告这一行是不是持 token 的领取方手里的未决行:还是 PENDING,令牌也还是它的。Claim 回读之后只下发这样的行。
func (s RecallOpState) heldBy(token uint64) bool { return s.IsPending() && s.LeaseToken == token }

// RecallPlanned 是等待派发的一行。
type RecallPlanned struct {
	OpID     uint64
	PlayerID uint64
}

// RecallDispatchResult 是派发一行的结果。
type RecallDispatchResult int

const (
	// RecallDispatched:已分配 seq,进入 PENDING。
	RecallDispatched RecallDispatchResult = iota
	// RecallDeferred:这次派不了(角色冻结、这条流积压满、目录读不到),行留在 PLANNED,稍后再来。
	RecallDeferred
	// RecallRoleGone:角色已删除或目录里没有,行已终结。
	RecallRoleGone
	// RecallLost:这一行不再是 PLANNED(已被取消或别的副本派发过)。
	RecallLost
)

// RecallStore 是回收 outbox 的 SQL 实现,同时是 assetop.Store。
type RecallStore struct {
	db       *sql.DB
	auditIDs idsegment.Source
	guard    *rolefence.Guard
}

// NewRecallStore 连流水库。表由 -migrate 预建,这里只读核护栏副本表。cfg 是 ForSchema 之后的流水库配置。
func NewRecallStore(ctx context.Context, cfg MySQLConfig) (*RecallStore, error) {
	// 和 RouteStore 同样钉 READ COMMITTED 和 1 秒锁等待:协调者问参与者的 1.5 秒预算按这个算。
	db, err := openMySQLWithDSN(cfg, buildRouteDSN(cfg))
	if err != nil {
		return nil, err
	}
	if err := rolefence.VerifySchema(ctx, db); err != nil {
		return nil, errors.Join(fmt.Errorf("recall store: role_fence: %w", err), db.Close())
	}
	return &RecallStore{db: db}, nil
}

// Close 关连接池。
func (s *RecallStore) Close() error { return s.db.Close() }

// DB 是护栏副本所在的连接池,给参与者服务用。
func (s *RecallStore) DB() *sql.DB { return s.db }

// Configure 接上审计行号和护栏读取。两个都要:少一个就写不了审计或判不了角色,装配方当拒启处理。
func (s *RecallStore) Configure(auditIDs idsegment.Source, reader rolefence.StateReader) error {
	if auditIDs == nil || reader == nil {
		return errors.New("recall store: audit row ids and role fence reader are required")
	}
	s.auditIDs = auditIDs
	s.guard = rolefence.NewGuard(s.db, reader)
	return nil
}

// isRecallTxRetryable 判「事务已整体回滚,整笔重跑是安全的」:1213 死锁、1205 锁等待超时、9007 TiDB 写冲突,
// 以及权威 SQL 守卫判定的「提交前中止」(authoritysql.IsAbortedBeforeCommit,R6):事务里只发过纯 DML、COMMIT 还没发时
// 连接断开或调用取消,库会回滚未提交的事务,确定没写,换一条连接整笔重跑。错误树里混有结局未知时它回假,照旧不重试。
func isRecallTxRetryable(err error) bool {
	if authoritysql.IsAbortedBeforeCommit(err) {
		return true
	}
	var me *mysql.MySQLError
	if !errors.As(err, &me) {
		return false
	}
	return me.Number == mysqlErrDeadlock || me.Number == mysqlErrLockWaitTimeout || me.Number == mysqlErrTiDBWriteConflict
}

func (s *RecallStore) tx(ctx context.Context, fn func(*sql.Tx) error) error {
	return assetop.WithTxRetry(ctx, s.db, 3, isRecallTxRetryable, fn)
}

func isDuplicateKey(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == mysqlErrDupEntry
}

// ── 冻结计划 ────────────────────────────────────────────────────────────────

// ConsumedSources 回这批流水里已经被回收过的 tx_id。
func (s *RecallStore) ConsumedSources(ctx context.Context, txIDs []uint64) (map[uint64]bool, error) {
	consumed := make(map[uint64]bool)
	for start := 0; start < len(txIDs); start += recallSourceChunk {
		end := min(start+recallSourceChunk, len(txIDs))
		args := make([]any, 0, end-start)
		for _, id := range txIDs[start:end] {
			args = append(args, id)
		}
		rows, err := s.db.QueryContext(ctx, "SELECT tx_id FROM gm_recall_source WHERE tx_id IN ("+placeholders(len(args))+")", args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id uint64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			consumed[id] = true
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return consumed, nil
}

// RecallSourceState 是一组流水在 gm_recall_source 里的登记情况。
type RecallSourceState struct {
	// Manual:其中有流水登记为交人工处置(op_id = 0)。
	Manual bool
	// Batch:其中有流水已被回收批次消费(op_id ≠ 0)。
	Batch bool
}

// recallSourceQueryer 是 *sql.DB 与 *sql.Tx 共有的读接口:登记前在事务外看一次,登记时在事务里再看一次。
type recallSourceQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// SourceState 读一组流水的登记情况。普通读,不加锁:登记时在事务里再读一次,并发由 gm_recall_source 的主键兜住。
func (s *RecallStore) SourceState(ctx context.Context, txIDs []uint64) (RecallSourceState, error) {
	return recallSourceStateOf(ctx, s.db, txIDs)
}

func recallSourceStateOf(ctx context.Context, q recallSourceQueryer, txIDs []uint64) (RecallSourceState, error) {
	var state RecallSourceState
	for start := 0; start < len(txIDs); start += recallSourceChunk {
		end := min(start+recallSourceChunk, len(txIDs))
		args := make([]any, 0, end-start)
		for _, id := range txIDs[start:end] {
			args = append(args, id)
		}
		rows, err := q.QueryContext(ctx, "SELECT op_id FROM gm_recall_source WHERE tx_id IN ("+placeholders(len(args))+")", args...)
		if err != nil {
			return RecallSourceState{}, err
		}
		for rows.Next() {
			var opID uint64
			if err := rows.Scan(&opID); err != nil {
				rows.Close()
				return RecallSourceState{}, err
			}
			if opID == recallManualSourceOpID {
				state.Manual = true
			} else {
				state.Batch = true
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return RecallSourceState{}, err
		}
	}
	return state, nil
}

// RecallReviewHandled 是一次「needs_review 指令交人工处置」的登记:这名玩家、这条指令此刻的全部流水(已按回收的资产过滤)。
type RecallReviewHandled struct {
	PlayerID      uint64
	CorrelationID uint64
	TxIDs         []uint64
	Operator      string
	Reason        string
	// Detail 进审计行:资产、原因码、对端地址,由调用方拼好。
	Detail string
}

// RegisterReviewHandled 把一条 needs_review 指令登记为交人工处置:它的全部流水在一个事务里按已消费写进 gm_recall_source
// (op_id = 0,request_id = 同一事务里写的审计行号),外加一行审计(操作人、理由、指令)。和冻结计划写同一个主键,以后的批次
// 和预览都把这条指令剔除;人工登记的流水没有放回路径。这条指令确实在 needs_review 里,由调用方(logic.RegisterRecallReviewHandled)
// 按计划的同一份代码核过,这里只管原子写入。
//
// 回 (true, nil):本次登记。(false, nil):其中已有流水登记为交人工处置,幂等,什么都没写。
// ErrRecallSourceConsumed:其中有流水已被回收批次消费,或并发写撞了主键;事务已回滚,调用方用 SourceState 重读分辨。
func (s *RecallStore) RegisterReviewHandled(ctx context.Context, h RecallReviewHandled, nowMs uint64) (bool, error) {
	if h.PlayerID == 0 || h.CorrelationID == 0 || len(h.TxIDs) == 0 || len(h.TxIDs) > maxRecallReviewHandledSources || h.Operator == "" {
		return false, fmt.Errorf("recall review handled: invalid registration player_id=%d correlation_id=%d sources=%d",
			h.PlayerID, h.CorrelationID, len(h.TxIDs))
	}
	auditID, err := nextRowID(ctx, s.auditIDs)
	if err != nil {
		return false, fmt.Errorf("mint recall audit row id: %w", err)
	}
	registered := false
	err = s.tx(ctx, func(tx *sql.Tx) error {
		registered = false
		state, err := recallSourceStateOf(ctx, tx, h.TxIDs)
		if err != nil {
			return err
		}
		switch {
		case state.Batch:
			return ErrRecallSourceConsumed
		case state.Manual:
			return nil
		}
		// 与冻结计划同一种写法:没有回收指令,op_id 记 0;request_id 记本次审计行号,事后按它找到操作人和理由。
		sources := []*RecallOpPlan{{OpID: recallManualSourceOpID, PlayerID: h.PlayerID, SourceTxIDs: h.TxIDs}}
		if err := insertRecallSources(ctx, tx, auditID, sources, nowMs); err != nil {
			if isDuplicateKey(err) {
				return ErrRecallSourceConsumed
			}
			return err
		}
		reason := fmt.Sprintf("batch_recall REVIEW_HANDLED player=%d correlation=%d sources=%d %s: %s",
			h.PlayerID, h.CorrelationID, len(h.TxIDs), h.Detail, h.Reason)
		if err := insertRecallAudit(ctx, tx, auditID, 0, 0, 0, reason, h.Operator, nowMs); err != nil {
			return err
		}
		registered = true
		return nil
	})
	return registered, err
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// CreateBatch 冻结计划:批次、全部 op 行、消费的流水、受理审计在一个事务里。提交之后计划不再变,重放只读现状。
//
// 撞上批次主键回 ErrRecallBatchExists;撞上流水主键回 ErrRecallSourceConsumed。两种都什么都没写。
func (s *RecallStore) CreateBatch(ctx context.Context, batch *RecallBatchRow, ops []*RecallOpPlan, nowMs uint64) error {
	auditID, err := nextRowID(ctx, s.auditIDs)
	if err != nil {
		return fmt.Errorf("mint recall audit row id: %w", err)
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		if err := insertRecallBatch(ctx, tx, batch, nowMs); err != nil {
			if isDuplicateKey(err) {
				return ErrRecallBatchExists
			}
			return err
		}
		if err := insertRecallOps(ctx, tx, batch.RequestID, ops, nowMs); err != nil {
			return err
		}
		if err := insertRecallSources(ctx, tx, batch.RequestID, ops, nowMs); err != nil {
			if isDuplicateKey(err) {
				return ErrRecallSourceConsumed
			}
			return err
		}
		reason := fmt.Sprintf("batch_recall ACCEPTED request_id=%d plan=%s ops=%d players=%d: %s", batch.RequestID,
			hex.EncodeToString(batch.PlanDigest), batch.OpCount, batch.PlayerCount, batch.Reason)
		return insertRecallAudit(ctx, tx, auditID, batch.TimeStart, 0, 0, reason, batch.Operator, nowMs)
	})
}

func insertRecallBatch(ctx context.Context, tx *sql.Tx, b *RecallBatchRow, nowMs uint64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO gm_recall_batch
		(request_id,request_digest,plan_digest,recall_kind,item_config_id,currency_type,time_start,time_end,operator,reason,peer_addr,
		 state,player_count,op_count,open_ops,source_rows,ops_complete,ops_shortfall,ops_failed,
		 amount_requested,amount_taken,amount_debt,amount_shortfall,cancelled_ms,created_ms,updated_ms,finished_ms)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,0,0,0,?,0,0,0,0,?,?,0)`,
		b.RequestID, b.RequestDigest, b.PlanDigest, b.Kind, b.ItemConfigID, b.CurrencyType, b.TimeStart, b.TimeEnd, b.Operator, b.Reason, b.PeerAddr,
		recallBatchRunning, b.PlayerCount, b.OpCount, b.OpCount, b.SourceRows, b.AmountRequested, nowMs, nowMs)
	return err
}

func insertRecallOps(ctx context.Context, tx *sql.Tx, requestID uint64, ops []*RecallOpPlan, nowMs uint64) error {
	const row = "(?,?,?,?,0,?,?,?,?,0,?,0,0,0,0,0,0,?,?,0,?,?,?,?,0,0,0,'','')"
	for start := 0; start < len(ops); start += recallInsertChunk {
		chunk := ops[start:min(start+recallInsertChunk, len(ops))]
		values := make([]string, 0, len(chunk))
		args := make([]any, 0, len(chunk)*15)
		for _, op := range chunk {
			values = append(values, row)
			// PLANNED 行没有 seq:stream_epoch = 0、seq = op_id 占唯一键,真实纪元恒非 0,不会和已派发的行撞。
			args = append(args, op.OpID, requestID, op.PlayerID, uint32(assetpb.AssetOpStream_ASSET_OP_STREAM_GM_RECALL), op.OpID,
				recallStatusPlanned, op.Payload, RecallTxType, nowMs, nowMs, nowMs,
				op.Kind, op.CurrencyType, op.ItemConfigID, op.Requested)
		}
		query := `INSERT INTO gm_recall_op
			(op_id,request_id,player_id,stream,stream_epoch,seq,status,payload,tx_type,attempts,next_attempt_ms,
			 lease_until_ms,lease_token,last_outcome,last_reason,durable,deadline_ms,created_ms,updated_ms,last_send_ms,
			 recall_kind,currency_type,item_config_id,requested,taken,debt,shortfall,resolved_by,resolve_reason)
			VALUES ` + strings.Join(values, ",")
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return err
		}
	}
	return nil
}

func insertRecallSources(ctx context.Context, tx *sql.Tx, requestID uint64, ops []*RecallOpPlan, nowMs uint64) error {
	values := make([]string, 0, recallInsertChunk)
	args := make([]any, 0, recallInsertChunk*5)
	flush := func() error {
		if len(values) == 0 {
			return nil
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO gm_recall_source(tx_id,request_id,player_id,op_id,created_ms) VALUES "+strings.Join(values, ","), args...)
		values, args = values[:0], args[:0]
		return err
	}
	for _, op := range ops {
		for _, txID := range op.SourceTxIDs {
			values = append(values, "(?,?,?,?,?)")
			args = append(args, txID, requestID, op.PlayerID, op.OpID, nowMs)
			if len(values) == recallInsertChunk {
				if err := flush(); err != nil {
					return err
				}
			}
		}
	}
	return flush()
}

func insertRecallAudit(ctx context.Context, tx *sql.Tx, id, targetTime uint64, affected, failed uint32, reason, operator string, nowMs uint64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO rollback_audit_log
		(id, player_id, zone_id, rollback_type, snapshot_id_used, pre_rollback_snapshot_id,
		 target_time, players_affected, players_failed, orphans_cleaned, reason, operator, created_at)
		VALUES (?, 0, 0, ?, 0, 0, ?, ?, ?, 0, ?, ?, ?)`,
		id, recallAuditRollbackType, targetTime, affected, failed, reason, operator, nowMs/1000)
	return err
}

// ── 读 ─────────────────────────────────────────────────────────────────────

const recallBatchColumns = `request_id,request_digest,plan_digest,recall_kind,item_config_id,currency_type,time_start,time_end,operator,reason,peer_addr,
	state,player_count,op_count,open_ops,source_rows,ops_complete,ops_shortfall,ops_failed,
	amount_requested,amount_taken,amount_debt,amount_shortfall,cancelled_ms,created_ms,finished_ms`

func scanRecallBatch(row interface{ Scan(...any) error }) (*RecallBatchRow, error) {
	b := &RecallBatchRow{}
	err := row.Scan(&b.RequestID, &b.RequestDigest, &b.PlanDigest, &b.Kind, &b.ItemConfigID, &b.CurrencyType, &b.TimeStart, &b.TimeEnd, &b.Operator, &b.Reason, &b.PeerAddr,
		&b.State, &b.PlayerCount, &b.OpCount, &b.OpenOps, &b.SourceRows, &b.OpsComplete, &b.OpsShortfall, &b.OpsFailed,
		&b.AmountRequested, &b.AmountTaken, &b.AmountDebt, &b.AmountShortfall, &b.CancelledMs, &b.CreatedMs, &b.FinishedMs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrRecallBatchNotFound
	}
	if err != nil {
		return nil, err
	}
	return b, nil
}

// ReadBatch 读一批。没有回 ErrRecallBatchNotFound。
func (s *RecallStore) ReadBatch(ctx context.Context, requestID uint64) (*RecallBatchRow, error) {
	return scanRecallBatch(s.db.QueryRowContext(ctx, "SELECT "+recallBatchColumns+" FROM gm_recall_batch WHERE request_id=?", requestID))
}

// ListOps 读一批的全部行,按 op_id 升序。行数由计划上限(1 万)封顶。
func (s *RecallStore) ListOps(ctx context.Context, requestID uint64) ([]*RecallOpRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT op_id,player_id,status,currency_type,item_config_id,requested,taken,debt,shortfall,last_reason,attempts
		FROM gm_recall_op WHERE request_id=? ORDER BY op_id`, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*RecallOpRow
	for rows.Next() {
		op := &RecallOpRow{}
		if err := rows.Scan(&op.OpID, &op.PlayerID, &op.Status, &op.CurrencyType, &op.ItemConfigID, &op.Requested, &op.Taken, &op.Debt, &op.Shortfall, &op.LastReason, &op.Attempts); err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

// ── 派发 ────────────────────────────────────────────────────────────────────

// ListPlannedOwned 读本副本负责的片里到期的 PLANNED 行,最多 limit 行,按 (next_attempt_ms, op_id) 升序。
//
// owned 是派发按 player_id 分片的条件(缺口清单第 25 条第四批,docs/design/assetop-redelivery-sharding-20261110.md §14.2),
// 接在 WHERE 里、LIMIT 之前:别的副本的行再早到期也不占本副本的名额。零值不过滤(成员表为空、或全部片都归本副本)。
// 普通读、不加锁:派不派得成由 Dispatch 在 role_fence 行锁下的状态 CAS 决定。limit 非正时不发 SQL。
func (s *RecallStore) ListPlannedOwned(ctx context.Context, nowMs uint64, limit int, owned workshard.Filter) ([]RecallPlanned, error) {
	if limit <= 0 {
		return nil, nil
	}
	args := append([]any{recallStatusPlanned, nowMs}, owned.Args()...)
	rows, err := s.db.QueryContext(ctx, recallListPlannedQuery(owned), append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RecallPlanned
	for rows.Next() {
		var p RecallPlanned
		if err := rows.Scan(&p.OpID, &p.PlayerID); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// recallListPlannedQuery 拼派发的到期扫描。分片条件只来自 workshard.SQLFilter(列名写死、片号只走绑定参数),
// 不含外部输入,位置在 ORDER BY / LIMIT 之前。实参顺序与占位符一致:PLANNED、now、片号……、LIMIT。
func recallListPlannedQuery(owned workshard.Filter) string {
	return "SELECT op_id,player_id FROM gm_recall_op WHERE status=? AND next_attempt_ms<=? AND " + owned.SQL() +
		" ORDER BY next_attempt_ms,op_id LIMIT ?"
}

// DeferPlanned 把一行 PLANNED 推迟到 until。
func (s *RecallStore) DeferPlanned(ctx context.Context, opID, until, nowMs uint64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE gm_recall_op SET next_attempt_ms=?,updated_ms=? WHERE op_id=? AND status=?", until, nowMs, opID, recallStatusPlanned)
	return err
}

// Dispatch 给一行 PLANNED 分配 seq。事务持本库 role_fence 行锁,和护栏 Prepare 串行:
// 票先赢,Prepare 读得到这一行;冻结先赢,这里被拒,行留在 PLANNED。
//
// 确定没写的情况都不回错误:角色冻结、这条流积压满、目录读不到回 RecallDeferred,行还在,稍后再来。
func (s *RecallStore) Dispatch(ctx context.Context, op RecallPlanned, nowMs uint64) (RecallDispatchResult, error) {
	stream := assetpb.AssetOpStream_ASSET_OP_STREAM_GM_RECALL
	if err := assetop.EnsureSeqRowRetry(ctx, s.db, recallSeqTables, op.PlayerID, stream, nowMs, isRecallTxRetryable); err != nil {
		return RecallDeferred, err
	}
	var roleDeleted bool
	err := s.guard.RunTx(ctx, func() error {
		roleDeleted = false
		return s.tx(ctx, func(tx *sql.Tx) error {
			fence, lockErr := rolefence.Lock(ctx, tx, op.PlayerID)
			// 不带原身份:GM 回收的是这名角色,不管现在归谁。只判冻结和删除。
			if err := rolefence.Check(fence, lockErr, rolefence.Expect{PlayerID: op.PlayerID, Reject: errRecallFenced}); err != nil {
				roleDeleted = lockErr == nil && fence.Account == "" && fence.FreezeToken == 0
				return err
			}
			alloc, err := assetop.AllocateSeq(ctx, tx, recallSeqTables, op.PlayerID, stream, assetop.DefaultLimits, nowMs)
			if err != nil {
				return err
			}
			// last_send_ms 在这里初始化(go/shared/assetop/abandon.go 文件头 last_send_ms 契约第 1 条):PLANNED 的行从没发过,
			// 进 PENDING 之后才会被领取、投递,所以此刻的 nowMs 不小于任何一次真实发送。不写的话它一直是 0(「Store 没维护」),
			// 从没投出去的行永远判不了安静:人工终结只能关成 PARTIAL(少回收),自动放弃也永远跳过它。
			res, err := tx.ExecContext(ctx, "UPDATE gm_recall_op SET status=?,seq=?,stream_epoch=?,next_attempt_ms=?,updated_ms=?,last_send_ms=GREATEST(last_send_ms,?) WHERE op_id=? AND status=?",
				recallStatusPending, alloc.Seq, alloc.Epoch, nowMs, nowMs, nowMs, op.OpID, recallStatusPlanned)
			if err != nil {
				return err
			}
			affected, err := res.RowsAffected()
			if err != nil {
				return err
			}
			if affected != 1 {
				return errRecallOpLost
			}
			return nil
		})
	})
	var notFound *rolefence.RoleNotFoundError
	switch {
	case err == nil:
		return RecallDispatched, nil
	case errors.Is(err, errRecallOpLost):
		return RecallLost, nil
	case errors.As(err, &notFound), errors.Is(err, errRecallFenced) && roleDeleted:
		return s.closeRoleGone(ctx, op, nowMs)
	case errors.Is(err, errRecallFenced), errors.Is(err, assetop.ErrTooManyPending), errors.Is(err, rolefence.ErrUnavailable):
		return RecallDeferred, nil
	default:
		return RecallDeferred, err
	}
}

// closeRoleGone 把一行 PLANNED 终结成 ROLE_GONE。
func (s *RecallStore) closeRoleGone(ctx context.Context, op RecallPlanned, nowMs uint64) (RecallDispatchResult, error) {
	auditID, err := nextRowID(ctx, s.auditIDs)
	if err != nil {
		return RecallDeferred, fmt.Errorf("mint recall audit row id: %w", err)
	}
	closed := false
	err = s.tx(ctx, func(tx *sql.Tx) error {
		closed = false
		var err error
		closed, err = s.settle(ctx, tx, recallSettlement{opID: op.OpID, from: recallStatusPlanned, to: recallStatusRoleGone}, auditID, nowMs)
		return err
	})
	if err != nil {
		return RecallDeferred, err
	}
	if !closed {
		return RecallLost, nil
	}
	return RecallRoleGone, nil
}

// ── 终结 ────────────────────────────────────────────────────────────────────

// recallSettlement 是一行走到终局的全部输入。line 为 nil 表示没有回收结果,整行算缺口。
type recallSettlement struct {
	opID     uint64
	from, to uint32
	line     *assetpb.AssetRecallLine
	// answer 是终结所凭的 scene 答复(重投循环拿到的落盘结局),写进 last_outcome / last_reason 并置 durable=1。
	// nil 表示这次终结不是 scene 的答复(人工终结、ROLE_GONE):这三列一个都不动,留着最后一次投递时 scene 的答复,事后核对用。
	answer     *recallAnswer
	resolvedBy string
	resolution string
	// witness 只有人工终结带:读证据那一刻行上的 lease_until_ms、last_send_ms。持行锁时对不上就不终结。
	witness *assetop.QuietWitness
}

// recallAnswer 是 scene 的一次答复:结局与原因码(assetpb.AssetOpOutcome 的数值、AssetReason)。
type recallAnswer struct {
	outcome, reason uint32
}

// gm_recall_op 的 durable、last_outcome、last_reason 三列描述同一件事:**最后一次 scene 答复**。last_outcome / last_reason 是那次答复
// 的结局与原因码;durable=1 表示那次答复报告结局已经落盘(写进了玩家存档),只有这样的答复才能终结一行(assetop.Result.Terminal)。
// 重排(Reschedule)每次按答复覆盖三列;自动终结(Finalize)把终结所凭的答复写进去,所以 durable 恒为 1。
// 人工终结和 ROLE_GONE 不是 scene 的答复,三列都不动:终态行上留着最后一次投递时 scene 实际回的是什么,事后和账本、流水对得上;
// 「这一行是谁终结的」看 resolved_by(人工终结写操作人,自动终结与 ROLE_GONE 为空),不看 durable。
const (
	// settleRecallOpWithAnswerSQL 是自动终结:写终态、明细,以及终结所凭的 scene 答复。
	settleRecallOpWithAnswerSQL = `UPDATE gm_recall_op SET status=?,durable=1,last_outcome=?,last_reason=?,lease_until_ms=0,updated_ms=?,
		taken=?,debt=?,shortfall=?,resolved_by=?,resolve_reason=? WHERE op_id=?`
	// settleRecallOpSQL 是人工终结与 ROLE_GONE:只写终态、明细与经办信息,durable、last_outcome、last_reason 保留原值。
	settleRecallOpSQL = `UPDATE gm_recall_op SET status=?,lease_until_ms=0,updated_ms=?,
		taken=?,debt=?,shortfall=?,resolved_by=?,resolve_reason=? WHERE op_id=?`
)

// settle 在调用方事务里走一行的终局:批次行 → op 行 → 改 op → 改批次,减到 0 写结束审计。回这一行是否本次终结。
// 行已经不在 from 状态时什么都不改,回 false:这是「一次且仅一次」的最后一道闸。人工终结另比见证,对不上同样回 false。
func (s *RecallStore) settle(ctx context.Context, tx *sql.Tx, st recallSettlement, auditID, nowMs uint64) (bool, error) {
	var requestID uint64
	if err := tx.QueryRowContext(ctx, "SELECT request_id FROM gm_recall_op WHERE op_id=?", st.opID).Scan(&requestID); err != nil {
		return false, err
	}
	batch, err := scanRecallBatch(tx.QueryRowContext(ctx, "SELECT "+recallBatchColumns+" FROM gm_recall_batch WHERE request_id=? FOR UPDATE", requestID))
	if err != nil {
		return false, err
	}
	var status uint32
	var requested, leaseUntilMs, lastSendMs uint64
	if err := tx.QueryRowContext(ctx, "SELECT status,requested,lease_until_ms,last_send_ms FROM gm_recall_op WHERE op_id=? FOR UPDATE", st.opID).
		Scan(&status, &requested, &leaseUntilMs, &lastSendMs); err != nil {
		return false, err
	}
	if status != st.from {
		return false, nil
	}
	// 人工终结的证据是按读行那一刻下的。读完之后有人领过这一行(领取必改 lease_until_ms)或发出过请求(重排推进
	// last_send_ms),那一次投递可能已经让 scene 扣了,证据作废,不终结,调用方重新读证据。和 status 在同一把行锁下比,
	// 与领取、重排、终结串行。只领没发(玩家不在线,本地 NOT_HERE 重排)会把租约清回 0、不动 last_send_ms,
	// 读证据时租约就是 0 的话比对照样通过:那一次没有请求发出去,证据不受影响(last_send_ms 契约见 assetop/abandon.go 文件头)。
	if st.witness != nil && (leaseUntilMs != st.witness.LeaseUntilMs || lastSendMs != st.witness.LastSendMs) {
		return false, nil
	}

	taken, debt, shortfall := uint64(0), uint64(0), requested
	if st.line != nil {
		taken, debt, shortfall = st.line.GetTaken(), st.line.GetDebt(), st.line.GetShortfall()
	}
	// 确定没回收到东西的行,流水放回去。部分应用的不放:可能已经扣过了,再回收一遍就是重复。
	if st.to == recallStatusRejected || st.to == recallStatusAborted || st.to == recallStatusRoleGone {
		if _, err := tx.ExecContext(ctx, "DELETE FROM gm_recall_source WHERE op_id=?", st.opID); err != nil {
			return false, err
		}
	}
	if st.answer != nil {
		_, err = tx.ExecContext(ctx, settleRecallOpWithAnswerSQL,
			st.to, st.answer.outcome, st.answer.reason, nowMs, taken, debt, shortfall, st.resolvedBy, st.resolution, st.opID)
	} else {
		_, err = tx.ExecContext(ctx, settleRecallOpSQL, st.to, nowMs, taken, debt, shortfall, st.resolvedBy, st.resolution, st.opID)
	}
	if err != nil {
		return false, err
	}

	complete, withShortfall, failed := uint32(0), uint32(0), uint32(0)
	switch {
	case st.to != recallStatusApplied:
		failed = 1
	case shortfall > 0:
		withShortfall = 1
	default:
		complete = 1
	}
	return true, s.advanceBatch(ctx, tx, batch, 1, complete, withShortfall, failed, taken, debt, shortfall, auditID, nowMs)
}

// advanceBatch 把批次的计数往前推。open_ops 减到 0 时批次结束,同一个事务写结束审计。
func (s *RecallStore) advanceBatch(ctx context.Context, tx *sql.Tx, b *RecallBatchRow, closed, complete, withShortfall, failed uint32, taken, debt, shortfall, auditID, nowMs uint64) error {
	if b.OpenOps < closed {
		// 计数和行对不上只可能是人动过库。宁可停在这里,也不把 uint32 减成一个极大值。
		return fmt.Errorf("recall batch %d open_ops=%d smaller than closing %d", b.RequestID, b.OpenOps, closed)
	}
	open := b.OpenOps - closed
	state, finishedMs := b.State, b.FinishedMs
	if open == 0 {
		state, finishedMs = recallBatchFinished, nowMs
	}
	if _, err := tx.ExecContext(ctx, `UPDATE gm_recall_batch SET open_ops=?,state=?,finished_ms=?,updated_ms=?,
		ops_complete=ops_complete+?,ops_shortfall=ops_shortfall+?,ops_failed=ops_failed+?,
		amount_taken=amount_taken+?,amount_debt=amount_debt+?,amount_shortfall=amount_shortfall+? WHERE request_id=?`,
		open, state, finishedMs, nowMs, complete, withShortfall, failed, taken, debt, shortfall, b.RequestID); err != nil {
		return err
	}
	if open != 0 {
		return nil
	}
	return s.auditFinished(ctx, tx, b, auditID, nowMs)
}

// auditFinished 写结束审计。players_affected、players_failed 从行上重算,不靠累计:审计表不能替没发生的回收背书。
func (s *RecallStore) auditFinished(ctx context.Context, tx *sql.Tx, b *RecallBatchRow, auditID, nowMs uint64) error {
	var affected, failed uint32
	if err := tx.QueryRowContext(ctx, `SELECT
		COUNT(DISTINCT CASE WHEN taken>0 OR debt>0 THEN player_id END),
		COUNT(DISTINCT CASE WHEN shortfall>0 THEN player_id END)
		FROM gm_recall_op WHERE request_id=?`, b.RequestID).Scan(&affected, &failed); err != nil {
		return err
	}
	reason := fmt.Sprintf("batch_recall FINISHED request_id=%d ops=%d: %s", b.RequestID, b.OpCount, b.Reason)
	return insertRecallAudit(ctx, tx, auditID, b.TimeStart, affected, failed, reason, b.Operator, nowMs)
}

// CancelBatch 取消一批。没派发的直接 CANCELLED;已派发的把截止时刻改成现在,下一次投递改发中止,scene 见过的 seq 回原结局。幂等。
func (s *RecallStore) CancelBatch(ctx context.Context, requestID uint64, operator, reason string, nowMs uint64) error {
	cancelAudit, err := nextRowID(ctx, s.auditIDs)
	if err != nil {
		return fmt.Errorf("mint recall audit row id: %w", err)
	}
	finishAudit, err := nextRowID(ctx, s.auditIDs)
	if err != nil {
		return fmt.Errorf("mint recall audit row id: %w", err)
	}
	return s.tx(ctx, func(tx *sql.Tx) error {
		batch, err := scanRecallBatch(tx.QueryRowContext(ctx, "SELECT "+recallBatchColumns+" FROM gm_recall_batch WHERE request_id=? FOR UPDATE", requestID))
		if err != nil {
			return err
		}
		if batch.CancelledMs != 0 || batch.Finished() {
			return nil
		}
		// 先改后数:派发不碰批次行,批次行锁挡不住它。先数的话,数到的 PLANNED 行可能正被一个没提交的派发改成 PENDING,
		// UPDATE 等它提交之后不再匹配,open_ops 就多减了,批次提前结束,那一行以后再也终结不了。
		res, err := tx.ExecContext(ctx, "UPDATE gm_recall_op SET status=?,shortfall=requested,updated_ms=? WHERE request_id=? AND status=?",
			recallStatusCancelled, nowMs, requestID, recallStatusPlanned)
		if err != nil {
			return err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return err
		}
		unsent := uint32(affected)
		// SUM 是 DECIMAL,一批的总量可能超过 int64,按字符串读。一批只能取消一次,所以 CANCELLED 全是这次改的。
		var unsentSum sql.NullString
		if err := tx.QueryRowContext(ctx, "SELECT SUM(requested) FROM gm_recall_op WHERE request_id=? AND status=?", requestID, recallStatusCancelled).Scan(&unsentSum); err != nil {
			return err
		}
		unsentAmount := uint64(0)
		if unsentSum.Valid {
			if unsentAmount, err = strconv.ParseUint(unsentSum.String, 10, 64); err != nil {
				return fmt.Errorf("recall batch %d unsent amount: %w", requestID, err)
			}
		}
		// 取消的行没回收到东西,流水放回去,改对条件之后下一批还能回收。
		if _, err := tx.ExecContext(ctx, "DELETE FROM gm_recall_source WHERE request_id=? AND op_id IN (SELECT op_id FROM gm_recall_op WHERE request_id=? AND status=?)",
			requestID, requestID, recallStatusCancelled); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE gm_recall_op SET deadline_ms=?,updated_ms=? WHERE request_id=? AND status=?",
			nowMs, nowMs, requestID, recallStatusPending); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE gm_recall_batch SET cancelled_ms=?,updated_ms=? WHERE request_id=?", nowMs, nowMs, requestID); err != nil {
			return err
		}
		auditReason := fmt.Sprintf("batch_recall CANCELLED request_id=%d unsent=%d: %s", requestID, unsent, reason)
		if err := insertRecallAudit(ctx, tx, cancelAudit, batch.TimeStart, 0, unsent, auditReason, operator, nowMs); err != nil {
			return err
		}
		if unsent == 0 {
			return nil
		}
		return s.advanceBatch(ctx, tx, batch, unsent, 0, 0, unsent, 0, 0, unsentAmount, finishAudit, nowMs)
	})
}

// ── assetop.Store ──────────────────────────────────────────────────────────

// ListDue 见 assetop.Store:扫全部片的到期行,即不带分片条件的 ListDueOwned。重投循环绑了按片分工
// (assetop.Loop.BindWorkOwner)之后不再调它。
func (s *RecallStore) ListDue(ctx context.Context, nowMs uint64, limit int) ([]uint64, error) {
	return s.ListDueOwned(ctx, nowMs, limit, workshard.Filter{})
}

// ListDueOwned 见 assetop.OwnedDueLister。两段读:新行优先,老行补缺口,按 op_id 去重。
// owned 是本副本负责的片(片号 = player_id % 64,缺口清单第 25 条,docs/design/assetop-redelivery-sharding-20261110.md),
// 两段都接在 WHERE 里、LIMIT 之前:别的副本的行再早到期也不占本副本的名额。零值不过滤。
func (s *RecallStore) ListDueOwned(ctx context.Context, nowMs uint64, limit int, owned workshard.Filter) ([]uint64, error) {
	if limit <= 0 {
		return nil, nil
	}
	seen := make(map[uint64]bool, limit)
	ids := make([]uint64, 0, limit)
	for _, comparison := range []string{"<", ">="} {
		remaining := limit - len(ids)
		if remaining == 0 {
			break
		}
		args := append([]any{recallStatusPending, nowMs, nowMs, assetop.FreshAttemptLimit}, owned.Args()...)
		rows, err := s.db.QueryContext(ctx, recallListDueQuery(comparison, owned), append(args, remaining)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id uint64
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return ids, nil
}

// recallListDueQuery 拼一段到期扫描:comparison 只取 "<"(新行)或 ">="(老行);分片条件只来自 workshard.SQLFilter
// (列名写死、片号只走绑定参数),不含外部输入,位置在 ORDER BY / LIMIT 之前。
// 实参顺序与占位符一致:PENDING、now、now、FreshAttemptLimit、片号……、LIMIT。
func recallListDueQuery(comparison string, owned workshard.Filter) string {
	return "SELECT op_id FROM gm_recall_op WHERE status=? AND next_attempt_ms<=? AND lease_until_ms<? AND attempts " +
		comparison + " ? AND " + owned.SQL() + " ORDER BY next_attempt_ms,op_id LIMIT ?"
}

// recallOpColumns 是还原一行请求要读的列,scanRecallOp 按这个列序扫。Claim 与 ReadOpState 共用。
const recallOpColumns = "player_id,stream,seq,stream_epoch,tx_type,payload,attempts,deadline_ms,last_reason,created_ms,last_send_ms,status,lease_until_ms,lease_token"

// scanRecallOp 按 recallOpColumns 扫一行,还原成资产通道里的请求。correlation_id 取 op_id:scene 流水与收据都按它对到这一行;
// tx_type 与载荷原样取冻结计划时写下的值,收据摘要才与首投逐字节一致。载荷解不开不算读失败:填 OpErr,由调用方决定怎么处理。
func scanRecallOp(row interface{ Scan(...any) error }, opID uint64) (RecallOpState, error) {
	state := RecallOpState{Op: assetop.Op{OpID: opID, CorrelationID: opID}}
	op := &state.Op
	var payload []byte
	var stream uint32
	if err := row.Scan(&op.PlayerID, &stream, &op.Seq, &op.StreamEpoch, &op.TxType, &payload, &op.Attempts, &op.DeadlineMs,
		&op.LastReason, &op.CreatedMs, &op.LastSendMs, &state.Status, &state.LeaseUntilMs, &state.LeaseToken); err != nil {
		return RecallOpState{}, err
	}
	op.Stream = assetpb.AssetOpStream(stream)
	bundle := &assetpb.AssetBundle{}
	if err := proto.Unmarshal(payload, bundle); err != nil {
		state.OpErr = fmt.Errorf("recall op %d: decode payload: %w", opID, err)
		return state, nil
	}
	op.Bundle = bundle
	return state, nil
}

// Claim 见 assetop.Store。
func (s *RecallStore) Claim(ctx context.Context, opID, nowMs, leaseUntilMs, poisonUntilMs, token uint64) (assetop.Op, bool, error) {
	res, err := s.db.ExecContext(ctx, "UPDATE gm_recall_op SET last_send_ms=GREATEST(last_send_ms,lease_until_ms),lease_until_ms=?,lease_token=? WHERE op_id=? AND status=? AND lease_until_ms<?",
		leaseUntilMs, token, opID, recallStatusPending, nowMs)
	if err != nil {
		return assetop.Op{}, false, err
	}
	affected, err := res.RowsAffected()
	if err != nil || affected == 0 {
		return assetop.Op{}, false, err
	}
	return s.claimedOp(ctx, opID, poisonUntilMs, token)
}

// claimedOp 是 Claim 在领取 CAS 之后的那一半:回读这一行,还原成要下发的请求。
//
// 回读的行必须仍是「我刚领到的未决行」(PENDING,令牌是自己的),否则不下发,写法同 guild 的 GuildAssetStore.Claim。
// 领取的 CAS 与这次回读是两条语句,中间可能被人工终结(ResolveRecallOp 的 settle 不换令牌),也可能被别的副本接管:
// 对已终结的行再投一次,结局写不回来,scene 还会再见到这个 seq。核对放在解载荷之前:已终结的坏包行不该再被当成毒行推迟。
func (s *RecallStore) claimedOp(ctx context.Context, opID, poisonUntilMs, token uint64) (assetop.Op, bool, error) {
	state, err := scanRecallOp(s.db.QueryRowContext(ctx, "SELECT "+recallOpColumns+" FROM gm_recall_op WHERE op_id=?", opID), opID)
	if err != nil {
		return assetop.Op{}, false, err
	}
	if !state.heldBy(token) {
		return assetop.Op{}, false, nil
	}
	if state.OpErr != nil {
		if err := s.deferPoison(ctx, opID, token, poisonUntilMs); err != nil {
			return assetop.Op{}, false, err
		}
		return assetop.Op{}, false, assetop.ErrPoisonRow
	}
	op := state.Op
	op.LeaseToken = token
	return op, true, nil
}

// deferPoison 把载荷解不开的一行推迟到 poisonUntilMs,last_outcome 记结局未知。只清租约,不动 last_send_ms
// (last_send_ms 契约第 4 条:解码在发送之前,这次租约内没有请求)。条件带本次令牌与 status = PENDING:回读之后这一行
// 若已被人工终结(令牌没换),推迟落空,终态行的 last_outcome、next_attempt_ms 不被改写。
func (s *RecallStore) deferPoison(ctx context.Context, opID, token, poisonUntilMs uint64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE gm_recall_op SET last_outcome=0,next_attempt_ms=?,lease_until_ms=0 WHERE op_id=? AND lease_token=? AND status=?",
		poisonUntilMs, opID, token, recallStatusPending)
	return err
}

// ReadOpState 读人工终结要核的一行:状态、与 Claim 下发同一套还原出的请求、判「安静」与终结见证要的租约和发送时刻。
// 普通读,不加锁:证据按这一刻的行下,终结时 ResolveManuallyIfQuiet 在结算事务里持行锁再比一次租约与发送时刻。
// 没有这一行回 ErrRecallOpNotFound。
func (s *RecallStore) ReadOpState(ctx context.Context, opID uint64) (RecallOpState, error) {
	state, err := scanRecallOp(s.db.QueryRowContext(ctx, "SELECT "+recallOpColumns+" FROM gm_recall_op WHERE op_id=?", opID), opID)
	if errors.Is(err, sql.ErrNoRows) {
		return RecallOpState{}, ErrRecallOpNotFound
	}
	return state, err
}

// Reschedule 见 assetop.Store。
func (s *RecallStore) Reschedule(ctx context.Context, op assetop.Op, nextAttemptMs uint64, res assetop.Result, nowMs uint64) error {
	durable := uint32(0)
	if res.Durable {
		durable = 1
	}
	lastSend := uint64(0)
	if !res.Local {
		lastSend = nowMs
	}
	result, err := s.db.ExecContext(ctx, `UPDATE gm_recall_op SET attempts=attempts+1,next_attempt_ms=?,lease_until_ms=0,durable=?,last_outcome=?,last_reason=?,updated_ms=?,
		last_send_ms=GREATEST(last_send_ms,?) WHERE op_id=? AND status=? AND lease_token=?`,
		nextAttemptMs, durable, uint32(res.Outcome), res.Reason, nowMs, lastSend, op.OpID, recallStatusPending, op.LeaseToken)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return fmt.Errorf("%w: op_id=%d", assetop.ErrLeaseLost, op.OpID)
	}
	return nil
}

// Finalize 见 assetop.Store。只收 SQL 已提交的结局,APPLIED 必须带明细:不按请求金额记账。
func (s *RecallStore) Finalize(ctx context.Context, op assetop.Op, status assetop.Status, res assetop.Result, nowMs uint64) (bool, error) {
	if !res.SQLCommitted {
		return false, errors.New("recall finalize without SQL commit proof")
	}
	st := recallSettlement{opID: op.OpID, from: recallStatusPending, answer: &recallAnswer{outcome: uint32(res.Outcome), reason: res.Reason}}
	switch status {
	case assetop.StatusApplied, assetop.StatusAppliedPartial:
		line, ok := assetop.RecallLineOf(op, res.Snapshot)
		if !ok {
			return false, fmt.Errorf("recall op %d committed without a matching recall line", op.OpID)
		}
		st.line = line
		st.to = recallStatusApplied
		if status == assetop.StatusAppliedPartial {
			st.to = recallStatusPartial
		}
	case assetop.StatusRejected:
		st.to = recallStatusRejected
	case assetop.StatusAborted:
		st.to = recallStatusAborted
	default:
		// 回收不开自动放弃:放弃等于玩家躲够时间就把赃物带回来。走到这里是装配错了。
		return false, fmt.Errorf("recall op %d: final status %s not allowed", op.OpID, status)
	}
	auditID, err := nextRowID(ctx, s.auditIDs)
	if err != nil {
		return false, fmt.Errorf("mint recall audit row id: %w", err)
	}
	finalized := false
	err = s.tx(ctx, func(tx *sql.Tx) error {
		var err error
		finalized, err = s.settle(ctx, tx, st, auditID, nowMs)
		return err
	})
	return finalized, err
}

// OldestPendingCreatedMs 见 assetop.PendingAgeReader。
func (s *RecallStore) OldestPendingCreatedMs(ctx context.Context, stream assetpb.AssetOpStream) (uint64, bool, error) {
	var stamp sql.NullInt64
	err := s.db.QueryRowContext(ctx, "SELECT MIN(created_ms) FROM gm_recall_op WHERE stream=? AND status=?", uint32(stream), recallStatusPending).Scan(&stamp)
	return uint64(stamp.Int64), stamp.Valid, err
}

// ListPendingByPlayer 见 assetop.PlayerPendingLister。
func (s *RecallStore) ListPendingByPlayer(ctx context.Context, playerID uint64, limit int) ([]uint64, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT op_id FROM gm_recall_op WHERE player_id=? AND status=? ORDER BY op_id LIMIT ?", playerID, recallStatusPending, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []uint64
	for rows.Next() {
		var id uint64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ResolveManuallyIfQuiet 是人工终结唯一的写路径(调用方 logic.ResolveRecallOp 先按已提交账本判过证据)。
// 只收拒绝、中止、部分:全额结果只能来自 scene 的明细。和 Finalize 走同一个 settle,另要求行上的 lease_until_ms、
// last_send_ms 仍是 witness(读证据时的值)。回 false、什么都不改:行已不是 PENDING,或读证据之后有人领过、发过它。
// 人工终结不是 scene 的答复:durable、last_outcome、last_reason 保留最后一次投递时的值(见 settleRecallOpSQL 上方的说明)。
//
// 没有不带见证的人工终结:回收不给 assetop.Loop 挂 Manual,不核证据的口子不留。
func (s *RecallStore) ResolveManuallyIfQuiet(ctx context.Context, r assetop.ManualResolution, witness assetop.QuietWitness, nowMs uint64) (bool, error) {
	st := recallSettlement{opID: r.OpID, from: recallStatusPending, resolvedBy: r.Operator, resolution: r.Reason, witness: &witness}
	switch r.Final {
	case assetop.StatusRejected:
		st.to = recallStatusRejected
	case assetop.StatusAborted:
		st.to = recallStatusAborted
	case assetop.StatusAppliedPartial:
		st.to = recallStatusPartial
	default:
		return false, ErrRecallFinalInvalid
	}
	auditID, err := nextRowID(ctx, s.auditIDs)
	if err != nil {
		return false, fmt.Errorf("mint recall audit row id: %w", err)
	}
	resolved := false
	err = s.tx(ctx, func(tx *sql.Tx) error {
		var err error
		resolved, err = s.settle(ctx, tx, st, auditID, nowMs)
		if err != nil || !resolved {
			return err
		}
		logx.Infof("[BatchRecall] op resolved manually op_id=%d final=%s operator=%s", r.OpID, r.Final, r.Operator)
		return nil
	})
	return resolved, err
}

// ── 角色护栏 ────────────────────────────────────────────────────────────────

// PendingForRole 是 rolefence.PendingFunc:PLANNED、PENDING 都算挂起。有未结束回收的角色不能过户、不能删。
//
// 普通读,不按 PendingFunc 的惯例加锁。连接钉了 READ COMMITTED(NewRecallStore),这条读在拿到副本行锁之后才取快照,看得到锁之前提交的派发;
// 派发排在后面,行只会离开这两个状态,多读到只会更保守。加锁读走二级索引 → 主键,和 settle 的主键 → 二级索引反序,能成环(同 assetop.AllocateSeq 的理由)。
// 冻计划不持副本锁,和 Prepare 同时提交时这里可能漏看:那一行派发时撞上冻结,留在 PLANNED,过户结束之后接着派,等于 GM 在过户之后才下的回收。
func (s *RecallStore) PendingForRole(ctx context.Context, tx *sql.Tx, state *rfpb.RoleFenceState) (string, error) {
	var opID uint64
	err := tx.QueryRowContext(ctx, "SELECT op_id FROM gm_recall_op WHERE player_id=? AND status IN (?,?) LIMIT 1",
		state.GetPlayerId(), recallStatusPlanned, recallStatusPending).Scan(&opID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return "gm_recall_op", nil
}

// RecallManualFinal 把 RPC 里的 GmRecallOpStatus 换成人工终结状态。只收拒绝、中止、部分。
func RecallManualFinal(status uint32) (assetop.Status, bool) {
	switch status {
	case recallStatusRejected:
		return assetop.StatusRejected, true
	case recallStatusAborted:
		return assetop.StatusAborted, true
	case recallStatusPartial:
		return assetop.StatusAppliedPartial, true
	default:
		return 0, false
	}
}

// RecallStatusLabel 是一行状态的低基数名字,给 RPC 的 error_detail 用。
func RecallStatusLabel(status uint32) string {
	name, ok := dbpb.GmRecallOpStatus_name[int32(status)]
	if !ok {
		return "unknown"
	}
	return strings.ToLower(strings.TrimPrefix(name, "GM_RECALL_OP_"))
}
