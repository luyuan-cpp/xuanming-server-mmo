# 背包:玩法规则的策略化分层 (2026-09-01)

> **状态(2026-09-02):三个原始需求全部落码,P0/P1/P2 清零,未编译。**
>
> 用户最初问的三件事,现在都有完整实现与回归用例:
> ①**装备栏两个手镯位**(具名槽 + 按部位 reserve,§6.1);
> ②**临时包先进先出**(淘汰轴 + **真入包序号**,不再是 guid 近似,§6.2);
> ③**节日包只收节日道具**(`AcceptByConfigSet` + profile 注册表 + `profile_id`
> 持久化,§6.4)。
>
> 落码顺序:9-01 落 §8 第 1/2/3/6 步 → 9-01 自查修一处顺序缺陷(§6.3)→
> 9-02 第二轮 8 视角审计 + 对抗证伪,18 条全部成立并修完(§6.5)→
> 9-02 补完真序号与节日包。落码过程中推翻了 §5 原先画的 `AcceptsBatch`(§5.1)。
>
> **改了三个源 proto**:`ItemComp.acquire_seq`(字段 4)、`ItemEntry.acquire_seq`
> (字段 **13** —— 6~12 被 TODO 表预定,早先说"用 9"是错的)、
> `DynamicBagData.profile_id`(字段 4)。需按 `CLAUDE.md` §4 重生成。
> **配置表(xlsx)一个字没动。**
>
> **编译清单见 §8.2:scene 工程不能漏**(`sizeof(Bag)` 变了)。
>
> 未做且是刻意的:`AcceptByTag`(等 `CfgItem` 加 tag 列;没有列它会什么都不收,
> 比没有更糟 —— 见 §6.4)、`CfgBagProfile` 表化(注册表是可用替代)、
> 第 7 步 `IWithdrawPolicy`(按判据等使用者)。
>
> ⚠ 另见 §6.1 的查证:**整个背包域在生产侧零调用点**,这套规则今天还没有任何
> 真实使用者。第 4 步之前应先接一条真实入包链路。

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

| 轴 | 只回答一个问题 | 有副作用? | 现状(2026-09-01) |
|---|---|---|---|
| **摆放** `IContainerLayout` | 放哪一格 | 有(写槽位) | ✅ 已有 |
| **准入** `IAdmissionPolicy` | 这个包收不收这种东西 | **无**(纯谓词) | ✅ 接口 + `AcceptAll` 已落码;`AcceptByTag`(节日包)待第 5 步 |
| **淘汰** `IEvictionPolicy` | 满了挤掉谁 | **有**(唯一一个) | ✅ 已落码:`RejectWhenFull`(三个包)+ `EvictOldestFirst`(临时格);FIFO 序仍是 guid 近似,见 §6.2 |
| **排序 / 整理** | 能不能重排、按什么排 | 有 | ✅ `SupportsCompaction()` + `CompactPolicy` |
| **流出** `IWithdrawPolicy` | 能不能拿出去 / 移到别的包 | 无 | ❌ 缺 → 绑定 / 任务道具 / 活动期结束(第 7 步,等使用者) |
| **过期** | 整包什么时候清 | 有 | ❌ 缺 |

> **注意"摆放"这一轴有个例外**:具名槽下"这批摆不摆得下"**不是**布局层能独自
> 回答的(要按部位分桶,而部位在配置表里)。那一问落在桥层的 `Bag::CanReserve()`,
> 见 §5.1 与 §6.1。这不是轴划错了,而是这一轴的答案天然需要两层的信息。

### 3.1 「有无副作用」那一列是纪律,不是描述

`Bag` 的核心是三段式 **`plan -> reserve -> commit`**(见 `Bag::AddStackableItem`
的注释)。这条纪律决定了每个轴能放在哪:

- **准入 / 流出必须是纯谓词** —— 只有零副作用,才能塞进 `reserve` 之前而不破坏三段。
  一旦准入检查会改状态,"失败一定发生在 reserve 之前,绝无半写状态"这句话就作废。
- **淘汰是唯一会改状态的规则** —— 所以它必须**有回执**,理由见 §6.2。

### 3.2 淘汰不能自己销毁

与 `ItemStore::MergePartialStacks` 同一条纪律:**销毁必须两层成对进行,那是桥层的活**。
淘汰策略只负责**选出**该退役哪些 guid,由 `Bag` 执行销毁、由 `BagService` 落流水。

`Bag::AddNonStackableItem` 里那句 `// TODO: overflow to temp bag or mail` 就是这一轴的
占位符 —— 它今天的实现是隐式的 `RejectWhenFull`。

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

    // 这个容器收不收这种东西。**只看 config,不看当前占用**。
    [[nodiscard]] virtual bool Accepts(uint32_t configId) const = 0;
};

class AcceptAll   final : public IAdmissionPolicy {};  // 已落码:四个固定包 + 全部动态包
class AcceptByTag final : public IAdmissionPolicy {};  // 待做(第 5 步):节日包
class AllOf       final : public IAdmissionPolicy {};  // 待做:Composite,规则可组合时再加
```

### 5.1 落码时推翻的一处:`AcceptsBatch` 不属于这一层

本节原先还画了一个 `AcceptsBatch(items, layout)`,用来回答"装备栏的两个手镯位
装不装得下这一批",并把 `AcceptEquippable` 列为装备栏的准入策略。**实现时发现这
是错的**,已改。

理由有两条,第二条是硬的:

1. "这批摆不摆得下"要同时读**布局的当前占用**和**配置表**,那是桥层的活,不是
   一个只认识 config 的谓词能回答的。
2. 更关键:它**必须与 `Bag::PlaceInstance` 在同一个条件上分叉**
   (`layout_->HasSlotSemantics()`)。§6.1 那个 bug 的根就是两侧分叉条件不一致 ——
   commit 侧按具名槽去查槽位表,reserve 侧却一律拿 `FreeCells()` 作答。
   **把它做成可插拔策略,等于让"两侧必须一致"重新变成一件靠人记住的事** ——
   下一次有人给装备栏配了 `AcceptAll`,这个 bug 就原样复活,而且更难看出来。

所以具名槽的批量判定落在桥层的 `Bag::CanReserve()`,它与 `PlaceInstance` 逐字
共用那个 `if`;`IAdmissionPolicy` 只保留与占用无关的纯 config 判定(节日包收不收
这种道具)。`AcceptEquippable` 因此不存在。

代价是装备栏的 profile 里写的是 `AcceptAll` —— 读起来像"装备栏没有准入规则",
容易误解。用例 `BagProfileTest.FixedBagsCarryNoAdmissionRuleYet` 把这个取舍钉住了:
谁想把"只收装备"挪进准入策略,会先撞到它。

**推论(给第 5 步):** `AcceptByTag` 是安全的,因为"是不是节日道具"跟占用无关。
但凡将来出现"这个包最多收 3 件任务道具"这种**跟占用有关**的规则,它同样不该
进准入层,而要走 `CanReserve` 那条路。

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

### 5.2 装配点(已落码)

`comp/player_bags_comp.h` 的构造函数原本就是雏形的 Abstract Factory,只是当时
只装配了布局一维。现在它装配整个 profile:

```cpp
// 落码后(bag_system.h 定义 BagProfile,player_bags_comp.h 装配)
bags[kInventory].SetProfile(BagProfile::Flat(kBagMaxCapacity));
bags[kWarehouse].SetProfile(BagProfile::Flat(kWarehouseMaxCapacity));
bags[kEquipment].SetProfile(BagProfile::Equipment(kEquipmentCapacity));
bags[kTemporary].SetProfile(BagProfile::Temporary(kTempBagMaxCapacity)); // 先进先出

// 待做(第 5 步):节日包按表装配
dynamicBags_[eventId].SetProfile(BagProfile::FromTable(profileId));
```

`BagProfile` 今天是 `{layout, admission, eviction}` 三个 `unique_ptr`;`expiry` 是
后面的字段 —— **加字段时上面四行一个字都不用改**,这正是做成 struct 而不是三个
setter 的理由。加淘汰轴时兑现了这句话:只多了一个字段、一个 `Temporary()` 工厂,
装配点只有 `kTemporary` 那一行从 `Flat` 换成了 `Temporary`。

`Flat()` 收一个容量参数(三个自由格包只差这个数字),`Equipment()` 收槽位数。
容量刻意**不写死在 profile 里**:它是 profile 给的**起点**,随后会被
`ExpandCapacity`(玩法解锁)或 `SetCapacityForRestore`(快照还原)覆盖 ——
它是状态,不是规则。

**它是装配器,不是新的一层** —— `Bag` 的公开 API 只增不改
(`SetProfile` / `SetAdmission` / `Admission()` 是新增的,老方法一个没动),
这与上一刀的验收判据同源。

一个刻意的不对称:`SetLayout` 要求背包**必须是空的**(槽位号在不同布局下含义
不同,带着物品换布局等于让所有已持久化的 `pos` 突然改变意义),而
`SetAdmission` **不要求** —— 准入是纯谓词,换掉它不重新解释任何已存状态。
换成更严的策略后包里可能留着新策略不接受的存量物品,那是刻意的:
**不能因为规则变了就丢玩家的东西**(同 §7 红线 7)。

---

## 6. 三个需求逐个落地

### 6.1 装备栏两个手镯位 —— 机制已完成,但 `reserve` 阶段有洞

**已完成的部分,不要重做:**
`FixedSlotLayout` + `CfgItem.equip_kind` + `CfgEquipSlot`(两行同 `equip_kind` = 两个
手镯位)+ `Bag::FindFreeSlotForKind()`(找"第一个空的接受槽")。整条链完整,
见 split 文档 §11.2(d)。

#### 缺陷:`CanFit()` 对具名槽说谎 —— 已落码,未编译

> **2026-09-01 已修,但尚未编译、尚未跑过用例**(按 `CLAUDE.md` §10.1,构建由
> Codex 执行)。下面的推演仍是**静态阅读**的产物;回归用例已一并写好
> (`EquipmentReserveTest` 套件),跑通之前不要当作"已验证"。

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
| 把判定提到桥层 | **正解,已采用**。与 `EquipKindFor` / `FindFreeSlotForKind` 同层同源 |

#### 实际修法

新增 `Bag::CanReserve(instancesByConfig, totalInstances)`,取代三处 reserve 里的
裸 `CanFit` / `IsSpaceInsufficient`(`CheckSpaceFor`、`AddNonStackableItem`、
`AddStackableItem`)。它做三件事:

1. 先问格子数(`layout_->CanFit`)—— 任何布局都要过;
2. `if (!layout_->HasSlotSemantics()) return true;` —— **这一行是整个修复的要点**,
   它与 `PlaceInstance` 的分叉条件逐字相同;
3. 具名槽:把需求按**部位**汇总(不是按 config —— 两件不同的手镯共用同一对槽位),
   再逐部位比对空槽数。

配套:抽出文件级静态函数 `IsFreeSlotForEquipKind()`,让
`FindFreeSlotForKind`(commit 侧)与新增的 `CountFreeSlotsForKind`(reserve 侧)
**共用同一条"可用空槽"判据** —— 两侧各写各的,正是这个 bug 的成因,不能再犯第二次。
它顺带补上了原先漏掉的 `slot < Capacity()` 过滤(容量可被快照还原改小)。

`IsSpaceInsufficient()` 保留为公开 API(对自由格布局仍然正确),但注释里标明了
它对具名槽不完整;`IContainerLayout::CanFit` 与 `FixedSlotLayout` 的注释也各加了
一段,说明"这里为什么不能覆盖 CanFit"。

**错误码刻意没变**:准入/槽位不足仍返回今天那两个码
(`kBagAddItemBagFull` / `kBagItemNotStacked`),只是**失败时机**从 commit 中途
提前到写入之前。想把"这个包不收"与"满了"分开,需要给 `bag_error_tip.proto` 加
新码 —— 那要重生成,留到后续。

#### 可达性:已查证 —— **今天不可达,因为整个背包域在生产侧零调用点**

2026-09-01 查证结果(`grep` 判据附后):

| 问题 | 答案 |
|---|---|
| 生产代码里谁调用 `Bag::AddItem` / `AddItems`? | **没有人。** `cpp/libs/services` 与 `cpp/nodes` 零命中 |
| 谁调用 `BagService::*`? | **没有人。** 全仓提到 `BagService` 的非测试位置全是注释 |
| 谁用 `PlayerBagsComp`? | 只有 `bag_marshal.{h,cpp}`(序列化 / 跨服还原) |

```bash
grep -rn "BagService" --include=*.cpp --include=*.h cpp/ | grep -v modules/bag/bag_service | grep -v ^cpp/tests
grep -rn "\.AddItem\|AddItems" --include=*.cpp cpp/libs/services cpp/nodes
```

所以**背包今天只被"存"和"还原",没有任何玩法入口**——加物品、扣物品、整理这些
公开 API 的唯一使用者是 `bag_test`。这与 `player_bags_comp.h` 里记的那段历史
(2026-05-17 之前"production code had ZERO instantiations")是同一件事的延续:
组件建起来了,玩法链路仍然没接。

**推论一(给这个 bug):** 它是**定时炸弹,不是正在流血**。穿戴 / 邮件附件 /
掉落任一条链路接上来的当天就会踩上,但今天线上不会因为它掉装备。修得早是对的,
优先级不必按"线上事故"排。

**推论二(给第 4~6 步,更重要):** 节日包、FIFO 临时包都是**玩法规则**,而玩法
入口不存在。在一个没有调用者的容器上继续叠规则,等于在空中盖楼 —— 规则写完了
也没有任何路径会去执行它,连"跑一遍看看对不对"都做不到,只能靠单测自证。
建议在推进第 4 步之前,先把**至少一条真实入包链路**接到 `BagService::AddItem`
(掉落拾取或邮件领取都行),让这套规则有第一个真实使用者。这与第 7 步的判据
是同一条:**没有使用者的东西不要提前造。**

### 6.2 临时背包 FIFO —— 已落码(排序依据仍是近似)

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

> **2026-09-02 已落地,不再是近似。** 新增 `ItemComp.acquire_seq`(字段 4)与
> `ItemEntry.acquire_seq`(**字段 13** —— 6~12 被上面那张 TODO 表预定了,早先
> 说"用 9"是错的)。
>
> 盖章点是 `ItemStore::Insert` —— 本仓库唯一的实例创建口径:
>   * `acquire_seq == 0` → 盖当前水位(新实例 / 旧存档没盖过章);
>   * `acquire_seq > 0` → 原样保留,并把水位抬到它之上(快照带回来的章)。
>
> 于是"店里每个实例都有非零、同包内单调的序号"是**结构保证**,淘汰策略可以直接
> 比大小。`Clear()` 复位水位,免得反复还原的实体涨到没有意义的大数。
>
> 刻意**不做** `seq==0 时回退 guid` 的混排:两个数域混排会让旧存档的物品永远
> 排在最后,先进先出照样挤错人。补盖比回退干净。
>
> 回归用例 `AcquireSeqTest.FifoFollowsSequenceNotGuid` 把这件事钉死:它让
> **更早进包的那件拿到更大的 guid**(跨服 / 邮件附件的真实形状),按 guid 排就会
> 挤错人。

#### (3) 落码后的实际形状

| 环节 | 落在哪 |
|---|---|
| 选谁退役(零副作用) | `EvictOldestFirst::SelectVictims` —— 腾不够就返回**空**,绝不"能腾多少算多少" |
| 真销毁(两层成对) | `Bag::ReserveOrEvict` —— 且**先算清楚腾了够不够,够了才销毁** |
| 回执 | `AddItem` / `AddItems` 新增的 `evictedOut`(可选参数,老调用方不受影响) |
| 落流水 | `BagService::LogEvictedInstances` → `LogItemDestroy`,**无论本次入包成败都落** |

三条容易写错、已经用例钉住的地方:

1. **具名槽一律不淘汰。** 装备栏配的是 `RejectWhenFull`,桥层还有第二道锁
   (`ReserveOrEvict` 见到 `HasSlotSemantics()` 直接拒)—— 两道一起,才挡得住
   "有人手工把 `EvictOldestFirst` 配到装备栏上"。
   用例 `EvictionPolicyTest.NamedSlotsNeverEvictEvenWithAnEvictingPolicy`。
2. **淘汰之后必须重新规划。** 被挤掉的可能正是本次打算并进去的未满堆(临时格里
   同 config 的旧堆恰恰最早进包),不重算就会拿着悬空的 entity 去 `ApplyStackFill`。
   `AddStackableItem` 与 `ReserveBatch` 各有一次重规划,两遍是确定上界不是循环。
3. **`CheckSpaceFor` 保持纯预测。** 它按"不破坏现状"回答,于是对会淘汰的包偏
   保守。真入包路径改走 `ReserveForBatchAdd` —— 否则纯预测会抢在淘汰之前把整批
   拒掉,先进先出在批量路径上永远不生效。这一条正是 `BagService` 那两处批量闸
   必须一起改的原因。用例 `CheckSpaceForStaysPureAndNeverEvicts`。

> 注意 `ItemComp`(`proto/common/component/item_base_comp.proto`)今天只有
> `item_id` / `config_id` / `size` **三个字段** —— split 文档里"proto 里已按已知
> 字段号预留了强化等级 / 词条 / 镶嵌"指的是 `ItemEntry`(持久化),不是 `ItemComp`
> (运行时)。FIFO 序要在运行时可读,**两边都得加**。

### 6.3 落码时自己踩出来的一个同族缺陷(已修)

加淘汰轴的当天,在同一份代码里又造出了一个**和 §6.1 同一家族**的问题:
**一个会失败的判断站错了位置。**

`ReserveOrEvict` 会为了腾位真的销毁实例,而它被放在了 `CanMintGuid` 这类
**零副作用预检的前面**。后果:发号器被 fence(跨服失租等)时,临时格已经挤掉
最早那件东西,然后整批拒绝 —— **玩家的资产白丢,而且这次连"放不下"都不是,
是根本不该开始**。

四处同病,全部已改为「纯预检 → reserve/腾位 → commit」:

| 位置 | 原来的顺序 | 改后 |
|---|---|---|
| `AddNonStackableItem` | reserve → 铸号预检 | 铸号预检 → reserve |
| `AddStackableItem` | plan → reserve → 铸号预检 | plan → 铸号预检 → reserve |
| `AddItems(ItemCountMap)` | reserve → 铸号预检 | `PlanInstances` → 铸号预检 → reserve |
| `AddItems(vector)` | reserve → guid 撞车预检 → 铸号预检 | `PlanInstances` → 两道预检 → reserve |

`AddStackableItem` 那处的铸号预检因此变得**略保守**(万一腾位后重新规划发现
全并得进去、根本不用铸号,也已经拒了)。这是刻意的取舍:**宁可少收一次,
不可错杀一件。**

判据升级成一条红线(§7 第 7 条),用例
`BagFencedGeneratorTest.EvictionNeverHappensBeforeAPureCheckCanRefuse`。

> 这条值得记住的原因不是"改了个 bug",而是:**引入第一根有副作用的轴,会把
> 整个模块里所有判断的先后顺序从风格问题变成正确性问题。** 三段式
> (`plan -> reserve -> commit`)此前之所以好写,是因为前两段都是纯的;淘汰打破
> 了这个前提,于是每一处 reserve 都要重新审一遍"我前面还有没有会失败的判断"。

### 6.4 节日背包 —— 已落码(用名单式准入,不等 tag 列)

`generated/code/proto/item_table.proto` 今天只有三列:

```proto
message ItemTable {
    uint32 id = 1;
    uint32 max_stack_size = 2;
    uint32 equip_kind = 3;
}
```

**没有任何分类 / 标签列。** 加列要改二进制 xlsx 源表 + 跑导表工具 —— 不是代码能
落地的事,而且据 `configtable-unversioned-source-xlsx` 那条记录,这些源表连 SVN
都没进,手改风险很高。

#### 落码采用的方案:名单式准入(`AcceptByConfigSet`)

**不等表列,今天就完整可用。** 名单由创建这个包的玩法(活动系统)在注册 profile
时给出 —— 那边本来就知道自己发哪些道具。

为什么不先把 `AcceptByTag` 写好占位:**没有 tag 列,它对所有东西都会读到
`tag == 0` → 什么都不收**,节日包直接变成黑洞,比没有更糟。等 tag 列真的落地,
再加一个约二十行的 `AcceptByTag` 与它并列即可 —— `IAdmissionPolicy` 一个字都不用
改,这正是把准入做成策略的意义。

#### profile 注册表:规则不进快照,进快照的只有一个 id

`BagProfileRegistry`(`bag_profile_registry.{h,cpp}`)把两件事分开:

| | 归属 | 去向 |
|---|---|---|
| "这个包是哪套规则" | **玩家数据** | `DynamicBagData.profile_id`,**必须持久化** |
| "那套规则具体是什么" | 代码 + 配置 | 注册表里的工厂,**绝不进快照**(§7 红线 5) |

`Make(profileId, capacity)` 查不到时 **fail-open**:退化成自由格 + 什么都收,
打 ERROR,但**保留 id** —— 活动下线 / profile 未注册 / 脏数据,都不能因此让玩家
的东西没地方放;保留 id 才能让活动重新上线后这个包自己变回去。口径与
`InsertItemForRestore` 一致:**位置可以变,规则可以退化,物品不能丢**。

`CfgBagProfile` 表仍是终局,但那时的改动**只在注册表内部**(`Make()` 改成先查表、
查不到再回退到已注册工厂),调用方一行都不用动。

#### 原设计里"先加列"的那段(保留作对照)

| 表 | 新增列 | 含义 |
|---|---|---|
| `CfgItem`(`data/Item.xlsx`) | `tag` / `category` / `activity_id` | 这件东西属于哪一类 |
| `CfgBagProfile`(新表) | `profile_id` / 布局类型 / 容量 / 接受哪些 tag / 淘汰策略 / 能否整理 / 过期时间 | 一个 profile 的全部规则 |

这与 `equip_kind` + `CfgEquipSlot` 是同一个套路:**规则由表数据定义,
C++ 里不写死任何一个 tag 值。** 这条在装备槽上已经做对了。

#### 载体用 `dynamicBags_`,但快照缺 `profile_id`

`comp/player_bags_comp.h` 的注释已经明确:节日包属于运行时 / 临时背包这一档,
**不要加进 `BagType` 枚举**(那是持久化契约,不能为活动增殖)。

**缺口(2026-09-02 已补)**:`BagAllData.DynamicBagData` 原先只带
`bag_id + capacity + items`,**没有 profile id** —— 规则一挂到包上,跨服回来就会
退化成默认自由包,规则静默消失。现已加 `profile_id = 4`,并在 `bag_marshal` 两侧
接好:Marshal 写 `bag.ProfileId()`;Unmarshal **先 `SetProfile(registry.Make(...))`
再放物品**(`SetProfile` 只接受空包,顺序反了规则装不上)。

---

### 6.5 第二轮审计(2026-09-02):8 视角独立审查 + 对抗证伪

当天代码零编译、零运行,又刚被同一个人复审过一遍 —— 盲区不变。于是换成
**8 个互不知情的审查视角**(三段纪律 / 淘汰 / 层不变量 / 编译级 / 测试有效性 /
编排与流水 / 还原路径 / 生命周期)各自读代码,36 条原始发现去重成 18 条,再对每条
派 2 个证伪者对着真实代码反驳。证伪阶段撞了会话限额(29 个证伪者未返回),
未被证伪的 13 条由人工逐条复核。**结果:18 条全部成立**(其中 1 条降为"存疑,
加防御")。

#### 6.5.1 P0(3 族 5 条)—— 全部是淘汰轴引入的,全部已修

| 族 | 缺陷 | 根因 | 修法 |
|---|---|---|---|
| **A** | 淘汰掉的正是本批要并入的同 config 未满堆 → 腾位后需求变大 → 已销毁却整批失败 | 缺口按**淘汰前**的规划算;`EvictOldestFirst` 不排除并入目标;第一版注释"不是数据能触发"是错的 —— 临时格拾取同种材料就是日常路径 | `ReserveOrEvict` 改为**先算到不动点再销毁**:把牺牲者当作已不存在重新规划(`PlanInstances` / `ItemStore` 两个规划函数新增 `exclude` 集),需求变大就多选牺牲者,稳定了才真 `DestroyItem`。收敛上界 `store_.Size()` 轮 |
| **B** | `BagService::AddItems` 自带一套批量循环,不经过 `Bag::AddItems`,于是 §6.3 挪到前面的预检在编排层根本没跑 —— fence / guid 撞车时先挤掉旧物再失败 | 预检写在 `Bag::AddItems` 里而不是 reserve 入口里,两套循环漂移 | 全部纯预检并进 `ReserveForBatchAdd`(两个重载),`Bag::AddItems` 与 `BagService::AddItems` 都只调它;vector 重载额外带预设 guid 撞车预检;fence 下**连腾位也不做**(腾位后重规划可能把纯并堆变成要铸号的溢出) |
| **C** | 单件沿用预设 guid 时,`store_.Contains` 这道零成本预检排在腾位之后 | 与 vector 批量入口不对称 | `AddNonStackableItem` 在 `ReserveOrEvict` 之前加 `Contains` 预检 |

#### 6.5.2 P1 / P2(8 条)—— 已修

| # | 缺陷 | 修法 |
|---|---|---|
| P1 | `count==0` 条目过了预检、逐件 `AddItem` 才拒,半批且取决于哈希序 | `ReserveForBatchAdd` 两个重载在最前面拒零数量(`CheckSpaceFor` 保持宽容,既有口径) |
| P2 | 槽位表出现重复 id 行时 `CountFreeSlotsForKind` 多数空槽,reserve 又比 commit 乐观 | 按物理槽位 id 去重 |
| P2 | `SetProfile` 非原子:非空包上 `SetLayout` 被拒、另外两根照换,得到"旧布局 + 新淘汰策略" | `SetProfile` 要求空包且三根轴齐全,否则整套不换 |
| P2 | `evictedOut == nullptr` 时照样销毁,回执无声丢弃 | **没有回执就不淘汰**(按 `RejectWhenFull` 处理)—— 从注释变成闸 |
| P2 | `SetCapacityForRestore` 可把容量压到已占槽位之下,Debug 断言 / Release 永远满 | 拒绝缩到 `OccupiedSlotCount()` 之下 |
| P2 | `ApplyStackFill` 对悬空 entity 零防御(entt Release 下 UB) | `registry_.valid()` 防御 + `LOG_ERROR`;真正的保证仍是"淘汰后必重规划" |
| P2 | 格子布局碎片时 `ReserveOrEvict` 把"空格够却摆不进"当编程错误吼 | 识别为"容量不是标量",按 `RejectWhenFull` 静默处理 |
| 文档 | `bag_service.h` 仍写"transactional / all-or-nothing space check" | 更正为实际口径:reserve 之后逐项不再失败,但临时格上 reserve 本身可能已销毁旧物 |

#### 6.5.3 覆盖缺口(3 条)—— 已补用例

- fence 顺序用例只走了不可叠加 + ItemCountMap → 补 `StackableSpillNeverEvictsWhenGeneratorIsFenced`、`VectorBatchNeverEvictsWhenGeneratorIsFenced`
- 准入轴全是 `AcceptAll` 走 true 分支,删掉两处 `Accepts` 判断也全绿 → 补 `RejectingAdmissionRefusesBeforeAnyWrite`(`RejectAllAdmission`)
- "被挤掉的实例必须落 `LogItemDestroy`"零用例 → 见 §6.5.4

新增回归:`EvictingTheMergeTargetStillLandsEverything`(族 A 核心,修复前必红)、
`EvictingTheMergeTargetInABatchStillLandsEverything`、`MergeTargetThatIsNotOldestSurvivesAndAbsorbs`
(对照:不能退化成一律按最坏情况多挤)、`PreassignedGuidCollisionRefusesBeforeEvicting`、
`ZeroCountEntryRefusesWholeBatchBeforeAnyWrite`、`NoReceiptSinkMeansNoEviction`、
`SetProfileOnANonEmptyBagChangesNothing`、`CapacityNeverShrinksBelowOccupiedSlots`。

#### 6.5.4 教训

1. **同一个人复审同一份代码,盲区不变。** §6.3 那次自查抓到了"预检站错位置",
   却没抓到"缺口按淘汰前算"—— 两者相隔十行。8 个视角里有 6 个各自独立报了后者。
2. **"不是数据能触发的情形"这句话每写一次都要给出触发不了的证明。** 第一版
   写了两处,两处都错。
3. **两套批量循环 = 两份预检 = 必然漂移。** 修法不是"记得两边同改",是把预检
   并进唯一的 reserve 入口,让漂移在结构上不可能。
4. **`sizeof(Bag)` 变了,scene 工程必须重编**(split 文档 §9.1 踩过同一个坑):
   `cross_zone_test` 链接 `modules.lib` + `scene.lib`,`bag_marshal.obj` 里
   `entt::basic_storage<PlayerBagsComp>` 的实例化步长是旧的。§8.2 已改。

## 7. 红线(别把已经挖开的缝焊回去)

1. **布局层永远不许认识 `config_id`。** split 文档 §2.2 的唯一判据。准入规则要查表
   → 它属于桥层。
2. **准入 / 流出必须零副作用**,才能放进 `reserve` 之前保住三段纪律。
3. **淘汰必须回执**,由 `BagService` 落流水。纯容器不写流水,这条已经定了。
4. **不要一个大 `IBagRule`。** 六个轴六个小接口,各自可单测、可组合。
5. **不要把策略对象序列化进快照。** 快照存 `profile_id`,还原时**重新装配**。
   策略是代码 + 配置,不是玩家数据。
6. **`BagType` 枚举不为活动增殖**,动态包走 `dynamicBags_`。
7. **所有零副作用的预检,必须排在唯一那步会改状态的操作之前。**
   淘汰进来之后,reserve 段不再是纯的了(`ReserveOrEvict` 会真销毁实例)。于是
   "判断的先后顺序"从风格问题变成了正确性问题:任何一个会失败的纯预检站在
   `ReserveOrEvict` 后面,都会造成**已经挤掉了玩家的东西、然后整批拒绝**。
   落码时在四处都踩了这一脚(见 §6.3),已全部修正并由用例
   `BagFencedGeneratorTest.EvictionNeverHappensBeforeAPureCheckCanRefuse` 钉住。
8. **入包严格、还原宽容。** split 文档 §11.2(d) 那条区别对准入规则同样适用:
   往节日包里塞非节日道具要拒;**但快照还原时不许因为规则不匹配就丢玩家东西**
   (表被裁过 / 活动已下线 / 脏数据都可能触发)。*位置可以变,物品不能丢。*

---

## 8. 执行清单(建议顺序)

| 步 | 做什么 | 状态 | 涉及 |
|---|---|---|---|
| 1 | 为 §6.1 的 `CanFit` 洞写回归用例 | ✅ 已落码,未编译 | `bag_test.cpp` 新增 `EquipmentReserveTest`(8 例)+ `BagProfileTest`(3 例) |
| 2 | 修 §6.1:把具名槽的批量 reserve 提到桥层 | ✅ 已落码,未编译 | `Bag::CanReserve` / `CountFreeSlotsForKind` / `IsFreeSlotForEquipKind` |
| 3 | 抽 `IAdmissionPolicy` + `BagProfile` 装配器 | ✅ 已落码,未编译 | 新增 `admission_policy.h`(仅头文件);`bag_system.{h,cpp}`、`player_bags_comp.h`、`modules.vcxproj` |
| 4 | ~~加 `CfgItem.tag` + `CfgBagProfile` 两张表~~ → **改用注册表 + 名单式准入** | ✅ 已落码(等价替代,§6.4);tag 列仍待日后 | 新增 `bag_profile_registry.{h,cpp}`、`AcceptByConfigSet` |
| 5 | 节日包:准入 + `DynamicBagData.profile_id` + 还原路径 | ✅ 已落码,未编译 | proto + `bag_marshal.cpp` + `BagProfile::Festival` |
| 6 | `IEvictionPolicy` + `EvictOldestFirst` + 流水回执 | ✅ 已落码,未编译 | 新增 `eviction_policy.{h,cpp}`;`bag_system.{h,cpp}`、`bag_service.{h,cpp}`、`player_bags_comp.h` |
| 6b | `ItemEntry`/`ItemComp` 加入包序号(**字段 13 / 4**),`AcquisitionOrderOf` 改读它 | ✅ 已落码,未编译 | 两个源 proto + `ItemStore::Insert` 盖章 + `bag_marshal` |
| 7 | `IWithdrawPolicy`(绑定 / 任务道具 / 活动结束) | ⬜ **等有真实使用者再做** | —— |

第 4~6 步都要改配置表或 proto,得跑导表工具 / `cd go && build.bat` 重生成,
所以它们不是"接着写代码"就能推进的,需要先安排那两条链路。

第 7 步的判据来自 `GridLayout` 自己的注释:

> 等真要做格子背包时再加,现在加就是没有使用者的臆测。

同样适用于规则轴 —— **没有使用者的策略接口不要提前造。**
(§5.1 里被推翻的 `AcceptsBatch` 就是这条判据的一次现场应用。)

### 8.1 第 1~3 步实际落码清单(2026-09-01)

| 文件 | 改动 |
|---|---|
| `cpp/libs/modules/bag/admission_policy.h` | **新增**(仅头文件,无 .cpp):`IAdmissionPolicy` + `AcceptAll` |
| `cpp/libs/modules/bag/bag_system.h` | `BagProfile` 结构 + `Flat()`/`Equipment()` 工厂;`SetAdmission`/`Admission()`/`SetProfile`;私有 `CountFreeSlotsForKind`/`CanReserve`;成员 `admission_`;顶部三层说明;更正 `EquipKindFor` 的陈旧注释(`equip_slot` -> `equip_kind`) |
| `cpp/libs/modules/bag/bag_system.cpp` | `SetAdmission`/`SetProfile` 实现;三处 reserve 改走 `CanReserve`;`AddItem`/`CheckSpaceFor` 加 `Accepts` 门;抽 `IsFreeSlotForEquipKind`;新增 `CountFreeSlotsForKind`/`CanReserve` |
| `cpp/libs/modules/bag/container_layout.h` | `CanFit` 与 `FixedSlotLayout` 各加一段"为什么这里答不完整、正确答案在桥层" |
| `cpp/libs/modules/bag/comp/player_bags_comp.h` | 四个包改走 `SetProfile`;更正"槽位口径还不在配置里"的陈旧注释 |
| `cpp/libs/modules/modules.vcxproj` | 注册 `bag\admission_policy.h`(`CMakeLists.txt` 只列 .cpp,无需改) |
| `cpp/tests/bag_test/bag_test.cpp` | 新增 `EquipmentReserveTest` × 8、`BagProfileTest` × 4 |

### 8.1b 第 6 步落码清单(2026-09-01,同日)

| 文件 | 改动 |
|---|---|
| `cpp/libs/modules/bag/eviction_policy.{h,cpp}` | **新增**:`IEvictionPolicy` + `RejectWhenFull`(inline)+ `EvictOldestFirst`;排序依据收在 `AcquisitionOrderOf()` |
| `cpp/libs/modules/bag/bag_system.h` | `BagProfile` 加 `eviction` 字段 + `Temporary()` 工厂;`SetEviction`/`Eviction()`;`ReserveForBatchAdd`(公开);私有 `PlanInstances`/`ReserveBatch`/`ReserveOrEvict`;`AddItem`/`AddItems` 加可选 `evictedOut` |
| `cpp/libs/modules/bag/bag_system.cpp` | `CheckSpaceFor` 拆成 `PlanInstances` + `CanReserve`(保持纯);三条入包路径改走 `ReserveOrEvict`/`ReserveBatch`;淘汰后重规划 |
| `cpp/libs/modules/bag/bag_service.{h,cpp}` | `LogEvictedInstances`;三处 `AddItem` 收回执并落流水;两处批量闸从 `CheckSpaceFor` 换成 `ReserveForBatchAdd` |
| `cpp/libs/modules/bag/comp/player_bags_comp.h` | `kTemporary` 改用 `BagProfile::Temporary` |
| `modules.vcxproj` / `CMakeLists.txt` | 注册 `eviction_policy.{h,cpp}`(这次有 .cpp,两个文件都要改) |
| `cpp/tests/bag_test/bag_test.cpp` | 新增 `EvictionPolicyTest` × 8 |

### 8.1c 行为变化(两条)

1. **具名槽布局下,放不下的批次现在在写入之前就被拒**(此前会写进去一部分再
   返回失败)。自由格布局逐字不变。
2. **临时格(`kTemporary`)满了会挤掉最早进包的实例**,而不是拒绝入包。这是
   本次唯一一条 gameplay 语义变更 —— 它正是"临时背包先进先出"这个需求本身。
   被挤掉的实例逐条落 `LogItemDestroy`,不会无声消失。
   其余三个包(人物背包 / 仓库 / 装备栏)行为不变:满了就拒。

错误码不变,线上协议与持久化格式不变,公开 API **只增不改**
(新增的都是带默认值的可选参数与新方法)。

### 8.2 编译(交 Codex)

按 `CLAUDE.md` §10.1:Claude 不执行编译。第 1~3 步已落码,**未编译**。

- **先重生成 proto**(本轮改了三个源 proto):按 `CLAUDE.md` §4,`cd go && build.bat`,
  再重编受影响的 C++ / Go / Java。新增字段:`ItemComp.acquire_seq=4`、
  `ItemEntry.acquire_seq=13`、`BagAllData.DynamicBagData.profile_id=4`。
  **都是新增、不复用任何已用字段号**,线上兼容(旧存档读到 0,行为与落地前一致)。
- **目标工程(顺序不能乱)**:`cpp/libs/modules`(modules)→
  **`cpp/libs/services/scene`(scene)** → `cpp/tests/bag_test` → `cpp/tests/cross_zone_test`。
  C++ MSBuild **必须串行** `/m:1`(并发会报假的 C1041/LNK1104)。
  **scene 不能漏**:`Bag` 新增了 `admission_` / `eviction_` 两个 `unique_ptr`,
  `sizeof(Bag)` 与 `PlayerBagsComp` 步长都变了;`cross_zone_test` 链接
  `modules.lib` + `scene.lib`,而 `bag_marshal.obj` 里 `entt::basic_storage<PlayerBagsComp>`
  的模板实例化带着旧步长 —— 只重编 modules 会复现 split 文档 §9.1 记录的
  `is_power_of_two` 断言 / `emplace` 读 `0xFFFF…` 访问违例。
- **新文件**:`cpp/libs/modules/bag/admission_policy.h` 已注册进 `modules.vcxproj`
  的 `ClInclude`。它是**纯头文件,没有 .cpp**,所以 `CMakeLists.txt` 的
  `SOURCE_FILES`(只列 .cpp)无需改动 —— 若 Linux 侧报"找不到符号",先确认这一点。
- **通过标准**:两个工程编译零错误;`bag_test` 全绿。新增用例
  `EquipmentReserveTest.*`(8 例)与 `BagProfileTest.*`(3 例)必须全过 ——
  其中 `ThreeOfOneKindLeaveNothingBehindWhenRefused` 是这次修复的核心判据
  (修复前它会红:返回失败但 `OccupiedGridCount()` 是 2 而不是 0)。
- **回归重点**:`FixedSlotLayoutTest.*`、`BagBatchAtomicityTest.*`、
  `PlayerBagsCompTest.*` 三套原有用例的行为**不应改变**。
  另外 `cpp/tests/cross_zone_test` 也要跑一遍:它往装备栏还原的是物品表里
  **不存在**的 config(3001/3002),走的是还原路径(不经 `CanReserve`),
  正好验证"入包严格、还原宽容"没有被这次改动破坏。
- **失败时保留**:完整编译错误输出;`bag_test` 失败用例的 gtest 输出全文。

---

## 关联

- [bag-instance-layout-split.md](bag-instance-layout-split.md) —— 上一刀(横切):
  `Bag` -> `ItemStore` + `IContainerLayout`。本文档是它 §11 遗留项的延续
- [bag-service-srp-refactor.md](bag-service-srp-refactor.md) —— 更早一刀(纵切):
  `Bag` -> `BagService`
- [cross-zone-readiness-audit.md](cross-zone-readiness-audit.md) §3.2 件 1 ——
  背包跨服迁移;§6.4 的 profile 持久化缺口挂在这里
- `proto/common/database/bag_quest_mail_data.proto` —— `ItemEntry` / `BagAllData` /
  `DynamicBagData` 线上格式
- `generated/code/proto/item_table.proto` / `equipslot_table.proto` —— 现有表列
