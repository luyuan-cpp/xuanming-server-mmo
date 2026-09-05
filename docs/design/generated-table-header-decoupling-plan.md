# 交接：mmorpg 导表器生成头文件解耦（①exprtk ②FK声明化 ③muduo）

仓库：`D:\luyuan\mmorpg`　生成器：`tools/data_table_exporter`
状态：**方案已审，代码未动**。本文档自包含，执行者不需要原对话。

---

## 0. 硬约束（用户明确要求，不可违反）

- **不许上 PCH**（precompiled header）。
- **不许搞 stdafx.h 那套**：不做伞形头 / 聚合头 / 「大家都 include 一个大头」。
- 曾讨论过的「manager 通用逻辑下沉手写 CRTP 基类」**已被砍掉**，不要重新提。
- 数据产物（`generated/tables/*.json`、`*.pb`、`manifest.json`）必须**逐字节不变**。本批只碰代码模板。

---

## 1. 问题是什么

`cpp/generated/table/code/mirror_table_fk.h` 这类生成头 include 了对应表头，改一处波及一片。量了之后真实成本分布是：

| 拖进来的 | 行数 | 谁真的要 |
|---|---|---|
| `table_expression.h` → `exprtk/exprtk.hpp` | **40983** | 22 张表里只有 Buff / Skill |
| `<x>_table.pb.h` + protobuf 运行时 | 数万 | 真要，**不许动** |
| `muduo/base/Logging.h` | 159 | 只有 6 个 `Lookup*` **宏体**里的 `LOG_ERROR` |
| 目标表整头（仅 fk 头） | ×1~2 | 只需前向声明 |

`grep -l 'ExcelExpression' cpp/generated/table/code/*_table.h` → 2 个文件（buff、skill）。
`grep -l 'table_expression.h' cpp/generated/table/code/*_table.h` → 21 个文件。

**实测收益（①）**：48/244 个 TU 现在会解析 exprtk，改完剩 15 个，**33 个 TU 各少解析 40983 行**。
对象文件佐证：`build/cpp/intermediate/lib/table/code/all_table.obj` = 41,973,979 字节、
`buff_table.obj` = 37,842,146，对比 `mirror_table.obj` = 1,740,741、`basescene_table.obj` = 1,341,886。

**②的收益要说实话**：全仓只有 `cpp/tests/configuration_table_test/configuration_table_test.cpp:7`
include 过任意 `*_table_fk.h`，5 个 fk 头里 4 个零消费者，全部 fk 辅助函数**零生产调用点**。
②是止血（防止 FK 变多后扩散），不是提速。愿不愿意为它承担「永久手工登记 build 文件」是**你的决定**。

---

## 2. 待你拍板的三件事

### 决策 A：③ 用哪个变体

先纠正一件事：**「3b = 前向声明 muduo」这条路不存在**。
`third_party/muduo/muduo/base/Logging.h:131`：
```cpp
#define LOG_ERROR muduo::Logger(__FILE__, __LINE__, muduo::Logger::ERROR_).stream()
```
宏在**调用点**展开，`operator<<` 需要完整的 `muduo::Logger` / `LogStream`。所以窄头只能声明一个**普通函数**让宏去调，这是行为可见的改动。

| | 做法 | 消费方改动 | 代价 |
|---|---|---|---|
| 3a | 直接删 `cpp_config.h.j2:14`，用宏的 TU 自己 include muduo | **3 个必改（含 2 个头文件）+ §4 P0-D 那 5 个** | 2 个必改是**头文件**（`time_cooldown.h`、`missions_config_comp.h`），muduo 照样传染给它们的 7 个下游 TU |
| **3b（推荐）** | 新增窄头声明 `TableLookupLogMissing(...)`，宏改成调它 | **宏使用方 0 个**，但 §4 P0-D 那 5 个照样要补 | 日志源位置字段变化（见 §4 P0-A） |
| 3c | 宏里干脆不打日志（调用方本来就拿到状态码） | 同上 5 个 | 47 个调用点静默丢观测性 |

> ⚠️ 原稿写的「3b = 零消费方改动」是**错的**，已按 §4 P0-D 修正：删掉表头里的 muduo 会停止向
> **全部 28 个 includer** re-export muduo，不只是用宏的那些。两个变体都得付这 5 个文件的账，
> 差别只在宏使用方那 3 个必改（其中 2 个是头，会继续往下游传染）。

**推荐 3b**：`table_log.h` 是这仓最窄的头（1 个声明 + 1 个 std include），它恰恰是在**拆掉**一个意外伞形头（生成表头现在把整个第三方日志库 re-export 给 28 个 TU），跟「不许 stdafx 思想」是同向的，不是反向。

### 决策 B：② 做不做，做到什么程度

- 做（放最后，独立提交对）／ 只做 ①③ 推迟 ②。
- 「保留 inline 但不 include 目标表头」这条**不存在** —— 每个函数体都调 `{{target}}TableManager::Instance().FindByIdSilent(...)`，需要完整 manager 类型。函数体必须搬走。
- 若做：**全声明式**（连自己那张表的头也前向声明），这样 fk 头可独立编译，是个可测属性。

### 决策 C：UE 门

**推荐推迟到后续单子**，三条理由都已核实：
1. 无法编译验证 —— 全仓找不到 `.uproject` 和 `*.Build.cs`。
2. 零痛点 —— `exporter_config.yaml:141` 是 `deploy: []`，UE 产物生成后不拷到任何地方。
3. ①③ 在 UE 侧**没有对应物**（UE 无表达式运行时，生成头里零 `UE_LOG`），硬套会造出假工作。

> 留给后续单子的真正大鱼：`ConfigSubsystem.h`（由 `ue_all_table.h.j2:33-35` 生成）include 全部 21 个 `<X>Table.h`，
> 对每个消费者约 5173 行，光靠前向声明就能修。另有陷阱：声明式 UE fk 头需要给 20 个自由函数加
> `MMORPGCONFIG_API`，会把 `exporter_config.yaml:134` 记录的「单模块留空」配置变成跨模块链接失败。

---

## 3. 执行步骤

### Step 0a — 先存基线（**必须最先做**）

```bash
python tools/data_table_exporter/tools/sandbox_export.py --out <scratch>/baseline
```
`sandbox_export.py:110-114` 从**活动工作树**复制 `templates/`，一旦改了 `.j2` 就再也造不出基线。
前置条件：`third_party/grpc/install_vs2026_dbg/bin/protoc.exe` 在位（`sandbox_export.py:48` 钉死），
tip / bit-index 状态干净；任一腿失败就**没有基线**（`:219-222` 直接 return），后续步骤失去唯一回归对照。

### Step 0b — 记录构建基线

`vcvars64.bat` 在 `E:\Program Files\Microsoft Visual Studio\18\Enterprise\VC\Auxiliary\Build\`。
默认工具集 14.51.36231 与 `build/cpp/intermediate/lib/table/table.tlog/table.lastbuildstate` 一致，无需钉 `-vcvars_ver`。

```bash
MSBuild.exe cpp\generated\table\table.vcxproj /p:Configuration=Debug /p:Platform=x64 /m /v:minimal
```
**按路径构建，绝不要用 `/t:<名字>`** —— `game.sln` 里 `scene`/`modules`/`gate`/`battle` 各有两个同名工程，只有 `table` 不歧义。

必须同时记录基线的还有：`dev.bat build`（整个 game.sln）+ **Linux 一遍**。
`configuration_table_test.exe`（09-03 03:46:35）比 `lib/table.lib`（09-03 05:47:38）旧，**如果它本来就是红的，别赖到这次重构头上**。

### Step 1a — ① exprtk 条件化

`tools/data_table_exporter/templates/cpp_config.h.j2:13`，照抄同文件 `:8-10` 已有的写法：
```jinja
{%- if table.expression_columns %}
#include "table_expression.h"
{%- endif %}
```
闸门属性 `table.expression_columns` 定义在 `core/schema.py:243-245`，两条 schema 入口都会填
（`schema.py:118-123` 老式 `expr:` token；`schema_proto.py:540` `("cfg_expr_type","expr:%s")`），
`table=` 已在每个 cpp ctx 里（`config_gen.py:33-42`），**不需要改 Python**。

`ExcelExpression` 必须留在 Buff/Skill 的**头**里：它是 Snapshot 的按值成员（`cpp_config.h.j2:51-53`），
`Get*/Set*Param` 是头内联（`:127,:130`）。

**不要碰 `:15`**（`table/proto/{{...}}_table.pb.h`）—— Snapshot 按值持有 `{{sheetname}}TableData`（`:45-47`），
而且它是 `<string>`/`<utility>`/`<cstddef>` 的**唯一供应者**（头里在用但从没 include）。

### Step 1b — 重新生成并提交两棵树

**不要跑 `run.py`**（见 §4 P0-C）。用 CI 用的窄路径：
```python
from core.config_loader import load_config
from core.table_source import read_all_tables
from core.generators.config_gen import generate_config_classes
cfg = load_config(<exporter_config.yaml>)
generate_config_classes(cfg, read_all_tables(cfg))
```
再手工只部署 cpp 一棵：`core.file_utils.md5_copy_dir(generated/code/cpp → cpp/generated/table/code)`。

两棵树都要提交：`generated/code/cpp/`（生成树）和 `cpp/generated/table/code/`（部署树，真正参与编译）。

### Step 2a/2b — ③ muduo（若选 3b）

**2a** 新建手写文件 `cpp/libs/engine/config/table_log.h`：
```cpp
#pragma once
#include <cstdint>

/// 配置表查行失败时打日志。
/// 单独放这里是为了让生成的 Lookup* 宏自洽，而不必把 muduo 塞进每个生成表头。
void TableLookupLogMissing(const char* tableName, uint32_t tableId,
                           const char* file, int line);
```
外加**手写** `cpp/libs/engine/config/table_log.cpp`（**不要**放进生成的 `all_table.cpp`，见 §4 P1-D）：
```cpp
#include "table_log.h"
#include "muduo/base/Logging.h"

void TableLookupLogMissing(const char* tableName, uint32_t tableId,
                           const char* file, int line) {
    LOG_ERROR << tableName << " row not found for ID: " << tableId
              << " @ " << file << ':' << line;
}
```
> **必须用 `LOG_ERROR` 宏本身，不许手写 `muduo::Logger::ERROR_`** —— 见 §4 P0-A。

`config.vcxproj` 加一条 `ClInclude`（table_log.h）+ 一条 `ClCompile`（table_log.cpp，已有 ItemGroup 在 `:26-29`），
`cpp/libs/engine/config/CMakeLists.txt` 同加一行。
`cpp/libs/engine/config/` 已在所有展开 Lookup 宏的工程的 include 路径上（`table.vcxproj:288`、modules、services/scene、nodes/{scene,gate,battle}、configuration_table_test；CMake 侧 `generated/table/CMakeLists.txt:34` 等），
所以 `#include "table_log.h"` 裸包含能解析，和 `table_expression.h` 现在的待遇一样。
**上线前先确认 `config.lib` 在这些目标的链接线上。**

**2b** 三处模板协同改：
1. `cpp_config.h.j2:14`：`#include "muduo/base/Logging.h"` → `#include "table_log.h"`
2. `cpp_config.h.j2` 六个宏体（`:259,:263,:267,:271,:275,:279`）：
   `LOG_ERROR << "{{sheetname}} row not found for ID: " << tableId;`
   → `TableLookupLogMissing("{{ sheetname }}", tableId, __FILE__, __LINE__);`
   **各宏自己的 bail-out 原样不动**（`return ...Result;` / `return prefix##...Result;` / `return customReturnValue;` / `return;` / `continue;` / `return false;`）。
3. **`cpp_config.cpp.j2:1-5` 必须加 `#include "muduo/base/Logging.h"`** —— 它自己没有 muduo，
   现在纯靠 `:5` 那句 `#include "table/code/{{...}}_table.h"` 蹭。它用了 `LOG_FATAL`（`:17,:23`）和 `LOG_ERROR`（`:63,:84`）。
   **漏了这条，22 个生成 .cpp 立刻全挂。**

`kInvalidTableId`/`kSuccess` 不会跟着受影响：它们是 `common_error_tip.pb.h:66-67` 的枚举值，只被 .cpp include（`cpp_config.cpp.j2:4`），
宏体从不提它们（只有 `:249/:253` 的注释提），状态值以裸 `uint32_t` 过界。

顺手更新 `cpp_config.h.j2:249-255` 的说明注释。

**2b 还必须做的两件事**（否则这步是假绿）：
- 按 §4bis P0-D 给那 5 个文件补 `#include "muduo/base/Logging.h"`（至少
  `modifier_buff_impl.cpp` 和 `player_skill.cpp` 两个「最可能断」的，其余三个按构建结果补）。
- **2a/2b 必须紧跟 1a/1b，不要重排** —— 理由见 §4bis P1-H。

### Step 3a/3b/3c — ② FK 声明化（**放最后**）

**3a 模板拆分。** 关键要求：**声明和定义必须从同一个 `{% macro %}` 发**（见 §4 P1-E），
不要维护两份平行的 7 个发射点。

头（`cpp_config_fk.h.j2:2-7` 整块替换）：
```jinja
#pragma once
#include <cstdint>
#include <vector>
{%- if <有 string 类型的 fk 列> %}
#include <string>
{%- endif %}

class {{ sheetname }}Table;
{%- for target in fk_targets %}
class {{ target }}Table;
{%- endfor %}
```
7 个发射点（`:16,:28,:37,:45,:56,:68,:82`）去掉 `inline` 和函数体，以 `;` 收尾。
**`{% if not table.multi_primary_key %}` 三处闸门（`:26,:43,:66`）原样保留** —— 它们是活的
（`testmultikey_table_fk.h` 一个 `*ById` 重载都没有，而 `mission_table_fk.h:20,:37` 有）。

新模板 `cpp_config_fk.cpp.j2`：函数体原样，include 用**裸引号**形式：
```jinja
#include "{{ sheetname | lower }}_table_fk.h"
#include "{{ sheetname | lower }}_table.h"
{%- for target in fk_targets %}
#include "{{ target | lower }}_table.h"
{%- endfor %}
```
> 生成树里两种风格都存在（`mirror_table.cpp:4` 是带根的 `table/code/...`，
> 而 `cpp_all_table.cpp.j2:2,8` 和全部 fk 头是裸的）。这里选**裸的**，两条理由：
> 与同一对里的 `.h`（`mirror_table_fk.h:3,5,7` 是裸的）一致，别让 fk 这对成为全树唯一
> .h 和 .cpp 风格打架的地方；而且裸包含按 includer 相对查找，**不依赖 `../../generated/` 在 -I 上**。

Python（`core/generators/config_gen.py`）：
- `:72` 加载新模板；
- `:78-81` 的 `if t.has_foreign_keys:` 块里加一次 `write_file`，
  **必须套 `_clean_output`**（对齐 `:80`，否则产物首行多一个空行，两棵树永久脏 diff）；
- `_cpp_fk_ctx`（`:105-113`）**无需改动**，同一个 ctx 渲染两个文件。

**为什么声明式合法**（两条独立理由）：
- 目标类型只以 `const XTable*` 或 `std::vector<const XTable*>` 的**元素**出现，指向不完整对象类型的指针本身是完整类型；
- 函数**声明**从不要求返回/参数类型完整（[dcl.fct]/12 的完整性要求只作用于**定义**）。
- `class MirrorTable;` 可证合法：**protoc 自己就在 `cpp/generated/table/proto/mirror_table.pb.h:56` 写了同一行**，
  全局作用域、无 `package`（`proto_table.proto.j2:2-9` 只发 option）、无 `dllexport_decl`（`proto_gen.py:170` 是裸 `--cpp_out=`）。
- **不需要**前向声明 manager：`XTableManager::Instance()` 只出现在函数体里。
- **必须加 `<cstdint>`**：`uint32_t` 现在是从 `<sheet>_table.h:3` 传递来的。

**3b 登记 build 文件。** 5 个新文件：`dungeon/mirror/mission/testmultikey/world_table_fk.cpp`。
- `table.vcxproj`：ClCompile ItemGroup 在 **`:118-139`**（不是 117-180），按字母序插入。
  对应的 5 条 `ClInclude ..._table_fk.h` **已存在**（`:44,:55,:58,:70,:75`），别重复加。
- `table.vcxproj.filters`：加 5 条 `<ClCompile ...><Filter>源文件</Filter></ClCompile>`。
- `cpp/generated/table/CMakeLists.txt`：`set(SOURCE_FILES` 块（`:41` 起）加 5 行。

**3a 和 3b 不可单独回滚** —— 回滚 3a 会留下引用不存在文件的 vcxproj（MSBuild 报错），
回滚 3b 会留下未定义符号（configuration_table_test LNK2019）。**成对回滚**。

**3c** 重新生成 + 部署 + 两棵树一起提交（同 Step 1b 的窄路径）。
`exporter_config.yaml` **不需要改** —— `:44-46` 整目录递归拷，`md5_copy_dir`（`file_utils.py:79-88`）会带上新文件。

---

## 4. 陷阱清单（这份文档最值钱的部分）

### P0-A　仓里有两份 muduo，`LOG_ERROR` 的枚举名不一样 —— 手写枚举必挂一端

| 构建 | include 路径 | `Logging.h:26` 枚举 |
|---|---|---|
| Windows（`table.vcxproj:288`） | `third_party/muduo` | `ERROR_` |
| Linux（`CMakeLists.txt:33`） | `third_party/muduo-linux` | `ERROR` |

（Windows 那份改名是因为 `ERROR` 在 wingdi.h 里是宏。）
`table_log.cpp` 是**两端都要编**的文件，所以**函数体里必须用 `LOG_ERROR` 宏本身**，
不许写 `muduo::Logger(..., muduo::Logger::ERROR_)`。
代价：muduo 自己的 SourceFile 字段会显示 `table_log.cpp`，调用点位置退到消息正文。
要保住那个字段就得加 `#if defined(_WIN32)` 选枚举名 —— 不值得。

### P0-B　① 不是零风险：`table_expression.h` 还 re-export 了 `type_define.h`

`cpp/libs/engine/config/table_expression.h:3-4` 同时 include `exprtk.hpp` **和**
`engine/core/type_define/type_define.h`，后者带来 `<set>/<string>/<unordered_map>/<unordered_set>/<vector>`
+ `absl/hash` + `absl/int128` + `entt/entity.hpp`，并定义 `Guid / NodeId / SessionId / StringVector /
UInt32Vector / EntityVector / EntityUnorderedSet / ItemCountMap` 等。
**现在 21 个生成表头把这一整片 re-export 给每个 include 表头的 TU。**

已核实：下列 7 个文件用了这些别名但**没有直接 include** `type_define.h`：
```
cpp/libs/modules/bag/bag_system.cpp                                 Guid GuidVector ItemCountMap
cpp/libs/modules/bag/item_store.cpp                                 EntityVector Guid GuidVector ItemCountMap
cpp/libs/modules/mission/system/mission.cpp                         Guid
cpp/libs/services/scene/actor/attribute/system/actor_attribute_calculator.cpp   Guid
cpp/libs/services/scene/battle/system/player_battle.cpp             Guid NodeId
cpp/tests/missions_test/missions_test.cpp                           Guid
cpp/tests/reward_test/reward_test.cpp                               Guid
```
（`actor_action_state.cpp` 和 `bag_test.cpp` 有直接 include，安全。）
大概率它们还有别的路径拿到，但**没人验过**。
→ ① 的风险等级从「极低」上调为「中 —— 大面积传递包含移除」，闸门必须是**整解决方案构建 + Linux 一遍**，
不能只是 `MSBuild table.vcxproj`（那个工程只编生成树，结构上照不到消费方）。

### P0-C　**不要跑 `run.py`**

`core/orchestrator.run()` 跑的是全流水线：`generate_json(:90)`、`generate_proto_files(:91)`、
五门 protoc（`:95-99`）、`generate_binary(:99)`、`generate_manifest(:108)`、
然后 `_deploy(:111)` 写进 `go/shared/generated`、`java/config_node/...`、
`client/unity/Assets/Scripts/Table/Generated`。
`_parse_args(:235-256)` **没有**「只跑代码生成」的开关。
照做就会重写全部 `.json/.pb/manifest.json` —— 正是本批禁止发生的事。
用 §3 Step 1b 的窄调用 + 手工只部署 cpp 一棵。

### P1-D　`TableLookupLogMissing` 的定义不要放生成的 `all_table.cpp`

`all_table.cpp` 会调 21 张表的 `Load()`（`cpp_all_table.cpp.j2:14-16,:29-37`），
一旦有 TU 引用 `TableLookupLogMissing`，链接器就得拉进 `all_table.obj`（**41,973,979 字节**），
再连带拖进 21 个表 obj + 各自 pb.obj。现在只展开 `LookupCooldown*` 的目标只拉 `cooldown_table.obj`。
→ 定义放手写的 `cpp/libs/engine/config/table_log.cpp`，和头放一起。

### P1-E　②的声明/定义分家会产生**没人能发现**的缺陷

全仓只有 `configuration_table_test.cpp:7` include 过 fk 头，而且只 include `testmultikey_table_fk.h`。
→ 3b 那个「真闸门」只证明了 5 个新 TU 里的 **1 个**能链接。
dungeon/mirror/mission/world 四个，若 `.h.j2` 和 `.cpp.j2` 在
`{% if not table.multi_primary_key %}` 闸门或 `to_cpp_param_type` 上分叉，
产生的是**无匹配定义的声明** —— 编译干净、链接干净，因为没人引用它。
（方案原稿只警告了反方向，那个是 MSVC 立刻报错的响case。）
**修法**：声明和定义从**同一个 jinja macro** 发，用 decl/def 标志参数化，让两边构造上不可能分叉。
退而求其次：给 `configuration_table_test.cpp` 补上另外 4 个 fk 头并取每个函数的地址，逼它们全链接。

### P1-F　方案里**没有 Linux 闸门**，而 Linux 才是出货路径

`deploy/k8s/Dockerfile.cpp:177` 跑 `bash tools/scripts/build_linux.sh --skip-deps --relwithdebinfo --split-debug`。
传递包含移除（①）和声明式头恰恰是 MSVC 与 GCC/libstdc++ 对「什么会漏出来」意见不一致的地方。
CI 也补不上：`cpp-build-ci.yml` 的 compile-nodes **默认不挂 PR**（只在每日/手动/打 `cpp-full-build` 标签时跑），
`include-cleaner-ci.yml` 只扫 `cpp/nodes/scene` 和 `cpp/nodes/gate` 且 `-ChangedOnly`，不编译。
→ 每个改模板的步骤后面都要跑一次 Linux 构建，或者至少给 PR 打 `cpp-full-build` 标签。

### P1-G　②让「陈旧产物」从无害变成有害，建议补一道 C++ 产物闸

`_validate_java_code_outputs`（`orchestrator.py:130-162`）是**唯一**的产物闸，而且 Java only（`:134-135` 直接 return）。
某张表将来丢掉最后一个 FK → `has_foreign_keys`（`schema.py:267-269`）变 false → 不再生成 fk.h/fk.cpp，
但两个文件在 `generated/code/cpp` 里**永远留着**，继续被部署
（`md5_copy_dir` 只覆盖不删，`file_utils.py:76-88`；`report_orphans` 拿 dst 比 src 而 src 里还有，`:142-143`），
而**手工加的 `<ClCompile>` 会继续编译这个陈旧 TU**。
→ 照抄 Java 那个写一个 `_validate_cpp_code_outputs`，约 15 行，在 `run()` 里 `:102` 旁边调用。

### P2-H　CI 的漂移闸**看不到部署树**

`exporter-tests.yml:343` 只查 `generated/code/{cpp,go,java,csharp,python,ue}`。
`cpp/generated/table/code`（真正参与编译的那棵）**不在任何 workflow 里**。
→ 忘记部署在 CI 上是**绿的**，只在本地 MSBuild 才炸。
「两棵树一致」必须当成人工检查项，用内容 diff，不能只看 `git status`。
（可选一行改进：把 `../../cpp/generated/table/code` 加进 `:343` 那个列表。）

### P2-I　行尾：vcxproj 是 CRLF，其它全是 LF

`.gitattributes` 有 `* text=auto eol=lf` 和显式 `*.h/*.cpp text eol=lf`，**没有 `*.vcxproj` 规则**。
`table.vcxproj` / `.filters` 是 CRLF（330 CR / 330 LF），模板和全部生成产物是纯 LF（0 CR）。
用任何吐 LF 的工具（本仓踩过的 sed/awk 吃 CR）编辑 vcxproj，会把 330 行全改写。
另外 `write_file` 用 `open(..., newline="")`（`file_utils.py:18`），产物行尾**逐字继承 .j2**，
新建的 `cpp_config_fk.cpp.j2` 若是 CRLF，整棵树 diff。
连带：`_clean_output` 的 `re.compile(r'\n{3,}')`（`config_gen.py:55`）是 LF-only，模板一旦变 CRLF 它静默失效。

### P2-J　「vcxproj 是脏的」这条已过期

我最初看到 ` M cpp/generated/table/table.vcxproj` + `.filters`，**现在已经是干净的**
（`git status --porcelain -- cpp/generated/table/` 空；全仓只有 5 个 third_party 子模块脏）。
执行时**现查一遍**，真报 ` M` 再考虑保留 VS 的重排。

### P2-K　sandbox 对拍的验收数字要改

`compare()` 调用形式是 `compare(out_dir/"generated", base)`（`sandbox_export.py:227`），
而部署树被重定向到 `<out>/deploy`（`:92,:108`）—— **在 `generated` 之外**，对拍**结构上看不到部署树**。
正确验收：5 个新增 `*_table_fk.cpp`、5 个变更 `*_table_fk.h`、**21** 个变更 `*_table.h`
（不是 22 —— `ls *_table.h` 出 22 是因为 `all_table.h` 也匹配这个通配，而 1a/2b 都不碰它）、
`generated/tables/` 与 go/java/csharp/python/ue 五棵树**零变化**。
manifest 天然不动：`manifest.py:80-88` 只对 `<name>.json` 和 `<name>.pb` 取指纹。

### P2-L　C++ 模板**零测试覆盖**

`tools/data_table_exporter/tests/` 和 `smoke_test.py` 里没有任何东西 import `config_gen`、
调 `generate_config_classes` 或渲染任何 C++ 模板。
→ 「哪些测试会红」的答案是：**一个都不会**。
`exporter-tests.yml` 的漂移闸只能发现「你忘了重新生成」，永远发现不了「你生成错了」。
最便宜的长期护栏：约 20 行 pytest，用现有 conftest 的 Field/TableSpec 夹具渲染 `cpp_config.h.j2` 两次
（有/无 expression 列），断言 `table_expression.h` 出现/不出现。**建议顺手加上。**

### P2-M　CMakeLists 已有的陈旧漂移（可选顺手修）

`grep -n equipslot cpp/generated/table/CMakeLists.txt` → 无命中，
而 `table.vcxproj:128,:149` 两条都在，磁盘上有 22 个 `*_table.cpp` 而 CMakeLists 只列 21 个。
出货 Linux 路径不受影响（`build_linux.sh:156` 用 `vcxproj2cmake.py` 从 vcxproj 重生成 CMakeLists），
但**提交的文件在撒谎**，谁读它推理 Linux 构建就会得到错答案。
建议独立提交修掉：加 `code/equipslot_table.cpp` 和 `proto/equipslot_table.pb.cc`。

> 顺带澄清一个被夸大的「文档冲突」：`AGENTS.md:47-49` 说要同改两处，但它的语境是**新开 tip 错误码域**，
> 不是通用政策；`build_linux.sh` 里「生成器会覆盖 CMakeLists，不要手改」才是通则。
> **vcxproj 是事实源**，改 CMakeLists 属于「让提交的文件不撒谎」的修饰性动作。

### P2-N　fk 头的 include 风格（已定：裸引号）

生成树里两种风格都有：`mirror_table_fk.h:3-7` 和 `cpp_all_table.cpp.j2:2,8` 是**裸**的，
`mirror_table.cpp:4` 是**带根**的（`table/code/...`）。新的 `cpp_config_fk.cpp.j2` **用裸的**，
理由见 §3 Step 3a。别让 fk 这一对成为全树唯一 .h 和 .cpp 风格打架的地方。

---

## 4bis. 第二轮评审补充（compile-correctness 镜头，已逐条复核）

### P0-D　③ **不是**「零消费方改动」—— 表头停止 re-export muduo 会打到 5 个文件

删掉 `cpp_config.h.j2:14` 会停止向**全部 28 个 includer** re-export muduo，不只是用 `Lookup*` 宏的那些。
下列文件 include 了某个 `*_table.h`、用了 `LOG_*` 或 `muduo::`、且**自己没 include** `muduo/base/Logging.h`：

| 文件 | LOG_/muduo:: 命中 | 现有的可能替代路径 | 判断 |
|---|---|---|---|
| `cpp/libs/services/scene/combat/buff/system/modifier_buff_impl.cpp` | 1 | 无 return_define / error_handling | **最可能断** |
| `cpp/libs/services/scene/player/system/player_skill.cpp` | 3 | 无 | **最可能断** |
| `cpp/nodes/scene/handler/rpc/player/player_skill_handler.cpp` | 2 | `macros/return_define.h:5` | 可能安全 |
| `cpp/libs/modules/bag/bag_system.cpp` | 40 | `error_handling.h:3` + `return_define.h:5` | 可能安全 |
| `cpp/libs/modules/bag/item_store.cpp` | 2 | `error_handling.h:3` | 可能安全 |

> 评审还点了 `node.cpp`(91 命中) 和 `node_entry.h`(4)，**我复核后排除**：它们包的是 `all_table.h`，
> 而那个头只有 `#pragma once` + `<cstdint>` + `<functional>`，**从来就没有 muduo**，与 ③ 无关。

3a 和 3b **都要**付这 5 个文件的账，差别只在宏使用方那 3 个必改。3b 仍然胜出。

### P1-H　顺序耦合：① 会拆掉 ③ 赖以生效的 include 路径保证

现在**每个**生成 `*_table.h` 都裸包含 `"table_expression.h"`（`cpp_config.h.j2:13`），
所以任何能编到表头的工程**必然**已经把 `cpp/libs/engine/config/` 放在 include 路径上
（`modules.vcxproj` 用 `../engine/config/`、`scene.vcxproj` 用 `../../engine/config/`、
`core.vcxproj` 用 `../config/`、`battle.vcxproj` 用 `../../engine/config/`）。
**Step 1a 把这条保证从 19 个头里删掉了。** 而 `table_log.h` 恰好也在那个目录。
→ **2a/2b 必须紧跟 1a/1b，不要重排**。在 1b 和 2b 之间，某个工程可以悄无声息地丢掉这条路径而不报错，
然后 2b 才炸。

### P1-I　链接面：展开 `Lookup*` 宏的每个 EXE 现在必须链 `config.lib`

定义搬到手写的 `cpp/libs/engine/config/table_log.cpp` 之后（P1-D），
链接依赖从 `table.lib` 换成 `config.lib`。
**两个宏展开点在头文件里**——`missions_config_comp.h`（8 处，`:28,34,40,46,52,58,64,69`）和
`time_cooldown.h`（2 处，`:15,39`），都在 inline 成员函数里——所以要求会传染到**每个 include 它们的 TU**，
是个比方案列举的 .cpp 集合**严格更大**的集合。
动手前先枚举 `cpp/nodes/*` 和 `cpp/tests/*` 里所有传递展开 Lookup 宏的 EXE 目标，确认 `config.lib` / `-lconfig` 在链接线上。

**工程名纠正**：是 `cpp/tests/missions_test/missions.vcxproj` 和 `cpp/tests/reward_test/reward.vcxproj`，
**不是** `*_test.vcxproj`。闸门里至少要加上 `missions.vcxproj`、`reward.vcxproj`、
`cool_down_time_test.vcxproj`（它拉 `time_cooldown.h`）、`buff_test.vcxproj`、
`bag_test.vcxproj`、`skill_test.vcxproj`，以及 `cpp/libs/services/battle/battle.vcxproj`
（它编 `table_battle_data_provider.cpp`，一个文件 include 6 个生成表头）。

### P1-J　`tableId` 会多出一次窄化，47 个点 × 新的 -Wconversion 面

宏体从 `<< tableId`（由 `muduo::LogStream::operator<<` 按 tableId 自己的类型选重载）
改成传给 `TableLookupLogMissing(..., uint32_t tableId, ...)`，等于**每个展开点多一个窄化点**。
`FindByIdSilent` 本来就收 `uint32_t`（`cpp_config.h.j2:99`），所以不会新增编译失败，
但这是 47 处新的 C4244 / `-Wconversion` 面，**而本仓有 Linux DS 撞 Clang `-Werror` 的前科**。
→ 把参数放宽成 `uint64_t`（流式输出行为相同，不新增窄化），或者显式记录两边警告等级都查过了。
另外：`tableId` 仍然被求值两次（和现在一样，不是新问题），所以实参必须无副作用 ——
真实调用点确实传表达式，如 `LookupItemOrReturnError(proto.config_id())`、`LookupItemOrContinue(item.config_id())`。

### P1-K　②的验收前提要先验

Step 1a 的验收是「改完 `grep -l table_expression.h` 只剩 2 个文件」，但**没人先确认只有 Buff/Skill
有非空 `expression_columns`**。两条 schema 入口都能设它（`schema.py:118-123` 老式 `expr:`；
`schema_proto.py` 的 `cfg_expr_type`）。若还有第三张表带 expr 列，闸门会失败并且**看起来像模板 bug**。
→ 动手前先 grep `data/schema/*.proto` 的 `cfg_expr_type` 和老式表头的 `expr:`。

### P2-O　1a 单独存在是危险中间态

如果 1a 落地、③ 后来被单独回滚，19 个生成头就会**既没有 `table_expression.h` 也没有 `muduo`**，
所有原本靠这两条之一蹭到东西的消费方一起断，且**原因不明显**。
→ 要么 1a 和 2b 同一个提交，要么明确写下「1a 单独存在是危险态」并指名关闭它的闸门。

### P2-P　`<string>` 守卫要用和它保护的循环**同一个**过滤表达式

守卫写的是 `foreign_key_columns | rejectattr('is_repeated') | selectattr('data_type','equalto','string')`，
它保护的反向 FK 循环是 `{% for col in table.foreign_key_columns if not col.is_repeated %}`（`:80`）。
今天一致纯属措辞巧合。→ 用同一个表达式，或干脆去掉 `rejectattr` 用更宽的
`foreign_key_columns | selectattr('data_type','equalto','string') | list`（repeated string FK 零实例，更宽严格安全）。

### P2-Q　`PROTOBUF_FUTURE_ADD_EARLY_WARN_UNUSED` **确实存在**（原稿说没有，是错的）

在 build 真正使用的那棵 protobuf 里：
`third_party/grpc/third_party/protobuf/src/google/protobuf/port_def.inc:145`
（`table.vcxproj:288` 的 include 列表里就是这条路径，原稿去搜的 `install_vs2026` 不是这棵）。
它是**纯警告属性**（`__attribute__((warn_unused))` / `ABSL_ATTRIBUTE_WARN_UNUSED`），无链接/可见性效应，
**声明上省略是合法的**。→ 这条不推翻 ②，反而**加强**它：现在前向声明的依据有两条独立事实，
protoc 自己写了裸的 `class MirrorTable;`（`mirror_table.pb.h:56`），且定义上那个属性（`:81`）可省。

### P2-R　丢掉 `inline` 的另一半（诚实账）

方案只把丢 inline 记成损失（失去跨 TU 内联）。另一半是：**它把一个静默 ODR 隐患变成硬诊断**。
今天两个 fk 头若发出同名自由函数会在 `inline` 下静默合并；拆开之后是 LNK2005。
今天不存在冲突（名字全部带 sheetname 前缀），但这是完整的权衡。
拆开还买到一条**可测属性**：声明式头可以独立编译 —— 今天没有任何东西保证这一点。

### P2-S　已核实不受影响，记录下来省得重查

- `*_table_comp.h`：`mission_table_comp.h:1-5` 只 include `<cstdint>` `<span>` `<string_view>` 和自己的 pb.h，
  **从来没碰过** `table_expression.h` 或 muduo，①③ 够不到它。
- `code/bit_index/`：只有两个文件（`mission_table_id_bit_index.h`、`reward_table_id_bit_index.h`），不受影响。
- ② 与 ③ 的唯一交互点：2b 之后每个生成 `*_table.h` 裸包含 `"table_log.h"`，
  而新的 `*_table_fk.cpp` include 表头，所以它也需要 `cpp/libs/engine/config/` 在路径上 ——
  `table.vcxproj:288` 和 `CMakeLists.txt:34` 都有，**能用**，但方案从没写下这条耦合。

---

## 5. 明确不要做的事

- **不要上 PCH。**（UE 侧 UBT 默认开共享 PCH 是它自己的默认，与本批无关，而且 UE 已推迟。）
- **不要造伞形头。** `table_log.h` 是 8 行、1 个函数声明、1 个 std include —— 它一旦长出第二个不相关的声明，
  就变成了被禁止的那个东西。
- **不要碰 `cpp_config.h.j2:15`** 那句 pb.h include（它是 `<string>/<utility>/<cstddef>` 的唯一来源），
  也不要在本批里给头补这三个 std include（单独的清理，单独批准）。
- **不要动 `#include <random>`**（`cpp_config.h.j2:7`）：`RandOne()` 在 `:196-201`，
  不在任何 `{% if %}` 里，21 张表全发。要摘掉它就得把 `RandOne` 声明留头、定义搬 .cpp ——
  形状同 ② 但作用于主 manager。顺带一提 `RandOne`/`Where`/`First`/`FindByIds` 在 `cpp/` 下
  **生成树之外零调用点**，21 个头里都是死重量。**另案另批。**
- **不要把 UE 塞进这一批。**（理由见决策 C。）
- **不要改 `exporter_config.yaml:117-118` 和 `:137-138` 那两条过期注释**（它们说 `_deploy()` 只认 cpp/go/java，
  实际 `orchestrator.py:170-194` 六门全有）—— 纯注释改动，会污染本批 diff。另开小单。

---

## 6. 一句话总结给执行者

按 `0a → 0b → 1a → 1b → 2a → 2b →（3a → 3b → 3c）` 走，
**`2a/2b` 必须紧跟 `1a/1b`，中间不许插别的**（§4bis P1-H），
每一步后面跑 **`dev.bat build` 整解决方案 + Linux 一遍**，
两棵生成树一起提交，`3a/3b` 成对回滚。

动手前先把这五条对一遍：
- §4 **P0-A** 两份 muduo，`ERROR` vs `ERROR_` —— 函数体只许用 `LOG_ERROR` 宏
- §4 **P0-B** `table_expression.h` 还 re-export 了 `type_define.h`，① 不是零风险
- §4 **P0-C** 别跑 `run.py`，会重写 `.json/.pb/manifest`
- §4bis **P0-D** ③ 不是零消费方改动，有 5 个文件要补 muduo
- §4 **P1-D** `TableLookupLogMissing` 定义放手写 `table_log.cpp`，别放生成的 `all_table.cpp`
