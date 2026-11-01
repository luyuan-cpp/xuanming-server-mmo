package authoritysql

import (
	"bytes"
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync/atomic"

	"google.golang.org/protobuf/proto"
	databasepb "proto/common/database"
)

const WriterSlotCount = 64

var ErrWriterDenied = errors.New("SQL writer generation、epoch、主体或ACTIVE资格不符")

// ErrWriterFrozen 表示本逻辑库的闸门被冻结:route_meta.writable=0,或事务进入期间 epoch 变了。
// 搬库切换窗口里业务看到的就是它,调用方原样重试即可(RPC 回 Unavailable,存盘按原序号退避)。
// 资格不符(主体、世代、已退休)仍是 ErrWriterDenied,那种错误重试没用。
// errors.Is(ErrWriterFrozen, ErrWriterDenied) 为真:只关心「没写进去」的调用方不用改,要区分时先判 ErrWriterFrozen。
var ErrWriterFrozen = fmt.Errorf("%w: 写者闸门已冻结", ErrWriterDenied)

// writerObservation 记这条 ctx 下的 SQL 撞过什么。frozen:撞过冻结的闸门。unknown:有过结局未知的语句或提交。
// noWriteRetry:事务回滚之后撞上「确定没写、重试就行」的失败,目前只有角色护栏自愈读不到目录。
type writerObservation struct {
	frozen       atomic.Bool
	unknown      atomic.Bool
	noWriteRetry atomic.Bool
}

type writerObservationKey struct{}

// ObserveWriterFrozen 返回一条带观察位的 ctx 和读观察位的函数:这条 ctx 下的 SQL 只要撞过冻结的闸门,观察位就置上。
// RPC 拦截器靠它把「业务层已经把错误翻成带内错误码」的失败也回成 Unavailable,不用改每个 handler 的错误映射。
func ObserveWriterFrozen(ctx context.Context) (context.Context, func() bool) {
	seen := &writerObservation{}
	return context.WithValue(ctx, writerObservationKey{}, seen), seen.frozen.Load
}

// NoteWriterFrozen 在 err 是 ErrWriterFrozen 时置上 ctx 的观察位。连接层在闸门上调它。
func NoteWriterFrozen(ctx context.Context, err error) {
	if !errors.Is(err, ErrWriterFrozen) {
		return
	}
	if seen, ok := ctx.Value(writerObservationKey{}).(*writerObservation); ok {
		seen.frozen.Store(true)
	}
}

// NoteWriterOutcomeUnknown 在 err 是结局未知时置上 ctx 的观察位。连接层在判定结局未知的地方调它。
func NoteWriterOutcomeUnknown(ctx context.Context, err error) {
	if !errors.Is(err, ErrOutcomeUnknown) {
		return
	}
	if seen, ok := ctx.Value(writerObservationKey{}).(*writerObservation); ok {
		seen.unknown.Store(true)
	}
}

// WriterOutcomeUnknownSeen 报告这条 ctx 下的 SQL 有没有过结局未知。handler 常把存储错误翻成带内错误码,
// 哨兵错误到不了拦截器,所以要判「这次确定没写」只能看这个观察位。ctx 必须来自 ObserveWriterFrozen。
func WriterOutcomeUnknownSeen(ctx context.Context) bool {
	seen, ok := ctx.Value(writerObservationKey{}).(*writerObservation)
	return ok && seen.unknown.Load()
}

// NoteNoWriteRetry 置上「这次确定没写、重试就行」的观察位。调用方要能证明事务已回滚。
// handler 把这种失败翻成故障码时,拦截器靠它改回可重试的状态,不打排空 Hold(serverbase.noWriteRetryFailure)。
func NoteNoWriteRetry(ctx context.Context) {
	if seen, ok := ctx.Value(writerObservationKey{}).(*writerObservation); ok {
		seen.noWriteRetry.Store(true)
	}
}

// NoWriteRetrySeen 报告这条 ctx 下有没有过 NoteNoWriteRetry。ctx 必须来自 ObserveWriterFrozen。
func NoWriteRetrySeen(ctx context.Context) bool {
	seen, ok := ctx.Value(writerObservationKey{}).(*writerObservation)
	return ok && seen.noWriteRetry.Load()
}

// IsWriterFrozen 报告 err 是不是「闸门冻结,这次确定没写」:可以原样重试,不算结局未知,也不该打成排空 HOLD。
//
// 比 errors.Is(err, ErrWriterFrozen) 严:一轮后台工作常把多个错误 Join 起来,只要混进一个别的失败(尤其 ErrOutcomeUnknown),
// 这一轮就不能按「确定没写」处理。所以要求错误树的每个叶子都是冻结。ErrOutcomeUnknown 的实现故意不暴露原因(unknownError 没有 Unwrap),
// 连接层把冻结升级成未知(比如回滚没确认)之后这里读到的就是假。
func IsWriterFrozen(err error) bool {
	if err == nil {
		return false
	}
	if err == ErrWriterFrozen {
		return true
	}
	switch e := err.(type) {
	case interface{ Unwrap() []error }:
		frozen := false
		for _, child := range e.Unwrap() {
			if child == nil {
				continue
			}
			if !IsWriterFrozen(child) {
				return false
			}
			frozen = true
		}
		return frozen
	case interface{ Unwrap() error }:
		return IsWriterFrozen(e.Unwrap())
	}
	return false
}

// Outcome 是一次权威 SQL 调用的结局分类。后台循环和排空判定按它决定要不要 HOLD。
type Outcome uint8

const (
	// OutcomeOK:没有错误。
	OutcomeOK Outcome = iota
	// OutcomeFrozen:闸门冻结,这笔确定没写,原样重试。
	OutcomeFrozen
	// OutcomeUnknown:可能已经提交,禁止自动重放。
	OutcomeUnknown
	// OutcomeFailed:别的失败(语句被拒、资格不符、取消)。写过没有由调用方的路径决定,这里不替它下结论。
	OutcomeFailed
)

// ClassifyOutcome 把错误归到 Outcome。错误树里只要有一个结局未知,整体就是未知。
func ClassifyOutcome(err error) Outcome {
	switch {
	case err == nil:
		return OutcomeOK
	case errors.Is(err, ErrOutcomeUnknown):
		return OutcomeUnknown
	case IsWriterFrozen(err):
		return OutcomeFrozen
	default:
		return OutcomeFailed
	}
}

type runtimeGate struct{ config WriterConfig }
type runtimeAttempt struct {
	config WriterConfig
	before *databasepb.RouteMeta
	slot   uint32
}
type passGate struct{}

func (passGate) beforeBegin(context.Context, driver.Conn) (gateAttempt, error) {
	return passGate{}, nil
}
func (passGate) enter(context.Context, driver.Conn) error { return nil }

// 每条物理连接只读核永久REGISTER全文；复位后也重核，不以可读UUID替代boot绑定。
func verifyBootRegistration(ctx context.Context, conn driver.Conn, grant *databasepb.SqlWriterBootGrant) error {
	if grant == nil {
		return ErrWriterDenied
	}
	values, err := rawOne(ctx, conn, "SELECT generation,instance_id,sql_principal,registration_digest,state FROM "+qualified(grant.AuthoritySchema, "writer_registry")+" WHERE writer_key=?", []driver.NamedValue{{Ordinal: 1, Value: grant.WriterKey}}, 5)
	if err != nil {
		return err
	}
	state, err := unsignedValue(values[4])
	if err != nil || state != 1 || stringValue(values[0]) != grant.Generation || !bytes.Equal(byteValue(values[1]), grant.InstanceId) || !bytes.Equal(byteValue(values[2]), grant.SqlPrincipal) || !bytes.Equal(byteValue(values[3]), grant.RegistrationDigest) {
		return ErrWriterDenied
	}
	values, err = rawOne(ctx, conn, "SELECT request_proto FROM "+qualified(grant.AuthoritySchema, "writer_lifecycle_receipt")+" WHERE kind=1 AND request_digest=?", []driver.NamedValue{{Ordinal: 1, Value: grant.RegistrationDigest}}, 1)
	if err != nil {
		return err
	}
	request := &databasepb.SqlWriterLifecycleReceipt_Request{}
	if err = proto.Unmarshal(byteValue(values[0]), request); err != nil || hasUnknownControl(request) {
		return ErrWriterDenied
	}
	if request.Writer == nil || !bytes.Equal(request.Writer.RegistrationDigest, grant.RegistrationDigest) {
		return ErrWriterDenied
	}
	canonical, err := CanonicalRegistrationRequest(request)
	if err != nil {
		return err
	}
	w := canonical.Writer
	if canonical.AuthoritySchema != grant.AuthoritySchema || !equalStrings(canonical.ProtectedSchemas, grant.ProtectedSchemas) || !bytes.Equal(canonical.DeploymentDigest, grant.PhysicalAuthoritySha256) || canonical.DeploymentId != grant.DeploymentId || canonical.LockPrimitive != grant.LockPrimitive || !bytes.Equal(canonical.LockQualificationSha256, grant.LockQualificationSha256) || !bytes.Equal(canonical.MaintenanceRunId, grant.MaintenanceRunId) || !bytes.Equal(w.WriterKey, grant.WriterKey) || w.Generation != grant.Generation || !bytes.Equal(w.InstanceId, grant.InstanceId) || !bytes.Equal(w.SqlPrincipal, grant.SqlPrincipal) || !bytes.Equal(w.RegistrationDigest, grant.RegistrationDigest) {
		return ErrWriterDenied
	}
	return nil
}

func (g runtimeGate) beforeBegin(ctx context.Context, conn driver.Conn) (gateAttempt, error) {
	if g.config.boot == nil || g.config.boot.closed.Load() {
		return nil, ErrWriterDenied
	}
	meta, err := readRawMeta(ctx, conn, g.config.AuthoritySchema())
	if err != nil {
		return nil, err
	}
	if meta.Generation != g.config.Generation() || meta.WriterEpoch == 0 {
		return nil, ErrWriterDenied
	}
	if meta.Writable != 1 {
		g.config.boot.noteFrozen()
		return nil, ErrWriterFrozen
	}
	slot := uint32((g.config.boot.next.Add(1) - 1) % WriterSlotCount)
	return &runtimeAttempt{config: g.config, before: meta, slot: slot}, nil
}
func (a *runtimeAttempt) enter(ctx context.Context, conn driver.Conn) error {
	grant := a.config.boot.grant
	if a.config.boot.closed.Load() {
		return ErrWriterDenied
	}
	query := "SELECT generation,writer_epoch,state FROM " + qualified(grant.AuthoritySchema, "writer_slot") + " WHERE writer_key=? AND slot=?"
	args := []driver.NamedValue{{Ordinal: 1, Value: grant.WriterKey}, {Ordinal: 2, Value: int64(a.slot)}}
	touched := true
	if grant.LockPrimitive == databasepb.SqlWriterBootGrant_LOCK_TOUCH {
		result, err := rawExec(ctx, conn, "UPDATE "+qualified(grant.AuthoritySchema, "writer_slot")+" SET lock_touch=1-lock_touch WHERE writer_key=? AND slot=? AND generation=? AND writer_epoch=? AND state=1 AND lock_touch IN (0,1)", append(args, driver.NamedValue{Ordinal: 3, Value: grant.Generation}, driver.NamedValue{Ordinal: 4, Value: a.before.WriterEpoch}))
		if err != nil {
			return err
		}
		if result == nil {
			return ErrWriterDenied
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return ErrWriterDenied
		}
		touched = affected == 1
	} else if grant.LockPrimitive == databasepb.SqlWriterBootGrant_SELECT_ONLY {
		query += " FOR UPDATE"
	} else {
		return ErrWriterDenied
	}
	values, err := rawOne(ctx, conn, query, args, 3)
	if err != nil {
		return err
	}
	epoch, e1 := unsignedValue(values[1])
	state, e2 := unsignedValue(values[2])
	if e1 != nil || e2 != nil || stringValue(values[0]) != grant.Generation || state != 1 {
		return ErrWriterDenied
	}
	// 条件写没命中、这里却读到 ACTIVE:epoch 被推进了(RR 旧快照可能还读到旧 epoch),按冻结处理,下一次尝试会读到真相。
	if !touched || epoch != a.before.WriterEpoch {
		a.config.boot.noteFrozen()
		return ErrWriterFrozen
	}
	// 退休永久REVOKED与freeze推进epoch都落在已CURRENT锁住的slot原件上，RR旧snapshot不能放行。
	meta, err := readRawMeta(ctx, conn, grant.AuthoritySchema)
	if err != nil {
		return err
	}
	if meta.Generation != grant.Generation {
		return ErrWriterDenied
	}
	if meta.Writable != 1 || meta.WriterEpoch != epoch {
		a.config.boot.noteFrozen()
		return ErrWriterFrozen
	}
	values, err = rawOne(ctx, conn, "SELECT generation,instance_id,sql_principal,registration_digest,state FROM "+qualified(grant.AuthoritySchema, "writer_registry")+" WHERE writer_key=?", []driver.NamedValue{{Ordinal: 1, Value: grant.WriterKey}}, 5)
	if err != nil {
		return err
	}
	state, err = unsignedValue(values[4])
	if err != nil || state != 1 || stringValue(values[0]) != grant.Generation || !bytes.Equal(byteValue(values[1]), grant.InstanceId) || !bytes.Equal(byteValue(values[2]), grant.SqlPrincipal) || !bytes.Equal(byteValue(values[3]), grant.RegistrationDigest) {
		return ErrWriterDenied
	}
	a.config.boot.noteAdmitted()
	return nil
}

func readRawMeta(ctx context.Context, conn driver.Conn, schema string) (*databasepb.RouteMeta, error) {
	values, err := rawOne(ctx, conn, "SELECT generation,writable,writer_epoch,catalog_revision,freeze_run_id FROM "+qualified(schema, "route_meta")+" WHERE id=1", nil, 5)
	if err != nil {
		return nil, err
	}
	writable, e1 := unsignedValue(values[1])
	epoch, e2 := unsignedValue(values[2])
	revision, e3 := unsignedValue(values[3])
	if e1 != nil || e2 != nil || e3 != nil || writable > 1 || epoch == 0 || revision == 0 {
		return nil, ErrWriterDenied
	}
	return &databasepb.RouteMeta{Id: 1, Generation: stringValue(values[0]), Writable: uint32(writable), WriterEpoch: epoch, CatalogRevision: revision, FreezeRunId: bytes.Clone(byteValue(values[4]))}, nil
}
func qualified(schema, table string) string { return "`" + schema + "`.`" + table + "`" }
func stringValue(value driver.Value) string {
	if v, ok := value.(string); ok {
		return v
	}
	if v, ok := value.([]byte); ok {
		return string(v)
	}
	return ""
}
func byteValue(value driver.Value) []byte {
	if value == nil {
		return nil
	}
	if v, ok := value.([]byte); ok {
		return v
	}
	if v, ok := value.(string); ok {
		return []byte(v)
	}
	return nil
}
func unsignedValue(value driver.Value) (uint64, error) {
	if v, ok := value.(int64); ok {
		if v >= 0 {
			return uint64(v), nil
		}
		return 0, ErrWriterDenied
	}
	if v, ok := value.(uint64); ok {
		return v, nil
	}
	text := stringValue(value)
	if text == "" {
		return 0, ErrWriterDenied
	}
	return strconv.ParseUint(text, 10, 64)
}

// rawOne仅用于受信内部参数化SQL，同一连接、严格一行一结果集；正文和身份无导出raw旁路。
func rawOne(ctx context.Context, conn driver.Conn, query string, args []driver.NamedValue, columns int) (result []driver.Value, resultErr error) {
	rows, closeStmt, err := rawQuery(ctx, conn, query, args)
	if err != nil {
		if rows != nil {
			if e := rows.Close(); e != nil {
				err = unknown("authority失败Rows关闭", e)
			}
		}
		if closeStmt != nil {
			if e := closeStmt(); e != nil {
				err = unknown("authority失败Stmt关闭", e)
			}
		}
		return nil, err
	}
	if rows == nil {
		if closeStmt != nil {
			if e := closeStmt(); e != nil {
				return nil, unknown("authority缺Rows关闭", e)
			}
		}
		return nil, ErrWriterDenied
	}
	defer func() {
		if err := rows.Close(); err != nil {
			resultErr = unknown("authority内部Rows关闭", err)
		}
		if closeStmt != nil {
			if err := closeStmt(); err != nil {
				resultErr = unknown("authority内部Stmt关闭", err)
			}
		}
	}()
	if len(rows.Columns()) != columns {
		return nil, ErrWriterDenied
	}
	result = make([]driver.Value, columns)
	if err = rows.Next(result); err != nil {
		if err == io.EOF {
			return nil, ErrWriterDenied
		}
		return nil, err
	}
	for i, value := range result {
		if v, ok := value.([]byte); ok {
			result[i] = bytes.Clone(v)
		}
	}
	if err = rows.Next(make([]driver.Value, columns)); err != io.EOF {
		if err != nil {
			return nil, err
		}
		return nil, ErrWriterDenied
	}
	if next, ok := rows.(driver.RowsNextResultSet); ok && next.HasNextResultSet() {
		return nil, ErrWriterDenied
	}
	return result, nil
}
