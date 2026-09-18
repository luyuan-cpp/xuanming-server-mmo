#pragma once

#include <unordered_map>

#include "muduo/net/TcpConnection.h"
#include "engine/core/type_define/type_define.h"
#include "message_limiter/message_limiter.h"

struct SessionInfo
{
	// Entity IDs are uint64_t (ENTT_ID_TYPE=uint64_t with version bits).
	// Using uint64_t avoids truncation that loses entity version/generation,
	// which causes registry.valid() to reject recycled entities.
	//
	// Naming note: these methods are named Entity*-Id (not Node*-Id) because
	// what we track per nodeType is the local entt::entity integer for the
	// remote node — NOT the business `node_id` on that node. Post uuid-primary-
	// key refactor of node_connector, entity and node_id are no longer equal,
	// so using "EntityId" terminology prevents a common footgun where callers
	// would assume the returned value can be fed into `entt::entity{...}` and
	// land on a valid slot without going through a FindNodeEntity* helper.
	static constexpr uint64_t kInvalidEntityId{UINT64_MAX};
	using NodeEntityMap = std::unordered_map<uint32_t, uint64_t>;

	SessionInfo() = default;

	void SetEntityId(uint32_t nodeType, uint64_t entityId)
	{
		entityIds[nodeType] = entityId;
	}

	uint64_t GetEntityId(uint32_t nodeType) const
	{
		auto it = entityIds.find(nodeType);
		if (it != entityIds.end())
		{
			return it->second;
		}
		return kInvalidEntityId;
	}

	bool HasEntityId(uint32_t nodeType) const
	{
		auto it = entityIds.find(nodeType);
		if (it == entityIds.end())
		{
			return false;
		}
		return it->second != kInvalidEntityId;
	}

	Guid playerId{kInvalidGuid};
	// weak_ptr(AGENTS.md §11.7:应用层不拥有连接):客户端连接由 gate 的 TcpServer 拥有,会话表只是索引。
	// 会话在 DOWN 回调里同步 erase、TcpServer 在那之后才放手,所以稳态下记录存在时 lock() 必成功;
	// 改 weak 是为了任何路径(含关机)都不可能把死连接连 fd 钉在 thread_local 的会话表里。
	std::weak_ptr<muduo::net::TcpConnection> conn;
	MessageLimiter messageLimiter;
	uint32_t sessionVersion{UINT32_MAX};
	uint32_t pendingEnterGsType{0}; // Pending login type to forward to Scene once scene node is assigned
	uint64_t sceneId{0};            // Scene instance GUID from SceneManager (RoutePlayerEvent)

	// 跨 zone 归属与所有权 epoch(cross-zone-scene-travel.md CZ-3 / CZ-4,scene-owner-reentry-barrier.md §3.3)。
	// 两项都来自 RoutePlayerEvent,由 scene_manager 在每次路由决策时下发;gate 不生产、不校验,只透传进
	// PlayerEnterGameNodeRequest。之所以要存进会话而不是在 RoutePlayer 里用完即弃:向 scene 的转发有两个入口
	// (RoutePlayerEventHandler / BindSessionEventHandler),BindSession 晚到时的补发读不到路由事件,只能从会话取。
	// 同一会话再次收到 RoutePlayerEvent 时整体覆盖:每次改派都铸新 epoch,旧值留着只会让补发带上已被废黜的 epoch。
	// 会话销毁即随之消失,不需要额外清理路径。
	uint32_t homeZoneId{0}; // 玩家归属 zone;0 = 未知(旧版 scene_manager 未填),scene 侧 fail-closed 落进程 zone 并计数
	uint64_t ownerEpoch{0}; // 本次路由的归属 epoch;0 = 旧版未铸造(兼容窗口),scene 侧存盘跳过 CAS 并计数

	// 重定向票据的持票者与目标 zone(cross-zone-scene-travel.md CZ-8)。DispatchTokenVerify 验签通过后
	// 写入,会话存续期内不变;gate 只把它们透传进 SessionDetails,主判定在 login EnterGame
	// (那是 player_id 已知、且尚未写任何会话状态的最早时刻;gate 自己要到 BindSession 才知道
	// player_id,那时 login 已经落了 ONLINE 会话,在 gate 拒只会留下残留)。
	// 默认值用 0 而不是 kInvalidGuid:与 proto 的"0 = 未填 / 普通票据"同形,login 对 0 一律放行。
	Guid ticketPlayerId{0};
	uint32_t ticketTargetZoneId{0};
	bool verified{false};			// True after client passes Gate connection token verification
	// True once Gate actually dispatches ClientPlayerLogin.Login to Login.
	// Disconnect uses this to clean Login's session-id state without turning
	// every verified-but-idle TCP close into a cross-service RPC.
	bool loginStarted{false};

	// Illegal-packet counter (todo.md #236). Increment whenever a packet is
	// rejected by the gate's incoming validation chain (rate-limit exceeded,
	// unknown messageId, malformed body, unauthenticated send, HMAC mismatch
	// once #76 lands). Hits the threshold → forceClose. Resets to 0 on a
	// fresh ClientTokenVerify so a transient burst doesn't poison the next
	// reconnect; otherwise monotonically increasing for the connection's
	// lifetime. Thresholds tuned per deployment via env / config (see
	// IllegalPacketCounter::ThresholdFromEnv).
	uint32_t illegalPacketCount{0};

	// Per-session HMAC key for message signing (todo.md #76 slice A).
	//
	// Populated by client_message_processor.cpp::HandleClientTokenVerify
	// AFTER the gate token signature is verified, by unmarshalling the
	// inner GateTokenPayload and copying its `hmac_session_key` field
	// here. Empty bytes mean "client/server pair hasn't rolled out the
	// signed-message path yet" — the gate falls back to adler32-only
	// validation in codec.cpp, which is the backward-compatible default.
	//
	// Wire-up of the unmarshal site is deferred until after the
	// in-flight #102 codegen lands — see hmac-message-signing.md slice A
	// for the exact lines that activate this field.
	std::string hmacSessionKey;
private:
	NodeEntityMap entityIds; // Sparse map, only stores assigned node entities
};
