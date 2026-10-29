// Package sqltest为真实SQL测试提供随机自有schema/主体及正式writer资格；生产不得引用。
package sqltest

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	pb "proto/common/database"
	dbopts "proto/db"
	"shared/authoritysql"
	"shared/drainproof"
)

type Options struct {
	AllowMySQL              bool
	AdminDSN                string
	AuthoritySchema         string
	Generation              string
	ProtectedSchemas        []string
	PhysicalAuthoritySHA256 []byte
	LockPrimitive           pb.SqlWriterBootGrant_LockPrimitive
	LockQualificationSHA256 []byte
	// ReceiptDirectory 是 runner 保留的私有目录；SQL清理未知时原件仍可用于人工恢复。
	ReceiptDirectory string
	PrepareAuthority func(context.Context, *sql.DB, string, string) error
}
type Fixture struct {
	controlMu           sync.Mutex
	bootOptions         Options
	registration        *pb.SqlWriterLifecycleReceipt_Request
	registrationReceipt *pb.SqlWriterLifecycleReceipt
	registrationPending bool // REGISTER已开始而grant/Config未完整落定时，清理只保留，不猜测删除。
	maintenance         *fixtureMaintenance
	admin               *mysql.Config
	adminPrincipal      string
	requireTiDB         bool
	Schema              *sql.DB
	Operator            *sql.DB
	Catalog             *authoritysql.Catalog
	Permission          authoritysql.Permission
	Config              authoritysql.WriterConfig
	Generation          string
	authority           string
	base                *mysql.Config
	user                string
	created             []string
	userCreated         bool
	owner               []byte
	physical            []byte
	receiptPath         string
	mu                  sync.Mutex
	pools               []*sql.DB
	cleanup             sync.Once
	t                   testing.TB
}

func randomBytes(size int) ([]byte, error) {
	value := make([]byte, size)
	_, err := rand.Read(value)
	return value, err
}
func identifier(value string) bool {
	if len(value) < 1 || len(value) > 64 {
		return false
	}
	for _, c := range []byte(value) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}
func q(value string) string { return "`" + value + "`" }

// OptionsFromEnv 只装配runner已核原件，不猜端口、主体、物理身份或锁资格。
func OptionsFromEnv(adminDSN string, authority string, protectedSchemas ...string) (Options, error) {
	physical, err := hex.DecodeString(os.Getenv("MMORPG_SQL_FIXTURE_PHYSICAL_AUTHORITY_SHA256"))
	if err != nil || len(physical) != 32 {
		return Options{}, errors.New("缺少fixture物理authority原件摘要")
	}
	qualification, err := hex.DecodeString(os.Getenv("MMORPG_SQL_FIXTURE_LOCK_QUALIFICATION_SHA256"))
	if err != nil || len(qualification) != 32 {
		return Options{}, errors.New("缺少fixture真实锁资格原件摘要")
	}
	var primitive pb.SqlWriterBootGrant_LockPrimitive
	switch os.Getenv("MMORPG_SQL_FIXTURE_LOCK_PRIMITIVE") {
	case "SELECT_ONLY":
		primitive = pb.SqlWriterBootGrant_SELECT_ONLY
	case "LOCK_TOUCH":
		primitive = pb.SqlWriterBootGrant_LOCK_TOUCH
	default:
		return Options{}, errors.New("fixture必须明确已核锁原语")
	}
	schemas := []string{authority}
	seen := map[string]bool{authority: true}
	for _, name := range protectedSchemas {
		if !seen[name] {
			schemas = append(schemas, name)
			seen[name] = true
		}
	}
	sort.Strings(schemas)
	return Options{AdminDSN: adminDSN, AuthoritySchema: authority, ProtectedSchemas: schemas, PhysicalAuthoritySHA256: physical, LockPrimitive: primitive, LockQualificationSHA256: qualification, ReceiptDirectory: os.Getenv("MMORPG_SQL_FIXTURE_RECEIPT_DIR")}, nil
}

// New只创建原本缺席的指定随机schema；发现已有对象时拒绝，不接管、不清理。
// PrepareAuthority复用正式DDL owner，shared不手写第二份catalog schema。
func New(ctx context.Context, t testing.TB, opts Options) (result *Fixture, resultErr error) {
	t.Helper()
	if !identifier(opts.AuthoritySchema) || len(opts.PhysicalAuthoritySHA256) != 32 || len(opts.LockQualificationSHA256) != 32 || (opts.LockPrimitive != pb.SqlWriterBootGrant_SELECT_ONLY && opts.LockPrimitive != pb.SqlWriterBootGrant_LOCK_TOUCH) {
		return nil, errors.New("SQL fixture缺少准确owner/schema/物理目标或已核最小权限原件")
	}
	schemaNames := append([]string(nil), opts.ProtectedSchemas...)
	sort.Strings(schemaNames)
	containsAuthority := false
	for i, name := range schemaNames {
		if !identifier(name) || i > 0 && schemaNames[i-1] == name {
			return nil, errors.New("SQL fixture schema清单无效")
		}
		containsAuthority = containsAuthority || name == opts.AuthoritySchema
	}
	if !containsAuthority {
		return nil, errors.New("SQL fixture受保护清单必须含authority schema")
	}
	if opts.PrepareAuthority == nil {
		opts.PrepareAuthority = authoritysql.PrepareCatalogSchema
	}
	base, err := mysql.ParseDSN(opts.AdminDSN)
	if err != nil {
		return nil, errors.New("SQL fixture DSN无效")
	}
	host, port, err := net.SplitHostPort(base.Addr)
	if err != nil || base.Net != "tcp" {
		return nil, errors.New("SQL fixture必须数值loopback TCP")
	}
	ip := net.ParseIP(host)
	number, err := strconv.Atoi(port)
	if err != nil || ip == nil || !ip.IsLoopback() || number < 1 || number > 65535 {
		return nil, errors.New("SQL fixture拒绝非本任务loopback端点")
	}
	base.DBName = "mysql"
	probe, err := authoritysql.Open(ctx, base.FormatDSN(), "mysql", !opts.AllowMySQL, 2, authoritysql.Reader())
	if err != nil {
		return nil, err
	}
	var principal string
	err = probe.QueryRowContext(ctx, "SELECT CURRENT_USER()").Scan(&principal)
	closeErr := probe.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	schema, err := authoritysql.OpenForMigration(ctx, base.FormatDSN(), "mysql", !opts.AllowMySQL, authoritysql.Schema(principal))
	if err != nil {
		return nil, err
	}
	owner, err := randomBytes(32)
	if err != nil {
		schema.Close()
		return nil, err
	}
	f := &Fixture{Schema: schema, base: base, admin: base.Clone(), adminPrincipal: principal, requireTiDB: !opts.AllowMySQL, authority: opts.AuthoritySchema, t: t, owner: owner, physical: append([]byte(nil), opts.PhysicalAuthoritySHA256...)}
	t.Cleanup(f.Close)
	defer func() {
		if resultErr != nil {
			f.Close()
		}
	}()
	receiptDir := opts.ReceiptDirectory
	if receiptDir == "" {
		receiptDir = os.Getenv("MMORPG_SQL_FIXTURE_RECEIPT_DIR")
	}
	if !filepath.IsAbs(receiptDir) {
		return nil, errors.New("SQL fixture缺少runner保留的私有原件目录")
	}
	if err := authoritysql.CheckPrivateControlDirectory(receiptDir); err != nil {
		return nil, errors.New("SQL fixture原件目录不是已有私有目录")
	}
	f.receiptPath = filepath.Join(receiptDir, "sql-fixture-"+hex.EncodeToString(owner)+".owner")
	if err = authoritysql.WritePrivateControlFile(f.receiptPath, append(append([]byte(nil), owner...), opts.PhysicalAuthoritySHA256...)); err != nil {
		return nil, err
	}
	schemas := append([]string(nil), opts.ProtectedSchemas...)
	sort.Strings(schemas)
	found := false
	for i, name := range schemas {
		if !identifier(name) || i > 0 && name == schemas[i-1] {
			return nil, errors.New("SQL fixture schema清单无效")
		}
		if name == opts.AuthoritySchema {
			found = true
		}
		var count int
		if err = schema.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name=?", name).Scan(&count); err != nil {
			return nil, err
		}
		if count != 0 {
			return nil, errors.New("SQL fixture拒绝接管已有schema")
		}
		if _, err = schema.ExecContext(ctx, "CREATE DATABASE "+q(name)); err != nil {
			return nil, err
		}
		// 已确认CREATE也不能代替持久归属。marker未确定时保留该库，绝不猜测清理。
		if _, err = schema.ExecContext(ctx, "CREATE TABLE "+q(name)+".`__sql_fixture_owner` (id TINYINT UNSIGNED NOT NULL,owner_nonce BINARY(32) NOT NULL,physical_sha256 BINARY(32) NOT NULL,normal_principal VARBINARY(288) NOT NULL DEFAULT '',PRIMARY KEY(id)) ENGINE=InnoDB"); err != nil {
			return nil, err
		}
		if _, err = schema.ExecContext(ctx, "INSERT INTO "+q(name)+".`__sql_fixture_owner`(id,owner_nonce,physical_sha256) VALUES(1,?,?)", owner, opts.PhysicalAuthoritySHA256); err != nil {
			return nil, err
		}
		f.created = append(f.created, name)
	}
	if !found {
		return nil, errors.New("SQL fixture受保护清单必须含authority schema")
	}
	generation := opts.Generation
	if generation == "" {
		id, err := randomBytes(16)
		if err != nil {
			return nil, err
		}
		generation = hex.EncodeToString(id)
	}
	f.Generation = generation
	// 预建回调含未限定的正规DDL，必须连接真正的authority schema，不能仍在mysql系统库。
	if err = schema.Close(); err != nil {
		return nil, err
	}
	base.DBName = opts.AuthoritySchema
	schema, err = authoritysql.OpenForMigration(ctx, base.FormatDSN(), opts.AuthoritySchema, f.requireTiDB, authoritysql.Schema(principal))
	if err != nil {
		return nil, err
	}
	f.Schema = schema
	if err = opts.PrepareAuthority(ctx, schema, opts.AuthoritySchema, generation); err != nil {
		return nil, err
	}
	operator, err := authoritysql.Open(ctx, base.FormatDSN(), opts.AuthoritySchema, f.requireTiDB, 2, authoritysql.Operator(principal))
	if err != nil {
		return nil, err
	}
	f.Operator = operator
	f.Catalog, err = authoritysql.NewCatalog(operator, authoritysql.CatalogConfig{AuthoritySchema: opts.AuthoritySchema, Generation: generation, ProtectedSchemas: schemas, PhysicalAuthoritySHA256: opts.PhysicalAuthoritySHA256, Principal: principal, RequireReconcile: true})
	if err != nil {
		return nil, err
	}
	name, err := randomBytes(8)
	if err != nil {
		return nil, err
	}
	f.user = "writer_fixture_" + hex.EncodeToString(name)
	secret, err := randomBytes(32)
	if err != nil {
		return nil, err
	}
	password := hex.EncodeToString(secret)
	// 用户与密码只用随机hex，不接受SQL文本；从不进入日志、plan或grant原件。
	if _, err = schema.ExecContext(ctx, "CREATE USER '"+f.user+"'@'%' IDENTIFIED BY '"+password+"'"); err != nil {
		return nil, err
	}
	f.userCreated = true
	for _, name := range f.created {
		result, err := schema.ExecContext(ctx, "UPDATE "+q(name)+".`__sql_fixture_owner` SET normal_principal=? WHERE id=1 AND owner_nonce=? AND physical_sha256=?", []byte(f.user+"@%"), owner, opts.PhysicalAuthoritySHA256)
		if err != nil {
			return nil, err
		}
		rows, err := result.RowsAffected()
		if err != nil || rows != 1 {
			return nil, errors.New("fixture写入自有schema主体原件未确认")
		}
	}
	for _, table := range []string{"route_meta", "writer_registry", "writer_slot", "writer_lifecycle_receipt", "sql_writer_drain_run", "sql_writer_freeze_ticket", "sql_writer_maintenance_receipt"} {
		if _, err = schema.ExecContext(ctx, "GRANT SELECT ON "+q(opts.AuthoritySchema)+"."+q(table)+" TO '"+f.user+"'@'%'"); err != nil {
			return nil, err
		}
	}
	if opts.LockPrimitive == pb.SqlWriterBootGrant_LOCK_TOUCH {
		if _, err = schema.ExecContext(ctx, "GRANT UPDATE(lock_touch) ON "+q(opts.AuthoritySchema)+".`writer_slot` TO '"+f.user+"'@'%'"); err != nil {
			return nil, err
		}
	}
	f.bootOptions = opts
	f.bootOptions.ProtectedSchemas = append([]string(nil), schemas...)
	f.base = base.Clone()
	f.base.User = f.user
	f.base.Passwd = password
	if err = f.registerBoot(ctx, nil); err != nil {
		return nil, err
	}
	return f, nil
}
func (f *Fixture) DSN(schema string) string {
	cfg := f.base.Clone()
	cfg.DBName = schema
	return cfg.FormatDSN()
}
func (f *Fixture) Open(ctx context.Context, schema string, maxOpen int) (*sql.DB, error) {
	allowed := false
	for _, name := range f.created {
		if name == schema {
			allowed = true
		}
	}
	if !allowed {
		return nil, errors.New("SQL fixture拒绝非自有schema")
	}
	db, err := authoritysql.Open(ctx, f.DSN(schema), schema, f.requireTiDB, maxOpen, f.Permission)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.pools = append(f.pools, db)
	f.mu.Unlock()
	return db, nil
}

func (f *Fixture) OpenSchema(ctx context.Context, schema string) (*sql.DB, error) {
	allowed := false
	for _, name := range f.created {
		if name == schema {
			allowed = true
		}
	}
	if !allowed {
		return nil, errors.New("fixture拒绝非自有schema准备池")
	}
	config := f.admin.Clone()
	config.DBName = schema
	db, err := authoritysql.OpenForMigration(ctx, config.FormatDSN(), schema, f.requireTiDB, authoritysql.Schema(f.adminPrincipal))
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.pools = append(f.pools, db)
	f.mu.Unlock()
	return db, nil
}

func (f *Fixture) GrantProtoTables(ctx context.Context, schema string, models []proto.Message) error {
	seen := make(map[string]bool)
	for _, model := range models {
		if model == nil {
			return errors.New("fixture proto表原型为空")
		}
		name, ok := proto.GetExtension(model.ProtoReflect().Descriptor().Options(), dbopts.E_OptionTableName).(string)
		if !ok || !identifier(name) {
			return errors.New("fixture proto未声明合法固定表名")
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		if err := f.GrantTables(ctx, schema, name); err != nil {
			return err
		}
	}
	return nil
}
func (f *Fixture) GrantTables(ctx context.Context, schema string, tables ...string) error {
	allowed := false
	for _, name := range f.created {
		if name == schema {
			allowed = true
		}
	}
	if !allowed {
		return errors.New("SQL fixture拒绝非自有schema授权")
	}
	for _, table := range tables {
		if !identifier(table) {
			return errors.New("SQL fixture业务表名无效")
		}
		if table == "__sql_fixture_owner" {
			return errors.New("SQL fixture禁止normal修改归属原件")
		}
		if schema == f.authority {
			switch table {
			case "route_meta", "writer_registry", "writer_slot", "writer_lifecycle_receipt", "sql_writer_drain_run", "sql_writer_freeze_ticket", "sql_writer_maintenance_receipt":
				return errors.New("SQL fixture禁止normal写authority元数据")
			}
		}
		if _, err := f.Schema.ExecContext(ctx, "GRANT SELECT,INSERT,UPDATE,DELETE ON "+q(schema)+"."+q(table)+" TO '"+f.user+"'@'%'"); err != nil {
			return err
		}
	}
	return nil
}
func (f *Fixture) Close() {
	f.cleanup.Do(func() {
		f.mu.Lock()
		pools := append([]*sql.DB(nil), f.pools...)
		f.mu.Unlock()
		for _, db := range pools {
			if err := db.Close(); err != nil {
				f.t.Errorf("fixture关闭业务池: %v", err)
			}
		}
		if f.Operator != nil {
			if err := f.Operator.Close(); err != nil {
				f.t.Errorf("fixture关闭operator池: %v", err)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		// 所有归属先核，再做第一个清理副作用；未知或被替换的对象保留，不猜测接管。
		owned := f.Schema != nil && !f.registrationPending
		if len(f.created) > 0 {
			raw, err := authoritysql.ReadPrivateControlFile(f.receiptPath, 64)
			if err != nil || !bytes.Equal(raw, append(append([]byte(nil), f.owner...), f.physical...)) {
				owned = false
			}
		}
		for _, name := range f.created {
			if !owned {
				break
			}
			var owner, physical, principal []byte
			err := f.Schema.QueryRowContext(ctx, "SELECT owner_nonce,physical_sha256,normal_principal FROM "+q(name)+".`__sql_fixture_owner` WHERE id=1").Scan(&owner, &physical, &principal)
			if err != nil || !bytes.Equal(owner, f.owner) || !bytes.Equal(physical, f.physical) || (f.userCreated && string(principal) != f.user+"@%") {
				owned = false
			}
		}
		if !owned {
			f.t.Errorf("fixture归属或启动登记原件未完整确认，保留全部剩余对象与原件 %s", f.receiptPath)
		} else {
			if f.userCreated {
				if _, err := f.Schema.ExecContext(ctx, "DROP USER '"+f.user+"'@'%'"); err != nil {
					f.t.Errorf("fixture清理自有主体未确认，保留schema与原件: %v", err)
					owned = false
				}
			}
			if owned {
				names := append([]string(nil), f.created...)
				sort.Slice(names, func(i, j int) bool {
					if names[i] == f.authority {
						return false
					}
					if names[j] == f.authority {
						return true
					}
					return names[i] < names[j]
				})
				for _, name := range names {
					if _, err := f.Schema.ExecContext(ctx, "DROP DATABASE "+q(name)); err != nil {
						f.t.Errorf("fixture清理自有schema未确认，保留原件: %v", err)
						break
					}
				}
			}
		}
		if f.Schema != nil {
			if err := f.Schema.Close(); err != nil {
				f.t.Errorf("fixture关闭schema池: %v", err)
			}
		}
	})
}

// PrepareOwnedSchema只给测试构建调用：核同一夹具持久归属后复用业务的正规建表实现。
// 生产schema入口仍执行fresh-prestart门禁；本方法不构造或授予任何在线writer权限。
func (f *Fixture) PrepareOwnedSchema(ctx context.Context, schema string, prepare func(context.Context, *sql.DB) error) error {
	if prepare == nil {
		return errors.New("fixture缺少明确schema准备操作")
	}
	db, err := f.OpenSchema(ctx, schema)
	if err != nil {
		return err
	}
	var owner, physical, principal []byte
	if err = db.QueryRowContext(ctx, "SELECT owner_nonce,physical_sha256,normal_principal FROM "+q(schema)+"."+q("__sql_fixture_owner")+" WHERE id=1").Scan(&owner, &physical, &principal); err != nil {
		return err
	}
	if !bytes.Equal(owner, f.owner) || !bytes.Equal(physical, f.physical) || string(principal) != f.user+"@%" {
		return errors.New("fixture schema持久归属漂移，拒绝准备")
	}
	return prepare(ctx, db)
}

// registerBoot每次形成永久新身份；释放后从不重新激活旧writer或复用消费标记。
func (f *Fixture) registerBoot(ctx context.Context, runID []byte) error {
	opts := f.bootOptions
	writerID, err := randomBytes(16)
	if err != nil {
		return err
	}
	instance, err := randomBytes(16)
	if err != nil {
		return err
	}
	requestID, err := randomBytes(16)
	if err != nil {
		return err
	}
	grantID, err := randomBytes(16)
	if err != nil {
		return err
	}
	writer := &pb.SqlWriterRegistry{WriterKey: writerID, Generation: f.Generation, InstanceId: instance, SqlPrincipal: []byte(f.user + "@%"), State: pb.SqlWriterRegistry_ACTIVE, RegisteredMs: uint64(time.Now().UnixMilli())}
	meta, err := f.Catalog.CurrentMeta(ctx)
	if err != nil {
		return err
	}
	deployment := "fixture-" + hex.EncodeToString(instance)
	request := &pb.SqlWriterLifecycleReceipt_Request{RequestId: requestID, Kind: pb.SqlWriterLifecycleReceipt_REGISTER, Writer: writer, ExpectedEpoch: meta.WriterEpoch, ExpectedCatalogRevision: meta.CatalogRevision, AuthoritySchema: f.authority, ProtectedSchemas: opts.ProtectedSchemas, DeploymentDigest: f.physical, DeploymentId: deployment, LockPrimitive: opts.LockPrimitive, LockQualificationSha256: opts.LockQualificationSHA256, MaintenanceRunId: bytes.Clone(runID)}
	request, err = authoritysql.CanonicalRegistrationRequest(request)
	if err != nil {
		return err
	}
	writer = request.Writer
	// 在第一次提交前保存同一canonical请求并置pending；错误可能发生于提交之后，禁止清理猜测。
	f.registration = request
	f.registrationReceipt = nil
	f.registrationPending = true
	registered, err := f.Catalog.Register(ctx, request)
	if err != nil {
		return err
	}
	// REGISTER已经确认；随后的本机grant/Config失败也必须保留这次真实登记，不覆盖或换身份重试。
	f.registrationReceipt = registered
	grant := &pb.SqlWriterBootGrant{Format: "SQL_WRITER_BOOT_V1", WriterKey: writerID, InstanceId: instance, Generation: f.Generation, AuthoritySchema: f.authority, ProtectedSchemas: opts.ProtectedSchemas, SqlPrincipal: writer.SqlPrincipal, RegistrationDigest: writer.RegistrationDigest, DeploymentId: deployment, GrantId: grantID, GrantedMs: writer.RegisteredMs, Permission: "NORMAL", PhysicalAuthoritySha256: f.physical, LockPrimitive: opts.LockPrimitive, LockQualificationSha256: opts.LockQualificationSHA256, MaintenanceRunId: bytes.Clone(runID)}
	raw, err := protojson.Marshal(grant)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(raw)
	// 复用New已核私有ACL且保留原件的本run目录；Windows的Chmod不提供私有DACL。
	// grantID每次登记都是新随机原件，CREATE_NEW拒碰撞，消费标记也只属于本次grant。
	dir := filepath.Dir(f.receiptPath)
	path := filepath.Join(dir, "fixture-grant-"+hex.EncodeToString(grantID)+".json")
	if err = authoritysql.WritePrivateControlFile(path, raw); err != nil {
		return err
	}
	env := map[string]string{"MMORPG_SQL_WRITER_GRANT_FILE": path, "MMORPG_SQL_WRITER_GRANT_SHA256": hex.EncodeToString(sum[:]), "MMORPG_SQL_WRITER_CONSUME_DIR": dir, "MMORPG_SQL_DEPLOYMENT_ID": grant.DeploymentId, "MMORPG_SQL_PHYSICAL_AUTHORITY_SHA256": hex.EncodeToString(f.physical)}
	config, err := authoritysql.LoadWriterConfig(func(name string) string { return env[name] })
	if err != nil {
		return err
	}
	f.Config = config
	f.Permission = authoritysql.Writer(config)
	f.registrationPending = false
	return nil
}

type fixtureMaintenance struct {
	plan         *pb.SqlWriterMaintenanceReceipt_Plan
	planRaw      []byte
	manifest     []byte
	latest       *pb.SqlWriterMaintenanceReceipt
	nonSQL       *pb.SqlWriterMaintenanceReceipt
	frozen       *pb.SqlWriterMaintenanceReceipt
	releaseInput *pb.SqlWriterMaintenanceReceipt
	released     *pb.SqlWriterMaintenanceReceipt
	ticketID     []byte
	reopened     bool
}

func (f *Fixture) controlPin(raw []byte) (*pb.SqlWriterMaintenanceReceipt_SourcePin, error) {
	id, err := randomBytes(16)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(filepath.Dir(f.receiptPath), "fixture-control-"+hex.EncodeToString(id)+".json")
	if err = authoritysql.WritePrivateControlFile(path, raw); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	return &pb.SqlWriterMaintenanceReceipt_SourcePin{AbsolutePath: path, Sha256: sum[:]}, nil
}
func (f *Fixture) maintenanceRequest(ctx context.Context, m *fixtureMaintenance, phase pb.SqlWriterDrainRun_Phase, kind pb.SqlWriterMaintenanceReceipt_Kind) (*pb.SqlWriterMaintenanceReceipt, error) {
	meta, err := f.Catalog.CurrentMeta(ctx)
	if err != nil {
		return nil, err
	}
	id, err := randomBytes(16)
	if err != nil {
		return nil, err
	}
	out := &pb.SqlWriterMaintenanceReceipt{RequestId: id, RunId: m.plan.RunId, Generation: f.Generation, BeforeEpoch: meta.WriterEpoch, AfterEpoch: meta.WriterEpoch, CatalogRevision: meta.CatalogRevision, PlanProto: m.planRaw, Kind: kind, RunPhase: phase, AttemptRevision: 1, Evidence: &pb.SqlWriterMaintenanceReceipt_Evidence{OperationId: m.plan.RunId, ManifestDigest: m.manifest}}
	if m.latest != nil {
		out.PriorReceiptId = bytes.Clone(m.latest.RequestId)
		out.PriorReceiptDigest = bytes.Clone(m.latest.ReceiptDigest)
	}
	return out, nil
}
func (f *Fixture) advanceFixture(ctx context.Context, m *fixtureMaintenance, input *pb.SqlWriterMaintenanceReceipt) error {
	sealed, err := authoritysql.SealMaintenanceRequest(input)
	if err != nil {
		return err
	}
	next := &pb.SqlWriterDrainRun{OperationId: m.plan.RunId, RequestDigest: sealed.RequestDigest, Generation: f.Generation, AttemptRevision: 1, ManifestProto: m.planRaw, ManifestDigest: m.manifest, Phase: input.RunPhase}
	m.latest, err = f.Catalog.Advance(ctx, next, sealed)
	return err
}

// Freeze只证明本夹具的同步SQL发行者关闭。没有Kafka/Redis发行者，不能用于全栈排空验收。
// 调用者须已join自己启动的业务goroutine；本方法关闭已登记测试池、永久Seal旧boot，再走正式退休和冻结。
func (f *Fixture) Freeze(ctx context.Context) (*pb.SqlWriterMaintenanceReceipt, error) {
	f.controlMu.Lock()
	defer f.controlMu.Unlock()
	if f.maintenance != nil && !f.maintenance.reopened {
		if f.maintenance.frozen != nil {
			return proto.Clone(f.maintenance.frozen).(*pb.SqlWriterMaintenanceReceipt), nil
		}
		return nil, errors.New("fixture维护已有未完成尝试，保留现场")
	}
	meta, err := f.Catalog.CurrentMeta(ctx)
	if err != nil {
		return nil, err
	}
	runID, err := randomBytes(16)
	if err != nil {
		return nil, err
	}
	issuerID, err := randomBytes(16)
	if err != nil {
		return nil, err
	}
	revival, err := randomBytes(16)
	if err != nil {
		return nil, err
	}
	pin, err := f.controlPin([]byte("本夹具只拥有同步SQL调用；不启动外部Kafka、Redis发布者或后台消费者。"))
	if err != nil {
		return nil, err
	}
	seed := &pb.SqlWriterMaintenanceReceipt_IssuerClosure{IssuerId: issuerID, InstanceId: f.Config.InstanceID(), PhysicalWriterKey: f.Config.WriterKey(), LogicalRole: "SOURCE", PermanentRevivalFenceId: revival, SourceEvidence: []*pb.SqlWriterMaintenanceReceipt_SourcePin{pin}}
	m := &fixtureMaintenance{plan: &pb.SqlWriterMaintenanceReceipt_Plan{PhysicalClusterIdentity: f.physical, AuthoritySchema: f.authority, ProtectedSchemas: f.bootOptions.ProtectedSchemas, RunId: runID, BeforeEpoch: meta.WriterEpoch, CatalogRevision: meta.CatalogRevision, ActiveWriterKeys: [][]byte{f.Config.WriterKey()}, RootIssuers: []*pb.SqlWriterMaintenanceReceipt_IssuerClosure{seed}}}
	f.maintenance = m
	m.planRaw, err = (proto.MarshalOptions{Deterministic: true}).Marshal(m.plan)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(m.planRaw)
	m.manifest = digest[:]
	input, err := f.maintenanceRequest(ctx, m, pb.SqlWriterDrainRun_PLAN_FIXED, pb.SqlWriterMaintenanceReceipt_PHASE_EVENT)
	if err != nil {
		return nil, err
	}
	if err = f.advanceFixture(ctx, m, input); err != nil {
		return nil, err
	}
	f.mu.Lock()
	pools := append([]*sql.DB(nil), f.pools...)
	f.mu.Unlock()
	for _, pool := range pools {
		if err = pool.Close(); err != nil {
			return nil, err
		}
	}
	f.Config.Seal()
	root := drainproof.New()
	root.CloseAdmission()
	closed, err := root.Closure(ctx, seed)
	if err != nil {
		return nil, err
	}
	input, err = f.maintenanceRequest(ctx, m, pb.SqlWriterDrainRun_ROOTS_CLOSED, pb.SqlWriterMaintenanceReceipt_ROLE_CLOSED)
	if err != nil {
		return nil, err
	}
	input.Evidence.Detail = &pb.SqlWriterMaintenanceReceipt_Evidence_Publication{Publication: &pb.SqlWriterMaintenanceReceipt_PublicationEvidence{LogicalRoles: []*pb.SqlWriterMaintenanceReceipt_IssuerClosure{closed}}}
	if err = f.advanceFixture(ctx, m, input); err != nil {
		return nil, err
	}
	retire := proto.Clone(f.registration).(*pb.SqlWriterLifecycleReceipt_Request)
	retire.RequestId, err = randomBytes(16)
	if err != nil {
		return nil, err
	}
	retire.Kind = pb.SqlWriterLifecycleReceipt_RETIRE
	retire.MaintenanceRunId = runID
	retire.ExpectedEpoch = meta.WriterEpoch
	retire.ExpectedCatalogRevision = meta.CatalogRevision
	if _, err = f.Catalog.Retire(ctx, retire); err != nil {
		return nil, err
	}
	input, err = f.maintenanceRequest(ctx, m, pb.SqlWriterDrainRun_DRAINING, pb.SqlWriterMaintenanceReceipt_PHASE_EVENT)
	if err != nil {
		return nil, err
	}
	if err = f.advanceFixture(ctx, m, input); err != nil {
		return nil, err
	}
	input, err = f.maintenanceRequest(ctx, m, pb.SqlWriterDrainRun_NON_SQL_CLOSED, pb.SqlWriterMaintenanceReceipt_PHASE_EVENT)
	if err != nil {
		return nil, err
	}
	input.Evidence.Detail = &pb.SqlWriterMaintenanceReceipt_Evidence_NonSqlClosure{NonSqlClosure: &pb.SqlWriterMaintenanceReceipt_NonSqlClosureEvidence{}}
	if err = f.advanceFixture(ctx, m, input); err != nil {
		return nil, err
	}
	m.nonSQL = m.latest
	m.ticketID, err = randomBytes(16)
	if err != nil {
		return nil, err
	}
	input, err = f.maintenanceRequest(ctx, m, pb.SqlWriterDrainRun_SQL_FROZEN, pb.SqlWriterMaintenanceReceipt_FREEZE)
	if err != nil {
		return nil, err
	}
	input.AfterEpoch++
	input.Phase = pb.SqlWriterMaintenanceReceipt_FROZEN
	input.Evidence.Detail = &pb.SqlWriterMaintenanceReceipt_Evidence_Ticket{Ticket: &pb.SqlWriterMaintenanceReceipt_TicketEvidence{TicketId: m.ticketID, AuthorityOperationId: runID, BoundRequestId: input.RequestId}}
	input, err = authoritysql.SealMaintenanceRequest(input)
	if err != nil {
		return nil, err
	}
	var now uint64
	if err = f.Operator.QueryRowContext(ctx, "SELECT CAST(UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000 AS UNSIGNED)").Scan(&now); err != nil {
		return nil, err
	}
	ticket := &pb.SqlWriterFreezeTicket{TicketId: m.ticketID, OperationId: runID, AttemptRevision: 1, Generation: f.Generation, ManifestDigest: m.manifest, NonSqlReceiptDigest: m.nonSQL.ReceiptDigest, FreezeRequestId: input.RequestId, FreezeRequestDigest: input.RequestDigest, NotBeforeMs: now, ExpiresMs: now + 30000, State: pb.SqlWriterFreezeTicket_AVAILABLE, AuthorizationKind: pb.SqlWriterFreezeTicket_NORMAL_DRAIN_FREEZE}
	ticketDigest, err := authoritysql.SealFreezeTicketRequest(ticket)
	if err != nil {
		return nil, err
	}
	grant, err := f.maintenanceRequest(ctx, m, pb.SqlWriterDrainRun_FREEZE_AUTHORIZED, pb.SqlWriterMaintenanceReceipt_TICKET_GRANTED)
	if err != nil {
		return nil, err
	}
	grant.Evidence.Detail = &pb.SqlWriterMaintenanceReceipt_Evidence_Ticket{Ticket: &pb.SqlWriterMaintenanceReceipt_TicketEvidence{TicketId: m.ticketID, AuthorityOperationId: runID, BoundRequestId: input.RequestId, BoundRequestDigest: input.RequestDigest, TicketRequestDigest: ticketDigest}}
	grant, err = authoritysql.SealMaintenanceRequest(grant)
	if err != nil {
		return nil, err
	}
	if _, err = f.Catalog.GrantTicket(ctx, ticket, grant); err != nil {
		return nil, err
	}
	m.frozen, err = f.Catalog.Freeze(ctx, m.ticketID, input)
	if err != nil {
		return nil, err
	}
	m.latest = m.frozen
	return proto.Clone(m.frozen).(*pb.SqlWriterMaintenanceReceipt), nil
}
func (f *Fixture) PrepareRelease(ctx context.Context, floor uint64) ([]byte, *pb.SqlWriterMaintenanceReceipt, error) {
	f.controlMu.Lock()
	defer f.controlMu.Unlock()
	return f.prepareRelease(ctx, floor)
}
func (f *Fixture) prepareRelease(ctx context.Context, floor uint64) ([]byte, *pb.SqlWriterMaintenanceReceipt, error) {
	m := f.maintenance
	if m == nil || m.frozen == nil || m.reopened {
		return nil, nil, errors.New("fixture缺少本轮正式冻结")
	}
	if m.releaseInput != nil {
		if m.releaseInput.Evidence.GetReleaseReadiness().RouteWatermarkFloorMs != floor {
			return nil, nil, errors.New("fixture释放请求水位已固定")
		}
		return bytes.Clone(m.ticketID), proto.Clone(m.releaseInput).(*pb.SqlWriterMaintenanceReceipt), nil
	}
	pin, err := f.controlPin([]byte("仅SQL隔离用例；没有历史非SQL队列；恢复业务断言由调用测试验证。"))
	if err != nil {
		return nil, nil, err
	}
	proof := &pb.SqlWriterMaintenanceReceipt_ReleaseReadiness{OriginalFreezeReceiptId: m.frozen.RequestId, OriginalFreezeReceiptDigest: m.frozen.ReceiptDigest, RecoveryAudit: []*pb.SqlWriterMaintenanceReceipt_SourcePin{pin}, NonSqlClosedReceiptId: m.nonSQL.RequestId, NonSqlClosedReceiptDigest: m.nonSQL.ReceiptDigest, RouteWatermarkFloorMs: floor}
	ready, err := f.maintenanceRequest(ctx, m, pb.SqlWriterDrainRun_RELEASE_READY, pb.SqlWriterMaintenanceReceipt_PHASE_EVENT)
	if err != nil {
		return nil, nil, err
	}
	ready.Evidence.Detail = &pb.SqlWriterMaintenanceReceipt_Evidence_ReleaseReadiness{ReleaseReadiness: proof}
	if err = f.advanceFixture(ctx, m, ready); err != nil {
		return nil, nil, err
	}
	input, err := f.maintenanceRequest(ctx, m, pb.SqlWriterDrainRun_SQL_RELEASED, pb.SqlWriterMaintenanceReceipt_RELEASE)
	if err != nil {
		return nil, nil, err
	}
	input.AfterEpoch++
	input.Phase = pb.SqlWriterMaintenanceReceipt_RELEASED
	input.Evidence.Detail = &pb.SqlWriterMaintenanceReceipt_Evidence_ReleaseReadiness{ReleaseReadiness: proto.Clone(proof).(*pb.SqlWriterMaintenanceReceipt_ReleaseReadiness)}
	m.releaseInput, err = authoritysql.SealMaintenanceRequest(input)
	if err != nil {
		return nil, nil, err
	}
	return bytes.Clone(m.ticketID), proto.Clone(m.releaseInput).(*pb.SqlWriterMaintenanceReceipt), nil
}
func (f *Fixture) Release(ctx context.Context, floor uint64) (*pb.SqlWriterMaintenanceReceipt, error) {
	f.controlMu.Lock()
	defer f.controlMu.Unlock()
	ticket, input, err := f.prepareRelease(ctx, floor)
	if err != nil {
		return nil, err
	}
	released, err := f.Catalog.Release(ctx, ticket, input)
	if err != nil {
		return nil, err
	}
	f.maintenance.released = released
	if err = f.reopenAfterRelease(ctx); err != nil {
		return nil, err
	}
	return released, nil
}
func (f *Fixture) ReopenAfterRelease(ctx context.Context) error {
	f.controlMu.Lock()
	defer f.controlMu.Unlock()
	return f.reopenAfterRelease(ctx)
}
func (f *Fixture) reopenAfterRelease(ctx context.Context) error {
	m := f.maintenance
	if m == nil || m.releaseInput == nil {
		return errors.New("fixture释放请求未准备")
	}
	if m.reopened {
		return nil
	}
	released, err := f.Catalog.MaintenanceReceipt(ctx, m.releaseInput.RequestId)
	if err != nil {
		return err
	}
	if released.Kind != pb.SqlWriterMaintenanceReceipt_RELEASE || released.RunPhase != pb.SqlWriterDrainRun_SQL_RELEASED || !bytes.Equal(released.RequestDigest, m.releaseInput.RequestDigest) {
		return errors.New("fixture释放尚未确定提交")
	}
	m.released = released
	m.latest = released
	if err = f.registerBoot(ctx, m.plan.RunId); err != nil {
		return err
	}
	raw, err := protojson.Marshal(f.registrationReceipt)
	if err != nil {
		return err
	}
	registrationPin, err := f.controlPin(raw)
	if err != nil {
		return err
	}
	gate := drainproof.NewPrepared()
	now := uint64(time.Now().UnixMilli())
	seed := m.plan.RootIssuers[0]
	pins := []*pb.SqlWriterMaintenanceReceipt_SourcePin{registrationPin, seed.SourceEvidence[0]}
	ready := &pb.SqlWriterMaintenanceReceipt_IssuerReadiness{IssuerId: seed.IssuerId, InstanceId: f.Config.InstanceID(), PhysicalWriterKey: f.Config.WriterKey(), LogicalRole: seed.LogicalRole, RegistrationReceiptId: f.registrationReceipt.RequestId, RegistrationReceiptDigest: f.registrationReceipt.RequestDigest, PreparedClosedMs: now, ConsumerReadyFrontierDigest: gate.SnapshotDigest(), SourcePins: pins}
	if err = gate.OpenPrepared(); err != nil {
		return err
	}
	openID, err := randomBytes(16)
	if err != nil {
		return err
	}
	ack := &pb.SqlWriterMaintenanceReceipt_SourceOpenAck{OperationId: m.plan.RunId, ManifestDigest: m.manifest, IssuerId: ready.IssuerId, InstanceId: ready.InstanceId, PhysicalWriterKey: ready.PhysicalWriterKey, LogicalRole: ready.LogicalRole, RegistrationReceiptId: ready.RegistrationReceiptId, RegistrationReceiptDigest: ready.RegistrationReceiptDigest, OpenRequestId: openID, OpenedMs: uint64(time.Now().UnixMilli()), AcceptedFrontierDigest: gate.SnapshotDigest(), SourcePins: pins}
	raw, err = protojson.Marshal(ack)
	if err != nil {
		return err
	}
	ackPin, err := f.controlPin(raw)
	if err != nil {
		return err
	}
	input, err := f.maintenanceRequest(ctx, m, pb.SqlWriterDrainRun_PUBLICATION_REOPENED, pb.SqlWriterMaintenanceReceipt_PHASE_EVENT)
	if err != nil {
		return err
	}
	input.Evidence.Detail = &pb.SqlWriterMaintenanceReceipt_Evidence_PublicationReadiness{PublicationReadiness: &pb.SqlWriterMaintenanceReceipt_PublicationReadiness{ReleaseReceiptId: released.RequestId, ReleaseReceiptDigest: released.ReceiptDigest, PreparedBoots: []*pb.SqlWriterMaintenanceReceipt_IssuerReadiness{ready}, SourceOpenAcks: []*pb.SqlWriterMaintenanceReceipt_SourcePin{ackPin}}}
	if err = f.advanceFixture(ctx, m, input); err != nil {
		return err
	}
	m.reopened = true
	return nil
}

// OperatorDSN与OperatorPermission仅由该自有SQL夹具的维护调用方显式使用。
func (f *Fixture) OperatorDSN(schema string) (string, error) {
	owned := false
	for _, name := range f.created {
		owned = owned || name == schema
	}
	if !owned {
		return "", errors.New("fixture拒绝非自有维护schema")
	}
	config := f.admin.Clone()
	config.DBName = schema
	return config.FormatDSN(), nil
}
func (f *Fixture) OperatorPermission() authoritysql.Permission {
	return authoritysql.Operator(f.adminPrincipal)
}
