package commerce

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"

	assetpb "proto/common/asset"
	tradepb "proto/trade"
	"trade/internal/data"
)

// 人工恢复入口(docs/design/jubaozhai-market.md §15.7,手册 docs/ops/trade-commerce-quarantine-runbook.md §4.2)。
//
// 合同转人工之后不在 Due 里,引擎不再推进。人工处理只改合同行:在合同行锁和版本条件下改 stage、status,
// 必要时加几条还没发出的腿(op_id=0)。改完合同重新到期,发号、写 outbox、投递、记结局都由引擎下一轮的 advance 做,
// 与玩家操作走同一条写路径,锁序、按角色名只建一次的腿、原归属身份核对都不变。人工处理不碰 op 行,
// 卡死的单条指令先用 go/trade/cmd/assetopfix 终结。

// 转人工的原因类别,写进 TradeCommerceQuarantine.reason,决定能做哪种人工处理。
const (
	manualReasonCorrupt         = "corrupt"             // 合同自身持久数据坏了(tick 隔离)
	manualReasonBadCounterparty = "bad_counterparty"    // 盘口上的对手单不可能成交,被移出盘口
	manualReasonAppliedPartial  = "applied_partial"     // 某条腿部分发放
	manualReasonFinalCredit     = "final_credit_failed" // 最终发放腿没有全部到账
	manualReasonMarketFill      = "market_fill_manual"  // 市场单名下有转人工的成交子单
	manualReasonRmbReturn       = "rmb_return_failed"   // 人民币寄售退回失败
)

// quarantineDetailMaxBytes 是转人工记录里错误原文的上限。
const quarantineDetailMaxBytes = 256

// manualOperatorMaxBytes、manualReasonMaxBytes 与资产指令人工终结同口径(assetop.ManualResolution)。
const (
	manualOperatorMaxBytes = 64
	manualReasonMaxBytes   = 191
)

// markManual 把合同转人工(status=Manual、stage="manual"),并把转人工之前的阶段、状态和原因记进 workflow:
// 人工恢复按它判断能做哪种处理,取证也不用再翻日志。已经在 manual 的不覆盖第一次的记录。
func markManual(r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow, reason, detail string, nowMs uint64) {
	if w.Stage != "manual" {
		w.Quarantine = &tradepb.TradeCommerceQuarantine{
			Stage:  w.Stage,
			Status: r.Status,
			Reason: reason,
			Detail: truncateUTF8(detail, quarantineDetailMaxBytes),
			AtMs:   nowMs,
		}
	}
	r.Status = Manual
	w.Stage = "manual"
}

func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// ManualAction 是一种人工处理。
type ManualAction string

const (
	// ManualResume 回到转人工之前的阶段,由引擎重新判定。用于原因已经消除的情况(数据修好了、卡死的腿已终结、
	// 子单已处理)。原因还在的话引擎会再次转人工,不会出错。
	ManualResume ManualAction = "resume"
	// ManualReissue 给没到账的最终发放腿各补发一条,原样的资产、原样的收件人。原腿留作证据,标上被谁取代。
	ManualReissue ManualAction = "reissue"
	// ManualRefund 把已扣进托管的资产原样退回给扣出它的人,合同按取消结案。只在还没有任何发放到账时可用。
	ManualRefund ManualAction = "refund"
)

// ParseManualAction 解析命令行写的动作名。
func ParseManualAction(s string) (ManualAction, bool) {
	switch a := ManualAction(s); a {
	case ManualResume, ManualReissue, ManualRefund:
		return a, true
	}
	return "", false
}

// ManualRequest 是一次人工处理。ExpectVersion 是操作人看过的合同版本:版本变了说明合同已被处理或推进过,
// 不能在旧观察上做决定;同一条命令重跑也因此只生效一次。
type ManualRequest struct {
	CommerceID    uint64
	ExpectVersion uint64
	Action        ManualAction
	Operator      string
	Reason        string
}

func (q ManualRequest) validate() error {
	if q.CommerceID == 0 || q.ExpectVersion == 0 {
		return notAllowed("commerce_id 和 expect-version 都必须给")
	}
	if _, ok := ParseManualAction(string(q.Action)); !ok {
		return notAllowed("未知动作 %q", q.Action)
	}
	for _, field := range []struct {
		name  string
		value string
		limit int
	}{{"operator", q.Operator, manualOperatorMaxBytes}, {"reason", q.Reason, manualReasonMaxBytes}} {
		if strings.TrimSpace(field.value) == "" || len(field.value) > field.limit {
			return notAllowed("%s 必须非空且不超过 %d 字节", field.name, field.limit)
		}
		if strings.IndexFunc(field.value, unicode.IsControl) >= 0 {
			return notAllowed("%s 不能带换行或控制字符", field.name)
		}
	}
	return nil
}

// ManualResult 是人工处理的结果;没执行时是计划。
type ManualResult struct {
	VersionBefore  uint64
	Quarantine     *tradepb.TradeCommerceQuarantine
	Stage          string
	Status         uint32
	NewLegs        []string
	SupersededLegs []string
}

var (
	// ErrManualNotAllowed 包装前置条件不满足的原因,命令行按「拒绝,未改动」退出。
	ErrManualNotAllowed = errors.New("人工处理前置条件不满足")
	// ErrManualVersionChanged:合同已不是操作人看过的那一版,未改动。
	ErrManualVersionChanged = errors.New("合同版本已变化,先重新查看再决定")
	// ErrManualNotFound:合同不存在。
	ErrManualNotFound = errors.New("合同不存在")
)

func notAllowed(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrManualNotAllowed, fmt.Sprintf(format, args...))
}

// ResolveManual 在合同行锁下执行一次人工处理。execute=false 时只算计划,不写库(同样要锁合同行,
// 计划才和执行时看到的一致)。
//
// 步骤:锁合同行 → 核对版本 → 把已有终局、还没记进 workflow 的腿记上(与 advance 同一判定)→
// 按动作改合同(planManualAction)→ 追加留痕 → 带版本条件保存。保存时 updated_ms 写成现在,合同立即重新到期。
func ResolveManual(ctx context.Context, repo *data.CommerceRepo, req ManualRequest, nowMs uint64, execute bool) (ManualResult, error) {
	if err := req.validate(); err != nil {
		return ManualResult{}, err
	}
	var result ManualResult
	err := repo.Tx(ctx, func(tx *sql.Tx) error {
		result = ManualResult{}
		r, err := repo.Lock(ctx, tx, req.CommerceID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrManualNotFound
		}
		if err != nil {
			return err
		}
		if r.Version != req.ExpectVersion {
			return fmt.Errorf("%w(现在是 %d,操作人看的是 %d)", ErrManualVersionChanged, r.Version, req.ExpectVersion)
		}
		w, err := data.Workflow(r)
		if err != nil {
			return notAllowed("workflow 解不开,只能取证后手工修数据: %v", err)
		}
		read := func(ctx context.Context, opID uint64) (data.LegOutcome, error) { return repo.LegResult(ctx, tx, opID) }
		if err := settleLegOutcomes(ctx, w, read); err != nil {
			return err
		}
		versionBefore := r.Version
		result, err = planManualAction(r, w, req.Action)
		if err != nil {
			return err
		}
		result.VersionBefore = versionBefore
		if !execute {
			return nil
		}
		w.ManualActions = append(w.ManualActions, &tradepb.TradeCommerceManualAction{
			Action:         string(req.Action),
			Operator:       req.Operator,
			Reason:         req.Reason,
			AtMs:           nowMs,
			VersionBefore:  versionBefore,
			Quarantine:     result.Quarantine,
			NewLegs:        result.NewLegs,
			SupersededLegs: result.SupersededLegs,
		})
		return repo.Save(ctx, tx, r, w, nowMs)
	})
	return result, err
}

// PlanManualReadOnly 不加锁地算一份计划,给命令行的 show 用:列出每种动作现在能不能做、为什么。
// 结论只供查看,执行时 ResolveManual 在锁下重新判定。
func PlanManualReadOnly(ctx context.Context, r *tradepb.TradeCommerceRecord, read func(context.Context, uint64) (data.LegOutcome, error), action ManualAction) (ManualResult, error) {
	w, err := data.Workflow(r)
	if err != nil {
		return ManualResult{}, notAllowed("workflow 解不开,只能取证后手工修数据: %v", err)
	}
	if err := settleLegOutcomes(ctx, w, read); err != nil {
		return ManualResult{}, err
	}
	return planManualAction(proto.Clone(r).(*tradepb.TradeCommerceRecord), w, action)
}

// settleLegOutcomes 把已经有终局、还没记进 workflow 的腿记上。转人工的合同不再走 advance,
// 隔离时被截止的托管扣款之后才有结局,所以人工处理之前要补记一次,判定与 advance 相同(recordLegOutcome)。
// 还在途的腿、查不到 outbox 行的腿、部分发放的腿都拒绝:前两者要等结局或取证,部分发放要逐件核对。
func settleLegOutcomes(ctx context.Context, w *tradepb.TradeCommerceWorkflow, read func(context.Context, uint64) (data.LegOutcome, error)) error {
	for _, l := range w.Legs {
		if l.SupersededBy != "" || l.Outcome != 0 {
			continue
		}
		if l.OpId == 0 {
			return notAllowed("腿 %s 还没发出", l.Role)
		}
		out, err := read(ctx, l.OpId)
		if errors.Is(err, sql.ErrNoRows) {
			return notAllowed("腿 %s op_id=%d 的资产待办行不存在,先取证", l.Role, l.OpId)
		}
		if err != nil {
			return err
		}
		if out.Pending() {
			return notAllowed("腿 %s op_id=%d 还在途,等它有结局(卡死的用 assetopfix 终结)", l.Role, l.OpId)
		}
		if out.Status == tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED_PARTIAL {
			return notAllowed("腿 %s op_id=%d 部分发放,要按资产通道 §4.33 逐件核对,不能整单处理", l.Role, l.OpId)
		}
		if err := recordLegOutcome(l, out); err != nil {
			return notAllowed("%v", err)
		}
	}
	return nil
}

// recordLegOutcome 把一条终局记进腿:结局和托管快照。advance 与人工处理共用。
func recordLegOutcome(l *tradepb.TradeCommerceLeg, out data.LegOutcome) error {
	if !out.Final() {
		return fmt.Errorf("腿 %s op_id=%d 的资产待办状态非法 (%d)", l.Role, l.OpId, int32(out.Status))
	}
	l.Outcome = uint32(out.Status)
	if len(out.Snapshot) > 0 {
		snapshot := &assetpb.AssetSnapshot{}
		if err := proto.Unmarshal(out.Snapshot, snapshot); err != nil {
			return fmt.Errorf("腿 %s op_id=%d 的托管快照损坏: %w", l.Role, l.OpId, err)
		}
		l.Snapshot = snapshot
	}
	return nil
}

// planManualAction 按动作改合同(r、w 原地改),回结果。纯函数:只看合同和腿的结局,不读库。
// 调用前腿的结局都已记上(settleLegOutcomes)。
func planManualAction(r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow, action ManualAction) (ManualResult, error) {
	if r.Status != Manual || w.Stage != "manual" {
		return ManualResult{}, notAllowed("合同不在人工状态(status=%d stage=%q)", r.Status, w.Stage)
	}
	q := w.GetQuarantine()
	if q == nil || q.Stage == "" {
		return ManualResult{}, notAllowed("合同没有转人工记录,只能取证后手工处理")
	}
	result := ManualResult{Quarantine: proto.Clone(q).(*tradepb.TradeCommerceQuarantine)}
	switch action {
	case ManualResume:
		if reissuable(q) {
			return ManualResult{}, notAllowed("发放没到账,重新判定还是转人工,用 reissue 或 refund")
		}
		w.Stage, r.Status = q.Stage, q.Status
	case ManualReissue:
		if !reissuable(q) {
			return ManualResult{}, notAllowed("reissue 只处理发放没到账的合同(原因 %s,原阶段 %s)", q.Reason, q.Stage)
		}
		failed := failedCredits(w)
		if len(failed) == 0 {
			return ManualResult{}, notAllowed("没有没到账的发放腿")
		}
		for _, l := range failed {
			role := reissueRole(w, l.Role)
			l.SupersededBy = role
			addLeg(w, role, l.PlayerId, 4, proto.Clone(l.Bundle).(*assetpb.AssetBundle))
			result.NewLegs = append(result.NewLegs, role)
			result.SupersededLegs = append(result.SupersededLegs, l.Role)
		}
		w.Stage, r.Status = q.Stage, q.Status
	case ManualRefund:
		if err := refundable(r, w); err != nil {
			return ManualResult{}, err
		}
		for _, l := range failedCredits(w) {
			l.SupersededBy = string(ManualRefund)
			result.SupersededLegs = append(result.SupersededLegs, l.Role)
		}
		legs := append([]*tradepb.TradeCommerceLeg(nil), w.Legs...)
		for _, l := range legs {
			if l.Stream != 3 || !applied(l) {
				continue
			}
			role := l.Role + "/refund"
			if err := transferLeg(w, role, l.PlayerId, l); err != nil {
				return ManualResult{}, notAllowed("腿 %s 已扣出但托管原件转不动,要按 player_snapshot 人工补发: %v", l.Role, err)
			}
			result.NewLegs = append(result.NewLegs, role)
		}
		w.Stage, r.Status = "return", CancelPending
	default:
		return ManualResult{}, notAllowed("未知动作 %q", action)
	}
	if len(result.NewLegs) > maxNewLegsPerStep {
		return ManualResult{}, notAllowed("要新建 %d 条腿,超过引擎单步上限 %d", len(result.NewLegs), maxNewLegsPerStep)
	}
	result.Stage, result.Status = w.Stage, r.Status
	return result, nil
}

// reissuable 报告转人工的原因是不是「发放没到账」:最终发放腿(deliver / return 阶段),或人民币寄售的退回腿。
// 补发之后回到原阶段,原阶段的判定沿 currentLeg 找到补发腿。
func reissuable(q *tradepb.TradeCommerceQuarantine) bool {
	switch q.GetReason() {
	case manualReasonFinalCredit:
		return q.GetStage() == "deliver" || q.GetStage() == "return"
	case manualReasonRmbReturn:
		return true
	}
	return false
}

// refundable 是整单退回的前置条件。不满足时说明退回会和别处的资产记账冲突,要人工判定。
func refundable(r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow) error {
	switch r.Kind {
	case 1:
		return notAllowed("官方商城交付的退款走人民币订单,不在这里")
	case 6:
		return notAllowed("人民币寄售有自己的退回流程")
	case BidKind:
		return notAllowed("竞价的冻结款可能已被拍品结算引用,要和拍品一起人工判定")
	case 4:
		if r.Side == 0 {
			return notAllowed("成交子单的资产来自两张母单,不能单独退回")
		}
		if r.FilledQuantity > 0 {
			return notAllowed("市场单已经部分成交,托管里只剩一部分,要人工判定")
		}
	case 5:
		if w.HighestBidder != 0 {
			return notAllowed("拍品已有出价,托管与竞价合同互相引用,要人工判定")
		}
	}
	for _, l := range w.Legs {
		if l.Stream == 4 && l.SupersededBy == "" && applied(l) {
			return notAllowed("发放腿 %s 已经到账,不能整单退回;没到账的用 reissue", l.Role)
		}
	}
	return nil
}

// failedCredits 是没到账、也没被取代的发放腿。
func failedCredits(w *tradepb.TradeCommerceWorkflow) []*tradepb.TradeCommerceLeg {
	var out []*tradepb.TradeCommerceLeg
	for _, l := range w.Legs {
		if l.Stream == 4 && l.SupersededBy == "" && done(l) && !applied(l) {
			out = append(out, l)
		}
	}
	return out
}

// reissueRole 是补发腿的名字:原名加 "#2",已经是补发腿的序号加一,跳过已有的名字。
// 名字在合同里唯一,addLeg 按名字只建一次。
func reissueRole(w *tradepb.TradeCommerceWorkflow, role string) string {
	base, n := role, 1
	if i := strings.LastIndex(role, "#"); i >= 0 {
		if k, err := strconv.Atoi(role[i+1:]); err == nil && k >= 2 {
			base, n = role[:i], k
		}
	}
	for k := n + 1; ; k++ {
		candidate := base + "#" + strconv.Itoa(k)
		if leg(w, candidate) == nil {
			return candidate
		}
	}
}
