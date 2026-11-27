package logic

import (
	"context"
	"testing"

	"match/internal/constants"
	"match/internal/pkg/ctxkeys"
	"match/internal/svc"

	base "proto/common/base"
	cgpb "proto/content_guard"
	matchpb "proto/match"

	"shared/contentguard"

	"github.com/stretchr/testify/require"
)

type stubSpectateChatGuard struct {
	fn func(ctx context.Context, scene cgpb.TextScene, playerID uint64, text string) contentguard.Result
}

func (g stubSpectateChatGuard) Check(ctx context.Context, scene cgpb.TextScene, playerID uint64, text string) contentguard.Result {
	return g.fn(ctx, scene, playerID, text)
}

func spectateChatCtx(playerID uint64) context.Context {
	return ctxkeys.WithSessionDetails(context.Background(), &base.SessionDetails{PlayerId: playerID})
}

func TestSpectateChatDisabledDoesNotBreakWatchingAndBlocksSend(t *testing.T) {
	orig := loadSpectateChatRoomFn
	defer func() { loadSpectateChatRoomFn = orig }()

	loadSpectateChatRoomFn = func(_ context.Context, _ *svc.ServiceContext, battleID uint64, _ uint64) (*spectateChatRoomInfo, error) {
		return &spectateChatRoomInfo{
			battleID:     battleID,
			visibility:   visPublic,
			chatEnabled:  false, // 正常可观战，但开局冻结关闭局外人交流
			ended:        false,
			deadlineMs:   nowMs() + 600_000,
			participants: map[uint64]struct{}{1001: {}, 1002: {}},
		}, nil
	}

	sc := &svc.ServiceContext{}
	// 观战者改包强发 → 必须被服务端硬拒绝
	sendResp, err := NewSpectateChatLogic(spectateChatCtx(2001), sc).SendBattleSpectateChat(&matchpb.SendBattleSpectateChatRequest{
		BattleId: 9001,
		Content:  "快集火右边！",
	})
	require.NoError(t, err)
	require.NotNil(t, sendResp.GetErrorMessage())
	require.Equal(t, constants.ErrSpectateChatDisabled, sendResp.GetErrorMessage().GetId())
	require.False(t, sendResp.GetEnabled())

	// 拉取静默回 enabled=false、无错误噪音
	pullResp, err := NewSpectateChatLogic(spectateChatCtx(2001), sc).PullBattleSpectateChat(&matchpb.PullBattleSpectateChatRequest{
		BattleId: 9001,
	})
	require.NoError(t, err)
	require.Nil(t, pullResp.GetErrorMessage())
	require.False(t, pullResp.GetEnabled())
	require.Empty(t, pullResp.GetMessages())
}

func TestSpectateChatSendPullRolesCooldownRateLimitAndGuard(t *testing.T) {
	orig := loadSpectateChatRoomFn
	defer func() { loadSpectateChatRoomFn = orig }()

	room := &spectateChatRoomInfo{
		battleID:     9002,
		visibility:   visPublic,
		chatEnabled:  true,
		ended:        false,
		deadlineMs:   nowMs() + 600_000,
		participants: map[uint64]struct{}{1001: {}, 1002: {}},
	}
	loadSpectateChatRoomFn = func(_ context.Context, _ *svc.ServiceContext, battleID uint64, _ uint64) (*spectateChatRoomInfo, error) {
		if battleID != room.battleID {
			return nil, nil
		}
		return room, nil
	}

	sc := &svc.ServiceContext{
		ContentGuard: stubSpectateChatGuard{fn: func(_ context.Context, _ cgpb.TextScene, _ uint64, text string) contentguard.Result {
			switch text {
			case "违禁广告加群":
				return contentguard.Result{Verdict: contentguard.Reject, Category: cgpb.TextCategory_TEXT_CATEGORY_AD}
			case "审核故障":
				return contentguard.Result{Verdict: contentguard.Unavailable}
			case "粗口测试":
				return contentguard.Result{Verdict: contentguard.Masked, Masked: "**测试"}
			default:
				return contentguard.Result{Verdict: contentguard.Pass}
			}
		}},
	}
	sc.Config.SpectateChat.RoomRateLimitPerSec = 3

	// 1. 观战者 2001 发言成功，服务端权威标注 SPECTATOR
	r1, err := NewSpectateChatLogic(spectateChatCtx(2001), sc).SendBattleSpectateChat(&matchpb.SendBattleSpectateChatRequest{
		BattleId: 9002, Content: "这波反杀太帅了",
	})
	require.NoError(t, err)
	require.Nil(t, r1.GetErrorMessage())
	require.Equal(t, uint64(1), r1.GetMessage().GetSeq())
	require.Equal(t, matchpb.BattleSpectateChatRole_BATTLE_SPECTATE_CHAT_ROLE_SPECTATOR, r1.GetMessage().GetRole())

	// 2. 观战者 2001 立即连发命中个人冷却
	rCd, err := NewSpectateChatLogic(spectateChatCtx(2001), sc).SendBattleSpectateChat(&matchpb.SendBattleSpectateChatRequest{
		BattleId: 9002, Content: "再来一句",
	})
	require.NoError(t, err)
	require.Equal(t, constants.ErrSpectateChatCooldown, rCd.GetErrorMessage().GetId())

	// 3. 参战者 1001 发言（命中打码），服务端权威标注 PARTICIPANT
	r2, err := NewSpectateChatLogic(spectateChatCtx(1001), sc).SendBattleSpectateChat(&matchpb.SendBattleSpectateChatRequest{
		BattleId: 9002, Content: "粗口测试",
	})
	require.NoError(t, err)
	require.Nil(t, r2.GetErrorMessage())
	require.Equal(t, uint64(2), r2.GetMessage().GetSeq())
	require.Equal(t, "**测试", r2.GetMessage().GetContent())
	require.Equal(t, matchpb.BattleSpectateChatRole_BATTLE_SPECTATE_CHAT_ROLE_PARTICIPANT, r2.GetMessage().GetRole())

	// 4. 内容安全拒收 & 审核不可用 fail-closed
	rRej, err := NewSpectateChatLogic(spectateChatCtx(2002), sc).SendBattleSpectateChat(&matchpb.SendBattleSpectateChatRequest{
		BattleId: 9002, Content: "违禁广告加群",
	})
	require.NoError(t, err)
	require.Equal(t, constants.ErrSpectateChatContentRejected, rRej.GetErrorMessage().GetId())

	rUnavail, err := NewSpectateChatLogic(spectateChatCtx(2002), sc).SendBattleSpectateChat(&matchpb.SendBattleSpectateChatRequest{
		BattleId: 9002, Content: "审核故障",
	})
	require.NoError(t, err)
	require.Equal(t, constants.ErrSpectateChatCheckUnavailable, rUnavail.GetErrorMessage().GetId())

	// 5. 第三位不同观众 2003 发言成功（达到每秒 3 条上限），第四位 2004 同秒触发单房总流速封顶
	r3, err := NewSpectateChatLogic(spectateChatCtx(2003), sc).SendBattleSpectateChat(&matchpb.SendBattleSpectateChatRequest{
		BattleId: 9002, Content: "稳住阵脚",
	})
	require.NoError(t, err)
	require.Nil(t, r3.GetErrorMessage())

	rBusy, err := NewSpectateChatLogic(spectateChatCtx(2004), sc).SendBattleSpectateChat(&matchpb.SendBattleSpectateChatRequest{
		BattleId: 9002, Content: "挤一挤",
	})
	require.NoError(t, err)
	require.Equal(t, constants.ErrSpectateChatRoomBusy, rBusy.GetErrorMessage().GetId())

	// 6. 增量拉取：after_seq=1 只返回 seq 2、3
	pull, err := NewSpectateChatLogic(spectateChatCtx(1002), sc).PullBattleSpectateChat(&matchpb.PullBattleSpectateChatRequest{
		BattleId: 9002, AfterSeq: 1,
	})
	require.NoError(t, err)
	require.Nil(t, pull.GetErrorMessage())
	require.True(t, pull.GetEnabled())
	require.False(t, pull.GetBattleEnded())
	require.Equal(t, uint64(3), pull.GetLatestSeq())
	require.Len(t, pull.GetMessages(), 2)
	require.Equal(t, uint64(2), pull.GetMessages()[0].GetSeq())
	require.Equal(t, uint64(3), pull.GetMessages()[1].GetSeq())

	// 7. 终局封房：禁止继续发消息，拉取返回 battle_ended=true
	room.ended = true
	rEnded, err := NewSpectateChatLogic(spectateChatCtx(2005), sc).SendBattleSpectateChat(&matchpb.SendBattleSpectateChatRequest{
		BattleId: 9002, Content: "赛后聊天",
	})
	require.NoError(t, err)
	require.Equal(t, constants.ErrSpectateChatEnded, rEnded.GetErrorMessage().GetId())
	require.True(t, rEnded.GetBattleEnded())
}
