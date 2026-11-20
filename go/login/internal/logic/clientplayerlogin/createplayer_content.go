package clientplayerloginlogic

import (
	login_proto_common "proto/common/base"
	cgpb "proto/content_guard"
	"shared/contentguard"
	"shared/generated/pb/table"

	"github.com/zeromicro/go-zero/core/logx"
)

// checkRoleNameContent 把玩家自己起的角色名送内容安全服务判定(docs/design/rename-and-content-safety.md §6.6)。
// 返回 nil 表示可以用;否则是要回给客户端的提示。
//
// 纯判定、没有副作用,所以和名字预检一样排在发号之前。几种不送检的情况:
//   - requested 为空:名字由服务端生成(前缀来自配表、后缀是 [a-z0-9]),起服时已经核过;
//   - ContentGuard 为 nil:只会出现在直接构造 ServiceContext 的单测里,生产路径起服后永不为 nil
//     (没配目标时是一个一律放行的客户端,见 svc.NewServiceContext)。
//
// 这时还没有 player_id(发号在后面),填 0。
func (l *CreatePlayerLogic) checkRoleNameContent(requested, account string) *login_proto_common.TipInfoMessage {
	guard := l.svcCtx.ContentGuard
	if requested == "" || guard == nil {
		return nil
	}
	return roleNameContentTip(guard.Check(l.ctx, cgpb.TextScene_TEXT_SCENE_ROLE_NAME, 0, requested), account)
}

// roleNameContentTip 把送检结局换成建角的提示。名字类场景只有「通过」才算能用:
//   - 命中屏蔽词 → 与内置词命中同一个提示(kRoleNameSensitive),客户端不用区分是哪一层拦的;
//   - 内容安全不可达或超时 → 可重试的提示。不放行没检过的名字:名字长期公开展示,宁可让玩家稍后再试;
//   - 其余取值(打码后的正文对名字没有意义,以及将来新增的结局)一律按命中处理。
//
// 日志不带名字原文。
func roleNameContentTip(result contentguard.Result, account string) *login_proto_common.TipInfoMessage {
	switch result.Verdict {
	case contentguard.Pass:
		return nil
	case contentguard.Unavailable:
		logx.Errorf("CreatePlayer rejected: content guard unavailable, the role name was not checked (account=%s)", account)
		return loginTip(table.LoginError_kRoleNameCheckUnavailable)
	default:
		logx.Infof("CreatePlayer rejected: role name refused by content guard (category=%s account=%s)", result.Category, account)
		return loginTip(table.LoginError_kRoleNameSensitive)
	}
}
