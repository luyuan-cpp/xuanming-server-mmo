package authority

import (
	"context"
	"strings"
	"testing"

	"chat/internal/constants"
	chatpb "proto/chat"
	cgpb "proto/content_guard"
	"shared/contentguard"

	"github.com/stretchr/testify/require"
)

// 正文只有「通过」和「打码」能落库;不可达回可重试的提示,其余(含不认识的结局、坏掉的打码结果)一律当命中。
func TestReviewOnlyTrustsPassAndSaneMask(t *testing.T) {
	ctx := context.Background()
	var gotScene cgpb.TextScene
	var gotPlayer uint64
	var gotText string
	result := contentguard.Result{Verdict: contentguard.Pass}
	s := &Store{rules: Rules{MaxContentBytes: 16}}
	WithContentGuard(contentguard.Func(func(_ context.Context, scene cgpb.TextScene, playerID uint64, text string) contentguard.Result {
		gotScene, gotPlayer, gotText = scene, playerID, text
		return result
	}))(s)

	content, tip := s.review(ctx, chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_WORLD, 7, "原文")
	require.Zero(t, tip)
	require.Equal(t, "原文", content)
	require.Equal(t, cgpb.TextScene_TEXT_SCENE_CHAT_WORLD, gotScene)
	require.EqualValues(t, 7, gotPlayer)
	require.Equal(t, "原文", gotText)

	_, _ = s.review(ctx, chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_PRIVATE, 7, "原文")
	require.Equal(t, cgpb.TextScene_TEXT_SCENE_CHAT_PRIVATE, gotScene)

	for name, c := range map[string]struct {
		result  contentguard.Result
		content string
		tip     uint32
	}{
		"masked":                  {contentguard.Result{Verdict: contentguard.Masked, Masked: "** 原文"}, "** 原文", 0},
		"masked but blank":        {contentguard.Result{Verdict: contentguard.Masked, Masked: " "}, "", constants.ErrContentRejected},
		"masked longer than max":  {contentguard.Result{Verdict: contentguard.Masked, Masked: strings.Repeat("*", 17)}, "", constants.ErrContentRejected},
		"reject":                  {contentguard.Result{Verdict: contentguard.Reject}, "", constants.ErrContentRejected},
		"unavailable":             {contentguard.Result{Verdict: contentguard.Unavailable}, "", constants.ErrContentCheckUnavailable},
		"zero value fails closed": {contentguard.Result{}, "", constants.ErrContentRejected},
	} {
		result = c.result
		content, tip = s.review(ctx, chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_WORLD, 7, "原文")
		require.Equal(t, c.tip, tip, name)
		require.Equal(t, c.content, content, name)
	}
}

// 没装内容安全(单测装配):不送检,原文照用。传 nil 不会把字段覆盖掉。
func TestReviewWithoutGuardPassesThrough(t *testing.T) {
	s := &Store{rules: Rules{MaxContentBytes: 16}}
	WithContentGuard(nil)(s)
	content, tip := s.review(context.Background(), chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_WORLD, 7, "原文")
	require.Zero(t, tip)
	require.Equal(t, "原文", content)
}
