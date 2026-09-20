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
//   - 映射里没有(NotFound / 老版 Unknown + 文案)→ 看这次落点的性质(homeZoneUnmappedPolicy):
//       · 首次落点 / 同 zone 换图 → 首登或未回填的存量号,gate zone 即 home zone,INFO;
//       · 跨 zone 传送的任一条腿  → **拒绝**。第二条腿的 gate zone 是目标 zone,回落过去
//                                   就是把访客的存盘写进目标 zone 的库(§6 不变量 2),
//                                   而且除了一条 INFO 零报错;
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

	// enter_scene_rejected_total 的 reason:跨 zone 传送遇到没有 player:zone 映射的玩家。
	// 两条腿共用这一个取值,zone_id 与 home_zone_unavailable 一样是 gate zone —— 第一条腿上
	// 它是源 zone(玩家留在原地收到 tip),第二条腿上是目标 zone(玩家已被源 scene 销毁,
	// 落不了地,要等映射补上)。后者只在两条腿之间映射丢失、或第一条腿的前置检查上线之前
	// 留下的等待落点上出现。
	rejectReasonHomeZoneUnmappedTravel = "home_zone_unmapped_travel"
)

// homeZoneUnmappedPolicy 说明「映射里没有这个玩家」时 resolveHomeZone 该怎么办。
// 由调用方按这次落点的性质给出:resolveHomeZone 自己看不到位置记录。
type homeZoneUnmappedPolicy uint8

const (
	// homeZoneUnmappedFallsBackToGateZone:gate zone 就是玩家所在的 zone —— 没有任何位置记录的
	// 首次落点(首登),或已有同 zone 位置记录的换图 / 重连。未映射按 gate zone 处理:新建角色
	// 由 login 的 createplayer fail-closed 登记映射,走到这里的是未回填的存量号,他们一直就是
	// 这么存盘的,不能因为缺映射突然换不了图。
	homeZoneUnmappedFallsBackToGateZone homeZoneUnmappedPolicy = iota
	// homeZoneUnmappedRejectsTravel:这次请求是跨 zone 传送的一条腿(第一条腿的离区放行,或
	// 第二条腿在消费「等待落点」)。gate zone 不能代表归属,未映射一律拒绝。
	homeZoneUnmappedRejectsTravel
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
// 只读:不改任何 Redis 状态,调用方只需成对释放自己已经做过的预占。
//
// dev 旁路 AllowGateZoneAsHomeZone=true(没配 DataServiceRpc)不看 unmapped 策略,一律回
// gate zone。在跨 zone 传送的第二条腿上这等于把访客记成目标 zone 归属、存盘写进目标 zone
// 的库,所以它**只许单 zone 本地联调用**;双 zone 联调 / 传送验收必须配 DataServiceRpc。
func (l *EnterSceneLogic) resolveHomeZone(in *scene_manager.EnterSceneRequest, unmapped homeZoneUnmappedPolicy) (uint32, *scene_manager.EnterSceneResponse) {
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
			metrics.ObserveHomeZoneLookup(in.GateZoneId, metrics.HomeZoneLookupUnmapped)
			if unmapped == homeZoneUnmappedRejectsTravel {
				return 0, l.rejectUnmappedTravel(in)
			}
			l.Logger.Infof("[home-zone] player=%d 尚无 player:zone 映射(首登 / 未回填的存量号),归属按 gate zone=%d",
				in.PlayerId, in.GateZoneId)
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
		metrics.ObserveHomeZoneLookup(in.GateZoneId, metrics.HomeZoneLookupUnmapped)
		if unmapped == homeZoneUnmappedRejectsTravel {
			return 0, l.rejectUnmappedTravel(in)
		}
		l.Logger.Infof("[home-zone] player=%d 映射为 0(视作首登),归属按 gate zone=%d", in.PlayerId, in.GateZoneId)
		return in.GateZoneId, nil
	}
	metrics.ObserveHomeZoneLookup(in.GateZoneId, metrics.HomeZoneLookupMapped)
	return home, nil
}

// rejectUnmappedTravel 是「跨 zone 传送 + 未映射」的拒绝应答。错误码沿用 ErrHomeZoneUnavailable:
// 对上游是同一件事(归属未知,未改任何状态),源 scene 按失败应答解冻并回 tip。指标 reason 单列,
// 因为它不是 data_service 故障,退避重试不会自己好 —— 要运维补映射
// (tools/merge_zone 的 -backfill-home-zone)。
// 这条 ERROR 由玩家操作直接触发(未回填的存量号每点一次跨 zone 传送就一条),不是一次性事件;
// 量上来了看指标而不是日志:告警 SceneManagerHomeZoneUnmappedTravel(deploy/k8s/scene-manager-alerts.yaml)。
// 跨 zone 传送对存量号开放之前应先跑完回填。
func (l *EnterSceneLogic) rejectUnmappedTravel(in *scene_manager.EnterSceneRequest) *scene_manager.EnterSceneResponse {
	metrics.ObserveEnterSceneRejected(in.GateZoneId, rejectReasonHomeZoneUnmappedTravel)
	l.Logger.Errorf("[home-zone] 跨 zone 传送被拒:玩家没有 player:zone 映射,gate zone 不能代表归属"+
		"(回落过去会把访客存盘写进目标 zone 的库)。需先回填映射(merge_zone -backfill-home-zone): player=%d gate_zone=%d req_zone=%d",
		in.PlayerId, in.GateZoneId, in.ZoneId)
	return errResp(constants.ErrHomeZoneUnavailable,
		fmt.Sprintf("player %d has no home zone mapping; cross-zone travel rejected, player state untouched", in.PlayerId))
}
