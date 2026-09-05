// ---------------------------------------------------------------------------
//  由 Data Table Exporter 从 Dungeon.xlsx 生成。**不要手改**。
//  重新生成:tools/data_table_exporter/run.py
// ---------------------------------------------------------------------------
//
//  模块依赖:本头要 Core + CoreUObject;配套的 DungeonTable.cpp 还要 Json +
//  JsonUtilities(FJsonObjectConverter)与 Engine。可直接抄的那一行在
//  ConfigSubsystem.h 顶部的「Build.cs」一节。
// ---------------------------------------------------------------------------
#pragma once

#include "CoreMinimal.h"
#include "UObject/Object.h"

#include "CfgDungeonRow.h"

#include "DungeonTable.generated.h"

/// Dungeon 的一代数据 + 全部索引。
///
/// 索引里存的是**行下标**而不是行指针:建索引时 Rows 还在增长,存指针会在
/// TArray 扩容那一刻全部失效。行下标在快照内永远有效。
struct FCfgDungeonSnapshot
{
	/// 全部行,按 dungeon.json 里的出现顺序。
	TArray<FCfgDungeonRow> Rows;
	/// id -> 行下标。
	TMap<int32, int32> IdIndex;
	/// monster[] 的元素值 -> 含有该值的全部行下标。
	/// 每行在同一个值下**最多出现一次**:数组里重复填同一个值(配置表里很常见,
	/// 定长数组补位就会)不该让这一行在结果里重复 N 遍。
	TMap<int32, TArray<int32>> MonsterValueIndex;
	/// scene_id -> 全部命中行的下标(二级索引)。
	TMap<int32, TArray<int32>> SceneIdIndex;
};

/**
 * Dungeon 配置表管理器。
 *
 * ---- 生命周期契约(热更)----
 *
 * Load*() 会**整批**建好一个新的 FCfgDungeonSnapshot 再原子替换成员指针;旧快照
 * 在最后一个持有者放手时析构。所以下面所有返回 `const FCfgDungeonRow*` /
 * `const TArray<...>&` / `const FCfgDungeonSnapshot&` 的接口,
 * **返回值只在下一次 Load 之前有效**:
 *
 *     ✅ 存 id,用的时候现查
 *     ❌ 把 const FCfgDungeonRow* 存进成员、容器、Lambda 捕获、Latent 节点
 *     ❌ 把**行下标**(int32)存起来 —— 下标只在取它的那一代快照里有意义。
 *        它可平凡拷贝,不会像野指针那样崩;换代之后是**静默读到另一行**,更难查。
 *
 * UE 的 GC **救不了你**:FCfgDungeonRow 是 USTRUCT 不是 UObject,它躺在一个
 * 普通 TArray 里,没有任何 GC 可达性可言。管理器自己(UObject)被 GC 保住,
 * 不代表它上一代快照里的那块行内存还活着 —— 那是 TArray 的堆内存,换代即释放,
 * 悬垂之后不会有任何报错。
 *
 * 真要跨热更持有,用 PinSnapshot() 把那一代整体钉住:
 *
 *     TSharedPtr<const FCfgDungeonSnapshot> Pinned = Table->PinSnapshot();
 *     const FCfgDungeonRow* Row = Pinned->Rows.IsValidIndex(I) ? &Pinned->Rows[I] : nullptr;
 *
 * ---- 线程契约 ----
 *
 * Load*() 与全部查询都在**游戏线程**。快照指针的换代不是原子操作,别在别的线程
 * 一边读一边 Load。工作线程要读,先在游戏线程 PinSnapshot() 再把 TSharedPtr 带过去。
 */
UCLASS(BlueprintType)
class MMORPGCONFIG_API UDungeonTable : public UObject
{
	GENERATED_BODY()

public:
	using FSnapshot = FCfgDungeonSnapshot;

	/// 这张表在 generated/tables 下的文件名(全小写,与 .pb 同口径)。
	static const TCHAR* FileName() { return TEXT("dungeon.json"); }

	// ---- 装载 ----

	/// 从 JSON 文本装载。失败时**不动**现有快照,错误写进 OutError。
	bool LoadFromJson(const FString& JsonText, FString& OutError);

	/// 从目录装载 <ConfigDir>/dungeon.json。
	bool LoadFromDir(const FString& ConfigDir, FString& OutError);

	/// 当前快照(永不为空)。
	/// **引用只在下一次 Load 之前有效** —— 这是整代数据的裸引用,换代即析构,
	/// 比单行指针更容易被顺手存成员。要跨 Load 持有就用 PinSnapshot()。
	const FSnapshot& GetSnapshot() const { return *Snapshot; }

	/// 把当前这一代钉住,让它活过后续的 Load。见上面的生命周期契约。
	TSharedPtr<const FSnapshot> PinSnapshot() const { return Snapshot; }

	/// 全部行。引用只在下一次 Load 之前有效。
	const TArray<FCfgDungeonRow>& GetRows() const { return Snapshot->Rows; }

	/// 按行下标取行;越界返回 nullptr。
	const FCfgDungeonRow* RowAt(int32 RowIndex) const
	{
		return Snapshot->Rows.IsValidIndex(RowIndex) ? &Snapshot->Rows[RowIndex] : nullptr;
	}

	// ---- 主键 ----

	/// 命中不到返回 nullptr **并打一条 Warning**。
	const FCfgDungeonRow* FindById(int32 Id) const;

	/// 命中不到返回 nullptr,不打日志(「查不到是正常分支」的场景用这个)。
	const FCfgDungeonRow* FindByIdSilent(int32 Id) const;

	UFUNCTION(BlueprintCallable, Category = "Config|Dungeon", DisplayName = "Find By Id")
	bool K2_FindById(int32 Id, FCfgDungeonRow& OutRow) const;

	UFUNCTION(BlueprintPure, Category = "Config|Dungeon")
	bool Exists(int32 Id) const { return Snapshot->IdIndex.Contains(Id); }

	/// 行数(主键可重复时 = 行数,不是不同 id 的个数)。
	UFUNCTION(BlueprintPure, Category = "Config|Dungeon")
	int32 Count() const { return Snapshot->Rows.Num(); }

	/// IN 查询。主键可重复时一个 id 会展开成多行。
	TArray<const FCfgDungeonRow*> FindByIds(const TArray<int32>& Ids) const;

	/** 随机取一行。表为空返回 nullptr。用 FMath::RandRange,不引 <random>。 */
	const FCfgDungeonRow* RandOne() const;

	// ---- 命名键(key)----
	//
	// 命名口径与 C++/Go/Java/C#/Python 五门对齐:**唯一键和多值键都叫 FindBy<Key>**,
	// 差别在返回值(单行 vs 集合)而不在函数名。
	// (主键那一层是另一回事:可重复主键在五门里都叫 FindAllById,那个名字是对的。)

	// ---- 二级索引(idx / 标量外键自动索引)----

	/// scene_id == Key 的全部行。
	TArray<const FCfgDungeonRow*> GetBySceneId(int32 Key) const;
	/// 零分配版:下标数组,配 RowAt() 用。
	const TArray<int32>& GetSceneIdIndices(int32 Key) const;
	int32 CountBySceneIdIndex(int32 Key) const;
	const TMap<int32, TArray<int32>>& GetSceneIdIndex() const
	{
		return Snapshot->SceneIdIndex;
	}

	// ---- repeated 标量的值索引 ----

	/// monster[] 里含有 Value 的全部行。同一行只出现一次,
	/// 哪怕它的 monster[] 里填了好几个同样的值。
	TArray<const FCfgDungeonRow*> GetRowsByMonster(int32 Value) const;
	/// 含有 Value 的**行数**(不是值出现的次数)。
	int32 CountByMonsterIndex(int32 Value) const;
	const TMap<int32, TArray<int32>>& GetMonsterIndex() const
	{
		return Snapshot->MonsterValueIndex;
	}

	// ---- 复合键 ----

	// ---- 过滤 ----

	TArray<const FCfgDungeonRow*> Where(TFunctionRef<bool(const FCfgDungeonRow&)> Pred) const;
	const FCfgDungeonRow* First(TFunctionRef<bool(const FCfgDungeonRow&)> Pred) const;

private:
	/// 把已经填好 Rows 的快照建成索引。
	static void BuildIndices(FSnapshot& Snap);

	/// 永不为空:构造时就是一个空快照,之后只会被整代替换。
	TSharedPtr<const FSnapshot> Snapshot = MakeShared<FSnapshot>();
};
