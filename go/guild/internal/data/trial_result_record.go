package data

// 活动对局结果的兜底来源:match 的 SQL 终局(设计 docs/design/guild-phase2/06-activities.md §6.33)。
//
// battle 经 MatchInternal.CommitBattleOutcome 提交终局,match 在事务里把 BattleResultEvent 原字节(含 activity_context)写进
// match_battle_completion(COMPLETE),之后由它的发件箱发 Kafka match-results,guild 的消费者结算。
// 消费者卡住或发件箱还没发到的对局,由巡检器经 RPC 读这一行结算,同一条路径、同一个幂等闸门。
//
//	RPC    MatchInternal.GetBattleCompletion,取代原来的跨库读(docs/design/horizontal-scale-logical-db.md §4)
//	COMPLETE  raw_payload 就是这局的结果
//	ABORT     match 在截止之后的裁决,没有 battle 原字节:这局不会再有结果,对 guild 等于「没有记录」,走超时语义
//
// 本文件只读,不解析值(反序列化与结算在 logic),也不改 match 的任何东西。

import (
	"context"
	"errors"
	"fmt"
	"time"

	matchpb "proto/match"
	"shared/serviceauth"
)

// trialResultRecordBudget:单次读的子预算。主键点查加一跳 RPC,跑满 1s 说明 match 或它的库出了状况。
const trialResultRecordBudget = 1000 * time.Millisecond

// TrialResultRecords 读 match 里一局活动对局的 SQL 终局。
// 线程模型:构造后只读,可被巡检器的 goroutine 共享。
type TrialResultRecords struct {
	client matchpb.MatchInternalClient
	secret string
}

// NewTrialResultRecords。secret 和取消、开局用同一把:guild 调 MatchInternal 只有这一把密钥。
func NewTrialResultRecords(client matchpb.MatchInternalClient, secret string) (*TrialResultRecords, error) {
	if client == nil {
		return nil, errors.New("guild trial result records: no match client")
	}
	if _, err := serviceauth.New(map[string]string{matchInternalCaller: secret}); err != nil {
		return nil, fmt.Errorf("guild trial result records: %w", err)
	}
	return &TrialResultRecords{client: client, secret: secret}, nil
}

// Load 读一局的结果原始字节。found=false = 还没有 COMPLETE 终局(战斗未结束、还没提交,或 match 判了 ABORT)。
// 巡检器用:有结果就反序列化后结算;没有结果且对局登记已久,才判 EXPIRED。
// error 表示这次没问到 —— 调用方本轮到此为止,**不得**把读不到当成"没有结果"去判过期。
func (r *TrialResultRecords) Load(ctx context.Context, battleID uint64) (payload []byte, found bool, err error) {
	if battleID == 0 {
		return nil, false, errors.New("guild trial result records: battle id must be non-zero")
	}
	in := &matchpb.GetBattleCompletionRequest{BattleId: battleID}
	if err = serviceauth.Sign(matchpb.MatchInternal_GetBattleCompletion_FullMethodName, in, matchInternalCaller, r.secret, uint64(time.Now().UnixMilli())); err != nil {
		return nil, false, fmt.Errorf("guild trial result records: sign battle %d: %w", battleID, err)
	}
	ctx, cancel := context.WithTimeout(ctx, trialResultRecordBudget)
	defer cancel()
	out, err := r.client.GetBattleCompletion(ctx, in)
	if err != nil {
		return nil, false, fmt.Errorf("guild trial result records: read battle %d: %w", battleID, err)
	}
	if out.GetReject() != matchpb.TrialOccupancyReject_TRIAL_OCCUPANCY_REJECT_NONE {
		return nil, false, fmt.Errorf("guild trial result records: read battle %d rejected: %v", battleID, out.GetReject())
	}
	if out.GetDecision() != matchpb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_COMPLETE {
		return nil, false, nil
	}
	return out.GetRawPayload(), true, nil
}
