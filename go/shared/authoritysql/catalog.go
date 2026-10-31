package authoritysql

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"math"
	"sort"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	databasepb "proto/common/database"
	dbopts "proto/db"
)

// CatalogConfig是部署配置，不是第二份持久化schema；行与请求一律用正式proto。
type CatalogConfig struct {
	AuthoritySchema         string
	Generation              string
	ProtectedSchemas        []string
	PhysicalAuthoritySHA256 []byte
	Principal               string
}
type Catalog struct {
	db     *sql.DB
	config CatalogConfig
}

func NewCatalog(db *sql.DB, config CatalogConfig) (*Catalog, error) {
	if db == nil || !validIdentifier(config.AuthoritySchema) || !validGeneration(config.Generation) || !validSchemas(config.ProtectedSchemas) || len(config.PhysicalAuthoritySHA256) != 32 || !validPrincipal(config.Principal) {
		return nil, ErrWriterDenied
	}
	config.ProtectedSchemas = append([]string(nil), config.ProtectedSchemas...)
	config.PhysicalAuthoritySHA256 = bytes.Clone(config.PhysicalAuthoritySHA256)
	return &Catalog{db: db, config: config}, nil
}
func (c *Catalog) table(name string) string { return qualified(c.config.AuthoritySchema, name) }
func deterministic(message proto.Message) ([]byte, error) {
	return (proto.MarshalOptions{Deterministic: true}).Marshal(message)
}

// 控制原件不能忽略未知字段；玩家业务blob的未知字段保留规则不在此路径。
func hasUnknownControl(message proto.Message) bool {
	if message == nil {
		return true
	}
	var visit func(protoreflect.Message) bool
	visit = func(current protoreflect.Message) bool {
		if !current.IsValid() || len(current.GetUnknown()) != 0 {
			return true
		}
		unknown := false
		current.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
			if field.IsMap() {
				if field.MapValue().Message() != nil {
					value.Map().Range(func(_ protoreflect.MapKey, item protoreflect.Value) bool {
						unknown = visit(item.Message())
						return !unknown
					})
				}
			} else if field.IsList() && field.Message() != nil {
				for index := 0; index < value.List().Len(); index++ {
					if visit(value.List().Get(index).Message()) {
						unknown = true
						break
					}
				}
			} else if field.Message() != nil {
				unknown = visit(value.Message())
			}
			return !unknown
		})
		return unknown
	}
	return visit(message.ProtoReflect())
}
func messageDigest(message proto.Message) ([]byte, error) {
	raw, err := deterministic(message)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	return sum[:], nil
}

func (c *Catalog) transaction(ctx context.Context, call func(*sql.Tx) error) error {
	tx, err := c.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	abort := func(cause error) error {
		if rollbackErr := tx.Rollback(); rollbackErr != nil {
			return unknown("catalog回滚未确认", errors.Join(cause, rollbackErr))
		}
		return cause
	}
	var principal string
	if err = tx.QueryRowContext(ctx, "SELECT CURRENT_USER()").Scan(&principal); err != nil {
		return abort(err)
	}
	if principal != c.config.Principal {
		return abort(ErrWriterDenied)
	}
	if err = call(tx); err != nil {
		return abort(err)
	}
	if err = tx.Commit(); err != nil {
		return unknown("catalog提交", err)
	}
	return nil
}
func (c *Catalog) meta(ctx context.Context, queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, lock bool) (*databasepb.RouteMeta, error) {
	meta := &databasepb.RouteMeta{Id: 1}
	query := "SELECT generation,writable,writer_epoch,catalog_revision,freeze_run_id FROM " + c.table("route_meta") + " WHERE id=1"
	if lock {
		query += " FOR UPDATE"
	}
	err := queryer.QueryRowContext(ctx, query).Scan(&meta.Generation, &meta.Writable, &meta.WriterEpoch, &meta.CatalogRevision, &meta.FreezeRunId)
	if err != nil {
		return nil, err
	}
	if meta.Generation != c.config.Generation || meta.WriterEpoch == 0 || meta.CatalogRevision == 0 || meta.Writable > 1 {
		return nil, ErrWriterDenied
	}
	return meta, nil
}
func (c *Catalog) active(ctx context.Context, queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, current bool) ([][]byte, error) {
	query := "SELECT writer_key FROM " + c.table("writer_registry") + " WHERE state=1 ORDER BY writer_key"
	if current {
		query += " FOR UPDATE"
	}
	rows, err := queryer.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys [][]byte
	for rows.Next() {
		var key []byte
		if err = rows.Scan(&key); err != nil {
			return nil, err
		}
		if len(key) != 16 {
			return nil, ErrWriterDenied
		}
		keys = append(keys, bytes.Clone(key))
	}
	return keys, rows.Err()
}
func exactKeys(a, b [][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}
func (c *Catalog) lockSlots(ctx context.Context, tx *sql.Tx, keys [][]byte, epoch uint64) error {
	for i, key := range keys {
		if len(key) != 16 || i > 0 && bytes.Compare(keys[i-1], key) >= 0 {
			return ErrWriterDenied
		}
		// 锁顺序精确按(writer_key,slot)，不把meta/registry JOIN入首锁。
		for slot := uint32(0); slot < WriterSlotCount; slot++ {
			row := &databasepb.SqlWriterSlot{}
			err := tx.QueryRowContext(ctx, "SELECT generation,writer_epoch,state FROM "+c.table("writer_slot")+" WHERE writer_key=? AND slot=? FOR UPDATE", key, slot).Scan(&row.Generation, &row.WriterEpoch, &row.State)
			if err != nil {
				return err
			}
			if row.Generation != c.config.Generation || row.WriterEpoch != epoch || row.State != databasepb.SqlWriterRegistry_ACTIVE {
				return ErrWriterDenied
			}
		}
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+c.table("writer_slot")+" WHERE writer_key=?", key).Scan(&count); err != nil {
			return err
		}
		if count != WriterSlotCount {
			return ErrWriterDenied
		}
	}
	return nil
}
func checkedIncrement(v uint64) (uint64, error) {
	if v == 0 || v == math.MaxUint64 {
		return 0, ErrWriterDenied
	}
	return v + 1, nil
}
func affected(result sql.Result, want int64) error {
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != want {
		return ErrWriterDenied
	}
	return nil
}

// LifecycleReceipt只读按原ID裁决UNKNOWN，不会换ID或重放原动作。
func (c *Catalog) LifecycleReceipt(ctx context.Context, id []byte) (*databasepb.SqlWriterLifecycleReceipt, error) {
	return c.readLifecycleReceipt(ctx, c.db, id)
}
func (c *Catalog) readLifecycleReceipt(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id []byte) (*databasepb.SqlWriterLifecycleReceipt, error) {
	r := &databasepb.SqlWriterLifecycleReceipt{RequestId: bytes.Clone(id)}
	err := q.QueryRowContext(ctx, "SELECT kind,generation,request_digest,request_proto,previous_epoch,resulting_epoch,catalog_revision FROM "+c.table("writer_lifecycle_receipt")+" WHERE request_id=?", id).Scan(&r.Kind, &r.Generation, &r.RequestDigest, &r.RequestProto, &r.PreviousEpoch, &r.ResultingEpoch, &r.CatalogRevision)
	if err != nil {
		return nil, err
	}
	request := &databasepb.SqlWriterLifecycleReceipt_Request{}
	if proto.Unmarshal(r.RequestProto, request) != nil || r.Generation != c.config.Generation || (r.Kind != databasepb.SqlWriterLifecycleReceipt_REGISTER && r.Kind != databasepb.SqlWriterLifecycleReceipt_RETIRE) {
		return nil, ErrWriterDenied
	}
	raw, digest, err := c.validateLifecycle(request, r.Kind)
	if err != nil || !bytes.Equal(request.RequestId, id) || !bytes.Equal(raw, r.RequestProto) || !bytes.Equal(digest, r.RequestDigest) || r.PreviousEpoch != request.ExpectedEpoch || r.ResultingEpoch != r.PreviousEpoch || r.CatalogRevision == 0 {
		return nil, ErrWriterDenied
	}
	if request.ExpectedCatalogRevision != 0 && (request.ExpectedCatalogRevision == math.MaxUint64 || r.CatalogRevision != request.ExpectedCatalogRevision+1) {
		return nil, ErrWriterDenied
	}
	return r, nil
}
func (c *Catalog) validateLifecycle(request *databasepb.SqlWriterLifecycleReceipt_Request, kind databasepb.SqlWriterLifecycleReceipt_Kind) ([]byte, []byte, error) {
	if request != nil && hasUnknownControl(request) {
		return nil, nil, ErrWriterDenied
	}
	if request == nil || request.Writer == nil || len(request.RequestId) != 16 || request.Kind != kind || request.AuthoritySchema != c.config.AuthoritySchema || !equalStrings(request.ProtectedSchemas, c.config.ProtectedSchemas) || !bytes.Equal(request.DeploymentDigest, c.config.PhysicalAuthoritySHA256) || len(request.Writer.WriterKey) != 16 || request.Writer.Generation != c.config.Generation {
		return nil, nil, ErrWriterDenied
	}
	if request.ExpectedEpoch == 0 || kind == databasepb.SqlWriterLifecycleReceipt_RETIRE && request.ExpectedCatalogRevision == 0 || request.DeploymentId == "" || len(request.DeploymentId) > 256 {
		return nil, nil, ErrWriterDenied
	}
	if len(request.Writer.InstanceId) != 16 || !validPrincipal(string(request.Writer.SqlPrincipal)) || len(request.Writer.RegistrationDigest) != 32 || request.Writer.State != databasepb.SqlWriterRegistry_ACTIVE || len(request.MaintenanceRunId) != 0 && len(request.MaintenanceRunId) != 16 {
		return nil, nil, ErrWriterDenied
	}
	raw, err := deterministic(request)
	if err != nil {
		return nil, nil, err
	}
	if kind == databasepb.SqlWriterLifecycleReceipt_REGISTER {
		canonical, err := CanonicalRegistrationRequest(request)
		if err != nil {
			return nil, nil, err
		}
		if !bytes.Equal(canonical.Writer.RegistrationDigest, request.Writer.RegistrationDigest) {
			return nil, nil, ErrWriterDenied
		}
		return raw, bytes.Clone(canonical.Writer.RegistrationDigest), nil
	}
	sum := sha256.Sum256(raw)
	return raw, sum[:], nil
}

// CanonicalRegistrationRequest 的唯一摘要规则包含deployment和已核锁资格，排除摘要自身。
func CanonicalRegistrationRequest(input *databasepb.SqlWriterLifecycleReceipt_Request) (*databasepb.SqlWriterLifecycleReceipt_Request, error) {
	if input != nil && hasUnknownControl(input) {
		return nil, ErrWriterDenied
	}
	if input == nil || input.Writer == nil || input.Kind != databasepb.SqlWriterLifecycleReceipt_REGISTER || len(input.RequestId) != 16 || len(input.Writer.WriterKey) != 16 || len(input.Writer.InstanceId) != 16 || input.DeploymentId == "" || len(input.LockQualificationSha256) != 32 || (input.LockPrimitive != databasepb.SqlWriterBootGrant_SELECT_ONLY && input.LockPrimitive != databasepb.SqlWriterBootGrant_LOCK_TOUCH) {
		return nil, ErrWriterDenied
	}
	request := proto.Clone(input).(*databasepb.SqlWriterLifecycleReceipt_Request)
	request.Writer.RegistrationDigest = nil
	raw, err := deterministic(request)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(raw)
	request.Writer.RegistrationDigest = bytes.Clone(digest[:])
	return request, nil
}

// RegistrationConsumedByOtherDeployment 仅供有限预批准roster跳过已被其它boot消费的同一entry。
// 任何静态字段漂移或未知收据都拒绝，不能把普通ErrWriterDenied解释为可选下一身份。
func (c *Catalog) RegistrationConsumedByOtherDeployment(ctx context.Context, input *databasepb.SqlWriterLifecycleReceipt_Request) (bool, error) {
	if _, _, err := c.validateLifecycle(input, databasepb.SqlWriterLifecycleReceipt_REGISTER); err != nil {
		return false, err
	}
	receipt, err := c.LifecycleReceipt(ctx, input.RequestId)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	prior := &databasepb.SqlWriterLifecycleReceipt_Request{}
	if err = proto.Unmarshal(receipt.RequestProto, prior); err != nil {
		return false, ErrWriterDenied
	}
	_, digest, err := c.validateLifecycle(prior, databasepb.SqlWriterLifecycleReceipt_REGISTER)
	if err != nil || receipt.Kind != databasepb.SqlWriterLifecycleReceipt_REGISTER || !bytes.Equal(digest, receipt.RequestDigest) {
		return false, ErrWriterDenied
	}
	if prior.DeploymentId == input.DeploymentId {
		if !proto.Equal(prior, input) {
			return false, ErrWriterDenied
		}
		return false, nil
	}
	a := proto.Clone(prior).(*databasepb.SqlWriterLifecycleReceipt_Request)
	b := proto.Clone(input).(*databasepb.SqlWriterLifecycleReceipt_Request)
	a.DeploymentId = ""
	b.DeploymentId = ""
	a.Writer.RegistrationDigest = nil
	b.Writer.RegistrationDigest = nil
	if !proto.Equal(a, b) {
		return false, ErrWriterDenied
	}
	return true, nil
}
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
func (c *Catalog) insertLifecycle(ctx context.Context, tx *sql.Tx, request *databasepb.SqlWriterLifecycleReceipt_Request, raw, digest []byte, epoch, revision uint64) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO "+c.table("writer_lifecycle_receipt")+"(request_id,kind,generation,request_digest,request_proto,previous_epoch,resulting_epoch,catalog_revision) VALUES(?,?,?,?,?,?,?,?)", request.RequestId, request.Kind, c.config.Generation, digest, raw, epoch, epoch, revision)
	return err
}

// Register只在metaX后INSERT全新key；不查修旧slot，不等待旧slot前缀。
func (c *Catalog) Register(ctx context.Context, request *databasepb.SqlWriterLifecycleReceipt_Request) (*databasepb.SqlWriterLifecycleReceipt, error) {
	raw, digest, err := c.validateLifecycle(request, databasepb.SqlWriterLifecycleReceipt_REGISTER)
	if err != nil {
		return nil, err
	}
	writer := request.Writer
	if len(writer.InstanceId) != 16 || !validPrincipal(string(writer.SqlPrincipal)) || len(writer.RegistrationDigest) != 32 || writer.State != databasepb.SqlWriterRegistry_ACTIVE {
		return nil, ErrWriterDenied
	}
	if old, readErr := c.LifecycleReceipt(ctx, request.RequestId); readErr == nil {
		if bytes.Equal(old.RequestDigest, digest) && bytes.Equal(old.RequestProto, raw) {
			return old, nil
		}
		return nil, ErrWriterDenied
	} else if !errors.Is(readErr, sql.ErrNoRows) {
		return nil, readErr
	}
	err = c.transaction(ctx, func(tx *sql.Tx) error {
		meta, err := c.meta(ctx, tx, true)
		if err != nil {
			return err
		}
		if meta.Writable != 1 || meta.WriterEpoch != request.ExpectedEpoch || request.ExpectedCatalogRevision != 0 && meta.CatalogRevision != request.ExpectedCatalogRevision {
			return ErrWriterDenied
		}
		if len(meta.FreezeRunId) != 0 {
			if len(request.MaintenanceRunId) != 16 || !bytes.Equal(request.MaintenanceRunId, meta.FreezeRunId) {
				return ErrWriterDenied
			}
			run, err := c.readRun(ctx, tx, request.MaintenanceRunId, true)
			if err != nil {
				return err
			}
			if run.Phase != databasepb.SqlWriterDrainRun_SQL_RELEASED || run.Generation != c.config.Generation {
				return ErrWriterDenied
			}
			release, err := c.readReceipt(ctx, tx, run.LatestReceiptId)
			if err != nil {
				return err
			}
			if release.Kind != databasepb.SqlWriterMaintenanceReceipt_RELEASE || release.Phase != databasepb.SqlWriterMaintenanceReceipt_RELEASED || !bytes.Equal(release.ReceiptDigest, run.LatestReceiptDigest) || release.AfterEpoch != meta.WriterEpoch {
				return ErrWriterDenied
			}
		} else if len(request.MaintenanceRunId) != 0 {
			return ErrWriterDenied
		}
		revision, err := checkedIncrement(meta.CatalogRevision)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO "+c.table("writer_registry")+"(writer_key,generation,instance_id,sql_principal,registration_digest,state,registered_ms,retirement_request_id,retirement_digest,retired_ms) VALUES(?,?,?,?,?,1,?,?,?,0)", writer.WriterKey, writer.Generation, writer.InstanceId, writer.SqlPrincipal, writer.RegistrationDigest, writer.RegisteredMs, []byte{}, []byte{})
		if err != nil {
			return err
		}
		for slot := 0; slot < WriterSlotCount; slot++ {
			if _, err = tx.ExecContext(ctx, "INSERT INTO "+c.table("writer_slot")+"(writer_key,slot,generation,writer_epoch,state,lock_touch) VALUES(?,?,?,?,1,0)", writer.WriterKey, slot, writer.Generation, meta.WriterEpoch); err != nil {
				return err
			}
		}
		if _, err = tx.ExecContext(ctx, "UPDATE "+c.table("route_meta")+" SET catalog_revision=? WHERE id=1", revision); err != nil {
			return err
		}
		return c.insertLifecycle(ctx, tx, request, raw, digest, meta.WriterEpoch, revision)
	})
	if err != nil {
		return nil, err
	}
	return c.LifecycleReceipt(ctx, request.RequestId)
}
func (c *Catalog) Retire(ctx context.Context, request *databasepb.SqlWriterLifecycleReceipt_Request) (*databasepb.SqlWriterLifecycleReceipt, error) {
	raw, digest, err := c.validateLifecycle(request, databasepb.SqlWriterLifecycleReceipt_RETIRE)
	if err != nil {
		return nil, err
	}
	if old, readErr := c.LifecycleReceipt(ctx, request.RequestId); readErr == nil {
		if bytes.Equal(old.RequestDigest, digest) && bytes.Equal(old.RequestProto, raw) {
			return old, nil
		}
		return nil, ErrWriterDenied
	} else if !errors.Is(readErr, sql.ErrNoRows) {
		return nil, readErr
	}
	err = c.transaction(ctx, func(tx *sql.Tx) error {
		if err := c.lockSlots(ctx, tx, [][]byte{request.Writer.WriterKey}, request.ExpectedEpoch); err != nil {
			return err
		}
		meta, err := c.meta(ctx, tx, true)
		if err != nil {
			return err
		}
		if meta.WriterEpoch != request.ExpectedEpoch || meta.CatalogRevision != request.ExpectedCatalogRevision {
			return ErrWriterDenied
		}
		// P0目录不可改写；同run的P1/P3退役由永久闭合原件证明，并在冻结前折叠进预期全集。
		if meta.Writable != 1 {
			return ErrWriterDenied
		}
		if len(meta.FreezeRunId) != 0 {
			if len(request.MaintenanceRunId) != 16 || !bytes.Equal(request.MaintenanceRunId, meta.FreezeRunId) {
				return ErrWriterDenied
			}
			run, err := c.readRun(ctx, tx, request.MaintenanceRunId, true)
			if err != nil {
				return err
			}
			if err = c.authorizeRunRetirement(ctx, tx, run, request, meta.CatalogRevision); err != nil {
				return err
			}
		} else if len(request.MaintenanceRunId) != 0 {
			return ErrWriterDenied
		}
		revision, err := checkedIncrement(meta.CatalogRevision)
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, "UPDATE "+c.table("writer_slot")+" SET state=2 WHERE writer_key=? AND generation=? AND writer_epoch=? AND state=1", request.Writer.WriterKey, c.config.Generation, meta.WriterEpoch)
		if err != nil {
			return err
		}
		if err = affected(result, WriterSlotCount); err != nil {
			return err
		}
		result, err = tx.ExecContext(ctx, "UPDATE "+c.table("writer_registry")+" SET state=2,retirement_request_id=?,retirement_digest=?,retired_ms=CAST(UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000 AS UNSIGNED) WHERE writer_key=? AND generation=? AND instance_id=? AND sql_principal=? AND registration_digest=? AND state=1", request.RequestId, digest, request.Writer.WriterKey, c.config.Generation, request.Writer.InstanceId, request.Writer.SqlPrincipal, request.Writer.RegistrationDigest)
		if err != nil {
			return err
		}
		if err = affected(result, 1); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE "+c.table("route_meta")+" SET catalog_revision=? WHERE id=1", revision); err != nil {
			return err
		}
		return c.insertLifecycle(ctx, tx, request, raw, digest, meta.WriterEpoch, revision)
	})
	if err != nil {
		return nil, err
	}
	return c.LifecycleReceipt(ctx, request.RequestId)
}

func (c *Catalog) ActiveWriterKeys(ctx context.Context) ([][]byte, error) {
	return c.active(ctx, c.db, false)
}
func (c *Catalog) CurrentMeta(ctx context.Context) (*databasepb.RouteMeta, error) {
	return c.meta(ctx, c.db, false)
}
func sortedWriterKeys(values [][]byte) [][]byte {
	keys := make([][]byte, len(values))
	for i := range values {
		keys[i] = bytes.Clone(values[i])
	}
	sort.Slice(keys, func(i, j int) bool { return bytes.Compare(keys[i], keys[j]) < 0 })
	return keys
}

// CatalogBootstrapStatements 返回本仓正式proto对应的唯一预建DDL副本。
// schema预建与跨Go模块fixture共用这里，调用者不得复制一份目录表DDL。
func CatalogBootstrapStatements() map[string]string {
	return map[string]string{
		"route_meta":                     catalogRouteMetaBootstrapDDL,
		"writer_registry":                catalogWriterRegistryBootstrapDDL,
		"writer_slot":                    catalogWriterSlotBootstrapDDL,
		"writer_lifecycle_receipt":       catalogWriterLifecycleReceiptBootstrapDDL,
		"sql_writer_drain_run":           catalogWriterDrainRunBootstrapDDL,
		"sql_writer_freeze_ticket":       catalogWriterFreezeTicketBootstrapDDL,
		"sql_writer_maintenance_receipt": catalogWriterMaintenanceReceiptBootstrapDDL,
	}
}

// PrepareCatalogSchema 仅用于未分发normal凭据的独立fresh-prestart和自有fixture。
// 必须连接目标schema；任何已有目录表均拒绝，不以IF NOT EXISTS升级未知旧布局。
func PrepareCatalogSchema(ctx context.Context, db *sql.DB, schema, generation string) error {
	if db == nil || !validIdentifier(schema) || !validGeneration(generation) {
		return ErrWriterDenied
	}
	var actual string
	if err := db.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&actual); err != nil {
		return err
	}
	if actual != schema {
		return ErrWriterDenied
	}
	statements := CatalogBootstrapStatements()
	names := make([]string, 0, len(statements))
	for name := range statements {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		var count uint64
		if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=? AND table_name=?", schema, name).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return errors.New("fresh schema准备拒绝接管已有authority表")
		}
	}
	for _, name := range names {
		if _, err := db.ExecContext(ctx, statements[name]); err != nil {
			return err
		}
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO route_meta(id,generation,writable,writer_epoch,catalog_revision,freeze_run_id) VALUES(1,?,1,1,1,?)", generation, []byte{}); err != nil {
		return err
	}
	return VerifyCatalogSchema(ctx, db, schema)
}

// VerifyCatalogSchema 只读核正式proto字段、完整主键及防复用唯一键，normal不得修补。
func VerifyCatalogSchema(ctx context.Context, db *sql.DB, schema string) error {
	if db == nil || !validIdentifier(schema) {
		return ErrWriterDenied
	}
	for _, model := range []proto.Message{
		&databasepb.RouteMeta{}, &databasepb.SqlWriterRegistry{}, &databasepb.SqlWriterSlot{},
		&databasepb.SqlWriterLifecycleReceipt{}, &databasepb.SqlWriterDrainRun{},
		&databasepb.SqlWriterFreezeTicket{}, &databasepb.SqlWriterMaintenanceReceipt{},
	} {
		descriptor := model.ProtoReflect().Descriptor()
		table, ok := proto.GetExtension(descriptor.Options(), dbopts.E_OptionTableName).(string)
		if !ok || !validIdentifier(table) {
			return ErrWriterDenied
		}
		rows, err := db.QueryContext(ctx, "SELECT column_name FROM information_schema.columns WHERE table_schema=? AND table_name=?", schema, table)
		if err != nil {
			return err
		}
		columns := make(map[string]bool)
		for rows.Next() {
			var name string
			if err = rows.Scan(&name); err != nil {
				break
			}
			columns[name] = true
		}
		rowErr, closeErr := rows.Err(), rows.Close()
		if err != nil {
			return err
		}
		if rowErr != nil {
			return rowErr
		}
		if closeErr != nil {
			return closeErr
		}
		fields := descriptor.Fields()
		for index := 0; index < fields.Len(); index++ {
			if !columns[string(fields.Get(index).Name())] {
				return errors.New("authority表缺少正式proto列，拒绝自动迁移")
			}
		}
		primary, ok := proto.GetExtension(descriptor.Options(), dbopts.E_OptionPrimaryKey).(string)
		if !ok {
			return ErrWriterDenied
		}
		wanted := strings.Split(primary, ",")
		for i := range wanted {
			wanted[i] = strings.TrimSpace(wanted[i])
			if !validIdentifier(wanted[i]) {
				return ErrWriterDenied
			}
		}
		rows, err = db.QueryContext(ctx, "SELECT column_name FROM information_schema.statistics WHERE table_schema=? AND table_name=? AND index_name='PRIMARY' ORDER BY seq_in_index", schema, table)
		if err != nil {
			return err
		}
		var got []string
		for rows.Next() {
			var name string
			if err = rows.Scan(&name); err != nil {
				break
			}
			got = append(got, name)
		}
		rowErr, closeErr = rows.Err(), rows.Close()
		if err != nil {
			return err
		}
		if rowErr != nil {
			return rowErr
		}
		if closeErr != nil {
			return closeErr
		}
		if strings.Join(got, ",") != strings.Join(wanted, ",") {
			return errors.New("authority表主键与正式proto不一致")
		}
	}
	for _, check := range []struct {
		table   string
		columns []string
		unique  bool
	}{
		{"writer_registry", []string{"sql_principal"}, true},
		{"writer_registry", []string{"instance_id"}, true},
		{"sql_writer_freeze_ticket", []string{"operation_id", "attempt_revision"}, true},
		{"sql_writer_freeze_ticket", []string{"freeze_request_id"}, true},
		{"writer_lifecycle_receipt", []string{"kind", "request_digest"}, false},
		{"writer_lifecycle_receipt", []string{"catalog_revision", "request_id"}, false},
	} {
		rows, err := db.QueryContext(ctx, "SELECT index_name,column_name,non_unique,sub_part FROM information_schema.statistics WHERE table_schema=? AND table_name=? ORDER BY index_name,seq_in_index", schema, check.table)
		if err != nil {
			return err
		}
		type indexShape struct {
			columns []string
			unique  bool
			full    bool
		}
		indexes := make(map[string]*indexShape)
		for rows.Next() {
			var name, column string
			var nonUnique uint64
			var prefix sql.NullInt64
			if err = rows.Scan(&name, &column, &nonUnique, &prefix); err != nil {
				break
			}
			shape := indexes[name]
			if shape == nil {
				shape = &indexShape{unique: nonUnique == 0, full: true}
				indexes[name] = shape
			}
			shape.columns = append(shape.columns, column)
			shape.full = shape.full && !prefix.Valid
		}
		rowErr, closeErr := rows.Err(), rows.Close()
		if err != nil {
			return err
		}
		if rowErr != nil {
			return rowErr
		}
		if closeErr != nil {
			return closeErr
		}
		found := false
		for _, shape := range indexes {
			if shape.full && (!check.unique || shape.unique) && strings.Join(shape.columns, ",") == strings.Join(check.columns, ",") {
				found = true
				break
			}
		}
		if !found {
			return errors.New("authority表缺少完整防复用索引")
		}
	}
	return nil
}

const catalogRouteMetaBootstrapDDL = "CREATE TABLE IF NOT EXISTS `route_meta` (\n" +
	"  `id` tinyint unsigned NOT NULL COMMENT 'pb:1',\n" +
	"  `generation` varchar(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL COMMENT 'pb:2',\n" +
	"  `writable` tinyint unsigned NOT NULL DEFAULT 1 COMMENT 'pb:3',\n" +
	"  `writer_epoch` bigint unsigned NOT NULL DEFAULT 1 COMMENT 'pb:4',\n" +
	"  `catalog_revision` bigint unsigned NOT NULL DEFAULT 1 COMMENT 'pb:5',\n" +
	"  `freeze_run_id` varbinary(16) NOT NULL DEFAULT '' COMMENT 'pb:6',\n" +
	"  PRIMARY KEY (`id`)\n" +
	") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin COMMENT='route_meta';"

const catalogWriterRegistryBootstrapDDL = `CREATE TABLE IF NOT EXISTS writer_registry (
 writer_key BINARY(16) NOT NULL COMMENT 'pb:1',
 generation VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL COMMENT 'pb:2',
 instance_id BINARY(16) NOT NULL COMMENT 'pb:3',
 sql_principal VARBINARY(288) NOT NULL COMMENT 'pb:4',
 registration_digest BINARY(32) NOT NULL COMMENT 'pb:5',
 state TINYINT UNSIGNED NOT NULL COMMENT 'pb:6',
 registered_ms BIGINT UNSIGNED NOT NULL COMMENT 'pb:7',
 retirement_request_id VARBINARY(16) NOT NULL DEFAULT '' COMMENT 'pb:8',
 retirement_digest VARBINARY(32) NOT NULL DEFAULT '' COMMENT 'pb:9',
 retired_ms BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT 'pb:10',
 PRIMARY KEY(writer_key), UNIQUE KEY uk_writer_principal(sql_principal), UNIQUE KEY uk_writer_instance(instance_id),
 KEY idx_writer_state(state,writer_key),
 CONSTRAINT chk_writer_state CHECK(state IN (1,2))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin COMMENT='writer_registry';`

const catalogWriterSlotBootstrapDDL = `CREATE TABLE IF NOT EXISTS writer_slot (
 writer_key BINARY(16) NOT NULL COMMENT 'pb:1',
 slot TINYINT UNSIGNED NOT NULL COMMENT 'pb:2',
 generation VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL COMMENT 'pb:3',
 writer_epoch BIGINT UNSIGNED NOT NULL COMMENT 'pb:4',
 state TINYINT UNSIGNED NOT NULL COMMENT 'pb:5',
 lock_touch TINYINT UNSIGNED NOT NULL DEFAULT 0 COMMENT 'pb:6',
 PRIMARY KEY(writer_key,slot),
 CONSTRAINT chk_writer_slot_number CHECK(slot<64),
 CONSTRAINT chk_writer_slot_state CHECK(state IN (1,2)),
 CONSTRAINT chk_writer_slot_epoch CHECK(writer_epoch>0),
 CONSTRAINT chk_writer_slot_touch CHECK(lock_touch IN (0,1))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin COMMENT='writer_slot';`

const catalogWriterLifecycleReceiptBootstrapDDL = `CREATE TABLE IF NOT EXISTS writer_lifecycle_receipt (
 request_id BINARY(16) NOT NULL COMMENT 'pb:1',
 kind TINYINT UNSIGNED NOT NULL COMMENT 'pb:2',
 generation VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL COMMENT 'pb:3',
 request_digest BINARY(32) NOT NULL COMMENT 'pb:4',
 request_proto MEDIUMBLOB NOT NULL COMMENT 'pb:5',
 previous_epoch BIGINT UNSIGNED NOT NULL COMMENT 'pb:6',
 resulting_epoch BIGINT UNSIGNED NOT NULL COMMENT 'pb:7',
 catalog_revision BIGINT UNSIGNED NOT NULL COMMENT 'pb:8',
 PRIMARY KEY(request_id), KEY idx_writer_lifecycle_request(kind,request_digest),
 KEY idx_writer_lifecycle_revision(catalog_revision,request_id),
 CONSTRAINT chk_writer_lifecycle_kind CHECK(kind IN (1,2)),
 CONSTRAINT chk_writer_lifecycle_epoch CHECK(previous_epoch>0 AND resulting_epoch>0),
 CONSTRAINT chk_writer_lifecycle_revision CHECK(catalog_revision>0)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin COMMENT='writer_lifecycle_receipt';`

const catalogWriterDrainRunBootstrapDDL = `CREATE TABLE IF NOT EXISTS sql_writer_drain_run (
 operation_id BINARY(16) NOT NULL COMMENT 'pb:1',
 request_digest BINARY(32) NOT NULL COMMENT 'pb:2',
 generation VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL COMMENT 'pb:3',
 attempt_revision BIGINT UNSIGNED NOT NULL COMMENT 'pb:4',
 manifest_proto MEDIUMBLOB NOT NULL COMMENT 'pb:5',
 manifest_digest BINARY(32) NOT NULL COMMENT 'pb:6',
 phase TINYINT UNSIGNED NOT NULL COMMENT 'pb:7',
 latest_receipt_digest VARBINARY(32) NOT NULL DEFAULT '' COMMENT 'pb:8',
 frozen_request_id VARBINARY(16) NOT NULL DEFAULT '' COMMENT 'pb:9',
 updated_ms BIGINT UNSIGNED NOT NULL COMMENT 'pb:10',
 latest_receipt_id VARBINARY(16) NOT NULL DEFAULT '' COMMENT 'pb:11',
 PRIMARY KEY(operation_id),
 CONSTRAINT chk_writer_drain_attempt CHECK(attempt_revision>0),
 CONSTRAINT chk_writer_drain_phase CHECK(phase IN (1,3,4,5,6,7,8,9,10,11,12))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin COMMENT='sql_writer_drain_run';`

const catalogWriterFreezeTicketBootstrapDDL = `CREATE TABLE IF NOT EXISTS sql_writer_freeze_ticket (
 ticket_id BINARY(16) NOT NULL COMMENT 'pb:1',
 operation_id BINARY(16) NOT NULL COMMENT 'pb:2',
 attempt_revision BIGINT UNSIGNED NOT NULL COMMENT 'pb:3',
 generation VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL COMMENT 'pb:4',
 manifest_digest BINARY(32) NOT NULL COMMENT 'pb:5',
 non_sql_receipt_digest VARBINARY(32) NOT NULL DEFAULT '' COMMENT 'pb:6',
 freeze_request_id BINARY(16) NOT NULL COMMENT 'pb:7',
 freeze_request_digest BINARY(32) NOT NULL COMMENT 'pb:8',
 not_before_ms BIGINT UNSIGNED NOT NULL COMMENT 'pb:9',
 expires_ms BIGINT UNSIGNED NOT NULL COMMENT 'pb:10',
 state TINYINT UNSIGNED NOT NULL COMMENT 'pb:11',
 consumed_receipt_digest VARBINARY(32) NOT NULL DEFAULT '' COMMENT 'pb:12',
 revocation_digest VARBINARY(32) NOT NULL DEFAULT '' COMMENT 'pb:13',
 updated_ms BIGINT UNSIGNED NOT NULL COMMENT 'pb:14',
 authorization_kind TINYINT UNSIGNED NOT NULL COMMENT 'pb:15',
 grant_receipt_id BINARY(16) NOT NULL COMMENT 'pb:16',
 grant_receipt_digest BINARY(32) NOT NULL COMMENT 'pb:17',
 PRIMARY KEY(ticket_id),
 UNIQUE KEY uk_writer_ticket_attempt(operation_id,attempt_revision),
 UNIQUE KEY uk_writer_ticket_freeze(freeze_request_id),
 CONSTRAINT chk_writer_ticket_state CHECK(state IN (1,2,3)),
 CONSTRAINT chk_writer_ticket_kind CHECK(authorization_kind IN (1,2)),
 CONSTRAINT chk_writer_ticket_window CHECK(not_before_ms>0 AND expires_ms>not_before_ms AND expires_ms-not_before_ms<=60000)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin COMMENT='sql_writer_freeze_ticket';`

const catalogWriterMaintenanceReceiptBootstrapDDL = `CREATE TABLE IF NOT EXISTS sql_writer_maintenance_receipt (
 request_id BINARY(16) NOT NULL COMMENT 'pb:1',
 request_digest BINARY(32) NOT NULL COMMENT 'pb:2',
 run_id BINARY(16) NOT NULL COMMENT 'pb:3',
 generation VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL COMMENT 'pb:4',
 before_epoch BIGINT UNSIGNED NOT NULL COMMENT 'pb:5',
 after_epoch BIGINT UNSIGNED NOT NULL COMMENT 'pb:6',
 catalog_revision BIGINT UNSIGNED NOT NULL COMMENT 'pb:7',
 phase TINYINT UNSIGNED NOT NULL COMMENT 'pb:8',
 plan_proto MEDIUMBLOB NOT NULL COMMENT 'pb:9',
 committed_ms BIGINT UNSIGNED NOT NULL COMMENT 'pb:10',
 kind TINYINT UNSIGNED NOT NULL COMMENT 'pb:11',
 run_phase TINYINT UNSIGNED NOT NULL COMMENT 'pb:12',
 attempt_revision BIGINT UNSIGNED NOT NULL COMMENT 'pb:13',
 prior_receipt_id VARBINARY(16) NOT NULL DEFAULT '' COMMENT 'pb:14',
 prior_receipt_digest VARBINARY(32) NOT NULL DEFAULT '' COMMENT 'pb:15',
 evidence MEDIUMBLOB NOT NULL COMMENT 'pb:16',
 receipt_digest BINARY(32) NOT NULL COMMENT 'pb:17',
 PRIMARY KEY(request_id), KEY idx_writer_receipt_run(run_id,attempt_revision),
 CONSTRAINT chk_writer_receipt_kind CHECK(kind BETWEEN 1 AND 8),
 CONSTRAINT chk_writer_receipt_phase CHECK(phase IN (0,1,2))
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin COMMENT='sql_writer_maintenance_receipt';`
