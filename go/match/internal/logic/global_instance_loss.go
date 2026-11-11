package logic

import (
	"context"
	"errors"
	"net"
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
//   - 触发:补签向原实例查询失败或回空包(globalRequestBattleTicket)、发房失败后问不到原实例(globalAbortPrepared)时,
//     把这一局记进本副本的可疑表(instanceLossWatcher.Suspect)。参战者的客户端在直连断开后会立刻补签,在线玩家几秒内就触发。
//     可疑表在内存里:副本重启丢了由下一次补签(含重登后的重连提示)重新触发;没有人补签的局没有人在等,由截止裁决兜底。
//   - 取证:与截止作废同一次观察(deadlineEvidenceProbe)、同一个复核记录器(instanceLossTracker),见下面的复核规则。
//   - 提交:authority.AbortInstanceGone 锁内复读终局再写 ABORT;作废结算由 commitAbortTx 同一事务写给每名成员。
//   - 过了权威截止加宽限的局交还截止裁决(InstanceLossCandidate 回 pending=false),它用同一份观察与同一条复核规则。
//
// 复核规则(唯一的时间判据):一次观察说「原执行进程已不在」(authority.DeadlineEvidenceInstanceGone:原地址上是另一个实例 /
// 原地址拒连且已不在册 / K8s 里原 IP 已没有 Pod)只是一次观察;同一局要隔不少于 instanceGoneSettle 再观察到一次(两次不必同种),
// 期间原实例没亲口回过 PRESENT 或 ABORTED,才算确认。
// instanceGoneSettle 必须大于 battle 停机排空上限(node.cpp kShutdownDrainBudget,15 秒;排空期间它还会补提交终局)加一次
// CommitBattleOutcome 调用的期限:第一次观察时原进程至少已不在原地址上应答,最多再排空 15 秒就装停止闸、不再提交;过了这段,
// 它发出的最后一次提交要么已经落库(作废事务读到终局行、什么都不做),要么已经失败。Pod 已不在时同理覆盖「死前刚发出、还在路上
// 的提交」。TestInstanceGoneSettleCoversBattleShutdownDrain 读 C++ 常量核对。
const instanceGoneSettle = 30 * time.Second

const (
	// instanceLossFastProbe:有一次「已不在」的观察、在等复核间隔时的取证节拍;也是刚进表时的节拍。
	instanceLossFastProbe = 5 * time.Second
	// instanceLossSlowProbe:判不了时退避的上限(从 instanceLossFastProbe 翻倍)。原 IP 是黑洞、又没配 Pod 查询时,
	// 这一局要等截止裁决,没必要每 5 秒问一次。
	instanceLossSlowProbe = 30 * time.Second
	// instanceLossMaxSuspects:本副本可疑表上限。一台 battle 崩溃时它上面的每一局都会进表;满了新来的不进(截止裁决兜底)并计数。
	instanceLossMaxSuspects = 4096
	// instanceLossRoundBatch / instanceLossConcurrency:一轮最多处理的到期局数与并发数。一次取证最长等
	// authority.DeadlineProbeTimeout,整台 battle 失联时逐局串行会让一轮拖上几分钟。
	instanceLossRoundBatch  = 256
	instanceLossConcurrency = 16
	// instanceLossStoreTimeout:一局的成局读与作废事务各自的期限。
	instanceLossStoreTimeout = 5 * time.Second
	// instanceLossSightingTTL:复核记录多久没再被观察就清掉(局已终结、副本不再负责它)。远大于两种取证节拍。
	instanceLossSightingTTL = 10 * time.Minute
	// battlePodAnswerTTL:同一 Pod IP 的查询结论复用多久。整台 battle 崩溃时上面的每一局都会问同一个 IP;
	// 复用 2 秒把 API server 的压力压到每个 IP 每 2 秒一次,又远短于复核间隔,不会让证据明显迟到。
	battlePodAnswerTTL = 2 * time.Second
	// battlePodErrorLogInterval:Pod 查询失败(没权限、API server 不可用)的告警日志间隔。
	battlePodErrorLogInterval = time.Minute
)

// ---- 复核记录器 ----

// instanceLossTracker 记每一局第一次「已不在」观察的时刻(本副本的单调钟),把单次观察换成可以据以作废的结论。
// 截止作废(到期恢复、帮会取消)与提前作废都经它,所以两条路径只有一份复核规则。并发安全。
// 记录只在本副本内存里:副本重启或换了副本负责,就从第一次观察重新计,只会晚一个复核间隔,不会提前。
type instanceLossTracker struct {
	now func() time.Time

	mu        sync.Mutex
	sightings map[uint64]instanceGoneSighting
	lastPrune time.Time
}

type instanceGoneSighting struct {
	first time.Time // 第一次「已不在」观察
	last  time.Time // 最近一次观察(不论结论),用来清理不再有人问的记录
}

func newInstanceLossTracker(now func() time.Time) *instanceLossTracker {
	return &instanceLossTracker{now: now, sightings: map[uint64]instanceGoneSighting{}}
}

// observe 收下一局的一次观察,返回可以据以作废的结论:
//   - InstanceGone:第一次只记时刻、回 Undetermined;距第一次不少于 instanceGoneSettle 时回 InstanceGone;
//   - BattleStillHolds / NoRoom:原实例亲口作答,清掉这一局的记录,原样返回;
//   - Undetermined:原样返回,不清记录(原 IP 变成黑洞时这一次问不到,不说明原进程回来了)。
func (t *instanceLossTracker) observe(battleID uint64, evidence authority.DeadlineAbortEvidence) authority.DeadlineAbortEvidence {
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked(now)
	switch evidence {
	case authority.DeadlineEvidenceInstanceGone:
		seen, ok := t.sightings[battleID]
		if !ok {
			t.sightings[battleID] = instanceGoneSighting{first: now, last: now}
			metrics.ObserveGlobalFlow("instance_loss", "evidence_first")
			return authority.DeadlineEvidenceUndetermined
		}
		seen.last = now
		t.sightings[battleID] = seen
		if now.Sub(seen.first) < instanceGoneSettle {
			return authority.DeadlineEvidenceUndetermined
		}
		return authority.DeadlineEvidenceInstanceGone
	case authority.DeadlineEvidenceBattleStillHolds, authority.DeadlineEvidenceNoRoom:
		delete(t.sightings, battleID)
		return evidence
	default:
		if seen, ok := t.sightings[battleID]; ok {
			seen.last = now
			t.sightings[battleID] = seen
		}
		return authority.DeadlineEvidenceUndetermined
	}
}

// pending 回报这一局是否已有一次「已不在」观察、还在等复核间隔(可疑表据此保持快节拍)。
func (t *instanceLossTracker) pending(battleID uint64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.sightings[battleID]
	return ok
}

// forget 清掉一局的记录(已写出终局之后)。
func (t *instanceLossTracker) forget(battleID uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.sightings, battleID)
}

// pruneLocked 每分钟至多一次清掉 instanceLossSightingTTL 内没再被观察的记录。调用方持锁。
func (t *instanceLossTracker) pruneLocked(now time.Time) {
	if now.Sub(t.lastPrune) < time.Minute {
		return
	}
	t.lastPrune = now
	for id, seen := range t.sightings {
		if now.Sub(seen.last) >= instanceLossSightingTTL {
			delete(t.sightings, id)
		}
	}
}

// confirmed 把单次观察的取证包成截止裁决用的取证:InstanceGone 先过复核。
func (t *instanceLossTracker) confirmed(probe authority.DeadlineAbortProbe) authority.DeadlineAbortProbe {
	return func(ctx context.Context, f *pb.MatchFormationRecord) authority.DeadlineAbortEvidence {
		return t.observe(f.GetBattleId(), probe(ctx, f))
	}
}

// ---- Pod 查询 ----

// newBattlePodGone 返回「battle 所在命名空间里已没有 Pod 持有这个地址的 IP」的查询(E3)。namespace 为空时恒为假、不发请求。
// 同一个 IP 的结论复用 battlePodAnswerTTL;查询失败不复用、按判不了处理,每 battlePodErrorLogInterval 打一行 ERROR
// (没配 RBAC 时这里会一直失败,E3 失效,只剩另两种证据)。
func newBattlePodGone(namespace string, now func() time.Time) func(context.Context, string) bool {
	if namespace == "" {
		return func(context.Context, string) bool { return false }
	}
	lookup := battlepods.InCluster(namespace)
	return newCachedPodGone(func(ctx context.Context, endpoint string) (bool, error) { return lookup.Gone(ctx, endpoint) }, now)
}

// newCachedPodGone 给 Pod 查询加按 IP 的短时复用与失败日志限速。lookup 可被多个 goroutine 同时调用。
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
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			if !errors.Is(err, battlepods.ErrNotConfigured) && now().Sub(lastErrLog) >= battlePodErrorLogInterval {
				lastErrLog = now()
				logx.Errorf("battle pod lookup failed; the pod-gone evidence is unavailable until it recovers: %v", err)
			}
			return false
		}
		at := now()
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
	probe   authority.DeadlineAbortProbe // 单次观察,未过复核
	tracker *instanceLossTracker
	hold    func(error) // 存储失败记成排空 HOLD(svc.ServiceContext.HoldRelay)
	now     func() time.Time
	// running:取证循环至少跑过一轮。没跑起来的副本(canary 不起循环)上 Suspect 什么都不做,免得表只进不出。
	running atomic.Bool

	mu       sync.Mutex
	suspects map[uint64]*instanceLossSuspect
}

type instanceLossSuspect struct {
	due      time.Time
	interval time.Duration
	busy     bool // 正被某一轮处理;处理完由 finish 排下一次
}

// instanceLossVerdict 是一局一次处理的结论。
type instanceLossVerdict uint8

const (
	instanceLossKeep    instanceLossVerdict = iota // 继续观察,按节拍再问
	instanceLossRelease                            // 撤出可疑表:已有终局、原实例作答、已归截止裁决、已作废或成局不存在
)

func newInstanceLossWatcher(store instanceLossStore, probe authority.DeadlineAbortProbe, tracker *instanceLossTracker, hold func(error), now func() time.Time) *instanceLossWatcher {
	return &instanceLossWatcher{store: store, probe: probe, tracker: tracker, hold: hold, now: now, suspects: map[uint64]*instanceLossSuspect{}}
}

// Suspect 把一局记成可疑,下一轮就处理。不阻塞、不做 IO;已在表里、表已满、或本副本没跑取证循环时什么都不做。
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
	w.suspects[battleID] = &instanceLossSuspect{due: w.now(), interval: instanceLossFastProbe}
	metrics.ObserveGlobalFlow("instance_loss", "suspected")
}

// Round 处理一轮到期的可疑局:至多 instanceLossRoundBatch 局、并发 instanceLossConcurrency,每局自带期限。
// 单局 panic 不拖垮整轮:那一局按继续观察处理,panic 由 safego 记录。
func (w *instanceLossWatcher) Round(ctx context.Context) {
	w.running.Store(true)
	due := w.takeDue()
	if len(due) == 0 {
		return
	}
	slots := make(chan struct{}, instanceLossConcurrency)
	var wg sync.WaitGroup
	for _, battleID := range due {
		slots <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			verdict := instanceLossKeep
			safego.Run("match.battle_instance_loss.check", func() { verdict = w.check(ctx, battleID) })
			w.finish(battleID, verdict)
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

// finish 按一局的结论撤出或排下一次:还在等复核间隔的局保持快节拍,判不了的翻倍退避到 instanceLossSlowProbe。
func (w *instanceLossWatcher) finish(battleID uint64, verdict instanceLossVerdict) {
	pending := w.tracker.pending(battleID)
	w.mu.Lock()
	defer w.mu.Unlock()
	s, ok := w.suspects[battleID]
	if !ok {
		return
	}
	if verdict == instanceLossRelease {
		delete(w.suspects, battleID)
		return
	}
	s.busy = false
	if pending {
		s.interval = instanceLossFastProbe
	} else {
		s.interval = min(s.interval*2, instanceLossSlowProbe)
	}
	s.due = w.now().Add(s.interval)
}

// check 处理一局一次:读成局 → 取一次观察 → 过复核 → 确认原进程已不在就锁内写 ABORT。
func (w *instanceLossWatcher) check(ctx context.Context, battleID uint64) instanceLossVerdict {
	readCtx, cancelRead := context.WithTimeout(ctx, instanceLossStoreTimeout)
	f, pending, err := w.store.InstanceLossCandidate(readCtx, battleID)
	cancelRead()
	if err != nil {
		w.hold(err)
		metrics.ObserveGlobalFlow("instance_loss", "store_error")
		return instanceLossKeep
	}
	if !pending {
		// 已有终局、成局不存在、原件不归自动路径,或已过截止加宽限(归截止裁决,它与这里共用复核记录,不清记录)。
		metrics.ObserveGlobalFlow("instance_loss", "released")
		return instanceLossRelease
	}
	probeCtx, cancelProbe := context.WithTimeout(ctx, authority.DeadlineProbeTimeout)
	sighting := w.probe(probeCtx, f)
	cancelProbe()
	switch w.tracker.observe(battleID, sighting) {
	case authority.DeadlineEvidenceInstanceGone:
		abortCtx, cancelAbort := context.WithTimeout(ctx, instanceLossStoreTimeout)
		decision, err := w.store.AbortInstanceGone(abortCtx, f, authority.InstanceGoneProof{OperationId: f.GetOperationId(), TargetInstanceId: f.GetTargetInstanceId()})
		cancelAbort()
		if err != nil {
			w.hold(err)
			metrics.ObserveGlobalFlow("instance_loss", "store_error")
			return instanceLossKeep
		}
		w.tracker.forget(battleID)
		if decision == pb.BattleOutcomeDecision_BATTLE_OUTCOME_DECISION_ABORT {
			logx.Infof("battle executing instance confirmed gone; formation aborted early: battle_id=%d instance=%s endpoint=%s",
				battleID, f.GetTargetInstanceId(), f.GetTargetEndpoint())
		}
		metrics.ObserveGlobalFlow("instance_loss", "resolved")
		return instanceLossRelease
	case authority.DeadlineEvidenceBattleStillHolds, authority.DeadlineEvidenceNoRoom:
		// 原实例亲口作答:它还在(PRESENT),或证明没建房(NoRoom,归发房前收尾与截止裁决)。
		metrics.ObserveGlobalFlow("instance_loss", "answered")
		return instanceLossRelease
	}
	return instanceLossKeep
}

// StartBattleInstanceLossWatcher 在跑后台任务的副本上启动提前作废的取证循环(每秒一轮,StartRelayLoop 管生命周期与排空)。
// canary 副本不起:撮合、恢复、巡检都由 stable 承担(发布轨道设计 §2.5);canary 上的补签不记可疑,那些局由截止裁决兜底。
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
