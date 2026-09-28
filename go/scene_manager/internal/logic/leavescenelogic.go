package logic

import (
	"context"
	"fmt"

	"proto/common/base"
	"proto/scene_manager"
	"scene_manager/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

// luaDeletePlayerLocationIfUnchanged:location 仍是调用方读到的那份原文时,删掉它并在同一次
// 提交里把人数减一(GO-4 残余)。
//
// KEYS[1]=player:{id}:location  KEYS[2]=instance:{sceneId}:player_count
// KEYS[3]=node:zone:{z}:{node}:player_count(scene:{id}:node 读不到时不传,脚本按 #KEYS 处理)
// ARGV[1]=调用方 GET 到、并据此核对过 SceneId 的 location 原文
// 返回 1=已删且人数已减;0=键已不在;-1=已被改写,什么都没动
//
//   - 为什么比对原文而不只比 SceneId:原文里带 node_id / owner_epoch / update_time,同一场景里
//     换了节点或铸了新 epoch 的新落点也能认出来。只比 SceneId 的裸 DEL 会把 GET 之后并发
//     EnterScene 写入的新落点一起删掉。
//   - 为什么人数也放进脚本:go-redis 读超时后会把同一条 EVAL 原样重发(与 owner_epoch.go 的
//     重放识别同一原因)。首发已删、应答丢失时,重发只能看到「键已不在」;如果减人数留在脚本外、
//     又按「不是本次删的就不减」处理,就没有任何一方减过,实例人数永久多 1,destroy-on-empty
//     永不触发。另一种情况是 go-redis 重试耗尽后返回错误,player_locator 再次调用时 GET 为空。
//     把删与减绑成同一次提交,「谁删谁减」对这两种情况都成立。
//   - 钳 0 与 DecrInstancePlayerCount 一致;键全部由 Go 构造、经 KEYS 声明,与 scene_atomic.go
//     同一约定(脚本从不用数据拼键)。
const luaDeletePlayerLocationIfUnchanged = `
local cur = redis.call('GET', KEYS[1])
if cur == false then
    return 0
end
if cur ~= ARGV[1] then
    return -1
end
redis.call('DEL', KEYS[1])
for i = 2, #KEYS do
    local v = redis.call('DECR', KEYS[i])
    if v < 0 then
        redis.call('SET', KEYS[i], '0')
    end
end
return 1
`

// deletePlayerLocationIfUnchanged 的三种结果(只用于日志与单测,不进指标 label)。
const (
	leaveOutcomeDeleted     = "deleted"
	leaveOutcomeAlreadyGone = "already_gone"
	leaveOutcomeSuperseded  = "superseded"
)

// deletePlayerLocationIfUnchanged 在 location 仍等于 expectedRaw 时删掉它,并在同一段 Lua 里把
// 场景人数(以及此刻 scene:{id}:node 指向的节点人数)减一。
//
// 节点人数键按**此刻**的 scene:{id}:node 找,找不到就只减场景计数 —— 与 DecrInstancePlayerCount
// 同一口径,也同样不原子:scene:{id}:node 在读与 EVAL 之间被改派时,节点人数最多偏差一次。
func deletePlayerLocationIfUnchanged(svcCtx *svc.ServiceContext, playerID uint64, expectedRaw string,
	zoneID uint32, sceneID uint64) (string, error) {
	keys := []string{getPlayerLocationKey(playerID), fmt.Sprintf(InstancePlayerCountKey, sceneID)}
	if node := lookupSceneNode(svcCtx, sceneID); node != "" {
		keys = append(keys, nodePlayerCountKey(zoneID, node))
	}

	result, err := svcCtx.Redis.Eval(luaDeletePlayerLocationIfUnchanged, keys, expectedRaw)
	if err != nil {
		return "", fmt.Errorf("比对删除 location 失败: player=%d scene=%d: %w", playerID, sceneID, err)
	}
	switch fmt.Sprint(result) {
	case "1":
		return leaveOutcomeDeleted, nil
	case "0":
		return leaveOutcomeAlreadyGone, nil
	case "-1":
		return leaveOutcomeSuperseded, nil
	}
	return "", fmt.Errorf("比对删除 location 返回值非法: player=%d scene=%d result=%v", playerID, sceneID, result)
}

type LeaveSceneLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewLeaveSceneLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LeaveSceneLogic {
	return &LeaveSceneLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// LeaveScene handles a player leaving a scene (or pre-transfer cleanup).
//
// 删除是「比对原文」的条件删除,删 location 与减人数在同一段 Lua 里提交(见
// luaDeletePlayerLocationIfUnchanged)。LeaveScene 不再单独调用 DecrInstancePlayerCount:
// 删掉它的那一方在脚本里减过;改写它的 EnterScene 已减过旧场景人数。
//
// 错误语义:Redis 读失败或删除失败返回 error(以前回成功)。依据是唯一调用方 player_locator 的
// 契约(leasemonitor.go 的租约清理:RPC 失败必须返回错误,出错时保留 claim 重试;MarkOffline
// 同步调用失败才入租约队列,cleanup-pending 屏障随之生效)。旧行为是「Redis 抖一下 → ACK →
// location 永久残留」。
//
// 挡不住的两类(被删之后,下一次换图会被当成首次落点):
//   - samePlacement 重连(enterscenelogic.go)不写 location,值没变,照样会被删;
//   - 同一场景的新落点在本次 GET 之前就已经写进去了。
//
// 根治在 player_locator(MarkOffline 的同步 LeaveScene 也处在 cleanup-pending 屏障之下),或在
// proto 里记会话 / 落点代际,不在本函数范围。
func (l *LeaveSceneLogic) LeaveScene(in *scene_manager.LeaveSceneRequest) (*base.Empty, error) {
	raw, err := l.svcCtx.Redis.Get(getPlayerLocationKey(in.PlayerId))
	if err != nil {
		l.Logger.Errorf("Player %d leave scene %d: read location failed, returning error so the caller keeps its claim and retries: %v",
			in.PlayerId, in.SceneId, err)
		return nil, fmt.Errorf("read location of player %d: %w", in.PlayerId, err)
	}
	if raw == "" {
		l.Logger.Infof("Player %d requested leave scene %d but has no location info", in.PlayerId, in.SceneId)
		return &base.Empty{}, nil
	}

	currentLoc := &scene_manager.PlayerLocation{}
	if err := proto.Unmarshal([]byte(raw), currentLoc); err != nil {
		// 写坏的记录不是瞬时故障:回错误会让 claim 变成永远重试的毒任务,而且没法核对 SceneId,
		// 所以不删、回成功(与旧行为一致;EnterScene 对写坏的记录同样 fail-closed)。
		l.Logger.Errorf("Player %d leave scene %d: location record is corrupt, leaving it untouched: %v",
			in.PlayerId, in.SceneId, err)
		return &base.Empty{}, nil
	}

	if currentLoc.SceneId != in.SceneId {
		l.Logger.Infof("Player %d requested leave scene %d but is in scene %d, ignoring",
			in.PlayerId, in.SceneId, currentLoc.SceneId)
		return &base.Empty{}, nil
	}

	outcome, err := deletePlayerLocationIfUnchanged(l.svcCtx, in.PlayerId, raw, currentLoc.ZoneId, in.SceneId)
	if err != nil {
		l.Logger.Errorf("Player %d leave scene %d: conditional delete failed, returning error for retry: %v",
			in.PlayerId, in.SceneId, err)
		return nil, err
	}
	if outcome != leaveOutcomeDeleted {
		// already_gone:别的调用(或 go-redis 重发的首发)已经删过并在同一段脚本里减过人数;
		// superseded:GET 之后有 EnterScene 写入了新落点,它已减过旧场景人数。两种都不动人数。
		l.Logger.Infof("Player %d leave scene %d: location not deleted by this call (outcome=%s), player counts untouched",
			in.PlayerId, in.SceneId, outcome)
		return &base.Empty{}, nil
	}

	l.Logger.Infof("Player %d left scene %d on node %s", in.PlayerId, in.SceneId, currentLoc.NodeId)
	return &base.Empty{}, nil
}
