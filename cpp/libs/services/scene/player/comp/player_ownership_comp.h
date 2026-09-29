#pragma once

#include <chrono>
#include <cstdint>
#include <string>

// 玩家归属三件套 —— 跨 zone 场景传送阶段 1 的 C++ 半边。
//
// 设计:docs/design/cross-zone-scene-travel.md §3 CZ-2..CZ-5、§6 不变量;
//       docs/design/scene-owner-reentry-barrier.md §3.3、§6。
//
// 三个组件都是纯运行时标记:不入 PlayerAllData、不序列化、不跨进程。它们描述的
// 是"这份数据归谁、由谁持有、正在交给谁",而这三件事只由 scene_manager 的
// EnterScene 闸口决定,节点只缓存、只校验、不铸造。
// 唯一的非路由写入口(GO-2 §12.8 判定表 B5):源端 ResolveTravelOutcome 在交接被 scene_manager 单调回滚到本节点时,
// 采纳 Redis 里回滚后的 owner_epoch(E+2)。三条前提写死,缺一不可:同一段原子脚本先删掉本次交接这一族标记;
// location.rollback_receipt 逐字节等于本次标记原文;location 指回本节点本 zone(且 epoch 恰好前进两格)。
// 以后任何"读 Redis 采纳 epoch"的新代码都必须同时满足这三条(scene-owner-reentry-barrier.md §3.3)。
//
// ── Redis 键契约(Go 与 C++ 两边必须一字不差,改任何一边都要同步另一边)──
//   player:{player_id}:owner_epoch  纯十进制整数字符串。只由 scene_manager 用 INCR
//                                    推进,单调递增、只进不退 —— 推路由失败后的回滚也是再 INCR 一格(N+1 → N+2),
//                                    任何值都不会被铸两次。节点存盘时用它做 CAS 守卫。
//   player:{player_id}:handoff      值 "{epoch}:{saved_at_ms}",语义 = 这一刻的状态已落盘、写它的节点
//                                    不再持有;EX 300 秒。scene_manager 只比对、从不删除。
//                                    **写入方与删除方不止一处**(完整清单与理由见 exit_release_mark.h 开头):
//                                    写:BeginTravelHandoff(交接,存盘落地回调之后)、DispatchEmergencyRelocate
//                                        (疏散 / 排空改派,owner_epoch 条件写)、A1′(干净退出收敛、实体销毁前,
//                                        owner_epoch 条件写)、A2′ 放弃补写(载入被放弃时把删掉的当前代际写回,
//                                        owner_epoch 条件写)、scene_manager 回滚转写(推路由失败的 bump 回滚,只在
//                                        所凭原标记原样还在时,把它转写成 "{N+2}:{同一 saved_at_ms}",后缀逐字节不变)。
//                                    删:WithdrawHandoffMark(按原文条件删)、ResolveTravelOutcome(原子取证脚本按
//                                        saved_at_ms 只删本次交接这一族:"E:t" 与转写出来的 "E+2:t")、
//                                        A2′(新载入建实体之前,核对 owner_epoch 后删代际 ≤N 的;路由不带
//                                        owner_epoch 时删 ≤ 当前 owner_epoch 的)。
//                                    不要假设"同节点新实体上不可能有有效标记":上一任的 A1′ 就写在同节点上,
//                                    只有 A2′ 能保证新持有期间不留有效标记。
//   player:{player_id}:location     PlayerLocation proto 二进制(Go 写),含 owner_epoch;路由失败回滚恢复出的那一条
//                                    还带 rollback_receipt(被回滚那次铸造所凭的标记原文,与 INCR 同一段 Lua 原子写入,
//                                    任何新落点整条重写 location 时随之消失)。C++ 只有 ResolveTravelOutcome 读它。
//
// ── epoch 的 0 语义 ──
//   0 = 未知 / 旧版 Go 未铸造。这是滚动升级的兼容窗口:校验一律跳过并计数
//   (owner_epoch_stats::OwnerEpochUnknown),不许静默。非 0 时"节点缓存值 ==
//   Redis 当前值"才允许写,否则本节点已被废黜。
namespace player_ownership
{
	inline std::string OwnerEpochRedisKey(uint64_t playerId)
	{
		return "player:" + std::to_string(playerId) + ":owner_epoch";
	}

	inline std::string HandoffRedisKey(uint64_t playerId)
	{
		return "player:" + std::to_string(playerId) + ":handoff";
	}

	// 与 go/scene_manager/internal/logic/changesceneutil.go getPlayerLocationKey 同一口径。
	// 值的解析只走 player_ownership::ParsePlayerLocationElement(player_lifecycle.h),不要各抄一份。
	inline std::string LocationRedisKey(uint64_t playerId)
	{
		return "player:" + std::to_string(playerId) + ":location";
	}

	// 交接标记的值格式 "{epoch}:{saved_at_ms}"。scene_manager 只解析冒号前的 epoch 与当前
	// owner_epoch 比对;saved_at_ms 仅供排障(看"落盘到放行"隔了多久)。
	inline std::string HandoffRedisValue(uint64_t epoch, uint64_t savedAtMs)
	{
		return std::to_string(epoch) + ":" + std::to_string(savedAtMs);
	}

	// 交接标记 TTL。放行后标记已无意义,300s 足够覆盖"源存盘落地 → 客户端重定向 →
	// 目标 zone 落点"的整条链(票据本身只有 5 分钟),过期即自然回收。
	inline constexpr int kHandoffMarkTtlSec = 300;
} // namespace player_ownership

// 玩家归属 zone:数据落库的目的地(DBTask 按它选 topic),与进程所在 zone 无关。
// 值来自 RoutePlayerEvent.home_zone_id → PlayerEnterGameNodeRequest.home_zone_id,
// 建实体时挂上;节点**不得**自己去 data_service 查(CZ-3:两次改派挨近时会读到后一次的值)。
//
// homeZoneId == 0:未知(旧版 gate / scene_manager 未填)。
// 存盘时 fail-closed 用进程 zone 并计数 owner_epoch_stats::HomeZoneUnknown + LOG_WARN,
// 绝不静默落错库(不变量 §6.2)。
struct PlayerHomeZoneComp
{
	uint32_t homeZoneId{0};
};

// 本节点持有该玩家时的归属 epoch(scene_manager 铸造,随 RoutePlayerEvent 下发)。
// 存盘时作为 Redis Lua CAS 的期望值,并写进 DBTask.owner_epoch 供 db 服务落库前比对。
//
// epoch == 0:旧版 Go 未铸造(兼容窗口),存盘走无守卫的旧 Save,并计数。
// 重连 / 顶号路径若请求带非 0 epoch,取 max 更新:epoch 单调,Redis 当前值 ≥ 见过的最大值,
// 取 max 能挡住两条乱序到达的路由请求把缓存值倒退、进而把自己误判成被废黜。
// 写入口只有两个:路由(EnterScene 的 ctx.ownerEpoch)与 ResolveTravelOutcome 的 B5 采纳(同样取 max,
// 前提见文件头"唯一的非路由写入口")。
struct PlayerOwnerEpochComp
{
	uint64_t epoch{0};
};

// 归属交接在途(CZ-5 源端释放链):冻结输入 → 存盘落地 → 写 handoff 标记 → 请求 EnterScene(目标)
// → 应答放行则销毁本地实体 / 应答错误则核实后解冻回 tip。两种交接共用这一个组件、同一条链:
//   * 跨 zone 传送        targetZoneId != GetZoneId()。入口是客户端 RPC
//                          SceneSceneClientPlayer.TravelToZone(PlayerLifecycleSystem::RequestZoneTravel
//                          校验 CZ-6)。放行的应答带 Redirect。
//   * 同 zone 跨节点换图  targetZoneId == GetZoneId()。入口是普通 EnterScene 被 scene_manager 以
//                          ErrHandoffPending(18)暂拒:scene 节点事先不知道目标场景在不在本节点,
//                          被拒才知道要"先存盘、出示标记"再请求一次。放行的应答**没有 Redirect**
//                          (error_code=0),只能靠 owner_epoch 变没变区分"已交给别的节点"与
//                          "重发后又落回本节点"(见 ResolveTravelOutcome)。
//
// 只由 PlayerLifecycleSystem::StartTravelHandoff 挂(唯一入口),并同时挂 PlayerFrozenComp 冻结输入
// (业务系统已按 PlayerFrozenComp 拦写,交接复用同一道闸)。两者成对挂、成对摘。
//
// 成员逐个赋值,不要用聚合初始化:字段还会加,位置参数一错位就是静默的错目标。
//
// targetZoneId  目标 zone;0 非法。等于本 zone = 同 zone 跨节点换图。
// sceneId       目标场景实例 id,填进 EnterSceneRequest.scene_id;0 = 不指定实例。跨 zone 传送恒为 0
//               (目标 zone 的实例由它自己的 scene_manager 挑);同 zone 换图照抄被 18 拒掉的那次
//               请求(加入已有镜像 / 副本时非 0)。
// sceneConfigId 目标地图配置 id,填进 EnterSceneRequest.scene_conf_id;0 = 让 scene_manager
//               按世界频道表挑大世界(与紧急疏散同款)。
// requestedAtMs 0 = 交接尚未发起(等本次存盘落地);非 0 = handoff 标记的 SET 已发出(已进缓冲 / 已执行 /
//               结果未知);EnterScene 发没发看 enterSceneCorrelationId。它在 SET 的 command() 之前就写了,命令发不
//               出去(同一次调用里)或 SET 收到 ERROR 应答(同一个回调里)时都会当场 AbortTravelHandoff 摘掉组件,
//               所以只要观察到非 0,SET 就一定已经发出。冻结上限与晚发闸按它切"标记已发出"
//               (travel_freeze_cap::IsHandoffMarkSent):已发出一侧它们只销毁、不解冻。
//               发起之后本节点**不得再写**该玩家(SavePlayerToRedis 直接跳过):标记一旦落地,
//               scene_manager 随时可能放行并 INCR epoch,再写只会被 CAS 拒、徒增
//               stale_owner_write_rejected 噪声。也是 EnterScene 应答超时看门狗的代际:
//               看门狗到期时 requestedAtMs 若已变化,说明是另一次交接,不动。
// markEpoch     BeginTravelHandoff 写 handoff 标记时用的 owner_epoch;0 = 写标记的 SET 没发出去过,或已确定盘上没有
//               这份标记(SET 收到 ERROR 应答;ResolveTravelOutcome 的取证脚本已原子删掉本族标记,判定表 B4 / B5)。
//               与 requestedAtMs 一起还原标记原文 "{markEpoch}:{requestedAtMs}",交接作废时按原文
//               条件撤回(PlayerLifecycleSystem::WithdrawHandoffMark)。不能到撤回时再去读
//               PlayerOwnerEpochComp:重连 / 顶号路径会把它取 max 更新,读到的未必是写标记那一刻的值。
// hasRecordedEvidence / recordedEvidence
//               本次交接已拿到的、比"看门狗到期"更具体的裁决证据(travel_outcome::Evidence 的底层值,
//               见 player_lifecycle.h;这里存底层值是为了不让组件头依赖系统头)。由
//               PlayerLifecycleSystem::ResolveTravelOutcome 在入口记下:应答 / 路由落点带来的证据若因 Redis
//               不可用没能当场裁决,首次挂的 kNoReply 看门狗(不取消)会先到期,裁决时必须用这里的证据,
//               否则同 zone 成功会补假失败 tip、跨 zone 协议异常会被当成"没收到应答"踢线。纯内存,随组件销毁。
// enterSceneCorrelationId
//               本代交接那条 EnterScene 的关联号(EnterSceneRequest.correlation_id),由 RequestTravelEnterScene
//               在发送时取号写入;0 = 交接的 EnterScene 还没发(SET 在途或还没开始)。只有回显号与它相等的
//               应答才算本代交接的应答(PlayerLifecycleSystem::DispatchEnterSceneReply)——requestedAtMs 在
//               SET 发出前就已置位,拿它当"已发"判据会把 SET 在途期间到达的外来应答错吃成交接证据。
//               每代只写一次;若被重写,只认最新一次。
//               冻结上限的收口日志也用它判"交接的 EnterScene 发没发出去"(enter_scene_sent=),不另设字段。
// frozenAtSteady
//               单调时钟(steady_clock)上的冻结起点,只由 StartTravelHandoff 与 PlayerFrozenComp 同时写,之后不改。
//               冻结硬上限与晚发闸(travel_freeze_cap.h)都以它为准。缺省值(时钟纪元)= "未打点":上限扫描发现时
//               就地补记为当前时刻并计 freeze_unstamped,不当作到期(漏打点只会晚处置,不会下一拍就被销毁)。
//               不复用 PlayerFrozenComp.frozenAtMs:那是墙钟,而且它是存盘阶段看门狗用来认代际的,不能动。
//               类型必须与 travel_freeze_cap::Clock::time_point 一致(player_lifecycle.cpp 有 static_assert);这里
//               直接写 std::chrono::steady_clock 而不包含 travel_freeze_cap.h,是为了不让这个被二十来个业务系统
//               间接包含的组件头依赖系统头。
// destroyDeferralReported
//               "handoff 标记已发出、该销毁了,但还有未落地的存盘,推迟销毁"(ConcludeHandoffAfterMarkSent)的
//               ERROR 与 destroy_deferred_unsettled_save 计数每次交接只打 / 只计一次;随组件销毁。
struct PlayerTravelHandoffComp
{
	uint32_t targetZoneId{0};
	uint64_t sceneId{0};
	uint32_t sceneConfigId{0};
	uint64_t requestedAtMs{0};
	uint64_t markEpoch{0};
	bool hasRecordedEvidence{false};
	uint8_t recordedEvidence{0};
	uint64_t enterSceneCorrelationId{0};
	std::chrono::steady_clock::time_point frozenAtSteady{};
	bool destroyDeferralReported{false};
};

// 本节点替在线玩家发出、应答还没回来的**普通** EnterScene(客户端换图 / 镜像创建后的自动进场 /
// 队伍跟随)。
//
// 为什么需要:EnterSceneResponse 不带请求内容(只回显 player_id 与 correlation_id)。scene_manager
// 以 18 暂拒时,只有靠它才知道"刚才要去哪"、才能用同一个目标起交接(StartTravelHandoff)。
// 应答按 correlation_id 对号(PlayerLifecycleSystem::DispatchEnterSceneReply),只有号相等的应答才会
// 摘它、起交接或回 tip,不再串号:过了 TTL 才到的迟到应答、上一次请求的应答对不上号,直接丢弃。
//
// 发送侧互斥(IsSceneChangeBusy)仍然必要,理由是**组件只有一个槽**:第二次登记会覆盖第一次的号,
// 第一次随后到达的 18 就对不上号而被丢弃,玩家既没换成图也收不到提示。旧版 scene_manager 不回显
// correlation_id(应答号为 0)时退回按 player_id 匹配,两条请求分不开,同样只能靠这道闸。
// 队伍跟随也必须挂:不挂的话发送侧闸看不见它,客户端换图会与它同时在途;旧版 scene_manager 下
// 跟随的应答仍会按 player_id 摘掉客户端那条换图的记录,那条随后到达的 18 找不到目标、同 zone
// 交接不发起。挂上之后同一玩家的普通 EnterScene 始终串行。
// 疏散 / 排空发的 EnterScene 不挂它(发完实体就销毁了,没有等待者)。
// 应答或传输失败到达即摘(DispatchEnterSceneReply / DispatchEnterSceneTransportFailure;生成的 gRPC 客户端
// 保证每次调用在 deadline 内以其一收场);完成通知永远不来时靠 sentAtMs 的 TTL(SceneManager deadline + 1s)
// 自然失效,不需要定时器。
//
// correlationId    本次 EnterScene 的关联号(EnterSceneRequest.correlation_id),由
//                  PlayerLifecycleSystem::RequestSceneChange 写入,经它登记的恒非 0;默认值 0 = 未登记。
//
// playerRequested  这次换图是不是玩家自己要的。true(默认)= 客户端在等结果:18 要起交接,其它失败
//                  要回 tip。false = 服务器替他发的队伍跟随:玩家没在等,被拒只记日志、队员留在原
//                  场景(跟随不跨节点拉人,team-system.md DV-6),不起交接、不回 tip ——
//                  否则玩家会凭空收到一条"进入场景失败"。
struct PlayerSceneChangeInFlightComp
{
	uint64_t sceneId{0};
	uint32_t sceneConfigId{0};
	uint64_t sentAtMs{0};
	bool playerRequested{true};
	uint64_t correlationId{0};
};
