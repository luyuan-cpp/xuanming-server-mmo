package svc

import (
	"fmt"

	"login/internal/config"
	"login/internal/logic/pkg/token"
)

// tokenConfigFrom 把 login.yaml 的 TokenConfig 映射成 token.Config 并校验;配错返回错误,调用方拒绝启动。
// 开关的方向相反:配置项 RefreshReuseRevoke 缺省 true(吊销),token.Config.ReuseObserveOnly 的零值才是吊销,
// 这样不经配置直接构造 token.Config 的地方也拿到安全缺省。
func tokenConfigFrom(c config.TokenConf) (token.Config, error) {
	cfg := token.Config{
		AccessTokenTTL:    c.AccessTokenTTL,
		RefreshTokenTTL:   c.RefreshTokenTTL,
		RefreshReuseGrace: c.RefreshReuseGrace,
		ReuseObserveOnly:  !c.RefreshReuseRevoke,
	}
	if err := cfg.Validate(); err != nil {
		return token.Config{}, fmt.Errorf("TokenConfig: %w", err)
	}
	return cfg, nil
}

// zeroGraceRevokes:窗口为 0 又开着吊销时,同一 RefreshToken 请求重复一次(客户端重发、驱动对读超时的重发)
// 就会吊销刚签出的那一对。合法但危险,启动时要提示。
func zeroGraceRevokes(cfg token.Config) bool {
	return cfg.RefreshReuseGrace == 0 && !cfg.ReuseObserveOnly
}
