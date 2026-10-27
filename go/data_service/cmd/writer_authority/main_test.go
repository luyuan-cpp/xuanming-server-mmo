package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/proto"
	dbpb "proto/common/database"
)

func lifecycleRequestForTest(kind dbpb.SqlWriterLifecycleReceipt_Kind) *dbpb.SqlWriterLifecycleReceipt_Request {
	return &dbpb.SqlWriterLifecycleReceipt_Request{
		RequestId: bytes.Repeat([]byte{1}, 16), Kind: kind,
		Writer: &dbpb.SqlWriterRegistry{WriterKey: bytes.Repeat([]byte{2}, 16),
			Generation: "00112233445566778899aabbccddeeff", InstanceId: bytes.Repeat([]byte{3}, 16),
			SqlPrincipal: []byte("normal_test@localhost"), RegistrationDigest: bytes.Repeat([]byte{4}, 32), RegisteredMs: 1},
		ExpectedEpoch: 7, ExpectedCatalogRevision: 11, AuthoritySchema: "authority_test",
		ProtectedSchemas: []string{"business_test"}, DeploymentDigest: bytes.Repeat([]byte{5}, 32),
		DeploymentId: "fixed-test-boot", LockPrimitive: dbpb.SqlWriterBootGrant_SELECT_ONLY,
		LockQualificationSha256: bytes.Repeat([]byte{6}, 32),
	}
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
		PreviousEpoch: request.ExpectedEpoch, ResultingEpoch: request.ExpectedEpoch,
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
	entry := &dbpb.SqlWriterBootRoster_Entry{Request: lifecycleRequestForTest(dbpb.SqlWriterLifecycleReceipt_REGISTER)}
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
