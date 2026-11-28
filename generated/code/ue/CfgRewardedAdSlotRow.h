// ---------------------------------------------------------------------------
//  由 Data Table Exporter 从 RewardedAdSlot.xlsx 生成。**不要手改**。
//  重新生成:tools/data_table_exporter/run.py
// ---------------------------------------------------------------------------
//
//  模块依赖:这个头只要 Core + Engine(Engine/DataTable.h)。
//  整套产物要加的模块见 ConfigSubsystem.h 顶部的「Build.cs」一节 —— 那里有可以
//  直接抄进 Build.cs 的一行。
//
//  形态说明(为什么不是 UDataTable):
//    1. UDataTable 的填表 API 是 WITH_EDITOR-only,shipping 包里没有「再 Load 一次」
//       这个动作,而本导表器五门语言的产物全部建立在「Load() 整批建新快照再原子换掉
//       旧的」之上 —— 用 UDataTable 等于让 UE 这一门独家丢掉热更。
//    2. UDataTable 行名唯一,与本导表器的「主键可重复」(table.multi_primary_key)
//       结构性冲突。
//  继承 FTableRowBase 是零成本的:把「将来想丢进 UDataTable 看一眼」的门留着,
//  但运行时一行代码都不依赖它。
//
//  字段名口径(**这是整套 USTRUCT 的命门**):
//    generated/tables/rewardedadslot.json 不是 protojson,是导表器自己 json.dumps 出来的,
//    键就是 proto 字段名本身 —— **snake_case**。
//    FJsonObjectConverter 用 `JsonAttributes.Find(Property->GetName())` 找值,
//    TMap<FString,...> 的比较/哈希大小写不敏感,所以匹配是「忽略大小写的逐字符相等」,
//    **它不做 snake_case <-> camelCase 转换**。因此下面每个 UPROPERTY 的名字都
//    刻意写成与 JSON 键逐字符相同的 snake_case。改成 UE 惯例的 PascalCase 的那一刻,
//    FJsonObjectConverter 会**静默跳过**这一列(找不到的字段不报错),整张表变成默认值。
//
//  UPROPERTY 说明符口径:
//    - Blueprint 能安全承接的列写 `UPROPERTY(BlueprintReadOnly, Category = ...)`;
//    - 其余列写**光板 `UPROPERTY()`**,不写 Category。光有 Category 而没有任何
//      Edit*/Visible*/BlueprintRead* 说明符,UHT 会对每个这种字段报一条警告
//      ("Property has a Category but is not editable or blueprint visible"),
//      21 张表能刷出一屏。光板 UPROPERTY() 一样有反射,
//      FJsonObjectConverter 照样认得 —— 装载不受影响。
// ---------------------------------------------------------------------------
#pragma once

#include "CoreMinimal.h"
#include "Engine/DataTable.h"

#include "CfgRewardedAdSlotRow.generated.h"

/// RewardedAdSlot 的一行。字段名与 rewardedadslot.json 的键逐字符一致(snake_case),
/// 见文件头「字段名口径」。
///
/// 数值类型取舍:proto 的 uint32/uint64 在这里是 int32/int64 —— UPROPERTY 的
/// 无符号整数**不能带 Blueprint 说明符**(UHT 报 "Type 'uint32' is not supported by
/// blueprint."),而配置行不能被 Blueprint 读就没什么用。代价是 >= 2^31 的 uint32
/// 会读成负数;本工程的表 id / 计数都远在这条线以下。真要放超过 2^31 的值,
/// 改成 int64 而不是改回 uint32。
/// 这个代价不再是静默的:RewardedAdSlotTable.cpp 在装载路径上对**由无符号收窄而来**的列
/// 做值域检查,越界会打 Error 并点名表/行/列。
USTRUCT(BlueprintType)
struct FCfgRewardedAdSlotRow : public FTableRowBase
{
	GENERATED_BODY()

	/** 广告位业务编号(首批固定 1 = 观影绑定银两小礼) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|RewardedAdSlot")
	int32 id = 0;

	/** 总开关:0 = 关闭(首审默认态),1 = 开启 */
	UPROPERTY(BlueprintReadOnly, Category = "Config|RewardedAdSlot")
	int32 enabled = 0;

	/** 解锁最低角色等级(默认 10) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|RewardedAdSlot")
	int32 min_level = 0;

	/** 每周最大领取次数(UTC+8 周一 05:00 切周,默认 14;字段名保留 daily_limit 兼容既有协议) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|RewardedAdSlot")
	int32 daily_limit = 0;

	/** 两次成功领取最短冷却秒数(默认 60) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|RewardedAdSlot")
	int32 cooldown_sec = 0;

	/** 签发会话到领奖的最短观看秒数(默认 15) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|RewardedAdSlot")
	int32 min_watch_sec = 0;

	/** 会话有效 TTL 秒数(默认 300,容忍看广告 30 秒断线重连补领) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|RewardedAdSlot")
	int32 session_ttl_sec = 0;

	/** 发放币种(CurrencyType:固定 3 = CURRENCY_TYPE_BIND_GOLD 绑定银两) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|RewardedAdSlot")
	int32 currency_type = 0;

	/** 单次发放绑定银两数量(默认 300) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|RewardedAdSlot")
	int64 reward_amount = 0;

	/** 是否强制校验平台 SSV 回调:0 = V1 轻量会话校验,1 = V2 强制 SSV */
	UPROPERTY(BlueprintReadOnly, Category = "Config|RewardedAdSlot")
	int32 require_ssv = 0;

	/** 允许开启宿主位掩码:1=微信小游戏 2=抖音小游戏 4=iOS App 8=Android App(默认 3 仅小游戏) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|RewardedAdSlot")
	int32 platform_mask = 0;

	/** 激励奖励类型:1=绑定银两小礼(300绑银),2=五行幻境V2奇策重随(单局限1次),3=衣柜续时1小时试穿(不授永久权),4=神秘商人个人换货(每轮限1次) */
	UPROPERTY(BlueprintReadOnly, Category = "Config|RewardedAdSlot")
	int32 reward_kind = 0;
};
