package authority

import (
	"context"
	"strings"

	"chat/internal/constants"
	chatpb "proto/chat"
	cgpb "proto/content_guard"
	"shared/contentguard"
)

// review 把一条聊天正文送内容安全服务(docs/design/rename-and-content-safety.md §6.6)。
// 第一个返回值是可以落库的正文(原文,或辱骂类打码之后的);第二个返回值非 0 表示这条不能发,是回给客户端的提示码。
//
// 调用点在发送事务之外:事务里不做远程调用。没装 guard(单测)时不送检。
// 只认「通过」与「打码」;不可达、超时回可重试的提示,其余一律当命中:没检过的正文不落库。
func (s *Store) review(ctx context.Context, channel chatpb.ChatChannelType, actor uint64, content string) (string, uint32) {
	if s.guard == nil {
		return content, 0
	}
	scene := cgpb.TextScene_TEXT_SCENE_CHAT_PRIVATE
	if channel == chatpb.ChatChannelType_CHAT_CHANNEL_TYPE_WORLD {
		scene = cgpb.TextScene_TEXT_SCENE_CHAT_WORLD
	}
	result := s.guard.Check(ctx, scene, actor, content)
	switch result.Verdict {
	case contentguard.Pass:
		return content, 0
	case contentguard.Masked:
		// 打码只把命中的字换成 *,不会变长、不会变成空白;不是这样就是内容安全服务的缺陷,不落库。
		if strings.TrimSpace(result.Masked) != "" && len(result.Masked) <= s.rules.MaxContentBytes {
			return result.Masked, 0
		}
	case contentguard.Unavailable:
		return "", constants.ErrContentCheckUnavailable
	}
	return "", constants.ErrContentRejected
}
