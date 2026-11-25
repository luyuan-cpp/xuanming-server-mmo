// ---------------------------------------------------------------------------
//  由 Data Table Exporter 从 RankedRule.xlsx 生成。**不要手改**。
//  重新生成:tools/data_table_exporter/run.py
//
//  模块依赖:Core、CoreUObject、Engine、Json、JsonUtilities。
//  可直接抄的一行见 ConfigSubsystem.h 顶部的「Build.cs」一节。
// ---------------------------------------------------------------------------
#include "RankedRuleTable.h"

#include "ConfigSubsystem.h"

#include "Dom/JsonObject.h"
#include "Dom/JsonValue.h"
#include "JsonObjectConverter.h"
#include "Misc/FileHelper.h"
#include "Misc/Paths.h"
#include "Serialization/JsonReader.h"
#include "Serialization/JsonSerializer.h"

// 文件内的静态量一律带表名前缀:UBT 的 unity build 会把多个 .cpp 拼进同一个翻译单元,
// 两张表里同名的文件级 static 会当场重定义。
static const TArray<int32> GRankedRuleEmptyIndices;

/// 把 JSON 里出现过、但 FCfgRankedRuleRow 里没有对应 UPROPERTY 的键报出来。
///
/// 这是本门语言唯一的**静默失效**面:FJsonObjectConverter 找不到同名属性时不报错、
/// 不返回 false,只是跳过 —— 表面上装载成功,那一列全是默认值。改列名 / 改大小写风格
/// 都会踩到。这个审计让它在日志里现形,而不是在玩法里现形。
///
/// 它只查「键有没有对应 UPROPERTY」,**不查值域** —— 值域是下面
/// RankedRuleTableCheckNarrowedRow 的事。
static void RankedRuleTableAuditJsonKeys(const TSharedPtr<FJsonObject>& RowObject, TSet<FString>& CheckedKeys)
{
	const UScriptStruct* RowStruct = FCfgRankedRuleRow::StaticStruct();
	// 用 const auto&:FJsonObject::Values 的**键类型跨 UE 版本变过**(FString -> FSharedString),
	// 写死 TPair<FString, ...> 在新引擎上编不过。键取 *Pair.Key 拿 const TCHAR*。
	for (const auto& Pair : RowObject->Values)
	{
		bool bAlready = false;
		CheckedKeys.Add(Pair.Key, &bAlready);
		if (bAlready)
		{
			continue;
		}
		if (RowStruct->FindPropertyByName(FName(*Pair.Key)) == nullptr)
		{
			UE_LOG(LogConfigTable, Warning,
				TEXT("[RankedRule] rankedrule.json 里的键 '%s' 在 FCfgRankedRuleRow 中没有对应 UPROPERTY,这一列被静默丢弃。导表器与 UE 产物不同代?"),
				*Pair.Key);
		}
	}
}

/// 一个「由无符号收窄而来」的值的值域检查。
///
/// proto 的 uint32/uint64 在 UE 这门是 int32/int64(UHT 不让无符号整数带 Blueprint
/// 说明符,见 CfgRankedRuleRow.h)。收窄本身是刻意的,**但从前是完全静默的**:
/// JSON 里一个 3000000000 会变成 -1294967296,FJsonObjectConverter 一声不吭。
/// 这里在装载路径上把越界值点名报出来。
///
/// 只对**由无符号收窄而来**的列调用 —— 本来就是 int32/int64 的列不查,
/// 负数对它们是合法值,查了就是误报。
///
/// 精度说明:JSON 数字在 FJsonValue 里是 double,超过 2^53 的整数在**解析那一步**
/// 就已经不精确了。所以 uint64 列的边界判定是近似的:它抓得住量级错误(> 2^63),
/// 抓不住 2^53 附近的最后几位 —— 那是 JSON 数字类型本身的下限,不是这个函数的。
static void RankedRuleTableCheckNarrowedValue(const TSharedPtr<FJsonValue>& Value, const TCHAR* ColumnName,
	int32 RowIndex, double MaxValue, const TCHAR* SourceType)
{
	if (!Value.IsValid() || Value->Type != EJson::Number)
	{
		// 缺席(空单元格)/ 非数字:不归这个函数管,键的审计在 AuditJsonKeys。
		return;
	}
	const double Number = Value->AsNumber();
	if (Number < 0.0 || Number > MaxValue)
	{
		UE_LOG(LogConfigTable, Error,
			TEXT("[RankedRule] rankedrule.json data[%d] 的列 '%s'(proto %s)值 %.0f 超出 UE 侧承接类型的范围 [0, %.0f];")
			TEXT("UE 这门把无符号整数收窄成有符号(见 CfgRankedRuleRow.h),这个值会读成负数或被截断。改用 int64 列或把值调小。"),
			RowIndex, ColumnName, SourceType, Number, MaxValue);
	}
}

/// 取一个键的值做检查;键是数组时逐个元素查。
static void RankedRuleTableCheckNarrowedField(const TSharedPtr<FJsonObject>& Object, const TCHAR* Key,
	const TCHAR* ColumnName, int32 RowIndex, double MaxValue, const TCHAR* SourceType)
{
	// 与本文件其它地方一致,用 Values.Find 而不是 TryGetField:
	// FJsonObject 的取值 API 跨 UE 版本改过签名,Values 这条路最稳。
	const TSharedPtr<FJsonValue>* Field = Object->Values.Find(Key);
	if (Field == nullptr || !Field->IsValid())
	{
		return;
	}
	if ((*Field)->Type == EJson::Array)
	{
		for (const TSharedPtr<FJsonValue>& Element : (*Field)->AsArray())
		{
			RankedRuleTableCheckNarrowedValue(Element, ColumnName, RowIndex, MaxValue, SourceType);
		}
		return;
	}
	RankedRuleTableCheckNarrowedValue(*Field, ColumnName, RowIndex, MaxValue, SourceType);
}

/// 一行里全部「由无符号收窄而来」的列的值域检查。
static void RankedRuleTableCheckNarrowedRow(const TSharedPtr<FJsonObject>& RowObject, int32 RowIndex)
{
	RankedRuleTableCheckNarrowedField(RowObject, TEXT("id"), TEXT("id"), RowIndex, 2147483647.0, TEXT("uint32"));
	RankedRuleTableCheckNarrowedField(RowObject, TEXT("rule_version"), TEXT("rule_version"), RowIndex, 2147483647.0, TEXT("uint32"));
	RankedRuleTableCheckNarrowedField(RowObject, TEXT("battle_config_id"), TEXT("battle_config_id"), RowIndex, 2147483647.0, TEXT("uint32"));
	RankedRuleTableCheckNarrowedField(RowObject, TEXT("reference_class_id"), TEXT("reference_class_id"), RowIndex, 2147483647.0, TEXT("uint32"));
	RankedRuleTableCheckNarrowedField(RowObject, TEXT("competitive_level"), TEXT("competitive_level"), RowIndex, 2147483647.0, TEXT("uint32"));
	RankedRuleTableCheckNarrowedField(RowObject, TEXT("default_character_preset_id"), TEXT("default_character_preset_id"), RowIndex, 2147483647.0, TEXT("uint32"));
	RankedRuleTableCheckNarrowedField(RowObject, TEXT("default_pet_preset_id"), TEXT("default_pet_preset_id"), RowIndex, 2147483647.0, TEXT("uint32"));
	RankedRuleTableCheckNarrowedField(RowObject, TEXT("season_duration_days"), TEXT("season_duration_days"), RowIndex, 2147483647.0, TEXT("uint32"));
	RankedRuleTableCheckNarrowedField(RowObject, TEXT("wait_prompt_seconds"), TEXT("wait_prompt_seconds"), RowIndex, 2147483647.0, TEXT("uint32"));
	RankedRuleTableCheckNarrowedField(RowObject, TEXT("spread_tier1_seconds"), TEXT("spread_tier1_seconds"), RowIndex, 2147483647.0, TEXT("uint32"));
	RankedRuleTableCheckNarrowedField(RowObject, TEXT("spread_tier1_points"), TEXT("spread_tier1_points"), RowIndex, 2147483647.0, TEXT("uint32"));
	RankedRuleTableCheckNarrowedField(RowObject, TEXT("spread_tier2_seconds"), TEXT("spread_tier2_seconds"), RowIndex, 2147483647.0, TEXT("uint32"));
	RankedRuleTableCheckNarrowedField(RowObject, TEXT("spread_tier2_points"), TEXT("spread_tier2_points"), RowIndex, 2147483647.0, TEXT("uint32"));
	RankedRuleTableCheckNarrowedField(RowObject, TEXT("spread_tier3_points"), TEXT("spread_tier3_points"), RowIndex, 2147483647.0, TEXT("uint32"));
	RankedRuleTableCheckNarrowedField(RowObject, TEXT("milestone_avatar_matches"), TEXT("milestone_avatar_matches"), RowIndex, 2147483647.0, TEXT("uint32"));
	RankedRuleTableCheckNarrowedField(RowObject, TEXT("milestone_card_matches"), TEXT("milestone_card_matches"), RowIndex, 2147483647.0, TEXT("uint32"));
	RankedRuleTableCheckNarrowedField(RowObject, TEXT("milestone_talisman_matches"), TEXT("milestone_talisman_matches"), RowIndex, 2147483647.0, TEXT("uint32"));
	RankedRuleTableCheckNarrowedField(RowObject, TEXT("tier_qingfeng_centi"), TEXT("tier_qingfeng_centi"), RowIndex, 2147483647.0, TEXT("uint32"));
	RankedRuleTableCheckNarrowedField(RowObject, TEXT("tier_yunzhang_centi"), TEXT("tier_yunzhang_centi"), RowIndex, 2147483647.0, TEXT("uint32"));
	RankedRuleTableCheckNarrowedField(RowObject, TEXT("tier_tianshu_centi"), TEXT("tier_tianshu_centi"), RowIndex, 2147483647.0, TEXT("uint32"));
	RankedRuleTableCheckNarrowedField(RowObject, TEXT("tier_wushuang_centi"), TEXT("tier_wushuang_centi"), RowIndex, 2147483647.0, TEXT("uint32"));
	RankedRuleTableCheckNarrowedField(RowObject, TEXT("milestone_win_matches"), TEXT("milestone_win_matches"), RowIndex, 2147483647.0, TEXT("uint32"));
}

bool URankedRuleTable::LoadFromJson(const FString& JsonText, FString& OutError)
{
	TSharedPtr<FJsonObject> Root;
	const TSharedRef<TJsonReader<TCHAR>> Reader = TJsonReaderFactory<TCHAR>::Create(JsonText);
	if (!FJsonSerializer::Deserialize(Reader, Root) || !Root.IsValid())
	{
		OutError = FString::Printf(TEXT("[RankedRule] %s 不是合法 JSON: %s"), TEXT("rankedrule.json"), *Reader->GetErrorMessage());
		return false;
	}

	// 顶层形状是 {"data": [ {...}, ... ]}(见 core/generators/json_gen.py)。
	const TSharedPtr<FJsonValue>* DataField = Root->Values.Find(TEXT("data"));
	if (DataField == nullptr || !DataField->IsValid() || (*DataField)->Type != EJson::Array)
	{
		OutError = TEXT("[RankedRule] rankedrule.json 顶层缺少数组字段 \"data\"");
		return false;
	}
	const TArray<TSharedPtr<FJsonValue>>& DataArray = (*DataField)->AsArray();

	// 整批建新快照;中途任何一行失败都直接返回,**现有快照原封不动**。
	TSharedRef<FSnapshot> NewSnapshot = MakeShared<FSnapshot>();
	NewSnapshot->Rows.Reserve(DataArray.Num());

	TSet<FString> CheckedKeys;
	for (int32 RowIndex = 0; RowIndex < DataArray.Num(); ++RowIndex)
	{
		const TSharedPtr<FJsonValue>& Element = DataArray[RowIndex];
		if (!Element.IsValid() || Element->Type != EJson::Object)
		{
			OutError = FString::Printf(TEXT("[RankedRule] data[%d] 不是对象"), RowIndex);
			return false;
		}
		const TSharedPtr<FJsonObject>& RowObject = Element->AsObject();
		RankedRuleTableAuditJsonKeys(RowObject, CheckedKeys);
		// 值域检查只打日志、不中断装载:一个越界的格子不该让整张表停在旧一代,
		// 但它必须在日志里有名有姓。
		RankedRuleTableCheckNarrowedRow(RowObject, RowIndex);

		// 必须 value-init:导表器的 JSON **按行省略空单元格**,而 FJsonObjectConverter
		// 对缺席的键是静默跳过、不写。写成 `Row;` 那些列就是未初始化内存 ——
		// 不是 0,不报错,shipping 里一样。
		FCfgRankedRuleRow Row{};
		// CheckFlags/SkipFlags 都传 0:全部 UPROPERTY 都参与匹配。
		// 匹配靠「属性名 == JSON 键」(TMap<FString> 查找,忽略大小写),所以
		// FCfgRankedRuleRow 的字段名是 snake_case —— 见 CfgRankedRuleRow.h 的文件头。
		if (!FJsonObjectConverter::JsonObjectToUStruct<FCfgRankedRuleRow>(RowObject.ToSharedRef(), &Row, 0, 0))
		{
			OutError = FString::Printf(TEXT("[RankedRule] data[%d] 转 FCfgRankedRuleRow 失败"), RowIndex);
			return false;
		}
		NewSnapshot->Rows.Add(MoveTemp(Row));
	}

	BuildIndices(NewSnapshot.Get());

	// 原子换代:旧快照在最后一个 PinSnapshot() 持有者放手时析构。
	Snapshot = NewSnapshot;
	UE_LOG(LogConfigTable, Log, TEXT("[RankedRule] 装载 %d 行"), NewSnapshot->Rows.Num());
	return true;
}

bool URankedRuleTable::LoadFromDir(const FString& ConfigDir, FString& OutError)
{
	const FString Path = FPaths::Combine(ConfigDir, FileName());
	FString JsonText;
	if (!FFileHelper::LoadFileToString(JsonText, *Path))
	{
		OutError = FString::Printf(TEXT("[RankedRule] 读不到配置文件: %s"), *Path);
		return false;
	}
	return LoadFromJson(JsonText, OutError);
}

void URankedRuleTable::BuildIndices(FSnapshot& Snap)
{
	const int32 RowCount = Snap.Rows.Num();
	Snap.IdIndex.Reserve(RowCount);
	for (int32 RowIndex = 0; RowIndex < RowCount; ++RowIndex)
	{
		const FCfgRankedRuleRow& Row = Snap.Rows[RowIndex];
		// 重复 id 会覆盖前一行。这张表的主键**不是** (cfg_multi),重复即配置错误。
		if (int32* Existing = Snap.IdIndex.Find(Row.id))
		{
			UE_LOG(LogConfigTable, Error,
				TEXT("[RankedRule] id=%s 重复(行 %d 与行 %d);这张表没有标 (cfg_multi),后一行会覆盖前一行"),
				*LexToString(Row.id), *Existing, RowIndex);
		}
		Snap.IdIndex.Add(Row.id, RowIndex);
		Snap.BattleConfigIdIndex.FindOrAdd(Row.battle_config_id).Add(RowIndex);
		Snap.ReferenceClassIdIndex.FindOrAdd(Row.reference_class_id).Add(RowIndex);
	}
}

const FCfgRankedRuleRow* URankedRuleTable::FindById(int32 Id) const
{
	const FCfgRankedRuleRow* Row = FindByIdSilent(Id);
	if (Row == nullptr)
	{
		UE_LOG(LogConfigTable, Warning, TEXT("[RankedRule] 查不到 id=%s"), *LexToString(Id));
	}
	return Row;
}

const FCfgRankedRuleRow* URankedRuleTable::FindByIdSilent(int32 Id) const
{
	const int32* RowIndex = Snapshot->IdIndex.Find(Id);
	return RowIndex != nullptr ? &Snapshot->Rows[*RowIndex] : nullptr;
}

bool URankedRuleTable::K2_FindById(int32 Id, FCfgRankedRuleRow& OutRow) const
{
	if (const FCfgRankedRuleRow* Row = FindByIdSilent(Id))
	{
		OutRow = *Row;
		return true;
	}
	OutRow = FCfgRankedRuleRow();
	return false;
}

TArray<const FCfgRankedRuleRow*> URankedRuleTable::FindByIds(const TArray<int32>& Ids) const
{
	TArray<const FCfgRankedRuleRow*> Result;
	Result.Reserve(Ids.Num());
	for (const int32 Id : Ids)
	{
		if (const FCfgRankedRuleRow* Row = FindByIdSilent(Id))
		{
			Result.Add(Row);
		}
	}
	return Result;
}

const FCfgRankedRuleRow* URankedRuleTable::RandOne() const
{
	// 返回的指针属于当前快照,只在下一次 Load() 之前有效 —— 调用方只存 id。
	if (Snapshot->Rows.Num() == 0)
	{
		return nullptr;
	}
	return &Snapshot->Rows[FMath::RandRange(0, Snapshot->Rows.Num() - 1)];
}

TArray<const FCfgRankedRuleRow*> URankedRuleTable::GetByBattleConfigId(int32 Key) const
{
	TArray<const FCfgRankedRuleRow*> Result;
	const TArray<int32>& Indices = GetBattleConfigIdIndices(Key);
	Result.Reserve(Indices.Num());
	for (const int32 RowIndex : Indices)
	{
		Result.Add(&Snapshot->Rows[RowIndex]);
	}
	return Result;
}

const TArray<int32>& URankedRuleTable::GetBattleConfigIdIndices(int32 Key) const
{
	const TArray<int32>* Found = Snapshot->BattleConfigIdIndex.Find(Key);
	return Found != nullptr ? *Found : GRankedRuleEmptyIndices;
}

int32 URankedRuleTable::CountByBattleConfigIdIndex(int32 Key) const
{
	return GetBattleConfigIdIndices(Key).Num();
}

TArray<const FCfgRankedRuleRow*> URankedRuleTable::GetByReferenceClassId(int32 Key) const
{
	TArray<const FCfgRankedRuleRow*> Result;
	const TArray<int32>& Indices = GetReferenceClassIdIndices(Key);
	Result.Reserve(Indices.Num());
	for (const int32 RowIndex : Indices)
	{
		Result.Add(&Snapshot->Rows[RowIndex]);
	}
	return Result;
}

const TArray<int32>& URankedRuleTable::GetReferenceClassIdIndices(int32 Key) const
{
	const TArray<int32>* Found = Snapshot->ReferenceClassIdIndex.Find(Key);
	return Found != nullptr ? *Found : GRankedRuleEmptyIndices;
}

int32 URankedRuleTable::CountByReferenceClassIdIndex(int32 Key) const
{
	return GetReferenceClassIdIndices(Key).Num();
}

TArray<const FCfgRankedRuleRow*> URankedRuleTable::Where(TFunctionRef<bool(const FCfgRankedRuleRow&)> Pred) const
{
	TArray<const FCfgRankedRuleRow*> Result;
	for (const FCfgRankedRuleRow& Row : Snapshot->Rows)
	{
		if (Pred(Row))
		{
			Result.Add(&Row);
		}
	}
	return Result;
}

const FCfgRankedRuleRow* URankedRuleTable::First(TFunctionRef<bool(const FCfgRankedRuleRow&)> Pred) const
{
	for (const FCfgRankedRuleRow& Row : Snapshot->Rows)
	{
		if (Pred(Row))
		{
			return &Row;
		}
	}
	return nullptr;
}
