package kafka

import (
	"context"
	"errors"
	"time"

	"db/internal/dbguard"
	"db/internal/saveoutbox"

	"github.com/zeromicro/go-zero/core/logx"
	playerpb "proto/common/database"
	db_proto "proto/db"
)

// 消费侧给存盘补投器留下的两类事实(data-overwrite-rollback-audit-20261018.md §2.2 go-db-pipeline#1):
//   - 终态:这一版本的严格写被确定性拒绝(进死信的围栏 / 归属 / 路由结局、Redis 归属守卫判陈旧、大字段闸、资产证据冲突)。
//     同一版本重投只会再被拒一次、再进一条接近整行大小的死信,补投器见到终态即停;载荷由死信或原意图保全,交人工处置。
//   - 在途重试:这名玩家的严格写刚被延后进重试队列。补投器在窗口内只推迟,不再另起一条重试链。
// 两者都只在 dual 下写(legacy 没有补投),只针对带 (owner_epoch, save_seq) 的两张玩家表写,且只是给补投器的提示:
// 写失败不影响本任务的终态处理,代价是补投器按退避与次数上限多投几次(有界)。

// saveOutboxNoteTimeout 是单次提示的 Redis 预算;不从 w.ctx 派生取消,停机途中也只多等这一点。
const saveOutboxNoteTimeout = 500 * time.Millisecond

var (
	playerDatabaseMsgType  = string((&playerpb.PlayerDatabase{}).ProtoReflect().Descriptor().FullName())
	playerDatabase1MsgType = string((&playerpb.PlayerDatabase_1{}).ProtoReflect().Descriptor().FullName())
)

// isSaveBarrierWrite 判定任务是否受父快照提交屏障约束:dual、严格版本、两张玩家表之一的写。
func isSaveBarrierWrite(t *db_proto.DBTask) bool {
	return fencingDual.Load() && t != nil && t.Op == db_proto.DBOp_DB_OP_WRITE && t.OwnerEpoch != 0 && t.SaveSeq != 0 &&
		(t.MsgType == playerDatabaseMsgType || t.MsgType == playerDatabase1MsgType)
}

// deterministicWriteRejectReason 识别重放同一载荷必然再次失败的延后原因;其余失败(存储、锁、Redis、失租)都可能自愈,不打终态。
func deterministicWriteRejectReason(cause error) (string, bool) {
	var tooLarge *dbguard.ErrBlobTooLarge
	switch {
	case errors.As(cause, &tooLarge):
		return "blob_too_large", true
	case errors.Is(cause, errFenceEvidenceConflict):
		return "asset_evidence_conflict", true
	}
	return "", false
}

// markSaveOutboxTerminal 在确定性拒写的决策点调用(不等死信入队确认:决策本身已经是这一版本的终局)。
func (w *worker) markSaveOutboxTerminal(t *db_proto.DBTask, reason string) {
	if !isSaveBarrierWrite(t) || w.redisClient == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(w.ctx), saveOutboxNoteTimeout)
	defer cancel()
	if _, err := saveoutbox.MarkTerminal(ctx, w.redisClient, t.Key, t.OwnerEpoch, t.SaveSeq, reason); err != nil {
		logx.Errorf("save outbox terminal mark failed; replay stays bounded by backoff and attempt limit: taskID=%s reason=%s err=%v",
			t.TaskId, reason, err)
	}
}

// noteSaveOutboxDeferral 在 deferFailedTask 入口调用:确定性原因打终态,其余记在途重试。
func (w *worker) noteSaveOutboxDeferral(t *db_proto.DBTask, cause error) {
	if !isSaveBarrierWrite(t) || w.redisClient == nil {
		return
	}
	if reason, ok := deterministicWriteRejectReason(cause); ok {
		w.markSaveOutboxTerminal(t, reason)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(w.ctx), saveOutboxNoteTimeout)
	defer cancel()
	if err := saveoutbox.NoteRetryDeferred(ctx, w.redisClient, t.Key, time.Now()); err != nil {
		// 存储或 Redis 故障期间每条延后都会走到这里,逐条 ERROR 会刷屏;没记上只会让补投按退避多投一次。
		logx.Debugf("save outbox retry stamp failed: taskID=%s err=%v", t.TaskId, err)
	}
}
