# 背包:物品实例层 / 容器布局层拆分 (2026-08-27)

## 概述

标准做法不是"选扁平还是选格子"——那个问题本身就问错了。标准做法是把**物品实例**和**容器布局**拆成两层:实例是领域实体,布局只是"这些实例怎么摆"。扁平和格子是同一个模型的两种布局策略,不是两套背包系统。Diablo / POE / Tarkov 全是这个结构。

`cpp/libs/modules/bag` 在这次拆分之前**没有**这两层。本文档记录:拆分前它是什么样、为什么那不算两层、拆成什么、每个成员去了哪、以及哪些行为变了哪些没变。

本次拆分得出的通用结论已沉淀为强制规则 `AGENTS.md` §11.6「实例与布局分层(数据层 / 显示层)」,适用于所有"多个实例 + 有位置概念"的系统。本文档是该规则的参考实现与完整推导。

一句话总结:

| | 拆分前 | 拆分后 |
|---|---|---|
| 实例状态 | `Bag::items` + `Bag::itemRegistry` | `ItemStore`(`item_store.h`) |
| 布局状态 | `Bag::posToGuid` + `Bag::capacity` | `IContainerLayout`(`container_layout.h`) |
| 玩法规则 | 与上面两者揉在同一批函数里 | `Bag` 变成薄桥,只做需要两层才能回答的事 |
| 布局策略 | 只有一种、且没有名字 | `FlatLayout`(背包/仓库/临时格)、`FixedSlotLayout`(装备栏)、`GridLayout`(暂无生产使用者) |
| 公开 API | — | **逐字未变** |

---

## 1. 拆分前不是两层

`Bag` 一个类同时私有持有四样东西:

```cpp
class Bag {
    ItemsMap       items;         // guid -> entt::entity      ← 实例层索引
    entt::registry itemRegistry;  // entity -> ItemComp        ← 实例层存储
    PosMap         posToGuid;     // pos -> guid               ← 布局层
    std::size_t    capacity;      // 格子数                     ← 布局层
};
```

"有两个成员变量"不等于"分成了两层"。真正的判据是**它们有没有被同一批函数同时读写**。答案是有,而且是系统性的:

| 焊点 | 拆分前的写法 | 为什么它是焊点 |
|---|---|---|
| `IsFull()` | `items.size() >= capacity` | 拿**实例数量**回答**布局问题**。只有在"1 实例恒占 1 格"的前提下才成立 |
| `IsSpaceInsufficient(n)` | `Capacity() - items.size() < n` | 同上 |
| `EmptyGridCount()` | `Capacity() - items.size()` | 同上 |
| `MergeAndCompact()` | 一个函数体内既 `set_size()` 合并堆叠、又清空重排 `posToGuid` | 一个函数干两层的活 |
| `IsAlreadyMergedAndCompact()` | ①堆叠可合并性 ②槽位连续性 ③排序,三条挤在一个谓词里 | ① 是实例层判据,② 是布局层判据 |
| `DestroyItem(guid)` | 销毁实体 + 从 `items` 删 + **线扫 `posToGuid`** 找槽位 | 布局没有自己的反向索引,因为它压根不是一层东西 |
| `GetItemPosByGuid()` | 遍历 `posToGuid` 比对 value,O(n) | 同上 |
| `AllocateGridSlot()` | 扫 `0..capacity-1` 找空位 | 扁平策略被写死在这里,没有名字、无法替换 |
| `GridsNeededFor()` | `(total + maxStack - 1) / maxStack` | 名字说"要几格",算的其实是"要几个实例"。两个概念被一个名字混为一谈 |
| `InsertItemForRestore()` | 直接 `posToGuid[pos] = guid` | 绕过一切布局校验 |

结论:**扁平布局不是"一种策略",它是唯一存在、且没有名字的布局**,散在上面这十处。要加格子背包,只能再写一套。

### 1.1 这与 BagService 那次重构的关系

[bag-service-srp-refactor.md](bag-service-srp-refactor.md)(2026-03-26)把封锁检查 / 交易流水 / 异常检测从 `Bag` 挪进 `BagService`。那是一刀**纵切**——把横切关注点(能不能给 + 副作用)从容器里剥出去。

本次是一刀**横切**——把容器内部劈成实例与布局。两刀互不相干,也互不替代:

```
BagService     ← 纵切:能不能给 + 副作用(封锁 / 流水 / 异常 / 跨服冻结)
──────────────
Bag            ← 桥:需要两层才能回答的玩法规则(堆叠 + 摆放 + 整理)
  ├ ItemStore        ← 横切:实例是什么
  └ IContainerLayout ← 横切:实例摆在哪
```

### 1.2 缺这一层已经在疼

`kEquipment`(装备栏,容量 10)本质是**具名固定槽**——头 / 胸 / 武器,每个槽只接受特定类型。`proto/common/database/bag_quest_mail_data.proto` 的 `ItemEntry.pos` 注释里白纸黑字写着这一点:

> Equipment slots have well-known semantics (head/chest/...) defined by BagType + a slot sub-enum

但拆分前 `Bag` 把装备栏和人物背包一视同仁:同样的 first-fit 自由格、同样会被整理重排。真要做装备槽,只能在 `Bag` 里加 `if (bagType == kEquipment)`。

---

## 2. 目标模型

**物品实例是领域实体。** 它有自己的身份(`item_uuid`,全局唯一、跨堆叠拆合稳定,`transaction_log` 与外挂回收都依赖它)、自己的生命期(铸号 → 存在 → 销毁)、自己的状态(今天只有 `config_id` + `size`;proto 里已按已知字段号预留了强化等级 / 词条 / 镶嵌 / 绑定态 / 限时 / 可交易次数)。

**容器布局只是"这些实例怎么摆"。** 它只认识 `(Guid, Footprint)`,不认识 `config_id`、不认识堆叠数量、不查 `ItemTable`、不 include entt。

两层的唯一接头是一个 `Guid`。

### 2.1 堆叠归哪层

归实例层。理由:堆叠回答的是"**这批数量要拆成几个实例**",不是"占几格"。

拆分前 `GridsNeededFor` 这个名字把两个问题混成了一个,因为在扁平布局下它们恰好相等。格子布局一上来就不等了——3 个 1x1 的实例占 3 格,3 个 2x2 的实例占 12 格。

拆分后:

- `ItemStore::StacksNeededFor(total, maxStack)` → 要几个实例(实例层)
- `IContainerLayout::CanFit(count, footprint)` → 这些形状摆不摆得下(布局层)
- `Bag::FootprintFor(configId)` → 一个实例是什么形状(桥层,唯一入口)

### 2.2 判断这条缝有没有塌回去的唯一判据

> **布局层永远不 include `table/code/item_table.h`,永远不出现 `config_id` / `size` / `ItemComp` / `entt`。**

哪天 `container_layout.{h,cpp}` 里需要读一次 `ItemComp`,这层就漏了。这一条比任何架构图都好用。

---

## 3. 职责边界

### 3.1 实例层 `ItemStore`(`cpp/libs/modules/bag/item_store.{h,cpp}`)

拥有:`guidToEntity_`(guid → 实体)+ `registry_`(实体 → `ItemComp`)。

- 身份与生命期:`Insert` / `Erase` / `Clear` / `Find` / `Contains` / `EntityOf` / `ForEach`
- 数量:`TotalCountOf` / `HasAll` / `DrainStacks`
- 堆叠语义:`MeasureFreeRoomPerConfig` / `PlanStackIntoExistingStacks` / `ApplyStackFill` / `HasMergeablePartials` / `MergePartialStacks` / `CanStack` / `StacksNeededFor`
- guid 发号:`MintGuid` / `CanMintGuid` / `IsInvalidGuid`

不知道:槽位、容量、满没满。

`guidToEntity_` 与 `registry_` 必须始终同步。为了让"改了一边忘了另一边"在结构上不可能发生,**所有**创建走 `Insert`、**所有**销毁走 `Erase` / `Clear`,任何地方都不直接碰 `registry_.create()/destroy()`。这条纪律是从拆分前的 `InsertItemEntity` / `DestroyItem` 原样继承下来的。

### 3.2 布局层 `IContainerLayout`(`cpp/libs/modules/bag/container_layout.{h,cpp}`)

拥有:槽位 ↔ guid 的映射,以及"还摆不摆得下"的答案。

```cpp
using SlotId = uint32_t;
inline constexpr SlotId kInvalidSlot{kInvalidU32Id};

struct Footprint { uint32_t width{1}; uint32_t height{1}; uint32_t Cells() const; };
inline constexpr Footprint kSingleCell{1, 1};

class IContainerLayout {
public:
    virtual ~IContainerLayout() = default;

    virtual std::size_t Capacity() const = 0;
    virtual void        Resize(std::size_t newCapacity) = 0;
    virtual std::size_t OccupiedSlotCount() const = 0;
    virtual std::size_t FreeCells() const = 0;
    virtual bool        CanFit(std::size_t count, Footprint footprint) const = 0;

    virtual SlotId Place(Guid guid, Footprint footprint) = 0;             // 自动选位
    virtual bool   PlaceAt(Guid guid, SlotId slot, Footprint footprint) = 0; // 快照还原专用
    virtual void   Remove(Guid guid) = 0;
    virtual void   Clear() = 0;

    virtual SlotId         SlotOf(Guid guid) const = 0;
    virtual Guid           At(SlotId slot) const = 0;
    virtual const PosMap  &Slots() const = 0;

    virtual bool SupportsCompaction() const = 0;
    virtual bool IsCompact() const = 0;
    virtual void ApplyOrder(const std::vector<Guid> &order) = 0;
};
```

两个设计点值得单说:

**`CanFit` 必须是虚函数,不能让桥层自己拿 `FreeCells()` 做减法。** 扁平布局下"空格数 >= count"是对的;格子布局下碎片化会让总空格够、却一块都摆不进去。回归用例 `GridLayoutTest.FreeCellCountIsNotAnAnswerToCanFit` 钉的就是这一条。

**`ApplyOrder` 收的是一个已经排好的 guid 列表,不是一个排序规则。** "按什么排"(config 升序、size 降序)要读 `ItemComp`,是实例层数据决定的;布局层只负责按序落位。于是排序规则可以随时改,两层都不用动。

### 3.3 桥层 `Bag`(`cpp/libs/modules/bag/bag_system.{h,cpp}`)

不再持有任何一层的状态,只持有两层各一份:

```cpp
ItemStore                          store_;
std::unique_ptr<IContainerLayout>  layout_{std::make_unique<FlatLayout>(kDefaultCapacity)};
```

留在这里的,全是**同时需要两层**才成立的规则,并且一律按三段走:

```
plan    : 问实例层「这批数量要填哪几个既有堆、还要新建几个实例」   不碰布局
reserve : 问布局层「这几个新实例摆得下吗」                        不碰实例
commit  : 两层一起写;失败一定发生在 reserve 之前,绝无半写状态
```

这三段不是新发明——拆分前 `AddStackableItem` 的注释里就写着"先规划后写入",只是当时两半没有名字。现在它们各有归属,于是**顺序错了会编译不过**(plan 拿到的是 `ItemStore::StackFill`,commit 才有 `layout_->Place`)。

判断一段代码该不该待在 `Bag` 里,就看它是不是两层都碰。`RemoveItemByPos` 的守卫链是最好的例子:前两项(槽位在不在、上面是谁)问布局层,后两项(它是什么、够不够扣)问实例层。

`Bag` 的特殊成员函数全部走隐式生成:`layout_` 用类内初始化器给出默认布局,于是 `Bag` 保持"默认可构造 + 可移动 + 不可复制",与拆分前(`entt::registry` 本身就不可复制)完全一致。`PlayerBagsComp` 的 `std::array<Bag, 4>`、`dynamicBags_` 的 `unordered_map<uint64_t, Bag>`、以及 entt 组件存储都依赖这一点,**不要去手写它们**。

---

## 4. 搬迁对照表

### 搬到布局层

| 拆分前(`Bag` 私有 / 公开) | 拆分后 |
|---|---|
| `PosMap posToGuid` | 各策略私有(`FlatLayout::slotToGuid_` 等) |
| `std::size_t capacity` | `IContainerLayout::Capacity()` / `Resize()` |
| `AllocateGridSlot(guid)` | `Place(guid, footprint) -> SlotId` |
| `EmptyGridCount()` | `FreeCells()` |
| `IsFull()` / `IsSpaceInsufficient(n)` 的判定 | `FreeCells()` / `CanFit(count, footprint)` |
| `ExpandCapacity` / `SetCapacityForRestore` 的实现 | `Resize(n)` |
| `GridSlots()` / `GridSlotCount()` | `Slots()` / `OccupiedSlotCount()` |
| `GetItemPosByGuid()` 的 O(n) 线扫 | `SlotOf(guid)`,**O(1)**(新增反向索引) |
| `DestroyItem` 里释放槽位的那半 | `Remove(guid)`,**O(1)** |
| `IsAlreadyMergedAndCompact()` 的第 ② 条(槽位连续) | `IsCompact()` |
| `MergeAndCompact()` 里重排 `posToGuid` 的那一段 | `ApplyOrder(order)` |

### 搬到实例层

| 拆分前 | 拆分后 |
|---|---|
| `ItemsMap items` + `entt::registry itemRegistry` | `ItemStore::guidToEntity_` + `registry_` |
| `InsertItemEntity` / `ClearAllItems` | `Insert` / `Clear` |
| `DestroyItem` 里销毁实体的那半 | `Erase(guid)` |
| `GetItemCompByGuid` / `GetTotalItemCount` / `ForEachItem` | `Find` / `TotalCountOf` / `ForEach` |
| `CheckItemsAvailable` 的判定 | `HasAll(required)`(返回 bool,错误码留在桥层) |
| `CanStack` / `DrainItemStacks` / `MeasureFreeRoomPerConfig` | 同名(`DrainStacks`) |
| `PlanStackIntoExistingStacks` / `ApplyStackFill` / `StackFill` | 同名 |
| `IsAlreadyMergedAndCompact()` 的第 ① 条(可合并性) | `HasMergeablePartials()` |
| `MergeAndCompact()` 里合并堆叠的 ①② 两段 | `MergePartialStacks()`,返回待销毁 guid |
| `GridsNeededFor` | `StacksNeededFor`(改名见 §2.1) |
| `GenerateItemGuid` / `CanMintItemGuid` / `IsInvalidItemGuid` | `MintGuid` / `CanMintGuid` / `IsInvalidGuid` |

### 留在桥层

`AddItem` / `AddItems`×2 / `RemoveItem` / `RemoveItems` / `RemoveItemByPos` / `CheckSpaceFor` / `CheckItemsAvailable` / `MergeAndCompact` / `ExpandCapacity` / `ResetFromSnapshot` / `InsertItemForRestore`,以及新增的三个私有件:

- `DestroyItem(guid)` —— 两层成对销毁。少做任何一半都会腐化:只删实例,槽位被幽灵 guid 永久占着,背包"莫名其妙满了";只删槽位,`size=0` 的孤儿实例仍在 `view<ItemComp>` 里,后续并堆会往一个没有位置的实例上灌数量,单位静默丢失。
- `DesiredOrder()` —— 从实例层算出 `(config 升序, size 降序)` 的 guid 顺序,交给 `ApplyOrder`。
- `IsOrderedByConfigThenSize()` —— 拆分前 `IsAlreadyMergedAndCompact()` 的第 ③ 条。它同时要读槽位顺序和 `ItemComp`,所以只能待在桥层。

---

## 5. 布局策略

### 5.1 `FlatLayout`(在用)

一个实例恒占一格,first-fit 铺 `0..capacity-1`。拆分前那套无名逻辑的原样搬迁,**行为逐字不变**。

使用者:`kInventory` / `kWarehouse` / `kTemporary` / 全部 `dynamicBags_`,以及(暂时的)`kEquipment`。

唯一的实现改动是多了一张反向索引 `guidToSlot_`,于是 `SlotOf` / `Remove` 从 O(n) 降到 O(1)。

### 5.2 `GridLayout`(已实现,尚无生产使用者)

W×H 二维格,实例按 `Footprint` 的宽高占据一块矩形,`SlotId = y * width + x`(左上角锚点)。

**它存在的意义是证明这条缝是真的**:加一种布局策略,`ItemStore` 与 `Bag` 一行都不用改。目前没有任何背包用它,`cpp/tests/bag_test/bag_test.cpp` 的 `GridLayoutTest` 套件就是它的使用者。

三条与扁平不同的行为,正好是"两种策略"这句话的证据:

1. `At(slot)` 返回**覆盖该单元的实例**,不只是锚点——点在一件 2x2 装备的任意一角都拿到它。扁平布局根本没有"一件东西盖住多格"这个概念。
2. `CanFit` 必须真去试摆(见 §3.2)。
3. `SupportsCompaction()` 返回 `false`:玩家手摆的格子布局就是布局本身,重排等于把人家摆好的东西全打乱。于是 `Bag::MergeAndCompact` 在格子背包上**只合并堆叠、一格不挪**。

不做旋转。Tarkov 允许物品 90° 旋转,那需要 `Footprint` 带朝向、`SlotId` 带一位旋转标记,并让 `ItemEntry.pos` 承载它。等真要做格子背包时再加,现在加就是没有使用者的臆测。

### 5.3 `FixedSlotLayout`(已实现,`kEquipment` 在用)

具名固定槽:槽位有含义(头 / 胸 / 武器 / …),**不参与自动重排**。

实现上就是 `FlatLayout` 的子类,唯一区别是 `SupportsCompaction()` 返回 `false` —— 槽位记账完全复用,不重复一遍。`PlayerBagsComp` 的构造函数把 `kEquipment` 装配成 `FixedSlotLayout(kEquipmentCapacity)`。

它**刻意没有**固定槽位数、也**刻意没有**每槽准入谓词 —— 两个都是有理由的取舍,写在 §11.2(d)。一句话:机制在这里,"哪件装备进哪个槽"的口径等 `CfgItem.equip_slot` 落地后在桥层加一道卫语句即可,两层都不用动。

---

## 6. 不变量

1. 布局层不认识 `config_id` / `size` / `ItemComp` / `ItemTable` / entt(§2.2)。
2. 实例层不认识槽位 / 容量 / 满没满。
3. 销毁必须两层成对,唯一入口 `Bag::DestroyItem`。
4. `ItemStore` 内 `guidToEntity_` 与 `registry_` 严格同步,创建只走 `Insert`、销毁只走 `Erase`/`Clear`。
5. 入包一律 plan → reserve → commit;任何失败都发生在第一次写入之前。
6. `SlotId` 序列化后就是 `ItemEntry.pos`(uint32),各策略对它的解释不同但宽度与语义位置不变。
7. 换布局策略只允许在空背包上做(`Bag::SetLayout` fail-closed)——槽位号在不同策略下含义不同,带着物品换等于让所有已持久化的 `pos` 突然改变意义。

---

## 7. 协议与持久化不受影响

**这次拆分对线上协议完全不可见。**

`proto/common/database/bag_quest_mail_data.proto` 一个字节没改:

- `ItemEntry.pos`(uint32)仍然是槽位号。扁平 = 下标,具名槽 = 槽位枚举值,格子 = `y*width+x`。
- `BagAllData.capacities`(repeated uint32)仍然是每个 `BagType` 的格子数,来自 `IContainerLayout::Capacity()`。
- `ItemEntry.item_uuid` 仍然是实例身份,跨堆叠拆合稳定。

`cpp/libs/services/scene/player/system/bag_marshal.{h,cpp}` 一行没改:它只用 `Capacity()` / `ForEachItem()` / `GetItemPosByGuid()` / `ResetFromSnapshot()` / `SetCapacityForRestore()` / `InsertItemForRestore()`,全部签名与语义不变。

跨服迁移 / 回档 / 落库路径的约束(单写者冻结、快照重放不重复记账、`item_uuid` 与 `transaction_log` 的关联)全部由 `BagService` 与 marshal 层承担,本次拆分没有触碰。参见 [cross-zone-readiness-audit.md](cross-zone-readiness-audit.md) §3.2 件 1 / §11.1 与 [bag-rollback-feasibility-analysis.md](bag-rollback-feasibility-analysis.md)。

---

## 8. 行为差异清单

拆分的目标是行为不变。下面是**全部**已知差异,逐条列出:

| # | 差异 | 影响 |
|---|---|---|
| 1 | `GetItemPosByGuid()` 与槽位释放从 O(n) 线扫变 O(1) 索引查 | 纯性能,语义相同(找不到仍返回 `kInvalidU32Id`) |
| 2 | `IsFull` / `IsSpaceInsufficient` / `EmptyGridCount` 改为**由布局占用**推导,不再是 `capacity - items.size()` | 仅在"实例存在但没有槽位"这一不可达状态下不同,且新答案是对的(那一格确实空着)。该状态只能由 `InsertItemForRestore(pos = kInvalidU32Id)` 造出,而 `Marshal` 只在物品本就没有槽位时才写出这个值 |
| 3 | `FlatLayout::Place` 会先摘掉同一 guid 的既有槽位 | 防御性。可达路径上是空操作(`Insert` 撞 guid 会先失败) |
| 4 | `FlatLayout::PlaceAt` 覆盖已占槽位时,会把被顶掉的 guid 从反向索引摘掉 | 反向索引是拆分后才有的,不摘就会残留一条指向已易主槽位的记录。对外表现与拆分前 `posToGuid[pos] = guid` 一致 |
| 5 | `FlatLayout::FreeCells()` 在 `slots > capacity` 时钳到 0,不再无符号下溢 | 拆分前该状态只有 `assert` 拦,Release 下会下溢成天文数字并让背包"永远不满" |
| 6 | 脏数据日志 `PlanStackIntoExistingStacks` 打印 `config_id` 而非 `PlayerGuid()` | `Bag::playerGuid` 从未被赋值过(见 §11 遗留 b),原来打印的一直是哨兵值 |

`MergeAndCompact` 的早退判据**刻意保持相邻比较**,没有改成"跟 `DesiredOrder()` 的结果比对是否相等":`(config, size)` 完全相同的两个实例之间顺序本就是任意的(`std::sort` 不稳定),拿排序结果去比会把本已整齐的布局判成需要整理,反而把玩家摆好的东西挪走。

---

## 9. 落码清单

新增:

- `cpp/libs/modules/bag/container_layout.h` / `.cpp` —— `SlotId` / `Footprint` / `IContainerLayout` / `FlatLayout` / `GridLayout`
- `cpp/libs/modules/bag/item_store.h` / `.cpp` —— `ItemStore`

改写(公开 API 逐字未变):

- `cpp/libs/modules/bag/bag_system.h` / `.cpp` —— `Bag` 变薄桥

新增用例(插在 `BagFencedGeneratorTest` 之前——那个套件因为 `Fence()` 是单向的必须保持在文件最后):

- `cpp/tests/bag_test/bag_test.cpp` —— `GridLayoutTest` ×5、`BagLayoutSwapTest` ×3

构建登记:

- `cpp/libs/modules/CMakeLists.txt` —— `SOURCE_FILES` 加两个 `.cpp`
- `cpp/libs/modules/modules.vcxproj` —— 加两组 `ClCompile` / `ClInclude`(顺带补上一直漏登记的 `bag\comp\player_bags_comp.h`)

**拆分本体没有改动的文件**(这正是它的验收判据):`bag_service.h/.cpp`、`comp/player_bags_comp.h`、`item_system.h`、`bag_marshal.h/.cpp`、`bag_quest_mail_data.proto`、`cross_zone_test.cpp`,以及 `bag_test.cpp` 里原有的全部 38 个用例。

**缺陷修复(§11.1)是拆分之后的第二批改动**,它按需要额外碰了三个文件 —— 这不违反上面那条判据,恰恰相反:先证明拆分是等价变换,再在稳定的地基上修 bug,两批改动可以分开评审、分开回滚。

- `cpp/libs/modules/bag/bag_service.h` / `.cpp` —— 新增 `BagService::MergeAndCompact`(整理的流水编排入口)
- `cpp/libs/modules/bag/comp/player_bags_comp.h` —— 改正"由 player_lifecycle.cpp 创建"的错误注释
- `cpp/libs/services/scene/player/system/bag_marshal.h` / `.cpp` —— 改正陈旧的 no-op 注释;还原时写入 `SetPlayerGuid`

`.proto` 与 `cross_zone_test.cpp` 两批改动都没碰。

### 9.1 重建注意:`sizeof(Bag)` 变了(踩过一次,记在这里)

拆分改了 `Bag` 的内存布局(现在是 512 字节)。`PlayerBagsComp` 直接内嵌 `std::array<Bag, 4>`,所以**每一个包含 `bag_system.h` 的编译单元都必须重新编译**。全仓只有两处:

- `cpp/libs/modules/`(→ `modules.lib`)
- `cpp/libs/services/scene/player/system/bag_marshal.cpp`(→ `scene.lib`)

MSBuild 的增量判据是时间戳。只重建其中一个(或者用比 `.obj` 还旧的时间戳把源文件换回去,例如 `Copy-Item` 从备份还原)时,**链接不会报任何错** —— 两边符号名完全一致,只是结构体步长不同。症状是运行期内存损坏:一半按旧步长构造、另一半按新步长访问。

判据(实际踩到时长这样):

- entt 深处的 `is_power_of_two` 断言,或在 `basic_storage::emplace` 里读 `0xFFFFFFFFFFFFFFFF` 的访问违例;
- 决定性的一条:调试器里 `Bag::playerGuid` 读出来是 `0` 而不是 `kInvalidGuid` —— 说明构造函数根本没在那块内存上跑过,即"构造用的布局"和"访问用的布局"不是同一个。

处理:`touch cpp/libs/modules/bag/*` 之后全量重建;真机复现前先确认 `bag_marshal.obj` 比 `bag_system.h` 新。这不是拆分的缺陷,是重建姿势的坑,但它伪装成运行期 bug,值得先排掉。

---

## 10. 验收判据(2026-08-27 实测)

| # | 判据 | 结果 |
|---|---|---|
| 1 | **公开 API 逐字未变** —— `BagService` / `bag_marshal` / `PlayerBagsComp` / 原有测试一行不改就能编过 | ✅ 四个工程 Debug\|x64 全部 0 error |
| 2 | 原有 38 个 bag 用例全绿,且**没有为迁就重构改过任何一个** | ✅ `bag_test.exe` 64/64,`bag_test.cpp` 的 diff 全程是纯新增(`0 deletions`) |
| 2b | **新增用例不是摆设**:每条修复 revert 回去都得有用例变红 | ✅ 变异分数 12/12,见 §12 |
| 2c | `Release\|x64`(NDEBUG,`assert` 被编译掉)也能编过 | ✅ `modules` 0 error(`/WX` 下) |
| 3 | `cross_zone_test` 全绿 | ✅ 7/7 |
| 4 | 协议与持久化零改动(§7) | ✅ `.proto` 与 `bag_marshal.*` 未改动 |
| 5 | `container_layout.{h,cpp}` 非注释行 grep 不到 `config_id` / `ItemComp` / `item_table` / `entt`(§2.2) | ✅ 零命中 |
| 6 | `item_store.{h,cpp}` 非注释行 grep 不到 `pos` / `SlotId` / `Footprint` / `capacity` | ✅ 零命中 |
| 7 | `GridLayoutTest` 全绿 —— 加一种布局策略不需要动另外两层 | ✅ 5/5 |

判据 2 的基线是实测出来的:先把改动整体 revert 回 HEAD 重建一遍,确认 `cross_zone_test` 在干净 HEAD 上是 7/7 绿,再把改动放回去比对 —— 否则"红了"分不清是不是本来就红。

复现命令(Debug\|x64,先重建 `modules` 与 `scene`,见 §9.1):

```bash
build/cpp/tests/bag_test.exe && build/cpp/tests/cross_zone_test.exe
```

两个 exe 都要求工作目录能找到 `bin/etc/*.yaml`(从仓库根目录跑),并且 `bin/` 与 `third_party/grpc/install_vs2026_dbg/bin/` 在 `PATH` 上(`rdkafka.dll` / `zlibd.dll`)。

---

## 11. 遗留缺陷与后续

拆分时挖出九条**拆分前就存在**的缺陷,全部已于 2026-08-27 修完并配了回归用例。

其中七条是纯缺陷(§11.1),修法没有可争议的地方。另外两条原本判定为"需要产品决策",按要求也做掉了(§11.2)—— 做法是**把机制做完、把口径留成一个待填的钩子**,而不是替策划发明内容;所依赖的假设都摊开写在那一节里,**请复核**。

### 11.1 已修(2026-08-27)

- **(a) `InsertItemForRestore` 不校验 pos 越界 —— 已修。** 拆分前是一句无校验的 `posToGuid[pos] = guid`:`pos=999 / capacity=50` 的脏快照会让物品落进一个永远扫不到的幽灵槽位;两条记录撞同一个 `pos` 则把先到的顶成"在 `items` 里但没有槽位"的孤儿。两种都是既查不到、又删不掉、还占着容量。
  现在 `FlatLayout::PlaceAt` **fail-closed**(越界或已被别人占则返回 `false` 且不改任何状态),桥层退化为自动选位:**位置可以变,物品不能丢**。实在放不下(快照物品数 > 容量)才丢弃并 `LOG_ERROR` —— 宁可丢一件也不能留下没有槽位的实例,那会让实例数与槽位数永久对不上。
  用例:`BagRestoreTest.OutOfRangeSnapshotPosIsRelocatedNotLost` / `.CollidingSnapshotPosDoesNotOrphanTheFirstItem` / `.OverCapacitySnapshotDropsRatherThanBreakTheInvariant` / `FlatLayoutTest.PlaceAtRejectsOutOfRangeAndOccupiedSlots`。
- **(b) `Bag::playerGuid` 从未被赋值 —— 已修。** 新增 `Bag::SetPlayerGuid`,由 `bag_marshal::Unmarshal` 在装配 `PlayerBagsComp` 时对四个固定背包与全部 `dynamicBags_` 逐一写入,取的是挂在实体上的 `Guid` 组件(与 `TransactionLogSystem::ResolvePlayerId` 同源)。此前所有带 `player=` 的错误日志打的都是哨兵,排障时等于没有。用例:`BagTest.PlayerGuidIsSettableForLogging`。
- **(c) 两处文档与代码矛盾 —— 已修。** `bag_marshal.h` 仍写着 "CURRENT BEHAVIOR (2026-05-16): no-op",而 `.cpp` 自 2026-05-17 起就是完整实现,陈旧了三个月;`player_bags_comp.h` 声称组件由 `player_lifecycle.cpp` 在 `InitPlayerFromAllData` 时 emplace,而那个文件**根本没提过 `PlayerBagsComp`** —— 唯一的创建点是 `bag_marshal::Unmarshal` 的 `get_or_emplace`。两处都已改正并注明"原文错在哪",免得下次又被当成事实。
- **(e) 放置失败没有任何人检查 —— 已修。** `Place()` 放不下时返回 `kInvalidSlot`,但两个调用点都丢弃返回值,于是容量判定一旦出错就会产生没有槽位的孤儿实例,而且**零日志**。现在两处都检查返回值:失败即 `store_.Erase(guid)` 回滚这一件、`LOG_ERROR` 打出容量/占用/config,并返回 `kBagAddItemBagFull`。
- **(f) 跨层不变量从没被断言过 —— 已修。** "每个实例恰好占一个槽位"此前只写在 `GridSlotCount()` 的注释里。新增 `Bag::AssertLayerConsistency()`(`store_.Size() == layout_->OccupiedSlotCount()` 且槽位数 ≤ 容量),在**每个公开写入方法的出口**各调一次。Release 下 `assert` 编译掉,零开销。修完 (a) 之后这条不变量才真正恒成立 —— 两者必须一起做,否则断言会在脏快照上误报。
- **(g) `MergeAndCompact` 销毁实例却不写流水 —— 已修。** 整理会退役一批 `item_uuid`,而那正是 `transaction_log` 做外挂回收关联的键;`BagService` 只包了 `AddItem`/`RemoveItem`,整理这条路上的 guid 是无声消失的。现在 `Bag::MergeAndCompact(std::vector<DestroyedInstance> *destroyedOut = nullptr)` 在销毁前抓拍 `(guid, config, size)` 作为回执(可选参数,老调用方不受影响),并新增 **`BagService::MergeAndCompact`** 作为生产入口:frozen 检查 → 纯容器整理 → 为每个退役实例写一条 `LogItemDestroy`。用例:`BagTest.MergeAndCompactReportsRetiredInstances` / `.MergeAndCompactWithoutReceiptStillWorks`。
- **(i) 不可叠加物品被扣到 `size=0` 后永远占着格子 —— 已修。** `RemoveItemByPos` 把一件装备扣到 0 时保留实例与槽位,而 `MergePartialStacks` 明确跳过 `max_stack_size() <= 1`,于是没有任何路径回收得了它。现在整理增加一步"回收所有 `size==0` 的实例",一次扫描不区分来路(合并清空的 / 被抽光的堆 / 不可叠加僵尸),并计入早退判据。`RemoveItemByPos` 自身的语义**没有改**——扣光后实例与槽位仍保留,回收只发生在整理时。用例:`BagTest.MergeAndCompactReclaimsDrainedNonStackableSlot`。

顺带一个内部简化:`ItemStore::MergePartialStacks` 不再返回 guid 列表(改 `void`),被它清空的实例由紧随其后的 `CollectEmptyInstances` 统一扫出 —— 一条名单胜过两条会漂移的名单。

### 11.2 已修,但**建立在我替你做的假设上** —— 这两条请复核

原本判定为"需要产品决策"的两条,按要求也做掉了。做法都是**把机制做完、把口径留成一个待填的钩子**,而不是替策划发明内容。下面把每一个假设摊开写,不同意就改这几处。

#### (d) `kEquipment` 改用 `FixedSlotLayout` + 槽位口径进配置表 —— 已修(2026-08-27 全部完成)

**部位与槽位口径现在完全在数据里,C++ 里没有任何地方写死过某个部位号或槽位号。**

拆成**两张表**,因为一张表表达不了"同一部位有多个槽":

| 表 | 列 | 含义 |
|---|---|---|
| `CfgItem`(`data/Item.xlsx`) | `equip_kind` | 这件东西属于哪个**部位**;`0` = 不是装备 |
| `CfgEquipSlot`(`data/EquipSlot.xlsx`,新增) | `id` / `equip_kind` | 每个**槽位**接受哪个部位;表里没出现的槽位不接受任何东西 |

**两个手镯位 = 槽位表里两行 `equip_kind` 相同。**

这是最初那版模型(`equip_slot` = "只能进第 N 号槽")根本表达不了的:两件同部位的东西会抢同一个槽号,第二件必然被拒。发现之后模型整个换掉了。教训是**别把"是什么"和"放在哪"塞进同一个字段** —— 这跟 §2.1 里 `GridsNeededFor` 那个错误是同一类:一个名字扛了两个概念,在退化场景下恰好重合,一遇到真需求就崩。

C++ 侧只有两个函数参与,都在桥层:

- `Bag::EquipKindFor(configId)` —— 查物品表拿部位
- `Bag::FindFreeSlotForKind(kind)` —— 在槽位表里找**第一个接受该部位、且当前空着**的槽(是"第一个空的",不是"那个唯一的")

槽位表由 `all_table.cpp` 自动注册加载(导表工具生成);`bag_test` 没有那条启动链,`main()` 里显式加载了它 —— 忘了加载的症状是 `FindAll()` 为空、任何装备都放不进具名槽。

布局层依旧不认识 config(§2.2 那条判据没破):它只多了一个 `HasSlotSemantics()` 谓词,回答"我的槽位有没有含义"。注意它**不等于** `!SupportsCompaction()` —— 格子背包同样不许自动重排,但槽位没有具名语义。

> **入包严格、还原宽容 —— 这个区别是 `cross_zone_test` 抓出来的真 bug。**
>
> 第一版实现让 `InsertItemForRestore` 也走严格路径:配置里查不到槽位就拒绝。结果 `cross_zone_test` 立刻红了 —— 它的装备 config `3001/3002` 根本不在物品表里,于是**玩家的装备被直接丢掉**。这正好违反 §11(a) 立下的原则:*位置可以变,物品不能丢*。
>
> 改法是把两条路径的严格程度分开:
> - **入包**(`AddItem` → `PlaceInstance`):往装备栏塞一个没声明槽位的东西,拒。
> - **还原**(`InsertItemForRestore`):配置优先 → 退回快照 pos → 退回自动选位。表被裁过、这件是新版本装备、或者干脆是脏数据,都不该让玩家掉装备。
>
> 用例 `FixedSlotLayoutTest.RestoreKeepsGearWhoseConfigDeclaresNoSlot` 把这个区别钉死了。

仍然**刻意没有**固定槽位数:硬编 `kEquipmentCapacity` 的话,一份 `capacities[kEquipment] = 20` 的存量快照会静默丢装备。用例 `CapacityStaysRestorableSoSnapshotsDoNotLoseGear` 拦住这个"优化"。

#### (d-旧) 装备栏不再被整理重排

装备栏本质是具名固定槽(§1.2),但拆分前它和人物背包共用扁平布局,**会被"整理"重排**——头盔可能被挪到鞋子的槽位上。这是实打实的危害,与槽位分类无关,所以先把它消掉。

新增 `FixedSlotLayout : public FlatLayout`,与扁平的唯一区别是 `SupportsCompaction()` 返回 `false`。槽位记账完全复用,不重复实现。

**我刻意没做的两件事,以及为什么:**

| 没做 | 理由 |
|---|---|
| 没把槽位数写死 | 看起来"具名槽的数量当然是契约、`Resize` 该拒绝",但代价是实打实的:一份 `capacities[kEquipment] = 20` 的存量快照撞上写死的 10,还原时会**静默丢装备**。口径落地前,忠实还原比假装有契约重要。用例 `FixedSlotLayoutTest.CapacityStaysRestorableSoSnapshotsDoNotLoseGear` 就是拦住这个"优化"的。 |
| 没加每槽准入谓词 | ~~配置里根本没有这个口径~~ —— **已补上**,见本节开头的 `equip_slot` 列。 |

**gameplay 可见的行为变更(2026-08-27 已拍板)**:`PlayerBagsComp` 此前四个背包全是默认构造,即**清一色 10 格**,和文件里那张容量表(100 / 200 / 10 / 200)对不上——那张表一直只是注释,没有任何代码兑现。现在构造函数把它兑现了,**新建角色**的背包/仓库/临时格从 10 格变成 100 / 200 / 200 格。老角色从快照还原容量,不受影响。

> **决定:新角色人物背包 = 100 格。** 仓库 / 装备栏 / 临时格沿用仓库里既有的 200 / 10 / 200 常量(这三个没被单独过问)。
>
> 这个数已经钉成用例 `PlayerBagsCompTest.NewCharacterInventoryIsOneHundredSlots`,断言的是**字面量 100** 而不是 `kBagMaxCapacity` —— 另一条用例断言"构造函数用了那个常量",但证明不了"那个常量还是当初拍板的数";有人把常量从 100 改掉,那条照样绿。要改这个数就是一次 gameplay 变更:改常量 + 改这条用例 + 知会策划,三件事缺一不可。

#### (h) 自动整理 vs 还原位置 —— 已修,改成"调用点必须表态"

这不是 bug,是**两个意图直接冲突**:"东西还在你离开时摆的地方"(跨服还原忠实复现 `pos`)vs "开背包自动整理"(重排把上一句抹掉)。拆分前只有后者,于是前者只在布局恰好已是 (config 升序, size 降序) 时才成立。

我没有替你选一个默认值,而是**把取舍变成参数**:

```cpp
enum class CompactPolicy : uint8_t {
    kMergeOnly,        // 只合并 + 回收空实例,一格不挪
    kMergeAndReorder,  // 再按 (config 升序, size 降序) 重铺
};
```

`BagService::MergeAndCompact(player, bag, policy, changedOut)` 的 `policy` **没有默认值** —— 你无法在不表态的情况下整理背包。建议口径写在注释里:自动触发路径(开背包、入包后顺手整理)用 `kMergeOnly`,玩家显式点"整理"用 `kMergeAndReorder`。

`Bag::MergeAndCompact` 那一层保留了 `kMergeAndReorder` 默认值,**只是为了让既有调用方和十个既有用例一行不改** —— 生产代码请走 `BagService`。

> **决定(2026-08-27):整理是玩家点击的动作,不是开背包的副作用。**
>
> 落点是 `BagService::SortByPlayerRequest(player, bag, changedOut)` —— 背包 UI 上那个"整理"按钮接到这里,它是**目前唯一**会重排位置的入口。自动触发的路径(开背包、入包后顺手收拾)走 `MergeAndCompact(..., kMergeOnly)`:只合并堆叠、回收空实例,一格不挪;这样"跨服回来东西还在原地"才成立。
>
> 以后要做成"打开背包自动整理"的玩家设置,两层都不用动:读设置,然后决定调哪个。`CompactPolicy` 这个口子就是为那一天留的。
>
> 用例 `BagServiceSortTest.SortByPlayerRequestReorders` 与 `.AutoTidyPathDoesNotReorder` 并排放,让这个决定在用例层面一眼可见。

用例:`CompactPolicyTest.MergeOnlyMergesWithoutTouchingPositions` / `.MergeAndReorderSortsByConfigAscending` / `.SameBagTwoPoliciesTwoOutcomes`。

另外值得知道的事实:全仓**至今没有任何生产代码调用 `MergeAndCompact`**(`grep` 只有 `bag_system` 与测试),也还没有"移动物品"的接口。所以这条冲突今天并不咬人 —— 但接整理入口时,`policy` 参数会强迫那个人当场表态。

后续三步(按依赖顺序,每步都可独立发布):

1. **`FixedSlotLayout` + `kEquipment` 切换。** 装备栏不再被整理重排,`pos` 从自由下标变成槽位枚举——需要一次数据迁移或一次"重新穿戴"。
2. **把 `ItemStore` 提到玩家级。** 今天每个 `Bag` 各有一份 `ItemStore`(即每个玩家 4+N 个 `entt::registry`)。提到 `PlayerBagsComp` 上之后,「背包 ↔ 仓库 ↔ 装备栏」的移动就变成**纯布局操作**:从布局 A 摘掉 guid、在布局 B 落位,实例一动不动——`item_uuid` 天然稳定,未来挂在实例上的 per-instance 组件也不会丢。这是本次拆分真正的收益兑现点。
3. **`Footprint` 接上配置表。** `Bag::FootprintFor(configId)` 今天恒返回 `kSingleCell`,是"形状从哪来"的唯一入口。给 `CfgItem` 加 `grid_w` / `grid_h` 两列并在这里读表,格子背包就通了——实例层与布局层都不用动。届时 `Bag::CheckSpaceFor` 里那次 `CanFit(n, kSingleCell)` 要改成按 config 分组、逐形状递交。

---

## 12. 用例本身的验证:变异测试(2026-08-27)

"加了回归用例"这句话不构成证据 —— 用例可能根本测不到它声称测的东西。所以每条修复都做了一遍变异测试:**把修复逐条 revert 回去,确认对应用例真的会红**。

方法上有一条纪律:每个变异都用**唯一锚点**做字面替换,锚点找不到或不唯一就 `die`。否则一个"没贴上"的变异会伪装成"没有用例发现它",结论正好反过来。

### 12.1 九个变异与结果

| 变异 | 把什么 revert 回去 | 结果 |
|---|---|---|
| `a` | `FlatLayout::PlaceAt` 去掉越界检查、撞位改回"顶替" | ✅ 被杀(跨层断言,exit 3) |
| `b` | `SetPlayerGuid` 变空实现 | ✅ 被杀(`PlayerGuidIsSettableForLogging`) |
| `d` | 装备栏改回 `FlatLayout(kDefaultCapacity)` | ✅ 被杀(`PlayerBagsCompTest.*`) |
| `e1` | 不可叠加路径不检查 `Place()` 返回值 | ✅ 被杀(跨层断言,exit 3) |
| `e2` | 可叠加溢出路径不检查 `Place()` 返回值 | ✅ 被杀(跨层断言,exit 3) |
| `f` | `IsLayerConsistent()` 恒返回 `true` | ✅ 被杀(`LayerConsistencyPredicateActuallyDetectsBreakage`) |
| `g` | `MergeAndCompact` 不填 `destroyedOut` | ✅ 被杀(3 个用例) |
| `h` | `MergeAndCompact` 忽略 `policy`,永远重排 | ✅ 被杀(2 个 `CompactPolicyTest`) |
| `i` | `CollectEmptyInstances` 恒返回空 | ✅ 被杀(10 个用例) |
| `j` | 去掉批量入包的预设 guid 撞车预检 | ✅ 被杀(2 个 `BagBatchAtomicityTest`) |
| `k` | 去掉 `AddItems(vector)` 的铸号预检 | ✅ 被杀(`VectorBatchRejectsWholeBatch…`) |
| `l` | 去掉 `AddItems(ItemCountMap)` 的铸号预检 | ✅ 被杀(`CountMapBatchRejectsWholeBatch…`) |
| `o` | `FindFreeSlotForKind` 不检查槽位是否已被占,退回"该部位第一个槽" | ✅ 被杀(3 个用例,含 `TwoItemsOfTheSameKindOccupyTwoSlots`) |
| `p` | `EquipKindFor` 返回错误的部位号 | ✅ 被杀(4 个用例) |

**变异分数 16/16。** 基线 `bag_test` 78/78 + `cross_zone_test` 7/7。

### 12.2 它抓到的两件事(这才是跑它的价值)

**① 我的度量方式本身是错的。** 第一轮 `a` / `e1` / `e2` 报 "SURVIVED"。原因不是用例没测到,而是 Debug 下 `assert(IsLayerConsistent())` 直接 `abort` 掉进程 —— gtest 来不及打印 `[ FAILED ]`,而我的判据只 grep 那一行。**进程崩溃被记成了"没测出来",结论正好反了。** 修法:判据改成「有 `[ FAILED ]` 行 **或** 退出码非 0 **或** 没走到 `tests from ... ran` 汇总行」。

顺带一提:这三条在 Release(assert 被编译掉)下同样会被抓到,因为那几个用例里还有非断言的期望(`EXPECT_EQ(kBagAddItemBagFull, ...)`、`EXPECT_EQ(0u, OccupiedGridCount())`、`EXPECT_LT(slot, 5u)`)—— 只是 Debug 下断言先一步炸掉。这一条是推理,没有实测。

**② `f` 原本是个同义反复的用例。** 之前只有正向断言 `EXPECT_TRUE(bag.IsLayerConsistent())`,把谓词改成 `return true;` 之后全套照样绿 —— 它只能证明"没报错",证明不了"报得出错"。补了负向用例 `LayerConsistencyPredicateActuallyDetectsBreakage`:用 `SetCapacityForRestore(0)` 造出"容量 0 却占着 1 个槽位",断言谓词必须返回 `false`。

> `Bag::SetCapacityForRestore` 是唯一**不带** `AssertLayerConsistency()` 的写入口。这是有意的:marshal 在 `ResetFromSnapshot()` 之后、插入物品之前调它,那一刻背包本来就是空的;而它也因此成了从外部制造不一致、进而验证谓词的唯一途径。

### 12.3 Linux 侧:能验到哪、验不到哪

**`CMakeLists.txt` 是生成物,不是源头。** `tools/archived/vcxproj2cmake.py` 从 `.vcxproj` 生成它,生成的文件头上明写着 "GENERATED FILE — DO NOT EDIT … hand edits are lost without warning"。而 `deploy/k8s/Dockerfile.cpp` 跑的是 `build_linux.sh --skip-deps --relwithdebinfo --split-debug`,**不带 `--skip-generate`** —— 也就是每次 Linux / Docker 构建都会重新生成一遍。

所以加新 `.cpp` 时**真正要改的是 `modules.vcxproj`**;`CMakeLists.txt` 里那两行只是为了让 `--skip-generate` 这条冷门路径也能用,不是必需的。已实测:跑一遍生成器,产出的 `SOURCE_FILES` 正好包含 `bag/container_layout.cpp` 与 `bag/item_store.cpp`,与手写的两行一致。

> 顺带一个仓库现状:跑生成器会让**另外 15 个** `CMakeLists.txt` 也出现 diff —— 仓库里 committed 的这批生成物早已与 `.vcxproj` 漂移(缺生成横幅、缺 `GOOGLE_CHECK_EQ` 等定义)。不是本次改动造成的,也没有一并提交(那是无关 churn),但值得知道:**别以为 committed 的 CMakeLists 反映了当前工程结构**。

### 12.3.1 导表链路是同一个坑,而且更大

给 `CfgItem` 加 `equip_slot` 列时踩到同一类问题,规模大 40 倍,记在这里:

**在本机不改任何数据跑一遍 `tools/data_table_exporter/run.py`,会产生 388 个文件的 diff。** 抽样看过,全是**环境差异**而非数据陈旧:

- 本机 protoc 的调用路径不同,`source: tip/bag_error_tip.proto` 变成 `source: bag_error_tip.proto`,连带所有 include guard 宏改名(`tip_2fbag_...` → `bag_...`);
- Go 侧 `protoc v6.31.1` → `v7.35.1` 的版本注释;
- 若干文件的末尾换行。

所以**这次没有做全量重新导表**,只保留了 item 表相关的 29 个文件(proto + Item.json + item.pb + C++/Go/Java/Python 绑定,自洽的一套),其余 361 个环境 churn 全部还原。

两个必须知道的事实:

1. **`generated/tables/Item.json` 会变。** 导出器把零值也显式写进 JSON(`"equip_slot": 0` 每行都有),不是省略。而 Go 的 `protojson.Unmarshal` **没有开 `DiscardUnknown`**,默认对未知字段报错 —— 所以 Go / Java / Python 的绑定**必须与 Item.json 同批更新**,不能只更 C++。这也是为什么那 29 个文件里包含了 Go 与 Java。
2. **C++ 侧的产物在本机是稳定的**(不在那 388 里),Java / Python / Go 才带环境 churn。item 表的 Go 绑定 diff 是 13 增 3 删,其中"环境"部分只有一行 protoc 版本注释,可接受。

真要把这条链路捋干净(让任何人在任何机器上重跑都得到零 diff),是导表工具自己的一次改动,不属于本次范围。

已在本机复现的 CI 门禁(`.github/workflows/cpp-build-ci.yml` 的 `build-script-contract` 层,Git Bash 下逐步跑通):

| 检查 | 结果 |
|---|---|
| `bash -n tools/scripts/build_linux.sh` | ✅ |
| `--dry-run` 解析出的工程清单与文件系统对账(16 项) | ✅ 全部存在 |
| 非法参数必须 `exit 2` | ✅ |

**验不到的:真正的 Linux 编译。** 这台机器只有 VS 自带的 clang(MSVC ABI),`cmake` 不在 PATH,WSL 只有 `docker-desktop` 那个最小发行版,而 `docker pull` 被网络挡死(cloudfront `EOF`,本机既有问题)。CI 的 `compile-nodes` 层默认**不阻塞 PR**(只在 push / 定时 / 打 `cpp-full-build` 标签时跑),所以这个改动大概率要等每日定时构建才会被真正编译一次。**要提前拿到结论,给 PR 打上 `cpp-full-build` 标签。**

作为替代,靠人工复核补了一处 GCC/Clang 与 MSVC 的真实差异:新头文件用了 `std::size_t` / `uint8_t` / `std::make_unique` 却没直接 include `<cstddef>` / `<cstdint>` / `<memory>` —— MSVC 传递性带进来了,libstdc++ 不保证。四个文件已补齐。

### 12.4 复现

变异脚本住在会话临时目录,不随仓库走 —— 上面那张表把每个变异改哪一处写清楚了,照着改一遍即可。流程:

```bash
# 每个变异:改一处 -> 重建 modules + bag_test -> 跑 -> 还原
build/cpp/tests/bag_test.exe
```

判据:**每个变异都必须让至少一个用例变红或让进程非 0 退出**;有 SURVIVED 的,要么补用例,要么承认那条修复没有保护。

---

## 13. 人工对抗复核(2026-08-27)

原计划的多 agent 对抗复核**没跑成**:3 个评审 agent 加完整性 critic 全部死在周额度上(`resets Aug 29, 6pm`),只有测绘阶段出了结果。以下是我自己逐段读最终代码做的复核 —— 它比独立复核弱,该由人再过一遍。

**已修的问题**见 §12.2(度量方式错误、同义反复用例)与 §12.3(缺失的标准头)。下面是**看出来但没有修**的,连同不修的理由。

- **`Bag::AddItems` 的"全有或全无"是过度承诺 —— 已修。** 注释写着"绝不做部分添加",实现却只有 `CheckSpaceFor` 一道预检 + 一个 `RETURN_ON_ERROR(AddItem(...))` 循环。那道预检挡容量和配置表,挡不住另外两条:**发号器被 fence**(第一件纯并堆成功、第二件要铸号才失败)和**预设 guid 撞车**(第 N 件才发现)。两种都留下半批。邮件附件是这条路径的主要用户,半批发放会让调用方以为整批失败而重发,**变成复制道具**。

  修法是把话兑现:写入任何一件之前跑完三道预检 —— ①容量与配置表(`CheckSpaceFor`,顺带回执"要新建几个实例")②预设 guid 既不撞背包里已有的、也不在批内自撞 ③除去沿用预设 guid 的那些,剩下要新建的实例都得铸得出号。三道全过才开始写。剩下唯一还能中途失败的是"布局层说能放却放不下",那是编程错误,由 `AssertLayerConsistency` 兜底,不是数据能触发的情形。

  铸号预检刻意做成**精确**而非"fence 了就一律拒绝":全部携带合法预设 guid 的批次(邮件附件的典型形状)根本不需要铸号,一律拒绝会误伤它。用例 `BagBatchAtomicityTest.AllPreassignedGuidsNeedNoMintingAndSucceed` 钉的就是这一点。

  > `ItemCountMap` 那个重载值得单说:它是 `unordered_map`,**遍历顺序不确定**,所以"没有整批预检时会不会留下半批"取决于哈希序 —— 同一份数据在不同构建下表现可能不一样,是个 heisenbug。有了整批预检,拒绝才是确定的。
- **`DestroyedInstance.size` 恒为 0。** 因为只收集 `size==0` 的实例。看着像信息冗余,但它恰恰是想要的证据:整理只退役 uuid,**不销毁任何数量**。流水里 `LogItemDestroy(..., quantity=0)` 读起来就是这个意思。
- **整理现在回收所有 `size==0` 实例,拆分前每个 config 至多留一个。** 旧 `MergeAndCompact` 的 index/break 逻辑让"孤零零一个空堆"逃过回收。新行为更干净,但确实是行为变化 —— 只在整理时发生,`RemoveItems` / `RemoveItemByPos` 保留空堆的语义没动。
- **`FixedSlotLayout` 继承了 first-fit 自动选位。** 具名槽语义下"头盔落进靴子槽"是错的,但没有槽位分类就做不了更好,而 `AddItem` 又必须能放。等 §11.2(d) 的 `equip_slot` 列落地后,在桥层加准入卫语句即可。

---

## 关联

- [bag-rule-policy-layering.md](bag-rule-policy-layering.md) —— 下一刀(规则):准入 / 淘汰 / 流出 等正交策略,含本文档 §11 遗留项的延续与一条 `CanFit` 现存缺陷
- [bag-service-srp-refactor.md](bag-service-srp-refactor.md) —— 上一刀(纵切):`Bag` → `BagService`
- [cross-zone-readiness-audit.md](cross-zone-readiness-audit.md) §3.2 件 1 / §11.1 —— 背包跨服迁移与单写者冻结
- [bag-rollback-feasibility-analysis.md](bag-rollback-feasibility-analysis.md) —— 回档对实例身份的要求
- [ecs-component-access-rules.md](ecs-component-access-rules.md) —— 组件访问纪律
- `proto/common/database/bag_quest_mail_data.proto` —— `ItemEntry` / `BagAllData` 线上格式
