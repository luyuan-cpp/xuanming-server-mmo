package logic

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"guild/internal/constants"
	"guild/internal/data"
	assetpb "proto/common/asset"
	rollbackpb "proto/common/rollback"
	pb "proto/guild"
	scenepb "proto/scene"
	"shared/assetop"
	tablepb "shared/generated/pb/table"
)

const (
	currencyTypeRealDiamond uint32 = 1 // CURRENCY_TYPE_DIAMOND=1，绝不扣帮会资金或绑定灵石
	assetKindEmblemUnlock          = "emblem_unlock"
)

// UnlockGuildEmblem 帮主自愿以个人真实灵石买断本帮永久定制徽记（1..3=180 灵石，4..6=300 灵石，零数值加成）。
func (l *GuildLogic) UnlockGuildEmblem(ctx context.Context, req *pb.UnlockGuildEmblemRequest) (*pb.UnlockGuildEmblemResponse, error) {
	eco := l.economy
	if eco == nil {
		return nil, errEconomyNotEnabled()
	}
	if req.GetClientRequestId() == 0 {
		return &pb.UnlockGuildEmblemResponse{
			ErrorMessage: tipErr(uint32(tablepb.CommonError_kInvalidParameter), "client_request_id required"),
		}, nil
	}
	resp, _, err := withClientRequest(req.GetClientRequestId(),
		func() (*pb.UnlockGuildEmblemResponse, string, bool, error) { return l.replayEmblemUnlock(ctx, eco, req) },
		func() (*pb.UnlockGuildEmblemResponse, string, error) { return l.unlockEmblemOnce(ctx, eco, req) },
		func(r *pb.UnlockGuildEmblemResponse) bool { return r.GetOpId() != 0 })
	return resp, err
}

func (l *GuildLogic) replayEmblemUnlock(ctx context.Context, eco *EconomyDeps, req *pb.UnlockGuildEmblemRequest) (*pb.UnlockGuildEmblemResponse, string, bool, error) {
	playerID, hit, found, err := l.clientRequestHit(ctx, eco, req.GetClientRequestId())
	if !found {
		return nil, "", false, err
	}
	if tip := clientRequestMismatch(hit, pb.GuildAssetOpKind_GUILD_ASSET_OP_KIND_EMBLEM_UNLOCK, req.GetEmblemId(), 1, 0); tip != nil {
		return &pb.UnlockGuildEmblemResponse{ErrorMessage: tip}, resultRequestConflict, true, nil
	}
	order, reason := orderViewOf(hit.Op.Status, hit.Op.LastReason, hit.Op.ReasonTipID)
	price, _ := constants.GuildEmblemPriceDiamond(hit.Op.RefID)
	return &pb.UnlockGuildEmblemResponse{
		ErrorMessage: donationRejectTip(order, reason),
		Replayed:     true,
		OpId:         hit.Op.OpID,
		Status:       order,
		ReasonTipId:  reason,
		EmblemId:     hit.Op.RefID,
		CostDiamond:  price,
		Guild:        l.freshGuildInfo(ctx, hit.Op.GuildID, playerID),
	}, resultReplayed, true, nil
}

func (l *GuildLogic) unlockEmblemOnce(ctx context.Context, eco *EconomyDeps, req *pb.UnlockGuildEmblemRequest) (*pb.UnlockGuildEmblemResponse, string, error) {
	start := eco.Now()
	playerID, g, tip, err := l.economyCaller(ctx)
	if err != nil {
		return nil, resultOfFault(err), err
	}
	if tip != nil {
		return &pb.UnlockGuildEmblemResponse{ErrorMessage: tip}, resultNotMember, nil
	}
	if eco.Loop == nil {
		return &pb.UnlockGuildEmblemResponse{ErrorMessage: assetChannelDisabledTip()}, resultDisabled, nil
	}
	price, ok := constants.GuildEmblemPriceDiamond(req.GetEmblemId())
	if !ok {
		return &pb.UnlockGuildEmblemResponse{
			ErrorMessage: tipErr(constants.ErrEmblemInvalid, "invalid guild emblem"),
		}, resultNotFound, nil
	}
	if req.GetExpectedPriceDiamond() != price {
		return &pb.UnlockGuildEmblemResponse{
			ErrorMessage: tipErr(constants.ErrEmblemPriceChanged, "guild emblem price mismatch"),
			EmblemId:     req.GetEmblemId(),
			CostDiamond:  price,
		}, resultOtherReject, nil
	}
	if g.LeaderID != playerID {
		return &pb.UnlockGuildEmblemResponse{
			ErrorMessage: tipErr(constants.ErrNotLeader, "only leader can unlock guild emblem"),
		}, resultRank, nil
	}
	if (g.UnlockedEmblemBits & (uint64(1) << (req.GetEmblemId() - 1))) != 0 {
		return &pb.UnlockGuildEmblemResponse{
			ErrorMessage: tipErr(constants.ErrEmblemAlreadyUnlocked, "guild emblem already unlocked"),
			EmblemId:     req.GetEmblemId(),
			Guild:        l.guildInfoFor(ctx, g, playerID),
		}, resultOtherReject, nil
	}
	deadlineAfterMs, ok := assetOpDeadlineMs()
	if !ok {
		return nil, resultError, status.Errorf(codes.Internal, "GuildRule row %d missing", guildRuleRowID)
	}
	bundle := &assetpb.AssetBundle{Currencies: []*assetpb.CurrencyAmount{{
		CurrencyType: currencyTypeRealDiamond,
		Amount:       price,
	}}}
	payload, err := proto.Marshal(bundle)
	if err != nil {
		return nil, resultError, fmt.Errorf("marshal emblem unlock bundle: %w", err)
	}
	token, err := newAssetOpToken()
	if err != nil {
		return nil, resultError, err
	}
	opID, tip := l.mintAssetOpID(ctx, eco, playerID)
	if tip != nil {
		return &pb.UnlockGuildEmblemResponse{ErrorMessage: tip}, resultIDUnavailable, nil
	}
	startMs := uint64(start.UnixMilli())
	deadlineMs := startMs + deadlineAfterMs
	reserved, err := eco.Repo.ReserveGuildEmblemUnlock(ctx, data.EmblemUnlockReserve{
		OpID:            opID,
		PlayerID:        playerID,
		GuildID:         g.GuildID,
		EmblemID:        req.GetEmblemId(),
		DeadlineMs:      deadlineMs,
		LeaseUntilMs:    startMs + durationMs(eco.Lease),
		LeaseToken:      token,
		NowMs:           startMs,
		Payload:         payload,
		ClientRequestID: req.GetClientRequestId(),
	})
	if errors.Is(err, data.ErrClientRequestReplayed) {
		return nil, resultReplayed, errClientRequestRaced
	}
	if errors.Is(err, data.ErrGuildEmblemAlreadyUnlocked) {
		return &pb.UnlockGuildEmblemResponse{
			ErrorMessage: tipErr(constants.ErrEmblemAlreadyUnlocked, "guild emblem already unlocked"),
			EmblemId:     req.GetEmblemId(),
		}, resultOtherReject, nil
	}
	if errors.Is(err, data.ErrRankTooLow) {
		return &pb.UnlockGuildEmblemResponse{
			ErrorMessage: tipErr(constants.ErrNotLeader, "only leader can unlock guild emblem"),
		}, resultRank, nil
	}
	if err != nil {
		rejectTip, result, fault := l.economyTip(ctx, playerID, g.GuildID, err)
		if fault != nil {
			return nil, result, fault
		}
		return &pb.UnlockGuildEmblemResponse{ErrorMessage: rejectTip}, result, nil
	}
	l.deliverNow(ctx, eco, assetop.Op{
		OpID:          opID,
		PlayerID:      playerID,
		Stream:        assetpb.AssetOpStream_ASSET_OP_STREAM_GUILD_DEBIT,
		Seq:           reserved.Seq,
		StreamEpoch:   reserved.StreamEpoch,
		CorrelationID: opID,
		TxType:        uint32(rollbackpb.TransactionType_TX_GUILD_EMBLEM_UNLOCK),
		Bundle:        bundle,
		DeadlineMs:    deadlineMs,
		LeaseToken:    token,
	}, assetKindEmblemUnlock)

	order, reason := l.settledView(ctx, eco, opID)
	fresh := l.freshGuildInfo(ctx, g.GuildID, playerID)
	if order == pb.GuildAssetOrderStatus_GUILD_ASSET_ORDER_STATUS_APPLIED {
		l.notify(ctx, pb.GuildChangeKind_GUILD_CHANGE_KIND_EMBLEM_CHANGED, g.GuildID, playerID, 0,
			membersExcept(g, playerID))
		l.bumpSync(membersPlus(g), scenepb.SyncDomain_SYNC_DOMAIN_GUILD)
	}
	return &pb.UnlockGuildEmblemResponse{
		ErrorMessage: donationRejectTip(order, reason),
		OpId:         opID,
		Status:       order,
		ReasonTipId:  reason,
		EmblemId:     req.GetEmblemId(),
		CostDiamond:  price,
		Guild:        fresh,
	}, resultApplied, nil
}

// EquipGuildEmblem 帮主免费在默认帮徽（0）与本帮已解锁的定制帮徽（1..6）之间切换。
func (l *GuildLogic) EquipGuildEmblem(ctx context.Context, req *pb.EquipGuildEmblemRequest) (*pb.EquipGuildEmblemResponse, error) {
	who := callerOf(ctx, 0)
	if err := requireClientSession(who); err != nil {
		return nil, err
	}
	if req.GetEmblemId() > constants.MaxGuildEmblemID {
		return &pb.EquipGuildEmblemResponse{
			ErrorMessage: tipErr(constants.ErrEmblemInvalid, "invalid guild emblem"),
		}, nil
	}
	g, err := l.repo.ResolvePlayerGuild(ctx, who.playerID)
	if err != nil {
		return nil, err
	}
	if g == nil || g.GuildID == 0 {
		return &pb.EquipGuildEmblemResponse{
			ErrorMessage: tipErr(constants.ErrNotInGuild, "not in guild"),
		}, nil
	}
	guildID := g.GuildID
	snapshot, err := l.repo.EquipGuildEmblem(ctx, guildID, who.playerID, req.GetEmblemId())
	switch {
	case err == nil:
	case errors.Is(err, data.ErrGuildEmblemNotUnlocked):
		return &pb.EquipGuildEmblemResponse{
			ErrorMessage: tipErr(constants.ErrEmblemNotUnlocked, "guild emblem not unlocked"),
		}, nil
	case errors.Is(err, data.ErrRankTooLow):
		return &pb.EquipGuildEmblemResponse{
			ErrorMessage: tipErr(constants.ErrNotLeader, "only leader can equip guild emblem"),
		}, nil
	default:
		if tip, mappedErr := l.mapWriteErr(ctx, who.playerID, guildID, guildID, err); mappedErr != nil || tip != nil {
			return &pb.EquipGuildEmblemResponse{ErrorMessage: tip}, mappedErr
		}
	}
	l.notify(ctx, pb.GuildChangeKind_GUILD_CHANGE_KIND_EMBLEM_CHANGED, guildID, who.playerID, 0,
		membersExcept(snapshot, who.playerID))
	l.bumpSync(membersPlus(snapshot), scenepb.SyncDomain_SYNC_DOMAIN_GUILD)
	return &pb.EquipGuildEmblemResponse{
		Guild: l.guildInfoFor(ctx, snapshot, who.playerID),
	}, nil
}
