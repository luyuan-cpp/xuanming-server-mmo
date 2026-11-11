package logic

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"scene_manager/internal/svc"

	"github.com/google/uuid"
	"github.com/zeromicro/go-zero/core/logx"
)

// Agones rooms 名额在 Redis 一侧的账(docs/design/agones-scene-node-high-density.md §13)。
//
// 名额是 GameServer 上 rooms 列表里的一个条目。每次占用一个条目(建场景、改派转移、对账补转各占自己的),
// 值是 newAgonesRoomEntry 生成的、全局唯一的串。Redis 里记两样:
//   - 记录 scene:{id}:agones_gs = "<GameServer 名>/<条目>"(agonesRoom):这个场景占的是哪台上的哪一条。
//     由登记脚本(scene_registration.go)或改派转移的 CAS(luaCommitAgonesRoomTransfer)写,认领脚本在销毁的
//     同一步读出、删掉,交给拆除任务去删条目。
//   - 在途占用 agones_rooms:zone:{z}:holds(有序集合,成员是条目,分数是截止时刻 Unix 秒):向 Agones 要名额
//     **之前**先写。把名额写进记录的脚本在同一步 ZREM 它,删到了才写记录;删不到说明已被对账取消,一个键都不写。
//
// 一个条目该留着,当且仅当下面三条之一成立,其余都是孤儿,对账删掉(agones_reconcile.go):
//   - 记录指向它;
//   - 场景的拆除任务快照指向它(场景已销毁,等拆除任务删,保持「实体销毁之后才还名额」的顺序);
//   - 它的在途占用还没过截止时刻。
// 对账取消过期的在途占用与判孤儿在同一段脚本里,取消之后晚到的提交一律被挡住,所以截止时刻只决定多久以后可以
// 回收,不影响正确性。

// agonesRoomHoldTTL 是在途占用的存活时间。要长过从写在途占用到提交的最慢正常路径:分配(K8s 请求 5s,
// 加可能的一次 Pod GET),加登记 EVAL 的 go-redis 重发(最坏约 33s),再加一次作废查询。慢过它的提交只会
// 被挡住(建场景失败、转移落空),不会记错。
const agonesRoomHoldTTL = 5 * time.Minute

// agonesRoomHoldsKey 是本 zone 的在途占用。
func agonesRoomHoldsKey(zoneID uint32) string {
	return fmt.Sprintf("agones_rooms:zone:%d:holds", zoneID)
}

// newAgonesRoomEntry 生成一次占用的条目:"<scene_id>.<32 位十六进制随机串>"。
//
// 不用裸 scene_id:同一个场景的两路操作可能落在同一台 GameServer 上(改派落空、发现属主恰好是自己挑的节点时补转,
// A→B→A 来回改派)。两路共用一个值时,一路「删掉自己刚加的」会删掉另一路正在用的那条。每次占用各有一条,谁都
// 只删自己的。scene_id 放在前面,对账才能从条目找到它的记录。
func newAgonesRoomEntry(sceneID uint64) string {
	return strconv.FormatUint(sceneID, 10) + "." + strings.ReplaceAll(uuid.NewString(), "-", "")
}

// agonesRoomEntrySceneID 解出条目里的 scene_id。不是 newAgonesRoomEntry 的格式时回 false。
func agonesRoomEntrySceneID(entry string) (uint64, bool) {
	idPart, token, found := strings.Cut(entry, ".")
	if !found || token == "" {
		return 0, false
	}
	sceneID, err := strconv.ParseUint(idPart, 10, 64)
	if err != nil || sceneID == 0 {
		return 0, false
	}
	return sceneID, true
}

// agonesRoom 是一个场景占的名额:哪台 GameServer 上的哪一条。
// Entry 为空表示非高密度模式:不记名额,记录只有 GameServer 名,归还是空操作。
type agonesRoom struct {
	GameServer string
	Entry      string
}

// agonesRoomRecordSep 分开记录里的 GameServer 名与条目。GameServer 名是 DNS 子域名,不含 "/"。
const agonesRoomRecordSep = "/"

// record 是写进 scene:{id}:agones_gs 的值。
func (r agonesRoom) record() string {
	if r.Entry == "" {
		return r.GameServer
	}
	return r.GameServer + agonesRoomRecordSep + r.Entry
}

// parseAgonesRoom 解 scene:{id}:agones_gs 的值。空串解出空的 agonesRoom。
func parseAgonesRoom(record string) agonesRoom {
	gs, entry, _ := strings.Cut(record, agonesRoomRecordSep)
	return agonesRoom{GameServer: gs, Entry: entry}
}

// holdAgonesRoom 写在途占用,截止时刻 = 现在 + agonesRoomHoldTTL。回错误时调用方不能去要名额。
func holdAgonesRoom(svcCtx *svc.ServiceContext, zoneID uint32, entry string) error {
	_, err := svcCtx.Redis.Zadd(agonesRoomHoldsKey(zoneID), time.Now().Add(agonesRoomHoldTTL).Unix(), entry)
	return err
}

// dropAgonesRoomHold 删在途占用,用在决定不提交的时候。删不掉只记日志:截止时刻一过,对账会取消它、收掉条目。
func dropAgonesRoomHold(svcCtx *svc.ServiceContext, zoneID uint32, entry string) {
	if _, err := svcCtx.Redis.Zrem(agonesRoomHoldsKey(zoneID), entry); err != nil {
		logx.Errorf("[AgonesRooms] zone %d: dropping the hold of room entry %s failed; the reconciler cancels it after its deadline: %v",
			zoneID, entry, err)
	}
}

// luaJudgeAgonesRoomEntry 判 GameServer 上的一个条目该不该留着,见文件头。过期的在途占用在同一步取消,
// 之后晚到的提交读不到它就不写记录。
//
// KEYS[1] = scene:{id}:zone,KEYS[2] = scene:{id}:agones_gs,KEYS[3] = scene:{id}:teardown,KEYS[4] = 本 zone 的在途占用。
// ARGV[1] = 本 zone,ARGV[2] = 「GameServer/条目」,ARGV[3] = 条目,ARGV[4] = 现在(Unix 秒)。
// 拆除任务的 reply 是认领应答 {状态, 属主, agones_gs, 镜像源, 镜像标记, 成员...}(scene_atomic.go),第 3 项是记录。
const luaJudgeAgonesRoomEntry = `
local zone = redis.call('GET', KEYS[1])
if zone and zone ~= ARGV[1] then
    return 'foreign'
end
if redis.call('GET', KEYS[2]) == ARGV[2] then
    return 'recorded'
end
local reply = redis.call('HGET', KEYS[3], 'reply')
if reply then
    local snapshot = cjson.decode(reply)
    if snapshot[3] == ARGV[2] then
        return 'releasing'
    end
end
local deadline = redis.call('ZSCORE', KEYS[4], ARGV[3])
if deadline then
    if tonumber(deadline) > tonumber(ARGV[4]) then
        return 'held'
    end
    redis.call('ZREM', KEYS[4], ARGV[3])
    return 'hold_expired'
end
return 'orphan'
`

// agonesRoomVerdict 是 luaJudgeAgonesRoomEntry 的结论。
type agonesRoomVerdict string

const (
	// agonesRoomForeign:场景登记在别的 zone,它的键只由那个 zone 读写,不碰。
	agonesRoomForeign agonesRoomVerdict = "foreign"
	// agonesRoomRecorded:记录指向这一条,在用。
	agonesRoomRecorded agonesRoomVerdict = "recorded"
	// agonesRoomReleasing:场景已销毁,拆除任务快照指向这一条,等拆除任务删。
	agonesRoomReleasing agonesRoomVerdict = "releasing"
	// agonesRoomHeld:在途占用还没过截止时刻,提交可能还在路上。
	agonesRoomHeld agonesRoomVerdict = "held"
	// agonesRoomHoldExpired:在途占用已过截止时刻,已在同一步取消;条目按孤儿删。
	agonesRoomHoldExpired agonesRoomVerdict = "hold_expired"
	// agonesRoomOrphan:没有记录、拆除任务、在途占用指向它,删。
	agonesRoomOrphan agonesRoomVerdict = "orphan"
)

// reclaimable 说明这个结论下条目该删。
func (v agonesRoomVerdict) reclaimable() bool {
	return v == agonesRoomOrphan || v == agonesRoomHoldExpired
}

// judgeAgonesRoomEntry 跑 luaJudgeAgonesRoomEntry。认不出的应答回错误,调用方按读不到处理(不删)。
func judgeAgonesRoomEntry(svcCtx *svc.ServiceContext, zoneID uint32, sceneID uint64, room agonesRoom, now time.Time) (agonesRoomVerdict, error) {
	keys := []string{sceneZoneKey(sceneID), sceneAgonesGsKey(sceneID), sceneTeardownJobKey(sceneID), agonesRoomHoldsKey(zoneID)}
	raw, err := svcCtx.Redis.Eval(luaJudgeAgonesRoomEntry, keys,
		strconv.FormatUint(uint64(zoneID), 10), room.record(), room.Entry, now.Unix())
	if err != nil {
		return "", err
	}
	verdict := agonesRoomVerdict(toString(raw))
	switch verdict {
	case agonesRoomForeign, agonesRoomRecorded, agonesRoomReleasing, agonesRoomHeld, agonesRoomHoldExpired, agonesRoomOrphan:
		return verdict, nil
	}
	return "", fmt.Errorf("unexpected verdict %v for room entry %s", raw, room.Entry)
}

// luaCancelExpiredAgonesRoomHold 取消一个已过截止时刻的在途占用。KEYS[1] = 本 zone 的在途占用,
// ARGV[1] = 条目,ARGV[2] = 现在(Unix 秒)。回 1:取消了;回 0:不在,或还没到截止时刻。
const luaCancelExpiredAgonesRoomHold = `
local deadline = redis.call('ZSCORE', KEYS[1], ARGV[1])
if deadline and tonumber(deadline) <= tonumber(ARGV[2]) then
    redis.call('ZREM', KEYS[1], ARGV[1])
    return 1
end
return 0
`

// cancelExpiredAgonesRoomHold 跑 luaCancelExpiredAgonesRoomHold,回是否取消了。
func cancelExpiredAgonesRoomHold(svcCtx *svc.ServiceContext, zoneID uint32, entry string, now time.Time) (bool, error) {
	raw, err := svcCtx.Redis.Eval(luaCancelExpiredAgonesRoomHold, []string{agonesRoomHoldsKey(zoneID)}, entry, now.Unix())
	if err != nil {
		return false, err
	}
	return toInt64(raw) == 1, nil
}
