#pragma once

#include <cstdint>

// 存盘序号的个人下限(docs/design/db-write-fencing-mysql.md §7.2,§14 第 13 条方案 2)。
//
// 按需状态,只为「加载时种子超界」的玩家存在;没有它 = 该玩家没有个人下限,发号只看进程水位。
//   创建:加载链 InitPlayerFromAllData —— save_seq::SaveSeqAllocator::Observe 返回 kBeyondBound 时 emplace,
//         floor = 那个超界的种子(认领返回的 last_save_seq);
//         新路径的 OLDER_SEQ 处置(PlayerLifecycleSystem::OnPlayerStoreSaveDone)—— 库里的序号同样超界时,floor 取 max。
//   读写:SavePlayerImpl 发号时 try_get,有则发号取 max(进程 Next, floor + 1)(save_seq::ApplyPersonalFloor),
//         发出后把 floor 推进到发出的值;floor == UINT64_MAX 时显式拒绝存盘,不回绕。其他路径不得创建或修改它。
// 纯运行时:不入 PlayerAllData、不序列化,随实体销毁回收。实体重建时由加载重新得到 —— 父键里的 last_save_seq 是按个人
// 下限发出的值,仍然超界;等墙钟追上(种子落进上界)之后它被进程水位正常采纳,就不再创建本组件。
struct PlayerSaveSeqFloorComp
{
	uint64_t floor{0};
};
