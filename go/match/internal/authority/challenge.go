package authority

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"math"

	"google.golang.org/protobuf/proto"
	pb "proto/match"
)

const challengeColumns = "challenge_id,challenger_player_id,target_player_id,config_id,challenger_account,challenger_revision,target_account,target_revision,created_ms,expires_ms,state,battle_id"

const (
	challengePending         uint32 = 1
	challengeAccepted        uint32 = 2
	challengeDeclined        uint32 = 3
	challengeAcceptedPending uint32 = 4
	challengeSafelyClosed    uint32 = 5
)

var ErrChallengePending = errors.New("目标已有未到期切磋邀请")
var ErrChallengeExpired = errors.New("切磋邀请不存在或已到期")
var ErrChallengeNotTarget = errors.New("切磋邀请不属于原应战者")

func scanChallenge(row scanner) (*pb.MatchChallengeInvitationRecord, error) {
	invitation := &pb.MatchChallengeInvitationRecord{}
	err := row.Scan(&invitation.ChallengeId, &invitation.ChallengerPlayerId, &invitation.TargetPlayerId, &invitation.ConfigId, &invitation.ChallengerAccount, &invitation.ChallengerRevision, &invitation.TargetAccount, &invitation.TargetRevision, &invitation.CreatedMs, &invitation.ExpiresMs, &invitation.State, &invitation.BattleId)
	return invitation, err
}

func challengeOriginals(invitation *pb.MatchChallengeInvitationRecord) []*pb.MatchAuthorityTicketRecord {
	return []*pb.MatchAuthorityTicketRecord{
		{PlayerId: invitation.ChallengerPlayerId, OwnerAccount: invitation.ChallengerAccount, OwnerRevision: invitation.ChallengerRevision},
		{PlayerId: invitation.TargetPlayerId, OwnerAccount: invitation.TargetAccount, OwnerRevision: invitation.TargetRevision},
	}
}

func matchesChallengeOriginal(a, b *pb.MatchChallengeInvitationRecord) bool {
	return a.ChallengeId == b.ChallengeId && a.ChallengerPlayerId == b.ChallengerPlayerId && a.TargetPlayerId == b.TargetPlayerId && a.ConfigId == b.ConfigId && a.ChallengerAccount == b.ChallengerAccount && a.ChallengerRevision == b.ChallengerRevision && a.TargetAccount == b.TargetAccount && a.TargetRevision == b.TargetRevision && a.CreatedMs == b.CreatedMs && a.ExpiresMs == b.ExpiresMs
}

// 新意图在双方P锁下捕获对端；已有同正文邀请只回放原编号/有效期，绝不刷新TTL或换主。
func (s *Store) CreateChallenge(ctx context.Context, id, challenger, target uint64, config uint32, ttlMs uint64) (invitation *pb.MatchChallengeInvitationRecord, err error) {
	err = s.Fenced(ctx, func() (e error) {
		invitation, e = s.createChallengeOnce(ctx, id, challenger, target, config, ttlMs)
		return e
	})
	return invitation, err
}

func (s *Store) createChallengeOnce(ctx context.Context, id, challenger, target uint64, config uint32, ttlMs uint64) (*pb.MatchChallengeInvitationRecord, error) {
	if id == 0 || challenger == 0 || target == 0 || challenger == target || ttlMs == 0 {
		return nil, errors.New("切磋邀请参数无效")
	}
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// 预读只定位已存在的邀请原件，不取匹配锁。重复请求必须核旧双方，
	// 不能把当前目录的新target owner变成旧邀请的原件。
	observed, readErr := scanChallenge(tx.QueryRowContext(ctx, "SELECT "+challengeColumns+" FROM match_challenge_invitation WHERE target_player_id=? AND state=? AND expires_ms>CAST(UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000 AS UNSIGNED) ORDER BY expires_ms,challenge_id LIMIT 1", target, challengePending))
	if readErr == nil {
		actor := originalPlayer(ctx, challenger)
		if observed.ChallengerPlayerId != challenger || observed.ConfigId != config || observed.ChallengerAccount != actor.OwnerAccount || observed.ChallengerRevision != actor.OwnerRevision {
			return nil, ErrChallengePending
		}
		if err = s.lockOriginalPlayers(ctx, tx, challengeOriginals(observed)); err != nil {
			return nil, err
		}
		current, err := scanChallenge(tx.QueryRowContext(ctx, "SELECT "+challengeColumns+" FROM match_challenge_invitation WHERE challenge_id=? FOR UPDATE", observed.ChallengeId))
		if err != nil {
			return nil, err
		}
		n, err := now(ctx, tx)
		if err != nil {
			return nil, err
		}
		if !matchesChallengeOriginal(current, observed) || current.State != challengePending || current.ExpiresMs <= n {
			return nil, ErrChallengeExpired
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		return current, nil
	}
	if !errors.Is(readErr, sql.ErrNoRows) {
		return nil, readErr
	}
	actor, peer, err := s.captureChallengeOriginals(ctx, tx, challenger, target)
	if err != nil {
		return nil, err
	}
	n, err := now(ctx, tx)
	if err != nil {
		return nil, err
	}
	if ttlMs > math.MaxUint64-n {
		return nil, errors.New("切磋有效期溢出")
	}
	old, err := scanChallenge(tx.QueryRowContext(ctx, "SELECT "+challengeColumns+" FROM match_challenge_invitation WHERE target_player_id=? AND state=? AND expires_ms>? ORDER BY expires_ms,challenge_id LIMIT 1 FOR UPDATE", target, challengePending, n))
	if err == nil {
		if old.ChallengerPlayerId != challenger || old.ConfigId != config || old.ChallengerAccount != actor.OwnerAccount || old.ChallengerRevision != actor.OwnerRevision || old.TargetAccount != peer.OwnerAccount || old.TargetRevision != peer.OwnerRevision {
			return nil, ErrChallengePending
		}
		if err = tx.Commit(); err != nil {
			return nil, err
		}
		return old, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	invitation := &pb.MatchChallengeInvitationRecord{ChallengeId: id, ChallengerPlayerId: challenger, TargetPlayerId: target, ConfigId: config, ChallengerAccount: actor.OwnerAccount, ChallengerRevision: actor.OwnerRevision, TargetAccount: peer.OwnerAccount, TargetRevision: peer.OwnerRevision, CreatedMs: n, ExpiresMs: n + ttlMs, State: challengePending}
	_, err = tx.ExecContext(ctx, "INSERT INTO match_challenge_invitation("+challengeColumns+") VALUES(?,?,?,?,?,?,?,?,?,?,?,?)", invitation.ChallengeId, challenger, target, config, invitation.ChallengerAccount, invitation.ChallengerRevision, invitation.TargetAccount, invitation.TargetRevision, n, invitation.ExpiresMs, invitation.State, 0)
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return invitation, nil
}

// 非锁定预读仅用于定位固定原身份；授权在P首锁之后重新核验，Redis正文不参与。
func (s *Store) ChallengeForActor(ctx context.Context, id, responder uint64) (invitation *pb.MatchChallengeInvitationRecord, err error) {
	err = s.Fenced(ctx, func() (e error) { invitation, e = s.challengeForActorOnce(ctx, id, responder); return e })
	return invitation, err
}

func (s *Store) challengeForActorOnce(ctx context.Context, id, responder uint64) (*pb.MatchChallengeInvitationRecord, error) {
	if id == 0 || responder == 0 {
		return nil, ErrChallengeExpired
	}
	observed, err := scanChallenge(s.DB.QueryRowContext(ctx, "SELECT "+challengeColumns+" FROM match_challenge_invitation WHERE challenge_id=?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrChallengeExpired
	}
	if err != nil {
		return nil, err
	}
	if observed.TargetPlayerId != responder {
		return nil, ErrChallengeNotTarget
	}
	actor := originalPlayer(ctx, responder)
	if actor.OwnerAccount != observed.TargetAccount || actor.OwnerRevision != observed.TargetRevision {
		return nil, ErrOriginalPlayer
	}
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = s.lockOriginalPlayers(ctx, tx, challengeOriginals(observed)); err != nil {
		return nil, err
	}
	current, err := scanChallenge(tx.QueryRowContext(ctx, "SELECT "+challengeColumns+" FROM match_challenge_invitation WHERE challenge_id=?", id))
	if err != nil {
		return nil, err
	}
	if !matchesChallengeOriginal(current, observed) {
		return nil, ErrDirectConflict
	}
	if current.State < challengePending || current.State > challengeSafelyClosed {
		return nil, errors.New("切磋邀请状态损坏")
	}
	if current.State == challengePending {
		n, err := now(ctx, tx)
		if err != nil {
			return nil, err
		}
		if current.ExpiresMs <= n {
			return nil, ErrChallengeExpired
		}
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return current, nil
}

// DecideChallenge先将原接受决定、direct退出原件及全组pending占用同事务提交。
// 原Remove正证明后CompletePreparedDirect才成局；重放不换config/名册/测量或退出对象。
func (s *Store) DecideChallenge(ctx context.Context, observed *pb.MatchChallengeInvitationRecord, accept bool, f *pb.MatchFormationRecord, group []*pb.MatchAuthorityTicketRecord) (formation *pb.MatchFormationRecord, decided bool, err error) {
	err = s.Fenced(ctx, func() (e error) {
		formation, decided, e = s.decideChallengeOnce(ctx, observed, accept, f, group)
		return e
	})
	return formation, decided, err
}

func (s *Store) decideChallengeOnce(ctx context.Context, observed *pb.MatchChallengeInvitationRecord, accept bool, f *pb.MatchFormationRecord, group []*pb.MatchAuthorityTicketRecord) (*pb.MatchFormationRecord, bool, error) {
	if observed == nil || observed.ChallengeId == 0 {
		return nil, false, ErrChallengeExpired
	}
	actor := originalPlayer(ctx, observed.TargetPlayerId)
	if actor.OwnerAccount != observed.TargetAccount || actor.OwnerRevision != observed.TargetRevision {
		return nil, false, ErrOriginalPlayer
	}
	decision := &pb.RespondChallengeRequest{PlayerId: observed.TargetPlayerId, ChallengeId: observed.ChallengeId, Accept: accept}
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(decision)
	if err != nil {
		return nil, false, err
	}
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	if err = s.lockOriginalPlayers(ctx, tx, challengeOriginals(observed)); err != nil {
		return nil, false, err
	}
	current, err := scanChallenge(tx.QueryRowContext(ctx, "SELECT "+challengeColumns+" FROM match_challenge_invitation WHERE challenge_id=? FOR UPDATE", observed.ChallengeId))
	if err != nil {
		return nil, false, err
	}
	if !matchesChallengeOriginal(current, observed) {
		return nil, false, ErrDirectConflict
	}
	wantedState := challengeDeclined
	if accept {
		wantedState = challengeAccepted
	}
	if current.State != challengePending {
		if current.State == challengeAcceptedPending || current.State == challengeSafelyClosed {
			if !accept {
				return nil, false, ErrDirectConflict
			}
			key := &pb.MatchDirectIntentRecord{Kind: 3, OwnerId: current.TargetPlayerId, RequestLow: current.ChallengeId}
			original, err := readDirectTx(ctx, tx, key)
			if err != nil {
				return nil, false, err
			}
			if original == nil || !original.ExitPrepared || !bytes.Equal(original.Payload, payload) || original.OwnerAccount != current.TargetAccount || original.OwnerRevision != current.TargetRevision {
				return nil, false, ErrDirectConflict
			}
			if current.State == challengeSafelyClosed || original.Cancelled {
				return nil, false, ErrDirectCancelled
			}
			return nil, false, tx.Commit()
		}
		if current.State != wantedState {
			return nil, false, ErrDirectConflict
		}
		var original []byte
		var battle, revision uint64
		var account string
		if err = tx.QueryRowContext(ctx, "SELECT payload,battle_id,owner_account,owner_revision FROM match_direct_intent WHERE kind=3 AND owner_id=? AND request_high=0 AND request_low=? FOR UPDATE", current.TargetPlayerId, current.ChallengeId).Scan(&original, &battle, &account, &revision); err != nil {
			return nil, false, err
		}
		if !bytes.Equal(original, payload) || account != current.TargetAccount || revision != current.TargetRevision || battle != current.BattleId {
			return nil, false, ErrDirectConflict
		}
		var result *pb.MatchFormationRecord
		if accept {
			if battle == 0 {
				return nil, false, errors.New("已接受切磋缺固定成局")
			}
			result, err = scanFormation(tx.QueryRowContext(ctx, "SELECT "+formationColumns+" FROM match_formation WHERE battle_id=?", battle))
			if err != nil {
				return nil, false, err
			}
		} else if battle != 0 {
			return nil, false, errors.New("拒战原件带有成局")
		}
		if err = tx.Commit(); err != nil {
			return nil, false, err
		}
		return result, false, nil
	}
	n, err := now(ctx, tx)
	if err != nil {
		return nil, false, err
	}
	if current.ExpiresMs <= n {
		return nil, false, ErrChallengeExpired
	}
	if accept {
		if f == nil || len(group) != 2 || f.Mode != uint32(pb.MatchMode_MATCH_MODE_PVP_CHALLENGE) || f.ConfigId != current.ConfigId || f.RequeueOnFailure {
			return nil, false, ErrDirectConflict
		}
		originals := challengeOriginals(current)
		for i, expected := range originals {
			if group[i] == nil || group[i].PlayerId != expected.PlayerId || group[i].OwnerAccount != expected.OwnerAccount || group[i].OwnerRevision != expected.OwnerRevision || group[i].ConfigId != current.ConfigId {
				return nil, false, ErrOriginalPlayer
			}
		}
		key := &pb.MatchDirectIntentRecord{Kind: 3, OwnerId: current.TargetPlayerId, RequestLow: current.ChallengeId}
		previous, err := readDirectTx(ctx, tx, key)
		if err != nil {
			return nil, false, err
		}
		if previous != nil {
			return nil, false, ErrDirectConflict
		}
		if _, err = s.prepareDirectAdmissionTx(ctx, tx, key, payload, f, group, actor, nil); err != nil {
			return nil, false, err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE match_challenge_invitation SET state=4 WHERE challenge_id=? AND state=1", current.ChallengeId); err != nil {
			return nil, false, err
		}
		return nil, true, tx.Commit()
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO match_direct_intent("+directColumns+") VALUES(3,?,0,?,?,0,0,?,?,?,0,?)", current.TargetPlayerId, current.ChallengeId, payload, current.TargetAccount, current.TargetRevision, []byte{}, []byte{}); err != nil {
		return nil, false, err
	}
	if _, err = tx.ExecContext(ctx, "UPDATE match_challenge_invitation SET state=3,battle_id=0 WHERE challenge_id=?", current.ChallengeId); err != nil {
		return nil, false, err
	}
	if err = ctx.Err(); err != nil {
		return nil, false, err
	}
	if err = tx.Commit(); err != nil {
		return nil, false, err
	}
	return nil, true, nil
}
