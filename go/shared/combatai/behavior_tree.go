package combatai

// 数据驱动行为树（融合 BehaviorTree.CPP 响应式抢占/Halt 与 behavior3go 静态树+实例黑板分离，
// 纯 Go 标准库，与 C++ modules/combat_ai/behavior_tree.h 语义逐项对齐）。

type BtStatus uint8

const (
	BtSuccess BtStatus = 0
	BtFailure BtStatus = 1
	BtRunning BtStatus = 2
)

type BtNodeID uint32

const InvalidBtNode BtNodeID = ^BtNodeID(0)

// Blackboard 整型键类型安全黑板：避免热路径字符串分配。
type Blackboard struct {
	entries map[uint32]any
}

func (b *Blackboard) ensureMap() {
	if b.entries == nil {
		b.entries = make(map[uint32]any)
	}
}

func (b *Blackboard) SetInt64(key uint32, v int64)   { b.ensureMap(); b.entries[key] = v }
func (b *Blackboard) SetUint64(key uint32, v uint64) { b.ensureMap(); b.entries[key] = v }
func (b *Blackboard) SetFloat64(key uint32, v float64) {
	b.ensureMap()
	b.entries[key] = v
}
func (b *Blackboard) SetBool(key uint32, v bool) { b.ensureMap(); b.entries[key] = v }

func (b *Blackboard) GetInt64(key uint32) (int64, bool) {
	v, ok := b.entries[key].(int64)
	return v, ok
}

func (b *Blackboard) GetInt64Or(key uint32, fallback int64) int64 {
	if v, ok := b.GetInt64(key); ok {
		return v
	}
	return fallback
}

func (b *Blackboard) GetUint64(key uint32) (uint64, bool) {
	v, ok := b.entries[key].(uint64)
	return v, ok
}

func (b *Blackboard) GetUint64Or(key uint32, fallback uint64) uint64 {
	if v, ok := b.GetUint64(key); ok {
		return v
	}
	return fallback
}

func (b *Blackboard) GetFloat64(key uint32) (float64, bool) {
	v, ok := b.entries[key].(float64)
	return v, ok
}

func (b *Blackboard) GetFloat64Or(key uint32, fallback float64) float64 {
	if v, ok := b.GetFloat64(key); ok {
		return v
	}
	return fallback
}

func (b *Blackboard) GetBool(key uint32) (bool, bool) {
	v, ok := b.entries[key].(bool)
	return v, ok
}

func (b *Blackboard) GetBoolOr(key uint32, fallback bool) bool {
	if v, ok := b.GetBool(key); ok {
		return v
	}
	return fallback
}

func (b *Blackboard) Has(key uint32) bool {
	_, ok := b.entries[key]
	return ok
}

func (b *Blackboard) Erase(key uint32) {
	delete(b.entries, key)
}

func (b *Blackboard) Clear() {
	clear(b.entries)
}

type btNodeMemory struct {
	runningChild      uint32
	repeatCount       uint32
	cooldownReadyTick uint64
	isRunning         bool
}

// BehaviorTreeState 每个实体独享的运行态快照。
type BehaviorTreeState struct {
	TickCount  uint64
	memory     []btNodeMemory
	Blackboard Blackboard
}

func (s *BehaviorTreeState) ResetRuntime() {
	clear(s.memory)
}

type btNodeKind uint8

const (
	btKindSequence btNodeKind = iota
	btKindSelector
	btKindReactiveSequence
	btKindReactiveSelector
	btKindParallel
	btKindInverter
	btKindForceSuccess
	btKindRepeat
	btKindCooldown
	btKindCondition
	btKindAction
)

type btNodeDef[Ctx any] struct {
	kind       btNodeKind
	childBegin uint32
	childCount uint32
	paramA     uint32
	paramB     uint32
	condition  func(ctx Ctx, bb *Blackboard) bool
	action     func(ctx Ctx, bb *Blackboard) BtStatus
	onHalt     func(ctx Ctx, bb *Blackboard)
}

// BehaviorTreeDef 共享只读行为树拓扑定义。
type BehaviorTreeDef[Ctx any] struct {
	root         BtNodeID
	nodes        []btNodeDef[Ctx]
	childIndices []BtNodeID
}

func (d *BehaviorTreeDef[Ctx]) Valid() bool {
	return d != nil && d.root != InvalidBtNode && uint64(d.root) < uint64(len(d.nodes))
}

func (d *BehaviorTreeDef[Ctx]) CreateState() BehaviorTreeState {
	return BehaviorTreeState{
		memory: make([]btNodeMemory, len(d.nodes)),
	}
}

// Tick 推进一帧；多个实体共享同一份 BehaviorTreeDef，各自持有独立 BehaviorTreeState。
func (d *BehaviorTreeDef[Ctx]) Tick(ctx Ctx, state *BehaviorTreeState) BtStatus {
	if !d.Valid() || state == nil {
		return BtFailure
	}
	if len(state.memory) != len(d.nodes) {
		state.memory = make([]btNodeMemory, len(d.nodes))
	}
	state.TickCount++
	return d.tickNode(d.root, ctx, state)
}

// HaltAll 外部强制中止整棵树（如怪物死亡 / 脱战重置 / 被强控）。
func (d *BehaviorTreeDef[Ctx]) HaltAll(ctx Ctx, state *BehaviorTreeState) {
	if !d.Valid() || state == nil || len(state.memory) != len(d.nodes) {
		return
	}
	d.haltNode(d.root, ctx, state)
}

func (d *BehaviorTreeDef[Ctx]) haltNode(nodeID BtNodeID, ctx Ctx, state *BehaviorTreeState) {
	mem := &state.memory[nodeID]
	if !mem.isRunning {
		return
	}
	node := &d.nodes[nodeID]
	for i := uint32(0); i < node.childCount; i++ {
		d.haltNode(d.childIndices[node.childBegin+i], ctx, state)
	}
	if node.kind == btKindAction && node.onHalt != nil {
		node.onHalt(ctx, &state.Blackboard)
	}
	mem.isRunning = false
	mem.runningChild = 0
	mem.repeatCount = 0
}

func (d *BehaviorTreeDef[Ctx]) tickNode(nodeID BtNodeID, ctx Ctx, state *BehaviorTreeState) BtStatus {
	node := &d.nodes[nodeID]
	mem := &state.memory[nodeID]
	result := BtFailure

	switch node.kind {
	case btKindCondition:
		if node.condition != nil && node.condition(ctx, &state.Blackboard) {
			result = BtSuccess
		}

	case btKindAction:
		if node.action != nil {
			result = node.action(ctx, &state.Blackboard)
		}

	case btKindSequence:
		start := uint32(0)
		if mem.isRunning {
			start = mem.runningChild
		}
		result = BtSuccess
		for i := start; i < node.childCount; i++ {
			childStatus := d.tickNode(d.childIndices[node.childBegin+i], ctx, state)
			if childStatus == BtRunning {
				mem.runningChild = i
				result = BtRunning
				break
			}
			if childStatus == BtFailure {
				mem.runningChild = 0
				result = BtFailure
				break
			}
		}
		if result != BtRunning {
			mem.runningChild = 0
		}

	case btKindSelector:
		start := uint32(0)
		if mem.isRunning {
			start = mem.runningChild
		}
		result = BtFailure
		for i := start; i < node.childCount; i++ {
			childStatus := d.tickNode(d.childIndices[node.childBegin+i], ctx, state)
			if childStatus == BtRunning {
				mem.runningChild = i
				result = BtRunning
				break
			}
			if childStatus == BtSuccess {
				mem.runningChild = 0
				result = BtSuccess
				break
			}
		}
		if result != BtRunning {
			mem.runningChild = 0
		}

	case btKindReactiveSequence:
		result = BtSuccess
		for i := uint32(0); i < node.childCount; i++ {
			childStatus := d.tickNode(d.childIndices[node.childBegin+i], ctx, state)
			if childStatus != BtSuccess {
				for j := i + 1; j < node.childCount; j++ {
					d.haltNode(d.childIndices[node.childBegin+j], ctx, state)
				}
				result = childStatus
				break
			}
		}

	case btKindReactiveSelector:
		result = BtFailure
		for i := uint32(0); i < node.childCount; i++ {
			childStatus := d.tickNode(d.childIndices[node.childBegin+i], ctx, state)
			if childStatus != BtFailure {
				for j := i + 1; j < node.childCount; j++ {
					d.haltNode(d.childIndices[node.childBegin+j], ctx, state)
				}
				result = childStatus
				break
			}
		}

	case btKindParallel:
		var successCount, failureCount uint32
		successNeed := node.paramA
		if successNeed == 0 {
			successNeed = node.childCount
		}
		failureNeed := node.paramB
		if failureNeed == 0 {
			failureNeed = 1
		}
		for i := uint32(0); i < node.childCount; i++ {
			switch d.tickNode(d.childIndices[node.childBegin+i], ctx, state) {
			case BtSuccess:
				successCount++
			case BtFailure:
				failureCount++
			}
		}
		switch {
		case successCount >= successNeed:
			result = BtSuccess
		case failureCount >= failureNeed:
			result = BtFailure
		default:
			result = BtRunning
		}
		if result != BtRunning {
			for i := uint32(0); i < node.childCount; i++ {
				d.haltNode(d.childIndices[node.childBegin+i], ctx, state)
			}
		}

	case btKindInverter:
		if node.childCount == 1 {
			switch d.tickNode(d.childIndices[node.childBegin], ctx, state) {
			case BtRunning:
				result = BtRunning
			case BtSuccess:
				result = BtFailure
			default:
				result = BtSuccess
			}
		}

	case btKindForceSuccess:
		if node.childCount == 1 {
			if d.tickNode(d.childIndices[node.childBegin], ctx, state) == BtRunning {
				result = BtRunning
			} else {
				result = BtSuccess
			}
		}

	case btKindRepeat:
		if node.childCount == 1 {
			child := d.childIndices[node.childBegin]
			result = BtSuccess
			for node.paramA == 0 || mem.repeatCount < node.paramA {
				childStatus := d.tickNode(child, ctx, state)
				if childStatus == BtRunning {
					result = BtRunning
					break
				}
				if childStatus == BtFailure {
					mem.repeatCount = 0
					result = BtFailure
					break
				}
				mem.repeatCount++
				if node.paramA == 0 {
					result = BtRunning
					break
				}
			}
			if result != BtRunning {
				mem.repeatCount = 0
			}
		}

	case btKindCooldown:
		if node.childCount == 1 {
			if !mem.isRunning && state.TickCount < mem.cooldownReadyTick {
				result = BtFailure
				break
			}
			result = d.tickNode(d.childIndices[node.childBegin], ctx, state)
			if result == BtSuccess {
				mem.cooldownReadyTick = state.TickCount + uint64(node.paramA)
			}
		}
	}

	mem.isRunning = result == BtRunning
	return result
}

type btDraftNode[Ctx any] struct {
	kind      btNodeKind
	paramA    uint32
	paramB    uint32
	children  []BtNodeID
	condition func(ctx Ctx, bb *Blackboard) bool
	action    func(ctx Ctx, bb *Blackboard) BtStatus
	onHalt    func(ctx Ctx, bb *Blackboard)
}

// BehaviorTreeBuilder 构树器：Build 阶段扁平化子节点表并做三色 DFS 环路检测（fail-closed）。
type BehaviorTreeBuilder[Ctx any] struct {
	drafts []btDraftNode[Ctx]
}

func NewBehaviorTreeBuilder[Ctx any]() *BehaviorTreeBuilder[Ctx] {
	return &BehaviorTreeBuilder[Ctx]{}
}

func (b *BehaviorTreeBuilder[Ctx]) pushDraft(node btDraftNode[Ctx]) BtNodeID {
	id := BtNodeID(len(b.drafts))
	b.drafts = append(b.drafts, node)
	return id
}

func (b *BehaviorTreeBuilder[Ctx]) addComposite(kind btNodeKind, children []BtNodeID) BtNodeID {
	cp := make([]BtNodeID, len(children))
	copy(cp, children)
	return b.pushDraft(btDraftNode[Ctx]{kind: kind, children: cp})
}

func (b *BehaviorTreeBuilder[Ctx]) addDecorator(kind btNodeKind, child BtNodeID, paramA uint32) BtNodeID {
	return b.pushDraft(btDraftNode[Ctx]{kind: kind, paramA: paramA, children: []BtNodeID{child}})
}

func (b *BehaviorTreeBuilder[Ctx]) Sequence(children ...BtNodeID) BtNodeID {
	return b.addComposite(btKindSequence, children)
}

func (b *BehaviorTreeBuilder[Ctx]) Selector(children ...BtNodeID) BtNodeID {
	return b.addComposite(btKindSelector, children)
}

func (b *BehaviorTreeBuilder[Ctx]) ReactiveSequence(children ...BtNodeID) BtNodeID {
	return b.addComposite(btKindReactiveSequence, children)
}

func (b *BehaviorTreeBuilder[Ctx]) ReactiveSelector(children ...BtNodeID) BtNodeID {
	return b.addComposite(btKindReactiveSelector, children)
}

func (b *BehaviorTreeBuilder[Ctx]) Parallel(successThreshold, failureThreshold uint32, children ...BtNodeID) BtNodeID {
	id := b.addComposite(btKindParallel, children)
	b.drafts[id].paramA = successThreshold
	b.drafts[id].paramB = failureThreshold
	return id
}

func (b *BehaviorTreeBuilder[Ctx]) Inverter(child BtNodeID) BtNodeID {
	return b.addDecorator(btKindInverter, child, 0)
}

func (b *BehaviorTreeBuilder[Ctx]) ForceSuccess(child BtNodeID) BtNodeID {
	return b.addDecorator(btKindForceSuccess, child, 0)
}

func (b *BehaviorTreeBuilder[Ctx]) Repeat(maxCount uint32, child BtNodeID) BtNodeID {
	return b.addDecorator(btKindRepeat, child, maxCount)
}

func (b *BehaviorTreeBuilder[Ctx]) Cooldown(cooldownTicks uint32, child BtNodeID) BtNodeID {
	return b.addDecorator(btKindCooldown, child, cooldownTicks)
}

func (b *BehaviorTreeBuilder[Ctx]) Condition(fn func(ctx Ctx, bb *Blackboard) bool) BtNodeID {
	return b.pushDraft(btDraftNode[Ctx]{kind: btKindCondition, condition: fn})
}

func (b *BehaviorTreeBuilder[Ctx]) Action(
	tickFn func(ctx Ctx, bb *Blackboard) BtStatus,
	haltFn func(ctx Ctx, bb *Blackboard),
) BtNodeID {
	return b.pushDraft(btDraftNode[Ctx]{kind: btKindAction, action: tickFn, onHalt: haltFn})
}

func (b *BehaviorTreeBuilder[Ctx]) validateAcyclic(id BtNodeID, color []uint8) bool {
	if uint64(id) >= uint64(len(b.drafts)) {
		return false
	}
	if color[id] == 1 {
		return false
	}
	if color[id] == 2 {
		return true
	}
	color[id] = 1
	for _, child := range b.drafts[id].children {
		if !b.validateAcyclic(child, color) {
			return false
		}
	}
	color[id] = 2
	return true
}

// Build 编译为不可变扁平树；根节点非法或检测到环路返回 (nil, false)。
func (b *BehaviorTreeBuilder[Ctx]) Build(root BtNodeID) (*BehaviorTreeDef[Ctx], bool) {
	if uint64(root) >= uint64(len(b.drafts)) {
		return nil, false
	}
	color := make([]uint8, len(b.drafts))
	if !b.validateAcyclic(root, color) {
		return nil, false
	}

	def := &BehaviorTreeDef[Ctx]{
		root:  root,
		nodes: make([]btNodeDef[Ctx], 0, len(b.drafts)),
	}
	for _, draft := range b.drafts {
		out := btNodeDef[Ctx]{
			kind:       draft.kind,
			childBegin: uint32(len(def.childIndices)),
			childCount: uint32(len(draft.children)),
			paramA:     draft.paramA,
			paramB:     draft.paramB,
			condition:  draft.condition,
			action:     draft.action,
			onHalt:     draft.onHalt,
		}
		def.childIndices = append(def.childIndices, draft.children...)
		def.nodes = append(def.nodes, out)
	}
	return def, true
}
