// ---------------------------------------------------------------------------
//  由 Data Table Exporter 从 Condition.xlsx 生成。**不要手改**。
//  重新生成:tools/data_table_exporter/run.py
//
//  模块依赖:Core、CoreUObject、Engine、Json、JsonUtilities。
//  可直接抄的一行见 ConfigSubsystem.h 顶部的「Build.cs」一节。
// ---------------------------------------------------------------------------
#include "ConditionTable.h"

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
static const TArray<int32> GConditionEmptyIndices;

/// 把 JSON 里出现过、但 FCfgConditionRow 里没有对应 UPROPERTY 的键报出来。
///
/// 这是本门语言唯一的**静默失效**面:FJsonObjectConverter 找不到同名属性时不报错、
/// 不返回 false,只是跳过 —— 表面上装载成功,那一列全是默认值。改列名 / 改大小写风格
/// 都会踩到。这个审计让它在日志里现形,而不是在玩法里现形。
///
/// 它只查「键有没有对应 UPROPERTY」,**不查值域** —— 值域是下面
/// ConditionTableCheckNarrowedRow 的事。
static void ConditionTableAuditJsonKeys(const TSharedPtr<FJsonObject>& RowObject, TSet<FString>& CheckedKeys)
{
	const UScriptStruct* RowStruct = FCfgConditionRow::StaticStruct();
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
				TEXT("[Condition] condition.json 里的键 '%s' 在 FCfgConditionRow 中没有对应 UPROPERTY,这一列被静默丢弃。导表器与 UE 产物不同代?"),
				*Pair.Key);
		}
	}
}

/// 一个「由无符号收窄而来」的值的值域检查。
///
/// proto 的 uint32/uint64 在 UE 这门是 int32/int64(UHT 不让无符号整数带 Blueprint
/// 说明符,见 CfgConditionRow.h)。收窄本身是刻意的,**但从前是完全静默的**:
/// JSON 里一个 3000000000 会变成 -1294967296,FJsonObjectConverter 一声不吭。
/// 这里在装载路径上把越界值点名报出来。
///
/// 只对**由无符号收窄而来**的列调用 —— 本来就是 int32/int64 的列不查,
/// 负数对它们是合法值,查了就是误报。
///
/// 精度说明:JSON 数字在 FJsonValue 里是 double,超过 2^53 的整数在**解析那一步**
/// 就已经不精确了。所以 uint64 列的边界判定是近似的:它抓得住量级错误(> 2^63),
/// 抓不住 2^53 附近的最后几位 —— 那是 JSON 数字类型本身的下限,不是这个函数的。
static void ConditionTableCheckNarrowedValue(const TSharedPtr<FJsonValue>& Value, const TCHAR* ColumnName,
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
			TEXT("[Condition] condition.json data[%d] 的列 '%s'(proto %s)值 %.0f 超出 UE 侧承接类型的范围 [0, %.0f];")
			TEXT("UE 这门把无符号整数收窄成有符号(见 CfgConditionRow.h),这个值会读成负数或被截断。改用 int64 列或把值调小。"),
			RowIndex, ColumnName, SourceType, Number, MaxValue);
	}
}

/// 取一个键的值做检查;键是数组时逐个元素查。
static void ConditionTableCheckNarrowedField(const TSharedPtr<FJsonObject>& Object, const TCHAR* Key,
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
			ConditionTableCheckNarrowedValue(Element, ColumnName, RowIndex, MaxValue, SourceType);
		}
		return;
	}
	ConditionTableCheckNarrowedValue(*Field, ColumnName, RowIndex, MaxValue, SourceType);
}

/// 一行里全部「由无符号收窄而来」的列的值域检查。
static void ConditionTableCheckNarrowedRow(const TSharedPtr<FJsonObject>& RowObject, int32 RowIndex)
{
	ConditionTableCheckNarrowedField(RowObject, TEXT("id"), TEXT("id"), RowIndex, 2147483647.0, TEXT("uint32"));
	ConditionTableCheckNarrowedField(RowObject, TEXT("condition_category"), TEXT("condition_category"), RowIndex, 2147483647.0, TEXT("uint32"));
	ConditionTableCheckNarrowedField(RowObject, TEXT("valid_duration"), TEXT("valid_duration"), RowIndex, 9223372036854775807.0, TEXT("uint64"));
	ConditionTableCheckNarrowedField(RowObject, TEXT("quantity_type"), TEXT("quantity_type"), RowIndex, 2147483647.0, TEXT("uint32"));
	ConditionTableCheckNarrowedField(RowObject, TEXT("target_count"), TEXT("target_count"), RowIndex, 2147483647.0, TEXT("uint32"));
	ConditionTableCheckNarrowedField(RowObject, TEXT("comparison_op"), TEXT("comparison_op"), RowIndex, 2147483647.0, TEXT("uint32"));
	ConditionTableCheckNarrowedField(RowObject, TEXT("condition1"), TEXT("condition1[]"), RowIndex, 2147483647.0, TEXT("uint32"));
	ConditionTableCheckNarrowedField(RowObject, TEXT("condition2"), TEXT("condition2[]"), RowIndex, 2147483647.0, TEXT("uint32"));
	ConditionTableCheckNarrowedField(RowObject, TEXT("condition3"), TEXT("condition3[]"), RowIndex, 2147483647.0, TEXT("uint32"));
	ConditionTableCheckNarrowedField(RowObject, TEXT("condition4"), TEXT("condition4[]"), RowIndex, 2147483647.0, TEXT("uint32"));
}

bool UConditionTable::LoadFromJson(const FString& JsonText, FString& OutError)
{
	TSharedPtr<FJsonObject> Root;
	const TSharedRef<TJsonReader<TCHAR>> Reader = TJsonReaderFactory<TCHAR>::Create(JsonText);
	if (!FJsonSerializer::Deserialize(Reader, Root) || !Root.IsValid())
	{
		OutError = FString::Printf(TEXT("[Condition] %s 不是合法 JSON: %s"), TEXT("condition.json"), *Reader->GetErrorMessage());
		return false;
	}

	// 顶层形状是 {"data": [ {...}, ... ]}(见 core/generators/json_gen.py)。
	const TSharedPtr<FJsonValue>* DataField = Root->Values.Find(TEXT("data"));
	if (DataField == nullptr || !DataField->IsValid() || (*DataField)->Type != EJson::Array)
	{
		OutError = TEXT("[Condition] condition.json 顶层缺少数组字段 \"data\"");
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
			OutError = FString::Printf(TEXT("[Condition] data[%d] 不是对象"), RowIndex);
			return false;
		}
		const TSharedPtr<FJsonObject>& RowObject = Element->AsObject();
		ConditionTableAuditJsonKeys(RowObject, CheckedKeys);
		// 值域检查只打日志、不中断装载:一个越界的格子不该让整张表停在旧一代,
		// 但它必须在日志里有名有姓。
		ConditionTableCheckNarrowedRow(RowObject, RowIndex);

		// 必须 value-init:导表器的 JSON **按行省略空单元格**,而 FJsonObjectConverter
		// 对缺席的键是静默跳过、不写。写成 `Row;` 那些列就是未初始化内存 ——
		// 不是 0,不报错,shipping 里一样。
		FCfgConditionRow Row{};
		// CheckFlags/SkipFlags 都传 0:全部 UPROPERTY 都参与匹配。
		// 匹配靠「属性名 == JSON 键」(TMap<FString> 查找,忽略大小写),所以
		// FCfgConditionRow 的字段名是 snake_case —— 见 CfgConditionRow.h 的文件头。
		if (!FJsonObjectConverter::JsonObjectToUStruct<FCfgConditionRow>(RowObject.ToSharedRef(), &Row, 0, 0))
		{
			OutError = FString::Printf(TEXT("[Condition] data[%d] 转 FCfgConditionRow 失败"), RowIndex);
			return false;
		}
		NewSnapshot->Rows.Add(MoveTemp(Row));
	}

	BuildIndices(NewSnapshot.Get());

	// 原子换代:旧快照在最后一个 PinSnapshot() 持有者放手时析构。
	Snapshot = NewSnapshot;
	UE_LOG(LogConfigTable, Log, TEXT("[Condition] 装载 %d 行"), NewSnapshot->Rows.Num());
	return true;
}

bool UConditionTable::LoadFromDir(const FString& ConfigDir, FString& OutError)
{
	const FString Path = FPaths::Combine(ConfigDir, FileName());
	FString JsonText;
	if (!FFileHelper::LoadFileToString(JsonText, *Path))
	{
		OutError = FString::Printf(TEXT("[Condition] 读不到配置文件: %s"), *Path);
		return false;
	}
	return LoadFromJson(JsonText, OutError);
}

void UConditionTable::BuildIndices(FSnapshot& Snap)
{
	const int32 RowCount = Snap.Rows.Num();
	Snap.IdIndex.Reserve(RowCount);
	for (int32 RowIndex = 0; RowIndex < RowCount; ++RowIndex)
	{
		const FCfgConditionRow& Row = Snap.Rows[RowIndex];
		// 重复 id 会覆盖前一行。这张表的主键**不是** (cfg_multi),重复即配置错误。
		if (int32* Existing = Snap.IdIndex.Find(Row.id))
		{
			UE_LOG(LogConfigTable, Error,
				TEXT("[Condition] id=%s 重复(行 %d 与行 %d);这张表没有标 (cfg_multi),后一行会覆盖前一行"),
				*LexToString(Row.id), *Existing, RowIndex);
		}
		Snap.IdIndex.Add(Row.id, RowIndex);
		for (const int32& Element : Row.condition1)
		{
			// 同一行同一个值只塞一次。定长数组补位、重复填同一个 id 在配置表里很常见,
			// 不去重的话 GetRowsByCondition1() 会把这一行返回 N 遍、
			// CountByCondition1Index() 会数成 N 而不是 1。
			// 行是按 RowIndex 递增处理的,所以本行的下标只可能在桶尾。
			TArray<int32>& Bucket = Snap.Condition1ValueIndex.FindOrAdd(Element);
			if (Bucket.Num() == 0 || Bucket.Last() != RowIndex)
			{
				Bucket.Add(RowIndex);
			}
		}
		for (const int32& Element : Row.condition2)
		{
			// 同一行同一个值只塞一次。定长数组补位、重复填同一个 id 在配置表里很常见,
			// 不去重的话 GetRowsByCondition2() 会把这一行返回 N 遍、
			// CountByCondition2Index() 会数成 N 而不是 1。
			// 行是按 RowIndex 递增处理的,所以本行的下标只可能在桶尾。
			TArray<int32>& Bucket = Snap.Condition2ValueIndex.FindOrAdd(Element);
			if (Bucket.Num() == 0 || Bucket.Last() != RowIndex)
			{
				Bucket.Add(RowIndex);
			}
		}
		for (const int32& Element : Row.condition3)
		{
			// 同一行同一个值只塞一次。定长数组补位、重复填同一个 id 在配置表里很常见,
			// 不去重的话 GetRowsByCondition3() 会把这一行返回 N 遍、
			// CountByCondition3Index() 会数成 N 而不是 1。
			// 行是按 RowIndex 递增处理的,所以本行的下标只可能在桶尾。
			TArray<int32>& Bucket = Snap.Condition3ValueIndex.FindOrAdd(Element);
			if (Bucket.Num() == 0 || Bucket.Last() != RowIndex)
			{
				Bucket.Add(RowIndex);
			}
		}
		for (const int32& Element : Row.condition4)
		{
			// 同一行同一个值只塞一次。定长数组补位、重复填同一个 id 在配置表里很常见,
			// 不去重的话 GetRowsByCondition4() 会把这一行返回 N 遍、
			// CountByCondition4Index() 会数成 N 而不是 1。
			// 行是按 RowIndex 递增处理的,所以本行的下标只可能在桶尾。
			TArray<int32>& Bucket = Snap.Condition4ValueIndex.FindOrAdd(Element);
			if (Bucket.Num() == 0 || Bucket.Last() != RowIndex)
			{
				Bucket.Add(RowIndex);
			}
		}
	}
}

const FCfgConditionRow* UConditionTable::FindById(int32 Id) const
{
	const FCfgConditionRow* Row = FindByIdSilent(Id);
	if (Row == nullptr)
	{
		UE_LOG(LogConfigTable, Warning, TEXT("[Condition] 查不到 id=%s"), *LexToString(Id));
	}
	return Row;
}

const FCfgConditionRow* UConditionTable::FindByIdSilent(int32 Id) const
{
	const int32* RowIndex = Snapshot->IdIndex.Find(Id);
	return RowIndex != nullptr ? &Snapshot->Rows[*RowIndex] : nullptr;
}

bool UConditionTable::K2_FindById(int32 Id, FCfgConditionRow& OutRow) const
{
	if (const FCfgConditionRow* Row = FindByIdSilent(Id))
	{
		OutRow = *Row;
		return true;
	}
	OutRow = FCfgConditionRow();
	return false;
}

TArray<const FCfgConditionRow*> UConditionTable::FindByIds(const TArray<int32>& Ids) const
{
	TArray<const FCfgConditionRow*> Result;
	Result.Reserve(Ids.Num());
	for (const int32 Id : Ids)
	{
		if (const FCfgConditionRow* Row = FindByIdSilent(Id))
		{
			Result.Add(Row);
		}
	}
	return Result;
}

const FCfgConditionRow* UConditionTable::RandOne() const
{
	// 返回的指针属于当前快照,只在下一次 Load() 之前有效 —— 调用方只存 id。
	if (Snapshot->Rows.Num() == 0)
	{
		return nullptr;
	}
	return &Snapshot->Rows[FMath::RandRange(0, Snapshot->Rows.Num() - 1)];
}

TArray<const FCfgConditionRow*> UConditionTable::GetRowsByCondition1(int32 Value) const
{
	TArray<const FCfgConditionRow*> Result;
	if (const TArray<int32>* Indices = Snapshot->Condition1ValueIndex.Find(Value))
	{
		Result.Reserve(Indices->Num());
		for (const int32 RowIndex : *Indices)
		{
			Result.Add(&Snapshot->Rows[RowIndex]);
		}
	}
	return Result;
}

int32 UConditionTable::CountByCondition1Index(int32 Value) const
{
	// 桶里同一行只有一个下标(见 BuildIndices),所以这是**行数**。
	const TArray<int32>* Indices = Snapshot->Condition1ValueIndex.Find(Value);
	return Indices != nullptr ? Indices->Num() : 0;
}

TArray<const FCfgConditionRow*> UConditionTable::GetRowsByCondition2(int32 Value) const
{
	TArray<const FCfgConditionRow*> Result;
	if (const TArray<int32>* Indices = Snapshot->Condition2ValueIndex.Find(Value))
	{
		Result.Reserve(Indices->Num());
		for (const int32 RowIndex : *Indices)
		{
			Result.Add(&Snapshot->Rows[RowIndex]);
		}
	}
	return Result;
}

int32 UConditionTable::CountByCondition2Index(int32 Value) const
{
	// 桶里同一行只有一个下标(见 BuildIndices),所以这是**行数**。
	const TArray<int32>* Indices = Snapshot->Condition2ValueIndex.Find(Value);
	return Indices != nullptr ? Indices->Num() : 0;
}

TArray<const FCfgConditionRow*> UConditionTable::GetRowsByCondition3(int32 Value) const
{
	TArray<const FCfgConditionRow*> Result;
	if (const TArray<int32>* Indices = Snapshot->Condition3ValueIndex.Find(Value))
	{
		Result.Reserve(Indices->Num());
		for (const int32 RowIndex : *Indices)
		{
			Result.Add(&Snapshot->Rows[RowIndex]);
		}
	}
	return Result;
}

int32 UConditionTable::CountByCondition3Index(int32 Value) const
{
	// 桶里同一行只有一个下标(见 BuildIndices),所以这是**行数**。
	const TArray<int32>* Indices = Snapshot->Condition3ValueIndex.Find(Value);
	return Indices != nullptr ? Indices->Num() : 0;
}

TArray<const FCfgConditionRow*> UConditionTable::GetRowsByCondition4(int32 Value) const
{
	TArray<const FCfgConditionRow*> Result;
	if (const TArray<int32>* Indices = Snapshot->Condition4ValueIndex.Find(Value))
	{
		Result.Reserve(Indices->Num());
		for (const int32 RowIndex : *Indices)
		{
			Result.Add(&Snapshot->Rows[RowIndex]);
		}
	}
	return Result;
}

int32 UConditionTable::CountByCondition4Index(int32 Value) const
{
	// 桶里同一行只有一个下标(见 BuildIndices),所以这是**行数**。
	const TArray<int32>* Indices = Snapshot->Condition4ValueIndex.Find(Value);
	return Indices != nullptr ? Indices->Num() : 0;
}

TArray<const FCfgConditionRow*> UConditionTable::Where(TFunctionRef<bool(const FCfgConditionRow&)> Pred) const
{
	TArray<const FCfgConditionRow*> Result;
	for (const FCfgConditionRow& Row : Snapshot->Rows)
	{
		if (Pred(Row))
		{
			Result.Add(&Row);
		}
	}
	return Result;
}

const FCfgConditionRow* UConditionTable::First(TFunctionRef<bool(const FCfgConditionRow&)> Pred) const
{
	for (const FCfgConditionRow& Row : Snapshot->Rows)
	{
		if (Pred(Row))
		{
			return &Row;
		}
	}
	return nullptr;
}
