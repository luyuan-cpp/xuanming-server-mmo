# 交接：`Lookup*OrContinue` 修复收尾 + exporter-tests CI 复活

> 2026-09-14 合并后状态见 §八；前文保留原基线的排查与执行计划，§五已回填本轮实际执行项。

**日期**：2026-09-14
**基线**：HEAD `bb7b1cfc8`（工作区另有他人在途改动，见 §四）
**分工前提**：Claude Code 只改代码 / 文档，**不编译、不跑生成器、不跑测试、不拉栈**；执行交给 ChatGPT，结果按 §五 的表格**回填进本文件**。提交 / 推送须用户明确发话（AGENTS.md §9）。

---

## 〇、一句话现状

`9d77abe55` 修掉了 `OrContinue` 宏的 `do{...}while(0)` 空守卫，但**没有闭环**：HEAD 里仍有 28 个坏宏（漏改了导表器的一级输出树 + 一张在旧模板上生成的新表被合进来），而本该拦住这两件事的 CI 漂移检查**自 09-05 起从未执行**——被前面一步两条 Linux 必红的单测挡住了。修复文件已在工作区，CI 那两条单测本轮已改，均**未提交、未运行**。

---

## 一、事实与证据

### 1.1 修复已提交，但只有绿证据

- `9d77abe55`（09-10）：模板 `tools/data_table_exporter/templates/cpp_config.h.j2` + `cpp/generated/table/code/` 下 27 个表头 + `scene.vcxproj.filters` 删悬空 `mount.h` + `configuration_table_test.cpp` 两条成对回归用例（`LookupGuardMacro.*`）。
- 提交信息称「configuration_table_test 编译 0 错误、39 例全过」；`build/cpp/tests/configuration_table_test.exe`（09-13 03:32）确含新用例名。
- **从未有「改回坏宏 → 用例变红」的证据**（AGENTS.md §11.4 要求红/绿）。

### 1.2 HEAD 仍有 28 个坏宏

| 位置 | 个数 | 成因 |
|---|---|---|
| `generated/code/cpp/*_table.h` | 27 | `9d77abe55` 只改了**部署副本**。导表器先写 `generated/code/cpp`（`exporter_config.yaml:39`），再拷到 `cpp/generated/table/code`（`:45-46`）；一级输出树没改。 |
| `cpp/generated/table/code/activityschedule_table.h` | 1 | `5328412de`（09-11）在**不含修复的平行分支**上用旧模板生成新表，经 `f82329a41`（09-12）合入 main。 |

运行时影响：C++ 编译用的是 `cpp/generated/`；唯一坏在部署副本里的 `LookupActivityScheduleOrContinue` **零调用点**，所以当前没有崩溃面。风险在于任何一次「只拷贝不重导」都会把 27 个坏宏带回编译树。

### 1.3 工作区已含恰好 28 个修复文件

09-13 23:52 有人在本机用已修好的模板重导过表。逐字节核对方法：对每个改动文件取 `HEAD` 版本，套用与 `9d77abe55` 完全相同的变换，再与工作区 `cmp`——**28 个完全一致，恰好覆盖 HEAD 的全部 28 个坏宏**。清单见 §3.1。

### 1.4 CI 漂移检查一周多没执行过

- `exporter-tests.yml` 在 main 上**能查到的每一次都是 failure**（最早 `71edf77f7` 09-05，最近 run `34762472080` / `f5983bd86` 09-13；修复前的 run `34020163705` 同样）。
- 全部挂在第一步「执行单元测试」：`tests/test_manifest.py` 两条用例 `FileNotFoundError: .../out/tables/Mission.json`、`.../Permission.json`。
- 根因：`d007e448e`（09-03）起 `json_gen.py:41` 与 `manifest.py:82` 输出**小写**文件名（注释写明是为修 Linux 读不到文件），测试 `:66` / `:147` 仍按大驼峰读。Windows 大小写不敏感，本机 129 例全绿；Linux CI 必红。
- 后果：其后 8 步全部 `skipped`，包括「校验六门语言的表管理器产物无漂移」——**正是能同时抓住 §1.2 两种情况的那道闸**。

### 1.5 修复后 12 个调用点的语义复核（不改代码）

`continue` 真正生效后，「缺行 → 跳过这一条」的后果逐一核过：

- **buff / 属性 / 背包 7 处**（`buff.cpp` ×2、`buff_impl.cpp` ×2、`actor_attribute_calculator.cpp`、`item_store.cpp` ×4 中的遍历类）：跳过即正确语义。
- **`mission.cpp:98`（接取）**：`CheckMissionAcceptance`（`5328412de` 加入）在任何副作用前逐条校验条件行，缺行 fail-closed 返回 `kInvalidTableData`；同一快照内循环不会再缺行。
- **`mission.cpp:231` / `:266`**：只在「接取后表行消失」时才会跳过，而 `ReloadTables()` 全仓无调用者（`battle_table_fingerprint.h:18`）→ **当前不可达**。
  - 备忘（将来接热重载时必须处理）：`:266` 有序任务缺行应 `break` 而不是 `continue`，否则会越过未知步推进后一格；`:231` 缺行会残留分类索引，消费者 `HandleConditionEvent` 找不到任务会 `continue`，无害。
- 注意：任务系统**已不是死代码**——`player_mission_handler.cpp:22` 的 `AcceptMission` 客户端 RPC 已接通。

---

## 二、本轮 Claude 已改（未运行）

| 文件 | 改动 |
|---|---|
| `tools/data_table_exporter/tests/test_manifest.py:66` | `"Mission.json"` → `"mission.json"` |
| `tools/data_table_exporter/tests/test_manifest.py:147` | `"Permission.json"` → `"permission.json"` |
| `docs/design/handoff-orcontinue-closure-20260914.md` | 本文件 |

§1.3 的 28 个生成文件**不是本轮所改**，只是核实并登记。

---

## 三、需要执行（ChatGPT，按顺序）

### 3.1 隔离提交（用户发话后才执行）

只提交下列 30 个文件，**不得带上 §四 的他人在途改动**：

```text
tools/data_table_exporter/tests/test_manifest.py
docs/design/handoff-orcontinue-closure-20260914.md
cpp/generated/table/code/activityschedule_table.h
generated/code/cpp/activityschedule_table.h
generated/code/cpp/actoractioncombatstate_table.h
generated/code/cpp/actoractionstate_table.h
generated/code/cpp/attributeautoplan_table.h
generated/code/cpp/attributedimension_table.h
generated/code/cpp/attributepool_table.h
generated/code/cpp/attributerule_table.h
generated/code/cpp/buff_table.h
generated/code/cpp/class_table.h
generated/code/cpp/condition_table.h
generated/code/cpp/cooldown_table.h
generated/code/cpp/dungeon_table.h
generated/code/cpp/equipslot_table.h
generated/code/cpp/globalvariable_table.h
generated/code/cpp/item_table.h
generated/code/cpp/messagelimiter_table.h
generated/code/cpp/mirror_table.h
generated/code/cpp/mission_table.h
generated/code/cpp/monster_table.h
generated/code/cpp/pet_table.h
generated/code/cpp/petrule_table.h
generated/code/cpp/reward_table.h
generated/code/cpp/skill_table.h
generated/code/cpp/skillpermission_table.h
generated/code/cpp/test_table.h
generated/code/cpp/testmultikey_table.h
generated/code/cpp/world_table.h
```

暂存前先确认暂存区为空（`git diff --cached --stat` 无输出），把上面清单存成 `files.txt` 后执行：

```bash
git add --pathspec-from-file=files.txt
```

暂存后三项核对（Git Bash，仓库根）：

```bash
git diff --cached --name-only | wc -l
```

期望 `30`。

```bash
git diff --cached -U0 | grep -c '^-.*continue; } } while(0)'
```

期望 `28`。

```bash
git grep --cached -l 'continue; } } while(0)' -- generated/code/cpp cpp/generated/table/code | wc -l
```

期望 `0`。

### 3.2 导表器单测

- **本机（Windows）**：按 `handoff-tip-axis-and-port-20260903.md` §一 #4 建隔离 venv，在 `tools/data_table_exporter` 下执行 `python -m pytest tests/ -q`，期望 **129 passed**。
  **注意：Windows 上改前改后都是绿的，不能当红/绿证据。**
- **红证据**：直接引用 CI run `34762472080` 的日志（2 failed，即 §1.4 的两条）。
- **可选：大小写敏感环境复现**（本机 WSL 只有 `docker-desktop`，不能直接用）。把文件打包进容器，确保跑在 ext4 上而不是 Windows 挂载卷上：
  - 红（HEAD 版本）：`git archive HEAD tools/data_table_exporter | docker run -i --rm python:3.12 sh -c "mkdir /w && tar -C /w -xf - && cd /w/tools/data_table_exporter && pip install -q -r requirements-dev.txt && python -m pytest tests/test_manifest.py -q"`，期望 `2 failed`。
  - 绿（工作区版本）：`tar -C tools -cf - data_table_exporter | docker run -i --rm python:3.12 sh -c "mkdir /w && tar -C /w -xf - && cd /w/data_table_exporter && pip install -q -r requirements-dev.txt && python -m pytest tests/test_manifest.py -q"`，期望全部 passed。
  - 拉镜像或 pip 失败（墙内）时在 `pip install` 后加 `-i https://mirrors.aliyun.com/pypi/simple/`；仍失败就记「未复现」，以 §3.4 的 CI 结果为准。

### 3.3 C++ 回归用例红 / 绿（`configuration_table_test`）

**前置**：工作区含他人重生成的表 `.pb`（`attributerule` / `skill`、新表 `attributeallocratio`），`table.lib` 与头文件可能不一致，漏建会 ODR 违反、运行期才炸。先按依赖序串行重建：`proto → rpc → thread_context → core → table → modules → battle → scene`。该测试另链 `gate / session / infra / config / muduo / third_party / proto_helpers`，这些库若早于对应头文件修改也须重建。MSBuild 一律 `/m:1`、Debug|x64。
**若构建因他人在途改动失败**：记录错误摘要，不要归因到本修复，也不要替他人改代码。

1. **绿**：
   `msbuild cpp/tests/configuration_table_test/configuration_table_test.vcxproj /m:1 /p:Configuration=Debug /p:Platform=x64`
   然后运行 `build/cpp/tests/configuration_table_test.exe --gtest_filter=LookupGuardMacro.*`，期望 **2/2 PASSED**；不带 filter 全量期望 **39 passed**。
2. **红**：把 `cpp/generated/table/code/test_table.h` 里 `LookupTestOrContinue` 的最后一行临时改回
   `    do { if (!(testRow)) { TableLookupLogMissing("Test", tableId, __FILE__, __LINE__); continue; } } while(0)`
   只重编测试工程再运行同一 filter。期望：
   - `OrContinueSkipsIterationWhenRowMissing` **FAILED**（`reachedBody` 实为 1）
   - `OrContinueDoesNotSkipWhenRowPresent` PASSED
3. **还原**：`git checkout -- cpp/generated/table/code/test_table.h`（该文件工作区原本干净，HEAD 即修复版），重编后期望 2/2 PASSED。

### 3.4 推送后 CI（用户发话推送后）

- `exporter-tests`「执行单元测试」期望转绿。
- 之后 8 步是**一周多来第一次真正执行**，可能暴露与本修复无关的漂移。逐条记录；只有指向 `OrContinue` / 生成 C++ 树的失败才归到本项。
- 「校验六门语言的表管理器产物无漂移」中 `generated/code/cpp` 的 OrContinue 部分期望干净（前提是 §3.1 已提交）。

---

## 四、当前工作区里不属于本项的改动（不要动、不要混提）

- `cpp/generated/table/code/` 与 `generated/code/cpp/` 各自的 `all_table.cpp`、`attributerule_table_comp.h`、`skill_table_comp.h`（共 6 个；与 HEAD 的差异不止宏修复）
- 未跟踪：`attributeallocratio_table.*`（两棵树），`cpp/generated/table/proto/attributeallocratio_table.pb.*`
- 已修改：`cpp/generated/table/proto/{attributerule,skill}_table.pb.{h,cc}`
- `third_party/*` 子模块状态；gate 析构修复（按记忆未提交）

---

## 五、回填表（ChatGPT 填写后写回本文件）

| # | 项 | 期望 | 实际 | 证据（日志摘要 / run id / 时间） |
|---|---|---|---|---|
| 3.1a | 暂存文件数 | 原计划 30 | 本轮范围 2 | 28 个生成表头已由远程 `8487d5e1f` 收录；本轮只提交测试与本交接文档 |
| 3.1b | 暂存差异中移除的坏宏行 | 原计划 28 | 本轮 0 | 生成修复已在远程提交，避免重复改写 |
| 3.1c | 暂存区残留坏宏文件 | 0 | 两棵工作树 0 | 逐个扫描当前 OrContinue 宏，未发现 do-while 包装 |
| 3.2a | 本机 pytest 全量 | 129 passed | 129 passed | 2026-09-14 合并后完整 tests 目录，8.87s；独立 test_manifest 为 8 passed |
| 3.2b | 容器 HEAD 版 test_manifest（可选） | 2 failed | | |
| 3.2c | 容器工作区版 test_manifest（可选） | all passed | | |
| 3.3a | 依赖库重建 | 0 error | | |
| 3.3b | 绿：LookupGuardMacro.* | 2/2 PASSED | | |
| 3.3c | 绿：全量 | 39 passed | | |
| 3.3d | 红：MissingRow 用例 | FAILED | | |
| 3.3e | 红：PresentRow 用例 | PASSED | | |
| 3.3f | 还原后 | 2/2 PASSED | | |
| 3.4a | CI 执行单元测试 | success | | |
| 3.4b | CI 表管理器产物无漂移 | success（或仅列与本项无关的文件） | | |
| 3.4c | CI 其余步骤新出现的红 | 逐条列出 | | |

---

## 六、遗留（不在本轮范围）

- **部署副本无漂移检查**：CI 只检查 `generated/code/cpp`，不检查拷贝结果 `cpp/generated/table/code`。只改副本、或漏做拷贝都不会被发现（`9d77abe55` 是反方向——改了副本没改源，这个方向由现有检查兜住）。建议后续加一步逐文件 `cmp`。
- **回归用例的覆盖面**：`LookupGuardMacro.*` 只验证 `Test` 表的渲染结果；某张表用过期模板生成，要靠 CI 漂移检查兜底——所以 §1.4 的 CI 复活是本修复闭环的一部分。
- `tools/data_table_exporter/templates/_old/config_template.h.jinja` 仍是坏形状，仅被归档的 `_old/gen_xls_to_language_config_file.py` 引用；活生成器 `core/generators/config_gen.py:70` 用的是 `cpp_config.h.j2`。
- `tests/test_orchestrator_gate.py:280` 写入大驼峰 `Deterministic.json`，目前不失败，未查其意图。
- `PROGRESS.md:2522` 两处记录已过时：「continue 不会跳过外层 for 的剩余语句」只在 `9d77abe55` 之前成立；「任务系统无入口死代码」已被 `5328412de` 推翻。PROGRESS 是共享日志，未改动，在此登记。

## 七、相关

- 提交：`9d77abe55`（修复）、`5328412de`（旧模板生成的新表）、`f82329a41`（合入）、`d007e448e`（JSON 改小写）
- CI：`34762472080`（09-13）、`34020163705`（09-06）
- 规则：AGENTS.md §9、§10.1、§11.4


## 八、2026-09-14 合并提交检查

- 已快进到 `8487d5e1f`，原计划的 28 个生成表头修复已随远程更新进入主干。本轮提交范围收敛为 `test_manifest.py` 的两行小写文件名修复与本交接记录。
- 使用本机既有 Python 3.14t 运行完整 exporter tests：129/129 通过；Windows 结果不替代 Linux 文件名敏感性验证，推送后的 CI 结果另行核验。
- 沙盒生成不覆盖仓内产物。原始 `--compare` 返回 1：962 项中 952 相同、10 项差异；逐一确认四对 JSON 路径仅大小写不同且内容一致，两个 Go 差异是被 Git 忽略的旧一级输出缓存。正式 `go/shared/generated` 的 126 个文件全部与沙盒部署结果字节一致。两棵 C++ 表头树均无 do-while 包装的 OrContinue 宏。
- 本机证据保存在仓库同级 `merge-backup-20260914-102631` 的 `exporter-tests.log`、`table-sandbox.log`、`generated-verification.json`。本轮未执行历史方案中的 RED 切换、客户端或压测验收。
