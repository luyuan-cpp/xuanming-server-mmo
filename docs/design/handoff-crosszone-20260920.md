# 跨 zone 场景传送:交接说明(2026-09-20)

> **给接手的 Claude Code 会话**:整份读完再动手。每一条都标了证据等级——
> 「**已核实**」= 本会话在代码上沿路径走通过;「**上一会话报出,未核实**」= 只见于上一会话的聊天记录,本机没有对应代码,无法复核。
> 设计与落码记录的权威出处是 [`cross-zone-scene-travel.md`](./cross-zone-scene-travel.md)(本轮内容在 §12);本文只讲"现在是什么状态、接下来做什么、别踩什么"。
> **验证现状(2026-09-29 更新)**:
> - **2026-09-21**:C++ 由 Codex 按 `msbuild game.sln /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64` 整体编译通过(退出码 0,见 PROGRESS.md 同日 Codex 条目)。那次编译早于同日落码的 Z1 / A′ / GO-5 / epoch==0 铸造(提交 `9cef7b2ec`、`e6b174712`)。
> - **2026-09-28 03:41**:属性数值线代 Codex 又跑了一次 `game.sln`(Debug x64,`/m:1 /nr:false`),exit 0,见 PROGRESS.md 09-28「防御 ×12 / 法力 ×4 联机验收通过」。证据在 `run/verify-attribute-20260928/build-game.log`,该目录被 gitignore,只在机器 A 上。
>   - 那次是增量构建:`scene.lib`、`cross_zone_test.exe`、`routing_identity_test.exe` 都是 09-25 10:13–10:16 编出的,这次被判为最新。
>   - 所以 **09-21 那批 C++(Z1 / A′,含 M3 探针)已能编译、链接**;同批的 GO-5 与 epoch==0 铸造在 Go 侧(`go/login`、`go/scene_manager`),仍无编译证据。这一条是本文按构建日志和产物时间戳推断的,那次构建不是专门为本线做的。
>   - `cross_zone_test`、`routing_identity_test` 编译过,但**跨 zone 相关用例一个都没运行过**。实际跑过的只有 `bag_test`(09-21 的 10 个结算用例、09-28 整套)与 09-28 的 `turn_battle_engine_test`(124/125,见 PROGRESS.md 同日条目),都不是跨 zone 用例。
> - **2026-09-28 本轮落码的全部代码都没有编译证据**(清单见 §0 最后一条),因为它们都晚于上面那次构建。同样没有证据的还有:Go(`go/scene_manager`、`go/login`)的 build / vet / test、Unity 编译与 EditMode 测试、任何联调与故障注入。
> - **2026-10-01**:GO-2 根治(owner_epoch 严格单调)的代码已全部进 main,同样**没有任何编译、测试证据**,连 `gofmt` 都没跑过。提交号、审查结论、偏离与残余见 §4 第 2 条的 GO-2 一段;设计与判定表见设计文档 §13.7。
> - 任何"已修 / 已落码"都应读作"已落码,待 Codex 验证"。验证顺序见 §4 末尾的「验证顺序」。

## 0. 三十秒版

- **先看这条**:端到端闭环核查查出一个 P0 —— scene 节点从来没装 SceneManager 的 gRPC 应答处理器,所有 EnterScene / CreateScene 应答被静默丢弃。**同 zone 跨节点换图、副本 / 镜像场景进入一直是坏的**,跨 zone 传送也要靠 30s 看门狗兜。2026-04-16 的一次 regen 丢的,与本轮新代码无关,已修(设计文档 §12.5.1),**必须实跑验证**。
- 服务端:上一会话留下的 4 个 P1 已全部处理,另外一轮只读审计 + 反驳式核实又确认了 8 条缺陷,其中 3 条已落码、5 条 + 若干 P3 写成已知限制(§12.3),全部在 `origin/main`。
- 客户端(`mmorpg-client`):**已推上去**(2026-09-20 晚,远端 `main = 120e2d8`,比 09-14 的 `d2b165a` 多 26 个提交;机器 B 已同步)。客户端那半条链已只读核查:主链闭环,另有 1 条 P1 + 4 条 P2 + 3 条 P3 未改,见设计文档 §12.5.4 与本文 §2.2。*2026-09-29:CL-1…CL-8 都已落码或关闭,其中 CL-6 / CL-8 已随合并提交 `96d36da` 推到客户端 `origin/main`(09-29 05:25,按本机跟踪引用);全部未经 Unity 验证,见 §2.2。*
- 传送**后半程**(第二条腿及之后)失败的出口与"回家入口会消失"两处,已按用户要求两端落码(设计文档 §12.5.5):新契约是"`EnterGame` 应答无错 = 已受理,之后进场没成服务端推 3023";客户端收到即秒级收口并显示原因,"当前所在区"改由 `GameClient.CurrentZoneId` 单一真源。**未编译、Unity 未跑**。
- **2026-09-21 追加(均已落码、待验证,未编译、Unity 未跑)**:S3L1-1 的第二层出口(跨 zone 第一条腿已被放行却没拿到放行应答时,源 scene 在销毁实体之前发 tip 3027 + 踢线 34,客户端显示原因并回选服重登)与 scene_manager 回滚 / 铸造 Lua 的 go-redis 重放识别(设计文档 §12.5.6);CL-2(客户端不再用本机时钟拦截票据过期)、S7-1(第一条腿 `scene_config_id=0` 也预检默认大世界)、CL-7(`SceneErrorTip.cs.meta` 入库)已随两仓的自动保存提交进库。**新发现 Z1**:真写盘的正常断线退出不销毁实体(僵尸),它与 CL-5 / GO-5 的根因修复一起写成规格放在设计文档 §12.6。*同日稍后已落码(见下一条)。***新的上线前置**:K8s 的 scene-manager ConfigMap 必须配 zrpc `Timeout`(§12.2)——2026-09-28 已落码(8000),待 Codex 跑契约测试。
- **2026-09-21 再追加(用户授权"按最标准的做法做",已落码待验证)**:Z1 修复 + 断线释放标记 A1′ / A2′(设计文档 §12.6,落码记录 §12.6.10)、**GO-5 决定**(访客重连窗口内回原处、窗口外或主动登出回家;login 在 ShortReconnect / ReplaceLogin 发 `ZoneId=0`,scene_manager 只跟随落在具体节点上的 location,"等待落点"不牵引、R8 被取代,§12.7)、scene_manager 在 owner_epoch 为 0 时同节点也铸造(比原计划更保守:location 记的 epoch 也为 0 才铸,键丢失时按 location 补种,§12.6.9)。主体在提交 `9cef7b2ec`,A′ 复审回修增量在工作树(`cpp/` 8 个文件)。**C++ 未重新编译**(Codex 那次 `game.sln` 编译早于这批代码),**Go 未编译**,均未测试。*2026-09-29:这批 C++ 后来已有编译证据,见文件头;Go 与全部测试仍无证据。*runbook 升到 v2.4,新增场景 Z(Z1 复现)与 R(GO-5 重连落点)。
- **2026-09-28 本轮(跨 zone 收尾第二轮)**。服务端代码都已进 main,**全部未编译、未测试**,待 Codex 验证。
  - **已落码**:
    - **EnterScene 应答关联号**:请求 `correlation_id = 10`,应答回显 `correlation_id = 6`。scene 进程内的 EnterScene 一律经 `SendCorrelatedEnterScene` 取号发出,应答由 `PlayerLifecycleSystem::DispatchEnterSceneReply` 按号分发,只有号匹配的应答才算本代交接的证据。以前只按 `player_id` 对应答,传送在途时组队跟随、疏散、镜像自动进场、普通换图的应答都可能被当成传送应答。旧版 scene_manager 不回显号时,仍退回按 `player_id` 对,并计入 `reply_uncorrelated`。提交 `9da27f9a4`(proto 源 + `enterscenelogic.go`,被每小时自动保存先带走)、`4e409c5c3`(其余部分)、`935ec83b1`(C++ 描述符的 go_package 修正)。
    - **交接冻结硬上限**:按单调时钟计 70s 硬上限,另加 35s 晚发闸。handoff SET 发出之后,四个收口点统一走"tip + 踢线 34 + 不存盘销毁",不再解冻。四个收口点是:冻结上限、晚发闸、没有 gate 会话、没有 SceneManager。两道 30s 看门狗改为按单调时钟复核。`StartTravelHandoff` 的快路径补查未落地存盘。提交 `09f71f9d5`。
    - **CPP-2**:gate 侧进场转发如实上报失败,补发有次数和时长上限,超限就推 `kEnterSceneFailed` 并踢线。链路是否就绪以 scene 的握手应答为准。提交 `09f71f9d5`。
    - **scene_manager 三条 P3**:GO-6、GO-4 残余、告警盲区。提交 `09f71f9d5`。
    - **infra 三项**:Hiredis ERR 分支 delete 回调;scene-manager zrpc `Timeout` 从 5000 改为 8000;删除 `robot/connected()`。提交 `9da27f9a4`、`24dd3e6c5`、`6941e7344`。
    - **客户端 CL-6 / CL-8**:在客户端分支 `crosszone/client-cl6-cl8` 的提交 `54034fe`,基于客户端 origin/main `8b21583`。已随合并提交 `96d36da`(链为 `54034fe` → `835aa1d` → `96d36da`,合入了 `90c38fd`)推到客户端 `origin/main`(09-29 05:25,按本机跟踪引用;组队线 `835aa1d` 同批进入)。**Unity 未编译、EditMode 未跑**。
    - **GO-2 根治(owner_epoch 严格单调)**:*2026-10-01 按 HEAD 核对,代码已全部进 main,未编译、未测试;设计、判定表与落点见设计文档 §13.7。*
      - 做法:路由 / 重定向失败回滚时,不再把 epoch SET 回旧值,而是在同一段 Lua 里再 INCR 一格(N+1→N+2),并原子写回旧 location 与回滚回执(`PlayerLocation.rollback_receipt = 7`)。源 scene 用一段 `#!lua` 原子脚本取证;handoff SET 发出之后,只有两种正向证据能解冻:epoch 未变,或读到与本次标记原文逐字节相同的回执(采纳新 epoch 后强制存盘一次)。其余一律不存盘销毁。
      - 提交:`ccafe300c`(proto 源 + Go)、`907a6b752`(C++ 大部分)、`80a1b0823`(窄面 regen 产物)、`a8c2d44a8`(C++ 余下部分、单测、跨语言 Lua 守卫)、`6b7a59287` 与 `567333aaf`(审查后的修正)。10-01 按本机跟踪引用查:前五个已在 `origin/main`,`567333aaf` 只在本地 main;以 `git ls-remote` 为准。
      - 三视角审查(编译面静读 / 判定表与不变量 / 规格一致)的结论:**没有必须改项**。
      - 偏离与残余、给 Codex 的验证顺序:见 §4 第 2 条的 GO-2 一段与 §4 末尾的「验证顺序」第 2 步。
  - **进行中(设计已定稿,落码中)**:CPP-3。它要改 `player_lifecycle.cpp`,和别的改动串行做。(原先并列在这里的 GO-2 根治已于 2026-10-01 前全部进 main,见上面"已落码"的最后一条。)
  - **本轮不落**:
    - GO-3:需要用户拍板,理由见 §4 第 2 条。
    - `handoff_pending_no_marker` 的比值告警:没有压测基线,推迟到有基线之后。
  - **跨会话变化,接手必读**:
    - 「让 C++ gRPC 客户端失败也回调并设超时」会话已落两批:模板 regen 在 `897ac8241`,scene 侧在 `b85f13c07`。它**推翻了**旧文档里"C++ 生成客户端对非 OK 状态只打日志、不回调"的前提,设计见 `docs/design/grpc-client-deadline-failure-callback.md`。交接路径上的传输失败 = 结果未知,只记日志,仍不作为证据。
    - 在途换图的 TTL 曾是 5s,小于服务端 `Timeout` 8s。那时慢但活着的 scene_manager 下,同一玩家可能有两条 EnterScene 并行,安全靠落点 CAS 和关联号丢弃迟到应答。`b85f13c07` 把 TTL 改为 SceneManager deadline + 1s(`SceneChangeInFlightTtlMs()`),这个窗口**已消除**。
    - Battle 结算依赖不变量 I3:`IsCrossZoneFrozen` 为 false 的任何时刻,本节点一定仍是属主。冻结硬上限的两个出口保证了这一点。
  - **提交状态**(09-29 按本机跟踪引用查):以上服务端提交都已在 `origin/main`;`b85f13c07` 早先只在本地 main,09-29 稍后复查时本机跟踪引用显示它也已在 `origin/main`。以 `git ls-remote` 为准。GO-2 的六个提交不在这句"以上"之内,它们的状态单列在上面 GO-2 那一条里。
- 最高优先级永远是**把它编出来**(§4),其余一切结论都悬在这上面。编译和测试的先后顺序不能乱,见 §4 末尾的「验证顺序」。

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
>
> **2026-09-29 更新**:下表 C-1 / C-2 / C-3 已在客户端 `origin/main` 上修好。本文先按客户端本机跟踪引用 `origin/main = 90c38fd`(09-28)逐条核过,核实点写在各行末尾;09-29 05:25 起本机跟踪引用已前进到 `96d36da`(合并提交,合入了 `90c38fd`),上述核实点不受影响。按 `git log -S` 查,修复来自机器 B 09-20 的两个提交:`6876247` 修了 C-1 / C-3,是 §12.5.5 的客户端那一半;`2ca620e` 是自动保存,带进了 C-2 的 `.meta`。上面那句"一行没改"只对 09-20 那一轮成立:此后客户端经用户授权改过两批。
> - 09-21:CL-2、CL-7,见 §4 第 2 条"客户端侧"。
> - 09-28:CL-6、CL-8,落在客户端分支 `crosszone/client-cl6-cl8` 的提交 `54034fe`,基于 `origin/main` 的 `8b21583`。已随合并提交 `96d36da` 推到客户端 `origin/main`(09-29 05:25,按本机跟踪引用;组队线 `835aa1d` 同批进入),**Unity 未编译、EditMode 未跑**。见下表末行和设计文档 §13.6「客户端 CL-6 / CL-8」。

| # | 位置 | 要做什么 | 怎么算做完 |
|---|---|---|---|
| — | **核实结果** | C-1 属实(= CL-6,P3,还缺 3014 与 `kSceneTransferInProgress`);C-2 属实(= CL-7,P3,是该目录唯一缺 `.meta` 的文件);C-3 属实(= CL-4,P2,且它会触发 CL-3"UI 上回不了家");C-4 确认:20 到客户端是 3027,表现为"提示 + 留在原地",不卡;C-6 **关闭**:tip 先到时 UI 代次 +1,迟到的应答被丢弃,遮罩不会重开;C-5 **有答案**:收到 34 后客户端一定断线回选服界面(`GameClient.cs:1463-1467`),"冻结硬上限"设计的前置解除 | — |
| C-1 | `GameClient.DescribeTravelTip` | 漏了 `kEnterSceneSceneNotFound`,该码目前落到裸编号文案 | 该码有对应中文文案;对照 `generated/code/proto/tip/scene_error_tip.proto` 把传送链上会回到客户端的码过一遍。**2026-09-29 核实:已修(`6876247`,在客户端 `origin/main`)**:`DescribeTravelTip` 已有 `KEnterSceneSceneNotFound`、`KEnterSceneChangingScene` 两条文案。剩下的 13000 `kSceneTransferInProgress` 要先让 `gen_proto.ps1` 收进 `cross_server_error_tip.proto`,由 CL-6 补,见末行 |
| C-2 | `SceneErrorTip.cs` | 缺 `.meta`,换机器后 Unity 会重新生成 GUID,引用它的资源会断 | `.meta` 进版本库。**2026-09-29 核实:已修**,`Assets/Scripts/Proto/Generated/SceneErrorTip.cs.meta` 已在 `origin/main` 的树里(= CL-7) |
| C-3 | `CityTravelUiRoot.cs:303` | 城内传送窗口仍是"见任何 tip 就收场"并拼裸编号;`GameClient.IsTravelFailureTip` / `DescribeTravelTip` 已是现成的 `public static`,两行就能接上 | 只对传送失败类 tip 收场,文案走 `DescribeTravelTip`。**2026-09-29 核实:已修(= CL-4)**:`CityTravelUiRoot.HandleServerTip` 在跨区在途(`_pendingZoneId != 0`)时只认 `GameClient.IsTravelFailureTip`,文案走 `GameClient.DescribeTravelTip`。原行号 `:303` 已漂移,按方法名找 |
| C-4(本轮新增) | 传送失败 tip | 服务端现在会对**没有 `player:zone` 映射的玩家**的 TravelToZone 回 20(`ErrHomeZoneUnavailable`),源 scene 解冻后回失败 tip | 确认客户端对这次失败的表现是"提示 + 留在原地",不是卡在传送中。**已确认**(见"核实结果"行) |
| C-6(本轮新增,待核) | 传送"已受理"应答 vs 失败 tip 的到达次序 | `player_scene.proto:74-76` 写的契约是"应答无错 = 已受理,未成 = **之后**收到 tip";但受理后同步失败(脏数据快路径 + Redis 未连接等)时,tip 会先于应答到达 | 确认客户端不是"先收到无错应答才开传送中遮罩" —— 若是,会先弹一次失败提示、再打开遮罩,只能靠自身超时收起。**已关闭**(见"核实结果"行) |
| C-5(决策前置) | 踢线消息 `SceneClientPlayerCommonKickPlayer`(34) | "冻结硬上限"的设计(§12.3)到期后要靠它让客户端回登录 | 回答一个问题:**客户端收到 34 之后是否一定断线并回到登录界面?** 答案决定那份设计能不能落。**已答:一定断线并回选服**(见"核实结果"行)。前置解除后,冻结硬上限已于 2026-09-28 落码(`09f71f9d5`,未编译) |
| CL-6 / CL-8(2026-09-28 落码,**未编译**;09-29 已随 `96d36da` 推到客户端 `origin/main`) | 客户端分支 `crosszone/client-cl6-cl8` 的提交 `54034fe`,已经合并提交 `96d36da` 进入客户端 `origin/main` | CL-6:`tools/gen_proto.ps1` 收进 `cross_server_error_tip.proto`;新增生成物 `CrossServerErrorTip.cs`(+ `.meta`),只用单个 protoc 生成,**没跑 gen_proto 全量**;`DescribeTravelTip` 补 `kSceneTransferInProgress`(13000)与 `kFeatureUnavailable` 两条文案,两者只走同步响应体,不进 `IsTravelFailureTip`。CL-8:跨区总预算 `CityTravelRequest.CrossZoneTimeoutSeconds` 改成表达式 = 交接预算 `AcceptedHandoffBudgetSeconds`(75)+ `GameClient.RedirectFlowWorstCaseSec`(105 = 探测 5 + 验票 10 + Login 15 + EnterGame 15 + 等入场 60)+ 余量 `CrossZoneSlackSeconds`(15)= 195,旧值 120。75 依赖服务端冻结硬上限:70s + 1s 扫描 = 71 < 75,服务端 `travel_freeze_cap::kClientAcceptedHandoffBudget` 用 `static_assert` 镜像 75,两边要同步改。验票、等入场两个等待点改用 `ClassifyPipelineWait`,被拒票或连接已失(`IsStillCurrentGate`,只看主线程的 `_gate`)时提前退出。`DisconnectInternal` 不再清 `_enterFailedTipId` | Unity 编译 + EditMode(`CityTravelRequestTests`、`GateTcpClientLifecycleTests`)通过;按设计文档 §13.6 的核对命令验证生成物与 protoc 单文件结果逐字节一致 |

## 3. 服务端:本会话做了什么

全部在设计文档 §12.1,这里只列要点与**对上一会话说法的更正**(别沿用旧说法):

- **P1-1 告警恒空**:已修;并且**多查出两条 critical 的 `*PoolEmpty` 同样恒空**(gauge 从不发布 0 值,`== 0` 永远匹配不到,这两条 pager 从未响过)。`scene_gone` 指标的发射点当年被误删,已恢复。
- **P1-2 Redis 断开时 handoff 标记撤不回**:已落码(待撤回表 + 条件删除 + 换图闸口)。更正:触发面比上一会话描述的**窄**(正常登出 / 租约到期后重登不受影响,只有 30s 租约内重连回同节点才中),但另有一个**更糟的在线变体**(`AbortTravelHandoff` 的撤回同样被静默跳过,不用重登就能命中),一并修了。
- **P1-3 失效 runbook**:重写为 v2,观测点逐个 grep 核对;文件头声明"静态编写、未实跑",首次执行发现不符要回改文档。
- **P1-4 路由文档教人清 location**:逐句核对改写。结论:手工清 location **不会**绕过 epoch 铸造,但**会**绕过换手门;唯一安全前提是源区 scene 节点已全部注销 + 负载集为空 + 再入屏障已过,而此时 scene_manager 本来就会自动忽略该记录——所以手工清只是卫生动作,首选 `merge_zone -clear-source-hot-state`。**绝不能删 `player:{id}:owner_epoch`。**
- **"交接冻结没有服务端上限"——这条大部分不成立。** 已有两道 30s 看门狗,应答丢失 / scene_manager 卡住 / 存盘回调不来都有界(最坏约 60s < 客户端 75s)。真正无上限的只有 zone Redis 不可用或半开时的三段。有完整设计稿但**未落码**,卡在 C-5。*2026-09-28 更正与现状*:
  - 仓库里其实没有"完整设计稿",只有 §12.3 表格和本条这两句摘要。
  - C-5 已于 09-20 解除(§2.2)。
  - 本轮按新定稿落码,提交 `09f71f9d5`,未编译、未测试。内容是:70s 单调时钟硬上限;35s 晚发闸;SET 已发出后统一收口为 tip + 踢线 34 + 不存盘销毁;看门狗按单调时钟复核;快路径补写未落地存盘。
  - 完整设计与验证清单见设计文档新增节「交接冻结硬上限 + 晚发闸 + 『标记已发出』统一收口」。
- **审计新增并已落码**:未映射玩家跨 zone 传送会把访客存盘写进目标 zone 的库(GO-1);gate 对"同 scene_id、不同节点"的路由不转发进场(CPP-1)。
- **§10.2 的 R7 声称已解决"访客掉线后从别区 gate 登录",实际从 login 走不到**(login 恒发 `ZoneId = 本 zone`),见 §12.3 GO-5。

**新增上线前置**:对存量号开放跨 zone 传送之前,必须先跑完 `tools/merge_zone -backfill-home-zone`(§12.2)。**2026-09-21 再加一条**:K8s 的 scene-manager ConfigMap 必须配 zrpc `Timeout` ≥ `KafkaWriteTimeoutSeconds` + 回滚余量(例如 8000),或用 `MethodTimeouts` 只放宽 `EnterScene`;现状落到 go-zero 默认 2000ms,任何超过 2s 的第一条腿都会让源端冻满 30s,gate-cmd 滞后时还会被误踢(§12.2)。~~`tools/scripts/k8s_deploy.ps1` 由有权限的会话去改,尚未改。~~
- **2026-09-28 已改,待 Codex 验证**:
  - 服务 yaml 是唯一真相,`Timeout` 为 8000(= 归属查询 1500 + Kafka 写 5000 + 余量 1500)。
  - `k8s_deploy.ps1` 从它镜像 `Timeout` / `KafkaWriteTimeoutSeconds` / `HomeZoneLookupTimeoutMs` 三项。
  - 契约测试 `k8s_deploy_contract.tests.ps1` 的 `Get-SceneManagerTimeoutBudgetViolations` 守住不等式,并要求三项为正、禁用 `MethodTimeouts`。上文"或用 `MethodTimeouts` 只放宽 `EnterScene`"作废,因为生成器不镜像它。
- **前提已变**:"冻满 30s"的依据是"C++ 生成客户端对非 OK 状态只打日志、不回调"。2026-09-28 起生成客户端有失败回调并设 deadline,见 `docs/design/grpc-client-deadline-failure-callback.md`。但在交接路径上,传输失败仍按"结果未知"只记日志,不作为证据、不提前核实,所以对交接来说"冻满 30s 应答看门狗"这个后果不变。

## 4. 服务端:剩余工作(按优先级)

0. **实跑验证 §12.5.1 的 P0 修复**(编译之后第一件事)。它激活了约 150 行从未执行过的代码。两条最小验证:① 同 zone 起 2 个 scene 节点 + `AllowUnsafeCrossNodeHandoff=false` 跑一次跨节点换图,scene 日志应出现 `SceneManager.EnterScene deferred (handoff pending)` 并随后起交接重发(修复前这条日志**从不出现**);② 进一次副本 / 镜像场景,确认玩家真的被自动带进去(修复前是"请求发出、无任何后续")。
0b. ~~**补上传送后半程的失败出口**~~ **已落码,待验证**(设计文档 §12.5.5,验证步骤在该节末尾;联调用例 a–d 缺一不可)。以下为落码前的原始描述,留作背景:(§12.5.2,4 条同根;客户端面是 §12.5.4 的 CL-1)。客户端核查更正了一点:玩家**不会永远卡住**,客户端等进场有 60s 硬上限,到期打回选服——但看不到失败原因,服务端刻意保留的登录会话 / 票据客户端也完全用不上,所以两端要一起定方案。最小做法是复用 login 已有的 `KickSessionOnGate` 让客户端明确回登录,或在 gate 侧加"已绑会话但 N 秒没收到 RoutePlayerEvent 就踢"的看门狗(顺带兜住 §12.3 的 CPP-2)。`go/login/**` 有别的会话在改,动手前先对范围。其中 S7-1 可单独低成本收敛:第一条腿在 `SceneConfId == 0`(回家的典型形态)时也做一次"目标 zone 有没有任何世界频道"的只读预检。*(2026-09-21:S7-1 已落码,见 0c。)*
0c. **2026-09-21 已落码、待验证**(设计文档 §12.5.6,验证步骤在该节末尾;故障注入是 runbook 新场景 B3):S3L1-1 第二层出口(C++ 证据枚举 + 单条 `MGET owner_epoch location` + 五条运行期条件满足才发 tip 3027 + 踢线 34;客户端按踢线原因显示文案)、scene_manager 回滚 Lua 三态 + 铸造 Lua 重放识别 + `scene_manager_enter_scene_rollback_total` 与告警;CL-2 / S7-1 / CL-7 已随自动保存提交(服务端 `4624ddf9e`、客户端 `2ca620e`)进库。铸造重放识别有计数 `enter_scene_mint_replay_recognized_total` 与日志 `[MintReplay]`。已知缺口:同 zone 的回滚 `redis_error` / 铸造后路由失败仍是哑连接。
0g. **2026-09-21 已落码、待验证 —— 下一步就是这一条**:Z1 + A′ + GO-5 + epoch==0 同节点铸造(0d / 0f 的内容)。由 Codex 按设计文档 §12.6.10 末尾的验证清单执行,顺序不能乱:① **先用修复前的二进制复现 Z1**(runbook 场景 Z-pre;期望 `ignoring stale UnregisterPlayer … live session <退出会话>` 且无 `Destroying player`)→ ② `msbuild game.sln /m:1 /nr:false /p:Configuration=Debug /p:Platform=x64` → ③ `cross_zone_test`(`ExitPersist*:ExitRelease*:HandoffMarkWithdrawQueue.*:TravelOutcomeReset.*:CrossZone*`,再全量)+ `run_cpp_tests.ps1 -Build` → ④ Go:`go/scene_manager` 与 `go/login` 的 build / vet / test(命令在 §12.6.10)→ ⑤ runbook v2.4 的场景 Z-post 与场景 R(R1–R5),以及 C1 / C2 / B2 / D2 的新期望;再加 §12.6.10 清单的 11b(M3 探针,`docker pause etcd` 约 100s)。**此前挂着的待决项已全部收口(2026-09-21 主会话)**:`redis.cpp:43`、`:57` 保持(用户授权按最标准做法、不必等其确认);M3 探针已收紧,新增只读查询 `EtcdService::IsIdentityConfirmedFresh`(engine 文件,只加不改);`owner_epoch.go` 文件头的"唯一例外"、`enter-scene-zone-routing.md` 规则 2 与 Offline-Return、`handoff_mark_withdraw.h` 键契约注释、`deploy/k8s/scene-manager-alerts.yaml` 的 `stale_marker` 口径(M12)都已补;客户端 `GameClient.cs` 重定向时 `PlayerId` 为 0 的问题已修(`ResolveRedirectPlayerId`,§12.7,客户端未编译);runbook 场景 R2 的前提(A 区能看到 B 区的断线会话)已静态核实(SharedRedis,D12)。
   *2026-09-29 批注*:
   - 这批 C++ 已有编译证据(见文件头),本条其余步骤仍未执行。
   - 09-28 本轮又在同一批文件上叠了新代码,整体顺序改按本节末尾的「验证顺序」走。
   - ① 的前提在机器 A 上已经没了:`bin/scene.exe` 是 09-25 10:15 编出的,已含 Z1 修复,"修复前的二进制"被覆盖了。要复现 Z1,只能在 `9cef7b2ec^` 的独立 worktree 里另编一份;做不到就如实记为"Z-pre 红态不可得"。
0d. ~~**待决(需用户拍板,未落码)**~~ **已落码待验证,见 0g**:新发现的 **Z1**(真写盘的正常断线退出不销毁实体,留下僵尸;有实跑日志)与 **CL-5 / GO-5 的根因修复**(断线释放标记 A1′ + 新载入前原子核归属再删 A2′),完整规格、两名复审的 15 条必须改项、替代方案与"为什么本轮不落码"都在设计文档 §12.6。建议顺序:先编译现有代码 → 复现 Z1 → 落 Z1 → 落 A′。**注意**:不能把 Z1 简单修成"落地即销毁"——今天的僵尸在几种竞态下正在补存差额(§12.6.1)。
0f. **与帮会二期 B4c 的分工(2026-09-21)**:K1 的同节点缺口里,`redis_client.h` 新旧颠倒由 B4c 修(并提供"该 key 是否有在途 / 排队存盘"的只读查询供 Z1 复用);Z1 僵尸与 GO-2 归本线。~~**本线欠一项**:scene_manager 在观察到的 owner_epoch 为 0 时同节点也铸造(一行,`enterscenelogic.go` 的 `mint: !samePhysicalNode` 改为 `!samePhysicalNode || observedEpoch == 0`),首次编译之后做,B4c 把它列为依赖。~~ **已落码待验证**(`enterscenelogic.go:516-543`、`:696-704`):只在键与 location 记的 epoch 同为 0 时铸造;键丢失而 location 记着 N 时 SETNX 补种 N、不铸造;这种铸造后路由失败只退 location、epoch 保留。另 `save outran reconnect lease` 原文保留,改为每次退出只打一条(需知会帮会线)。详见设计文档 §12.6.9。
0e. **新的上线前置**:K8s scene-manager ConfigMap 配 zrpc `Timeout`(设计文档 §12.2),改 `tools/scripts/k8s_deploy.ps1`,需有权限的会话。**已落码 2026-09-28,待验证**(`Timeout: 8000`,生成器镜像三项,契约测试守不等式,详见 §3 末尾)。login 的 SceneManagerRpc 客户端 `Timeout` 仍是 5000,小于 8000,这个既有的不对称没动,`go/login/**` 归帮会会话。
0h. **2026-09-28 本轮逐项状态**(代码都已进 main,**全部未编译、未测试**)。各项的设计与验证清单在设计文档里按标题找,新节编号以落笔时为准。
   - **已落码**:
     - EnterScene 应答关联号 → 设计文档「EnterScene 应答关联号」。单测是 `cross_zone_test` 的 `EnterSceneReplyRoute.*`(9 个)与 `EnterSceneReplyEcs.*`(8 个),另有 Go 侧 `logic_test.go` / `owner_epoch_test.go` 的新增用例。
     - 冻结硬上限 → 见第 2 条。
     - CPP-2 → 见第 2 条。
     - GO-6 / GO-4 残余 / 告警盲区 → 见第 5 条。
     - Hiredis / zrpc Timeout / `robot/connected()` → 见 0e 与第 5 条。
     - 客户端 CL-6 / CL-8 → 见 §2.2 末行。
     - GO-2 根治(owner_epoch 严格单调)→ 见第 2 条的 GO-2 一段与设计文档 §13.7。*2026-10-01:已全部进 main,从"进行中"移到这里。*单测是 `cross_zone_test` 的 `TravelOwnership.*`(7 个),Go 侧是 `owner_epoch_test.go` 的新增 / 改写用例与新文件 `owner_epoch_crosslang_test.go`(3 个用例)。
   - **进行中(设计已定稿,落码中)**:CPP-3,见第 2 条。
   - **不落**:GO-3,见第 2 条;`handoff_pending_no_marker` 比值告警,见第 5 条。
   - **跨会话**:gRPC 失败回调的 scene 侧 `DispatchEnterSceneTransportFailure` 已落地(`b85f13c07`),分发规则如下:
     - 按请求的 `correlation_id` 分发。
     - 命中交接:只记日志,不当证据,不提前核实。
     - 普通换图:释放在途槽,回 `kServiceUnavailable`。
     - 组队跟随:只记日志。
     - SceneManager 的 C++ deadline = 服务端 `Timeout` 8000 + 2000(`bin/etc/base_deploy_config.yaml` 的 `GrpcClient.CallDeadlineMs.SceneManagerNodeService: 10000`)。在途 TTL = deadline + 1000。
1. **编译 + 单测 + 冒烟**(阻塞其余一切)。命令、通过标准、失败时保留什么:设计文档 §12.4,在 §9 / §11.4 基础上追加。main 上还叠着组队、战斗 G1–G9、帮会二期、聚宝斋、friend 等多批未编译代码,总顺序见 `turn-battle-gap-closure.md` §7;C++ 必须串行 `msbuild … /m:1 /nr:false`。
2. **§12.3 的 P2 五条**,每条都写了"为什么没修 / 修法":
   - GO-2 回滚让 epoch 回退、毒化 db 守卫——要改 proto(`EnterSceneResponse` 回显 epoch)或调 C++ 发 DBTask 的时机;
     **2026-10-01:已全部进 main,未编译、未测试,待 Codex 验证;见设计文档 §13.7。**(09-28 写这一条时的状态是"设计已定稿,落码中"。)
     - 状态注:`EnterSceneResponse` 的回显(`owner_epoch_after_rollback = 5`)**只用于日志和核对,不是采纳凭证**。凭回显采纳,会在第三方已凭转写标记铸出更新值时造成双持有。根治靠回滚回执加交接标记。
     - epoch 严格单调:路由 / 重定向失败回滚时,在同一段 Lua 里再 INCR 一格(N+1→N+2),不再 SET 回旧值。同一段 Lua 还原子写回旧 location 和回滚回执 `PlayerLocation.rollback_receipt = 7`,回执的值是被回滚那次铸造所凭的 handoff 标记原文。
     - 源端取证:C++ 源端改用单个 `#!lua` 原子脚本,只删本次交接这一族标记,并在同一脚本里读 epoch 与 location。只有两种正向证据能解冻:一是 epoch 未变;二是读到与自己标记原文逐字节相同的回执,这时先采纳新 epoch,再强制存盘。其余情况一律不存盘销毁。
     - 与 I0 的关系:I0 机制 2(DEL 与 MGET 走不重放的同一连接)改由这段原子脚本保证。
     - 不做的:次选方案"CAS 成功后再发 DBTask"不做,`go/db` 不改。
     - 落点提交(2026-10-01 按 HEAD `bbb5bc6bc` 核对;09-29 那条"字段 5 / 7 未提交、还没做窄面 regen"的进度已过时):
       - `ccafe300c`(自动保存):proto 源两份(`PlayerLocation.rollback_receipt = 7`、`EnterSceneResponse.owner_epoch_after_rollback = 5`);`go/scene_manager` 的 `owner_epoch.go` / `enterscenelogic.go` / `changesceneutil.go` / `metrics.go` 与测试;`go/shared/ownerepoch/ownerepoch.go`;`deploy/k8s/scene-manager-alerts.yaml` 的注释。
       - `907a6b752`(自动保存):C++ `player_lifecycle.{h,cpp}`、`travel_freeze_cap.h` 的大部分。
       - `80a1b0823`:两个 proto 字段的窄面 regen 产物,共 12 个文件:三个暂存目录(`_unified` / `db` / `login`)下的 proto 副本、`go/proto/scene_manager` 的两个 `.pb.go`、`cpp/generated/proto/scene_manager` 的两套 `.pb.{h,cc}`。C++ 以仓库根作 proto_path;service 定义没变,grpc 产物不动;`robot/vendor` 不含这个包,所以没有 robot 侧产物。**不需要再 regen。**
       - `a8c2d44a8`(每小时自动保存):`player_ownership_comp.h`、`exit_release_mark.h`、`player_exit_intent.h`、`player_lifecycle.{h,cpp}` 余下部分、`travel_freeze_cap.h`、`scene_node_service.cpp` 与 `scene_handler.cpp`(守护段内只改注释)、`cross_zone_test.cpp` 的新用例、`go/scene_manager/internal/logic/owner_epoch_crosslang_test.go`(守住 C++ / Go 两份 Lua 逐字节一致的跨语言用例)。
       - `6b7a59287` + `567333aaf`(自动保存):三视角审查后的修正,内容见下面"审查后的修正"。
       - 提交状态(10-01 按本机跟踪引用,`origin/main` 指向 `6b7a59287`):前五个已在 `origin/main`,`567333aaf` 只在本地 main。以 `git ls-remote` 为准。
     - 三视角审查(编译面静读 / 判定表与不变量 / 规格一致)的结论:**没有必须改项**。审查确认的要点:
       - handoff SET 发出之后,"保留实体又解冻"的出口只有三条。两条是 `JudgeTravelOutcomeReply` 的正向证据:`kUnchanged`(B4)与 `kRolledBackToSelf`(B5)。第三条是同一代 SET 被 Redis 回 ERROR(`HandleTravelMarkWriteRejected`):那时 SET 没执行,按 I0 属于"未发出"。
       - "放行后又回到本节点"(B7)不会被判成回滚。
       - 取证是一段原子 `#!lua` 脚本 `travel_outcome::kLuaJudgeTravelOutcome`:按 saved_at_ms 删本族标记,再 MGET owner_epoch 与 location。它取代了原来的"同连接 DEL 再 MGET"。
       - Go 回滚 Lua `luaRollbackPlayerPlacement` 与目标节点的 A2′ 互斥。
     - 审查后的修正(`6b7a59287`、`567333aaf`):
       - B6(回执异常)只在 `ConcludeHandoffAfterMarkSent` 真正收口(返回 true)时才计 `rollback_receipt_anomaly`;推迟销毁时不计。
       - `SavePlayerToRedisImpl` 强制写的调用方从两处变成三处,新增的是 B5 采纳。
       - 告警 `SceneManagerEnterSceneRollbackRedisError` 的表达式改成 `outcome=~"redis_error|plan_error"`。`plan_error` 的终态同 `redis_error`,原先没有任何规则覆盖它。
       - 若干过时注释;runbook 三处同步。
     - 偏离与残余(一句话版,展开见设计文档 §13.7):
       1. `ResolveTravelOutcome` 在 `requestedAtMs == 0` 时只打 ERROR 就返回,不按判定表 A1 调 `AbortTravelHandoff`。按构造不可达,由 70s 冻结上限兜底,是安全方向。
       2. `HandleTravelMarkWriteRejected` 为了单测改成了 public,头文件写明"业务代码不要调"。
       3. B6(`site=travel_receipt_anomaly`)收口后,Redis 里没有可当 A1′ 用的标记:取证脚本已原子删掉本族标记,含原标记 `E:t` 与转写出来的 `E+2:t`。踢线后重登若被挑到别的节点,会一直回 18(`ErrHandoffPending`),直到 location 被 LeaveScene 清掉或挑回本节点。只伤活性;B6 按设计恒为 0。
       4. `plan_error` 并入 `redis_error` 的告警规则。规格原说"本批不加规则",审查后改了。
       5. 代码 / proto / yaml 注释里约 36 处写的是 `cross-zone-scene-travel.md §12.8`,而正文在 §13.7。处理办法不是逐处改注释,而是在设计文档 §12.7 之后加一个只有两三行的编号占位小节 §12.8 指到 §13.7(随同批文档更新做;没看到这个小节时直接读 §13.7)。
       6. 没跑 `gofmt`:`owner_epoch.go` 的 doc comment 用了大量列表 / 代码块,Go 1.19 起 gofmt 会重排这类注释。另外 miniredis 有三个行为要靠实跑确认:Lua 表里 false 转 nil;`SET … EX "300"` 用字符串参数;负数整数应答。
     - 原计划"GO-2 落定后顺手改"的 SM 预检日志:原文 `no SceneManager node reachable` **决定不改**。`player_lifecycle.cpp` 里带这段原文的日志共三处,runbook 引的也是这个原文。
     - 给 Codex 的验证顺序:见本节末尾「验证顺序」第 2 步。
     - 规格:已入档到设计文档 §13.7「GO-2 根治:owner_epoch 严格单调」。原始规格 `spec-go2.md` 只在机器 A 主会话的 scratchpad 里,仅作备注。
     - 下游依赖:帮会 `guild-phase2/08-save-owner-fence.md` §8.3 门禁第 3 条以它为前置。代码进 main 不等于门禁通过,要等 Codex 的验证结果。runbook 侧:§2.3 的 `scene_manager_enter_scene_rollback_total` 一行与 §5 不变量第 6 条已按 GO-2 改过(回 7 且已回滚时,铸造过的 epoch 只升不降);§2.2 错误码表的 7 号一行 10-01 查仍写着"location 与 epoch 已回滚",还是旧口径;场景 B3 在 GO-2 二进制上按原文不执行,runbook 自己写着"§7 记 SKIP"。以 runbook 当前文字和它的 Changelog 为准。
   - GO-3 同 node_id 重注册后死节点接管永不触发——要在 `PlayerLocation` 里记进程实例标识(proto);
     **2026-09-28:本轮不落,待用户拍板**。
     - 已有设计:把 C++ 进程的 `node_uuid` 与发现键的 create_revision 盖进 `PlayerLocation`,判"已替换"要四条证据同时成立(第 ④ 条是时序闸:记录写入早于现任进程 launch_time 至少 6s)。
     - 不落的原因是设计残余 R1:
       - 滞留在 Kafka / gate 里的路由事件,可能在同号新进程注册之后才投递给它。
       - 这会让一个健康的活进程在没发 handoff SET 的情况下被判"已替换",从而丢掉归属。
       - 那个进程若正冻结交接、SET 未发,就违反 I0:SET 未发出,不可能被放行。
       - 若没在冻结,`IsCrossZoneFrozen` 为 false,而本节点已不是属主,违反 I3,Battle 结算依赖 I3。
       - §11.5 的例外只覆盖"属主进程已从注册表消失"。
     - 根治二选一,都要改 C++,需用户拍板:
       - `RoutePlayerEvent` 带目标进程 uuid,由 gate / scene 校验(还要改协议);
       - 或由持有节点在 A2′ 的 Lua 里自己盖章。
     - 推迟的代价:GO-3 本身是活性问题,回可重试的 18,租约到期后自愈,推迟不引入数据风险。
     - 以后落码要 rebase 到 scene_manager P3 那批之上,并补一个用例:推迟摘除期间来一条不同 uuid 的 PUT,仍判 Replaced 且仍被屏障挡住。`PlayerLocation` 的 7 号已被 GO-2 占用,GO-3 的新字段从 8 起。
     - 规格:设计与 R1 全文已入档到设计文档 §13.9「本轮不落」。原始规格 `spec-go3.md` 只在机器 A 主会话的 scratchpad 里,仅作备注。
   - GO-5 R7 / R8 从 login 不可达——要动 `go/login`(本轮有「帮会二期」会话在改那个目录,先对一下);*2026-09-21:其中"访客 / 快速重登卡在 18"的根因修复不在 login,而在 C++ scene(Z1 + 断线释放标记),见 0d / 设计文档 §12.6;GO-3 的 300s 内缓解同在 §12.6*;*同日稍后 GO-5 已按用户决定两侧落码待验证(login 发 `ZoneId=0`、scene_manager 只跟随具体节点的 location,§12.7),见 0g*;
   - CPP-2 gate 丢路由无补发、CPP-3 疏散改派 fire-and-forget——都需要"补发 / 待确认表 + 超限发 tip 并踢线"的设计。它们与冻结硬上限共用的前置 C-5 **已解除**(客户端收到 34 一定回选服),三者现在都可以落。2026-09-28 三者的状态如下。
     - **CPP-2:已落码待验证(设计文档「CPP-2 gate 侧进场转发补发」),未编译。**提交 `09f71f9d5`。
       - 做法:gate 如实上报"未连接 / 未握手 / 找不到节点";单次进场的补发预算 20s,其中找不到节点最多等 3s,最多 16 次,退避 250ms 起步、封顶 2s;超限先推 `kEnterSceneFailed`(3023),再推 34(reason 同码),然后关连接。链路是否就绪以 scene 的握手应答为准。
       - 新文件:`pending_scene_entry_comp.h`、`scene_entry_dispatch.{h,cpp}`;gate `main.cpp` 挂了扫描定时器。
       - 日志前缀 `[SceneEntry]`。
       - 单测是 `routing_identity_test` 的 `SceneEntryRetry.*`、`SceneLinkReady.*`、`SceneEntryAttempt.*`、`SceneEntryLateLogin.*`;`SceneRouteEntry.*` 从 16 个增加到 17 个。
       - 残余:"路由根本没到 gate"那一半没做;"location 指向一个从未载入玩家的节点"也没变。
     - **CPP-3:进行中,设计已定稿,落码中。**
       - 做法:改派从"发完即忘"改成"登记 → 确认 → 收口"。票据消费时登记进按 `player_id` 建键的待确认表(新纯头文件 `relocate_confirm.h`)。以 SM 应答、进场路由和各阶段的单调时钟截止为信号;在 zone Redis 上核实 location 仍在源端 / 本节点,并且 SM 已不可能再处理本次改派之后,才按票据里的 (sessionId, playerId) 先推 tip `kEnterSceneFailed` 再推 34。
       - 跨配置契约:它的 `kSmSettleWindow = 15s` 是按 zrpc `Timeout` 8000 推出来的,`Timeout` 调到约 13s 以上时必须同步改它。
       - 规格:已入档到设计文档 §13.8「CPP-3 疏散 / 排空改派的待确认表」。原始规格 `spec-cpp3.md` 只在机器 A 主会话的 scratchpad 里,仅作备注。
     - **冻结硬上限:已落码待验证,未编译。**提交 `09f71f9d5`,见 §3 与设计文档「交接冻结硬上限 + 晚发闸 + 『标记已发出』统一收口」。
       - 常量在 `travel_freeze_cap.h`:`kFreezeCap` 70s、`kDispatchWindow` 35s、`kSweepInterval` 1s。`kClientAcceptedHandoffBudget` 75s 镜像客户端的 `AcceptedHandoffBudgetSeconds`。
       - 收口点名表:`travel_freeze_cap` / `travel_dispatch_window` / `travel_no_gate_session` / `travel_no_scene_manager`。收口日志是 `[ZoneTravel][MarkSentDestroy] … site=…`;跨区用 tip 3027,同区用 tip 3023。
       - `[TravelHandoff]` 汇总行新增 8 个 key:`freeze_cap_reached`、`dispatch_window_closed`、`mark_sent_destroyed`、`mark_sent_client_reset`、`destroy_deferred_unsettled_save`、`freeze_unstamped`、`watchdog_early_fire`、`handoff_fastpath_forced`。
       - 单测是 `cross_zone_test` 的 `TravelFreezeCap.*` 与 `TravelFreezeCapEcs.*`,各 8 个。*2026-10-01:GO-2 在 `a8c2d44a8` 给 `TravelFreezeCapEcs` 加了第 9 个用例 `LateMarkSetErrorOfOlderGenerationLeavesNewHandoffFrozen`。*
       - robot 的 travel-smoke 单跳预算是 60s,短于 70s,所以 robot 看不到上限分支。上限分支(runbook 场景 H)要用 Unity 客户端验。
   - 客户端侧:CL-3 / CL-4(回家入口消失)已随 §12.5.5 修复;~~CL-2 仍未修~~ **CL-2 已落码待验证**(2026-09-21,`ValidateRedirectTarget` 只打日志不拦截,以 gate(B) 验票为准,§12.5.6);CL-7 已入库;CL-5(手动重进无冷却)的根因修复见 0d(已落码待验证,见 0g)。CL-6 / CL-8 已落码待验证,见设计文档 §13.6(客户端分支 `crosszone/client-cl6-cl8` 的 `54034fe`,已随合并提交 `96d36da` 推到客户端 `origin/main`,09-29 05:25,按本机跟踪引用;见本文 §2.2 末行)。
3. **需要随一次全量 regen 做的**:真删 `proto/common/event/player_migration_event.proto`(整条 DEPRECATED、无调用方,仍占着 event id 27 / 41,清单写在该文件头注释里);`proto/common/database/player_cache.proto:7` 与 `bag_quest_mail_data.proto:7-8` 的注释仍把 `PlayerAllData` 说成"跨 zone 迁移携带的快照"。「Friend 服务移植后续工作」会话计划跑 regen,可与它同批。*2026-09-29 仍未做*:`player_migration_event.proto` 仍在,`player_cache.proto:7` 的注释未改。09-28 gRPC 失败回调那次 regen 是在隔离 worktree 里跑的,只拷回了 40 个文件,不算全量 regen。本轮 proto 一律走窄面 regen,只动 `scene_manager_service.proto` / `storage.proto`。
4. **本轮因归属没碰、只登记的过期文字**:`go/login/internal/logic/clientplayerlogin/entergamelogic.go:470`、`:575` 附近(仍写 14 `ErrUnsafeCrossNodeHandoff` 与"重定向前先解析场景");`docs/design/guild-phase2/04-asset-channel.md:29 / :300 / :335 / :352`(C6 的正确性论证依赖已删的"迁移包");`cpp/libs/services/scene/battle/system/player_battle.cpp:838`;`docs/notes/` 下 3 处。*2026-09-29 状态*:
   - `entergamelogic.go`:已改,现在两处都写成"历史上的 14 `ErrUnsafeCrossNodeHandoff` 已不再发出"。
   - `04-asset-channel.md`::29 / :300 / :335 / :352 仍以已删的迁移包 / `HandleCrossZoneTransfer` 作论据(`HandleCrossZoneTransfer` 在 `cpp/` 下已无定义)。替换文本由帮会会话交用户,转给 09-21 接手人,本线不改。
   - `player_battle.cpp`:注释"跨 zone 迁移在途:实体只读"行号已漂到 :1107,由 Battle 会话改,措辞在冻结上限定稿后发给它。
   - `docs/notes/`:主会话 09-28 复核为无残留,本文未逐条复核。
5. **P3**:GO-6(`death_at` 写失败仍 ZREM——注意"写失败就不 ZREM"是错的修法)、GO-4 残余(`LeaveScene` 改 compare-and-delete)、告警盲区(整 zone 节点全灭无从触发)、`Hiredis.cc` 在 `redisvAsyncCommand` 返回 ERR 时泄漏 `CommandCallback`、`robot/connected()` 是个被 git 跟踪的 0 字节空文件(疑似 shell 重定向误操作残留)。2026-09-28 各项状态如下。
   - **GO-6、GO-4 残余、告警盲区:已落码待验证**,见设计文档「P3 收尾:GO-6 / GO-4 残余 / 告警盲区」。提交 `09f71f9d5`,未编译、未测试。
     - GO-6:`death_at` 没写进、或摘负载集失败时,不摘,改进有截止时间的推迟队列(`retryDeferredNodeDetaches`,日志前缀 `[ReentryBarrier][DeferDetach]`,计数 `node_detach_deferred_total{zone_id,outcome}`)。`world_init` 的改派补上 `!IsNodeAlive`。没有用原文写的"fullSync 返回 error"。
     - GO-4 残余:`LeaveScene` 改为按原文比对删除(`luaDeletePlayerLocationIfUnchanged`),删 location 与减人数在同一段 Lua 里提交。
     - 告警盲区:`nodes_by_role` 对已知 zone 与本进程 ZoneId 的四种角色发布 0 值;两条 `PoolEmpty` 追加 `or on (zone_id)` 的整 zone 全灭分支;新增 warning `SceneManagerNodeDetachWithoutDeathMark`。`deploy/k8s/scene-manager-alerts.yaml` 现共 19 条规则,**未经 promtool 校验**。
     - 新单测:`node_detach_deferred_test.go`、`leavescene_cas_test.go`、`nodes_by_role_metrics_test.go`。
   - **Hiredis ERR 泄漏:已落码 2026-09-28,未编译。**
     - `muduo_windows` 与 Linux overlay 两份 `Hiredis.cc` 逐字节一致。
     - 回归用例 4 个,在 `rpc_controller_test` 的 `HiredisCommandLifecycle.*`,新文件 `hiredis_command_lifecycle_test.cpp`。
     - 提交 `9da27f9a4` + `24dd3e6c5` + `6941e7344`。
     - 红态取法见「验证顺序」第 1 步。
   - **`robot/connected()`:删除已随 `9da27f9a4` 提交**。那是 09-28 的每小时自动保存,本机跟踪引用显示已在 `origin/main`。原先写的"已暂存在 index、待带路径 commit"已过时,不需要再执行任何 git 操作。
   - **`handoff_pending_no_marker` 比值告警:本轮不落。** 口径 A / B 的阈值和 `for` 都没有压测基线。等首轮双 zone 压测(`AllowUnsafeCrossNodeHandoff=false`)拿到 p99 后,再以 `severity: info` 起步落码。口径与校准步骤写在「P3 收尾」一节。

**验证顺序(2026-09-29;2026-10-01 按"GO-2 已全部进 main"改了第 2、4、5 步)**:由 Codex 执行,Claude 不跑。设计文档 §13 目前**没有**合并后的统一验证顺序,只有各小节自己的验证清单。细节来源是设计文档 §13 各小节的验证清单(13.1–13.6,GO-2 的在 13.7)与 `grpc-client-deadline-failure-callback.md` §9.2;先后顺序以本节列出的硬约束为准。等 §13 真有了统一顺序小节,再改回指向它。

1. **先取红态,再做任何全量构建。**
   - C++ Hiredis:只构建 `cpp/tests/rpc_controller_test/rpc_controller_test.vcxproj`,**不要构建 muduo**。这样它链接的是 09-25 的 `lib/muduo.lib`(09-29 查 mtime 仍为 2026-09-25 09:45,早于修复的 2026-09-28 03:41)。再跑 `--gtest_filter=HiredisCommandLifecycle.*`,期望前三个 FAIL、`AcceptedCommandCallbackRunsExactlyOnceWithNullReplyOnFree` PASS。任何一次 `game.sln` 全量构建都会重建 muduo,之后这个红态就**永久拿不到**了。
   - k8s 契约测试:红态要在 `c2c5ec505`(= `9da27f9a4^`)的临时 worktree 上取。HEAD 已含修复,在 HEAD 上跑是假绿。
   - Z1 的 Z-pre:见 0g 批注。
2. **GO-2 根治已全部进 main(2026-10-01 按 HEAD 核对),CPP-3 还在落码。**
   - GO-2 对 `scene_manager_service.proto` + `storage.proto` 的窄面 regen 已随 `80a1b0823` 做完,**不要再 regen**。产物在三处:三个暂存目录下的 proto 副本、`go/proto`、`cpp/generated`;`robot/vendor` 不含这个包,没有产物。以后若再做 C++ 窄面 regen,必须用仓库根作 proto_path,`935ec83b1` 就是在修内嵌 go_package 的漂移。
   - CPP-3 要改 `player_lifecycle.cpp`。整批编译排在它之前还是之后,目前没有成文的统一决定;执行前先用 `git log` / `git status` 确认 CPP-3 是否已提交,再定。
   - GO-2 这一项自己的先后顺序如下,细节在设计文档 §13.7 的验证清单:
     1. 在仓库根跑 `gofmt -l go/scene_manager go/shared`,应无输出。有输出就先处理格式,再往下跑(原因见第 2 条 GO-2 一段的残余 6)。
     2. 在 `go/scene_manager` 下跑 `go vet ./...` 与 `go test ./... -count=1`。其中 `owner_epoch_crosslang_test.go` 的 `TestSourceJudgeScriptAndInheritClearCopiesMatchCpp` 会读 C++ 头文件(`exit_release_mark.h`、`player_lifecycle.h`)里的 Lua 字面量,与 Go 副本逐字节比。它找不到 C++ 源码时是 **SKIP,不是失败**,所以要在完整仓库里跑,并确认这一条是 PASS。
     3. C++:编 scene 库(`scene.vcxproj`)与 `cross_zone_test`,跑 `TravelOwnership.*` 与 `TravelFreezeCap*`。
     4. runbook 场景 B1 / B2 / B3 与 §5 不变量(见第 5 步)。
3. **C++ MSBuild 一律串行 `/m:1 /nr:false`**,并和其他会话的构建错开,否则会报假的 C1041 / LNK1104。gRPC 失败回调批次的编译和单测(`grpc-client-deadline-failure-callback.md` §9.2)与本线合并在同一次全量编译里做。
4. **单测**:
   - `cross_zone_test`:`EnterSceneReplyRoute.*`、`EnterSceneReplyEcs.*`、`EnterSceneTransportFailureEcs.*`、`TravelFreezeCap*`,以及 09-21 那批的 `ExitPersist*` / `ExitRelease*` / `HandoffMarkWithdrawQueue.*` / `TravelOutcomeReset.*`。GO-2 新增 `TravelOwnership.*`(7 个);`HandleTravelMarkWriteRejected` 的代际用例是 `TravelFreezeCapEcs.LateMarkSetErrorOfOlderGenerationLeavesNewHandoffFrozen`。
   - `routing_identity_test`:`SceneRoute*`、`SceneEntry*`、`SceneLinkReady.*`。
   - `go/scene_manager`:`go test ./... -count=1`。GO-2 的用例在 `owner_epoch_test.go`(如 `TestPlanRouteRollback`、`TestClassifyRollbackReply`)与 `owner_epoch_crosslang_test.go`,先后顺序见第 2 步。
   - 客户端:在客户端 `origin/main`(`96d36da` 或之后,已含 CL-6 / CL-8)上跑 Unity 编译 + EditMode;分支 `crosszone/client-cl6-cl8` 已不是唯一落点。生成物只用单个 protoc 核对,**不跑 `gen_proto.ps1` 全量**,全量会漂 6 个文件。
5. **联调与故障注入**:按 runbook(`docs/ops/cross-zone-failure-test-runbook.md`)。runbook v2.5 已新增场景 H(冻结硬上限)、SE / SE2(CPP-2)、E2(death_at 写失败注入),静态编写、未实跑;以 runbook 的 Changelog 为准。场景 H 要用 Unity 客户端,原因是 robot 的 60s 预算先到,看不到上限分支。
   - GO-2:跑场景 B1 / B2 / B3,并逐场景核 §5 不变量。B1 的期望仍是"解冻 + tip,不踢线";B3 在 GO-2 二进制上按原文不执行,runbook 写的是"§7 记 SKIP"。回滚采纳的专项注入(设计文档 §13.7 验证清单里的 B4a / B4b / B5)10-01 查时 runbook 里还没有对应场景,步骤与期望只在 §13.7。

## 5. 给接手会话的开场提示词(可整段粘贴)

```text
你在接手「跨 zone 场景传送」的收尾。先按 AGENTS.md 的会话启动门禁读完必读文件,再读
docs/design/handoff-crosszone-20260920.md(先看文件头"验证现状"和 §0 最后一条)、
docs/design/cross-zone-scene-travel.md 的 §12 与 §13(各小节的验证清单),
以及 docs/design/grpc-client-deadline-failure-callback.md。
注意:
1. 编译与测试现状:
   - 09-21 那批 C++ 有编译证据,但跨 zone 单测从未运行。
   - 09-28 本轮的服务端代码全部未编译、未测试,包括关联号、冻结硬上限、CPP-2、
     scene_manager 三条 P3、Hiredis / zrpc Timeout;Go 侧同样没有任何 build / test 证据。
   - GO-2 根治(owner_epoch 严格单调 + 回滚回执)10-01 核对已全部进 main,
     同样未编译、未测试,连 gofmt 都没跑过。
   - 你不许编译 / 跑测试 / regen(AGENTS §10.1),改完给出可执行命令。
   - 验证顺序按交接说明 §4 末尾的「验证顺序」(设计文档 §13 没有统一顺序,只有各小节的验证清单)。
   - Hiredis 红态:任何 game.sln 全量构建之前,只构建 rpc_controller_test、链接 09-25 的 muduo.lib
     (hiredis_command_lifecycle_test.cpp 在 c2c5ec505 上不存在);
     k8s 契约测试红态:在 c2c5ec505 的临时 worktree 上取。
2. GO-2 根治(owner_epoch 严格单调 + 回滚回执)的代码已全部进 main,设计与判定表在设计文档 §13.7
   (代码注释里写的 §12.8 指的就是它)。提交:ccafe300c、907a6b752、80a1b0823(窄面 regen,不要再 regen)、
   a8c2d44a8、6b7a59287、567333aaf。三视角审查没有必须改项;偏离与残余见交接说明 §4 第 2 条的 GO-2 一段。
   给 Codex 的顺序:gofmt -l → go/scene_manager 的 vet / test(跨语言用例要 PASS 不能 SKIP)
   → C++ 编 scene 库与 cross_zone_test → runbook 场景 B1 / B2 / B3 与 §5 不变量。
   CPP-3(疏散改派待确认表)可能仍在别的会话落码,它改 player_lifecycle.*。
   先用 git log 和 git status 看是否已提交;没提交之前不要碰这些文件。
3. GO-3 本轮刻意不落(残余 R1 会违反交接不变量 I0 / I3),等用户拍板,不要自行开工。
4. 同一个 main 工作树上有别的 Claude 会话在改文件,还有每小时一次的自动保存会把半成品卷进 main:
   - 动手前 ListAgents + SendMessage 互报范围。
   - 提交只用 git commit -- <显式路径>,提交前 git diff --cached 逐行核对。
   - 不 stash / reset / checkout / rebase。
5. 客户端:
   - C-1 / C-2 / C-3 已在客户端远端 main 修好。
   - CL-6 / CL-8 落在客户端分支 crosszone/client-cl6-cl8(54034fe,基于 8b21583),
     09-29 已随合并提交 96d36da 推到客户端 origin/main(按机器 A 的本机跟踪引用);
     先 git -C ../mmorpg-client ls-remote origin 复核它确实在远端 main 上。
   - 客户端仓的改动需要用户单独授权(AGENTS §9),未获授权时只读。
6. 交接说明里标「上一会话报出,未核实」的条目,先在代码上核实再动手,不要默认它成立。
   「2026-09-28 起 C++ 生成客户端有失败回调」这件事推翻了旧文档里"非 OK 不回调"的前提;
   但交接路径上传输失败仍不作为证据。
现在先告诉我:
- 你在哪台机器上;
- origin/main 与本地 HEAD 各是什么;
- GO-2 的六个提交在不在本地 HEAD 与 origin/main 上(567333aaf 在 10-01 时只在机器 A 的本地 main);
- CPP-3 是否已提交;
- 客户端远端 main 是什么、CL-6 / CL-8 的 54034fe 在不在远端 main 上
  (分支 crosszone/client-cl6-cl8 本身按本机跟踪引用没有推成远端分支,提交是经合并提交 96d36da 进的 main)。
然后给出你打算做的第一件事。
```
