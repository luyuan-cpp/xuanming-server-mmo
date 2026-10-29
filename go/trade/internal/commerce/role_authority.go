package commerce

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"sort"

	base "proto/common/base"
	dspb "proto/data_service"
	tradepb "proto/trade"
	"shared/accountdirectory"
	"shared/roleidentity"
	"trade/internal/data"
)

// RoleLocker 的事务来自同TiDB强会话池。所有新增参与者先锁角色，再锁合同；不在事务内调用RPC。
type RoleLocker interface {
	Lock(context.Context, *sql.Tx, uint64, ...uint64) error
	Ownership(context.Context, *sql.Tx, uint64) (*dspb.AccountRoleOwnership, error)
}
type RoleAuthority struct{ Schema string }

var authoritySchema = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

func NewRoleAuthority(schema string) (*RoleAuthority, error) {
	if !authoritySchema.MatchString(schema) {
		return nil, errors.New("交易角色目录schema未显式配置")
	}
	return &RoleAuthority{Schema: schema}, nil
}
func (a *RoleAuthority) Ownership(ctx context.Context, tx *sql.Tx, player uint64) (*dspb.AccountRoleOwnership, error) {
	if a == nil || !authoritySchema.MatchString(a.Schema) || player == 0 {
		return nil, errors.New("交易角色权威未装配")
	}
	out := &dspb.AccountRoleOwnership{Role: &base.AccountSimplePlayer{PlayerId: player}}
	if err := tx.QueryRowContext(ctx, "SELECT account,owner_revision,trade_listing_id FROM `"+a.Schema+"`.account_role WHERE player_id=? FOR UPDATE", player).Scan(&out.Account, &out.OwnerRevision, &out.TradeListingId); err != nil {
		return nil, err
	}
	if !accountdirectory.ValidAccount(out.Account) || out.OwnerRevision == 0 {
		return nil, ErrConflict
	}
	return out, nil
}
func (a *RoleAuthority) Lock(ctx context.Context, tx *sql.Tx, allowListing uint64, players ...uint64) error {
	ids := append([]uint64(nil), players...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var previous uint64
	for _, id := range ids {
		if id == 0 || id == previous {
			continue
		}
		previous = id
		role, err := a.Ownership(ctx, tx, id)
		if err != nil {
			return err
		}
		if role.TradeListingId != 0 && role.TradeListingId != allowListing {
			return ErrConflict
		}
	}
	return nil
}
func (e *Engine) lockRoles(ctx context.Context, tx *sql.Tx, allowListing uint64, players ...uint64) error {
	if e.Roles == nil {
		return errors.New("交易角色提交互锁未配置")
	}
	// 客户端actor可能也是交易参与者；并入同一升序首锁，不能先锁参与者再回头锁actor。
	original, present := roleidentity.Original(ctx)
	if present {
		players = append(players, original.Role.PlayerId)
	}
	// 目录给出的 ErrConflict(角色被另一条挂单冻结、目录行不合格)与换主一样是长期状态,标成 role_conflict
	// 供后台推进分钟级复查(2026-10-22);客户端拿到的 tip 码不变。
	if err := e.Roles.Lock(ctx, tx, allowListing, players...); err != nil {
		return asRoleConflict(err)
	}
	if present {
		current, err := e.Roles.Ownership(ctx, tx, original.Role.PlayerId)
		if err != nil {
			return asRoleConflict(err)
		}
		if current.Account != original.Account || current.OwnerRevision != original.OwnerRevision {
			return roleConflict()
		}
	}
	return nil
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
		if owned.Account != party.identity.Account || owned.OwnerRevision != party.identity.OwnerRevision {
			// 原主已换(owner_revision 只增不减,不会再对上):合同与对方托管保留,等人工,不按存储抖动每 10 秒重试。
			return roleConflict()
		}
	}
	return nil
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
	return e.Repo.Tx(ctx, func(tx *sql.Tx) error { return e.lockRoles(ctx, tx, allow) })
}
