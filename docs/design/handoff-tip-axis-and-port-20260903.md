# 交接清单：tip 码轴收尾 + 玄冥移植未开工项

**日期**：2026-09-03
**用途**：新会话从这里接手。按「谁来做」分三段，不按主题分。
**分工前提**：Claude 只改代码/文档；**编译、构建、导表、压测由人执行**（AGENTS.md §10.1）。

---

## 一、需要你执行（构建 / 环境，Claude 不做）

tip 码轴的**代码已全部完成**，剩下的都卡在需要跑东西：

| # | 事项 | 命令 / 前提 | 通过标准 |
|---|---|---|---|
| 1 | **全量导表** | 需 **protoc 恰 35.1**。仓库自带 `third_party/grpc/install_vs2026/bin/protoc.exe` 是 **31.1**，不能用（签入 gencode 为 7.35.1，跨大版本运行时拒绝加载）。<br>`cd tools/data_table_exporter && python run.py` | exit 0；连跑两次第二次 `git diff` 为空 |
| 2 | **C++ 构建** | `msbuild game.sln /m:1 /p:Configuration=Debug;Platform=x64` | 0 error。本轮只增删了 `table.vcxproj` / `.filters` 的条目，未碰任何 `.h/.cc` |
| 3 | **Java 构建** | `java/config_node/mvnw.cmd -B test` | BUILD SUCCESS（该工程无 Java 测试，只是编译门禁） |
| 4 | **导表器测试**（可选复跑） | 本机无 pytest，需建隔离 venv：<br>`python -m venv <tmp>/venv && <tmp>/venv/Scripts/python -m pip install -r tools/data_table_exporter/requirements-dev.txt`<br>再 `cd tools/data_table_exporter && <venv>/python -m pytest tests/ -q`<br>**不要装进 `D:/luyuan/Pandora-Server/run/localinfra` 那套 3.14t** —— 那是另一个项目的运行时 | **129 passed**（2026-09-05 本机；09-03 时是 73，之后加了 fault 列用例与上游合并带来的用例） |
| 5 | `no-raw-pointer-member` 检查 | 缺 `no_raw_ptr_check.exe` / `clang-query` | 与本次改动无关，装不装随意 |
| 6 | **提交** | 按仓库规则 `git add` / `git commit` 须人明确发话，Claude 与 Codex 都没提交 | — |

> 已验证过的（2026-09-05 本机）：导表器 129 passed；`go/shared` build+vet+serverbase 测试通过；`guild`/`friend`/`match` 各自 build + constants（guild/friend 另含 server 的 in-band 观测）测试通过。
> Codex 上一轮用 35.1 完成过全量导表 + 三语言编译 + 二次跑零差异，本轮改动（filters / 文档 / 删孤儿 / conftest）不影响导表产物。

---

## 二、需要你拍板（Claude 无权决定）

### 2.1 tip 码轴的遗留（小，可延后）

| # | 事项 | 为什么要你定 |
|---|---|---|
| 1 | 删掉 3 份 v1 旧 state 副本 + `core/_old/` + `templates/_old/` | 删的是 **git 跟踪的文件**。三份副本路径见 `tip-code-axis.md` §5 第 6 条，里面的号还是旧的 1..129，下一个人很可能改错文件或照旧号排查线上问题 |
| 2 | `GlobalVariable.xlsx` 的 `to_double` 列 | owner 为空但**有 1 个数据单元格正在被静默丢弃**。是策划表的数据问题，不是本次引入 |
| 3 | 79 个码补中文文案 | 策划的活。直接打开 `Tip.xlsx` 填 B 列空格。**team / mission / bag / cross_server 四组是整组没有文案**，这几个域的错误今天在客户端一个字都显示不出来 |

### 2.2 移植项目：11 条开工前决策（大，阻塞一切）

**整个移植一行代码都没开工**，卡在 `docs/design/xuanming-port-feasibility-20260902.md` §10 的检查清单。原本 12 条，tip 那条（第 3 条）已由本轮实施消化，**还剩 11 条**。

其中 **4 条是硬卡口**，不拍连底座的两组文件清单都只能开一半：

1. **目标定义**：「B 上跑起来」还是「代码进仓库」——决定全部批次的验收口径与总人日（≈205 vs −10%）
2. **go.mod 授权**：允许 `go/shared/go.mod` 加 `go-sql-driver/mysql v1.9.0` + `miniredis v2.35.0`。反方已证「会波及 5-8 个 module」是错的（`shared` 早就直连 require sarama，而 `player_locator` 的 go.mod/go.sum 各 0 行 sarama 且 CI 全绿），实际只波及 friend/guild 两个
3. **调用方鉴权是否上 Redis**：是否推翻 `callerauth.go:52-56` 明写的「刻意不上 Redis 避免抬登录 P99」。不拍则第一批最后一项不开工，其余 8 项不受影响
4. **Codex/构建机环境**：protoc 恰 35.1（本轮已证明这条是真卡口——仓库自带的 31.1 不能用）

另有 **D1 发物入口**（20 人日 + 3 人日 bag 前置）需要点头，它阻塞冷域三件（mail / leaderboard / auction）全部。同时要**协调 `cpp/libs/modules/bag/` 的并发编辑者**——D1 的 C++ 前置得等他们提交。

其余 7 条见该文档 §10。

---

## 三、需要写代码（新窗口的 Claude 做）

按「拍板后才能动」排序：

| # | 事项 | 前提 | 规模 |
|---|---|---|---|
| 1 | ~~**故障分类收口**~~ **已完成（2026-09-05）**：`Tip.xlsx` 加了 `fault` 列，生成 `go/shared/generated/tip/faults.go`，`serverbase.tipFaultCodes` 与 guild 本地 map 删除。见 `tip-code-axis.md` §2.9 / §6.3 | — | 只动了 Go 产物（没动三语言 proto）。**需要你**：跑一次导表器确认 `faults.go` 与 CI 漂移检查一致（本机已用纯 Python 路径生成并 `cmp` 通过） |
| 2 | C++ 侧段表（照 `tip_segments.go.j2` 加个 C++ 模板） | 有需求时才做 | 小。目前只有 Go 的 `serverbase` 需要按段定性，C++ 的 `PlayerTipSystem` 不做分类 |
| 3 | `tip_text.json` 进运行时批次清单（给 manifest 加 extras 概念） | 无 | 小。CI 已用重建+漂移检查覆盖，但运行时校验没有 |
| 4 | **移植底座第 0 批**（≈80 人日） | 上面 §2.2 的 11 条 | 大。含 D1 发物入口、D4 迁移器、D9 可用性契约、D16 入站通道清单等 13 项 |
| 5 | 移植第 1 批冷域三件（54 人日） | 第 0 批 + D1 | 大 |
| 6 | 移植第 2 批社交/组队（69.5 人日） | 同上 | 大 |

---

## 四、当前工作区状态（重要）

- ~~未提交~~ **2026-09-05 更新**：tip 码轴的代码与产物已随 `71edf77f7` 进仓（`PROGRESS.md` 的 09-03「收尾复核」段落除外，它还在工作区）。本轮（fault 列收口）的改动**未提交**，暂存区为空；本文件自身仍是未跟踪文件。本轮改动集的完整清单见 `PROGRESS.md` 2026-09-05「tip 故障分类收口」一节。
- **有并发编辑者**。2026-09-05 时 `git status` 里下列都**不是**本轮的：`cpp/nodes/battle/**`（含未跟踪的 `battle_security.h` / `client/` / `tests/`）、`cpp/libs/engine/core/security/`、`cpp/libs/engine/config/config.cpp`、`cpp/nodes/gate/gate_security.h` 及其测试、`proto/battle/**`、`proto/common/base/config.proto`、`bin/etc/base_deploy_config.yaml`、`robot/**`、`docs/design/moba-battle-target-architecture.md` / `turn-based-battle-server.md`、`third_party/*` 子模块指针。**动手前务必 `git status` 确认归属，不要看 mtime。**
- **`PROGRESS.md` 是混写文件**：同一份未提交 diff 里有三段——09-03「收尾复核」、本轮「tip 故障分类收口」、并发编辑者的「回合制战斗：客户端直连 battle 节点……未编译」。提交时用 `git add -p` 只挑前两段，**不要整文件 add**，否则会把别人未编译的记录一起带进 tip 提交。
- 本轮结束时的验证状态见 `docs/design/tip-code-axis.md` §6（Codex 那轮）与 §6.2（本机复核）。

---

## 五、相关文档

| 文档 | 内容 |
|---|---|
| `docs/design/tip-code-axis.md` | tip 码轴改造全文：问题、机制、重排映射、加码手册、已知残留、两轮验证记录 |
| `docs/design/xuanming-port-feasibility-20260902.md` | 移植定谳：三桶分法、31 项组件去留表、18 份 ADR + 反方、分批计划、§10 开工前检查清单、首两批文件级清单 |
| `AGENTS.md` §4 / §7 | 已登记的新规则：tip 错误码只能由导表器发号，不许手写数字 |
| `PROGRESS.md` 末尾 | 本轮流水账 |
