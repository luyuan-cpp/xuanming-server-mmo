# tip 码轴：段是发号器的输入

**状态**：已完成。Codex 完成 35.1 全量导表 + 三语言编译；本机完成二次复核并修掉 4 项遗留，导表器 73 passed、Go 全绿。证据见 §6 与 §6.2。
2026-09-05 追加：故障分类收口到 `Tip.xlsx` 的 `fault` 列（§2.9），导表器 129 passed、Go 全绿，证据见 §6.3
**日期**：2026-09-03（§2.9 / §6.3 为 2026-09-05）
**影响面**：`data/tip/Tip.xlsx`、4 张内嵌 tip ID 的业务表、导表器、三语言生成产物、`go/shared/serverbase`、guild / friend / match 的错误码常量

---

## 1. 问题不是「没分段」，是分段和发号是两拨人

`TipInfoMessage.id` 是**客户端可见的契约**：客户端拿这个数字去查文案。全仓只有一条数轴，所有域共用。改造前，这条轴上有**三拨人各自发号，互相不知道对方存在**：

| | 在哪 | 干什么 | 知道别人吗 |
|---|---|---|---|
| 发号的 | `tools/data_table_exporter/core/generators/enum_gen.py` | 读 Tip.xlsx，给每个名字分配数字 | **不知道「段」是什么**。全文 grep `range/segment/base/domain/lo/hi/overlap` 只命中两处 Python 内置的 `range()` |
| 分段的 | `go/shared/serverbase/tipcode.go` 的 `tipDomains` | 一张 `{域名, 起, 止}` 表 | **不知道号是谁发的**。grep `enum_gen / tip_enum_ids / xlsx` 零命中。它是人照着生成结果**手抄**的 |
| 各自发号的 | 6 个服务的 `constants.go` | 自己手写常量 | guild / friend 知道有这条轴（专门写了 `TipClassifier` 绕开它）；**match 零引用，完全不知道** |

### 后果一：加一个码就破段

改造前 13 个组紧挨着排满 `1..129`，**零余量**。发号规则是：

```python
global_id = max(所有组的所有号) + 1     # 一个跨所有组的全局计数器
```

于是往 `common` 组（段 1-19）加第 20 个码，它拿到的号是 **130**——落在 `cross_server` 后面，不在 common 段里。而 `tipDomains` 里 common 仍写着 `{1, 19}`，没人会发现；运行时客户端收到 130，`TipVerdict` 判 `130 > TipMaxKnownCode(129)` → 返回「未知码」。**全程零报错零告警。**

段只在「已经发出去的这 129 个号」上成立，再加一个就破。它是**事后描述**，不是分配规则。

### 后果二：约定拦不住新来的

`go/match` 从 1 开始重新数了一遍：`ErrInBattle=1` 压在 common 段上，`ErrChallengeSelf=20` 压在 login 段上。共 20 个码全部撞号。没人告诉过它有这条轴，也没有任何机制会告诉它。

guild / friend 早先撞过同样的坑（`ErrGuildNotFound=2` vs `kInvalidTableId=2`），当时的修法是手写一个「私有段」约定 + 手抄的镜像表 + 各自的 `TipClassifier`。**约定不是机制**：违反它不会报错，只会在几个月后以「客户端弹错文案」的形式冒出来。

### 后果三：文案通道从来没通过

`Tip.xlsx` 是两列：A 列码名、B 列中文文案。但 `enum_gen` **只读 A 列**，B 列从来没被任何东西读过，`generated/tables/` 下也没有任何 tip 产物。所以「客户端按 id 查提示表」这条链**本来就是断的**——不是私有码特有的问题，是所有码的问题。129 个码里只有 50 个填了文案。

---

## 2. 改成什么

**段是发号器的输入，不是事后校验的对象。**

### 2.1 段声明进 Tip.xlsx 的组头行

```
//common_error base=1000 width=1000
    Success              成功
    InvalidTableId       表id无效
//login_error  base=2000 width=1000
    ...
```

组头 vs 注释靠「`//` 后面有没有空格」区分：`//common_error ...` 是组头（紧贴），`// 这是一句说明` 是注释（有空格）。不能用「有没有 `base=`」来分——那样旧格式的 `//common_error` 会被当成注释静默跳过，它下面的码全部落进上一组，恰好是这次要消灭的那类静默错误。

### 2.2 发号按段进行

`_assign_tip_ids` 对每个组：已发过的号原样保留，新名字取**本段内最小空位**；段满即 `SystemExit` 并点名是哪个组、哪个码。

### 2.3 state 只增不减

`tip_enum_ids.json` 升到 v2，结构带上 `base/width`：

```json
{
  "schema_version": 2,
  "groups": { "common_error": { "base": 1000, "width": 1000, "ids": { "Success": 1000 } } },
  "legacy_ids": { "1": { "group": "common_error", "name": "Success", "new": 1000 } }
}
```

从 xlsx 删掉一行**不回收它的号**（留在 state 里当墓碑），避免号被复用后老客户端把新含义当旧含义。改造前 `_save_json` 用当前 xlsx 内容整体重写 state，删一行号就没了——`kSceneTransferFailed=130` 就是这么被抹掉的。

整组也不能静默消失：停用一组时必须保留空组头，让段墓碑继续占位；组头被删除或改名而 state 仍有旧组时，导表直接中止。已有 state 若损坏、不是合法 JSON 或无法读取也一律 fail-closed，不能把它当首次运行的空状态重新发号。

state 整个缺失同样默认中止；只有确认码轴从未发布过，才允许显式传
`--tip-axis-bootstrap` 从空初始化。单看当前 JSON 无法发现有人删过墓碑，
所以 CI 还会把 state 与 PR base / push 前一提交比较：已有名字→ID 与
`legacy_ids` 不得删除或改写，已有段不得缩小或平移，只允许增加名字、
新开组或向外扩段。

v1 格式一律拒绝并给出迁移指引，不静默沿用。

### 2.4 生成期自检

- 段两两不重叠
- 每个码都在自己段内（兜住手改 state 的情况）
- **枚举名全局唯一**——生成的 tip proto **没有 package 声明**，protobuf 的 enum 值是 enum 的兄弟而非子成员，所有 tip proto 的枚举值处在同一命名空间，重名会在 protoc 阶段炸，这里提前拦
- 码值全局唯一
- `Tip.xlsx` / state 缺失、损坏或不可读均 fail-closed
- 普通业务表中名字含 `tip` 的整数列，以及显式标了 `tip_ref` 的列，只能引用 0 或 Tip.xlsx 当前仍活动的码；state 墓碑只防复用，不能继续被引用

protoc 与部署也属于同一批事务的闸门：任一编译或任一部署目标失败，导表器
必须以非 0 退出，不再打印 `DONE`。输入 xlsx 解析失败、并行 JSON/proto/PB
生成失败、合法空表和部署源缺失也都不得静默沿用旧文件。C++ / Go / Java 的
根 proto 只编译当前目录，tip/operator 各编译一次；Python 全树只编译一次，
避免 Windows 上同一输出文件被连续覆盖造成半套产物。PB 使用 deterministic
序列化，保证 Windows 本地与 Linux CI 的 map 字段产物可逐字节比较。

Java protobuf 三批输入先全部编译到同一个空 staging 目录，只有
protoc 全部成功且当前输出根没有已删 message 的孤儿类时才同步。
Java Manager/Comp 的生成源文件集也由当前 TableSchema 反推校验；部署目标
中「源树已无、目标仍在」的编译单元从告警升级为失败。三道闸门
都不自动删除文件，避免覆盖并行工作；须审计报告的确切路径后再清理。

### 2.5 镜像改成生成的

`tipcode.go` 里手抄的 `tipDomains` 与 `TipMaxKnownCode` 删除，改为消费生成产物 `shared/generated/tip`（`Segments` / `DomainOf` / `InKnownSegment` / `InAllocatedRange`）。

「未知码」的判据随之细化：段之间有空隙，所以不能再用「大于某个最大值」。现在是 `InAllocatedRange`——落在所有段之外，**或者高于本二进制编译时该段的已分配上界**（说明对端跑的是更新的码表），才算码表漂移。段内低于上界的空洞按业务拒绝处理：域是确定的，只是具体含义未知。

### 2.6 文案有了出口

新增产物 `generated/tables/tip_text.json`（`{"1000": "成功", ...}`）。没填文案的码不编造，导表时按数量告警并列出前 20 个。

### 2.7 「Go 私有段」这个概念删除

guild（9 个）、friend（7 个）、match（20 个）共 36 个码作为普通组进 Tip.xlsx，白拿中文文案和机械保护。三个服务的常量改成引用生成枚举：

```go
ErrAlreadyInGuild = uint32(table.GuildError_kGuildAlreadyInGuild)
```

**符号名一个都没改**，所以这三个服务的 79 处源码调用点一处不动。

### 2.8 普通表里的数字引用也纳入码轴

复核真实消费点后发现，“调用点都用符号”只覆盖源码，并不覆盖表数据：
`SkillPermission.skill_type`、`ActorActionState.state_tip`、
`ActorActionCombatState.state_tip` 与 `MessageLimiter.tip_message` 共 20 个单元格
仍写着旧 `kSuccess=1`。这些值会被 C++ 直接返回，编译完全发现不了错轴。

四张表已迁到 `kSuccess=1000`。字段名能表达语义的 `*_tip` 自动识别；
`SkillPermission.skill_type` 与 `MessageLimiter.tip_message` 在 options 行显式标
`tip_ref`。导表器在任何产物落盘前扫描全部业务表，本批实际检查 46 个非空
引用。CI 还会重建并比较全部表的 JSON、PB 与 manifest，防止只改 xlsx 却漏交
运行时数据产物，或者从一个合法码改到另一个合法码后仍部署旧 PB。

### 2.9 故障分类也在表里（2026-09-05）

「这个码算不算服务端内部故障」是码的属性。它决定 `serverbase.UnaryInterceptor` 对一个 in-band 失败是**打 Error 日志 + 计数 + 告警**（故障），还是**只计数**（业务拒绝）。此前它散在两处手写：`serverbase/tipcode.go` 的 `tipFaultCodes`（33 条）与 guild 的本地 `faultCodes`（1 条）——加码的人不知道要去另一个文件登记，与 §1 的「段和发号分家」是同一类病。（match 的 `kMatchInternal` 两张表都没登记；本次复核后**刻意仍不标**，原因见下面的清单。）

现在 `Tip.xlsx` 多一列 **`fault`**：

```
name                      commen          fault
//common_error base=1000 width=1000
Success                   成功
ServiceUnavailable        服务不可用      1
```

- **按第 1 行表头名定位，不按列位**。策划在中间插列不会改变它的含义。
- **表头必须存在**。列被删/改名时导表器中止，而不是静默变成「没有任何码是故障」——那会让所有 in-band 故障告警一起消失且零报错。
- **取值是闭集**：`1 / true / yes / 是` 为故障，空 / `0 / false / no / 否` 为不是，其他任何值（`TODO`、`故障`、`2`）都点名单元格中止。不猜：把一个业务拒绝误判成故障会刷出满屏假告警，比漏报更糟。
- 组头行 / 注释行 / 没有码名的行标了 fault 也中止——多半是行错位。
- **不进 state**：fault 是分类不是号，翻转一个码的分类不需要任何迁移。
- **墓碑不进故障表**：已删行只占号，没有定义也就没有分类。

产物是 Go 侧的 `shared/generated/tip/faults.go`（`tip.Faults` 切片 + `tip.IsFault`），与段表同一目录同一部署链。`serverbase.TipVerdict` 的顺序是：`0/kSuccess` → OK；`!InAllocatedRange` → Unknown；`IsFault` → Fault；否则 BizReject。手写的 `tipFaultCodes` 与 guild 的 `faultCodes` 已删除；guild / friend 的 `TipClassifier()` 都直接返回 `serverbase.TipVerdict`，只是本服务固定下来的注入接缝，**不要再在服务里包一层本地 map**——那就是把分家重新制造出来。要改分类，改表。

只生成 Go 产物：只有 Go 的 `serverbase` 按故障 / 拒绝定性；C++ 的 `PlayerTipSystem` 只把 tipId 发给客户端，`tip_text.json` 是客户端契约，都不需要这个属性。所以本次**没有**动三语言 proto 产物。

**判定原则**（改 `fault` 列时照着这条线，别凭感觉）：

| | 例 |
|---|---|
| **是故障**：码明确指向服务端自身或其依赖出错 | 存储 / 序列化失败、状态机走死、超时、服务端会话 / 组件 / 场景状态缺失、无可用节点、发号器被 fence |
| **不是故障**：客户端传错参数、协议层拒绝、一切游戏规则拒绝 | 满、重复、冷却、权限、已领取、限流、功能未开放。**容量满是规则拒绝** |

拿不准的一律**不标**。当前 34 个故障码（= 原 `tipFaultCodes` 33 + guild 本地 map 1，逐符号对照过，无漏迁无误标），按组：

| 组 | 故障码 |
|---|---|
| common（9） | InvalidTableData, ServiceUnavailable, EntityIsNull, IndexOutOfRange, ThisEntityIsInvalid, SessionNotFound, PlayerNotFoundInSession, ResponseMessageParseError, FailedToRegisterTheNode |
| login（14） | LoginUnknownError, LoginSessionIdNotFound, LoginSessionNotFound, LoginFsmFailed, LoginFSMLoadFailed, LoginFSMEventFailed, LoginFsmInvalidEvent, LoginDataSerializeFailed, LoginDataParseFailed, LoginRedisError, LoginRedisSetFailed, LoginAccountDataLoadFaile（拼写错的历史码，数值真实存在）, LoginAccountDataLoadFailed, LoginTimeout |
| scene（7） | EnterNodeUnavailable, EnterSceneGsInfoNull, EnterSceneYourSceneIsNull, ChangeScenePlayerQueueNotFound, ChangeScenePlayerQueueComponentGsNull, ChangeScenePlayerQueueComponentEmpty, EnterSceneFailed |
| mission / bag / entity（各 1） | PlayerMissionComponentNotFound, BagAddItemHasNotBaseComponent, EntityTransformNotFound |
| guild（1） | GuildIdGenUnavailable（原 guild 本地 map 迁入） |

**刻意不算故障**的，记在这里免得下个人反复纠结：

- common：`InvalidTableId / InvalidParameter`（请求参数不合法）、`FeatureUnavailable`（功能未开放，产品行为）、`RateLimitExceeded`（限流是保护生效）、`MessageSizeExceeded / MessageIdNotFound / RequestMessageParseError / ArraySizeTooLargeInMessage / NegativeValueInMessage`（全是客户端上行侧的问题）
- login：`LoginAccountNotFound / LoginAccountPlayerFull / LoginInProgress / LoginEnteringGame / LoginPlaying / TooManyDevices / LoginBeKickByAnOtherAccount / LoginSessionDisconnect`（正常的登录态与业务规则）
- scene：`EnterSceneSceneFull / EnterSceneMainFull / EnterSceneGsFull / ChangeScenePlayerQueueFull`（容量拒绝）、`EnterSceneYouInCurrentScene / EnterSceneChangingScene`（状态拒绝）、`InvalidEnterSceneParameters / EnterSceneParamError`（参数问题）
- attribute（25000 段，2026-09-05 由他人新增的 15 个码）：全部按名字是规则拒绝（池未解锁、点数不足、冷却、金币不足……）；`AttributePoolNotFound / DimensionNotFound / SchemeNotFound` 既可能是配置缺失也可能是客户端传错 id，拿不准，不标
- match：`MatchInternal` **拿不准，不标**。它的注释说是 Redis / snowflake 内部错误，但 match 同时把它当「缺少玩家身份」的参数校验出口（`joinqueuelogic.go` 的 `playerId == 0` 分支，`challengelogic.go` / `watchbattlelogic.go` 同形），一码两用；标了会把参数拒绝刷成故障告警。要标，先拆成两个码。另外两件事接手人必须知道：**match 至今没挂 `serverbase.UnaryInterceptor`**（`match_service.go` 只挂了 session 与 grpcstats 两层），所以 match 的任何 tip 码目前都没有运行时的故障定性消费者，标不标都不会产生一条 `rpc_inband_fault`；而且 `JoinQueueResponse` 同时带 `error_code` 与 `TipInfoMessage`，拦截器对非 0 `error_code` 走 `ErrorCodeClassifier`（见 `bizcode.go`），接拦截器时要把 `ErrorCodeClassifier` 也指到 tip 轴上，否则 fault 列对 JoinQueue 仍不生效

---

## 3. 一次性重排

现有 13 组紧挨着排满、零余量，不重排就等于「老组永远加不了码」。既然 Tip.xlsx 可以完全改动、而且文案通道本来就没通（说明这套东西还没真正被客户端依赖），现在是最便宜的时候。

| | 旧 | 新 |
|---|---|---|
| common | 1-19 | 1000-1018 |
| login | 20-51 | 2000-2031 |
| scene | 52-75 | 3000-3023 |
| team | 76-93 | 4000-4017 |
| mission | 94-100 | 5000-5006 |
| bag | 101-115 | 6000-6014 |
| skill | 116-122 | 7000-7006 |
| buff | 123-124 | 8000-8001 |
| entity / actor_action / mount / reward / cross_server | 125-129 | 9000 / 10000 / 11000 / 12000 / 13000 |
| **guild**（原手写私有段 200-208） | — | 14000-14008 |
| **friend**（原手写私有段 220-226） | — | 15000-15006 |
| **match**（原从 1 开始撞号） | — | 16000-16019 |

每组段宽 1000，当前余量 ≈980。`17000` 起为移植域预留（mail / chat / auction / trade / rank / dialogue / battle_result / grant），在 Tip.xlsx 末尾以注释占位。

**重排对源码调用点是符号安全的**：源码调用点引用的是 `kSuccess` 这样的符号而不是字面量；表内的 20 个数字引用则已显式迁移并加生成前闸门。三语言产物一起重新生成，旧号→新号的 129 条映射记在 state 的 `legacy_ids` 里，供排查历史日志。

但 `legacy_ids` **不是运行时翻译层**。如果旧数字已经进入已发布客户端、持久化数据、
队列消息或跨版本 RPC，单靠符号重编译并不兼容；必须协调客户端与全部生产者/消费者
同批升级，不能让旧轴与新轴滚动混跑。本次重排成立的发布前提是旧数字尚未形成这类
外部兼容承诺。客户端目录按仓库规则未由本次服务器侧变更读取，因此发布负责人必须
在上线前单独确认该前提。

---

## 4. 怎么加一个新码

1. 在 `data/tip/Tip.xlsx` 的对应 `//xxx_error` 组下加一行：A 列码名（**全局唯一**，建议带域前缀）、B 列中文文案；若它是**服务端内部故障**（依赖挂了 / 状态缺失 / 序列化 / 超时，见 §2.9 的判定原则），`fault` 列填 `1`，否则留空
2. 跑导表器
3. 在服务的 `constants.go` 里加一行 `ErrX = uint32(table.XxxError_kXxxX)`。**不要**在服务里再写 fault map——分类由 `fault` 列生成到 `tip.Faults`，`serverbase.TipVerdict` 直接认得

**不要手写数字。** 三个服务的常量测试会逐个检查 `Err*` 右值，只接受
`uint32(table.<本域>Error_k...)`，并要求扫描到的常量集合与测试清单完全一致；
手写数字、包一层转换、别名或引用其他码轴都会失败。

新开一个域：在 Tip.xlsx 加一行组头 `//mail_error base=17000 width=1000`，其余同上。新组会生成新的 `.proto` 文件，需要同步 `cpp/generated/table/CMakeLists.txt` 与 `table.vcxproj`。

---

## 5. 已知残留

按「不假装做完」的口径列出来：

1. ~~**故障分类还是散的。**~~ **已收口（2026-09-05，见 §2.9）**：`Tip.xlsx` 加了 `fault` 列，`serverbase.tipFaultCodes`（33 条手写）与 guild 本地 map 删除，改消费生成产物 `tip.Faults`（34 条 = 33 + 1，迁移保真）。`kMatchInternal` 一码两用，刻意不标（§2.9）。只动了 Go 产物，没动三语言 proto。**仍散着的是消费侧**：match 没挂 in-band 拦截器，它的码没有运行时定性消费者（§2.9 末条）。
2. **79 个码没有中文文案**（165 个里有 86 个有）。导表时会告警列出。这是策划的活——直接打开 `Tip.xlsx` 填 B 列的空格即可，代码侧通道已经通了。按组分布：

   | 组 | 缺 / 共 | 例 |
   |---|---|---|
   | login_error | 18 / 32 | LoginSessionDisconnect … |
   | scene_error | 20 / 24 | EnterSceneSceneFull … |
   | team_error | **18 / 18** | TeamMembersFull … |
   | mission_error | **7 / 7** | MissionAlreadyCompleted … |
   | bag_error | **15 / 15** | BagAddItemBagFull … |
   | cross_server_error | **1 / 1** | SceneTransferInProgress |

   注意 team / mission / bag / cross_server 四组是**整组都没有文案**——这几个域的错误今天在客户端一个字都显示不出来。本次新加的 guild / friend / match 三组文案已写全。
3. **C++ 侧没有段表。**`tip.Segments` 只生成了 Go 版，因为只有 Go 的 `serverbase` 需要按段定性；C++ 的 `PlayerTipSystem` 只负责把 tipId 发给客户端，不做分类。真需要时照 `tip_segments.go.j2` 加一个 C++ 模板即可。
4. **客户端消费 `tip_text.json` 的那一半没做。**产物已经出了，客户端仓怎么读、什么时候读，属于客户端排期。
5. **`data_service` 与 `scene_manager` 有各自独立的 `error_code` 轴**（0..17 / 0..16），不在本次范围。它们不经 `TipInfoMessage`，与本轴无关，但同样是「三套码表并存」的一部分，见 `serverbase/doc.go`。
6. **导表器下有 3 份 v1 旧状态文件副本，全是陷阱。**（本条初稿只写了 1 份，是漏查——`find . -name tip_enum_ids.json` 实际 4 份命中。）

   | 路径 | 格式 | 说明 |
   |---|---|---|
   | `state/mapping/tip_enum_ids/tip_enum_ids.json` | **v2** | **唯一活的**（配置 `state_dir: ./state`） |
   | `mapping/tip_enum_ids/tip_enum_ids.json` | v1 | `core/_old/` 旧实现遗留，仍被 git 跟踪，2026-06-03 后没动过 |
   | `core/mapping/tip_enum_ids/tip_enum_ids.json` | v1 | 同上 |
   | `core/state/mapping/tip_enum_ids/tip_enum_ids.json` | v1 | 同上 |

   `core/_old/` 已确认零 import（死代码）。三份 v1 副本里的号还是旧的 `1..129`，
   下一个人很可能改错文件、然后疑惑为什么不生效——或者更糟，照着旧号去排查线上问题。
   建议连同 `core/_old/`、`templates/_old/` 一起清理；删的是被跟踪的文件，留给你拍板。
   （`mapping/table_index_mapping/` 下两份与 state 里内容相同，同理。）
7. **`tip_text.json` 不在运行时批次清单里。**`generate_manifest` 按 `tables` 列表遍历、不扫目录，所以只覆盖表的 json/pb 产物——`generated/code/**` 下的三语言代码与 tip proto 本来也都不在。CI 已通过重新生成 + 工作区漂移检查覆盖 state、tip proto、段表、文案表及全部业务表 JSON/PB/manifest，并静态核对 Go/C++/Java protobuf 枚举、Java 部署副本及 C++ 工程 wiring；但运行时若也要校验文案表批次完整性，仍需给 manifest 增加 extras 概念。

---

## 6. 验证记录（Codex）

2026-09-03 在 Windows live checkout 依次执行；导表器使用
`protoc 35.1`、Python 3.14.7 / protobuf 7.36.0，C++ 使用 VS 18 MSBuild
`Debug|x64` 且始终 `/m:1`。

- 导表器：`pytest tests/ -q` 为 **73 passed**。覆盖分段发号、墓碑、
  真表 tip 引用、异常传播、deterministic PB、Java staging 及生成源/部署
  孤儿 fail-closed。
- 全量导表：exit 0；16 组 / 165 码自检通过，扫描 46 个业务表内
  tip 引用，三语言 protobuf 生成成功，`Deploy: 10 OK, 0 failed`。
  同一受控产物集再跑一次：**1158 个文件，added/deleted/changed 均为 0**。
- Go：`shared`、`guild`、`friend`、`match` 均 `go build ./...` exit 0；
  `serverbase` 及三个服务的 constants 测试均 exit 0。
- C++：`game.sln` 全解决方案串行构建 exit 0；`table.vcxproj` Rebuild
  exit 0，新增 guild / friend / match 三份 `pb.cc` 已编译进 `table.lib`。
  `turn_battle_engine_test` 运行 **24/24 PASS**，`configuration_table_test` 读取真实
  PB 运行 **37/37 PASS**。
- Java：`config_node/mvnw.cmd -B test` exit 0，编译 196 个源文件，
  `BUILD SUCCESS`。该工程当前没有 Java 测试（Surefire: `No tests to run`），
  因此这条只是编译门禁证据。
- CI 定义：YAML 解析通过；真实 Tip/业务表闸门与 16 组三语言
  protobuf/C++ wiring 脚本均在本地按 workflow 正文执行通过；workflow
  还会直接编译 Java `config_node` 消费端。

未计为 PASS 的边界：本机缺 `no_raw_ptr_check.exe` / `clang-query`，所以 C++
`no-raw-pointer-member` 检查显示 **SKIP**；本次遵守仓库规则没有读取或运行客户端，
发布前仍必须按 §3 确认旧码未形成外部兼容承诺。

### 6.1 复核发现的遗留项与处置（2026-09-03）

产物对拍与工程 wiring 复核又查出 4 项，其中 2 项是本次改造自己的漏项：

| # | 项 | 性质 | 处置 |
|---|---|---|---|
| 1 | `table.vcxproj.filters` 漏了 friend/guild/match 六项 | **本次漏项**：只改了 `table.vcxproj`（编译清单），忘了配套的 `.filters`（VS 分组视图）。不影响编译，但新文件在 VS 里会散落在工程根下 | **已补**。并做了三方对拍：CMakeLists 16 项 / vcxproj 16+16 / filters 16+16，集合完全一致，且每个列出的文件都存在；两份 XML 经解析器验证良构 |
| 2 | 文档只登记 1 份旧 v1 state，实际 3 份 | **本次漏项**：只 `ls` 了一层就下结论 | **已改**，见 §5 第 6 条的完整表格 |
| 3 | `generated/code/cpp/constants/globalvariable_table_id_constants.h` 陈旧 | **不是本次回归**：来自 2026-03-14 的提交，工作区未修改。它是旧生成器行为的遗留——当年会给没有 `constants_name` 的行按 id 兜底生成 `kGlobalVariable_2..16`，现在只生成具名的 `kGlobalVariable_Abnormal_logout`。两者符号不重叠，不是重定义冲突，纯粹是死文件 | **已删**（源树 + 部署树两份），并清掉工程引用。见下方 §6.2 的证明 |
| 4 | 本机缺 pytest，73 条导表器测试无法重跑 | 环境缺失，非代码问题 | **已解决**：建隔离 venv 装 `requirements-dev.txt`，**73 passed**。刻意不装进 `D:/luyuan/Pandora-Server/run/localinfra` 那套 3.14t —— 那是另一个项目的运行时 |

### 6.2 二次复核（本机自行执行，2026-09-03）

Codex 额度耗尽后由本机接手完成，工具链：隔离 venv（pytest 9.1.1 / openpyxl 3.1.5 /
jinja2 3.1.6 / protobuf 7.36.1）、go1.26.5。

**修掉的 4 项**

1. **`table.vcxproj.filters` 补齐 friend/guild/match 六项。** 三方清单对拍：
   `CMakeLists.txt` 16 / `table.vcxproj` 16 头 + 16 源 / `.filters` 16 头 + 16 源，
   集合完全一致，每个列出的文件都存在，两份 XML 经解析器验证良构。
2. **删除陈旧产物 `globalvariable_table_id_constants.h`**（源树与部署树各一份）。
   **证明**：单独运行 `generate_constants`（纯 Python，不需要 protoc）到临时目录，
   当前生成器对 C++ / Go **各产出 3 个文件**，均不含它；且两语言文件数从此一致
   —— 此前 C++ 4 个 / Go 3 个的不对称，差的正好就是这个孤儿。
3. **清掉 10 条悬空工程引用**（此前就存在，与 tip 无关）：
   `cpp_table_id_constants_name\`（该目录在部署树里根本不存在）4 条 +
   `cpp_table_id_bit_index\` 2 条，及其 filters 对应块。这些都是更早一代生成器
   留下的重复条目——正确的 `code\constants\` 与 `code\bit_index\` 条目一直都在。
   现在 `table.vcxproj` / `.filters` 里**所有 ClInclude/ClCompile 指向的文件均存在（悬空 0 条）**。
4. **修 `tests/conftest.py` 的 `make_config` 漏设 `schema_dir`。** 它直接构造
   `ExporterConfig` 绕过了 `load_config`，于是拿到 dataclass 默认值 `Path()` == `.`
   （= 跑 pytest 时的 CWD），而 `index_schema_protos` 对「目录存在但没有
   `cfg_options.proto`」是 fail-closed 的，导致两条编排门禁用例误报。
   按 `load_config` 的推导补成 `data_dir/schema`。**生产侧不受影响**
   （实测 `schema_dir` 解析到 `data/schema`，22 个 proto 齐全），
   那条 fail-closed 是要保留的：schema_dir 配错必须立刻炸。

**通过项**

- 导表器：`pytest tests/ -q` → **73 passed**（修 conftest 前是 71 passed / 2 failed）。
- Go：`shared` build + vet + `go test ./serverbase/...` 通过；
  `guild` / `friend` / `match` 各自 `go build ./...` 与 `./internal/constants/...` 测试全部通过。

**仍未覆盖（诚实列出）**

- **没跑全量导表**。仓库自带 `third_party/grpc/install_vs2026/bin/protoc.exe` 是 **31.1**，
  而本管线要求恰好 **35.1**（签入 gencode 为 7.35.1，跨大版本运行时拒绝加载）。
  用 31.1 跑会产生大量与本次无关的 diff，因此刻意不跑。
  Codex 已用 35.1 完成过一次全量导表 + 二次跑零差异，本轮的改动
  （filters/文档/删孤儿/conftest）不影响导表产物。
- **没跑 C++ / Java 构建**。本轮对 C++ 侧只动了工程文件的条目增删，未触碰任何 `.h/.cc`；
  已用 XML 解析 + 悬空引用归零替代编译验证。真正的 `msbuild` 由 Codex 上一轮完成
  （0 warning / 0 error，表测试 24/24 与 37/37）。
- `no-raw-pointer-member` 仍 **SKIP**（缺 `no_raw_ptr_check.exe` / `clang-query`），与本次改动无关。

**顺带发现（非本次引入，未处理）**：`GlobalVariable.xlsx` 的 `to_uint32/to_int32/to_string/
to_float/to_double` 五列 owner 为空，导表器会警告「整列丢弃」；其中 `to_double`
**确实有 1 个数据单元格**，正在被静默丢掉。属策划表的数据问题，留给表的负责人。

关于「工作区不干净（484 项）」：其中绝大多数是**并发编辑者**的改动（`go/proto/**` 等在本次工作开始前 14 小时就已存在）。
本次改造开工前逐目录确认过要动的路径是干净的。「新增产物与 workflow 未跟踪」是预期状态——
按仓库规则 `git add` / `git commit` 须由人明确发话。

### 6.3 故障分类收口（本机，2026-09-05）

工具链：隔离 venv（pytest / openpyxl / jinja2 / PyYAML / protobuf 7.36）、go1.26.5；**本机仍无 protoc 35.1**。

**通过项**

- 导表器：`pytest tests/ -q` → **129 passed**（含本轮新增的「红线 7：故障分类在表里」用例：表头按名定位、表头缺失中止、闭集取值、非法值点名单元格、组头/注释行标 fault 中止、墓碑不进表、fault 不进 state、空故障表仍是合法 Go、产物末尾有换行）。
- 四视角独立评审（导表器 / Go / CI 与文档 / 分类数据保真，每条发现 2 个反驳者复核，20 个 agent）：分类数据逐符号对照**无漏迁无误标**；确认 4 条 P2 全是文案与交接一致性（match 没挂拦截器却把 `MatchInternal` 写成「已收口」、`Options.TipClassifier` 注释仍邀请按服务微调、handoff 并发编辑者清单不全、handoff 仍写 73 passed），已全部修掉；驳回 4 条（非码行填 `0` 也中止是 fail-closed 而非缺陷；空故障表的 `[]Fault{\n}` 形状顺手改成 `[]Fault{}`；CI 对 `generated/code/go` 的 `git status` 因 `.gitignore` 永远安静、闸门是 `cmp`，已把死条目删掉并注明）。评审后把 `MatchInternal` 的标记撤回（一码两用）。
- 生成：用纯 Python 路径跑 `generate_tip_enums(load_config())`（不需要 protoc）：17 段、**34 个故障码**；`state` / 17 份 tip proto / `tip_text.json` / `segments.go` **零变化**；`faults.go` 生成后手动部署到 `go/shared/generated/tip/`，与 `generated/code/go/generated/tip/` 两份 `cmp` 一致；`gofmt -l` 干净（`faultSet` 改为从 `Faults` 派生，是因为 4 位码与 5 位码混排时 gofmt 会重排 map 字面量对齐；Jinja 吃掉的末尾换行由生成器补回）。
- Go：`go/shared` build + vet + `serverbase` 测试；`guild` build + constants + server（in-band 观测）测试；`friend` build + constants + server 测试；`match` build + constants 测试，全部通过（`-count=1` 复跑一次）。

**顺带修的既有红**：`go/match/internal/constants/errors_test.go` 的 `tipCodes` 清单缺 `ErrNotInScene`（`71edf77f7` 合并上游时 `errors.go` 加了码但清单没跟上），HEAD 上 `TestNoHandWrittenTipCodes` 就红。补了一行。

**仍未覆盖**

- 没跑全量导表（同 §6.2 的原因）。本改动对全量导表的影响只有 `_generate_tip_faults` 多写一个 Go 文件；`_deploy` 会把 `generated/code/go/generated` 整目录同步到 `go/shared/generated`，`faults.go` 会自然到位。
- 没跑 C++ / Java 构建：本改动没碰它们的任何输入（proto 产物零变化）。
