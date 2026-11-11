package authoritysql

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"
	databasepb "proto/common/database"
)

// 槽位换代的纯规则(writer-roster-exhaustion-20261106.md §5.2):不连库。真库上的事务见 writer_succession_tidb_test.go。

func successionCatalogForTest() *Catalog {
	return &Catalog{config: CatalogConfig{AuthoritySchema: "zone_1_db", Generation: "ab", ProtectedSchemas: []string{"zone_1_db"},
		PhysicalAuthoritySHA256: bytes.Repeat([]byte{5}, 32), Principal: "registrar@%"}}
}

func successionRegisterForTest(t *testing.T, bootSeq uint64) *databasepb.SqlWriterLifecycleReceipt_Request {
	t.Helper()
	request := &databasepb.SqlWriterLifecycleReceipt_Request{RequestId: bytes.Repeat([]byte{1}, 16), Kind: databasepb.SqlWriterLifecycleReceipt_REGISTER,
		Writer: &databasepb.SqlWriterRegistry{WriterKey: bytes.Repeat([]byte{2}, 16), Generation: "ab", InstanceId: bytes.Repeat([]byte{3}, 16),
			SqlPrincipal: []byte("slot_0@%"), State: databasepb.SqlWriterRegistry_ACTIVE, RegisteredMs: 1, BootSeq: bootSeq},
		AuthoritySchema: "zone_1_db", ProtectedSchemas: []string{"zone_1_db"}, DeploymentDigest: bytes.Repeat([]byte{5}, 32), DeploymentId: "pod-uid",
		LockPrimitive: databasepb.SqlWriterBootGrant_SELECT_ONLY, LockQualificationSha256: bytes.Repeat([]byte{6}, 32)}
	canonical, err := CanonicalRegistrationRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

func successionPredecessorForTest(bootSeq uint64) *databasepb.SqlWriterRegistry {
	return &databasepb.SqlWriterRegistry{WriterKey: bytes.Repeat([]byte{7}, 16), Generation: "ab", InstanceId: bytes.Repeat([]byte{8}, 16),
		SqlPrincipal: []byte("slot_0@%"), RegistrationDigest: bytes.Repeat([]byte{4}, 32), State: databasepb.SqlWriterRegistry_ACTIVE, RegisteredMs: 1, BootSeq: bootSeq}
}

func TestSuccessionRules(t *testing.T) {
	active := successionPredecessorForTest(4)
	open := &databasepb.RouteMeta{Writable: 1}
	running := &databasepb.RouteMeta{Writable: 1, FreezeRunId: bytes.Repeat([]byte{9}, 16)}
	for _, c := range []struct {
		name   string
		maxSeq uint64
		active *databasepb.SqlWriterRegistry
		locked []byte
		next   uint64
		meta   *databasepb.RouteMeta
		ok     bool
	}{
		{"槽位第一次启动", 0, nil, nil, 1, open, true},
		{"上一代已被人工或排空退休", 4, nil, nil, 5, open, true},
		{"run 开着而上一代已退休:同 run 释放之后只注册", 4, nil, nil, 5, running, true},
		{"换代", 4, active, active.WriterKey, 5, open, true},
		{"号可以有空洞", 4, active, active.WriterKey, 9, open, true},
		{"同号:另一个同序号 Pod 先注册了", 4, active, active.WriterKey, 4, open, false},
		{"更小的号:陈旧请求", 4, nil, nil, 3, open, false},
		{"ACTIVE 的不是锁住的那一代", 4, active, bytes.Repeat([]byte{6}, 16), 5, open, false},
		{"没锁上一代却有 ACTIVE", 4, active, nil, 5, open, false},
		{"排空期间不换代", 4, active, active.WriterKey, 5, running, false},
	} {
		err := succession(c.maxSeq, c.active, c.locked, &databasepb.SqlWriterRegistry{BootSeq: c.next}, c.meta)
		if (err == nil) != c.ok || err != nil && (!errors.Is(err, ErrWriterDenied) || IsWriterFrozen(err)) {
			t.Fatalf("%s: %v", c.name, err)
		}
	}
	if succession(0, nil, nil, nil, open) == nil || succession(0, nil, nil, &databasepb.SqlWriterRegistry{BootSeq: 1}, nil) == nil {
		t.Fatal("缺请求或 meta 要拒绝")
	}
}

// 每一代都带槽位序号;successor_request_id 只出现在换代退休里,不和维护 run 同时出现。
func TestLifecycleRequiresBootSeqAndConfinesSuccessor(t *testing.T) {
	c := successionCatalogForTest()
	register := successionRegisterForTest(t, 1)
	if _, _, err := c.validateLifecycle(register, databasepb.SqlWriterLifecycleReceipt_REGISTER); err != nil {
		t.Fatal(err)
	}
	noSeq := proto.Clone(register).(*databasepb.SqlWriterLifecycleReceipt_Request)
	noSeq.Writer.BootSeq = 0
	if _, err := CanonicalRegistrationRequest(noSeq); err == nil {
		t.Fatal("REGISTER 缺槽位序号")
	}
	if _, _, err := c.validateLifecycle(noSeq, databasepb.SqlWriterLifecycleReceipt_REGISTER); err == nil {
		t.Fatal("REGISTER 缺槽位序号")
	}
	withSuccessor := proto.Clone(register).(*databasepb.SqlWriterLifecycleReceipt_Request)
	withSuccessor.SuccessorRequestId = bytes.Repeat([]byte{9}, 16)
	if _, err := CanonicalRegistrationRequest(withSuccessor); err == nil {
		t.Fatal("REGISTER 不带 successor")
	}
	retire := successionRetirement(successionRegisterForTest(t, 5), successionPredecessorForTest(4), 7, 11)
	if _, _, err := c.validateLifecycle(retire, databasepb.SqlWriterLifecycleReceipt_RETIRE); err != nil {
		t.Fatalf("换代退休是合法的 RETIRE 全文: %v", err)
	}
	for name, change := range map[string]func(*databasepb.SqlWriterLifecycleReceipt_Request){
		"带维护 run": func(r *databasepb.SqlWriterLifecycleReceipt_Request) {
			r.MaintenanceRunId = bytes.Repeat([]byte{8}, 16)
		},
		"successor 太短": func(r *databasepb.SqlWriterLifecycleReceipt_Request) {
			r.SuccessorRequestId = r.SuccessorRequestId[:15]
		},
		"退休的一代缺序号": func(r *databasepb.SqlWriterLifecycleReceipt_Request) { r.Writer.BootSeq = 0 },
	} {
		bad := proto.Clone(retire).(*databasepb.SqlWriterLifecycleReceipt_Request)
		change(bad)
		if _, _, err := c.validateLifecycle(bad, databasepb.SqlWriterLifecycleReceipt_RETIRE); err == nil {
			t.Fatalf("%s 的换代退休被接受了", name)
		}
	}
}

// 换代退休的全文:退休的是上一代原样的注册行,由这次 REGISTER 发起,request_id 由它派生。
func TestSuccessionRetirementLinksTheRegistration(t *testing.T) {
	register := successionRegisterForTest(t, 5)
	predecessor := successionPredecessorForTest(4)
	retire := successionRetirement(register, predecessor, 7, 11)
	if retire.Kind != databasepb.SqlWriterLifecycleReceipt_RETIRE || len(retire.RequestId) != 16 || bytes.Equal(retire.RequestId, register.RequestId) {
		t.Fatalf("RETIRE 有自己的 request_id: %v", retire)
	}
	if again := successionRetirement(register, predecessor, 7, 11); !bytes.Equal(again.RequestId, retire.RequestId) {
		t.Fatal("同一次换代只对应一份退休")
	}
	if !bytes.Equal(retire.SuccessorRequestId, register.RequestId) || retire.DeploymentId != register.DeploymentId || len(retire.MaintenanceRunId) != 0 {
		t.Fatalf("退休指回这次 REGISTER,由新一代的部署发起,没有 run: %v", retire)
	}
	if retire.ExpectedEpoch != 7 || retire.ExpectedCatalogRevision != 11 || !proto.Equal(retire.Writer, predecessor) {
		t.Fatalf("退休绑定当前 epoch、REGISTER 之后的版本和上一代原样的注册行: %v", retire)
	}
	retire.Writer.BootSeq = 99
	if predecessor.BootSeq != 4 {
		t.Fatal("退休全文要复制注册行,不和调用方共用")
	}
}

// 换代退休只由 Register 在同一事务里写:Retire 在碰任何 SQL 之前就拒绝(这里的目录没有连接)。
func TestRetireRefusesSupersessionRequests(t *testing.T) {
	retire := successionRetirement(successionRegisterForTest(t, 5), successionPredecessorForTest(4), 7, 11)
	if _, err := successionCatalogForTest().Retire(context.Background(), retire); !errors.Is(err, ErrWriterDenied) {
		t.Fatalf("Retire 不接受换代退休: %v", err)
	}
}
