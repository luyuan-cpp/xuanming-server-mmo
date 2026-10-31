package data

// 活动对局结果的兜底来源:match 权威库的 SQL 终局(设计 docs/design/guild-phase2/06-activities.md §6.33)。
//
// battle 经 MatchInternal.CommitBattleOutcome 提交终局,match 在事务里把 BattleResultEvent 原字节(含 activity_context)写进
// match_battle_completion(COMPLETE),之后由它的发件箱发 Kafka match-results,guild 的消费者结算。
// 消费者卡住或发件箱还没发到的对局,由巡检器直接读这一行结算,同一条路径、同一个幂等闸门。
//
//	表     <MatchAuthoritySchema>.match_battle_completion,主键 battle_id;guild 只读,与主连接池同一事务域、显式跨 schema
//	COMPLETE  raw_payload 就是这局的结果
//	ABORT     match 在截止之后的裁决,没有 battle 原字节:这局不会再有结果,对 guild 等于「没有记录」,走超时语义
//
// 本文件只读这一行,不解析值(反序列化与结算在 logic),也不写 match 的任何表。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	matchpb "proto/match"
)

// trialResultRecordBudget:单次读的子预算。主键点查,跑满 1s 说明库出了状况。
const trialResultRecordBudget = 1000 * time.Millisecond

// TrialResultRecords 读 match 权威库里一局活动对局的 SQL 终局。
// 线程模型:构造后只读,可被巡检器的 goroutine 共享。
type TrialResultRecords struct {
	db    *sql.DB
	query string
}

// NewTrialResultRecords。guilds 必须已经 ConfigureMatchAuthority(启动期已核过这张表的列)。
func NewTrialResultRecords(guilds *GuildRepo) (*TrialResultRecords, error) {
	if guilds == nil || guilds.db == nil || guilds.matchAuthority == "" {
		return nil, errors.New("guild trial result records: match authority schema not configured")
	}
	return &TrialResultRecords{
		db:    guilds.db,
		query: "SELECT raw_payload FROM `" + guilds.matchAuthority + "`.match_battle_completion WHERE battle_id=? AND decision=?",
	}, nil
}

// Load 读一局的结果原始字节。found=false = 还没有 COMPLETE 终局(战斗未结束、还没提交,或 match 判了 ABORT)。
// 巡检器用:有结果就反序列化后结算;没有结果且对局登记已久,才判 EXPIRED。
// error 表示 SQL 故障 —— 调用方本轮到此为止,**不得**把读不到当成"没有结果"去判过期。
func (r *TrialResultRecords) Load(ctx context.Context, battleID uint64) (payload []byte, found bool, err error) {
	if battleID == 0 {
		return nil, false, errors.New("guild trial result records: battle id must be non-zero")
	}
	ctx, cancel := context.WithTimeout(ctx, trialResultRecordBudget)
	defer cancel()
	err = r.db.QueryRowContext(ctx, r.query, battleID, uint32(matchpb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_COMPLETE)).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("guild trial result records: read battle %d: %w", battleID, err)
	}
	return payload, true, nil
}
