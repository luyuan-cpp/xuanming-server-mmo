package logic

import (
	"context"

	"github.com/zeromicro/go-zero/core/logx"

	"guild/internal/constants"
	base "proto/common/base"
	cgpb "proto/content_guard"
	"shared/contentguard"
)

// 帮会名与帮会公告的内容送检(docs/design/rename-and-content-safety.md §6.6)。
//
// 两条贯穿本文件的纪律:
//
//  1. **送检在事务之外、在任何写之前**。它是一次 RPC,事务里不调 RPC(AGENTS §7 第 6 条);
//     被拒是玩家侧的常态,不该为它铸号或开事务。
//  2. **不放行没检过的文字**。帮会名和公告都是长期公开展示的:内容安全不可达时回可重试的提示,
//     而不是先放行。只有「通过」才算能用;打码后的正文对名字和公告没有意义,按命中处理。

// WithContentGuard 注入内容安全客户端;不传或传 nil = 不送检(单测,以及没配目标的本机环境由客户端自己放行)。
// 生产由 guild.go 传 contentguard.New 的结果。
func WithContentGuard(c contentguard.Checker) Option {
	return func(l *GuildLogic) {
		if c != nil {
			l.contentGuard = c
		}
	}
}

// checkText 送检一段文字。返回 nil 表示可以用;否则是要回给客户端的提示。
// rejectTip 是这一类文字命中时的 tip 码(帮会名、公告各一个),不可达统一回 ErrTextCheckUnavailable。
// 日志不带被检文字。
func (l *GuildLogic) checkText(ctx context.Context, scene cgpb.TextScene, playerID uint64, text string, rejectTip uint32) *base.TipInfoMessage {
	if l.contentGuard == nil || text == "" {
		return nil
	}
	result := l.contentGuard.Check(ctx, scene, playerID, text)
	switch result.Verdict {
	case contentguard.Pass:
		return nil
	case contentguard.Unavailable:
		logx.Errorf("[guild] content guard unavailable, text was not checked and the request is refused (scene=%s player=%d)", scene, playerID)
		return tipErr(constants.ErrTextCheckUnavailable, "content check unavailable")
	default:
		logx.Infof("[guild] text refused by content guard (scene=%s category=%s player=%d)", scene, result.Category, playerID)
		return tipErr(rejectTip, "text refused")
	}
}
