package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	dspb "proto/data_service"
	tradepb "proto/trade"
	"shared/accountdirectory"
	"shared/authoritysql"
	"shared/inboxshard"
	"shared/matchentry"

	"google.golang.org/protobuf/proto"
)

type roleAssetContractGuard struct {
	schema, guildSchema, mailSchema, matchSchema, inboxSchema string
	disabled                                                  bool
}

func (s *AccountDirectoryStore) ConfigureAssetContracts(schema string, peers ...string) error {
	if s == nil || s.db == nil {
		return accountdirectory.ErrUnavailable
	}
	// 装配失败必须撤掉旧配置，不能因重配错误继续使用不完整的挂起域。
	s.contracts = nil
	if !roleGuildSchema.MatchString(schema) || len(peers) != 4 {
		return errors.New("角色交易的trade/guild/mail/match/inbox权威schema未显式配置")
	}
	for _, peer := range peers {
		if !roleGuildSchema.MatchString(peer) {
			return errors.New("角色跨域权威schema非法或未配置")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := authoritysql.AssertCurrentSession(ctx, s.db, s.backend); err != nil {
		return err
	}
	guard := &roleAssetContractGuard{schema: schema, guildSchema: peers[0], mailSchema: peers[1], matchSchema: peers[2], inboxSchema: peers[3]}
	// 只读核表列；不创建/修改别的服务的表，也不从DSN或默认库推断权威域。
	for _, query := range []string{
		"SELECT commerce_id,kind,workflow_proto,seller_player_id,buyer_player_id,status FROM `" + guard.schema + "`.trade_commerce WHERE 1=0",
		"SELECT op_id,player_id,status FROM `" + guard.schema + "`.trade_asset_op WHERE 1=0",
		"SELECT op_id,player_id,status FROM `" + guard.guildSchema + "`.guild_asset_op WHERE 1=0",
		"SELECT battle_id,player_id FROM `" + guard.guildSchema + "`.guild_trial_reward_owed WHERE 1=0",
		"SELECT request_id,player_id,state FROM `" + guard.guildSchema + "`.guild_trial_launch_role WHERE 1=0",
		"SELECT request_id,state FROM `" + guard.guildSchema + "`.guild_trial_launch WHERE 1=0",
		"SELECT player_id,lobby_id FROM `" + guard.guildSchema + "`.guild_trial_lobby_slot WHERE 1=0",
		"SELECT lobby_id,state FROM `" + guard.guildSchema + "`.guild_trial_lobby WHERE 1=0",
		"SELECT op_id,player_id,status FROM `" + guard.mailSchema + "`.mail_asset_op WHERE 1=0",
		"SELECT player_id,state,ticket,request_id,formation_id,mode,config_id,team_size,enqueued_ms,not_before_ms,zone_id,rating_centi,team_id,network_profile,expires_ms,selected_region,preferred_region FROM `" + guard.matchSchema + "`.match_authority_ticket WHERE 1=0",
		"SELECT challenge_id,challenger_player_id,target_player_id,state,expires_ms FROM `" + guard.matchSchema + "`.match_challenge_invitation WHERE 1=0",
		"SELECT player_id,team_id,epoch FROM `" + guard.matchSchema + "`.team_membership WHERE 1=0",
		"SELECT invitee_id,team_id,expires_ms FROM `" + guard.matchSchema + "`.team_invite_index WHERE 1=0",
		"SELECT applicant_id,team_id,expires_ms FROM `" + guard.matchSchema + "`.team_application_index WHERE 1=0",
		"SELECT actor_id,request_high,request_low,player_id,state FROM `" + guard.matchSchema + "`.team_launch_role WHERE 1=0",
		"SELECT actor_id,request_high,request_low,state FROM `" + guard.matchSchema + "`.team_launch WHERE 1=0",
		"SELECT player_id,state,operation_id,owner_account,owner_revision FROM `" + guard.matchSchema + "`.match_spectator WHERE 1=0",
		"SELECT shard,player_id,state,owner_account,owner_revision FROM `" + guard.inboxSchema + "`.player_inbox_item WHERE 1=0",
	} {
		rows, err := s.db.QueryContext(ctx, query)
		if err != nil {
			return fmt.Errorf("角色跨域挂起守卫表列不可用: %w", err)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
	}
	s.contracts = guard
	return nil
}
func (s *AccountRoleTradeStore) ConfigureAssetContracts(schema string, peers ...string) error {
	if s == nil || s.directory == nil {
		return accountdirectory.ErrUnavailable
	}
	s.contracts = nil
	if err := s.directory.ConfigureAssetContracts(schema, peers...); err != nil {
		return err
	}
	s.contracts = s.directory.contracts
	return nil
}
func (s *AccountDirectoryStore) requireNoAssetContracts(ctx context.Context, tx *sql.Tx, player uint64) error {
	return s.contracts.verify(ctx, tx, &dspb.BeginAccountRoleTradeRequest{PlayerId: player})
}

// 与交易新合同/新参与者的account_role首锁互斥；在途资产属于原账号，不能随角色过户给新主。
// 撮合/竞价子单由未终局父合同在同事务派生，故所有资产合同类型均检查，不只人民币寄售。
func (s *AccountRoleTradeStore) verifyNoAssetContracts(ctx context.Context, tx *sql.Tx, in *dspb.BeginAccountRoleTradeRequest) error {
	return s.contracts.verify(ctx, tx, in)
}
func (s *roleAssetContractGuard) verify(ctx context.Context, tx *sql.Tx, in *dspb.BeginAccountRoleTradeRequest) error {
	if s == nil {
		return accountdirectory.ErrUnavailable
	}
	if s.disabled {
		return nil
	} // 只由隔离状态机测试显式关闭；生产装配没有关闭入口。
	if !roleGuildSchema.MatchString(s.schema) || !roleGuildSchema.MatchString(s.guildSchema) || !roleGuildSchema.MatchString(s.mailSchema) || !roleGuildSchema.MatchString(s.matchSchema) || !roleGuildSchema.MatchString(s.inboxSchema) {
		return accountdirectory.ErrUnavailable
	}
	// 与入箱、消费和作废共用原account_role首锁。未知/未终局条目属于原主，不能在维护或交易中换主。
	var pendingInbox uint64
	err := tx.QueryRowContext(ctx, "SELECT player_id FROM `"+s.inboxSchema+"`.player_inbox_item WHERE shard=? AND player_id=? AND (state IS NULL OR state NOT IN (2,3,4,5)) LIMIT 1 FOR UPDATE", inboxshard.RowShard(in.PlayerId), in.PlayerId).Scan(&pendingInbox)
	if err == nil {
		return accountdirectory.ErrConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	rows, err := tx.QueryContext(ctx, "SELECT commerce_id,kind,status,workflow_proto FROM `"+s.schema+"`.trade_commerce WHERE (seller_player_id=? OR buyer_player_id=?) AND (status NOT IN (7,11) OR status IS NULL) ORDER BY commerce_id LIMIT 257 FOR UPDATE", in.PlayerId, in.PlayerId)
	if err != nil {
		return err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		count++
		if count > 256 {
			return accountdirectory.ErrUnavailable
		} // 不截断全集后宣称无挂起合同。
		var id uint64
		var kind, status uint32
		var raw []byte
		if err := rows.Scan(&id, &kind, &status, &raw); err != nil {
			return err
		}
		w := &tradepb.TradeCommerceWorkflow{}
		if len(raw) > 1<<20 || proto.Unmarshal(raw, w) != nil {
			return accountdirectory.ErrConflict
		}
		// 官方退款合同已确定零新增资产腿；商城仍完成原款退款，不再阻止角色后续出售。
		if kind == 1 && status == 8 && w.Stage == "official_refund" {
			continue
		}
		if id != in.ListingId || kind != 6 || status != 3 || w.Rmb == nil || w.Rmb.CharacterPlayerId != in.PlayerId || w.Rmb.SellerAccount != in.SellerAccount || w.Rmb.SellerOwnerRevision != in.ExpectedOwnerRevision || w.Stage != "rmb_freezing" {
			return accountdirectory.ErrConflict
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, query := range []string{
		// trade/guild的6=ABANDONED均已证明scene未见过该seq；完整终态以外（含0/manual/未知）全部阻塞。
		"SELECT op_id FROM `" + s.schema + "`.trade_asset_op WHERE player_id=? AND (status NOT IN (2,3,4,6) OR status IS NULL) LIMIT 1 FOR UPDATE",
		"SELECT op_id FROM `" + s.guildSchema + "`.guild_asset_op WHERE player_id=? AND (status NOT IN (2,3,4,6) OR status IS NULL) LIMIT 1 FOR UPDATE",
		"SELECT battle_id FROM `" + s.guildSchema + "`.guild_trial_reward_owed WHERE player_id=? LIMIT 1 FOR UPDATE",
		"SELECT r.request_id FROM `" + s.guildSchema + "`.guild_trial_launch_role r LEFT JOIN `" + s.guildSchema + "`.guild_trial_launch l ON l.request_id=r.request_id WHERE r.player_id=? AND (r.state IS NULL OR r.state<>2 OR l.request_id IS NULL OR l.state IS NULL OR l.state<>3) LIMIT 1 FOR UPDATE",
		"SELECT s.player_id FROM `" + s.guildSchema + "`.guild_trial_lobby_slot s LEFT JOIN `" + s.guildSchema + "`.guild_trial_lobby l ON l.lobby_id=s.lobby_id WHERE s.player_id=? AND (s.lobby_id IS NULL OR (s.lobby_id<>0 AND (l.lobby_id IS NULL OR l.state IS NULL OR l.state<>4))) LIMIT 1 FOR UPDATE",
		"SELECT op_id FROM `" + s.mailSchema + "`.mail_asset_op WHERE player_id=? AND (status NOT IN (2,3,5) OR status IS NULL) LIMIT 1 FOR UPDATE",
		// Cancel先到会留下state0空票占位；身份字段可已登记，但没有原票/请求/成局就没有在途工作。
		// 只有精确空占位和state4安全，过期但未被Match终结的票也不能在本域擅自放行。
		"SELECT player_id FROM `" + s.matchSchema + "`.match_authority_ticket WHERE player_id=? AND " + matchentry.TicketOccupiedSQL + " LIMIT 1 FOR UPDATE",
		// 成员与同意原件属于原账号，角色过户或维护导入前必须先解除，不能由新主补权。
		"SELECT player_id FROM `" + s.matchSchema + "`.team_membership WHERE player_id=? AND (team_id IS NULL OR team_id<>0) LIMIT 1 FOR UPDATE",
		"SELECT team_id FROM `" + s.matchSchema + "`.team_invite_index WHERE invitee_id=? AND (expires_ms IS NULL OR expires_ms>" + dbNowMsSQL + ") LIMIT 1 FOR UPDATE",
		"SELECT team_id FROM `" + s.matchSchema + "`.team_application_index WHERE applicant_id=? AND (expires_ms IS NULL OR expires_ms>" + dbNowMsSQL + ") LIMIT 1 FOR UPDATE",
		"SELECT r.player_id FROM `" + s.matchSchema + "`.team_launch_role r LEFT JOIN `" + s.matchSchema + "`.team_launch l ON l.actor_id=r.actor_id AND l.request_high=r.request_high AND l.request_low=r.request_low WHERE r.player_id=? AND (r.state IS NULL OR r.state<>5 OR l.actor_id IS NULL OR l.state IS NULL OR l.state<>5) LIMIT 1 FOR UPDATE",
		"SELECT player_id FROM `" + s.matchSchema + "`.match_spectator WHERE player_id=? AND (state IS NULL OR state<>5) LIMIT 1 FOR UPDATE",
	} {
		var pending uint64
		err = tx.QueryRowContext(ctx, query, in.PlayerId).Scan(&pending)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		return accountdirectory.ErrConflict
	}
	// 邀请接受在同P事务内交接到真实票/formation；过期PENDING和精确2/3不再有独立邀请待办。
	var challenge uint64
	err = tx.QueryRowContext(ctx, "SELECT challenge_id FROM `"+s.matchSchema+"`.match_challenge_invitation WHERE (challenger_player_id=? OR target_player_id=?) AND (state IS NULL OR state NOT IN (1,2,3,5) OR (state=1 AND (expires_ms IS NULL OR expires_ms>"+dbNowMsSQL+"))) LIMIT 1 FOR UPDATE", in.PlayerId, in.PlayerId).Scan(&challenge)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return accountdirectory.ErrConflict
}
