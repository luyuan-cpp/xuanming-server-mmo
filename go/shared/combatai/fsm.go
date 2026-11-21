package combatai

import "strings"

// 事件驱动有限状态机（完整移植 looplab/fsm 核心设计，纯 Go 标准库，与 C++ modules/combat_ai/fsm.h 对齐）。
//
// 核心能力：
//   1. 声明式多源转移表（AddEvent / AddEvents）：一条规则支持多个 Src 映射到同一 Dst，另支持 AddAnyTransition。
//   2. 完整 5 阶段回调管线：
//        BeforeEvent / BeforeAnyEvent  （可 ev.Cancel(errCode) 零副作用取消）
//          -> LeaveState / LeaveAnyState / OnLeave （可 ev.Cancel(errCode) 或 ev.Async() 异步挂起）
//          -> 状态提交（current = dst）+ 规则 OnTransition
//          -> OnEnter / EnterState / EnterAnyState
//          -> AfterEvent / AfterAnyEvent
//   3. 异步/两阶段迁移（ev.Async() + CommitPendingTransition / CancelPendingTransition）：
//      离开旧态需等待异步操作时挂起迁移，期间新 Trigger fail-closed 返回 FsmStatusInProgress。
//   4. 自省与可视化：CanTrigger / AvailableTransitions / ExportMermaid（stateDiagram-v2）。
//   5. 重入保护：同步回调内再调 Trigger 进入有界延迟队列顺序消化，避免递归破坏状态不变量。

const MaxFsmDeferredEvents = 16

type FsmStatus uint8

const (
	FsmStatusOK           FsmStatus = 0 // 同步转移完成
	FsmStatusAsyncPending FsmStatus = 1 // Leave 回调调用了 ev.Async()，等待 Commit / Cancel
	FsmStatusNoTransition FsmStatus = 2 // 无匹配转移或 Guard 未通过
	FsmStatusCanceled     FsmStatus = 3 // Before 或 Leave 回调调用了 ev.Cancel(errCode)
	FsmStatusInProgress   FsmStatus = 4 // 已有挂起的异步转移未收尾，拒绝并发新事件（fail-closed）
	FsmStatusQueueFull    FsmStatus = 5 // 回调内重入队列已满（fail-closed）
)

type FsmResult struct {
	Status      FsmStatus
	CancelError uint32
}

func (r FsmResult) Ok() bool {
	return r.Status == FsmStatusOK
}

func (r FsmResult) PendingAsync() bool {
	return r.Status == FsmStatusAsyncPending
}

// FsmEventView 回调阶段可读写的事件视图（控制 Cancel / Async 权限边界）。
type FsmEventView[S comparable, E comparable] struct {
	event       E
	src         S
	dst         S
	allowCancel bool
	allowAsync  bool
	canceled    bool
	async       bool
	cancelError uint32
}

func (v *FsmEventView[S, E]) Event() E           { return v.event }
func (v *FsmEventView[S, E]) Src() S             { return v.src }
func (v *FsmEventView[S, E]) Dst() S             { return v.dst }
func (v *FsmEventView[S, E]) Canceled() bool     { return v.canceled }
func (v *FsmEventView[S, E]) CancelError() uint32 { return v.cancelError }
func (v *FsmEventView[S, E]) IsAsync() bool      { return v.async }

// Cancel 在 Before* 或 Leave* 阶段中止本次转移（fail-closed，状态不改变，不进入 Enter/After）。
func (v *FsmEventView[S, E]) Cancel(errorCode uint32) {
	if !v.allowCancel {
		return
	}
	v.canceled = true
	v.cancelError = errorCode
	v.async = false
}

// Async 在 Leave* 阶段声明异步挂起：等待外部完成后调用 CommitPendingTransition / CancelPendingTransition。
func (v *FsmEventView[S, E]) Async() {
	if !v.allowAsync || v.canceled {
		return
	}
	v.async = true
}

func (v *FsmEventView[S, E]) setPhasePermissions(allowCancel, allowAsync bool) {
	v.allowCancel = allowCancel
	v.allowAsync = allowAsync
}

// EventDesc looplab/fsm 风格的声明式事件描述：一条描述将多个源状态映射到同一目标状态。
type EventDesc[S comparable, E comparable, Ctx any] struct {
	Event        E
	Srcs         []S
	Dst          S
	Guard        func(ctx Ctx) bool
	OnTransition func(from, to S, event E, ctx Ctx)
}

type transitionKey[S comparable, E comparable] struct {
	from  S
	event E
}

type transitionRule[S comparable, E comparable, Ctx any] struct {
	from         S
	event        E
	to           S
	guard        func(ctx Ctx) bool
	onTransition func(from, to S, event E, ctx Ctx)
}

type anyTransitionRule[S comparable, E comparable, Ctx any] struct {
	to           S
	guard        func(ctx Ctx) bool
	onTransition func(from, to S, event E, ctx Ctx)
	allowSelf    bool
}

type fsmMatchResult[S comparable] struct {
	matched   bool
	isAnyRule bool
	ruleIndex int
	to        S
}

type pendingAsyncTransition[S comparable, E comparable] struct {
	from  S
	event E
	match fsmMatchResult[S]
}

// Fsm 泛型事件驱动有限状态机（单协程所有权，不内置锁，无热路径反射）。
type Fsm[S comparable, E comparable, Ctx any] struct {
	current       S
	inTransition  bool
	pendingAsync  *pendingAsyncTransition[S, E]
	deferredQueue [MaxFsmDeferredEvents]E
	deferredHead  int
	deferredCount int

	transitions     map[transitionKey[S, E]][]transitionRule[S, E, Ctx]
	transitionOrder []transitionKey[S, E]
	anyTransitions  map[E][]anyTransitionRule[S, E, Ctx]
	anyEventOrder   []E
	eventOrder      []E
	eventSeen       map[E]struct{}

	onBeforeEvent    map[E]func(*FsmEventView[S, E], Ctx)
	onBeforeAnyEvent func(*FsmEventView[S, E], Ctx)
	onLeave          map[S]func(Ctx)
	onLeaveEvent     map[S]func(*FsmEventView[S, E], Ctx)
	onLeaveAnyState  func(*FsmEventView[S, E], Ctx)
	onEnter          map[S]func(Ctx)
	onEnterEvent     map[S]func(*FsmEventView[S, E], Ctx)
	onEnterAnyState  func(*FsmEventView[S, E], Ctx)
	onAfterEvent     map[E]func(*FsmEventView[S, E], Ctx)
	onAfterAnyEvent  func(*FsmEventView[S, E], Ctx)
}

func NewFsm[S comparable, E comparable, Ctx any](initialState S) *Fsm[S, E, Ctx] {
	return &Fsm[S, E, Ctx]{
		current:        initialState,
		transitions:    make(map[transitionKey[S, E]][]transitionRule[S, E, Ctx]),
		anyTransitions: make(map[E][]anyTransitionRule[S, E, Ctx]),
		eventSeen:      make(map[E]struct{}),
		onBeforeEvent:  make(map[E]func(*FsmEventView[S, E], Ctx)),
		onLeave:        make(map[S]func(Ctx)),
		onLeaveEvent:   make(map[S]func(*FsmEventView[S, E], Ctx)),
		onEnter:        make(map[S]func(Ctx)),
		onEnterEvent:   make(map[S]func(*FsmEventView[S, E], Ctx)),
		onAfterEvent:   make(map[E]func(*FsmEventView[S, E], Ctx)),
	}
}

func (f *Fsm[S, E, Ctx]) trackEvent(event E) {
	if _, exists := f.eventSeen[event]; !exists {
		f.eventSeen[event] = struct{}{}
		f.eventOrder = append(f.eventOrder, event)
	}
}

// AddTransition 注册显式转移规则。同一 (from, event) 允许配多条不同 Guard，按注册顺序取首条命中。
func (f *Fsm[S, E, Ctx]) AddTransition(
	from S,
	event E,
	to S,
	guard func(ctx Ctx) bool,
	onTransition func(from, to S, event E, ctx Ctx),
) {
	key := transitionKey[S, E]{from: from, event: event}
	if _, exists := f.transitions[key]; !exists {
		f.transitionOrder = append(f.transitionOrder, key)
	}
	f.transitions[key] = append(f.transitions[key], transitionRule[S, E, Ctx]{
		from:         from,
		event:        event,
		to:           to,
		guard:        guard,
		onTransition: onTransition,
	})
	f.trackEvent(event)
}

// AddEvent looplab/fsm 风格：一条事件批量绑定多个源状态 -> 目标状态。
func (f *Fsm[S, E, Ctx]) AddEvent(desc EventDesc[S, E, Ctx]) {
	for _, src := range desc.Srcs {
		f.AddTransition(src, desc.Event, desc.Dst, desc.Guard, desc.OnTransition)
	}
}

func (f *Fsm[S, E, Ctx]) AddEvents(descs ...EventDesc[S, E, Ctx]) {
	for _, desc := range descs {
		f.AddEvent(desc)
	}
}

// AddAnyTransition 注册任意态可响应的全局转移（优先级低于显式规则；allowSelf=false 时已在目标态则忽略）。
func (f *Fsm[S, E, Ctx]) AddAnyTransition(
	event E,
	to S,
	guard func(ctx Ctx) bool,
	onTransition func(from, to S, event E, ctx Ctx),
	allowSelf bool,
) {
	if _, exists := f.anyTransitions[event]; !exists {
		f.anyEventOrder = append(f.anyEventOrder, event)
	}
	f.anyTransitions[event] = append(f.anyTransitions[event], anyTransitionRule[S, E, Ctx]{
		to:           to,
		guard:        guard,
		onTransition: onTransition,
		allowSelf:    allowSelf,
	})
	f.trackEvent(event)
}

// ---- 5 阶段回调注册 ----

func (f *Fsm[S, E, Ctx]) OnBeforeEvent(event E, hook func(*FsmEventView[S, E], Ctx)) {
	f.onBeforeEvent[event] = hook
}

func (f *Fsm[S, E, Ctx]) OnBeforeAnyEvent(hook func(*FsmEventView[S, E], Ctx)) {
	f.onBeforeAnyEvent = hook
}

func (f *Fsm[S, E, Ctx]) OnLeave(state S, hook func(Ctx)) {
	f.onLeave[state] = hook
}

func (f *Fsm[S, E, Ctx]) OnLeaveState(state S, hook func(*FsmEventView[S, E], Ctx)) {
	f.onLeaveEvent[state] = hook
}

func (f *Fsm[S, E, Ctx]) OnLeaveAnyState(hook func(*FsmEventView[S, E], Ctx)) {
	f.onLeaveAnyState = hook
}

func (f *Fsm[S, E, Ctx]) OnEnter(state S, hook func(Ctx)) {
	f.onEnter[state] = hook
}

func (f *Fsm[S, E, Ctx]) OnEnterState(state S, hook func(*FsmEventView[S, E], Ctx)) {
	f.onEnterEvent[state] = hook
}

func (f *Fsm[S, E, Ctx]) OnEnterAnyState(hook func(*FsmEventView[S, E], Ctx)) {
	f.onEnterAnyState = hook
}

func (f *Fsm[S, E, Ctx]) OnAfterEvent(event E, hook func(*FsmEventView[S, E], Ctx)) {
	f.onAfterEvent[event] = hook
}

func (f *Fsm[S, E, Ctx]) OnAfterAnyEvent(hook func(*FsmEventView[S, E], Ctx)) {
	f.onAfterAnyEvent = hook
}

// ---- 查询与可视化 ----

func (f *Fsm[S, E, Ctx]) CanTrigger(event E, ctx Ctx) bool {
	if f.pendingAsync != nil {
		return false
	}
	return f.findMatch(f.current, event, ctx).matched
}

// AvailableTransitions 返回当前状态下所有可触发的事件（按事件首次注册顺序，完全确定性）。
func (f *Fsm[S, E, Ctx]) AvailableTransitions(ctx Ctx) []E {
	if f.pendingAsync != nil {
		return nil
	}
	var out []E
	for _, ev := range f.eventOrder {
		if f.findMatch(f.current, ev, ctx).matched {
			out = append(out, ev)
		}
	}
	return out
}

// ExportMermaid 导出 Mermaid stateDiagram-v2 流程图文本。
func (f *Fsm[S, E, Ctx]) ExportMermaid(stateName func(S) string, eventName func(E) string) string {
	var sb strings.Builder
	sb.WriteString("stateDiagram-v2\n")
	for _, key := range f.transitionOrder {
		for _, rule := range f.transitions[key] {
			sb.WriteString("    ")
			sb.WriteString(stateName(rule.from))
			sb.WriteString(" --> ")
			sb.WriteString(stateName(rule.to))
			sb.WriteString(" : ")
			sb.WriteString(eventName(rule.event))
			sb.WriteByte('\n')
		}
	}
	for _, ev := range f.anyEventOrder {
		for _, rule := range f.anyTransitions[ev] {
			sb.WriteString("    [*] --> ")
			sb.WriteString(stateName(rule.to))
			sb.WriteString(" : ")
			sb.WriteString(eventName(ev))
			sb.WriteString(" (any)\n")
		}
	}
	return sb.String()
}

// ---- 触发与异步收尾 ----

func (f *Fsm[S, E, Ctx]) TriggerEx(event E, ctx Ctx) FsmResult {
	if f.pendingAsync != nil {
		return FsmResult{Status: FsmStatusInProgress}
	}
	if f.inTransition {
		if f.deferredCount >= MaxFsmDeferredEvents {
			return FsmResult{Status: FsmStatusQueueFull}
		}
		f.deferredQueue[(f.deferredHead+f.deferredCount)%MaxFsmDeferredEvents] = event
		f.deferredCount++
		return FsmResult{Status: FsmStatusOK}
	}

	first := f.executeSingle(event, ctx)
	if first.Status == FsmStatusAsyncPending {
		return first
	}
	f.drainDeferredQueue(ctx)
	return first
}

func (f *Fsm[S, E, Ctx]) Trigger(event E, ctx Ctx) bool {
	res := f.TriggerEx(event, ctx)
	return res.Ok() || res.PendingAsync()
}

func (f *Fsm[S, E, Ctx]) HasPendingTransition() bool {
	return f.pendingAsync != nil
}

// CommitPendingTransition 完成先前由 ev.Async() 挂起的迁移：Commit -> OnTransition -> Enter* -> After* -> 消化重入队列。
func (f *Fsm[S, E, Ctx]) CommitPendingTransition(ctx Ctx) FsmResult {
	if f.pendingAsync == nil {
		return FsmResult{Status: FsmStatusNoTransition}
	}
	if f.inTransition {
		return FsmResult{Status: FsmStatusInProgress}
	}
	pending := *f.pendingAsync
	f.pendingAsync = nil

	f.finishCommitAndAfter(pending.from, pending.match, pending.event, ctx)
	f.drainDeferredQueue(ctx)
	return FsmResult{Status: FsmStatusOK}
}

// CancelPendingTransition 放弃先前挂起的异步迁移，保持原状态不变。
func (f *Fsm[S, E, Ctx]) CancelPendingTransition() bool {
	if f.pendingAsync == nil {
		return false
	}
	f.pendingAsync = nil
	f.deferredHead = 0
	f.deferredCount = 0
	return true
}

func (f *Fsm[S, E, Ctx]) Reset(state S) {
	f.current = state
	f.inTransition = false
	f.pendingAsync = nil
	f.deferredHead = 0
	f.deferredCount = 0
}

func (f *Fsm[S, E, Ctx]) Current() S {
	return f.current
}

func (f *Fsm[S, E, Ctx]) Is(state S) bool {
	return f.current == state
}

func (f *Fsm[S, E, Ctx]) findMatch(from S, event E, ctx Ctx) fsmMatchResult[S] {
	specific := f.transitions[transitionKey[S, E]{from: from, event: event}]
	for i, rule := range specific {
		if rule.guard == nil || rule.guard(ctx) {
			return fsmMatchResult[S]{matched: true, isAnyRule: false, ruleIndex: i, to: rule.to}
		}
	}
	globals := f.anyTransitions[event]
	for i, rule := range globals {
		if !rule.allowSelf && rule.to == from {
			continue
		}
		if rule.guard == nil || rule.guard(ctx) {
			return fsmMatchResult[S]{matched: true, isAnyRule: true, ruleIndex: i, to: rule.to}
		}
	}
	return fsmMatchResult[S]{}
}

func (f *Fsm[S, E, Ctx]) executeSingle(event E, ctx Ctx) FsmResult {
	from := f.current
	match := f.findMatch(from, event, ctx)
	if !match.matched {
		return FsmResult{Status: FsmStatusNoTransition}
	}

	ev := FsmEventView[S, E]{event: event, src: from, dst: match.to}

	runBeforeAndLeave := func() (FsmResult, bool) {
		f.inTransition = true
		defer func() { f.inTransition = false }()

		// 阶段 1：BeforeEvent -> BeforeAnyEvent（允许 Cancel，不允许 Async）
		ev.setPhasePermissions(true, false)
		if hook := f.onBeforeEvent[event]; hook != nil {
			hook(&ev, ctx)
			if ev.Canceled() {
				return FsmResult{Status: FsmStatusCanceled, CancelError: ev.CancelError()}, true
			}
		}
		if f.onBeforeAnyEvent != nil {
			f.onBeforeAnyEvent(&ev, ctx)
			if ev.Canceled() {
				return FsmResult{Status: FsmStatusCanceled, CancelError: ev.CancelError()}, true
			}
		}

		// 阶段 2：LeaveState -> LeaveAnyState -> OnLeave（允许 Cancel 与 Async；先跑可取消钩子）
		ev.setPhasePermissions(true, true)
		if hook := f.onLeaveEvent[from]; hook != nil {
			hook(&ev, ctx)
			if ev.Canceled() {
				return FsmResult{Status: FsmStatusCanceled, CancelError: ev.CancelError()}, true
			}
		}
		if f.onLeaveAnyState != nil {
			f.onLeaveAnyState(&ev, ctx)
			if ev.Canceled() {
				return FsmResult{Status: FsmStatusCanceled, CancelError: ev.CancelError()}, true
			}
		}
		if simpleLeave := f.onLeave[from]; simpleLeave != nil {
			simpleLeave(ctx)
		}

		if ev.IsAsync() {
			f.pendingAsync = &pendingAsyncTransition[S, E]{from: from, event: event, match: match}
			return FsmResult{Status: FsmStatusAsyncPending}, true
		}
		return FsmResult{}, false
	}

	if res, stopped := runBeforeAndLeave(); stopped {
		return res
	}

	// 阶段 3~5：Commit -> Enter -> After
	f.finishCommitAndAfter(from, match, event, ctx)
	return FsmResult{Status: FsmStatusOK}
}

func (f *Fsm[S, E, Ctx]) finishCommitAndAfter(from S, match fsmMatchResult[S], event E, ctx Ctx) {
	ev := FsmEventView[S, E]{event: event, src: from, dst: match.to}
	ev.setPhasePermissions(false, false)

	f.inTransition = true
	defer func() { f.inTransition = false }()

	// 阶段 3：提交新状态 + 规则转移回调
	f.current = match.to
	if match.isAnyRule {
		if hook := f.anyTransitions[event][match.ruleIndex].onTransition; hook != nil {
			hook(from, match.to, event, ctx)
		}
	} else {
		if hook := f.transitions[transitionKey[S, E]{from: from, event: event}][match.ruleIndex].onTransition; hook != nil {
			hook(from, match.to, event, ctx)
		}
	}

	// 阶段 4：OnEnter -> EnterState -> EnterAnyState
	if simpleEnter := f.onEnter[match.to]; simpleEnter != nil {
		simpleEnter(ctx)
	}
	if hook := f.onEnterEvent[match.to]; hook != nil {
		hook(&ev, ctx)
	}
	if f.onEnterAnyState != nil {
		f.onEnterAnyState(&ev, ctx)
	}

	// 阶段 5：AfterEvent -> AfterAnyEvent
	if hook := f.onAfterEvent[event]; hook != nil {
		hook(&ev, ctx)
	}
	if f.onAfterAnyEvent != nil {
		f.onAfterAnyEvent(&ev, ctx)
	}
}

func (f *Fsm[S, E, Ctx]) drainDeferredQueue(ctx Ctx) {
	drained := 0
	for f.deferredCount > 0 && drained < MaxFsmDeferredEvents && f.pendingAsync == nil {
		next := f.deferredQueue[f.deferredHead]
		f.deferredHead = (f.deferredHead + 1) % MaxFsmDeferredEvents
		f.deferredCount--
		drained++
		if res := f.executeSingle(next, ctx); res.PendingAsync() {
			break
		}
	}
}
