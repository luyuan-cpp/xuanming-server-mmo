package playerstore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"

	"db/internal/metrics"

	dbpb "proto/common/database"
	rollbackpb "proto/common/rollback"
	storepb "proto/player_store"
)

// 审计发件箱的写入端(docs/design/player-audit-outbox.md §3、§6,断线重连 R13)。
//
// scene 把这名玩家水位不大于这份快照的审计记录随存盘交来:流水原样,快照只是请求(号、触发、zone)。
// COMMITTED 时它们和脏行在同一个事务里写进本存档库的 player_audit_outbox,「发件箱里有这条」等于「它描述的改动已提交」。
// 转发循环(internal/auditoutbox)把发件箱投 Kafka、确认后删除。表声明在 proto/common/database/mysql_database_table.proto。

const (
	// AuditOutboxTable 是发件箱表名。
	AuditOutboxTable = "player_audit_outbox"
	// AuditOutboxShards 是每个存档库发件箱的片数,片号 = player_id % AuditOutboxShards(设计 §3)。
	// 转发循环按 (存档库, 片) 分工、按片扫描。片号写在主键里,改它要清表(项目未上线可以直接改)。
	AuditOutboxShards = 16

	// MaxAuditBytesPerSave 与 C++ AuditOutbox::kMaxBytesPerPlayer 同值(单测读头文件核对):scene 每名玩家排着的审计记录不超过它,
	// 所以一笔存盘总能带走全部到期记录,不必拆成两笔(设计 §5.1)。New 用它核行预算还装不装得下审计。
	MaxAuditBytesPerSave = 512 << 10
	// MaxAuditSnapshotBytes 是一份快照(序列化后的 PlayerSnapshotEntry)的上限:低于 TiDB 单 KV 6 MiB、
	// 快照 topic 的 max.message.bytes 16 MiB。超了只跳过这份快照,存盘照常提交(设计 §6.3)。
	MaxAuditSnapshotBytes = 4 << 20
	// MaxAuditTransactionLogBytes 是一条流水(序列化后的 TransactionLogEntry)的上限,与 C++ AuditOutbox::kMaxRecordBytes 同值
	// (单测读头文件核对)。scene 入队时已经按它拒收;这里再核一次,是因为一行进了发件箱就必须投得出去:流水 topic 用 broker
	// 缺省的约 1 MiB 上限,装不进一次请求的行会永远卡在发件箱里(设计 §6.3)。
	MaxAuditTransactionLogBytes = 64 << 10

	// maxSaveRequestBytes 与 C++ PlayerStoreClient::Options::maxRequestBytes 的缺省值相同:gRPC 单消息 4 MiB 减签名余量。
	// scene 发送前按它预检,超了就在本地按 TOO_LARGE 拒掉整笔存盘。
	maxSaveRequestBytes = 4<<20 - 64<<10
	// saveRequestOverheadBytes 给请求里 payload 与审计记录之外的内容(身份、行头、签名)留的余量。
	saveRequestOverheadBytes = 64 << 10

	// auditOutboxInsertBatch 是一条 INSERT 最多写的行数:每行 7 个占位符,远低于预编译语句 65535 个的上限。
	auditOutboxInsertBatch = 1000

	sqlAuditOutboxHead = "INSERT INTO player_audit_outbox (shard,kind,created_ms,player_id,record_id,save_seq,payload) VALUES "
	sqlAuditOutboxRow  = "(?,?,?,?,?,?,?)"
	// 同一笔请求里重复的记录撞主键时不报错。不用 INSERT IGNORE:它会把类型截断之类的错误也吞成警告。
	sqlAuditOutboxTail = " ON DUPLICATE KEY UPDATE record_id=record_id"

	sqlSelectAuditOutboxColumns = "SELECT COLUMN_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='player_audit_outbox'"
)

// requiredAuditOutboxColumns 是 player_audit_outbox 的全部列,要与 proto 的 message player_audit_outbox 一致。
var requiredAuditOutboxColumns = []string{"shard", "kind", "created_ms", "player_id", "record_id", "save_seq", "payload"}

// 跳过一条审计记录的原因(db_audit_outbox_records_skipped_total 的 reason,封闭集合,设计 §6.3)。
const (
	AuditSkipInvalid        = "invalid"          // 流水 tx_id 为 0,或快照请求 snapshot_id 为 0、玩家号对不上
	AuditSkipUnknownRowKind = "unknown_row_kind" // 这名玩家有本 go/db 不认识的行,拼出来会缺字段
	AuditSkipTooLarge       = "too_large"        // 拼出来的快照超过 MaxAuditSnapshotBytes,或流水超过 MaxAuditTransactionLogBytes
	AuditSkipUndecodable    = "undecodable"      // 行拼接后解不开(行被写坏)
)

// AuditSkipReasons 是全部跳过原因,指标预置用。
var AuditSkipReasons = []string{AuditSkipInvalid, AuditSkipUnknownRowKind, AuditSkipTooLarge, AuditSkipUndecodable}

// AuditShardOf 返回玩家在发件箱里的片号。
func AuditShardOf(playerID uint64) uint32 { return uint32(playerID % AuditOutboxShards) }

// AuditKindName 是 kind 在指标与日志里的名字。
func AuditKindName(kind storepb.PlayerAuditKind) string {
	switch kind {
	case storepb.PlayerAuditKind_PLAYER_AUDIT_KIND_TRANSACTION_LOG:
		return metrics.AuditKindTransactionLog
	case storepb.PlayerAuditKind_PLAYER_AUDIT_KIND_PLAYER_SNAPSHOT:
		return metrics.AuditKindPlayerSnapshot
	default:
		return "unknown"
	}
}

// auditRecord 是要写进发件箱的一行。
type auditRecord struct {
	kind     storepb.PlayerAuditKind
	recordID uint64
	payload  []byte
}

// auditWriteSummary 记下这一笔写了几条、跳过几条。只在提交成功之后计进指标:结果未知时 scene 会重发同一份请求,先计就重复了。
type auditWriteSummary struct {
	transactionLogs int
	snapshots       int
	skipped         map[string]int
}

func (s *auditWriteSummary) skip(reason string) {
	if s.skipped == nil {
		s.skipped = make(map[string]int, len(AuditSkipReasons))
	}
	s.skipped[reason]++
}

func (s auditWriteSummary) observe() {
	metrics.ObserveAuditOutboxWritten(metrics.AuditKindTransactionLog, s.transactionLogs)
	metrics.ObserveAuditOutboxWritten(metrics.AuditKindPlayerSnapshot, s.snapshots)
	for reason, n := range s.skipped {
		metrics.ObserveAuditOutboxSkipped(reason, n)
	}
}

// writeAuditOutbox 在 Save 的提交事务里写发件箱(设计 §6.1)。只在 COMMITTED 时调用,这一笔的脏行已经写进本事务。
// 返回错误 = 整笔结果未知:调用方回滚,scene 同内容同序号重试。坏的单条记录只跳过,不让存盘失败(设计 §6.3)。
func writeAuditOutbox(ctx context.Context, tx *sql.Tx, req *storepb.SavePlayerRequest, now time.Time) (auditWriteSummary, error) {
	var summary auditWriteSummary
	logs, snapshots := req.GetTransactionLogs(), req.GetAuditSnapshots()
	if len(logs) == 0 && len(snapshots) == 0 {
		return summary, nil
	}
	records := make([]auditRecord, 0, len(logs)+len(snapshots))
	for _, entry := range logs {
		if entry.GetTxId() == 0 {
			// scene 对 tx_id=0 是 fail-closed 的,消费端也会跳过;0 做记录号会让所有坏记录互撞。
			summary.skip(AuditSkipInvalid)
			logx.Errorf("[audit_outbox] transaction log with tx_id=0 in the save of player %d seq %d skipped", req.GetPlayerId(), req.GetSaveSeq())
			continue
		}
		payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(entry)
		if err != nil {
			summary.skip(AuditSkipInvalid)
			logx.Errorf("[audit_outbox] cannot marshal transaction log tx_id=%d of player %d: %v", entry.GetTxId(), req.GetPlayerId(), err)
			continue
		}
		if len(payload) > MaxAuditTransactionLogBytes {
			summary.skip(AuditSkipTooLarge)
			logx.Errorf("[audit_outbox] transaction log tx_id=%d of player %d is %d bytes, over the %d-byte limit; skipped",
				entry.GetTxId(), req.GetPlayerId(), len(payload), MaxAuditTransactionLogBytes)
			continue
		}
		records = append(records, auditRecord{kind: storepb.PlayerAuditKind_PLAYER_AUDIT_KIND_TRANSACTION_LOG, recordID: entry.GetTxId(), payload: payload})
		summary.transactionLogs++
	}
	if len(snapshots) > 0 {
		// 读已提交 + 同一事务:读得到本事务刚写的行,快照就是这一笔提交之后的完整存档。
		rows, err := readRows(ctx, tx, req.GetPlayerId())
		if err != nil {
			return summary, err
		}
		for _, request := range snapshots {
			payload, reason := buildAuditSnapshot(req, request, rows, now)
			if reason != "" {
				summary.skip(reason)
				logx.Errorf("[audit_outbox] snapshot_id=%d trigger=%s of player %d seq %d skipped: %s",
					request.GetSnapshotId(), request.GetTrigger(), req.GetPlayerId(), req.GetSaveSeq(), reason)
				continue
			}
			records = append(records, auditRecord{kind: storepb.PlayerAuditKind_PLAYER_AUDIT_KIND_PLAYER_SNAPSHOT, recordID: request.GetSnapshotId(), payload: payload})
			summary.snapshots++
		}
	}
	if err := insertAuditRecords(ctx, tx, req, records, now); err != nil {
		return summary, err
	}
	return summary, nil
}

// buildAuditSnapshot 按这名玩家写完这一笔之后的全部数据行拼出快照(设计 §6.2)。返回非空 reason 表示跳过。
//
// 数据行都是 player_database 按字段投影出来的片段,拼接后 Unmarshal 就是合并(和 scene 加载同一个做法),再填 player_id 与
// persist_schema_gen(这一笔请求的世代)。快照请求里的其余字段原样保留(包括本 go/db 不认识的新字段),
// player_database_blob、snapshot_time、total_bytes、player_id 由这里填。
func buildAuditSnapshot(req *storepb.SavePlayerRequest, request *rollbackpb.PlayerSnapshotEntry, rows []*storepb.PlayerStoreRow, now time.Time) ([]byte, string) {
	if request.GetSnapshotId() == 0 || (request.GetPlayerId() != 0 && request.GetPlayerId() != req.GetPlayerId()) {
		return nil, AuditSkipInvalid
	}
	var joined []byte
	for _, row := range rows {
		// 不认识的行(更新的 scene 写的,或退役号残留)按别的布局写成,拼进来会缺字段或串字段:宁可少一份快照。
		if _, known := storepb.PlayerStoreKind_name[int32(row.GetKind())]; !known {
			return nil, AuditSkipUnknownRowKind
		}
		joined = append(joined, row.GetPayload()...)
	}
	data := &dbpb.PlayerDatabase{}
	if err := proto.Unmarshal(joined, data); err != nil {
		return nil, AuditSkipUndecodable
	}
	data.PlayerId = req.GetPlayerId()
	data.PersistSchemaGen = req.GetSchemaGen()
	blob, err := proto.MarshalOptions{Deterministic: true}.Marshal(data)
	if err != nil {
		return nil, AuditSkipUndecodable
	}
	entry, ok := proto.Clone(request).(*rollbackpb.PlayerSnapshotEntry)
	if !ok {
		return nil, AuditSkipInvalid
	}
	entry.PlayerId = req.GetPlayerId()
	entry.SnapshotTime = uint64(now.Unix())
	entry.PlayerDatabaseBlob = blob
	// total_bytes 是整条消息的大小:先按 0 序列化量出大小,再带上它序列化一次(与 scene 原来的做法相同)。
	entry.TotalBytes = 0
	payload, err := proto.Marshal(entry)
	if err != nil {
		return nil, AuditSkipUndecodable
	}
	entry.TotalBytes = uint64(len(payload))
	if payload, err = proto.Marshal(entry); err != nil {
		return nil, AuditSkipUndecodable
	}
	if len(payload) > MaxAuditSnapshotBytes {
		return nil, AuditSkipTooLarge
	}
	return payload, ""
}

// insertAuditRecords 把记录写进发件箱,每条语句至多 auditOutboxInsertBatch 行。同一笔的记录片号、写入时刻、序号都相同。
func insertAuditRecords(ctx context.Context, tx *sql.Tx, req *storepb.SavePlayerRequest, records []auditRecord, now time.Time) error {
	shard := AuditShardOf(req.GetPlayerId())
	createdMs := uint64(now.UnixMilli())
	empty := []byte{}
	for start := 0; start < len(records); start += auditOutboxInsertBatch {
		batch := records[start:min(start+auditOutboxInsertBatch, len(records))]
		var query strings.Builder
		query.WriteString(sqlAuditOutboxHead)
		args := make([]any, 0, 7*len(batch))
		for i, record := range batch {
			if i > 0 {
				query.WriteByte(',')
			}
			query.WriteString(sqlAuditOutboxRow)
			payload := record.payload
			if payload == nil {
				payload = empty // nil 会被驱动写成 NULL,撞 NOT NULL
			}
			args = append(args, shard, int32(record.kind), createdMs, req.GetPlayerId(), record.recordID, req.GetSaveSeq(), payload)
		}
		query.WriteString(sqlAuditOutboxTail)
		if _, err := tx.ExecContext(ctx, query.String(), args...); err != nil {
			return fmt.Errorf("playerstore: write audit outbox of player %d: %w", req.GetPlayerId(), err)
		}
	}
	return nil
}

// verifyAuditOutboxSchema 核对发件箱表的列都在。理由同 VerifySchema:基线已跑过的库上 cmd/migrate 建不出新表,
// 缺表时带审计记录的存盘每次都以 1146 失败、scene 一直重试。
func verifyAuditOutboxSchema(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, sqlSelectAuditOutboxColumns)
	if err != nil {
		return fmt.Errorf("playerstore: read %s columns: %w", AuditOutboxTable, err)
	}
	defer rows.Close()
	have := make(map[string]struct{}, len(requiredAuditOutboxColumns))
	for rows.Next() {
		var column string
		if err := rows.Scan(&column); err != nil {
			return fmt.Errorf("playerstore: scan %s columns: %w", AuditOutboxTable, err)
		}
		have[strings.ToLower(column)] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("playerstore: read %s columns: %w", AuditOutboxTable, err)
	}
	var missing []string
	for _, column := range requiredAuditOutboxColumns {
		if _, ok := have[column]; !ok {
			missing = append(missing, column)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("playerstore: %s is missing columns %v; on a database whose baseline already ran, "+
			"cmd/migrate cannot create the table, rebuild the database (the project has not launched)", AuditOutboxTable, missing)
	}
	return nil
}

// checkAuditBudget 核行预算加上审计记录还装得下一笔存盘请求(设计 §6.4)。装不下时 scene 会在本地按 TOO_LARGE 拒掉整笔存盘。
func checkAuditBudget(limits Limits) error {
	if limits.MaxRequestBytes+MaxAuditBytesPerSave+saveRequestOverheadBytes > maxSaveRequestBytes {
		return fmt.Errorf("playerstore: MaxRequestBytes %d leaves no room for %d bytes of audit records within the %d-byte save request",
			limits.MaxRequestBytes, MaxAuditBytesPerSave, maxSaveRequestBytes)
	}
	return nil
}
