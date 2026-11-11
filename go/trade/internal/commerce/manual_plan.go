package commerce

// manual_plan.go —— 人工处置的判定:纯函数,只看锁内读到的合同、腿的结局、两方归属与关联合同,不读库。
// 入口、锁与写见 manual.go;每类情形的处置与理由见 docs/design/jubaozhai-market.md §15.7、§15.7.1。
//
// 所有判定先把拒绝条件查完再改 r、w(拒绝时合同原样不动);改的只有合同行上的字段与还没发出的新腿。

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"

	assetpb "proto/common/asset"
	dspb "proto/data_service"
	tradepb "proto/trade"
)

// partialOutcome 是 APPLIED_PARTIAL 记进腿上的结局值。只有 partial 动作会把它记进 workflow,而且同一步就处理掉
// (发放腿被缺口补发腿取代,扣款腿随整单退回结案),所以引擎的 finalCredits 不会看到没处理的部分发放腿。
const partialOutcome = uint32(tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED_PARTIAL)

// manualContext 是 planManualAction 除合同本身之外要看的锁内事实。
type manualContext struct {
	req ManualRequest
	// changes:合同两方里已不可逆地不归原身份的(换主、删角)。
	changes []partyRoleChange
	// target:redirect 的目标角色,已核过没删、没被冻结、不在投票中。
	target *dspb.AccountRoleOwnership
	// lot、lotW:出价所属的拍品。拍品不存在时两个都是 nil;拍品的 workflow 解不开时 lot 不为 nil 而 lotW 为 nil。
	lot  *tradepb.TradeCommerceRecord
	lotW *tradepb.TradeCommerceWorkflow
	// bid、bidW、bidChanges:void-auction 联动的出价(腿的结局已补记)与它那一方的归属。
	bid        *tradepb.TradeCommerceRecord
	bidW       *tradepb.TradeCommerceWorkflow
	bidChanges []partyRoleChange
}

// planManualAction 按动作改合同(r、w 原地改),回结果。调用前腿的结局都已补记(settleLegOutcomes)。
func planManualAction(r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow, c manualContext) (ManualResult, error) {
	if r.Status != Manual || w.Stage != "manual" {
		return ManualResult{}, notAllowed("合同不在人工状态(status=%d stage=%q)", r.Status, w.Stage)
	}
	q := w.GetQuarantine()
	if q == nil || q.Stage == "" {
		return ManualResult{}, notAllowed("合同没有转人工记录,只能取证后手工处理")
	}
	if c.req.Action != ManualRedirect && len(c.changes) > 0 {
		// 有一方已不归原身份时,引擎的每一步推进都会被原归属身份核对挡住,别的处置加的腿发不出去;先改绑。
		return ManualResult{}, notAllowed("参与方已不归原身份(%s),先用 redirect 改绑", describeRoleChanges(c.changes))
	}
	result := ManualResult{Quarantine: proto.Clone(q).(*tradepb.TradeCommerceQuarantine)}
	var err error
	switch c.req.Action {
	case ManualResume:
		err = planResume(r, w, q)
	case ManualReissue:
		err = planReissue(r, w, q, &result)
	case ManualRefund:
		err = planRefund(r, w, c, nil, &result)
	case ManualPartial:
		err = planPartial(r, w, q, c, &result)
	case ManualRedirect:
		err = planRedirect(r, w, q, c, &result)
	case ManualVoidAuction:
		err = planVoidAuction(r, w, c, &result)
	default:
		err = notAllowed("未知动作 %q", c.req.Action)
	}
	if err != nil {
		return ManualResult{}, err
	}
	if len(result.NewLegs) > maxNewLegsPerStep {
		return ManualResult{}, notAllowed("要新建 %d 条腿,超过引擎单步上限 %d", len(result.NewLegs), maxNewLegsPerStep)
	}
	result.Stage, result.Status = w.Stage, r.Status
	return result, nil
}

// planResume 回到转人工之前的阶段,由引擎重新判定。
func planResume(r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow, q *tradepb.TradeCommerceQuarantine) error {
	if reissuable(q) {
		return notAllowed("发放没到账,重新判定还是转人工,用 reissue 或 refund")
	}
	w.Stage, r.Status = q.Stage, q.Status
	return nil
}

// planReissue 给没到账的最终发放腿各补发一条:原样的资产、原样的收件人,原腿标被它取代;回到原阶段,
// 原阶段的判定(finalCredits)只看没被取代的腿。
func planReissue(r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow, q *tradepb.TradeCommerceQuarantine, result *ManualResult) error {
	if !reissuable(q) {
		return notAllowed("reissue 只处理发放没到账的合同(原因 %s,原阶段 %s)", q.Reason, q.Stage)
	}
	failed := failedCredits(w)
	if len(failed) == 0 {
		return notAllowed("没有没到账的发放腿")
	}
	for _, l := range failed {
		role := reissueRole(w, l.Role)
		l.SupersededBy = role
		addLeg(w, role, l.PlayerId, 4, proto.Clone(l.Bundle).(*assetpb.AssetBundle))
		result.NewLegs = append(result.NewLegs, role)
		result.SupersededLegs = append(result.SupersededLegs, l.Role)
	}
	w.Stage, r.Status = q.Stage, q.Status
	return nil
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

// failedCredits 是没到账、也没被取代的发放腿。部分发放的腿不在其中:它有一部分已到账,只能按缺口补(partial)。
func failedCredits(w *tradepb.TradeCommerceWorkflow) []*tradepb.TradeCommerceLeg {
	var out []*tradepb.TradeCommerceLeg
	for _, l := range w.Legs {
		if l.Stream == 4 && l.SupersededBy == "" && done(l) && !applied(l) && l.Outcome != partialOutcome {
			out = append(out, l)
		}
	}
	return out
}

// reissueRole 是补发腿的名字:原名加 "#2",已经是补发腿的序号加一,跳过已有的名字。
// 名字在合同里唯一,addLeg 按名字只建一次;退回腿补发之后仍按退回记(leg_tx_type.go 按 "#" 之前的部分认)。
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

// ── refund ──────────────────────────────────────────────────────

// refundLegRole 是整单退回给一条扣出腿加的退回腿的名字。leg_tx_type.go 的 isReturnLeg 按 "/refund" 后缀认退回腿。
func refundLegRole(debitRole string) string { return debitRole + "/refund" }

// isRefundLeg 报告一条腿是不是整单退回加的(或它的补发)。
func isRefundLeg(role string) bool {
	base, _, _ := strings.Cut(role, "#")
	return strings.HasSuffix(base, "/refund")
}

// plannedLeg 是一条要新建的腿。
type plannedLeg struct {
	role   string
	player uint64
	bundle *assetpb.AssetBundle
}

// planRefund 整单退回:每条已扣进托管的腿各退一条给扣出它的人,没到账的发放腿标被退回取代,stage 改 return,按取消结案。
// taken 是 partial 认定的部分扣出腿实际扣到的部分;整单退回本身传 nil。
func planRefund(r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow, c manualContext, taken map[string]*assetpb.AssetBundle, result *ManualResult) error {
	if err := refundBlockers(r, w, c); err != nil {
		return err
	}
	returns, err := refundReturns(r, w, taken)
	if err != nil {
		return err
	}
	applyRefund(r, w, returns, result)
	return nil
}

// refundBlockers 是整单退回的前置条件。不满足时说明退回会和别处的资产记账冲突,要换动作或人工判定。
func refundBlockers(r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow, c manualContext) error {
	switch r.Kind {
	case 1:
		return notAllowed("官方商城交付的退款走人民币订单,不在这里")
	case 6:
		return notAllowed("人民币寄售有自己的退回流程")
	case BidKind:
		// 被拍品引用的出价,冻结款归拍品结算(结拍时铸给卖家的货款就是它);没被引用的只属于出价人自己。
		// 转人工的出价不会再被拍品接受(acceptBid 只接 bid_ready),所以「不引用」确认之后不会再变。
		switch {
		case c.lot == nil:
		case c.lotW == nil:
			return notAllowed("拍品 %d 的 workflow 解不开,判不了它是否引用这条出价", c.lot.CommerceId)
		case c.lotW.WinningBidId == r.CommerceId:
			return notAllowed("拍品 %d 引用这条出价,冻结款归拍品结算:拍品已结案用 resume,拍品转人工用 void-auction", c.lot.CommerceId)
		}
	case 4:
		switch {
		case r.Side == 0:
			return notAllowed("成交子单的资产来自两张母单,不能单独退回;没到账的用 reissue 或 partial")
		case r.FilledQuantity > r.Quantity:
			return notAllowed("市场单已成交量 %d 超过总量 %d,数据自相矛盾,只能取证后手工处理", r.FilledQuantity, r.Quantity)
		case r.FilledQuantity > 0 && r.FilledQuantity == r.Quantity:
			return notAllowed("市场单已全部成交,托管里没有剩余可退;用 resume 等成交子单收尾")
		}
	case 5:
		if w.HighestBidder != 0 {
			return notAllowed("拍品已有出价,托管与竞价合同互相引用,用 void-auction")
		}
	}
	return refundCommon(w)
}

// refundCommon 是整单退回与作废拍卖共用的前置:没有发放到账、没有整单退回过。
// 已经整单退回过的合同再退一次,旧写法会把没到账的退回腿标「被退回取代」,再按同名加退回腿时因名字已存在什么也没加上,
// 合同按「没有发放腿」直接结案,托管再也不退(2026-11-11 修)。退回没到账用 reissue。
func refundCommon(w *tradepb.TradeCommerceWorkflow) error {
	for _, l := range w.Legs {
		if l.Stream != 4 || l.SupersededBy != "" {
			continue
		}
		if applied(l) {
			return notAllowed("发放腿 %s 已经到账,不能整单退回;没到账的用 reissue", l.Role)
		}
		if isRefundLeg(l.Role) {
			return notAllowed("合同已经整单退回过(腿 %s);退回没到账用 reissue", l.Role)
		}
	}
	return nil
}

// refundReturns 算整单退回要加的腿:每条已扣出的托管各退一条给扣出它的人。市场单只退剩余部分(marketRemainder,
// 已成交的部分归成交子单);部分扣出的腿退 taken 里给的实际扣到的部分。先全部算完再改合同,有一条转不动就一条都不加。
func refundReturns(r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow, taken map[string]*assetpb.AssetBundle) ([]plannedLeg, error) {
	var out []plannedLeg
	for _, l := range w.Legs {
		if l.Stream != 3 || l.SupersededBy != "" {
			continue
		}
		var b *assetpb.AssetBundle
		switch {
		case applied(l):
			var err error
			if b, err = Transfer(l.Bundle, l.Snapshot); err != nil {
				return nil, notAllowed("腿 %s 已扣出但托管原件转不动,要按 player_snapshot 人工补发: %v", l.Role, err)
			}
			if r.Kind == 4 && r.Side != 0 && l.Role == "seller_debit" {
				if b, err = marketRemainder(r); err != nil {
					return nil, notAllowed("%v", err)
				}
			}
		case l.Outcome == partialOutcome:
			if b = taken[l.Role]; b == nil {
				return nil, notAllowed("腿 %s 部分扣出,用 partial 给出它的缺口", l.Role)
			}
		default:
			continue // 没扣到,资产还在玩家身上
		}
		if emptyBundle(b) {
			continue
		}
		out = append(out, plannedLeg{role: refundLegRole(l.Role), player: l.PlayerId, bundle: b})
	}
	return out, nil
}

// applyRefund 把算好的退回加进合同。
func applyRefund(r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow, returns []plannedLeg, result *ManualResult) {
	for _, l := range failedCredits(w) {
		l.SupersededBy = string(ManualRefund)
		result.SupersededLegs = append(result.SupersededLegs, l.Role)
	}
	for _, p := range returns {
		addLeg(w, p.role, p.player, 4, p.bundle)
		result.NewLegs = append(result.NewLegs, p.role)
	}
	w.Stage, r.Status = "return", CancelPending
}

// ── partial ─────────────────────────────────────────────────────

// planPartial 按人认定的缺口处理部分发放 / 部分扣出的腿(§15.7.1 第 2 类)。一次要给出这张合同全部部分发放的腿:
// 漏掉的那条会被引擎当成没到账,再按全量补发就会把已到账的部分发两遍。
//   - 发放腿:补发一条只含缺口的新腿给原收件人,原腿记部分发放并标被它取代,回到转人工之前的阶段;
//   - 托管扣款腿:交易没有托管齐,按整单退回结案,这条腿只退实际扣到的部分。
func planPartial(r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow, q *tradepb.TradeCommerceQuarantine, c manualContext, result *ManualResult) error {
	if r.Kind == 1 || r.Kind == 6 {
		return notAllowed("官方商城与人民币寄售的部分发放要和商城订单一起处理,不在这里")
	}
	type shortfall struct {
		leg     *tradepb.TradeCommerceLeg
		missing *assetpb.AssetBundle
	}
	var credits []shortfall
	taken := map[string]*assetpb.AssetBundle{}
	named := make(map[string]bool, len(c.req.Parts))
	var parts []*tradepb.TradeCommercePartialPart
	for _, part := range c.req.Parts {
		l := leg(w, part.Leg)
		switch {
		case l == nil:
			return notAllowed("合同里没有腿 %s", part.Leg)
		case l.SupersededBy != "":
			return notAllowed("腿 %s 已被 %s 取代", l.Role, l.SupersededBy)
		case l.Outcome != partialOutcome:
			return notAllowed("腿 %s 不是部分发放(结局 %d)", l.Role, l.Outcome)
		}
		named[l.Role] = true
		if l.Stream == 4 {
			missing, err := creditShortfall(l.Bundle, part.Missing)
			if err != nil {
				return notAllowed("腿 %s: %v", l.Role, err)
			}
			credits = append(credits, shortfall{leg: l, missing: missing})
			parts = append(parts, &tradepb.TradeCommercePartialPart{Leg: l.Role, Missing: missing})
			continue
		}
		got, missing, err := debitTaken(l.Bundle, part.Missing)
		if err != nil {
			return notAllowed("腿 %s: %v", l.Role, err)
		}
		taken[l.Role] = got
		parts = append(parts, &tradepb.TradeCommercePartialPart{Leg: l.Role, Missing: missing})
	}
	for _, l := range w.Legs {
		if l.SupersededBy == "" && l.Outcome == partialOutcome && !named[l.Role] {
			return notAllowed("腿 %s 也是部分发放,要在同一次 partial 里一起给出缺口", l.Role)
		}
	}
	if len(credits) > 0 && len(taken) > 0 {
		return notAllowed("发放腿与托管扣款腿不能在一次 partial 里混着处理")
	}
	if len(taken) > 0 {
		if err := planRefund(r, w, c, taken, result); err != nil {
			return err
		}
		result.PartialParts = parts
		return nil
	}
	for _, s := range credits {
		role := reissueRole(w, s.leg.Role)
		s.leg.SupersededBy = role
		addLeg(w, role, s.leg.PlayerId, 4, s.missing)
		result.NewLegs = append(result.NewLegs, role)
		result.SupersededLegs = append(result.SupersededLegs, s.leg.Role)
	}
	result.PartialParts = parts
	w.Stage, r.Status = q.Stage, q.Status
	return nil
}

// assetUnit 是缺口校验里的一件资产:实例(按 guid)、宝宝、一条货币(按类型)。
type assetUnit struct {
	kind string // "instance" / "pet" / "currency"
	id   uint64
}

func (u assetUnit) String() string {
	if u.kind == "pet" {
		return "pet"
	}
	return u.kind + ":" + strconv.FormatUint(u.id, 10)
}

func missingUnits(m PartialMissing) map[assetUnit]bool {
	units := make(map[assetUnit]bool, len(m.Instances)+len(m.Currencies)+1)
	for _, id := range m.Instances {
		units[assetUnit{"instance", id}] = true
	}
	if m.Pet {
		units[assetUnit{"pet", 0}] = true
	}
	for _, t := range m.Currencies {
		units[assetUnit{"currency", uint64(t)}] = true
	}
	return units
}

// creditShortfall 校验发放腿的缺口,回缺口那部分资产(原样的实例原件、整条货币)。
//
// scene 的发放顺序固定(资产通道 §4.33,asset_op_system.cpp 的 Credit 应用):按数量的物品整批(失败就零改动)→
// 实例逐件 → 宝宝 → 货币逐条,有改动之后再失败才记部分发放,失败之后的都不做。所以缺口只能是
// 「实例(按包内顺序)、宝宝、货币(按包内顺序)」上的一个后缀,按数量的物品不会缺。不符合这个形状的缺口一律拒绝:
// 那说明证据读错了,照它补就会把已到账的再发一遍。
func creditShortfall(b *assetpb.AssetBundle, m PartialMissing) (*assetpb.AssetBundle, error) {
	if m.empty() {
		return nil, errors.New("缺口为空")
	}
	if len(b.GetItemUuids()) > 0 || b.GetPetId() != 0 {
		return nil, errors.New("发放腿带着扣出用的选择字段,数据不对,只能取证后手工处理")
	}
	var order []assetUnit
	for _, it := range b.GetItemInstances() {
		order = append(order, assetUnit{"instance", it.GetItemUuid()})
	}
	if b.GetPetInstance() != nil {
		order = append(order, assetUnit{"pet", 0})
	}
	for _, cur := range b.GetCurrencies() {
		order = append(order, assetUnit{"currency", uint64(cur.GetCurrencyType())})
	}
	positions := make(map[assetUnit]int, len(order))
	for i, u := range order {
		if _, dup := positions[u]; dup {
			return nil, fmt.Errorf("包里 %s 不止一条,缺口点名不清,只能取证后手工处理", u)
		}
		positions[u] = i
	}
	missing := missingUnits(m)
	first := len(order)
	for u := range missing {
		i, ok := positions[u]
		if !ok {
			return nil, fmt.Errorf("包里没有 %s", u)
		}
		first = min(first, i)
	}
	for i := first; i < len(order); i++ {
		if !missing[order[i]] {
			return nil, fmt.Errorf("缺口不符合 scene 的发放顺序:%s 没到账,排在它后面的 %s 却算到账了(资产通道 §4.33)", order[first], order[i])
		}
	}
	if first == 0 && len(b.GetItems()) == 0 {
		return nil, errors.New("缺口是整条腿:一件都没到账就不是部分发放,用 reissue")
	}
	out := &assetpb.AssetBundle{}
	for _, it := range b.GetItemInstances() {
		if missing[assetUnit{"instance", it.GetItemUuid()}] {
			out.ItemInstances = append(out.ItemInstances, proto.Clone(it).(*assetpb.AssetItemInstance))
		}
	}
	if missing[assetUnit{"pet", 0}] {
		out.PetInstance = proto.Clone(b.GetPetInstance()).(*assetpb.AssetPetInstance)
	}
	for _, cur := range b.GetCurrencies() {
		if missing[assetUnit{"currency", uint64(cur.GetCurrencyType())}] {
			out.Currencies = append(out.Currencies, proto.Clone(cur).(*assetpb.CurrencyAmount))
		}
	}
	return out, nil
}

// debitTaken 校验托管扣款腿的缺口(没扣到的部分),回实际扣到的部分(给退回用)和缺口(给留痕)。
//
// scene 的扣款顺序(asset_op_system.cpp 的 ApplyDebit):按数量的物品(整批,失败就零改动)→ 按 guid 的实例 → 宝宝 →
// 货币逐条,前一步失败后面都不做;可预知的失败都在扣之前复核,只有提交阶段内部不变量损坏才会部分扣出。所以:
//   - 有实例没扣到时,宝宝和全部货币也都没扣到;宝宝没扣到时,全部货币也没扣到;货币缺的是后缀;按数量的物品不会缺;
//   - 部分扣出的托管原件不进 escrow_snapshot(只有 APPLIED 才写),扣到的部分里有实例或宝宝就退不了,回错误,
//     要按 player_snapshot 人工补发。
func debitTaken(b *assetpb.AssetBundle, m PartialMissing) (taken, missing *assetpb.AssetBundle, err error) {
	if m.empty() {
		return nil, nil, errors.New("缺口为空")
	}
	selected := make(map[uint64]bool, len(b.GetItemUuids()))
	for _, id := range b.GetItemUuids() {
		selected[id] = true
	}
	for _, id := range m.Instances {
		if !selected[id] {
			return nil, nil, fmt.Errorf("包里没有实例 %d", id)
		}
	}
	if m.Pet && b.GetPetId() == 0 {
		return nil, nil, errors.New("包里没有宝宝")
	}
	positions := make(map[uint32]int, len(b.GetCurrencies()))
	for i, cur := range b.GetCurrencies() {
		if _, dup := positions[cur.GetCurrencyType()]; dup {
			return nil, nil, fmt.Errorf("包里货币 %d 不止一条,缺口点名不清,只能取证后手工处理", cur.GetCurrencyType())
		}
		positions[cur.GetCurrencyType()] = i
	}
	missingCurrency := make(map[uint32]bool, len(m.Currencies))
	firstCurrency := len(b.GetCurrencies())
	for _, t := range m.Currencies {
		i, ok := positions[t]
		if !ok {
			return nil, nil, fmt.Errorf("包里没有货币 %d", t)
		}
		missingCurrency[t] = true
		firstCurrency = min(firstCurrency, i)
	}
	allCurrencyMissing := len(m.Currencies) == len(b.GetCurrencies())
	switch {
	case len(m.Currencies) > 0 && len(m.Currencies) != len(b.GetCurrencies())-firstCurrency:
		return nil, nil, errors.New("缺口不符合 scene 的扣款顺序:货币缺的必须是包里的最后几条")
	case len(m.Instances) > 0 && (b.GetPetId() != 0 && !m.Pet || !allCurrencyMissing):
		return nil, nil, errors.New("缺口不符合 scene 的扣款顺序:有实例没扣到时,宝宝和全部货币也不会扣")
	case m.Pet && !allCurrencyMissing:
		return nil, nil, errors.New("缺口不符合 scene 的扣款顺序:宝宝没扣到时,全部货币也不会扣")
	case len(m.Instances) < len(b.GetItemUuids()) || b.GetPetId() != 0 && !m.Pet:
		return nil, nil, errors.New("扣到的部分里有实例或宝宝:部分扣出不留托管原件,退不了,要按 player_snapshot 人工补发")
	}
	taken = &assetpb.AssetBundle{}
	for _, item := range b.GetItems() {
		taken.Items = append(taken.Items, proto.Clone(item).(*assetpb.ItemGrant))
	}
	missing = &assetpb.AssetBundle{ItemUuids: append([]uint64(nil), m.Instances...)}
	if m.Pet {
		missing.PetId = b.GetPetId()
	}
	for _, cur := range b.GetCurrencies() {
		if missingCurrency[cur.GetCurrencyType()] {
			missing.Currencies = append(missing.Currencies, proto.Clone(cur).(*assetpb.CurrencyAmount))
		} else {
			taken.Currencies = append(taken.Currencies, proto.Clone(cur).(*assetpb.CurrencyAmount))
		}
	}
	if emptyBundle(taken) {
		return nil, nil, errors.New("缺口是整条腿:一件都没扣到就不是部分扣出")
	}
	return taken, missing, nil
}

// ── redirect ────────────────────────────────────────────────────

// planRedirect 把已换主或已删的一方改绑到原账号名下的另一名角色(也可以是被原账号买回的同一名角色,§15.7.1 第 1 类)。
// 改绑等于「合同上这一方整体换成目标角色」:此后该付的从它扣,该收的发给它;已经发生的不动。
//   - 合同上这一方的 player_id 换成目标角色,原身份换成「原账号 + 目标角色现在的修订号」;拍品的买方同时改最高出价人;
//   - 这一方还没发出的腿原地改投(没有资产动过);发往原角色、没到账的发放腿由发往目标角色的补发腿取代;
//   - 已到账的发放腿、已扣出的托管不动,以后的退回、交付按合同上的新一方发;
//   - 回到转人工之前的阶段;另一方也已不归原身份时保持人工,等它也改绑。
func planRedirect(r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow, q *tradepb.TradeCommerceQuarantine, c manualContext, result *ManualResult) error {
	if r.Kind == 1 || r.Kind == 6 {
		return notAllowed("官方商城与人民币寄售不改绑:它们的归属核对在商城订单与角色过户流程里")
	}
	var p *contractParty
	parties := contractParties(r, w)
	for i := range parties {
		if parties[i].name == c.req.Party {
			p = &parties[i]
		}
	}
	switch {
	case p == nil:
		return notAllowed("合同没有 %s 一方", c.req.Party)
	case p.identity == nil:
		return notAllowed("合同缺 %s 的原归属身份,只能取证后手工修数据", p.name)
	}
	var change *partyRoleChange
	for i := range c.changes {
		if c.changes[i].party == p.name {
			change = &c.changes[i]
		}
	}
	switch {
	case change == nil:
		return notAllowed("%s 的角色 %d 仍归原身份(或副本判不了),不能改绑", p.name, p.player)
	case c.target == nil:
		return notAllowed("读不到目标角色 %d 的归属", c.req.To)
	case c.target.GetAccount() != p.identity.GetAccount():
		return notAllowed("目标角色 %d 不属于这一方的原账号", c.req.To)
	}
	for _, other := range parties {
		if other.name != p.name && other.player == c.req.To {
			return notAllowed("目标角色 %d 是合同的另一方", c.req.To)
		}
	}
	after := &tradepb.TradeRoleIdentity{Account: p.identity.GetAccount(), OwnerRevision: c.target.GetOwnerRevision()}
	result.Party, result.FromPlayer, result.ToPlayer = p.name, p.player, c.req.To
	result.IdentityBefore = proto.Clone(p.identity).(*tradepb.TradeRoleIdentity)
	result.IdentityAfter = after
	switch p.name {
	case partySeller:
		r.SellerPlayerId, w.SellerIdentity = c.req.To, after
	case partyBuyer:
		r.BuyerPlayerId, w.BuyerIdentity = c.req.To, after
		if r.Kind == 5 && w.HighestBidder == p.player {
			w.HighestBidder = c.req.To // 拍品的买方就是最高出价人:结拍时拍品发给它
		}
	}
	if c.req.To != p.player {
		for _, l := range append([]*tradepb.TradeCommerceLeg(nil), w.Legs...) {
			if l.SupersededBy != "" || l.PlayerId != p.player {
				continue
			}
			switch {
			case l.OpId == 0:
				l.PlayerId = c.req.To
				result.RetargetedLegs = append(result.RetargetedLegs, l.Role)
			case l.Stream == 4 && done(l) && !applied(l) && l.Outcome != partialOutcome:
				role := reissueRole(w, l.Role)
				l.SupersededBy = role
				addLeg(w, role, c.req.To, 4, proto.Clone(l.Bundle).(*assetpb.AssetBundle))
				result.NewLegs = append(result.NewLegs, role)
				result.SupersededLegs = append(result.SupersededLegs, l.Role)
			}
		}
	}
	if len(c.changes) > 1 {
		return nil // 另一方也已不归原身份:合同保持人工,等它也改绑之后再回到原阶段
	}
	w.Stage, r.Status = q.Stage, q.Status
	return nil
}

// ── void-auction ────────────────────────────────────────────────

// planVoidAuction 作废有出价、成交不了的拍卖(§15.7.1 第 3 类):拍品的托管退回卖家、按取消结案;它引用的出价改成
// bid_refund(与被超价同一条路),由引擎退回冻结款。两张合同是一笔交易的两半,要么一起成交、要么一起退回。
func planVoidAuction(r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow, c manualContext, result *ManualResult) error {
	if r.Kind != 5 {
		return notAllowed("void-auction 只用于拍品(这是 kind=%d)", r.Kind)
	}
	if w.HighestBidder == 0 || w.WinningBidId == 0 {
		return notAllowed("拍品没有出价,用 refund")
	}
	bid, bw := c.bid, c.bidW
	switch {
	case bid == nil || bw == nil:
		return notAllowed("读不到拍品引用的出价 %d", w.WinningBidId)
	case w.WinningBidId != bid.CommerceId:
		return notAllowed("拍品引用的是出价 %d,不是 %d", w.WinningBidId, bid.CommerceId)
	case bid.Kind != BidKind || bid.ParentId != r.CommerceId:
		return notAllowed("合同 %d 不是这件拍品的出价", bid.CommerceId)
	case len(c.bidChanges) > 0:
		return notAllowed("出价人已不归原身份(%s),先对出价 redirect", describeRoleChanges(c.bidChanges))
	}
	held := bw.Stage == "bid_held" || bid.Status == Manual && bw.Stage == "manual" && bw.GetQuarantine().GetStage() == "bid_held"
	if !held {
		return notAllowed("出价 %d 不在 bid_held(stage=%q),拍品与它的引用对不上,只能取证后手工处理", bid.CommerceId, bw.Stage)
	}
	if !applied(leg(bw, "bid_debit")) {
		return notAllowed("出价 %d 的冻结款没扣到", bid.CommerceId)
	}
	for _, l := range bw.Legs {
		if l.Stream == 4 && l.SupersededBy == "" && done(l) {
			return notAllowed("出价 %d 已有发放腿 %s,对不上 bid_held", bid.CommerceId, l.Role)
		}
	}
	if err := refundCommon(w); err != nil {
		return err
	}
	debit := leg(w, "seller_debit")
	if !applied(debit) {
		return notAllowed("拍品托管没扣到,对不上已有出价")
	}
	back, err := Transfer(debit.Bundle, debit.Snapshot)
	if err != nil {
		return notAllowed("拍品托管原件转不动,要按 player_snapshot 人工补发: %v", err)
	}
	applyRefund(r, w, []plannedLeg{{role: refundLegRole(debit.Role), player: debit.PlayerId, bundle: back}}, result)
	var bidQuarantine *tradepb.TradeCommerceQuarantine
	if q := bw.GetQuarantine(); q != nil && bid.Status == Manual {
		bidQuarantine = proto.Clone(q).(*tradepb.TradeCommerceQuarantine)
	}
	bw.Stage, bid.Status = "bid_refund", CancelPending
	result.LinkedID = bid.CommerceId
	result.Linked = &ManualResult{Quarantine: bidQuarantine, Stage: bw.Stage, Status: bid.Status}
	return nil
}
