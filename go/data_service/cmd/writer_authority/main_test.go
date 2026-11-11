package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	dbpb "proto/common/database"
	"shared/authoritysql"
	"shared/dbplacement"
)

// lifecycleEpochForTest 是夹具里闸门当前的 epoch。RETIRE 的请求绑定它,REGISTER 的请求不带,收据记实际值。
const lifecycleEpochForTest = 7

func schemaPrepareEnvironment(extra map[string]string) func(string) string {
	values := map[string]string{"MMORPG_SQL_SCHEMA_DSN": "schema_test:test-only@tcp(127.0.0.1:1)/mall_test", "MMORPG_SQL_SCHEMA_PRINCIPAL": "schema_test@localhost"}
	for name, value := range extra {
		values[name] = value
	}
	return func(name string) string { return values[name] }
}

func TestPrepareSchemaRequiresDeploymentPlacementBeforeSQL(t *testing.T) {
	getenv := schemaPrepareEnvironment(map[string]string{dbplacement.EnvRequired: "1"})
	if err := prepareSchema(context.Background(), "mall_test", getenv); !errors.Is(err, dbplacement.ErrPlacementRequired) {
		t.Fatalf("缺少部署要求的映射必须先拒绝，不能拨SQL：%v", err)
	}
	getenv = schemaPrepareEnvironment(map[string]string{dbplacement.EnvConfig: "{"})
	if err := prepareSchema(context.Background(), "mall_test", getenv); err == nil {
		t.Fatal("坏映射配置不能回退旧DSN")
	}
}

// 真实etcd客户端只连本例有限gRPC替身；SQL端点是不可用端口，正确路径
// 必须在任何SQL连接前因MOVING拒绝。误用ForOneShot会变成拨SQL错误，本例判红。
type movingSchemaPlacement struct {
	etcdserverpb.UnimplementedKVServer
}

func (movingSchemaPlacement) Txn(context.Context, *etcdserverpb.TxnRequest) (*etcdserverpb.TxnResponse, error) {
	return &etcdserverpb.TxnResponse{Header: &etcdserverpb.ResponseHeader{Revision: 1}, Succeeded: true}, nil
}

func (movingSchemaPlacement) Range(_ context.Context, request *etcdserverpb.RangeRequest) (*etcdserverpb.RangeResponse, error) {
	if string(request.Key) != dbplacement.Key("mall_test") {
		return nil, errors.New("本例只接受Mall目标库映射")
	}
	return &etcdserverpb.RangeResponse{Header: &etcdserverpb.ResponseHeader{Revision: 2}, Count: 1, Kvs: []*mvccpb.KeyValue{{Key: request.Key, Value: []byte(`{"cluster":"moved","version":2,"state":"MOVING"}`), ModRevision: 2}}}, nil
}

func TestPrepareSchemaRefusesMovingBeforeOpeningOldSQLAddress(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	etcdserverpb.RegisterKVServer(server, &movingSchemaPlacement{})
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Stop(); _ = listener.Close(); <-done })
	config := dbplacement.Config{DefaultCluster: "main", SqlClusters: map[string]dbplacement.SqlCluster{"main": {Hosts: "127.0.0.1:1"}, "moved": {Hosts: "127.0.0.1:2"}}, EtcdHosts: []string{listener.Addr().String()}}
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	getenv := schemaPrepareEnvironment(map[string]string{dbplacement.EnvRequired: "1", dbplacement.EnvConfig: string(raw)})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := prepareSchema(ctx, "mall_test", getenv); !errors.Is(err, dbplacement.ErrMoving) {
		t.Fatalf("MOVING应拒绝DDL，不能回退或先连SQL：%v", err)
	}
}

func TestPrepareSchemaRejectsMixedCapabilitiesBeforeSQL(t *testing.T) {
	for _, args := range [][]string{
		{"-command", "prepare-schema", "-schema", "mall_test", "-request", ""},
		{"-command", "prepare-schema", "-schema", "mall_test", "-timeout", "0s"},
		{"-command", "prepare-schema", "-schema", "mall_test", "-timeout", "61s"},
		{"-command", "prepare-schema", "-schema", "mall_test", "extra"},
		{"-command", "register", "-schema", "mall_test"},
	} {
		reads := 0
		getenv := func(string) string { reads++; return "" }
		if err := run(context.Background(), args, getenv, &bytes.Buffer{}); err == nil || reads != 0 {
			t.Fatalf("混合命令应在读取能力前拒绝：args=%v,env_reads=%d,err=%v", args, reads, err)
		}
	}
	for _, key := range []string{"MMORPG_SQL_WRITER_GRANT_FILE", "MMORPG_SQL_REGISTRAR_DSN", "MMORPG_SQL_MAINTENANCE_DSN"} {
		getenv := func(name string) string {
			if name == key {
				return "test-only-present"
			}
			return ""
		}
		if err := prepareSchema(context.Background(), "mall_test", getenv); err == nil {
			t.Fatalf("不应接受混入的能力：%s", key)
		}
	}
}

func TestPrepareSchemaRejectsMissingOrWrongDatabaseBeforeSQL(t *testing.T) {
	for _, tc := range []struct{ schema, dsn, principal string }{
		{"", "", ""},
		{"mall_test", "", "schema_test@localhost"},
		{"mall_test", "schema_test:test-only@tcp(127.0.0.1:1)/another_test", "schema_test@localhost"},
		{"mall_test;DROP", "schema_test:test-only@tcp(127.0.0.1:1)/mall_test;DROP", "schema_test@localhost"},
		{"mall_test", "schema_test:test-only@tcp(127.0.0.1:1)/mall_test", ""},
	} {
		getenv := func(key string) string {
			switch key {
			case "MMORPG_SQL_SCHEMA_DSN":
				return tc.dsn
			case "MMORPG_SQL_SCHEMA_PRINCIPAL":
				return tc.principal
			}
			return ""
		}
		if err := prepareSchema(context.Background(), tc.schema, getenv); err == nil {
			t.Fatal("缺失Schema身份或目标库不符不应进入SQL")
		}
	}
}

func lifecycleRequestForTest(kind dbpb.SqlWriterLifecycleReceipt_Kind) *dbpb.SqlWriterLifecycleReceipt_Request {
	request := &dbpb.SqlWriterLifecycleReceipt_Request{
		RequestId: bytes.Repeat([]byte{1}, 16), Kind: kind,
		Writer: &dbpb.SqlWriterRegistry{WriterKey: bytes.Repeat([]byte{2}, 16),
			Generation: "00112233445566778899aabbccddeeff", InstanceId: bytes.Repeat([]byte{3}, 16),
			SqlPrincipal: []byte("normal_test@localhost"), RegistrationDigest: bytes.Repeat([]byte{4}, 32), RegisteredMs: 1, BootSeq: 1},
		ExpectedEpoch: lifecycleEpochForTest, ExpectedCatalogRevision: 11, AuthoritySchema: "authority_test",
		ProtectedSchemas: []string{"authority_test"}, DeploymentDigest: bytes.Repeat([]byte{5}, 32),
		DeploymentId: "fixed-test-boot", LockPrimitive: dbpb.SqlWriterBootGrant_SELECT_ONLY,
		LockQualificationSha256: bytes.Repeat([]byte{6}, 32),
	}
	if kind == dbpb.SqlWriterLifecycleReceipt_REGISTER {
		request.ExpectedEpoch = 0
	}
	return request
}

func lifecycleReceiptForTest(t *testing.T, request *dbpb.SqlWriterLifecycleReceipt_Request) *dbpb.SqlWriterLifecycleReceipt {
	t.Helper()
	raw, err := (proto.MarshalOptions{Deterministic: true}).Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	digest := sum[:]
	if request.Kind == dbpb.SqlWriterLifecycleReceipt_REGISTER {
		digest = request.Writer.RegistrationDigest
	}
	return &dbpb.SqlWriterLifecycleReceipt{RequestId: bytes.Clone(request.RequestId), Kind: request.Kind,
		Generation: request.Writer.Generation, RequestProto: raw, RequestDigest: bytes.Clone(digest),
		PreviousEpoch: lifecycleEpochForTest, ResultingEpoch: lifecycleEpochForTest,
		CatalogRevision: request.ExpectedCatalogRevision + 1}
}

func TestLifecycleCommandIdentityRejectsChangedBootAndPhysicalAuthority(t *testing.T) {
	request := lifecycleRequestForTest(dbpb.SqlWriterLifecycleReceipt_RETIRE)
	physical := bytes.Clone(request.DeploymentDigest)
	if err := validateRequestIdentity(request, "retire", physical); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*dbpb.SqlWriterLifecycleReceipt_Request){
		func(r *dbpb.SqlWriterLifecycleReceipt_Request) { r.RequestId = nil },
		func(r *dbpb.SqlWriterLifecycleReceipt_Request) { r.Writer.InstanceId = nil },
		func(r *dbpb.SqlWriterLifecycleReceipt_Request) { r.Writer.RegistrationDigest = nil },
		func(r *dbpb.SqlWriterLifecycleReceipt_Request) { r.DeploymentDigest[0] ^= 1 },
		func(r *dbpb.SqlWriterLifecycleReceipt_Request) { r.ExpectedEpoch = 0 },
		func(r *dbpb.SqlWriterLifecycleReceipt_Request) { r.ExpectedCatalogRevision = 0 },
		func(r *dbpb.SqlWriterLifecycleReceipt_Request) { r.Kind = dbpb.SqlWriterLifecycleReceipt_REGISTER },
		func(r *dbpb.SqlWriterLifecycleReceipt_Request) { r.ProtectedSchemas = []string{"business_test"} },
		func(r *dbpb.SqlWriterLifecycleReceipt_Request) {
			r.ProtectedSchemas = []string{"authority_test", "business_test"}
		},
	} {
		bad := proto.Clone(request).(*dbpb.SqlWriterLifecycleReceipt_Request)
		change(bad)
		if validateRequestIdentity(bad, "retire", physical) == nil {
			t.Fatal("错误接受改变身份或缺失原件的 RETIRE")
		}
	}
	if validateRequestIdentity(nil, "retire", physical) == nil {
		t.Fatal("错误接受空请求")
	}
	// 换代退休只由下一代的注册事务写,不从命令行发起;没有槽位序号的原件也拒绝。
	for _, change := range []func(*dbpb.SqlWriterLifecycleReceipt_Request){
		func(r *dbpb.SqlWriterLifecycleReceipt_Request) { r.SuccessorRequestId = bytes.Repeat([]byte{9}, 16) },
		func(r *dbpb.SqlWriterLifecycleReceipt_Request) { r.Writer.BootSeq = 0 },
	} {
		bad := proto.Clone(request).(*dbpb.SqlWriterLifecycleReceipt_Request)
		change(bad)
		if validateRequestIdentity(bad, "retire", physical) == nil {
			t.Fatal("错误接受换代退休或缺槽位序号的 RETIRE")
		}
	}
	// 名册是预先渲染的:REGISTER 不带 epoch,带了就是旧渲染器出的。status 可以查 REGISTER 原件。
	register := lifecycleRequestForTest(dbpb.SqlWriterLifecycleReceipt_REGISTER)
	if err := validateRequestIdentity(register, "status", physical); err != nil {
		t.Fatal(err)
	}
	register.ExpectedEpoch = 3
	if validateRequestIdentity(register, "status", physical) == nil {
		t.Fatal("REGISTER 不该带 epoch")
	}
}

func TestConfirmedLifecycleRequiresExactRequestEpochAndCatalogSuccessor(t *testing.T) {
	for _, kind := range []dbpb.SqlWriterLifecycleReceipt_Kind{dbpb.SqlWriterLifecycleReceipt_REGISTER, dbpb.SqlWriterLifecycleReceipt_RETIRE} {
		request := lifecycleRequestForTest(kind)
		receipt := lifecycleReceiptForTest(t, request)
		if err := confirmLifecycle(receipt, request); err != nil {
			t.Fatal(err)
		}
		if confirmLifecycle(nil, request) == nil {
			t.Fatal("查无永久收据不能当作确认")
		}
		for _, change := range []func(*dbpb.SqlWriterLifecycleReceipt){
			func(r *dbpb.SqlWriterLifecycleReceipt) { r.RequestId[0] ^= 1 },
			func(r *dbpb.SqlWriterLifecycleReceipt) { r.RequestProto = append(r.RequestProto, 0) },
			func(r *dbpb.SqlWriterLifecycleReceipt) { r.RequestDigest[0] ^= 1 },
			func(r *dbpb.SqlWriterLifecycleReceipt) { r.Generation = "other" },
			func(r *dbpb.SqlWriterLifecycleReceipt) { r.PreviousEpoch-- },
			func(r *dbpb.SqlWriterLifecycleReceipt) { r.ResultingEpoch++ },
			func(r *dbpb.SqlWriterLifecycleReceipt) { r.CatalogRevision = 0 },
			func(r *dbpb.SqlWriterLifecycleReceipt) { r.CatalogRevision++ },
		} {
			bad := proto.Clone(receipt).(*dbpb.SqlWriterLifecycleReceipt)
			change(bad)
			if confirmLifecycle(bad, request) == nil {
				t.Fatal("永久收据的正文、摘要或版本漂移未拒绝")
			}
		}
		otherBoot := proto.Clone(request).(*dbpb.SqlWriterLifecycleReceipt_Request)
		otherBoot.DeploymentId = "other-boot"
		if confirmLifecycle(receipt, otherBoot) == nil {
			t.Fatal("旧收据不得授权另一个 boot")
		}
		request.ExpectedCatalogRevision = ^uint64(0)
		if confirmLifecycle(lifecycleReceiptForTest(t, request), request) == nil {
			t.Fatal("目录版本溢出不得确认")
		}
	}
}

func TestRuntimeEnvironmentRejectsControlNamesAndOversizedRosterBeforeReading(t *testing.T) {
	entry := &dbpb.SqlWriterBootRoster_Entry{Requests: []*dbpb.SqlWriterLifecycleReceipt_Request{lifecycleRequestForTest(dbpb.SqlWriterLifecycleReceipt_REGISTER)}}
	root := t.TempDir()
	for _, name := range []string{"MMORPG_SQL_REGISTRAR_DSN", "MMORPG_SQL_OPERATOR_PASSWORD", "PATH", "CHAT_AUTHORITY_DSN;echo unsafe"} {
		entry.RuntimeEnvironment = []*dbpb.SqlWriterBootRoster_RuntimeEnvironment{{Name: name,
			ValuePath: "missing-private-file", ValueSha256: bytes.Repeat([]byte{7}, 32)}}
		if _, err := readRuntimeEnvironment(context.Background(), entry, root); err == nil {
			t.Fatal("控制凭据或任意环境名不得进入 normal")
		}
	}
	entry.RuntimeEnvironment = make([]*dbpb.SqlWriterBootRoster_RuntimeEnvironment, 6)
	if _, err := readRuntimeEnvironment(context.Background(), entry, root); err == nil {
		t.Fatal("超过五项实际 SQL 白名单未拒绝")
	}
	entry.RuntimeEnvironment = nil
	values, err := readRuntimeEnvironment(context.Background(), entry, root)
	if err != nil || len(values) != 0 {
		t.Fatalf("无 SQL 环境的正式 entry 应返回空集: %v", err)
	}
}

// 一条 entry 是一个 boot:每个逻辑库各一份 Request,身份相同,库不重复;名册里的候选写同一组库。
func TestRosterEntryBindsOneBootAcrossLogicalDBs(t *testing.T) {
	requestFor := func(schema string) *dbpb.SqlWriterLifecycleReceipt_Request {
		request := lifecycleRequestForTest(dbpb.SqlWriterLifecycleReceipt_REGISTER)
		request.AuthoritySchema = schema
		request.ProtectedSchemas = []string{schema}
		return request
	}
	entry := &dbpb.SqlWriterBootRoster_Entry{Requests: []*dbpb.SqlWriterLifecycleReceipt_Request{requestFor("zone_1_db"), requestFor("zone_2_db")}}
	if err := validateRosterEntry(entry); err != nil {
		t.Fatal(err)
	}
	duplicate := &dbpb.SqlWriterBootRoster_Entry{Requests: []*dbpb.SqlWriterLifecycleReceipt_Request{requestFor("zone_1_db"), requestFor("zone_1_db")}}
	if err := validateRosterEntry(duplicate); err == nil {
		t.Fatal("同一boot在一个逻辑库注册了两份")
	}
	other := requestFor("zone_2_db")
	other.Writer.SqlPrincipal = []byte("other@%")
	mixed := &dbpb.SqlWriterBootRoster_Entry{Requests: []*dbpb.SqlWriterLifecycleReceipt_Request{requestFor("zone_1_db"), other}}
	if err := validateRosterEntry(mixed); err == nil {
		t.Fatal("同一boot在两个逻辑库用了不同账号")
	}
	reordered := &dbpb.SqlWriterBootRoster_Entry{Requests: []*dbpb.SqlWriterLifecycleReceipt_Request{requestFor("zone_2_db"), requestFor("zone_1_db")}}
	if rosterEntryShape(entry) != rosterEntryShape(reordered) {
		t.Fatal("库集合相同的候选被判成不同形状")
	}
	single := &dbpb.SqlWriterBootRoster_Entry{Requests: []*dbpb.SqlWriterLifecycleReceipt_Request{requestFor("zone_1_db")}}
	if rosterEntryShape(entry) == rosterEntryShape(single) {
		t.Fatal("库集合不同的候选被判成相同形状")
	}
}

// generationForTest 是夹具里各逻辑库 route_meta 的世代。
const generationForTest = "00112233445566778899aabbccddeeff"

var physicalForTest = bytes.Repeat([]byte{5}, 32)

// slotTemplateForTest 是名册里一个槽位在一个逻辑库上的 REGISTER 模板:账号与固定字段齐全,身份字段留空。
func slotTemplateForTest(schema string) *dbpb.SqlWriterLifecycleReceipt_Request {
	return &dbpb.SqlWriterLifecycleReceipt_Request{Kind: dbpb.SqlWriterLifecycleReceipt_REGISTER,
		Writer:          &dbpb.SqlWriterRegistry{Generation: generationForTest, SqlPrincipal: []byte("slot_0@%"), State: dbpb.SqlWriterRegistry_ACTIVE},
		AuthoritySchema: schema, ProtectedSchemas: []string{schema}, DeploymentDigest: bytes.Clone(physicalForTest),
		LockPrimitive: dbpb.SqlWriterBootGrant_SELECT_ONLY, LockQualificationSha256: bytes.Repeat([]byte{6}, 32)}
}

func slotEntryForTest(schemas ...string) *dbpb.SqlWriterBootRoster_Entry {
	entry := &dbpb.SqlWriterBootRoster_Entry{RuntimeConfigPath: "runtime.yaml", RuntimeConfigSha256: bytes.Repeat([]byte{8}, 32)}
	for _, schema := range schemas {
		entry.Requests = append(entry.Requests, slotTemplateForTest(schema))
	}
	return entry
}

// fakeSlotCatalog 是一个逻辑库上的目录:槽位的最新一代与收据都在内存里,boot_seq 做 CAS,REGISTER 按当前 epoch 落地。
type fakeSlotCatalog struct {
	t                  *testing.T
	epoch              uint64
	frozen             bool
	frozenAtWrite      bool // 预检之后、提交之前闸门被冻结:只作用一次
	frozenAfterCommit  bool // 注册提交之后闸门被冻结
	unknownAfterCommit bool // 提交落地了,回包却是结局未知:只作用一次
	readyErr           error
	latest             *dbpb.SqlWriterRegistry
	receipts           map[string]*dbpb.SqlWriterLifecycleReceipt
	registers          int
	retired            [][]byte // 被换代退休的 writer_key
}

func (c *fakeSlotCatalog) LatestBoot(context.Context, []byte) (*dbpb.SqlWriterRegistry, error) {
	if c.latest == nil {
		return nil, nil
	}
	return proto.Clone(c.latest).(*dbpb.SqlWriterRegistry), nil
}

func (c *fakeSlotCatalog) RegistrationReady(_ context.Context, request *dbpb.SqlWriterLifecycleReceipt_Request) error {
	if _, landed := c.receipts[string(request.RequestId)]; landed {
		return nil
	}
	if c.frozen {
		return authoritysql.ErrWriterFrozen
	}
	if c.readyErr != nil {
		return c.readyErr
	}
	if c.latest != nil && request.Writer.BootSeq <= c.latest.BootSeq {
		return authoritysql.ErrWriterDenied
	}
	return nil
}

func (c *fakeSlotCatalog) Register(_ context.Context, request *dbpb.SqlWriterLifecycleReceipt_Request) (*dbpb.SqlWriterLifecycleReceipt, error) {
	c.registers++
	if receipt, landed := c.receipts[string(request.RequestId)]; landed {
		return receipt, nil
	}
	if c.frozen || c.frozenAtWrite {
		c.frozenAtWrite = false
		return nil, authoritysql.ErrWriterFrozen
	}
	if c.latest != nil && request.Writer.BootSeq <= c.latest.BootSeq {
		return nil, authoritysql.ErrWriterDenied
	}
	if c.latest != nil && c.latest.State == dbpb.SqlWriterRegistry_ACTIVE {
		c.retired = append(c.retired, bytes.Clone(c.latest.WriterKey))
	}
	c.latest = proto.Clone(request.Writer).(*dbpb.SqlWriterRegistry)
	receipt := lifecycleReceiptForTest(c.t, request)
	receipt.PreviousEpoch, receipt.ResultingEpoch = c.epoch, c.epoch
	if c.receipts == nil {
		c.receipts = map[string]*dbpb.SqlWriterLifecycleReceipt{}
	}
	c.receipts[string(request.RequestId)] = receipt
	if c.unknownAfterCommit {
		c.unknownAfterCommit = false
		return nil, errors.Join(authoritysql.ErrOutcomeUnknown, errors.New("commit"))
	}
	return receipt, nil
}

func (c *fakeSlotCatalog) CurrentMeta(context.Context) (*dbpb.RouteMeta, error) {
	writable := uint32(1)
	if c.frozen || c.frozenAfterCommit {
		writable = 0
	}
	return &dbpb.RouteMeta{Generation: generationForTest, Writable: writable, WriterEpoch: c.epoch}, nil
}

// slotHarness 把一个槽位模板接到内存里的目录上。每个 Pod 有自己的私有 emptyDir,启动记录按 Pod 分开放。
type slotHarness struct {
	catalogs map[string]*fakeSlotCatalog
	records  map[string]*dbpb.SqlWriterBootRoster_Entry
	template *dbpb.SqlWriterBootRoster_Entry
}

func newSlotHarness(t *testing.T, schemas ...string) *slotHarness {
	t.Helper()
	h := &slotHarness{catalogs: map[string]*fakeSlotCatalog{}, records: map[string]*dbpb.SqlWriterBootRoster_Entry{}, template: slotEntryForTest(schemas...)}
	for _, schema := range schemas {
		h.catalogs[schema] = &fakeSlotCatalog{t: t, epoch: 9}
	}
	return h
}

// pod 是一个 Pod 上的注册器:pod 是它的 UID,也是它私有目录的名字。
func (h *slotHarness) pod(pod string) slotRegistration {
	return slotRegistration{
		bootOutput:   "/run/sql-writer/grant/private/boot-grant.json",
		deploymentID: pod,
		physical:     physicalForTest,
		open: func(request *dbpb.SqlWriterLifecycleReceipt_Request) (registrationCatalog, error) {
			return h.catalogs[request.AuthoritySchema], nil
		},
		loadRecord: func(path string) (*dbpb.SqlWriterBootRoster_Entry, error) {
			record, ok := h.records[pod+path]
			if !ok {
				return nil, os.ErrNotExist
			}
			return proto.Clone(record).(*dbpb.SqlWriterBootRoster_Entry), nil
		},
		saveRecord: func(path string, entry *dbpb.SqlWriterBootRoster_Entry) error {
			if _, ok := h.records[pod+path]; ok {
				return errors.New("the boot record already exists")
			}
			h.records[pod+path] = proto.Clone(entry).(*dbpb.SqlWriterBootRoster_Entry)
			return nil
		},
		now:    func() time.Time { return time.UnixMilli(1_700_000_000_000) },
		random: rand.Reader,
	}
}

func (h *slotHarness) record(pod string) *dbpb.SqlWriterBootRoster_Entry {
	return h.records[pod+bootRecordPath("/run/sql-writer/grant/private/boot-grant.json")]
}

func TestSlotOrdinalParsesStatefulSetPodNames(t *testing.T) {
	for name, want := range map[string]int{"db-0": 0, "data-service-12": 12, "chat-canary-3": 3, "x-1023": 1023} {
		if got, err := slotOrdinal(name); err != nil || got != want {
			t.Fatalf("%s: got %d err %v, want %d", name, got, err, want)
		}
	}
	for _, name := range []string{"", "db", "db-", "-1", "db-01", "db-+1", "db-1a", "db-12345", "db- 1"} {
		if _, err := slotOrdinal(name); err == nil {
			t.Fatalf("%q 不是 StatefulSet Pod 名,不能当槽位", name)
		}
	}
}

// 名册里只放模板:身份由注册器每代现场生成,模板里预先填了就是旧格式或手写错误。
func TestSlotTemplateMustLeaveIdentityEmpty(t *testing.T) {
	if err := validateSlotTemplate(slotTemplateForTest("zone_1_db"), physicalForTest); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*dbpb.SqlWriterLifecycleReceipt_Request){
		"request_id":    func(r *dbpb.SqlWriterLifecycleReceipt_Request) { r.RequestId = bytes.Repeat([]byte{1}, 16) },
		"deployment_id": func(r *dbpb.SqlWriterLifecycleReceipt_Request) { r.DeploymentId = "PLACEHOLDER_POD_UID" },
		"writer_key":    func(r *dbpb.SqlWriterLifecycleReceipt_Request) { r.Writer.WriterKey = bytes.Repeat([]byte{2}, 16) },
		"instance_id":   func(r *dbpb.SqlWriterLifecycleReceipt_Request) { r.Writer.InstanceId = bytes.Repeat([]byte{3}, 16) },
		"registration": func(r *dbpb.SqlWriterLifecycleReceipt_Request) {
			r.Writer.RegistrationDigest = bytes.Repeat([]byte{4}, 32)
		},
		"registered_ms":    func(r *dbpb.SqlWriterLifecycleReceipt_Request) { r.Writer.RegisteredMs = 1 },
		"boot_seq":         func(r *dbpb.SqlWriterLifecycleReceipt_Request) { r.Writer.BootSeq = 1 },
		"epoch":            func(r *dbpb.SqlWriterLifecycleReceipt_Request) { r.ExpectedEpoch = 3 },
		"catalog revision": func(r *dbpb.SqlWriterLifecycleReceipt_Request) { r.ExpectedCatalogRevision = 3 },
		"successor":        func(r *dbpb.SqlWriterLifecycleReceipt_Request) { r.SuccessorRequestId = bytes.Repeat([]byte{1}, 16) },
		"retire":           func(r *dbpb.SqlWriterLifecycleReceipt_Request) { r.Kind = dbpb.SqlWriterLifecycleReceipt_RETIRE },
		"physical":         func(r *dbpb.SqlWriterLifecycleReceipt_Request) { r.DeploymentDigest[0] ^= 1 },
		"other schema":     func(r *dbpb.SqlWriterLifecycleReceipt_Request) { r.ProtectedSchemas = []string{"other_db"} },
		"no account":       func(r *dbpb.SqlWriterLifecycleReceipt_Request) { r.Writer.SqlPrincipal = nil },
		"no lock primitive": func(r *dbpb.SqlWriterLifecycleReceipt_Request) {
			r.LockPrimitive = dbpb.SqlWriterBootGrant_LOCK_PRIMITIVE_UNKNOWN
		},
	} {
		request := slotTemplateForTest("zone_1_db")
		change(request)
		if validateSlotTemplate(request, physicalForTest) == nil {
			t.Fatalf("%s 填了的模板被接受了", name)
		}
	}
}

// 两个槽位共用一个账号会互相换代;各槽位要写同一组库。
func TestSlotRosterRejectsSharedAccountsAndMixedShapes(t *testing.T) {
	second := slotEntryForTest("zone_1_db", "zone_2_db")
	for _, request := range second.Requests {
		request.Writer.SqlPrincipal = []byte("slot_1@%")
	}
	good := &dbpb.SqlWriterBootRoster{Format: slotRosterFormat, Entries: []*dbpb.SqlWriterBootRoster_Entry{slotEntryForTest("zone_1_db", "zone_2_db"), second}}
	if err := validateSlotRoster(good, physicalForTest); err != nil {
		t.Fatal(err)
	}
	shared := &dbpb.SqlWriterBootRoster{Format: slotRosterFormat, Entries: []*dbpb.SqlWriterBootRoster_Entry{slotEntryForTest("zone_1_db", "zone_2_db"), slotEntryForTest("zone_1_db", "zone_2_db")}}
	if validateSlotRoster(shared, physicalForTest) == nil {
		t.Fatal("两个槽位共用一个账号")
	}
	other := slotEntryForTest("zone_1_db")
	for _, request := range other.Requests {
		request.Writer.SqlPrincipal = []byte("slot_1@%")
	}
	mixed := &dbpb.SqlWriterBootRoster{Format: slotRosterFormat, Entries: []*dbpb.SqlWriterBootRoster_Entry{slotEntryForTest("zone_1_db", "zone_2_db"), other}}
	if validateSlotRoster(mixed, physicalForTest) == nil {
		t.Fatal("槽位写的库集合不同")
	}
	accounts := slotEntryForTest("zone_1_db", "zone_2_db")
	accounts.Requests[1].Writer.SqlPrincipal = []byte("slot_9@%")
	if validateSlotRoster(&dbpb.SqlWriterBootRoster{Format: slotRosterFormat, Entries: []*dbpb.SqlWriterBootRoster_Entry{accounts}}, physicalForTest) == nil {
		t.Fatal("一个槽位在两个库用了不同账号")
	}
}

// 槽位第一次启动:现场生成一代,各库同一个身份、同一个 boot_seq=1;第一份写下去之前先记下启动记录。
func TestFirstBootOfSlotMintsIdentityAndRecordsIt(t *testing.T) {
	h := newSlotHarness(t, "zone_1_db", "zone_2_db")
	set, boot, err := h.pod("pod-a").register(context.Background(), h.template)
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Grants) != 2 || !bytes.Equal(set.Grants[0].WriterKey, set.Grants[1].WriterKey) || !bytes.Equal(set.Grants[0].InstanceId, set.Grants[1].InstanceId) {
		t.Fatalf("一代在两个库是同一个身份: %v", set.Grants)
	}
	for _, request := range boot.Requests {
		if request.Writer.BootSeq != 1 || request.DeploymentId != "pod-a" || len(request.RequestId) != 16 || len(request.Writer.RegistrationDigest) != 32 {
			t.Fatalf("第一代: %v", request)
		}
	}
	if !bytes.Equal(set.Grants[0].GrantId, boot.Requests[0].RequestId) || set.Grants[0].DeploymentId != "pod-a" {
		t.Fatalf("写资格绑定本 Pod 和原请求: %v", set.Grants[0])
	}
	record := h.record("pod-a")
	if record == nil || !proto.Equal(record, boot) {
		t.Fatal("启动记录就是这一代的完整请求")
	}
	if err = recordMatchesTemplate(record, h.template, "pod-a"); err != nil {
		t.Fatal(err)
	}
}

// 新一代的号取各库已有最大号 + 1 里最大的那个,各库同一个号;还 ACTIVE 的上一代在注册时被换代。
func TestNextBootTakesHighestSeqAcrossDatabases(t *testing.T) {
	h := newSlotHarness(t, "zone_1_db", "zone_2_db")
	h.catalogs["zone_1_db"].latest = &dbpb.SqlWriterRegistry{WriterKey: bytes.Repeat([]byte{1}, 16), BootSeq: 3, State: dbpb.SqlWriterRegistry_REVOKED}
	h.catalogs["zone_2_db"].latest = &dbpb.SqlWriterRegistry{WriterKey: bytes.Repeat([]byte{2}, 16), BootSeq: 5, State: dbpb.SqlWriterRegistry_ACTIVE}
	_, boot, err := h.pod("pod-b").register(context.Background(), h.template)
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range boot.Requests {
		if request.Writer.BootSeq != 6 {
			t.Fatalf("各库同一个号,取最大: %d", request.Writer.BootSeq)
		}
	}
	if len(h.catalogs["zone_2_db"].retired) != 1 || len(h.catalogs["zone_1_db"].retired) != 0 {
		t.Fatal("只有还 ACTIVE 的上一代被换代退休")
	}
}

// init 重跑(同一个 Pod):读启动记录,还是同一代。已注册的库收据在,原样返回;没注册的接着注册。
func TestInitRestartReplaysRecordedBoot(t *testing.T) {
	h := newSlotHarness(t, "zone_1_db", "zone_2_db")
	first, second := h.catalogs["zone_1_db"], h.catalogs["zone_2_db"]
	second.frozenAtWrite = true
	if _, _, err := h.pod("pod-a").register(context.Background(), h.template); !authoritysql.IsWriterFrozen(err) {
		t.Fatalf("提交时冻结要回可重试: %v", err)
	}
	recorded := h.record("pod-a")
	if recorded == nil || first.latest == nil || second.latest != nil {
		t.Fatal("第一份已注册,第二份没落地,启动记录留着")
	}
	set, boot, err := h.pod("pod-a").register(context.Background(), h.template)
	if err != nil {
		t.Fatalf("重跑: %v", err)
	}
	if !proto.Equal(boot, recorded) || !bytes.Equal(set.Grants[0].WriterKey, recorded.Requests[0].Writer.WriterKey) {
		t.Fatal("重跑不生成新身份")
	}
	if first.registers != 2 || len(first.receipts) != 1 || second.latest == nil || second.latest.BootSeq != 1 {
		t.Fatalf("第一份重发同一请求、只落地一次,第二份接着注册: %d %d", first.registers, len(first.receipts))
	}
}

// 预检就被挡住:什么都没写,也不写启动记录;下次重新生成这一代。
func TestFrozenPrecheckWritesNothing(t *testing.T) {
	for name, block := range map[string]func(*fakeSlotCatalog){
		"资格不符": func(c *fakeSlotCatalog) { c.readyErr = authoritysql.ErrWriterDenied },
		"闸门冻结": func(c *fakeSlotCatalog) { c.frozen = true },
	} {
		t.Run(name, func(t *testing.T) {
			h := newSlotHarness(t, "zone_1_db", "zone_2_db")
			block(h.catalogs["zone_2_db"])
			if _, _, err := h.pod("pod-a").register(context.Background(), h.template); err == nil {
				t.Fatal("要报错")
			}
			if h.catalogs["zone_1_db"].registers != 0 || h.catalogs["zone_2_db"].registers != 0 || h.record("pod-a") != nil {
				t.Fatal("预检挡住时一份都不写,不留启动记录")
			}
		})
	}
}

// 提交结局未知:重跑原样重发同一请求,注册幂等,只落地一次,不卡死这个 Pod。
func TestUnknownOutcomeReplaysSameRequest(t *testing.T) {
	h := newSlotHarness(t, "zone_1_db")
	catalog := h.catalogs["zone_1_db"]
	catalog.unknownAfterCommit = true
	if _, _, err := h.pod("pod-a").register(context.Background(), h.template); !errors.Is(err, authoritysql.ErrOutcomeUnknown) {
		t.Fatalf("结局未知: %v", err)
	}
	if _, _, err := h.pod("pod-a").register(context.Background(), h.template); err != nil {
		t.Fatalf("重发同一请求: %v", err)
	}
	if catalog.registers != 2 || len(catalog.receipts) != 1 || catalog.latest.BootSeq != 1 {
		t.Fatalf("同一请求只落地一次: %d %d", catalog.registers, len(catalog.receipts))
	}
}

// 注册提交之后闸门才冻结:收据已确认但资格不发,回可重试;解冻之后重发同一请求,写资格照发。
func TestGateFrozenAfterCommitIsRetryable(t *testing.T) {
	h := newSlotHarness(t, "zone_1_db")
	catalog := h.catalogs["zone_1_db"]
	catalog.frozenAfterCommit = true
	if _, _, err := h.pod("pod-a").register(context.Background(), h.template); !authoritysql.IsWriterFrozen(err) {
		t.Fatalf("要回可重试的冻结: %v", err)
	}
	catalog.frozenAfterCommit = false
	set, _, err := h.pod("pod-a").register(context.Background(), h.template)
	if err != nil {
		t.Fatalf("解冻之后: %v", err)
	}
	if len(catalog.receipts) != 1 || len(set.Grants) != 1 {
		t.Fatalf("不再落地第二次,写资格照发: %d %d", len(catalog.receipts), len(set.Grants))
	}
}

// 名册是很久以前渲染的,这期间 cutover、回滚把 epoch 推进了好几轮:REGISTER 照样成功,收据记实际 epoch。
func TestRegisterSucceedsAfterEpochChanged(t *testing.T) {
	h := newSlotHarness(t, "zone_1_db")
	h.catalogs["zone_1_db"].epoch = 41
	_, boot, err := h.pod("pod-a").register(context.Background(), h.template)
	if err != nil {
		t.Fatal(err)
	}
	if got := h.catalogs["zone_1_db"].receipts[string(boot.Requests[0].RequestId)].PreviousEpoch; got != 41 {
		t.Fatalf("收据记实际 epoch: %d", got)
	}
}

// 启动记录必须是本 Pod、本槽位模板生成的:换了 Pod 或换了模板都拒绝,不拿别人的身份写。
func TestBootRecordOfOtherPodOrSlotRefused(t *testing.T) {
	h := newSlotHarness(t, "zone_1_db")
	if _, _, err := h.pod("pod-a").register(context.Background(), h.template); err != nil {
		t.Fatal(err)
	}
	record := h.record("pod-a")
	if recordMatchesTemplate(record, h.template, "pod-b") == nil {
		t.Fatal("别的 Pod 的启动记录")
	}
	otherSlot := slotEntryForTest("zone_1_db")
	otherSlot.Requests[0].Writer.SqlPrincipal = []byte("slot_1@%")
	if recordMatchesTemplate(record, otherSlot, "pod-a") == nil {
		t.Fatal("别的槽位的启动记录")
	}
	tampered := proto.Clone(record).(*dbpb.SqlWriterBootRoster_Entry)
	tampered.Requests[0].Writer.BootSeq = 9
	if recordMatchesTemplate(tampered, h.template, "pod-a") == nil {
		t.Fatal("改过的启动记录(注册摘要对不上)")
	}
}

// 同序号出现两个 Pod(强删之后旧节点还活着):后来的那个先注册成功,旧 Pod 拿着过期的启动记录重跑时被 CAS 挡住,不会把新一代顶掉。
func TestStaleBootRecordCannotSupersedeNewerBoot(t *testing.T) {
	h := newSlotHarness(t, "zone_1_db")
	catalog := h.catalogs["zone_1_db"]
	catalog.frozenAtWrite = true
	if _, _, err := h.pod("pod-old").register(context.Background(), h.template); !authoritysql.IsWriterFrozen(err) {
		t.Fatalf("旧 Pod 第一次没注册成: %v", err)
	}
	if _, _, err := h.pod("pod-new").register(context.Background(), h.template); err != nil {
		t.Fatalf("新 Pod 注册: %v", err)
	}
	newest := proto.Clone(catalog.latest).(*dbpb.SqlWriterRegistry)
	if _, _, err := h.pod("pod-old").register(context.Background(), h.template); !errors.Is(err, authoritysql.ErrWriterDenied) {
		t.Fatalf("过期的启动记录要被拒绝: %v", err)
	}
	if !proto.Equal(catalog.latest, newest) {
		t.Fatal("新一代不能被顶掉")
	}
}

// 缺口清单第 22 条的回归:一个槽位反复启动(写过之后崩溃、被回收、驱逐、滚动发布),每次是这个槽位的下一代,
// 名册不消耗。超过原来 1024 条的上限也照常注册,ACTIVE 始终只有当前这一代。
func TestFrequentRestartsNeverExhaustRoster(t *testing.T) {
	h := newSlotHarness(t, "zone_1_db")
	catalog := h.catalogs["zone_1_db"]
	const boots = 1100
	for boot := 1; boot <= boots; boot++ {
		if _, _, err := h.pod(fmt.Sprintf("pod-uid-%d", boot)).register(context.Background(), h.template); err != nil {
			t.Fatalf("第 %d 次启动: %v", boot, err)
		}
		if catalog.latest.BootSeq != uint64(boot) {
			t.Fatalf("第 %d 次启动是第 %d 代", boot, catalog.latest.BootSeq)
		}
	}
	if len(catalog.retired) != boots-1 {
		t.Fatalf("每次启动换代退休上一代: %d", len(catalog.retired))
	}
	if len(h.template.Requests) != 1 {
		t.Fatal("名册还是一个槽位")
	}
}

func TestBoundedInputRejectsCancellationBeforeAnyFileAccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	root := t.TempDir()
	if _, err := readBoundedInput(ctx, filepath.Join(root, "missing"), root, requestLimit, true); err != context.Canceled {
		t.Fatalf("取消应先于文件访问返回: %v", err)
	}
	if _, err := resolveInputRoot("relative-input"); err == nil {
		t.Fatal("相对输入根未拒绝")
	}
	if checkOutputPaths("relative-output", "") == nil {
		t.Fatal("相对 grant 输出未拒绝")
	}
	same := filepath.Join(root, "same")
	if checkOutputPaths(same, same) == nil || checkOutputPaths(same, same+".sha256") == nil {
		t.Fatal("配置不得覆盖 grant 或摘要")
	}
}

func TestInvalidLifecycleCLIStopsBeforeLookingUpCredentials(t *testing.T) {
	for _, args := range [][]string{
		{"--command", "register"},
		{"--command", "unknown", "--request", "relative"},
		{"--command", "retire", "--request", "relative"},
		{"--command", "status", "--request", "relative", "--timeout", "61s"},
		{"--command", "register", "--request", "a", "--roster", "b"},
		{"--command", "retire", "--request", filepath.Join(t.TempDir(), "r.json"), "--receipt-output", filepath.Join(t.TempDir(), "o.json"), "--pod-name", "db-0"},
	} {
		lookedUp := false
		var out bytes.Buffer
		err := run(context.Background(), args, func(string) string { lookedUp = true; return "must-not-read" }, &out)
		if err == nil || lookedUp || out.Len() != 0 {
			t.Fatal("非法 CLI 在纯校验前读取凭据或输出确认")
		}
	}
}

func TestPublicationControlPinsExactFormalBytesBeforeAnySQL(t *testing.T) {
	raw := []byte("{\"runId\":\"AQID\",\"generation\":\"fixture\"}")
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	parsed := &dbpb.SqlWriterMaintenanceReceipt{}
	if err := decodePinnedPublication(raw, digest, parsed); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(parsed.RunId, []byte{1, 2, 3}) || parsed.Generation != "fixture" {
		t.Fatal("正式原件未按原bytes读取")
	}
	changed := append(bytes.Clone(raw), ' ')
	if decodePinnedPublication(changed, digest, &dbpb.SqlWriterMaintenanceReceipt{}) == nil {
		t.Fatal("同语义但不同原bytes不得借旧SHA")
	}
	unknown := []byte("{\"passed\":true}")
	unknownSum := sha256.Sum256(unknown)
	if decodePinnedPublication(unknown, hex.EncodeToString(unknownSum[:]), &dbpb.SqlWriterMaintenanceReceipt{}) == nil {
		t.Fatal("passed布尔不能替代正式Receipt")
	}
	if decodePinnedPublication(raw, "", &dbpb.SqlWriterMaintenanceReceipt{}) == nil {
		t.Fatal("publication不可省原件SHA")
	}
}

func TestPublicationControlRejectsMixedMutationFlagsBeforeEnvironmentOrSQL(t *testing.T) {
	getenv := func(string) string { t.Fatal("参数拒绝必须发生在读取控制凭据前"); return "" }
	for _, args := range [][]string{
		{"-command", "verify-publication-prestart", "-request", "x", "-request-sha256", "00", "-boot-output", "x"},
		{"-command", "verify-publication-runtime", "-request", "x", "-prepared", "y", "-receipt-output", "z"},
		{"-command", "retire", "-request", "x", "-prepared", "y"},
	} {
		if err := run(context.Background(), args, getenv, &bytes.Buffer{}); err == nil {
			t.Fatal("拒绝publication与生命周期写入混参")
		}
	}
}
