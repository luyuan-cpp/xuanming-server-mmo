package data

// 活动对局结果的持久记录:guild 一侧的读与销账(设计 docs/design/guild-phase2/06-activities.md §6.19–§6.21、§6.29 第 7 步、§6.33)。
//
// 通道契约(跨语言;C++ 侧在 cpp/libs/services/battle/system/battle_result_activity.h 的 kActivityResultKeyPrefix /
// ActivityResultKey,两边改一处必须同改另一处):
//
//	键   battle:activity_result:{battle_id}(十进制)
//	值   BattleResultEvent 序列化字节(含 activity_context)
//	存储 battle 的 zone Redis(文档里称 SharedRedis)—— 必须与 guild 的 PlayerLocatorRedis 是同一实例、同一 DB
//	TTL  7 天(battle 写入时设置,与 Kafka match-results 的保留期一致)
//	写入 battle:只在带活动上下文的对局结束时写,写成功后才发 Kafka;之后每 10s 查一次键,仍在就按原字节重发(至多 30 次)
//	销账 guild:该局在 guild 侧有了定论之后 DEL(本文件的 Ack)
//
// 这条记录是 battle 的"发件箱":键还在 = guild 还没确认收到。所以
//   - 销账必须在结算 / 标记的事务**提交之后**:先销账后提交,提交失败这一局的奖励就永久丢了;
//   - 销账失败不是错误:battle 会重发,guild 再走一遍幂等闸门回 Duplicate,然后再销一次;
//   - 巡检器(06 §6.33)对超时仍未结算的对局直接读这条记录来结算,兜住"Kafka 把事件弄丢了"与"重发次数用尽"。
//
// 本文件只管这一个键的读与删,不解析值(反序列化与结算在 logic),也不碰任何别的 Redis 键。

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// activityResultKeyPrefix 与 C++ 的 kActivityResultKeyPrefix 逐字一致;完整键 = 前缀 + 十进制 battle_id。
const activityResultKeyPrefix = "battle:activity_result:"

// trialResultRecordBudget:单次读 / 删的子预算。单键操作,跑满 1s 说明 Redis 出了状况。
const trialResultRecordBudget = 1000 * time.Millisecond

// activityResultKey 返回一局活动对局的结果记录键(与 C++ 的 ActivityResultKey 同形)。
func activityResultKey(battleID uint64) string {
	return activityResultKeyPrefix + strconv.FormatUint(battleID, 10)
}

// TrialResultRecords 读、销 battle 落在 SharedRedis 上的活动对局结果记录。
// 线程模型:构造后只读,可被消费者与巡检器的 goroutine 共享。
type TrialResultRecords struct {
	rdb *redis.Client
}

// NewTrialResultRecords。rdb 必须是 battle 写结果记录的那个 Redis(guild 的 PlayerLocatorRedis);
// 指错实例的表现是"永远销不掉账、巡检器永远读不到记录",启动期无从自检,所以由部署核对(06 §6.46 B6b-srv1 第 0 步)。
// nil 时报错:没有它,结算之后无法销账,battle 会把每一局都重发满 30 次。
func NewTrialResultRecords(rdb *redis.Client) (*TrialResultRecords, error) {
	if rdb == nil {
		return nil, errors.New("guild trial result records: nil redis client")
	}
	return &TrialResultRecords{rdb: rdb}, nil
}

// Load 读一局的结果记录原始字节。found=false = 记录不存在(从未写过、已销账、或 7 天 TTL 已过)。
// 巡检器用:有记录就反序列化后结算;没有记录且对局登记已久,才判 EXPIRED。
// error 表示 Redis 故障 —— 调用方本轮到此为止,**不得**把读不到当成"没有记录"去判过期。
func (r *TrialResultRecords) Load(ctx context.Context, battleID uint64) (payload []byte, found bool, err error) {
	if battleID == 0 {
		return nil, false, errors.New("guild trial result records: battle id must be non-zero")
	}
	ctx, cancel := context.WithTimeout(ctx, trialResultRecordBudget)
	defer cancel()
	payload, err = r.rdb.Get(ctx, activityResultKey(battleID)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("guild trial result records: read battle %d: %w", battleID, err)
	}
	return payload, true, nil
}

// Ack 销账:删掉一局的结果记录,battle 的重发随之停止。键本来就不在也算成功(幂等)。
// 只在这一局于 guild 侧已有定论之后调用:结算提交(含 Duplicate / GuildGone / ContextMismatch)、毒消息标记完成、
// 或上下文残缺无法处理。error 表示 Redis 故障;调用方只打 WARN,不因此重做结算(battle 重发后会再销一次)。
func (r *TrialResultRecords) Ack(ctx context.Context, battleID uint64) error {
	if battleID == 0 {
		return errors.New("guild trial result records: battle id must be non-zero")
	}
	ctx, cancel := context.WithTimeout(ctx, trialResultRecordBudget)
	defer cancel()
	if err := r.rdb.Del(ctx, activityResultKey(battleID)).Err(); err != nil {
		return fmt.Errorf("guild trial result records: ack battle %d: %w", battleID, err)
	}
	return nil
}
