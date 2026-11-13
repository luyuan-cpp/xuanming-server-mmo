package authority

import (
	"context"
	"database/sql"
	"errors"

	battlepb "proto/battle"
	pb "proto/match"
)

// 战斗剧本的存储(docs/design/battle-script.md §6)。match 是剧本的唯一写者,三张表都在本库:
//
//	match_battle_script          玩家剧本:当前提交版 + 审核通过版两份正文
//	match_battle_script_review   审核流水:与剧本行的状态变更同一事务写入,只增不改
//	match_battle_script_loadout  玩家的选用:(玩家, 模式, 副本) → 官方剧本 id 或本人剧本 id
//
// 并发:玩家的写入(提交、删除、选用)先锁本人的 role_fence,同一玩家的写入串行,份数与条目上限在锁内数,
// 不会并发超限;剧本行再加行锁(FOR UPDATE),与审核按 (revision, 待审) 做的 CAS 串行。
// 不变量:选用里的玩家剧本一定存在、属于本人、至少通过过一次审核 —— 删除剧本与清掉指向它的选用在同一事务里。
// 事务里不调任何 RPC(新建剧本的 id 由调用方在事务外向发号器领好)。

var (
	// ErrBattleScriptNotFound 剧本不存在、已删除或不是本人的(不区分,不暴露别人的剧本是否存在)
	ErrBattleScriptNotFound = errors.New("battle script not found")
	// ErrBattleScriptRevision expected_revision 与当前版本不一致
	ErrBattleScriptRevision = errors.New("battle script revision conflict")
	// ErrBattleScriptLimit 本人的剧本份数已达上限
	ErrBattleScriptLimit = errors.New("battle script limit reached")
	// ErrBattleScriptNotApproved 选用的剧本从未通过审核
	ErrBattleScriptNotApproved = errors.New("battle script not approved")
	// ErrBattleScriptLoadoutFull 本人的选用条目已达上限
	ErrBattleScriptLoadoutFull = errors.New("battle script loadout full")
	// ErrBattleScriptReviewStale 审核的这一版已不是当前待审版(作者改了、删了,或已被审过)
	ErrBattleScriptReviewStale = errors.New("battle script review is stale")
	// ErrBattleScriptReviewInvalid 要通过的这一版按当前配表已不合法(配表改过),只能驳回
	ErrBattleScriptReviewInvalid = errors.New("battle script no longer validates")
)

const battleScriptColumns = "script_id,owner_player_id,revision,content,review_state,approved_revision,approved_content,reject_reason,submitted_ms,reviewed_ms"

const battleScriptLoadoutColumns = "player_id,match_mode,battle_config_id,origin,official_script_id,player_script_id,updated_ms"

var (
	reviewPending  = uint32(pb.BattleScriptReviewState_BATTLE_SCRIPT_REVIEW_PENDING)
	reviewApproved = uint32(pb.BattleScriptReviewState_BATTLE_SCRIPT_REVIEW_APPROVED)
	reviewRejected = uint32(pb.BattleScriptReviewState_BATTLE_SCRIPT_REVIEW_REJECTED)
)

func scanBattleScript(row scanner) (*pb.MatchBattleScriptRecord, error) {
	r := &pb.MatchBattleScriptRecord{}
	if err := row.Scan(&r.ScriptId, &r.OwnerPlayerId, &r.Revision, &r.Content, &r.ReviewState, &r.ApprovedRevision,
		&r.ApprovedContent, &r.RejectReason, &r.SubmittedMs, &r.ReviewedMs); err != nil {
		return nil, err
	}
	return r, nil
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func queryBattleScripts(ctx context.Context, q queryer, query string, args ...any) ([]*pb.MatchBattleScriptRecord, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pb.MatchBattleScriptRecord
	for rows.Next() {
		r, err := scanBattleScript(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func queryBattleScriptLoadout(ctx context.Context, q queryer, player uint64) ([]*pb.MatchBattleScriptLoadoutRecord, error) {
	rows, err := q.QueryContext(ctx, "SELECT "+battleScriptLoadoutColumns+
		" FROM match_battle_script_loadout WHERE player_id=? ORDER BY match_mode,battle_config_id", player)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*pb.MatchBattleScriptLoadoutRecord
	for rows.Next() {
		r := &pb.MatchBattleScriptLoadoutRecord{}
		if err = rows.Scan(&r.PlayerId, &r.MatchMode, &r.BattleConfigId, &r.Origin, &r.OfficialScriptId,
			&r.PlayerScriptId, &r.UpdatedMs); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// BattleScriptsOf 本人的全部剧本,新提交的在前。只读,不进事务(列表是展示数据,以后续写入的 CAS 为准)
func (s *Store) BattleScriptsOf(ctx context.Context, player uint64) ([]*pb.MatchBattleScriptRecord, error) {
	return queryBattleScripts(ctx, s.DB, "SELECT "+battleScriptColumns+
		" FROM match_battle_script WHERE owner_player_id=? ORDER BY submitted_ms DESC,script_id", player)
}

// BattleScriptLoadout 本人的全部选用,按 (match_mode, battle_config_id) 升序。只读
func (s *Store) BattleScriptLoadout(ctx context.Context, player uint64) ([]*pb.MatchBattleScriptLoadoutRecord, error) {
	return queryBattleScriptLoadout(ctx, s.DB, player)
}

// ApprovedBattleScript 建房时读一份玩家剧本的审核通过版。只认 owner 本人的剧本(选用只能指向本人的),
// 不存在 / 不是本人的回 ErrBattleScriptNotFound,从未通过回 ErrBattleScriptNotApproved
func (s *Store) ApprovedBattleScript(ctx context.Context, scriptID, owner uint64) (uint32, []byte, error) {
	var (
		actualOwner uint64
		revision    uint32
		content     []byte
	)
	err := s.DB.QueryRowContext(ctx, "SELECT owner_player_id,approved_revision,approved_content FROM match_battle_script WHERE script_id=?",
		scriptID).Scan(&actualOwner, &revision, &content)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && actualOwner != owner) {
		return 0, nil, ErrBattleScriptNotFound
	}
	if err != nil {
		return 0, nil, err
	}
	if revision == 0 {
		return 0, nil, ErrBattleScriptNotApproved
	}
	return revision, content, nil
}

// BattleScriptSubmit 是一次提交:ScriptID 为 0 表示新建,此时 NewScriptID 必须是调用方在事务外向发号器领好的 id
type BattleScriptSubmit struct {
	Player           uint64
	ScriptID         uint64
	NewScriptID      uint64
	ExpectedRevision uint32
	Content          []byte // 确定性序列化、已校验的 battle.BattleScript
	MaxScripts       uint32
}

// SubmitBattleScript 存一版新剧本并置为待审。新建时数本人份数(锁内,不会并发超限);
// 改写时按 expected_revision 做 CAS,通过版(approved_*)不动 —— 审核通过前对局继续用上一份通过版
func (s *Store) SubmitBattleScript(ctx context.Context, in BattleScriptSubmit) (*pb.MatchBattleScriptRecord, error) {
	var out *pb.MatchBattleScriptRecord
	err := s.Fenced(ctx, func() error {
		record, err := s.submitBattleScriptOnce(ctx, in)
		out = record
		return err
	})
	return out, err
}

func (s *Store) submitBattleScriptOnce(ctx context.Context, in BattleScriptSubmit) (*pb.MatchBattleScriptRecord, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = s.lockActor(ctx, tx, in.Player); err != nil {
		return nil, err
	}
	n, err := now(ctx, tx)
	if err != nil {
		return nil, err
	}

	if in.ScriptID == 0 {
		var count uint32
		if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM match_battle_script WHERE owner_player_id=?", in.Player).Scan(&count); err != nil {
			return nil, err
		}
		if count >= in.MaxScripts {
			return nil, ErrBattleScriptLimit
		}
		if in.NewScriptID == 0 {
			return nil, errors.New("battle script submit: new script id is required")
		}
		record := &pb.MatchBattleScriptRecord{ScriptId: in.NewScriptID, OwnerPlayerId: in.Player, Revision: 1,
			Content: in.Content, ReviewState: reviewPending, SubmittedMs: n}
		if _, err = tx.ExecContext(ctx, "INSERT INTO match_battle_script("+battleScriptColumns+") VALUES(?,?,?,?,?,?,?,?,?,?)",
			record.ScriptId, record.OwnerPlayerId, record.Revision, record.Content, record.ReviewState,
			record.ApprovedRevision, []byte{}, record.RejectReason, record.SubmittedMs, record.ReviewedMs); err != nil {
			return nil, err
		}
		return record, tx.Commit()
	}

	current, err := scanBattleScript(tx.QueryRowContext(ctx, "SELECT "+battleScriptColumns+
		" FROM match_battle_script WHERE script_id=? FOR UPDATE", in.ScriptID))
	if errors.Is(err, sql.ErrNoRows) || (err == nil && current.OwnerPlayerId != in.Player) {
		return nil, ErrBattleScriptNotFound
	}
	if err != nil {
		return nil, err
	}
	if current.Revision != in.ExpectedRevision {
		return nil, ErrBattleScriptRevision
	}
	current.Revision++
	current.Content = in.Content
	current.ReviewState = reviewPending
	current.RejectReason = ""
	current.SubmittedMs = n
	if _, err = tx.ExecContext(ctx, "UPDATE match_battle_script SET revision=?,content=?,review_state=?,reject_reason='',submitted_ms=? WHERE script_id=?",
		current.Revision, current.Content, current.ReviewState, current.SubmittedMs, current.ScriptId); err != nil {
		return nil, err
	}
	return current, tx.Commit()
}

// DeleteBattleScript 删除本人的一份剧本,并在同一事务清掉本人指向它的选用。审核流水保留(审计)
func (s *Store) DeleteBattleScript(ctx context.Context, player, scriptID uint64, expectedRevision uint32) error {
	return s.Fenced(ctx, func() error { return s.deleteBattleScriptOnce(ctx, player, scriptID, expectedRevision) })
}

func (s *Store) deleteBattleScriptOnce(ctx context.Context, player, scriptID uint64, expectedRevision uint32) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = s.lockActor(ctx, tx, player); err != nil {
		return err
	}
	var (
		owner    uint64
		revision uint32
	)
	err = tx.QueryRowContext(ctx, "SELECT owner_player_id,revision FROM match_battle_script WHERE script_id=? FOR UPDATE", scriptID).Scan(&owner, &revision)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && owner != player) {
		return ErrBattleScriptNotFound
	}
	if err != nil {
		return err
	}
	if revision != expectedRevision {
		return ErrBattleScriptRevision
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM match_battle_script WHERE script_id=?", scriptID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM match_battle_script_loadout WHERE player_id=? AND player_script_id=?", player, scriptID); err != nil {
		return err
	}
	return tx.Commit()
}

// BattleScriptChoice 是一条选用写入:Origin 为 UNSPECIFIED 表示清除 (MatchMode, BattleConfigID) 这条
type BattleScriptChoice struct {
	Player           uint64
	MatchMode        uint32
	BattleConfigID   uint32
	Origin           battlepb.EBattleScriptOrigin
	OfficialScriptID uint32 // 调用方已对照官方目录校验过
	PlayerScriptID   uint64
	MaxEntries       uint32
}

// SetBattleScriptLoadout 写或清一条选用,返回改完后本人的全部选用。
// 选玩家剧本时在锁内核对它属于本人、至少通过过一次审核,并锁住剧本行,与删除串行
func (s *Store) SetBattleScriptLoadout(ctx context.Context, in BattleScriptChoice) ([]*pb.MatchBattleScriptLoadoutRecord, error) {
	var out []*pb.MatchBattleScriptLoadoutRecord
	err := s.Fenced(ctx, func() error {
		list, err := s.setBattleScriptLoadoutOnce(ctx, in)
		out = list
		return err
	})
	return out, err
}

func (s *Store) setBattleScriptLoadoutOnce(ctx context.Context, in BattleScriptChoice) ([]*pb.MatchBattleScriptLoadoutRecord, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = s.lockActor(ctx, tx, in.Player); err != nil {
		return nil, err
	}

	if in.Origin == battlepb.EBattleScriptOrigin_BATTLE_SCRIPT_ORIGIN_UNSPECIFIED {
		if _, err = tx.ExecContext(ctx, "DELETE FROM match_battle_script_loadout WHERE player_id=? AND match_mode=? AND battle_config_id=?",
			in.Player, in.MatchMode, in.BattleConfigID); err != nil {
			return nil, err
		}
	} else {
		if in.Origin == battlepb.EBattleScriptOrigin_BATTLE_SCRIPT_ORIGIN_PLAYER {
			var (
				owner    uint64
				approved uint32
			)
			err = tx.QueryRowContext(ctx, "SELECT owner_player_id,approved_revision FROM match_battle_script WHERE script_id=? FOR UPDATE",
				in.PlayerScriptID).Scan(&owner, &approved)
			if errors.Is(err, sql.ErrNoRows) || (err == nil && owner != in.Player) {
				return nil, ErrBattleScriptNotFound
			}
			if err != nil {
				return nil, err
			}
			if approved == 0 {
				return nil, ErrBattleScriptNotApproved
			}
		}
		var others uint32
		if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM match_battle_script_loadout WHERE player_id=? AND NOT (match_mode=? AND battle_config_id=?)",
			in.Player, in.MatchMode, in.BattleConfigID).Scan(&others); err != nil {
			return nil, err
		}
		if others >= in.MaxEntries {
			return nil, ErrBattleScriptLoadoutFull
		}
		n, err := now(ctx, tx)
		if err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO match_battle_script_loadout("+battleScriptLoadoutColumns+") VALUES(?,?,?,?,?,?,?)"+
			" ON DUPLICATE KEY UPDATE origin=VALUES(origin),official_script_id=VALUES(official_script_id),player_script_id=VALUES(player_script_id),updated_ms=VALUES(updated_ms)",
			in.Player, in.MatchMode, in.BattleConfigID, uint32(in.Origin), in.OfficialScriptID, in.PlayerScriptID, n); err != nil {
			return nil, err
		}
	}
	list, err := queryBattleScriptLoadout(ctx, tx, in.Player)
	if err != nil {
		return nil, err
	}
	return list, tx.Commit()
}

// PendingBattleScripts 待审列表,先提交的在前。只读
func (s *Store) PendingBattleScripts(ctx context.Context, limit uint32) ([]*pb.MatchBattleScriptRecord, error) {
	return queryBattleScripts(ctx, s.DB, "SELECT "+battleScriptColumns+
		" FROM match_battle_script WHERE review_state=? ORDER BY submitted_ms,script_id LIMIT ?", reviewPending, limit)
}

// BattleScriptReview 是一次审核:Revision 必须仍是当前提交版且待审。
// Validate 在通过时对锁住的那一版正文再校验一次(纯函数,不调 RPC);返回错误则不通过、回 ErrBattleScriptReviewInvalid
type BattleScriptReview struct {
	ScriptID uint64
	Revision uint32
	Approve  bool
	Reviewer string
	Reason   string
	Validate func(content []byte) error
}

// ReviewBattleScript 通过或驳回一版剧本,同一事务写审核流水。审的不是当前待审版回 ErrBattleScriptReviewStale。
// 不锁作者的 role_fence:审核只改剧本行与流水,剧本行的行锁已与作者的改写、删除串行
func (s *Store) ReviewBattleScript(ctx context.Context, in BattleScriptReview) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	current, err := scanBattleScript(tx.QueryRowContext(ctx, "SELECT "+battleScriptColumns+
		" FROM match_battle_script WHERE script_id=? FOR UPDATE", in.ScriptID))
	if errors.Is(err, sql.ErrNoRows) {
		return ErrBattleScriptReviewStale
	}
	if err != nil {
		return err
	}
	if current.Revision != in.Revision || current.ReviewState != reviewPending {
		return ErrBattleScriptReviewStale
	}
	if in.Approve && in.Validate != nil && in.Validate(current.Content) != nil {
		return ErrBattleScriptReviewInvalid
	}
	n, err := now(ctx, tx)
	if err != nil {
		return err
	}
	if in.Approve {
		_, err = tx.ExecContext(ctx, "UPDATE match_battle_script SET review_state=?,approved_revision=?,approved_content=?,reject_reason='',reviewed_ms=? WHERE script_id=?",
			reviewApproved, current.Revision, current.Content, n, current.ScriptId)
	} else {
		_, err = tx.ExecContext(ctx, "UPDATE match_battle_script SET review_state=?,reject_reason=?,reviewed_ms=? WHERE script_id=?",
			reviewRejected, in.Reason, n, current.ScriptId)
	}
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO match_battle_script_review(script_id,revision,owner_player_id,approved,reviewer,reason,reviewed_ms,content) VALUES(?,?,?,?,?,?,?,?)",
		current.ScriptId, current.Revision, current.OwnerPlayerId, in.Approve, in.Reviewer, in.Reason, n, current.Content); err != nil {
		return err
	}
	return tx.Commit()
}
