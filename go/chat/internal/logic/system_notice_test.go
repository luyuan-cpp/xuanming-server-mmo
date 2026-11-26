package logic

import (
	"context"
	"strings"
	"testing"

	"chat/internal/authority"
	"chat/internal/constants"
	chatpb "proto/chat"

	"github.com/stretchr/testify/require"
)

func TestPublishSystemWorldNoticeWhitelistAndDedup(t *testing.T) {
	sc, spy := fixture()
	pusher := &pushSpy{}
	sc.WorldPush = pusher

	// 1. 合法白名单模板首次提交：推送一次世界播报。
	req := &chatpb.PublishSystemWorldNoticeRequest{
		NoticeId:    "jh_duel:7001:start",
		TemplateId:  chatpb.SystemWorldNoticeTemplate_SYSTEM_WORLD_NOTICE_JIANGHU_DUEL_START_GOLD,
		BattleId:    7001,
		ActorAName:  "青风\n剑客",
		ActorBName:  "墨渊",
		TeamSize:    3,
	}
	resp, err := NewChatLogic(context.Background(), sc).PublishSystemWorldNotice(req)
	require.NoError(t, err)
	require.Nil(t, resp.GetErrorMessage())
	require.False(t, resp.GetDeduplicated())
	require.Equal(t, 1, spy.notices)
	require.Len(t, pusher.pushed, 1)
	require.Contains(t, pusher.pushed[0].GetContent(), "金帖生死状")
	require.Contains(t, pusher.pushed[0].GetContent(), "3V3")
	require.NotContains(t, pusher.pushed[0].GetContent(), "\n")

	// 2. 幂等重放：不重复广播世界频道。
	spy.notice = func(_ context.Context, _ *chatpb.PublishSystemWorldNoticeRequest) (authority.SendResult, error) {
		return authority.SendResult{Duplicate: true}, nil
	}
	resp, err = NewChatLogic(context.Background(), sc).PublishSystemWorldNotice(req)
	require.NoError(t, err)
	require.Nil(t, resp.GetErrorMessage())
	require.True(t, resp.GetDeduplicated())
	require.Len(t, pusher.pushed, 1)

	// 3. 宿敌连胜/终结模板校验与违禁词零出现。
	for _, tpl := range []chatpb.SystemWorldNoticeTemplate{
		chatpb.SystemWorldNoticeTemplate_SYSTEM_WORLD_NOTICE_JIANGHU_DUEL_START_SILVER,
		chatpb.SystemWorldNoticeTemplate_SYSTEM_WORLD_NOTICE_JIANGHU_DUEL_START_GOLD,
		chatpb.SystemWorldNoticeTemplate_SYSTEM_WORLD_NOTICE_JIANGHU_DUEL_WIN_SILVER,
		chatpb.SystemWorldNoticeTemplate_SYSTEM_WORLD_NOTICE_JIANGHU_DUEL_WIN_GOLD,
		chatpb.SystemWorldNoticeTemplate_SYSTEM_WORLD_NOTICE_JIANGHU_DUEL_DRAW,
		chatpb.SystemWorldNoticeTemplate_SYSTEM_WORLD_NOTICE_JIANGHU_DUEL_STREAK_MILESTONE,
		chatpb.SystemWorldNoticeTemplate_SYSTEM_WORLD_NOTICE_JIANGHU_DUEL_STREAK_BROKEN,
	} {
		text, ok := authority.RenderSystemWorldNotice(&chatpb.PublishSystemWorldNoticeRequest{
			NoticeId:    "jh_duel:7002:t",
			TemplateId:  tpl,
			ActorAName:  "青风",
			ActorBName:  "墨渊",
			TotalRounds: 12,
			StreakCount: 4,
			TeamSize:    1,
		})
		require.True(t, ok, "template %v must render", tpl)
		for _, banned := range []string{"杀人", "强杀", "监狱", "认怂"} {
			require.False(t, strings.Contains(text, banned), "template %v must not contain banned term %s", tpl, banned)
		}
	}

	// 4. 非法模板 / 连胜不足 3 / 空 ID 均被参数拒绝。
	badResp, err := NewChatLogic(context.Background(), sc).PublishSystemWorldNotice(&chatpb.PublishSystemWorldNoticeRequest{
		NoticeId:    "jh_duel:bad",
		TemplateId:  chatpb.SystemWorldNoticeTemplate_SYSTEM_WORLD_NOTICE_JIANGHU_DUEL_STREAK_MILESTONE,
		ActorAName:  "青风",
		ActorBName:  "墨渊",
		StreakCount: 2,
	})
	require.NoError(t, err)
	require.NotNil(t, badResp.GetErrorMessage())
	require.Equal(t, constants.ErrInvalidParameter, badResp.GetErrorMessage().GetId())
}
