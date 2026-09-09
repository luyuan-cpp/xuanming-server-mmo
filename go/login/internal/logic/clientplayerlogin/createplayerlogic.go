package clientplayerloginlogic

import (
	"context"
	"errors"
	"login/internal/config"
	"login/internal/constants"
	"login/internal/logic/pkg/ctxkeys"
	"login/internal/logic/pkg/locker"
	"login/internal/logic/pkg/loginsession"
	"login/internal/svc"
	login_proto_common "proto/common/base"
	login_data_base "proto/common/database"
	login_proto "proto/login"
	gametable "shared/generated/table"
	"shared/generated/pb/table"
	"time"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"

	"github.com/zeromicro/go-zero/core/logx"
)

type CreatePlayerLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCreatePlayerLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreatePlayerLogic {
	return &CreatePlayerLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *CreatePlayerLogic) CreatePlayer(in *login_proto.CreatePlayerRequest) (*login_proto.CreatePlayerResponse, error) {
	resp := &login_proto.CreatePlayerResponse{
		Players: make([]*login_proto.AccountSimplePlayerWrapper, 0),
	}

	// 1. Get Session details
	sessionDetails, sessionFound := ctxkeys.GetSessionDetails(l.ctx)
	if !sessionFound || sessionDetails.SessionId <= 0 {
		logx.Error("SessionId not found or invalid during player creation")
		resp.ErrorMessage = &login_proto_common.TipInfoMessage{Id: uint32(table.LoginError_kLoginSessionIdNotFound)}
		return resp, nil
	}

	// 2. Get account
	account, err := loginsession.GetAccount(l.ctx, l.svcCtx.RedisClient, sessionDetails.SessionId)
	if err != nil {
		logx.Errorf("GetAccount failed: %v", err)
		resp.ErrorMessage = &login_proto_common.TipInfoMessage{Id: uint32(table.LoginError_kLoginSessionNotFound)}
		return resp, nil
	}

	// 3. Lock (UUID + Lua safe release — prevents concurrent character creation)
	createLock, err := locker.NewRedisLocker(l.svcCtx.RedisClient).TryLock(
		l.ctx, "account_lock:create:"+account,
		time.Duration(config.AppConfig.Locker.AccountLockTTL)*time.Second,
	)
	if err != nil || !createLock.IsLocked() {
		logx.Errorf("CreatePlayer lock acquire failed for account=%s: %v", account, err)
		resp.ErrorMessage = &login_proto_common.TipInfoMessage{Id: uint32(table.LoginError_kLoginInProgress)}
		return resp, nil
	}
	defer func() {
		if _, err := createLock.Release(l.ctx); err != nil {
			logx.Errorf("CreatePlayer lock release failed for account=%s: %v", account, err)
		}
	}()

	// 4. Load + decode account data
	accountDataKey := constants.GetAccountDataKey(account)
	dataBytes, err := l.svcCtx.RedisClient.Get(l.ctx, accountDataKey).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			logx.Infof("Account not found in redis: %s", account)
			resp.ErrorMessage = &login_proto_common.TipInfoMessage{Id: uint32(table.LoginError_kLoginAccountNotFound)}
			return resp, nil
		}
		resp.ErrorMessage = &login_proto_common.TipInfoMessage{Id: uint32(table.LoginError_kLoginRedisError)}
		logx.Errorf("RedisClient get failed, account: %s, err: %v", account, err)
		return resp, err
	}

	userAccount := &login_data_base.UserAccounts{}
	if err := proto.Unmarshal(dataBytes, userAccount); err != nil {
		logx.Errorf("Failed to unmarshal user account, err: %v", err)
		resp.ErrorMessage = &login_proto_common.TipInfoMessage{Id: uint32(table.LoginError_kLoginDataParseFailed)}
		return resp, nil
	}
	if userAccount.SimplePlayers == nil {
		userAccount.SimplePlayers = &login_proto_common.AccountSimplePlayerList{Players: make([]*login_proto_common.AccountSimplePlayer, 0)}
	}

	// 6. Create character
	if len(userAccount.SimplePlayers.Players) >= config.AppConfig.Account.MaxPlayersPerAccount {
		resp.ErrorMessage = &login_proto_common.TipInfoMessage{Id: uint32(table.LoginError_kLoginAccountPlayerFull)}
		logx.Infof("Account player limit reached: %s", account)
		return resp, nil
	}

	// 6a. 职业/性别参数(class_id=0 / gender=0 兼容旧客户端与 robot:取配表第一个职业、默认男)
	classId := in.ClassId
	if classId == 0 {
		if rows := gametable.ClassTableManagerInstance.FindAll(); len(rows) > 0 {
			classId = rows[0].Id
		}
	} else if !gametable.ClassTableManagerInstance.Exists(classId) {
		resp.ErrorMessage = &login_proto_common.TipInfoMessage{Id: uint32(table.LoginError_kLoginUnknownError)}
		logx.Errorf("CreatePlayer rejected: class_id=%d not in Class table (account=%s)", classId, account)
		return resp, nil
	}
	gender := in.Gender
	if gender == 0 {
		gender = 1
	}
	if gender > 2 {
		resp.ErrorMessage = &login_proto_common.TipInfoMessage{Id: uint32(table.LoginError_kLoginUnknownError)}
		logx.Errorf("CreatePlayer rejected: gender=%d invalid (account=%s)", in.Gender, account)
		return resp, nil
	}
	// 6b. 铸 PlayerId:号段优先(设计稿 §6),是否回退 snowflake 由 IdSegment 配置决定。
	// 发号失败建角整体失败。绝不能吞掉错误再用 0 或自造 id —— snowflake 那条路上,
	// 那会与接管了同一 worker id 的进程发出逐位相同的 PlayerId,而 player_database
	// 的写路径是 INSERT ... ON DUPLICATE KEY UPDATE,撞号不报错而是静默串档。
	newPlayerId, tip := l.mintPlayerID(account)
	if tip != nil {
		resp.ErrorMessage = tip
		return resp, nil
	}
	// 6c. 登记 player:zone 归属映射,**fail-closed**:登记失败整体拒绝建角,一个字节都不落盘。
	// 这条映射是数据路由 / 合服重映射 / 公会 / 榜的归属权威,而生产侧**只有这一个写入点**
	// (其余写入者只有 data_service 的 debug_import 与合服重映射)。先落账号 blob 再登记失败,
	// 造出的是「账号里有、映射里没有」的玩家:tools/merge_zone 的重映射按映射扫,扫不到他,
	// 合服后他的数据就烂在已下线的源 zone —— 这正是修复前线上的状态(映射零写入,
	// 合服重映射找到 0 个玩家)。建角失败玩家重试一次就好,代价远小于一个静默错路由的角色。
	// 顺序也因此固定:先登记(幂等、可重试、无本地副作用),后落盘。
	// HomeZone 为 nil / DataServiceRpc 未配置时 RegisterPlayerZone 返回 ErrUnavailable,
	// 同样走这条拒绝路径 —— 接线缺失在启动时已有 ERROR 日志(svc/home_zone.go)。
	if tip := l.registerHomeZone(account, newPlayerId); tip != nil {
		resp.ErrorMessage = tip
		return resp, nil
	}
	newPlayer := &login_proto_common.AccountSimplePlayer{
		PlayerId: newPlayerId,
		ClassId:  classId,
		Gender:   gender,
		ZoneId:   config.AppConfig.Node.ZoneId,
	}
	userAccount.SimplePlayers.Players = append(userAccount.SimplePlayers.Players, newPlayer)

	// 7. Write back to Redis
	dataBytes, err = proto.Marshal(userAccount)
	if err != nil {
		resp.ErrorMessage = &login_proto_common.TipInfoMessage{Id: uint32(table.LoginError_kLoginDataSerializeFailed)}
		logx.Errorf("Failed to marshal user account, err: %v", err)
		return resp, nil
	}
	if err := l.svcCtx.RedisClient.Set(l.ctx, accountDataKey, dataBytes, config.AppConfig.Account.CacheExpire).Err(); err != nil {
		resp.ErrorMessage = &login_proto_common.TipInfoMessage{Id: uint32(table.LoginError_kLoginRedisSetFailed)}
		logx.Errorf("Failed to set user account in RedisClient, account: %s, err: %v", account, err)
		return resp, nil
	}

	// 7b. Write reverse mapping: player_id → account (for rollback orphan cleanup)
	reverseKey := constants.PlayerToAccountKey(newPlayerId)
	if err := l.svcCtx.RedisClient.Set(l.ctx, reverseKey, account, 0).Err(); err != nil {
		logx.Errorf("Failed to set player-to-account reverse mapping, playerId: %d, account: %s, err: %v", newPlayerId, account, err)
		// Non-fatal: player can still play, only rollback cleanup is affected
	}

	// 8. Return player info
	for _, p := range userAccount.SimplePlayers.Players {
		resp.Players = append(resp.Players, &login_proto.AccountSimplePlayerWrapper{Player: p})
	}

	logx.Infof("Player created successfully, account: %s, playerId: %d", account, newPlayerId)
	return resp, nil
}

// mintPlayerID 是建角的发号步骤。单独抽出来只为可测:CreatePlayer 其余每一步都要 Redis。
//
// 失败返回的 tip 沿用发号器不可用时一直在用的 kLoginDataSerializeFailed —— 不另发新码:
// 客户端对它的处理(提示后重试)正是我们要的行为,而 Tip 码轴按段由导表器发号,
// 这里不手写数字。
func (l *CreatePlayerLogic) mintPlayerID(account string) (uint64, *login_proto_common.TipInfoMessage) {
	minter := l.svcCtx.PlayerIDMinter
	if minter == nil {
		// 接线错误(NewServiceContext 一定会装上它),按不可用处理而不是 nil 解引用崩掉整个进程。
		logx.Errorf("Refusing to create player: PlayerIDMinter not wired (account=%s)", account)
		return 0, &login_proto_common.TipInfoMessage{Id: uint32(table.LoginError_kLoginDataSerializeFailed)}
	}
	id, err := minter.Mint(l.ctx)
	if err != nil {
		logx.Errorf("Refusing to create player: id generator unavailable (account=%s): %v", account, err)
		return 0, &login_proto_common.TipInfoMessage{Id: uint32(table.LoginError_kLoginDataSerializeFailed)}
	}
	return id, nil
}

// registerHomeZone 把新角色登记进 data_service 的 player:zone 映射(见调用点 6c 的理由)。
// 与 mintPlayerID 一样单独抽出来只为可测:CreatePlayer 其余每一步都要 Redis。
// 返回 nil 表示登记成功,可以继续落盘;返回 tip 表示必须拒绝建角。
//
// tip 复用发号失败那条 kLoginDataSerializeFailed:两者语义同档(「服务端此刻建不了角,
// 稍后重试」),客户端对它的处理正是我们要的;而 Tip 码轴按段由导表器发号,
// 这里不手写数字、也不为这一个分支去申请新码。
func (l *CreatePlayerLogic) registerHomeZone(account string, playerID uint64) *login_proto_common.TipInfoMessage {
	zone := config.AppConfig.Node.ZoneId
	if err := l.svcCtx.HomeZone.RegisterPlayerZone(l.ctx, playerID, zone); err != nil {
		logx.Errorf("Refusing to create player: player:zone registration failed "+
			"(account=%s player_id=%d zone=%d): %v", account, playerID, zone, err)
		return &login_proto_common.TipInfoMessage{Id: uint32(table.LoginError_kLoginDataSerializeFailed)}
	}
	logx.Infof("[home-zone] registered player:zone player_id=%d zone=%d (account=%s)", playerID, zone, account)
	return nil
}
