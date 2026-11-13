package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"google.golang.org/protobuf/proto"
	"path/filepath"
	dbpb "proto/common/database"
	"shared/authoritysql"
	"strings"
	"testing"
)

// retire-removed-slots 的纯规则(writer-roster-exhaustion-20261106.md §5.8):目录与集群都在内存里,不连库、不连 API server。

// retirementLog 按先后记下读了什么、退休了什么,用来核「先读目录、后看集群」。
type retirementLog struct{ calls []string }

func (l *retirementLog) add(call string) { l.calls = append(l.calls, call) }

// fakeRetirementCatalog 是一个逻辑库上的写者闸门:按账号记最新一代;退休按 epoch、目录版本与这一代原样的注册行 CAS,和 Catalog.Retire 同一判据。
type fakeRetirementCatalog struct {
	t        *testing.T
	schema   string
	log      *retirementLog
	latest   map[string]*dbpb.SqlWriterRegistry
	epoch    uint64
	revision uint64
	frozen   bool
	runID    []byte
	// raceRevisions:接下来这么多次退休,都先被同库别的注册推进一步目录版本(读 meta 之后、退休之前)。
	raceRevisions int
	// supersedeBeforeRetire:退休之前,同序号的新 Pod 先换了代。
	supersedeBeforeRetire bool
	retireErr             error
	retired               []*dbpb.SqlWriterLifecycleReceipt_Request
	// registeredBy:账号 → 这个账号在役一代的 REGISTER 里记的登记身份;没列的账号是本 namespace 的工作负载登记的(workloadIdentityForTest)。
	registeredBy map[string]string
	// registrationErr:读 REGISTER 收据失败。
	registrationErr error
}

// Registration 回这一代的 REGISTER 原件:名册模板的形状加上这一代的登记行与登记身份。
func (c *fakeRetirementCatalog) Registration(_ context.Context, writer *dbpb.SqlWriterRegistry) (*dbpb.SqlWriterLifecycleReceipt_Request, error) {
	c.log.add("registration " + c.schema + " " + string(writer.SqlPrincipal))
	if c.registrationErr != nil {
		return nil, c.registrationErr
	}
	identity, overridden := c.registeredBy[string(writer.SqlPrincipal)]
	if !overridden {
		identity = workloadIdentityForTest
	}
	request := slotTemplateForTest(c.schema)
	request.Writer = proto.Clone(writer).(*dbpb.SqlWriterRegistry)
	request.RegisteredBy = identity
	return request, nil
}

func (c *fakeRetirementCatalog) LatestBoot(_ context.Context, principal []byte) (*dbpb.SqlWriterRegistry, error) {
	c.log.add("latest " + c.schema + " " + string(principal))
	row := c.latest[string(principal)]
	if row == nil {
		return nil, nil
	}
	return proto.Clone(row).(*dbpb.SqlWriterRegistry), nil
}

func (c *fakeRetirementCatalog) CurrentMeta(context.Context) (*dbpb.RouteMeta, error) {
	writable := uint32(1)
	if c.frozen {
		writable = 0
	}
	return &dbpb.RouteMeta{Generation: generationForTest, Writable: writable, WriterEpoch: c.epoch, CatalogRevision: c.revision, FreezeRunId: bytes.Clone(c.runID)}, nil
}

func (c *fakeRetirementCatalog) Retire(_ context.Context, request *dbpb.SqlWriterLifecycleReceipt_Request) (*dbpb.SqlWriterLifecycleReceipt, error) {
	c.log.add("retire " + c.schema + " " + string(request.Writer.SqlPrincipal))
	if c.retireErr != nil {
		return nil, c.retireErr
	}
	principal := string(request.Writer.SqlPrincipal)
	if c.raceRevisions > 0 {
		c.raceRevisions--
		c.revision++
	}
	if c.supersedeBeforeRetire {
		c.supersedeBeforeRetire = false
		next := proto.Clone(c.latest[principal]).(*dbpb.SqlWriterRegistry)
		next.WriterKey = bytes.Repeat([]byte{0xee}, 16)
		next.BootSeq++
		c.latest[principal] = next
		c.revision += 2
	}
	current := c.latest[principal]
	if request.ExpectedEpoch != c.epoch || request.ExpectedCatalogRevision != c.revision || current == nil || current.State != dbpb.SqlWriterRegistry_ACTIVE || !proto.Equal(current, request.Writer) {
		return nil, authoritysql.ErrWriterDenied
	}
	current.State = dbpb.SqlWriterRegistry_REVOKED
	c.revision++
	c.retired = append(c.retired, proto.Clone(request).(*dbpb.SqlWriterLifecycleReceipt_Request))
	receipt := lifecycleReceiptForTest(c.t, request)
	receipt.PreviousEpoch, receipt.ResultingEpoch = c.epoch, c.epoch
	return receipt, nil
}

// fakeStatefulSet 是集群里的写者 StatefulSet:第 n 次读副本数回 replicas[n](读多了回最后一个),pods 里为 true 的 Pod 对象在。
// serviceAccounts 是本 namespace 里的 ServiceAccount(名字 → UID);nil 表示只有夹具写者用的那一个(default)。
type fakeStatefulSet struct {
	log               *retirementLog
	replicas          []int
	pods              map[string]bool
	podErr            error
	reads             int
	serviceAccounts   map[string]string
	serviceAccountErr error
}

func (s *fakeStatefulSet) ServiceAccountUID(_ context.Context, name string) (string, bool, error) {
	s.log.add("service-account " + name)
	if s.serviceAccountErr != nil {
		return "", false, s.serviceAccountErr
	}
	accounts := s.serviceAccounts
	if accounts == nil {
		accounts = map[string]string{serviceAccountForTest: serviceAccountUIDForTest}
	}
	uid, found := accounts[name]
	return uid, found, nil
}

func (s *fakeStatefulSet) Replicas(_ context.Context, statefulSet string) (int, error) {
	s.log.add("replicas " + statefulSet)
	value := s.replicas[min(s.reads, len(s.replicas)-1)]
	s.reads++
	return value, nil
}

func (s *fakeStatefulSet) PodExists(_ context.Context, pod string) (bool, error) {
	s.log.add("pod " + pod)
	if s.podErr != nil {
		return false, s.podErr
	}
	return s.pods[pod], nil
}

// retirementHarness:StatefulSet db 的名册有 slots 个槽位,每个槽位写 schemas 这些逻辑库。
type retirementHarness struct {
	log      *retirementLog
	roster   *dbpb.SqlWriterBootRoster
	catalogs map[string]*fakeRetirementCatalog
	workload *fakeStatefulSet
}

func slotPrincipalForTest(slot int) string { return fmt.Sprintf("slot_%d@%%", slot) }

// slotWriterForTest 是槽位 slot 第 seq 代的注册行。
func slotWriterForTest(slot int, seq uint64, state dbpb.SqlWriterRegistry_State) *dbpb.SqlWriterRegistry {
	return &dbpb.SqlWriterRegistry{WriterKey: bytes.Repeat([]byte{byte(slot + 1)}, 16), Generation: generationForTest, InstanceId: bytes.Repeat([]byte{byte(slot + 101)}, 16),
		SqlPrincipal: []byte(slotPrincipalForTest(slot)), RegistrationDigest: bytes.Repeat([]byte{byte(slot + 51)}, 32), State: state, RegisteredMs: 1_700_000_000_000, BootSeq: seq}
}

func newRetirementHarness(t *testing.T, slots int, schemas ...string) *retirementHarness {
	t.Helper()
	h := &retirementHarness{log: &retirementLog{}, roster: &dbpb.SqlWriterBootRoster{Format: slotRosterFormat}, catalogs: map[string]*fakeRetirementCatalog{}}
	for slot := 0; slot < slots; slot++ {
		entry := slotEntryForTest(schemas...)
		for _, request := range entry.Requests {
			request.Writer.SqlPrincipal = []byte(slotPrincipalForTest(slot))
		}
		h.roster.Entries = append(h.roster.Entries, entry)
	}
	if err := validateSlotRoster(h.roster, physicalForTest); err != nil {
		t.Fatal(err)
	}
	for _, schema := range schemas {
		h.catalogs[schema] = &fakeRetirementCatalog{t: t, schema: schema, log: h.log, latest: map[string]*dbpb.SqlWriterRegistry{}, epoch: 9, revision: 40}
	}
	h.workload = &fakeStatefulSet{log: h.log, pods: map[string]bool{}}
	return h
}

// boot 让槽位 slot 在全部逻辑库的最新一代是第 seq 代,state 是它的状态。
func (h *retirementHarness) boot(slot int, seq uint64, state dbpb.SqlWriterRegistry_State) {
	for _, catalog := range h.catalogs {
		catalog.latest[slotPrincipalForTest(slot)] = slotWriterForTest(slot, seq, state)
	}
}

func (h *retirementHarness) retirer() slotRetirer {
	return slotRetirer{statefulSet: "db", namespace: namespaceForTest, deploymentID: "sql-writer-slot-retirer/db/job-pod-uid",
		open: func(request *dbpb.SqlWriterLifecycleReceipt_Request) (retirementCatalog, error) {
			return h.catalogs[request.AuthoritySchema], nil
		},
		workload: h.workload, random: rand.Reader}
}

func (h *retirementHarness) run(t *testing.T) (retirementReport, error) {
	t.Helper()
	return h.retirer().run(context.Background(), h.roster)
}

func outcomesOf(report retirementReport) map[string]int {
	counts := map[string]int{}
	for _, result := range report.results {
		counts[fmt.Sprintf("%d/%s", result.ordinal, result.outcome)]++
	}
	return counts
}

func (h *retirementHarness) called(prefix string) int {
	count := 0
	for _, call := range h.log.calls {
		if strings.HasPrefix(call, prefix) {
			count++
		}
	}
	return count
}

// 缩到 2 个副本:序号 2 的 Pod 已经没了,它在每个逻辑库的在役一代被结束;序号 3 的 Pod 还在优雅退出,不动它;
// 序号 0、1 在副本数以内,目录都不读。结束用的 RETIRE 与人工退休同一形状:writer 原样是注册行(含 boot_seq),不带 run 与 successor。
func TestRetireRemovedSlotsEndsRemovedSlotsWhosePodsAreGone(t *testing.T) {
	h := newRetirementHarness(t, 4, "zone_1_db", "zone_2_db")
	for slot := 0; slot < 4; slot++ {
		h.boot(slot, 3, dbpb.SqlWriterRegistry_ACTIVE)
	}
	h.workload.replicas = []int{2}
	h.workload.pods["db-3"] = true
	report, err := h.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if got := outcomesOf(report); got["2/"+slotRetired] != 2 || got["3/"+slotPodPresent] != 2 || len(got) != 2 {
		t.Fatalf("结局: %v", got)
	}
	for schema, catalog := range h.catalogs {
		if len(catalog.retired) != 1 {
			t.Fatalf("%s 结束了 %d 代", schema, len(catalog.retired))
		}
		request := catalog.retired[0]
		if request.Kind != dbpb.SqlWriterLifecycleReceipt_RETIRE || !proto.Equal(request.Writer, slotWriterForTest(2, 3, dbpb.SqlWriterRegistry_ACTIVE)) ||
			request.ExpectedEpoch != 9 || request.ExpectedCatalogRevision != 40 || request.DeploymentId != "sql-writer-slot-retirer/db/job-pod-uid" ||
			len(request.MaintenanceRunId) != 0 || len(request.SuccessorRequestId) != 0 || len(request.RequestId) != 16 ||
			request.AuthoritySchema != schema || len(request.ProtectedSchemas) != 1 || request.ProtectedSchemas[0] != schema || !bytes.Equal(request.DeploymentDigest, physicalForTest) {
			t.Fatalf("%s 的 RETIRE: %v", schema, request)
		}
		if err := validateRequestIdentity(request, "retire", physicalForTest); err != nil {
			t.Fatalf("和人工退休过同一道核对: %v", err)
		}
		if catalog.latest[slotPrincipalForTest(2)].State != dbpb.SqlWriterRegistry_REVOKED || catalog.latest[slotPrincipalForTest(3)].State != dbpb.SqlWriterRegistry_ACTIVE ||
			catalog.latest[slotPrincipalForTest(0)].State != dbpb.SqlWriterRegistry_ACTIVE {
			t.Fatalf("%s 只结束序号 2", schema)
		}
	}
	for _, call := range h.log.calls {
		if strings.Contains(call, "slot_0@") || strings.Contains(call, "slot_1@") {
			t.Fatalf("副本数以内的槽位不读目录: %s", call)
		}
	}
	if lines := report.lines("db"); !strings.Contains(strings.Join(lines, "\n"), "result=retired request_id=") || !strings.Contains(lines[len(lines)-1], "retired=2") {
		t.Fatalf("日志: %v", lines)
	}
}

// 先读目录、后看集群:第二次读副本数和查 Pod 都排在全部目录读之后。反过来的话,看完集群、读目录之前新建的 Pod 注册的一代会被误结束。
func TestRetireRemovedSlotsReadsCatalogBeforeCluster(t *testing.T) {
	h := newRetirementHarness(t, 3, "zone_1_db", "zone_2_db")
	h.boot(1, 2, dbpb.SqlWriterRegistry_ACTIVE)
	h.boot(2, 5, dbpb.SqlWriterRegistry_ACTIVE)
	h.workload.replicas = []int{1}
	if _, err := h.run(t); err != nil {
		t.Fatal(err)
	}
	if len(h.log.calls) == 0 || !strings.HasPrefix(h.log.calls[0], "replicas ") {
		t.Fatalf("第一步只读副本数,缩小扫描范围: %v", h.log.calls)
	}
	lastCatalogRead, secondReplicas, firstPod := -1, -1, -1
	replicasReads := 0
	for index, call := range h.log.calls {
		switch {
		case strings.HasPrefix(call, "latest ") && secondReplicas < 0:
			lastCatalogRead = index
		case strings.HasPrefix(call, "replicas "):
			if replicasReads++; replicasReads == 2 {
				secondReplicas = index
			}
		case strings.HasPrefix(call, "pod ") && firstPod < 0:
			firstPod = index
		}
	}
	if lastCatalogRead < 0 || secondReplicas <= lastCatalogRead || firstPod <= lastCatalogRead {
		t.Fatalf("目录要先读完: %v", h.log.calls)
	}
	if h.called("retire ") != 4 {
		t.Fatalf("序号 1、2 在两个库各结束一代: %v", h.log.calls)
	}
}

// 读完目录之后副本数又涨回来了(扩容):这些序号的新 Pod 会换代,这里不结束,也不用查 Pod。
func TestRetireRemovedSlotsLeavesSlotsScaledBackUp(t *testing.T) {
	h := newRetirementHarness(t, 4, "zone_1_db")
	h.boot(2, 1, dbpb.SqlWriterRegistry_ACTIVE)
	h.boot(3, 1, dbpb.SqlWriterRegistry_ACTIVE)
	h.workload.replicas = []int{2, 4}
	report, err := h.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if got := outcomesOf(report); got["2/"+slotScaledBackUp] != 1 || got["3/"+slotScaledBackUp] != 1 || h.called("pod ") != 0 || h.called("retire ") != 0 {
		t.Fatalf("扩回来的序号不动: %v %v", got, h.log.calls)
	}
}

// 整个 StatefulSet 删了(副本数读成 0)而 Pod 都没了:它的槽位全算被缩掉,全部结束。
func TestRetireRemovedSlotsEndsAllSlotsOfDeletedStatefulSet(t *testing.T) {
	h := newRetirementHarness(t, 2, "zone_1_db")
	h.boot(0, 7, dbpb.SqlWriterRegistry_ACTIVE)
	h.boot(1, 2, dbpb.SqlWriterRegistry_ACTIVE)
	h.workload.replicas = []int{0}
	report, err := h.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if got := outcomesOf(report); got["0/"+slotRetired] != 1 || got["1/"+slotRetired] != 1 {
		t.Fatalf("全部结束: %v", got)
	}
}

// 闸门冻结、排空 run 开着:这一轮不动它们,也不算失败;下一轮再来。排空要退休的写者按排空的流程走,非 run 的退休本来就会被拒。
func TestRetireRemovedSlotsWaitsOutFrozenGateAndDrainRun(t *testing.T) {
	h := newRetirementHarness(t, 2, "zone_1_db", "zone_2_db")
	h.boot(1, 4, dbpb.SqlWriterRegistry_ACTIVE)
	h.workload.replicas = []int{1}
	h.catalogs["zone_1_db"].frozen = true
	h.catalogs["zone_2_db"].runID = bytes.Repeat([]byte{7}, 16)
	report, err := h.run(t)
	if err != nil {
		t.Fatal(err)
	}
	outcomes := map[string]string{}
	for _, result := range report.results {
		outcomes[result.schema] = result.outcome
	}
	if outcomes["zone_1_db"] != slotGateFrozen || outcomes["zone_2_db"] != slotDrainRunOpen || h.called("retire ") != 0 {
		t.Fatalf("冻结与排空期间不退休: %v %v", outcomes, h.log.calls)
	}
	for schema, catalog := range h.catalogs {
		if catalog.latest[slotPrincipalForTest(1)].State != dbpb.SqlWriterRegistry_ACTIVE {
			t.Fatalf("%s 的这一代还在役", schema)
		}
	}
}

// 退休被拒,重读目录发现这一代已经不在役(同序号的新 Pod 先换了代):已结束,不算失败。
func TestRetireRemovedSlotsTreatsGenerationEndedElsewhereAsDone(t *testing.T) {
	h := newRetirementHarness(t, 2, "zone_1_db")
	h.boot(1, 4, dbpb.SqlWriterRegistry_ACTIVE)
	h.workload.replicas = []int{1}
	h.catalogs["zone_1_db"].supersedeBeforeRetire = true
	report, err := h.run(t)
	if err != nil {
		t.Fatal(err)
	}
	if got := outcomesOf(report); got["1/"+slotAlreadyEnded] != 1 || len(h.catalogs["zone_1_db"].retired) != 0 {
		t.Fatalf("已被换代的一代不再结束: %v", got)
	}
	if latest := h.catalogs["zone_1_db"].latest[slotPrincipalForTest(1)]; latest.State != dbpb.SqlWriterRegistry_ACTIVE || latest.BootSeq != 5 {
		t.Fatalf("新一代不受影响: %v", latest)
	}
}

// 退休被拒而这一代还在役:读 meta 之后目录版本被同库别的注册推进了,拿新的 meta 再试;一直被拒就报错,这一代留着在役。
func TestRetireRemovedSlotsRetriesWithFreshMetaAndGivesUp(t *testing.T) {
	h := newRetirementHarness(t, 2, "zone_1_db")
	h.boot(1, 4, dbpb.SqlWriterRegistry_ACTIVE)
	h.workload.replicas = []int{1}
	h.catalogs["zone_1_db"].raceRevisions = 1
	report, err := h.run(t)
	if err != nil {
		t.Fatal(err)
	}
	catalog := h.catalogs["zone_1_db"]
	if got := outcomesOf(report); got["1/"+slotRetired] != 1 || len(catalog.retired) != 1 || catalog.retired[0].ExpectedCatalogRevision != 41 {
		t.Fatalf("第二次按新目录版本落地: %v", got)
	}

	h = newRetirementHarness(t, 2, "zone_1_db")
	h.boot(1, 4, dbpb.SqlWriterRegistry_ACTIVE)
	h.workload.replicas = []int{1}
	h.catalogs["zone_1_db"].raceRevisions = retireRemovedSlotsAttempts
	report, err = h.run(t)
	if err == nil || !strings.Contains(err.Error(), "still denied") || h.called("retire ") != retireRemovedSlotsAttempts {
		t.Fatalf("试满还拒就报错: %v %v", err, h.log.calls)
	}
	if got := outcomesOf(report); got["1/"+slotFailed] != 1 || h.catalogs["zone_1_db"].latest[slotPrincipalForTest(1)].State != dbpb.SqlWriterRegistry_ACTIVE {
		t.Fatalf("没有结束: %v", got)
	}
}

// 结局未知:报错让 Job 失败,不在本地记任何东西;别的库照常结束。下一轮按目录状态重来,落地过的话这一代已经不在役,不会结束两次。
func TestRetireRemovedSlotsReportsUnknownOutcome(t *testing.T) {
	h := newRetirementHarness(t, 2, "zone_1_db", "zone_2_db")
	h.boot(1, 4, dbpb.SqlWriterRegistry_ACTIVE)
	h.workload.replicas = []int{1}
	h.catalogs["zone_1_db"].retireErr = errors.Join(authoritysql.ErrOutcomeUnknown, errors.New("commit"))
	report, err := h.run(t)
	if !errors.Is(err, authoritysql.ErrOutcomeUnknown) {
		t.Fatalf("结局未知要报出来: %v", err)
	}
	outcomes := map[string]string{}
	for _, result := range report.results {
		outcomes[result.schema] = result.outcome
	}
	if outcomes["zone_1_db"] != slotFailed || outcomes["zone_2_db"] != slotRetired {
		t.Fatalf("一个库失败不挡别的库: %v", outcomes)
	}
}

// 没有在役的被缩掉槽位:不再读集群、不退休;副本数不小于槽位数时连目录都不读。
func TestRetireRemovedSlotsDoesNothingWithoutActiveRemovedSlots(t *testing.T) {
	h := newRetirementHarness(t, 3, "zone_1_db")
	h.boot(2, 6, dbpb.SqlWriterRegistry_REVOKED)
	h.workload.replicas = []int{1}
	report, err := h.run(t)
	if err != nil || len(report.results) != 0 || h.called("replicas ") != 1 || h.called("pod ") != 0 || h.called("retire ") != 0 {
		t.Fatalf("没有候选: %v %v", err, h.log.calls)
	}

	h = newRetirementHarness(t, 3, "zone_1_db")
	h.boot(2, 6, dbpb.SqlWriterRegistry_ACTIVE)
	h.workload.replicas = []int{3}
	if _, err = h.run(t); err != nil || h.called("latest ") != 0 {
		t.Fatalf("副本数以内不读目录: %v %v", err, h.log.calls)
	}
}

// 查 Pod 失败:报错,什么都不结束。看不到集群就不知道 Pod 还在不在,不能替它下结论。
func TestRetireRemovedSlotsStopsWhenPodsCannotBeRead(t *testing.T) {
	h := newRetirementHarness(t, 2, "zone_1_db")
	h.boot(1, 4, dbpb.SqlWriterRegistry_ACTIVE)
	h.workload.replicas = []int{1}
	h.workload.podErr = errors.New("HTTP 403")
	if _, err := h.run(t); err == nil || h.called("retire ") != 0 {
		t.Fatalf("查不到 Pod 不结束: %v %v", err, h.log.calls)
	}
}

// 命令行:缺参数或混入别的命令的参数,在读任何凭据之前就拒绝;别的命令也不收 -statefulset。
func TestRetireRemovedSlotsCLIStopsBeforeLookingUpCredentials(t *testing.T) {
	root := t.TempDir()
	roster := filepath.Join(root, "request", "roster.json")
	sha := strings.Repeat("ab", 32)
	base := func(extra ...string) []string {
		return append([]string{"--command", "retire-removed-slots", "--roster", roster, "--roster-sha256", sha, "--input-root", root, "--deployment-id", "0f8fad5b-d9cb-469f-a165-70867728950e"}, extra...)
	}
	for name, args := range map[string][]string{
		"缺 StatefulSet":     base(),
		"StatefulSet 名不合法":  base("--statefulset", "DB_0"),
		"缺 deployment-id":   {"--command", "retire-removed-slots", "--roster", roster, "--roster-sha256", sha, "--input-root", root, "--statefulset", "db"},
		"相对名册路径":            {"--command", "retire-removed-slots", "--roster", "roster.json", "--roster-sha256", sha, "--input-root", root, "--statefulset", "db", "--deployment-id", "uid"},
		"缺名册 SHA":           {"--command", "retire-removed-slots", "--roster", roster, "--input-root", root, "--statefulset", "db", "--deployment-id", "uid"},
		"混入 pod-name":       base("--statefulset", "db", "--pod-name", "db-0"),
		"混入 request":        base("--statefulset", "db", "--request", filepath.Join(root, "r.json")),
		"混入 boot-output":    base("--statefulset", "db", "--boot-output", filepath.Join(root, "grant.json")),
		"超时超过 60 秒":         base("--statefulset", "db", "--timeout", "61s"),
		"register 不收":       {"--command", "register", "--statefulset", "db"},
		"retire 不收":         {"--command", "retire", "--request", filepath.Join(root, "r.json"), "--receipt-output", filepath.Join(root, "o.json"), "--statefulset", "db"},
		"prepare-schema 不收": {"--command", "prepare-schema", "--schema", "zone_1_db", "--statefulset", "db"},
	} {
		lookedUp := false
		var out bytes.Buffer
		err := run(context.Background(), args, func(string) string { lookedUp = true; return "must-not-read" }, &out)
		if err == nil || lookedUp || out.Len() != 0 {
			t.Fatalf("%s: 非法命令行要在读凭据之前拒绝: err=%v lookedUp=%v", name, err, lookedUp)
		}
	}
}

// 只结束本 namespace 的工作负载登记的一代(设计稿 §5.9「归属」)。同一份名册和注册账号被带到另一个集群时(集群级备份恢复、
// 备用集群把写者缩到 0 却没停 CronJob、两个环境误共用名册),那边「Pod 不在」不说明这一代没有进程了:不核归属,
// 那边的收尾会把正式集群在役的写者结束掉。归属说不清的一律不动、这一轮算失败;同一轮里自己登记的照常结束。
func TestRetireRemovedSlotsLeavesGenerationsRegisteredElsewhere(t *testing.T) {
	otherCluster := workloadIdentity{namespace: namespaceForTest, serviceAccount: serviceAccountForTest, uid: "11111111-2222-3333-4444-555555555555"}.String()
	for name, identity := range map[string]string{
		"另一个集群:同名 namespace 与 ServiceAccount,UID 不同": otherCluster,
		"另一个 namespace":           workloadIdentity{namespace: "mmorpg-zone-2", serviceAccount: serviceAccountForTest, uid: serviceAccountUIDForTest}.String(),
		"集群外登记的空身份":               "",
		"认不出的身份":                  "node:worker-1",
		"这里已经没有这个 ServiceAccount": workloadIdentity{namespace: namespaceForTest, serviceAccount: "removed-account", uid: serviceAccountUIDForTest}.String(),
	} {
		h := newRetirementHarness(t, 3, "zone_1_db")
		h.boot(1, 4, dbpb.SqlWriterRegistry_ACTIVE)
		h.boot(2, 6, dbpb.SqlWriterRegistry_ACTIVE)
		h.workload.replicas = []int{1}
		catalog := h.catalogs["zone_1_db"]
		catalog.registeredBy = map[string]string{slotPrincipalForTest(1): identity}
		report, err := h.run(t)
		if !errors.Is(err, errRegisteredElsewhere) {
			t.Fatalf("%s: 归属说不清要报出来: %v", name, err)
		}
		if got := outcomesOf(report); got["1/"+slotRegisteredElsewhere] != 1 || got["2/"+slotRetired] != 1 || len(got) != 2 {
			t.Fatalf("%s: 别处登记的不动,自己登记的照常结束: %v", name, got)
		}
		if catalog.latest[slotPrincipalForTest(1)].State != dbpb.SqlWriterRegistry_ACTIVE || catalog.latest[slotPrincipalForTest(2)].State != dbpb.SqlWriterRegistry_REVOKED {
			t.Fatalf("%s: 只结束自己登记的那一代", name)
		}
		if len(catalog.retired) != 1 || string(catalog.retired[0].Writer.SqlPrincipal) != slotPrincipalForTest(2) {
			t.Fatalf("%s: 别处登记的一代连 RETIRE 都不发: %v", name, catalog.retired)
		}
		if lines := report.lines("db"); !strings.Contains(strings.Join(lines, "\n"), "slot=1 logical_db=zone_1_db boot_seq=4 result=registered-elsewhere") || !strings.HasSuffix(lines[len(lines)-1], " candidates=2 retired=1 pending=1") {
			t.Fatalf("%s: 日志点名这一代,汇总行算作没做完: %v", name, lines)
		}
	}
}

// 归属只在 Pod 已经没了、真要结束的时候才核:Pod 还在、读完目录又扩回来的候选不读 REGISTER、不查 ServiceAccount。
// 同一轮里同一个 ServiceAccount 只查一次。
func TestRetireRemovedSlotsChecksOwnershipOnlyBeforeRetiring(t *testing.T) {
	h := newRetirementHarness(t, 4, "zone_1_db", "zone_2_db")
	for slot := 1; slot < 4; slot++ {
		h.boot(slot, 2, dbpb.SqlWriterRegistry_ACTIVE)
	}
	h.workload.replicas = []int{1, 2}
	h.workload.pods["db-2"] = true
	if _, err := h.run(t); err != nil {
		t.Fatal(err)
	}
	if h.called("registration ") != 2 || h.called("service-account ") != 1 || h.called("retire ") != 2 {
		t.Fatalf("只有序号 3(Pod 没了)在两个库各核一次归属,ServiceAccount 只查一次: %v", h.log.calls)
	}
	for _, call := range h.log.calls {
		if strings.HasPrefix(call, "registration ") && !strings.Contains(call, slotPrincipalForTest(3)) {
			t.Fatalf("扩回来的序号 1、Pod 还在的序号 2 不核归属: %s", call)
		}
	}
}

// 读不到 REGISTER 收据、查不到 ServiceAccount(API server 报错):不结束,记失败,不当成「别处登记的」;别的库照常。
func TestRetireRemovedSlotsDoesNotRetireWhenOwnershipCannotBeRead(t *testing.T) {
	h := newRetirementHarness(t, 2, "zone_1_db", "zone_2_db")
	h.boot(1, 4, dbpb.SqlWriterRegistry_ACTIVE)
	h.workload.replicas = []int{1}
	h.catalogs["zone_1_db"].registrationErr = errors.New("connection reset")
	report, err := h.run(t)
	if err == nil || errors.Is(err, errRegisteredElsewhere) {
		t.Fatalf("读不到收据是失败,不是别处登记: %v", err)
	}
	outcomes := map[string]string{}
	for _, result := range report.results {
		outcomes[result.schema] = result.outcome
	}
	if outcomes["zone_1_db"] != slotFailed || outcomes["zone_2_db"] != slotRetired || len(h.catalogs["zone_1_db"].retired) != 0 {
		t.Fatalf("读不到收据的库不结束,另一个库照常: %v", outcomes)
	}

	h = newRetirementHarness(t, 2, "zone_1_db")
	h.boot(1, 4, dbpb.SqlWriterRegistry_ACTIVE)
	h.workload.replicas = []int{1}
	h.workload.serviceAccountErr = errors.New("HTTP 403")
	report, err = h.run(t)
	if err == nil || errors.Is(err, errRegisteredElsewhere) || h.called("retire ") != 0 {
		t.Fatalf("查不到 ServiceAccount 不结束: %v %v", err, h.log.calls)
	}
	if got := outcomesOf(report); got["1/"+slotFailed] != 1 {
		t.Fatalf("记失败: %v", got)
	}
}

// 汇总行的 pending 是部署脚本判「这一轮收尾做完没有」的依据(k8s_deploy.ps1 Complete-SqlWriterSlotRetirement):
// Pod 还在、闸门冻结、排空 run 开着、失败、不是本集群登记的都算还没做完;结束了的、别处先结束的、又扩回来的不算。
func TestRetirementReportCountsPendingCandidates(t *testing.T) {
	report := retirementReport{replicasBefore: 1, replicasAfter: 1, slots: 4}
	for _, outcome := range []string{slotRetired, slotAlreadyEnded, slotScaledBackUp, slotPodPresent, slotGateFrozen, slotDrainRunOpen, slotFailed, slotRegisteredElsewhere} {
		report.results = append(report.results, slotRetirementResult{ordinal: 2, schema: "zone_1_db", bootSeq: 3, outcome: outcome})
	}
	lines := report.lines("db")
	if len(lines) != len(report.results)+1 {
		t.Fatalf("每个候选一行,再加一行汇总: %v", lines)
	}
	want := "writer_authority command=retire-removed-slots statefulset=db replicas_before=1 replicas_after=1 roster_slots=4 candidates=8 retired=1 pending=5"
	if summary := lines[len(lines)-1]; summary != want {
		t.Fatalf("汇总行:\n got %s\nwant %s", summary, want)
	}
	done := retirementReport{slots: 4}.lines("db")
	if len(done) != 1 || !strings.HasSuffix(done[0], " candidates=0 retired=0 pending=0") {
		t.Fatalf("没有候选时汇总行 pending=0: %v", done)
	}
}
