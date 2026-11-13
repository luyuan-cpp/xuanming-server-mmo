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
	"time"

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
func (passGate) enter(context.Context, driver.Conn) error     { return nil }
func (passGate) admitRead(context.Context, driver.Conn) error { return nil }

// 每条物理连接建连时只读核一次永久REGISTER全文，不以可读UUID替代boot绑定。
// 连接复用时不重核(2026-11-12,connector.revalidate):收据只追加、按 registration_digest 取,不会变;
// 注册行还在不在、摘要对不对,每笔写事务在 enter 里、slot 行锁之下核。
func verifyBootRegistration(ctx context.Context, conn driver.Conn, grant *databasepb.SqlWriterBootGrant) error {
	if grant == nil {
		return ErrWriterDenied
	}
	values, err := rawOne(ctx, conn, "SELECT generation,instance_id,sql_principal,registration_digest,state FROM "+qualified(grant.AuthoritySchema, "writer_registry")+" WHERE writer_key=?", []driver.NamedValue{{Ordinal: 1, Value: grant.WriterKey}}, 5)
	if err != nil {
		return err
	}
	if !registrationMatchesGrant(values, grant) {
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
	defer observeWriterGate(g.config.AuthoritySchema(), gatePhaseBeforeBegin, time.Now())
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

// admitRead 是事务外纯读的闸门(2026-11-06,缺口清单第 21 条)。只核本逻辑库 route_meta 的世代与可写:
//   - 读不需要写资格:主体、注册、slot 是写的前提;读的数据都在主库上,谁读到的都是同一份已提交数据,
//     所以不开事务、不锁不改 writer_slot(LOCK_TOUCH 档下那是一次写,所有实例的读都会改写这张小表)、不读注册表;
//   - 冻结照旧挡回去(ErrWriterFrozen):搬库切换之后源库冻结,还连着源库的读会读到已被目标库超过的数据;
//     启动阶段放不放行由连接层定(readPassesFrozenGateLocked),与写事务被挡时同一条规则;
//   - 世代不符、写资格已关闭回 ErrWriterDenied,与 beforeBegin 相同。
func (g runtimeGate) admitRead(ctx context.Context, conn driver.Conn) error {
	if g.config.boot == nil || g.config.boot.closed.Load() {
		return ErrWriterDenied
	}
	defer observeWriterGate(g.config.AuthoritySchema(), gatePhaseRead, time.Now())
	meta, err := readRawMeta(ctx, conn, g.config.AuthoritySchema())
	if err != nil {
		return err
	}
	if meta.Generation != g.config.Generation() || meta.WriterEpoch == 0 {
		return ErrWriterDenied
	}
	if meta.Writable != 1 {
		g.config.boot.noteFrozen()
		return ErrWriterFrozen
	}
	g.config.boot.noteReadable()
	return nil
}

// enter 在 BEGIN 之后锁住本次轮到的 slot 行,再复核 route_meta 与本写者的注册行。
//
// 每笔写事务在这里只发两条语句(2026-11-11,缺口清单第 20 条;以前是三到四条):
//   - 锁 slot 行(lockSlot):LOCK_TOUCH 档是一条按 beforeBegin 读到的 epoch 的条件写,SELECT_ONLY 档是一条加锁读;
//   - 一条普通读同时取 route_meta 与 writer_registry(readGateView)。这两张表以前分两条读,本来就落在同一个事务快照上,
//     合成一条读到的是同一版,判定不变。
//
// 退休永久REVOKED与freeze推进epoch都落在已CURRENT锁住的slot原件上，RR旧snapshot不能放行:锁之后的这条普通读只做复核。
// 依据与取舍见 docs/design/db-write-fencing-mysql.md「2026-11-10 写事务闸门的开销与热点」及其 11-11 定案。
func (a *runtimeAttempt) enter(ctx context.Context, conn driver.Conn) error {
	grant := a.config.boot.grant
	if a.config.boot.closed.Load() {
		return ErrWriterDenied
	}
	defer observeWriterGate(grant.AuthoritySchema, gatePhaseEnter, time.Now())
	epoch, err := a.lockSlot(ctx, conn)
	if err != nil {
		return err
	}
	meta, registry, err := readGateView(ctx, conn, grant.AuthoritySchema, grant.WriterKey)
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
	if !registrationMatchesGrant(registry, grant) {
		return ErrWriterDenied
	}
	a.config.boot.noteAdmitted()
	return nil
}

// lockSlot 锁住本次轮到的 slot 行并核它,回这一行的 writer_epoch。资格不符(世代不对、已退休、行不在)回 ErrWriterDenied;
// epoch 被推进了回 ErrWriterFrozen。
func (a *runtimeAttempt) lockSlot(ctx context.Context, conn driver.Conn) (uint64, error) {
	grant := a.config.boot.grant
	slotTable := qualified(grant.AuthoritySchema, "writer_slot")
	slotSelect := "SELECT generation,writer_epoch,state FROM " + slotTable + " WHERE writer_key=? AND slot=?"
	key := []driver.NamedValue{{Ordinal: 1, Value: grant.WriterKey}, {Ordinal: 2, Value: int64(a.slot)}}
	switch grant.LockPrimitive {
	case databasepb.SqlWriterBootGrant_LOCK_TOUCH:
		touchArgs := []driver.NamedValue{key[0], key[1], {Ordinal: 3, Value: grant.Generation}, {Ordinal: 4, Value: a.before.WriterEpoch}}
		result, err := rawExec(ctx, conn, "UPDATE "+slotTable+" SET lock_touch=1-lock_touch WHERE writer_key=? AND slot=? AND generation=? AND writer_epoch=? AND state=1 AND lock_touch IN (0,1)", touchArgs)
		if err != nil {
			return 0, err
		}
		if result == nil {
			return 0, ErrWriterDenied
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return 0, ErrWriterDenied
		}
		if affected == 1 {
			// 条件写命中:这一行此刻就是 generation=本世代、writer_epoch=beforeBegin 读到的 epoch、state=ACTIVE。
			// lock_touch 每次都翻转,命中必改一行。以前紧跟着的那次普通读,读到的是本事务刚写的这一版,值与条件逐列相同,所以不再读。
			return a.before.WriterEpoch, nil
		}
		// 条件写没命中:读这一行,分清是资格没了(拒绝)还是 epoch 被推进了(冻结)。
		generation, _, state, err := readSlotRow(ctx, conn, slotSelect, key)
		if err != nil {
			return 0, err
		}
		if generation != grant.Generation || state != 1 {
			return 0, ErrWriterDenied
		}
		// 没命中却读到 ACTIVE:epoch 被推进了(RR 旧快照可能还读到旧 epoch),按冻结处理,下一次尝试会读到真相。
		a.config.boot.noteFrozen()
		return 0, ErrWriterFrozen
	case databasepb.SqlWriterBootGrant_SELECT_ONLY:
		generation, epoch, state, err := readSlotRow(ctx, conn, slotSelect+" FOR UPDATE", key)
		if err != nil {
			return 0, err
		}
		if generation != grant.Generation || state != 1 {
			return 0, ErrWriterDenied
		}
		if epoch != a.before.WriterEpoch {
			a.config.boot.noteFrozen()
			return 0, ErrWriterFrozen
		}
		return epoch, nil
	default:
		return 0, ErrWriterDenied
	}
}

// readSlotRow 按 query 读本写者的一行 slot(generation,writer_epoch,state)。行不在、列取值不合法都按资格不符。
func readSlotRow(ctx context.Context, conn driver.Conn, query string, key []driver.NamedValue) (generation string, epoch, state uint64, err error) {
	values, err := rawOne(ctx, conn, query, key, 3)
	if err != nil {
		return "", 0, 0, err
	}
	epoch, e1 := unsignedValue(values[1])
	state, e2 := unsignedValue(values[2])
	if e1 != nil || e2 != nil {
		return "", 0, 0, ErrWriterDenied
	}
	return stringValue(values[0]), epoch, state, nil
}

// gateViewColumns 是 readGateView 那条读的列数:route_meta 五列在前,writer_registry 五列在后。
const gateViewColumns = 10

// readGateView 用一条普通读同时取 route_meta 与本写者的 writer_registry 行(2026-11-11,缺口清单第 20 条)。
// 以 route_meta 为驱动表左连 writer_registry:注册行不在时后五列是 NULL,registrationMatchesGrant 判成不符,
// 与分两条读时「注册行读不到 → 拒绝」相同;route_meta 不在时一行都读不到,同样拒绝。
// 不加锁:SELECT_ONLY 档那条加锁读不能并进来,FOR UPDATE 会把 route_meta 也锁上,本库全部写事务就被串行化了。
func readGateView(ctx context.Context, conn driver.Conn, schema string, writerKey []byte) (*databasepb.RouteMeta, []driver.Value, error) {
	values, err := rawOne(ctx, conn, "SELECT m.generation,m.writable,m.writer_epoch,m.catalog_revision,m.freeze_run_id,"+
		"r.generation,r.instance_id,r.sql_principal,r.registration_digest,r.state FROM "+qualified(schema, "route_meta")+" m"+
		" LEFT JOIN "+qualified(schema, "writer_registry")+" r ON r.writer_key=? WHERE m.id=1",
		[]driver.NamedValue{{Ordinal: 1, Value: writerKey}}, gateViewColumns)
	if err != nil {
		return nil, nil, err
	}
	meta, err := parseRawMeta(values[:5])
	if err != nil {
		return nil, nil, err
	}
	return meta, values[5:], nil
}

// registrationMatchesGrant 核 writer_registry 的一行(generation,instance_id,sql_principal,registration_digest,state)
// 是不是这份 boot grant 登记的那一代、而且仍 ACTIVE。整行为 NULL(注册行不在)判不符。启动核对与每笔写事务共用这一条规则。
func registrationMatchesGrant(values []driver.Value, grant *databasepb.SqlWriterBootGrant) bool {
	state, err := unsignedValue(values[4])
	return err == nil && state == 1 && stringValue(values[0]) == grant.Generation &&
		bytes.Equal(byteValue(values[1]), grant.InstanceId) && bytes.Equal(byteValue(values[2]), grant.SqlPrincipal) &&
		bytes.Equal(byteValue(values[3]), grant.RegistrationDigest)
}

func readRawMeta(ctx context.Context, conn driver.Conn, schema string) (*databasepb.RouteMeta, error) {
	values, err := rawOne(ctx, conn, "SELECT generation,writable,writer_epoch,catalog_revision,freeze_run_id FROM "+qualified(schema, "route_meta")+" WHERE id=1", nil, 5)
	if err != nil {
		return nil, err
	}
	return parseRawMeta(values)
}

// parseRawMeta 解 route_meta 的五列(generation,writable,writer_epoch,catalog_revision,freeze_run_id)。取值不合法按资格不符。
func parseRawMeta(values []driver.Value) (*databasepb.RouteMeta, error) {
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
