package commerce

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	base "proto/common/base"
	dspb "proto/data_service"
	rfpb "proto/role_fence"
	tradepb "proto/trade"
	"shared/accountdirectory"
	"shared/rolefence"
	"shared/roleidentity"
	"trade/internal/data"

	"google.golang.org/protobuf/proto"
)

// RoleLocker 是合同事务和角色护栏的接缝(docs/design/horizontal-scale-logical-db.md §3)。
// 所有新增参与者先锁角色,再锁合同;事务里不调 RPC。
type RoleLocker interface {
	// RunTx 跑一次 action;action 回 *rolefence.StaleError 时读权威状态写入副本,再跑一次。action 自己开事务,RPC 只发生在两次之间。
	RunTx(ctx context.Context, action func() error) error
	// Lock 按 player_id 升序锁全部角色的副本并逐个判定。确定拒绝回 Expect.Reject,副本判不了回 *rolefence.StaleError。
	Lock(ctx context.Context, tx *sql.Tx, expects ...rolefence.Expect) error
	// Ownership 读本事务要用的角色归属。trade_listing_id 是最终令牌,prepare_token 是还在投票的令牌。调用方须已 Lock 过这名角色。
	Ownership(ctx context.Context, tx *sql.Tx, player uint64) (*dspb.AccountRoleOwnership, error)
}

// RoleAuthority 读本库的 role_fence 副本。目录是唯一权威,副本判不了的交给 Guard 自愈。
type RoleAuthority struct{ guard *rolefence.Guard }

func NewRoleAuthority(db *sql.DB, reader rolefence.StateReader) (*RoleAuthority, error) {
	if db == nil || reader == nil {
		return nil, errors.New("交易角色护栏缺连接或权威状态读取")
	}
	return &RoleAuthority{guard: rolefence.NewGuard(db, reader)}, nil
}

func (a *RoleAuthority) RunTx(ctx context.Context, action func() error) error {
	return a.guard.RunTx(ctx, action)
}

func (a *RoleAuthority) Lock(ctx context.Context, tx *sql.Tx, expects ...rolefence.Expect) error {
	_, err := rolefence.LockAll(ctx, tx, expects...)
	return err
}

// VerifyOriginalOwner 是客户端入口的归属预检(docs/design/gate-online-identity.md §9.3):不加锁读本库护栏副本,
// 判 ctx 里原会话身份的角色此刻是否仍归它。浏览、详情、我的货架、收藏、盘口挂单列表不进合同事务,这是它们唯一的归属检查;
// 合同写入照旧在 lockRoles 持锁复核。
//
// 不看冻结:卖家把自己这名角色挂人民币寄售之后角色就是冻结的,重放与查询仍要放行(verifyReplayActor)。
// 确定不归(已换主、已删、目录里没有)回 ErrConflict;其余错误是这一次判不了,入口按可重试处理。
func (a *RoleAuthority) VerifyOriginalOwner(ctx context.Context) error {
	original, ok := roleidentity.Original(ctx)
	if !ok || a == nil {
		return ErrConflict
	}
	err := a.guard.VerifyOwner(ctx, rolefence.Expect{PlayerID: original.Role.GetPlayerId(), Account: original.Account, OwnerRevision: original.OwnerRevision, Reject: ErrConflict})
	if errors.Is(err, rolefence.ErrRoleNotFound) {
		return ErrConflict
	}
	return err
}

// OwnerRejected 判 VerifyOriginalOwner 的错误是不是确定不归;为假就是这一次判不了。
func OwnerRejected(err error) bool { return errors.Is(err, ErrConflict) }

func (a *RoleAuthority) Ownership(ctx context.Context, tx *sql.Tx, player uint64) (*dspb.AccountRoleOwnership, error) {
	if player == 0 {
		return nil, ErrConflict
	}
	fence, err := rolefence.Lock(ctx, tx, player)
	if errors.Is(err, rolefence.ErrMissing) {
		return nil, staleRole(player)
	}
	if err != nil {
		return nil, err
	}
	if fence.Account == "" {
		return nil, ErrConflict
	}
	owned := &dspb.AccountRoleOwnership{Account: fence.Account, OwnerRevision: fence.OwnerRevision, Role: &base.AccountSimplePlayer{PlayerId: player}}
	// 两个令牌分开给:把「角色被冻结」当最终结论的地方(退款)只认最终令牌。
	if fence.FreezeFinal {
		owned.TradeListingId = fence.FreezeToken
	} else {
		owned.PrepareToken = fence.FreezeToken
	}
	return owned, nil
}

// staleRole 表示副本判不了这名角色,要读权威状态再判一次。还判不了就是角色冲突。
func staleRole(player uint64) error {
	return &rolefence.StaleError{Players: []uint64{player}, Reject: roleConflict()}
}

// roleTx 是带角色锁的合同事务入口。副本判不了时事务已回滚,读完权威状态整笔重跑一次。
func (e *Engine) roleTx(ctx context.Context, body func(*sql.Tx) error) error {
	if e.Roles == nil {
		return errors.New("交易角色提交互锁未配置")
	}
	return rolledBack(e.Roles.RunTx(ctx, func() error { return e.Repo.Tx(ctx, body) }))
}

// lockRoles 是合同事务的第一把锁。allowListing 是这笔业务自己持有的令牌:令牌等于自己的 listing 才放行,别的角色和合同不获得这份例外。
//
// 拒绝一律标成 role_conflict:角色被另一条挂单冻结、已删除、换了主都是长期状态,供后台推进分钟级复查(2026-10-22);客户端拿到的 tip 码不变。
func (e *Engine) lockRoles(ctx context.Context, tx *sql.Tx, allowListing uint64, players ...uint64) error {
	expects := make([]rolefence.Expect, 0, len(players)+1)
	for _, player := range players {
		if player != 0 {
			expects = append(expects, rolefence.Expect{PlayerID: player, AllowToken: allowListing, Reject: roleConflict()})
		}
	}
	// 客户端actor可能也是交易参与者;并入同一升序首锁,不能先锁参与者再回头锁actor。
	if original, present := roleidentity.Original(ctx); present {
		expects = append(expects, rolefence.Expect{PlayerID: original.Role.PlayerId, Account: original.Account, OwnerRevision: original.OwnerRevision, AllowToken: allowListing, Reject: roleConflict()})
	}
	return e.Roles.Lock(ctx, tx, expects...)
}

// 后台不重新制造调用者身份：只核持久合同原账号/修订，缺原件或已换主均保留合同拒发资产。
func (e *Engine) checkContractIdentities(ctx context.Context, tx *sql.Tx, r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow) error {
	for _, party := range []struct {
		player   uint64
		identity *tradepb.TradeRoleIdentity
	}{{r.SellerPlayerId, w.SellerIdentity}, {r.BuyerPlayerId, w.BuyerIdentity}} {
		if party.player == 0 {
			continue
		}
		if party.identity == nil || !accountdirectory.ValidAccount(party.identity.Account) || party.identity.OwnerRevision == 0 {
			return corrupt(errors.New("交易合同缺少原归属身份"))
		}
		owned, err := e.Roles.Ownership(ctx, tx, party.player)
		if err != nil {
			return asRoleConflict(err)
		}
		if owned.OwnerRevision < party.identity.OwnerRevision {
			return staleRole(party.player)
		}
		if owned.Account != party.identity.Account || owned.OwnerRevision != party.identity.OwnerRevision {
			// 原主已换(owner_revision 只增不减,不会再对上):合同与对方托管保留,等人工,不按存储抖动每 10 秒重试。
			return roleConflict()
		}
	}
	return nil
}

// ownerChanged 判参与方的角色是否已不可逆地不归合同里的原身份(jubaozhai-market.md §15.7.1 第 1 类):
// 副本修订号已超过原身份,或修订号相同而账号不同。owner_revision 只增不减,这两种复查多少次都不会变回来。
// 副本修订号落后于原身份是副本要自愈,不算;角色已删(Ownership 回 ErrConflict)由调用方另判。
func ownerChanged(owned *dspb.AccountRoleOwnership, identity *tradepb.TradeRoleIdentity) bool {
	return owned.GetOwnerRevision() > identity.GetOwnerRevision() ||
		owned.GetOwnerRevision() == identity.GetOwnerRevision() && owned.GetAccount() != identity.GetAccount()
}

// 合同的两方。改绑(redirect)按名字点名,转人工记录也用这两个名字。
const (
	partySeller = "seller"
	partyBuyer  = "buyer"
)

// contractParty 是合同的一方:它的角色与合同里存的原身份。
type contractParty struct {
	name     string
	player   uint64
	identity *tradepb.TradeRoleIdentity
}

// contractParties 是合同的卖方与买方(没有的一方不列),按 player_id 升序,证据与留痕的顺序因此稳定。
func contractParties(r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow) []contractParty {
	parties := make([]contractParty, 0, 2)
	if r.GetSellerPlayerId() != 0 {
		parties = append(parties, contractParty{name: partySeller, player: r.GetSellerPlayerId(), identity: w.GetSellerIdentity()})
	}
	if r.GetBuyerPlayerId() != 0 {
		parties = append(parties, contractParty{name: partyBuyer, player: r.GetBuyerPlayerId(), identity: w.GetBuyerIdentity()})
	}
	if len(parties) == 2 && parties[1].player < parties[0].player {
		parties[0], parties[1] = parties[1], parties[0]
	}
	return parties
}

// partyPlayers 是合同两方的角色(没有的一方是 0,readRoleFacts 会跳过)。
func partyPlayers(r *tradepb.TradeCommerceRecord) []uint64 {
	return []uint64{r.GetSellerPlayerId(), r.GetBuyerPlayerId()}
}

// roleFact 是锁内读到的一名角色的归属。
type roleFact struct {
	owned   *dspb.AccountRoleOwnership // 角色已删、副本缺行时为 nil
	deleted bool                       // 账号已清空(删角墓碑)
	unknown bool                       // 副本缺行,判不了
}

// readRoleFacts 在调用方的事务里按 player_id 升序读一组角色的护栏副本。Ownership 自带行锁,所以这就是加锁:
// 与 advance、acceptBid 同一锁序,先角色,后 guard 与合同行。
//   - heal=true:副本缺行回 *rolefence.StaleError,由 RoleLocker.RunTx 读权威状态写入副本之后整笔重跑(人工处理用);
//   - heal=false:副本缺行记成 unknown(引擎的推迟路径只有 tickDeferBudget,不在那里读目录)。
//
// 存储错误原样返回。
func readRoleFacts(ctx context.Context, tx *sql.Tx, roles RoleLocker, players []uint64, heal bool) (map[uint64]roleFact, error) {
	sorted := make([]uint64, 0, len(players))
	for _, player := range players {
		if player != 0 {
			sorted = append(sorted, player)
		}
	}
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)
	facts := make(map[uint64]roleFact, len(sorted))
	var missing []uint64
	for _, player := range sorted {
		owned, err := roles.Ownership(ctx, tx, player)
		var stale *rolefence.StaleError
		switch {
		case err == nil:
			facts[player] = roleFact{owned: owned}
		case errors.As(err, &stale):
			// 缺行的一次收齐再自愈(与 rolefence.LockAll 同一做法):逐个回的话第二名还要再自愈一次,就被 RunTx 当成真实拒绝了。
			missing = append(missing, player)
			facts[player] = roleFact{unknown: true}
		case errors.Is(err, ErrConflict):
			// Ownership 只在账号已清空(删角墓碑)时回 ErrConflict;player 为 0 的已在上面跳过。
			facts[player] = roleFact{deleted: true}
		default:
			return nil, err
		}
	}
	if heal && len(missing) > 0 {
		return nil, &rolefence.StaleError{Players: missing, Reject: roleConflict()}
	}
	return facts, nil
}

// partyRoleChange 是一方的角色已不可逆地不归原身份的证据,写进转人工记录与改绑留痕。不带账号原文,只说账号是否相同。
type partyRoleChange struct {
	party       string
	player      uint64
	deleted     bool   // 角色已删(账号清空)
	revision    uint64 // 副本里现在的修订号(删角时为 0)
	contractRev uint64 // 合同里原身份的修订号
	sameAccount bool
}

func (c partyRoleChange) String() string {
	if c.deleted {
		return fmt.Sprintf("%s player=%d role deleted (contract owner_revision=%d)", c.party, c.player, c.contractRev)
	}
	return fmt.Sprintf("%s player=%d owner_revision=%d (contract %d) same_account=%t", c.party, c.player, c.revision, c.contractRev, c.sameAccount)
}

// describeRoleChanges 把几方的证据拼成一行,写进转人工记录的 detail。
func describeRoleChanges(changes []partyRoleChange) string {
	parts := make([]string, 0, len(changes))
	for _, c := range changes {
		parts = append(parts, c.String())
	}
	return strings.Join(parts, "; ")
}

// partyChange 判一方是否已不可逆地不归原身份(纯函数):角色已删,或 ownerChanged。
// 副本缺行判不了、合同缺原身份(坏数据,由 checkContractIdentities 按 corrupt 隔离)时回 nil。
func partyChange(p contractParty, fact roleFact) *partyRoleChange {
	switch {
	case p.identity == nil || fact.unknown:
		return nil
	case fact.deleted:
		return &partyRoleChange{party: p.name, player: p.player, deleted: true, contractRev: p.identity.GetOwnerRevision()}
	case fact.owned == nil || !ownerChanged(fact.owned, p.identity):
		return nil
	}
	return &partyRoleChange{party: p.name, player: p.player, revision: fact.owned.GetOwnerRevision(), contractRev: p.identity.GetOwnerRevision(),
		sameAccount: fact.owned.GetAccount() == p.identity.GetAccount()}
}

// partyChanges 是合同里已不可逆地不归原身份的几方,按 player_id 升序。facts 要覆盖合同两方(readRoleFacts 读 partyPlayers)。
func partyChanges(r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow, facts map[uint64]roleFact) []partyRoleChange {
	var out []partyRoleChange
	for _, p := range contractParties(r, w) {
		if change := partyChange(p, facts[p.player]); change != nil {
			out = append(out, *change)
		}
	}
	return out
}

// OwnsRecord 用持久原账号/修订筛选私有历史，角色的新主人不会继承旧主的合同身份。
func OwnsRecord(ctx context.Context, r *tradepb.TradeCommerceRecord, w *tradepb.TradeCommerceWorkflow) bool {
	original, ok := roleidentity.Original(ctx)
	if !ok || r == nil || w == nil {
		return false
	}
	for _, party := range []struct {
		player   uint64
		identity *tradepb.TradeRoleIdentity
	}{{r.SellerPlayerId, w.SellerIdentity}, {r.BuyerPlayerId, w.BuyerIdentity}} {
		if party.player == original.Role.PlayerId && party.identity != nil && party.identity.Account == original.Account && party.identity.OwnerRevision == original.OwnerRevision {
			return true
		}
	}
	return false
}

func (e *Engine) verifyReplayActor(ctx context.Context, old *tradepb.TradeCommerceRecord) error {
	if _, present := roleidentity.Original(ctx); !present {
		return nil
	}
	allow := uint64(0)
	if old.Kind == 6 {
		workflow, err := data.Workflow(old)
		if err != nil {
			return err
		}
		if workflow.Rmb != nil && workflow.Rmb.CharacterPlayerId != 0 {
			allow = old.CommerceId
		}
	}
	return e.roleTx(ctx, func(tx *sql.Tx) error { return e.lockRoles(ctx, tx, allow) })
}

// 挂起 blocker 是低基数枚举,进协调者的指标 label。
const (
	roleBlockerCommerce     = "trade_commerce"
	roleBlockerAssetOp      = "trade_asset_op"
	roleBlockerTooManyDeals = "too_many_contracts"
)

// maxRolePendingCommerce 是一名角色名下的未终局合同上限。超过就是挂起:不能截断全集后宣称无挂起合同。
// 它是确定性结论,不能回错误:那样协调者按不可达处理,令牌留着等重放,而重放还是同样的结果。
const maxRolePendingCommerce = 256

// RolePending 是 trade 的挂起全集,在 PrepareRoleFreeze 的事务里跑,此时已持有副本行锁。
// 在途资产属于原账号,不能随角色过户给新主;撮合/竞价子单由未终局父合同在同事务派生,故所有资产合同类型均检查,不只人民币寄售。
//
// 两条例外,判据一条都不能松:官方退款合同已确定零新增资产腿,商城仍完成原款退款,不阻止后续出售;
// 角色寄售自己的 rmb_freezing 只放行令牌等于本 listing、原账号和修订都对得上的那一条。
func RolePending(ctx context.Context, tx *sql.Tx, state *rfpb.RoleFenceState) (string, error) {
	blocker, err := rolePendingCommerce(ctx, tx, state)
	if err != nil || blocker != "" {
		return blocker, err
	}
	// 6=ABANDONED 已证明 scene 未见过该 seq;完整终态以外(含 0、manual、未知)全部阻塞。
	var op uint64
	err = tx.QueryRowContext(ctx, "SELECT op_id FROM trade_asset_op WHERE player_id=? AND (status NOT IN (2,3,4,6) OR status IS NULL) LIMIT 1 FOR UPDATE", state.PlayerId).Scan(&op)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return roleBlockerAssetOp, nil
}

func rolePendingCommerce(ctx context.Context, tx *sql.Tx, state *rfpb.RoleFenceState) (string, error) {
	rows, err := tx.QueryContext(ctx, "SELECT commerce_id,kind,status,workflow_proto FROM trade_commerce WHERE (seller_player_id=? OR buyer_player_id=?) AND (status NOT IN (7,11) OR status IS NULL) ORDER BY commerce_id LIMIT ? FOR UPDATE", state.PlayerId, state.PlayerId, maxRolePendingCommerce+1)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
		if count > maxRolePendingCommerce {
			return roleBlockerTooManyDeals, nil
		}
		var id uint64
		var kind, status uint32
		var raw []byte
		if err := rows.Scan(&id, &kind, &status, &raw); err != nil {
			return "", err
		}
		w := &tradepb.TradeCommerceWorkflow{}
		if len(raw) > 1<<20 || proto.Unmarshal(raw, w) != nil {
			return roleBlockerCommerce, nil
		}
		if kind == 1 && status == 8 && w.Stage == "official_refund" {
			continue
		}
		if id != state.FreezeToken || kind != 6 || status != 3 || w.Rmb == nil || w.Rmb.CharacterPlayerId != state.PlayerId || w.Rmb.SellerAccount != state.Account || w.Rmb.SellerOwnerRevision != state.OwnerRevision || w.Stage != "rmb_freezing" {
			return roleBlockerCommerce, nil
		}
	}
	return "", rows.Err()
}
