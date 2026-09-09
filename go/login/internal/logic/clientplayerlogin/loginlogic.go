package clientplayerloginlogic

import (
	"context"
	"errors"
	"fmt"
	"login/internal/config"
	"login/internal/constants"
	"login/internal/logic/pkg/auth"
	"login/internal/logic/pkg/ctxkeys"
	"login/internal/logic/pkg/homezone"
	"login/internal/logic/pkg/locker"
	"login/internal/logic/pkg/loginsession"
	"login/internal/logic/pkg/token"
	"login/internal/svc"
	login_proto_common "proto/common/base"
	login_proto_data_base "proto/common/database"
	login_proto "proto/login"
	"shared/generated/pb/table"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"github.com/zeromicro/go-zero/core/logx"
)

type LoginLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewLoginLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LoginLogic {
	return &LoginLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *LoginLogic) Login(in *login_proto.LoginRequest) (*login_proto.LoginResponse, error) {
	resp := &login_proto.LoginResponse{}

	// Resolve account via auth provider
	account, err := l.resolveAccount(in)
	if err != nil {
		logx.Errorf("Auth failed: type=%s err=%v", in.AuthType, err)
		resp.ErrorMessage = &login_proto_common.TipInfoMessage{Id: uint32(table.LoginError_kLoginAccountNotFound)}
		return resp, nil
	}

	// 1. Distributed lock (UUID + Lua safe release)
	accountLock, err := locker.NewRedisLocker(l.svcCtx.RedisClient).TryLock(
		l.ctx, "account_lock:login:"+account,
		time.Duration(config.AppConfig.Locker.AccountLockTTL)*time.Second,
	)
	if err != nil || !accountLock.IsLocked() {
		logx.Errorf("Login lock failed for account=%s: %v", account, err)
		resp.ErrorMessage = &login_proto_common.TipInfoMessage{Id: uint32(table.LoginError_kLoginInProgress)}
		return resp, nil
	}
	defer func() {
		if _, err := accountLock.Release(l.ctx); err != nil {
			logx.Errorf("Login lock release failed for account=%s: %v", account, err)
		}
	}()

	// 2. Validate session
	sessionDetails, sessionFound := ctxkeys.GetSessionDetails(l.ctx)

	// Deprecation hint: when SessionDetails are attached, the call arrived
	// through cpp gate.HandleGrpcNodeMessage — i.e. the legacy path where the
	// client runs the Login RPC on its TCP channel after hitting assign-gate.
	// The new path (Java Gateway POST /api/login) calls this RPC directly
	// without a session_id, so an empty SessionDetails means "new path".
	//
	// We log the legacy path at warn, throttled, so ops can see the migration
	// progress without flooding the log. Functionality is unchanged — there is
	// no plan to break this path in the current release; a future version may
	// gate it behind a config flag.
	isLegacyPath := sessionFound && sessionDetails != nil && sessionDetails.SessionId > 0
	warnLegacyLoginCaller(isLegacyPath, in.AuthType)

	// ARCH §12 T+2 step: when ops has confirmed the legacy_login_count
	// counter is below the agreed threshold, they flip
	// `LegacyGateLoginEnabled: false` in login.yaml to harden the kill
	// switch. From that moment the legacy branch short-circuits BEFORE
	// any lock / Redis / device-set work, returning a generic login
	// error to the small tail of remaining old clients (which will
	// fall back to /api/login via their normal retry path). The flag
	// is off only when ops explicitly turns it off — default-true
	// preserves the current rollout state.
	//
	// We deliberately reuse kLoginUnknownError rather than minting a
	// new enum value: adding to the `login_error` proto requires a
	// codegen round and a cpp/go/java rebuild, which is overkill for
	// a kill-switch path the client SDK already handles with its
	// generic "login failed, please retry" UI. A dedicated error code
	// can land alongside the next proto codegen if the deprecation
	// telemetry ever shows the tail isn't shrinking.
	if shouldRejectLegacyRequest(isLegacyPath, config.AppConfig.LegacyGateLoginEnabled) {
		logx.Errorf("[DEPRECATION] legacy gate Login RPC disabled by config; "+
			"rejecting account=%s auth_type=%s session_id=%d. "+
			"Client should migrate to POST /api/login on Java Gateway.",
			account, in.AuthType, sessionDetails.SessionId)
		resp.ErrorMessage = &login_proto_common.TipInfoMessage{Id: uint32(table.LoginError_kLoginUnknownError)}
		return resp, nil
	}

	// New path (Java Gateway HTTP): no session exists yet because the player
	// hasn't touched the gate TCP channel. Degenerate login: validate creds,
	// mint access/refresh tokens, return the player list. Device-limit + the
	// login_session:* write happen the first time the client hits the gate
	// (via the legacy path's EnterGame → bindSession flow).
	if !isLegacyPath {
		userAccount, err := GetOrInitUserAccount(l.ctx, l.svcCtx.RedisClient, account, config.AppConfig.Account.CacheExpire)
		if err != nil {
			return nil, err
		}

		authType := in.AuthType
		if authType == "" {
			authType = "password"
		}
		if authType != "access_token" {
			tokenPair, tokenErr := l.issueTokens(account, authType)
			if tokenErr != nil {
				logx.Errorf("Failed to issue tokens for account=%s: %v", account, tokenErr)
				// Non-fatal: client just won't get tokens for silent re-login
			} else {
				resp.AccessToken = tokenPair.AccessToken
				resp.RefreshToken = tokenPair.RefreshToken
				resp.AccessTokenExpire = tokenPair.AccessTokenExpire
				resp.RefreshTokenExpire = tokenPair.RefreshTokenExpire
			}
		}

		resp.Players = l.roleListWithCurrentHomeZone(userAccount)
		return resp, nil
	}

	if !sessionFound || sessionDetails.SessionId <= 0 {
		logx.Error("SessionId not found in context during login")
		resp.ErrorMessage = &login_proto_common.TipInfoMessage{Id: uint32(table.LoginError_kLoginSessionIdNotFound)}
		return resp, nil
	}

	logx.Infof("Login start: account=%s sessionId=%d", account, sessionDetails.SessionId)

	// 3. Save login session (stores account for this session in Redis)
	if err := loginsession.Save(l.ctx, l.svcCtx.RedisClient, sessionDetails.SessionId, account); err != nil {
		logx.Errorf("Login save failed: sessionId=%d account=%s err=%v", sessionDetails.SessionId, account, err)
		resp.ErrorMessage = &login_proto_common.TipInfoMessage{Id: uint32(table.LoginError_kLoginRedisSetFailed)}
		return resp, nil
	}

	// 4. Enforce device limit
	sessionKey := constants.GenerateSessionKey(account)
	expire := time.Duration(config.AppConfig.Node.SessionExpireMin) * time.Minute

	// Self-heal: drop any device-set entries whose login_session:{id} key has
	// already been cleaned up. This absorbs the Gate→login.Disconnect race
	// that would otherwise trip MaxDevicesPerAccount under rapid reconnect.
	loginsession.PruneStaleFromDeviceSet(l.ctx, l.svcCtx.RedisClient, sessionKey)

	_, err = l.svcCtx.RedisClient.TxPipelined(l.ctx, func(pipe redis.Pipeliner) error {
		pipe.SAdd(l.ctx, sessionKey, sessionDetails.SessionId)
		pipe.Expire(l.ctx, sessionKey, expire)
		return nil
	})
	if err != nil {
		logx.Errorf("Device set pipeline failed: sessionKey=%s err=%v", sessionKey, err)
		resp.ErrorMessage = &login_proto_common.TipInfoMessage{Id: uint32(table.LoginError_kLoginRedisSetFailed)}
		return resp, nil
	}

	count, err := l.svcCtx.RedisClient.SCard(l.ctx, sessionKey).Result()
	if err != nil {
		logx.Errorf("SCard error: %v", err)
		resp.ErrorMessage = &login_proto_common.TipInfoMessage{Id: uint32(table.LoginError_kLoginRedisSetFailed)}
		return resp, nil
	}
	if count > config.AppConfig.Account.MaxDevicesPerAccount {
		logx.Infof("Account %s exceeds device limit: %d > %d", account, count, config.AppConfig.Account.MaxDevicesPerAccount)
		resp.ErrorMessage = &login_proto_common.TipInfoMessage{Id: uint32(table.LoginError_kTooManyDevices)}
		return resp, nil
	}

	// 5. Load account data
	userAccount, err := GetOrInitUserAccount(l.ctx, l.svcCtx.RedisClient, account, config.AppConfig.Account.CacheExpire)
	if err != nil {
		return nil, err
	}

	// 6. Issue access/refresh tokens (skip for access_token re-auth to avoid token churn)
	authType := in.AuthType
	if authType == "" {
		authType = "password"
	}
	if authType != "access_token" {
		tokenPair, tokenErr := l.issueTokens(account, authType)
		if tokenErr != nil {
			logx.Errorf("Failed to issue tokens for account=%s: %v", account, tokenErr)
			// Non-fatal: login succeeds, client just won't have tokens for reconnect
		} else {
			resp.AccessToken = tokenPair.AccessToken
			resp.RefreshToken = tokenPair.RefreshToken
			resp.AccessTokenExpire = tokenPair.AccessTokenExpire
			resp.RefreshTokenExpire = tokenPair.RefreshTokenExpire
		}
	}

	// 7. Return player list
	resp.Players = l.roleListWithCurrentHomeZone(userAccount)

	return resp, nil
}

// roleListWithCurrentHomeZone 把账号 blob 里的角色列表转成响应,并用 data_service
// 的 player:zone 映射覆盖每个角色的 zone_id。
//
// 账号 blob 里的 zone_id 只是建角时盖的章(createplayerlogic.go),合服只改映射
// 不改 blob,所以直接返回它会把玩家引向已下线的源 zone。这里刻意**不把刷新后的
// zone 回写 blob**:映射是唯一真源,回写等于制造第二份真相,再次合服 / 回滚时两
// 份必然打架。映射查不到 / RPC 失败 / 开关关闭 → 原样返回建角 zone,登录不受影响。
func (l *LoginLogic) roleListWithCurrentHomeZone(userAccount *login_proto_data_base.UserAccounts) []*login_proto.AccountSimplePlayerWrapper {
	players := userAccount.GetSimplePlayers().GetPlayers()
	return buildRoleList(l.ctx, l.svcCtx.HomeZone, config.AppConfig.HomeZone.RefreshRoleListDisabled, players)
}

// buildRoleList 是 roleListWithCurrentHomeZone 去掉 ServiceContext 依赖的纯函数版,
// 单测直接打它。resolver 为 nil 或 disabled=true 时原样透传(不复制、不查询)。
func buildRoleList(ctx context.Context, resolver *homezone.Resolver, disabled bool,
	players []*login_proto_common.AccountSimplePlayer) []*login_proto.AccountSimplePlayerWrapper {
	if len(players) == 0 {
		return nil
	}
	if resolver != nil && !disabled {
		players = resolver.RefreshRoleZones(ctx, players)
	}
	out := make([]*login_proto.AccountSimplePlayerWrapper, 0, len(players))
	for _, v := range players {
		out = append(out, &login_proto.AccountSimplePlayerWrapper{Player: v})
	}
	return out
}

func GetOrInitUserAccount(ctx context.Context, rdb *redis.Client, account string, ttl time.Duration) (*login_proto_data_base.UserAccounts, error) {
	key := constants.GetAccountDataKey(account)

	// Try Redis first
	cmd := rdb.Get(ctx, key)
	valueBytes, err := cmd.Bytes()

	if errors.Is(err, redis.Nil) {
		// Not in Redis; create empty default
		logx.Infof("UserAccounts not found for account=%s, initializing default", account)
		userAccount := &login_proto_data_base.UserAccounts{}

		valueBytes, err = proto.Marshal(userAccount)
		if err != nil {
			logx.Errorf("Marshal default UserAccounts failed: %v", err)
			return nil, err
		}

		// Save to Redis
		err = rdb.Set(ctx, key, valueBytes, ttl).Err()
		if err != nil {
			logx.Errorf("Failed to save default UserAccounts to Redis for account=%s: %v", account, err)
			return nil, err
		}

		return userAccount, nil
	}

	if err != nil {
		logx.Errorf("Failed to get UserAccounts from Redis: %v", err)
		return nil, err
	}

	// Deserialize and return
	userAccount := &login_proto_data_base.UserAccounts{}
	if err := proto.Unmarshal(valueBytes, userAccount); err != nil {
		logx.Errorf("Unmarshal user account failed for account=%s: %v", account, err)
		return nil, err
	}

	return userAccount, nil
}

// resolveAccount determines the account identifier based on the auth type.
// Empty auth_type is the legacy spelling of "password" and therefore follows
// the same fail-closed path. Third-party auth_token values use the registry.
func (l *LoginLogic) resolveAccount(in *login_proto.LoginRequest) (string, error) {
	authType := in.AuthType
	if authType == "" {
		authType = "password"
	}
	if authType == "password" {
		return resolvePasswordAccount(l.ctx, in, auth.GetPassword())
	}

	provider := auth.Get(authType)
	if provider == nil {
		return "", fmt.Errorf("unknown auth type: %s", authType)
	}

	result, err := provider.Validate(l.ctx, in.AuthToken)
	if err != nil {
		return "", err
	}
	return result.Account, nil
}

func resolvePasswordAccount(ctx context.Context, in *login_proto.LoginRequest, provider auth.PasswordAuthenticator) (string, error) {
	if provider == nil {
		return "", auth.ErrPasswordAuthDisabled
	}
	result, err := provider.ValidatePassword(ctx, in.Account, in.Password)
	if err != nil {
		return "", err
	}
	if result == nil || result.Account == "" {
		return "", auth.ErrInvalidCredentials
	}
	return result.Account, nil
}

// issueTokens generates an access/refresh token pair for the authenticated account.
func (l *LoginLogic) issueTokens(account, authType string) (*token.TokenPair, error) {
	return l.svcCtx.TokenManager.Issue(l.ctx, account, authType, "")
}
