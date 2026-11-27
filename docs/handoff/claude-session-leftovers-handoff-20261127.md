# 旧电脑一个会话的剩余事项（2026-11-27，做完删除本单）

> 来源：旧电脑上的「交接单代码改动清零与七项方案入档」会话（Claude Code）。旧电脑已回收。
> 负责人在新电脑说「继续做完 handoff」时，先按 `new-computer-start-here-handoff-20261127.md` §1 做开工核对，再做本单。
> 本单只列这个会话剩下的事。已被别的交接单接住的只写去哪做，不重复写；两边不一致时，以那份交接单开头的状态行为准。

**规矩**（详见 AGENTS.md 与总入口单 §2）：只在 main 上改，不开分支；代码由 Claude Code 用最高档模型写，Gemini 只做生成、编译、跑测；负责人没说就不提交；Java 版只在 Java 仓 `PARITY.md` 登记待做；问题分析、设计步骤和解决办法都写进设计稿。

## 1. 要写代码的（Claude Code）

### 1.1 账号注销没清江湖恩怨数据（只有本单记着）

- **来源**：r2 第 4 轮查找里，看四条 low 的子代理顺带发现的，当时没深核。这条记在第 4 轮交接单 §6.1 待做清单的末条，r2 剩余单没有收进去。负责人定了「现在修，不等以后」。
- **疑点**（行号是 11-27 的，以代码为准）：
  1. match 的删角清理不删 `match_grudge_relation`、`match_grudge_log`。这两张表的读写在 `go/match/internal/authority/jianghu_grudge.go`。先找到 match 处理注销和删角的入口，看清理清单里有没有这两张表。
  2. guild 的恩怨墙表 `guild_jianghu_grudge_record` 存了双方队长名。`go/guild/internal/data/jianghu_grudge_repo.go` 约 80–89 行从 `GetChallengerCaptainName()` / `GetTargetCaptainName()` 取名字，约 144 行写入。
  3. 注销设计 `docs/design/account-deletion.md` 的 §6.2 清单没给这三张表归类。
- **做法**：
  1. 先按 §6.2 里已有各行的口径逐条复核，确认疑点属实。
  2. 再定每张表是删除，还是保留但去标识。建议的默认做法：恩怨关系和流水是玩家个人的数据，随角色删除；帮会恩怨墙是帮会的历史，保留战绩，只把队长名换成注销占位名。复核时如果 §6.2 有同类先例，就跟先例走。
  3. 写代码，配单测。真库用例照 match、guild 现有 TiDB 用例的写法。
  4. §6.2 补三行，写明理由。PROGRESS 追加一条，写进 Gemini 要跑的命令。Java 只登记。
- **完成标准**：代码和用例写完，§6.2 已补，PROGRESS 已追加，Gemini 跑绿。

### 1.2 guild 人工终结的条件写 SQL 带上 `attempts`

r2 剩余单已经列了这条（`go/guild/internal/data/asset_store.go` 约 259 行，`sqlResolveAssetOpQuiet`），按那边做，本单不重复。背景：见证口径已经改成观察窗只比租约和最后发送时刻（`go/shared/assetop/manual_evidence.go` 的 `LeaseAndSendUnchanged`），条件写那一步带上第二次读到的 attempts。trade 早就这样写了，guild 那条 SQL 还没补。

## 2. 只交 Gemini 跑的（本会话的代码已写完）

本会话全部代码改动的验证命令，都在 PROGRESS 2026-11-26「两份新交接单的代码改由 Claude 写」一条，以及它指向的第 4 轮交接单 §6.1、`review-suggestions-remaining-handoff-20261126.md` §2/§3。总入口单 §4 已经把它们列进去了。为防漏，要点再写一遍：

- **先导表**：`Tip.xlsx` 新增 `SkillBookshelfFull`、`SkillWantedPosterBookshelfFull` 两码（脚本 `tools/scripts/craft_bookshelf_cap_xlsx_patch.py`，可重复跑）。导之前先看一眼 `git status data/`，再 `proto-gen-run`。
- `tools/dbmove`：`go test -count=1 ./...`，里面有修复身份的 5 条新用例。
- `go/db`、`go/data_service`：`go vet ./...`、`go build ./...`，覆盖迁移完成后提醒重跑授权的日志。
- `go/shared`：`go test -count=1 ./assetop/`。`go/trade`、`go/guild`：`go test -count=1 ./cmd/assetopfix/`，里面有新用例 `TestRunResolve_ObservationWindowToleratesAttemptsOnlyAndUsesTheSecondRead`。
- TiDB 列权限：照 `docs/design/tidb-phase1-and-launch-readiness.md` §2.6 核对。版本钉在 v8.5.5，按定案保持表级授权。
- 已知的旧格式问题：`go/shared/assetop/abandon_test.go`、`go/trade/cmd/assetopfix/main_test.go` 各有一处 gofmt 不齐，不是本会话造成的。Gemini 报出来时交回 Claude Code，顺手 gofmt。

## 3. 怎么收尾

1. §1.1 写完，§1.2 在 r2 剩余单那边做完，§2 全绿并写进 PROGRESS 之后，删掉本单。
2. 删掉 `docs/README.md` 里本单那一行：先跑 `python tools/scripts/gen_docs_index.py --check`，再生成，diff 只许删本单那行。
3. 把总入口单 §3 表里本单那一行划掉。
4. PROGRESS 追加一条，写明删了本单。原文用 `git show <删除前的提交>:docs/handoff/claude-session-leftovers-handoff-20261127.md` 取。
