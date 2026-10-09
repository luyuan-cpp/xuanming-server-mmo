package logic

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strconv"
	"time"

	scenepb "proto/scene"

	"scene_manager/internal/metrics"
	"scene_manager/internal/svc"
	"shared/safego"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

// 分线目录:一张大世界地图在一个 zone 里「有哪些线、各是几线、多少人、什么状态」的读模型。
// 设计文档:docs/design/world-channel-switch.md §4.1–§4.3。
//
// 为什么要有它:完整的线列表只有 scene_manager 有(scene 节点只知道自己身上的线),而客户端的
// 线路面板要一次看到全部。本文件由领导者周期性地把快照写进 Redis,scene 节点只读转发给客户端。
//
// 目录**只用于展示和客户端预判**(最多滞后一个发布周期),不是放行凭据:玩家选线的请求仍由
// EnterScene 的选线预检(channel_pick.go)重新校验。
//
// 本文件对既有状态全程只读:只读 world_channels / draining 集合、scene:{id}:node、人数键和节点
// 负载集;写的只有自己的两把键(线号表、目录)。刻意不调用 GetBestWorldChannel /
// ReserveBestWorldChannelForEnter —— 它们在线全部陈旧时会懒改派并发 CreateScene,而列线每 2 秒
// 跑一次,不能带这种副作用。也不调用 reentryBarrierBlocks:它每次被挡都会记
// reentry_barrier_blocked_total 并打一条 Error,放进周期循环会把那条「稳态恒 0」的指标刷脏。
const (
	// WorldChannelLineNoKeyFmt:HASH per (zone, confId),field = scene_id(十进制),value = 线号(从 1 起)。
	// 无 TTL:线号要在线的整个生命周期里保持不变。
	//
	// 这两把键刻意不用 world_channels:zone: 前缀:孤儿清理按那个前缀 SCAN(orphan_cleanup.go
	// orphanKeyPrefix),会把它们当成频道集合来解析。
	WorldChannelLineNoKeyFmt = "world_channel_lineno:zone:%d:%d"

	// WorldChannelDirectoryKeyFmt:STRING per (zone, confId),值是 scenepb.SceneChannelDirectory 的
	// 序列化字节,带 TTL(见 worldChannelDirectoryTTLSeconds)。
	//
	// **跨语言契约,字面量不许改**:C++ scene 节点按同一格式拼键来读,对应常量是
	// cpp/libs/services/scene/player/system/player_scene.cpp 的 kWorldChannelDirectoryKeyFmt
	// (那边的占位符是 C printf 的 %u,这边是 Go 的 %d;契约是**渲染出来的键**逐字节相同:
	// world_channel_directory:zone:{zone 十进制}:{conf 十进制})。
	// 改这里必须同批改那里;TestWorldChannelDirectoryKeyFmt_CrossLanguageContract 钉住本侧字面量与渲染结果。
	WorldChannelDirectoryKeyFmt = "world_channel_directory:zone:%d:%d"

	// 目录 TTL = worldChannelDirectoryTTLRounds × 发布周期 + 一个选主竞选间隔(worldChannelDirectoryTTLSeconds)。
	//
	// 3 轮:稳态下容忍连续两轮发布失败 / 被跳过而不让客户端看到「线路信息暂不可用」。
	//
	// 加一个竞选间隔:领导者**优雅让位**(每次滚动更新都会发生)时目录不能断档。那时最长的无发布间隔 =
	//   让位前最后一次发布距让位 ≤ 1 个发布周期
	//   + 接任者等到自己的下一次竞选 ≤ 1 个竞选间隔(shared/leader:锁 TTL / 3,默认 30s / 3 = 10s)
	//   + 当选后等到发布循环的下一个 tick ≤ 1 个发布周期(safego.Loop 只按 ticker 跑,没有「当选即发」)
	// 即 2 个周期 + 1 个竞选间隔;3 轮里多出来的那 1 个周期是余量(放锁、SETNX、首轮发布本身的耗时)。
	// 默认值下:无发布间隔最长 2 + 10 + 2 = 14s,TTL = 3 × 2 + 10 = 16s。
	// 竞选间隔必须按选主参数算、不能写死:调大 LeaderLockTTLSeconds 时它跟着变长。
	//
	// **盖不住领导者崩溃**(进程被杀、来不及放锁):接任者要先等锁自己过期,无发布间隔最长约
	// 1 个周期 + 锁 TTL + 1 个竞选间隔 + 1 个周期(默认 2 + 30 + 10 + 2 = 44s),目录会缺失最多约 28s,
	// 客户端显示「线路信息暂不可用」(切线本身不受影响)。这是取舍不是遗漏:要盖住得把 TTL 拉到 45s 以上,
	// 代价是领导者全挂时那份不再更新的旧目录也要多留这么久。
	// 过期才是「目录不可信」的信号,所以不能没有 TTL:领导者全挂时旧目录必须自己消失。
	worldChannelDirectoryTTLRounds int64 = 3

	// worldChannelDirectoryMaxCampaignSeconds:计入目录 TTL 的竞选间隔上限(一天)。锁 TTL 配到三天以上
	// 只可能是笔误;钳住是为了让目录 TTL 换算成 Redis EX(int)时不溢出(发布周期那一项在 config.go 已钳)。
	worldChannelDirectoryMaxCampaignSeconds int64 = 24 * 60 * 60
)

func worldChannelLineNoKey(zoneID uint32, confID uint64) string {
	return fmt.Sprintf(WorldChannelLineNoKeyFmt, zoneID, confID)
}

func worldChannelDirectoryKey(zoneID uint32, confID uint64) string {
	return fmt.Sprintf(WorldChannelDirectoryKeyFmt, zoneID, confID)
}

// luaAllocWorldChannelLineNumbers 原子地把线号表对齐到「这张图当前全部的线」,并返回整张表。
//
// KEYS[1] = world_channel_lineno:zone:{zone}:{conf}
// ARGV[i] = scene_id 的十进制字符串,Go 侧已按**数值**升序排好、去重。
// Return  = {scene_id, 线号, scene_id, 线号, ...},顺序与 ARGV 一致,线号是十进制字符串。
//
// 规则(world-channel-switch.md §4.2):
//  1. 表里不在 ARGV 中的 field 删掉 —— 线彻底销毁后号被释放,之后新建的线可以复用;
//  2. 已有合法线号的成员原样保留 —— 线在役或回收中期间线号不变;
//  3. 没有号的成员按 ARGV 顺序依次拿「最小的空闲正整数」—— 存量线首次补号时等于按 scene_id
//     升序(snowflake 即创建先后)编成 1..N。
//
// scene_id 在脚本里**只当字符串用**:snowflake 超过 2^53,进 Lua number(double)会丢低位,
// 两条相邻的线会被当成同一条。线号是小整数,可以 tonumber。
//
// 表里的坏值(非数字、< 1、非整数、超过 uint32、与别的成员重号)按「没有号」处理:删掉重发。
// 不这样做的话,一条被写坏的线号会让 Go 侧每一轮都解析失败,这张图的目录从此发布不出来。
//
// ARGV 为空时什么都不做:调用方本来就不该在空输入时执行(Redis 抖动时 SMEMBERS 可能读空,
// 照规则 1 执行会把整张表清掉,恢复后线号全部重排),这里是第二道保险。
//
// 只有领导者发布目录,所以实际是单写者;放进 Lua 是防领导权交接窗口里新旧领导者各跑一轮
// 时读改写交错(两条新线拿到同一个号)。
//
// **已知残余**(规则 2 在这一种情形下不成立):脚本只能把「不在 ARGV 里」理解成「线已销毁」,而 autoscaler
// 开始排空一条线并不是原子的 —— beginDrainWorldChannel(world_autoscale.go)先 SREM 在役集合,中间隔着
// 读写期望频道数、写排空标记至少 3 次 Redis 往返,才 SADD 回收中集合(SADD 失败还只记日志)。
// 发布的两次 SMEMBERS 若都落在这两步之间,这条线两个集合都读不到:当轮目录里没有它,它的号在这里被删;
// 下一轮它按规则 3 重新拿「最小的空闲号」,表里有更小的空号时线号就变了(例:{1,3,4} 里的 4 线变成
// 2 线),仍在线上的玩家角标跟着跳。窗口是毫秒级、发布周期是秒级,且只在 WorldAutoscale.Enabled 并恰好
// 开始缩容时可达;后果限于展示,不涉及归属。
// 根治在 world_autoscale.go:把那两步合成一次原子的 SMOVE,让一条线任何时刻至少在一个集合里 ——
// 本文件区分不了「正在摘」与「已销毁」。发布侧为此保证的只有读的顺序,见 publishWorldChannelDirectory。
const luaAllocWorldChannelLineNumbers = `
if #ARGV == 0 then
    return {}
end

local wanted = {}
for i = 1, #ARGV do
    wanted[ARGV[i]] = true
end

local kept = {}
local used = {}
local flat = redis.call('HGETALL', KEYS[1])
for i = 1, #flat, 2 do
    local sceneId = flat[i]
    local lineNo = tonumber(flat[i + 1])
    if wanted[sceneId] and lineNo and lineNo >= 1 and lineNo <= 4294967295
        and lineNo == math.floor(lineNo) and not used[lineNo] then
        kept[sceneId] = lineNo
        used[lineNo] = true
    else
        redis.call('HDEL', KEYS[1], sceneId)
    end
end

local out = {}
local candidate = 1
for i = 1, #ARGV do
    local sceneId = ARGV[i]
    local lineNo = kept[sceneId]
    if not lineNo then
        while used[candidate] do
            candidate = candidate + 1
        end
        lineNo = candidate
        used[lineNo] = true
        kept[sceneId] = lineNo
        redis.call('HSET', KEYS[1], sceneId, tostring(lineNo))
    end
    out[#out + 1] = sceneId
    out[#out + 1] = tostring(lineNo)
end
return out
`

// allocateWorldChannelLineNumbers 把 sceneIDs(这张图当前「在役 ∪ 回收中」的全部线)对齐进线号表,
// 返回 scene_id → 线号。
//
// 输入为空时直接返回空表、**不执行脚本**(理由见 luaAllocWorldChannelLineNumbers)。
// 任何执行或解析异常都返回 error:调用方本轮不发布目录。宁可让客户端多看一轮旧目录,
// 也不能把某条线悄悄发布成 0 号或错号 —— 玩家是按线号约人的。
func allocateWorldChannelLineNumbers(svcCtx *svc.ServiceContext, zoneID uint32, confID uint64, sceneIDs []uint64) (map[uint64]uint32, error) {
	ids := sortedUniqueSceneIDs(sceneIDs)
	if len(ids) == 0 {
		return map[uint64]uint32{}, nil
	}

	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, strconv.FormatUint(id, 10))
	}
	raw, err := svcCtx.Redis.Eval(luaAllocWorldChannelLineNumbers,
		[]string{worldChannelLineNoKey(zoneID, confID)}, args...)
	if err != nil {
		return nil, fmt.Errorf("线号分配脚本执行失败: %w", err)
	}
	return parseWorldChannelLineNumbers(raw, ids)
}

// sortedUniqueSceneIDs 去掉 0 与重复值,并按 uint64 数值升序排列。
// 排序必须在 Go 侧做:Lua 里 scene_id 是字符串,字符串序与数值序不同("10…" < "9…")。
func sortedUniqueSceneIDs(sceneIDs []uint64) []uint64 {
	seen := make(map[uint64]struct{}, len(sceneIDs))
	ids := make([]uint64, 0, len(sceneIDs))
	for _, id := range sceneIDs {
		if id == 0 {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// parseWorldChannelLineNumbers 校验并解析线号分配脚本的返回值。ids 是传给脚本的那份输入。
// 要求返回值恰好覆盖每一个输入各一次、线号为正且互不相同;任何一条不满足都是 error。
func parseWorldChannelLineNumbers(raw any, ids []uint64) (map[uint64]uint32, error) {
	parts, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("线号分配脚本返回值类型非法: %T", raw)
	}
	if len(parts) != 2*len(ids) {
		return nil, fmt.Errorf("线号分配脚本返回 %d 个元素,期望 %d 个", len(parts), 2*len(ids))
	}

	expected := make(map[uint64]struct{}, len(ids))
	for _, id := range ids {
		expected[id] = struct{}{}
	}
	lineNos := make(map[uint64]uint32, len(ids))
	owners := make(map[uint32]uint64, len(ids))
	for i := 0; i < len(parts); i += 2 {
		sceneRaw, lineRaw := toString(parts[i]), toString(parts[i+1])
		sceneID, err := strconv.ParseUint(sceneRaw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("线号分配脚本返回的 scene_id 非法 %q: %w", sceneRaw, err)
		}
		if _, wanted := expected[sceneID]; !wanted {
			return nil, fmt.Errorf("线号分配脚本返回了输入之外的 scene_id %d", sceneID)
		}
		if _, dup := lineNos[sceneID]; dup {
			return nil, fmt.Errorf("线号分配脚本重复返回 scene_id %d", sceneID)
		}
		lineNo64, err := strconv.ParseUint(lineRaw, 10, 32)
		if err != nil || lineNo64 == 0 {
			return nil, fmt.Errorf("线号分配脚本给 scene_id %d 返回的线号非法 %q", sceneID, lineRaw)
		}
		lineNo := uint32(lineNo64)
		if other, clash := owners[lineNo]; clash {
			return nil, fmt.Errorf("线号分配脚本把线号 %d 同时给了 scene_id %d 与 %d", lineNo, other, sceneID)
		}
		lineNos[sceneID] = lineNo
		owners[lineNo] = sceneID
	}
	return lineNos, nil
}

// worldChannelState 判定一条线对玩家可见的状态。纯函数,不读任何外部状态。
//
// 自上而下第一条命中(world-channel-switch.md §4.3):
//
//	closing                                   → CLOSING     回收中:已摘出选线集合,等人走空
//	!nodeUsable                               → UNAVAILABLE 映射缺失 / 节点身份歧义 / 节点不存活
//	maxPlayers > 0 且 players ≥ maxPlayers     → FULL
//	maxPlayers > 0 且 players×100 ≥ maxPlayers×busyPercent → BUSY
//	其它                                      → SMOOTH
//
// maxPlayers ≤ 0 表示不设上限,此时永远不会是 FULL / BUSY。
// 只有 SMOOTH / BUSY 可以被玩家主动选中;这与 EnterScene 选线预检的口径一致
// (回收中 / 满员会被拒,见 channel_pick.go)。
//
// 乘法不会溢出的前提由调用方保证:maxPlayers 来自 Config.ChannelMaxPlayers(钳到 uint32 上限),
// busyPercent 来自 EffectiveBusyPercent([1,100]);走到乘法那一行时 players < maxPlayers。
func worldChannelState(closing, nodeUsable bool, players, maxPlayers, busyPercent int64) scenepb.SceneChannelState {
	if closing {
		return scenepb.SceneChannelState_SCENE_CHANNEL_STATE_CLOSING
	}
	if !nodeUsable {
		return scenepb.SceneChannelState_SCENE_CHANNEL_STATE_UNAVAILABLE
	}
	if maxPlayers > 0 {
		if players >= maxPlayers {
			return scenepb.SceneChannelState_SCENE_CHANNEL_STATE_FULL
		}
		if players*100 >= maxPlayers*busyPercent {
			return scenepb.SceneChannelState_SCENE_CHANNEL_STATE_BUSY
		}
	}
	return scenepb.SceneChannelState_SCENE_CHANNEL_STATE_SMOOTH
}

// worldChannelNodeUsable:这条线映射到的节点此刻能不能接人。
// 三个条件对应 resolveScene 对指定 scene_id 的进入所做的三道检查(映射存在、身份不歧义、节点存活)。
// IsNodeAlive 内部已经把歧义身份判成 false,这里仍把歧义单独写出来,是为了不依赖那个实现细节。
// 注意 IsNodeAlive 在 Redis 抖动时按存活返回(它服务的是「别误改派」),目录因此可能把一条
// 实际不可用的线短暂显示成可选 —— 只影响展示,进入时 resolveScene 会重新判定。
func worldChannelNodeUsable(svcCtx *svc.ServiceContext, zoneID uint32, nodeID string) bool {
	return nodeID != "" && !isKnownNodeIdentityAmbiguous(zoneID, nodeID) && IsNodeAlive(svcCtx, zoneID, nodeID)
}

// worldChannelDirectoryTTLSeconds 返回目录键的 TTL(秒)= 3 轮发布周期 + 一个选主竞选间隔,
// 取值理由见 worldChannelDirectoryTTLRounds。
//
// 两个入参都传取值方法的返回值:refreshSeconds = ChannelSwitch.EffectiveDirectoryRefreshSeconds()(≥ 0),
// leaderLockTTLSeconds = Config.EffectiveLeaderLockTTLSeconds()(> 0),此时结果恒 ≥ 1。
// 竞选间隔是锁 TTL 的 1/3(shared/leader Elector.Run 的 interval),这里向上取整到秒:
// 锁 TTL 不是 3 的倍数时宁可多盖不到 1 秒,也不能少盖。
func worldChannelDirectoryTTLSeconds(refreshSeconds, leaderLockTTLSeconds int64) int {
	campaignSeconds := int64(0)
	if leaderLockTTLSeconds > 0 {
		campaignSeconds = (leaderLockTTLSeconds-1)/3 + 1
	}
	if campaignSeconds > worldChannelDirectoryMaxCampaignSeconds {
		campaignSeconds = worldChannelDirectoryMaxCampaignSeconds
	}
	return int(refreshSeconds*worldChannelDirectoryTTLRounds + campaignSeconds)
}

// parseWorldChannelMembers 把 SMEMBERS 的结果解析成 scene_id,跳过非数字与 0
// (与 GetAllWorldChannels 的口径一致;坏成员由排空 sweep / 孤儿清理处理,不归列线管)。
func parseWorldChannelMembers(members []string) []uint64 {
	ids := make([]uint64, 0, len(members))
	for _, m := range members {
		id, err := strconv.ParseUint(m, 10, 64)
		if err != nil || id == 0 {
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

// clampCountToUint32 把人数转成目录里的 uint32:负数按 0,超过上限钳到上限。
func clampCountToUint32(n int64) uint32 {
	if n <= 0 {
		return 0
	}
	if n > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(n)
}

// publishWorldChannelDirectory 对一张图跑一轮:对齐线号、读各线人数与节点状态、把目录写进 Redis。
// 返回本轮的 outcome(metrics.ChannelDirectoryPublish*);outcome 为 error 时第二个返回值非 nil。
//
// 失败处理一律是「本轮不发布」,不写半份目录:
//   - 读在役 / 回收中集合失败 → 不发布,也**不动线号表**(读空与读失败必须区分,
//     否则一次 Redis 抖动会让线号全部重排);
//   - 两个集合都空 → 不发布,旧目录靠 TTL 自然过期;
//   - 线号分配失败、读 scene:{id}:node 失败、序列化 / 写入失败 → 不发布。
//
// now 由调用方传入:updated_at_ms 是目录里唯一依赖墙钟的字段,显式传入便于单测断言。
func publishWorldChannelDirectory(svcCtx *svc.ServiceContext, zoneID uint32, confID uint64, now time.Time) (string, error) {
	// 先读在役、再读回收中,顺序不能反:线只会从在役单向移到回收中(beginDrainWorldChannel)。
	// 反过来读,就算那一步是原子的,也会出现「读回收中时它还没移过去、读在役时它已经移走」而两头落空,
	// 线号随之被释放(见 luaAllocWorldChannelLineNumbers 的「已知残余」)。按现在的顺序,原子的移动
	// 最多让它两边都读到,下面按回收中处理。
	activeMembers, err := svcCtx.Redis.Smembers(worldChannelsKey(zoneID, confID))
	if err != nil {
		return metrics.ChannelDirectoryPublishError, fmt.Errorf("读在役线集合失败: %w", err)
	}
	drainingMembers, err := svcCtx.Redis.Smembers(worldDrainingSetKey(zoneID, confID))
	if err != nil {
		return metrics.ChannelDirectoryPublishError, fmt.Errorf("读回收中线集合失败: %w", err)
	}
	activeIDs := parseWorldChannelMembers(activeMembers)
	drainingIDs := parseWorldChannelMembers(drainingMembers)
	if len(activeIDs)+len(drainingIDs) == 0 {
		return metrics.ChannelDirectoryPublishEmpty, nil
	}

	// 回收中的线继续留在目录里(显示为 CLOSING)并保留线号:它上面还有人,
	// 这些人的角标要能继续显示「N线」,别的玩家也要看得到它正在关。
	// 同一个 id 同时出现在两个集合里时按回收中处理(排空集合是排空状态的权威)。
	closing := make(map[uint64]struct{}, len(drainingIDs))
	for _, id := range drainingIDs {
		closing[id] = struct{}{}
	}
	allIDs := make([]uint64, 0, len(activeIDs)+len(drainingIDs))
	allIDs = append(allIDs, activeIDs...)
	allIDs = append(allIDs, drainingIDs...)

	lineNos, err := allocateWorldChannelLineNumbers(svcCtx, zoneID, confID, allIDs)
	if err != nil {
		return metrics.ChannelDirectoryPublishError, err
	}

	cfg := svcCtx.Config.ChannelSwitch
	maxPlayers := svcCtx.Config.ChannelMaxPlayers()
	busyPercent := cfg.EffectiveBusyPercent()

	channels := make([]*scenepb.SceneChannelInfo, 0, len(lineNos))
	for sceneID, lineNo := range lineNos {
		_, isClosing := closing[sceneID]
		nodeUsable := false
		if !isClosing {
			// 回收中的线状态已定,不必再读节点映射(排空收尾时它的映射本来就会先被删)。
			nodeID, nodeErr := svcCtx.Redis.Get(sceneNodeKey(sceneID))
			if nodeErr != nil {
				return metrics.ChannelDirectoryPublishError, fmt.Errorf("读 scene %d 的节点映射失败: %w", sceneID, nodeErr)
			}
			nodeUsable = worldChannelNodeUsable(svcCtx, zoneID, nodeID)
		}
		// readScenePlayerCount 把读失败 / 负数 / 非数字都当 0:人数只用于展示和软上限,
		// 而且全仓没有对账(world-channel-switch.md §9.3),不值得为它放弃整轮发布。
		players := readScenePlayerCount(svcCtx, sceneID)
		channels = append(channels, &scenepb.SceneChannelInfo{
			SceneId:     sceneID,
			ChannelNo:   lineNo,
			PlayerCount: clampCountToUint32(players),
			State:       worldChannelState(isClosing, nodeUsable, players, maxPlayers, busyPercent),
		})
	}
	// SMEMBERS 与 map 遍历都无序;线号互不相同,按它排完顺序就是确定的。
	sort.Slice(channels, func(i, j int) bool { return channels[i].ChannelNo < channels[j].ChannelNo })

	directory := &scenepb.SceneChannelDirectory{
		ZoneId: zoneID,
		// World 表的 scene_id 本身是 uint32(worldConfIds 只是把它放宽成 uint64),不会截断。
		SceneConfigId: uint32(confID),
		Channels:      channels,
		UpdatedAtMs:   uint64(now.UnixMilli()),
		// 两个取值方法都已钳到 uint32 装得下的范围(见 config.go)。
		SwitchCooldownSeconds: uint32(cfg.EffectiveCooldownSeconds()),
		MaxPlayersPerChannel:  uint32(maxPlayers),
		SwitchEnabled:         !cfg.Disabled,
	}
	payload, err := proto.Marshal(directory)
	if err != nil {
		return metrics.ChannelDirectoryPublishError, fmt.Errorf("序列化目录失败: %w", err)
	}
	// 值是 protobuf 字节,直接当 Go string 写(Redis 字符串二进制安全;player:{id}:location 同款)。
	ttlSeconds := worldChannelDirectoryTTLSeconds(cfg.EffectiveDirectoryRefreshSeconds(),
		svcCtx.Config.EffectiveLeaderLockTTLSeconds())
	if err := svcCtx.Redis.Setex(worldChannelDirectoryKey(zoneID, confID), string(payload), ttlSeconds); err != nil {
		return metrics.ChannelDirectoryPublishError, fmt.Errorf("写目录失败: %w", err)
	}
	return metrics.ChannelDirectoryPublishOK, nil
}

// PublishWorldChannelDirectoriesForZone 对一个 zone 的全部大世界地图各发布一轮分线目录,
// 返回成功发布的地图数。导出是为了让单测直接驱动一轮(同 AutoscaleWorldChannelsForZone)。
//
// **只有领导者执行**,闸门放在这里而不是只放在循环里:线号表是读改写,虽然有 Lua 兜底,
// 目录键却是整份覆盖 —— 两个副本各写各的会让 updated_at_ms 与人数来回跳。跟着写入口走,
// 任何调用方都绕不过。
//
// 单张图失败只记日志与指标,继续下一张:一张图的线集合读不出来不该让别的图跟着没有目录。
func PublishWorldChannelDirectoriesForZone(ctx context.Context, svcCtx *svc.ServiceContext, zoneID uint32, now time.Time) int {
	if !isLeader() {
		return 0
	}
	published := 0
	for _, confID := range worldConfIds() {
		if ctx.Err() != nil {
			// 进程正在退出:剩下的图不再发布,已发布的靠 TTL 过期或由接任的领导者覆盖。
			return published
		}
		outcome, err := publishWorldChannelDirectory(svcCtx, zoneID, confID, now)
		metrics.ObserveWorldChannelDirectoryPublish(zoneID, outcome)
		if err != nil {
			logx.Errorf("[ChannelDirectory] zone=%d conf=%d: 本轮不发布分线目录: %v", zoneID, confID, err)
			continue
		}
		if outcome == metrics.ChannelDirectoryPublishOK {
			published++
		}
	}
	return published
}

// StartWorldChannelDirectoryPublisher 起分线目录的周期发布循环。
//
// 与 WorldAutoscale.Enabled 无关:自动伸缩关着,线照样存在,玩家照样要看线路列表。
// ChannelSwitch.DirectoryRefreshSeconds 配成负数时不起循环(等于关掉列线;已发布的目录靠 TTL 过期)。
func StartWorldChannelDirectoryPublisher(ctx context.Context, svcCtx *svc.ServiceContext) {
	refreshSeconds := svcCtx.Config.ChannelSwitch.EffectiveDirectoryRefreshSeconds()
	if refreshSeconds <= 0 {
		logx.Info("[ChannelDirectory] 未启动:ChannelSwitch.DirectoryRefreshSeconds < 0,不发布分线目录(客户端拿不到线路列表)")
		return
	}

	logx.Infof("[ChannelDirectory] 已启动: interval=%ds ttl=%ds max_players_per_channel=%d busy_percent=%d switch_cooldown=%ds switch_enabled=%v",
		refreshSeconds, worldChannelDirectoryTTLSeconds(refreshSeconds, svcCtx.Config.EffectiveLeaderLockTTLSeconds()),
		svcCtx.Config.ChannelMaxPlayers(),
		svcCtx.Config.ChannelSwitch.EffectiveBusyPercent(), svcCtx.Config.ChannelSwitch.EffectiveCooldownSeconds(),
		!svcCtx.Config.ChannelSwitch.Disabled)

	// safego.Loop:单轮 panic 只丢那一轮。发布停摆在外面看只是「线路列表暂不可用」,
	// 没人会联想到 goroutine,所以必须带点位名进 safego_panic_total。
	safego.Loop(ctx, SafePointWorldChannelDirectory, time.Duration(refreshSeconds)*time.Second,
		func(ctx context.Context) {
			// 先在这里挡一次只是为了让跟随者每拍不白拷一份 zone 列表;
			// 真正的闸门在 PublishWorldChannelDirectoriesForZone 里。
			if !isLeader() {
				return
			}
			now := time.Now()
			for _, zoneID := range GetActiveZones() {
				PublishWorldChannelDirectoriesForZone(ctx, svcCtx, zoneID, now)
			}
		})
}
