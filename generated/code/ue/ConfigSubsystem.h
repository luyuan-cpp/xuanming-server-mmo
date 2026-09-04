// ---------------------------------------------------------------------------
//  由 Data Table Exporter 生成。**不要手改**。
//  重新生成:tools/data_table_exporter/run.py
// ---------------------------------------------------------------------------
//
//  ---- Build.cs(第一次接这套产物必看)----
//
//  这些产物用到 Json / JsonUtilities / Engine 三个模块的头(FJsonObjectConverter、
//  FJsonObject、UGameInstanceSubsystem、FTableRowBase),不加依赖第一次编译就是
//  一串 "Cannot open include file: 'JsonObjectConverter.h'"。
//  把这一行抄进承载这些文件的模块的 Build.cs 构造函数里:
//
//      PublicDependencyModuleNames.AddRange(new string[] { "Core", "CoreUObject", "Engine", "Json", "JsonUtilities" });
//
//  谁需要谁:
//    Core         CoreMinimal.h、TArray/TMap/FString
//    CoreUObject  UObject/Object.h、USTRUCT/UCLASS 反射
//    Engine       Engine/DataTable.h(FTableRowBase)、Subsystems/GameInstanceSubsystem.h
//    Json         Dom/JsonObject.h、Serialization/JsonSerializer.h
//    JsonUtilities JsonObjectConverter.h(FJsonObjectConverter::JsonObjectToUStruct)
//
//  另外:UCLASS 前的导出宏由 exporter_config.yaml 的 languages.ue.package 决定
//  (当前 = MMORPGCONFIG_API)。填了就必须在模块里 #define 它 —— 标准做法是让模块名
//  的大写形式 + _API 由 UBT 自动生成(模块名 XxxConfig -> XXXCONFIG_API),
//  也就是说这些文件要放在那个模块里编译。
// ---------------------------------------------------------------------------
#pragma once

#include "CoreMinimal.h"
#include "Subsystems/GameInstanceSubsystem.h"

#include "ActorActionCombatStateTable.h"
#include "ActorActionStateTable.h"
#include "BaseSceneTable.h"
#include "BuffTable.h"
#include "ClassTable.h"
#include "ConditionTable.h"
#include "CooldownTable.h"
#include "DungeonTable.h"
#include "EquipSlotTable.h"
#include "GlobalVariableTable.h"
#include "ItemTable.h"
#include "MessageLimiterTable.h"
#include "MirrorTable.h"
#include "MissionTable.h"
#include "MonsterTable.h"
#include "RewardTable.h"
#include "SkillTable.h"
#include "SkillPermissionTable.h"
#include "TestTable.h"
#include "TestMultiKeyTable.h"
#include "WorldTable.h"

#include "ConfigSubsystem.generated.h"

/// 全部配置表产物共用的日志分类。声明在这里、定义在 ConfigSubsystem.cpp,
/// 每张表的 .cpp 都 include 本头文件来用它 —— 21 份产物各声明一次会重定义。
DECLARE_LOG_CATEGORY_EXTERN(LogConfigTable, Log, All);

/**
 * 全部配置表的聚合装载器。
 *
 * 为什么是 GameInstanceSubsystem 而不是进程级单例:配置随 GameInstance 生死,
 * PIE 反复起停不会把上一局的表带进下一局;而且 UObject 生命周期由引擎管,
 * 不必自己写 shutdown 顺序。
 *
 * ---- 热更契约 ----
 *
 * 每张表的 Load 都是「整批建新快照再原子换掉旧的」。LoadAll() 走完之后全部表都换到
 * 了新一代,但它**不是**一个跨表的原子点:中途读表会看到部分新、部分旧;某张表装载
 * 失败时那张表**留在旧一代**,其余的已经换了代。要跨表一致就等 LoadAll() 返回 true
 * 再开始读,或者看 IsCoherent()。
 *
 * 返回出去的 `const FCfg*Row*` 属于当时那一代快照,GC **保不住**它(USTRUCT 不是
 * UObject,它躺在快照的 TArray 里)。调用方只存 id;真要跨热更持有,用对应表的
 * PinSnapshot()。
 *
 * ---- 线程契约 ----
 *
 * LoadAll / ReloadAll 与全部查询都在游戏线程。这里刻意**没有**多线程版:
 * 异步的正确形状是「后台线程取字节 -> 游戏线程调 LoadAllWithProvider」,
 * 见 LoadAllWithProvider。
 */
UCLASS(BlueprintType)
class MMORPGCONFIG_API UConfigSubsystem : public UGameInstanceSubsystem
{
	GENERATED_BODY()

public:
	/// 本产物覆盖的表数量。
	static constexpr int32 TableCount = 21;

	UFUNCTION(BlueprintPure, Category = "Config", meta = (WorldContext = "WorldContextObject"))
	static UConfigSubsystem* Get(const UObject* WorldContextObject);

	virtual void Initialize(FSubsystemCollectionBase& Collection) override;
	virtual void Deinitialize() override;

	// ---- 装载 ----

	/// 从目录装载全部表(每张表读 <ConfigDir>/<表名小写>.json)。
	/// 任一张失败就返回 false,失败的那张留在旧一代;失败原因逐条打进 LogConfigTable。
	UFUNCTION(BlueprintCallable, Category = "Config")
	bool LoadAll(const FString& ConfigDir);

	/// 用调用方提供的取文本回调装载。打包后配置常在 pak / Addressable / 网络里,
	/// FFileHelper 读不到,这时用这个:回调按文件名(见 TableFileNames)返回内容。
	/// 回调在**当前线程**同步调用 —— 要异步就先在后台把文本取全,再回游戏线程调它。
	bool LoadAllWithProvider(TFunctionRef<bool(const TCHAR* /*FileName*/, FString& /*OutJsonText*/)> TextProvider);

	/// 用上一次 LoadAll 的目录重装。没装载过返回 false。
	UFUNCTION(BlueprintCallable, Category = "Config")
	bool ReloadAll();

	/// 全部表的数据文件名,与 LoadAll 的装载顺序一致。
	static TArray<FString> TableFileNames();

	/// **失效通知**计数器:只要有**任何一张表**换过代就 +1。
	///
	/// 语义刻意选的是「有东西变了」而不是「全部表都成功了」:部分失败(第 N 张挂了、
	/// 前 N-1 张已经换代)正是缓存最需要被通知的时刻 —— 那些指向旧一代行的指针
	/// 已经悬垂,而表集合还处在混代状态。所以只要有一张换了代,这个数就 +1,
	/// 哪怕 LoadAll 返回的是 false。
	///
	/// 反过来说:**它不表示一致截面**。想知道当前是不是「全表同一代」,看 IsCoherent()。
	/// 一次全失败的 LoadAll(一张都没换代)不会 +1 —— 没有任何东西过期。
	UFUNCTION(BlueprintPure, Category = "Config")
	int32 GetLoadVersion() const { return LoadVersion; }

	/// 上一次 LoadAll / LoadAllWithProvider 是不是**全部表都成功**。
	/// false = 混代(有表停在旧一代),或者还没装载过。
	/// 这是「一致截面」的那一半语义,GetLoadVersion() 是「失效通知」的那一半。
	UFUNCTION(BlueprintPure, Category = "Config")
	bool IsCoherent() const { return bAllTablesCoherent; }

	/// 上一次 LoadAll 用的目录(LoadAllWithProvider 装的则为空)。
	UFUNCTION(BlueprintPure, Category = "Config")
	FString GetConfigDir() const { return ConfigDir; }

	// ---- 取表 ----

	UFUNCTION(BlueprintPure, Category = "Config|Tables")
	UActorActionCombatStateTable* GetActorActionCombatStateTable() const { return ActorActionCombatStateTable; }

	UFUNCTION(BlueprintPure, Category = "Config|Tables")
	UActorActionStateTable* GetActorActionStateTable() const { return ActorActionStateTable; }

	UFUNCTION(BlueprintPure, Category = "Config|Tables")
	UBaseSceneTable* GetBaseSceneTable() const { return BaseSceneTable; }

	UFUNCTION(BlueprintPure, Category = "Config|Tables")
	UBuffTable* GetBuffTable() const { return BuffTable; }

	UFUNCTION(BlueprintPure, Category = "Config|Tables")
	UClassTable* GetClassTable() const { return ClassTable; }

	UFUNCTION(BlueprintPure, Category = "Config|Tables")
	UConditionTable* GetConditionTable() const { return ConditionTable; }

	UFUNCTION(BlueprintPure, Category = "Config|Tables")
	UCooldownTable* GetCooldownTable() const { return CooldownTable; }

	UFUNCTION(BlueprintPure, Category = "Config|Tables")
	UDungeonTable* GetDungeonTable() const { return DungeonTable; }

	UFUNCTION(BlueprintPure, Category = "Config|Tables")
	UEquipSlotTable* GetEquipSlotTable() const { return EquipSlotTable; }

	UFUNCTION(BlueprintPure, Category = "Config|Tables")
	UGlobalVariableTable* GetGlobalVariableTable() const { return GlobalVariableTable; }

	UFUNCTION(BlueprintPure, Category = "Config|Tables")
	UItemTable* GetItemTable() const { return ItemTable; }

	UFUNCTION(BlueprintPure, Category = "Config|Tables")
	UMessageLimiterTable* GetMessageLimiterTable() const { return MessageLimiterTable; }

	UFUNCTION(BlueprintPure, Category = "Config|Tables")
	UMirrorTable* GetMirrorTable() const { return MirrorTable; }

	UFUNCTION(BlueprintPure, Category = "Config|Tables")
	UMissionTable* GetMissionTable() const { return MissionTable; }

	UFUNCTION(BlueprintPure, Category = "Config|Tables")
	UMonsterTable* GetMonsterTable() const { return MonsterTable; }

	UFUNCTION(BlueprintPure, Category = "Config|Tables")
	URewardTable* GetRewardTable() const { return RewardTable; }

	UFUNCTION(BlueprintPure, Category = "Config|Tables")
	USkillTable* GetSkillTable() const { return SkillTable; }

	UFUNCTION(BlueprintPure, Category = "Config|Tables")
	USkillPermissionTable* GetSkillPermissionTable() const { return SkillPermissionTable; }

	UFUNCTION(BlueprintPure, Category = "Config|Tables")
	UTestTable* GetTestTable() const { return TestTable; }

	UFUNCTION(BlueprintPure, Category = "Config|Tables")
	UTestMultiKeyTable* GetTestMultiKeyTable() const { return TestMultiKeyTable; }

	UFUNCTION(BlueprintPure, Category = "Config|Tables")
	UWorldTable* GetWorldTable() const { return WorldTable; }

private:
	UPROPERTY(Transient)
	TObjectPtr<UActorActionCombatStateTable> ActorActionCombatStateTable;
	UPROPERTY(Transient)
	TObjectPtr<UActorActionStateTable> ActorActionStateTable;
	UPROPERTY(Transient)
	TObjectPtr<UBaseSceneTable> BaseSceneTable;
	UPROPERTY(Transient)
	TObjectPtr<UBuffTable> BuffTable;
	UPROPERTY(Transient)
	TObjectPtr<UClassTable> ClassTable;
	UPROPERTY(Transient)
	TObjectPtr<UConditionTable> ConditionTable;
	UPROPERTY(Transient)
	TObjectPtr<UCooldownTable> CooldownTable;
	UPROPERTY(Transient)
	TObjectPtr<UDungeonTable> DungeonTable;
	UPROPERTY(Transient)
	TObjectPtr<UEquipSlotTable> EquipSlotTable;
	UPROPERTY(Transient)
	TObjectPtr<UGlobalVariableTable> GlobalVariableTable;
	UPROPERTY(Transient)
	TObjectPtr<UItemTable> ItemTable;
	UPROPERTY(Transient)
	TObjectPtr<UMessageLimiterTable> MessageLimiterTable;
	UPROPERTY(Transient)
	TObjectPtr<UMirrorTable> MirrorTable;
	UPROPERTY(Transient)
	TObjectPtr<UMissionTable> MissionTable;
	UPROPERTY(Transient)
	TObjectPtr<UMonsterTable> MonsterTable;
	UPROPERTY(Transient)
	TObjectPtr<URewardTable> RewardTable;
	UPROPERTY(Transient)
	TObjectPtr<USkillTable> SkillTable;
	UPROPERTY(Transient)
	TObjectPtr<USkillPermissionTable> SkillPermissionTable;
	UPROPERTY(Transient)
	TObjectPtr<UTestTable> TestTable;
	UPROPERTY(Transient)
	TObjectPtr<UTestMultiKeyTable> TestMultiKeyTable;
	UPROPERTY(Transient)
	TObjectPtr<UWorldTable> WorldTable;

	FString ConfigDir;
	int32 LoadVersion = 0;
	bool bAllTablesCoherent = false;
};
