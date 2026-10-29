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
	"login/internal/logic/pkg/playernamereg"
	"login/internal/logic/pkg/sessionfence"
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

	// 入口分类见 classifyLoginPath:不带会话 = Java Gateway POST /api/login 直调;带会话 = 经
	// cpp gate HandleGrpcNodeMessage 转发。gate 上 auth_type=access_token 是新流程
	// (/api/login → assign-gate → gate Login)绑定会话的必经一步,不是弃用路径;只有在 gate 上
	// 用口令 / 第三方凭据首认证才算 legacy。分类只看会话头与 auth_type,认证之前就能确定,
	// 所以分类、弃用遥测(限频的弃用日志 + login_auth_path_total)与 kill switch 都放在认证之前。
	// 遥测因此计的是请求数(含认证失败、抢锁失败与被拒的请求),auth_type 先收敛再做 label(见 authTypeMetricLabel)。
	sessionDetails, sessionFound := ctxkeys.GetSessionDetails(l.ctx)
	path := classifyLoginPath(sessionDetails, sessionFound, in.AuthType)
	warnLegacyLoginCaller(path, in.AuthType)

	// ARCH §12 T+2:运维确认 legacy 计数低于约定阈值后,在 login.yaml 把 LegacyGateLoginEnabled 置 false。
	// 此后 gate 上的凭据首认证一律回通用登录错误,剩下的老客户端按常规重试改走 /api/login。
	// gate 上的 access_token 绑定不受影响:它是 login_session 与设备集的唯一写入点,拦了它
	// CreatePlayer / EnterGame 全部读不到 login_session(kLoginSessionNotFound),所有客户端都进不了游戏。
	//
	// 拒绝发生在认证之前:被拒的请求不调用认证器(第三方 code 不被消费,客户端改走 /api/login 时还能用;
	// 口令不被校验,也不占口令配额)、不抢账号锁、不建账号目录、不写 login_session / 设备集,开关翻回去无需清理。
	// 代价是凭据真假都回同一个码(不再先回 kLoginAccountNotFound),对旧客户端都是「登录失败,请重试」。
	// 此时账号还没解析,日志只记收敛后的 auth_type 与 session_id,不记客户端送来的未认证账号。
	//
	// 刻意沿用 kLoginUnknownError 不新开码:新码要改 Tip.xlsx、重新导表、三端重编,对一个客户端 SDK
	// 已经用通用「登录失败,请重试」处理的 kill switch 路径不值得;弃用遥测显示尾巴不收敛时再随下一轮导表加专用码。
	if shouldRejectLegacyRequest(path, config.AppConfig.LegacyGateLoginEnabled) {
		logx.Errorf("[DEPRECATION] legacy gate credential Login disabled by config; "+
			"rejecting before authentication auth_type=%s session_id=%d. "+
			"Client should migrate to POST /api/login on Java Gateway.",
			authTypeMetricLabel(in.AuthType), sessionDetails.SessionId)
		resp.ErrorMessage = &login_proto_common.TipInfoMessage{Id: uint32(table.LoginError_kLoginUnknownError)}
		return resp, nil
	}

	// Resolve account via auth provider
	account, err := l.resolveAccount(in)
	if err != nil {
		tip := authFailureTip(err)
		logx.Errorf("Auth failed: type=%s tip=%d err=%v", in.AuthType, tip, err)
		resp.ErrorMessage = &login_proto_common.TipInfoMessage{Id: tip}
		return resp, nil
	}

	// 1. Distributed lock (UUID + Lua safe release)
	accountLock, err := locker.NewRedisLocker(l.svcCtx.RedisClient).TryLock(
		l.ctx, accountLoginLockKey(account),
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

	// New path (Java Gateway HTTP): no session exists yet because the player
	// hasn't touched the gate TCP channel. Degenerate login: validate creds,
	// mint access/refresh tokens, return the player list. Device-limit + the
	// login_session:* write happen when the client then runs Login with
	// auth_type="access_token" on the gate (loginPathGateBind, below).
	if path == loginPathHTTP {
		userAccount, err := readAccountDirectory(l.ctx, l.svcCtx, account, true)
		if err != nil {
			resp.ErrorMessage = &login_proto_common.TipInfoMessage{Id: accountDirectoryFailureTip(err)}
			return resp, nil
		}

		authType := in.AuthType
		if authType == "" {
			authType = "password"
		}
		if authType != "access_token" {
			// 这条路径上 token 就是登录的全部产出:客户端接着要拿 access_token 去 gate 上 Login 绑定会话。
			// 签发失败时不能回「成功但没有 token」——第三方 code 已被消费,客户端只会拿着空 token 卡在下一步。
			// 回故障码,让客户端重新走一遍登录(渠道凭证要重新取)。
			tokenPair, tokenErr := l.issueTokens(account, authType)
			if tokenErr != nil {
				logx.Errorf("Failed to issue tokens for account=%s: %v", account, tokenErr)
				resp.ErrorMessage = &login_proto_common.TipInfoMessage{Id: uint32(table.LoginError_kLoginTokenIssueFailed)}
				return resp, nil
			}
			resp.AccessToken = tokenPair.AccessToken
			resp.RefreshToken = tokenPair.RefreshToken
			resp.AccessTokenExpire = tokenPair.AccessTokenExpire
			resp.RefreshTokenExpire = tokenPair.RefreshTokenExpire
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

	sessionKey := constants.GenerateSessionKey(account)
	expire := time.Duration(config.AppConfig.Node.SessionExpireMin) * time.Minute
	// 登录态按 gate 连接建键(gate_instance_id + session_incarnation),不再只按 session_id:
	// gate 重启后序号从 0 开始,只按 session_id 会让新连接继承旧进程留下的登录态(loginsession 包注释)。
	sessionRef := loginsession.RefOf(sessionDetails)

	if fenceKey, fenced := sessionfence.KeyOf(sessionDetails); fenced {
		// 3+4. gate 连接会话栅栏(docs/design/client-request-ordering.md §6.2):「连接未关闭」的检查与
		// 写 login_session、登记设备集放在同一段脚本里。gate 合成的 Disconnect 可能抢在这条仍在途的 Login 之前执行,
		// 原先 Login 晚落地会留下没人清理的 login_session 与设备集条目(最长 SessionExpireMin),还可能误触设备数上限。
		//
		// Self-heal 照旧在提交前做:剔除 login_session 已不存在的设备集条目。
		loginsession.PruneStaleFromDeviceSet(l.ctx, l.svcCtx.RedisClient, sessionKey)
		if err := sessionfence.CommitLogin(l.ctx, l.svcCtx.RedisClient, fenceKey,
			sessionRef.WriteKeys(), account, loginsession.TTL(),
			sessionKey, sessionRef.DeviceMember(), expire); err != nil {
			if errors.Is(err, sessionfence.ErrClosed) {
				// 连接已断开,回包也送不到;什么都没写。
				logx.Infof("Login skipped: gate connection already closed sessionId=%d account=%s", sessionDetails.SessionId, account)
				resp.ErrorMessage = &login_proto_common.TipInfoMessage{Id: uint32(table.LoginError_kLoginSessionNotFound)}
				return resp, nil
			}
			logx.Errorf("Login commit failed: sessionId=%d account=%s err=%v", sessionDetails.SessionId, account, err)
			resp.ErrorMessage = &login_proto_common.TipInfoMessage{Id: uint32(table.LoginError_kLoginRedisSetFailed)}
			return resp, nil
		}
	} else {
		// 旧版 gate(没有会话代次):沿用原先的两步写,登录态仍按 session_id 建键(计入 loginsession_legacy_key_total)。
		// 3. Save login session (stores account for this session in Redis)
		if err := loginsession.Save(l.ctx, l.svcCtx.RedisClient, sessionRef, account); err != nil {
			logx.Errorf("Login save failed: sessionId=%d account=%s err=%v", sessionDetails.SessionId, account, err)
			resp.ErrorMessage = &login_proto_common.TipInfoMessage{Id: uint32(table.LoginError_kLoginRedisSetFailed)}
			return resp, nil
		}

		// 4. Enforce device limit
		// Self-heal: drop any device-set entries whose login_session:{id} key has
		// already been cleaned up. This absorbs the Gate→login.Disconnect race
		// that would otherwise trip MaxDevicesPerAccount under rapid reconnect.
		loginsession.PruneStaleFromDeviceSet(l.ctx, l.svcCtx.RedisClient, sessionKey)

		_, err = l.svcCtx.RedisClient.TxPipelined(l.ctx, func(pipe redis.Pipeliner) error {
			pipe.SAdd(l.ctx, sessionKey, sessionRef.DeviceMember())
			pipe.Expire(l.ctx, sessionKey, expire)
			return nil
		})
		if err != nil {
			logx.Errorf("Device set pipeline failed: sessionKey=%s err=%v", sessionKey, err)
			resp.ErrorMessage = &login_proto_common.TipInfoMessage{Id: uint32(table.LoginError_kLoginRedisSetFailed)}
			return resp, nil
		}
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
	userAccount, err := readAccountDirectory(l.ctx, l.svcCtx, account, true)
	if err != nil {
		resp.ErrorMessage = &login_proto_common.TipInfoMessage{Id: accountDirectoryFailureTip(err)}
		return resp, nil
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
			// Non-fatal(与上面 HTTP 分支刻意不同):gate 上的会话此时已经绑定,登录本身成功了,
			// token 只用于之后的静默重连;这里回失败反而让「会话已绑定、客户端以为失败」两边不一致。
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

// roleListWithCurrentHomeZone 把 SQL 目录角色列表转成响应,并用 data_service
// 的 player:zone 映射覆盖每个角色的 zone_id。
//
// 目录角色里的 zone_id 只是建角时盖的章(createplayerlogic.go),合服只改映射
// 不改目录副本,所以直接返回它会把玩家引向已下线的源 zone。这里刻意**不把刷新后的
// zone 回写目录**:映射是唯一真源,回写等于制造第二份真相,再次合服 / 回滚时两
// 份必然打架。映射查不到 / RPC 失败 / 开关关闭 → 原样返回建角 zone,登录不受影响。
//
// 缺名的旧角色随后由 fillMissingRoleNames 回源名字注册表补上,
// 同样只补在返回的那一份上、不回写目录。
//
// 三段补齐**串行**,各自独立计时、不共享一份预算,失败都放行:归属(HomeZone.RoleListLookupTimeout,缺省 500ms)
// → 缺外观的角色整份读核心 Redis 的 PlayerAllData(fillMissingRoleAppearances 写死 1s)→ 缺名的角色回源名字
// 注册表(同用 RoleListLookupTimeout)。1s 与名字那段只约束 ctx,单次 socket 读还受 Node.RedisClient.ReadTimeout
// 约束,所以核心 Redis 变慢时最坏会超过三段之和。调用之前 Login 已做完一次不能失败的账号目录读
// (readAccountDirectory,最长 2.5s),整个 Login 受服务端 Timeout 约束:调大任一段之前先核总和。
func (l *LoginLogic) roleListWithCurrentHomeZone(userAccount *login_proto_data_base.UserAccounts) []*login_proto.AccountSimplePlayerWrapper {
	players := userAccount.GetSimplePlayers().GetPlayers()
	roles := buildRoleList(l.ctx, l.svcCtx.HomeZone, config.AppConfig.HomeZone.RefreshRoleListDisabled, players)
	fillMissingRoleAppearances(l.ctx, l.svcCtx.RedisClient, roles)
	fillMissingRoleNames(l.ctx, l.svcCtx.PlayerNames, roles, config.AppConfig.HomeZone.RoleListLookupTimeout)
	return roles
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

// roleNameLookup 是读侧回源名字注册表(data_service BatchGetPlayerName)所需的最小接口,
// *playernamereg.Client 实现它;角色列表与 EnterGame(resolveEnterName)共用,单测用假实现替换。
// 返回的 map 只含注册表里存在的 id。
type roleNameLookup interface {
	Lookup(ctx context.Context, ids []uint64) (map[uint64]string, error)
}

// fillMissingRoleNames 给角色列表里 Name 为空的角色回源补名,原地改 roles 的元素。
//
// 目录角色里的名字副本可能缺失(早于名字功能的
// 旧角色),直接下发就是一行空白。全部有名时不发 RPC —— 这是绝大多数登录;有缺名的才
// 一次 Lookup 查完,预算 timeout(<=0 用 playernamereg.DefaultLookupTimeout)。
//
// 名字是展示数据,读侧 fail-open:Lookup 报错 / 超时 / 注册表未配置 → 只记 Info,列表原样
// 返回,登录不受影响。命中的角色先 proto.Clone 再写 Name:buildRoleList 在 resolver 为 nil
// 或开关关闭时透传的是账号对象本身的指针,原地写会把回源结果带进之后任何一次
// Marshal(userAccount);与 zone 刷新同一条纪律 —— 注册表是唯一真源,不回写目录。
//
// names 为 nil 接口时直接返回;*playernamereg.Client 的 nil 指针装进接口后**不等于**
// nil 接口,那种情况由 Client.Lookup 的 nil 接收者分支返回 ErrUnavailable,走错误分支。
func fillMissingRoleNames(ctx context.Context, names roleNameLookup,
	roles []*login_proto.AccountSimplePlayerWrapper, timeout time.Duration) {
	ids := make([]uint64, 0, len(roles))
	for _, w := range roles {
		if p := w.GetPlayer(); p != nil && p.GetName() == "" {
			ids = append(ids, p.GetPlayerId())
		}
	}
	if len(ids) == 0 || names == nil {
		return
	}

	if timeout <= 0 {
		timeout = playernamereg.DefaultLookupTimeout
	}
	lookupCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	found, err := names.Lookup(lookupCtx, ids)
	if err != nil {
		// 每个请求最多一条:这里不会循环打日志。
		logx.WithContext(ctx).Infof("[role-name] Character list has %d unnamed characters not backfilled this time: failed to query name registry: %v",
			len(ids), err)
		return
	}

	for _, w := range roles {
		p := w.GetPlayer()
		if p == nil || p.GetName() != "" {
			continue
		}
		name := found[p.GetPlayerId()]
		if name == "" {
			continue
		}
		named := proto.Clone(p).(*login_proto_common.AccountSimplePlayer)
		named.Name = name
		w.Player = named
	}
}

// errAccountDirectoryCorrupt 标记账号目录记录存在但解析不了(数据损坏)，与 SQL/RPC 依赖故障区分开。
var errAccountDirectoryCorrupt = errors.New("account character directory is corrupt")

// accountDirectoryFailureTip 把账号目录读取失败映射成 in-band 故障码(Tip.xlsx 的 fault 列,serverbase 据此告警)。
// 以前直接 return err:gRPC 层变成 UNKNOWN,Java 网关把它当鉴权拒绝回 401,玩家被要求重走 OAuth,
// 而 serverbase 看不到这条 in-band 故障。
func accountDirectoryFailureTip(err error) uint32 {
	if errors.Is(err, errAccountDirectoryCorrupt) {
		return uint32(table.LoginError_kLoginDataParseFailed)
	}
	return uint32(table.LoginError_kLoginDataSerializeFailed)
}

// authFailureTip 把认证失败映射成回给客户端的码:渠道或本服务渠道配置侧的故障(auth.ErrProviderUnavailable:
// 网络 / 超时、渠道非 2xx、AppSecret 无效、拿不到必需的 unionid 等)回故障码 kLoginAuthProviderUnavailable;
// 其余(凭证无效、未知 auth_type、口令认证关闭)仍回 kLoginAccountNotFound,不向调用方区分原因。
// 口令认证从不包 ErrProviderUnavailable:它统一回 2000 是有意的防账号枚举(auth-provider-framework.md 2026-10-15 节)。
func authFailureTip(err error) uint32 {
	if errors.Is(err, auth.ErrProviderUnavailable) {
		return uint32(table.LoginError_kLoginAuthProviderUnavailable)
	}
	return uint32(table.LoginError_kLoginAccountNotFound)
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

// accountLoginLockKey 是 Login 认证之后按账号互斥的分布式锁 key。单独成函数是为了让测试按同一个 key
// 断言「被 kill switch 拒绝的请求不抢账号锁」,key 改名时断言跟着走,不会悄悄变成空转。
func accountLoginLockKey(account string) string {
	return "account_lock:login:" + account
}

// issueTokens generates an access/refresh token pair for the authenticated account.
func (l *LoginLogic) issueTokens(account, authType string) (*token.TokenPair, error) {
	return l.svcCtx.TokenManager.Issue(l.ctx, account, authType, "")
}
