package authoritysql

import (
	"bytes"
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"strconv"

	"google.golang.org/protobuf/proto"
	databasepb "proto/common/database"
)

const WriterSlotCount = 64

var ErrWriterDenied = errors.New("SQL writer generation、epoch、主体或ACTIVE资格不符")

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
	if meta.Generation != g.config.Generation() || meta.Writable != 1 || meta.WriterEpoch == 0 {
		return nil, ErrWriterDenied
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
	if grant.LockPrimitive == databasepb.SqlWriterBootGrant_LOCK_TOUCH {
		result, err := rawExec(ctx, conn, "UPDATE "+qualified(grant.AuthoritySchema, "writer_slot")+" SET lock_touch=1-lock_touch WHERE writer_key=? AND slot=? AND generation=? AND writer_epoch=? AND state=1 AND lock_touch IN (0,1)", append(args, driver.NamedValue{Ordinal: 3, Value: grant.Generation}, driver.NamedValue{Ordinal: 4, Value: a.before.WriterEpoch}))
		if err != nil {
			return err
		}
		if result == nil {
			return ErrWriterDenied
		}
		affected, err := result.RowsAffected()
		if err != nil || affected != 1 {
			return ErrWriterDenied
		}
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
	if e1 != nil || e2 != nil || stringValue(values[0]) != grant.Generation || epoch != a.before.WriterEpoch || state != 1 {
		return ErrWriterDenied
	}
	// 退休永久REVOKED与freeze推进epoch都落在已CURRENT锁住的slot原件上，RR旧snapshot不能放行。
	meta, err := readRawMeta(ctx, conn, grant.AuthoritySchema)
	if err != nil {
		return err
	}
	if meta.Generation != grant.Generation || meta.Writable != 1 || meta.WriterEpoch != epoch {
		return ErrWriterDenied
	}
	values, err = rawOne(ctx, conn, "SELECT generation,instance_id,sql_principal,registration_digest,state FROM "+qualified(grant.AuthoritySchema, "writer_registry")+" WHERE writer_key=?", []driver.NamedValue{{Ordinal: 1, Value: grant.WriterKey}}, 5)
	if err != nil {
		return err
	}
	state, err = unsignedValue(values[4])
	if err != nil || state != 1 || stringValue(values[0]) != grant.Generation || !bytes.Equal(byteValue(values[1]), grant.InstanceId) || !bytes.Equal(byteValue(values[2]), grant.SqlPrincipal) || !bytes.Equal(byteValue(values[3]), grant.RegistrationDigest) {
		return ErrWriterDenied
	}
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
