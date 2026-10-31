package logic

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"

	"player_locator/internal/config"
	"player_locator/internal/svc"
	pb "proto/player_locator"
	"shared/redisslot"
	"shared/safego"
)

// 死 gate 清扫:每个副本只扫分给自己的 slot(slot_shard.go),不需要选主。
//
// 每拍读自己那些 slot 的 gate 登记集和 etcd 存活集,连续两拍缺席的 gate 确认死亡,把它在这些 slot 上的索引逐页处理:
// 仍在它上面 ONLINE 的会话走与 SetDisconnecting 相同的提交点转成 DISCONNECTING,其余按字节守卫摘成员。
// 两个副本短暂扫同一个 slot 无害:所有动作都是整字节 CAS 或字节守卫脚本。
//
// 副本看不清 etcd 或 Redis(连续 3 拍失败,或 3 拍内没有一次成功)就让出它的段(eligible=false,分片随后释放段、撤销成员
// 登记),其余副本接手。让出之后每拍照常试一次,成功了再回来。全部副本都看不清时没人清扫,与「fail-closed:宁可不清理」一致。

// gateSweeper 是一个副本上的清扫状态。suspects / cursors 只由 run 所在的 goroutine 访问。
type gateSweeper struct {
	svcCtx   *svc.ServiceContext
	rdb      redis.UniversalClient
	owner    slotOwner
	cfg      config.ReconcileConf
	interval time.Duration
	deps     reconcilerDeps
	// transitionPacer 是本副本的转换限速;memberBudget 是本副本一拍最多检查的死 gate 索引成员数
	// (DeadGateMembersPerRound,再被转换限速 × 半拍截住)。都按副本计。
	transitionPacer pacer
	memberBudget    int

	suspects map[string]int      // gate uuid → 连续缺席拍数(封顶 2)
	cursors  map[gateSlot]uint64 // 已确认死亡、被预算截断的索引键 → 下一拍续扫的 SSCAN 游标

	// eligible 为 false 时本副本退出清扫分片(slot_shard 的 healthy)。
	eligible atomic.Bool
}

func newGateSweeper(svcCtx *svc.ServiceContext, owner slotOwner, cfg config.ReconcileConf, interval time.Duration, deps reconcilerDeps) *gateSweeper {
	budget := cfg.DeadGateMembersPerRound
	if cfg.MaxTransitionsPerSecond > 0 {
		// 限速调低时,一拍的转换耗时不超过半个拍长,拍不会因为在限速里排队而超时(一拍的 ctx 超时就是一个拍长)。
		// 代价:大面积 gate 故障时每个副本的**持续**转换速率约为 MaxTransitionsPerSecond / 2(每拍只用半拍去转换),
		// 估恢复时间按「持段副本数 × 这个值」算(设计文档 §7)。不放宽到更大比例:超时的拍计入让位门槛。
		capped := int(float64(cfg.MaxTransitionsPerSecond) * interval.Seconds() / 2)
		budget = max(min(budget, capped), 1)
	}
	s := &gateSweeper{
		svcCtx:          svcCtx,
		rdb:             svcCtx.PlayerRedis,
		owner:           owner,
		cfg:             cfg,
		interval:        interval,
		deps:            deps,
		transitionPacer: deps.newPacer(cfg.MaxTransitionsPerSecond),
		memberBudget:    budget,
		suspects:        make(map[string]int),
		cursors:         make(map[gateSlot]uint64),
	}
	s.eligible.Store(true)
	return s
}

// run 按拍清扫,直到 ctx 结束。
func (s *gateSweeper) run(ctx context.Context) {
	wait, stop := s.deps.newTicker(s.interval)
	defer stop()
	lastSuccess := s.deps.now() // 启动时刻作为「上次成功」的基线
	failures := 0
	for {
		outcome := s.runIndexRound(ctx)
		if ctx.Err() != nil {
			return // 关停造成的取消不计数
		}
		now := s.deps.now()
		reconcileRoundTotal.Inc(reconcileLoopIndex, outcome.result())
		if outcome.err == nil {
			failures = 0
			lastSuccess = now
			reconcileLastSuccessUnix.Set(float64(now.Unix()), reconcileLoopIndex)
			if s.eligible.CompareAndSwap(false, true) {
				logx.Info("GateSweeper: round succeeded, rejoining the sweep shard")
			}
		} else {
			failures++
			logx.Errorf("GateSweeper: index round failed stage=%s consecutive_failures=%d: %v", outcome.stage, failures, outcome.err)
		}
		if s.eligible.Load() && (failures >= maxConsecutiveRoundFailures || now.Sub(lastSuccess) > staleSuccessIntervals*s.interval) {
			s.eligible.Store(false)
			logx.Errorf("GateSweeper: leaving the sweep shard, other replicas take over: consecutive_failures=%d since_last_success=%v last_stage=%s",
				failures, now.Sub(lastSuccess), outcome.stage)
		}
		if err := wait(ctx); err != nil {
			return
		}
	}
}

// runIndexRound 跑一拍,recover 收在这一拍:对账本身就是兜底链路,一条畸形数据 panic 不能让它永久停摆。
func (s *gateSweeper) runIndexRound(ctx context.Context) roundOutcome {
	out := roundOutcome{stage: stagePanic, err: errors.New("index round panicked")}
	safego.Run("player_locator.session_reconciler.round", func() {
		roundCtx, cancel := context.WithTimeout(ctx, s.interval)
		defer cancel()
		out = s.indexRound(roundCtx)
	})
	return out
}

// indexRound 是一拍,顺序固定:登记集快照 → etcd 存活集 → 推进去抖 → 预算内处理死 gate → 刷 gauge。
func (s *gateSweeper) indexRound(ctx context.Context) roundOutcome {
	rep := indexRoundReport{actions: map[string]int{}, keyErrors: keyErrorTally{limit: maxKeyTypeErrorsPerRound}}

	// 没分到段:没有登记集要读。已让出段的副本照常往下走,靠这一拍的读来判断能不能回去。
	slots := s.owner.OwnedSlots()
	if len(slots) == 0 && s.eligible.Load() {
		s.refreshIndexGauges(nil, nil, 0, &rep)
		return roundOutcome{report: rep}
	}

	// a) 登记集快照必须先于 b 的 etcd 读:gate 先在 etcd 注册,login 才会把玩家分给它,
	//    所以快照里的每个 gate 都早于下面那次 Range 注册过,活着就一定在存活集里。
	pairs, err := readGateRegistries(ctx, s.rdb, slots, rep.actions, &rep.keyErrors)
	if err != nil {
		return roundOutcome{stage: stageRedis, err: fmt.Errorf("read gate index registries: %w", err), report: rep}
	}

	// b) 看不清存活集就不推进去抖、不转换(fail-closed)。
	live, err := s.deps.view.LiveGateInstances(ctx)
	if err != nil {
		return roundOutcome{stage: stageEtcd, err: fmt.Errorf("list live gates: %w", err), report: rep}
	}

	// c)
	dead, deadPairs, livePairs := s.advanceSuspects(pairs, live)
	rep.deadGates = dead

	// d)
	if err := s.processDeadGates(ctx, deadPairs, &rep); err != nil {
		return roundOutcome{stage: stageRedis, err: err, report: rep}
	}

	// e)
	s.refreshIndexGauges(dead, deadPairs, livePairs, &rep)
	if len(dead) > 0 || len(rep.actions) > 0 {
		logx.Infof("GateSweeper: round dead_gates=%d released_keys=%d dead_index_keys_left=%d live_index_keys=%d key_type_errors=%d actions=%v",
			len(dead), len(rep.released), rep.deadIndexKeys, rep.liveIndexKeys, rep.keyErrors.count, rep.actions)
	}
	return roundOutcome{report: rep}
}

// advanceSuspects 推进 gate 级去抖。返回已确认死亡(连续两拍缺席)的 gate(排过序)、这些 gate 的索引键(按 gate、slot 排序),
// 以及存活 gate 的索引键数。
//
// 去抖按 gate 算,不按索引键算:gate 死没死与 slot 无关,新接管一个段时,已经确认死亡的 gate 在新段上不必重新等两拍。
// 已确认死亡的 gate 只要还在本副本持有的登记集里就保持确认,预算截断的处理下一拍直接续上;
// gate 在 etcd 重新出现(重注册沿用同一 uuid)即清掉嫌疑与游标。不再出现在这些登记集里的条目一并丢弃。
func (s *gateSweeper) advanceSuspects(pairs []gateSlot, live map[string]struct{}) (dead []string, deadPairs []gateSlot, livePairs int) {
	inRegistry := make(map[gateSlot]struct{}, len(pairs))
	gates := make(map[string]struct{})
	for _, p := range pairs {
		inRegistry[p] = struct{}{}
		gates[p.gate] = struct{}{}
	}
	deadSet := make(map[string]struct{})
	for gate := range gates {
		if _, ok := live[gate]; ok {
			delete(s.suspects, gate)
			continue
		}
		if s.suspects[gate] < 2 {
			s.suspects[gate]++
		}
		if s.suspects[gate] >= 2 {
			dead = append(dead, gate)
			deadSet[gate] = struct{}{}
		}
	}
	for gate := range s.suspects {
		if _, ok := gates[gate]; !ok {
			delete(s.suspects, gate)
		}
	}
	for p := range s.cursors {
		_, stillIndexed := inRegistry[p]
		_, stillDead := deadSet[p.gate]
		if !stillIndexed || !stillDead {
			delete(s.cursors, p)
		}
	}
	for _, p := range pairs {
		if _, ok := deadSet[p.gate]; ok {
			deadPairs = append(deadPairs, p)
			continue
		}
		if _, ok := live[p.gate]; ok {
			livePairs++
		}
	}
	sort.Strings(dead)
	sort.Slice(deadPairs, func(i, j int) bool {
		if deadPairs[i].gate != deadPairs[j].gate {
			return deadPairs[i].gate < deadPairs[j].gate
		}
		return deadPairs[i].slot < deadPairs[j].slot
	})
	return dead, deadPairs, livePairs
}

// processDeadGates 在本拍预算内处理已确认死亡的 gate 的索引键:逐页 SSCAN、一次 MGET 本页会话、逐个判定后转换或摘除。
// 预算(成员数,另有半拍的时间上限)用完就记下游标留到下一拍,保证这一拍按时完成、新死的 gate 照常被识别。
// 一把索引键扫完一遍后空了就释放;没空(CAS 冲突留下的成员、扫描期间新加的成员、毒成员)就下一拍从头再扫。
// 某把索引键类型不对(SSCAN / 释放报 WRONGTYPE)只跳过它,不中止整拍(rep.keyErrors 计数,有上限)。
func (s *gateSweeper) processDeadGates(ctx context.Context, pairs []gateSlot, rep *indexRoundReport) error {
	budget := s.memberBudget
	start := s.deps.now()
	for _, p := range pairs {
		if budget <= 0 || s.deps.now().Sub(start) >= s.interval/2 {
			return nil
		}
		indexKey := gateSessionIndexKey(p.slot, p.gate)
		for budget > 0 && s.deps.now().Sub(start) < s.interval/2 {
			members, next, err := s.rdb.SScan(ctx, indexKey, s.cursors[p], "", s.cfg.ScanCount).Result()
			if err != nil {
				if aerr := rep.keyErrors.absorb(err, rep.actions, reconcileActionGateError,
					fmt.Sprintf("dead gate_instance=%s slot=%d (SSCAN of its index)", p.gate, p.slot)); aerr != nil {
					return fmt.Errorf("sscan dead gate %s index of slot %d: %w", p.gate, p.slot, aerr)
				}
				delete(s.cursors, p)
				break // 只跳过这把键,本拍继续下一把;gate 保持确认死亡,下一拍重试
			}
			if err := sweepIndexPage(ctx, s.rdb, p, members, s.transitionOnline, rep.actions, &rep.keyErrors); err != nil {
				return err
			}
			budget -= len(members)
			if next != 0 {
				s.cursors[p] = next
				continue
			}
			delete(s.cursors, p)
			released, err := releaseGateIndexIfEmptyScript.Run(ctx, s.rdb, releaseGateIndexKeys(p), p.gate).Int()
			if err != nil {
				if aerr := rep.keyErrors.absorb(err, rep.actions, reconcileActionGateError,
					fmt.Sprintf("dead gate_instance=%s slot=%d (release of its index)", p.gate, p.slot)); aerr != nil {
					return fmt.Errorf("release dead gate %s index of slot %d: %w", p.gate, p.slot, aerr)
				}
				break
			}
			if released == 1 {
				rep.released = append(rep.released, p)
			}
			break
		}
	}
	return nil
}

// transitionOnline 把死 gate 上的 ONLINE 会话转成 DISCONNECTING:与 SetDisconnecting 同一个提交点,先过本副本的转换限速。
// 会话此刻已被改写(重连 / 重登)则 CAS 失败 no-op,绝不误杀活人。
func (s *gateSweeper) transitionOnline(ctx context.Context, pid uint64, expected []byte, session *pb.PlayerSession) (bool, error) {
	if err := s.transitionPacer.Wait(ctx); err != nil {
		return false, err
	}
	updatedSession := proto.Clone(session).(*pb.PlayerSession)
	updatedSession.State = pb.PlayerSessionState_SESSION_STATE_DISCONNECTING
	updatedSession.SessionVersion++
	updated, err := proto.Marshal(updatedSession)
	if err != nil {
		// 刚解出来的消息重新编码不会失败;真失败也只跳过这一个,不拖垮整拍。
		logx.Errorf("GateSweeper: marshal updated session player=%d failed: %v", pid, err)
		return false, nil
	}
	ttl := time.Duration(s.svcCtx.Config.Lease.DefaultTTLSeconds) * time.Second
	if ttl == 0 {
		ttl = 30 * time.Second
	}
	swapped, err := transitionToDisconnecting(ctx, s.svcCtx, pid, expected, updated, s.deps.now().Add(ttl).Unix(), session)
	if err != nil || !swapped {
		return false, err
	}
	logx.Infof("GateSweeper: orphaned ONLINE session -> DISCONNECTING player=%d session=%d dead_gate_instance=%s",
		pid, session.SessionId, session.GateInstanceId)
	return true, nil
}

// refreshIndexGauges 刷新本副本这份的 gauge:死 gate 数、死 gate 剩余的索引键数(进度)、存活 gate 的索引键数。
//
// 按索引键数计,不逐键 SCARD:目标规模下索引键约等于在线人数(一把键平均一两个成员),逐键 SCARD 每拍是千万条命令,
// 登记集本来就读过,键数不多花一条命令。一把键的成员很少超过几个,所以键数就是人数的近似。
func (s *gateSweeper) refreshIndexGauges(dead []string, deadPairs []gateSlot, livePairs int, rep *indexRoundReport) {
	released := make(map[gateSlot]struct{}, len(rep.released))
	for _, p := range rep.released {
		released[p] = struct{}{}
	}
	pendingGates := make(map[string]struct{}, len(dead))
	pendingKeys := 0
	for _, p := range deadPairs {
		if _, ok := released[p]; !ok {
			pendingKeys++
			pendingGates[p.gate] = struct{}{}
		}
	}
	rep.deadIndexKeys, rep.liveIndexKeys = pendingKeys, livePairs
	reconcileDeadGates.Set(float64(len(pendingGates)))
	reconcileDeadGateMembers.Set(float64(pendingKeys))
	gateIndexMembers.Set(float64(livePairs))
}

// sweepIndexPage 判定一把索引键的一页成员并处置:
//   - 坏成员(不是 player_id,或不是这个 slot 的玩家)直接摘;会话缺失 / 解不开 / 已不在这个 gate 上 ONLINE 的,
//     按判定时看到的字节守卫摘;
//   - 仍在这个 gate 上 ONLINE 的:transition 非 nil(死 gate)就转换;nil(审计)原样保留。
//
// 单个成员上的 WRONGTYPE(会话键或本 slot 的租约键类型不对)由 tally 吸收:跳过该成员、计 member_error,成员留在索引里。
// 其余错误(连接、超时、READONLY 等实例级故障)原样返回,整拍 / 这一页失败。
//
// 坏掉的租约键只影响这一个 slot 的玩家,所以不再有「同一页全部转换失败就整拍失败」的判定:那是租约键全局时的做法,
// 现在它会让一个坏键把持段的副本一个个赶走。坏键多到超过 tally 的上限才整拍失败。
func sweepIndexPage(ctx context.Context, rdb redis.UniversalClient, p gateSlot, members []string,
	transition func(ctx context.Context, pid uint64, expected []byte, session *pb.PlayerSession) (bool, error),
	actions map[string]int, tally *keyErrorTally) error {
	indexKey := gateSessionIndexKey(p.slot, p.gate)
	valid := make([]string, 0, len(members))
	pids := make([]uint64, 0, len(members))
	for _, member := range members {
		pid, ok := parseIndexMember(member)
		if !ok || redisslot.PlayerSlot(pid) != p.slot {
			if err := rdb.SRem(ctx, indexKey, member).Err(); err != nil {
				if aerr := tally.absorb(err, actions, reconcileActionMemberError,
					fmt.Sprintf("bad member %q of gate_instance=%s index of slot=%d", member, p.gate, p.slot)); aerr != nil {
					return fmt.Errorf("prune bad member of gate %s index of slot %d: %w", p.gate, p.slot, aerr)
				}
				continue
			}
			countReconcileAction(actions, reconcileActionPrunedBadMember)
			logx.Errorf("SessionReconciler: pruned member %q from gate_instance=%s index of slot=%d: not a player id of this slot",
				member, p.gate, p.slot)
			continue
		}
		valid = append(valid, member)
		pids = append(pids, pid)
	}
	raws, found, err := mgetSessions(ctx, rdb, pids)
	if err != nil {
		return fmt.Errorf("mget sessions of gate %s index of slot %d: %w", p.gate, p.slot, err)
	}
	for i, member := range valid {
		pid, session, act := classifyIndexedMember(member, raws[i], found[i], p.gate)
		if act == indexedOnlineHere {
			if transition == nil {
				continue
			}
			swapped, err := transition(ctx, pid, raws[i], session)
			if err != nil {
				if aerr := tally.absorb(err, actions, reconcileActionMemberError,
					fmt.Sprintf("transition of player=%d on dead gate_instance=%s", pid, p.gate)); aerr != nil {
					return fmt.Errorf("transition player %d to disconnecting: %w", pid, aerr)
				}
				continue
			}
			if swapped {
				countReconcileAction(actions, reconcileActionToDisconnecting)
			} else {
				// 会话在判定之后被改写(重连 / 重登):成员留着,下一次判定重来。
				countReconcileAction(actions, reconcileActionCASConflict)
			}
			continue
		}
		action := reconcileActionPrunedStale
		switch act {
		case indexedMissing:
			action = reconcileActionPrunedMissing
		case indexedMalformed:
			action = reconcileActionPrunedMalformed
			logx.Errorf("SessionReconciler: malformed session behind index member gate_instance=%s player=%d; "+
				"member pruned, session left untouched", p.gate, pid)
		}
		removed, err := pruneGateIndexMember(ctx, rdb, p.gate, member, pid, raws[i], found[i])
		if err != nil {
			// 典型来源:会话键被写成了 hash / set 等(MGET 对非 string 回 nil,判成 missing,守卫脚本里的 GET 报 WRONGTYPE)。
			if aerr := tally.absorb(err, actions, reconcileActionMemberError,
				fmt.Sprintf("member %s of gate_instance=%s index (session key %s)", member, p.gate, sessionKey(pid))); aerr != nil {
				return fmt.Errorf("prune member %s of gate %s index: %w", member, p.gate, aerr)
			}
			continue
		}
		if removed {
			countReconcileAction(actions, action)
		}
	}
	return nil
}
