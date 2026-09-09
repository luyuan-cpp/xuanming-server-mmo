package gameobject

import (
	"context"
	"math/rand"
	"sync"
	"time"

	"go.uber.org/zap"

	"proto/battle"
	"proto/scene"
)

// Player represents a robot's in-game player state.
type Player struct {
	ID uint64

	mu             sync.RWMutex
	entityID       uint64   // own entt entity in scene
	knownEntities  []uint64 // all visible entities (including self)
	ownedSkillIDs  []uint32 // skills returned by ListSkills
	sceneID        uint64
	sceneConfigID  uint32
	sceneEnterCnt  int
	skillAckCnt    int
	skillUsedCnt   int
	lastSkillError string

	sceneReady     chan struct{} // closed when first NotifyEnterScene arrives
	sceneReadyOnce sync.Once

	skillsReady     chan struct{} // closed when ListSkills response arrives
	skillsReadyOnce sync.Once

	// currency state populated by GetCurrencyListResponse / GmAddCurrencyResponse.
	// values mirrors CurrencyComp.values (slot index = currency type id).
	// lastBalanceAfter is whatever the most recent GmAddCurrency / GmDeductCurrency
	// returned, useful when the scenario only cares about a single type round-trip.
	currencyValues       []uint64
	currencyHasList      bool // GetCurrencyListResponse seen at least once
	lastBalanceAfter     uint64
	lastBalanceAfterSeen bool
	currencyListReady    chan struct{} // closed on first GetCurrencyListResponse
	currencyListReadyOne sync.Once
	currencyAddReady     chan struct{} // closed on first GmAddCurrencyResponse
	currencyAddReadyOne  sync.Once

	// ---- 战斗冒烟(battle-smoke)状态:参战侧 ----
	// battleId 由 NotifyBattleStart 写入;battleStart/battleEnd 是一次性广播
	// 通道(close 即广播),照 sceneReady 的惯例做懒初始化 + sync.Once 保护。
	battleId        uint64        // 当前对局 id(NotifyBattleStart 记录)
	battleOutcome   int32         // 战斗结果(EBattleOutcome,NotifyBattleEnd 记录)
	turnCount       int           // 收到的 NotifyTurnResult 条数(参战视角回合数)
	battleStart     chan struct{} // NotifyBattleStart 到达时 close
	battleStartOnce sync.Once
	battleEnd       chan struct{} // NotifyBattleEnd 到达时 close
	battleEndOnce   sync.Once

	// battleAssigned 是战斗直连的落点分配(NotifyBattleAssigned / 补签应答记录),
	// openBattleDirectConn 等它拿 host:port + 票据。与 battleStart 同一套惯例:
	// 懒初始化通道 + sync.Once 广播。补签(RequestBattleTicket)会再次 Signal,
	// 那时通道已关,只更新快照——等待方本来就只关心"最新一份分配"。
	battleAssignedInfo *battle.BattleAssignedS2C
	battleAssigned     chan struct{}
	battleAssignedOnce sync.Once

	// ---- 战斗冒烟(battle-smoke)状态:观战侧 ----
	spectateBattleId  uint64        // 正在观战的对局 id(NotifySpectateState 记录)
	spectateObservers uint32        // 观战人数(NotifySpectateState 记录)
	spectateTurnCount int           // 收到的 NotifySpectateTurnResult 条数(观战视角回合数)
	spectateEndReason int32         // 观战结束原因(ESpectateEndReason,NotifySpectateEnd 记录)
	spectateState     chan struct{} // 首个 NotifySpectateState 到达时 close
	spectateStateOnce sync.Once
	spectateEnd       chan struct{} // NotifySpectateEnd 到达时 close
	spectateEndOnce   sync.Once

	// ---- 属性加点冒烟(attribute-smoke)状态 ----
	// 面板是服务器唯一真相:每个写操作的响应都带全量面板,这里只存最近一份。
	// attrPanel 用 chan 广播"面板已到",照 sceneReady 的懒初始化 + sync.Once 惯例;
	// 但与战斗不同,属性面板会反复刷新,所以额外用 attrPanelSeq 让等待方能区分
	// "拿到的是这次请求的新面板"还是"上一次留下的旧面板"。
	attrPanel       *scene.AttributePanelInfo
	attrPanelSeq    uint64
	attrPanelMsg    uint32            // 最近一份面板的来源消息号(响应 = 请求号;主动推送 = 170)
	attrLastTip     uint32            // 最近一次属性 RPC 的 error_message.id(0 = 成功)
	attrSuggested   map[uint32]uint32 // 最近一次自动加点建议(dimension_id → 目标已分配)
	attrSuggestPool uint32
	attrReady       chan struct{} // 首份面板到达时 close
	attrReadyOnce   sync.Once
}

// NewPlayer creates a Player with an initialized scene-ready channel.
func NewPlayer(id uint64) *Player {
	return &Player{
		ID:                id,
		sceneReady:        make(chan struct{}),
		skillsReady:       make(chan struct{}),
		currencyListReady: make(chan struct{}),
		currencyAddReady:  make(chan struct{}),
	}
}

func (p *Player) ensureSceneReadyChannel() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sceneReady == nil {
		p.sceneReady = make(chan struct{})
	}
}

// SetEntityID sets the player's own entity ID.
func (p *Player) SetEntityID(id uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.entityID = id
	for _, e := range p.knownEntities {
		if e == id {
			return
		}
	}
	p.knownEntities = append(p.knownEntities, id)
}

// GetEntityID returns the player's own entity ID.
func (p *Player) GetEntityID() uint64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.entityID
}

// SetSceneInfo updates the player's current scene snapshot.
func (p *Player) SetSceneInfo(sceneID uint64, sceneConfigID uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sceneID != 0 && p.sceneID != sceneID {
		p.entityID = 0
		p.knownEntities = nil
	}
	p.sceneID = sceneID
	p.sceneConfigID = sceneConfigID
	p.sceneEnterCnt++
}

// SignalSceneReady marks the player as having entered a scene.
func (p *Player) SignalSceneReady() {
	p.ensureSceneReadyChannel()
	p.sceneReadyOnce.Do(func() { close(p.sceneReady) })
}

// WaitSceneReady blocks until the scene is ready or the context is cancelled.
func (p *Player) WaitSceneReady(ctx context.Context) error {
	p.ensureSceneReadyChannel()
	select {
	case <-p.sceneReady:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *Player) GetSceneID() uint64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.sceneID
}

func (p *Player) GetSceneConfigID() uint32 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.sceneConfigID
}

func (p *Player) GetSceneEnterCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.sceneEnterCnt
}

// AddEntity adds an entity to the known list.
func (p *Player) AddEntity(id uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.knownEntities {
		if e == id {
			return
		}
	}
	p.knownEntities = append(p.knownEntities, id)
}

// RemoveEntity removes an entity from the known list.
func (p *Player) RemoveEntity(id uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, e := range p.knownEntities {
		if e == id {
			p.knownEntities = append(p.knownEntities[:i], p.knownEntities[i+1:]...)
			return
		}
	}
}

// RemoveEntities removes multiple entities from the known list.
func (p *Player) RemoveEntities(ids []uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	removeSet := make(map[uint64]struct{}, len(ids))
	for _, id := range ids {
		removeSet[id] = struct{}{}
	}
	filtered := p.knownEntities[:0]
	for _, e := range p.knownEntities {
		if _, ok := removeSet[e]; !ok {
			filtered = append(filtered, e)
		}
	}
	p.knownEntities = filtered
}

// GetRandomEntity returns a random known entity ID, or 0 if none.
func (p *Player) GetRandomEntity() uint64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.knownEntities) == 0 {
		return p.entityID
	}
	return p.knownEntities[rand.Intn(len(p.knownEntities))]
}

func (p *Player) NoteSkillResponse(err string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err == "" {
		p.skillAckCnt++
		p.lastSkillError = ""
		return
	}
	p.lastSkillError = err
}

func (p *Player) NoteSkillUsed() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.skillUsedCnt++
}

func (p *Player) GetSkillStats() (ackCount, usedCount int, lastErr string) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.skillAckCnt, p.skillUsedCnt, p.lastSkillError
}

// SetOwnedSkillIDs sets the player's owned skill IDs and signals skills ready.
func (p *Player) SetOwnedSkillIDs(ids []uint32) {
	p.mu.Lock()
	p.ownedSkillIDs = ids
	p.mu.Unlock()
	p.SignalSkillsReady()
}

// SignalSkillsReady marks the player's skill list as received.
func (p *Player) SignalSkillsReady() {
	p.skillsReadyOnce.Do(func() { close(p.skillsReady) })
}

// WaitSkillsReady blocks until the skill list is received or the context is cancelled.
func (p *Player) WaitSkillsReady(ctx context.Context) error {
	select {
	case <-p.skillsReady:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// GetOwnedSkillIDs returns a copy of the player's owned skill IDs.
func (p *Player) GetOwnedSkillIDs() []uint32 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if len(p.ownedSkillIDs) == 0 {
		return nil
	}
	out := make([]uint32, len(p.ownedSkillIDs))
	copy(out, p.ownedSkillIDs)
	return out
}

// SetCurrencyValues records the full CurrencyComp.values slice from a
// GetCurrencyListResponse, then signals currencyListReady so a scenario can
// proceed only after the initial balance snapshot lands.
func (p *Player) SetCurrencyValues(values []uint64) {
	p.mu.Lock()
	if values == nil {
		p.currencyValues = nil
	} else {
		p.currencyValues = make([]uint64, len(values))
		copy(p.currencyValues, values)
	}
	p.currencyHasList = true
	p.mu.Unlock()
	p.currencyListReadyOne.Do(func() { close(p.currencyListReady) })
}

// GetCurrencyValue returns the balance at slot `typeID` (CurrencyType enum).
// Returns (0, false) if no GetCurrencyListResponse has landed yet, or the
// slot is out of range.
func (p *Player) GetCurrencyValue(typeID uint32) (uint64, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !p.currencyHasList {
		return 0, false
	}
	idx := int(typeID)
	if idx < 0 || idx >= len(p.currencyValues) {
		return 0, true // list seen, slot just empty (proto repeated may be short)
	}
	return p.currencyValues[idx], true
}

// SetLastBalanceAfter records balance_after from a GmAddCurrencyResponse /
// GmDeductCurrencyResponse, and signals currencyAddReady on first call so a
// scenario can wait for the GM RPC to round-trip.
func (p *Player) SetLastBalanceAfter(balance uint64) {
	p.mu.Lock()
	p.lastBalanceAfter = balance
	p.lastBalanceAfterSeen = true
	p.mu.Unlock()
	p.currencyAddReadyOne.Do(func() { close(p.currencyAddReady) })
}

// GetLastBalanceAfter returns the most recent GmAddCurrency / GmDeductCurrency
// balance_after; second return is false if no GM response has been seen yet.
func (p *Player) GetLastBalanceAfter() (uint64, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lastBalanceAfter, p.lastBalanceAfterSeen
}

// ResetCurrencyListReady re-arms the currencyListReady channel so the
// next WaitCurrencyListReady call blocks until the *next* GetCurrencyList
// response arrives. The currency-crash-window scenario calls this before
// each round-trip so a single session can sample the balance multiple
// times. Safe to call from any goroutine.
func (p *Player) ResetCurrencyListReady() {
	p.mu.Lock()
	p.currencyListReady = make(chan struct{})
	p.currencyListReadyOne = sync.Once{}
	p.currencyHasList = false
	p.currencyValues = nil
	p.mu.Unlock()
}

// ResetCurrencyAddReady is the GmAddCurrency twin of ResetCurrencyListReady.
func (p *Player) ResetCurrencyAddReady() {
	p.mu.Lock()
	p.currencyAddReady = make(chan struct{})
	p.currencyAddReadyOne = sync.Once{}
	p.lastBalanceAfterSeen = false
	p.lastBalanceAfter = 0
	p.mu.Unlock()
}

// WaitCurrencyListReady blocks until SetCurrencyValues is called or ctx
// is cancelled. Used after sending GetCurrencyListRequest.
func (p *Player) WaitCurrencyListReady(ctx context.Context) error {
	p.mu.RLock()
	ch := p.currencyListReady
	p.mu.RUnlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// WaitCurrencyAddReady blocks until SetLastBalanceAfter fires (= a
// GmAddCurrencyResponse landed) or ctx is cancelled.
func (p *Player) WaitCurrencyAddReady(ctx context.Context) error {
	p.mu.RLock()
	ch := p.currencyAddReady
	p.mu.RUnlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ---------------------------------------------------------------------------
// 战斗冒烟(battle-smoke)信号:参战侧
// 通道全部懒初始化(ensureXxxChannel),Signal/Wait 双侧都先 ensure,
// 保证任意调用顺序下都不会对 nil channel 做 select/close。
// ---------------------------------------------------------------------------

func (p *Player) ensureBattleStartChannel() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.battleStart == nil {
		p.battleStart = make(chan struct{})
	}
}

func (p *Player) ensureBattleEndChannel() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.battleEnd == nil {
		p.battleEnd = make(chan struct{})
	}
}

// SignalBattleStart 记录对局 id 并广播"战斗已开始"。由 NotifyBattleStart
// handler 调用;重复到达只生效一次(sync.Once)。
func (p *Player) SignalBattleStart(battleId uint64) {
	p.ensureBattleStartChannel()
	p.mu.Lock()
	p.battleId = battleId
	p.mu.Unlock()
	p.battleStartOnce.Do(func() { close(p.battleStart) })
}

// WaitBattleStart 阻塞等待 NotifyBattleStart,返回对局 id。
func (p *Player) WaitBattleStart(ctx context.Context) (uint64, error) {
	p.ensureBattleStartChannel()
	p.mu.RLock()
	ch := p.battleStart
	p.mu.RUnlock()
	select {
	case <-ch:
		return p.GetBattleId(), nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (p *Player) ensureBattleAssignedChannel() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.battleAssigned == nil {
		p.battleAssigned = make(chan struct{})
	}
}

// SignalBattleAssigned 记录战斗直连的落点分配并广播。由 NotifyBattleAssigned
// (battle 主动推)和 RequestBattleTicket 的补签应答共同调用;nil 分配忽略,
// 免得等待方拿到一份空落点还以为拿到了票据。
func (p *Player) SignalBattleAssigned(assigned *battle.BattleAssignedS2C) {
	if assigned == nil {
		return
	}
	p.ensureBattleAssignedChannel()
	p.mu.Lock()
	p.battleAssignedInfo = assigned
	p.mu.Unlock()
	p.battleAssignedOnce.Do(func() { close(p.battleAssigned) })
}

// WaitBattleAssigned 阻塞等待落点分配,返回最近一份(补签会覆盖)。
func (p *Player) WaitBattleAssigned(ctx context.Context) (*battle.BattleAssignedS2C, error) {
	p.ensureBattleAssignedChannel()
	p.mu.RLock()
	ch := p.battleAssigned
	p.mu.RUnlock()
	select {
	case <-ch:
		return p.GetBattleAssigned(), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// GetBattleAssigned 返回最近一份落点分配(nil = 还没收到)。
func (p *Player) GetBattleAssigned() *battle.BattleAssignedS2C {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.battleAssignedInfo
}

// GetBattleId 返回 NotifyBattleStart 记录的对局 id(0 = 尚未开战)。
func (p *Player) GetBattleId() uint64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.battleId
}

// SignalBattleEnd 记录战斗结果并广播"战斗已结束"。由 NotifyBattleEnd
// handler 调用;outcome 为 EBattleOutcome 的数值。
//
// 按 battle_id 过滤:玩家登录时 scene 会把离线期间结束的**上一局**结算以
// BattleEndS2C 补推给客户端(离线挂起结算),它先于本进程的 NotifyBattleStart 到达。
// 不过滤的话这条陈旧结束会提前关掉 battleEnd 通道,WaitBattleEnd 在新局刚开始时就
// 返回"0 回合结束"(2026-09-02 跨 zone 冒烟复跑实测)。只接受与当前对局一致的结束;
// 尚未开战(battleId==0)时收到的结束一律视为陈旧,只记录不广播。
func (p *Player) SignalBattleEnd(battleId uint64, outcome int32) {
	p.ensureBattleEndChannel()
	p.mu.Lock()
	current := p.battleId
	if battleId != 0 && current != battleId {
		p.mu.Unlock()
		zap.L().Info("[robot] ignore stale BattleEnd",
			zap.Uint64("end_battle_id", battleId), zap.Uint64("current_battle_id", current))
		return
	}
	p.battleOutcome = outcome
	p.mu.Unlock()
	p.battleEndOnce.Do(func() { close(p.battleEnd) })
}

// WaitBattleEnd 阻塞等待 NotifyBattleEnd,返回战斗结果(EBattleOutcome 数值)。
func (p *Player) WaitBattleEnd(ctx context.Context) (int32, error) {
	p.ensureBattleEndChannel()
	p.mu.RLock()
	ch := p.battleEnd
	p.mu.RUnlock()
	select {
	case <-ch:
		p.mu.RLock()
		defer p.mu.RUnlock()
		return p.battleOutcome, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// AddTurnResult 参战视角回合计数 +1(每收到一条 NotifyTurnResult 调一次)。
func (p *Player) AddTurnResult() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.turnCount++
}

// GetTurnCount 返回参战视角收到的回合结算条数。
func (p *Player) GetTurnCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.turnCount
}

// ---------------------------------------------------------------------------
// 战斗冒烟(battle-smoke)信号:观战侧
// ---------------------------------------------------------------------------

func (p *Player) ensureSpectateStateChannel() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.spectateState == nil {
		p.spectateState = make(chan struct{})
	}
}

func (p *Player) ensureSpectateEndChannel() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.spectateEnd == nil {
		p.spectateEnd = make(chan struct{})
	}
}

// SignalSpectateState 记录观战对局 id / 观战人数并广播"观战快照已到"。
// 由 NotifySpectateState handler 调用;后续快照仍会刷新数据,但只广播一次。
func (p *Player) SignalSpectateState(battleId uint64, observerCount uint32) {
	p.ensureSpectateStateChannel()
	p.mu.Lock()
	p.spectateBattleId = battleId
	p.spectateObservers = observerCount
	p.mu.Unlock()
	p.spectateStateOnce.Do(func() { close(p.spectateState) })
}

// WaitSpectateState 阻塞等待首个 NotifySpectateState,返回观战对局 id。
func (p *Player) WaitSpectateState(ctx context.Context) (uint64, error) {
	p.ensureSpectateStateChannel()
	p.mu.RLock()
	ch := p.spectateState
	p.mu.RUnlock()
	select {
	case <-ch:
		p.mu.RLock()
		defer p.mu.RUnlock()
		return p.spectateBattleId, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// GetSpectateObserverCount 返回最近一次 NotifySpectateState 里的观战人数。
func (p *Player) GetSpectateObserverCount() uint32 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.spectateObservers
}

// AddSpectateTurnResult 观战视角回合计数 +1(每收到一条 NotifySpectateTurnResult 调一次)。
func (p *Player) AddSpectateTurnResult() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.spectateTurnCount++
}

// GetSpectateTurnCount 返回观战视角收到的回合结算条数。
func (p *Player) GetSpectateTurnCount() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.spectateTurnCount
}

// SignalSpectateEnd 记录观战结束原因并广播"观战已结束"。
// 由 NotifySpectateEnd handler 调用;reason 为 ESpectateEndReason 的数值。
func (p *Player) SignalSpectateEnd(reason int32) {
	p.ensureSpectateEndChannel()
	p.mu.Lock()
	p.spectateEndReason = reason
	p.mu.Unlock()
	p.spectateEndOnce.Do(func() { close(p.spectateEnd) })
}

// WaitSpectateEnd 阻塞等待 NotifySpectateEnd,返回观战结束原因。
func (p *Player) WaitSpectateEnd(ctx context.Context) (int32, error) {
	p.ensureSpectateEndChannel()
	p.mu.RLock()
	ch := p.spectateEnd
	p.mu.RUnlock()
	select {
	case <-ch:
		p.mu.RLock()
		defer p.mu.RUnlock()
		return p.spectateEndReason, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// playerMap is a concurrent-safe map of player ID → *Player.
type playerMap struct {
	mu sync.RWMutex
	m  map[uint64]*Player
}

// PlayerList is the global player registry used by message handlers.
var PlayerList = &playerMap{m: make(map[uint64]*Player)}

func (pm *playerMap) Get(id uint64) (*Player, bool) {
	pm.mu.RLock()
	defer pm.mu.RUnlock()
	p, ok := pm.m[id]
	return p, ok
}

func (pm *playerMap) Set(id uint64, p *Player) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.m[id] = p
}

func (pm *playerMap) Delete(id uint64) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	delete(pm.m, id)
}

// ---------------------------------------------------------------------------
// 属性加点冒烟(attribute-smoke)信号
// ---------------------------------------------------------------------------

func (p *Player) ensureAttrChannel() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.attrReady == nil {
		p.attrReady = make(chan struct{})
	}
}

// SetAttributePanel 记录服务器下发的全量面板(每个属性 RPC 的响应与主动推送都会调)。
// seq 自增,让等待方能等到"比某个时刻更新"的那一份;messageId 记来源,让等待方只认
// "本次请求的响应"——GmSetPlayerLevel 会先推一份(170)再回响应(175),不带来源就会错位消费。
func (p *Player) SetAttributePanel(panel *scene.AttributePanelInfo, tipId uint32, messageId uint32) {
	p.ensureAttrChannel()
	p.mu.Lock()
	p.attrLastTip = tipId
	if panel != nil {
		p.attrPanel = panel
		p.attrPanelSeq++
		p.attrPanelMsg = messageId
	}
	p.mu.Unlock()
	if panel != nil {
		p.attrReadyOnce.Do(func() { close(p.attrReady) })
	}
}

// SetAttributeSuggestion 记录自动加点建议(只算不落,不动面板)。
func (p *Player) SetAttributeSuggestion(poolId uint32, suggested map[uint32]uint32, tipId uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.attrSuggestPool = poolId
	p.attrSuggested = suggested
	p.attrLastTip = tipId
}

// GetAttributeSuggestion 返回最近一次自动加点建议。
func (p *Player) GetAttributeSuggestion() (uint32, map[uint32]uint32) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.attrSuggestPool, p.attrSuggested
}

// GetAttributePanel 返回最近一份面板与它的序号(序号用于等待"更新的一份")。
func (p *Player) GetAttributePanel() (*scene.AttributePanelInfo, uint64) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.attrPanel, p.attrPanelSeq
}

// GetAttributePanelSource 返回最近一份面板的来源消息号(响应 = 请求号;推送 = 170)。
func (p *Player) GetAttributePanelSource() uint32 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.attrPanelMsg
}

// GetAttributeLastTip 返回最近一次属性 RPC 的 tip id(0 = 成功)。
func (p *Player) GetAttributeLastTip() uint32 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.attrLastTip
}

// WaitAttributePanelAfter 等到面板序号超过 sinceSeq 的那一份(传 0 即"等第一份")。
// 轮询而非条件变量:冒烟脚本用,50ms 粒度足够,省一套 sync.Cond 状态。
func (p *Player) WaitAttributePanelAfter(ctx context.Context, sinceSeq uint64) (*scene.AttributePanelInfo, uint64, error) {
	p.ensureAttrChannel()
	for {
		p.mu.RLock()
		panel, seq := p.attrPanel, p.attrPanelSeq
		p.mu.RUnlock()
		if panel != nil && seq > sinceSeq {
			return panel, seq, nil
		}
		select {
		case <-ctx.Done():
			return nil, seq, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
