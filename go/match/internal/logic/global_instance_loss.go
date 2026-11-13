package logic

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"match/internal/authority"
	"match/internal/battlepods"
	"match/internal/metrics"
	"match/internal/svc"

	pb "proto/match"
	"shared/safego"

	"github.com/zeromicro/go-zero/core/logx"
)

// ---- 原执行进程失效的提前作废(2026-11-11,docs/design/battle-instance-loss-early-abort.md §4) ----
//
// battle 进程崩溃时,房间与冻结待提交的终局都只在它的内存里,这一局永远不会有 COMPLETE。以前要等到权威截止加宽限之后才由
// 截止裁决作废(长模板一个多小时),全员一直被冻着、排不了队,也收不到任何结局。现在:
//   - 触发:补签向原实例查询失败或回空包(globalRequestBattleTicket)、发房失败后没能证明原实例不会建房(globalAbortPrepared)时,
//     把这一局记进本副本的可疑表(instanceLossWatcher.Suspect)。参战者的客户端在直连断开后会立刻补签,在线玩家几秒内就触发。
//     可疑表在内存里:副本重启丢了由下一次补签(含重登后的重连提示)重新触发;没有人补签的局没有人在等,由截止裁决兜底。
//   - 取证:与截止作废同一套旁证(originalInstanceWitness),但只向原实例做只读查询(queryOriginalInstance)。结论是实例级的,
//     按实例复核(instanceLossTracker),同一个实例上的各局共用:一台 battle 崩溃时,它上面的几千局只需要两次取证。
//   - 提交:authority.AbortInstanceGone 逐局在锁内复读终局再写 ABORT;作废结算由 commitAbortTx 同一事务写给每名成员。
//
// 复核规则(唯一的时间判据,截止作废也经同一个记录器):一次观察说「原执行进程已不在」(instanceSeenGone)只是一次观察;
// 同一个实例要隔不少于 instanceGoneSettle 再观察到一次(两次不必同种),期间它没有亲口应答,才算确认。取值与理由见
// authority.InstanceGoneSettleMs。实例号每个进程生成一次、不会复用,所以确认之后不必再取证:同实例的其余各局直接作废。
const instanceGoneSettle = time.Duration(authority.InstanceGoneSettleMs) * time.Millisecond

const (
	// instanceLossFastProbe:刚进表、存储读写失败之后、以及复核间隔已过而这一次没问到时的重试节拍。
	instanceLossFastProbe = 5 * time.Second
	// instanceLossSlowProbe:对一个实例一直判不了时的取证间隔上限(从 instanceLossFastProbe 翻倍)。原 IP 是黑洞、
	// 又没配 Pod 查询时,这个实例上的局要等截止裁决,没必要每 5 秒问一次。
	instanceLossSlowProbe = 30 * time.Second
	// instanceLossBusyRetry:同一个实例正被另一局取证时,这一局隔多久再看(一次取证最长 authority.DeadlineProbeTimeout)。
	instanceLossBusyRetry = time.Second
	// instanceLossMaxSuspects:本副本可疑表上限。一台 battle 崩溃时它上面有人补签的每一局都会进表;满了新来的不进并计数,
	// 那些局由后续补签(腾出位置之后)或截止裁决收尾 —— 实例一旦确认不在,截止裁决对它的各局也不再取证。
	instanceLossMaxSuspects = 16384
	// instanceLossRoundBatch / instanceLossConcurrency:一批处理的到期局数与并发数。
	instanceLossRoundBatch  = 256
	instanceLossConcurrency = 16
	// instanceLossRoundBudget:一轮(一次节拍)最多连续处理多久。实例确认不在之后各局只剩一个作废事务,一轮内连着做完。
	instanceLossRoundBudget = 5 * time.Second
	// instanceLossStoreTimeout:一局的成局读与作废事务各自的期限。
	instanceLossStoreTimeout = 5 * time.Second
	// instanceLossRecordTTL:一个实例的复核记录多久没被问到就清掉。远大于各种取证节拍;清掉之后再有人问,从第一次观察重新计。
	instanceLossRecordTTL = 10 * time.Minute
	// battlePodAnswerTTL:同一 Pod IP 的查询结论复用多久。截止裁决一页并行取证时,同一台 battle 上的各局会同时问同一个 IP;
	// 复用 2 秒把 API server 的压力压到每个 IP 每 2 秒一次,又远短于复核间隔。
	battlePodAnswerTTL = 2 * time.Second
	// battlePodErrorLogInterval:Pod 查询失败(没权限、API server 不可用)的告警日志间隔。
	battlePodErrorLogInterval = time.Minute
)

// ---- 按实例的复核记录器 ----

// instanceLossVerdict 是记录器对一个实例的当前结论。
type instanceLossVerdict uint8

const (
	instanceVerdictUnknown  instanceLossVerdict = iota // 还没有任何结论
	instanceVerdictPending                             // 有过一次「已不在」的观察,等复核
	instanceVerdictAnswered                            // 这一次观察里原实例亲口应答:它还活着
	instanceVerdictGone                                // 确认已不在
)

// instanceProbePlan 是提前作废的一局对它所在实例下一步该做什么。
type instanceProbePlan uint8

const (
	instancePlanProbe instanceProbePlan = iota // 由这一局去取证(已占住这个实例的取证位,必须 finishProbe)
	instancePlanWait                           // 等到给出的时刻再看
	instancePlanAbort                          // 实例已确认不在,直接作废
)

// trackedInstance 是一个 battle 实例(进程)的复核状态。
type trackedInstance struct {
	firstGone time.Time     // 第一次「已不在」观察的时刻;零值 = 还没有
	gone      bool          // 已确认不在
	probing   bool          // 提前作废的某一局正在对它取证
	nextProbe time.Time     // 提前作废下一次可以取证的时刻
	interval  time.Duration // 判不了时的退避间隔
	touched   time.Time     // 最近一次被问到,用来清理
}

// instanceLossTracker 按实例号记「原执行进程已不在」的观察,把单次观察换成可以据以作废的结论。
// 截止作废(到期恢复、帮会取消)与提前作废都经它,所以只有一份复核规则。并发安全。
// 记录只在本副本内存里,时刻用本机单调钟:副本重启就从第一次观察重新计,只会晚一个复核间隔,不会提前。
type instanceLossTracker struct {
	now func() time.Time

	mu        sync.Mutex
	instances map[string]*trackedInstance
	lastPrune time.Time
}

func newInstanceLossTracker(now func() time.Time) *instanceLossTracker {
	return &instanceLossTracker{now: now, instances: map[string]*trackedInstance{}}
}

// entryLocked 取或建一个实例的记录并刷新它的访问时刻,顺带按 instanceLossRecordTTL 清理。调用方持锁,instance 非空。
func (t *instanceLossTracker) entryLocked(instance string, now time.Time) *trackedInstance {
	if now.Sub(t.lastPrune) >= time.Minute {
		t.lastPrune = now
		for id, e := range t.instances {
			if !e.probing && now.Sub(e.touched) >= instanceLossRecordTTL {
				delete(t.instances, id)
			}
		}
	}
	e, ok := t.instances[instance]
	if !ok {
		e = &trackedInstance{}
		t.instances[instance] = e
	}
	e.touched = now
	return e
}

// observeLocked 收下对一个实例的一次观察,返回它的当前结论。调用方持锁。
func (t *instanceLossTracker) observeLocked(instance string, e *trackedInstance, sighting instanceSighting, now time.Time) instanceLossVerdict {
	switch sighting {
	case instanceAnswered:
		if e.gone {
			// 不该发生:确认不在的实例又亲口应答了,说明两次观察都错了(例如回 RST 的防火墙加上丢了租约的存活进程)。
			// 撤回结论;已经据它作废的局不会恢复,值班要查这台 battle 的网络与租约。
			metrics.ObserveGlobalFlow("instance_loss", "confirmed_then_answered")
			logx.Errorf("battle instance %s answered after it was confirmed gone; the confirmation is withdrawn, battles already aborted on it stay void", instance)
		}
		e.gone, e.firstGone = false, time.Time{}
		return instanceVerdictAnswered
	case instanceSeenGone:
		switch {
		case e.gone:
		case e.firstGone.IsZero():
			e.firstGone = now
			metrics.ObserveGlobalFlow("instance_loss", "evidence_first")
		case now.Sub(e.firstGone) >= instanceGoneSettle:
			e.gone = true
			metrics.ObserveGlobalFlow("instance_loss", "instance_confirmed_gone")
			logx.Infof("battle instance %s confirmed gone: seen gone twice, %s apart, without an answer in between; its unfinished battles are aborted",
				instance, now.Sub(e.firstGone).Round(time.Second))
		}
	}
	switch {
	case e.gone:
		return instanceVerdictGone
	case !e.firstGone.IsZero():
		return instanceVerdictPending
	}
	return instanceVerdictUnknown
}

// gone 回报这个实例是否已确认不在。
func (t *instanceLossTracker) gone(instance string) bool {
	if instance == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.instances[instance]
	if ok {
		e.touched = t.now()
	}
	return ok && e.gone
}

// observe 收下对一个实例的一次观察,返回它的当前结论。没有实例号(坏数据)时不记,恒回 Unknown。
func (t *instanceLossTracker) observe(instance string, sighting instanceSighting) instanceLossVerdict {
	if instance == "" {
		return instanceVerdictUnknown
	}
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.observeLocked(instance, t.entryLocked(instance, now), sighting, now)
}

// plan 给提前作废的一局安排对它所在实例的下一步。回 instancePlanProbe 时已占住这个实例的取证位,调用方必须 finishProbe。
// instance 非空。
func (t *instanceLossTracker) plan(instance string) (instanceProbePlan, time.Time) {
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.entryLocked(instance, now)
	switch {
	case e.gone:
		return instancePlanAbort, time.Time{}
	case e.probing:
		return instancePlanWait, now.Add(instanceLossBusyRetry)
	case now.Before(e.nextProbe):
		return instancePlanWait, e.nextProbe
	}
	e.probing = true
	return instancePlanProbe, time.Time{}
}

// finishProbe 交回取证位、收下这一次观察,返回实例的当前结论与提前作废下一次可以取证的时刻:
//   - 等复核:排在第一次观察之后满 instanceGoneSettle;已满而这一次没问到就按快节拍再试;
//   - 没有结论:从 instanceLossFastProbe 起翻倍退避到 instanceLossSlowProbe;
//   - 原实例应答:不设等待(同实例的其余各局各问一次,问到就各自撤出)。
func (t *instanceLossTracker) finishProbe(instance string, sighting instanceSighting) (instanceLossVerdict, time.Time) {
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.entryLocked(instance, now)
	e.probing = false
	verdict := t.observeLocked(instance, e, sighting, now)
	switch verdict {
	case instanceVerdictPending:
		e.interval = 0
		e.nextProbe = e.firstGone.Add(instanceGoneSettle)
		if !now.Before(e.nextProbe) {
			e.nextProbe = now.Add(instanceLossFastProbe)
		}
	case instanceVerdictUnknown:
		if e.interval == 0 {
			e.interval = instanceLossFastProbe
		} else {
			e.interval = min(e.interval*2, instanceLossSlowProbe)
		}
		e.nextProbe = now.Add(e.interval)
	default:
		e.interval, e.nextProbe = 0, now
	}
	return verdict, e.nextProbe
}

// deadlineEvidence 把截止作废的单次取证包成可以据以裁决的证据:
//   - 实例已确认不在:不再取证,直接回 InstanceGone(一台 battle 崩溃时,它上面到期的各局不必逐局等取证期限);
//   - 这一次观察说不在:过了复核回 InstanceGone,否则回 InstanceGonePending(截止裁决据此把下一次取证排在复核间隔之后);
//   - 原实例亲口应答(仍持有房间 / 没有房间):清掉这个实例的「已不在」观察,证据原样返回。
func (t *instanceLossTracker) deadlineEvidence(probe authority.DeadlineAbortProbe) authority.DeadlineAbortProbe {
	return func(ctx context.Context, f *pb.MatchFormationRecord) authority.DeadlineAbortEvidence {
		instance := f.GetTargetInstanceId()
		if t.gone(instance) {
			return authority.DeadlineEvidenceInstanceGone
		}
		evidence := probe(ctx, f)
		switch evidence {
		case authority.DeadlineEvidenceInstanceGone:
			switch t.observe(instance, instanceSeenGone) {
			case instanceVerdictGone:
				return authority.DeadlineEvidenceInstanceGone
			case instanceVerdictPending:
				return authority.DeadlineEvidenceInstanceGonePending
			}
			return authority.DeadlineEvidenceUndetermined
		case authority.DeadlineEvidenceBattleStillHolds, authority.DeadlineEvidenceNoRoom:
			t.observe(instance, instanceAnswered)
		}
		return evidence
	}
}

// ---- Pod 查询 ----

// newBattlePodGone 返回「battle 所在命名空间里已没有存活的 Pod 持有这个地址的 IP」的查询。namespace 为空时回 nil(不查)。
// 进程跑在 K8s 里却没配命名空间时打一行 ERROR:常驻进程的配置来自运维预置的 runtimeConfigSecret,部署脚本渲染的 ConfigMap 只是
// 模板(docs/ops/k8s-open-server-runbook.md),这一项漏抄不会拒绝启动,但 Agones 下 battle 崩溃后的对局就只能等截止作废。
func newBattlePodGone(namespace string, now func() time.Time) func(context.Context, string) bool {
	if namespace == "" {
		if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
			logx.Error("battle pod lookup disabled: GlobalMatch.BattlePodNamespace is empty although this process runs inside Kubernetes; " +
				"battles on a crashed battle pod wait for their deadline. Copy the key from the rendered ConfigMap into the runtime config")
		}
		return nil
	}
	lookup := battlepods.InCluster(namespace)
	logx.Infof("battle pod lookup enabled: namespace=%s; a battle whose pod is gone or terminated counts as evidence that its process is gone", namespace)
	return newCachedPodGone(lookup.Gone, now)
}

// newCachedPodGone 给 Pod 查询加按 IP 的短时复用与失败日志限速。查询失败不复用、按判不了处理,每 battlePodErrorLogInterval
// 打一行 ERROR:没配 RBAC 时这里会一直失败,Pod 证据失效,只剩另两种。lookup 可被多个 goroutine 同时调用。
func newCachedPodGone(lookup func(context.Context, string) (bool, error), now func() time.Time) func(context.Context, string) bool {
	type answer struct {
		gone bool
		at   time.Time
	}
	var (
		mu         sync.Mutex
		answers    = map[string]answer{}
		lastErrLog time.Time
	)
	return func(ctx context.Context, endpoint string) bool {
		host, _, err := net.SplitHostPort(endpoint)
		if err != nil {
			return false
		}
		mu.Lock()
		cached, ok := answers[host]
		mu.Unlock()
		if ok && now().Sub(cached.at) < battlePodAnswerTTL {
			return cached.gone
		}
		gone, err := lookup(ctx, endpoint)
		at := now()
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			if !errors.Is(err, battlepods.ErrNotConfigured) && at.Sub(lastErrLog) >= battlePodErrorLogInterval {
				lastErrLog = at
				logx.Errorf("battle pod lookup failed; the pod-gone evidence is unavailable until it recovers: %v", err)
			}
			return false
		}
		for ip, old := range answers {
			if at.Sub(old.at) >= battlePodAnswerTTL {
				delete(answers, ip)
			}
		}
		answers[host] = answer{gone: gone, at: at}
		return gone
	}
}

// ---- 可疑表与取证循环 ----

// instanceLossStore 是提前作废对成局权威的全部依赖,生产实现是 *authority.Store;收窄成接口便于替身测试。
type instanceLossStore interface {
	InstanceLossCandidate(ctx context.Context, battleID uint64) (*pb.MatchFormationRecord, bool, error)
	AbortInstanceGone(ctx context.Context, f *pb.MatchFormationRecord, proof authority.InstanceGoneProof) (pb.BattleOutcomeDecision, error)
}

// instanceLossWatcher 是本副本的可疑表与取证循环,实现 svc.BattleInstanceLossWatcher。
// Suspect 可被任意 goroutine 调用;Round 由 StartRelayLoop 串行调用(同一时刻只有一轮),一轮内的各局并行处理。
type instanceLossWatcher struct {
	store   instanceLossStore
	observe func(context.Context, *pb.MatchFormationRecord) instanceSighting // 只读查询的一次观察
	tracker *instanceLossTracker
	hold    func(error) // 作废事务失败(提交结果可能未知)记成排空 HOLD(svc.ServiceContext.HoldRelay)
	now     func() time.Time
	// running:取证循环至少跑过一轮。没起循环的进程里 Suspect 什么都不做,免得表只进不出。
	running atomic.Bool

	mu       sync.Mutex
	suspects map[uint64]*instanceLossSuspect
}

type instanceLossSuspect struct {
	due  time.Time // 下一次处理的时刻
	busy bool      // 正被某一批处理
}

func newInstanceLossWatcher(store instanceLossStore, observe func(context.Context, *pb.MatchFormationRecord) instanceSighting, tracker *instanceLossTracker, hold func(error), now func() time.Time) *instanceLossWatcher {
	return &instanceLossWatcher{store: store, observe: observe, tracker: tracker, hold: hold, now: now, suspects: map[uint64]*instanceLossSuspect{}}
}

// Suspect 把一局记成可疑,下一轮就处理。不阻塞、不做 IO;已在表里、表已满、或本进程没起取证循环时什么都不做。
func (w *instanceLossWatcher) Suspect(battleID uint64) {
	if w == nil || battleID == 0 || !w.running.Load() {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.suspects[battleID]; ok {
		return
	}
	if len(w.suspects) >= instanceLossMaxSuspects {
		metrics.ObserveGlobalFlow("instance_loss", "suspect_rejected_full")
		return
	}
	w.suspects[battleID] = &instanceLossSuspect{due: w.now()}
	metrics.ObserveGlobalFlow("instance_loss", "suspected")
}

// Round 处理到期的可疑局:一批至多 instanceLossRoundBatch 局、并发 instanceLossConcurrency,在 instanceLossRoundBudget 之内
// 连续处理到没有到期的为止。每局自带期限。单局 panic 不拖垮整轮:那一局按快节拍再试,panic 由 safego 记录。
func (w *instanceLossWatcher) Round(ctx context.Context) {
	w.running.Store(true)
	budgetEnds := w.now().Add(instanceLossRoundBudget)
	for {
		due := w.takeDue()
		if len(due) == 0 {
			return
		}
		w.runBatch(ctx, due)
		if !w.now().Before(budgetEnds) {
			return
		}
	}
}

// runBatch 并行处理一批到期的局,全部结束才返回。
func (w *instanceLossWatcher) runBatch(ctx context.Context, due []uint64) {
	slots := make(chan struct{}, instanceLossConcurrency)
	var wg sync.WaitGroup
	for _, battleID := range due {
		slots <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			release, next := false, w.now().Add(instanceLossFastProbe)
			safego.Run("match.battle_instance_loss.check", func() { release, next = w.check(ctx, battleID) })
			w.finish(battleID, release, next)
		}()
	}
	wg.Wait()
}

// takeDue 取出到期且没在处理的局,标成处理中。
func (w *instanceLossWatcher) takeDue() []uint64 {
	now := w.now()
	w.mu.Lock()
	defer w.mu.Unlock()
	var due []uint64
	for battleID, s := range w.suspects {
		if s.busy || now.Before(s.due) {
			continue
		}
		s.busy = true
		due = append(due, battleID)
		if len(due) >= instanceLossRoundBatch {
			break
		}
	}
	return due
}

// finish 按一局的处理结果撤出可疑表,或排下一次。
func (w *instanceLossWatcher) finish(battleID uint64, release bool, next time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	s, ok := w.suspects[battleID]
	if !ok {
		return
	}
	if release {
		delete(w.suspects, battleID)
		return
	}
	s.busy, s.due = false, next
}

// check 处理一局一次:读成局 → 看它所在实例该做什么 → 轮到这一局就取证 → 实例确认不在就作废。
// 返回是否撤出可疑表;不撤出时 next 是下一次处理的时刻。
func (w *instanceLossWatcher) check(ctx context.Context, battleID uint64) (release bool, next time.Time) {
	readCtx, cancelRead := context.WithTimeout(ctx, instanceLossStoreTimeout)
	f, pending, err := w.store.InstanceLossCandidate(readCtx, battleID)
	cancelRead()
	if err != nil {
		// 只读失败没有任何副作用:不记排空 HOLD(那是给「写入结果未知」用的,见 abort),按快节拍再读。
		metrics.ObserveGlobalFlow("instance_loss", "store_error")
		return false, w.now().Add(instanceLossFastProbe)
	}
	instance := f.GetTargetInstanceId()
	if !pending || instance == "" {
		// 已有终局、成局不存在、原件不归自动路径,或成局没有实例号(坏数据,交给截止裁决与人工处置)。
		metrics.ObserveGlobalFlow("instance_loss", "released")
		return true, time.Time{}
	}
	plan, until := w.tracker.plan(instance)
	if plan == instancePlanWait {
		return false, until
	}
	verdict := instanceVerdictGone
	if plan == instancePlanProbe {
		verdict, until = w.probe(ctx, f)
	}
	switch verdict {
	case instanceVerdictGone:
		return w.abort(ctx, f)
	case instanceVerdictAnswered:
		// 原实例还活着:这一局不归提前作废。它之后再出事,下一次补签会重新记可疑。
		metrics.ObserveGlobalFlow("instance_loss", "answered")
		return true, time.Time{}
	}
	return false, until
}

// probe 对这一局所在的实例取一次证(调用方已经用 plan 占住取证位),把结果交回记录器。取证 panic 时也交回取证位
// (按判不了记),panic 继续向上由 safego 记录。
func (w *instanceLossWatcher) probe(ctx context.Context, f *pb.MatchFormationRecord) (instanceLossVerdict, time.Time) {
	instance := f.GetTargetInstanceId()
	finished := false
	defer func() {
		if !finished {
			w.tracker.finishProbe(instance, instanceUndetermined)
		}
	}()
	probeCtx, cancel := context.WithTimeout(ctx, authority.DeadlineProbeTimeout)
	defer cancel()
	sighting := w.observe(probeCtx, f)
	finished = true
	return w.tracker.finishProbe(instance, sighting)
}

// abort 在实例确认不在之后作废这一局。已有终局(另一条路径先写了)、原件不归自动路径都算处理完,撤出可疑表。
func (w *instanceLossWatcher) abort(ctx context.Context, f *pb.MatchFormationRecord) (release bool, next time.Time) {
	abortCtx, cancel := context.WithTimeout(ctx, instanceLossStoreTimeout)
	decision, err := w.store.AbortInstanceGone(abortCtx, f, authority.InstanceGoneProof{OperationId: f.GetOperationId(), TargetInstanceId: f.GetTargetInstanceId()})
	cancel()
	if err != nil {
		// 提交结果可能未知:记排空 HOLD,这一局留在表里。下一次复核在锁内重读终局,已经落库就照它收尾,不会写第二份。
		w.hold(err)
		metrics.ObserveGlobalFlow("instance_loss", "store_error")
		logx.Errorf("battle early abort unconfirmed; the next check re-reads the terminal: battle_id=%d instance=%s: %v",
			f.GetBattleId(), f.GetTargetInstanceId(), err)
		return false, w.now().Add(instanceLossFastProbe)
	}
	if decision == pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT {
		logx.Infof("battle aborted early because its executing instance is gone: battle_id=%d instance=%s endpoint=%s",
			f.GetBattleId(), f.GetTargetInstanceId(), f.GetTargetEndpoint())
	}
	metrics.ObserveGlobalFlow("instance_loss", "resolved")
	return true, time.Time{}
}

// StartBattleInstanceLossWatcher 启动提前作废的取证循环(每秒一轮,StartRelayLoop 管生命周期与排空)。
// stable 与 canary 副本都起:可疑表只由本副本收到的补签驱动,是随请求走的数据面工作,不是按片分工的全局巡检;
// 全员金丝雀的对局只会向 canary 副本补签,不起就没有副本替它们提前作废。
func StartBattleInstanceLossWatcher(ctx context.Context, s *svc.ServiceContext) {
	if s.BattleInstanceLoss == nil {
		return
	}
	s.StartRelayLoop(ctx, "match.battle_instance_loss", time.Second, s.BattleInstanceLoss.Round)
}

// suspectBattleInstanceLoss 把一局记成可疑(没装配提前作废时什么都不做)。只在问不到原实例时调,见文件头的触发。
func suspectBattleInstanceLoss(s *svc.ServiceContext, battleID uint64) {
	if s != nil && s.BattleInstanceLoss != nil {
		s.BattleInstanceLoss.Suspect(battleID)
	}
}
