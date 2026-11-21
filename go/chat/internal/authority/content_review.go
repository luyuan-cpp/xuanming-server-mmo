package authority

import (
	"context"
	"strings"

	"chat/internal/constants"
	chatpb "proto/chat"
	cgpb "proto/content_guard"
	"shared/contentguard"
)

// worldChecker 是支持世界频道异步平台补审的内容安全客户端扩展接口。
type worldChecker interface {
	CheckWorld(ctx context.Context, playerID uint64, text string) contentguard.Result
}

// review 把一条聊天正文送内容安全服务(docs/design/rename-and-content-safety.md §6.6)。
// 返回值依次为:可落库的正文(原文或辱骂打码文)、非 0 拒绝提示码、是否需要后台异步复检外部平台。
//
// 调用点在发送事务之外:事务里不做远程调用。没装 guard(单测)时不送检。
// 只认「通过」与「打码」;不可达、超时回可重试的提示,其余一律当命中:没检过的正文不落库。
func (s *Store) review(ctx context.Context, channel chatpb.ChatChannelType, actor uint64, content string) (string, uint32, bool) {
	if s.guard == nil {
		return content, 0, false
	}
	var result contentguard.Result
	if channel == chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_WORLD {
		if wc, ok := s.guard.(worldChecker); ok {
			result = wc.CheckWorld(ctx, actor, content)
		} else {
			result = s.guard.Check(ctx, cgpb.TextScene_TEXT_SCENE_CHAT_WORLD, actor, content)
		}
	} else {
		result = s.guard.Check(ctx, cgpb.TextScene_TEXT_SCENE_CHAT_PRIVATE, actor, content)
	}

	switch result.Verdict {
	case contentguard.Pass:
		return content, 0, result.PendingPlatformReview
	case contentguard.Masked:
		// 打码只把命中的字换成 *,不会变长、不会变成空白;不是这样就是内容安全服务的缺陷,不落库。
		if strings.TrimSpace(result.Masked) != "" && len(result.Masked) <= s.rules.MaxContentBytes {
			return result.Masked, 0, result.PendingPlatformReview
		}
	case contentguard.Unavailable:
		return "", constants.ErrContentCheckUnavailable, false
	}
	if result.Category == cgpb.TextCategory_TEXT_CATEGORY_AD {
		return "", constants.ErrContactRejected, false
	}
	return "", constants.ErrContentRejected, false
}
