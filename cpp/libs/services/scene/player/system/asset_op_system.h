#pragma once

// 通用资产通道的 scene 侧入口(docs/design/guild-phase2/04-asset-channel.md §S4)。
//
// 一句话:Go 服务(guild / trade)先在自己库里写一行"待办资产指令",拿到该 (玩家, 流) 的
// 递增 seq 与流纪元,再按 player:{id}:location 找到玩家所在 scene,调
// SceneNodeGrpc.AssetDebit / AssetAbortDebit / AssetCredit。本类在 **loop 线程**的同一次
// 处理里做完"验签 → 找人 → 查账本 → 闸门 → 改资产 → 记账本 → 触发存盘",账本
// (PlayerAssetOpLedgerComp)与资产(currency / bag_component)同在 player_database 一条记录里,
// 一起落盘、一起丢(不变量 I3)。
//
// 契约(调用方必须知道的全部):
//   * **线程**:三个入口只能在 scene 的 muduo loop 线程上调用(gRPC 包装已 runInLoop 投递)。
//     内部读写 ECS、读账本、触发存盘,没有任何自己的锁。
//   * **gRPC status 恒为 OK**,结局写在 response.outcome;reason 是 asset_error 段的 tip 码
//     (无原因时 id = 0)。四种结局的含义见 §4.10 与下面的 Kind 注释。
//   * **幂等**:同一 (player_id, stream, stream_epoch, seq) 重复调用只读答复,绝不重办
//     (不变量 I2 结局固定)。未记账的结局(RETRY / NOT_HERE / UNKNOWN)可以安全重投。
//   * **不在本调用里等落盘**:记账后立刻触发一次 SavePlayerToRedis 并如实回报 durable;
//     Go 用同一 seq 重查直到 durable=true 才终结(不变量 I4)。生成器模板在 future.get()
//     之后没有守护段(grpc_handler_gen.go:130-141),硬塞等待会被重生成吞掉。
//   * **性能**:只有"要记账"和"已见但尚未 durable 且距上次请求 ≥500ms"两种情况会触发
//     marshal + proto 比较;其余路径是几次组件查找。
//
// 闸门只在本文件(资产 RPC 入口层)判,**绝不下沉** BagService / CurrencySystem:
// 战斗结算、任务发奖等内部路径共用那两个模块,下沉冻结/战斗闸会让结算永久卡死(D48 红线)。

#include "entt/src/entt/entity/entity.hpp"

#include "modules/currency/constants/currency.h"
#include "player/system/asset_op_auth.h"
#include "proto/common/asset/asset_op.pb.h"
#include "proto/common/rollback/transaction_log.pb.h"

// ── 运行时组件(纯内存,不入库,不序列化)──────────────────────────────────────

// 上一次为"确认 durable"而请求存盘的墙钟毫秒。用于给重查限频:已见但尚未 durable 的
// 重查每 500ms 最多再触发一次存盘,免得 Go 的 100/200/400ms 重查把 marshal + proto
// 比较打成热路径。0 = 本实体还没请求过。
struct PlayerAssetOpPersistRequestComp
{
	int64_t lastRequestMs{0};
};

// 加载时 ValidateAssetOpLedger 判定账本损坏的标签(空结构,不入库)。挂上之后该玩家的
// 全部资产 RPC 一律回 RETRY + kAssetBlocked,**不改写**原数据,等人工排查(§4.4 fail-closed)。
struct PlayerAssetOpLedgerInvalidComp
{
};

// 已见但未 durable 的重查,两次触发存盘之间的最小间隔。
constexpr int64_t kAssetOpResaveMinIntervalMs = 500;

class PlayerAssetOpSystem
{
public:
	// 三个 RPC 的业务入口。response 由本函数完整填写(调用方不必预置任何字段):
	//   APPLIED   已扣/已发,账本已记;partial=true 表示只发放了一部分(§4.33),
	//             Go 不做对侧入账,转人工补偿。
	//   REJECTED  终局拒绝(余额不足 / 包非法 / 被封禁 / 中止占位),账本已记。
	//   RETRY     暂时条件(冻结或归属交接在途 / 战斗中 / 背包满 / 退出中 / 账本损坏),**未记账**,
	//             Go 稍后重投同一 seq。
	//   NOT_HERE  玩家不在本节点,**未记账**,Go 重新定位。
	//   UNKNOWN   信封畸形 / 验签失败 / 纪元过期 / 跳号过远 / seq 已滑出窗口,**未记账**,
	//             Go 告警并转人工,不得终结该行。
	static void Debit(const ::AssetOpRequest& request, ::AssetOpResponse& response);
	static void AbortDebit(const ::AssetOpRequest& request, ::AssetOpResponse& response);
	static void Credit(const ::AssetOpRequest& request, ::AssetOpResponse& response);

	// ── 测试注入点(AGENTS §11.2 显式依赖:存盘、发币、密钥、时间都要能替换)──
	// 全部传 nullptr 恢复默认实现。生产代码**不调用**这些 setter。

	// 默认 &PlayerLifecycleSystem::SavePlayerToRedis。
	// 返回 true = 确实压了一次写、稍后会有落盘回调;false = 与上次落盘快照逐字段相等、本次不写
	// (此时结局必然已在盘上,调用方立刻重算 durable)。
	using PersistFn = bool (*)(entt::entity);
	static void SetPersistFnForTest(PersistFn fn);

	// 默认 &CurrencySystem::AddCurrency。只有 Credit 用它;Debit 固定走 DeductCurrency
	// (部分发放只可能发生在 Credit 上,§4.33)。
	using AddCurrencyFn = uint32_t (*)(entt::entity, CurrencyType, int64_t, TransactionType, uint64_t);
	static void SetAddCurrencyFnForTest(AddCurrencyFn fn);

	// 默认 nullptr,含义是"由 VerifyAssetOpAuth 走默认实现:读环境变量
	// MMORPG_ASSET_OP_SECRET_<CALLER> 并缓存"(§4.32)。密钥值不进日志、不进仓库。
	static void SetSecretLookupForTest(AssetOpSecretLookup fn);

	// 默认 &TimeSystem::NowMillisecondsUTC。签名时钟偏差校验与重查限频都用它;
	// 单测冻结时间后,限频与 300s 偏差窗才是确定性的(AGENTS §11.4:不依赖真实墙钟)。
	using NowMsFn = int64_t (*)();
	static void SetNowFnForTest(NowMsFn fn);
};
