package inbox

import (
	"context"
	"fmt"

	"player_inbox/internal/constants"

	componentpb "proto/common/component"
	inboxpb "proto/player_inbox"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

// 单玩家物品获取封禁的运维查询(设计 §2.2;PlayerInboxAdmin.GetItemGainBlocks)。
//
// 回答两件事:已落盘的本人封禁表(经 data_service.GetPlayerItemGainBlock 读玩家存档)和 GM 域里还没生效的
// 物品封禁 / 解封单(本服务的收件箱库)。两份来源不在一个事务里,所以**先读收件箱、后读存档**:
// scene 只在封禁结果随存盘提交之后才确认条目(落盘之后才 Ack,§7.7),一条单在读收件箱那一刻要么还是 PENDING
// (出现在 pending 里),要么已经确认 —— 那它的效果在更早时就已提交,随后读存档一定看得到。
// 反过来先读存档的话,一条单可以在两次读之间「落盘并确认」,两边都看不到它,运维会读成「没封上」。
// 两边同时出现(已执行、等确认)无害:封禁与解封天然幂等。

// ItemGainBlockReader 读一名玩家已落盘的单玩家物品获取封禁表(player_database.item_gain_block)。
// found=false = 没有写权行(从未建角,或角色还没建完);found=true 时 block 非 nil(从未被封过时是空消息)。
type ItemGainBlockReader interface {
	ReadItemGainBlock(ctx context.Context, playerID uint64) (block *componentpb.PlayerItemGainBlockComp, found bool, err error)
}

// itemGainSanctionTypes 是这个查询列出的两种条目类型。所在的域取自注册表(类型的 domain 由 proto option 声明),不在这里另写一份。
var itemGainSanctionTypes = []inboxpb.PlayerInboxMessageType{
	inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_GM_BLOCK_ITEM_GAIN,
	inboxpb.PlayerInboxMessageType_PLAYER_INBOX_MSG_GM_UNBLOCK_ITEM_GAIN,
}

// ItemGainBlocks 查一名玩家的单玩家物品获取封禁(只读,不写审计)。
// player_id 为 0 回 kInvalidParameter;读收件箱库或已落盘存档失败返回 error(适配层折成 kPlayerInboxStoreUnavailable)。
// 不含全服封禁:那是 scene 节点上的线程级集合,不在玩家存档里。
func (s *Store) ItemGainBlocks(ctx context.Context, playerID uint64, blocks ItemGainBlockReader) (*inboxpb.GetPlayerItemGainBlocksResponse, error) {
	resp := &inboxpb.GetPlayerItemGainBlocksResponse{PlayerId: playerID}
	if playerID == 0 {
		resp.ErrorMessage = tipOf(constants.ErrInvalidParameter)
		return resp, nil
	}
	if blocks == nil {
		return nil, fmt.Errorf("inbox: ItemGainBlocks has no item gain block reader")
	}
	// 先收件箱、后存档,顺序见文件头。
	rows, err := s.pendingItemGainSanctions(ctx, playerID)
	if err != nil {
		return nil, err
	}
	readCtx, cancel := context.WithTimeout(ctx, constants.LedgerReadTimeout)
	block, found, err := blocks.ReadItemGainBlock(readCtx, playerID)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("inbox: read persisted item gain blocks player=%d: %w", playerID, err)
	}
	resp.Found = found
	resp.BlockedItemConfigIds = block.GetItemConfigIds()
	resp.Pending = itemGainPendingViews(ctx, playerID, rows)
	return resp, nil
}

// pendingItemGainSanctions 读一名玩家仍 PENDING 的物品封禁 / 解封行,seq 升序(scene 的执行顺序)。
// 一个域的 PENDING 行数受 max_pending 约束(入箱在流行锁下判满箱);读到超过它说明 open_count 不变量已破,
// 回错误,不拿一份可能残缺的列表当完整答案。
func (s *Store) pendingItemGainSanctions(ctx context.Context, playerID uint64) ([]*inboxpb.PlayerInboxItemRow, error) {
	domain, err := s.itemGainSanctionDomain()
	if err != nil {
		return nil, err
	}
	spec, ok := s.registry.Domain(domain)
	if !ok {
		return nil, fmt.Errorf("inbox: domain %s of item gain sanctions is not registered", domain)
	}
	limit := int(spec.GetMaxPending())
	args := []any{RowShard(playerID), playerID, uint32(domain), uint32(inboxpb.PlayerInboxItemState_PLAYER_INBOX_ITEM_PENDING)}
	for _, t := range itemGainSanctionTypes {
		args = append(args, uint32(t))
	}
	args = append(args, limit+1)
	rows, err := queryItems(ctx, s.db, "SELECT "+itemColumns+" FROM "+itemTable+
		" WHERE `shard` = ? AND `player_id` = ? AND `domain` = ? AND `state` = ? AND `msg_type` IN ("+
		placeholders(len(itemGainSanctionTypes))+") ORDER BY `seq` ASC LIMIT ?", args...)
	if err != nil {
		return nil, fmt.Errorf("inbox: read pending item gain sanctions player=%d: %w", playerID, err)
	}
	if len(rows) > limit {
		return nil, fmt.Errorf("inbox: player=%d has more than max_pending=%d pending item gain sanctions in domain %s; "+
			"the open_count invariant is broken, pending list is unknown", playerID, limit, domain)
	}
	return rows, nil
}

// itemGainSanctionDomain 返回两种物品获取处置类型所在的域。两者必须同域(封禁与解封按同一条流的 seq 定先后,§2.2),
// 注册表不认识或不同域都是注册表损坏,回错误。
func (s *Store) itemGainSanctionDomain() (inboxpb.PlayerInboxDomain, error) {
	var domain inboxpb.PlayerInboxDomain
	for i, t := range itemGainSanctionTypes {
		entry, ok := s.registry.Type(t)
		if !ok {
			return 0, fmt.Errorf("inbox: message type %s is not registered", TypeLabel(t))
		}
		d := entry.Spec.GetDomain()
		if i > 0 && d != domain {
			return 0, fmt.Errorf("inbox: item gain sanction types live in different domains (%s vs %s)", domain, d)
		}
		domain = d
	}
	return domain, nil
}

// itemGainPendingViews 把 PENDING 行折成运维视图,载荷解出 item_config_id。
// 载荷解不开或配置号为 0(入箱时校验过,只可能是数据损坏)时 item_config_id 记 0、打 ERROR,这一条照样列出:
// scene 会判它 REJECTED,不会生效;不列出来就是把损坏藏起来。
func itemGainPendingViews(ctx context.Context, playerID uint64, rows []*inboxpb.PlayerInboxItemRow) []*inboxpb.PlayerItemGainSanctionPending {
	out := make([]*inboxpb.PlayerItemGainSanctionPending, 0, len(rows))
	for _, row := range rows {
		var payload inboxpb.GmItemGainPayload
		if err := proto.Unmarshal(row.GetPayload(), &payload); err != nil || payload.GetItemConfigId() == 0 {
			logx.WithContext(ctx).Errorf("[player_inbox] GetItemGainBlocks undecodable payload player=%d domain=%d seq=%d type=%s: %v",
				playerID, row.GetDomain(), row.GetSeq(), TypeLabel(inboxpb.PlayerInboxMessageType(row.GetMsgType())), err)
			payload.Reset()
		}
		out = append(out, &inboxpb.PlayerItemGainSanctionPending{
			Seq:           row.GetSeq(),
			MsgType:       inboxpb.PlayerInboxMessageType(row.GetMsgType()),
			ItemConfigId:  payload.GetItemConfigId(),
			RequestId:     row.GetRequestId(),
			Directive:     inboxpb.PlayerInboxDirective(row.GetDirective()),
			DeliveryCount: row.GetDeliveryCount(),
			CreatedMs:     row.GetCreatedMs(),
			OperatorName:  row.GetOperatorName(),
			Note:          row.GetNote(),
		})
	}
	return out
}
