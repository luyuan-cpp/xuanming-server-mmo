# 跨 zone 场景传送:交接说明(2026-09-20)

> **给接手的 Claude Code 会话**:整份读完再动手。每一条都标了证据等级——
> 「**已核实**」= 本会话在代码上沿路径走通过;「**上一会话报出,未核实**」= 只见于上一会话的聊天记录,本机没有对应代码,无法复核。
> 设计与落码记录的权威出处是 [`cross-zone-scene-travel.md`](./cross-zone-scene-travel.md)(本轮内容在 §12);本文只讲"现在是什么状态、接下来做什么、别踩什么"。
> **这条链路的全部服务端代码从未被编译、从未跑过任何测试。** 任何"已修"都应读作"已落码,待验证"。

## 0. 三十秒版

- 服务端:上一会话留下的 4 个 P1 已全部处理,另外一轮只读审计 + 反驳式核实又确认了 8 条缺陷,其中 3 条已落码、5 条 + 若干 P3 写成已知限制(§12.3),全部在 `origin/main`。
- 客户端(`mmorpg-client`):**推送没有成功**。远端 `main` 仍停在 09-14 的 `d2b165a`,约 542 MB 提交只存在于 luyuan 那台机器上。这是现在**唯一真正阻塞**的事,见 §2。
- 最高优先级永远是**把它编出来**(§4),其余一切结论都悬在这上面。

## 1. 环境事实(不读会踩)

1. **两台开发机。** 提交作者 `luyuan` 的那台(下称机器 A)持有客户端的全部新提交,并跑着每小时 `git add -A` 的自动保存;本文写于另一台(机器 B,git 用户 `luhailong`),机器 B 的 `mmorpg-client` 是 09-14 的旧基线。接手任何记录之前先 `git cat-file -t <sha>` 与 `git ls-remote origin`,确认那些提交在不在你这台机器 / 远端上。
2. **多个 Claude 会话共用同一个 main 工作树**,都直接往 `main` 提交并推送。纪律:动手前用 `ListAgents` + `SendMessage` 互报文件范围;提交一律 `git commit -- <显式路径>`,提交前 `git diff --cached` 逐行看新增行是不是自己的;不 stash / reset / checkout / rebase;`PROGRESS.md` 只在末尾追加且放到最后一步。机器 A 上还要提防每小时自动保存把半成品扫进 main。
3. **Claude 不编译、不跑测试、不 regen**(`AGENTS.md` §10.1)。改完给出可直接执行的命令与通过标准;没有运行结果之前不得写"通过 / 已验证"。
4. 机器 B 有 `gofmt`、`node`、`pwsh`,**没有 python、没有 promtool**。

## 2. 必须在机器 A 上做的事:客户端

### 2.1 把客户端推上去(阻塞项)

上一会话的推送失败过多次,根因与做法如下(**上一会话报出,未核实**——机器 B 看不到那些提交;但远端现状"`main = d2b165a`、无任何含 `3447cc1` 的分支"是本会话 `git ls-remote` **已核实**的,说明分段推的中间分支也没留下):

- 症状是 83 MB 的包写到 100% 后才报 `fatal: the remote end hung up unexpectedly`,上行约 250 KiB/s——chunked 传输的典型死法。做法:`http.postBuffer` 调到 600 MB(让 git 用 Content-Length)、钉 `http.version=HTTP/1.1`、放宽低速中断阈值、**逐个提交推**(`git push origin <sha>:refs/heads/<临时分支>`,最后再推 `main`)。
- **判 `git push` 自己的退出码**,不要判管道末端 `tail` 的——上一会话有一轮"全部 OK"是假的。
- 停掉一个后台推送时,`TaskStop` 只杀 shell,`git` / `git-remote-https` 子进程会继续占着上行。重推前先查进程树并清掉孤儿,也不要让多个会话同时推大包(当时有三个上传在抢同一条链路)。
- 那 542 MB 里有约 320 MB 集中在一个自动保存提交(上一会话记的是 `4fb0515`,09-18 06:58):`Assets/Editor/CityTiles4KReview/Tiles/` 下几十张 15–18 MB 的 4K 审图 PNG。这是仓库卫生问题,**清掉要改写历史,而那条分支上有别的会话在提交,必须由用户拍板**;不改写历史的话,至少加进 `.gitignore` 或挪出 `Assets/`(Unity 会给每张图生成 `.meta` 并导入)。

### 2.2 客户端剩余项(均为**上一会话报出,未核实**)

| # | 位置 | 要做什么 | 怎么算做完 |
|---|---|---|---|
| C-1 | `GameClient.DescribeTravelTip` | 漏了 `kEnterSceneSceneNotFound`,该码目前落到裸编号文案 | 该码有对应中文文案;对照 `generated/code/proto/tip/scene_error_tip.proto` 把传送链上会回到客户端的码过一遍 |
| C-2 | `SceneErrorTip.cs` | 缺 `.meta`,换机器后 Unity 会重新生成 GUID,引用它的资源会断 | `.meta` 进版本库 |
| C-3 | `CityTravelUiRoot.cs:303` | 城内传送窗口仍是"见任何 tip 就收场"并拼裸编号;`GameClient.IsTravelFailureTip` / `DescribeTravelTip` 已是现成的 `public static`,两行就能接上 | 只对传送失败类 tip 收场,文案走 `DescribeTravelTip` |
| C-4(本轮新增) | 传送失败 tip | 服务端现在会对**没有 `player:zone` 映射的玩家**的 TravelToZone 回 20(`ErrHomeZoneUnavailable`),源 scene 解冻后回失败 tip | 确认客户端对这次失败的表现是"提示 + 留在原地",不是卡在传送中 |
| C-5(决策前置) | 踢线消息 `SceneClientPlayerCommonKickPlayer`(34) | "冻结硬上限"的设计(§12.3)到期后要靠它让客户端回登录 | 回答一个问题:**客户端收到 34 之后是否一定断线并回到登录界面?** 答案决定那份设计能不能落 |

## 3. 服务端:本会话做了什么

全部在设计文档 §12.1,这里只列要点与**对上一会话说法的更正**(别沿用旧说法):

- **P1-1 告警恒空**:已修;并且**多查出两条 critical 的 `*PoolEmpty` 同样恒空**(gauge 从不发布 0 值,`== 0` 永远匹配不到,这两条 pager 从未响过)。`scene_gone` 指标的发射点当年被误删,已恢复。
- **P1-2 Redis 断开时 handoff 标记撤不回**:已落码(待撤回表 + 条件删除 + 换图闸口)。更正:触发面比上一会话描述的**窄**(正常登出 / 租约到期后重登不受影响,只有 30s 租约内重连回同节点才中),但另有一个**更糟的在线变体**(`AbortTravelHandoff` 的撤回同样被静默跳过,不用重登就能命中),一并修了。
- **P1-3 失效 runbook**:重写为 v2,观测点逐个 grep 核对;文件头声明"静态编写、未实跑",首次执行发现不符要回改文档。
- **P1-4 路由文档教人清 location**:逐句核对改写。结论:手工清 location **不会**绕过 epoch 铸造,但**会**绕过换手门;唯一安全前提是源区 scene 节点已全部注销 + 负载集为空 + 再入屏障已过,而此时 scene_manager 本来就会自动忽略该记录——所以手工清只是卫生动作,首选 `merge_zone -clear-source-hot-state`。**绝不能删 `player:{id}:owner_epoch`。**
- **"交接冻结没有服务端上限"——这条大部分不成立。** 已有两道 30s 看门狗,应答丢失 / scene_manager 卡住 / 存盘回调不来都有界(最坏约 60s < 客户端 75s)。真正无上限的只有 zone Redis 不可用或半开时的三段。有完整设计稿但**未落码**,卡在 C-5。
- **审计新增并已落码**:未映射玩家跨 zone 传送会把访客存盘写进目标 zone 的库(GO-1);gate 对"同 scene_id、不同节点"的路由不转发进场(CPP-1)。
- **§10.2 的 R7 声称已解决"访客掉线后从别区 gate 登录",实际从 login 走不到**(login 恒发 `ZoneId = 本 zone`),见 §12.3 GO-5。

**新增上线前置**:对存量号开放跨 zone 传送之前,必须先跑完 `tools/merge_zone -backfill-home-zone`(§12.2)。

## 4. 服务端:剩余工作(按优先级)

1. **编译 + 单测 + 冒烟**(阻塞其余一切)。命令、通过标准、失败时保留什么:设计文档 §12.4,在 §9 / §11.4 基础上追加。main 上还叠着组队、战斗 G1–G9、帮会二期、聚宝斋、friend 等多批未编译代码,总顺序见 `turn-battle-gap-closure.md` §7;C++ 必须串行 `msbuild … /m:1 /nr:false`。
2. **§12.3 的 P2 五条**,每条都写了"为什么没修 / 修法":
   - GO-2 回滚让 epoch 回退、毒化 db 守卫——要改 proto(`EnterSceneResponse` 回显 epoch)或调 C++ 发 DBTask 的时机;
   - GO-3 同 node_id 重注册后死节点接管永不触发——要在 `PlayerLocation` 里记进程实例标识(proto);
   - GO-5 R7 / R8 从 login 不可达——要动 `go/login`(本轮有「帮会二期」会话在改那个目录,先对一下);
   - CPP-2 gate 丢路由无补发、CPP-3 疏散改派 fire-and-forget——都需要"补发 / 待确认表 + 超限发 tip 并踢线"的设计,和冻结硬上限共用 C-5 这个前置。
3. **需要随一次全量 regen 做的**:真删 `proto/common/event/player_migration_event.proto`(整条 DEPRECATED、无调用方,仍占着 event id 27 / 41,清单写在该文件头注释里);`proto/common/database/player_cache.proto:7` 与 `bag_quest_mail_data.proto:7-8` 的注释仍把 `PlayerAllData` 说成"跨 zone 迁移携带的快照"。「Friend 服务移植后续工作」会话计划跑 regen,可与它同批。
4. **本轮因归属没碰、只登记的过期文字**:`go/login/internal/logic/clientplayerlogin/entergamelogic.go:470`、`:575` 附近(仍写 14 `ErrUnsafeCrossNodeHandoff` 与"重定向前先解析场景");`docs/design/guild-phase2/04-asset-channel.md:29 / :300 / :335 / :352`(C6 的正确性论证依赖已删的"迁移包");`cpp/libs/services/scene/battle/system/player_battle.cpp:838`;`docs/notes/` 下 3 处。
5. **P3**:GO-6(`death_at` 写失败仍 ZREM——注意"写失败就不 ZREM"是错的修法)、GO-4 残余(`LeaveScene` 改 compare-and-delete)、告警盲区(整 zone 节点全灭无从触发)、`Hiredis.cc` 在 `redisvAsyncCommand` 返回 ERR 时泄漏 `CommandCallback`、`robot/connected()` 是个被 git 跟踪的 0 字节空文件(疑似 shell 重定向误操作残留)。

## 5. 给接手会话的开场提示词(可整段粘贴)

```text
你在接手「跨 zone 场景传送」的收尾。先按 AGENTS.md 的会话启动门禁读完必读文件,再读
docs/design/handoff-crosszone-20260920.md 和 docs/design/cross-zone-scene-travel.md 的 §12。
注意:
1. 这条链路的服务端代码从未编译、从未测试;你不许编译 / 跑测试 / regen,改完给出可执行命令。
2. 同一个 main 工作树上有别的 Claude 会话在改文件:动手前 ListAgents + SendMessage 互报范围,
   提交只用 git commit -- <显式路径>,不 stash / reset / checkout / rebase。
3. 先确认你在哪台机器上:git -C ../mmorpg-client log -1 如果停在 09-14 的 d2b165a,你就在机器 B,
   客户端的活(交接说明 §2)在这台机器上做不了,只能做服务端(§4)。
   如果能看到 ZoneTravelClient.cs / GameClient.IsTravelFailureTip,你在机器 A:第一件事是 §2.1
   把客户端推上去,推之前先查有没有残留的 git-remote-https 进程,推的过程判 git push 自己的退出码。
4. 交接说明里标「上一会话报出,未核实」的条目,先在代码上核实再动手,不要默认它成立。
现在先告诉我:你在哪台机器上、origin/main 与本地 HEAD 各是什么、客户端远端 main 是什么,然后给出你打算做的第一件事。
```
