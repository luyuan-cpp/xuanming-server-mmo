# 仓库目录整理:交接说明(2026-10-07)

> 三个提交,基于 `origin/main` 的 `26ceb70ca5`,2026-10-07 经用户同意以快进方式推送到 `origin/main`。
> 推送前未编译、未运行任何测试(AGENTS.md §10.1);源码改动只有注释里的文档路径,验证见 §3。
> 本机的主工作区当时停在一次未解完的 `git pull` 合并上,所以还没有这三个提交,带入步骤见 §2。

## 1. 做了什么

起因是"目录太乱、不清晰"。盘点后的结论:顶层按语言划分(`cpp/ go/ java/ proto/ deploy/ docs/ tools/`)
这副骨架本身是多语言仓库的常规做法,没有问题;乱在骨架之外的四处,本次只处理这四处。

| 乱在哪 | 处理 |
|--------|------|
| GitHub 首页 37 项,混着流水账、审计快照、教学素材、空壳目录、中文文件名 | 减到 32 项(含新增的 `README_zh.md`),每一项都有明确含义 |
| 不该入库的文件在库里:22 MB 调试二进制、压测结果、本机权限白名单 | 取消跟踪并加进 `.gitignore` |
| `docs/design/` 273 个文件平铺,压测记录、交接说明、SLG/MOBA 面试笔记混在设计文档里 | 分出 `stress/ handoff/ notes/ archive/`,剩 212 篇设计文档,并建了按主题分类的索引 |
| README 与实际不符(架构图画错、列着已下线的 centre、Go 只列 5 个服务),各目录没有说明 | 根 README 重写,每个顶层目录补 README |

### 1.1 根目录变化

| 原路径 | 现路径 |
|--------|--------|
| `AUDIT.md` | `docs/archive/cross-zone-merge-rollback-audit-2026-05.md` |
| `PROGRESS.md` | `docs/PROGRESS.md` |
| `output/teaching*` | `docs/teaching/` |
| `run/README.md` | `docs/ops/run-directory.md` |
| `启动服务器.cmd` | `start-server.cmd` |
| `启动服务器并打开游戏.cmd` | `start-game.cmd` |
| `lib/lslib.bat` | 删除 |
| `bin/` 下的 k8s 机器人压测清单、一次性脚本、旧 Lua 脚本 | `tools/archived/` |
| `.claude/settings.local.json`、`.claude/scheduled_tasks.lock` | 取消跟踪(磁盘上保留) |

`lib/`、`run/`、`output/` 不再出现在版本库里;前两个仍会在编译、运行时自动创建。

### 1.2 docs 变化

| 去向 | 内容 | 数量 |
|------|------|------|
| `docs/stress/` | `stress-*` 复盘与 `prev-summary*.txt` 基线 | 29 |
| `docs/handoff/` | `handoff-*`、`friend-handoff-*`、会话小结 | 8 |
| `docs/notes/slg-moba/` | `slg-*`、`moba-*` | 15 |
| `docs/archive/` | `centre_decommission_*`(centre 已下线) | 9 |
| `docs/notes/event-priority*.md` | 原 `docs/architecture*.md`(内容是事件优先级笔记,不是架构文档) | 3 |

判据:只搬没有被生成物引用的文档。被源码注释引用的 15 处已同步改路径;`docs/PROGRESS.md` 是流水账,
只修了它自己的相对链接,正文里的历史路径没有改。

规范同步:AGENTS.md 的会话门禁、§5(决策记录入口)、§6.2(压测基线位置)已改为新路径。
压测复盘今后写到 `docs/stress/`,`tools/scripts/stress_round19.ps1` 的基线默认路径也已跟着改。

## 2. 本机主工作区怎么带入

远端已经是整理后的样子。本机主工作区(`E:\work\xuanming-server-mmo`)要先把那次进行中的合并
(4 个冲突文件)解完并提交,再拉一次远端。试合并显示,这三个提交与本地 main 的冲突只有那次合并
自己的 4 个文件,没有带来新的冲突。

```powershell
cd E:\work\xuanming-server-mmo

# 1. 先解完并提交进行中的合并(PROGRESS.md、agones_rest_client.cpp/.h、k8s_deploy.ps1)

# 2. 备份本机权限白名单(这三个提交把它从版本库删掉,拉取时可能连磁盘文件一起删)
Copy-Item .claude\settings.local.json E:\work\_git-backups\claude-settings.local.json.bak

# 3. 拉取
git pull

# 4. 如果第 3 步对 .claude/settings.local.json 报 modify/delete 冲突:
git rm --cached .claude/settings.local.json
#    然后无论是否冲突,把备份拷回来
Copy-Item E:\work\_git-backups\claude-settings.local.json.bak .claude\settings.local.json

# 5. 刷新文档索引(期间别人可能新增了文档)
python tools/scripts/gen_docs_index.py

# 6. 整理用的工作副本可以删了
git worktree remove E:\work\xuanming-server-mmo-wt-layout
```

带入之后注意三件事:

- `E:\work\启动服务器.cmd` 与 `E:\work\启动游戏.cmd` 已改成新旧文件名都认,不需要再动。
- 合并前别人新加在 `docs/design/` 下的压测复盘、交接说明不会自动搬家,按 §1.2 的规则手动归位。
- 其他会话如果还在往根目录写 `PROGRESS.md`,会重新造出一个根文件;把内容并进 `docs/PROGRESS.md` 即可。

## 3. 请 Codex 验证

这三个提交的源码改动全是注释,理论上不影响编译;下面的验证是为了把"理论上"变成证据。
在带入这三个提交之后的 main 上执行,按顺序,任何一步失败就停下并保留输出。

| 步骤 | 工作目录 | 命令 | 通过标准 |
|------|----------|------|----------|
| 文档索引 | 仓库根 | `python tools/scripts/gen_docs_index.py --check` | 退出码 0 |
| 脚本契约测试 | 仓库根 | 对 `tools/scripts/tests/*.tests.ps1` 逐个 `pwsh -NoProfile -File <路径>` | 全部退出码 0 |
| 启动器自检 | 仓库根 | `pwsh -File tools/scripts/start_game.ps1 -CheckOnly` | 与合并前结果一致 |
| Go(改了注释的模块) | `go/friend`、`go/login`、`go/player_locator`、`robot` | `go build ./...` 然后 `go vet ./...` | 无错误 |
| Java | `java/gateway_node` | `mvn -q compile` | 成功 |
| C++ | 仓库根 | `msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64` | 0 个错误(C++ 只改了 `etcd_service.cpp` 里一行注释) |
| 一键启动 | 仓库根 | 双击 `start-server.cmd` | 一区开放,与改名前行为一致 |

## 4. 这次没有动的部分

下面这些也属于"不够清晰",但每一项都要改构建路径、生成器配置或部署清单,不编译就无法确认没改坏,
所以没有盲改。按"收益 ÷ 风险"排序,建议一批一批做,每批做完由 Codex 全量编译并跑冒烟。

| # | 问题 | 为什么不能盲改 |
|---|------|----------------|
| 1 | `generated/code/python/`、`generated/code/ue/` 共 159 个文件没有消费者 | 要在导表器配置里关掉这两种输出,再过 CI 的产物漂移检查 |
| 2 | `go/battle/`、`go/team/` 是只有消息号常量的空壳目录 | 由协议生成器按域输出,要改生成器配置并确认没有 import |
| 3 | `robot/` 根目录平铺 28 个 `package main` 的 `.go` 文件 | 拆子包要改包名和导出符号,必须编译并跑全部冒烟模式 |
| 4 | 文档有 70 个 `_en` / `_zh` 多语言副本 | 留哪一份是内容决定,需要人来定;删副本本身零引用风险 |
| 5 | Go 服务入口命名不统一(`chat.go` / `match_service.go` / `scene_manager_service.go`),`login/client`、`login/model`、`db/client`、`db/model`、`scene_manager/base` 等是 goctl 的历史输出目录 | `go_services.ps1`、`Dockerfile.go-svc`、k8s 清单按文件名引用入口 |
| 6 | `java/config_node/`(303 个文件)日常不启动,只在导表器 CI 里编译 | 是否保留是产品决定;它是导表器 Java 产物的部署目标 |
| 7 | `bin/` 既是运行目录又放入库的配置 | C++ 节点以 `bin` 为当前目录按相对路径读配置,`Dockerfile.runtime` 与 k8s ConfigMap 挂的是 `/app/bin/etc`,要代码与部署同批改 |
| 8 | `cpp/libs/engine/core/node/system/node/` 这类重复嵌套 | 改 include 路径、vcxproj、CMake,要全量 C++ 编译 |
| 9 | `docs/design/` 文件名下划线与连字符混用 | 被 550 多个源码文件按路径引用,其中 174 行在生成物里,要改 proto 注释并重新生成 |
| 10 | `tools/patches/boost/` 的 13 个 zip 分卷(26 MB)、`robot/vendor/`(1246 个文件) | 关系到离线构建,换成发布附件或不入库都要先验证镜像构建 |

## 5. 顺带发现、没有处理的问题

- **文档里的脚本不存在**:`docs/design/data-consistency-stress-testing.md` 与 `go/test.ps1` 都提到
  `tools/scripts/chaos_test.ps1`,但这个文件不在仓库里,本地能看到的提交历史里也查不到。根 `.gitignore` 曾经整体忽略
  `scripts/`,很可能它从未被加进版本库。本次只删掉了 `tools/AGENTS.md` 里指向它的条目。
- **`.github/copilot-instructions.md` 仍在讲 centre 节点**(第 84、85、203 行),涉及 Kafka topic
  命名的事实,需要懂这部分的人核对后改。
- **`docs/design/onboarding.md` 过时**:login 端口写的是 50000(实际 53000),提到的 `docker compose up -d`
  与 `dev.bat test` 都不存在。新 README 没有引用其中的命令。
- **`robot` 的 `features-smoke` 模式**在 `main.go` 里有分支,但不在 `config.go` 的合法模式清单里。
- **`k8s_deploy.ps1` 仍有 `CentreReplicas` 参数**,`zones.sample.yaml` 仍写 `centre: 1`。
- **22 MB 的 `robot/__debug_bin.exe*` 仍在提交历史里**。取消跟踪只影响今后的提交;要让仓库体积降下来
  需要改写历史并强制推送,这会影响所有已有的检出,没有做。
- **提交历史里有大量 `WIP: hourly save`**。同理,改写已推送的历史代价很大;可行的做法是今后让自动保存
  提交到单独的分支,合进 main 时压成有意义的提交。
