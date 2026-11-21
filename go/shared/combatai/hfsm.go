package combatai

// 分层有限状态机（移植 HFSM2 核心思想，纯 Go 标准库，与 C++ modules/combat_ai/hfsm.h 语义逐项对齐）。
//
// 核心机制：
//   1. 扁平切片按 HfsmStateID(uint16) 索引，零链表指针。
//   2. 自动下潜初始子态（InitialChild）：切到父态自动落至叶子初始态。
//   3. LCA（最近公共祖先）切换：只退出 A -> LCA 之下，再进入 LCA 之下 -> B，共享父态不重复退出重入。
//   4. 事件冒泡（Dispatch）：从当前叶子态向父态传递，首个 Handled / TransitionTo 截断冒泡。
//   5. O(1) 活跃祖先位图（64 位快速路径，超 64 状态自动退化到祖先链遍历）。

type HfsmStateID uint16

const (
	InvalidHfsmState          HfsmStateID = ^HfsmStateID(0)
	MaxHfsmHierarchyDepth                 = 16
	MaxHfsmPendingTransitions             = 16
)

// HfsmEventOutcome 单层状态处理事件后的返回值。
type HfsmEventOutcome struct {
	Handled          bool
	HasTransition    bool
	TransitionTarget HfsmStateID
}

func HfsmIgnored() HfsmEventOutcome {
	return HfsmEventOutcome{}
}

func HfsmHandled() HfsmEventOutcome {
	return HfsmEventOutcome{Handled: true}
}

func HfsmTransitionTo(target HfsmStateID) HfsmEventOutcome {
	return HfsmEventOutcome{Handled: true, HasTransition: true, TransitionTarget: target}
}

// HfsmStateConfig 单个层级状态的声明。
// 注意：Go 零值 0 可能是合法状态号（如 Root=0），构造时请显式填 Parent / InitialChild 为 InvalidHfsmState，
// 或使用 NewHfsmStateConfig() 获取安全默认值。
type HfsmStateConfig[Ctx any, Ev any] struct {
	Parent       HfsmStateID
	InitialChild HfsmStateID
	OnEnter      func(ctx Ctx)
	OnUpdate     func(ctx Ctx, deltaMs uint32) (HfsmStateID, bool)
	OnExit       func(ctx Ctx)
	OnEvent      func(ev Ev, ctx Ctx) HfsmEventOutcome
}

// NewHfsmStateConfig 返回 Parent / InitialChild 均为 InvalidHfsmState 的安全默认配置。
func NewHfsmStateConfig[Ctx any, Ev any]() HfsmStateConfig[Ctx, Ev] {
	return HfsmStateConfig[Ctx, Ev]{
		Parent:       InvalidHfsmState,
		InitialChild: InvalidHfsmState,
	}
}

type hfsmStateSlot[Ctx any, Ev any] struct {
	registered bool
	config     HfsmStateConfig[Ctx, Ev]
}

// Hfsm 扁平层级有限状态机。
type Hfsm[Ctx any, Ev any] struct {
	states         []hfsmStateSlot[Ctx, Ev]
	currentLeaf    HfsmStateID
	activeMask     uint64
	started        bool
	inTransition   bool
	pendingTargets [MaxHfsmPendingTransitions]HfsmStateID
	pendingHead    int
	pendingCount   int
}

func NewHfsm[Ctx any, Ev any]() *Hfsm[Ctx, Ev] {
	return &Hfsm[Ctx, Ev]{
		currentLeaf: InvalidHfsmState,
	}
}

// AddState 注册状态定义（允许按任意顺序注册，Start 时统一做环路与深度校验）。
func (h *Hfsm[Ctx, Ev]) AddState(id HfsmStateID, cfg HfsmStateConfig[Ctx, Ev]) bool {
	if id == InvalidHfsmState {
		return false
	}
	idx := int(id)
	if idx >= len(h.states) {
		grown := make([]hfsmStateSlot[Ctx, Ev], idx+1)
		copy(grown, h.states)
		h.states = grown
	}
	h.states[idx] = hfsmStateSlot[Ctx, Ev]{registered: true, config: cfg}
	return true
}

// Start 校验层级无环后，自顶向下进入 initialState 及其叶子初始子态。
func (h *Hfsm[Ctx, Ev]) Start(initialState HfsmStateID, ctx Ctx) bool {
	if !h.validateHierarchy() || !h.hasState(initialState) {
		return false
	}
	targetLeaf, ok := h.resolveLeaf(initialState)
	if !ok {
		return false
	}

	h.started = true
	h.currentLeaf = InvalidHfsmState
	h.activeMask = 0

	var enterPath [MaxHfsmHierarchyDepth]HfsmStateID
	enterLen := h.buildRootToNodePath(targetLeaf, &enterPath)
	if enterLen == 0 {
		h.started = false
		return false
	}

	func() {
		h.inTransition = true
		defer func() { h.inTransition = false }()
		for i := 0; i < enterLen; i++ {
			h.currentLeaf = enterPath[i]
			h.rebuildActiveMask()
			if fn := h.states[enterPath[i]].config.OnEnter; fn != nil {
				fn(ctx)
			}
		}
	}()
	h.drainPendingTransitions(ctx)
	return true
}

// Update 按自顶向下顺序更新当前活跃链（父态先于子态），任意一层返回目标态即触发 LCA 切换。
func (h *Hfsm[Ctx, Ev]) Update(ctx Ctx, deltaMs uint32) {
	if !h.started || h.currentLeaf == InvalidHfsmState {
		return
	}
	var path [MaxHfsmHierarchyDepth]HfsmStateID
	pathLen := h.buildRootToNodePath(h.currentLeaf, &path)
	for i := 0; i < pathLen; i++ {
		stateID := path[i]
		if updateFn := h.states[stateID].config.OnUpdate; updateFn != nil {
			if next, ok := updateFn(ctx, deltaMs); ok {
				h.TransitionTo(next, ctx)
				return
			}
			if !h.IsInState(stateID) {
				return
			}
		}
	}
}

// Dispatch 事件冒泡：从当前叶子态逐级向父态派发，首个 Handled / TransitionTo 终止冒泡。
func (h *Hfsm[Ctx, Ev]) Dispatch(event Ev, ctx Ctx) bool {
	if !h.started || h.currentLeaf == InvalidHfsmState {
		return false
	}
	cursor := h.currentLeaf
	for steps := 0; cursor != InvalidHfsmState && steps < MaxHfsmHierarchyDepth; steps++ {
		parent := h.states[cursor].config.Parent
		if eventFn := h.states[cursor].config.OnEvent; eventFn != nil {
			outcome := eventFn(event, ctx)
			if outcome.HasTransition {
				h.TransitionTo(outcome.TransitionTarget, ctx)
				return true
			}
			if outcome.Handled {
				return true
			}
		}
		cursor = parent
	}
	return false
}

// TransitionTo LCA 切换：目标可为父态（自动下潜到其叶子初始态）或叶子态。
func (h *Hfsm[Ctx, Ev]) TransitionTo(target HfsmStateID, ctx Ctx) bool {
	if !h.started || !h.hasState(target) {
		return false
	}
	if h.inTransition {
		if h.pendingCount >= MaxHfsmPendingTransitions {
			return false
		}
		h.pendingTargets[(h.pendingHead+h.pendingCount)%MaxHfsmPendingTransitions] = target
		h.pendingCount++
		return true
	}

	ok := h.executeTransition(target, ctx)
	h.drainPendingTransitions(ctx)
	return ok
}

func (h *Hfsm[Ctx, Ev]) Started() bool {
	return h.started
}

func (h *Hfsm[Ctx, Ev]) CurrentLeaf() HfsmStateID {
	return h.currentLeaf
}

// IsInState O(1) 查询是否正处于某状态（包含其任意祖先态）。
func (h *Hfsm[Ctx, Ev]) IsInState(state HfsmStateID) bool {
	if !h.started || h.currentLeaf == InvalidHfsmState || !h.hasState(state) {
		return false
	}
	if state < 64 {
		return (h.activeMask & (uint64(1) << state)) != 0
	}
	cursor := h.currentLeaf
	for steps := 0; cursor != InvalidHfsmState && steps < MaxHfsmHierarchyDepth; steps++ {
		if cursor == state {
			return true
		}
		cursor = h.states[cursor].config.Parent
	}
	return false
}

func (h *Hfsm[Ctx, Ev]) hasState(id HfsmStateID) bool {
	idx := int(id)
	return idx < len(h.states) && h.states[idx].registered
}

func (h *Hfsm[Ctx, Ev]) validateHierarchy() bool {
	for i := range h.states {
		if !h.states[i].registered {
			continue
		}
		cursor := h.states[i].config.Parent
		depth := 1
		for cursor != InvalidHfsmState {
			if !h.hasState(cursor) || int(cursor) == i || depth >= MaxHfsmHierarchyDepth {
				return false
			}
			cursor = h.states[cursor].config.Parent
			depth++
		}
		child := h.states[i].config.InitialChild
		if child != InvalidHfsmState {
			if !h.hasState(child) || h.states[child].config.Parent != HfsmStateID(i) {
				return false
			}
		}
	}
	return true
}

func (h *Hfsm[Ctx, Ev]) resolveLeaf(state HfsmStateID) (HfsmStateID, bool) {
	cursor := state
	depth := 0
	for h.hasState(cursor) && h.states[cursor].config.InitialChild != InvalidHfsmState {
		depth++
		if depth >= MaxHfsmHierarchyDepth {
			return InvalidHfsmState, false
		}
		cursor = h.states[cursor].config.InitialChild
	}
	if !h.hasState(cursor) {
		return InvalidHfsmState, false
	}
	return cursor, true
}

func (h *Hfsm[Ctx, Ev]) buildRootToNodePath(node HfsmStateID, out *[MaxHfsmHierarchyDepth]HfsmStateID) int {
	var rev [MaxHfsmHierarchyDepth]HfsmStateID
	count := 0
	cursor := node
	for cursor != InvalidHfsmState {
		if !h.hasState(cursor) || count >= MaxHfsmHierarchyDepth {
			return 0
		}
		rev[count] = cursor
		count++
		cursor = h.states[cursor].config.Parent
	}
	for i := 0; i < count; i++ {
		out[i] = rev[count-1-i]
	}
	return count
}

func (h *Hfsm[Ctx, Ev]) rebuildActiveMask() {
	var mask uint64
	cursor := h.currentLeaf
	for steps := 0; cursor != InvalidHfsmState && steps < MaxHfsmHierarchyDepth; steps++ {
		if cursor < 64 {
			mask |= uint64(1) << cursor
		}
		cursor = h.states[cursor].config.Parent
	}
	h.activeMask = mask
}

func (h *Hfsm[Ctx, Ev]) executeTransition(target HfsmStateID, ctx Ctx) bool {
	targetLeaf, ok := h.resolveLeaf(target)
	if !ok {
		return false
	}
	var fromPath, toPath [MaxHfsmHierarchyDepth]HfsmStateID
	fromLen := h.buildRootToNodePath(h.currentLeaf, &fromPath)
	toLen := h.buildRootToNodePath(targetLeaf, &toPath)
	if fromLen == 0 || toLen == 0 {
		return false
	}

	lcaPrefix := 0
	for lcaPrefix < fromLen && lcaPrefix < toLen && fromPath[lcaPrefix] == toPath[lcaPrefix] {
		lcaPrefix++
	}
	if lcaPrefix == fromLen && lcaPrefix == toLen {
		lcaPrefix = fromLen - 1
	}

	h.inTransition = true
	defer func() { h.inTransition = false }()

	// 1) 自底向上退出旧分支
	for i := fromLen; i > lcaPrefix; i-- {
		exiting := fromPath[i-1]
		if exitFn := h.states[exiting].config.OnExit; exitFn != nil {
			exitFn(ctx)
		}
		if i >= 2 {
			h.currentLeaf = fromPath[i-2]
		} else {
			h.currentLeaf = InvalidHfsmState
		}
		h.rebuildActiveMask()
	}

	// 2) 自顶向下进入新分支
	for i := lcaPrefix; i < toLen; i++ {
		entering := toPath[i]
		h.currentLeaf = entering
		h.rebuildActiveMask()
		if enterFn := h.states[entering].config.OnEnter; enterFn != nil {
			enterFn(ctx)
		}
	}
	return true
}

func (h *Hfsm[Ctx, Ev]) drainPendingTransitions(ctx Ctx) {
	drained := 0
	for h.pendingCount > 0 && drained < MaxHfsmPendingTransitions {
		next := h.pendingTargets[h.pendingHead]
		h.pendingHead = (h.pendingHead + 1) % MaxHfsmPendingTransitions
		h.pendingCount--
		drained++
		h.executeTransition(next, ctx)
	}
}
