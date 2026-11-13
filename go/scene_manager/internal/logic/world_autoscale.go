package logic

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"scene_manager/internal/config"
	"scene_manager/internal/metrics"
	"scene_manager/internal/svc"
	"shared/safego"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
)

// 大世界频道按人数自动扩缩容。
//
// 口径(见 docs/design/world-channel-autoscale.md):
//   - 人数 = `instance:{sceneId}:members` 的 SCARD,**按频道**算,不是按进程。
//   - 扩容:该地图**所有**频道都 >= ScaleOutPlayerThreshold 才加一个频道。
//     要求"所有"而不是"任一",是因为负载不均时(新频道空、老频道满)按
//     "任一"会连续扩出一堆空频道。
//   - 缩容:某频道 < ScaleInPlayerThreshold 时把它排空 —— 玩家被强制改派到
//     同图的其它频道(大世界)—— 然后销毁。
//   - 每个大世界地图**至少保留 1 个频道**,永远不会缩到 0。
//
// 为什么频道数要落在 Redis 而不是配置:
//
//	`initWorldScenesForZone` 会按"期望频道数"把缺的频道补齐,它在 fullSync 和
//	每次节点 PUT 事件时都会跑。如果期望值仍然直接读 `ChannelCountFor(confId)`
//	这个静态配置,自动缩容刚摘掉的频道会在几秒内被它重新建出来 —— 缩容根本
//	不成立。所以期望值下沉到 Redis,配置只作为**初始种子**,扩缩容改的是
//	Redis 里那份,两边共用同一个权威。
//
// 下面以 zone 为键的格式里,第一个 %s 是 sceneControlTag(zone)。
const (
	// WorldDesiredChannelsKeyFmt: hash confId -> 期望频道数。
	WorldDesiredChannelsKeyFmt = "world_channels:desired:%s"
	// SceneDrainingKeyFmt: 该频道正在排空(已从路由集合摘除,等玩家走干净)。
	// 带 TTL,防止进程在排空中途挂掉后留下永久标记。键是 scene:{ctl:<z>}:<id>:draining。
	SceneDrainingKeyFmt = "scene:%s:%d:draining"
	// WorldAutoscaleCooldownKeyFmt: 每个 (zone, confId) 的冷却窗口。
	// 扩完不马上缩、缩完不马上扩,避免在阈值边缘抖动。
	WorldAutoscaleCooldownKeyFmt = "world_channels:cooldown:%s:%d"
	// WorldDrainingSetKeyFmt: 正在排空的频道集合(SET of sceneId)。
	// 需要单独一个索引:排空的第一步就是把频道从 world_channels 摘掉,
	// 摘掉之后 GetAllWorldChannels 就再也看不到它,没有这个集合就没法
	// 在后续 tick 里找回来继续收敛。
	WorldDrainingSetKeyFmt = "world_channels:draining:%s:%d"
)

func worldDrainingSetKey(zoneID uint32, confID uint64) string {
	return fmt.Sprintf(WorldDrainingSetKeyFmt, sceneControlTag(zoneID), confID)
}

func worldDesiredChannelsKey(zoneID uint32) string {
	return fmt.Sprintf(WorldDesiredChannelsKeyFmt, sceneControlTag(zoneID))
}

func sceneDrainingKey(zoneID uint32, sceneID uint64) string {
	return fmt.Sprintf(SceneDrainingKeyFmt, sceneControlTag(zoneID), sceneID)
}

func worldAutoscaleCooldownKey(zoneID uint32, confID uint64) string {
	return fmt.Sprintf(WorldAutoscaleCooldownKeyFmt, sceneControlTag(zoneID), confID)
}

// DesiredWorldChannelCount 返回某地图当前的期望频道数。
//
// 第一次访问时用配置里的 ChannelCountFor 播种。之后以 Redis 为准 ——
// 改配置不会覆盖已经自动伸缩过的值(否则一次重启就把伸缩结果抹掉了)。
// 运维想强制回到配置值,删掉这个 hash field 即可。
//
// 读或播种失败(超时、断连、熔断)时回 error,不回种子值(2026-10-23)。调用方都拿它做决策:补建按它算
// 「缺几个」,扩缩容把它 ±1 写回成权威值。以前读错当「还没播种」回种子值,于是缩掉的频道被补建回来,
// 或者权威值被改写成种子 ±1。字段值非法时仍按「没播种」处理(回种子值),与以前一致。
func DesiredWorldChannelCount(svcCtx *svc.ServiceContext, zoneID uint32, confID uint64) (int, error) {
	key := worldDesiredChannelsKey(zoneID)
	field := strconv.FormatUint(confID, 10)

	n, found, err := readDesiredWorldChannelField(svcCtx, key, field)
	if err != nil {
		return 0, err
	}
	if found {
		return n, nil
	}

	seed := svcCtx.Config.ChannelCountFor(confID)
	if seed < 1 {
		seed = 1
	}
	// Hsetnx:并发的多个 SceneManager 实例同时播种时,只有一个赢,
	// 其余读到已有值。不能用 Hset —— 那会把别人已经伸缩过的值冲掉。
	seeded, err := svcCtx.Redis.Hsetnx(key, field, strconv.Itoa(seed))
	if err != nil {
		return 0, fmt.Errorf("seed desired channels zone=%d conf=%d: %w", zoneID, confID, err)
	}
	if seeded {
		return seed, nil
	}
	// 别的实例抢先播种了(或字段值非法):以 Redis 里的为准。
	n, found, err = readDesiredWorldChannelField(svcCtx, key, field)
	if err != nil {
		return 0, err
	}
	if found {
		return n, nil
	}
	return seed, nil
}

// readDesiredWorldChannelField 读期望频道数字段。found=false 表示字段不存在,或值不是正整数(按「没播种」
// 处理);err 只表示读失败。控制集群句柄的 Hget 不吞 redis.Nil(与 Get 不同),字段不存在就是 redis.Nil。
func readDesiredWorldChannelField(svcCtx *svc.ServiceContext, key, field string) (int, bool, error) {
	raw, err := svcCtx.Redis.Hget(key, field)
	if errors.Is(err, redis.Nil) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read desired channels %s[%s]: %w", key, field, err)
	}
	n, convErr := strconv.Atoi(raw)
	if convErr != nil || n < 1 {
		return 0, false, nil
	}
	return n, true, nil
}

// clampDesiredWorldChannelCount 把期望频道数钳到 [min, max]。
func clampDesiredWorldChannelCount(svcCtx *svc.ServiceContext, n int) int {
	minCh := svcCtx.Config.WorldAutoscale.MinChannelsPerMap
	if minCh < 1 {
		minCh = 1 // 硬下限:每个大世界地图至少 1 个频道,不接受配置成 0
	}
	maxCh := svcCtx.Config.WorldAutoscale.MaxChannelsPerMap
	if maxCh > 0 && n > maxCh {
		n = maxCh
	}
	if n < minCh {
		n = minCh
	}
	return n
}

// 伸缩决策提交脚本的返回值(luaCommitWorldScaleOut 与 luaBeginDrainWorldChannel 共用,2026-11-11)。
const (
	// worldDrainNotRouted:只有缩容脚本回它 —— 频道已不在路由集合里(别的实例抢先摘掉了),什么都没写。
	worldDrainNotRouted = 0
	// worldScaleCommitted:写成了;或 go-redis 重发了本次已写成的同一条命令(冷却标记里是本次的令牌)。
	worldScaleCommitted = 1
	// worldScaleCooldown:冷却中 —— 本轮读冷却之后,别的领导者抢先伸缩了这张图。什么都没写。
	worldScaleCooldown = 2
	// worldScaleDesiredChanged:期望频道数与本轮读到的旧值不同(读之后被改过,或字段值非法),什么都没写。
	worldScaleDesiredChanged = 3
)

// luaCommitWorldScaleOut 把一次扩容决策提交成一步:判冷却、比对期望频道数旧值、写新值、起冷却
// (2026-11-11,docs/design/infra-reconnect-overview.md §4.1 R7)。此前是 EXISTS 冷却 → HGET 期望数 → HSET 期望数 + 1 →
// SETEX 冷却四次往返:两个重叠的领导者(Redis 主从切换丢了选主锁)都过了冷却检查时,后到的会读到先到的新值再 +1,多扩一条频道。
// 现在后到的那个在脚本里看到冷却标记(或期望数已变),什么都不写。
//
// KEYS[1] = 冷却标记 world_channels:cooldown:{ctl:<z>}:<conf>,KEYS[2] = 期望频道数哈希 world_channels:desired:{ctl:<z>}
// (同一个 zone、同一个控制域实例,与 reentry_barrier.go 的多键脚本同一口径)。
// ARGV[1] = confID,ARGV[2] = 本轮读到的期望频道数(旧值,十进制),ARGV[3] = 新的期望频道数,ARGV[4] = 冷却秒数,
// ARGV[5] = 本次决策的令牌(写进冷却标记;别处只看冷却标记在不在)。
// 返回 worldScaleCommitted / worldScaleCooldown / worldScaleDesiredChanged。
//
// 比对用字符串相等:期望数由 strconv.Itoa 写,权威值不会有别的写法;字段被写成非法值时 DesiredWorldChannelCount 回种子值,
// 这里比不上、不写,由调用方打 ERROR —— 保留坏值并告警,不拿种子值 +1 把它盖掉。
// 重发安全:同一条 EVAL 可能执行两次(结果未知后调用方再发;2026-11-12 以前 go-zero 句柄还会自动重发);第一次已执行时
// 冷却标记里是本次的令牌,回已提交。
const luaCommitWorldScaleOut = `
local cd = redis.call('GET', KEYS[1])
if cd then
    if cd == ARGV[5] then
        return 1
    end
    return 2
end
if redis.call('HGET', KEYS[2], ARGV[1]) ~= ARGV[2] then
    return 3
end
redis.call('HSET', KEYS[2], ARGV[1], ARGV[3])
redis.call('SET', KEYS[1], ARGV[5], 'EX', ARGV[4])
return 1
`

// worldAutoscaleCooldownSeconds 是一次伸缩之后冷却标记的存活秒数;配置非正时取 120。
func worldAutoscaleCooldownSeconds(svcCtx *svc.ServiceContext) int64 {
	if sec := svcCtx.Config.WorldAutoscale.CooldownSeconds; sec > 0 {
		return sec
	}
	return 120
}

// commitWorldScaleOut 跑 luaCommitWorldScaleOut,把期望频道数从 oldDesired 改成 newDesired 并起冷却。
// err 非空表示结局未知(脚本可能执行了,也可能没有),调用方不得按「已扩容」继续。
func commitWorldScaleOut(svcCtx *svc.ServiceContext, zoneID uint32, confID uint64, oldDesired, newDesired int) (int64, error) {
	res, err := svcCtx.Redis.Eval(luaCommitWorldScaleOut,
		[]string{worldAutoscaleCooldownKey(zoneID, confID), worldDesiredChannelsKey(zoneID)},
		strconv.FormatUint(confID, 10), strconv.Itoa(oldDesired), strconv.Itoa(newDesired),
		worldAutoscaleCooldownSeconds(svcCtx), uuid.NewString())
	if err != nil {
		return 0, err
	}
	return toInt64(res), nil
}

// StartWorldAutoscaler 起一个周期性扩缩容循环。
// IntervalSeconds<=0 或 Enabled=false 时直接返回,不起 goroutine。
func StartWorldAutoscaler(ctx context.Context, svcCtx *svc.ServiceContext) {
	cfg := svcCtx.Config.WorldAutoscale
	if !cfg.Enabled {
		logx.Info("[WorldAutoscale] disabled")
		return
	}
	interval := cfg.CheckIntervalSeconds
	if interval <= 0 {
		interval = 30
	}

	logx.Infof("[WorldAutoscale] started: interval=%ds scale_out>=%d scale_in<%d min=%d max=%d",
		interval, cfg.ScaleOutPlayerThreshold, cfg.ScaleInPlayerThreshold,
		cfg.MinChannelsPerMap, cfg.MaxChannelsPerMap)

	// 用 safego.Loop 而不是裸 go func:recover 的作用域精确到"一轮",某一轮
	// panic 只丢掉那一轮,循环本身继续按节拍跑;panic 会带着点位名进
	// safego_panic_total{point="scene_manager.world_autoscale"}(压测期恒 0 是
	// 健康判据)。裸写法一轮炸掉,要么打死整个进程,要么循环从此静默停摆 ——
	// 而伸缩停摆在外面看起来只是"人多了不扩频道",没人会联想到 goroutine。
	safego.Loop(ctx, SafePointWorldAutoscale, time.Duration(interval)*time.Second,
		func(ctx context.Context) {
			// 多副本时只有领导者做伸缩决策 —— 两份 autoscaler 并发对同一批
			// zone 扩缩正是部署清单曾用 replicas:1 + Recreate 兜的口子,
			// 现在由 shared/leader 选主收敛(接线见 scene_manager_service.go)。
			if !isLeader() {
				return
			}
			for _, zoneID := range orchestratedZones(svcCtx) {
				AutoscaleWorldChannelsForZone(ctx, svcCtx, zoneID)
			}
		})
}

// channelLoad 是一个频道的人数快照。
type channelLoad struct {
	sceneID   string
	sceneID64 uint64
	players   int64
}

// AutoscaleWorldChannelsForZone 对一个 zone 跑一轮扩缩容。
// 返回 (扩容次数, 开始排空的频道数),导出是为了让单测直接驱动一轮。
func AutoscaleWorldChannelsForZone(ctx context.Context, svcCtx *svc.ServiceContext, zoneID uint32) (int, int) {
	// 先收尾上一轮还没排干净的频道,再考虑新的伸缩决策。
	sweepDrainingWorldChannels(ctx, svcCtx, zoneID)

	scaledOut, scaledIn := 0, 0
	for _, confID := range worldConfIds() {
		out, in := autoscaleOneWorldMap(ctx, svcCtx, zoneID, confID)
		scaledOut += out
		scaledIn += in
	}
	return scaledOut, scaledIn
}

func autoscaleOneWorldMap(ctx context.Context, svcCtx *svc.ServiceContext, zoneID uint32, confID uint64) (int, int) {
	cfg := svcCtx.Config.WorldAutoscale

	channels, err := GetAllWorldChannels(ctx, svcCtx, confID, zoneID)
	if err != nil || len(channels) == 0 {
		return 0, 0
	}

	loads := make([]channelLoad, 0, len(channels))
	for _, sceneID := range channels {
		// 正在排空的频道不参与决策:它已经从路由集合里摘掉了,
		// 它身上剩的人正在被改派走。
		// 排空标记或人数任何一项读不到,这张图本轮不做决策:人数读错当 0 会把一个满员频道
		// 挑成缩容对象,排空标记读错当「不在排空」会让它重复参与决策。伸缩是可选动作,下一拍再来。
		draining, err := svcCtx.Redis.Exists(sceneDrainingKey(zoneID, sceneID))
		if err != nil {
			noteReadUnknown(zoneID, readUnknownSiteAutoscaleDecide, fmt.Sprintf("draining flag of scene %d", sceneID), err)
			return 0, 0
		}
		if draining {
			continue
		}
		players, err := readScenePlayerCountChecked(svcCtx, zoneID, sceneID)
		if err != nil {
			noteReadUnknown(zoneID, readUnknownSiteAutoscaleDecide, fmt.Sprintf("player count of scene %d", sceneID), err)
			return 0, 0
		}
		loads = append(loads, channelLoad{
			sceneID:   strconv.FormatUint(sceneID, 10),
			sceneID64: sceneID,
			players:   players,
		})
	}
	if len(loads) == 0 {
		return 0, 0
	}

	// 人少的排前面,缩容挑第一个、扩容看最后一个。
	sort.Slice(loads, func(i, j int) bool { return loads[i].players < loads[j].players })

	if inCooldown(svcCtx, zoneID, confID) {
		return 0, 0
	}

	minCh := cfg.MinChannelsPerMap
	if minCh < 1 {
		minCh = 1
	}

	// ── 缩容 ──────────────────────────────────────────────────────────
	// 条件:最闲的频道人数 < 阈值,且缩掉之后还剩至少 minCh 个频道,
	// 且其余频道装得下它的人(不能把人并过去反而把别人推过扩容线)。
	lightest := loads[0]
	if len(loads) > minCh && lightest.players < cfg.ScaleInPlayerThreshold {
		if victim, ok := pickScaleInVictim(svcCtx, zoneID, loads, cfg); ok {
			// 冷却标记由排空脚本在同一步里写(2026-11-11 起,见 luaBeginDrainWorldChannel)。
			if beginDrainWorldChannel(ctx, svcCtx, zoneID, confID, victim) {
				return 0, 1
			}
		} else {
			logx.Infof("[WorldAutoscale] zone=%d conf=%d: channel %s has %d players (<%d) but no channel "+
				"is safe to drain (no headroom, or it is a mirror source); keeping them all",
				zoneID, confID, lightest.sceneID, lightest.players, cfg.ScaleInPlayerThreshold)
		}
	}

	// ── 扩容 ──────────────────────────────────────────────────────────
	// 要求**所有**频道都到线。loads 已按人数升序,查最闲的那个就够。
	if lightest.players >= cfg.ScaleOutPlayerThreshold {
		desired, err := DesiredWorldChannelCount(svcCtx, zoneID, confID)
		if err != nil {
			// 读不到权威值就不写:拿种子值 +1 写回会把已经伸缩过的值冲掉。
			noteReadUnknown(zoneID, readUnknownSiteAutoscaleDecide, fmt.Sprintf("desired channel count of conf %d", confID), err)
			return 0, 0
		}
		if cfg.MaxChannelsPerMap > 0 && desired >= cfg.MaxChannelsPerMap {
			logx.Errorf("[WorldAutoscale] zone=%d conf=%d: every channel is at/over %d players but "+
				"MaxChannelsPerMap=%d is reached; the map is over capacity",
				zoneID, confID, cfg.ScaleOutPlayerThreshold, cfg.MaxChannelsPerMap)
			metrics.ObserveWorldAutoscale(zoneID, "scale_out", "max_reached")
			return 0, 0
		}

		// 判冷却、比对期望数旧值、写新值并起冷却,一段脚本(2026-11-11,R7):上面的 inCooldown 只是省一次往返的预检,
		// 重叠的领导者都过了预检时,以脚本里的判定为准。
		newDesired := clampDesiredWorldChannelCount(svcCtx, desired+1)
		outcome, err := commitWorldScaleOut(svcCtx, zoneID, confID, desired, newDesired)
		if err != nil {
			// 结局未知:写成了的话期望数已是新值,下一次 fullSync / 节点 PUT 的补建会把频道建出来;没写成就什么都没变。
			logx.Errorf("[WorldAutoscale] zone=%d conf=%d: committing scale-out %d -> %d failed, outcome unknown: %v",
				zoneID, confID, desired, newDesired, err)
			metrics.ObserveWorldAutoscale(zoneID, "scale_out", "error")
			return 0, 0
		}
		switch outcome {
		case worldScaleCommitted:
		case worldScaleCooldown:
			logx.Infof("[WorldAutoscale] zone=%d conf=%d: another leader scaled this map after this round read the cooldown; "+
				"not scaling out", zoneID, confID)
			return 0, 0
		case worldScaleDesiredChanged:
			logx.Errorf("[WorldAutoscale] zone=%d conf=%d: desired channel count changed since it was read as %d, or holds an "+
				"invalid value; not scaling out this round (check %s field %d)",
				zoneID, confID, desired, worldDesiredChannelsKey(zoneID), confID)
			return 0, 0
		default:
			logx.Errorf("[WorldAutoscale] zone=%d conf=%d: unexpected scale-out commit result %d; not scaling out",
				zoneID, confID, outcome)
			return 0, 0
		}
		// 立刻把新频道建出来,不等下一次 fullSync —— 玩家现在就挤着。
		// waitForLock=true:跑在 autoscaler 自己的 goroutine 上,等锁无害,
		// 而期望频道数已 +1,尽快落地。
		initWorldScenesForZone(ctx, svcCtx, zoneID, []uint64{confID}, true)

		logx.Infof("[WorldAutoscale] zone=%d conf=%d: all %d channel(s) >= %d players, scaled out to %d",
			zoneID, confID, len(loads), cfg.ScaleOutPlayerThreshold, newDesired)
		metrics.ObserveWorldAutoscale(zoneID, "scale_out", "ok")
		return 1, 0
	}

	return 0, 0
}

// hasHeadroomFor 判断把 victim 的人并进其余频道后,是否会把某个频道推过扩容线。
//
// 这条检查不是洁癖:少了它,缩容会把人挤进别的频道触发扩容,扩容又让某个
// 频道人数掉到 100 以下触发缩容 —— 自激振荡,玩家被反复强制改派。
func hasHeadroomFor(loads []channelLoad, victim channelLoad, scaleOutThreshold int64) bool {
	var headroom int64
	for _, l := range loads {
		if l.sceneID64 == victim.sceneID64 {
			continue
		}
		if room := scaleOutThreshold - l.players; room > 0 {
			headroom += room
		}
	}
	return headroom >= victim.players
}

// beginDrainWorldChannel 开始排空一个频道。
//
// 顺序是**先摘路由再排空**:先从 world_channels 集合里 SREM,新玩家就不会再被
// 路由进来;然后才让 C++ 把残留玩家改派走。反过来做的话,刚被改派出去的玩家
// 有可能又被分回这个正在销毁的频道。
// channelHasMirrors 报告是否有镜像 Scene 以这个频道为源。
//
// 查询失败时返回 true(当作"有镜像")—— 这是刻意的 fail-closed 方向:
// 缩容是可选的省钱动作,查不清就别动,比误伤镜像里的玩家划算。
func channelHasMirrors(svcCtx *svc.ServiceContext, zoneID uint32, sceneID uint64) bool {
	members, err := svcCtx.Redis.Smembers(sceneMirrorsKey(zoneID, sceneID))
	if err != nil {
		logx.Errorf("[WorldAutoscale] cannot read mirrors of scene %d (%v); treating it as a mirror source",
			sceneID, err)
		return true
	}
	return len(members) > 0
}

// pickScaleInVictim 在升序的 loads 里挑第一个可以安全排空的频道。
//
// 三个条件都要满足:
//  1. 人数低于缩容线;
//  2. **不是镜像源**。镜像与源共置(见 scene-creation-architecture.md 的
//     mirror co-location),源频道被销毁的话镜像会变成谁也进不去的孤儿。
//     节点死亡那条路径是强制级联销毁镜像的,但缩容是可选动作 —— 宁可少省
//     一个频道,也不要把镜像里的玩家踢下线;
//  3. 其余频道装得下它的人,且不会因此把某个频道推过扩容线(否则缩完立刻扩,来回抖)。
//
// 不是只看 loads[0]:最闲的那个恰好托着镜像时,继续往下找仍然能缩容。
func pickScaleInVictim(svcCtx *svc.ServiceContext, zoneID uint32, loads []channelLoad, cfg config.WorldAutoscaleConfig) (channelLoad, bool) {
	for _, candidate := range loads {
		if candidate.players >= cfg.ScaleInPlayerThreshold {
			// loads 升序,后面的只会更多人。
			break
		}
		if channelHasMirrors(svcCtx, zoneID, candidate.sceneID64) {
			logx.Infof("[WorldAutoscale] channel %s is a mirror source; not draining it", candidate.sceneID)
			continue
		}
		if !hasHeadroomFor(loads, candidate, cfg.ScaleOutPlayerThreshold) {
			continue
		}
		return candidate, true
	}
	return channelLoad{}, false
}

// luaBeginDrainWorldChannel 把频道从路由集合移进排空集合,一步完成。分两步时,进程在两步之间崩溃,频道既不可
// 路由也不再收敛;读方也可能两头落空(channel_pick.go 的 locateWorldChannel)。
//
// 期望频道数同步减 1,否则 initWorldScenesForZone 会立刻把它补回来。
// 排空标记带 TTL:进程在排空中途挂掉时,标记会自己过期,下一轮 sweep 会重新接手,不会留下永久"半死"频道。
//
// 判冷却与比对期望数旧值也在这一步里(2026-11-11,docs/design/infra-reconnect-overview.md §4.1 R7):此前冷却在脚本外判、
// 期望数在脚本外读,冷却在排空之后另写。两个重叠的领导者都过了冷却检查时,各自摘掉一条频道、期望数各减一次,多缩一条。
// 现在冷却标记在同一步里写(值是本次决策的令牌),后到的看到冷却或期望数已变就什么都不动。
//
// 重发安全:回包丢失、同一条命令再发一次时(结果未知后再发;2026-11-12 以前 go-zero 句柄还会自动重发),冷却标记里是本次的令牌,
// 回 1,调用方照常推进排空。(此前重发时
// 第二次 SREM 回 0,调用方按「别的实例抢先」收手,本拍不推进,等下一拍 sweep。)
//
// KEYS[1] = 频道集合,KEYS[2] = 排空集合,KEYS[3] = 排空标记,KEYS[4] = 期望频道数哈希,KEYS[5] = 冷却标记
// (都在控制域实例上,除排空标记外同一个 zone;与 reentry_barrier.go 的多键脚本同一口径)
// ARGV[1] = sceneId,ARGV[2] = 排空标记 TTL 秒,ARGV[3] = confID,ARGV[4] = 新的期望频道数,
// ARGV[5] = 本轮读到的期望频道数(旧值,十进制),ARGV[6] = 冷却秒数,ARGV[7] = 本次决策的令牌
// Return: worldScaleCommitted(1)= 移动了;worldDrainNotRouted(0)= 不在频道集合里(别的实例抢先摘掉了);
// worldScaleCooldown(2)、worldScaleDesiredChanged(3)见常量。后三种什么都没写。
const luaBeginDrainWorldChannel = `
local cd = redis.call('GET', KEYS[5])
if cd then
    if cd == ARGV[7] then
        return 1
    end
    return 2
end
if redis.call('HGET', KEYS[4], ARGV[3]) ~= ARGV[5] then
    return 3
end
if redis.call('SREM', KEYS[1], ARGV[1]) == 0 then
    return 0
end
redis.call('SADD', KEYS[2], ARGV[1])
redis.call('SET', KEYS[3], '1', 'EX', ARGV[2])
redis.call('HSET', KEYS[4], ARGV[3], ARGV[4])
redis.call('SET', KEYS[5], ARGV[7], 'EX', ARGV[6])
return 1
`

func beginDrainWorldChannel(ctx context.Context, svcCtx *svc.ServiceContext, zoneID uint32, confID uint64, victim channelLoad) bool {
	// 期望频道数在摘路由之前读:读不到就什么都不写。
	desired, err := DesiredWorldChannelCount(svcCtx, zoneID, confID)
	if err != nil {
		noteReadUnknown(zoneID, readUnknownSiteAutoscaleDecide, fmt.Sprintf("desired channel count of conf %d", confID), err)
		return false
	}

	ttl := svcCtx.Config.WorldAutoscale.DrainTimeoutSeconds
	if ttl <= 0 {
		ttl = 300
	}
	res, err := svcCtx.Redis.Eval(luaBeginDrainWorldChannel,
		[]string{
			worldChannelsKey(zoneID, confID),
			worldDrainingSetKey(zoneID, confID),
			sceneDrainingKey(zoneID, victim.sceneID64),
			worldDesiredChannelsKey(zoneID),
			worldAutoscaleCooldownKey(zoneID, confID),
		},
		victim.sceneID, ttl, strconv.FormatUint(confID, 10), clampDesiredWorldChannelCount(svcCtx, desired-1),
		strconv.Itoa(desired), worldAutoscaleCooldownSeconds(svcCtx), uuid.NewString())
	if err != nil {
		// 结局未知:脚本没执行时频道还在路由集合里,下一拍重新决策;执行了时频道在排空集合里,sweep 接着推进。
		logx.Errorf("[WorldAutoscale] zone=%d conf=%d: moving channel %s to draining failed: %v",
			zoneID, confID, victim.sceneID, err)
		metrics.ObserveWorldAutoscale(zoneID, "scale_in", "error")
		return false
	}
	switch outcome := toInt64(res); outcome {
	case worldScaleCommitted:
	case worldDrainNotRouted:
		// 别的实例抢先摘掉了。不重复处理。
		return false
	case worldScaleCooldown:
		logx.Infof("[WorldAutoscale] zone=%d conf=%d: another leader scaled this map after this round read the cooldown; "+
			"not draining channel %s", zoneID, confID, victim.sceneID)
		return false
	case worldScaleDesiredChanged:
		logx.Errorf("[WorldAutoscale] zone=%d conf=%d: desired channel count changed since it was read as %d, or holds an "+
			"invalid value; not draining channel %s this round (check %s field %d)",
			zoneID, confID, desired, victim.sceneID, worldDesiredChannelsKey(zoneID), confID)
		return false
	default:
		logx.Errorf("[WorldAutoscale] zone=%d conf=%d: unexpected drain result %d for channel %s; nothing assumed moved",
			zoneID, confID, outcome, victim.sceneID)
		return false
	}

	logx.Infof("[WorldAutoscale] zone=%d conf=%d: draining channel %s (%d players < %d); "+
		"removed from routing, relocating residents to the remaining channels",
		zoneID, confID, victim.sceneID, victim.players, svcCtx.Config.WorldAutoscale.ScaleInPlayerThreshold)
	metrics.ObserveWorldAutoscale(zoneID, "scale_in", "ok")

	// 第一次 DestroyScene:C++ 侧发现还有人就只改派、不销毁实体,
	// 由 sweep 在后续 tick 收敛。
	drainOrDestroyChannel(ctx, svcCtx, zoneID, confID, victim.sceneID64)
	return true
}

// sweepDrainingWorldChannels 收敛所有正在排空的频道。
//
// 每拍重新观察真实状态(人数),而不是记"我做到第几步了"。C++ 的
// DestroyScene 是幂等的:还有人就改派,没人就真销毁。
func sweepDrainingWorldChannels(ctx context.Context, svcCtx *svc.ServiceContext, zoneID uint32) {
	for _, confID := range worldConfIds() {
		members, err := svcCtx.Redis.Smembers(worldDrainingSetKey(zoneID, confID))
		if err != nil || len(members) == 0 {
			continue
		}
		for _, m := range members {
			sceneID, err := strconv.ParseUint(m, 10, 64)
			if err != nil || sceneID == 0 {
				svcCtx.Redis.Srem(worldDrainingSetKey(zoneID, confID), m)
				continue
			}
			drainOrDestroyChannel(ctx, svcCtx, zoneID, confID, sceneID)
		}
	}
}

// drainOrDestroyChannel 对一个正在排空的频道推进一步。
//
// 属主或人数读不到时本拍什么都不做:频道还在排空集合里,下一拍 sweep 重来。读错当「映射已没」
// 会跳过 C++ DestroyScene 直接收尾,实体留在节点上;读错当「0 人」会在还有人时收尾。
func drainOrDestroyChannel(ctx context.Context, svcCtx *svc.ServiceContext, zoneID uint32, confID uint64, sceneID uint64) {
	nodeID, err := svcCtx.Redis.Get(sceneNodeKey(zoneID, sceneID))
	if err != nil {
		noteReadUnknown(zoneID, readUnknownSiteAutoscaleDrain, fmt.Sprintf("owner of draining scene %d", sceneID), err)
		return
	}
	if nodeID == "" {
		// 映射已经没了 —— 频道已经销毁完,或者节点死亡时被清理过。
		finishDrainedChannel(ctx, svcCtx, zoneID, confID, sceneID, "")
		return
	}
	if isKnownNodeIdentityAmbiguous(zoneID, nodeID) {
		logx.Errorf("[WorldAutoscale] draining channel %d: zone=%d node=%s has duplicate registrations; refusing cleanup",
			sceneID, zoneID, nodeID)
		return
	}

	if !IsNodeAlive(svcCtx, zoneID, nodeID) {
		// 节点已死:实体随进程一起没了,玩家会走各自的掉线/重连路径。
		// 这里只负责把 Redis 状态清干净。
		//
		// 再入屏障(2026-11-05):摘出负载集不代表老节点已经停笔,它若走了冲突关停还有有界疏散在
		// 存盘(SavePlayer)。收尾删映射、成员集合与名额记录,属于「销毁」一类,与死节点收尾、孤儿清理同受屏障约束
		// (reentry_barrier.go 顶部)。屏障未到本拍什么都不做,频道留在排空集合,下一拍 sweep 再来。
		if reentryBarrierBlocks(svcCtx, zoneID, nodeID, barrierSiteAutoscaleDrain) {
			return
		}
		logx.Infof("[WorldAutoscale] draining channel %d: node %s is gone, cleaning up state",
			sceneID, nodeID)
		finishDrainedChannel(ctx, svcCtx, zoneID, confID, sceneID, nodeID)
		return
	}

	players, err := readScenePlayerCountChecked(svcCtx, zoneID, sceneID)
	if err != nil {
		noteReadUnknown(zoneID, readUnknownSiteAutoscaleDrain, fmt.Sprintf("player count of draining scene %d", sceneID), err)
		return
	}

	// 无论有没有人都调:有人时 C++ 会把他们改派到大世界并保留实体,
	// 没人时才真正销毁。幂等,可以每拍调。
	if err := RequestNodeDestroyScene(ctx, svcCtx, zoneID, nodeID, sceneID); err != nil {
		logx.Errorf("[WorldAutoscale] draining channel %d: DestroyScene on node %s failed: %v",
			sceneID, nodeID, err)
		return
	}

	if players > 0 {
		logx.Infof("[WorldAutoscale] draining channel %d: %d player(s) still relocating", sceneID, players)
		return
	}

	finishDrainedChannel(ctx, svcCtx, zoneID, confID, sceneID, nodeID)
}

// finishDrainedChannel 清掉一个排空完成的频道的全部 Redis 状态。nodeID 是本拍读到的属主,空串表示
// 映射已经没了。
//
// 场景键(属主映射、成员集合、agones_gs)由 atomicTeardownScene 按本拍读到的属主 nodeID 一次性认领并删除,
// 同一步把它从排空集合摘掉、改好属主的反向索引与计数、写下拆除任务(2026-10-23;拆除任务 2026-11-05)。以前是逐条
// DEL:读到属主 X 之后、删映射之前,若有写者把频道改派到活节点 Y 并 CreateScene,逐条 DEL 会把 Y 上那份带着玩家的
// 实体变成没有映射的孤儿,还会扣错节点的计数。现在属主已不是 nodeID(nodeID 为空时:期间被写入了属主)就一个键
// 都不动,下一拍按新属主重来(还有人就继续排空)。认领之后的副作用见 runWorldDrainTeardown。
//
// 镜像子集合在认领之前预读一次:读不到本拍什么都不做,频道留在排空集合等下一拍。
func finishDrainedChannel(ctx context.Context, svcCtx *svc.ServiceContext, zoneID uint32, confID uint64, sceneID uint64, nodeID string) {
	preMirrorChildren, err := svcCtx.Redis.Smembers(sceneMirrorsKey(zoneID, sceneID))
	if err != nil {
		noteReadUnknown(zoneID, readUnknownSiteAutoscaleFinish, fmt.Sprintf("mirror set of draining scene %d", sceneID), err)
		return
	}

	req := sceneTeardownRequest{
		ZoneID: zoneID, SceneID: sceneID, Kind: teardownKindWorldDrain, Reason: "world_channel_scale_in",
		ExpectedOwner: nodeID,
		// 只扣活属主的 scene_count(2026-11-05,与孤儿清理 tearDownOrphanScene 同口径)。死属主的计数键由死节点收尾在
		// 收尾完成时整个删掉;死属主分支又要等屏障走完,与那次删除差不多同时到期,删了之后再扣会把键重建成 -1,
		// 被复用同一 node_id 的新进程继承。扣减在认领脚本里做,活不活在认领之前判。
		CountOwner: nodeID != "" && IsNodeAlive(svcCtx, zoneID, nodeID),
		UnlistKeys: []string{worldDrainingSetKey(zoneID, confID)},
	}
	claim, err := atomicTeardownScene(svcCtx, req)
	if err != nil {
		// 脚本可能已执行也可能没有,结论未知:本拍不再往下做,频道若还在排空集合里,下一拍重读属主再来。
		logx.Errorf("[WorldAutoscale] draining channel %d: atomic teardown (expected owner %q) failed, outcome unknown: %v",
			sceneID, nodeID, err)
		metrics.ObserveWorldAutoscale(zoneID, "scale_in", "error")
		return
	}
	if !claim.Claimed {
		logx.Infof("[WorldAutoscale] owner of draining channel %d changed since it was read (expected %q); retrying next sweep",
			sceneID, nodeID)
		return
	}

	// 认领已提交,剩下的副作用记在拆除任务里:这里当场做,没做完的由扫描补(scale_in/drained 在任务做完时记)。
	completeSceneTeardown(ctx, svcCtx, newSceneTeardownJob(req, claim), preMirrorChildren, false)
	logx.Infof("[WorldAutoscale] channel %d drained and removed (zone=%d node=%s)", sceneID, zoneID, nodeID)
}

// runWorldDrainTeardown 是排空完成的世界频道认领之后的副作用(拆除任务 teardownKindWorldDrain),认领方与扫描共用,
// 可以重跑:级联镜像、删排空标记与放置代次、摘剩余成员、还 Agones 名额。属主的反向索引与计数已在认领脚本里改好。
//
// 级联兜底:排空窗口里可能又有镜像以这个频道为源建起来了(pickScaleInVictim 只保证**开始排空那一刻**
// 没有镜像)。源频道马上要没了,留着镜像就是谁也进不去的孤儿,而且 scene:{id}:mirrors 会永久泄漏。
func runWorldDrainTeardown(ctx context.Context, svcCtx *svc.ServiceContext, job sceneTeardownJob, preMirrorChildren []string) error {
	zoneID, sceneID, nodeID := job.ZoneID, job.SceneID, job.Claim.NodeID
	var deferred []string
	if !cascadeDestroyMirrors(ctx, svcCtx, zoneID, sceneID, preMirrorChildren, "source_scaled_in") {
		deferred = append(deferred, "mirror cascade")
	}
	// 排空标记带 TTL,删不掉也会自己过期。
	svcCtx.Redis.Del(sceneDrainingKey(zoneID, sceneID))
	if err := deletePlacementGeneration(svcCtx, zoneID, sceneID); err != nil {
		deferred = append(deferred, "delete the placement generation")
	}
	if nodeID != "" {
		removeResidualSceneMembers(svcCtx, sceneID, zoneID, nodeID, job.Claim.Members)
	}
	if err := releaseTeardownRoom(ctx, svcCtx, job); err != nil {
		deferred = append(deferred, "release the Agones room")
	}
	return teardownDeferred(deferred)
}

// inCooldown 报告这张图是否还在伸缩冷却期。读不到冷却标记按「仍在冷却」处理:本轮不伸缩,
// 下一拍再看。按「不在冷却」处理会在 Redis 抖动时连续扩缩,正是冷却要防的来回抖。
//
// (2026-11-11 起)这里只是省掉人数统计之后那几次往返的预检:冷却标记由伸缩脚本在提交的同一步里写
// (luaCommitWorldScaleOut、luaBeginDrainWorldChannel,值是那次决策的令牌),脚本里还会再判一次。原来的 markCooldown
// 在伸缩之后另写一次 SETEX,随之删除。
func inCooldown(svcCtx *svc.ServiceContext, zoneID uint32, confID uint64) bool {
	exists, err := svcCtx.Redis.Exists(worldAutoscaleCooldownKey(zoneID, confID))
	if err != nil {
		noteReadUnknown(zoneID, readUnknownSiteAutoscaleDecide, fmt.Sprintf("cooldown flag of conf %d", confID), err)
		return true
	}
	return exists
}
