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
	dspb "proto/data_service"
	tradepb "proto/trade"
	"shared/rolefence"
	"trade/internal/data"
)

// 转人工合同的人工处置(docs/design/jubaozhai-market.md §15.7、§15.7.1,手册 docs/ops/trade-commerce-quarantine-runbook.md §4.2)。
//
// 合同转人工之后不在 Due 里,引擎不再推进。人工处置只改合同行:在合同行锁和版本条件下改 stage、status、参与方,
// 必要时加几条还没发出的腿(op_id=0)。改完合同重新到期,发号、写 outbox、投递、记结局都由引擎下一轮的 advance 做,
// 与玩家操作走同一条写路径,原归属身份核对、按角色名只建一次的腿都不变。人工处置不碰 op 行,
// 卡死的单条指令先用 go/trade/cmd/assetopfix 终结。
//
// 本文件管锁、读与写;六种动作的判定是纯函数,在 manual_plan.go。
//   - 锁序与引擎相同:先按 player_id 升序锁涉及的角色护栏副本(readRoleFacts),再锁拍品的 guard 行与拍品
//     (出价、作废拍卖,与 acceptBid 相同),最后锁要改的合同行;
//   - 版本条件:操作人看过的版本与锁内读到的不同就不改,同一条命令重跑只生效一次;
//   - 留痕:manual_actions 追加一条;入口(logic.ManualContractOps)另打 [CommerceManual] 审计行。
// 入口只有 TradeAdmin.ResolveManualContract(运维工具 go/trade/cmd/commercefix 调),由运行中的 trade 副本执行,
// 沿用本库的写者闸门与角色护栏。

// 转人工的原因类别,写进 TradeCommerceQuarantine.reason,决定能做哪种人工处置。
const (
	manualReasonCorrupt         = "corrupt"             // 合同自身持久数据坏了(tick 隔离)
	manualReasonBadCounterparty = "bad_counterparty"    // 盘口上的对手单不可能成交,被移出盘口
	manualReasonAppliedPartial  = "applied_partial"     // 某条腿部分发放 / 部分扣出
	manualReasonFinalCredit     = "final_credit_failed" // 最终发放腿没有全部到账
	manualReasonRmbReturn       = "rmb_return_failed"   // 人民币寄售退回失败
	manualReasonOwnerChanged    = "owner_changed"       // 参与方的角色已换主或已删(quarantineOwnerChanged,2026-11-11)
)

// quarantineDetailMaxBytes 是转人工记录里错误原文的上限。
const quarantineDetailMaxBytes = 256

// ManualListPageMax 是列转人工合同一页最多的条数(TradeAdmin.ListManualContracts 与运维工具共用)。
const ManualListPageMax = 500

// manualOperatorMaxBytes、manualReasonMaxBytes 与资产指令人工终结同口径(assetop.ManualResolution)。
const (
	manualOperatorMaxBytes = 64
	manualReasonMaxBytes   = 191
)

// markManual 把合同转人工(status=Manual、stage="manual"),并把转人工之前的阶段、状态和原因记进 workflow:
// 人工处置按它判断能做哪种处理,取证也不用再翻日志。已经在 manual 的不覆盖第一次的记录。
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

// ManualAction 是一种人工处置。每种动作什么时候用、做什么、什么时候拒绝,见设计 §15.7 的表与 §15.7.1。
type ManualAction string

const (
	// ManualResume 回到转人工之前的阶段,由引擎重新判定。用于原因已经消除的情况(数据修好了、卡死的腿已终结、
	// 拍品已结算)。原因还在的话引擎会再次转人工,不会出错。
	ManualResume ManualAction = "resume"
	// ManualReissue 给没到账的最终发放腿各补发一条,原样的资产、原样的收件人。原腿留作证据,标上被谁取代。
	ManualReissue ManualAction = "reissue"
	// ManualRefund 把已扣进托管的资产原样退回给扣出它的人,合同按取消结案。只在还没有任何发放到账时可用;
	// 已部分成交的市场单只退剩余部分。
	ManualRefund ManualAction = "refund"
	// ManualPartial 按人从证据里认定的缺口处理部分发放 / 部分扣出的腿(§15.7.1 第 2 类)。
	ManualPartial ManualAction = "partial"
	// ManualRedirect 把已换主或已删的一方改绑到原账号名下的另一名角色(§15.7.1 第 1 类)。
	ManualRedirect ManualAction = "redirect"
	// ManualVoidAuction 作废有出价、成交不了的拍卖:拍品与它引用的出价在同一事务里一起退回(§15.7.1 第 3 类)。
	ManualVoidAuction ManualAction = "void-auction"
)

// ManualActions 是全部动作,show 按这个顺序列每种动作能否处理。
var ManualActions = []ManualAction{ManualResume, ManualReissue, ManualRefund, ManualPartial, ManualRedirect, ManualVoidAuction}

// ParseManualAction 解析运维工具写的动作名。
func ParseManualAction(s string) (ManualAction, bool) {
	for _, a := range ManualActions {
		if string(a) == s {
			return a, true
		}
	}
	return "", false
}

// PartialPart 是 partial 动作里一条腿的缺口。
type PartialPart struct {
	Leg     string
	Missing PartialMissing
}

// PartialMissing 是人工认定的缺口:只能点名实例、宝宝和整条货币。按数量的物品是一整批,scene 失败就零改动,
// 不会只成一部分(资产通道 §4.33),所以不在缺口里;一条货币也是整条到账或整条没到。
type PartialMissing struct {
	Instances  []uint64 // 物品实例 GUID
	Pet        bool
	Currencies []uint32 // 货币类型(0 是银两,合法)
}

func (m PartialMissing) empty() bool {
	return len(m.Instances) == 0 && !m.Pet && len(m.Currencies) == 0
}

// String 是缺口的规范写法,与 ParsePartialMissing 互逆。
func (m PartialMissing) String() string {
	parts := make([]string, 0, len(m.Instances)+len(m.Currencies)+1)
	for _, id := range m.Instances {
		parts = append(parts, "instance:"+strconv.FormatUint(id, 10))
	}
	if m.Pet {
		parts = append(parts, "pet")
	}
	for _, t := range m.Currencies {
		parts = append(parts, "currency:"+strconv.FormatUint(uint64(t), 10))
	}
	return strings.Join(parts, ",")
}

// ParsePartialPart 解析运维工具的 "<腿名>=<缺口>"。
func ParsePartialPart(s string) (PartialPart, error) {
	leg, spec, ok := strings.Cut(s, "=")
	leg = strings.TrimSpace(leg)
	if !ok || leg == "" || strings.TrimSpace(spec) == "" {
		return PartialPart{}, notAllowed("部分发放写成 <腿名>=<缺口>,缺口是逗号分隔的 instance:<guid>、pet、currency:<类型>(收到 %q)", s)
	}
	missing, err := ParsePartialMissing(spec)
	if err != nil {
		return PartialPart{}, err
	}
	return PartialPart{Leg: leg, Missing: missing}, nil
}

// ParsePartialMissing 解析缺口:逗号分隔的 instance:<guid>、pet、currency:<类型>,同一项不能写两次。
func ParsePartialMissing(spec string) (PartialMissing, error) {
	var m PartialMissing
	instances := map[uint64]bool{}
	currencies := map[uint32]bool{}
	for _, raw := range strings.Split(spec, ",") {
		token := strings.TrimSpace(raw)
		kind, value, hasValue := strings.Cut(token, ":")
		switch {
		case token == "pet":
			if m.Pet {
				return PartialMissing{}, notAllowed("缺口里 pet 写了两次")
			}
			m.Pet = true
		case kind == "instance" && hasValue:
			id, err := strconv.ParseUint(value, 10, 64)
			if err != nil || id == 0 {
				return PartialMissing{}, notAllowed("实例 guid 不合法: %q", token)
			}
			if instances[id] {
				return PartialMissing{}, notAllowed("缺口里实例 %d 写了两次", id)
			}
			instances[id] = true
			m.Instances = append(m.Instances, id)
		case kind == "currency" && hasValue:
			t, err := strconv.ParseUint(value, 10, 32)
			if err != nil {
				return PartialMissing{}, notAllowed("货币类型不合法: %q", token)
			}
			if currencies[uint32(t)] {
				return PartialMissing{}, notAllowed("缺口里货币 %d 写了两次", t)
			}
			currencies[uint32(t)] = true
			m.Currencies = append(m.Currencies, uint32(t))
		default:
			return PartialMissing{}, notAllowed("不认识 %q:缺口只能是 instance:<guid>、pet、currency:<类型>(按数量的物品是一整批,不会只缺一部分)", token)
		}
	}
	return m, nil
}

// ManualRequest 是一次人工处置。ExpectVersion 是操作人看过的合同版本:版本变了说明合同已被处理或推进过,
// 不能在旧观察上做决定;同一条命令重跑也因此只生效一次。
type ManualRequest struct {
	CommerceID    uint64
	ExpectVersion uint64
	Action        ManualAction
	Operator      string
	Reason        string
	// Parts:partial 的每条腿与它的缺口。
	Parts []PartialPart
	// Party、To:redirect 改绑哪一方(seller / buyer)、改到哪名角色。
	Party string
	To    uint64
	// LinkedID、LinkedVersion:void-auction 时拍品引用的出价号,与操作人看过的出价版本。
	LinkedID      uint64
	LinkedVersion uint64
}

func (q ManualRequest) validate() error {
	if q.CommerceID == 0 || q.ExpectVersion == 0 {
		return notAllowed("commerce_id 和 expect_version 都必须给")
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
	// 每个动作只收自己的参数:多给的参数说明操作人想的和要执行的不是一回事。
	if q.Action == ManualPartial {
		if len(q.Parts) == 0 || len(q.Parts) > maxNewLegsPerStep {
			return notAllowed("partial 要给出 1~%d 条 <腿名>=<缺口>", maxNewLegsPerStep)
		}
		seen := map[string]bool{}
		for _, p := range q.Parts {
			if p.Leg == "" || p.Missing.empty() {
				return notAllowed("partial 的每条都要有腿名和缺口")
			}
			if seen[p.Leg] {
				return notAllowed("腿 %s 给了两次", p.Leg)
			}
			seen[p.Leg] = true
		}
	} else if len(q.Parts) > 0 {
		return notAllowed("缺口只用于 partial")
	}
	if q.Action == ManualRedirect {
		if q.Party != partySeller && q.Party != partyBuyer {
			return notAllowed("redirect 的 party 只能是 %s 或 %s", partySeller, partyBuyer)
		}
		if q.To == 0 {
			return notAllowed("redirect 要给目标角色")
		}
	} else if q.Party != "" || q.To != 0 {
		return notAllowed("party、目标角色只用于 redirect")
	}
	if q.Action == ManualVoidAuction {
		if q.LinkedID == 0 || q.LinkedVersion == 0 || q.LinkedID == q.CommerceID {
			return notAllowed("void-auction 要给拍品引用的出价号和出价版本")
		}
	} else if q.LinkedID != 0 || q.LinkedVersion != 0 {
		return notAllowed("联动的出价只用于 void-auction")
	}
	return nil
}

// ManualResult 是人工处置的结果;没执行时是计划。
type ManualResult struct {
	VersionBefore  uint64
	Quarantine     *tradepb.TradeCommerceQuarantine
	Stage          string
	Status         uint32
	NewLegs        []string
	SupersededLegs []string
	// redirect:改绑的一方、原角色、目标角色、改前改后的原身份,以及原地改投的未发出腿。
	Party          string
	FromPlayer     uint64
	ToPlayer       uint64
	IdentityBefore *tradepb.TradeRoleIdentity
	IdentityAfter  *tradepb.TradeRoleIdentity
	RetargetedLegs []string
	// PartialParts:partial 认定的每条腿的缺口。
	PartialParts []*tradepb.TradeCommercePartialPart
	// LinkedID、Linked:void-auction 联动的出价与它那一侧的结果(只填版本、转人工记录、阶段、状态、留痕)。
	LinkedID uint64
	Linked   *ManualResult
	// Audit 是写进 manual_actions 的那一条;只算计划时也给出,供核对。
	Audit *tradepb.TradeCommerceManualAction
}

var (
	// ErrManualNotAllowed 包装前置条件不满足的原因,入口按「拒绝,未改动」回。
	ErrManualNotAllowed = errors.New("人工处置前置条件不满足")
	// ErrManualVersionChanged:合同(或联动的出价)已不是操作人看过的那一版,未改动。
	ErrManualVersionChanged = errors.New("合同版本已变化,先重新查看再决定")
	// ErrManualNotFound:合同不存在。
	ErrManualNotFound = errors.New("合同不存在")
)

func notAllowed(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrManualNotAllowed, fmt.Sprintf(format, args...))
}

func versionChanged(id, current, expected uint64) error {
	return fmt.Errorf("%w(合同 %d 现在是 %d,操作人看的是 %d)", ErrManualVersionChanged, id, current, expected)
}

// ManualDeps 是人工处置的依赖:交易库,与读参与方归属、锁改绑目标用的角色护栏(生产是 Engine 的 Repo 与 Roles)。
type ManualDeps struct {
	Repo  *data.CommerceRepo
	Roles RoleLocker
}

func (d ManualDeps) ready() error {
	if d.Repo == nil || d.Roles == nil {
		return errors.New("人工处置缺交易库或角色护栏")
	}
	return nil
}

// ResolveManual 在锁下执行一次人工处置。execute=false 时只算计划,不写库(同样加锁,计划才和执行时看到的一致)。
//
// 步骤:锁角色 → (出价、作废拍卖)锁拍品 guard 与拍品 → 锁合同 → 核对版本 → 补记已有终局、还没记进 workflow 的腿
// (与 advance 同一判定)→ 按动作改合同(planManualAction)→ 追加留痕 → 带版本条件保存。
// 保存时 updated_ms 写成 nowMs,合同立即重新到期;nowMs 要用引擎的时钟(Due 按它取到期行)。
func ResolveManual(ctx context.Context, deps ManualDeps, req ManualRequest, nowMs uint64, execute bool) (ManualResult, error) {
	if err := req.validate(); err != nil {
		return ManualResult{}, err
	}
	if err := deps.ready(); err != nil {
		return ManualResult{}, err
	}
	locks, err := manualLockSet(ctx, deps.Repo, req)
	if err != nil {
		return ManualResult{}, err
	}
	var result ManualResult
	err = deps.Roles.RunTx(ctx, func() error {
		return deps.Repo.Tx(ctx, func(tx *sql.Tx) error {
			var err error
			result, err = resolveManualTx(ctx, deps, tx, req, locks, nowMs, execute)
			return err
		})
	})
	if err != nil {
		return ManualResult{}, manualError(err)
	}
	return result, nil
}

// manualError 把角色护栏的结论翻成人工处置的拒绝:目录里没有这名角色、副本自愈之后仍判不了,都是前置条件不满足,
// 不是故障;其余错误(存储、提交)原样回,由入口按故障或结局未知处理。
func manualError(err error) error {
	switch {
	case errors.Is(err, ErrManualNotAllowed), errors.Is(err, ErrManualVersionChanged), errors.Is(err, ErrManualNotFound):
		return err
	case errors.Is(err, rolefence.ErrRoleNotFound):
		return notAllowed("目录里没有这名角色: %v", err)
	case errors.Is(err, errRoleConflict):
		return notAllowed("参与方角色的归属判不了(副本自愈之后仍缺): %v", err)
	}
	return err
}

// manualLocks 是一次人工处置要先锁的东西:角色(升序锁),以及拍品(先锁它的 guard 行与拍品行,再锁出价)。
type manualLocks struct {
	players []uint64
	lotID   uint64 // 非 0:出价所属的拍品,或作废拍卖的拍品本身
}

// manualLockSet 不加锁读一次合同(作废拍卖时连同出价),定锁序:先锁哪些角色、要不要先锁拍品。
// 参与方只有带版本条件的写会改,锁内版本对得上,参与方就是这里读到的;对不上回版本已变。
func manualLockSet(ctx context.Context, repo *data.CommerceRepo, req ManualRequest) (manualLocks, error) {
	pre, err := repo.Get(ctx, req.CommerceID)
	if errors.Is(err, sql.ErrNoRows) {
		return manualLocks{}, ErrManualNotFound
	}
	if err != nil {
		return manualLocks{}, err
	}
	if pre.Version != req.ExpectVersion {
		return manualLocks{}, versionChanged(pre.CommerceId, pre.Version, req.ExpectVersion)
	}
	locks := manualLocks{players: partyPlayers(pre)}
	switch {
	case pre.Kind == BidKind:
		locks.lotID = pre.ParentId
	case req.Action == ManualVoidAuction && pre.Kind == 5:
		locks.lotID = pre.CommerceId
		bid, err := repo.Get(ctx, req.LinkedID)
		if errors.Is(err, sql.ErrNoRows) {
			return manualLocks{}, notAllowed("出价 %d 不存在", req.LinkedID)
		}
		if err != nil {
			return manualLocks{}, err
		}
		if bid.Version != req.LinkedVersion {
			return manualLocks{}, versionChanged(bid.CommerceId, bid.Version, req.LinkedVersion)
		}
		locks.players = append(locks.players, partyPlayers(bid)...)
	}
	if req.Action == ManualRedirect {
		locks.players = append(locks.players, req.To)
	}
	return locks, nil
}

// resolveManualTx 是 ResolveManual 的事务体,可能被事务重试或角色副本自愈重跑,每次从头读。
func resolveManualTx(ctx context.Context, deps ManualDeps, tx *sql.Tx, req ManualRequest, locks manualLocks, nowMs uint64, execute bool) (ManualResult, error) {
	repo := deps.Repo
	// 1. 角色护栏副本,按 player_id 升序(先角色,后 guard 与合同行,与 advance、acceptBid 同一锁序)。
	facts, err := readRoleFacts(ctx, tx, deps.Roles, locks.players, true)
	if err != nil {
		return ManualResult{}, err
	}
	c := manualContext{req: req}
	if req.Action == ManualRedirect {
		if c.target, err = redirectTarget(req.To, facts[req.To]); err != nil {
			return ManualResult{}, err
		}
	}
	// 2. 拍品:先锁 guard 行,再锁拍品行(出价所属的拍品可以不存在)。
	var lot *tradepb.TradeCommerceRecord
	if locks.lotID != 0 {
		if err := lockAuctionGuard(ctx, tx, locks.lotID); err != nil {
			return ManualResult{}, err
		}
		lot, err = repo.Lock(ctx, tx, locks.lotID)
		if errors.Is(err, sql.ErrNoRows) {
			lot, err = nil, nil
		}
		if err != nil {
			return ManualResult{}, err
		}
	}
	// 3. 要改的合同(作废拍卖时就是拍品本身)。
	r := lot
	if locks.lotID != req.CommerceID {
		r, err = repo.Lock(ctx, tx, req.CommerceID)
		if errors.Is(err, sql.ErrNoRows) {
			return ManualResult{}, ErrManualNotFound
		}
		if err != nil {
			return ManualResult{}, err
		}
	}
	if r == nil {
		return ManualResult{}, ErrManualNotFound
	}
	if r.Version != req.ExpectVersion {
		return ManualResult{}, versionChanged(r.CommerceId, r.Version, req.ExpectVersion)
	}
	w, err := data.Workflow(r)
	if err != nil {
		return ManualResult{}, notAllowed("workflow 解不开,只能取证后手工修数据: %v", err)
	}
	if r.Kind == BidKind && lot != nil {
		c.lot = lot
		if lw, decodeErr := data.Workflow(lot); decodeErr == nil {
			c.lotW = lw
		}
	}
	read := func(ctx context.Context, opID uint64) (data.LegOutcome, error) { return repo.LegResult(ctx, tx, opID) }
	// 4. 作废拍卖联动的出价:锁在拍品之后。
	if req.Action == ManualVoidAuction && r.Kind == 5 {
		bid, err := repo.Lock(ctx, tx, req.LinkedID)
		if errors.Is(err, sql.ErrNoRows) {
			return ManualResult{}, notAllowed("出价 %d 不存在", req.LinkedID)
		}
		if err != nil {
			return ManualResult{}, err
		}
		if bid.Version != req.LinkedVersion {
			return ManualResult{}, versionChanged(bid.CommerceId, bid.Version, req.LinkedVersion)
		}
		bw, err := data.Workflow(bid)
		if err != nil {
			return ManualResult{}, notAllowed("出价 %d 的 workflow 解不开: %v", bid.CommerceId, err)
		}
		if err := settleLegOutcomes(ctx, bw, read, legSettling{}); err != nil {
			return ManualResult{}, err
		}
		c.bid, c.bidW, c.bidChanges = bid, bw, partyChanges(bid, bw, facts)
	}
	// 5. 补记已有终局的腿,再按动作改合同。
	if err := settleLegOutcomes(ctx, w, read, settlingFor(req)); err != nil {
		return ManualResult{}, err
	}
	c.changes = partyChanges(r, w, facts)
	versionBefore := r.Version
	result, err := planManualAction(r, w, c)
	if err != nil {
		return ManualResult{}, err
	}
	result.VersionBefore = versionBefore
	result.Audit = manualAudit(req, result, nowMs)
	if c.bid != nil {
		result.Linked.VersionBefore = req.LinkedVersion
		result.Linked.Audit = &tradepb.TradeCommerceManualAction{
			Action: string(req.Action), Operator: req.Operator, Reason: req.Reason, AtMs: nowMs,
			VersionBefore: req.LinkedVersion, Quarantine: result.Linked.Quarantine, LinkedCommerceId: r.CommerceId,
		}
	}
	if !execute {
		return result, nil
	}
	w.ManualActions = append(w.ManualActions, result.Audit)
	if err := repo.Save(ctx, tx, r, w, nowMs); err != nil {
		return ManualResult{}, err
	}
	if c.bid != nil {
		c.bidW.ManualActions = append(c.bidW.ManualActions, result.Linked.Audit)
		if err := repo.Save(ctx, tx, c.bid, c.bidW, nowMs); err != nil {
			return ManualResult{}, err
		}
	}
	return result, nil
}

// redirectTarget 核对改绑目标角色:锁内读到的副本里它没删、没被冻结、不在投票中。冻结、投票中也可能是解冻推送丢了,
// 所以先回 *rolefence.StaleError 让 RoleLocker.RunTx 读权威状态再判一次,第二次还是冻结就回拒绝。
func redirectTarget(player uint64, fact roleFact) (*dspb.AccountRoleOwnership, error) {
	switch {
	case fact.deleted:
		return nil, notAllowed("目标角色 %d 已删除", player)
	case fact.unknown || fact.owned == nil:
		return nil, notAllowed("读不到目标角色 %d 的归属", player)
	case fact.owned.GetTradeListingId() != 0 || fact.owned.GetPrepareToken() != 0:
		return nil, &rolefence.StaleError{Players: []uint64{player}, Reject: notAllowed("目标角色 %d 被冻结或正在投票(出售、删除),等它结束再改绑", player)}
	}
	return fact.owned, nil
}

// manualAudit 是写进 manual_actions 的那一条。
func manualAudit(req ManualRequest, result ManualResult, nowMs uint64) *tradepb.TradeCommerceManualAction {
	return &tradepb.TradeCommerceManualAction{
		Action:           string(req.Action),
		Operator:         req.Operator,
		Reason:           req.Reason,
		AtMs:             nowMs,
		VersionBefore:    result.VersionBefore,
		Quarantine:       result.Quarantine,
		NewLegs:          result.NewLegs,
		SupersededLegs:   result.SupersededLegs,
		Party:            result.Party,
		FromPlayerId:     result.FromPlayer,
		ToPlayerId:       result.ToPlayer,
		IdentityBefore:   result.IdentityBefore,
		IdentityAfter:    result.IdentityAfter,
		RetargetedLegs:   result.RetargetedLegs,
		LinkedCommerceId: result.LinkedID,
		PartialParts:     result.PartialParts,
	}
}

// legSettling 是 settleLegOutcomes 对个别动作的放宽。
type legSettling struct {
	partialLegs map[string]bool // partial 点名的腿:允许是部分发放,记下结局
	unissuedOK  bool            // redirect:还没发出的腿原地改投,不算在途
}

func settlingFor(req ManualRequest) legSettling {
	switch req.Action {
	case ManualPartial:
		named := make(map[string]bool, len(req.Parts))
		for _, p := range req.Parts {
			named[p.Leg] = true
		}
		return legSettling{partialLegs: named}
	case ManualRedirect:
		return legSettling{unissuedOK: true}
	}
	return legSettling{}
}

// settleLegOutcomes 把已经有终局、还没记进 workflow 的腿记上。转人工的合同不再走 advance,
// 隔离时被截止的托管扣款之后才有结局,所以人工处置之前要补记一次,判定与 advance 相同(recordLegOutcome)。
// 还在途的腿、查不到 outbox 行的腿拒绝:要等结局或取证。部分发放的腿只有 partial 点名的才记,
// 其余拒绝(要给出缺口);还没发出的腿只有 redirect 接受。
func settleLegOutcomes(ctx context.Context, w *tradepb.TradeCommerceWorkflow, read func(context.Context, uint64) (data.LegOutcome, error), opt legSettling) error {
	for _, l := range w.Legs {
		if l.SupersededBy != "" || l.Outcome != 0 {
			continue
		}
		if l.OpId == 0 {
			if opt.unissuedOK {
				continue
			}
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
		if out.Status == tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED_PARTIAL && !opt.partialLegs[l.Role] {
			return notAllowed("腿 %s op_id=%d 部分发放,用 partial 给出它的缺口(资产通道 §4.33)", l.Role, l.OpId)
		}
		if err := recordLegOutcome(l, out); err != nil {
			return notAllowed("%v", err)
		}
	}
	return nil
}

// recordLegOutcome 把一条终局记进腿:结局和托管快照。advance 与人工处置共用。
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

// ── show ────────────────────────────────────────────────────────

// ManualReport 是 show 的结果:合同、每条腿与它的 op 行、两方的归属、关联合同、每种动作现在能否处理。
// 腿与归属是锁外读的;每种动作能否处理按 ResolveManual 只算计划(加锁、不写)判定。执行时仍在锁下重新判定。
type ManualReport struct {
	Record    *tradepb.TradeCommerceRecord
	Workflow  *tradepb.TradeCommerceWorkflow // 解不开时为 nil,见 DecodeErr
	DecodeErr error
	Legs      []ManualLegFact
	Parties   []ManualPartyFact
	Linked    *tradepb.TradeCommerceRecord // 出价所属的拍品,或拍品引用的出价;没有为 nil
	Verdicts  []ManualVerdict
}

// ManualLegFact 是一条腿与它 op 行的现状。
type ManualLegFact struct {
	Leg      *tradepb.TradeCommerceLeg
	OpStatus tradepb.TradeAssetOpStatus // 没发出为 UNSPECIFIED
	FinalMs  uint64                     // 终态行的终结时刻
	ReadErr  error
}

// ManualPartyFact 是一方的角色现在归谁(不带账号原文)。
type ManualPartyFact struct {
	Party            string
	Player           uint64
	ContractRevision uint64 // 合同里原身份的修订号
	CurrentRevision  uint64 // 副本里现在的修订号;删角、判不了时为 0
	SameAccount      bool
	Deleted          bool
	Frozen           bool // 被挂单冻结或在投票中
	Unknown          bool // 副本缺行,判不了
	Changed          bool // 已不可逆地不归原身份:用 redirect
}

// ManualVerdict 是一种动作现在能否处理。要额外参数的动作(partial、redirect)列出候选,执行时再给参数。
type ManualVerdict struct {
	Action  ManualAction
	Allowed bool
	Detail  string
}

// ShowManual 给运维核对用。nowMs 只用于试算计划的留痕时刻。
func ShowManual(ctx context.Context, deps ManualDeps, id, nowMs uint64) (ManualReport, error) {
	if err := deps.ready(); err != nil {
		return ManualReport{}, err
	}
	r, err := deps.Repo.Get(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return ManualReport{}, ErrManualNotFound
	}
	if err != nil {
		return ManualReport{}, err
	}
	report := ManualReport{Record: r}
	w, err := data.Workflow(r)
	if err != nil {
		report.DecodeErr = err
		return report, nil
	}
	report.Workflow = w
	for _, l := range w.Legs {
		fact := ManualLegFact{Leg: l}
		if l.OpId != 0 {
			out, readErr := deps.Repo.LegResult(ctx, deps.Repo.DB, l.OpId)
			fact.OpStatus, fact.FinalMs, fact.ReadErr = out.Status, out.FinalMs, readErr
		}
		report.Legs = append(report.Legs, fact)
	}
	if report.Parties, err = showParties(ctx, deps, r, w); err != nil {
		return ManualReport{}, err
	}
	linkedID := uint64(0)
	switch {
	case r.Kind == BidKind:
		linkedID = r.ParentId
	case r.Kind == 5:
		linkedID = w.WinningBidId
	}
	if linkedID != 0 {
		linked, err := deps.Repo.Get(ctx, linkedID)
		switch {
		case err == nil:
			report.Linked = linked
		case !errors.Is(err, sql.ErrNoRows):
			return ManualReport{}, err
		}
	}
	report.Verdicts = showVerdicts(ctx, deps, report, nowMs)
	return report, nil
}

// showParties 读两方的归属:在一个只为加锁读的事务里读副本(不自愈),读完回滚。
func showParties(ctx context.Context, deps ManualDeps, r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow) ([]ManualPartyFact, error) {
	tx, err := deps.Repo.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	facts, err := readRoleFacts(ctx, tx, deps.Roles, partyPlayers(r), false)
	if err != nil {
		return nil, err
	}
	var out []ManualPartyFact
	for _, p := range contractParties(r, w) {
		fact := facts[p.player]
		pf := ManualPartyFact{Party: p.name, Player: p.player, ContractRevision: p.identity.GetOwnerRevision(), Deleted: fact.deleted, Unknown: fact.unknown}
		if fact.owned != nil {
			pf.CurrentRevision = fact.owned.GetOwnerRevision()
			pf.SameAccount = p.identity != nil && fact.owned.GetAccount() == p.identity.GetAccount()
			pf.Frozen = fact.owned.GetTradeListingId() != 0 || fact.owned.GetPrepareToken() != 0
		}
		pf.Changed = partyChange(p, fact) != nil
		out = append(out, pf)
	}
	return out, nil
}

// showVerdicts 逐个动作试算。不要额外参数的动作按 ResolveManual 只算计划判定(与执行同一锁序、同一判据);
// partial、redirect 列出候选(哪几条腿部分发放、哪一方已换主),执行时由操作人给出缺口和目标角色。
func showVerdicts(ctx context.Context, deps ManualDeps, report ManualReport, nowMs uint64) []ManualVerdict {
	r, w := report.Record, report.Workflow
	verdicts := make([]ManualVerdict, 0, len(ManualActions))
	for _, action := range ManualActions {
		req := ManualRequest{CommerceID: r.CommerceId, ExpectVersion: r.Version, Action: action, Operator: "show", Reason: "show"}
		switch action {
		case ManualPartial:
			var legs []string
			for _, fact := range report.Legs {
				l := fact.Leg
				if l.SupersededBy == "" && (l.Outcome == partialOutcome || l.Outcome == 0 && fact.OpStatus == tradepb.TradeAssetOpStatus_TRADE_ASSET_OP_STATUS_APPLIED_PARTIAL) {
					legs = append(legs, l.Role)
				}
			}
			if len(legs) == 0 {
				verdicts = append(verdicts, ManualVerdict{Action: action, Detail: "没有部分发放的腿"})
				continue
			}
			verdicts = append(verdicts, ManualVerdict{Action: action, Allowed: true,
				Detail: "部分发放的腿 " + strings.Join(legs, ", ") + ":按 scene 的 [AssetOp] partial 日志与流水给出每条的缺口"})
			continue
		case ManualRedirect:
			var changed []string
			for _, p := range report.Parties {
				if p.Changed {
					changed = append(changed, fmt.Sprintf("%s player=%d", p.Party, p.Player))
				}
			}
			if len(changed) == 0 {
				verdicts = append(verdicts, ManualVerdict{Action: action, Detail: "两方的角色都还归原身份(或副本判不了)"})
				continue
			}
			verdicts = append(verdicts, ManualVerdict{Action: action, Allowed: true,
				Detail: "已不归原身份: " + strings.Join(changed, ", ") + ";按原账号的申请给出它名下的目标角色"})
			continue
		case ManualVoidAuction:
			if r.Kind != 5 || w.WinningBidId == 0 || report.Linked == nil {
				verdicts = append(verdicts, ManualVerdict{Action: action, Detail: "不是有出价的拍品"})
				continue
			}
			req.LinkedID, req.LinkedVersion = report.Linked.CommerceId, report.Linked.Version
		}
		plan, err := ResolveManual(ctx, deps, req, nowMs, false)
		if err != nil {
			verdicts = append(verdicts, ManualVerdict{Action: action, Detail: err.Error()})
			continue
		}
		verdicts = append(verdicts, ManualVerdict{Action: action, Allowed: true, Detail: describePlan(plan)})
	}
	return verdicts
}

// describePlan 是一份计划的一行摘要。
func describePlan(plan ManualResult) string {
	text := fmt.Sprintf("stage=%s status=%d new_legs=%v superseded=%v", plan.Stage, plan.Status, plan.NewLegs, plan.SupersededLegs)
	if len(plan.RetargetedLegs) > 0 {
		text += fmt.Sprintf(" retargeted=%v", plan.RetargetedLegs)
	}
	if plan.Linked != nil {
		text += fmt.Sprintf(" linked=%d linked_stage=%s linked_status=%d", plan.LinkedID, plan.Linked.Stage, plan.Linked.Status)
	}
	return text
}
