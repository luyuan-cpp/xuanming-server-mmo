# 只保留 main:分支和 worktree 清理方案(2026-11-01)

负责人要求:仓库只保留 `main`。这份给 Codex 执行,脚本是同目录的 `main-only-cleanup-20261101.ps1`。

## 当前状态(2026-11-01 23 点)

- **22:44 跑过一次 `-Apply`,没有清完**。脚本有 bug(`commit-tree` 的 `-p` 没加引号),11 个有未提交改动的 worktree 留底失败,所以都保留了,没有丢东西。删掉的是 4 个没有未提交改动的 worktree、2 条失效记录和 8 个分支;这 8 个分支都在 `refs/archive/branch/*`。
- **bug 已修**,`-ArchiveOnly` 实际跑过一遍:`refs/archive` 共 26 条(15 个分支、11 个 worktree),`Problems: 0`,没有删除。
- **还要做**:删掉剩下的 11 个 worktree 和 6 个分支。
- **要等 Codex 第二轮验证跑完,主工作区没有别的进程在写,再跑 `-Apply`**。脚本比较清理前后的工作区状态,上次第二轮同时在跑,所以报了 `The main checkout status changed during cleanup`,这条是误报。

## 硬性约束:不能把旧设计带回来

项目已经换成玩家存档只放 TiDB,`player_store` 一个事务提交;战斗结算只走 match SQL 加 player_inbox。这些分支和 worktree 里的是换之前或换到一半的东西,不是待合并的代码。

- **这次只删除,不往 `main` 放任何内容**:不 merge、cherry-pick、`git apply`,也不从分支、worktree 或留底里拷文件。看到分支有 `main` 之外的提交,也不要合。
- **这些不能回来**:Redis 存玩家数据(父快照 `PlayerAllData`、子表缓存、存盘意图和补投)、开关 `PlayerStorePath`、Kafka 存盘管线、`settlement_outbox.h`、结算账本、`battle:settlement:pending`、结算发件箱、单 zone 回档脚本 `k8s_zone_rollback.ps1`。
- **留底只防误删,不代表要恢复**。要从留底里拿东西进 `main`,必须负责人单独批准。
- **怎么证明没带回来**:清理前后 `main` 的 HEAD 和工作区状态完全相同。脚本会核对这一条,不同就写进 `Problems`。

## 结论:两个分支都不合并

`main` 上已有的提交都不动。下面两个分支各有 1 个提交不在 `main` 的历史上,但内容已经被取代,合并会把删掉的旧设计带回来。

| 分支 | 提交 | 为什么不合并 | 依据 |
|---|---|---|---|
| `claude/recover-battle-settlement-f39089db` | `b5131475b` | 恢复的是旧结算设计:`settlement_outbox.h`、结算账本、`battle:settlement:pending`、发件箱。`main` 在 `65c69f4a9` 改成只走 match SQL 加 player_inbox,这些都删了。其中「按 battle_id 升序逐局应用」也已经是错的,发号器的 battle_id 不单调。锁可靠性那部分已按 `main` 重写 | [回合制战斗缺口](../design/turn-battle-gap-closure.md) §10,`docs/PROGRESS.md` 2026-11-01 |
| `claude/player-store-direct-commit` | `01444196d` | 这批改动 10-30 的中途快照,带开关 `PlayerStorePath`,缺省还走 Redis 旧路径。同一批工作后来继续写,进了 `fc504300b` 及之后的提交:快照的 209 个文件在 `fc504300b` 里都有更新或相同的版本;`idissuer`、`playerstore` 都在 `main`;Go 代码里已没有 `PlayerStorePath`。快照里 `main` 没有的 16 个文件,是后来有意删掉的旧路径(如 `go/shared/playerstorepath`、`tools/scripts/k8s_zone_rollback.ps1`) | 按文件比对。没有逐行证实,所以留底再删 |

`codex/battle-settlement-integration` 同第一行,不合并。它的 worktree 里 69 条未提交改动没有进 `main`,也是有意的。

其余 13 个分支没有 `main` 之外的提交,直接删。

## worktree 里的未提交改动

| worktree | 条数 | 情况 |
|---|---|---|
| `sleepy-dhawan-974040`、`musing-jang-65ade6` | 8、147 | 修复已并回,在 `e33b99351` 里 |
| `peaceful-haibt-6a19a3`、`laughing-wright-5dc98a` | 8、8 | 11-01 逐文件核对过,代码、测试、设计文档都在 `main` |
| `relaxed-cerf-d5da4d` | 24 | 11-01 逐文件核对过,已作废。`main` 按账号单槽重写了 Refresh token,打上这份会把它改回去 |
| `compassionate-dirac-4d87e1` | 9 | 代码已在 `main`。负责人 11-01 定并回的 4 处文档更正已写进主工作区,未提交:`gate-connection-admission-control.md` G5 行、`global-singletons-p0-measure-steps-20261014.md`、`deploy/k8s/README.md`、`docs/PROGRESS.md`。其中 `DisableLegacyPerNodeTopic` 那条已过时,按现状改写 |
| `wt-recover-battle-settlement` | 69 | 同上表第一行,不并回 |
| Codex 的 `ac12` | 303 | **没有核对过**,是旧的 Codex worktree,改到 go 和 C++。留底,不并回;要用时按下面的办法取回 |
| Codex 的 `f452`、`session-fence-6281`、`buff-idle-duration` | 8、52、10 | **没有核对过**。`f452` 全是文档,`session-fence-6281` 大多是生成物,`buff-idle-duration` 的子模块目录已坏。留底,不并回 |

条数不含子模块。这些改动最晚到 10-30,都留底,所以删掉之后还能取回。`AppData/Local/Temp` 下的 `p0-measure-2f897bd07`、`xm-stable-bd2b9715c` 已经不是有效的 worktree,脚本只清它们的记录,不删目录。

## Codex 怎么做

在 `D:\luyuan\xuanming-server-mmo-cpp` 根目录执行。**不要** merge、rebase、cherry-pick、reset、checkout、stash、push,不要 `git gc --prune`,不要动 `main` 工作区的未提交改动和子模块。

1. 确认 `D:\luyuan\wt-recover-battle-settlement` 上的自动续跑(automation-2)已经停了,也没有 Codex 会话还在用这些 worktree。还在用就停下来告诉负责人。
2. 只读试跑,把输出记下来。清单和上面对不上就停:

   ```bash
   pwsh -NoProfile -File docs/handoff/main-only-cleanup-20261101.ps1
   ```

3. 真正执行:

   ```bash
   pwsh -NoProfile -File docs/handoff/main-only-cleanup-20261101.ps1 -Apply
   ```

   它按顺序做:分支写进 `refs/archive/branch/*`;有未提交改动的 worktree 把当前内容(含未跟踪文件,`.gitignore` 忽略的和子模块不算)写成提交,放进 `refs/archive/worktree/*`;删 worktree;`git worktree prune`;`git branch -D`。分支留底失败就停,还没删任何东西。某个 worktree 留底失败,或者 24 小时内有改动,就保留它,写进结果。

4. 脚本结束后核对,结果也在 `build/main-only-cleanup/result.txt`:
   - `git branch --list` 只有 `* main`。
   - `git worktree list` 只有主目录。
   - `git status -sb` 第一行还是 `## main...origin/main`。
   - `Problems: 0`。不是 0 就把列出的项报告给负责人,不要重试,不要手工强删。

5. 把这几项结果回给负责人。

## 删完之后

Claude Desktop 里对应 worktree 的旧会话由负责人归档。

取回一份留底,只用来查阅,不要合进 `main`:

```bash
git worktree add <新目录> refs/archive/worktree/<名字>
```

```bash
git branch <分支名> refs/archive/branch/<分支名>
```

`refs/archive/*` 不会被推送,`git branch` 也看不到。确认不要了再删:

```bash
git for-each-ref --format="delete %(refname)" refs/archive | git update-ref --stdin
```
