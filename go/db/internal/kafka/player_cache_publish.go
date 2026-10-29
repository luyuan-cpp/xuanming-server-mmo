package kafka

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"db/internal/metrics"
	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
	assetpb "proto/common/asset"
	dbpb "proto/db"
	"shared/assetop"
	"shared/savebarrier"
)

var errFenceEvidenceConflict = errors.New("同一 SQL 围栏版本的资产证据冲突")
var errFenceRedis = errors.New("围栏 Redis 步骤失败")

// 所有类型检查与冲突检查都先于写；Redis 脚本出错不会回滚已经写出的命令。
// KEYS: 锁、缓存、证据或缓存占位、存盘屏障或缓存占位。
// ARGV: 令牌、锁TTL毫秒、缓存原件、缓存TTL毫秒、版本、证据原件、发布证据、屏障表名。
// 回复: 发布结局(个位,cachePublish*) + ACK 领先码(十位,ackAhead* × ackAheadUnit),由 decodeCachePublishReply 拆开。
//
// ACK 领先 pending(docs/design/zone-redis-regression-guard-20261022.md §4.2 G2)：不变量是「同一屏障里每张表的
// ACK ≤ pending」—— C++ 存盘 Lua 先写 pending=V 再投递 V 的 DBTask，pending 只增不减，go/db 只在 SQL 提交 V 之后
// 才发布 ACK=V。所以本次要写下的 ACK 严格大于 pending、或屏障里根本没有 pending，只可能是 zone Redis 丢了已确认的写
// (崩溃、切主、从备份恢复)、手工改写或合服拷错。ACK 仍然照写：它是已提交的 SQL 事实，login 核验回退正要靠它；
// 拒写会丢掉这份证据，还会让补投永远等不到两表 ACK 追平 pending。脚本只把领先种类带回，由 Go 侧计数并打日志。
// ACK 没被写(已有 ACK ≥ 本次版本)时不带标志、不重复报：那份 ACK 写下时若领先 pending，当时已经报过。
// pending 存在却不是规范版本串时，与已存 ACK 畸形同口径，在任何写之前报错。
const playerCachePublishLua = savebarrier.LuaVersionHelpers + `
if redis.call('GET',KEYS[1]) ~= ARGV[1] then return 0 end
if not tonumber(ARGV[2]) or tonumber(ARGV[2])<=0 or not tonumber(ARGV[4]) or tonumber(ARGV[4])<=0 then return redis.error_reply('invalid publish TTL') end
local writeEvidence=false
local result=1
if ARGV[7]=='1' then
 if not is_version(ARGV[5]) then return redis.error_reply('asset sql commit version is malformed') end
 local old=redis.call('HGET',KEYS[3],'version')
 if not old then writeEvidence=true
 elseif not is_version(old) then return redis.error_reply('stored asset sql commit version is malformed')
 elseif old==ARGV[5] then
  if redis.call('HGET',KEYS[3],'data')~=ARGV[6] then return -1 end
 elseif version_gt(ARGV[5],old) then writeEvidence=true
 else result=2 end
end
local writeBarrier=false
local ackAhead=0
if ARGV[8]~='' then
 if not is_version(ARGV[5]) then return redis.error_reply('save barrier version is malformed') end
 local stored=redis.call('HMGET',KEYS[4],ARGV[8],'pending')
 local old,pending=stored[1],stored[2]
 if old and not is_version(old) then return redis.error_reply('stored save barrier version is malformed') end
 if pending and not is_version(pending) then return redis.error_reply('stored save barrier pending is malformed') end
 writeBarrier=not old or version_gt(ARGV[5],old)
 if writeBarrier and not pending then ackAhead=20
 elseif writeBarrier and version_gt(ARGV[5],pending) then ackAhead=10 end
end
redis.call('PEXPIRE',KEYS[1],ARGV[2])
redis.call('SET',KEYS[2],ARGV[3],'PX',ARGV[4])
if writeEvidence then redis.call('HSET',KEYS[3],'version',ARGV[5],'data',ARGV[6]) end
if writeBarrier then redis.call('HSET',KEYS[4],ARGV[8],ARGV[5]) end
return result+ackAhead
`

var playerCachePublishScript = redis.NewScript(playerCachePublishLua)

// 发布脚本回复的个位：发布结局。
const (
	cachePublishNotOwner         = 0  // 已不持锁，什么都没写
	cachePublishWritten          = 1  // 缓存已写；需要发布的证据已写或已是同版本同内容
	cachePublishEvidenceAhead    = 2  // 缓存已写；已有证据版本高于本次，证据不回退
	cachePublishEvidenceConflict = -1 // 同一版本的证据内容不同，什么都没写
)

// 发布脚本回复的十位：本次写下的屏障 ACK 领先 pending 的种类，只会叠加在 cachePublishWritten / EvidenceAhead 上。
//
// 压进同一个整数而不回数组：令牌脚本经 writeLease.runTokenScript → locker.RunTokenScript 执行，那条约定只认整数
// 回复(0 = 失锁，>0 = 已写并续期)。叠加后的值仍为正，失锁与续期语义原样沿用，不必为一个旁路标志改租约接口。
// Lua 里写的是字面量 10 / 20，与这里一一对应，由 TestDecodeCachePublishReply 钉住。
const (
	ackAheadUnit           = 10
	ackAheadNone           = 0
	ackAheadAbovePending   = 1 // 本次 ACK 严格大于 pending
	ackAheadPendingMissing = 2 // 屏障里没有 pending
)

// decodeCachePublishReply 把发布脚本的整数回复拆成发布结局与 ACK 领先码。取值越出约定说明脚本与解码不同步，报错而不猜。
func decodeCachePublishReply(reply int64) (outcome, ackAhead int64, err error) {
	switch {
	case reply == cachePublishNotOwner || reply == cachePublishEvidenceConflict:
		return reply, ackAheadNone, nil
	case reply > 0:
		outcome, ackAhead = reply%ackAheadUnit, reply/ackAheadUnit
		if (outcome == cachePublishWritten || outcome == cachePublishEvidenceAhead) && ackAhead <= ackAheadPendingMissing {
			return outcome, ackAhead, nil
		}
	}
	return 0, 0, fmt.Errorf("unexpected cache publication result %d", reply)
}

const appliedAdvanceLua = luaDecimalGreaterFn + `
if redis.call('GET',KEYS[1])~=ARGV[1] then return 0 end
local e=ARGV[4]
if not is_dec(e) or #e>20 or (#e==20 and dec_gt(e,'18446744073709551615')) then return redis.error_reply('task owner epoch is not canonical uint64') end
local cur=false
if e~='0' then
 cur=redis.call('GET',KEYS[3])
 if cur and (not is_dec(cur) or #cur>20 or (#cur==20 and dec_gt(cur,'18446744073709551615'))) then return redis.error_reply('stored owner epoch is not canonical uint64') end
end
if not tonumber(ARGV[2]) or tonumber(ARGV[2])<=0 then return redis.error_reply('invalid applied TTL') end
redis.call('PEXPIRE',KEYS[1],ARGV[2])
if ARGV[3]~='' then redis.call('SET',KEYS[2],ARGV[3]) end
if e~='0' and (not cur or dec_gt(e,cur)) then redis.call('SET',KEYS[3],e) end
return 1
`

var appliedAdvanceScript = redis.NewScript(appliedAdvanceLua)

func publishDBWriteResult(ctx context.Context, lease writeLease, task *dbpb.DBTask, msg proto.Message) error {
	raw, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	cache := buildCacheKey(task)
	keys := []string{cache, cache, cache}
	version, evidence, hasEvidence, barrierField := "", []byte(nil), "0", ""
	if fencingDual.Load() && task.OwnerEpoch != 0 && task.SaveSeq != 0 && isFenceTable(msg.ProtoReflect().Descriptor()) {
		version = savebarrier.Version(task.OwnerEpoch, task.SaveSeq)
		barrierField = task.MsgType
		keys[2] = savebarrier.Key(task.Key)
		if task.MsgType == "player_database" {
			field := msg.ProtoReflect().Descriptor().Fields().ByName("asset_op_ledger")
			ledger, marshalErr := (proto.MarshalOptions{Deterministic: true}).Marshal(msg.ProtoReflect().Get(field).Message().Interface())
			if marshalErr != nil {
				return marshalErr
			}
			evidence, err = (proto.MarshalOptions{Deterministic: true}).Marshal(&assetpb.PlayerAssetSQLCommit{PlayerId: task.Key, OwnerEpoch: task.OwnerEpoch, SaveSeq: task.SaveSeq, LedgerProto: ledger})
			if err != nil {
				return err
			}
			keys[1] = assetop.SQLCommitKey(task.Key)
			hasEvidence = "1"
		}
	}
	start := time.Now()
	result, err := lease.runTokenScript(ctx, playerCachePublishScript, keys, raw, cacheTTL().Milliseconds(), version, evidence, hasEvidence, barrierField)
	cost := time.Since(start)
	metrics.ObserveStage(metrics.StageCacheWrite, "write", cost)
	metrics.ObserveRedisStep("write", metrics.RedisStepCachePublish, cost)
	if err != nil {
		return err
	}
	outcome, ackAhead, err := decodeCachePublishReply(result)
	if err != nil {
		return err
	}
	// decodeCachePublishReply 已保证 outcome 落在这四个取值里。
	switch outcome {
	case cachePublishNotOwner:
		metrics.CountFenceCache("write", "not_owner")
		return fmt.Errorf("%w: cache publisher no longer owns lock", errOrderingLeaseLost)
	case cachePublishEvidenceConflict:
		metrics.CountFenceCache("write", "evidence_conflict")
		logx.Errorf("ASSET-SQL-COMMIT-CONFLICT taskID=%s version=%s", task.TaskId, version)
		return errFenceEvidenceConflict
	case cachePublishWritten:
		metrics.CountFenceCache("write", "written")
	case cachePublishEvidenceAhead:
		metrics.CountFenceCache("write", "written")
		metrics.CountFenceCache("write", "evidence_ahead")
		logx.Errorf("ASSET-SQL-COMMIT-AHEAD taskID=%s version=%s", task.TaskId, version)
	}
	reportSaveBarrierAckAhead(task, version, ackAhead)
	return nil
}

// reportSaveBarrierAckAhead 记一次「ACK 领先 pending」的回退证据：计数并打 ERROR；ACK 已照写，任务照常确认。
// 玩家 ID 只进日志，不进指标 label。回复只有一个整数，带不回 pending 原值：above_pending 时日志只能写明
// pending 低于 ack，具体值按日志里的 barrierKey 去读屏障。
func reportSaveBarrierAckAhead(task *dbpb.DBTask, ack string, ackAhead int64) {
	var kind, pending string
	switch ackAhead {
	case ackAheadAbovePending:
		kind, pending = metrics.SaveBarrierAckAbovePending, "below_ack"
	case ackAheadPendingMissing:
		kind, pending = metrics.SaveBarrierAckPendingMissing, "missing"
	default:
		return
	}
	metrics.CountSaveBarrierAckAhead(kind)
	logx.Errorf("SAVE-BARRIER-ACK-AHEAD kind=%s playerID=%d table=%s ack=%s pending=%s barrierKey=%s taskID=%s",
		kind, task.Key, task.MsgType, ack, pending, savebarrier.Key(task.Key), task.TaskId)
}

func advanceApplied(ctx context.Context, lease writeLease, topic string, dbTask *dbpb.DBTask, task *workerTask) error {
	if dbTask.Op != dbpb.DBOp_DB_OP_WRITE {
		return nil
	}
	cursor := ""
	if task.seq != 0 {
		cursor = encodeAppliedCursor(appliedCursor{seq: task.seq, partition: task.originPartition, hasPartition: task.hasOriginPartition})
	}
	start := time.Now()
	result, err := lease.runTokenScript(ctx, appliedAdvanceScript, []string{appliedSeqKey(topic, dbTask.Key, dbTask.MsgType), appliedEpochKey(topic, dbTask.Key, dbTask.MsgType)}, cursor, strconv.FormatUint(dbTask.OwnerEpoch, 10))
	metrics.ObserveRedisStep("write", metrics.RedisStepAppliedAdvance, time.Since(start))
	if err != nil {
		return err
	}
	if result == 0 {
		return fmt.Errorf("%w: applied publisher no longer owns lock", errOrderingLeaseLost)
	}
	return nil
}
