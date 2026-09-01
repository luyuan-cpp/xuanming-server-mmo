# 背包:玩法规则的策略化分层 (2026-09-01)

> **状态:只有设计,未落码。** 本文档记录"背包规则该怎么分层"的结论、判据与执行清单,
> 供后续实施。**2026-09-01 当天没有改任何代码、配置表或 proto。**
> 唯一的例外是文档里记下了一条**现存缺陷**(§6.1),它是本次分析顺带挖出来的,
> 同样没有修。

## 概述

问题:`kEquipment` 的两个格子只能放手镯、`kTemporary` 应该先进先出、节日背包只能放
节日道具 —— 这些背包**数据结构完全一样**,不一样的只是规则。该不该为此抽一层?

结论:**该,但抽的不是"一层",是"几个各自只回答一个问题的正交策略"。**

标准做法是 **一个参数化的容器 + 一组正交的 Strategy,按背包类型装配出来**,
而不是继承出 `EquipmentBag / TempBag / FestivalBag`。

这句话对"布局"这一维,[bag-instance-layout-split.md](bag-instance-layout-split.md)
已经说过了:

> 扁平和格子是同一个模型的两种布局策略,不是两套背包系统。

本文档做的是**把同一个口径推广到其余几维**。上一刀拆的是**状态**(实例 / 布局),
这一刀拆的是**规则**(准入 / 淘汰 / 流出 / 过期)。

| | 上一刀(2026-08-27) | 这一刀(本文档) |
|---|---|---|
| 拆什么 | 状态:实例层 / 布局层 | 规则:六个正交轴 |
| 产物 | `ItemStore` + `IContainerLayout` + 桥层 `Bag` | `BagProfile` = 一组可替换策略 |
| 判据 | 布局层永远不认识 `config_id` | 规则永远不进快照,快照只存 `profile_id` |
| 公开 API | 逐字未变 | **应当同样逐字未变** |

---

## 1. 先把问题拆对:那三个例子不在同一个轴上

把它们抽成"一层"恰恰是错的 —— 它们回答的是三个不同的问题。

| 需求 | 真正的问题 | 属于哪一轴 |
|---|---|---|
| 装备栏两个格子只能放手镯 | 这件东西**能进哪个位置** | 摆放 Placement + 准入 Admission |
| 临时背包先进先出 | 满了之后**谁被挤掉** | 溢出 / 淘汰 Eviction |
| 节日背包只能放节日道具 | 这件东西**能不能进这个容器** | 准入 Admission |
| 显示规则 | —— | **不属于服务器**,见 §1.1 |

### 1.1 显示规则不进服务器容器

分页、排序、红点、分类页签、图标 —— 一律是客户端 View + 配置表的事。

服务器容器对"显示"唯一该负的责任是 **`pos` 与 `bag_type` 稳定** —— 这正是
`CompactPolicy`(见 split 文档 §11.2(h))那个取舍在守的东西。

一旦服务器容器开始持有"怎么显示",它就同时是模型和视图了:跨服还原、流水追溯、
单测会一起被污染。**这条不妥协。**

---

## 2. 为什么不是 `class EquipmentBag : public Bag`

这是最常见的错误答案。在本仓库它有四条各自独立的致命理由:

1. **维度爆炸。** 规则是多维正交的(准入 × 摆放 × 淘汰 × 整理 × 流出 × 过期)。
   继承只有一维。"只收节日道具 + FIFO + 不许整理"这种组合一出现,子类就要按
   笛卡尔积增殖。
2. **规则在运行期随运营配置变。** 春节包和中秋包是两套准入规则,但它们不该是两个
   C++ 类 —— 否则每上一个活动要发一次版本。
3. **值语义存不下多态。** `PlayerBagsComp::bags` 是 `std::array<Bag, kBagTypeCount>`,
   `dynamicBags_` 是 `unordered_map<uint64_t, Bag>`。装 `Bag` 子类会 object slicing。
   现在的 `SetLayout(std::unique_ptr<IContainerLayout>)` 已经绕开了这个坑 ——
   那就是正确方向,沿着它走。
4. **快照还原要的是"同一个类的不同配置"。** `bag_marshal::Unmarshal` 是
   `get_or_emplace` 出组件再逐条回填。子类型意味着还原时要先知道"这个包是哪个类"
   再构造 —— 那本质上还是配置驱动,只是绕了一圈。

对应原则:**组合优于继承**、**策略正交**、**规则数据化**。

---

## 3. 正交轴清单(本文档的主体)

按**问题**拆接口,不按**背包**拆。每个接口只回答一件事(ISP)——
**不要**做一个大 `IBagRule`。

| 轴 | 只回答一个问题 | 有副作用? | 现状 |
|---|---|---|---|
| **摆放** `IContainerLayout` | 放哪一格 | 有(写槽位) | ✅ 已有,做得对 |
| **准入** `IAdmissionPolicy` | 这件东西能不能进来 | **无**(纯谓词) | ❌ 缺 → 节日包 |
| **淘汰** `IEvictionPolicy` | 满了挤掉谁 | **有**(唯一一个) | ❌ 缺 → 临时包 FIFO |
| **排序 / 整理** | 能不能重排、按什么排 | 有 | ✅ `SupportsCompaction()` + `CompactPolicy` |
| **流出** `IWithdrawPolicy` | 能不能拿出去 / 移到别的包 | 无 | ❌ 缺 → 绑定 / 任务道具 / 活动期结束 |
| **过期** | 整包什么时候清 | 有 | ❌ 缺 |

### 3.1 「有无副作用」那一列是纪律,不是描述

`Bag` 的核心是三段式 **`plan -> reserve -> commit`**(见 `Bag::AddStackableItem`
的注释)。这条纪律决定了每个轴能放在哪:

- **准入 / 流出必须是纯谓词** —— 只有零副作用,才能塞进 `reserve` 之前而不破坏三段。
  一旦准入检查会改状态,"失败一定发生在 reserve 之前,绝无半写状态"这句话就作废。
- **淘汰是唯一会改状态的规则** —— 所以它必须**有回执**,理由见 §6.2。

### 3.2 淘汰不能自己销毁

与 `ItemStore::MergePartialStacks` 同一条纪律:**销毁必须两层成对进行,那是桥层的活**。
淘汰策略只负责**选出**该退役哪些 guid,由 `Bag` 执行销毁、由 `BagService` 落流水。

`cpp/libs/modules/bag/bag_system.cpp:340` 那句 `// TODO: overflow to temp bag or mail`
就是这一轴的占位符 —— 它今天的实现是隐式的 `RejectWhenFull`。

---

## 4. 模式对照(正式名字)

| 用在哪 | 模式 | 出处 |
|---|---|---|
| 摆放 / 淘汰 —— 一族可替换算法 | **Strategy** | GoF |
| 准入 —— 可组合的布尔判定 | **Specification** + **Composite**(`And`/`Or`/`Not`) | Evans《DDD》/ Fowler |
| 按背包类型装配一整套策略 | **Abstract Factory**(实践中叫 **Profile / Descriptor**) | GoF |
| 规则本身 | **数据驱动**,写在配置表而不是 C++ | 本仓库已有此文化:`CfgItem.equip_kind` + `CfgEquipSlot` |

### 4.1 明确**不要**用的

| 不要用 | 为什么 |
|---|---|
| **Policy-based design**(模板参数,Alexandrescu) | 编译期绑定。需要按 `BagType` / 活动 id 在**运行期**换,不适用 |
| **Chain of Responsibility** | Specification 的 `And` 组合已覆盖,再引入责任链只是多一套词汇 |
| **Template Method**(在 `Bag` 里开虚钩子给子类覆盖) | 伪装成"抽层"的继承,回到 §2 的四条理由 |
| **`if (bagType == kXxx)`** | split 文档 §1.2 已经点名这是要避免的终局 |

---

## 5. 目标形状

新增的东西**和 `layout_` 并列**挂在桥层 `Bag` 上,**绝不能进布局层**。

理由是 split 文档 §2.2 立下的那条判据:**布局层永远不认识 `config_id`,
"哪天这里需要读一次 ItemComp,这条缝就漏了"**。而准入规则天生要认识 config
(要知道这是不是节日道具),所以它只能和 `Bag::EquipKindFor()` 一样长在桥层。

```cpp
// admission_policy.h —— 纯谓词,零副作用,可脱离 Bag 单测
class IAdmissionPolicy
{
public:
    virtual ~IAdmissionPolicy() = default;

    // 单件:这件东西能不能进这个容器。
    [[nodiscard]] virtual bool Accepts(uint32_t configId) const = 0;

    // 整批:这一批能不能一次性容纳(reserve 阶段用)。
    // 为什么必须有这一条而不是循环调 Accepts —— 见 §6.1。
    [[nodiscard]] virtual bool AcceptsBatch(const ItemCountMap &items,
                                            const IContainerLayout &layout) const = 0;
};

class AcceptAll        final : public IAdmissionPolicy {};  // 人物背包 / 仓库 / 临时格
class AcceptByTag      final : public IAdmissionPolicy {};  // 节日包
class AcceptEquippable final : public IAdmissionPolicy {};  // 装备栏(含按部位的槽位可用性)
class AllOf            final : public IAdmissionPolicy {};  // Composite
```

```cpp
// eviction_policy.h —— 唯一有副作用的轴,所以必须回执
class IEvictionPolicy
{
public:
    virtual ~IEvictionPolicy() = default;

    // 要腾出 needed 个实例位,应该退役哪些 guid(按退役顺序)。
    // **它自己不销毁任何东西** —— 见 §3.2。
    [[nodiscard]] virtual GuidVector SelectVictims(
        std::size_t needed,
        const ItemStore &store,
        const IContainerLayout &layout) const = 0;
};

class RejectWhenFull   final : public IEvictionPolicy {};  // 返回空 = 拒绝入包,今天的行为
class EvictOldestFirst final : public IEvictionPolicy {};  // 临时包 FIFO
```

### 5.1 装配点已经存在

`comp/player_bags_comp.h` 的构造函数**已经是雏形的 Abstract Factory**,
只是今天只装配了布局一维:

```cpp
// 今天:
bags[kEquipment].SetLayout(std::make_unique<FixedSlotLayout>(kEquipmentCapacity));

// 推广后:一次装配一整套 profile
bags[kEquipment].SetProfile(BagProfile::Equipment());
bags[kTemporary].SetProfile(BagProfile::Temporary());          // Flat + AcceptAll + EvictOldestFirst
dynamicBags_[eventId].SetProfile(BagProfile::FromTable(profileId));  // 节日包,查表
```

`BagProfile` 就是 `{layout, admission, eviction, ordering}` 四个 `unique_ptr` 的一个包。
**它是装配器,不是新的一层** —— `Bag` 的公开 API 一行不变,这与上一刀的验收判据同源。

---

## 6. 三个需求逐个落地

### 6.1 装备栏两个手镯位 —— 机制已完成,但 `reserve` 阶段有洞

**已完成的部分,不要重做:**
`FixedSlotLayout` + `CfgItem.equip_kind` + `CfgEquipSlot`(两行同 `equip_kind` = 两个
手镯位)+ `Bag::FindFreeSlotForKind()`(找"第一个空的接受槽")。整条链完整,
见 split 文档 §11.2(d)。

#### 现存缺陷:`CanFit()` 对具名槽说谎

> **未修。** 2026-09-01 由静态阅读发现,**没有跑过用例验证**,实施前请先写一个
> 失败用例确认。

`FixedSlotLayout` 继承 `FlatLayout` 且**没有覆盖 `CanFit()`**,于是:

| 位置 | 代码 | 后果 |
|---|---|---|
| `container_layout.cpp:16` | `FlatLayout::CanFit` = `FreeCells() >= count` | 只数空格,不问部位 |
| `bag_system.h` `IsSpaceInsufficient()` | `!layout_->CanFit(gridCount, kSingleCell)` | 装备栏预检失真 |
| `bag_system.cpp:339` `AddNonStackableItem` | `if (IsSpaceInsufficient(pieceCount))` | 同上 |
| `bag_system.cpp:105` `CheckSpaceFor` | `if (!layout_->CanFit(instancesNeeded, kSingleCell))` | 同上 |

推演路径(装备栏 10 格、2 个手镯位、当前全空,一次放 **3 只手镯**):

1. `AddNonStackableItem` 的 `IsSpaceInsufficient(3)` **通过** —— 10 格空着
2. 循环:第 1、2 只 `PlaceInstance` 成功
3. 第 3 只 `PlaceInstance` -> `FindFreeSlotForKind` 返回 `kInvalidSlot`
   -> `store_.Erase(guid)` -> 返回 `kBagAddItemBagFull`
4. **前两只留在包里,函数却返回失败**

`AddItems(ItemCountMap)`(`bag_system.cpp:524`)同样漏 —— `CheckSpaceFor` 也只调
`CanFit`,`RETURN_ON_ERROR` 会在半批状态下退出。这正是那段注释发誓不会发生的事:

> 事务语义:与 RemoveItems 完全对称。任何一项不满足就整体失败,
> 绝不做"前几种已进包、后一种失败"的部分添加。

**这个洞就是"缺准入轴"的直接证据**,也正是 `IAdmissionPolicy` 必须有
`AcceptsBatch(items, layout)` 而不能只有单件 `Accepts` 的原因:
**具名槽的容量是按部位分桶的,不是一个标量**,`FreeCells()` 这个数字对它没有意义。

两种修法:

| 修法 | 评价 |
|---|---|
| `FixedSlotLayout` 覆盖 `CanFit` | 做不到 —— 它认不得 `config_id`,而"这批里几只是手镯"必须查表。硬做就是让布局层认识 config,§5 那条判据当场破 |
| 把判定提到桥层的准入策略 | **正解**。与 `EquipKindFor` / `FindFreeSlotForKind` 同层同源 |

> **可达性存疑,实施前先确认。** 今天是否真有生产路径往 `kEquipment` 批量塞同部位
> 装备(邮件附件发两只手镯?),没有查证。即使今天不可达,它也是一颗定时炸弹 ——
> 穿戴流程一落地就踩上。

### 6.2 临时背包 FIFO —— 淘汰必须有回执,且缺"入包顺序"这个状态

#### (1) 挤掉的东西是玩家资产,不能无声消失

这个坑已经踩过并解决过一次:`MergeAndCompact` 退役实例时用
`std::vector<DestroyedInstance> *destroyedOut` 回执,由 `BagService::MergeAndCompact`
落 `transaction_log` —— 因为 `item_uuid` 是外挂回收的关联键(split 文档 §11.1(g))。

**FIFO 淘汰与它完全同构,照抄这个形状:**

- `Bag` 只**报告**退役了哪些(销毁前抓拍 `guid` / `config` / `size`)
- `BagService` 落流水
- 生产代码不许直接调纯容器那层

#### (2) FIFO 需要"入包顺序",而现在没有这个状态

| 候选 | 能不能用 |
|---|---|
| `pos` | ❌ 布局的事,没有时间序 |
| `guid`(snowflake) | ⚠️ 高位是时间,**近似**单调,但两条路径会打乱:①跨服迁移带回来的是**源服 node** 铸的号;②`AddItems(std::vector<InitItemParam>)`(邮件附件)**沿用调用方预设的 guid** |
| 新增显式字段 | ✅ **正解** |

给 `ItemEntry` 加一个显式的获得序号 / 时间戳字段。
**字段号 6/7/8 已被 TODO 预定**(enchant / affixes / gem,见
`proto/common/database/bag_quest_mail_data.proto`),**用 9**。

> 注意 `ItemComp`(`proto/common/component/item_base_comp.proto`)今天只有
> `item_id` / `config_id` / `size` **三个字段** —— split 文档里"proto 里已按已知
> 字段号预留了强化等级 / 词条 / 镶嵌"指的是 `ItemEntry`(持久化),不是 `ItemComp`
> (运行时)。FIFO 序要在运行时可读,**两边都得加**。

### 6.3 节日背包 —— 缺的是表列,不是 C++

`generated/code/proto/item_table.proto` 今天只有三列:

```proto
message ItemTable {
    uint32 id = 1;
    uint32 max_stack_size = 2;
    uint32 equip_kind = 3;
}
```

**没有任何分类 / 标签列**,所以"只能放节日道具"这句话在数据里根本表达不出来。
**先加列,再写代码:**

| 表 | 新增列 | 含义 |
|---|---|---|
| `CfgItem`(`data/Item.xlsx`) | `tag` / `category` / `activity_id` | 这件东西属于哪一类 |
| `CfgBagProfile`(新表) | `profile_id` / 布局类型 / 容量 / 接受哪些 tag / 淘汰策略 / 能否整理 / 过期时间 | 一个 profile 的全部规则 |

这与 `equip_kind` + `CfgEquipSlot` 是同一个套路:**规则由表数据定义,
C++ 里不写死任何一个 tag 值。** 这条在装备槽上已经做对了。

#### 载体用 `dynamicBags_`,但快照缺 `profile_id`

`comp/player_bags_comp.h` 的注释已经明确:节日包属于运行时 / 临时背包这一档,
**不要加进 `BagType` 枚举**(那是持久化契约,不能为活动增殖)。

**缺口:** `BagAllData.DynamicBagData` 今天只带 `bag_id + capacity + items`,
**没有 profile id**(见 `bag_marshal.cpp:74` 的 Marshal 与 `:149` 的 Unmarshal)。
一旦规则挂到包上,跨服回来节日包会退化成默认 `FlatLayout(kDefaultCapacity)` 的
自由包 —— **规则静默消失**。加 proto 字段和 `SetProfile` 还原路径必须一起做。

---

## 7. 红线(别把已经挖开的缝焊回去)

1. **布局层永远不许认识 `config_id`。** split 文档 §2.2 的唯一判据。准入规则要查表
   → 它属于桥层。
2. **准入 / 流出必须零副作用**,才能放进 `reserve` 之前保住三段纪律。
3. **淘汰必须回执**,由 `BagService` 落流水。纯容器不写流水,这条已经定了。
4. **不要一个大 `IBagRule`。** 六个轴六个小接口,各自可单测、可组合。
5. **不要把策略对象序列化进快照。** 快照存 `profile_id`,还原时**重新装配**。
   策略是代码 + 配置,不是玩家数据。
6. **`BagType` 枚举不为活动增殖**,动态包走 `dynamicBags_`。
7. **入包严格、还原宽容。** split 文档 §11.2(d) 那条区别对准入规则同样适用:
   往节日包里塞非节日道具要拒;**但快照还原时不许因为规则不匹配就丢玩家东西**
   (表被裁过 / 活动已下线 / 脏数据都可能触发)。*位置可以变,物品不能丢。*

---

## 8. 执行清单(建议顺序)

> 全部未开工。每一步都应当能独立编译、独立跑 `bag_test`,不要合并成一个大改动。

| 步 | 做什么 | 为什么排这里 | 涉及 |
|---|---|---|---|
| 1 | 为 §6.1 的 `CanFit` 洞写一个**失败用例**,确认可达性 | 先证明缺陷存在,再动结构 | `cpp/tests/bag_test/` |
| 2 | 修 §6.1:把具名槽的批量准入判定提到桥层 | 是现存 bug,且能顺手验证"准入必须批量可判定" | `bag_system.{h,cpp}` |
| 3 | 抽 `IAdmissionPolicy` + `BagProfile` 装配器,四个固定包全部走 `AcceptAll` | 纯重构,行为逐字不变,可用现有 `bag_test` 当验收 | 新增 `admission_policy.{h,cpp}`、`player_bags_comp.h` |
| 4 | 加 `CfgItem.tag` + `CfgBagProfile` 两张表 | 规则数据化的前提 | `data/*.xlsx` + 导表 |
| 5 | 节日包:`AcceptByTag` + `DynamicBagData.profile_id` + 还原路径 | 第一个真实使用者,证明这条缝是真的 | proto + `bag_marshal.cpp` |
| 6 | `IEvictionPolicy` + `EvictOldestFirst` + `ItemEntry`/`ItemComp` 序号字段 + 流水回执 | 唯一有副作用的轴,单独一批做,风险最高 | proto + `bag_system` + `bag_service` |
| 7 | `IWithdrawPolicy`(绑定 / 任务道具 / 活动结束) | **等有真实使用者再做** | —— |

第 7 步的判据来自 `GridLayout` 自己的注释:

> 等真要做格子背包时再加,现在加就是没有使用者的臆测。

同样适用于规则轴 —— **没有使用者的策略接口不要提前造。**

### 8.1 编译

按 `CLAUDE.md` §10.1:Claude 不执行编译。任一步落码后交 Codex 编译
`modules` + `bag_test`(C++ MSBuild 串行 `/m:1`),未拿到结果前不得声称"测试绿"。

---

## 关联

- [bag-instance-layout-split.md](bag-instance-layout-split.md) —— 上一刀(横切):
  `Bag` -> `ItemStore` + `IContainerLayout`。本文档是它 §11 遗留项的延续
- [bag-service-srp-refactor.md](bag-service-srp-refactor.md) —— 更早一刀(纵切):
  `Bag` -> `BagService`
- [cross-zone-readiness-audit.md](cross-zone-readiness-audit.md) §3.2 件 1 ——
  背包跨服迁移;§6.3 的 profile 持久化缺口挂在这里
- `proto/common/database/bag_quest_mail_data.proto` —— `ItemEntry` / `BagAllData` /
  `DynamicBagData` 线上格式
- `generated/code/proto/item_table.proto` / `equipslot_table.proto` —— 现有表列
