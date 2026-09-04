// ---------------------------------------------------------------------------
//  由 Data Table Exporter 生成。**不要手改**。
//  重新生成:tools/data_table_exporter/run.py
//
//  模块依赖(Build.cs)见 ConfigSubsystem.h 顶部。
// ---------------------------------------------------------------------------
#include "ConfigSubsystem.h"

#include "Engine/GameInstance.h"
#include "Engine/World.h"
#include "Misc/Paths.h"

DEFINE_LOG_CATEGORY(LogConfigTable);

UConfigSubsystem* UConfigSubsystem::Get(const UObject* WorldContextObject)
{
	if (WorldContextObject == nullptr)
	{
		return nullptr;
	}
	const UWorld* World = WorldContextObject->GetWorld();
	if (World == nullptr)
	{
		return nullptr;
	}
	UGameInstance* GameInstance = World->GetGameInstance();
	return GameInstance != nullptr ? GameInstance->GetSubsystem<UConfigSubsystem>() : nullptr;
}

void UConfigSubsystem::Initialize(FSubsystemCollectionBase& Collection)
{
	Super::Initialize(Collection);

	// 只建管理器,**不装载**:配置文件从哪来是工程决策(Content 目录 / pak /
	// 下载目录),生成器不替工程猜。装载由调用方显式调 LoadAll。
	ActorActionCombatStateTable = NewObject<UActorActionCombatStateTable>(this);
	ActorActionStateTable = NewObject<UActorActionStateTable>(this);
	BaseSceneTable = NewObject<UBaseSceneTable>(this);
	BuffTable = NewObject<UBuffTable>(this);
	ClassTable = NewObject<UClassTable>(this);
	ConditionTable = NewObject<UConditionTable>(this);
	CooldownTable = NewObject<UCooldownTable>(this);
	DungeonTable = NewObject<UDungeonTable>(this);
	EquipSlotTable = NewObject<UEquipSlotTable>(this);
	GlobalVariableTable = NewObject<UGlobalVariableTable>(this);
	ItemTable = NewObject<UItemTable>(this);
	MessageLimiterTable = NewObject<UMessageLimiterTable>(this);
	MirrorTable = NewObject<UMirrorTable>(this);
	MissionTable = NewObject<UMissionTable>(this);
	MonsterTable = NewObject<UMonsterTable>(this);
	RewardTable = NewObject<URewardTable>(this);
	SkillTable = NewObject<USkillTable>(this);
	SkillPermissionTable = NewObject<USkillPermissionTable>(this);
	TestTable = NewObject<UTestTable>(this);
	TestMultiKeyTable = NewObject<UTestMultiKeyTable>(this);
	WorldTable = NewObject<UWorldTable>(this);
}

void UConfigSubsystem::Deinitialize()
{
	ActorActionCombatStateTable = nullptr;
	ActorActionStateTable = nullptr;
	BaseSceneTable = nullptr;
	BuffTable = nullptr;
	ClassTable = nullptr;
	ConditionTable = nullptr;
	CooldownTable = nullptr;
	DungeonTable = nullptr;
	EquipSlotTable = nullptr;
	GlobalVariableTable = nullptr;
	ItemTable = nullptr;
	MessageLimiterTable = nullptr;
	MirrorTable = nullptr;
	MissionTable = nullptr;
	MonsterTable = nullptr;
	RewardTable = nullptr;
	SkillTable = nullptr;
	SkillPermissionTable = nullptr;
	TestTable = nullptr;
	TestMultiKeyTable = nullptr;
	WorldTable = nullptr;
	ConfigDir.Reset();
	LoadVersion = 0;
	bAllTablesCoherent = false;
	Super::Deinitialize();
}

TArray<FString> UConfigSubsystem::TableFileNames()
{
	return TArray<FString>{
		UActorActionCombatStateTable::FileName(),
		UActorActionStateTable::FileName(),
		UBaseSceneTable::FileName(),
		UBuffTable::FileName(),
		UClassTable::FileName(),
		UConditionTable::FileName(),
		UCooldownTable::FileName(),
		UDungeonTable::FileName(),
		UEquipSlotTable::FileName(),
		UGlobalVariableTable::FileName(),
		UItemTable::FileName(),
		UMessageLimiterTable::FileName(),
		UMirrorTable::FileName(),
		UMissionTable::FileName(),
		UMonsterTable::FileName(),
		URewardTable::FileName(),
		USkillTable::FileName(),
		USkillPermissionTable::FileName(),
		UTestTable::FileName(),
		UTestMultiKeyTable::FileName(),
		UWorldTable::FileName()
	};
}

bool UConfigSubsystem::LoadAll(const FString& InConfigDir)
{
	bool bAllOk = true;
	// 单张表的 Load 成功 == 那张表换了代(失败时它原封不动留在旧一代)。
	// LoadVersion 是失效通知,所以只要**有一张**换了代就得 +1 —— 见 ConfigSubsystem.h。
	bool bAnySwapped = false;
	FString Error;

	if (ActorActionCombatStateTable == nullptr)
	{
		UE_LOG(LogConfigTable, Error, TEXT("[ActorActionCombatState] 管理器未创建"));
		bAllOk = false;
	}
	else if (!ActorActionCombatStateTable->LoadFromDir(InConfigDir, Error))
	{
		UE_LOG(LogConfigTable, Error, TEXT("%s"), *Error);
		bAllOk = false;
	}
	else
	{
		bAnySwapped = true;
	}
	if (ActorActionStateTable == nullptr)
	{
		UE_LOG(LogConfigTable, Error, TEXT("[ActorActionState] 管理器未创建"));
		bAllOk = false;
	}
	else if (!ActorActionStateTable->LoadFromDir(InConfigDir, Error))
	{
		UE_LOG(LogConfigTable, Error, TEXT("%s"), *Error);
		bAllOk = false;
	}
	else
	{
		bAnySwapped = true;
	}
	if (BaseSceneTable == nullptr)
	{
		UE_LOG(LogConfigTable, Error, TEXT("[BaseScene] 管理器未创建"));
		bAllOk = false;
	}
	else if (!BaseSceneTable->LoadFromDir(InConfigDir, Error))
	{
		UE_LOG(LogConfigTable, Error, TEXT("%s"), *Error);
		bAllOk = false;
	}
	else
	{
		bAnySwapped = true;
	}
	if (BuffTable == nullptr)
	{
		UE_LOG(LogConfigTable, Error, TEXT("[Buff] 管理器未创建"));
		bAllOk = false;
	}
	else if (!BuffTable->LoadFromDir(InConfigDir, Error))
	{
		UE_LOG(LogConfigTable, Error, TEXT("%s"), *Error);
		bAllOk = false;
	}
	else
	{
		bAnySwapped = true;
	}
	if (ClassTable == nullptr)
	{
		UE_LOG(LogConfigTable, Error, TEXT("[Class] 管理器未创建"));
		bAllOk = false;
	}
	else if (!ClassTable->LoadFromDir(InConfigDir, Error))
	{
		UE_LOG(LogConfigTable, Error, TEXT("%s"), *Error);
		bAllOk = false;
	}
	else
	{
		bAnySwapped = true;
	}
	if (ConditionTable == nullptr)
	{
		UE_LOG(LogConfigTable, Error, TEXT("[Condition] 管理器未创建"));
		bAllOk = false;
	}
	else if (!ConditionTable->LoadFromDir(InConfigDir, Error))
	{
		UE_LOG(LogConfigTable, Error, TEXT("%s"), *Error);
		bAllOk = false;
	}
	else
	{
		bAnySwapped = true;
	}
	if (CooldownTable == nullptr)
	{
		UE_LOG(LogConfigTable, Error, TEXT("[Cooldown] 管理器未创建"));
		bAllOk = false;
	}
	else if (!CooldownTable->LoadFromDir(InConfigDir, Error))
	{
		UE_LOG(LogConfigTable, Error, TEXT("%s"), *Error);
		bAllOk = false;
	}
	else
	{
		bAnySwapped = true;
	}
	if (DungeonTable == nullptr)
	{
		UE_LOG(LogConfigTable, Error, TEXT("[Dungeon] 管理器未创建"));
		bAllOk = false;
	}
	else if (!DungeonTable->LoadFromDir(InConfigDir, Error))
	{
		UE_LOG(LogConfigTable, Error, TEXT("%s"), *Error);
		bAllOk = false;
	}
	else
	{
		bAnySwapped = true;
	}
	if (EquipSlotTable == nullptr)
	{
		UE_LOG(LogConfigTable, Error, TEXT("[EquipSlot] 管理器未创建"));
		bAllOk = false;
	}
	else if (!EquipSlotTable->LoadFromDir(InConfigDir, Error))
	{
		UE_LOG(LogConfigTable, Error, TEXT("%s"), *Error);
		bAllOk = false;
	}
	else
	{
		bAnySwapped = true;
	}
	if (GlobalVariableTable == nullptr)
	{
		UE_LOG(LogConfigTable, Error, TEXT("[GlobalVariable] 管理器未创建"));
		bAllOk = false;
	}
	else if (!GlobalVariableTable->LoadFromDir(InConfigDir, Error))
	{
		UE_LOG(LogConfigTable, Error, TEXT("%s"), *Error);
		bAllOk = false;
	}
	else
	{
		bAnySwapped = true;
	}
	if (ItemTable == nullptr)
	{
		UE_LOG(LogConfigTable, Error, TEXT("[Item] 管理器未创建"));
		bAllOk = false;
	}
	else if (!ItemTable->LoadFromDir(InConfigDir, Error))
	{
		UE_LOG(LogConfigTable, Error, TEXT("%s"), *Error);
		bAllOk = false;
	}
	else
	{
		bAnySwapped = true;
	}
	if (MessageLimiterTable == nullptr)
	{
		UE_LOG(LogConfigTable, Error, TEXT("[MessageLimiter] 管理器未创建"));
		bAllOk = false;
	}
	else if (!MessageLimiterTable->LoadFromDir(InConfigDir, Error))
	{
		UE_LOG(LogConfigTable, Error, TEXT("%s"), *Error);
		bAllOk = false;
	}
	else
	{
		bAnySwapped = true;
	}
	if (MirrorTable == nullptr)
	{
		UE_LOG(LogConfigTable, Error, TEXT("[Mirror] 管理器未创建"));
		bAllOk = false;
	}
	else if (!MirrorTable->LoadFromDir(InConfigDir, Error))
	{
		UE_LOG(LogConfigTable, Error, TEXT("%s"), *Error);
		bAllOk = false;
	}
	else
	{
		bAnySwapped = true;
	}
	if (MissionTable == nullptr)
	{
		UE_LOG(LogConfigTable, Error, TEXT("[Mission] 管理器未创建"));
		bAllOk = false;
	}
	else if (!MissionTable->LoadFromDir(InConfigDir, Error))
	{
		UE_LOG(LogConfigTable, Error, TEXT("%s"), *Error);
		bAllOk = false;
	}
	else
	{
		bAnySwapped = true;
	}
	if (MonsterTable == nullptr)
	{
		UE_LOG(LogConfigTable, Error, TEXT("[Monster] 管理器未创建"));
		bAllOk = false;
	}
	else if (!MonsterTable->LoadFromDir(InConfigDir, Error))
	{
		UE_LOG(LogConfigTable, Error, TEXT("%s"), *Error);
		bAllOk = false;
	}
	else
	{
		bAnySwapped = true;
	}
	if (RewardTable == nullptr)
	{
		UE_LOG(LogConfigTable, Error, TEXT("[Reward] 管理器未创建"));
		bAllOk = false;
	}
	else if (!RewardTable->LoadFromDir(InConfigDir, Error))
	{
		UE_LOG(LogConfigTable, Error, TEXT("%s"), *Error);
		bAllOk = false;
	}
	else
	{
		bAnySwapped = true;
	}
	if (SkillTable == nullptr)
	{
		UE_LOG(LogConfigTable, Error, TEXT("[Skill] 管理器未创建"));
		bAllOk = false;
	}
	else if (!SkillTable->LoadFromDir(InConfigDir, Error))
	{
		UE_LOG(LogConfigTable, Error, TEXT("%s"), *Error);
		bAllOk = false;
	}
	else
	{
		bAnySwapped = true;
	}
	if (SkillPermissionTable == nullptr)
	{
		UE_LOG(LogConfigTable, Error, TEXT("[SkillPermission] 管理器未创建"));
		bAllOk = false;
	}
	else if (!SkillPermissionTable->LoadFromDir(InConfigDir, Error))
	{
		UE_LOG(LogConfigTable, Error, TEXT("%s"), *Error);
		bAllOk = false;
	}
	else
	{
		bAnySwapped = true;
	}
	if (TestTable == nullptr)
	{
		UE_LOG(LogConfigTable, Error, TEXT("[Test] 管理器未创建"));
		bAllOk = false;
	}
	else if (!TestTable->LoadFromDir(InConfigDir, Error))
	{
		UE_LOG(LogConfigTable, Error, TEXT("%s"), *Error);
		bAllOk = false;
	}
	else
	{
		bAnySwapped = true;
	}
	if (TestMultiKeyTable == nullptr)
	{
		UE_LOG(LogConfigTable, Error, TEXT("[TestMultiKey] 管理器未创建"));
		bAllOk = false;
	}
	else if (!TestMultiKeyTable->LoadFromDir(InConfigDir, Error))
	{
		UE_LOG(LogConfigTable, Error, TEXT("%s"), *Error);
		bAllOk = false;
	}
	else
	{
		bAnySwapped = true;
	}
	if (WorldTable == nullptr)
	{
		UE_LOG(LogConfigTable, Error, TEXT("[World] 管理器未创建"));
		bAllOk = false;
	}
	else if (!WorldTable->LoadFromDir(InConfigDir, Error))
	{
		UE_LOG(LogConfigTable, Error, TEXT("%s"), *Error);
		bAllOk = false;
	}
	else
	{
		bAnySwapped = true;
	}

	ConfigDir = InConfigDir;
	bAllTablesCoherent = bAllOk;
	if (bAnySwapped)
	{
		++LoadVersion;
	}
	if (bAllOk)
	{
		UE_LOG(LogConfigTable, Log, TEXT("全部 %d 张配置表装载完成(第 %d 代): %s"), TableCount, LoadVersion, *InConfigDir);
	}
	else if (bAnySwapped)
	{
		// 失败的表留在旧一代 —— 这里是「部分新、部分旧」,不是一个一致的截面。
		// LoadVersion 照样 +1:混代恰恰是缓存最需要被通知的情形。
		UE_LOG(LogConfigTable, Error,
			TEXT("配置表装载有失败项,表集合处于混代状态(失效版本号已 +1 到 %d): %s"), LoadVersion, *InConfigDir);
	}
	else
	{
		// 一张都没换代:没有任何东西过期,版本号不动。
		UE_LOG(LogConfigTable, Error, TEXT("配置表全部装载失败,表集合原封不动(仍为第 %d 代): %s"), LoadVersion, *InConfigDir);
	}
	return bAllOk;
}

bool UConfigSubsystem::LoadAllWithProvider(TFunctionRef<bool(const TCHAR*, FString&)> TextProvider)
{
	bool bAllOk = true;
	bool bAnySwapped = false;
	FString Error;
	FString JsonText;

	JsonText.Reset();
	if (ActorActionCombatStateTable == nullptr)
	{
		UE_LOG(LogConfigTable, Error, TEXT("[ActorActionCombatState] 管理器未创建"));
		bAllOk = false;
	}
	else if (!TextProvider(UActorActionCombatStateTable::FileName(), JsonText))
	{
		UE_LOG(LogConfigTable, Error, TEXT("[ActorActionCombatState] 取不到 %s"), UActorActionCombatStateTable::FileName());
		bAllOk = false;
	}
	else if (!ActorActionCombatStateTable->LoadFromJson(JsonText, Error))
	{
		UE_LOG(LogConfigTable, Error, TEXT("%s"), *Error);
		bAllOk = false;
	}
	else
	{
		bAnySwapped = true;
	}
	JsonText.Reset();
	if (ActorActionStateTable == nullptr)
	{
		UE_LOG(LogConfigTable, Error, TEXT("[ActorActionState] 管理器未创建"));
		bAllOk = false;
	}
	else if (!TextProvider(UActorActionStateTable::FileName(), JsonText))
	{
		UE_LOG(LogConfigTable, Error, TEXT("[ActorActionState] 取不到 %s"), UActorActionStateTable::FileName());
		bAllOk = false;
	}
	else if (!ActorActionStateTable->LoadFromJson(JsonText, Error))
	{
		UE_LOG(LogConfigTable, Error, TEXT("%s"), *Error);
		bAllOk = false;
	}
	else
	{
		bAnySwapped = true;
	}
	JsonText.Reset();
	if (BaseSceneTable == nullptr)
	{
		UE_LOG(LogConfigTable, Error, TEXT("[BaseScene] 管理器未创建"));
		bAllOk = false;
	}
	else if (!TextProvider(UBaseSceneTable::FileName(), JsonText))
	{
		UE_LOG(LogConfigTable, Error, TEXT("[BaseScene] 取不到 %s"), UBaseSceneTable::FileName());
		bAllOk = false;
	}
	else if (!BaseSceneTable->LoadFromJson(JsonText, Error))
	{
		UE_LOG(LogConfigTable, Error, TEXT("%s"), *Error);
		bAllOk = false;
	}
	else
	{
		bAnySwapped = true;
	}
	JsonText.Reset();
	if (BuffTable == nullptr)
	{
		UE_LOG(LogConfigTable, Error, TEXT("[Buff] 管理器未创建"));
		bAllOk = false;
	}
	else if (!TextProvider(UBuffTable::FileName(), JsonText))
	{
		UE_LOG(LogConfigTable, Error, TEXT("[Buff] 取不到 %s"), UBuffTable::FileName());
		bAllOk = false;
	}
	else if (!BuffTable->LoadFromJson(JsonText, Error))
	{
		UE_LOG(LogConfigTable, Error, TEXT("%s"), *Error);
		bAllOk = false;
	}
	else
	{
		bAnySwapped = true;
	}
	JsonText.Reset();
	if (ClassTable == nullptr)
	{
		UE_LOG(LogConfigTable, Error, TEXT("[Class] 管理器未创建"));
		bAllOk = false;
	}
	else if (!TextProvider(UClassTable::FileName(), JsonText))
	{
		UE_LOG(LogConfigTable, Error, TEXT("[Class] 取不到 %s"), UClassTable::FileName());
		bAllOk = false;
	}
	else if (!ClassTable->LoadFromJson(JsonText, Error))
	{
		UE_LOG(LogConfigTable, Error, TEXT("%s"), *Error);
		bAllOk = false;
	}
	else
	{
		bAnySwapped = true;
	}
	JsonText.Reset();
	if (ConditionTable == nullptr)
	{
		UE_LOG(LogConfigTable, Error, TEXT("[Condition] 管理器未创建"));
		bAllOk = false;
	}
	else if (!TextProvider(UConditionTable::FileName(), JsonText))
	{
		UE_LOG(LogConfigTable, Error, TEXT("[Condition] 取不到 %s"), UConditionTable::FileName());
		bAllOk = false;
	}
	else if (!ConditionTable->LoadFromJson(JsonText, Error))
	{
		UE_LOG(LogConfigTable, Error, TEXT("%s"), *Error);
		bAllOk = false;
	}
	else
	{
		bAnySwapped = true;
	}
	JsonText.Reset();
	if (CooldownTable == nullptr)
	{
		UE_LOG(LogConfigTable, Error, TEXT("[Cooldown] 管理器未创建"));
		bAllOk = false;
	}
	else if (!TextProvider(UCooldownTable::FileName(), JsonText))
	{
		UE_LOG(LogConfigTable, Error, TEXT("[Cooldown] 取不到 %s"), UCooldownTable::FileName());
		bAllOk = false;
	}
	else if (!CooldownTable->LoadFromJson(JsonText, Error))
	{
		UE_LOG(LogConfigTable, Error, TEXT("%s"), *Error);
		bAllOk = false;
	}
	else
	{
		bAnySwapped = true;
	}
	JsonText.Reset();
	if (DungeonTable == nullptr)
	{
		UE_LOG(LogConfigTable, Error, TEXT("[Dungeon] 管理器未创建"));
		bAllOk = false;
	}
	else if (!TextProvider(UDungeonTable::FileName(), JsonText))
	{
		UE_LOG(LogConfigTable, Error, TEXT("[Dungeon] 取不到 %s"), UDungeonTable::FileName());
		bAllOk = false;
	}
	else if (!DungeonTable->LoadFromJson(JsonText, Error))
	{
		UE_LOG(LogConfigTable, Error, TEXT("%s"), *Error);
		bAllOk = false;
	}
	else
	{
		bAnySwapped = true;
	}
	JsonText.Reset();
	if (EquipSlotTable == nullptr)
	{
		UE_LOG(LogConfigTable, Error, TEXT("[EquipSlot] 管理器未创建"));
		bAllOk = false;
	}
	else if (!TextProvider(UEquipSlotTable::FileName(), JsonText))
	{
		UE_LOG(LogConfigTable, Error, TEXT("[EquipSlot] 取不到 %s"), UEquipSlotTable::FileName());
		bAllOk = false;
	}
	else if (!EquipSlotTable->LoadFromJson(JsonText, Error))
	{
		UE_LOG(LogConfigTable, Error, TEXT("%s"), *Error);
		bAllOk = false;
	}
	else
	{
		bAnySwapped = true;
	}
	JsonText.Reset();
	if (GlobalVariableTable == nullptr)
	{
		UE_LOG(LogConfigTable, Error, TEXT("[GlobalVariable] 管理器未创建"));
		bAllOk = false;
	}
	else if (!TextProvider(UGlobalVariableTable::FileName(), JsonText))
	{
		UE_LOG(LogConfigTable, Error, TEXT("[GlobalVariable] 取不到 %s"), UGlobalVariableTable::FileName());
		bAllOk = false;
	}
	else if (!GlobalVariableTable->LoadFromJson(JsonText, Error))
	{
		UE_LOG(LogConfigTable, Error, TEXT("%s"), *Error);
		bAllOk = false;
	}
	else
	{
		bAnySwapped = true;
	}
	JsonText.Reset();
	if (ItemTable == nullptr)
	{
		UE_LOG(LogConfigTable, Error, TEXT("[Item] 管理器未创建"));
		bAllOk = false;
	}
	else if (!TextProvider(UItemTable::FileName(), JsonText))
	{
		UE_LOG(LogConfigTable, Error, TEXT("[Item] 取不到 %s"), UItemTable::FileName());
		bAllOk = false;
	}
	else if (!ItemTable->LoadFromJson(JsonText, Error))
	{
		UE_LOG(LogConfigTable, Error, TEXT("%s"), *Error);
		bAllOk = false;
	}
	else
	{
		bAnySwapped = true;
	}
	JsonText.Reset();
	if (MessageLimiterTable == nullptr)
	{
		UE_LOG(LogConfigTable, Error, TEXT("[MessageLimiter] 管理器未创建"));
		bAllOk = false;
	}
	else if (!TextProvider(UMessageLimiterTable::FileName(), JsonText))
	{
		UE_LOG(LogConfigTable, Error, TEXT("[MessageLimiter] 取不到 %s"), UMessageLimiterTable::FileName());
		bAllOk = false;
	}
	else if (!MessageLimiterTable->LoadFromJson(JsonText, Error))
	{
		UE_LOG(LogConfigTable, Error, TEXT("%s"), *Error);
		bAllOk = false;
	}
	else
	{
		bAnySwapped = true;
	}
	JsonText.Reset();
	if (MirrorTable == nullptr)
	{
		UE_LOG(LogConfigTable, Error, TEXT("[Mirror] 管理器未创建"));
		bAllOk = false;
	}
	else if (!TextProvider(UMirrorTable::FileName(), JsonText))
	{
		UE_LOG(LogConfigTable, Error, TEXT("[Mirror] 取不到 %s"), UMirrorTable::FileName());
		bAllOk = false;
	}
	else if (!MirrorTable->LoadFromJson(JsonText, Error))
	{
		UE_LOG(LogConfigTable, Error, TEXT("%s"), *Error);
		bAllOk = false;
	}
	else
	{
		bAnySwapped = true;
	}
	JsonText.Reset();
	if (MissionTable == nullptr)
	{
		UE_LOG(LogConfigTable, Error, TEXT("[Mission] 管理器未创建"));
		bAllOk = false;
	}
	else if (!TextProvider(UMissionTable::FileName(), JsonText))
	{
		UE_LOG(LogConfigTable, Error, TEXT("[Mission] 取不到 %s"), UMissionTable::FileName());
		bAllOk = false;
	}
	else if (!MissionTable->LoadFromJson(JsonText, Error))
	{
		UE_LOG(LogConfigTable, Error, TEXT("%s"), *Error);
		bAllOk = false;
	}
	else
	{
		bAnySwapped = true;
	}
	JsonText.Reset();
	if (MonsterTable == nullptr)
	{
		UE_LOG(LogConfigTable, Error, TEXT("[Monster] 管理器未创建"));
		bAllOk = false;
	}
	else if (!TextProvider(UMonsterTable::FileName(), JsonText))
	{
		UE_LOG(LogConfigTable, Error, TEXT("[Monster] 取不到 %s"), UMonsterTable::FileName());
		bAllOk = false;
	}
	else if (!MonsterTable->LoadFromJson(JsonText, Error))
	{
		UE_LOG(LogConfigTable, Error, TEXT("%s"), *Error);
		bAllOk = false;
	}
	else
	{
		bAnySwapped = true;
	}
	JsonText.Reset();
	if (RewardTable == nullptr)
	{
		UE_LOG(LogConfigTable, Error, TEXT("[Reward] 管理器未创建"));
		bAllOk = false;
	}
	else if (!TextProvider(URewardTable::FileName(), JsonText))
	{
		UE_LOG(LogConfigTable, Error, TEXT("[Reward] 取不到 %s"), URewardTable::FileName());
		bAllOk = false;
	}
	else if (!RewardTable->LoadFromJson(JsonText, Error))
	{
		UE_LOG(LogConfigTable, Error, TEXT("%s"), *Error);
		bAllOk = false;
	}
	else
	{
		bAnySwapped = true;
	}
	JsonText.Reset();
	if (SkillTable == nullptr)
	{
		UE_LOG(LogConfigTable, Error, TEXT("[Skill] 管理器未创建"));
		bAllOk = false;
	}
	else if (!TextProvider(USkillTable::FileName(), JsonText))
	{
		UE_LOG(LogConfigTable, Error, TEXT("[Skill] 取不到 %s"), USkillTable::FileName());
		bAllOk = false;
	}
	else if (!SkillTable->LoadFromJson(JsonText, Error))
	{
		UE_LOG(LogConfigTable, Error, TEXT("%s"), *Error);
		bAllOk = false;
	}
	else
	{
		bAnySwapped = true;
	}
	JsonText.Reset();
	if (SkillPermissionTable == nullptr)
	{
		UE_LOG(LogConfigTable, Error, TEXT("[SkillPermission] 管理器未创建"));
		bAllOk = false;
	}
	else if (!TextProvider(USkillPermissionTable::FileName(), JsonText))
	{
		UE_LOG(LogConfigTable, Error, TEXT("[SkillPermission] 取不到 %s"), USkillPermissionTable::FileName());
		bAllOk = false;
	}
	else if (!SkillPermissionTable->LoadFromJson(JsonText, Error))
	{
		UE_LOG(LogConfigTable, Error, TEXT("%s"), *Error);
		bAllOk = false;
	}
	else
	{
		bAnySwapped = true;
	}
	JsonText.Reset();
	if (TestTable == nullptr)
	{
		UE_LOG(LogConfigTable, Error, TEXT("[Test] 管理器未创建"));
		bAllOk = false;
	}
	else if (!TextProvider(UTestTable::FileName(), JsonText))
	{
		UE_LOG(LogConfigTable, Error, TEXT("[Test] 取不到 %s"), UTestTable::FileName());
		bAllOk = false;
	}
	else if (!TestTable->LoadFromJson(JsonText, Error))
	{
		UE_LOG(LogConfigTable, Error, TEXT("%s"), *Error);
		bAllOk = false;
	}
	else
	{
		bAnySwapped = true;
	}
	JsonText.Reset();
	if (TestMultiKeyTable == nullptr)
	{
		UE_LOG(LogConfigTable, Error, TEXT("[TestMultiKey] 管理器未创建"));
		bAllOk = false;
	}
	else if (!TextProvider(UTestMultiKeyTable::FileName(), JsonText))
	{
		UE_LOG(LogConfigTable, Error, TEXT("[TestMultiKey] 取不到 %s"), UTestMultiKeyTable::FileName());
		bAllOk = false;
	}
	else if (!TestMultiKeyTable->LoadFromJson(JsonText, Error))
	{
		UE_LOG(LogConfigTable, Error, TEXT("%s"), *Error);
		bAllOk = false;
	}
	else
	{
		bAnySwapped = true;
	}
	JsonText.Reset();
	if (WorldTable == nullptr)
	{
		UE_LOG(LogConfigTable, Error, TEXT("[World] 管理器未创建"));
		bAllOk = false;
	}
	else if (!TextProvider(UWorldTable::FileName(), JsonText))
	{
		UE_LOG(LogConfigTable, Error, TEXT("[World] 取不到 %s"), UWorldTable::FileName());
		bAllOk = false;
	}
	else if (!WorldTable->LoadFromJson(JsonText, Error))
	{
		UE_LOG(LogConfigTable, Error, TEXT("%s"), *Error);
		bAllOk = false;
	}
	else
	{
		bAnySwapped = true;
	}

	ConfigDir.Reset();
	bAllTablesCoherent = bAllOk;
	if (bAnySwapped)
	{
		++LoadVersion;
	}
	if (bAllOk)
	{
		UE_LOG(LogConfigTable, Log, TEXT("全部 %d 张配置表装载完成(第 %d 代,provider)"), TableCount, LoadVersion);
	}
	else if (bAnySwapped)
	{
		UE_LOG(LogConfigTable, Error,
			TEXT("配置表装载有失败项,表集合处于混代状态(失效版本号已 +1 到 %d,provider)"), LoadVersion);
	}
	else
	{
		UE_LOG(LogConfigTable, Error, TEXT("配置表全部装载失败,表集合原封不动(仍为第 %d 代,provider)"), LoadVersion);
	}
	return bAllOk;
}

bool UConfigSubsystem::ReloadAll()
{
	if (ConfigDir.IsEmpty())
	{
		UE_LOG(LogConfigTable, Warning, TEXT("ReloadAll 前没有成功的 LoadAll(ConfigDir),无处可重装"));
		return false;
	}
	// 拷一份再传:LoadAll 会写回 ConfigDir,直接把成员按 const& 传进去是自赋值。
	const FString Dir = ConfigDir;
	return LoadAll(Dir);
}
