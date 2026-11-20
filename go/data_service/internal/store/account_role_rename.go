package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	base "proto/common/base"
	dspb "proto/data_service"
	"shared/accountdirectory"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

// 角色改名的目录一侧(docs/design/rename-and-content-safety.md §4)。
//
// 一次改名跨两个逻辑库:角色行、改名记录、名字副本在玩家片,新旧两条名字行在各自的名字片。没有跨片事务,所以分步做,
// 每一步只写一个逻辑库、各自幂等,事务里不调别的库:
//
//  1. 玩家片一个事务(beginRename):锁角色行,再锁改名记录。判这个 seq 是新的、重发还是迟到的;新的就在角色行锁之下判
//     冻结、同名、冷却,把结局连同 seq 记下(「进行中」或「已拒绝」)。被拒绝的 seq 也要记:否则同一个请求的迟到副本
//     以后条件变了又能提交,而 scene 早已把改名卡还给了玩家。
//  2. 名字片:预留新名(PlayerNameStore.ReserveForRename,只写名字行)。被别人占着就把记录置为「已拒绝」。
//  3. 玩家片一个事务(commitRename),**这是提交点**:角色行里的名字、名字副本换成新名,记录置为「已提交」,
//     记下这次成功的时刻(冷却从它算)和旧名保护的到期时刻。提交之前失败,角色还是旧名;提交之后角色就是新名。
//  4. 收尾(settleRename),可以重做:新名置为已绑定 → 旧名置为保护 → 删名字读缓存 → 记录置为「已收尾」。
//
// 改名记录每名玩家一行,只保留最近一次。它承担三件事:同一个 seq 重发回同一个结局;seq 比记录小的迟到请求一律拒绝;
// 上一次成功的时刻。seq 是 scene 按玩家递增发的,不用发号器的 ID(那种 ID 不能比先后)。
//
// 没收完的记录(进行中、已提交未收尾、已拒绝未清理)由后台 ResumeRenames 接着做;进行中太久没有结局的由它作废
// (scene 之后用同一个 seq 重发拿到 EXPIRED)。没有单独的中止接口:scene 拿不到结局就一直用同一个 seq 重发,
// 服务端的作废就是中止。
//
// 和删角、过户互斥(§4.6),判定都在角色行锁之下:改名开始时角色挂着冻结或准备令牌就拒绝;删角、过户开始时这名角色有
// 没收完的改名就回可重试(renameOpenTx)。不让两者交错,是因为删角按角色当前的名字释放名字行。

const AccountRoleRenameTableName = "account_role_rename"

// accountRoleRenameDDL 手写:名字两列要和 player_name 同列型、同排序规则。二级索引只给后台续做找没收完的记录用,
// 表里绝大多数行是收完的(状态 3、5),按 (state, updated_ms) 只扫得到没收完的那几行。
// 列注释 pb:N 等于 proto/common/database/rollback_database_table.proto 里 account_role_rename 的字段号。
const accountRoleRenameDDL = "CREATE TABLE IF NOT EXISTS `account_role_rename` (\n" +
	"  `player_id` bigint unsigned NOT NULL COMMENT 'pb:1',\n" +
	"  `seq` bigint unsigned NOT NULL COMMENT 'pb:2',\n" +
	"  `state` int unsigned NOT NULL COMMENT 'pb:3',\n" +
	"  `result` int unsigned NOT NULL DEFAULT 0 COMMENT 'pb:4',\n" +
	"  `new_name` VARCHAR(64) NOT NULL DEFAULT '' COMMENT 'pb:5',\n" +
	"  `new_norm` VARCHAR(191) NOT NULL DEFAULT '' COMMENT 'pb:6',\n" +
	"  `old_norm` VARCHAR(191) NOT NULL DEFAULT '' COMMENT 'pb:7',\n" +
	"  `renamed_ms` bigint unsigned NOT NULL DEFAULT 0 COMMENT 'pb:8',\n" +
	"  `held_until_ms` bigint unsigned NOT NULL DEFAULT 0 COMMENT 'pb:9',\n" +
	"  `updated_ms` bigint unsigned NOT NULL DEFAULT 0 COMMENT 'pb:10',\n" +
	"  PRIMARY KEY (`player_id`) /*T![clustered_index] NONCLUSTERED */,\n" +
	"  KEY `idx_account_role_rename_open` (`state`, `updated_ms`)\n" +
	") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin" +
	" /*T! SHARD_ROW_ID_BITS=4 PRE_SPLIT_REGIONS=4 */ COMMENT='account_role_rename';"

// 改名记录的状态(account_role_rename.state)。1、2、4 是没收完的,后台按它们找。
const (
	renamePending       uint32 = 1 // 进行中:seq 已登记,还没到提交点
	renameCommitted     uint32 = 2 // 已提交:角色已是新名,名字片的收尾还没做完
	renameSettled       uint32 = 3 // 已收尾
	renameRejectedDirty uint32 = 4 // 已拒绝,预留的新名可能还在名字片上,等清理
	renameRejected      uint32 = 5 // 已拒绝
)

const (
	// renameSettleTimeout 是提交之后收尾的预算。不跟请求 ctx:角色已经改名,调用方的截止可能已过,收尾还是要尽量做完;
	// 没做完由后台接着做。
	renameSettleTimeout = 2 * time.Second

	renameRecordColumns = "seq,state,result,new_name,new_norm,old_norm,renamed_ms,held_until_ms"

	// renameUpsertSQL 登记一个新的 seq,覆盖这名玩家的上一条记录。renamed_ms 不在更新列里:上一次成功的时刻要留着算冷却。
	// 绑参:(player_id, seq, state, result, new_name, new_norm, now)。
	renameUpsertSQL = "INSERT INTO account_role_rename(player_id,seq,state,result,new_name,new_norm,old_norm,renamed_ms,held_until_ms,updated_ms)" +
		" VALUES (?,?,?,?,?,?,'',0,0,?) ON DUPLICATE KEY UPDATE seq=VALUES(seq),state=VALUES(state),result=VALUES(result)," +
		"new_name=VALUES(new_name),new_norm=VALUES(new_norm),old_norm='',held_until_ms=0,updated_ms=VALUES(updated_ms)"

	// renameOwnerOverwriteSQL 在提交点把名字副本换成新名。副本只是按玩家读名字的索引,真相是名字行,所以直接覆盖。
	renameOwnerOverwriteSQL = "INSERT INTO player_name_owner (player_id, name_norm, name) VALUES (?, ?, ?)" +
		" ON DUPLICATE KEY UPDATE name_norm = VALUES(name_norm), name = VALUES(name)"
)

// renameNames 是改名要的几步名字操作,*PlayerNameStore 实现。和 roleNameRegistry 分开,是为了不让只测建角、删角的替身跟着加方法。
type renameNames interface {
	ReserveForRename(ctx context.Context, playerID uint64, name, norm string, nowMs uint64) (ReserveOutcome, uint64, error)
	Bind(ctx context.Context, playerID uint64, name, norm string, nowMs uint64) (BindOutcome, uint64, error)
	HoldRenamed(ctx context.Context, playerID uint64, norm string, untilMs, nowMs uint64) error
	Release(ctx context.Context, playerID uint64, norm string, minCreatedMs uint64) (ReleaseOutcome, error)
}

// RenameRequest 是一次改名请求。Display / Norm 来自同一次 playername.Normalize;名字不合规时 Invalid 为真、两者为空
// (仍然要把这个 seq 记成已拒绝)。Cooldown / Hold 是调用方带来的规则数值(配表 RoleNameRule 在 scene)。
type RenameRequest struct {
	Player, Seq    uint64
	Display, Norm  string
	Invalid        bool
	Cooldown, Hold time.Duration
}

// RenameOutcome 是一次改名请求的答案,字段含义同 dspb.RenameRoleResponse。
type RenameOutcome struct {
	Result        dspb.RenameRoleResult
	CurrentSeq    uint64
	Name          string
	RenamedMs     uint64
	NextAllowedMs uint64
}

type renameRecord struct {
	seq         uint64
	state       uint32
	result      uint32
	newName     string
	newNorm     string
	oldNorm     string
	renamedMs   uint64
	heldUntilMs uint64
}

func renameStateOpen(state uint32) bool {
	return state == renamePending || state == renameCommitted || state == renameRejectedDirty
}

func scanRenameRecord(row *sql.Row) (*renameRecord, error) {
	var r renameRecord
	err := row.Scan(&r.seq, &r.state, &r.result, &r.newName, &r.newNorm, &r.oldNorm, &r.renamedMs, &r.heldUntilMs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// lockRenameRecord 锁这名玩家的改名记录;没有记录回 nil。
func lockRenameRecord(ctx context.Context, tx *sql.Tx, player uint64) (*renameRecord, error) {
	return scanRenameRecord(tx.QueryRowContext(ctx, "SELECT "+renameRecordColumns+" FROM account_role_rename WHERE player_id=? FOR UPDATE", player))
}

func (s *AccountDirectoryStore) readRenameRecord(ctx context.Context, player uint64) (*renameRecord, error) {
	return scanRenameRecord(s.shards.forPlayer(player).QueryRowContext(ctx, "SELECT "+renameRecordColumns+" FROM account_role_rename WHERE player_id=?", player))
}

// renameOpenTx 报告这名角色有没有没收完的改名。在调用方锁着角色行的事务里调(删角、过户的第 1 步):
// 改名的开始和提交都要先拿角色行锁,所以这里读到「没有」之后它们只能排在本事务之后,届时会看到本事务写下的令牌而拒绝。
// 只读不锁:别的写者只会把记录从没收完改成收完,读旧了最多多回一次可重试。
func renameOpenTx(ctx context.Context, tx *sql.Tx, player uint64) (bool, error) {
	var state uint32
	err := tx.QueryRowContext(ctx, "SELECT state FROM account_role_rename WHERE player_id=?", player).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return renameStateOpen(state), nil
}

// renameStep 是 beginRename 之后该做什么。
type renameStep uint8

const (
	renameStepDone    renameStep = iota + 1 // 答案已经有了
	renameStepProceed                       // 记录是本次的「进行中」:接着预留新名、提交
	renameStepSettle                        // 本次已提交、还没收尾:答案是成功,顺手再收尾一次
	renameStepCleanup                       // 本次已被拒绝、预留的新名可能还在:清一遍再回答案
	renameStepResolve                       // 上一次改名还没收完:先把它做完再重来
)

// replayRename 是同一个 seq 重发时的答案:只看记录,不重新判定。
func replayRename(rec *renameRecord, cooldown time.Duration) (renameStep, RenameOutcome) {
	out := RenameOutcome{CurrentSeq: rec.seq}
	switch rec.state {
	case renamePending:
		return renameStepProceed, out
	case renameCommitted, renameSettled:
		out.Result, out.Name, out.RenamedMs = dspb.RenameRoleResult_RENAME_ROLE_OK, rec.newName, rec.renamedMs
		if rec.state == renameCommitted {
			return renameStepSettle, out
		}
		return renameStepDone, out
	default:
		out.Result = dspb.RenameRoleResult(rec.result)
		if out.Result == dspb.RenameRoleResult_RENAME_ROLE_COOLDOWN {
			out.NextAllowedMs = rec.renamedMs + uint64(cooldown.Milliseconds())
		}
		if rec.state == renameRejectedDirty {
			return renameStepCleanup, out
		}
		return renameStepDone, out
	}
}

// RenameRole 处理一次改名请求。回错误表示这一次没有结局(库不可用、结局未知),调用方用同一个 seq 重发;
// 其余情况答案在 RenameOutcome.Result 里(BUSY 同样表示稍后用同一个 seq 重发)。
func (s *AccountDirectoryStore) RenameRole(ctx context.Context, req RenameRequest) (RenameOutcome, error) {
	if req.Player == 0 || req.Seq == 0 || req.Cooldown < 0 || req.Hold < 0 || (req.Invalid != (req.Norm == "")) || (req.Invalid != (req.Display == "")) {
		return RenameOutcome{}, accountdirectory.ErrInvalid
	}
	if s.renames == nil {
		return RenameOutcome{}, accountdirectory.ErrUnavailable
	}
	for attempt := 1; ; attempt++ {
		step, out, err := s.beginRename(ctx, req)
		if err != nil {
			return RenameOutcome{}, err
		}
		switch step {
		case renameStepDone:
			return out, nil
		case renameStepSettle:
			s.settleRenameBestEffort(ctx, req.Player, req.Seq)
			return out, nil
		case renameStepCleanup:
			s.cleanupRejectedRenameBestEffort(ctx, req.Player, req.Seq)
			return out, nil
		case renameStepResolve:
			// 上一次改名还没收完(scene 的存档回过档,或者上一次的收尾卡住了):接着做一遍再重来;还是没收完就让调用方稍后重发。
			if attempt >= 2 {
				out.Result = dspb.RenameRoleResult_RENAME_ROLE_BUSY
				return out, nil
			}
			if err := s.resolveRename(ctx, req.Player); err != nil {
				logx.Errorf("[role-rename] previous rename not resolved yet, the caller retries: player=%d: %v", req.Player, err)
				out.Result = dspb.RenameRoleResult_RENAME_ROLE_BUSY
				return out, nil
			}
			continue
		}
		break
	}

	nowMs := uint64(time.Now().UnixMilli())
	reserved, _, err := s.renames.ReserveForRename(ctx, req.Player, req.Display, req.Norm, nowMs)
	if err != nil {
		return RenameOutcome{}, fmt.Errorf("reserve the new name of player %d: %w", req.Player, err)
	}
	if reserved == ReserveTaken {
		return s.rejectPendingRename(ctx, req, dspb.RenameRoleResult_RENAME_ROLE_TAKEN)
	}

	out, err := s.commitRename(ctx, req)
	if err != nil {
		return RenameOutcome{}, err
	}
	if out.Result != dspb.RenameRoleResult_RENAME_ROLE_OK {
		// 预留期间记录不再是本次的「进行中」:这一次不提交。
		if out.Result == dspb.RenameRoleResult_RENAME_ROLE_STALE_SEQ {
			// 更大的 seq 先到了。它开始之前已经把本次作废并清过预留行;同一个名字现在可能是它的预留,不能由本次去删。
			return out, nil
		}
		// 被后台作废(记录还是本次的):后台清理可能跑在本次预留之前,所以刚才预留的新名自己放掉。
		// Release 只删预留态,已经绑定的、还在保护期内的都碰不到;没删成由核账工具报告。
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), renameSettleTimeout)
		defer cancel()
		if _, releaseErr := s.renames.Release(releaseCtx, req.Player, req.Norm, 0); releaseErr != nil {
			logx.Errorf("[role-rename] reserved name of an abandoned rename is not released, create_residual reports it: player=%d seq=%d: %v",
				req.Player, req.Seq, releaseErr)
		}
		return out, nil
	}
	s.settleRenameBestEffort(ctx, req.Player, req.Seq)
	return out, nil
}

// beginRename 是第 1 步(见文件头)。
func (s *AccountDirectoryStore) beginRename(ctx context.Context, req RenameRequest) (renameStep, RenameOutcome, error) {
	var step renameStep
	var out RenameOutcome
	err := s.playerTx(ctx, req.Player, func(tx *sql.Tx) error {
		step, out = renameStepDone, RenameOutcome{}
		owned, _, err := lockFencedOwnership(ctx, tx, req.Player)
		if errors.Is(err, accountdirectory.ErrMissing) || (err == nil && owned.Account == "") {
			out.Result = dspb.RenameRoleResult_RENAME_ROLE_NOT_FOUND
			return nil
		}
		if err != nil {
			return err
		}
		rec, err := lockRenameRecord(ctx, tx, req.Player)
		if err != nil {
			return err
		}
		var renamedMs uint64
		if rec != nil {
			renamedMs = rec.renamedMs
			switch {
			case rec.seq > req.Seq, rec.seq == req.Seq && rec.newNorm != req.Norm:
				// 迟到的请求,或者同一个 seq 换了名字(scene 的存档回过档又发了一次):这个 seq 作废。
				out.Result, out.CurrentSeq = dspb.RenameRoleResult_RENAME_ROLE_STALE_SEQ, rec.seq
				return nil
			case rec.seq == req.Seq:
				step, out = replayRename(rec, req.Cooldown)
				return nil
			case renameStateOpen(rec.state):
				step, out.CurrentSeq = renameStepResolve, rec.seq
				return nil
			}
		}
		var now uint64
		if err := tx.QueryRowContext(ctx, "SELECT "+dbNowMsSQL).Scan(&now); err != nil {
			return err
		}
		out.CurrentSeq = req.Seq
		current, _ := residualNameKeyOf(owned.GetRole().GetName())
		cooldownMs := uint64(req.Cooldown.Milliseconds())
		result := dspb.RenameRoleResult_RENAME_ROLE_RESULT_UNSPECIFIED
		switch {
		case req.Invalid:
			result = dspb.RenameRoleResult_RENAME_ROLE_INVALID
		case owned.TradeListingId != 0 || owned.PrepareToken != 0:
			result = dspb.RenameRoleResult_RENAME_ROLE_FROZEN
		case current.norm == req.Norm:
			result = dspb.RenameRoleResult_RENAME_ROLE_SAME_NAME
		case renamedMs != 0 && now < renamedMs+cooldownMs:
			result = dspb.RenameRoleResult_RENAME_ROLE_COOLDOWN
			out.NextAllowedMs = renamedMs + cooldownMs
		}
		state := renamePending
		if result != dspb.RenameRoleResult_RENAME_ROLE_RESULT_UNSPECIFIED {
			state = renameRejected
		}
		if _, err := tx.ExecContext(ctx, renameUpsertSQL, req.Player, req.Seq, state, uint32(result), req.Display, req.Norm, now); err != nil {
			return err
		}
		if state == renamePending {
			step = renameStepProceed
		} else {
			out.Result = result
		}
		return nil
	})
	if err != nil {
		return 0, RenameOutcome{}, err
	}
	return step, out, nil
}

// lockThisRename 锁改名记录并核它还是本次请求的那一条。回 nil 记录表示不是了,答案已经写进 out。
func lockThisRename(ctx context.Context, tx *sql.Tx, req RenameRequest, out *RenameOutcome) (*renameRecord, error) {
	rec, err := lockRenameRecord(ctx, tx, req.Player)
	if err != nil {
		return nil, err
	}
	if rec == nil || rec.seq != req.Seq || rec.newNorm != req.Norm {
		out.Result = dspb.RenameRoleResult_RENAME_ROLE_STALE_SEQ
		if rec != nil {
			out.CurrentSeq = rec.seq
		}
		return nil, nil
	}
	if rec.state != renamePending {
		_, *out = replayRename(rec, req.Cooldown)
		return nil, nil
	}
	return rec, nil
}

// rejectPendingRename 把本次的「进行中」置为已拒绝(新名被别人占着:名字片上没有本角色的预留,不用清理)。
func (s *AccountDirectoryStore) rejectPendingRename(ctx context.Context, req RenameRequest, result dspb.RenameRoleResult) (RenameOutcome, error) {
	var out RenameOutcome
	err := s.playerTx(ctx, req.Player, func(tx *sql.Tx) error {
		out = RenameOutcome{CurrentSeq: req.Seq}
		rec, err := lockThisRename(ctx, tx, req, &out)
		if err != nil || rec == nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE account_role_rename SET state=?,result=?,updated_ms="+dbNowMsSQL+" WHERE player_id=? AND seq=? AND state=?",
			renameRejected, uint32(result), req.Player, req.Seq, renamePending); err != nil {
			return err
		}
		out.Result = result
		return nil
	})
	if err != nil {
		return RenameOutcome{}, err
	}
	return out, nil
}

// commitRename 是提交点(见文件头第 3 步)。回的 Result 不是 OK 表示记录已经不是本次的「进行中」,什么都没改。
func (s *AccountDirectoryStore) commitRename(ctx context.Context, req RenameRequest) (RenameOutcome, error) {
	var out RenameOutcome
	err := s.playerTx(ctx, req.Player, func(tx *sql.Tx) error {
		out = RenameOutcome{CurrentSeq: req.Seq}
		owned, _, err := lockFencedOwnership(ctx, tx, req.Player)
		if err != nil {
			return err
		}
		rec, err := lockThisRename(ctx, tx, req, &out)
		if err != nil || rec == nil {
			return err
		}
		if owned.Account == "" || owned.TradeListingId != 0 || owned.PrepareToken != 0 {
			// 删角、过户开始时会看改名记录,进行中的改名挡着它们。走到这里说明互斥被破坏:不提交,记录留给后台作废。
			return fmt.Errorf("role %d is removed or frozen while its rename is pending", req.Player)
		}
		old, _ := residualNameKeyOf(owned.GetRole().GetName())
		role, ok := proto.Clone(owned.GetRole()).(*base.AccountSimplePlayer)
		if !ok || role == nil {
			return fmt.Errorf("role %d has no role record to rename", req.Player)
		}
		role.Name = req.Display
		raw, err := proto.Marshal(role)
		if err != nil {
			return err
		}
		var now uint64
		if err := tx.QueryRowContext(ctx, "SELECT "+dbNowMsSQL).Scan(&now); err != nil {
			return err
		}
		// 名字不属于护栏状态:不动 owner_revision 和 fence_version,在线的会话、入场登记都不受影响。
		if _, err := tx.ExecContext(ctx, "UPDATE account_role SET role_proto=?,updated_ms=? WHERE player_id=?", raw, now, req.Player); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, renameOwnerOverwriteSQL, req.Player, req.Norm, req.Display); err != nil {
			return err
		}
		// 保护到期时刻非 0(时长配成 0 时就是现在,等于马上可以被接管):名字行靠它非 0 认保护态。
		heldUntil := now + uint64(req.Hold.Milliseconds())
		if _, err := tx.ExecContext(ctx, "UPDATE account_role_rename SET state=?,old_norm=?,renamed_ms=?,held_until_ms=?,updated_ms=? WHERE player_id=? AND seq=? AND state=?",
			renameCommitted, old.norm, now, heldUntil, now, req.Player, req.Seq, renamePending); err != nil {
			return err
		}
		out.Result, out.Name, out.RenamedMs = dspb.RenameRoleResult_RENAME_ROLE_OK, req.Display, now
		return nil
	})
	if err != nil {
		return RenameOutcome{}, err
	}
	return out, nil
}

func (s *AccountDirectoryStore) settleRenameBestEffort(ctx context.Context, player, seq uint64) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), renameSettleTimeout)
	defer cancel()
	if err := s.settleRename(ctx, player, seq); err != nil {
		logx.Errorf("[role-rename] rename committed but not settled yet, the rename resumer retries: player=%d seq=%d: %v", player, seq, err)
	}
}

// settleRename 是提交之后的收尾(见文件头第 4 步)。每一步都幂等,整段可以重做;记录不是「已提交」时什么都不做。
// 收尾做完之前,新名行还是预留态、旧名行还是已绑定,两个名字都在本角色名下,别人拿不到。
func (s *AccountDirectoryStore) settleRename(ctx context.Context, player, seq uint64) error {
	rec, err := s.readRenameRecord(ctx, player)
	if err != nil {
		return err
	}
	if rec == nil || rec.seq != seq || rec.state != renameCommitted {
		return nil
	}
	nowMs := uint64(time.Now().UnixMilli())
	outcome, holder, err := s.renames.Bind(ctx, player, rec.newName, rec.newNorm, nowMs)
	if err != nil {
		return fmt.Errorf("bind the new name: %w", err)
	}
	switch outcome {
	case BindBound, BindAlreadyBound:
	case BindRestored:
		// 预留行在提交之前就没了(被误释放,或者恢复点错开)。现在补回来了,但中间那段时间别人能登记同一个名字。
		logx.Errorf("[role-rename] renamed role had lost the registration of its new name, re-registered and bound it: player=%d", player)
	default:
		return fmt.Errorf("bind the new name: outcome=%s holder=%d", outcome, holder)
	}
	if rec.oldNorm != "" && rec.oldNorm != rec.newNorm {
		if err := s.renames.HoldRenamed(ctx, player, rec.oldNorm, rec.heldUntilMs, nowMs); err != nil {
			return fmt.Errorf("hold the old name: %w", err)
		}
	}
	// 按玩家查名字的缓存还是旧名:删掉,下一次读回源拿到新名(删失败只记日志,最多按正缓存 TTL 再展示一阵旧名)。
	s.invalidateNameCache(ctx, player)
	return s.playerTx(ctx, player, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE account_role_rename SET state=?,updated_ms="+dbNowMsSQL+" WHERE player_id=? AND seq=? AND state=?",
			renameSettled, player, seq, renameCommitted)
		return err
	})
}

func (s *AccountDirectoryStore) cleanupRejectedRenameBestEffort(ctx context.Context, player, seq uint64) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), renameSettleTimeout)
	defer cancel()
	if err := s.cleanupRejectedRename(ctx, player, seq); err != nil {
		logx.Errorf("[role-rename] reserved name of a rejected rename is not released yet, the rename resumer retries: player=%d seq=%d: %v", player, seq, err)
	}
}

// cleanupRejectedRename 删掉一次已拒绝的改名在名字片上留下的预留行,再把记录置为「已拒绝」。记录不是「已拒绝、未清理」时什么都不做。
// Release 只删本角色名下预留态的那一行:已绑定的、还在保护期内的都碰不到;这时名字副本还指着现名,按名字删副本删不到。
func (s *AccountDirectoryStore) cleanupRejectedRename(ctx context.Context, player, seq uint64) error {
	rec, err := s.readRenameRecord(ctx, player)
	if err != nil {
		return err
	}
	if rec == nil || rec.seq != seq || rec.state != renameRejectedDirty {
		return nil
	}
	if rec.newNorm != "" {
		if _, err := s.renames.Release(ctx, player, rec.newNorm, 0); err != nil {
			return fmt.Errorf("release the reserved name: %w", err)
		}
	}
	return s.playerTx(ctx, player, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE account_role_rename SET state=?,updated_ms="+dbNowMsSQL+" WHERE player_id=? AND seq=? AND state=?",
			renameRejected, player, seq, renameRejectedDirty)
		return err
	})
}

// expireRename 把一条「进行中」作废:置为「已拒绝、未清理」,结局 EXPIRED。和提交抢同一行,改到了这次改名就再也提交不了。
func (s *AccountDirectoryStore) expireRename(ctx context.Context, player, seq uint64) error {
	return s.playerTx(ctx, player, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE account_role_rename SET state=?,result=?,updated_ms="+dbNowMsSQL+" WHERE player_id=? AND seq=? AND state=?",
			renameRejectedDirty, uint32(dspb.RenameRoleResult_RENAME_ROLE_EXPIRED), player, seq, renamePending)
		return err
	})
}

// resolveRename 把这名玩家没收完的那一条做完:已提交的收尾,已拒绝的清理,进行中的作废再清理。
// 进行中也作废,是因为调用方手里已经是更大的 seq:scene 只在上一次有了结局(或者存档回过档)之后才发新的,旧的不会再有人来要结局。
func (s *AccountDirectoryStore) resolveRename(ctx context.Context, player uint64) error {
	rec, err := s.readRenameRecord(ctx, player)
	if err != nil || rec == nil {
		return err
	}
	switch rec.state {
	case renameCommitted:
		return s.settleRename(ctx, player, rec.seq)
	case renamePending:
		if err := s.expireRename(ctx, player, rec.seq); err != nil {
			return err
		}
		return s.cleanupRejectedRename(ctx, player, rec.seq)
	case renameRejectedDirty:
		return s.cleanupRejectedRename(ctx, player, rec.seq)
	}
	return nil
}

// RenameResumeResult 是一轮后台续做的结果。Failed 留在表里,下一轮再试。
//   - Settled:已提交的改名收尾做完;
//   - Cleaned:已拒绝的改名留下的预留行清掉了;
//   - Expired:进行中太久没有结局的改名被作废(并清掉预留行);
//   - Failed:这一轮没做下去(名字片不可达、库出错)。
type RenameResumeResult struct {
	Settled, Cleaned, Expired, Failed int
}

// ResumeRenames 把没收完的改名接着做。olderThan 之内刚动过的不碰(前台请求自己会收尾);进行中的要超过 pendingStaleAfter
// 才作废,它必须远大于 scene 重发一轮的间隔和单次请求的时限,否则会把还在路上的改名作废掉。
// 每轮最多 limit 行,逐片读,每轮换一个起点。多副本同时跑靠各步的条件更新串行,重复做是幂等的。
func (s *AccountDirectoryStore) ResumeRenames(ctx context.Context, olderThan, pendingStaleAfter time.Duration, limit int) (RenameResumeResult, error) {
	var out RenameResumeResult
	if s.renames == nil {
		return out, accountdirectory.ErrUnavailable
	}
	k := s.shards.k
	start := int(s.renameCursor.Add(1) % uint64(k))
	var failure error
	for i := 0; i < k && limit > 0; i++ {
		open, err := readOpenRenames(ctx, s.shards.pools[(start+i)%k], olderThan, limit)
		if err != nil {
			return out, errors.Join(failure, err)
		}
		limit -= len(open)
		for _, r := range open {
			var err error
			switch r.state {
			case renameCommitted:
				if err = s.settleRename(ctx, r.player, r.seq); err == nil {
					out.Settled++
				}
			case renameRejectedDirty:
				if err = s.cleanupRejectedRename(ctx, r.player, r.seq); err == nil {
					out.Cleaned++
				}
			case renamePending:
				if r.ageMs < uint64(pendingStaleAfter.Milliseconds()) {
					continue
				}
				if err = s.expireRename(ctx, r.player, r.seq); err == nil {
					err = s.cleanupRejectedRename(ctx, r.player, r.seq)
				}
				if err == nil {
					out.Expired++
				}
			}
			if err != nil {
				out.Failed++
				failure = errors.Join(failure, fmt.Errorf("player %d seq %d: %w", r.player, r.seq, err))
			}
		}
	}
	return out, failure
}

type openRename struct {
	player, seq uint64
	state       uint32
	ageMs       uint64
}

func readOpenRenames(ctx context.Context, db *sql.DB, olderThan time.Duration, limit int) ([]openRename, error) {
	rows, err := db.QueryContext(ctx, "SELECT player_id,seq,state,"+dbNowMsSQL+"-updated_ms FROM account_role_rename"+
		" WHERE state IN (?,?,?) AND updated_ms+?<"+dbNowMsSQL+" ORDER BY updated_ms LIMIT ?",
		renamePending, renameCommitted, renameRejectedDirty, olderThan.Milliseconds(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []openRename
	for rows.Next() {
		var r openRename
		if err := rows.Scan(&r.player, &r.seq, &r.state, &r.ageMs); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
