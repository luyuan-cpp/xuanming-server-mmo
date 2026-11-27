package logic

// wanted_skill_book_logic.go —— 悬赏求书求购单(kind 7)的三个客户端入口:发榜、揭榜、悬赏榜
// (docs/design/skill-book-wanted-order-trade-escrow.md §3.2、§3.3、§3.8)。撤单与查单走通用合同入口
// (CancelCommerceOrder / GetCommerceOrder),不在这里。业务结果一律 in-band,放在响应的 error_message 里。

import (
	"context"
	"strconv"

	tradepb "proto/trade"
	"shared/roleidentity"
	"trade/internal/commerce"
	"trade/internal/constants"
	"trade/internal/data"
	"trade/internal/session"
)

// wantedListDefaultLimit / wantedListMaxLimit 是悬赏榜一页的缺省与上限。
const (
	wantedListDefaultLimit = 20
	wantedListMaxLimit     = 50
)

// CreateWantedSkillBookOrder 发榜。回包里的单子通常还在托管悬赏银两(phase ESCROWING),
// 客户端按 GetCommerceOrder 轮询到挂榜(WAITING_CONFIRMATION)或撤单结案(银两不足)。
func (l *JubaozhaiLogic) CreateWantedSkillBookOrder(in *tradepb.CreateWantedSkillBookOrderRequest) (*tradepb.WantedSkillBookOrderResponse, error) {
	var record *tradepb.TradeCommerceRecord
	var me uint64
	answer, err := l.commerceCall(func(ctx context.Context, player uint64, e *commerce.Engine) (*tradepb.TradeCommerceRecord, error) {
		me = player
		r, err := e.CreateWantedSkillBookOrder(ctx, player, in)
		record = r
		return r, err
	})
	return l.wantedAnswer(answer, record, me), err
}

// FulfillWantedSkillBookOrder 揭榜。同一请求号重发查的是那一次揭榜的结果:交易中、交付中、已成交回单子,
// 书没扣到回 TradeAssetNotTradable,书不符合要求、已原样退回回 TradeWantedBookMismatch。
func (l *JubaozhaiLogic) FulfillWantedSkillBookOrder(in *tradepb.FulfillWantedSkillBookOrderRequest) (*tradepb.WantedSkillBookOrderResponse, error) {
	var record *tradepb.TradeCommerceRecord
	var me uint64
	answer, err := l.commerceCall(func(ctx context.Context, player uint64, e *commerce.Engine) (*tradepb.TradeCommerceRecord, error) {
		me = player
		id, parseErr := strconv.ParseUint(in.GetOrderId(), 10, 64)
		if parseErr != nil || id == 0 {
			return nil, commerce.ErrInvalid
		}
		r, err := e.FulfillWantedSkillBookOrder(ctx, player, id, in.GetClientRequestId(), in.GetSealedItemId())
		record = r
		return r, err
	})
	return l.wantedAnswer(answer, record, me), err
}

// wantedAnswer 把通用合同回包换成求购单视图。合同解不开按故障回(同 orderAnswer)。
func (l *JubaozhaiLogic) wantedAnswer(answer *tradepb.CommerceOrderResponse, r *tradepb.TradeCommerceRecord, me uint64) *tradepb.WantedSkillBookOrderResponse {
	out := &tradepb.WantedSkillBookOrderResponse{ErrorMessage: answer.GetErrorMessage(), ServerNowMs: answer.GetServerNowMs()}
	if answer.GetErrorMessage().GetId() != 0 || r == nil || l.deps.Commerce == nil {
		return out
	}
	view, ok := wantedOrderView(r, me, l.deps.Commerce.WantedSkills)
	if !ok {
		out.ErrorMessage = tipOf(constants.ErrServiceUnavailable)
		return out
	}
	out.Order = view
	return out
}

// ListWantedSkillBookOrders 悬赏榜。公开榜全服可见(与寄售浏览同一口径),只列挂榜中的单,按悬赏额从高到低;
// only_mine 列本人还没结案的单(含托管中、交易中、交付中),按原账号 / 修订筛,角色过户之后新主看不到旧主的单。
// 开关关掉之后公开榜回「功能未开放」(挂着的单揭不了),本人的单照常列出,好让发榜人撤单取回悬赏。
func (l *JubaozhaiLogic) ListWantedSkillBookOrders(in *tradepb.ListWantedSkillBookOrdersRequest) (*tradepb.ListWantedSkillBookOrdersResponse, error) {
	l, cancel := l.withRequestBudget()
	defer cancel()
	out := &tradepb.ListWantedSkillBookOrdersResponse{ServerNowMs: nowMillis(l.deps.Now)}
	me, ok := session.ClientPlayerID(l.ctx)
	if !ok {
		out.ErrorMessage = tipOf(constants.ErrInvalidParameter)
		return out, nil
	}
	engine := l.deps.Commerce
	if engine == nil || !in.GetOnlyMine() && engine.WantedSkills == nil {
		out.ErrorMessage = tipOf(constants.ErrFeatureDisabled)
		return out, nil
	}
	limit := int(in.GetLimit())
	if limit == 0 {
		limit = wantedListDefaultLimit
	}
	limit = min(max(limit, 1), wantedListMaxLimit)

	var rows []*tradepb.TradeCommerceRecord
	var err error
	if in.GetOnlyMine() {
		owned, identityErr := l.actorIdentity()
		if identityErr != nil {
			out.ErrorMessage = tipOf(commerceCode(identityErr))
			return out, nil
		}
		if l.ctx, identityErr = roleidentity.Capture(l.ctx, owned); identityErr != nil {
			out.ErrorMessage = tipOf(commerceCode(identityErr))
			return out, nil
		}
		after := data.ParseCommerceCursor(in.GetCursor())
		rows, err = engine.ListMyWantedSkillBookOrders(l.ctx, me, after.CreatedMs, after.CommerceId, limit)
	} else {
		after, hasCursor := commerce.ParseWantedBoardCursor(in.GetCursor())
		rows, err = engine.ListOpenWantedSkillBookOrders(l.ctx, after, hasCursor, limit)
	}
	if err != nil {
		l.Errorf("trade wanted skill book list failed only_mine=%t: %v", in.GetOnlyMine(), err)
		out.ErrorMessage = tipOf(commerceCode(err))
		return out, nil
	}
	if len(rows) == limit {
		last := rows[len(rows)-1]
		if in.GetOnlyMine() {
			out.NextCursor = data.CommerceCursorOf(last).String()
		} else {
			out.NextCursor = commerce.WantedBoardCursor{RewardGold: last.GetUnitPrice(), CommerceID: last.GetCommerceId()}.String()
		}
	}
	for _, r := range rows {
		if in.GetOnlyMine() {
			w, decodeErr := data.Workflow(r)
			if decodeErr != nil || !commerce.OwnsRecord(l.ctx, r, w) {
				continue
			}
		}
		view, ok := wantedOrderView(r, me, engine.WantedSkills)
		if !ok {
			if in.GetOnlyMine() {
				continue
			}
			// 公开榜上的单解不开是数据坏了(合同推进会把它隔离成人工、移出这一页),这一页按故障回,同 BrowseCommerceOffers。
			out.ErrorMessage = tipOf(constants.ErrServiceUnavailable)
			out.Orders, out.NextCursor = nil, ""
			return out, nil
		}
		if filter := in.GetFilterSkillTableId(); filter != 0 && view.GetSkillTableId() != filter {
			continue // 页内过滤:一页可能不满,客户端按 next_cursor 继续取
		}
		out.Orders = append(out.Orders, view)
	}
	return out, nil
}

// wantedOrderView 是求购单给客户端的视图;技能与卷宗名按当前配表现查(求购单没开时 skills 为 nil,名字留空)。
// 合同解不开、不是求购单或缺条款回 ok=false。
func wantedOrderView(r *tradepb.TradeCommerceRecord, me uint64, skills commerce.WantedSkillCatalog) (*tradepb.WantedSkillBookOrderView, bool) {
	if r.GetKind() != commerce.KindWantedSkillBook {
		return nil, false
	}
	w, err := data.Workflow(r)
	if err != nil || w.GetWantedSkillBook() == nil {
		return nil, false
	}
	terms := w.GetWantedSkillBook()
	v := &tradepb.WantedSkillBookOrderView{
		OrderId:                strconv.FormatUint(r.GetCommerceId(), 10),
		PosterPlayerId:         r.GetBuyerPlayerId(),
		PosterName:             terms.GetPosterName(),
		SkillTableId:           terms.GetSkillTableId(),
		ScrollId:               terms.GetScrollId(),
		RequireMasterSignature: terms.GetRequireMasterSignature(),
		RewardGold:             terms.GetRewardGold(),
		SellerNetGold:          terms.GetSellerNetGold(),
		Phase:                  commerce.WantedPhase(r, w),
		FulfillerPlayerId:      r.GetSellerPlayerId(),
		CreatedMs:              r.GetCreatedMs(),
		Version:                r.GetVersion(),
		IsMine:                 me != 0 && r.GetBuyerPlayerId() == me,
	}
	if skills != nil {
		info, _ := skills.WantedSkill(terms.GetSkillTableId())
		v.SkillName, v.ScrollName = info.SkillName, info.ScrollName
	}
	return v, true
}
