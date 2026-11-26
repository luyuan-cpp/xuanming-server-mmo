package authority

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode"
	"unicode/utf8"

	chatpb "proto/chat"

	"github.com/go-sql-driver/mysql"
	"google.golang.org/protobuf/proto"
)

const (
	maxSystemNoticeIDBytes   = 64
	maxSystemNoticeNameBytes = 64
)

func sanitizeNoticeActorName(raw string) (string, bool) {
	if !utf8.ValidString(raw) {
		return "", false
	}
	var b strings.Builder
	for _, r := range raw {
		if unicode.IsControl(r) || r == '\n' || r == '\r' || r == '\t' {
			continue
		}
		b.WriteRune(r)
	}
	trimmed := strings.TrimSpace(b.String())
	if trimmed == "" || len(trimmed) > maxSystemNoticeNameBytes {
		return "", false
	}
	return trimmed, true
}

// RenderSystemWorldNotice 严格按白名单模板渲染全服系统播报正文（绝不接收客户端自由文本，且不含违规敏感词）。
func RenderSystemWorldNotice(in *chatpb.PublishSystemWorldNoticeRequest) (string, bool) {
	if in == nil {
		return "", false
	}
	noticeID := strings.TrimSpace(in.GetNoticeId())
	if noticeID == "" || len(noticeID) > maxSystemNoticeIDBytes || !utf8.ValidString(noticeID) {
		return "", false
	}
	actorA, okA := sanitizeNoticeActorName(in.GetActorAName())
	actorB, okB := sanitizeNoticeActorName(in.GetActorBName())
	if !okA || !okB {
		return "", false
	}
	teamSize := in.GetTeamSize()
	if teamSize == 0 {
		teamSize = 1
	}
	if teamSize > 5 {
		return "", false
	}
	rounds := in.GetTotalRounds()
	streak := in.GetStreakCount()

	switch in.GetTemplateId() {
	case chatpb.SystemWorldNoticeTemplate_SYSTEM_WORLD_NOTICE_JIANGHU_DUEL_START_SILVER:
		if teamSize <= 1 {
			return fmt.Sprintf("【天墉生死擂】%s 向 %s 下达江湖战书，点击观战！", actorA, actorB), true
		}
		return fmt.Sprintf("【天墉生死擂】%s 向 %s 下达江湖战书（%dV%d），点击观战！", actorA, actorB, teamSize, teamSize), true
	case chatpb.SystemWorldNoticeTemplate_SYSTEM_WORLD_NOTICE_JIANGHU_DUEL_START_GOLD:
		if teamSize <= 1 {
			return fmt.Sprintf("【金帖生死状】%s 向 %s 掷出金帖生死状，名动天墉，点击观战！", actorA, actorB), true
		}
		return fmt.Sprintf("【金帖生死状】%s 向 %s 掷出金帖生死状（%dV%d），名动天墉，点击观战！", actorA, actorB, teamSize, teamSize), true
	case chatpb.SystemWorldNoticeTemplate_SYSTEM_WORLD_NOTICE_JIANGHU_DUEL_WIN_SILVER:
		if rounds == 0 {
			rounds = 1
		}
		return fmt.Sprintf("【天墉生死擂】%s 历经 %d 回合击败 %s，略胜一筹！", actorA, rounds, actorB), true
	case chatpb.SystemWorldNoticeTemplate_SYSTEM_WORLD_NOTICE_JIANGHU_DUEL_WIN_GOLD:
		if rounds == 0 {
			rounds = 1
		}
		return fmt.Sprintf("【金帖生死状】%s 历经 %d 回合大破 %s，快意恩仇！", actorA, rounds, actorB), true
	case chatpb.SystemWorldNoticeTemplate_SYSTEM_WORLD_NOTICE_JIANGHU_DUEL_DRAW:
		if rounds == 0 {
			rounds = 1
		}
		return fmt.Sprintf("【天墉生死擂】%s 与 %s 大战 %d 回合难分胜负，各自收剑！", actorA, actorB, rounds), true
	case chatpb.SystemWorldNoticeTemplate_SYSTEM_WORLD_NOTICE_JIANGHU_DUEL_STREAK_MILESTONE:
		if streak < 3 {
			return "", false
		}
		return fmt.Sprintf("【宿敌连胜】%s 再度力克宿敌 %s，豪取 %d 连胜！", actorA, actorB, streak), true
	case chatpb.SystemWorldNoticeTemplate_SYSTEM_WORLD_NOTICE_JIANGHU_DUEL_STREAK_BROKEN:
		if streak < 3 {
			return "", false
		}
		return fmt.Sprintf("【快意雪耻】%s 终结了宿敌 %s 的 %d 连胜，扬眉吐气！", actorA, actorB, streak), true
	default:
		return "", false
	}
}

// PublishSystemWorldNotice 幂等写入系统全服播报到世界频道权威历史(player_id=0, request_id=notice_id)。
// 首次提交返回 SendResult{Message: msg}；命中重复 notice_id 返回 SendResult{Duplicate: true} 且不重复广播。
func (s *Store) PublishSystemWorldNotice(ctx context.Context, in *chatpb.PublishSystemWorldNoticeRequest) (SendResult, error) {
	content, ok := RenderSystemWorldNotice(in)
	if !ok {
		return SendResult{}, ErrInvalid
	}
	noticeID := strings.TrimSpace(in.GetNoticeId())
	raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(in)
	if err != nil {
		return SendResult{}, err
	}
	digest := sha256.Sum256(raw)

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return SendResult{}, err
	}
	defer tx.Rollback()

	if _, readErr := scanReceipt(tx.QueryRowContext(ctx, receiptSelectSQL, uint64(0), []byte(noticeID))); readErr == nil {
		return SendResult{Duplicate: true}, nil
	} else if !errors.Is(readErr, sql.ErrNoRows) {
		return SendResult{}, readErr
	}

	now, err := s.clock(ctx, tx)
	if err != nil {
		return SendResult{}, err
	}
	if now == 0 || now > math.MaxInt64 {
		return SendResult{}, errors.New("聊天SQL时间非法")
	}
	message := &chatpb.ChatMessage{
		SenderPlayerId: 0,
		TargetPlayerId: 0,
		Channel:        chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_WORLD,
		Content:        content,
		SendTimeMs:     int64(now),
	}
	payload, err := (proto.MarshalOptions{Deterministic: true}).Marshal(message)
	if err != nil {
		return SendResult{}, err
	}
	if _, err = tx.ExecContext(ctx,
		"INSERT INTO chat_send_authority(player_id,request_id,"+receiptColumns+",accepted_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
		uint64(0), []byte(noticeID), []byte("system"), uint64(0), digest[:], raw, payload,
		[]byte(worldStreamKey(0)), acceptedState, now, uint32(0), acceptedAt(now)); err != nil {
		var duplicate *mysql.MySQLError
		if errors.As(err, &duplicate) && duplicate.Number == 1062 {
			return SendResult{Duplicate: true}, nil
		}
		return SendResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return SendResult{}, err
	}
	if s.world != nil {
		s.world.invalidate()
	}
	return SendResult{Message: message}, nil
}
