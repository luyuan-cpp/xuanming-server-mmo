package svc

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/service"

	"login/internal/config"
	"login/internal/logic/pkg/auth"
	"login/internal/logic/pkg/token"
	"shared/playerredis"
)

// InitAuthProviders 按显式配置注册认证器。生产口令认证只读权威 MySQL 中的
// Argon2id 哈希；没有显式启用时仍 fail-closed。开发共享密钥认证与生产认证
// 互斥，避免配置合并时把弱入口带进生产。
func InitAuthProviders() (cleanup func()) {
	cleanup = func() {}
	initialized := false
	defer func() {
		if !initialized {
			cleanup()
		}
	}()
	if config.AppConfig.DevSkipAuth {
		panic("DevSkipAuth has been removed because it trusted arbitrary accounts; use the guarded DevPasswordAuth block")
	}

	productionPassword := config.AppConfig.PasswordAuth
	devPassword := config.AppConfig.DevPasswordAuth
	if productionPassword.Enabled && devPassword.Enabled {
		panic("PasswordAuth and DevPasswordAuth cannot both be enabled")
	}

	if productionPassword.Enabled {
		provider, closeProvider, err := productionPasswordProviderFromConfig(productionPassword, config.AppConfig.EffectiveLoginRedis(), os.LookupEnv)
		if err != nil {
			panic(fmt.Sprintf("initialize PasswordAuth provider: %v", err))
		}
		cleanup = closeProvider
		auth.RegisterPassword(provider)
		logx.Info("Production password auth registered (authoritative MySQL Argon2id, read-only)")
	}
	if devPassword.Enabled {
		if err := validateDevelopmentPasswordMode(config.AppConfig.Mode); err != nil {
			panic(fmt.Sprintf("invalid DevPasswordAuth configuration: %v", err))
		}
		provider, err := developmentPasswordProviderFromConfig(devPassword, os.LookupEnv)
		if err != nil {
			panic(fmt.Sprintf("invalid DevPasswordAuth configuration: %v", err))
		}
		auth.RegisterPassword(provider)
		logx.Errorf("SECURITY WARNING: development password auth ENABLED for account prefixes %v; never enable this in production",
			devPassword.AllowedAccountPrefixes)
	} else if !productionPassword.Enabled {
		logx.Info("Password auth disabled (fail-closed); configure PasswordAuth or guarded DevPasswordAuth")
	}

	cfg := config.AppConfig.AuthProviders

	if cfg.SaToken != nil {
		if err := cfg.SaToken.Redis.ValidatePoolSize(); err != nil {
			panic(fmt.Errorf("AuthProviders.SaToken.Redis: %w", err))
		}
		rdb := redis.NewClient(&redis.Options{
			Addr:             cfg.SaToken.Redis.Host,
			Password:         cfg.SaToken.Redis.Password,
			DB:               int(cfg.SaToken.Redis.DB),
			DialTimeout:      cfg.SaToken.Redis.DialTimeout,
			ReadTimeout:      cfg.SaToken.Redis.ReadTimeout,
			WriteTimeout:     cfg.SaToken.Redis.WriteTimeout,
			PoolSize:         cfg.SaToken.Redis.PoolSize, // 同一个 RedisConf:写了就生效,不静默忽略
			DisableIndentity: true,
		})
		if err := rdb.Ping(context.Background()).Err(); err != nil {
			panic(fmt.Errorf("failed to connect SA-Token Redis for auth provider: %w", err))
		}
		auth.Register("satoken", &auth.SaTokenProvider{
			RedisClient: rdb,
			TokenName:   cfg.SaToken.TokenName,
			LoginType:   cfg.SaToken.LoginType,
		})
		logx.Infof("Auth provider registered: satoken (Redis=%s)", cfg.SaToken.Redis.Host)
	}

	// 第三方凭据只从 ResolveSecrets 的结果取(auth_providers.go 已校验 Env / 占位符 / https),不再读 yaml 明文。
	providerSecrets := activeSecrets().AuthProviders
	if cfg.WeChat != nil {
		provider, err := weChatProviderFromConfig(cfg.WeChat, providerSecrets)
		if err != nil {
			panic(fmt.Sprintf("invalid AuthProviders.WeChat configuration: %v", err))
		}
		auth.Register("wechat", provider)
		logx.Infof("Auth provider registered: wechat (require_unionid=%v endpoint_override=%q)",
			cfg.WeChat.RequireUnionId, cfg.WeChat.Endpoint)
	}

	if cfg.QQ != nil {
		auth.Register("qq", qqProviderFromConfig(cfg.QQ, providerSecrets))
		logx.Infof("Auth provider registered: qq (request_unionid=%v require_unionid=%v endpoint_override=%q)",
			cfg.QQ.RequestUnionId, cfg.QQ.RequireUnionId, cfg.QQ.Endpoint)
	}

	if cfg.NetEase != nil {
		auth.Register("netease", &auth.NeteaseProvider{
			AppKey:    cfg.NetEase.AppKey,
			AppSecret: cfg.NetEase.AppSecret,
		})
		logx.Info("Auth provider registered: netease")
	}
	initialized = true
	return cleanup
}

// 口令配额在登录域。专用小池使认证依赖具有独立硬上限；不继承主缓存池的等待策略。
// 地址、口令、集群形态随登录域,连接数与超时是这条路径自己的预算;认 ctx 截止、不重放结果未知的命令由 playerredis 保证。
func passwordQuotaRedisConfig(loginRedis playerredis.Config) playerredis.Config {
	budgetMs := int(auth.PasswordQuotaBudget.Milliseconds())
	loginRedis.PoolSizePerNode = 4
	loginRedis.DisableContextTimeout = false
	loginRedis.DialTimeoutMs = budgetMs
	loginRedis.ReadTimeoutMs = budgetMs
	loginRedis.WriteTimeoutMs = budgetMs
	loginRedis.PoolTimeoutMs = budgetMs
	return loginRedis
}

func productionPasswordProviderFromConfig(conf config.PasswordAuthConf, loginRedis playerredis.Config, lookup func(string) (string, bool)) (*auth.ProductionPasswordProvider, func(), error) {
	if err := conf.ValidateFailureQuota(); err != nil {
		return nil, nil, err
	}
	dsn, err := passwordAuthDSNFromConfig(conf, lookup)
	if err != nil {
		return nil, nil, err
	}
	store, err := auth.NewSQLPasswordHashStore(dsn, conf.MaxOpenConns, conf.MaxIdleConns)
	if err != nil {
		return nil, nil, err
	}
	rdb, err := playerredis.New(passwordQuotaRedisConfig(loginRedis))
	if err != nil {
		_ = store.Close()
		return nil, nil, fmt.Errorf("password quota Redis config: %w", err)
	}
	var once sync.Once
	cleanup := func() { once.Do(func() { _ = rdb.Close(); _ = store.Close() }) }
	ready := false
	defer func() {
		if !ready {
			cleanup()
		}
	}()
	quota, err := auth.NewRedisPasswordFailureQuota(rdb, conf.FailureLimit, conf.FailureWindow, conf.FailureLock)
	if err != nil {
		return nil, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), auth.PasswordQuotaBudget)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		return nil, nil, fmt.Errorf("password quota Redis startup unavailable")
	}
	provider, err := auth.NewProductionPasswordProviderWithLimits(store, quota, conf.KDFConcurrency, conf.KDFWaitTimeout)
	if err != nil {
		return nil, nil, err
	}
	ready = true
	return provider, cleanup, nil
}

func passwordAuthDSNFromConfig(conf config.PasswordAuthConf, lookup func(string) (string, bool)) (string, error) {
	envName := strings.TrimSpace(conf.DSNEnv)
	if envName == "" {
		return "", fmt.Errorf("DSNEnv must name a non-empty environment variable")
	}
	dsn, ok := lookup(envName)
	if !ok || strings.TrimSpace(dsn) == "" {
		return "", fmt.Errorf("environment variable %q is missing or empty", envName)
	}
	return dsn, nil
}

// validateDevelopmentPasswordMode 是 DevPasswordAuth 的启动门禁。只检查
// Enabled 不够：一份误带开关的生产配置即使有前缀和环境密钥，仍会把共享
// 密码认证暴露到生产。go-zero 的 dev/test Mode 是唯一允许注册该认证器的环境。
func validateDevelopmentPasswordMode(mode string) error {
	if mode == service.DevMode || mode == service.TestMode {
		return nil
	}
	return fmt.Errorf("DevPasswordAuth requires go-zero Mode %q or %q, got %q",
		service.DevMode, service.TestMode, mode)
}

// developmentPasswordProviderFromConfig 把“配置中的环境变量名”和真正密钥
// 分开。lookup 作为参数只用于单元测试；生产固定传 os.LookupEnv，禁止从 YAML
// 或其他受管配置字段直接读取共享密钥。
func developmentPasswordProviderFromConfig(conf config.DevPasswordAuthConf, lookup func(string) (string, bool)) (*auth.DevelopmentPasswordProvider, error) {
	envName := strings.TrimSpace(conf.SharedSecretEnv)
	if envName == "" {
		return nil, fmt.Errorf("SharedSecretEnv must name a non-empty environment variable")
	}
	sharedSecret, ok := lookup(envName)
	if !ok || sharedSecret == "" {
		return nil, fmt.Errorf("environment variable %q is missing or empty", envName)
	}
	return auth.NewDevelopmentPasswordProvider(sharedSecret, conf.AllowedAccountPrefixes)
}

// weChatProviderFromConfig 把配置与 ResolveSecrets 解析出的密钥组装成 provider。不注册,便于单测
// (auth 注册表是进程级的,同名重复注册会 panic)。密钥为空说明 ResolveSecrets 没跑:拒绝,不带空密钥上线。
func weChatProviderFromConfig(conf *config.WeChatAuthConf, secrets config.AuthProviderSecrets) (*auth.WeChatProvider, error) {
	if secrets.WeChatAppSecret == "" {
		return nil, fmt.Errorf("AppSecret was not resolved; config.ResolveSecrets must run before InitAuthProviders")
	}
	return &auth.WeChatProvider{
		AppId:               conf.AppId,
		AppSecret:           secrets.WeChatAppSecret,
		Endpoint:            conf.Endpoint,
		AllowOpenIdFallback: !conf.RequireUnionId,
	}, nil
}

// qqProviderFromConfig 同 weChatProviderFromConfig;QQ 不需要密钥,AppKey 只是透传保留字段。
func qqProviderFromConfig(conf *config.QQAuthConf, secrets config.AuthProviderSecrets) *auth.QQProvider {
	return &auth.QQProvider{
		AppId:                 conf.AppId,
		AppKey:                secrets.QQAppKey,
		Endpoint:              conf.Endpoint,
		AllowOpenIdFallback:   !conf.RequireUnionId,
		DisableUnionIdRequest: !conf.RequestUnionId,
	}
}

// RegisterAccessTokenProvider registers the access_token provider.
// Called after TokenManager is created in NewServiceContext.
func RegisterAccessTokenProvider(tokenMgr *token.Manager) {
	auth.Register("access_token", &auth.AccessTokenProvider{
		TokenManager: tokenMgr,
	})
	logx.Info("Auth provider registered: access_token")
}
