package authority

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	chatpb "proto/chat"

	"google.golang.org/protobuf/proto"
)

// 世界频道的平台补审待办(docs/design/rename-and-content-safety.md §7.3)。
//
// 世界消息先按本地层的结果发出。平台在先行时限里没答复的,发送事务里同时写一行待补审:消息落了库,待办就在,
// 进程在发出之后崩溃也丢不了。各副本的后台循环(svc/platform_recheck.go)按行认领、送平台、违规就撤回,最后写结局。
// 运行账号只有 SELECT / INSERT,所以三张表都只追加:
//   - chat_platform_recheck:待补审,一条世界消息一行;
//   - chat_platform_recheck_claim:认领,第 n 次尝试一行(attempt 从 0 起),主键冲突决定哪个副本拿到这一次;
//     认领过了租期还没有结局(那个副本崩了,或平台这一次不可用),别的副本认领下一次;
//   - chat_platform_recheck_done:结局,有了就不再扫到它。
//
// 同一条消息只有拿到认领的副本会送平台,平台的调用量不随副本数翻倍。
const (
	// platformRecheckWindowMs 是扫描待补审的回看窗口。要远大于「尝试次数 × 租期」(3 × 20 秒),
	// 留出整个聊天服务不在的时间:全部副本停了超过这么久,更早的待办不再补审(消息照常可见,打不到平台这一层)。
	platformRecheckWindowMs = 15 * 60 * 1000
	// platformRecheckLeaseMs 是一次认领的租期:要盖住一次平台调用(最长 4 秒)加一次撤回落库(最长 3 秒)。
	// 平台不可用时下一次尝试也要等到租期过了才开始,它同时是重试的间隔。
	platformRecheckLeaseMs = 20 * 1000
	// PlatformRecheckMaxAttempts 是一条待办最多送几次平台;用完了还没有结局就记「放弃」并报错。
	PlatformRecheckMaxAttempts = 3
)

// RecheckOutcome 是一条待补审的结局,存在 chat_platform_recheck_done.outcome。只追加取值,不复用。
type RecheckOutcome uint8

const (
	RecheckPassed    RecheckOutcome = 1 // 平台判通过
	RecheckRetracted RecheckOutcome = 2 // 平台判违规并已撤回,或在补审之前已被撤回
	RecheckGaveUp    RecheckOutcome = 3 // 尝试次数用完平台仍不可用,或原件已不在
)

// PlatformRecheckTask 是认领到的一条待补审。
type PlatformRecheckTask struct {
	SenderPlayerID uint64
	AcceptedMs     int64
	// Attempt 是这一次是第几次尝试,从 0 起。
	Attempt uint32
	// Content 是发送请求里的原文(去掉首尾空白)。送平台的是原文,不是打码之后落库的显示原件。
	Content string
	// AlreadyRetracted:补审之前这条已经撤回了(运维撤回,或上一次尝试撤回成功但没来得及写结局),不用再送平台。
	AlreadyRetracted bool
	// Exhausted:尝试次数用完了还没有结局。这条没有被认领,调用方只记一条「放弃」。
	Exhausted bool
	// Missing:原件已经不在(保留期删掉了),送不了平台,调用方记「放弃」。
	Missing bool
}

// enqueuePlatformRecheck 在发送事务里写一行待补审,和消息原件同进同退。
func enqueuePlatformRecheck(ctx context.Context, tx *sql.Tx, sender, acceptedMs uint64) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO chat_platform_recheck(sender_player_id,accepted_ms,accepted_at) VALUES (?,?,?)",
		sender, acceptedMs, acceptedAt(acceptedMs))
	return err
}

// ClaimPlatformRechecks 认领至多 limit 条该做的待补审:回看窗口里还没有结局、上一次认领已过租期(或从没被认领过)的。
// 别的副本先认领了的跳过。尝试次数已用完的也返回(Exhausted),由调用方记「放弃」。
func (s *Store) ClaimPlatformRechecks(ctx context.Context, limit int) ([]PlatformRecheckTask, error) {
	if limit <= 0 {
		return nil, ErrInvalid
	}
	now, err := s.readClock(ctx)
	if err != nil {
		return nil, err
	}
	since := uint64(0)
	if now > platformRecheckWindowMs {
		since = now - platformRecheckWindowMs
	}
	// 多取一些候选:在租期里的、被别的副本抢先认领的都会跳过。
	rows, err := s.db.QueryContext(ctx,
		"SELECT r.sender_player_id,r.accepted_ms FROM chat_platform_recheck r WHERE r.accepted_ms>=? AND NOT EXISTS "+
			"(SELECT 1 FROM chat_platform_recheck_done d WHERE d.sender_player_id=r.sender_player_id AND d.accepted_ms=r.accepted_ms) "+
			"ORDER BY r.accepted_ms LIMIT ?", since, limit*4)
	if err != nil {
		return nil, err
	}
	type candidate struct{ sender, acceptedMs uint64 }
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err = rows.Scan(&c.sender, &c.acceptedMs); err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, c)
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	var tasks []PlatformRecheckTask
	for _, c := range candidates {
		if len(tasks) == limit {
			break
		}
		task, claimed, claimErr := s.claimPlatformRecheck(ctx, c.sender, c.acceptedMs)
		if claimErr != nil {
			return tasks, claimErr
		}
		if claimed {
			tasks = append(tasks, task)
		}
	}
	return tasks, nil
}

// claimPlatformRecheck 认领一条待补审的下一次尝试。返回 false = 这一次不归本副本(在租期里,或别的副本先认领了)。
func (s *Store) claimPlatformRecheck(ctx context.Context, sender, acceptedMs uint64) (PlatformRecheckTask, bool, error) {
	task := PlatformRecheckTask{SenderPlayerID: sender, AcceptedMs: int64(acceptedMs)}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return task, false, err
	}
	defer tx.Rollback()
	now, err := s.clock(ctx, tx)
	if err != nil {
		return task, false, err
	}
	var attempts uint32
	var lastClaimMs uint64
	if err = tx.QueryRowContext(ctx,
		"SELECT COUNT(*),COALESCE(MAX(claimed_ms),0) FROM chat_platform_recheck_claim WHERE sender_player_id=? AND accepted_ms=?",
		sender, acceptedMs).Scan(&attempts, &lastClaimMs); err != nil {
		return task, false, err
	}
	if attempts > 0 && lastClaimMs+platformRecheckLeaseMs > now {
		return task, false, nil // 上一次还在租期里:可能有副本正在做,也可能是平台不可用之后的重试间隔
	}
	task.Attempt = attempts
	if attempts >= PlatformRecheckMaxAttempts {
		task.Exhausted = true
		return task, true, nil
	}
	res, err := tx.ExecContext(ctx,
		"INSERT IGNORE INTO chat_platform_recheck_claim(sender_player_id,accepted_ms,attempt,claimed_ms,accepted_at) VALUES (?,?,?,?,?)",
		sender, acceptedMs, attempts, now, acceptedAt(acceptedMs))
	if err != nil {
		return task, false, err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		return task, false, nil // 别的副本先认领了这一次
	}
	var found int
	err = tx.QueryRowContext(ctx, "SELECT 1 FROM chat_world_retraction WHERE sender_player_id=? AND accepted_ms=? LIMIT 1",
		sender, acceptedMs).Scan(&found)
	switch {
	case err == nil:
		task.AlreadyRetracted = true
	case !errors.Is(err, sql.ErrNoRows):
		return task, false, err
	default:
		var payload []byte
		err = tx.QueryRowContext(ctx,
			"SELECT request_payload FROM chat_send_authority WHERE player_id=? AND state=1 AND accepted_ms=? AND stream_key=? LIMIT 1",
			sender, acceptedMs, []byte(worldStreamKey(sender))).Scan(&payload)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			task.Missing = true
		case err != nil:
			return task, false, err
		default:
			request := &chatpb.SendChatRequest{}
			if proto.Unmarshal(payload, request) != nil || request.GetMessage() == nil {
				return task, false, errors.New("聊天永久原件损坏")
			}
			task.Content = strings.TrimSpace(request.GetMessage().GetContent())
		}
	}
	if err = tx.Commit(); err != nil {
		return task, false, err
	}
	return task, true, nil
}

// FinishPlatformRecheck 记下一条待补审的结局。同一条记过就不再记(INSERT IGNORE):两个副本都判了「放弃」也只有一行。
func (s *Store) FinishPlatformRecheck(ctx context.Context, sender uint64, acceptedMs int64, outcome RecheckOutcome) error {
	if sender == 0 || acceptedMs <= 0 || outcome < RecheckPassed || outcome > RecheckGaveUp {
		return ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now, err := s.clock(ctx, tx)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx,
		"INSERT IGNORE INTO chat_platform_recheck_done(sender_player_id,accepted_ms,outcome,done_ms,accepted_at) VALUES (?,?,?,?,?)",
		sender, uint64(acceptedMs), uint8(outcome), now, acceptedAt(uint64(acceptedMs))); err != nil {
		return err
	}
	return tx.Commit()
}

// readClock 在一个短事务里取库的时间:各副本按同一个钟判回看窗口与租期。不设 ReadOnly,与本包其余事务同一写法。
func (s *Store) readClock(ctx context.Context) (uint64, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	return s.clock(ctx, tx)
}
