package svc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	componentpb "proto/common/component"
	dspb "proto/data_service"

	"shared/ownerepoch"

	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 本文件是 inbox 包三个外部依赖接口的生产实现:玩家存在校验、围栏上界读取、已落盘账本读取。
// 放在装配层而不是 inbox:inbox 只认接口(DIP),单测与集成测试注入假实现。

// unmappedErrText 是老版 data_service 对缺失映射返回 Unknown 时的消息片段;新版返回 NotFound,
// 且消息仍含这段文本。与 go/guild/internal/logic/home_zone.go 同一判据,两个 module 不互相 import,各留一份。
const unmappedErrText = "no home zone mapping"

// DataServicePlayers 用 data_service.GetPlayerHomeZone 实现 inbox.PlayerDirectory(设计 §6.2 第 2 步):
// 映射不存在 → (false, nil);查询失败 → err(fail-closed,不入箱);映射值为 0 同样视为不存在。
type DataServicePlayers struct {
	Client  dspb.DataServiceClient
	Timeout time.Duration
}

var errNoDataServiceClient = errors.New("player_inbox: data_service client is not wired")

// PlayerExists 实现 inbox.PlayerDirectory。
func (d *DataServicePlayers) PlayerExists(ctx context.Context, playerID uint64) (bool, error) {
	if d == nil || d.Client == nil {
		return false, errNoDataServiceClient
	}
	if d.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d.Timeout)
		defer cancel()
	}
	resp, err := d.Client.GetPlayerHomeZone(ctx, &dspb.GetPlayerHomeZoneRequest{PlayerId: playerID})
	if err != nil {
		if isHomeZoneUnmapped(err) {
			return false, nil
		}
		return false, fmt.Errorf("data_service.GetPlayerHomeZone player_id=%d: %w", playerID, err)
	}
	return resp.GetHomeZoneId() != 0, nil
}

func isHomeZoneUnmapped(err error) bool {
	st, ok := status.FromError(err)
	if !ok {
		return false
	}
	switch st.Code() {
	case codes.NotFound:
		return true
	case codes.Unknown:
		return strings.Contains(st.Message(), unmappedErrText)
	default:
		return false
	}
}

// RedisEpochs仅用于STALE诊断；正常消费依赖完整SQL原入场，Redis镜像不再提供授权上界。
// GET player:{id}:owner_epoch按shared/ownerepoch解析。
// 键不存在按 0 返回(ownerepoch.ParseEpoch 的约定);非十进制整数是错误(数据面事故,不折成 0)。
type RedisEpochs struct {
	Client redis.UniversalClient
}

// OwnerEpoch 实现 inbox.EpochReader。
func (r *RedisEpochs) OwnerEpoch(ctx context.Context, playerID uint64) (uint64, error) {
	if r == nil || r.Client == nil {
		return 0, errors.New("player_inbox: player Redis client is not wired")
	}
	raw, err := r.Client.Get(ctx, ownerepoch.OwnerEpochKey(playerID)).Result()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return ownerepoch.ParseEpoch(raw)
}

// DataServiceLedgers 用 data_service.GetPlayerInboxLedger 实现 inbox.LedgerReader(设计 §9.3)。
// 映射(与 assetop.DataServiceLedger 同口径):found=false → (nil, false, nil);found=true → 非 nil 账本
// (玩家从未消费过时是空消息);任何 gRPC 错误(含尚未实现该 RPC 的旧版 data_service 的 Unimplemented)→ err。
type DataServiceLedgers struct {
	Client  dspb.DataServiceClient
	Timeout time.Duration
}

// ReadInboxLedger 实现 inbox.LedgerReader。
func (d *DataServiceLedgers) ReadInboxLedger(ctx context.Context, playerID uint64) (*componentpb.PlayerInboxLedgerComp, bool, error) {
	if d == nil || d.Client == nil {
		return nil, false, errNoDataServiceClient
	}
	if d.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d.Timeout)
		defer cancel()
	}
	resp, err := d.Client.GetPlayerInboxLedger(ctx, &dspb.GetPlayerInboxLedgerRequest{PlayerId: playerID})
	if err != nil {
		return nil, false, fmt.Errorf("data_service.GetPlayerInboxLedger player_id=%d: %w", playerID, err)
	}
	if !resp.GetFound() {
		return nil, false, nil
	}
	if ledger := resp.GetLedger(); ledger != nil {
		return ledger, true, nil
	}
	return &componentpb.PlayerInboxLedgerComp{}, true, nil
}
