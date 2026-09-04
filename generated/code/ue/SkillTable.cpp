// ---------------------------------------------------------------------------
//  由 Data Table Exporter 从 Skill.xlsx 生成。**不要手改**。
//  重新生成:tools/data_table_exporter/run.py
//
//  模块依赖:Core、CoreUObject、Engine、Json、JsonUtilities。
//  可直接抄的一行见 ConfigSubsystem.h 顶部的「Build.cs」一节。
// ---------------------------------------------------------------------------
#include "SkillTable.h"

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
static const TArray<int32> GSkillEmptyIndices;

/// 把 JSON 里出现过、但 FCfgSkillRow 里没有对应 UPROPERTY 的键报出来。
///
/// 这是本门语言唯一的**静默失效**面:FJsonObjectConverter 找不到同名属性时不报错、
/// 不返回 false,只是跳过 —— 表面上装载成功,那一列全是默认值。改列名 / 改大小写风格
/// 都会踩到。这个审计让它在日志里现形,而不是在玩法里现形。
///
/// 它只查「键有没有对应 UPROPERTY」,**不查值域** —— 值域是下面
/// SkillTableCheckNarrowedRow 的事。
static void SkillTableAuditJsonKeys(const TSharedPtr<FJsonObject>& RowObject, TSet<FString>& CheckedKeys)
{
	const UScriptStruct* RowStruct = FCfgSkillRow::StaticStruct();
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
				TEXT("[Skill] skill.json 里的键 '%s' 在 FCfgSkillRow 中没有对应 UPROPERTY,这一列被静默丢弃。导表器与 UE 产物不同代?"),
				*Pair.Key);
		}
	}
}

/// 一个「由无符号收窄而来」的值的值域检查。
///
/// proto 的 uint32/uint64 在 UE 这门是 int32/int64(UHT 不让无符号整数带 Blueprint
/// 说明符,见 CfgSkillRow.h)。收窄本身是刻意的,**但从前是完全静默的**:
/// JSON 里一个 3000000000 会变成 -1294967296,FJsonObjectConverter 一声不吭。
/// 这里在装载路径上把越界值点名报出来。
///
/// 只对**由无符号收窄而来**的列调用 —— 本来就是 int32/int64 的列不查,
/// 负数对它们是合法值,查了就是误报。
///
/// 精度说明:JSON 数字在 FJsonValue 里是 double,超过 2^53 的整数在**解析那一步**
/// 就已经不精确了。所以 uint64 列的边界判定是近似的:它抓得住量级错误(> 2^63),
/// 抓不住 2^53 附近的最后几位 —— 那是 JSON 数字类型本身的下限,不是这个函数的。
static void SkillTableCheckNarrowedValue(const TSharedPtr<FJsonValue>& Value, const TCHAR* ColumnName,
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
			TEXT("[Skill] skill.json data[%d] 的列 '%s'(proto %s)值 %.0f 超出 UE 侧承接类型的范围 [0, %.0f];")
			TEXT("UE 这门把无符号整数收窄成有符号(见 CfgSkillRow.h),这个值会读成负数或被截断。改用 int64 列或把值调小。"),
			RowIndex, ColumnName, SourceType, Number, MaxValue);
	}
}

/// 取一个键的值做检查;键是数组时逐个元素查。
static void SkillTableCheckNarrowedField(const TSharedPtr<FJsonObject>& Object, const TCHAR* Key,
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
			SkillTableCheckNarrowedValue(Element, ColumnName, RowIndex, MaxValue, SourceType);
		}
		return;
	}
	SkillTableCheckNarrowedValue(*Field, ColumnName, RowIndex, MaxValue, SourceType);
}

/// 一行里全部「由无符号收窄而来」的列的值域检查。
static void SkillTableCheckNarrowedRow(const TSharedPtr<FJsonObject>& RowObject, int32 RowIndex)
{
	SkillTableCheckNarrowedField(RowObject, TEXT("id"), TEXT("id"), RowIndex, 2147483647.0, TEXT("uint32"));
	SkillTableCheckNarrowedField(RowObject, TEXT("require_target"), TEXT("require_target"), RowIndex, 2147483647.0, TEXT("uint32"));
	SkillTableCheckNarrowedField(RowObject, TEXT("target_status"), TEXT("target_status"), RowIndex, 2147483647.0, TEXT("uint32"));
	SkillTableCheckNarrowedField(RowObject, TEXT("immediate"), TEXT("immediate"), RowIndex, 2147483647.0, TEXT("uint32"));
	SkillTableCheckNarrowedField(RowObject, TEXT("channel_think"), TEXT("channel_think"), RowIndex, 2147483647.0, TEXT("uint32"));
	SkillTableCheckNarrowedField(RowObject, TEXT("channel_finish"), TEXT("channel_finish"), RowIndex, 2147483647.0, TEXT("uint32"));
	SkillTableCheckNarrowedField(RowObject, TEXT("think_interval"), TEXT("think_interval"), RowIndex, 2147483647.0, TEXT("uint32"));
	SkillTableCheckNarrowedField(RowObject, TEXT("channel_time"), TEXT("channel_time"), RowIndex, 2147483647.0, TEXT("uint32"));
	SkillTableCheckNarrowedField(RowObject, TEXT("self_status"), TEXT("self_status"), RowIndex, 2147483647.0, TEXT("uint32"));
	SkillTableCheckNarrowedField(RowObject, TEXT("required_status"), TEXT("required_status"), RowIndex, 2147483647.0, TEXT("uint32"));
	SkillTableCheckNarrowedField(RowObject, TEXT("cooldown_id"), TEXT("cooldown_id"), RowIndex, 2147483647.0, TEXT("uint32"));
	SkillTableCheckNarrowedField(RowObject, TEXT("skill_type"), TEXT("skill_type[]"), RowIndex, 2147483647.0, TEXT("uint32"));
	SkillTableCheckNarrowedField(RowObject, TEXT("targeting_mode"), TEXT("targeting_mode[]"), RowIndex, 2147483647.0, TEXT("uint32"));
	SkillTableCheckNarrowedField(RowObject, TEXT("effect"), TEXT("effect[]"), RowIndex, 2147483647.0, TEXT("uint32"));
	// 子消息 required_item[]:每个元素是一个对象,逐个元素查它的收窄列。
	if (const TSharedPtr<FJsonValue>* RequiredItemField = RowObject->Values.Find(TEXT("required_item")))
	{
		if (RequiredItemField->IsValid() && (*RequiredItemField)->Type == EJson::Array)
		{
			for (const TSharedPtr<FJsonValue>& Element : (*RequiredItemField)->AsArray())
			{
				if (!Element.IsValid() || Element->Type != EJson::Object)
				{
					continue;
				}
				const TSharedPtr<FJsonObject>& ElementObject = Element->AsObject();
				SkillTableCheckNarrowedField(ElementObject, TEXT("required_item_type"), TEXT("required_item[].required_item_type"), RowIndex, 2147483647.0, TEXT("uint32"));
				SkillTableCheckNarrowedField(ElementObject, TEXT("required_item_value"), TEXT("required_item[].required_item_value"), RowIndex, 9223372036854775807.0, TEXT("uint64"));
			}
		}
	}
	// 子消息 required_resource[]:每个元素是一个对象,逐个元素查它的收窄列。
	if (const TSharedPtr<FJsonValue>* RequiredResourceField = RowObject->Values.Find(TEXT("required_resource")))
	{
		if (RequiredResourceField->IsValid() && (*RequiredResourceField)->Type == EJson::Array)
		{
			for (const TSharedPtr<FJsonValue>& Element : (*RequiredResourceField)->AsArray())
			{
				if (!Element.IsValid() || Element->Type != EJson::Object)
				{
					continue;
				}
				const TSharedPtr<FJsonObject>& ElementObject = Element->AsObject();
				SkillTableCheckNarrowedField(ElementObject, TEXT("required_resource_type"), TEXT("required_resource[].required_resource_type"), RowIndex, 2147483647.0, TEXT("uint32"));
				SkillTableCheckNarrowedField(ElementObject, TEXT("required_resource_value"), TEXT("required_resource[].required_resource_value"), RowIndex, 2147483647.0, TEXT("uint32"));
			}
		}
	}
	// 子消息 cost_resource[]:每个元素是一个对象,逐个元素查它的收窄列。
	if (const TSharedPtr<FJsonValue>* CostResourceField = RowObject->Values.Find(TEXT("cost_resource")))
	{
		if (CostResourceField->IsValid() && (*CostResourceField)->Type == EJson::Array)
		{
			for (const TSharedPtr<FJsonValue>& Element : (*CostResourceField)->AsArray())
			{
				if (!Element.IsValid() || Element->Type != EJson::Object)
				{
					continue;
				}
				const TSharedPtr<FJsonObject>& ElementObject = Element->AsObject();
				SkillTableCheckNarrowedField(ElementObject, TEXT("cost_resource_id"), TEXT("cost_resource[].cost_resource_id"), RowIndex, 2147483647.0, TEXT("uint32"));
				SkillTableCheckNarrowedField(ElementObject, TEXT("cost_resource_cost"), TEXT("cost_resource[].cost_resource_cost"), RowIndex, 2147483647.0, TEXT("uint32"));
			}
		}
	}
}

bool USkillTable::LoadFromJson(const FString& JsonText, FString& OutError)
{
	TSharedPtr<FJsonObject> Root;
	const TSharedRef<TJsonReader<TCHAR>> Reader = TJsonReaderFactory<TCHAR>::Create(JsonText);
	if (!FJsonSerializer::Deserialize(Reader, Root) || !Root.IsValid())
	{
		OutError = FString::Printf(TEXT("[Skill] %s 不是合法 JSON: %s"), TEXT("skill.json"), *Reader->GetErrorMessage());
		return false;
	}

	// 顶层形状是 {"data": [ {...}, ... ]}(见 core/generators/json_gen.py)。
	const TSharedPtr<FJsonValue>* DataField = Root->Values.Find(TEXT("data"));
	if (DataField == nullptr || !DataField->IsValid() || (*DataField)->Type != EJson::Array)
	{
		OutError = TEXT("[Skill] skill.json 顶层缺少数组字段 \"data\"");
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
			OutError = FString::Printf(TEXT("[Skill] data[%d] 不是对象"), RowIndex);
			return false;
		}
		const TSharedPtr<FJsonObject>& RowObject = Element->AsObject();
		SkillTableAuditJsonKeys(RowObject, CheckedKeys);
		// 值域检查只打日志、不中断装载:一个越界的格子不该让整张表停在旧一代,
		// 但它必须在日志里有名有姓。
		SkillTableCheckNarrowedRow(RowObject, RowIndex);

		// 必须 value-init:导表器的 JSON **按行省略空单元格**,而 FJsonObjectConverter
		// 对缺席的键是静默跳过、不写。写成 `Row;` 那些列就是未初始化内存 ——
		// 不是 0,不报错,shipping 里一样。
		FCfgSkillRow Row{};
		// CheckFlags/SkipFlags 都传 0:全部 UPROPERTY 都参与匹配。
		// 匹配靠「属性名 == JSON 键」(TMap<FString> 查找,忽略大小写),所以
		// FCfgSkillRow 的字段名是 snake_case —— 见 CfgSkillRow.h 的文件头。
		if (!FJsonObjectConverter::JsonObjectToUStruct<FCfgSkillRow>(RowObject.ToSharedRef(), &Row, 0, 0))
		{
			OutError = FString::Printf(TEXT("[Skill] data[%d] 转 FCfgSkillRow 失败"), RowIndex);
			return false;
		}
		NewSnapshot->Rows.Add(MoveTemp(Row));
	}

	BuildIndices(NewSnapshot.Get());

	// 原子换代:旧快照在最后一个 PinSnapshot() 持有者放手时析构。
	Snapshot = NewSnapshot;
	UE_LOG(LogConfigTable, Log, TEXT("[Skill] 装载 %d 行"), NewSnapshot->Rows.Num());
	return true;
}

bool USkillTable::LoadFromDir(const FString& ConfigDir, FString& OutError)
{
	const FString Path = FPaths::Combine(ConfigDir, FileName());
	FString JsonText;
	if (!FFileHelper::LoadFileToString(JsonText, *Path))
	{
		OutError = FString::Printf(TEXT("[Skill] 读不到配置文件: %s"), *Path);
		return false;
	}
	return LoadFromJson(JsonText, OutError);
}

void USkillTable::BuildIndices(FSnapshot& Snap)
{
	const int32 RowCount = Snap.Rows.Num();
	Snap.IdIndex.Reserve(RowCount);
	for (int32 RowIndex = 0; RowIndex < RowCount; ++RowIndex)
	{
		const FCfgSkillRow& Row = Snap.Rows[RowIndex];
		// 重复 id 会覆盖前一行。这张表的主键**不是** (cfg_multi),重复即配置错误。
		if (int32* Existing = Snap.IdIndex.Find(Row.id))
		{
			UE_LOG(LogConfigTable, Error,
				TEXT("[Skill] id=%s 重复(行 %d 与行 %d);这张表没有标 (cfg_multi),后一行会覆盖前一行"),
				*LexToString(Row.id), *Existing, RowIndex);
		}
		Snap.IdIndex.Add(Row.id, RowIndex);
		for (const int32& Element : Row.skill_type)
		{
			// 同一行同一个值只塞一次。定长数组补位、重复填同一个 id 在配置表里很常见,
			// 不去重的话 GetRowsBySkillType() 会把这一行返回 N 遍、
			// CountBySkillTypeIndex() 会数成 N 而不是 1。
			// 行是按 RowIndex 递增处理的,所以本行的下标只可能在桶尾。
			TArray<int32>& Bucket = Snap.SkillTypeValueIndex.FindOrAdd(Element);
			if (Bucket.Num() == 0 || Bucket.Last() != RowIndex)
			{
				Bucket.Add(RowIndex);
			}
		}
		for (const int32& Element : Row.targeting_mode)
		{
			// 同一行同一个值只塞一次。定长数组补位、重复填同一个 id 在配置表里很常见,
			// 不去重的话 GetRowsByTargetingMode() 会把这一行返回 N 遍、
			// CountByTargetingModeIndex() 会数成 N 而不是 1。
			// 行是按 RowIndex 递增处理的,所以本行的下标只可能在桶尾。
			TArray<int32>& Bucket = Snap.TargetingModeValueIndex.FindOrAdd(Element);
			if (Bucket.Num() == 0 || Bucket.Last() != RowIndex)
			{
				Bucket.Add(RowIndex);
			}
		}
		for (const int32& Element : Row.effect)
		{
			// 同一行同一个值只塞一次。定长数组补位、重复填同一个 id 在配置表里很常见,
			// 不去重的话 GetRowsByEffect() 会把这一行返回 N 遍、
			// CountByEffectIndex() 会数成 N 而不是 1。
			// 行是按 RowIndex 递增处理的,所以本行的下标只可能在桶尾。
			TArray<int32>& Bucket = Snap.EffectValueIndex.FindOrAdd(Element);
			if (Bucket.Num() == 0 || Bucket.Last() != RowIndex)
			{
				Bucket.Add(RowIndex);
			}
		}
	}
}

const FCfgSkillRow* USkillTable::FindById(int32 Id) const
{
	const FCfgSkillRow* Row = FindByIdSilent(Id);
	if (Row == nullptr)
	{
		UE_LOG(LogConfigTable, Warning, TEXT("[Skill] 查不到 id=%s"), *LexToString(Id));
	}
	return Row;
}

const FCfgSkillRow* USkillTable::FindByIdSilent(int32 Id) const
{
	const int32* RowIndex = Snapshot->IdIndex.Find(Id);
	return RowIndex != nullptr ? &Snapshot->Rows[*RowIndex] : nullptr;
}

bool USkillTable::K2_FindById(int32 Id, FCfgSkillRow& OutRow) const
{
	if (const FCfgSkillRow* Row = FindByIdSilent(Id))
	{
		OutRow = *Row;
		return true;
	}
	OutRow = FCfgSkillRow();
	return false;
}

TArray<const FCfgSkillRow*> USkillTable::FindByIds(const TArray<int32>& Ids) const
{
	TArray<const FCfgSkillRow*> Result;
	Result.Reserve(Ids.Num());
	for (const int32 Id : Ids)
	{
		if (const FCfgSkillRow* Row = FindByIdSilent(Id))
		{
			Result.Add(Row);
		}
	}
	return Result;
}

const FCfgSkillRow* USkillTable::RandOne() const
{
	// 返回的指针属于当前快照,只在下一次 Load() 之前有效 —— 调用方只存 id。
	if (Snapshot->Rows.Num() == 0)
	{
		return nullptr;
	}
	return &Snapshot->Rows[FMath::RandRange(0, Snapshot->Rows.Num() - 1)];
}

TArray<const FCfgSkillRow*> USkillTable::GetRowsBySkillType(int32 Value) const
{
	TArray<const FCfgSkillRow*> Result;
	if (const TArray<int32>* Indices = Snapshot->SkillTypeValueIndex.Find(Value))
	{
		Result.Reserve(Indices->Num());
		for (const int32 RowIndex : *Indices)
		{
			Result.Add(&Snapshot->Rows[RowIndex]);
		}
	}
	return Result;
}

int32 USkillTable::CountBySkillTypeIndex(int32 Value) const
{
	// 桶里同一行只有一个下标(见 BuildIndices),所以这是**行数**。
	const TArray<int32>* Indices = Snapshot->SkillTypeValueIndex.Find(Value);
	return Indices != nullptr ? Indices->Num() : 0;
}

TArray<const FCfgSkillRow*> USkillTable::GetRowsByTargetingMode(int32 Value) const
{
	TArray<const FCfgSkillRow*> Result;
	if (const TArray<int32>* Indices = Snapshot->TargetingModeValueIndex.Find(Value))
	{
		Result.Reserve(Indices->Num());
		for (const int32 RowIndex : *Indices)
		{
			Result.Add(&Snapshot->Rows[RowIndex]);
		}
	}
	return Result;
}

int32 USkillTable::CountByTargetingModeIndex(int32 Value) const
{
	// 桶里同一行只有一个下标(见 BuildIndices),所以这是**行数**。
	const TArray<int32>* Indices = Snapshot->TargetingModeValueIndex.Find(Value);
	return Indices != nullptr ? Indices->Num() : 0;
}

TArray<const FCfgSkillRow*> USkillTable::GetRowsByEffect(int32 Value) const
{
	TArray<const FCfgSkillRow*> Result;
	if (const TArray<int32>* Indices = Snapshot->EffectValueIndex.Find(Value))
	{
		Result.Reserve(Indices->Num());
		for (const int32 RowIndex : *Indices)
		{
			Result.Add(&Snapshot->Rows[RowIndex]);
		}
	}
	return Result;
}

int32 USkillTable::CountByEffectIndex(int32 Value) const
{
	// 桶里同一行只有一个下标(见 BuildIndices),所以这是**行数**。
	const TArray<int32>* Indices = Snapshot->EffectValueIndex.Find(Value);
	return Indices != nullptr ? Indices->Num() : 0;
}

TArray<const FCfgSkillRow*> USkillTable::Where(TFunctionRef<bool(const FCfgSkillRow&)> Pred) const
{
	TArray<const FCfgSkillRow*> Result;
	for (const FCfgSkillRow& Row : Snapshot->Rows)
	{
		if (Pred(Row))
		{
			Result.Add(&Row);
		}
	}
	return Result;
}

const FCfgSkillRow* USkillTable::First(TFunctionRef<bool(const FCfgSkillRow&)> Pred) const
{
	for (const FCfgSkillRow& Row : Snapshot->Rows)
	{
		if (Pred(Row))
		{
			return &Row;
		}
	}
	return nullptr;
}
