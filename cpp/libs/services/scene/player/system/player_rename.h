#pragma once

// 角色改名系统(docs/design/rename-and-content-safety.md §4)。
//
// 强一致链路:
//   1. RequestRename: 本地预检(可写、无在途改名、规则、字数、冷却、改名卡可用数) -> ContentGuardClient 异步送检;
//   2. 送检通过回调: 复核状态 -> 铸 item hold_id -> BagService::PlaceHold(kRename) -> 写 PlayerProfileComp.rename_op
//      并递增 rename_seq -> 立即请求存盘(绝不在落盘前发 DataService.RenameRole);
//   3. OnSaveCommitted / OnPlayerLoaded: 确认 rename_op 已落盘后,带 scene ServiceCallAuth 签名发 DataService.RenameRole;
//   4. 收到结局:
//      - OK: ConsumeInventoryAndGrant(consumeFromHoldId) 扣卡 + ReleaseHold + 写 PlayerProfileComp.name / renamed_ms
//        + 清 rename_op + 存盘 + 推送 NotifyPlayerRenamed(新名与下次可改时间);
//      - 明确拒绝(TAKEN/INVALID/COOLDOWN/FROZEN/SAME_NAME/EXPIRED/STALE_SEQ/NOT_FOUND):
//        STALE_SEQ 时把 rename_seq 追平到 current_seq;一律 ReleaseHold + 清 rename_op + 存盘 + 推送失败提示;
//      - 瞬时错误(BUSY / gRPC 失败): 保留 rename_op 与 hold,由 Tick 按指数退避重发同一个 seq。
//
// 线程模型:只在 scene EventLoop 线程调用;不新起线程、不持有裸指针成员。

#include <cstdint>
#include <functional>
#include <string>

#include "engine/core/type_define/type_define.h"
#include "entt/src/entt/entity/entity.hpp"

class PlayerAllData;
class PlayerRenamedNotify;

namespace data_service
{
	class RenameRoleRequest;
	class RenameRoleResponse;
}

class PlayerRenameSystem
{
public:
	// 启动时校验 RoleNameRule 改名列与改名卡道具存在性,非法记录 ERROR。
	static bool ValidateTables();

	// 客户端 RenamePlayer RPC 入口。返回 kSuccess 表示已受理(最终结局经 NotifyPlayerRenamed 推送);
	// 非 kSuccess 表示同步拒绝,未做任何副作用。
	static uint32_t RequestRename(entt::entity player, const std::string &rawName, uint64_t nowMs);

	// 玩家从数据库加载完成:若存档里带着未完结的 rename_op,立即续发 RenameRole。
	static void OnPlayerLoaded(entt::entity player, uint64_t nowMs);

	// 存盘提交完成:若已提交快照带有 rename_op 且与内存一致,发出 RenameRole。
	static void OnSaveCommitted(Guid playerId, const PlayerAllData &committed, uint64_t nowMs);

	// 玩家实体销毁:清理运行期在途/退避表项(持久化 rename_op 留在存档中,下次登录由 OnPlayerLoaded 续做)。
	static void OnPlayerDestroyed(Guid playerId);

	// 1 秒节拍扫描到期的瞬时故障重试。
	static void Tick(uint64_t nowMs);

	// ── 测试注入接缝(生产代码不调用;置空即恢复默认生产实现) ──
	using RenameRoleDone = std::function<void(bool callOk, const data_service::RenameRoleResponse *reply)>;
	using RenameRoleTransport = std::function<bool(const data_service::RenameRoleRequest &request, RenameRoleDone done)>;
	using NotifySender = std::function<void(entt::entity player, const PlayerRenamedNotify &notify)>;
	using SaveRequester = std::function<bool(entt::entity player)>;
	using HoldIdGenerator = std::function<bool(Guid &outHoldId)>;

	struct TestHooks
	{
		RenameRoleTransport transport;
		NotifySender notify;
		SaveRequester save;
		HoldIdGenerator nextHoldId;
	};
	static void SetTestHooks(TestHooks hooks);
	static void ResetTestState();
};
