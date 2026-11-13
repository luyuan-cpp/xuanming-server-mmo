package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"google.golang.org/protobuf/proto"
	"io"
	"path/filepath"
	dbpb "proto/common/database"
	"regexp"
	"shared/authoritysql"
	"strings"
)

// 缩容之后,被缩掉的序号没有下一代来换代,它们的最后一代在每个逻辑库都还 ACTIVE(writer-roster-exhaustion-20261106.md §5.8)。
// 留着它有两个问题:下一次排空收不了尾(没有进程替它关闭);被缩掉的 Pod 若其实还活着(节点失联),它仍能写。
// retire-removed-slots 由每个写者 StatefulSet 自己的 CronJob 定时跑(deploy/k8s/manifests/go-svc/sql-writer-slot-retirer.yaml),
// 用和 init 里 registrar 同一个注册账号,把这些槽位的在役一代按非 run 的 RETIRE 结束。结束一代与换代同一段写(endGeneration),
// 注册账号本来就能做:它能注册新一代,也能在换代里结束上一代,这里只是没有下一代的那种结束。
// 只结束本 namespace 的工作负载登记的一代(REGISTER 的 registered_by,workload_identity.go):同一份名册和账号被带到另一个集群时,
// 那边「Pod 不在」不说明这一代没有进程了。

// retireRemovedSlotsAttempts 是一份退休被拒之后最多试几次。拒绝多半是读 meta 之后,目录版本被同库别的注册推进了,拿新的 meta 再试就过。
const retireRemovedSlotsAttempts = 3

// kubernetesNamePattern 是 namespace、StatefulSet 与 Pod 名的形状(DNS-1123 label)。名字拼进 API 路径之前先过它。
var kubernetesNamePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// retireRemovedSlotsOptions 是 retire-removed-slots 的命令行输入,纯校验都在读凭据之前做完。
type retireRemovedSlotsOptions struct {
	rosterPath, rosterSHA, inputRoot, statefulSet, deploymentID string
}

func (o retireRemovedSlotsOptions) check() error {
	if !filepath.IsAbs(o.rosterPath) || !filepath.IsAbs(o.inputRoot) {
		return errors.New("retire-removed-slots needs absolute -roster and -input-root paths")
	}
	if o.rosterSHA == "" {
		return errors.New("retire-removed-slots needs the approved -roster-sha256")
	}
	// 写进每份 RETIRE 的 deployment_id 是「sql-writer-slot-retirer/<StatefulSet>/<本 Job Pod 的 UID>」,上限 256 字节。
	if strings.TrimSpace(o.deploymentID) != o.deploymentID || o.deploymentID == "" || len(o.deploymentID) > 128 {
		return errors.New("retire-removed-slots needs -deployment-id, the UID of this Job pod (at most 128 bytes, no spaces): it is written into every RETIRE it sends")
	}
	if !kubernetesNamePattern.MatchString(o.statefulSet) {
		return fmt.Errorf("-statefulset %q is not a StatefulSet name", o.statefulSet)
	}
	return nil
}

// runRetireRemovedSlots:读名册 → 连这些槽位的写者闸门 → 读集群里这个 StatefulSet → 结束被缩掉槽位的在役一代。
func runRetireRemovedSlots(ctx context.Context, o retireRemovedSlotsOptions, getenv func(string) string, out io.Writer) error {
	root, err := resolveInputRoot(o.inputRoot)
	if err != nil {
		return err
	}
	physical, err := physicalAuthoritySHA(getenv)
	if err != nil {
		return err
	}
	roster, err := loadSlotRoster(ctx, o.rosterPath, o.rosterSHA, root, physical)
	if err != nil {
		return err
	}
	workload, err := inClusterWorkload(getenv)
	if err != nil {
		return err
	}
	opener, err := newCatalogOpener(ctx, "MMORPG_SQL_REGISTRAR_DSN", physical, getenv)
	if err != nil {
		return err
	}
	defer opener.close()
	retirer := slotRetirer{
		statefulSet:  o.statefulSet,
		namespace:    workload.namespace,
		deploymentID: "sql-writer-slot-retirer/" + o.statefulSet + "/" + o.deploymentID,
		open: func(request *dbpb.SqlWriterLifecycleReceipt_Request) (retirementCatalog, error) {
			return opener.open(request)
		},
		workload: workload,
		random:   rand.Reader,
	}
	report, runErr := retirer.run(ctx, roster)
	for _, line := range report.lines(o.statefulSet) {
		if _, err = fmt.Fprintln(out, line); err != nil {
			return errors.Join(runErr, err)
		}
	}
	return runErr
}

// retirementCatalog 是结束一代用到的目录操作,生产实现 *authoritysql.Catalog。
type retirementCatalog interface {
	LatestBoot(ctx context.Context, principal []byte) (*dbpb.SqlWriterRegistry, error)
	// Registration 回一代的 REGISTER 原件(永久收据里的完整请求),用它的 registered_by 判这一代是谁登记的。
	Registration(ctx context.Context, writer *dbpb.SqlWriterRegistry) (*dbpb.SqlWriterLifecycleReceipt_Request, error)
	CurrentMeta(ctx context.Context) (*dbpb.RouteMeta, error)
	Retire(ctx context.Context, request *dbpb.SqlWriterLifecycleReceipt_Request) (*dbpb.SqlWriterLifecycleReceipt, error)
}

// statefulSetState 是集群里写者 StatefulSet 的现状,生产实现 kubeWorkload。
type statefulSetState interface {
	// Replicas 回期望副本数(spec.replicas)。StatefulSet 不存在回 0:整个工作负载删了,它的槽位全都算被缩掉。
	Replicas(ctx context.Context, statefulSet string) (int, error)
	// PodExists 回这个名字的 Pod 对象在不在。正在删的(带 deletionTimestamp,还在优雅退出)也算在。
	PodExists(ctx context.Context, pod string) (bool, error)
	// ServiceAccountUID 回本 namespace 里这个 ServiceAccount 对象的 UID;对象不存在回 found=false。
	ServiceAccountUID(ctx context.Context, name string) (uid string, found bool, err error)
}

// slotRetirer 结束一个写者 StatefulSet 被缩掉的槽位上的在役一代。
type slotRetirer struct {
	statefulSet string
	// namespace 是本 Job 所在的 namespace,也就是这个 StatefulSet 所在的 namespace:只结束这里的工作负载登记的一代。
	namespace    string
	deploymentID string
	open         func(*dbpb.SqlWriterLifecycleReceipt_Request) (retirementCatalog, error)
	workload     statefulSetState
	random       io.Reader
}

// retirementCandidate 是目录快照里,一个被缩掉的槽位在一个逻辑库上的在役一代。
type retirementCandidate struct {
	ordinal  int
	template *dbpb.SqlWriterLifecycleReceipt_Request
	catalog  retirementCatalog
	writer   *dbpb.SqlWriterRegistry
}

// 一个候选的结局。只有 retired 写了目录;其余都没动它,下一轮按当时的目录与集群状态重新判。
const (
	slotRetired      = "retired"        // 非 run 的 RETIRE 落地:登记行 REVOKED、slot 行删掉、收据在
	slotAlreadyEnded = "already-ended"  // 退休被拒,而这一代已经不在役:别的事务先换了代或退休了
	slotPodPresent   = "pod-present"    // Pod 对象还在(多半在优雅退出),等它没了再结束,不打断它最后的写
	slotScaledBackUp = "scaled-back-up" // 读完目录之后副本数又涨回来了,这个序号的新 Pod 会换代
	slotGateFrozen   = "gate-frozen"    // 闸门冻结(搬库、排空),解冻之后的下一轮再来
	slotDrainRunOpen = "drain-run-open" // 排空 run 开着:非 run 的退休会被拒,排空要退休的写者按排空的流程走
	slotFailed       = "failed"         // 结局未知或别的错误:Job 失败,下一轮按目录状态重来
	// 这一代不是本 namespace 的工作负载登记的(另一个集群或 namespace 在用同一份名册,或者登记时不在 K8s 里):不动它,Job 失败,要人来看。
	slotRegisteredElsewhere = "registered-elsewhere"
)

// slotRetirementResult 是一个候选的结局,写进日志。
type slotRetirementResult struct {
	ordinal   int
	schema    string
	bootSeq   uint64
	requestID []byte
	outcome   string
}

// retirementReport 是一轮的结果:先后读到的副本数、扫过的槽位与每个候选的结局。
type retirementReport struct {
	replicasBefore, replicasAfter int
	slots                         int
	results                       []slotRetirementResult
}

// stillPending 判一个结局是不是「被缩掉、这一轮还没能结束的在役一代」:Pod 还在、闸门冻结、排空 run 开着、失败、不是本集群登记的。
// 已结束(retired、already-ended)和又扩回来的(scaled-back-up,新 Pod 会换代)都不算。
func stillPending(outcome string) bool {
	switch outcome {
	case slotRetired, slotAlreadyEnded, slotScaledBackUp:
		return false
	default:
		return true
	}
}

// lines 是写进日志的结果:每个候选一行,最后一行是汇总。汇总行的 pending 是部署脚本判「这一轮收尾做完没有」的依据
// (k8s_deploy.ps1 Complete-SqlWriterSlotRetirement 按这一行的格式解析,改格式要同批改脚本与契约测试)。
func (r retirementReport) lines(statefulSet string) []string {
	retired, pending := 0, 0
	lines := make([]string, 0, len(r.results)+1)
	for _, result := range r.results {
		line := fmt.Sprintf("writer_authority command=retire-removed-slots statefulset=%s slot=%d logical_db=%s boot_seq=%d result=%s", statefulSet, result.ordinal, result.schema, result.bootSeq, result.outcome)
		if len(result.requestID) != 0 {
			line += " request_id=" + hex.EncodeToString(result.requestID)
		}
		if result.outcome == slotRetired {
			retired++
		}
		if stillPending(result.outcome) {
			pending++
		}
		lines = append(lines, line)
	}
	return append(lines, fmt.Sprintf("writer_authority command=retire-removed-slots statefulset=%s replicas_before=%d replicas_after=%d roster_slots=%d candidates=%d retired=%d pending=%d",
		statefulSet, r.replicasBefore, r.replicasAfter, r.slots, len(r.results), retired, pending))
}

// run 结束被缩掉槽位的在役一代。顺序就是这条命令的安全性所在:
//  1. 读副本数,只扫序号 >= 它的槽位。这一步只缩小扫描范围,不做判定。
//  2. 读目录:这些槽位在每个逻辑库的最新一代,ACTIVE 的记为候选。
//  3. 目录读完之后再读一次副本数,再逐个看候选序号的 Pod 在不在。只结束「序号 >= 这次读到的副本数、Pod 对象已经没了」的候选。
//     第 2 步之后才建出来的 Pod 注册的是更新的一代,不在快照里,不会被结束;第 2、3 步之间扩回来的序号,这一步看得到,跳过。
//     第 3 步之后才扩回来的序号:新 Pod 的换代和这里的退休锁的是同一代的 slot 行,谁先谁后结局都对 —— 退休在先,
//     新 Pod 看到上一代已结束,只注册;换代在先,这里的退休读不到 slot 行,回拒绝,重读目录得知已结束。
//  4. 归属:Pod 没了的候选,读这一代的 REGISTER 原件,核它是本 namespace 的工作负载登记的(registeredHere)。不是就不动它、
//     这一轮算失败:「Pod 不在」只对本集群自己登记的一代才说明没有进程了(workload_identity.go)。
//  5. 结束:每个候选一份非 run 的 RETIRE,按当时的 meta 绑 epoch 与目录版本(retire)。
//
// 不靠时钟,也不在本地记任何状态:每一轮都从目录和集群的现状重新判,所以 Job 半途失败、重跑都不会多结束或漏结束。
func (r slotRetirer) run(ctx context.Context, roster *dbpb.SqlWriterBootRoster) (retirementReport, error) {
	report := retirementReport{slots: len(roster.Entries)}
	replicas, err := r.workload.Replicas(ctx, r.statefulSet)
	if err != nil {
		return report, fmt.Errorf("read the replicas of statefulset %s: %w", r.statefulSet, err)
	}
	report.replicasBefore, report.replicasAfter = replicas, replicas
	var candidates []retirementCandidate
	for ordinal := replicas; ordinal < len(roster.Entries); ordinal++ {
		for _, template := range roster.Entries[ordinal].Requests {
			catalog, err := r.open(template)
			if err != nil {
				return report, fmt.Errorf("open the writer gate of %s: %w", template.AuthoritySchema, err)
			}
			latest, err := catalog.LatestBoot(ctx, template.Writer.SqlPrincipal)
			if err != nil {
				return report, fmt.Errorf("read slot %d in %s: %w", ordinal, template.AuthoritySchema, err)
			}
			if latest != nil && latest.State == dbpb.SqlWriterRegistry_ACTIVE {
				candidates = append(candidates, retirementCandidate{ordinal: ordinal, template: template, catalog: catalog, writer: latest})
			}
		}
	}
	if len(candidates) == 0 {
		return report, nil
	}
	if replicas, err = r.workload.Replicas(ctx, r.statefulSet); err != nil {
		return report, fmt.Errorf("re-read the replicas of statefulset %s: %w", r.statefulSet, err)
	}
	report.replicasAfter = replicas
	podGone := map[int]bool{}
	serviceAccounts := map[string]string{}
	var failures []error
	for _, candidate := range candidates {
		result := slotRetirementResult{ordinal: candidate.ordinal, schema: candidate.template.AuthoritySchema, bootSeq: candidate.writer.BootSeq}
		if candidate.ordinal < replicas {
			result.outcome = slotScaledBackUp
			report.results = append(report.results, result)
			continue
		}
		gone, checked := podGone[candidate.ordinal]
		if !checked {
			exists, err := r.workload.PodExists(ctx, fmt.Sprintf("%s-%d", r.statefulSet, candidate.ordinal))
			if err != nil {
				return report, errors.Join(append(failures, fmt.Errorf("look up pod %s-%d: %w", r.statefulSet, candidate.ordinal, err))...)
			}
			gone = !exists
			podGone[candidate.ordinal] = gone
		}
		if !gone {
			result.outcome = slotPodPresent
			report.results = append(report.results, result)
			continue
		}
		if err = r.registeredHere(ctx, candidate, serviceAccounts); err != nil {
			result.outcome = slotFailed
			if errors.Is(err, errRegisteredElsewhere) {
				result.outcome = slotRegisteredElsewhere
			}
			failures = append(failures, fmt.Errorf("slot %d in %s, boot %d: %w", result.ordinal, result.schema, result.bootSeq, err))
			report.results = append(report.results, result)
			continue
		}
		result, err = r.retire(ctx, candidate, result)
		if err != nil {
			result.outcome = slotFailed
			failures = append(failures, err)
		}
		report.results = append(report.results, result)
	}
	return report, errors.Join(failures...)
}

// registeredHere 核一个候选是本 namespace 的工作负载登记的:它的 REGISTER 记着登记进程的 ServiceAccount 与 UID(registered_by),
// namespace 要是本 namespace,而且这里现在这个名字的 ServiceAccount 对象就是那个 UID。是就回 nil。
// 不是(别的 namespace、别的集群的 UID、这个 ServiceAccount 已经不在或被重建、集群外登记的空身份)回 errRegisteredElsewhere:
// 归属说不清就不结束,由人按手册核实后人工退休。读不到 REGISTER 或 API server 的错误原样回,同样不结束。
// uids 缓存这一轮查过的 ServiceAccount(名字 → UID,不存在记空串):一个 StatefulSet 的各代几乎都是同一个 ServiceAccount。
func (r slotRetirer) registeredHere(ctx context.Context, candidate retirementCandidate, uids map[string]string) error {
	registration, err := candidate.catalog.Registration(ctx, candidate.writer)
	if err != nil {
		return fmt.Errorf("read its REGISTER receipt: %w", err)
	}
	owner, ok := parseWorkloadIdentity(registration.RegisteredBy)
	if !ok || owner.namespace != r.namespace {
		return fmt.Errorf("%w (registered_by=%q, this namespace is %s); it is left active", errRegisteredElsewhere, registration.RegisteredBy, r.namespace)
	}
	uid, checked := uids[owner.serviceAccount]
	if !checked {
		current, found, err := r.workload.ServiceAccountUID(ctx, owner.serviceAccount)
		if err != nil {
			return fmt.Errorf("look up service account %s: %w", owner.serviceAccount, err)
		}
		if found {
			uid = current
		}
		uids[owner.serviceAccount] = uid
	}
	if uid != owner.uid {
		return fmt.Errorf("%w (registered_by=%q, service account %s/%s here has uid %q); it is left active", errRegisteredElsewhere, registration.RegisteredBy, r.namespace, owner.serviceAccount, uid)
	}
	return nil
}

// retire 结束一个候选。结局按目录状态判,不靠本地记录:
//   - RETIRE 落地:retired;
//   - 闸门冻结或排空 run 开着:这一轮不动它,下一轮再来;
//   - 被拒而这一代已经不在役(别的事务先换了代或退休了):already-ended,不算失败;
//   - 被拒而它还在役:多半是读 meta 之后目录版本被同库别的注册推进了,拿新的 meta 再试,试满还拒就报错;
//   - 结局未知或别的错误:报错,Job 失败。下一轮按目录状态重来,不会结束两次:落地过的话这一代已经不在役。
func (r slotRetirer) retire(ctx context.Context, candidate retirementCandidate, result slotRetirementResult) (slotRetirementResult, error) {
	for attempt := 1; attempt <= retireRemovedSlotsAttempts; attempt++ {
		meta, err := candidate.catalog.CurrentMeta(ctx)
		if err != nil {
			return result, fmt.Errorf("read the writer gate of %s: %w", result.schema, err)
		}
		if meta.Writable != 1 {
			result.outcome = slotGateFrozen
			return result, nil
		}
		if len(meta.FreezeRunId) != 0 {
			result.outcome = slotDrainRunOpen
			return result, nil
		}
		request, err := slotRetirementRequest(candidate.template, candidate.writer, meta, r.deploymentID, r.random)
		if err != nil {
			return result, err
		}
		result.requestID = request.RequestId
		receipt, err := candidate.catalog.Retire(ctx, request)
		switch {
		case err == nil:
			if err = confirmLifecycle(receipt, request); err != nil {
				return result, fmt.Errorf("retire slot %d in %s: %w", result.ordinal, result.schema, err)
			}
			result.outcome = slotRetired
			return result, nil
		case authoritysql.IsWriterFrozen(err):
			result.outcome = slotGateFrozen
			return result, nil
		case errors.Is(err, authoritysql.ErrOutcomeUnknown) || !errors.Is(err, authoritysql.ErrWriterDenied):
			return result, fmt.Errorf("retire slot %d in %s (request %x): %w", result.ordinal, result.schema, request.RequestId, err)
		}
		latest, err := candidate.catalog.LatestBoot(ctx, candidate.writer.SqlPrincipal)
		if err != nil {
			return result, fmt.Errorf("re-read slot %d in %s after a denied retirement: %w", result.ordinal, result.schema, err)
		}
		if latest == nil || !bytes.Equal(latest.WriterKey, candidate.writer.WriterKey) || latest.State != dbpb.SqlWriterRegistry_ACTIVE {
			result.outcome = slotAlreadyEnded
			return result, nil
		}
	}
	return result, fmt.Errorf("retire slot %d in %s: still denied after %d attempts while boot %d is active", result.ordinal, result.schema, retireRemovedSlotsAttempts, result.bootSeq)
}

// slotRetirementRequest 是结束一个槽位在役一代的非 run RETIRE:名册模板加上这一代的注册行,按当前 meta 绑 epoch 与目录版本。
// writer 原样是注册行(含 boot_seq),与人工退休「原样复制 REGISTER 收据里的 writer」同一形状;不带维护 run,也不带 successor。
func slotRetirementRequest(template *dbpb.SqlWriterLifecycleReceipt_Request, writer *dbpb.SqlWriterRegistry, meta *dbpb.RouteMeta, deploymentID string, random io.Reader) (*dbpb.SqlWriterLifecycleReceipt_Request, error) {
	id, err := randomID(random)
	if err != nil {
		return nil, err
	}
	request := proto.Clone(template).(*dbpb.SqlWriterLifecycleReceipt_Request)
	request.RequestId = id
	request.Kind = dbpb.SqlWriterLifecycleReceipt_RETIRE
	request.Writer = proto.Clone(writer).(*dbpb.SqlWriterRegistry)
	request.ExpectedEpoch = meta.WriterEpoch
	request.ExpectedCatalogRevision = meta.CatalogRevision
	request.DeploymentId = deploymentID
	request.MaintenanceRunId = nil
	request.SuccessorRequestId = nil
	return request, nil
}
