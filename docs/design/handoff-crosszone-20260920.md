# 跨 zone 场景传送:交接说明(2026-09-20)

> **给接手的 Claude Code 会话**:整份读完再动手。每一条都标了证据等级——
> 「**已核实**」= 本会话在代码上沿路径走通过;「**上一会话报出,未核实**」= 只见于上一会话的聊天记录,本机没有对应代码,无法复核。
> 设计与落码记录的权威出处是 [`cross-zone-scene-travel.md`](./cross-zone-scene-travel.md)(本轮内容在 §12);本文只讲"现在是什么状态、接下来做什么、别踩什么"。
> **这条链路的全部服务端代码从未被编译、从未跑过任何测试。** 任何"已修"都应读作"已落码,待验证"。

## 0. 三十秒版

- **先看这条**:端到端闭环核查查出一个 P0 —— scene 节点从来没装 SceneManager 的 gRPC 应答处理器,所有 EnterScene / CreateScene 应答被静默丢弃。**同 zone 跨节点换图、副本 / 镜像场景进入一直是坏的**,跨 zone 传送也要靠 30s 看门狗兜。2026-04-16 的一次 regen 丢的,与本轮新代码无关,已修(设计文档 §12.5.1),**必须实跑验证**。
- 服务端:上一会话留下的 4 个 P1 已全部处理,另外一轮只读审计 + 反驳式核实又确认了 8 条缺陷,其中 3 条已落码、5 条 + 若干 P3 写成已知限制(§12.3),全部在 `origin/main`。
- 客户端(`mmorpg-client`):**已推上去**(2026-09-20 晚,远端 `main = 120e2d8`,比 09-14 的 `d2b165a` 多 26 个提交;机器 B 已同步)。客户端那半条链已只读核查:主链闭环,另有 1 条 P1 + 4 条 P2 + 3 条 P3 未改,见设计文档 §12.5.4 与本文 §2.2。
- 传送**后半程**(第二条腿及之后)失败时,服务端对客户端零通知:无 tip、无踢线、无解冻,而那时源实体已销毁,玩家停在进入中。4 条发现同一个根,修法要动 `go/login`(别的会话在改),本轮只落文档,见 §12.5.2。
- 最高优先级永远是**把它编出来**(§4),其余一切结论都悬在这上面。

## 1. 环境事实(不读会踩)

1. **两台开发机。** 提交作者 `luyuan` 的那台(下称机器 A)持有客户端的全部新提交,并跑着每小时 `git add -A` 的自动保存;本文写于另一台(机器 B,git 用户 `luhailong`),机器 B 的 `mmorpg-client` 是 09-14 的旧基线。接手任何记录之前先 `git cat-file -t <sha>` 与 `git ls-remote origin`,确认那些提交在不在你这台机器 / 远端上。
2. **多个 Claude 会话共用同一个 main 工作树**,都直接往 `main` 提交并推送。纪律:动手前用 `ListAgents` + `SendMessage` 互报文件范围;提交一律 `git commit -- <显式路径>`,提交前 `git diff --cached` 逐行看新增行是不是自己的;不 stash / reset / checkout / rebase;`PROGRESS.md` 只在末尾追加且放到最后一步。机器 A 上还要提防每小时自动保存把半成品扫进 main。
3. **Claude 不编译、不跑测试、不 regen**(`AGENTS.md` §10.1)。改完给出可直接执行的命令与通过标准;没有运行结果之前不得写"通过 / 已验证"。
4. 机器 B 有 `gofmt`、`node`、`pwsh`,**没有 python、没有 promtool**。

## 2. 客户端

### 2.1 把客户端推上去(~~阻塞项~~ 已完成,以下留作"大包推送"的经验记录)

> **2026-09-20 晚更新**:用户已从机器 A 推送成功,远端 `main = 120e2d8`。下面的做法与坑不再是待办,保留是因为同一条慢上行以后还会推大包。那批 4K 审图 PNG(`Assets/Editor/CityTiles4KReview/`,53 个文件)**已随推送进了远端历史**,要不要清仍需用户拍板。

上一会话的推送失败过多次,根因与做法如下(**上一会话报出,未核实**——机器 B 看不到那些提交;但远端现状"`main = d2b165a`、无任何含 `3447cc1` 的分支"是本会话 `git ls-remote` **已核实**的,说明分段推的中间分支也没留下):

- 症状是 83 MB 的包写到 100% 后才报 `fatal: the remote end hung up unexpectedly`,上行约 250 KiB/s——chunked 传输的典型死法。做法:`http.postBuffer` 调到 600 MB(让 git 用 Content-Length)、钉 `http.version=HTTP/1.1`、放宽低速中断阈值、**逐个提交推**(`git push origin <sha>:refs/heads/<临时分支>`,最后再推 `main`)。
- **判 `git push` 自己的退出码**,不要判管道末端 `tail` 的——上一会话有一轮"全部 OK"是假的。
- 停掉一个后台推送时,`TaskStop` 只杀 shell,`git` / `git-remote-https` 子进程会继续占着上行。重推前先查进程树并清掉孤儿,也不要让多个会话同时推大包(当时有三个上传在抢同一条链路)。
- 那 542 MB 里有约 320 MB 集中在一个自动保存提交(上一会话记的是 `4fb0515`,09-18 06:58):`Assets/Editor/CityTiles4KReview/Tiles/` 下几十张 15–18 MB 的 4K 审图 PNG。这是仓库卫生问题,**清掉要改写历史,而那条分支上有别的会话在提交,必须由用户拍板**;不改写历史的话,至少加进 `.gitignore` 或挪出 `Assets/`(Unity 会给每张图生成 `.meta` 并导入)。

### 2.2 客户端剩余项

> **2026-09-20 晚更新**:客户端到位后已逐条核实,证据等级从"上一会话报出,未核实"改为下表"核实结果"一列;完整清单(含新查出的 CL-1…CL-8)在设计文档 §12.5.4。**客户端仓本轮一行没改**(AGENTS §9:客户端改动须单独授权)。

| # | 位置 | 要做什么 | 怎么算做完 |
|---|---|---|---|
| — | **核实结果** | C-1 属实(= CL-6,P3,还缺 3014 与 `kSceneTransferInProgress`);C-2 属实(= CL-7,P3,是该目录唯一缺 `.meta` 的文件);C-3 属实(= CL-4,P2,且它会触发 CL-3"UI 上回不了家");C-4 确认:20 到客户端是 3027,表现为"提示 + 留在原地",不卡;C-6 **关闭**:tip 先到时 UI 代次 +1,迟到的应答被丢弃,遮罩不会重开;C-5 **有答案**:收到 34 后客户端一定断线回选服界面(`GameClient.cs:1463-1467`),"冻结硬上限"设计的前置解除 | — |
| C-1 | `GameClient.DescribeTravelTip` | 漏了 `kEnterSceneSceneNotFound`,该码目前落到裸编号文案 | 该码有对应中文文案;对照 `generated/code/proto/tip/scene_error_tip.proto` 把传送链上会回到客户端的码过一遍 |
| C-2 | `SceneErrorTip.cs` | 缺 `.meta`,换机器后 Unity 会重新生成 GUID,引用它的资源会断 | `.meta` 进版本库 |
| C-3 | `CityTravelUiRoot.cs:303` | 城内传送窗口仍是"见任何 tip 就收场"并拼裸编号;`GameClient.IsTravelFailureTip` / `DescribeTravelTip` 已是现成的 `public static`,两行就能接上 | 只对传送失败类 tip 收场,文案走 `DescribeTravelTip` |
| C-4(本轮新增) | 传送失败 tip | 服务端现在会对**没有 `player:zone` 映射的玩家**的 TravelToZone 回 20(`ErrHomeZoneUnavailable`),源 scene 解冻后回失败 tip | 确认客户端对这次失败的表现是"提示 + 留在原地",不是卡在传送中 |
| C-6(本轮新增,待核) | 传送"已受理"应答 vs 失败 tip 的到达次序 | `player_scene.proto:74-76` 写的契约是"应答无错 = 已受理,未成 = **之后**收到 tip";但受理后同步失败(脏数据快路径 + Redis 未连接等)时,tip 会先于应答到达 | 确认客户端不是"先收到无错应答才开传送中遮罩" —— 若是,会先弹一次失败提示、再打开遮罩,只能靠自身超时收起 |
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

0. **实跑验证 §12.5.1 的 P0 修复**(编译之后第一件事)。它激活了约 150 行从未执行过的代码。两条最小验证:① 同 zone 起 2 个 scene 节点 + `AllowUnsafeCrossNodeHandoff=false` 跑一次跨节点换图,scene 日志应出现 `SceneManager.EnterScene deferred (handoff pending)` 并随后起交接重发(修复前这条日志**从不出现**);② 进一次副本 / 镜像场景,确认玩家真的被自动带进去(修复前是"请求发出、无任何后续")。
0b. **补上传送后半程的失败出口**(§12.5.2,4 条同根;客户端面是 §12.5.4 的 CL-1)。客户端核查更正了一点:玩家**不会永远卡住**,客户端等进场有 60s 硬上限,到期打回选服——但看不到失败原因,服务端刻意保留的登录会话 / 票据客户端也完全用不上,所以两端要一起定方案。最小做法是复用 login 已有的 `KickSessionOnGate` 让客户端明确回登录,或在 gate 侧加"已绑会话但 N 秒没收到 RoutePlayerEvent 就踢"的看门狗(顺带兜住 §12.3 的 CPP-2)。`go/login/**` 有别的会话在改,动手前先对范围。其中 S7-1 可单独低成本收敛:第一条腿在 `SceneConfId == 0`(回家的典型形态)时也做一次"目标 zone 有没有任何世界频道"的只读预检。
1. **编译 + 单测 + 冒烟**(阻塞其余一切)。命令、通过标准、失败时保留什么:设计文档 §12.4,在 §9 / §11.4 基础上追加。main 上还叠着组队、战斗 G1–G9、帮会二期、聚宝斋、friend 等多批未编译代码,总顺序见 `turn-battle-gap-closure.md` §7;C++ 必须串行 `msbuild … /m:1 /nr:false`。
2. **§12.3 的 P2 五条**,每条都写了"为什么没修 / 修法":
   - GO-2 回滚让 epoch 回退、毒化 db 守卫——要改 proto(`EnterSceneResponse` 回显 epoch)或调 C++ 发 DBTask 的时机;
   - GO-3 同 node_id 重注册后死节点接管永不触发——要在 `PlayerLocation` 里记进程实例标识(proto);
   - GO-5 R7 / R8 从 login 不可达——要动 `go/login`(本轮有「帮会二期」会话在改那个目录,先对一下);
   - CPP-2 gate 丢路由无补发、CPP-3 疏散改派 fire-and-forget——都需要"补发 / 待确认表 + 超限发 tip 并踢线"的设计。它们与冻结硬上限共用的前置 C-5 **已解除**(客户端收到 34 一定回选服),三者现在都可以落。
   - 客户端侧:CL-2(本机时钟预判票据过期,时钟快 5 分钟的玩家每次传送必败)与 CL-3 / CL-4(UI 自记的"当前所在区"会变脏,变脏后 UI 上回不了家)是最该先修的两处,修法写在 §12.5.4。
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
3. 客户端已推到远端(main = 120e2d8 或更新)。先 git -C ../mmorpg-client pull,确认能看到
   ZoneTravelClient.cs / GameClient.IsTravelFailureTip。客户端仓的改动需要用户单独授权(AGENTS §9),
   未获授权时只读;待修清单在 cross-zone-scene-travel.md §12.5.4(CL-1…CL-8)。
4. 交接说明里标「上一会话报出,未核实」的条目,先在代码上核实再动手,不要默认它成立。
现在先告诉我:你在哪台机器上、origin/main 与本地 HEAD 各是什么、客户端远端 main 是什么,然后给出你打算做的第一件事。
```
