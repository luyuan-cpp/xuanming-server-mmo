package logic

import (
	"context"
	"errors"
	"time"

	"match/internal/constants"
	"match/internal/metrics"
	"match/internal/poolgroup"
	matchpb "proto/match"

	"github.com/zeromicro/go-zero/core/logx"
)

// 非评分队列(PVE 人数档、旧格式 PVE_TEAM 队列)的选人:按「单位」凑满
// (设计文档 docs/design/team-backfill-matching.md §4.2)。
//
// 一个单人是 1 人单位;一支队伍补位单是 k 人单位,不可拆。要找的是一组**人数之和恰好等于凑满人数**的单位。
// 评分队列(1v1 / 5v5)不走这里,仍是 popGroup 里的锚点 + 容差算法 —— 补位单只会进 PVE 人数档队列。
//
// 没有补位单时,结果与改动前的非评分路径逐位相同:等待序里前 required 个有效且不重复的成员。

// queueUnit 一个凑单单位。
type queueUnit struct {
	// members 进组顺序:单人 1 个;补位单凑齐后 = 票据里的名单(队长在前)。
	members []uint64
	// seed 补位单第一个被扫到的成员的票据(PartyId / PartyMembers / TeamId / QueueKey);单人为 nil。
	seed *queueTicket
	// entryIdx 属于这个单位的快照下标(撤单后标成 dropped,本轮不再碰)。
	entryIdx []int
	// got 补位单已经在快照里见到的成员。
	got map[uint64]bool
	// malformed 补位单的票据彼此对不上(名单读不出来、成员不在名单里、各人记的名单不一致)。
	malformed bool
	// complete 可以参与成组:单人恒真;补位单要名单全员都在快照里且票据有效。
	complete bool
	// removed 本轮已被撤出队列(team 复核不通过 / 不完整)。
	removed bool
	// checked 本轮已通过 team 复核。
	checked bool
	// deferred 本轮搁置:不进组,也不判它失效。两种来源 —— 成员带着上一局开局失败没清掉的备战锁
	// (groupPicker.deferred),或向 team 复核时读不到(不能让一支眼下核不了的队伍堵住后面的人)。
	deferred bool
}

func (u *queueUnit) usable() bool { return u.complete && !u.removed && !u.deferred }

// popUnits popGroup 的非评分分支:选单位 → 一条 Lua 摘出 → 摘出不足时整批放回队首。
// 返回值与 popGroup 相同。
func (p *groupPicker) popUnits(required uint32, configName string) ([]uint64, map[uint64]string, bool) {
	units, group, ok := p.pickUnits(required)
	if !ok {
		return nil, nil, false
	}
	now := nowMs()
	if group == nil {
		// 本轮没弹出组:饥饿 gauge 报仍在排的单位里等得最久的(没有则归零),与改动前的口径一致。
		longest := int64(0)
		for _, unit := range units {
			if unit.removed {
				continue
			}
			if wait := p.unitWaitSeconds(unit, now); wait > longest {
				longest = wait
			}
		}
		setStarvedAnchorWaitFn(p.modeName, configName, queueTeamSizeLabel(p.queueKey), float64(longest))
		warnNoExactCombination(p.queueKey, units, required)
		return nil, nil, false
	}

	members := make([]uint64, 0, required)
	for _, unit := range group {
		members = append(members, unit.members...)
	}
	removed, err := removeQueueMembers(p.svcCtx, p.queueKey, members)
	if err != nil {
		logx.Errorf("[matcher] Failed to remove matched group queue=%s members=%v: %v", p.queueKey, members, err)
		return nil, nil, false
	}
	if uint32(len(removed)) < required {
		// 校验与摘出之间有人被 CancelQueue 摘走:已摘出者按原相对顺序放回队首,下一轮再凑。
		// 按单位放回(从后往前,每个单位一条 Lua):补位单不会只回去一部分;被摘走的若是补位单成员,
		// 那一单正在撤单(票据已删),放回去的残留下一轮扫到时按不完整处理。
		// 放不回去的单位见 abandonUnreturned。
		logx.Infof("[matcher] Only %d/%d players still queued when removed; returning to queue front queue=%s",
			len(removed), required, p.queueKey)
		stillThere := make(map[uint64]bool, len(removed))
		for _, pid := range removed {
			stillThere[pid] = true
		}
		for i := len(group) - 1; i >= 0; i-- {
			pushes := make([]queuePush, 0, len(group[i].members))
			for _, pid := range group[i].members {
				if stillThere[pid] {
					pushes = append(pushes, queuePush{playerId: pid, rating: p.tickets[pid].Rating})
				}
			}
			if len(pushes) == 0 {
				continue
			}
			if err := pushMembersRegistered(context.Background(), p.svcCtx, p.queueKey, pushes, true); err != nil {
				logx.Errorf("[matcher] Failed to return players to queue front; deleting their tickets to allow requeue queue=%s members=%v: %v",
					p.queueKey, group[i].members, err)
				p.abandonUnreturned(group[i], pushes)
			}
		}
		return nil, nil, false
	}

	tickets := make(map[uint64]string, len(members))
	for _, pid := range members {
		tickets[pid] = p.tickets[pid].Ticket
	}
	waitSeconds := p.unitWaitSeconds(group[0], now)
	metrics.ObserveMatchWait(p.modeName, float64(waitSeconds))
	parties := 0
	for _, unit := range group {
		if unit.seed != nil {
			parties++
		}
	}
	logx.Infof("[matcher] Group formed queue=%s anchor=%d wait=%ds units=%d parties=%d members=%v",
		p.queueKey, members[0], waitSeconds, len(group), parties, members)
	return members, tickets, true
}

// pickUnits 按等待序扫快照、逐个校验并归成单位,每出现一个新的完整单位就尝试成组。
// 返回 (本轮扫到的全部单位, 选中的组, ok):组为 nil = 本轮凑不出;ok=false = Redis 抖动,结束本轮、不做任何判定。
//
// 扫到能成组就停,所以常见情形(队首就是 required 个单人)读的票据数与改动前一样。凑不出时会把快照里的
// 票据都读一遍(上限 queueScanLimit),这是「一个人数档里全是互相凑不成的补位单」时每轮的代价(§6 已知边界)。
func (p *groupPicker) pickUnits(required uint32) ([]*queueUnit, []*queueUnit, bool) {
	var units []*queueUnit
	parties := make(map[string]*queueUnit)
	seen := make(map[uint64]bool)
	checkDeadline := time.Now().Add(partyCheckRoundBudget)
	for idx := range p.entries {
		if p.dropped[idx] {
			continue
		}
		valid, err := p.validate(idx)
		if err != nil {
			logx.Errorf("[matcher] Failed to validate queue member; ending current round queue=%s: %v", p.queueKey, err)
			return nil, nil, false
		}
		if !valid {
			// 被搁置的成员(票据有效、带着上一局没清掉的备战锁)若属于补位单:整单本轮不可用,但不算不完整。
			if ticket := p.deferred[p.entries[idx].playerId]; ticket != nil && ticket.PartyId != "" {
				unit := parties[ticket.PartyId]
				if unit == nil {
					unit = &queueUnit{seed: ticket, got: make(map[uint64]bool, len(ticket.PartyMembers))}
					parties[ticket.PartyId] = unit
					units = append(units, unit)
				}
				unit.entryIdx = append(unit.entryIdx, idx)
				unit.deferred = true
			}
			continue
		}
		pid := p.entries[idx].playerId
		if seen[pid] {
			continue // 同一玩家两份(并发 JoinQueue 与自愈 / 回队首窄窗口叠加):只算一份,摘出时 LREM 0 一并清掉
		}
		seen[pid] = true
		ticket := p.tickets[pid]

		if ticket.PartyId == "" {
			units = append(units, &queueUnit{members: []uint64{pid}, entryIdx: []int{idx}, complete: true})
		} else {
			unit := parties[ticket.PartyId]
			if unit == nil {
				unit = &queueUnit{seed: ticket, got: make(map[uint64]bool, len(ticket.PartyMembers))}
				parties[ticket.PartyId] = unit
				units = append(units, unit) // 位置按第一名成员算:同单成员是一条 Lua 压进去的,在 list 里相邻
			}
			unit.entryIdx = append(unit.entryIdx, idx)
			unit.got[pid] = true
			if len(unit.seed.PartyMembers) == 0 || !containsPlayer(unit.seed.PartyMembers, pid) ||
				!samePlayerList(ticket.PartyMembers, unit.seed.PartyMembers) {
				unit.malformed = true
			}
			if unit.malformed || len(unit.got) < len(unit.seed.PartyMembers) {
				continue // 这一单还没凑齐,没有新的完整单位
			}
			unit.members = unit.seed.PartyMembers
			unit.complete = true
		}

		group, ok := p.selectCheckedUnits(units, required, checkDeadline)
		if !ok {
			return nil, nil, false
		}
		if group != nil {
			return units, group, true
		}
	}
	p.evictIncompleteParties(units)
	return units, nil, true
}

// abandonUnreturned 摘出之后放不回队首的单位:票据是 queued、人却不在 list 里,matcher 再也弹不到他们;
// 客户端排队态不会再发 JoinQueue,队伍面板「匹配中」又锁着开战按钮,票据自愈也触发不了。
// 与 requeueUnit 的压回失败同口径:删票让他们可以立刻重排,补位单通知一次。
//   - 补位单:cancelParty —— 逐人「仍是 queued 才删」、整批摘 list(结果未知时那条 Lua 可能已执行,
//     这一步正好把可能压回去的项摘掉)、通知一次,与并发撤单重叠的情形它也已经处理;
//   - 单人:删票 + 摘一次 list。
func (p *groupPicker) abandonUnreturned(unit *queueUnit, pushes []queuePush) {
	if unit.seed != nil {
		cancelParty(p.svcCtx, unit.seed, constants.PartyEndMemberNotReady, 0)
		return
	}
	ids := make([]uint64, 0, len(pushes))
	for _, push := range pushes {
		ids = append(ids, push.playerId)
		ticket := p.tickets[push.playerId]
		if ticket == nil {
			continue
		}
		if _, err := cancelTicketIfQueued(p.svcCtx, push.playerId, ticket.Ticket); err != nil {
			logx.Errorf("[matcher] Failed to delete ticket after return-to-front failure (ticket TTL will recover) player=%d: %v",
				push.playerId, err)
		}
	}
	if _, err := removeQueueMembers(p.svcCtx, p.queueKey, ids); err != nil {
		logx.Errorf("[matcher] Failed to dequeue after return-to-front failure (matcher ticket validation will handle remnants) queue=%s: %v",
			p.queueKey, err)
	}
}

// selectCheckedUnits 在已有的完整单位里选一组,并在弹出前对组里的补位单逐单向 team 复核
// (队伍记录还在、成员集合与名单相等、全员在线)。复核不过的整单撤出队列、复核读不到的整单本轮搁置,
// 然后重新选;每轮至少有一单退出候选,所以循环必然结束。返回 (组, ok):组为 nil = 目前凑不出;
// ok 目前恒为 true,保留给票据 / 队列自身读写出错时结束本轮。
func (p *groupPicker) selectCheckedUnits(units []*queueUnit, required uint32, checkDeadline time.Time) ([]*queueUnit, bool) {
	for {
		group := selectUnits(units, int(required))
		mode, _, _ := parseQueueKey(p.queueKey)
		if matchpb.MatchMode(mode) == matchpb.MatchMode_MATCH_MODE_PVP_TEAM {
			group = p.selectPvpUnits(units, int(queueTeamSize(p.queueKey)))
		}
		if group == nil {
			return nil, true
		}
		evicted := false
		for _, unit := range group {
			if unit.seed == nil || unit.checked {
				continue
			}
			reason, culprit, err := p.checkParty(unit, checkDeadline)
			if err != nil {
				// 复核读不到(team 的 Redis 抖动、会话读不出、本轮复核预算用完):不做失效判定,
				// 只把这一单本轮搁置,用其余单位重新选 —— 不能让一支眼下核不了的队伍堵住后面本可成组的人。
				warnPartyCheckFailed(p.queueKey, unit.seed.TeamId, err)
				unit.deferred = true
				evicted = true
				continue
			}
			if reason == constants.PartyEndNone {
				unit.checked = true
				continue
			}
			p.evictUnit(unit, reason, culprit)
			evicted = true
		}
		if !evicted {
			return group, true
		}
	}
}

// 两侧装箱共用纯函数；每个补位单只能完整进入同一侧，不能在人数凑齐后再蛇形拆队。
func (p *groupPicker) selectPvpUnits(units []*queueUnit, sideSize int) []*queueUnit {
	var eligible []*queueUnit
	for _, unit := range units {
		if unit.usable() && len(unit.members) <= sideSize {
			eligible = append(eligible, unit)
		}
	}
	for anchor := range eligible {
		candidates := eligible[anchor:]
		sizes := make([]int, len(candidates))
		for i, unit := range candidates {
			sizes[i] = len(unit.members)
		}
		indices, sides := poolgroup.PickPvpUnits(sizes, sideSize)
		if len(indices) == 0 {
			continue
		}
		clear(p.teamByPlayer)
		group := make([]*queueUnit, 0, len(indices))
		for i, index := range indices {
			unit := candidates[index]
			group = append(group, unit)
			for _, player := range unit.members {
				p.teamByPlayer[player] = sides[i]
			}
		}
		return group
	}
	return nil
}

// selectUnits 在可用单位里找人数之和**恰好**等于 required 的一组;多解时取等待序字典序最小的那组
// (能带上更靠前的单位就一定带上)。找不到返回 nil。
//
// 效果:队首单位只要凑得满就优先成组;队首是一支眼下凑不满的大队时,后面的单位照常成组,不堵队。
// 做法:reach[i][r] = units[i:] 能否凑出恰好 r 人(从后往前的子集和),再从前往后贪心取。
// 规模很小:单位数 ≤ queueScanLimit,required ≤ 引擎每队上限。
func selectUnits(units []*queueUnit, required int) []*queueUnit {
	if required <= 0 {
		return nil
	}
	n := len(units)
	reach := make([][]bool, n+1)
	reach[n] = make([]bool, required+1)
	reach[n][0] = true
	for i := n - 1; i >= 0; i-- {
		row := make([]bool, required+1)
		copy(row, reach[i+1])
		if units[i].usable() {
			size := len(units[i].members)
			for r := size; r <= required; r++ {
				if reach[i+1][r-size] {
					row[r] = true
				}
			}
		}
		reach[i] = row
	}
	if !reach[0][required] {
		return nil
	}
	var group []*queueUnit
	remaining := required
	for i := 0; i < n && remaining > 0; i++ {
		if !units[i].usable() {
			continue
		}
		size := len(units[i].members)
		if size <= remaining && reach[i+1][remaining-size] {
			group = append(group, units[i])
			remaining -= size
		}
	}
	return group
}

// checkParty 向 team 复核一支补位单。端口未注入时返回 err(调用方把这一单本轮搁置):进程重启后 Redis 里
// 还留着存量补位单,「没人能复核」不等于「仍然有效」—— 不复核就成组会把已离队 / 已掉线的人拉进战斗。
// 一轮弹组里的全部复核共用 checkDeadline 这份预算:凑单锁不续期,不能被慢读拖过期。
func (p *groupPicker) checkParty(unit *queueUnit, checkDeadline time.Time) (uint32, uint64, error) {
	port := p.svcCtx.PartyTeam()
	if port == nil {
		return constants.PartyEndNone, 0, errors.New("party team port is not wired")
	}
	remaining := time.Until(checkDeadline)
	if remaining <= 0 {
		return constants.PartyEndNone, 0, errors.New("party check budget for this round exhausted")
	}
	if remaining > partyCheckTimeout {
		remaining = partyCheckTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), remaining)
	defer cancel()
	return port.CheckParty(ctx, unit.seed.TeamId, unit.members)
}

// evictUnit 把一支补位单整单撤出队列(删票、摘 list、通知 team),并把它在快照里的下标标成已处理。
// 名单损坏时 cancelParty 只覆盖种子票据名单里的人,其余扫到的成员各按自己的票清掉。
func (p *groupPicker) evictUnit(unit *queueUnit, reason uint32, culprit uint64) {
	unit.removed = true
	cancelParty(p.svcCtx, unit.seed, reason, culprit)
	for _, idx := range unit.entryIdx {
		pid := p.entries[idx].playerId
		if containsPlayer(unit.seed.PartyMembers, pid) {
			p.dropped[idx] = true
			continue
		}
		if ticket := p.tickets[pid]; ticket != nil {
			if _, err := cancelTicketIfQueued(p.svcCtx, pid, ticket.Ticket); err != nil {
				logx.Errorf("[matcher] Failed to delete ticket of malformed party member player=%d: %v", pid, err)
			}
		}
		p.drop(idx)
	}
}

// evictIncompleteParties 扫完快照仍不完整的补位单整单撤出:名单里有人不在 list 里,或他的票据无效
// (已取消 / 过期 / 换成了别的票 / 已在战斗中)。补位单进出 list 永远是一条 Lua,所以持锁期间不存在
// 「只有一部分成员在 list 里」的合法瞬间(party.go 文件头的不变量)。
// 三种情况不判:快照读满(队列比 queueScanLimit 还长,缺的人可能只是排在快照之外);
// 本轮被搁置的单;缺席成员仍持有本单票据(别的持锁方正在处理,见函数内注释)。
func (p *groupPicker) evictIncompleteParties(units []*queueUnit) {
	if len(p.entries) >= queueScanLimit {
		return
	}
	for _, unit := range units {
		if unit.seed == nil || unit.complete || unit.removed || unit.deferred {
			continue
		}
		culprit := uint64(0)
		inFlight := false
		for _, pid := range unit.seed.PartyMembers {
			if unit.got[pid] {
				continue
			}
			// 缺席的人若仍持有本单的票(matched / queued),这一单正被别的持锁方处理 —— 凑单锁过期或
			// 主从切换丢锁后,另一个实例可能正在对它逐人推进 matched。本轮不判,否则会把一支已经凑满、
			// 马上开战的队伍撤掉。只有票据缺失或已换成别的票才算真的缺席。读不到同样不判。
			ticket, err := loadTicket(p.svcCtx, pid)
			if err != nil || (ticket != nil && ticket.PartyId == unit.seed.PartyId) {
				inFlight = true
				break
			}
			if culprit == 0 {
				culprit = pid
			}
		}
		if inFlight {
			continue
		}
		logx.Infof("[matcher] Party incomplete in queue; removing whole party queue=%s team=%d party=%s absent=%d malformed=%t",
			p.queueKey, unit.seed.TeamId, unit.seed.PartyId, culprit, unit.malformed)
		p.evictUnit(unit, constants.PartyEndMemberNotReady, culprit)
	}
}

// warnNoExactCombination 队列里人数够了却凑不出「恰好 required 人」的组合时(例如 3 人档里两支 2 人队),
// 记一条限频(每队列 10s)Info 日志,带各可用单位的人数。此时 queue_depth ≥ 凑满人数且长期不降、
// 饥饿等待一路上涨,只看指标与 matcher 卡住分不出来,靠这条日志区分。
func warnNoExactCombination(queueKey string, units []*queueUnit, required uint32) {
	sizes := make([]int, 0, len(units))
	total := 0
	for _, unit := range units {
		if !unit.usable() {
			continue
		}
		sizes = append(sizes, len(unit.members))
		total += len(unit.members)
	}
	if uint32(total) < required {
		return
	}
	now := time.Now().UnixNano()
	if last, ok := lastNoCombinationWarnNs.Load(queueKey); ok && now-last.(int64) < int64(unmatchableQueueWarnInterval) {
		return
	}
	lastNoCombinationWarnNs.Store(queueKey, now)
	logx.Infof("[matcher] Enough players queued but no combination sums to exactly the required size (parties cannot be split) queue=%s required=%d units=%v",
		queueKey, required, sizes)
}

// unitWaitSeconds 一个单位已等的秒数:单人看本人票据,补位单看种子票据(同单成员入队时间相同)。
func (p *groupPicker) unitWaitSeconds(unit *queueUnit, now uint64) int64 {
	ticket := unit.seed
	if ticket == nil && len(unit.members) > 0 {
		ticket = p.tickets[unit.members[0]]
	}
	if ticket == nil || ticket.EnqueuedAtMs == 0 || now <= ticket.EnqueuedAtMs {
		return 0
	}
	return int64((now - ticket.EnqueuedAtMs) / 1000)
}

func containsPlayer(ids []uint64, playerId uint64) bool {
	for _, id := range ids {
		if id == playerId {
			return true
		}
	}
	return false
}

// samePlayerList 两份名单逐位相同(顺序也是契约:它决定站位)。
func samePlayerList(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
