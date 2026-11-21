package combatai

import "math/bits"

// 位图状态互斥与打断矩阵（纯 Go 标准库，零外部依赖，与 C++ modules/combat_ai/state_mutex_matrix.h 语义逐项对齐）。
//
// 适用场景：
//   - 正交可叠加状态（战斗中 / 跟随 / 骑乘 / 沉默 / 眩晕等）的 O(1) 准入校验与打断。
//   - 规范化规则表（StateRule: {Action, State, Mode, Tip}）为一等入口，新增状态只需加行，不依赖定长槽位。
//   - 显式携带 Allowed 与调用方传入的 successTip（兼容全仓 kSuccess = 1000 与独立单测 0）。
//   - 两阶段执行：先纯只读校验，全部通过后再依次打断并派发退出钩子，最后入态，拒绝路径零副作用。

const (
	MaxMutexStates uint32 = 64
	NoMutexState   uint32 = ^uint32(0)
)

// StateMode 统一互斥模式枚举（0=互斥, 1=允许, 2=打断）。
type StateMode uint8

const (
	StateModeBlock     StateMode = 0
	StateModeAllow     StateMode = 1
	StateModeInterrupt StateMode = 2
)

// StateExitReason 状态退出原因。
type StateExitReason uint8

const (
	StateExitRemoved     StateExitReason = 0 // 显式移除状态
	StateExitInterrupted StateExitReason = 1 // 被另一动作按互斥表打断
)

// StateRule 规范化单条互斥规则：显式携带 Action 与 State 编号，消除定长槽位下标错位。
type StateRule struct {
	Action uint32
	State  uint32
	Mode   StateMode
	Tip    uint32
}

// LegacySlotEntry 兼容定长槽位行（第 i 槽隐式对应 state = i）。
type LegacySlotEntry struct {
	Mode uint32
	Tip  uint32
}

// ActionMutexPolicy 加载期预编译后的单动作位图策略。
type ActionMutexPolicy struct {
	BlockMask     uint64
	InterruptMask uint64
	BlockTips     [MaxMutexStates]uint32
}

// MutexEvaluateResult 纯只读评估结果。
type MutexEvaluateResult struct {
	Allowed       bool
	Tip           uint32
	InterruptMask uint64
}

// RefCountedStateBitset 支持多来源叠加（如多个 Buff 同时施加沉默/眩晕）的引用计数位图。
// 计数 0 -> 1 置位，1 -> 0 清位；溢出按 uint16 上限饱和截断，防回绕导致状态误消失。
type RefCountedStateBitset struct {
	activeMask uint64
	refCounts  [MaxMutexStates]uint16
}

// Acquire 叠加一层状态，仅在 0 -> 1 翻位时返回 true。
func (b *RefCountedStateBitset) Acquire(state uint32) bool {
	if state >= MaxMutexStates {
		return false
	}
	becameActive := b.refCounts[state] == 0
	if b.refCounts[state] < ^uint16(0) {
		b.refCounts[state]++
	}
	if becameActive {
		b.activeMask |= uint64(1) << state
	}
	return becameActive
}

// Release 释放一层状态，仅在 1 -> 0 彻底退出时返回 true。
func (b *RefCountedStateBitset) Release(state uint32) bool {
	if state >= MaxMutexStates || b.refCounts[state] == 0 {
		return false
	}
	b.refCounts[state]--
	if b.refCounts[state] == 0 {
		b.activeMask &^= uint64(1) << state
		return true
	}
	return false
}

// ClearState 强制清零某个状态（例如被驱散或被互斥动作打断），返回原先是否处于激活态。
func (b *RefCountedStateBitset) ClearState(state uint32) bool {
	if state >= MaxMutexStates {
		return false
	}
	wasActive := b.refCounts[state] != 0
	b.refCounts[state] = 0
	b.activeMask &^= uint64(1) << state
	return wasActive
}

// Reset 清空全部状态与引用计数。
func (b *RefCountedStateBitset) Reset() {
	b.activeMask = 0
	clear(b.refCounts[:])
}

func (b *RefCountedStateBitset) Has(state uint32) bool {
	return state < MaxMutexStates && (b.activeMask&(uint64(1)<<state)) != 0
}

func (b *RefCountedStateBitset) RefCount(state uint32) uint16 {
	if state >= MaxMutexStates {
		return 0
	}
	return b.refCounts[state]
}

func (b *RefCountedStateBitset) Mask() uint64 {
	return b.activeMask
}

// StateExitHook 状态退出收尾回调。
type StateExitHook[Entity any] func(entity Entity, reason StateExitReason)

// StateExitHookRegistry 状态退出钩子注册表：按状态编号 O(1) 派发子系统收尾（脱跟随 / 下马 / 停施法）。
type StateExitHookRegistry[Entity any] struct {
	hooks [MaxMutexStates]StateExitHook[Entity]
}

func (r *StateExitHookRegistry[Entity]) Register(state uint32, hook StateExitHook[Entity]) {
	if state >= MaxMutexStates {
		return
	}
	r.hooks[state] = hook
}

func (r *StateExitHookRegistry[Entity]) Unregister(state uint32) {
	if state >= MaxMutexStates {
		return
	}
	r.hooks[state] = nil
}

func (r *StateExitHookRegistry[Entity]) Clear() {
	clear(r.hooks[:])
}

func (r *StateExitHookRegistry[Entity]) NotifyExited(entity Entity, state uint32, reason StateExitReason) {
	if state >= MaxMutexStates {
		return
	}
	if hook := r.hooks[state]; hook != nil {
		hook(entity, reason)
	}
}

// StateMutexMatrix 编译期/加载期构建的 O(1) 动作 × 状态互斥矩阵。
// 线程模型：加载期构建一次，热路径只读。
type StateMutexMatrix struct {
	policies map[uint32]*ActionMutexPolicy
}

func NewStateMutexMatrix() *StateMutexMatrix {
	return &StateMutexMatrix{
		policies: make(map[uint32]*ActionMutexPolicy),
	}
}

func (m *StateMutexMatrix) Clear() {
	clear(m.policies)
}

// EnsureAction 显式注册某动作（即使全部允许也能通过 HasAction 校验）。
func (m *StateMutexMatrix) EnsureAction(action uint32) {
	if m.policies == nil {
		m.policies = make(map[uint32]*ActionMutexPolicy)
	}
	if _, ok := m.policies[action]; !ok {
		m.policies[action] = &ActionMutexPolicy{}
	}
}

// SetRule 按显式 (action, state) 写入单条规则，不依赖槽位位置。
func (m *StateMutexMatrix) SetRule(action, state uint32, mode StateMode, tip uint32) bool {
	if state >= MaxMutexStates {
		return false
	}
	if m.policies == nil {
		m.policies = make(map[uint32]*ActionMutexPolicy)
	}
	policy, ok := m.policies[action]
	if !ok {
		policy = &ActionMutexPolicy{}
		m.policies[action] = policy
	}

	bit := uint64(1) << state
	policy.BlockMask &^= bit
	policy.InterruptMask &^= bit
	policy.BlockTips[state] = 0

	switch mode {
	case StateModeBlock:
		policy.BlockMask |= bit
		policy.BlockTips[state] = tip
		return true
	case StateModeAllow:
		return true
	case StateModeInterrupt:
		policy.InterruptMask |= bit
		return true
	default:
		return false
	}
}

// LoadRules 批量装载规范化规则列表；返回非法规则条数（0 表示全部合法）。
func (m *StateMutexMatrix) LoadRules(rules []StateRule) int {
	invalidCount := 0
	for _, rule := range rules {
		if !m.SetRule(rule.Action, rule.State, rule.Mode, rule.Tip) {
			invalidCount++
		}
	}
	return invalidCount
}

// LoadLegacyActionRow 兼容定长槽位行：第 i 槽对应 state = i。返回越界或非法 mode 的槽位数。
func (m *StateMutexMatrix) LoadLegacyActionRow(action uint32, slots []LegacySlotEntry) int {
	m.EnsureAction(action)
	invalidCount := 0
	for i, slot := range slots {
		if uint32(i) >= MaxMutexStates || slot.Mode > uint32(StateModeInterrupt) {
			invalidCount++
			continue
		}
		m.SetRule(action, uint32(i), StateMode(slot.Mode), slot.Tip)
	}
	return invalidCount
}

func (m *StateMutexMatrix) HasAction(action uint32) bool {
	_, ok := m.policies[action]
	return ok
}

func (m *StateMutexMatrix) ActionCount() int {
	return len(m.policies)
}

// Evaluate 纯只读判定：O(1) 位运算，零堆分配，零副作用。
// 存在多个互斥状态时，按最小状态编号（最低位）稳定返回对应 tip。
func (m *StateMutexMatrix) Evaluate(activeMask uint64, action, successTip, unknownActionTip uint32) MutexEvaluateResult {
	policy, ok := m.policies[action]
	if !ok {
		return MutexEvaluateResult{Allowed: false, Tip: unknownActionTip, InterruptMask: 0}
	}
	if blocked := activeMask & policy.BlockMask; blocked != 0 {
		firstBlocked := uint32(bits.TrailingZeros64(blocked))
		return MutexEvaluateResult{Allowed: false, Tip: policy.BlockTips[firstBlocked], InterruptMask: 0}
	}
	return MutexEvaluateResult{Allowed: true, Tip: successTip, InterruptMask: activeMask & policy.InterruptMask}
}

func (m *StateMutexMatrix) CanPerform(activeMask uint64, action, successTip, unknownActionTip uint32) uint32 {
	return m.Evaluate(activeMask, action, successTip, unknownActionTip).Tip
}

// ForEachBit 按升序遍历位掩码里的每一个置位状态编号（栈上纯整数循环，零分配）。
func ForEachBit(mask uint64, fn func(state uint32)) {
	for mask != 0 {
		state := uint32(bits.TrailingZeros64(mask))
		mask &= mask - 1
		fn(state)
	}
}

// TryPerform 两阶段原语：先纯校验（含入态边界），全部通过后才依次执行打断回调与入态回调。
func (m *StateMutexMatrix) TryPerform(
	activeMask uint64,
	action, enterState, maxValidState uint32,
	successTip, unknownActionTip, invalidStateTip uint32,
	onInterrupt func(interruptedState uint32),
	onEnter func(stateToEnter uint32),
) uint32 {
	if enterState != NoMutexState && (enterState >= maxValidState || enterState >= MaxMutexStates) {
		return invalidStateTip
	}
	eval := m.Evaluate(activeMask, action, successTip, unknownActionTip)
	if !eval.Allowed {
		return eval.Tip
	}
	if onInterrupt != nil {
		ForEachBit(eval.InterruptMask, onInterrupt)
	}
	if enterState != NoMutexState && onEnter != nil {
		onEnter(enterState)
	}
	return successTip
}
