// Package savebarrier 定义父快照与两张 SQL 子表共用的提交屏障版本。
package savebarrier

import (
	"fmt"
	"strconv"

	databasepb "proto/common/database"
	"shared/generated/persistschema"
	"shared/redisslot"

	"google.golang.org/protobuf/proto"
)

// InitialOwnerEpoch/InitialSaveSeq 只供严格全新 ID 的目录和两张玩家 SQL 行共同提交使用。
// 这不是 actor 号源：加载先采纳此地板，首任 actor 仍由 SceneManager 从 1 铸为 2。
const InitialOwnerEpoch uint64 = 1
const InitialSaveSeq uint64 = 1

// InitialPlayerData 与初始 SQL INSERT 的列值相同，不包含注册、职业或资产等业务初值。
func InitialPlayerData(playerID uint64) *databasepb.PlayerAllData {
	return &databasepb.PlayerAllData{
		PlayerDatabaseData: &databasepb.PlayerDatabase{PlayerId: playerID, FenceOwnerEpoch: InitialOwnerEpoch,
			FenceSaveSeq: InitialSaveSeq, PersistSchemaGen: persistschema.Gen},
		PlayerDatabase_1Data: &databasepb.PlayerDatabase_1{PlayerId: playerID, FenceOwnerEpoch: InitialOwnerEpoch,
			FenceSaveSeq: InitialSaveSeq, PersistSchemaGen: persistschema.Gen},
		LastSaveSeq: InitialSaveSeq,
	}
}

// IsInitialPlayerData 拒绝额外业务内容和未知字段，不能拿任意玩家快照当成可写种子。
func IsInitialPlayerData(parent *databasepb.PlayerAllData) bool {
	return parent != nil && parent.GetPlayerDatabaseData().GetPlayerId() != 0 &&
		proto.Equal(parent, InitialPlayerData(parent.GetPlayerDatabaseData().GetPlayerId()))
}

// ParentKey 返回父快照键 PlayerAllData:{<pid>}：PlayerAllData 的 proto 全名（proto 无 package 声明，全名即消息名）
// 加玩家 hash tag（docs/design/redis-player-store-cluster.md §5.2）。与 C++ MessageAsyncClient::RedisKeyFor 逐字一致，
// C++ 严格存盘 Lua 要求父键以 ":{<pid>}" 结尾；父快照与 owner_epoch、本屏障、补投索引同槽，一次存盘在同一段 Lua 里原子提交。
// 读写父快照的服务一律经它构造，不再手拼 "<全名>:<pid>"。tools/merge_zone 是独立 module，在 player_rows.go 镜像一份。
func ParentKey(playerID uint64) string {
	return ParentKeyFor(&databasepb.PlayerAllData{}, playerID)
}

// ParentKeyFor 是按父消息类型泛化的 ParentKey，供按消息列表预热的路径使用（login dataloader.buildParentKey）。
func ParentKeyFor(msg proto.Message, playerID uint64) string {
	return string(msg.ProtoReflect().Descriptor().FullName()) + ":" + redisslot.PlayerTag(playerID)
}

// Key 不设 TTL；恢复或合服时必须与父快照、SQL 提交证据一起处理。
func Key(playerID uint64) string {
	return fmt.Sprintf("player:{%d}:save_barrier", playerID)
}

// Version 的两段各占 20 字节，中间冒号，总长 41；逐字节顺序等于整数元组顺序。
func Version(ownerEpoch, saveSeq uint64) string {
	return fmt.Sprintf("%020d:%020d", ownerEpoch, saveSeq)
}

// Parse 拒绝非规范格式和 uint64 溢出，避免不同写者对同一个令牌作不同解释。
func Parse(version string) (ownerEpoch, saveSeq uint64, err error) {
	if len(version) != 41 || version[20] != ':' {
		return 0, 0, fmt.Errorf("存盘屏障版本格式错误")
	}
	for i := range version {
		if i != 20 && (version[i] < '0' || version[i] > '9') {
			return 0, 0, fmt.Errorf("存盘屏障版本包含非数字")
		}
	}
	ownerEpoch, err = strconv.ParseUint(version[:20], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("存盘屏障归属代次越界: %w", err)
	}
	saveSeq, err = strconv.ParseUint(version[21:], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("存盘屏障存盘序号越界: %w", err)
	}
	return ownerEpoch, saveSeq, nil
}

// LuaVersionHelpers 禁用 tonumber 和受 locale 影响的字符串大小比较。
// 调用方必须在任何 Redis 写入之前校验所有输入和存量字段；脚本错误不会回滚已做的写入。
const LuaVersionHelpers = `
local function version_gt(a, b)
    for i = 1, #a do
        local x, y = string.byte(a, i), string.byte(b, i)
        if x ~= y then return x > y end
    end
    return false
end
local function is_version(s)
    if type(s) ~= 'string' or #s ~= 41 or string.sub(s, 21, 21) ~= ':'
        or not string.match(s, '^%d+:%d+$') then return false end
    local max = '18446744073709551615'
    return not version_gt(string.sub(s, 1, 20), max)
        and not version_gt(string.sub(s, 22, 41), max)
end
`
