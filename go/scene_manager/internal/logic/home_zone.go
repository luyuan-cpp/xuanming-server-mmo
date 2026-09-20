package logic

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	dspb "proto/data_service"
	"proto/scene_manager"
	"scene_manager/internal/constants"
	"scene_manager/internal/metrics"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ---------------------------------------------------------------------------
// 归属 zone(home_zone)—— EnterScene 在发 RoutePlayerEvent 之前查一次
//
// 为什么由 scene_manager 查、随路由事件下发,而不是让 C++ 节点自己去问
// data_service:节点多一个同步依赖不说,两次改派挨得近时它会读到后一次的值
// (与 owner_epoch 必须随事件走是同一个理由)。data_service 的 player:zone 映射
// 是归属的唯一真源(login 也这么取),见 cross-zone-scene-travel.md CZ-3。
//
// 结果语义(与 login 的 homezone 包对齐):
//   - 映射存在                → 用它;
//   - 映射里没有(NotFound / 老版 Unknown + 文案)→ 首登,gate zone 即 home zone,INFO;
//   - 传输层故障 / 超时       → **拒绝**并让上游重试。归属决定存盘落哪个库,未知时
//                               不得静默落进程 zone 库(§6 不变量 2);login 那边
//                               可以"保留存量值不阻断登录",这里不行 —— 这里的
//                               后果是写错库;
//   - 本进程没配 DataServiceRpc → 本地单区联调,按 gate zone 处理,WARN 一次 + 计数。
// ---------------------------------------------------------------------------

const (
	// defaultHomeZoneLookupTimeout 与 login 的 EnterLookupTimeout(1.5s)对齐;
	// 配置 HomeZoneLookupTimeoutMs <= 0 时用它。
	defaultHomeZoneLookupTimeout = 1500 * time.Millisecond

	// homeZoneUnmappedErrText 是老版 data_service 对缺失映射返回的 Unknown 错误文案
	// 片段(go/data_service/internal/routing/router.go ErrHomeZoneNotMapped)。新版
	// 返回 codes.NotFound 且消息仍含这段文案;两者都要认,滚动升级期间两种并存。
	homeZoneUnmappedErrText = "no home zone mapping"
)

// homeZoneUnconfiguredWarnOnce 让「没配 DataServiceRpc」只在进程生命周期里报一次:
// 它是部署形态而不是每个请求的事件,每次进场景都打一条只会淹没别的日志。
// 计数(metrics)照样每次记,多 zone 部署漏配要能在仪表盘上看出来。
var homeZoneUnconfiguredWarnOnce sync.Once

// isHomeZoneUnmapped 判断 GetPlayerHomeZone 的错误是否只是「映射里没有这个玩家」。
func isHomeZoneUnmapped(err error) bool {
	if err == nil {
		return false
	}
	if st, ok := status.FromError(err); ok {
		if st.Code() == codes.NotFound {
			return true
		}
		return strings.Contains(st.Message(), homeZoneUnmappedErrText)
	}
	return strings.Contains(err.Error(), homeZoneUnmappedErrText)
}

// resolveHomeZone 返回要写进 RoutePlayerEvent.home_zone_id 的归属 zone。
// 第二个返回值非 nil 表示本次 EnterScene 必须以它作为响应拒绝(可重试)。
func (l *EnterSceneLogic) resolveHomeZone(in *scene_manager.EnterSceneRequest) (uint32, *scene_manager.EnterSceneResponse) {
	if l.svcCtx.HomeZone == nil {
		metrics.ObserveHomeZoneLookup(in.GateZoneId, metrics.HomeZoneLookupUnconfigured)
		// 没配 DataServiceRpc 时「按 gate zone 当归属」只对单 zone 成立。多 zone 下访客
		// 的 gate zone ≠ home zone,存盘会写进目标 zone 的库,而且除了一条日志零报错
		// (不变量 2)。所以默认 fail-closed,只有部署方显式声明自己是单 zone
		// (AllowGateZoneAsHomeZone)才退回 gate zone。
		if !l.svcCtx.Config.AllowGateZoneAsHomeZone {
			homeZoneUnconfiguredWarnOnce.Do(func() {
				l.Logger.Errorf("[home-zone] DataServiceRpc 未配置且 AllowGateZoneAsHomeZone=false:拒绝所有进场景请求。"+
					"多 zone 部署必须给 scene_manager 配 DataServiceRpc(etcd Key=dataservice.rpc);"+
					"单 zone 联调可显式置 AllowGateZoneAsHomeZone: true。player=%d gate_zone=%d",
					in.PlayerId, in.GateZoneId)
			})
			metrics.ObserveEnterSceneRejected(in.GateZoneId, "home_zone_unavailable")
			return 0, errResp(constants.ErrHomeZoneUnavailable,
				"scene_manager 未配置 DataServiceRpc,无法确定玩家归属 zone;已拒绝且未修改玩家状态")
		}
		homeZoneUnconfiguredWarnOnce.Do(func() {
			l.Logger.Errorf("[home-zone] WARN: DataServiceRpc 未配置,按 AllowGateZoneAsHomeZone=true 把 gate zone 当归属 zone "+
				"(只对单 zone 部署正确): player=%d gate_zone=%d", in.PlayerId, in.GateZoneId)
		})
		return in.GateZoneId, nil
	}

	timeout := time.Duration(l.svcCtx.Config.HomeZoneLookupTimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = defaultHomeZoneLookupTimeout
	}
	ctx, cancel := context.WithTimeout(l.ctx, timeout)
	defer cancel()

	resp, err := l.svcCtx.HomeZone.GetPlayerHomeZone(ctx, &dspb.GetPlayerHomeZoneRequest{PlayerId: in.PlayerId})
	if err != nil {
		if isHomeZoneUnmapped(err) {
			l.Logger.Infof("[home-zone] player=%d 尚无 player:zone 映射(首登),归属按 gate zone=%d",
				in.PlayerId, in.GateZoneId)
			metrics.ObserveHomeZoneLookup(in.GateZoneId, metrics.HomeZoneLookupUnmapped)
			return in.GateZoneId, nil
		}
		l.Logger.Errorf("[home-zone] GetPlayerHomeZone 失败,拒绝进场景让上游重试(归属未知不得落进程 zone 库): player=%d gate_zone=%d err=%v",
			in.PlayerId, in.GateZoneId, err)
		metrics.ObserveHomeZoneLookup(in.GateZoneId, metrics.HomeZoneLookupError)
		metrics.ObserveEnterSceneRejected(in.GateZoneId, "home_zone_unavailable")
		return 0, errResp(constants.ErrHomeZoneUnavailable,
			fmt.Sprintf("home zone lookup failed for player %d, retry later: %v", in.PlayerId, err))
	}
	home := resp.GetHomeZoneId()
	if home == 0 {
		// 新版契约里「没有映射」是 NotFound,不会走到这里;保留这一支只为兼容
		// 曾经用 HomeZoneId=0 + nil 表示缺席的服务端。
		l.Logger.Infof("[home-zone] player=%d 映射为 0(视作首登),归属按 gate zone=%d", in.PlayerId, in.GateZoneId)
		metrics.ObserveHomeZoneLookup(in.GateZoneId, metrics.HomeZoneLookupUnmapped)
		return in.GateZoneId, nil
	}
	metrics.ObserveHomeZoneLookup(in.GateZoneId, metrics.HomeZoneLookupMapped)
	return home, nil
}
