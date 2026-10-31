#pragma once

// 落地恢复的 scene 侧:读版本摘要推给客户端,以及进场完成后发布资产待办唤醒。
// 设计:docs/design/landing-recovery-sync-digest.md §4.2(取摘要)、§5.2(唤醒发布)。
//
// 职责边界:
//   - 摘要的权威写方是 Go 侧 go/shared/playersync(BumpScript)。scene 只读、只转发,不写摘要,
//     也不把 scene 自己持有的数据(背包、货币、宠物、属性、任务)放进摘要(设计 §3.4)。
//   - 唤醒只是提示:丢了退回资产通道原有的退避重投,不影响正确性(设计 L-4)。
//
// Redis 契约(改一边必须同步改另一边):
//   player:{player_id}:sync_digest  玩家域(tlsPlayerRedis),hash
//                                         g          = 代次(hash 创建时刻的 Redis 服务器毫秒,只在 hash 不存在时写)
//                                         "<域号十进制>" = 该域的变更计数(HINCRBY)
//   assetop:wake                    控制域(tlsRedis),Pub/Sub 频道,载荷 = 十进制 player_id
// kSyncDigestKeyFmt 由 Go 测试逐字解析比对(沿用 owner_epoch_crosslang_test.go 的 cppStringConstant:
// 它在文件里找第一处「星号 + 名字 + 空格 + 等号」,再取其后的字符串字面量),所以声明必须保持
// "inline constexpr const char 指针" 的写法,名字与值都不要改;注释里也不要写出那个模式,否则会先被它匹配到。
//
// 线程模型:所有方法只在 scene 节点 loop 线程调用;hiredis 回调同样在该线程执行。
// 都由事件触发(客户端请求 / 进场完成),不在 per-tick 路径上。

#include <charconv>
#include <cstdint>
#include <limits>
#include <string>
#include <system_error>
#include <utility>
#include <vector>

#include "entt/src/entt/entity/entity.hpp"

#include "proto/scene/player_scene.pb.h"

namespace player_landing
{
	// 摘要 hash 的键;%llu = player_id。与 Go 侧 playersync 的 "player:{%d}:sync_digest" 是同一份契约。
	inline constexpr const char *kSyncDigestKeyFmt = "player:{%llu}:sync_digest";

	// 摘要 hash 里代次字段的名字;其余字段名都是域号十进制。
	inline constexpr const char *kSyncDigestGenerationField = "g";

	// 资产待办唤醒频道;guild / trade 的 assetopwake 订阅它。
	inline constexpr const char *kAssetWakeChannel = "assetop:wake";

	// 同一实体两次唤醒之间的最小间隔(毫秒,单调时钟)。
	inline constexpr uint64_t kAssetWakeCoalesceMs = 5000;
} // namespace player_landing

class PlayerLandingSystem
{
public:
	// ---- 纯函数(无 I/O、无 ECS,头文件内联,供 cpp/tests/player_landing_test 直接单测,
	//      测试不必链接 scene 库)----

	// 把 HGETALL player:{id}:sync_digest 的字段名 / 值对解析进 *out(设计 §4.2 第 4 步)。
	//   - "g"                    → generation;
	//   - 能解析成已知 SyncDomain 的字段(规范十进制、> 0、是已定义的枚举值)→ entries,保持输入顺序;
	//   - 其余字段一律视为未知并跳过,值也不校验(新服务端先上、旧 scene 未升级时字段会多于枚举)。
	// 返回 false = 应答畸形,调用方必须推 UNAVAILABLE(客户端全量拉),此时 *out 已被清空:
	//   - g 或已知域的值不是十进制无符号整数(空串、负号、空白、溢出都算);
	//   - g 或同一个已知域出现两次(HGETALL 不会产生,出现即输入契约被破坏);
	//   - hash 非空却没有 g,或 g 为 0。BumpScript 在同一段原子脚本里先起种 g 再写域计数,
	//     非空 hash 必有 g(> 0);缺 g 说明 hash 不是它写的,L-3「重建必换 g」的保证不成立,
	//     不能把这些计数当真。
	// 空列表(键不存在:从未 bump 或已过期)是合法的:generation = 0、entries 为空,返回 true。
	// 返回 true 时 status = OK。out 为 nullptr 时返回 false。
	static bool ParseSyncDigestFields(const std::vector<std::pair<std::string, std::string>> &fields,
									  SyncDigestS2C *out)
	{
		if (out == nullptr)
		{
			return false;
		}
		out->Clear();

		bool hasGeneration = false;
		uint64_t generation = 0;
		for (const auto &[name, value] : fields)
		{
			if (name == player_landing::kSyncDigestGenerationField)
			{
				if (hasGeneration || !ParseDecimalUint64(value, generation))
				{
					out->Clear();
					return false;
				}
				hasGeneration = true;
				continue;
			}

			const SyncDomain domain = KnownDomainOf(name);
			if (domain == SYNC_DOMAIN_UNSPECIFIED)
			{
				continue;
			}
			uint64_t version = 0;
			if (!ParseDecimalUint64(value, version) || HasDomain(*out, domain))
			{
				out->Clear();
				return false;
			}
			auto *entry = out->add_entries();
			entry->set_domain(domain);
			entry->set_version(version);
		}

		if (!fields.empty() && (!hasGeneration || generation == 0))
		{
			out->Clear();
			return false;
		}
		out->set_status(SYNC_DIGEST_STATUS_OK);
		out->set_generation(generation);
		return true;
	}

	// ---- 客户端请求 ----

	// GetSyncDigest 的实现(handler 只委托到这里,设计 §4.2 第 1–5 步):
	//   1. Redis 未就绪 → 立即推 UNAVAILABLE;
	//   2. 按值捕获 player_id 发 HGETALL;命令没被 hiredis 收下(返回值 != REDIS_OK)时回调不会被调,就地推 UNAVAILABLE;
	//   3. 回调里按 player_id 重新找实体(实体可能已销毁或换人),找不到就丢弃(客户端会超时重试);
	//   4. ParseSyncDigestFields 解析,应答畸形 → UNAVAILABLE 并打 ERROR;
	//   5. 推 NotifySyncDigest。
	// 除"实体无效 / 回调时玩家已不在本节点"外,每次调用都恰好推一条 NotifySyncDigest,客户端靠它结束等待。
	// 应答类型是 Empty，没有 error_message 字段，结果只经 NotifySyncDigest 传达。
	static void SendSyncDigest(entt::entity player);

	// ---- 场景生命周期入口 ----

	// 玩家进场完成之后调用(PlayerLifecycleSystem::EnterScene 末尾、PlayerBattleSystem::OnPlayerEnterScene 之后,
	// 挂点由批次 D3 加,设计 §5.1)。新载入、复用实体、gate 补发的重复路由三条路径都经过那里。
	// enterGsType 是 EnterScene 的 enter_gs_type,目前只进日志、不据此分支:跨 zone 第二条腿是 RECONNECT 还是
	// REPLACE 说法不一(设计 §5.1),唤醒对所有取值都该发。
	static void OnEnteredScene(entt::entity player, uint32_t enterGsType);

	// 唤醒来源。合并窗口只压同一来源的重复触发(进场被反复调用、gate 补发的重复路由),
	// 不能拦战斗解冻那一次:典型场景是重连复用实体时玩家仍在战斗中,进场唤醒撞上 kAssetInBattle 回 RETRY,
	// 几秒后战斗结束,解冻唤醒正是为了兜住这一刻;若被同一个 5 秒窗口吞掉,它就失去了唯一要覆盖的场景。
	enum class AssetWakeReason : uint8_t
	{
		kEnteredScene,
		kBattleFreezeCleared,
	};

	// 发布资产待办唤醒:PUBLISH assetop:wake <player_id 十进制>(设计 §5.2)。
	//   - 实体带 PlayerFrozenComp(归属交接在途)时跳过:资产写路径此时本来就会拒绝;
	//   - 只有 kEnteredScene 走合并窗口:同一实体 kAssetWakeCoalesceMs 内只发一次(PlayerLandingWakeComp);
	//     kBattleFreezeCleared 直接发送,也不刷新窗口;
	//   - Redis 未就绪 / 命令没发出去只打 WARN,不重试(唤醒丢了退回原退避);回调只在出错时打 WARN。
	// 调用点:OnEnteredScene(kEnteredScene);战斗冻结解除,即 player_battle.cpp 的 RemoveInBattleComp(kBattleFreezeCleared)。
	static void PublishAssetWake(entt::entity player, AssetWakeReason reason);

private:
	// 整串都是十进制无符号整数才返回 true(不接受空串、符号、空白、溢出)。
	static bool ParseDecimalUint64(const std::string &text, uint64_t &out)
	{
		if (text.empty())
		{
			return false;
		}
		const char *begin = text.data();
		const char *end = text.data() + text.size();
		uint64_t parsed = 0;
		const auto [ptr, ec] = std::from_chars(begin, end, parsed, 10);
		if (ec != std::errc{} || ptr != end)
		{
			return false;
		}
		out = parsed;
		return true;
	}

	// 字段名 → 已知域;不是已知域时返回 SYNC_DOMAIN_UNSPECIFIED。
	// 只认规范十进制(不带前导零):BumpScript 写的就是规范形式,接受 "04" 会让同一个域有两种写法。
	// 判据跟着生成的 SyncDomain_IsValid 走,不写死 [1, 9]:枚举只追加,会长。
	static SyncDomain KnownDomainOf(const std::string &name)
	{
		uint64_t number = 0;
		if (!ParseDecimalUint64(name, number) || (name.size() > 1 && name.front() == '0') ||
			number > static_cast<uint64_t>(std::numeric_limits<int>::max()))
		{
			return SYNC_DOMAIN_UNSPECIFIED;
		}
		const int value = static_cast<int>(number);
		if (value == SYNC_DOMAIN_UNSPECIFIED || !SyncDomain_IsValid(value))
		{
			return SYNC_DOMAIN_UNSPECIFIED;
		}
		return static_cast<SyncDomain>(value);
	}

	static bool HasDomain(const SyncDigestS2C &digest, SyncDomain domain)
	{
		for (const auto &entry : digest.entries())
		{
			if (entry.domain() == domain)
			{
				return true;
			}
		}
		return false;
	}
};
