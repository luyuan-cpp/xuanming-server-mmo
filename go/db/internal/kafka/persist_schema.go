package kafka

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"db/internal/metrics"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	db_proto "proto/db"
	"shared/generated/persistschema"
)

// 玩家持久 schema 世代校验(docs/design/mixed-version-persist-field-protection.md §6.2)。
//
// 两张玩家表消息带覆盖世代 persist_schema_gen:这份记录里该世代及以前定义的字段,值都是真实的。go/db 只对这两张表判定:
//
//	C0  写,进 SQL 之前:本 zone 引导期已关闭,且 body 的戳原值为 0      → 确定性拒写进死信(persist_unstamped)
//	C1  写,进 SQL 之前:body 世代 > 本二进制世代                      → 延后,不耗重试预算(schema_too_new)
//	C1b 写,围栏判定之后、Save 之前(同一把行锁):body 世代 < 行世代   → 确定性拒写进死信(schema_regression)
//	C2  读,SELECT 之后:行世代 > 本二进制世代                         → 立即回失败回执,不回填缓存(schema_too_new)
//
// 比较前戳为 0 一律按 1 处理(§3.3)。C1 只看世代号,不看「body 有没有顶层未知字段」:proto3 不序列化零值,
// 新写方把新列清零时 body 里根本没有这个字段(评审 S3)。
const persistSchemaGenField protoreflect.Name = "persist_schema_gen"

// errSchemaTooNew:任务(写)或行(读)的世代高于本二进制,本实例保存或读出的都会是截断副本。
var errSchemaTooNew = errors.New("persist schema generation is newer than this go/db binary")

// 确定性拒写的写结局,走 quarantineOrderingConflict 进死信。
const (
	writePersistUnstamped = "persist_unstamped" // C0
	writeSchemaRegression = "schema_regression" // C1b
)

// PersistBootstrapClosedValue 是引导关闭标记唯一合法的值。
const PersistBootstrapClosedValue = "closed"

// persistBootstrapRecheckInterval 是引导关闭标记的复查周期,与存盘协议世代的 legacy 复查同节拍。
const persistBootstrapRecheckInterval = 5 * time.Second

// persistBootstrapClosed 为 true 时 C0 生效。启动时由 LoadPersistBootstrapState 读一次,之后由 WatchPersistBootstrap 刷新;
// 只会从 false 变成 true:引导期一旦关闭就不再回到「接受未盖戳写」,标记被误删也不放宽。
var persistBootstrapClosed atomic.Bool

// PersistBootstrapClosedKey 是 zone 级引导关闭标记。与父快照、save_barrier、存盘协议世代标记同一 Redis 实例,
// 由负责人在确认全网已无机制前写方后手工写入(§8.1 第 4 步):SET <key> closed,不带 TTL。
func PersistBootstrapClosedKey(zone uint32) string {
	return fmt.Sprintf("db:persist_schema:bootstrap_closed:zone:%d", zone)
}

// readPersistBootstrapClosed 读标记:不存在为 false;值为 closed 为 true;其余取值、类型不对、Redis 出错都返回错误。
func readPersistBootstrapClosed(ctx context.Context, rc redis.Cmdable, zone uint32) (bool, error) {
	v, err := rc.Get(ctx, PersistBootstrapClosedKey(zone)).Result()
	switch {
	case errors.Is(err, redis.Nil):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("读取持久 schema 引导关闭标记 %s 失败: %w", PersistBootstrapClosedKey(zone), err)
	case v != PersistBootstrapClosedValue:
		return false, fmt.Errorf("持久 schema 引导关闭标记 %s 的值 %q 不认识(只接受 %q)", PersistBootstrapClosedKey(zone), v, PersistBootstrapClosedValue)
	}
	return true, nil
}

// LoadPersistBootstrapState 在消费者启动之前调用一次:读引导关闭标记并记下本二进制世代。
// 失败即拒启(fail-closed):判不清引导期是否关闭时,放行未盖戳写可能让机制前的写方覆盖新数据。
func LoadPersistBootstrapState(ctx context.Context, rc redis.Cmdable, zone uint32) error {
	if rc == nil || zone == 0 {
		return errors.New("持久 schema 引导状态缺少 Redis 或 zone")
	}
	closed, err := readPersistBootstrapClosed(ctx, rc, zone)
	if err != nil {
		return err
	}
	setPersistBootstrapClosed(closed)
	metrics.SetPersistSchemaGen(persistschema.Gen)
	return nil
}

// WatchPersistBootstrap 周期复查引导关闭标记,直到 ctx 取消。读失败、取值不认识只记日志,保持当前状态:
// 状态只会从开放变成关闭,复查失败时维持现状不会放宽。
func WatchPersistBootstrap(ctx context.Context, rc redis.Cmdable, zone uint32) {
	ticker := time.NewTicker(persistBootstrapRecheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		stepCtx, cancel := context.WithTimeout(ctx, persistBootstrapRecheckInterval)
		err := refreshPersistBootstrap(stepCtx, rc, zone)
		cancel()
		if err != nil && ctx.Err() == nil {
			logx.Errorf("[PersistSchema] 引导关闭标记复查失败,维持当前状态,下一拍重试: %v", err)
		}
	}
}

// refreshPersistBootstrap 是复查的一拍,单独拆出来便于不依赖墙钟地测试。已关闭时不再读 Redis。
func refreshPersistBootstrap(ctx context.Context, rc redis.Cmdable, zone uint32) error {
	if persistBootstrapClosed.Load() {
		return nil
	}
	closed, err := readPersistBootstrapClosed(ctx, rc, zone)
	if err != nil {
		return err
	}
	if closed {
		setPersistBootstrapClosed(true)
		logx.Infof("[PersistSchema] zone=%d 引导期已关闭:此后未盖戳的玩家表写进死信(persist_unstamped)", zone)
	}
	return nil
}

func setPersistBootstrapClosed(closed bool) {
	if closed {
		persistBootstrapClosed.Store(true)
	}
	metrics.SetPersistSchemaBootstrapClosed(persistBootstrapClosed.Load())
}

// persistSchemaFieldOf 返回玩家表消息的世代字段;不是带世代字段的玩家表时返回 nil。
func persistSchemaFieldOf(desc protoreflect.MessageDescriptor) protoreflect.FieldDescriptor {
	f := desc.Fields().ByName(persistSchemaGenField)
	if f == nil || f.Kind() != protoreflect.Uint32Kind || f.IsList() || f.IsMap() || f.HasOptionalKeyword() {
		return nil
	}
	return f
}

// requirePersistSchemaField 启动期确认两张玩家表都有世代字段;缺了说明 proto 没重新生成,go/db 不能按世代判定。
func requirePersistSchemaField(msg proto.Message) error {
	if persistSchemaFieldOf(msg.ProtoReflect().Descriptor()) == nil {
		return fmt.Errorf("玩家表 %T 缺少 uint32 字段 %s;先按 AGENTS §4 重新生成 proto", msg, persistSchemaGenField)
	}
	return nil
}

// effectiveGen 把戳归一:0(未盖戳)按世代 1 处理。
func effectiveGen(raw uint64) uint64 {
	if raw == 0 {
		return 1
	}
	return raw
}

// checkWriteSchema 在进 SQL 之前判定 C0 / C1。只对带世代字段的玩家表生效,其余表返回 ("", nil)。
// 返回非空 kind 表示确定性拒写(C0);返回包着 errSchemaTooNew 的错误表示延后(C1)。
func checkWriteSchema(task *db_proto.DBTask, msg proto.Message) (string, error) {
	if task.GetOp() != db_proto.DBOp_DB_OP_WRITE || !isFenceTable(msg.ProtoReflect().Descriptor()) {
		return "", nil
	}
	field := persistSchemaFieldOf(msg.ProtoReflect().Descriptor())
	if field == nil {
		// 启动期 requirePersistSchemaField 已拦住;运行中走到这里是装配错误,按延后处理,不写半截数据。
		return "", fmt.Errorf("%w: table %s has no %s field", errSchemaTooNew, msg.ProtoReflect().Descriptor().FullName(), persistSchemaGenField)
	}
	raw := msg.ProtoReflect().Get(field).Uint()
	if raw == 0 {
		metrics.CountPersistSchemaUnstampedWrite()
		if persistBootstrapClosed.Load() {
			return writePersistUnstamped, nil
		}
	}
	if effectiveGen(raw) > uint64(persistschema.Gen) {
		return "", fmt.Errorf("%w: taskID=%s msgType=%s task_gen=%d binary_gen=%d",
			errSchemaTooNew, task.GetTaskId(), task.GetMsgType(), raw, persistschema.Gen)
	}
	return "", nil
}

// schemaRegressed 在行锁之下判定 C1b:body 世代低于已落库行的世代。两者任一没有世代字段时不判定。
func schemaRegressed(target, current proto.Message) bool {
	field := persistSchemaFieldOf(target.ProtoReflect().Descriptor())
	if field == nil {
		return false
	}
	return effectiveGen(target.ProtoReflect().Get(field).Uint()) < effectiveGen(current.ProtoReflect().Get(field).Uint())
}

// checkReadSchema 在 SELECT 之后判定 C2。只对带世代字段的玩家表生效;行不存在时(全零消息)世代按 1,不拦。
func checkReadSchema(task *db_proto.DBTask, msg proto.Message) error {
	field := persistSchemaFieldOf(msg.ProtoReflect().Descriptor())
	if field == nil || !isFenceTable(msg.ProtoReflect().Descriptor()) {
		return nil
	}
	if raw := msg.ProtoReflect().Get(field).Uint(); effectiveGen(raw) > uint64(persistschema.Gen) {
		return fmt.Errorf("%w: taskID=%s msgType=%s row_gen=%d binary_gen=%d",
			errSchemaTooNew, task.GetTaskId(), task.GetMsgType(), raw, persistschema.Gen)
	}
	return nil
}

// logSchemaTooNew 限速记录 C1 延后:go/db 回滚或违反发布顺序时,同一 key 的每次存盘都会走到这里。
// 持续多久看 db_task_deferred_total{reason="schema_too_new"}。
func logSchemaTooNew(dbTask *db_proto.DBTask, cause error) {
	ok, suppressed := storeLogLimiter.allow(metrics.DeferSchemaTooNew, time.Now())
	if !ok {
		return
	}
	logx.Errorf("task deferred, persist schema generation newer than this go/db (retry budget kept for writes; upgrade go/db or finish the rollout): taskID=%s op=%s suppressed_since_last=%d cause=%v",
		dbTask.GetTaskId(), taskOpLabel(dbTask), suppressed, cause)
}

// failReadSchemaTooNew 以失败回执结束一条读(C2):旧版 go/db 读不到新列,冷加载拿到的会是截断副本。
// 立即回失败、不回填缓存、不进重试:等待方(login 预加载)按既有失败路径收尾,玩家稍后重试时由新版实例处理。
func (w *worker) failReadSchemaTooNew(task *workerTask, dbTask *db_proto.DBTask, cause error) {
	w.failReadAuthority(task, dbTask, metrics.ReadAbandonedSchemaTooNew, cause)
}

var errMissingPlayerSQLRow = errors.New("玩家 SQL 权威历史行缺失")

// 两种确定的权威读拒绝均不进入重试；失败通知发布失败仍沿原可靠延后，不丢原任务。
func (w *worker) failReadAuthority(task *workerTask, dbTask *db_proto.DBTask, reason string, cause error) {
	if dbTask.TaskId != "" {
		failure := &db_proto.TaskResult{Success: false, Error: reason}
		if err := publishTaskResult(w.ctx, w.redisClient, dbTask, failure); err != nil {
			w.deferFailedTask(task, dbTask, fmt.Errorf("publish authority read failure result: %w", err))
			return
		}
	}
	metrics.CountReadAbandoned(reason)
	if ok, suppressed := storeLogLimiter.allow("read_"+reason, time.Now()); ok {
		logx.Errorf("权威读取已回失败，不发布玩家缓存: reason=%s taskID=%s msgType=%s suppressed_since_last=%d cause=%v",
			reason, dbTask.TaskId, dbTask.MsgType, suppressed, cause)
	}
	if task.fromRetry {
		if err := ackRetryReceipt(w.ctx, w.redisClient, w.retryProcessingKey, task.retryReceipt); err != nil {
			logx.Errorf("ack schema-too-new read retry receipt failed; it remains recoverable: taskID=%s err=%v", dbTask.TaskId, err)
		}
	}
	ackKafkaTask(task)
}
