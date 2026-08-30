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
	// 失去 worker id 所有权后发号器被 fence,建角整体失败。
	// 绝不能吞掉错误再用 0 或自造 id —— 那会与接管了同一 worker id 的进程
	// 发出逐位相同的 PlayerId,而 player_database 上并没有唯一索引兜底。
	generatedID, err := l.svcCtx.SnowFlake.Generate()
	if err != nil {
		resp.ErrorMessage = &login_proto_common.TipInfoMessage{Id: uint32(table.LoginError_kLoginDataSerializeFailed)}
		logx.Errorf("Refusing to create player: id generator unavailable (account=%s): %v", account, err)
		return resp, nil
	}
	newPlayerId := uint64(generatedID)
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
