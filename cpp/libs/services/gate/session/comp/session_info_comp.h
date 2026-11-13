#pragma once

#include <chrono>
#include <memory>
#include <optional>
#include <unordered_map>

#include "muduo/net/TcpConnection.h"
#include "engine/core/type_define/type_define.h"
#include "message_limiter/message_limiter.h"
#include "network/client_liveness.h"
#include "pending_scene_entry_comp.h"
#include "pre_auth_slot.h"
#include "router_lane_item.h"
#include "scene_lane_item.h"

// 登录态续租在一条会话上的状态(docs/design/gate-online-identity.md §10.4)。
// 只由 nodes/gate/handler/rpc/login_session_renewal.cpp 读写。
enum class LoginRenewalState : uint8_t
{
	kIdle,             // 没有续租在排:还没登录成功,或 login 已回「没有可续的记录」
	kQueued,           // 排着,等到期
	kInFlight,         // 请求已发出,等应答
	kInFlightRelogged, // 在途期间这条连接又登录成功过:应答若是「没有可续的记录」,它可能早于新的登录,要再问一次
};

struct SessionInfo
{
	// 句柄属于 tlsNodeContextManager.GetRegistry(nodeType)，只在本进程有效。
	using NodeEntityMap = std::unordered_map<uint32_t, entt::entity>;

	SessionInfo() = default;

	void SetNodeEntity(uint32_t nodeType, entt::entity node)
	{
		nodeEntities[nodeType] = node;
	}

	entt::entity GetNodeEntity(uint32_t nodeType) const
	{
		auto it = nodeEntities.find(nodeType);
		if (it != nodeEntities.end())
		{
			return it->second;
		}
		return entt::null;
	}

	bool HasNodeEntity(uint32_t nodeType) const
	{
		auto it = nodeEntities.find(nodeType);
		if (it == nodeEntities.end())
		{
			return false;
		}
		return it->second != entt::null;
	}

	Guid playerId{kInvalidGuid};
	// weak_ptr(AGENTS.md §11.7:应用层不拥有连接):客户端连接由 gate 的 TcpServer 拥有,会话表只是索引。
	// 会话在 DOWN 回调里同步 erase、TcpServer 在那之后才放手,所以稳态下记录存在时 lock() 必成功;
	// 改 weak 是为了任何路径(含关机)都不可能把死连接连 fd 钉在 thread_local 的会话表里。
	std::weak_ptr<muduo::net::TcpConnection> conn;
	MessageLimiter messageLimiter;
	uint32_t sessionVersion{UINT32_MAX};
	uint32_t pendingEnterGsType{0}; // Pending login type to forward to Scene once scene node is assigned;转发失败后由 pendingSceneEntry 驱动补发
	uint64_t sceneId{0};            // Scene instance GUID from SceneManager (RoutePlayerEvent)

	// 跨 zone 归属与所有权 epoch(cross-zone-scene-travel.md CZ-3 / CZ-4,scene-owner-reentry-barrier.md §3.3)。
	// 两项都来自 RoutePlayerEvent,由 scene_manager 在每次路由决策时下发;gate 不生产、不校验,只透传进
	// PlayerEnterGameNodeRequest。之所以要存进会话而不是在 RoutePlayer 里用完即弃:向 scene 的转发有两个入口
	// (RoutePlayerEventHandler / BindSessionEventHandler),BindSession 晚到时的补发读不到路由事件,只能从会话取。
	// 同一会话再次收到 RoutePlayerEvent 时,每次路由决策都整体覆盖(同落点重连不铸造新 epoch、沿用观察值,
	// 改派才铸新值),旧值留着只会让补发带上已被废黜的 epoch。
	// 会话销毁即随之消失,不需要额外清理路径。
	uint32_t homeZoneId{0}; // 玩家归属 zone;0 = 未知(旧版 scene_manager 未填),scene 侧 fail-closed 落进程 zone 并计数
	uint64_t ownerEpoch{0}; // 本次路由的归属 epoch;0 = 旧版未铸造(兼容窗口),scene 侧存盘跳过 CAS 并计数
	// 账号目录修订与本次路由绑定，不能用登录代次的max合并规则补成新所有者。
	uint64_t ownerRevision{0};
	// login 绑定的原账号与修订只由 BindSession 写，不能被场景路由提升权限。
	std::string authenticatedAccount;
	uint64_t authenticatedOwnerRevision{0};
	uint64_t forwardedOwnerRevision{0};
	// 上一次**实际转发给 scene** 的 PlayerEnterGameNodeRequest.owner_epoch(ownerEpoch 是"最近一次路由决策",转发前就被覆盖)。
	// scene_route_helper.h ApplyRoute 第 4 条据此判断"同场景同节点、但归属换了代"的路由要不要再转发一次
	// (docs/design/location-release-binding.md §3.5,审计 load-handover#0)。只由 ApplyRoute 在转发成功后写,ResetSceneRoute 清零。
	uint64_t forwardedOwnerEpoch{0};
	// 客户端 LeaveGame 之后、下一次登录真正把进场交给 scene 之前为 true(location-release-binding.md §3.5 R5)。
	// 期间到达的、属于 LeaveGame 之前那条登录链的 Bind / 路由一律丢弃,不进会话:不丢的话它会重新填上 lastSceneRoute,
	// 下一次 BindSession 就按这条旧路由、带着旧 epoch 补发。判据见 scene_route_helper.h IsRouteFromBeforeLeaveGame /
	// IsBindFromBeforeLeaveGame:
	//   - enterGameSentAfterLeave 为 false(重置之后客户端还没在这条连接上发过 EnterGame):login 只在处理 EnterGame 时发 Bind,
	//     这段时间里到达的 Bind / 路由只可能来自旧链,不看代次一律丢(第二轮审计 r2-gate#1:旧链的 Bind 晚于 LeaveGame 到达时,
	//     它的代次大于重置时记下的值,单靠代次拦不住);
	//   - 之后按代次认:属于 playerIdAtReset、代次不大于 loginGenerationAtReset,或比会话已见过的本角色更新登录旧的,是旧链。
	// playerIdAtReset / loginGenerationAtReset 是"被离开的那条登录链"的角色与最高代次:重置时取会话当时的值;会话那时还没绑角色
	// (Bind 还在路上就 LeaveGame)时取第一条被丢弃的旧链 Bind / 路由带来的;被丢弃的旧链 Bind / 路由只把代次往上抬
	// (NoteDroppedFromBeforeLeaveGame),让同一条旧链在客户端再次 EnterGame 之后才到的兄弟事件仍被认出。
	// 写入只经 scene_route_helper.h:ResetSceneRoute 置位并记下身份;NoteEnterGameRequested 置 enterGameSentAfterLeave;
	// ApplyRoute 转发成功、ApplyLoginBinding 换角色或收到不带代次的旧版 Bind 时清除 sceneRouteResetPending。
	bool sceneRouteResetPending{false};
	bool enterGameSentAfterLeave{false};
	Guid playerIdAtReset{kInvalidGuid};
	uint64_t loginGenerationAtReset{0};

	// 登录代次(docs/design/scene-enter-session-fence.md §4):本会话、本角色见过的最高代次。来源是
	// BindSessionEvent 与 RoutePlayerEvent,同一角色取 max;同一条连接上换了角色时以新角色的值为准(上一个
	// 角色的代次与新角色无关)。用于给 PlayerEnterGameNodeRequest 盖章,新旧由 scene 入场栅栏判;另由踢线身份栅栏
	// (session_identity_fence.h ClassifyKick)只读比较:会话已见过不小于踢线者的代次时不踢
	// (docs/design/scene-enter-chain-residuals.md §2.2 ④)。gate 不据此
	// 拒绝或丢弃任何路由:一次失败的重登(Bind 已到、scene_manager 拒绝)会把会话代次抬高,之后同连接上
	// scene 自发的换图路由带实体代次(≤ 会话值),旧版 scene 为 0(都由本字段盖章),按代次丢会把它们全吞掉。
	// 0 = 上游未带(旧版),原样透传。
	// 写入仍只经 scene_route_helper.h 的 ApplyLoginBinding / ApplyRouteIdentity,且都必须先于转发:
	// 转发从会话取值盖章。会话销毁即随之消失。
	uint64_t loginGeneration{0};

	// 会话代次(docs/design/client-request-ordering.md §4.2):建连时由 gate_lane::NextIncarnation() 分配,
	// 进程内单调、会话存续期间不变,与 gate_instance_id 合起来全局唯一。随 SessionDetails.session_incarnation 发出:
	// gate 认领回包时用它挡住 session_id 被新连接复用后迟到的旧回包,login 会话栅栏用它建键。0 = 未分配。
	uint64_t incarnation{0};
	// 路由 lane:本会话经路由服转发的 gRPC 类请求按收包顺序单飞(client-request-ordering.md §4)。
	// 放在会话里 = 会话 erase 即取消,也不怕 session_id 复用(与 pendingSceneEntry 同一先例);
	// 只由 nodes/gate/handler/rpc/client_request_lane.cpp 读写。
	gate_lane::SessionRouterLane routerLane;
	// 场景 lane:本会话发往 scene 的客户端消息按收包顺序单飞;玩家换场景节点的窗口里,scene 答复「确定未执行、稍后重投」的
	// 请求留在这里按序重投(docs/design/client-scene-request-handoff-hold.md §4)。放在会话里 = 会话 erase 即取消,
	// 换几次节点队列都跟着会话;只由 nodes/gate/handler/rpc/client_scene_lane.cpp 读写。
	gate_scene_lane::SessionSceneLane sceneLane;

	// 最近一次 RoutePlayerEvent 的决策(CPP-2);每次到达无条件覆盖,与是否转发成功无关。
	// 只供 BindSession 晚到时按 node_id 重建欠账(scene_route_helper.h EntryForLateLoginBinding):
	// 不从已提交的节点实体反查 NodeInfo —— 已提交的指向可能已被节点摘除置无效,或因同 uuid 重注册而悬空。
	SceneRouteTarget lastSceneRoute;
	// CPP-2 进场转发欠账:路由到了 gate 却还没交给 scene。放在会话里 = 会话 erase 即取消,
	// 也不怕 session_id 复用;只由 nodes/gate/handler/event/scene_entry_dispatch.cpp 读写(单测直接调纯函数)。
	std::optional<PendingSceneEntry> pendingSceneEntry;

	// 重定向票据的持票者与目标 zone(cross-zone-scene-travel.md CZ-8)。DispatchTokenVerify 验签通过后
	// 写入,会话存续期内不变;gate 只把它们透传进 SessionDetails,主判定在 login EnterGame
	// (那是 player_id 已知、且尚未写任何会话状态的最早时刻;gate 自己要到 BindSession 才知道
	// player_id,那时 login 已经落了 ONLINE 会话,在 gate 拒只会留下残留)。
	// 默认值用 0 而不是 kInvalidGuid:与 proto 的"0 = 未填 / 普通票据"同形,login 对 0 一律放行。
	Guid ticketPlayerId{0};
	uint32_t ticketTargetZoneId{0};
	bool verified{false};			// True after client passes Gate connection token verification
	// 未验签占位(pre_auth_slot.h):建连时发给还没验签的会话,验签成功时归还;会话销毁时随之归还。
	// 只由 nodes/gate/handler/rpc/connection_admission.cpp 读写。空 = 已验签,或建连即免验签(dev)。
	std::shared_ptr<PreAuthSlot> preAuthSlot;
	// True once Gate actually dispatches ClientPlayerLogin.Login to Login.
	// Disconnect uses this to clean Login's session-id state without turning
	// every verified-but-idle TCP close into a cross-service RPC.
	bool loginStarted{false};
	// 本连接至少收到过一次成功的 Login 回包(handler/rpc/login_admission.h)。CreatePlayer / EnterGame / LeaveGame
	// 发出前要求它为真:login 的登录态已按 (gate_instance_id, 会话代次) 建键,这里是纵深防御,并在新旧 login
	// 并存的滚动窗口里挡住「跳过 Login、撞上旧 gate 进程同号 session_id」的请求
	// (docs/design/data-overwrite-rollback-audit-20261018.md §2.3)。只置真不复位:同一连接的后续登录失败
	// 不撤销已成功的那次,本连接登录态是否仍有效由 login 判定。
	bool loginSucceeded{false};

	// Illegal-packet counter (todo.md #236). Increment whenever a packet is
	// rejected by the gate's incoming validation chain (rate-limit exceeded,
	// unknown messageId, malformed body, unauthenticated send, HMAC mismatch
	// once #76 lands). Hits the threshold → forceClose. Resets to 0 on a
	// fresh ClientTokenVerify so a transient burst doesn't poison the next
	// reconnect; otherwise monotonically increasing for the connection's
	// lifetime. Thresholds tuned per deployment via env / config (see
	// IllegalPacketCounter::ThresholdFromEnv).
	uint32_t illegalPacketCount{0};

	// 应用层心跳的存活状态(docs/design/gate-client-heartbeat.md,纯逻辑在 core 的 network/client_liveness.h)。发过心跳的连接
	// 连续 30s 收不到任何一帧,gate 就 forceClose,之后照常走断开回调。只由 nodes/gate/handler/rpc/client_heartbeat.{h,cpp}
	// 读写;会话 erase 即随之消失,判死检查点按会话代次核对,不需要另外清理。
	client_liveness::ClientLiveness liveness;

	// 关连接之前置位:断开回调代发 ExitGame 时填 EXIT_GAME_REASON_LEASE_EXPIRED,scene 立即退出,不按断线保留角色
	// (docs/design/player-connection-lost-hold.md §3)。目前只有断线租约到期、gate 关残留假死连接这一处置位:
	// 后端已不认这个会话,再保留一个重连窗口没有意义。不置位的关闭(断网、gate 停机、顶号踢旧连接、高水位)按断线处理。
	bool exitSceneImmediatelyOnClose{false};

	// Per-session HMAC key for message signing (todo.md #76 slice A).
	//
	// Populated by client_session_handler.cpp::HandleClientTokenVerify
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

	// gate 排空迁移与重定向(docs/design/gate-drain-session-migration.md §4.3)。只由 nodes/gate 的
	// handler/rpc/drain_migration.cpp 与 handler/gate_redirect_delivery.cpp 读写;放在会话里 = 会话 erase 即随之消失,
	// 也不怕 session_id 复用。
	//   - drainMigrationInFlight:本会话的 RequestGateMigration 还没回,不重复请求;
	//   - drainMigrationRetryAt:上次没要到票(无目标、调用失败、没发出)之后的退避截止,默认值 = 不退避;
	//   - lastEnterGameAt:最近一次转发客户端 EnterGame 的时刻。停在选角界面的连接在它之后一段时间内不迁,
	//     避开「EnterGame 已发出、绑定还没回来」的窗口;
	//   - redirectedAt:推 msg 124 的时刻,没推过为空。滞留关闭按它算。
	bool drainMigrationInFlight{false};
	std::chrono::steady_clock::time_point drainMigrationRetryAt{};
	std::optional<std::chrono::steady_clock::time_point> lastEnterGameAt;
	std::optional<std::chrono::steady_clock::time_point> redirectedAt;

	// 登录态续租(docs/design/gate-online-identity.md §10.4):连接活着期间,gate 定期替它向 login 续租登录态、设备集成员与
	// 绑定栅栏。放在会话里 = 会话 erase 即随之消失,到期登记按会话代次核对,不怕 session_id 复用。
	LoginRenewalState loginRenewal{LoginRenewalState::kIdle};
private:
	NodeEntityMap nodeEntities; // Sparse map, only stores assigned node entities
};
