// ---------------------------------------------------------------------------
//  由 Data Table Exporter 从 TestMultiKey.xlsx 生成。**不要手改**。
//  重新生成:tools/data_table_exporter/run.py
// ---------------------------------------------------------------------------
//
//  模块依赖:本头要 Core + CoreUObject;配套的 TestMultiKeyTable.cpp 还要 Json +
//  JsonUtilities(FJsonObjectConverter)与 Engine。可直接抄的那一行在
//  ConfigSubsystem.h 顶部的「Build.cs」一节。
// ---------------------------------------------------------------------------
#pragma once

#include "CoreMinimal.h"
#include "UObject/Object.h"

#include "CfgTestMultiKeyRow.h"

#include "TestMultiKeyTable.generated.h"

/// TestMultiKey 的一代数据 + 全部索引。
///
/// 索引里存的是**行下标**而不是行指针:建索引时 Rows 还在增长,存指针会在
/// TArray 扩容那一刻全部失效。行下标在快照内永远有效。
struct FCfgTestMultiKeySnapshot
{
	/// 全部行,按 testmultikey.json 里的出现顺序。
	TArray<FCfgTestMultiKeyRow> Rows;
	/// 主键可重复((cfg_multi)):id -> 全部命中行的下标。
	TMap<int32, TArray<int32>> IdIndex;
	/// string_key(唯一键) -> 行下标。
	TMap<FString, int32> StringKeyMap;
	/// uint32_key(唯一键) -> 行下标。
	TMap<int32, int32> Uint32KeyMap;
	/// int32_key(唯一键) -> 行下标。
	TMap<int32, int32> Int32KeyMap;
	/// m_string_key(多值键) -> 全部命中行的下标。
	TMap<FString, TArray<int32>> MStringKeyMap;
	/// m_uint32_key(多值键) -> 全部命中行的下标。
	TMap<int32, TArray<int32>> MUint32KeyMap;
	/// m_int32_key(多值键) -> 全部命中行的下标。
	TMap<int32, TArray<int32>> MInt32KeyMap;
	/// effect[] 的元素值 -> 含有该值的全部行下标。
	/// 每行在同一个值下**最多出现一次**:数组里重复填同一个值(配置表里很常见,
	/// 定长数组补位就会)不该让这一行在结果里重复 N 遍。
	TMap<int32, TArray<int32>> EffectValueIndex;
	/// test_refs[] 的元素值 -> 含有该值的全部行下标。
	/// 每行在同一个值下**最多出现一次**:数组里重复填同一个值(配置表里很常见,
	/// 定长数组补位就会)不该让这一行在结果里重复 N 遍。
	TMap<int32, TArray<int32>> TestRefsValueIndex;
	/// level -> 全部命中行的下标(二级索引)。
	TMap<int32, TArray<int32>> LevelIndex;
	/// test_ref -> 全部命中行的下标(二级索引)。
	TMap<int32, TArray<int32>> TestRefIndex;
};

/**
 * TestMultiKey 配置表管理器。
 *
 * ---- 生命周期契约(热更)----
 *
 * Load*() 会**整批**建好一个新的 FCfgTestMultiKeySnapshot 再原子替换成员指针;旧快照
 * 在最后一个持有者放手时析构。所以下面所有返回 `const FCfgTestMultiKeyRow*` /
 * `const TArray<...>&` / `const FCfgTestMultiKeySnapshot&` 的接口,
 * **返回值只在下一次 Load 之前有效**:
 *
 *     ✅ 存 id,用的时候现查
 *     ❌ 把 const FCfgTestMultiKeyRow* 存进成员、容器、Lambda 捕获、Latent 节点
 *     ❌ 把**行下标**(int32)存起来 —— 下标只在取它的那一代快照里有意义。
 *        它可平凡拷贝,不会像野指针那样崩;换代之后是**静默读到另一行**,更难查。
 *
 * UE 的 GC **救不了你**:FCfgTestMultiKeyRow 是 USTRUCT 不是 UObject,它躺在一个
 * 普通 TArray 里,没有任何 GC 可达性可言。管理器自己(UObject)被 GC 保住,
 * 不代表它上一代快照里的那块行内存还活着 —— 那是 TArray 的堆内存,换代即释放,
 * 悬垂之后不会有任何报错。
 *
 * 真要跨热更持有,用 PinSnapshot() 把那一代整体钉住:
 *
 *     TSharedPtr<const FCfgTestMultiKeySnapshot> Pinned = Table->PinSnapshot();
 *     const FCfgTestMultiKeyRow* Row = Pinned->Rows.IsValidIndex(I) ? &Pinned->Rows[I] : nullptr;
 *
 * ---- 线程契约 ----
 *
 * Load*() 与全部查询都在**游戏线程**。快照指针的换代不是原子操作,别在别的线程
 * 一边读一边 Load。工作线程要读,先在游戏线程 PinSnapshot() 再把 TSharedPtr 带过去。
 */
UCLASS(BlueprintType)
class MMORPGCONFIG_API UTestMultiKeyTable : public UObject
{
	GENERATED_BODY()

public:
	using FSnapshot = FCfgTestMultiKeySnapshot;

	/// 这张表在 generated/tables 下的文件名(全小写,与 .pb 同口径)。
	static const TCHAR* FileName() { return TEXT("testmultikey.json"); }

	// ---- 装载 ----

	/// 从 JSON 文本装载。失败时**不动**现有快照,错误写进 OutError。
	bool LoadFromJson(const FString& JsonText, FString& OutError);

	/// 从目录装载 <ConfigDir>/testmultikey.json。
	bool LoadFromDir(const FString& ConfigDir, FString& OutError);

	/// 当前快照(永不为空)。
	/// **引用只在下一次 Load 之前有效** —— 这是整代数据的裸引用,换代即析构,
	/// 比单行指针更容易被顺手存成员。要跨 Load 持有就用 PinSnapshot()。
	const FSnapshot& GetSnapshot() const { return *Snapshot; }

	/// 把当前这一代钉住,让它活过后续的 Load。见上面的生命周期契约。
	TSharedPtr<const FSnapshot> PinSnapshot() const { return Snapshot; }

	/// 全部行。引用只在下一次 Load 之前有效。
	const TArray<FCfgTestMultiKeyRow>& GetRows() const { return Snapshot->Rows; }

	/// 按行下标取行;越界返回 nullptr。
	const FCfgTestMultiKeyRow* RowAt(int32 RowIndex) const
	{
		return Snapshot->Rows.IsValidIndex(RowIndex) ? &Snapshot->Rows[RowIndex] : nullptr;
	}

	// ---- 主键 ----

	/// 这张表的主键可重复((cfg_multi)),**没有 FindById** —— 单值语义不成立,
	/// 提供它只会让调用方悄悄只看第一行。
	TArray<const FCfgTestMultiKeyRow*> FindAllById(int32 Id) const;

	/// 零分配版:直接拿下标数组,配 RowAt() 用。命中不到返回空数组。
	const TArray<int32>& FindAllIdIndices(int32 Id) const;

	UFUNCTION(BlueprintCallable, Category = "Config|TestMultiKey", DisplayName = "Find All By Id")
	void K2_FindAllById(int32 Id, TArray<FCfgTestMultiKeyRow>& OutRows) const;

	int32 CountById(int32 Id) const;

	UFUNCTION(BlueprintPure, Category = "Config|TestMultiKey")
	bool Exists(int32 Id) const { return Snapshot->IdIndex.Contains(Id); }

	/// 行数(主键可重复时 = 行数,不是不同 id 的个数)。
	UFUNCTION(BlueprintPure, Category = "Config|TestMultiKey")
	int32 Count() const { return Snapshot->Rows.Num(); }

	/// IN 查询。主键可重复时一个 id 会展开成多行。
	TArray<const FCfgTestMultiKeyRow*> FindByIds(const TArray<int32>& Ids) const;

	/** 随机取一行。表为空返回 nullptr。用 FMath::RandRange,不引 <random>。 */
	const FCfgTestMultiKeyRow* RandOne() const;

	// ---- 命名键(key)----
	//
	// 命名口径与 C++/Go/Java/C#/Python 五门对齐:**唯一键和多值键都叫 FindBy<Key>**,
	// 差别在返回值(单行 vs 集合)而不在函数名。
	// (主键那一层是另一回事:可重复主键在五门里都叫 FindAllById,那个名字是对的。)

	/// string_key 是唯一键(key)。重复值在装载时打 Error(见 TestMultiKeyTable.cpp 的 BuildIndices)。
	const FCfgTestMultiKeyRow* FindByStringKey(const FString& Key) const;
	bool ExistsByStringKey(const FString& Key) const
	{
		return Snapshot->StringKeyMap.Contains(Key);
	}

	/// uint32_key 是唯一键(key)。重复值在装载时打 Error(见 TestMultiKeyTable.cpp 的 BuildIndices)。
	const FCfgTestMultiKeyRow* FindByUint32Key(int32 Key) const;
	bool ExistsByUint32Key(int32 Key) const
	{
		return Snapshot->Uint32KeyMap.Contains(Key);
	}

	/// int32_key 是唯一键(key)。重复值在装载时打 Error(见 TestMultiKeyTable.cpp 的 BuildIndices)。
	const FCfgTestMultiKeyRow* FindByInt32Key(int32 Key) const;
	bool ExistsByInt32Key(int32 Key) const
	{
		return Snapshot->Int32KeyMap.Contains(Key);
	}

	/// m_string_key 是多值键(multi):同一个值可能命中多行。
	TArray<const FCfgTestMultiKeyRow*> FindByMStringKey(const FString& Key) const;
	/// 零分配版:下标数组,配 RowAt() 用。命中不到返回空数组。
	const TArray<int32>& FindByMStringKeyIndices(const FString& Key) const;
	int32 CountByMStringKey(const FString& Key) const;

	/// m_uint32_key 是多值键(multi):同一个值可能命中多行。
	TArray<const FCfgTestMultiKeyRow*> FindByMUint32Key(int32 Key) const;
	/// 零分配版:下标数组,配 RowAt() 用。命中不到返回空数组。
	const TArray<int32>& FindByMUint32KeyIndices(int32 Key) const;
	int32 CountByMUint32Key(int32 Key) const;

	/// m_int32_key 是多值键(multi):同一个值可能命中多行。
	TArray<const FCfgTestMultiKeyRow*> FindByMInt32Key(int32 Key) const;
	/// 零分配版:下标数组,配 RowAt() 用。命中不到返回空数组。
	const TArray<int32>& FindByMInt32KeyIndices(int32 Key) const;
	int32 CountByMInt32Key(int32 Key) const;

	// ---- 二级索引(idx / 标量外键自动索引)----

	/// level == Key 的全部行。
	TArray<const FCfgTestMultiKeyRow*> GetByLevel(int32 Key) const;
	/// 零分配版:下标数组,配 RowAt() 用。
	const TArray<int32>& GetLevelIndices(int32 Key) const;
	int32 CountByLevelIndex(int32 Key) const;
	const TMap<int32, TArray<int32>>& GetLevelIndex() const
	{
		return Snapshot->LevelIndex;
	}

	/// test_ref == Key 的全部行。
	TArray<const FCfgTestMultiKeyRow*> GetByTestRef(int32 Key) const;
	/// 零分配版:下标数组,配 RowAt() 用。
	const TArray<int32>& GetTestRefIndices(int32 Key) const;
	int32 CountByTestRefIndex(int32 Key) const;
	const TMap<int32, TArray<int32>>& GetTestRefIndex() const
	{
		return Snapshot->TestRefIndex;
	}

	// ---- repeated 标量的值索引 ----

	/// effect[] 里含有 Value 的全部行。同一行只出现一次,
	/// 哪怕它的 effect[] 里填了好几个同样的值。
	TArray<const FCfgTestMultiKeyRow*> GetRowsByEffect(int32 Value) const;
	/// 含有 Value 的**行数**(不是值出现的次数)。
	int32 CountByEffectIndex(int32 Value) const;
	const TMap<int32, TArray<int32>>& GetEffectIndex() const
	{
		return Snapshot->EffectValueIndex;
	}

	/// test_refs[] 里含有 Value 的全部行。同一行只出现一次,
	/// 哪怕它的 test_refs[] 里填了好几个同样的值。
	TArray<const FCfgTestMultiKeyRow*> GetRowsByTestRefs(int32 Value) const;
	/// 含有 Value 的**行数**(不是值出现的次数)。
	int32 CountByTestRefsIndex(int32 Value) const;
	const TMap<int32, TArray<int32>>& GetTestRefsIndex() const
	{
		return Snapshot->TestRefsValueIndex;
	}

	// ---- 复合键 ----

	// ---- 过滤 ----

	TArray<const FCfgTestMultiKeyRow*> Where(TFunctionRef<bool(const FCfgTestMultiKeyRow&)> Pred) const;
	const FCfgTestMultiKeyRow* First(TFunctionRef<bool(const FCfgTestMultiKeyRow&)> Pred) const;

private:
	/// 把已经填好 Rows 的快照建成索引。
	static void BuildIndices(FSnapshot& Snap);

	/// 永不为空:构造时就是一个空快照,之后只会被整代替换。
	TSharedPtr<const FSnapshot> Snapshot = MakeShared<FSnapshot>();
};
