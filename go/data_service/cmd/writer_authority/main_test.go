package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"

	"google.golang.org/protobuf/proto"
	dbpb "proto/common/database"
	"shared/authoritysql"
)

// lifecycleEpochForTest 是夹具里闸门当前的 epoch。RETIRE 的请求绑定它,REGISTER 的请求不带,收据记实际值。
const lifecycleEpochForTest = 7

func lifecycleRequestForTest(kind dbpb.SqlWriterLifecycleReceipt_Kind) *dbpb.SqlWriterLifecycleReceipt_Request {
	request := &dbpb.SqlWriterLifecycleReceipt_Request{
		RequestId: bytes.Repeat([]byte{1}, 16), Kind: kind,
		Writer: &dbpb.SqlWriterRegistry{WriterKey: bytes.Repeat([]byte{2}, 16),
			Generation: "00112233445566778899aabbccddeeff", InstanceId: bytes.Repeat([]byte{3}, 16),
			SqlPrincipal: []byte("normal_test@localhost"), RegistrationDigest: bytes.Repeat([]byte{4}, 32), RegisteredMs: 1},
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
	// 名册是预先渲染的:REGISTER 不带 epoch,带了就是旧渲染器出的。
	register := lifecycleRequestForTest(dbpb.SqlWriterLifecycleReceipt_REGISTER)
	if err := validateRequestIdentity(register, "register", physical); err != nil {
		t.Fatal(err)
	}
	register.ExpectedEpoch = 3
	if validateRequestIdentity(register, "register", physical) == nil {
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

// fakeRegistrationCatalog 是一个逻辑库上的目录。闸门状态和收据都在内存里,REGISTER 按当前 epoch 落地。
type fakeRegistrationCatalog struct {
	t                 *testing.T
	epoch             uint64
	frozen            bool
	frozenAtWrite     bool // 预检之后、提交之前闸门被冻结:只作用一次
	frozenAfterCommit bool // 注册提交之后闸门被冻结
	readyErr          error
	registerErr       error
	foreign           bool
	receipts          map[string]*dbpb.SqlWriterLifecycleReceipt
	registers         int
}

func (c *fakeRegistrationCatalog) RegistrationConsumedByOtherDeployment(context.Context, *dbpb.SqlWriterLifecycleReceipt_Request) (bool, error) {
	return c.foreign, nil
}

func (c *fakeRegistrationCatalog) RegistrationReady(context.Context, *dbpb.SqlWriterLifecycleReceipt_Request) error {
	if c.frozen {
		return authoritysql.ErrWriterFrozen
	}
	return c.readyErr
}

func (c *fakeRegistrationCatalog) Register(_ context.Context, request *dbpb.SqlWriterLifecycleReceipt_Request) (*dbpb.SqlWriterLifecycleReceipt, error) {
	c.registers++
	if c.frozen || c.frozenAtWrite {
		c.frozenAtWrite = false
		return nil, authoritysql.ErrWriterFrozen
	}
	if c.registerErr != nil {
		return nil, c.registerErr
	}
	receipt := lifecycleReceiptForTest(c.t, request)
	receipt.PreviousEpoch, receipt.ResultingEpoch = c.epoch, c.epoch
	if c.receipts == nil {
		c.receipts = map[string]*dbpb.SqlWriterLifecycleReceipt{}
	}
	c.receipts[string(request.RequestId)] = receipt
	return receipt, nil
}

func (c *fakeRegistrationCatalog) LifecycleReceipt(_ context.Context, id []byte) (*dbpb.SqlWriterLifecycleReceipt, error) {
	receipt, ok := c.receipts[string(id)]
	if !ok {
		return nil, sql.ErrNoRows
	}
	return receipt, nil
}

func (c *fakeRegistrationCatalog) CurrentMeta(context.Context) (*dbpb.RouteMeta, error) {
	writable := uint32(1)
	if c.frozen || c.frozenAfterCommit {
		writable = 0
	}
	return &dbpb.RouteMeta{Generation: "00112233445566778899aabbccddeeff", Writable: writable, WriterEpoch: c.epoch}, nil
}

// registrationHarness 把名册 entry 接到内存里的目录和尝试记录上。尝试记录的文件语义(私有目录、一次写)由 recordAttempt 自己的路径负责,这里只看结局。
type registrationHarness struct {
	catalogs map[string]*fakeRegistrationCatalog
	attempts map[string]bool
	entry    *dbpb.SqlWriterBootRoster_Entry
	reg      bootRegistration
}

func newRegistrationHarness(t *testing.T, schemas ...string) *registrationHarness {
	t.Helper()
	h := &registrationHarness{catalogs: map[string]*fakeRegistrationCatalog{}, attempts: map[string]bool{}, entry: &dbpb.SqlWriterBootRoster_Entry{}}
	for index, schema := range schemas {
		request := lifecycleRequestForTest(dbpb.SqlWriterLifecycleReceipt_REGISTER)
		request.RequestId = bytes.Repeat([]byte{byte(index + 1)}, 16)
		request.AuthoritySchema = schema
		request.ProtectedSchemas = []string{schema}
		h.entry.Requests = append(h.entry.Requests, request)
		h.catalogs[schema] = &fakeRegistrationCatalog{t: t, epoch: 9}
	}
	h.reg = bootRegistration{
		bootOutput: "boot",
		physical:   bytes.Repeat([]byte{5}, 32),
		open: func(request *dbpb.SqlWriterLifecycleReceipt_Request) (registrationCatalog, error) {
			return h.catalogs[request.AuthoritySchema], nil
		},
		recordAttempt: func(path string, _ *dbpb.SqlWriterLifecycleReceipt_Request) (bool, error) {
			if h.attempts[path] {
				return false, nil
			}
			h.attempts[path] = true
			return true, nil
		},
		retractAttempt: func(path string) error {
			delete(h.attempts, path)
			return nil
		},
	}
	return h
}

func (h *registrationHarness) register(t *testing.T) (*dbpb.SqlWriterBootGrantSet, error) {
	t.Helper()
	set, _, consumed, err := h.reg.register(context.Background(), h.entry)
	if consumed {
		t.Fatal("这条 entry 没有被别的 boot 用过")
	}
	return set, err
}

// 名册是很久以前渲染的,这期间 cutover、回滚把 epoch 推进了好几轮:REGISTER 照样成功,收据记实际 epoch。
func TestRegisterSucceedsAfterEpochChanged(t *testing.T) {
	h := newRegistrationHarness(t, "zone_1_db")
	h.catalogs["zone_1_db"].epoch = 41
	set, err := h.register(t)
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Grants) != 1 || set.Grants[0].AuthoritySchema != "zone_1_db" {
		t.Fatalf("写资格: %v", set.Grants)
	}
	if got := h.catalogs["zone_1_db"].receipts[string(h.entry.Requests[0].RequestId)].PreviousEpoch; got != 41 {
		t.Fatalf("收据记实际 epoch: %d", got)
	}
}

// 冻结期 REGISTER 回可重试:什么都没写,没有尝试记录,这条 entry 没烧掉。解冻之后同一条接着注册。
func TestRegisterWhileFrozenBurnsNothing(t *testing.T) {
	h := newRegistrationHarness(t, "zone_1_db")
	catalog := h.catalogs["zone_1_db"]
	catalog.frozen = true
	if _, err := h.register(t); !authoritysql.IsWriterFrozen(err) {
		t.Fatalf("要回可重试的冻结: %v", err)
	}
	if catalog.registers != 0 || len(h.attempts) != 0 {
		t.Fatalf("预检就挡住了,不写也不留尝试记录: registers=%d attempts=%v", catalog.registers, h.attempts)
	}
	if consumed, err := h.reg.consumed(context.Background(), h.entry); err != nil || consumed {
		t.Fatalf("这条 entry 还能用: %v %v", consumed, err)
	}
	catalog.frozen = false
	if _, err := h.register(t); err != nil {
		t.Fatalf("解冻之后: %v", err)
	}
}

// 预检之后才冻结:提交时被挡,事务已回滚,尝试记录要撤掉,否则重试按「结局未定」处理,这条 entry 就废了。
func TestRegisterFrozenAtCommitRetractsAttempt(t *testing.T) {
	h := newRegistrationHarness(t, "zone_1_db")
	catalog := h.catalogs["zone_1_db"]
	catalog.frozenAtWrite = true
	if _, err := h.register(t); !authoritysql.IsWriterFrozen(err) {
		t.Fatalf("要回可重试的冻结: %v", err)
	}
	if catalog.registers != 1 || len(h.attempts) != 0 {
		t.Fatalf("尝试记录要撤掉: registers=%d attempts=%v", catalog.registers, h.attempts)
	}
	if _, err := h.register(t); err != nil {
		t.Fatalf("重试: %v", err)
	}
	if catalog.registers != 2 {
		t.Fatalf("重试要真的再注册一次: %d", catalog.registers)
	}
}

// 多库 entry 第一份成了、第二份撞上冻结:重试时第一份只查收据,第二份接着注册,一份不多一份不少。
func TestRegisterPartialEntryResumes(t *testing.T) {
	h := newRegistrationHarness(t, "zone_1_db", "zone_2_db")
	first, second := h.catalogs["zone_1_db"], h.catalogs["zone_2_db"]
	second.frozenAtWrite = true
	if _, err := h.register(t); !authoritysql.IsWriterFrozen(err) {
		t.Fatalf("要回可重试的冻结: %v", err)
	}
	if first.registers != 1 || second.registers != 1 || len(h.attempts) != 1 {
		t.Fatalf("第一份留着尝试记录,第二份撤掉: %d %d %v", first.registers, second.registers, h.attempts)
	}
	set, err := h.register(t)
	if err != nil {
		t.Fatalf("重试: %v", err)
	}
	if first.registers != 1 || second.registers != 2 {
		t.Fatalf("第一份不重发,第二份接着注册: %d %d", first.registers, second.registers)
	}
	if len(set.Grants) != 2 {
		t.Fatalf("写资格要齐: %d", len(set.Grants))
	}
}

// 有一个库注定失败时,第一份都不写,不留半套注册。
func TestRegisterPrecheckPreventsPartialEntry(t *testing.T) {
	for name, block := range map[string]func(*fakeRegistrationCatalog){
		"资格不符": func(c *fakeRegistrationCatalog) { c.readyErr = authoritysql.ErrWriterDenied },
		"闸门冻结": func(c *fakeRegistrationCatalog) { c.frozen = true },
	} {
		t.Run(name, func(t *testing.T) {
			h := newRegistrationHarness(t, "zone_1_db", "zone_2_db")
			block(h.catalogs["zone_2_db"])
			if _, err := h.register(t); err == nil {
				t.Fatal("要报错")
			}
			if h.catalogs["zone_1_db"].registers != 0 || len(h.attempts) != 0 {
				t.Fatalf("第一份不能写: %d %v", h.catalogs["zone_1_db"].registers, h.attempts)
			}
		})
	}
}

// 注册提交之后闸门才冻结:收据已确认但资格不发,回可重试。解冻之后只查收据,不重发。
func TestRegisterGateFrozenAfterConfirmIsRetryable(t *testing.T) {
	h := newRegistrationHarness(t, "zone_1_db")
	catalog := h.catalogs["zone_1_db"]
	catalog.frozenAfterCommit = true
	if _, err := h.register(t); !authoritysql.IsWriterFrozen(err) {
		t.Fatalf("要回可重试的冻结: %v", err)
	}
	if catalog.registers != 1 {
		t.Fatalf("注册已经落地: %d", catalog.registers)
	}
	catalog.frozenAfterCommit = false
	set, err := h.register(t)
	if err != nil {
		t.Fatalf("解冻之后: %v", err)
	}
	if catalog.registers != 1 || len(set.Grants) != 1 {
		t.Fatalf("不重发,写资格照发: %d %d", catalog.registers, len(set.Grants))
	}
}

// 结局未知的尝试记录留着:重试只查原请求,查不到就停,不重发。
func TestRegisterUnknownOutcomeKeepsAttempt(t *testing.T) {
	h := newRegistrationHarness(t, "zone_1_db")
	catalog := h.catalogs["zone_1_db"]
	catalog.registerErr = errors.Join(authoritysql.ErrOutcomeUnknown, errors.New("commit"))
	if _, err := h.register(t); !errors.Is(err, authoritysql.ErrOutcomeUnknown) {
		t.Fatalf("结局未知: %v", err)
	}
	if len(h.attempts) != 1 {
		t.Fatalf("尝试记录要留着: %v", h.attempts)
	}
	catalog.registerErr = nil
	if _, err := h.register(t); err == nil {
		t.Fatal("查不到收据不能重发")
	}
	if catalog.registers != 1 {
		t.Fatalf("不重发: %d", catalog.registers)
	}
}

func TestRegisterEntryConsumedByOtherBoot(t *testing.T) {
	h := newRegistrationHarness(t, "zone_1_db", "zone_2_db")
	h.catalogs["zone_2_db"].foreign = true
	consumed, err := h.reg.consumed(context.Background(), h.entry)
	if err != nil || !consumed {
		t.Fatalf("任一份被别的 boot 用过,整条算用掉: %v %v", consumed, err)
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
