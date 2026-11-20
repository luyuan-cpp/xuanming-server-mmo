package svc

import (
	"fmt"
	"shared/accountdirectory"
	"shared/rolefence"
	"shared/serviceauth"
	"strings"
)

// 独立caller密钥必须互异；共享同一HMAC值会让只读SM伪装login/scene/trade，purpose本身挡不住此种配置。
func validateGlobalCallerSecrets(getenv func(string) string) error {
	seen := map[string]bool{}
	for _, entry := range []struct{ caller, env string }{{"login", accountdirectory.SecretEnv}, {"trade", accountdirectory.TradeSecretEnv}, {"scene", accountdirectory.SceneSecretEnv}, {"scene_manager", "MMORPG_ACCOUNT_DIRECTORY_SECRET_SCENE_MANAGER"}} {
		secret := strings.TrimSpace(getenv(entry.env))
		if _, err := serviceauth.New(map[string]string{entry.caller: secret}); err != nil {
			return fmt.Errorf("全球目录caller %s独立密钥缺失或非法", entry.caller)
		}
		if seen[secret] {
			return fmt.Errorf("全球目录caller密钥不能跨用途复用")
		}
		seen[secret] = true
	}
	// 角色护栏的密钥缺了只关闭对应能力(见 configureRoleFence),不拒启;配了就同样不能跨用途复用。
	// payment 查实名(GetAccountRealName)同一规矩:缺了只停用这个接口,配了就不能和别的调用方共用,否则持那一把的服务能读全体玩家的实名。
	guarded := map[string]string{rolefence.CoordinatorCaller: rolefence.CoordinatorSecretEnv, accountdirectory.RouteCaller: accountdirectory.RouteSecretEnv,
		accountdirectory.PaymentCaller: accountdirectory.PaymentSecretEnv}
	for _, caller := range rolefence.Callers {
		guarded[caller] = rolefence.SecretEnv(caller)
	}
	for caller, env := range guarded {
		secret := strings.TrimSpace(getenv(env))
		if secret == "" {
			continue
		}
		if _, err := serviceauth.New(map[string]string{caller: secret}); err != nil {
			return fmt.Errorf("角色护栏caller %s密钥非法", caller)
		}
		if seen[secret] {
			return fmt.Errorf("角色护栏caller %s的密钥不能跨用途复用", caller)
		}
		seen[secret] = true
	}
	return nil
}
