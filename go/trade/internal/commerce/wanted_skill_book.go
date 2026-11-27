package commerce

// wanted_skill_book.go —— 悬赏求书求购单(kind 7,docs/design/skill-book-wanted-order-trade-escrow.md)。
//
// 求购单是反过来的寄售。发榜人是合同的买家(BuyerPlayerId):建单之后先托管悬赏银两(buyer_debit,stage wanted_escrow),
// 托管成了才挂榜(wanted_open)。揭榜人是合同的卖家(SellerPlayerId),只在一次揭榜进行中非 0:揭榜托管一枚封卷道籍
// (seller_debit_<揭榜序号>_<请求键>,stage wanted_fill),trade 按 scene 扣出时交回的完整实例核对条款(wantedBookMatches)。
//   - 相符:先把书交给发榜人(buyer_receive),到账之后才把税后银两交给揭榜人(seller_receive,delivery_order.go 的闸压着),
//     两条都到账结案;5% 税不发给任何人,即销毁。发榜人那条被终局拒绝时整单退回,悬赏退全额(settleBuyerFirstDelivery)。
//   - 不符:原样退书(seller_return_<揭榜序号>,退回腿),退书有结局之后揭榜人清零、单子回到挂榜;书没扣到(已不在背包、
//     已绑定、被战斗预留)直接回到挂榜。
//
// 两名玩家的资产都只经资产通道由各自所在 scene 在本人存档里改,合同状态只在 trade 库,一条合同一个状态机。
// 撤单走 Mutate 的 cancel,只有发榜人能撤、只在托管与挂榜阶段能撤;已托管的悬赏按 buyer_return 原样退回。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"

	assetpb "proto/common/asset"
	componentpb "proto/common/component"
	tradepb "proto/trade"
	"shared/currencytype"
	"shared/generated/pb/table"
	tablemgr "shared/generated/table"
	"shared/roleidentity"
	"trade/internal/data"
)

const (
	// KindWantedSkillBook 是求购单的合同种类(tradepb.CommerceOrderKind_COMMERCE_ORDER_KIND_SKILL_BOOK_WANTED)。
	KindWantedSkillBook = uint32(tradepb.CommerceOrderKind_COMMERCE_ORDER_KIND_SKILL_BOOK_WANTED)

	// 以下四个与 C++ equip_craft_quality_rules.h 是同一条业务规则(kWantedOrderMinGold / kWantedOrderMaxGold /
	// kMaxActiveWantedOrders / kProxyTaxBps),wanted_skill_book_crosslang_test.go 读 C++ 头文件比对,防止两边漂移。
	WantedRewardMinGold   = uint64(100)
	WantedRewardMaxGold   = uint64(50000)
	MaxActiveWantedOrders = 3
	WantedTaxBps          = uint64(500)

	// SealedSkillBookConfigID 是封卷道籍的道具模板(C++ equipcraft::kSealedSkillBookConfigId)。
	SealedSkillBookConfigID = uint32(3414)

	// maxWantedFulfillAttempts 是一张求购单累计揭榜次数的上限,与寄售每单 32 次购买尝试(QuantityReserved)同口径:
	// 每次揭榜在 workflow 里留一两条腿,不设上限 workflow 会无界增长。用满之后这张单揭不了,发榜人撤单(全额退回)重发。
	maxWantedFulfillAttempts = 32

	stageWantedEscrow = "wanted_escrow" // 托管发榜人的悬赏银两
	stageWantedOpen   = "wanted_open"   // 挂榜,可揭榜、可撤单
	stageWantedFill   = "wanted_fill"   // 一次揭榜进行中:托管揭榜人的书、核对、不符时退书

	wantedFulfillScope = "wanted_fulfill" // 揭榜请求键的作用域(RequestKey)
	wantedDebitPrefix  = "seller_debit_"  // 揭榜托管腿:seller_debit_<揭榜序号>_<请求键>
	// wantedReturnPrefix 是不符的书原样退回的腿:seller_return_<揭榜序号>。leg_tx_type.go 把它登记为退回腿(TX_AUCTION_RETURN)。
	wantedReturnPrefix = "seller_return_"
)

var (
	// ErrWantedOrderLimit:发榜人同时生效的求购单已满 MaxActiveWantedOrders 张。
	ErrWantedOrderLimit = Rejection(table.TradeError_kTradeWantedOrderLimitReached)
	// ErrWantedBookMismatch:揭榜交上的书不符合条款,已原样退回(按同一请求号重发揭榜时回)。
	ErrWantedBookMismatch = Rejection(table.TradeError_kTradeWantedBookMismatch)
	// ErrWantedDisabled:本部署没开悬赏求书(Commerce.WantedSkillBook.Enabled=false)。
	ErrWantedDisabled = Rejection(table.TradeError_kTradeFeatureDisabled)
)

// WantedTaxGold 是一笔悬赏要销毁的集市税:悬赏额 × 5% 向上取整(C++ equipcraft::ProxyTaxGold)。
func WantedTaxGold(reward uint64) uint64 {
	if reward == 0 {
		return 0
	}
	return (reward*WantedTaxBps + 9999) / 10000
}

// WantedSellerNetGold 是揭榜人税后实得(C++ equipcraft::ProxyNetGold)。悬赏额有上限,乘法不会溢出。
func WantedSellerNetGold(reward uint64) uint64 {
	tax := WantedTaxGold(reward)
	if reward <= tax {
		return 0
	}
	return reward - tax
}

// WantedSkill 是一门技能在配表里的展示信息。
type WantedSkill struct {
	ScrollID   uint32
	SkillName  string
	ScrollName string
}

// WantedSkillCatalog 查一门技能能不能悬赏求书。生产读 Skill 与 SkillScrollCraft 两张表(TableWantedSkills),单测注入假表。
// 返回值:配表里查得到时 info 带名字与所属卷宗(展示用,不论能不能悬赏);eligible 是这门技能此刻能不能发榜。
type WantedSkillCatalog interface {
	WantedSkill(skillTableID uint32) (info WantedSkill, eligible bool)
}

// TableWantedSkills 读全局表管理器(svc 启动时经 tablebundle 加载,热更提交之后读到新快照),每次现查、不缓存行(data/AGENTS.md「只存 id」)。
type TableWantedSkills struct{}

func (TableWantedSkills) WantedSkill(id uint32) (WantedSkill, bool) {
	skill, ok := tablemgr.SkillTableManagerInstance.FindById(id)
	if !ok || skill == nil {
		return WantedSkill{}, false
	}
	info := WantedSkill{ScrollID: skill.GetScrollId(), SkillName: skill.GetName()}
	scroll, ok := tablemgr.SkillScrollCraftTableManagerInstance.FindById(skill.GetScrollId())
	if !ok || scroll == nil {
		return info, false
	}
	info.ScrollName = scroll.GetName()
	return info, wantedSkillEligible(skill, scroll)
}

// wantedSkillEligible 与 C++ PlayerCraftSystem::PostWantedOrder 的判据逐条一致:常规永久技能(非赛季、永久战斗模式、非稀世),
// 所属卷宗开放且不是稀世卷宗。
func wantedSkillEligible(skill *table.SkillTable, scroll *table.SkillScrollCraftTable) bool {
	return skill.GetSeasonId() == 0 && skill.GetBattleModel() == 1 && skill.GetRarityTier() == 0 &&
		scroll.GetEnabled() != 0 && scroll.GetRarityTier() == 0
}

// wantedRewardBundle 是一份流通银两(币种 0)。悬赏托管、税后所得、撤单退回都只走流通银两:绑定银两不进求购单,堵住洗白。
func wantedRewardBundle(amount uint64) *assetpb.AssetBundle {
	return &assetpb.AssetBundle{Currencies: []*assetpb.CurrencyAmount{{CurrencyType: currencytype.Gold, Amount: amount}}}
}

// CreateWantedSkillBookOrder 发榜(§3.2):无状态校验之后建 kind 7 合同,悬赏银两由合同推进托管(stage wanted_escrow)。
// 银两不足时托管腿被 scene 拒绝,合同按撤单结案(planWantedSkillBook)。
// 同一发榜人同时生效的求购单至多 MaxActiveWantedOrders 张,计数与插入在同一把角色锁下(countActiveWantedOrders)。
func (e *Engine) CreateWantedSkillBookOrder(ctx context.Context, me uint64, in *tradepb.CreateWantedSkillBookOrderRequest) (*tradepb.TradeCommerceRecord, error) {
	if e.WantedSkills == nil {
		return nil, ErrWantedDisabled
	}
	if in.GetRewardGold() < WantedRewardMinGold || in.GetRewardGold() > WantedRewardMaxGold {
		return nil, ErrInvalid
	}
	skill, eligible := e.WantedSkills.WantedSkill(in.GetSkillTableId())
	if !eligible {
		return nil, ErrInvalid
	}
	posterName := ""
	if original, ok := roleidentity.Original(ctx); ok {
		posterName = original.GetRole().GetName()
	}
	terms := &tradepb.TradeWantedSkillBookTerms{
		SkillTableId:           in.GetSkillTableId(),
		ScrollId:               skill.ScrollID,
		RequireMasterSignature: in.GetRequireMasterSignature(),
		RewardGold:             in.GetRewardGold(),
		SellerNetGold:          WantedSellerNetGold(in.GetRewardGold()),
		PosterName:             posterName,
	}
	// 不过期(ExpiresMs=0):现行口径「随时可撤、不过期」,也不逼玩家按时上线处理。托管腿因此没有截止,撤单时由 cancel 截止在途扣款。
	// item_config_id 与 unit_price 是悬赏榜按现成索引排序与过滤用的投影(ListOpenWantedSkillBookOrders)。
	record := &tradepb.TradeCommerceRecord{Kind: KindWantedSkillBook, Status: Escrowing, BuyerPlayerId: me,
		UnitPrice: in.GetRewardGold(), CurrencyType: currencytype.Gold, ItemConfigId: SealedSkillBookConfigID}
	workflow := &tradepb.TradeCommerceWorkflow{Stage: stageWantedEscrow, WantedSkillBook: terms}
	return e.createLocked(ctx, me, in.GetClientRequestId(), in, record, workflow, func(ctx context.Context, tx *sql.Tx) error {
		active, err := countActiveWantedOrders(ctx, tx, me)
		if err != nil {
			return err
		}
		if active >= MaxActiveWantedOrders {
			return ErrWantedOrderLimit
		}
		return nil
	})
}

// countActiveWantedOrders 在发榜的建单事务里、发榜人的角色锁之下数本人还没结案的求购单(转人工的也算:托管还在),数到上限为止。
// 用加锁读:TiDB 悲观事务里普通读取的是事务开始时的快照,看不到本事务等角色锁期间别人刚提交的那张单;
// FOR UPDATE 读最新提交的版本,两笔并发发榜在角色锁上串行之后,后一笔一定数得到前一笔。
func countActiveWantedOrders(ctx context.Context, tx *sql.Tx, poster uint64) (int, error) {
	rows, err := tx.QueryContext(ctx, "SELECT commerce_id FROM trade_commerce WHERE buyer_player_id=? AND kind=? AND status NOT IN (?,?) ORDER BY commerce_id LIMIT ? FOR UPDATE",
		poster, KindWantedSkillBook, Cancelled, Settled, MaxActiveWantedOrders)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		n++
	}
	return n, rows.Err()
}

// FulfillWantedSkillBookOrder 揭榜(§3.3):交出背包里一枚封卷道籍。书架里的书由客户端先在 scene 封卷(免费)再揭榜。
// 同一揭榜人同一请求号重放查的是那一次揭榜的结果(wantedAttemptResult)。
func (e *Engine) FulfillWantedSkillBookOrder(ctx context.Context, me, id uint64, requestID string, sealedItemID uint64) (*tradepb.TradeCommerceRecord, error) {
	if e.WantedSkills == nil {
		return nil, ErrWantedDisabled
	}
	if len(requestID) < 8 || len(requestID) > 80 || sealedItemID == 0 {
		return nil, ErrInvalid
	}
	return e.mutate(ctx, me, id, "fulfill", sealedItemID, requestID)
}

// fulfillWantedLocked 是揭榜在合同行锁与双方角色锁之下的那一步。回 replayed=true 表示这是一次重放,合同不改、不重存。
func (e *Engine) fulfillWantedLocked(ctx context.Context, tx *sql.Tx, me uint64, r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow, requestID string, sealedItemID uint64) (bool, error) {
	if r.Kind != KindWantedSkillBook || w.GetWantedSkillBook() == nil {
		return false, ErrConflict
	}
	key := RequestKey(wantedFulfillScope, me, requestID)
	if attempt, debit := wantedAttemptByKey(w, key); debit != nil {
		if len(debit.GetBundle().GetItemUuids()) != 1 || debit.GetBundle().GetItemUuids()[0] != sealedItemID {
			return true, ErrReplay
		}
		return true, wantedAttemptResult(r, w, me, attempt, debit)
	}
	if w.Stage != stageWantedOpen || r.Status != Waiting || r.SellerPlayerId != 0 || w.CancelRequested {
		// 有人正在交书、托管中、撤单中或已结案。同一张单同时只允许一次揭榜在途。
		return false, ErrConflict
	}
	if me == r.BuyerPlayerId {
		return false, ErrSelf
	}
	if w.QuantityReserved >= maxWantedFulfillAttempts {
		return false, ErrConflict
	}
	owned, err := e.Roles.Ownership(ctx, tx, me)
	if err != nil {
		return false, err
	}
	r.SellerPlayerId = me
	w.SellerIdentity = &tradepb.TradeRoleIdentity{Account: owned.Account, OwnerRevision: owned.OwnerRevision}
	w.QuantityReserved++
	addLeg(w, wantedDebitRole(w.QuantityReserved, key), me, 3, &assetpb.AssetBundle{ItemUuids: []uint64{sealedItemID}})
	w.Stage = stageWantedFill
	r.Status = Escrowing
	return false, nil
}

func wantedDebitRole(attempt uint64, key string) string {
	return wantedDebitPrefix + strconv.FormatUint(attempt, 10) + "_" + key
}

func wantedReturnRole(attempt uint64) string {
	return wantedReturnPrefix + strconv.FormatUint(attempt, 10)
}

// wantedAttemptDebit 是第 attempt 次揭榜的托管腿。
func wantedAttemptDebit(w *tradepb.TradeCommerceWorkflow, attempt uint64) *tradepb.TradeCommerceLeg {
	prefix := wantedDebitPrefix + strconv.FormatUint(attempt, 10) + "_"
	for _, l := range w.GetLegs() {
		if strings.HasPrefix(l.GetRole(), prefix) {
			return l
		}
	}
	return nil
}

// wantedAttemptByKey 按揭榜请求键找那一次揭榜的托管腿与揭榜序号;没有回 (0, nil)。
func wantedAttemptByKey(w *tradepb.TradeCommerceWorkflow, key string) (uint64, *tradepb.TradeCommerceLeg) {
	for _, l := range w.GetLegs() {
		rest, ok := strings.CutPrefix(l.GetRole(), wantedDebitPrefix)
		if !ok || !strings.HasSuffix(rest, "_"+key) {
			continue
		}
		attempt, err := strconv.ParseUint(strings.TrimSuffix(rest, "_"+key), 10, 64)
		if err != nil {
			continue
		}
		return attempt, l
	}
	return 0, nil
}

// wantedAttemptResult 是重放一次揭榜时回给揭榜人的结论:这一次还是当前这一次(交易中、交付中、已结案)回 nil,
// 由调用方回合同;已经作废的,书没扣到回 ErrNotTradable,书扣到了却不符、已原样退回回 ErrWantedBookMismatch。
func wantedAttemptResult(r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow, me, attempt uint64, debit *tradepb.TradeCommerceLeg) error {
	if attempt == w.QuantityReserved && r.SellerPlayerId == me {
		return nil
	}
	if applied(debit) {
		return ErrWantedBookMismatch
	}
	return ErrNotTradable
}

// planWantedSkillBook 推进求购单自己的三个阶段。走到交付(deliver)或退回(return)之后,由 plan 的通用收尾接着处理
// (delivery_order.go 的先买家后卖家闸、finalCredits)。
func planWantedSkillBook(r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow, now uint64) error {
	terms := w.GetWantedSkillBook()
	if terms == nil {
		return errors.New("求购单缺少条款")
	}
	if w.Stage == stageWantedEscrow {
		addLeg(w, "buyer_debit", r.BuyerPlayerId, 3, wantedRewardBundle(terms.GetRewardGold()))
		escrow := leg(w, "buyer_debit")
		if !done(escrow) {
			return nil
		}
		if !applied(escrow) {
			// 银两不足、被撤单截止等:没扣到,银两还在发榜人身上,按撤单结案。
			w.Stage = "done"
			r.Status = Cancelled
			return nil
		}
		w.Stage = stageWantedOpen
		r.Status = Waiting
	}
	if w.Stage == stageWantedOpen && w.CancelRequested {
		w.Stage = "return"
		r.Status = CancelPending
		return transferLeg(w, "buyer_return", r.BuyerPlayerId, leg(w, "buyer_debit"))
	}
	if w.Stage == stageWantedFill {
		return planWantedFill(r, w, terms, now)
	}
	return nil
}

// planWantedFill 推进当前这一次揭榜(第 QuantityReserved 次):等书扣出,按快照核对,相符进交付,不符退书,没扣到直接回挂榜。
func planWantedFill(r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow, terms *tradepb.TradeWantedSkillBookTerms, now uint64) error {
	attempt := w.QuantityReserved
	debit := wantedAttemptDebit(w, attempt)
	if debit == nil {
		return fmt.Errorf("求购单第 %d 次揭榜缺托管腿", attempt)
	}
	if !done(debit) {
		return nil
	}
	if !applied(debit) {
		// 书已不在背包、已绑定、被战斗预留,或角色忙:这次揭榜作废,书还在揭榜人身上。
		reopenWanted(r, w)
		return nil
	}
	book, err := Transfer(debit.GetBundle(), debit.GetSnapshot())
	if err != nil {
		return err
	}
	if !wantedBookMatches(book, terms) {
		// 原样退书。退书期间单子仍是「交易中」:退书腿的角色锁与 seq 行按当时的揭榜人取(advance 按 SellerPlayerId 取)。
		role := wantedReturnRole(attempt)
		addEscrowLeg(w, role, debit.GetPlayerId(), debit.GetPlayerId(), book)
		returned := currentLeg(w, role)
		if !done(returned) {
			return nil
		}
		if !applied(returned) {
			markManual(r, w, manualReasonFinalCredit, fmt.Sprintf("求购单第 %d 次揭榜的书不符合条款,退书腿 %s 没有到账", attempt, returned.GetRole()), now)
			return nil
		}
		reopenWanted(r, w)
		return nil
	}
	escrow := leg(w, "buyer_debit")
	if !applied(escrow) {
		return errors.New("求购单进入揭榜时悬赏银两没有托管")
	}
	w.Stage = "deliver"
	r.Status = Delivering
	addEscrowLeg(w, "buyer_receive", r.BuyerPlayerId, debit.GetPlayerId(), book)
	// 税后所得出自发榜人托管的悬赏;差额(5% 税)留在合同里不发,即销毁。seller_receive 由先买家后卖家闸压到书到账之后才发。
	addEscrowLeg(w, "seller_receive", r.SellerPlayerId, escrow.GetPlayerId(), wantedRewardBundle(terms.GetSellerNetGold()))
	return nil
}

// reopenWanted 让一次没成的揭榜作废、单子回到挂榜:揭榜人清零(同寄售下单失败清零买家)。那一次的腿都已有结局,留在 workflow 里作证据。
func reopenWanted(r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow) {
	r.SellerPlayerId = 0
	w.SellerIdentity = nil
	w.Stage = stageWantedOpen
	r.Status = Waiting
}

// wantedBookMatches 是 §3.4 的快照核对:scene 扣出时交回的那一件必须是一枚未绑定的常规封卷道籍,技能与条款一致,
// 要求宗师署名时带署名。判据与 C++ PlayerCraftSystem::FulfillWantedOrder 逐条一致;稀世书本来封不了卷、绑定书本来
// 封不了卷,这里再判一遍是纵深防御。实例解不开也算不符:原样退回,不吞玩家的东西。
func wantedBookMatches(book *assetpb.AssetBundle, terms *tradepb.TradeWantedSkillBookTerms) bool {
	if len(book.GetItemInstances()) != 1 || len(book.GetItems()) != 0 || len(book.GetCurrencies()) != 0 || book.GetPetInstance() != nil {
		return false
	}
	instance := book.GetItemInstances()[0]
	if instance.GetConfigId() != SealedSkillBookConfigID || instance.GetStackSize() != 1 {
		return false
	}
	item := &componentpb.ItemComp{}
	if proto.Unmarshal(instance.GetInstanceProto(), item) != nil {
		return false
	}
	sealed := item.GetSealedSkillBook()
	if item.GetItemId() != instance.GetItemUuid() || item.GetConfigId() != SealedSkillBookConfigID || item.GetSize() != 1 || item.GetBound() || sealed == nil {
		return false
	}
	if sealed.GetRarityTier() != 0 || sealed.GetSkillTableId() != terms.GetSkillTableId() {
		return false
	}
	return !terms.GetRequireMasterSignature() || sealed.GetCrafterName() != "" && sealed.GetCrafterPlayerId() != 0
}

// WantedPhase 是求购单给客户端看的阶段,由合同状态推导,不落库。
func WantedPhase(r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow) tradepb.WantedSkillBookOrderPhase {
	switch r.GetStatus() {
	case Manual:
		return tradepb.WantedSkillBookOrderPhase_WANTED_SKILL_BOOK_ORDER_PHASE_MANUAL
	case Settled:
		return tradepb.WantedSkillBookOrderPhase_WANTED_SKILL_BOOK_ORDER_PHASE_SETTLED
	case Cancelled:
		return tradepb.WantedSkillBookOrderPhase_WANTED_SKILL_BOOK_ORDER_PHASE_CANCELLED
	case CancelPending:
		return tradepb.WantedSkillBookOrderPhase_WANTED_SKILL_BOOK_ORDER_PHASE_CANCELLING
	}
	switch w.GetStage() {
	case stageWantedEscrow:
		return tradepb.WantedSkillBookOrderPhase_WANTED_SKILL_BOOK_ORDER_PHASE_ESCROWING
	case stageWantedOpen:
		return tradepb.WantedSkillBookOrderPhase_WANTED_SKILL_BOOK_ORDER_PHASE_OPEN
	case stageWantedFill:
		return tradepb.WantedSkillBookOrderPhase_WANTED_SKILL_BOOK_ORDER_PHASE_FULFILLING
	case "deliver":
		return tradepb.WantedSkillBookOrderPhase_WANTED_SKILL_BOOK_ORDER_PHASE_DELIVERING
	case "return":
		return tradepb.WantedSkillBookOrderPhase_WANTED_SKILL_BOOK_ORDER_PHASE_CANCELLING
	}
	return tradepb.WantedSkillBookOrderPhase_WANTED_SKILL_BOOK_ORDER_PHASE_UNSPECIFIED
}

// WantedBoardCursor 是公开悬赏榜的翻页游标:榜按(悬赏额降序, commerce_id 降序)排,游标是上一页最后一行的这两个值。
// commerce_id 只用来在同额的单之间给出稳定的先后,不代表建单先后(发号器的号是打散的)。对客户端它只是不透明字符串。
type WantedBoardCursor struct {
	RewardGold uint64
	CommerceID uint64
}

// ParseWantedBoardCursor 解析 WantedBoardCursor.String 的结果;空串或格式不对回 ok=false(从第一页开始)。
func ParseWantedBoardCursor(raw string) (WantedBoardCursor, bool) {
	reward, id, found := strings.Cut(raw, ":")
	if !found {
		return WantedBoardCursor{}, false
	}
	rewardGold, err := strconv.ParseUint(reward, 10, 64)
	if err != nil {
		return WantedBoardCursor{}, false
	}
	commerceID, err := strconv.ParseUint(id, 10, 64)
	if err != nil {
		return WantedBoardCursor{}, false
	}
	return WantedBoardCursor{RewardGold: rewardGold, CommerceID: commerceID}, true
}

func (c WantedBoardCursor) String() string {
	return strconv.FormatUint(c.RewardGold, 10) + ":" + strconv.FormatUint(c.CommerceID, 10)
}

// ListOpenWantedSkillBookOrders 列全服挂榜中的求购单(status=Waiting 只有 wanted_open 一个阶段),按悬赏额降序。
// 条件与排序正好落在 trade_commerce 现成的 (kind,market_id,item_config_id,side,status,on_book,unit_price,commerce_id) 索引上:
// 求购单的 market_id、side 恒为 0,不进盘口(on_book=0),item_config_id 恒为封卷道籍,unit_price 是悬赏额。
func (e *Engine) ListOpenWantedSkillBookOrders(ctx context.Context, after WantedBoardCursor, hasCursor bool, limit int) ([]*tradepb.TradeCommerceRecord, error) {
	const where = " FROM trade_commerce WHERE kind=? AND market_id=0 AND item_config_id=? AND side=0 AND status=? AND on_book=0"
	if !hasCursor {
		return e.Repo.Query(ctx, "SELECT "+data.CommerceColumns+where+" ORDER BY unit_price DESC,commerce_id DESC LIMIT ?",
			KindWantedSkillBook, SealedSkillBookConfigID, Waiting, limit)
	}
	return e.Repo.Query(ctx, "SELECT "+data.CommerceColumns+where+" AND (unit_price<? OR (unit_price=? AND commerce_id<?)) ORDER BY unit_price DESC,commerce_id DESC LIMIT ?",
		KindWantedSkillBook, SealedSkillBookConfigID, Waiting, after.RewardGold, after.RewardGold, after.CommerceID, limit)
}

// ListMyWantedSkillBookOrders 列发榜人自己还没结案的求购单(含托管中、交易中、交付中、转人工),按 (created_ms, commerce_id) 翻页。
// 走 (buyer_player_id, commerce_id) 索引;每人生效单数有上限,结果集很小。私有可见性(原账号 / 修订)由调用方按 OwnsRecord 筛。
func (e *Engine) ListMyWantedSkillBookOrders(ctx context.Context, poster uint64, afterCreatedMs, afterID uint64, limit int) ([]*tradepb.TradeCommerceRecord, error) {
	return e.Repo.Query(ctx, "SELECT "+data.CommerceColumns+" FROM trade_commerce WHERE buyer_player_id=? AND kind=? AND status NOT IN (?,?) AND (created_ms>? OR (created_ms=? AND commerce_id>?)) ORDER BY created_ms,commerce_id LIMIT ?",
		poster, KindWantedSkillBook, Cancelled, Settled, afterCreatedMs, afterCreatedMs, afterID, limit)
}
