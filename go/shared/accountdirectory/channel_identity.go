package accountdirectory

import (
	"time"

	dspb "proto/data_service"
	"shared/serviceauth"
)

// 按玩家或账号查渠道身份(GetPlayerChannelIdentity,docs/design/rename-and-content-safety.md §6.5)的调用方。
// 应答里有玩家的账号、渠道与平台 openid,只给这两个调用方读,各用各的密钥签名。
const (
	// ContentGuardCaller 是内容安全服务查渠道身份(平台审核要带 openid)时的 ServiceCallAuth.caller。
	ContentGuardCaller = "content_guard"
	// ContentGuardSecretEnv 是它的密钥,与其它调用方互不复用;data_service 验同一把。
	ContentGuardSecretEnv = "MMORPG_ACCOUNT_DIRECTORY_SECRET_CONTENT_GUARD"
	// ChatCaller 是 chat 读玩家画像(世界频道等级门槛、新号私聊上限)时的 caller。它用自己读角色护栏的那把密钥签名
	// (rolefence.SecretEnv("chat")),不另发一把:密钥按调用方发,签名里带着方法名,一把密钥签不出另一个方法的请求。
	ChatCaller = "chat"
)

// SignChannelIdentityRequest 给一次渠道身份查询签上调用方证明。每次调用前现签:证明带时间,data_service 只收 5 分钟以内的。
func SignChannelIdentityRequest(in *dspb.GetPlayerChannelIdentityRequest, caller, secret string) error {
	return serviceauth.Sign(MethodPrefix+"GetPlayerChannelIdentity", in, caller, secret, uint64(time.Now().UnixMilli()))
}
