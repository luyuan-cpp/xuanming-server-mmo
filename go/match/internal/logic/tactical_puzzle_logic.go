package logic

import (
	"context"
	"encoding/binary"
	"errors"

	"match/internal/authority"
	"match/internal/constants"
	"match/internal/pkg/ctxkeys"
	"match/internal/puzzle"
	"match/internal/svc"

	battlepb "proto/battle"
	cgpb "proto/content_guard"
	matchpb "proto/match"
	"shared/contentguard"

	"github.com/google/uuid"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

const directIntentKindTacticalPuzzle uint32 = 7

// TacticalPuzzleLogic 实现万象残局坊 V1 的 7 条客户端 RPC（575..581）。
// 所有接口仅认 gate 注入的权威会话 player_id；出题文本强走 ContentGuard（fail-closed）；
// 开战走统一 direct formation + gather 管线，确定性种子 + 免费公平预设 + 作者验题录播。
type TacticalPuzzleLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewTacticalPuzzleLogic(ctx context.Context, svcCtx *svc.ServiceContext) *TacticalPuzzleLogic {
	return &TacticalPuzzleLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func puzzleErrorCode(err error) uint32 {
	switch {
	case errors.Is(err, authority.ErrPuzzleNotFound):
		return constants.ErrPuzzleNotFound
	case errors.Is(err, authority.ErrPuzzleNotOwner):
		return constants.ErrPuzzleNotOwner
	case errors.Is(err, authority.ErrPuzzleLimitReached):
		return constants.ErrPuzzleLimitReached
	case errors.Is(err, authority.ErrPuzzleSpecInvalid), errors.Is(err, puzzle.ErrSpecInvalid):
		return constants.ErrPuzzleSpecInvalid
	case errors.Is(err, puzzle.ErrTextInvalid):
		return constants.ErrPuzzleTextRejected
	case errors.Is(err, authority.ErrPuzzleNotPublished):
		return constants.ErrPuzzleNotPublished
	case errors.Is(err, authority.ErrPuzzleCannotLikeOwn):
		return constants.ErrPuzzleCannotLikeOwn
	case errors.Is(err, authority.ErrPuzzleClearRequiredToLike):
		return constants.ErrPuzzleClearRequiredToLike
	case errors.Is(err, authority.ErrPuzzleSolutionReplayNotFound):
		return constants.ErrPuzzleSolutionReplayNotFound
	default:
		return constants.ErrInternal
	}
}

func (l *TacticalPuzzleLogic) sessionPlayerID() uint64 {
	detail, ok := ctxkeys.GetSessionDetails(l.ctx)
	if !ok || detail == nil {
		return 0
	}
	return detail.GetPlayerId()
}

func (l *TacticalPuzzleLogic) checkTextGuard(playerID uint64, text string) uint32 {
	if text == "" || l.svcCtx == nil || l.svcCtx.ContentGuard == nil {
		return 0
	}
	res := l.svcCtx.ContentGuard.Check(l.ctx, cgpb.TextScene_TEXT_SCENE_GUILD_NAME, playerID, text)
	if res.Verdict == contentguard.Unavailable {
		return constants.ErrInternal
	}
	if res.Verdict != contentguard.Pass {
		return constants.ErrPuzzleTextRejected
	}
	return 0
}

func (l *TacticalPuzzleLogic) ListTacticalPuzzles(in *matchpb.ListTacticalPuzzlesRequest) (*matchpb.ListTacticalPuzzlesResponse, error) {
	playerID := l.sessionPlayerID()
	if playerID == 0 || l.svcCtx == nil || l.svcCtx.Authority == nil {
		return &matchpb.ListTacticalPuzzlesResponse{ErrorMessage: tipErr(constants.ErrInternal, "残局坊暂不可用")}, nil
	}
	items, total, profile, err := l.svcCtx.Authority.ListTacticalPuzzles(
		l.ctx, playerID, in.GetTab(), in.GetTagFilterMask(), in.GetOffset(), in.GetLimit(),
	)
	if err != nil {
		l.Errorf("[tactical_puzzle] ListTacticalPuzzles failed player=%d tab=%v: %v", playerID, in.GetTab(), err)
		return &matchpb.ListTacticalPuzzlesResponse{ErrorMessage: tipErr(constants.ErrInternal, "读取残局列表失败")}, nil
	}
	return &matchpb.ListTacticalPuzzlesResponse{
		Puzzles:    items,
		TotalCount: total,
		Profile:    profile,
	}, nil
}

func (l *TacticalPuzzleLogic) GetTacticalPuzzle(in *matchpb.GetTacticalPuzzleRequest) (*matchpb.GetTacticalPuzzleResponse, error) {
	playerID := l.sessionPlayerID()
	if playerID == 0 || l.svcCtx == nil || l.svcCtx.Authority == nil {
		return &matchpb.GetTacticalPuzzleResponse{ErrorMessage: tipErr(constants.ErrInternal, "残局坊暂不可用")}, nil
	}
	view, err := l.svcCtx.Authority.GetTacticalPuzzle(l.ctx, playerID, in.GetPuzzleId())
	if err != nil {
		code := puzzleErrorCode(err)
		if code == constants.ErrInternal {
			l.Errorf("[tactical_puzzle] GetTacticalPuzzle failed player=%d puzzle=%d: %v", playerID, in.GetPuzzleId(), err)
		}
		return &matchpb.GetTacticalPuzzleResponse{ErrorMessage: tipErr(code, "残局不存在或暂不可查看")}, nil
	}
	return &matchpb.GetTacticalPuzzleResponse{Puzzle: view}, nil
}

func (l *TacticalPuzzleLogic) SaveTacticalPuzzleDraft(in *matchpb.SaveTacticalPuzzleDraftRequest) (*matchpb.SaveTacticalPuzzleDraftResponse, error) {
	playerID := l.sessionPlayerID()
	if playerID == 0 || l.svcCtx == nil || l.svcCtx.Authority == nil || l.svcCtx.BattleIDGen == nil {
		return &matchpb.SaveTacticalPuzzleDraftResponse{ErrorMessage: tipErr(constants.ErrInternal, "残局坊暂不可用")}, nil
	}
	title, hint, err := puzzle.NormalizeAndValidateText(in.GetTitle(), in.GetHintText())
	if err != nil {
		return &matchpb.SaveTacticalPuzzleDraftResponse{ErrorMessage: tipErr(constants.ErrPuzzleTextRejected, "标题需为 4～20 个合法字符，思路提示不超过 40 字")}, nil
	}
	if err = puzzle.ValidateTagMask(in.GetTagMask()); err != nil {
		return &matchpb.SaveTacticalPuzzleDraftResponse{ErrorMessage: tipErr(constants.ErrPuzzleSpecInvalid, "标签数量超出限制")}, nil
	}
	if err = puzzle.ValidateSpec(in.GetSpec()); err != nil {
		return &matchpb.SaveTacticalPuzzleDraftResponse{ErrorMessage: tipErr(constants.ErrPuzzleSpecInvalid, "残局配置超出允许范围")}, nil
	}
	// 内容安全审核在 SQL 事务外执行，fail-closed 拦截违规词或服务不可达。
	if code := l.checkTextGuard(playerID, title); code != 0 {
		return &matchpb.SaveTacticalPuzzleDraftResponse{ErrorMessage: tipErr(code, "标题未通过内容安全审核")}, nil
	}
	if code := l.checkTextGuard(playerID, hint); code != 0 {
		return &matchpb.SaveTacticalPuzzleDraftResponse{ErrorMessage: tipErr(code, "思路提示未通过内容安全审核")}, nil
	}
	authorName := challengePublicName(l.ctx, l.svcCtx, playerID)
	view, err := l.svcCtx.Authority.SaveTacticalPuzzleDraft(
		l.ctx, playerID, authorName, in.GetPuzzleId(), title, hint, in.GetTagMask(), in.GetSpec(), l.svcCtx.BattleIDGen.Generate,
	)
	if err != nil {
		code := puzzleErrorCode(err)
		if code == constants.ErrInternal {
			l.Errorf("[tactical_puzzle] SaveTacticalPuzzleDraft failed player=%d puzzle=%d: %v", playerID, in.GetPuzzleId(), err)
		}
		return &matchpb.SaveTacticalPuzzleDraftResponse{ErrorMessage: tipErr(code, "保存残局草稿失败")}, nil
	}
	return &matchpb.SaveTacticalPuzzleDraftResponse{Puzzle: view}, nil
}

func (l *TacticalPuzzleLogic) DeleteTacticalPuzzle(in *matchpb.DeleteTacticalPuzzleRequest) (*matchpb.DeleteTacticalPuzzleResponse, error) {
	playerID := l.sessionPlayerID()
	if playerID == 0 || l.svcCtx == nil || l.svcCtx.Authority == nil {
		return &matchpb.DeleteTacticalPuzzleResponse{ErrorMessage: tipErr(constants.ErrInternal, "残局坊暂不可用")}, nil
	}
	if err := l.svcCtx.Authority.DeleteTacticalPuzzle(l.ctx, playerID, in.GetPuzzleId()); err != nil {
		code := puzzleErrorCode(err)
		if code == constants.ErrInternal {
			l.Errorf("[tactical_puzzle] DeleteTacticalPuzzle failed player=%d puzzle=%d: %v", playerID, in.GetPuzzleId(), err)
		}
		return &matchpb.DeleteTacticalPuzzleResponse{ErrorMessage: tipErr(code, "删除残局失败")}, nil
	}
	return &matchpb.DeleteTacticalPuzzleResponse{}, nil
}

func (l *TacticalPuzzleLogic) StartTacticalPuzzleBattle(in *matchpb.StartTacticalPuzzleBattleRequest) (*matchpb.StartTacticalPuzzleBattleResponse, error) {
	playerID := l.sessionPlayerID()
	if playerID == 0 || l.svcCtx == nil || l.svcCtx.Authority == nil || l.svcCtx.BattleIDGen == nil {
		return &matchpb.StartTacticalPuzzleBattleResponse{ErrorMessage: tipErr(constants.ErrInternal, "残局开战暂不可用")}, nil
	}
	pctx, err := l.svcCtx.Authority.PrepareTacticalPuzzleLaunch(l.ctx, playerID, in.GetPuzzleId(), in.GetIsAuthorVerify())
	if err != nil {
		code := puzzleErrorCode(err)
		if code == constants.ErrInternal {
			l.Errorf("[tactical_puzzle] PrepareTacticalPuzzleLaunch failed player=%d puzzle=%d: %v", playerID, in.GetPuzzleId(), err)
		}
		return &matchpb.StartTacticalPuzzleBattleResponse{ErrorMessage: tipErr(code, "残局开战校验未通过")}, nil
	}

	reqUUID, err := uuid.Parse(in.GetRequestId())
	if err != nil {
		reqUUID = uuid.New()
	}
	key := &matchpb.MatchDirectIntentRecord{
		Kind:        directIntentKindTacticalPuzzle,
		OwnerId:     playerID,
		RequestHigh: binary.BigEndian.Uint64(reqUUID[:8]),
		RequestLow:  binary.BigEndian.Uint64(reqUUID[8:]),
	}
	payload, err := proto.MarshalOptions{Deterministic: true}.Marshal(pctx)
	if err != nil {
		return &matchpb.StartTacticalPuzzleBattleResponse{ErrorMessage: tipErr(constants.ErrInternal, "残局开战编码失败")}, nil
	}

	cat := l.svcCtx.Authority.RankedCatalog()
	rankedCtx := &battlepb.RankedMatchContext{
		SeasonId:          0,
		RuleVersion:       cat.RuleVersion(),
		TeamSize:          1,
		ConfigFingerprint: cat.Fingerprint(),
		Members: []*battlepb.RankedMemberLoadout{{
			PlayerId: playerID,
			Presets: &battlepb.RankedPresetSelection{
				CharacterPresetId: pctx.GetSpec().GetCharacterPresetId(),
				PetPresetId:       pctx.GetSpec().GetPetPresetId(),
			},
		}},
		UseFreePreset: true,
	}

	f, derr := reserveGlobalDirect(
		l.ctx, l.svcCtx, key, payload,
		matchpb.MatchMode_MATCH_MODE_PVE_SOLO,
		puzzle.WorkshopDungeonID,
		[]uint64{playerID},
		directOptions{
			rankedContext:         rankedCtx,
			tacticalPuzzleContext: pctx,
		},
	)
	if derr != nil {
		rej := joinRejection(derr, "残局开战暂不可用")
		return &matchpb.StartTacticalPuzzleBattleResponse{ErrorMessage: rej.GetErrorMessage()}, nil
	}
	if f != nil && f.State < 4 {
		launchGlobalFormation(l.svcCtx, f)
	}
	return &matchpb.StartTacticalPuzzleBattleResponse{
		BattleId:     f.GetBattleId(),
		SpecRevision: pctx.GetSpecRevision(),
	}, nil
}

func (l *TacticalPuzzleLogic) LikeTacticalPuzzle(in *matchpb.LikeTacticalPuzzleRequest) (*matchpb.LikeTacticalPuzzleResponse, error) {
	playerID := l.sessionPlayerID()
	if playerID == 0 || l.svcCtx == nil || l.svcCtx.Authority == nil {
		return &matchpb.LikeTacticalPuzzleResponse{ErrorMessage: tipErr(constants.ErrInternal, "残局坊暂不可用")}, nil
	}
	likes, liked, err := l.svcCtx.Authority.LikeTacticalPuzzle(l.ctx, playerID, in.GetPuzzleId())
	if err != nil {
		code := puzzleErrorCode(err)
		if code == constants.ErrInternal {
			l.Errorf("[tactical_puzzle] LikeTacticalPuzzle failed player=%d puzzle=%d: %v", playerID, in.GetPuzzleId(), err)
		}
		return &matchpb.LikeTacticalPuzzleResponse{ErrorMessage: tipErr(code, "点赞残局失败")}, nil
	}
	return &matchpb.LikeTacticalPuzzleResponse{LikeCount: likes, SelfLiked: liked}, nil
}

func (l *TacticalPuzzleLogic) GetTacticalPuzzleSolutionReplay(in *matchpb.GetTacticalPuzzleSolutionReplayRequest) (*matchpb.GetTacticalPuzzleSolutionReplayResponse, error) {
	playerID := l.sessionPlayerID()
	if playerID == 0 || l.svcCtx == nil || l.svcCtx.Authority == nil {
		return &matchpb.GetTacticalPuzzleSolutionReplayResponse{ErrorMessage: tipErr(constants.ErrInternal, "残局坊暂不可用")}, nil
	}
	env, err := l.svcCtx.Authority.GetTacticalPuzzleSolutionReplay(l.ctx, playerID, in.GetPuzzleId())
	if err != nil {
		code := puzzleErrorCode(err)
		if code == constants.ErrInternal {
			l.Errorf("[tactical_puzzle] GetTacticalPuzzleSolutionReplay failed player=%d puzzle=%d: %v", playerID, in.GetPuzzleId(), err)
		}
		return &matchpb.GetTacticalPuzzleSolutionReplayResponse{ErrorMessage: tipErr(code, "标准解录像暂不可用")}, nil
	}
	return &matchpb.GetTacticalPuzzleSolutionReplayResponse{Replay: env}, nil
}
