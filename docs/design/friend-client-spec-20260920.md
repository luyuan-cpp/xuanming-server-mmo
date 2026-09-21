# friend 客户端任务规格(Unity,2026-09-20)

> **复核说明(2026-09-20)**:客户端仓已从 `d2b165a` fast-forward 到 **`120e2d8`**(`main`,与 `origin/main` 持平)。本文按「`120e2d8` 已提交内容 + 当前工作区」重新做了只读核对。工作区里除了 `tools/gen_proto.ps1` 末尾那行未提交的 `"proto/friend/friend.proto"`,还发现有人跑过一次 handler 生成:11 个未跟踪的 `ClientPlayerFriend*Handler.cs`、`HandlerRegistry.cs` 多了 11 行、20 个 Team / Jubaozhai / TravelToZone 桩被改成 LF 行尾;`Proto/Generated/Friend.cs` 还没有生成(见 §1.1、§9)。本轮共核对 **40** 条差异(s0-s1 9、s2-s3 11、s4-s6 7、s5-s9 13),**确认 40 条,驳回 0 条**,都已在正文就地订正。基线变化导致的过期处注明「(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)」。
> **会让实现者做错事的更正**:
> ① proto-gen 只新增 Friend 的 11 个桩(92→103),不是 31 个;team / jubaozhai 已经在清单里,不用再补(§0-2、§1.2-§1.4)。
> ② `friend_error_tip.proto` 要加进 `$files`,tip 一律按生成的枚举写,不手抄数字(§1.2、§2.5、§5、§7)。
> ③ 样板改以 `GuildClient` 为主(推送 + 申请列表 + 审批),发号仍用桩常量(§0-4)。
> ④ 在途标志按「连接标识变了」来复位,不是「入场了」就复位;超时后要进入 `RequiresReconnect` 隔离(§4、§5、§8)。
> ⑤ UI 根的 `_playerId` 检测要忽略 0,并且不许清 Client 数据(§4 末段)。
> ⑥ sortingOrder 用 191;HUD 左右两列都已排满,(68,600) 已被帮会占用,入口位置待用户拍板(§3)。
> ⑦ 互斥补行共 9 处(§5)。
> ⑧ 测试可以放独立的 Friend 程序集,只能复制 `FakeBattleTransport`(§5);`client_compile_check.ps1` 不编测试(§8)。
> ⑨ Team 窗口的控件引用的是 TeamV2 切图,照抄时要改映射(§3)。

> **这是什么**:`docs/design/friend-handoff-20260920.md` §4.3 的展开 —— friend 功能在 Unity 客户端(同级独立仓库 `../mmorpg-client/`)要做的事,分「生成管线」和「手写业务层」两部分。
> **依据**:2026-09-20 对客户端仓(HEAD `d2b165a`)与服务端仓(HEAD `d9e471b80`)的**只读**核对,用户已授权只读客户端仓。全程没有跑任何构建 / 生成 / 测试命令,两个仓都没有被修改。(同日按客户端 `120e2d8` + 工作区复核,见上方复核说明。)
> **还没有的授权**:**修改**客户端仓。本文落在服务端仓正是因为这个原因;开工前先向用户要客户端任务授权(`AGENTS.md §9`)。客户端仓自己的惯例是每个功能在 `Docs/` 下一篇(如 `Docs/team-ui.md`,新近功能则用 `Docs/GuildUI.md` / `Docs/SocialUI.md` 这类命名),开工后应把本文的 UI 部分沉淀成 `Docs/FriendUI.md`。(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)
> **行号只是辅助坐标,定位一律按符号名 grep。** 标「未核实」的条目没有读到证据,接手人需自行确认。
> **纪律**:按服务端 `AGENTS.md §10.1`,Claude 不执行编译 / 生成 / 测试命令,这些交给 Codex 或人工;没有运行结果之前不得声称"编译通过""测试绿"。

---

## 0. 结论速览(先读这七条)

1. **生成的 handler 桩不是接线点。** `HandlerRegistry.Register` 在客户端**全仓零调用**(唯一出现处是它自己的定义)。`GameClient.WireSceneNotifyHandlers` 里(msg 124 处理器上方)的注释明确警告:`OnNotify` 是**覆盖语义**,一旦有人把 `HandlerRegistry.Register` 接上,生成的空壳会**静默盖掉**手写的 `RedirectToGate` 处理器。注释原文只点名 124,但同理也会盖掉 `NotifyEnterScene`:`HandlerRegistry` 里同样登记了 `SceneSceneClientPlayerNotifyEnterSceneHandler`。手写代码只用桩的常量 `XxxHandler.MessageId`,桩的 `Parser` 零引用;解析一律用 proto 类型自己的 `Xxx.Parser`(如 `EnterSceneS2C.Parser`)。所以"proto-gen 生成出 `ClientPlayerFriendNotifyFriendEventHandler.cs`" ≠ "客户端接上了推送" —— 接线要手写。
2. **客户端 `tools/gen_proto.ps1` 只缺 friend,而且已补在工作区。** 客户端 main 从 `120e2d8` 起,`$files` 已收录 `proto/team/team.proto`、`proto/trade/jubaozhai.proto`,以及 guild / scene / trade / common / team 五份 `generated/code/proto/tip/*_error_tip.proto`。`proto/friend/friend.proto` 这一行已补在客户端工作区末尾,尚未提交,须随本批生成物一起提交。服务端按默认配置(`enable_unity_client: true`)跑 proto-gen,客户端只会新增 Friend 的 **11** 个桩(现有 92 → 103)。如果手上的 `gen_proto.ps1` 缺 friend 那一行,这 11 个桩会报 CS0246。(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)
3. **两个生成脚本的默认 `-ProtoRoot` 是错的。** `gen_proto.ps1` 与 `gen_messageids.ps1` 的默认值都是 `$PSScriptRoot/../../..`(按"客户端是 `<服务端>/client/unity` 子模块"的旧布局写的;客户端 `README.md` 这一段同样过期)。两仓同级布局下它解析到两个仓共同父目录的再上一级,目录存在所以参数绑定不报错,要到 protoc 找不到 proto 才失败。**必须显式传 `-ProtoRoot <服务端仓根>`。**
4. **样板分两类挂法。**(1)挂在 `GameClient` 构造函数上的单例 Client:`PetClient`、`PlayerFeaturesClient`,写法是 `Pets = PetClient.Attach(new GameClientBattleTransport(this));`。防旧回包的三重过滤照抄 `PlayerFeaturesClient`。(2)由各自 UiRoot 在 `Update` 里懒建的 Client:`GuildClient`(`Assets/Scripts/Game/Guild/`,由 `GuildUiRoot` 建)、`JubaozhaiClient`(`Assets/Scripts/Game/Jubaozhai/`)、`SocialClient`(Chat,`Assets/Scripts/Game/Social/`)。它们的构造形如 `new GuildClient(new GameClientBattleTransport(game), () => game.GateConnectionIdentity)`,并用 `ObserveConnection()` 比对 `GateConnectionIdentity` 来识别换 gate。配套文档是 `Docs/GuildUI.md`、`Docs/JUBAOZHAI_UI.md`、`Docs/SocialUI.md`。

   形态最接近 friend 的是 `GuildClient`:C2S 请求,加一条 `RegisterNotify(MessageIds.NotifyGuildChanged, …)` 推送,再加 `ListGuildApplications` / `ReviewGuildApplication` 待审批列表,与 friend 的请求列表及同意/拒绝一一对应。其次是 `PetClient`。

   注意发号来源:Guild 没有生成桩,走 `MessageIds.*`;friend 会生成 `ClientPlayerFriend*Handler` 桩,发号应照 `SocialClient` 用 `ClientPlayerXxxHandler.MessageId`,不要照抄 Guild 的 `MessageIds.*`。

   同意/拒绝列表、分页、入口文字带计数照抄 `TeamWindow` / `TeamUiRoot`。Team 目前仍只有 UI 和视图状态(`TeamUiState`),没有网络层。15 个 `ClientPlayerTeam*` 桩与 `Proto/Generated/Team.cs` 已经生成,但 `Docs/team-ui.md` 写明「服务端仍未提供主城组队创建、申请、同意、拒绝或成员资料 RPC」。

   friend 选哪种挂法、两种挂法怎么取舍,见 §5。(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)
5. **客户端没有这些东西,不要去找,也不要为 friend 新造**:红点系统、全局 tip 文案表(`README.md`:「Localization: tip table loader not yet wired」)、UIManager / 面板注册表、「登录后首拉」的先例(现有功能全是开面板才拉)、自动重连(断线后回选服界面)。
6. **friend 在客户端零存量(以已提交内容 `120e2d8` 为准)。** 没有手写代码、UI 资源、生成桩、`Friendpb` 类型、消息号常量。和 friend 相关的改动都在工作区、尚未提交:`tools/gen_proto.ps1` 清单末尾加了 `"proto/friend/friend.proto"` 一行(附注释说明刻意不收 `friend_table.proto`);另有一批未跟踪的 Friend 桩(见 §1.1、§9)。`git grep -il friend HEAD` 只命中无关的战斗特效素材名 `qdao_battle_spawn_ring_friendly_*`,以及引用它的 `SpawnRingFriendlyPath`(`Assets/Scripts/UI/Ugui/Battle/BattleArtCatalog.cs`、`Assets/Tests/EditMode/Battle/BattleArtCatalogCacheTests.cs`、`Docs/VerificationEvidence/tianyong-2026090{9,10}/*.json`)。另外还命中 `ProjectSettings/ProjectSettings.asset`(`pnFriends`)。这个命中集合和旧基线 `d2b165a` 完全一样。不带 `HEAD` 跑 `git grep -i friend` 会搜工作区,因此还会多命中 `tools/gen_proto.ps1` 与那批 Friend 桩。(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)
7. **硬前置**:服务端先跑完导表器与全量 proto-gen(交接文档 §2 第 2、3 步)。当前 `proto/message_id.txt` 里 friend 仍是旧的 8 个 `FriendService*`,11 个 `ClientPlayerFriend*` 的号还不存在。

---

## 1. 生成管线

### 1.1 客户端仓的规范

- 仓内没有 `AGENTS.md` / `CLAUDE.md`;成文规范只有 `README.md` 和 `Docs/*.md`。
- **生成物纳入版本管理**(`.gitignore` 注释原文:「Generated/*.cs IS committed … Re-run that script after .proto changes and commit the result.」)。已跟踪(含 .meta 与 .gitkeep):`Proto/Generated` 80、`Net/Generated` 187、`Table/Generated` 144。**`.meta` 文件要一并提交**(先例:`1d3a0a7 补齐宝宝系统 7 个缺失的 .meta`)。注意:`Proto/Generated/SceneErrorTip.cs` 被跟踪了,但没有 `.meta`(b15c94f 漏提),这是整个目录里唯一缺 `.meta` 的 `.cs`(`.gitkeep` 本来就不需要 meta)。它不属于本批,不要顺手补;本批新增的每个生成文件都必须带 `.meta` 一起提交。(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)
- **UI 是原生 uGUI + TextMeshPro。** FairyGUI 已在 `69e22a0` 移除;仓根的 `FairyGUI.csproj` / `FairyGUI-Editor.csproj` 是未跟踪的旧 IDE 残留,忽略。
- 客户端仓现状(HEAD `120e2d8`):分支 `main`,与 `origin/main` 持平(本次未重新 fetch)。原先那条 `M ProjectSettings/ProjectSettings.asset` 已经没有了。生成目录上次改动:`Proto/Generated` 是 `b15c94f`(09-19),`Net/Generated` 是 `fffd0da`(09-18),`Table/Generated` 是 `4fb0515`(09-18)。工作区(2026-09-20 复核时)有这些未提交改动:
  - ` M tools/gen_proto.ps1`:末尾补了 `proto/friend/friend.proto` 一行,属于本批,要随生成物一起提交。
  - 11 个未跟踪的 `ClientPlayerFriend*Handler.cs`(没有 .meta)。
  - ` M HandlerRegistry.cs`:多了 11 行 Friend 注册。
  - 20 个 Team / Jubaozhai / TravelToZone 桩只改了行尾(LF),内容不变,**不应随本批提交**。

  其中桩和注册那部分是某次未提交的 handler 生成留下的,处置见 §9。(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)

### 1.2 `tools/gen_proto.ps1`

- proto 源:`--proto_path=$ProtoRoot`,`$ProtoRoot` 默认值见上(错的,必须显式传)。
- protoc:`[string]$Protoc = "protoc"`,走 PATH;仓内 `tools/` 下没有 `protoc.exe`。服务端仓自带一份:`third_party/grpc/install_vs2026_dbg/bin/protoc.exe`(`libprotoc 35.1`),可用 `-Protoc <绝对路径>` 传入。⚠ **protoc 35.1 生成的 C# 与客户端内置的 Google.Protobuf 3.28.3 运行时是否兼容,未核实**(现有 `Proto/Generated` 是用哪个版本的 protoc 生成的也未核实)—— 重生成后若出现大面积无关 diff 或编译错误,先查这一条。
- 输出:`Assets/Scripts/Proto/Generated`,平铺,文件名由 proto 文件名转 PascalCase。
- `$files` 中货币块(`player_currency.proto` 上方)的注释原文:「handler 生成器…是按服务端 proto 全量出 handler 的,这份清单漏配哪个 proto,就会多出一批引用不存在类型的 handler(CS0246)。服务端新增 proto 时,这里必须同步加,否则下次重生成必红。」
- **清单里已有这三行,不用再补**(客户端 `120e2d8` + 工作区,2026-09-20 核对):`proto/trade/jubaozhai.proto`(连同 `trade_error_tip` / `common_error_tip`)和 `proto/team/team.proto`(连同 `team_error_tip`)由 `120e2d8` 带入。`"proto/friend/friend.proto"` 是工作区未提交的改动,附两行注释,并给 `team_error_tip` 行补了逗号;它现在是末项,没有尾逗号。实现者只需确认 friend 行还在,并随本批一起提交(本批提交里要显式列出 `tools/gen_proto.ps1`)。(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)
  三个 proto 的 import 只有 `proto_option` / `empty` / `tip`,都已在清单里。`Trade` / `Teampb` 已随新基线生成并被手写层使用;`Friendpb` 与现有手写代码不撞名(已核实,见 §9)。
- `generated/code/proto/tip/friend_error_tip.proto` **要同批加进 `$files`**:加在 friend 行之后,并给 friend 行补逗号,新末项不带尾逗号。**〔2026-09-20 晚已补在客户端工作区,未提交;随本批生成物一起提交〕**理由:
  - 已有 guild / scene / trade / common / team 五个先例,对应生成了 `Proto/Generated/{Guild,Scene,Trade,Common,Team}ErrorTip.cs`。
  - tip proto 没有 package,枚举进全局 namespace;现有五份并存未撞名,`friend_error` 与它们同形。
  - 清单注释写明,客户端要按码分辨就必须有这份枚举,AGENTS §7.5 禁止在客户端手抄 tip 数字。
  - handler 编译本身不依赖它,但 §2.5 / §5 的错误码展示要直接用它的枚举。生成后得到 `FriendErrorTip.cs`(`enum friend_error`)。

  **时序要求**:服务端 `generated/code/proto/tip/friend_error_tip.proto` 目前只到 `kFriendTooManyPending = 15006`。要等导表器跑完、15007–15009 落表之后再跑 gen_proto,否则枚举会缺码。原先「无先例、客户端没有 tip 枚举」的理由作废。(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)

### 1.3 `Assets/Scripts/Net/Generated`

- 一个 `HandlerRegistry.cs` + 平铺的 `Handlers/`。现有 92 个桩(`120e2d8` 已提交):BattleClientPlayer 12 / ClientPlayerChat 2 / ClientPlayerJubaozhai 4 / ClientPlayerLogin 6 / ClientPlayerTeam 15 / Scene\* 53(Activity 1、Attribute 9、Bag 2、ClientPlayerCommon 3、Currency 5、Mission 3、Movement 8、Pet 9、Scene 9(含 TravelToZone)、Skill 4)。`HandlerRegistry.cs` 有 92 条注册。和原来的 72 个比,新增的是 Team 15、Jubaozhai 4、`SceneSceneClientPlayerTravelToZone` 1。没有桩的是 Guild(0 个:GuildService 名字里没有 ClientPlayer,不出桩)和 Friend。(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)
- 生成器过滤规则(服务端 `tools/proto_generator/protogen/internal/generator/unity/unity_client_handler.go`):服务带 `OptionIsClientProtocolService`,**且**服务名含 `ClientPlayer` 或 `GamePlayer`。所以 `MatchService`、`GuildService` 不出桩;`SceneRollbackClientPlayer` 的 12 个方法只标了 `OptionIsPlayerService`、没标客户端协议,也不出桩。
- S2C 方法的响应是 `Empty` 时,桩的 `Parser` 回落用请求类型 —— `NotifyFriendEvent` 对应 `Friendpb.FriendEventS2C`,正好可用。
- 生成器只创建或覆盖文件,**不会删除旧文件**;`HandlerRegistry.cs` 每次整体重写。
- **消息号一致性基线**:以客户端 `120e2d8` 已提交的内容为准,92 个桩的 `MessageId`,以及 `Net/MessageIds.cs` 的 102 个常量(`d2b165a` 时是 72 / 73),与服务端 `proto/message_id.txt` 逐一比对,**当前零漂移**。重生成后再比一次:这 92 + 102 个值**应当一个都不变**,新增的 11 个 Friend 桩的号也应与 message_id.txt 一致。有任何一个变了就是出了意外,停下来查。不要拿工作区当基线:工作区已被人跑过一次 handler 生成,多了 11 个未跟踪的 Friend 桩。(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)
- 期望新增的 friend 桩(按生成器规则 `Service+Method+"Handler"` 推导):`ClientPlayerFriendGetFriendListHandler.cs` … `ClientPlayerFriendNotifyFriendEventHandler.cs`,共 11 个。截至复核时,它们已作为未跟踪文件出现在工作区,但不是出自一次完整的同批生成,见 §9。

### 1.4 生成侧操作清单(交给 Codex / 人工执行)

0. 先确认以下事项。(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)
   - 客户端工作区里 `proto/friend/friend.proto`(以及按 §1.2 补的 friend tip 行)还在。
   - 没有别的会话在同时改客户端协议。
   - 工作区里那批未提交的 Friend 桩和行尾改动已按 §9 处置。

   如果 friend 行缺了、又暂时不补,服务端就用 `enable_unity_client: false` 的配置副本跑 proto-gen,本节推迟。
1. 环境:PATH 上要有 `protoc`(或给脚本传 `-Protoc`);导表器要 Python。缺哪个先停下来报告。
2. 客户端仓 `git status --porcelain` 存基线。
3. 服务端先跑导表器(会经 deploy 覆盖客户端 `Table/Generated`)。跑完核对客户端 diff 应只有表相关变化(会包含新表 `RoleNameRule` —— 帮会二期 B3a 的内容,见交接文档 §2 第 2 步)。服务端 `friend_error_tip.proto` 应补齐 15007–15009。
4. 核对 `$files` 已含 `proto/friend/friend.proto` 与 `generated/code/proto/tip/friend_error_tip.proto`。这处工作区改动随生成物一起提交。(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)
5. 服务端跑一次全量 proto-gen(默认配置)。期望客户端 `Handlers/` 新增 11 个 `ClientPlayerFriend*Handler.cs`,`HandlerRegistry.cs` 重写为 103 条注册,现有 92 个桩的 `MessageId` 不变。(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)
6. 客户端依次执行 `pwsh -File tools/gen_proto.ps1 -ProtoRoot <服务端仓根>` 和 `pwsh -File tools/gen_messageids.ps1 -ProtoRoot <服务端仓根>`。期望新增 `Friend.cs` 与 `FriendErrorTip.cs`,其余生成文件(包括 `Team.cs` / `Jubaozhai.cs` / `PlayerScene.cs`)不应有实质 diff;如果有,说明服务端 proto 在 120e2d8 之后又变过,停下来查。(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)
7. 验证:`pwsh -File tools/client_compile_check.ps1`(离线 Roslyn,只编 `Assets/Scripts`,退出码 0 = 过;CS0246 = `$files` 漏了 proto)。之后用 Unity 打开工程一次生成新文件的 `.meta`,**`.meta` 与生成物一起提交**。
8. 同批发布:gate 的 `rpc_event_registry`、路由服的 `route_table`、robot,以及 Unity 的 Handlers / Proto / MessageIds,必须出自第 5 步的同一次生成。

---

## 2. 样板分层与两条链路

### 2.1 分层(以 Pet 为例)

| 层 | 文件(相对客户端仓根) | 关键符号 |
|---|---|---|
| 生成桩(不手改) | `Assets/Scripts/Net/Generated/Handlers/ScenePetClientPlayerNotifyPetListChangedHandler.cs` | `partial class`,含 `const uint MessageId`、`Parser`、空的 `static partial void Handle(GameClient, T)`、`Dispatch` |
| 生成注册表(**从未被调用**) | `Assets/Scripts/Net/Generated/HandlerRegistry.cs` | `HandlerRegistry.Register(GameClient)` |
| 消息号 | `Assets/Scripts/Net/MessageIds.cs`(`tools/gen_messageids.ps1` 按白名单生成) | `MessageIds.GetPetList` 等 |
| 传输接缝 | `Assets/Scripts/Game/Battle/IBattleTransport.cs` | `PlayerId`、`IsReady`、`event Disconnected`、`RegisterNotify`、`Call<TResp>`、`SendOneWay` |
| 传输生产实现 | `Assets/Scripts/Game/Battle/GameClientBattleTransport.cs` | 原样转发到 `GameClient.OnNotify` / `Call` / `OnDisconnected` |
| 管线 | `Assets/Scripts/Game/GameClient.cs` | `Call<TResp>`、`SendOneWay`、`OnNotify`、`DispatchInbound`、`WireSceneNotifyHandlers`、`event OnSceneEntered` |
| 手写逻辑兼数据模型(单例,不引用 UnityEngine) | `Assets/Scripts/Game/Pet/PetClient.cs` | `PetClient.Attach(transport)`、`static Instance`、属性 `Pets`/`HasList`/`Busy`、事件 `OnList`/`OnBusyChanged`/`OnError` |
| 挂接点 | `GameClient` 构造函数 | `Pets = PetClient.Attach(new GameClientBattleTransport(this));` / `Features = PlayerFeaturesClient.Attach(...)` |
| UI 根(自举单例 MonoBehaviour) | `Assets/Scripts/UI/Ugui/Pet/PetUiRoot.cs` | `[RuntimeInitializeOnLoadMethod(AfterSceneLoad)] AutoSpawn`、`EnsureBound`、`ShowToast` |
| UI 面板(纯 C# 类,代码搭控件) | `Assets/Scripts/UI/Ugui/Pet/PetPanel.cs` | `Show()`(内部调 `RequestList()`)、`ApplyList`、`SetStatus` |

新基线还有第二种现行挂法(Guild / Jubaozhai / Social 用;Mail 在 UiRoot 里用法相同):
- Client 不进 GameClient,由 UiRoot 持有,换连接时重建。
- 构造时传 `() => game.GateConnectionIdentity`(GameClient.cs:156,`public object GateConnectionIdentity => _gate;`,静默重登录 / 重定向都会换成新对象)。
- 回包到达时用 `ReferenceEquals(发请求时记下的标识, _identity())` 判断,丢掉重定向之前发出的请求的回包(参照 SocialClient.cs 的 `Current(...)`;ZoneTravelClient / CityTravelUiRoot 也用同一个标识)。

friend 仍按本规格挂进 GameClient 构造函数,理由是首拉不依赖 UI 根存活。构造时同样传 `() => this.GateConnectionIdentity`,回包过滤时额外比对这个标识。它补上了 §4 说的「重定向不触发 Disconnected」那个缺口,但只是多一道校验,还要配合 §4 时序细节 2 的按连接标识复位。(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)

数据模型没有单独的 Model 类,状态就是 Client 单例上的属性,类型直接用 proto 生成类。

### 2.2 链路 A:收到 S2C → UI 刷新

`GateTcpClient` 读线程入 inbox → `AppBootstrap.Update` → `GameClient.Tick` → `_gate.Poll()` → `GameClient.DispatchInbound`:`Id != 0` 且命中 pending 走回包;`Id == 0` 且该 `message_id` 有 FIFO pending 走回包;否则查 `_notifyHandlers[mc.MessageId]` → Client 构造时经 `_net.RegisterNotify(...)` 登记的处理器 → `Parser.ParseFrom(content.SerializedMessage)` → 更新属性、触发事件 → UiRoot 订阅者 → 面板重绘。

**GameClient 自有的推送不许子模块再注册。**(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)
- `GameClient.WireSceneNotifyHandlers` 已为场景层推送登记了处理器,包括 msg 23 `MessageIds.TipToClient`、msg 124 `MessageIds.RedirectToGate`、msg 79 `MessageIds.NotifyEnterScene`、`MessageIds.KickPlayer` 等。
- `OnNotify` / `RegisterNotify` 是覆盖语义。子模块对其中任何一个消息号再注册一次,都会把 GameClient 的处理器静默盖掉(例如跨 zone 传送的失败判定和重定向会失效),而且不报任何错。
- 服务端通用 tip 推送的处理器会在跨 zone 传送判定之后调 `OnServerTip?.Invoke(tip)`。要听 tip 一律订阅 `GameClient.OnServerTip`(`event Action<TipInfoMessage>`;先例:`CityTravelUiRoot`、`DevAutoPilot`)。
- friend 不依赖 msg 23:它的业务错误走回包信封(`mc.ErrorMessage`)和响应体里的 `ErrorMessage`(见 2.3)。所以 FriendClient 不订阅 `OnServerTip`,也不对上述任何消息号调 `RegisterNotify`。

### 2.3 链路 B:点按钮 → C2S → 回包 / tip

面板按钮 → Client 方法 → `BeginWrite`(检查 `_net.IsReady`、本地校验、`Busy` 单飞)→ `_net.Call(messageId, req, Parser, onResp, onErr)` → `GameClient.Call` 包 `ClientRequest{Id=自增, MessageId, Body}` 发出,协程等待,**15 秒超时** → 回包分三种:

- **信封级**错误(`mc.ErrorMessage.Id != 0`)→ `onError("server tip=N")`;
- 正常 → 解析响应体 → `onResp`;
- 本地失败 → `onError`。`GameClient.Call` 给出的字符串为 `"not connected"` / `"send failed: …"` / `"disconnected"` / `"rpc timeout"` / `"parse response: …"`。此外,传输层 `GameClientBattleTransport.Call` 在 `GameClient.CoroutineRunner` 未接线时,不进 `GameClient.Call`,直接给出 `"协程宿主未就绪(CoroutineRunner 未接线)"`。注意 `IsReady` 只看 `IsGateReady`,`BeginWrite` 的就绪检查拦不住这一种。

`onResp` 里再看**响应体**的 `resp.ErrorMessage`(`HasTip`),有 tip 就 `Fail(DescribeTip(...))`。

经 Go 服务(gate → client_rpc_router → gRPC)的请求,先例是 `BattleClient` 调 `MatchService*`;friend 走同一条路。回包关联是两级:先按 `MessageContent.id` 精确匹配(scene 路径会回显 id),`id == 0` 时按 `message_id` FIFO 匹配(gRPC 路径)—— 所以同一个 message_id 并发多发时按先进先出配对,**friend 每类请求单飞即可规避错配**。超时是例外(超时分支已删掉在途记录,迟到回包会被下一次同号请求误领),见 §5「超时隔离」。

### 2.4 消息号怎么取

两种写法并存:
(a) `MessageIds.X`:要改 `tools/gen_messageids.ps1` 的白名单,并重新生成 `Net/MessageIds.cs`。使用者有 `GameClient`(登录 / 进场景 / 移动 / 通知 / tip),以及 Pet / Attribute / Battle / Spectate / Guild / Jubaozhai / ZoneTravel。
(b) 直接用桩常量,如 `SceneBagClientPlayerGetBagHandler.MessageId`、`ClientPlayerChatPullChatHistoryHandler.MessageId`:`PlayerFeaturesClient` 和 `SocialClient` 这样用,不用改白名单。

**friend 用 (b)**,理由有三:
1. `Block` / `Unblock` 这种短名放进 `MessageIds` 有歧义。
2. 桩常量与路由表出自同一次 proto-gen,天然满足"同批生成"。
3. (a) 要求白名单和 `MessageIds.cs` 两处同步重生成,漏一步就断。截至客户端 `120e2d8`,白名单已经加了帮会二期的 8 项,但 `MessageIds.cs`(102 个常量)没有重生成,`GuildClient` 引用的 `MessageIds.NotifyGuildChanged / ListMyGuildApplications / ListGuildApplications / CancelGuildApplication / SetGuildMemberRole / KickGuildMember / TransferGuildLeader / ReviewGuildApplication` 在其中都不存在,这就是现成的反例。friend 的 11 个桩常量出自 proto-gen 本身,不会有这种错位。

(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)

### 2.5 tip 与限频

- **tip 转文案没有全局文案表**(README 里「Localization: tip table loader not yet wired」仍成立)。信封级 tip 统一变成 `"server tip=N"`。响应体 tip 由各 Client 自己的 `DescribeTip` switch 翻译,未收录的退回 `"tip=N"`。客户端现在已有导表器生成的 tip 枚举(`Assets/Scripts/Proto/Generated/` 下的 CommonErrorTip / GuildErrorTip / SceneErrorTip / TeamErrorTip / TradeErrorTip.cs)。**分支一律写枚举名,不写数字**(AGENTS.md §7.5:号由导表器发,手抄的数字下次导表就可能对不上)。先例:
  - `GameClient.DescribeTravelTip`:`(uint)scene_error.KZoneTravelInBattle => …`
  - `GuildClient`:约 387 行起,`(uint)guild_error.KGuildFull => …`
  - `JubaozhaiClient.DescribeTip`:第 140 行起,`(uint)trade_error.KTradeListingNotFound`、`(uint)common_error.KServiceUnavailable`

  `PetClient.DescribeTip` / `AttributeClient.DescribeTip` 里手抄的 26000/25000 段数字是旧写法,**不要照抄**。friend 写成 `(uint)friend_error.KFriendXxx => …`,再加 `(uint)common_error.KRateLimitExceeded` / `KInvalidParameter` / `KServiceUnavailable => …`。前提是按 §1.2 把 `friend_error_tip.proto` 加进清单。(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)
- **限频被拒(`common_error.KRateLimitExceeded`)客户端只有部分处理,没有专门文案**。(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)现有处理在 `Assets/Scripts/Game/Social/SocialClient.cs` 的 `IsDefiniteRejection`:
  - 先认信封串前缀 `"server tip="`,再用 `uint.TryParse` 取码,然后与 `(uint)common_error.KInvalidParameter / KFeatureUnavailable / KRateLimitExceeded / KMessageSizeExceeded` 比较。
  - 命中即视为明确拒绝:不重试、不要求重连,只给一句笼统提示。
  - 它**刻意不收** `KServiceUnavailable`(注释:这个码也代表上游超时,那次发送可能已落库)。

  friend 的 `DescribeTransportError` 沿用这个解析方式(前缀 + TryParse + 与 `(uint)common_error.Kxxx` 比较,不写 1008 / 1003 字面量),但要给限频单独的文案。这个解析只覆盖信封级;限频有两条到达路径,friend 都要处理:
  1. **gate 限流器**:`cpp/nodes/gate/handler/rpc/client_message_processor.cpp` 的 `CheckMessageLimit` 被拒时回**信封级** `KRateLimitExceeded` → 客户端表现为 `onError("server tip=<该码>")`。⚠ **同一函数里每次被拒还会计入该会话的非法包计数(`IllegalPacketCounter::RegisterAndShouldKill`),到阈值 gate 直接 `forceClose`。所以被限频后不得自动重试。**
  2. **friend 服务自己的每分钟配额**(仅 `AddFriend`)回**响应体** 1008(`go/friend/internal/constants/constants.go` 的 `ErrRateLimited`)。
- 其他会出现的 common 码:1005 `kInvalidParameter`;1003 `kServiceUnavailable`(服务端存储故障,以及路由服把一切 gRPC 错误翻成的信封码 —— 客户端调 `NotifyFriendEvent` 被拒也是它)。
- friend 段的码:客户端代码按 `friend_error` 枚举名引用,编号由导表器发,客户端不关心具体数字。下表的数字只供服务端对照,客户端不得抄。(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)

| 码 | 枚举名 | 表里的文案 |
|---|---|---|
| 15000 | FriendCannotAddSelf | 不能添加自己为好友 |
| 15001 | FriendAlreadyFriends | 你们已经是好友了 |
| 15002 | FriendListFull | 好友列表已满 |
| 15003 | FriendRequestAlreadySent | 已发送过好友申请 |
| 15004 | FriendTargetListFull | 对方好友列表已满 |
| 15005 | FriendNoPendingRequest | 没有待处理的好友申请 |
| 15006 | FriendTooManyPending | 待处理的申请过多 |
| 15007(推断) | FriendBlocked | 对方已将你拉黑 |
| 15008(推断) | FriendBlockListFull | 黑名单已满 |
| 15009(推断) | FriendTargetInboxFull | 对方的好友申请已满 |

  ⚠ **15007 的文案与服务端设计意图矛盾**:`ErrBlocked` 注释写明这个码双向共用、刻意不泄露方向,表里的文案却点明了方向(交接文档 §2 第 2 步已把它列为需要用户拍板的事项)。**客户端一律展示中性文案**(如「暂时无法添加对方为好友」),不管服务端最后怎么定。

---

## 3. UI 约定

- **面板不是预制体,全部用代码搭。** 运行时加载的 UI 预制体只有选服界面(`Assets/Resources/UI/Ugui/Prefabs/QdaoServerSelect.prefab`,由 `QdaoUguiRuntime` 经 `Resources.Load` 加载)。`Assets/Prefabs/UI/JubaozhaiOfflinePreview.prefab` 不是面板预制体范式,不要仿照:它是编辑器菜单 `MMORPG/UI/Jubaozhai/Build offline preview scene` 生成的离线预览宿主(只挂 `JubaozhaiPreviewHost`),运行时不加载。(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)功能窗口用 `QdaoUguiFactory`(`CreateRect` / `CreateCenteredRect` / `CreateStretch` / `CreateImage` / `CreateText` / `CreateInputField` / `ConfigureHudCanvas`)与 `GameplayUiArt`(`Art` / `Text` / `Button` / `Clear`,`Assets/Scripts/UI/Ugui/Gameplay/GameplayUiArt.cs`)。
  - **素材**:在 `Assets/Resources/UI/Ugui/GameplayV1/`(`main_frame`、`title_plate`、`content_panel`、`section_header`、`tab_normal`、`tab_selected`、`button_primary`、`button_secondary`、`button_disabled`、`portrait_frame`、`close`、`lantern`、`close_tassel` 等),背包 / 任务 / 活动窗(`GameplayUiRoot`)用这套。组队、帮会、聚宝斋、邮件、聊天后来各自有独立切图和对应的 art 类:`TeamUiArt` → `TeamV2/`,`GuildUiArt` → `GuildV2/`,`JubaozhaiArt` → `JubaozhaiV1/`,`MailUiArt` → `MailV1/`,`SocialUiArt` → `SocialV1/`。其中 `TeamUiArt` 只有 `lantern` / `close_tassel` 回落 GameplayV1,`Clear` 委托给 `GameplayUiArt.Clear`;`TeamUiRoot` 里只有 HUD 入口按钮仍用 `GameplayUiArt.Button`。**friend 一期直接用 `GameplayUiArt` + GameplayV1,不新增美术。**
  - ⚠ **照抄 `TeamWindow` 时要改切图映射**:它通过 `using static …TeamUiArt` 引入控件,用到的 `section_plate`、`member_row`、`empty_slot`、`application_card`、`badge_leader`、`badge_self`、`status_online`、`status_offline` 只在 TeamV2 里有,GameplayV1 没有。只改 using 会在 `RequireSprite` 处运行时抛异常。这些 key 要逐个改映射:分区标题用 `section_header`;行底板和空位用 `content_panel`(或 `QdaoUguiFactory.CreateImage` 纯色底);在线状态用纯色圆点或文字「在线/离线」;徽章用文字。不要直接引用 TeamV2 的切图。(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)
- **没有 UIManager。** 每个功能一个 `XxxUiRoot : MonoBehaviour`,`[RuntimeInitializeOnLoadMethod(AfterSceneLoad)]` 自举,`DontDestroyOnLoad`,自带 Canvas。CanvasScaler:`ScaleWithScreenSize`,参考分辨率 2560×1080,`ScreenMatchMode.Expand`。现有 `sortingOrder`:选服/主画布 100 / QdaoUguiFactory 140 / 角色流 150 / 属性 160 / 宝宝 170 / 背包任务活动 180 / 地图 185 / 组队 186 / 帮会 187 / 聚宝斋 188 / 仙笺(邮件)189 / 仙友会(聊天)190 / 战斗 200(预览宿主与 Editor 验证画布另用 188/190/350/360/500)→ **friend 建议 191**(高于仙友会 190、低于战斗 200;191–199 当前空闲,开工前在客户端仓再 grep `sortingOrder\s*=` 确认未被占)。(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)
- **打开用 `Toggle()`,打开前手动互斥**:调其他根的 `Instance?.HidePanel()`。新增 friend 后,其余各根的 `Open` / `Toggle` 里也要各补一行 `FriendUiRoot.Instance?.HidePanel()`(完整清单见 §5)。
- 可用性判定:`game.InGame && game.IsGateReady && !(BattleUiRoot.Instance?.IsBattleLayerVisible ?? false)`,不可用时自动 `HidePanel`。快捷键在 `Update` 里读,先判 `IsTyping()` 和 `GameplayInputGate.IsKeyboardBlocked`。窗口根节点挂 `GameplayInputBlocker`(`Assets/Scripts/World/Tianyong/GameplayInputGate.cs`)阻挡地图移动。
- **HUD 入口**:新基线两列入口都已排满。(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)
  - 右侧列:`BattleUiStyle.HudEntryY(0..7)`,公式 176+i*104,再排第 8 格会到 1008+80=1088,越出设计高。
  - 左侧列(x=68):组队 496 [T]、帮会 600 [G]、聚宝斋 704 [U]、仙笺 808 [N]、仙友会 912 [C],步长 104、高 80(`BattleUiStyle.HudEntryHeight`)。再往下一格是 1016~1096,越出 `QdaoUguiTheme.DesignHeight=1080`。旧稿说的 (68,600) 已被帮会入口占用,**不可使用**。
  - **friend 入口位置待用户/美术拍板**,可选:① 并入仙友会(Social)窗口,作为一页或一个入口(现有页签只有群组/世界/谣言,需要新增);② 缩小左列步长或入口高度,腾出一格;③ 另开入口列。定下后截图确认不和任务追踪框、既有入口重叠。
  - **快捷键 `F`(已核实可用)**:Assets 内无 `KeyCode.F` / `fKey` 占用。已占用的 HUD 键有 T/G/U/N/C/B/J/O/M/Escape;Tianyong 地图/沙盒另占 F1–F5,不和 F 冲突。代码层 F 可用,但仍需先判 `IsTyping()` / `GameplayInputGate.IsKeyboardBlocked`;是否与策划规划的按键冲突仍需确认。
- **红点:无现成系统。** 最接近的先例是 `TeamUiRoot.Changed` 把入口文字改成 `组队 · N条申请`;friend 照此做「好友 · N」(规则见 §6)。
- **列表:无虚拟列表。** 两种现成做法:
  - 定长分页重建(`TeamWindow.Render()`):先按选中按钮记下 `focusName`,再 `Clear(_members)` / `Clear(_applications)`,然后逐行重建。申请列表 `RenderApplications()` 按 `RowsPerPage=4` 分页;成员列表 `RenderMembers(capacity)` 按 `MaxMemberCards` 定长席位分页。上一页/下一页统一走 `MovePage(TeamPage page, int delta)`。(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)
  - 原生 `ScrollRect` + `RectMask2D`(`PetPanel`)。

  friend 的四个列表统一用分页重建 —— 行内「同意/拒绝」与 `TeamWindow` 完全同形,它的焦点恢复逻辑可直接照抄:见 `TeamWindow.Render()` 末尾的 `focusName` 恢复段,优先级是同名且可点的按钮 → 第一个可点的按钮 → `_refresh` / `_close`。
- **文本安全**:凡来自服务器或玩家的字符串一律 `richText = false`(`GameplayUiArt.Text` 已默认如此)。

---

## 4. 生命周期钩子 —— 三个拉取时机挂在哪

**入场事件只有 `GameClient.OnSceneEntered`**(`event Action<SceneInfoComp>`,在 `WireSceneNotifyHandlers` 的 `NotifyEnterScene` 处理器里触发;`RedirectFlow` 与 `ZoneTravelClient.TravelToZone` 都没有对外的完成事件)。它覆盖全部三种入场:(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)

1. **首次登录**:`EnterZone` → `ConnectAndEnter` → `EnterGameAndWaitScene`,等的就是这条推送。UI 路(`QdaoServerSelectView`)与 `DevAutoPilot` 直连路都经过这里;`QdaoServerSelectView` 的 `onSuccess` 回调只覆盖 UI 路,所以不要挂它。
2. **跨区 / 换 gate 落地**:跨区有两个入口:(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)
   - **玩家主动**:`GameClient.ZoneTravel` → `ZoneTravelClient.TravelToZone`(226 号,`Assets/Scripts/Game/WorldTravel/ZoneTravelClient.cs`;调用方是地图窗 `CityTravelUiRoot.RequestZoneTravel` 和 `DevAutoPilot -travelZone`;在途期间 `IsTravelPending == true`)。它的应答无错只代表**已受理**(源 scene 已冻结玩家并开始存盘),之后同样要等 msg 124。受理之后如果失败,只会以 msg 23 经 `GameClient.OnServerTip` 到达,玩家留在原地,不会触发这次重跑。
   - **服务端主动**:推送 `RedirectToGate`(124 号)。

   两种情况最终都由 `GameClient.RedirectFlow` 完成:探测 → 连新 gate → `ResetConnectionState` → **完整重跑** `ConnectAndEnter`。落地以 `NotifyEnterScene` 为准,所以 `OnSceneEntered` 会再触发一次;此时 `IsRedirecting` 仍为 true(`finally` 在 `ConnectAndEnter` 返回后才清)。

   有两个回调 friend 都不要挂,只认 `OnSceneEntered`:
   - TravelToZone 的 `onAccepted`:不代表抵达(msg 124 抢在应答之前到时也会触发它),而且只属于发起方。
   - `RedirectFlow` 的 `onSuccess`:是私有闭包,没有对外事件,且晚于 `OnSceneEntered`。
3. **断线后重登**:没有自动重连,`OnDisconnected` 后 UI 回选服,玩家重新走 `EnterZone`,同 1。
- 同 zone 切场景(`CityTravelUiRoot.RequestTravel`)也会触发它。客户端现在**能**分清这次入场有没有换连接:拿 `GameClient.GateConnectionIdentity` 和上次记下的值做引用比较即可,先例是 `CityTravelUiRoot.HandleSceneEntered` 与 `GuildClient.ObserveConnection`。friend 统一「入场即拉」并去重(两条读请求,在建议限频档位之内),但**在途标志是否复位按连接标识判断**(见时序细节 2),不在入场时无条件复位。`IsTravelPending` / `IsRedirecting` 两个标志 friend 用不上。`OnServerTip` 是 msg 23 唯一的出口;friend 不要对 msg 23 / 124 调 `RegisterNotify`(覆盖语义,会把 GameClient 自己的处理器静默盖掉)。(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)

| 时机 | 钩子 | 动作 |
|---|---|---|
| 登录后 | `GameClient.OnSceneEntered` | `FriendClient.HandleSceneEntered()`:连接标识变了 → 请求序号 +1、清在途标志,`RequestPendingRequests()` + `RequestFriendList()`;标识没变 → 只补拉当前不在途的读请求(见下方第 2 条) |
| 开面板 | `FriendUiRoot.Toggle` → `FriendWindow.Show(page)`(先例:`PetPanel.Show` 调 `RequestList`) | 拉当前页对应的列表;申请页和好友页**总是**拉 `GetPendingRequests`;「黑名单」「推荐」只在切到该页时拉 |
| 跨区 / 换 gate 落地 | 同一个 `GameClient.OnSceneEntered` | 同「登录后」 |
| 同 zone 换图 | 同一个 `GameClient.OnSceneEntered` | 标识不变分支:不清在途标志,只补拉 |

**这才是"传送窗口内推送会丢"的真正兜底**(交接文档 §4.3 / 服务端已知限制):传送期间 gate 绑定在换,窗口内发出的推送会丢(最坏约 60 秒),只有主动拉取能补回来。

**三个必须知道的时序细节:**

1. 登录与跨区落地时,`OnSceneEntered` 触发那一刻 **`InGame` 仍为 false**,要等 `EnterGameAndWaitScene` 协程下一次恢复才置位:
   - 推送若由 `AppBootstrap.Update` 里的 `GameClient.Tick()` 派发,协程在同一帧稍后恢复;
   - 若由协程自己循环里的 `Tick()` 派发,则到下一帧。

   同 zone 换图(`CityTravelUiRoot.RequestTravel`)时 `InGame` 本来就是 true。三种情况下 `IsGateReady == true` 与 `PlayerId != 0` 都已成立(`PlayerId` 在等推送之前即由 EnterGame 回包赋值,`TokenVerified` 更早置位)。所以 friend 的就绪判据照 `PlayerFeaturesClient.PrepareRequest` 用 `_net.IsReady && _net.PlayerId != 0`,**不要判 `InGame`**。(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)
2. **重定向成功时不触发 `OnDisconnected`**。换连接要等新连接建好之后才调 `ResetConnectionState`(走的是 `notify:false`),在此之前老 gate 的 `OnDisconnected` 已经从 `HandleTransportDisconnected` 上摘掉。在途 `Call` 以 `onError("disconnected")` 收场,`PlayerId` 先被清成 0,随后恢复成同一个 id。所以 friend **不能只靠 `Disconnected` 事件复位在途标志**。

   **重定向失败则会触发一次强制的 `OnDisconnected`**(`DisconnectInternal(notify:true, forceNotification:true)`),分两类:换连接前由 `FailRedirect` 触发(目标校验失败 / 环路熔断 / 探测不通 / 新连接建不起来),换连接后由 `ConnectAndEnter` 的 onError 触发。两类的 UI 都回选服,friend 按第 3 条断线处理即可,不需要额外分支。

   **复位的判据是「连接对象换了」,不是「入场了」**。同 zone 换图(`CityTravelUiRoot.RequestTravel` → `GameClient.EnterScene`)同样触发 `OnSceneEntered`,这时连接没换,在途 `Call` 仍然有效。如果此时清 Loading / Busy,同一连接上就会再发同一 message_id 的请求,破坏 §2.3 依赖的单飞(gRPC 回包 id==0 按 message_id FIFO 配对)。

   做法照新基线 `GuildClient` / `JubaozhaiClient`:
   - `FriendClient` 构造函数加第二个参数 `Func<object> connectionIdentity`(缺省 `() => _net`,生产注入 `() => game.GateConnectionIdentity`),并记住已观察的连接标识。
   - `HandleSceneEntered()` 先用 `ReferenceEquals` 比较标识:
     - **变了**(首次登录 / 跨区 / 换 gate / 静默重登):记下新标识,请求序号 +1,清 Loading / Busy 和 §5 的 `RequiresReconnect`,再发两条读请求。
     - **没变**(同 zone 换图):不动序号和在途标志,只对当前没有在途的那一类读请求补拉,绝不在同一连接上重发同一 message_id。
   - `Disconnected` 处理器照 `GuildClient.HandleDisconnected`,也把已观察标识刷新成当前值。
   - 回调侧照 `GuildClient` 同时校验请求序号和连接标识,过期回包一律丢弃。

   (2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)
3. 此刻 friend 服务对该会话是否已可达(gate 会话是否已绑定玩家并能注入会话 metadata)**未核实** —— 服务端 robot `friend-smoke` 是在 EnterGame 之后才调 friend 的,需要联机确认。若确有时间窗口,备选方案是改在 `InGame` 由 false 变 true 的边沿拉取(在 `FriendUiRoot.Update` 里检测,照 `GameplayUiRoot`)。

断线清理分两层,**数据归 Client,视图归 UI 根**:(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)
- `FriendClient` 订阅 `_net.Disconnected` 清快照(epoch +1,照 `PlayerFeaturesClient.ClearSnapshots`)。但这不够:重定向(`RedirectFlow`)和 `EnterZone` 重进都走 `ResetConnectionState`,即 `notify:false`,**不会**触发 `OnDisconnected`,却会把 `PlayerId` 清成 0。所以 Client 另外记一个 `_lastPlayerId`:读到 `_net.PlayerId == 0` 时**跳过**(那是传送或重连途中);读到一个**不同的非 0 值**时才算换了角色,这时清快照、N 和提示位,并把 epoch +1。
- UI 根另有 `_playerId` 变化检测(先例:`GameplayUiRoot.Update` / `TeamUiRoot.Update` 的 `ResetSession`)。⚠ 这两个先例按 `InGame && IsGateReady ? PlayerId : 0` 取 id,跨区传送途中 `InGame` / `IsGateReady` / `PlayerId` 会短暂变成 false / 0(`GameClient.IsRedirecting` 的注释),UI 根会把一次传送看成 id→0→同一个 id,连续执行两次 `ResetSession`。
  - `FriendUiRoot` 的 `ResetSession` 只清窗口的视图状态(页码、焦点、输入框、面板开关),**不得调用 `FriendClient` 的任何清空方法**。`GuildUiRoot.Update`(第 92 行)和 `JubaozhaiUiRoot.Update`(第 90 行)在 id 变化时会 `_client?.Reset()`,这是**反例,不要照抄**。
  - 建议 UI 根也忽略 0:只在 id 变成另一个非 0 值时 `ResetSession`。
  - 传送途中(`IsRedirecting || IsTravelPending`,或 `!InGame`)入口可以隐藏、面板可以收起,但不要据此清数据。

**〔2026-09-20 晚,跨 zone 传送会话的客户端改动之后〕** 该会话改了 `GameClient.cs` / `ZoneTravelClient.cs` 等,并回告:本节依赖的三条语义**都没变**(`OnSceneEntered` 的触发点与"触发时 `InGame` 仍为 false"照旧;`OnDisconnected` 仍在 `DisconnectInternal` 末尾、`shouldNotify` 时触发,重定向换连接期间仍不触发;`WireSceneNotifyHandlers` / `OnNotify` 覆盖语义 / `_net.RegisterNotify` 未改)。新增两个只读属性,friend 可以直接用、不必自己推导:
- `GameClient.CurrentZoneId`:当前连着的 gate 所属区(普通进入取所选区;重定向换连接成功后从票据解析 `target_zone_id`;断线清零)。上面"跨区落地"那一行若要判断"落地后是不是换了区",读它即可 —— 但**连接标识变了就复位**这条判据不变,区号只作辅助(同区换 gate 也要复位)。
- `GameClient.DisconnectReason`:在 `OnDisconnected` 触发**之前**赋值,失败流程主动断线时带人话原因,普通断线为 `null`。friend 的断线提示若要区分"传送失败被断"与"普通掉线",读它,不要自己拼原因。
另:服务端契约新增"EnterGame 应答无错 = 已受理;之后进场没成,login 经 gate 推 3023(`kEnterSceneFailed`)"(`docs/design/cross-zone-scene-travel.md` §12.5.5)。friend 的首拉挂在 `OnSceneEntered` 上,那时入场已经成功,不受这条影响。
(已对客户端 `6876247`(`120e2d8` 的直接后继,跨 zone 那批已推远端)核对:`GameClient.cs` 第 115 行 `public uint CurrentZoneId { get; private set; }`、第 122 行 `public string DisconnectReason { get; private set; }`;`DisconnectReason = reason;` 紧挨在 `OnDisconnected?.Invoke();` 之前(第 1756–1757 行)。行号会漂,按符号名定位。**本规格的客户端基线自此以 `6876247` 为准。**)

---

## 5. 建议新增 / 修改的文件(相对客户端仓根)

挂法取舍(对应 §0 第 4 条):friend 选第一类挂法,即挂进 `GameClient` 构造函数。原因是首拉属于数据正确性,不应依赖 UI 根存活。同时借用第二类挂法的连接标识注入,构造时传 `() => GateConnectionIdentity`。(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)

**新增(手写):**

| 路径 | 职责 |
|---|---|
| `Assets/Scripts/Game/Friend/FriendClient.cs` | 网络层 + 数据模型 + 红点真相,**不引用 UnityEngine**。`FriendClient.Attach(IBattleTransport, Func<object> connectionIdentity)`、`static Instance`。属性:`Friends` / `PendingRequests` / `Blocks` / `Candidates`,各自的 Loading / Error,`Busy`(写单飞),`RequiresReconnect`(超时隔离),`PendingIncomingCount`,`HasUnseenEvent`。事件:`OnChanged` / `OnError` / `OnFriendEvent(reason, byPlayerId)`。方法:`RequestFriendList` / `RequestPendingRequests` / `RequestBlocks` / `RequestRecommend(exclude)` / `AddFriend` / `Accept` / `Reject` / `Remove` / `Block` / `Unblock` / `HandleSceneEntered()` / `ObserveConnection()` / `Dispose()` |
| `Assets/Scripts/UI/Ugui/Friend/FriendUiRoot.cs` | 照 `TeamUiRoot` + `GameplayUiRoot`。自举单例,自带 Canvas(191),入口「好友 [F]」,入口位置待用户拍板(见 §3 HUD 入口,(68,600) 已被帮会占用)。`Update` 里绑定 `AppBootstrap.Instance?.GameClient?.Friends`,订阅事件。检测 `_playerId` 变化(忽略 0),变化时执行只清视图的 `ResetSession`,不清 `FriendClient` 数据(见 §4 末段)。负责互斥 `HidePanel`、Toast、入口文字「好友 · N」 |
| `Assets/Scripts/UI/Ugui/Friend/FriendWindow.cs` | 照 `TeamWindow`(切图映射见 §3)。纯 C# 类,四个页签(好友 / 申请 / 黑名单 / 推荐),分页重建。**只抛意图事件**(`AcceptRequested(id)` 等),`SetState(...)` 渲染,`ResetSession()`。根节点挂 `GameplayInputBlocker`。加好友的 id 输入框照 `BattleQueuePanel._targetInput`(`CreateInputField` + `ContentType.IntegerNumber` + `ulong.TryParse`)。控件 name 要稳定(`FriendAccept_<id>` 等),测试按 name 找按钮 |
| `Assets/Tests/EditMode/<Friend 或 Battle>/FriendClientTests.cs` | 放在哪里二选一,并在交付说明里写清选了哪种。**(A,新基线惯例)** 新建 `Assets/Tests/EditMode/Friend/` 和 `MmorpgClient.Tests.EditMode.Friend.asmdef`。asmdef 字段照抄 `Social/MmorpgClient.Tests.EditMode.Social.asmdef`(或 Battle 的,两者只差 name / rootNamespace):references MmorpgClient / UnityEngine.UI / Unity.TextMeshPro,includePlatforms Editor,overrideReferences true,precompiledReferences Google.Protobuf.dll,autoReferenced false,optionalUnityReferences TestAssemblies。`FakeBattleTransport` 是 internal,跨程序集看不见,所以要在该目录下放一份 `internal sealed class FriendFakeTransport : IBattleTransport`。**只能从 `Battle/FakeBattleTransport.cs` 复制**,保留 `Calls` / `RecordedCall.{MessageId,Request,Respond,FailWith}` / `OneWays` / `PushNotify` / `RaiseDisconnected` / `CallsOf`,这样 §8 的用例可以原样成立。不要照 `SocialFakeTransport` 抄,它的 API 是 `Id` / `Success` / `Error`,对不上;`Assets/Tests/EditMode/Mail/` 没有假传输,也不能当复制模板。Guild / Jubaozhai / Social 都是这种"各功能一个程序集、各自复制假传输"的做法。**(B,零复制)** 仍放 `Assets/Tests/EditMode/Battle/`,直接复用 `FakeBattleTransport`,先例是 `PetClientTests` / `PlayerFeaturesClientTests`。Battle 的 asmdef 已经引用 UnityEngine.UI / TMP,窗口测试也能放在这里。用例见 §8。(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正) |
| `Assets/Tests/EditMode/<同上目录>/FriendWindowTests.cs` | 和 FriendClientTests 放在同一目录、同一程序集。写法照 `Assets/Tests/EditMode/Tianyong/TeamWindowTests.cs`:点击只抛意图事件、不改列表;空态 / 加载 / 错误态;分页 |
| `Assets/Tests/PlayMode/FriendWindowPlayModeTests.cs` | 照 `TeamWindowPlayModeTests`:打开、关闭、销毁时输入阻挡器正确释放 |
| `Docs/FriendUI.md`(沿用规格原名 `Docs/friend-ui.md` 也可以) | 照 `Docs/SocialUI.md` 的体例(入口与服务边界、真实 RPC 接线、旧回包过滤、显式离线预览分节写);`Docs/team-ui.md` 只借鉴 UI 布局部分。(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正) |
| `Assets/Editor/Friend/FriendUiVerification.cs`(可选) | 照 `Assets/Editor/Social/SocialUiVerification.cs` 做离线截图(新基线惯例是按功能建子目录)。只有 Friend 运行时代码自带 asmdef 时,才照 `Assets/Editor/Guild/MmorpgClient.Guild.Editor.asmdef` 补一个 `MmorpgClient.Friend.Editor.asmdef`;否则像 Mail / Social / Jubaozhai 一样不加 asmdef。平铺的旧写法 `TeamUiVerification.cs` 仍可参考。(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正) |

**修改(手写):**

- `Assets/Scripts/Game/GameClient.cs`:
  - 构造函数里(`Features = …` 那行之后)加 `Friends = FriendClient.Attach(new GameClientBattleTransport(this), () => GateConnectionIdentity);`。
  - 加属性 `public FriendClient Friends { get; }`。
  - 登记入场首拉 `OnSceneEntered += _ => Friends.HandleSceneEntered();`。这一行**无先例**,是本规格的建议:拉取属于数据正确性,不应依赖 UI 根存活,而且可以用假传输直接测。备选:照 `CityTravelUiRoot.Bind` 在 `FriendUiRoot` 里订阅。
- `Assets/Scripts/UI/AppBootstrap.cs`:`OnDestroy` 里照 `GameClient.Features?.Dispose()` 加 `GameClient.Friends?.Dispose()`。
- 互斥补行:以下 9 处打开入口各加一行 `Friend.FriendUiRoot.Instance?.HidePanel();`,照已有的 `Guild.GuildUiRoot.Instance?.HidePanel();` 那批补行的先例,紧挨着它加:`GameplayUiRoot.Open`、`TeamUiRoot.Toggle`、`CityTravelUiRoot.Toggle`、`GuildUiRoot.Toggle`、`JubaozhaiUiRoot.Toggle`、`MailUiRoot.Toggle`、`SocialUiRoot.Toggle`、`PetPanel.Show`、`AttributePanel.Show`。反过来,`FriendUiRoot.Toggle` 打开前要隐藏以下 9 个根:Team / Guild / Jubaozhai / Mail / Social / Gameplay / CityTravel / Attribute / Pet(即 `MailUiRoot.Toggle` 隐藏的 8 个根再加上 Mail 本身,以 `MailUiRoot.cs:78-79` 和 `SocialUiRoot.cs:64-71` 为准)。(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)
- `tools/gen_proto.ps1`:(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)
  - **已有、不用补**:新基线 `120e2d8` 的 `$files` 已经包含 `proto/trade/jubaozhai.proto`、`proto/team/team.proto`,以及 guild/scene/trade/common/team 五个 `generated/code/proto/tip/*_error_tip.proto`。
  - **核对后保留**:`"proto/friend/friend.proto"` 已作为工作区未提交改动追加在末项(team_error_tip 那行已补逗号)。
  - ~~**要补**~~ **已补(2026-09-20 晚,工作区未提交)**:按清单现有惯例,在 friend 行后面补了 `"generated/code/proto/tip/friend_error_tip.proto"`(服务端仓已有该文件,时序见 §1.2)。
  - ⚠ 客户端工作区里 11 个 `ClientPlayerFriend*Handler.cs` 已经生成(未跟踪),但 `Assets/Scripts/Proto/Generated/Friend.cs` / `FriendErrorTip.cs` 还没生成。所以必须带 `-ProtoRoot <服务端仓根>` 重跑 `gen_proto.ps1`,否则会报 CS0246。
  - 清单改动、proto 生成物、handler/registry/messageid 生成物要同批提交。

**生成(不手改):** `Assets/Scripts/Proto/Generated/Friend.cs`、`FriendErrorTip.cs`、11 个 `ClientPlayerFriend*Handler.cs`、`HandlerRegistry.cs`、`MessageIds.cs`。**不要写 `.user.cs` partial**(仓里 `*.user.cs` 为零,因为 `Dispatch` 从未被调用)。

**接线要点:**

样板引用:
- `Assets/Scripts/Game/Jubaozhai/JubaozhaiClient.cs`:超时判据。
- `Assets/Scripts/Game/Guild/GuildClient.cs`:推送 + 申请列表 + 审批形态,以及三重校验。
- `Assets/Scripts/UI/Ugui/Guild/GuildUiRoot.cs:84`:传入连接身份。
- 测试:`Assets/Tests/EditMode/Guild/GuildUiTests.cs`、`Assets/Tests/EditMode/Jubaozhai/JubaozhaiClientTests.cs`。客户端没有 `GuildClientTests`,不要引用这个名字。

(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)

- **S2C**:`FriendClient` 构造函数里 `_net.RegisterNotify(ClientPlayerFriendNotifyFriendEventHandler.MessageId, HandleFriendEvent)`。按 `reason` 分路:`REQUEST_RECEIVED` → 置 `HasUnseenEvent = true`、立即触发 `OnChanged`(红点先亮)、再 `RequestPendingRequests()`;`REQUEST_ACCEPTED` → `RequestFriendList()` + 触发 `OnFriendEvent` 供 Toast;未知 reason(含 0)只记日志、不报错(前向兼容)。**这个消息号全仓只许注册这一处**(`OnNotify` 是覆盖语义)。
- **C2S**:写操作的响应只带 `error_message`,**不回全量列表**(与 Pet 不同)—— 成功后必须**自己再拉**一次(`Accept` 成功 → `RequestPendingRequests()` + `RequestFriendList()`),不允许本地增删行去猜结果。回调先用 `IsCurrent(epoch, playerId, connection)` 过滤旧回包,即代次、玩家、连接对象三重校验,照 `JubaozhaiClient` / `GuildClient`。`FriendClient` 的 `connectionIdentity` 构造参数默认 `() => net`,生产由 `GameClient` 构造函数传入 `() => GateConnectionIdentity`(见上);发请求时记下当时的连接对象。每次发请求前先调 `ObserveConnection()`:连接对象变了就清 Loading / Busy,解除隔离,代次 +1。`Disconnected` 事件做同样处理。(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)
- 传输错误映射(`DescribeTransportError`):按 `"server tip="` 前缀 `uint.TryParse` 出码,再与枚举比较(照 `SocialClient.IsDefiniteRejection`)。比较串如需拼接,也用 `$"server tip={(uint)common_error.KRateLimitExceeded}"`,不写死数字。映射如下:
  - `(uint)common_error.KRateLimitExceeded` →「操作太频繁,请稍后再试」。
  - `(uint)common_error.KServiceUnavailable` →「好友服务暂不可用,请稍后重试」。它可能是上游超时,不代表没写入,不得当作确定失败而自动重发。
  - `"rpc timeout"` / `"disconnected"` / `"not connected"` →「网络不稳定,请重试」。
  - `"协程宿主未就绪(CoroutineRunner 未接线)"` →「客户端未就绪,请稍后重试」,并 `Debug.LogError` 记下原串。这是接线缺陷,不归入网络不稳定,也不把内部术语原样展示给玩家。
  - 其余原样。

  响应体的限频 / 参数错 / 服务不可用也要收进 `DescribeTip`,写成 `(uint)common_error.KRateLimitExceeded` / `(uint)common_error.KInvalidParameter` / `(uint)common_error.KServiceUnavailable`(`CommonErrorTip.cs` 已在清单内生成)。friend 自己的码写 `(uint)friend_error.KFriendCannotAddSelf` 等。一律不写数字字面量。15007–15009 在枚举生成前不写分支(走 `"tip=N"` 兜底),不许先填数字占位。(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)
- **超时隔离**:(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)
  - **问题**:GameClient 对零编号 gRPC 回包按 message_id 先进先出匹配(GameClient.cs:1264-1275),而超时分支已经删掉了在途记录(:1207)。迟到的旧回包会被同一连接上下一次同号请求误领,单飞和 epoch 都挡不住。
  - **上锁**:只要 onError 等于 `"rpc timeout"`(与 GameClient 逐字一致,定义为常量),就置 `RequiresReconnect = true`。同一连接上所有 friend 读写都在本地拒绝,不发 Call,提示「好友请求状态尚未确认,请重新登录角色后再试」。
  - **解锁**:`ObserveConnection` 发现连接对象变化,或收到 `Disconnected`。
  - **不上锁的错误**:`"server tip=…"`(包括 1003)、`"parse response: …"`、`"disconnected"`、`"not connected"`、`"send failed"`。前两类的回包已被当前请求消费,后三类的旧连接已经作废,不会串包。
  - **判据照 `JubaozhaiClient`**(文件头注释第 2 条、Request 的 error 分支)。不要照 GuildClient(任何错误都上锁,过于保守),也不要照 SocialClient(它对 1003 上锁是出于写幂等,与 FIFO 无关)。
- **请求体不带自己的 `player_id`**:proto 里已 `reserved 1`,生成类里不会有这个字段(D-9:身份只从会话取)。
- 服务端推荐参数(核自 `go/friend/internal/config/config.go`):`RecommendDefaultLimit` 10、`RecommendMaxLimit` 20、**`RecommendMaxExclude` 64**(超出回 1005)—— 客户端「换一批」回传的 `exclude_player_ids` 只保留最近若干批,别无限累加。

---

## 6. 红点规则

- **真相**:`PendingIncomingCount` = 最近一次**成功**的 `GetPendingRequestsResponse.requests.Count`。服务端只返回 `to_player_id = 我` 且 PENDING 的行 —— **只有收到的申请,没有「我发出的」列表**,UI 不要做「已发送」页。
- **提示位**:`HasUnseenEvent` 由 `REQUEST_RECEIVED` 推送置位,让红点在拉取返回之前先亮。
- **显示条件**:`PendingIncomingCount > 0 || HasUnseenEvent`。入口文字「好友 · N」;提示位为真但 N 还是 0 时显示「好友 · 新」。
- **熄灭**:一次成功的 `GetPendingRequests` 返回后清 `HasUnseenEvent`,N 以真相为准;同意 / 拒绝成功后重拉,N 自然下降。**打开面板不直接清 N** —— 申请还在,红点就该在。
- **拉取失败**:保留上一次的 N 与提示位,不清零也不自增。照 `PlayerFeaturesClient` 的 `FailMissions` / `FailActivities`:失败只写 `*Error` 字段并触发 `OnChanged` / `OnError`,不动已有快照。口径出自客户端 `Docs/GameplayUI-20260910.md`「保留失败前的有效快照并提示重试」。
- **`REQUEST_ACCEPTED`**:只拉好友列表并弹 Toast。是否给「新好友」亮红点**产品口径未定**。
- 推送是 at-most-once。**红点的正确性只建立在 §4 的三次拉取上,推送只是加速手段。**
- 以下三种情况清零:真断线(`Disconnected`);`_net.PlayerId` 换成**另一个非 0 值**(含不经 `Disconnected` 的 `EnterZone` 换角色);`Dispose`。**跨区传送不算**:传送途中 `PlayerId` 短暂为 0,此时保留旧的 N 与提示位,落地后 `OnSceneEntered` 的首拉会校正。(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)

---

## 7. 不要做的事

1. **不要调用 `NotifyFriendEvent`。** 它的消息号只许出现在 `RegisterNotify` 的第一个参数里,不许传给 `Call` / `SendOneWay`。它和 C2S 同 service,服务端在会话拦截器的方法白名单里刻意排除了它(否则客户端可以伪造好友事件),调了只会拿到信封码 1003。
2. **不要调用 `HandlerRegistry.Register`,也不要写 `*.user.cs` partial** —— 会静默盖掉 `GameClient` 手写的 `RedirectToGate` / `NotifyEnterScene` 处理器。
3. **不要在请求里带自己的 player_id**,也不要用客户端推断"我是谁"。
4. **不要本地推算列表**(点了同意就把行挪到好友页、点了删除就删行)。只认拉回来的快照。
5. **不要在被限频或失败后自动重试、轮询或定时刷新** —— gate 会把每次被拒计入非法包,累计到阈值就断开连接。「换一批」推荐按钮要有单飞和冷却(建议档位 1 次/秒)。
6. **不要手改生成物**(`MessageIds.cs`、`Net/Generated/*`、`Proto/Generated/*`),不要手填消息号数字,**也不要手填 tip 码数字(一律用 `common_error` / `friend_error` 枚举)**,不要混用两次 proto-gen 的产物。(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)
7. **不要用 FairyGUI,不要新建预制体式面板,不要新造 UIManager 或红点框架**(只有一个功能要用,YAGNI),不要新增美术资源。
8. **不要伪造昵称、等级或头像。** 协议里只有 id、在线状态、时间戳、共同好友数(`display_name` 留给玩家档案二期)。用中性占位如「道友 · {id}」,照 `GuildWindow` 成员行 / 申请人行在名字为空时的写法 `"道友 · " + id`(见 GuildWindow.cs:295、:350)。`TeamWindow.DisplayName` 的「无名道友」同样不伪造昵称,但不带 id;好友列表需要用 id 区分多人,所以不照它。(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)
9. **不要用 `InGame` 作为首拉的放行条件;不要只靠 `Disconnected` 复位在途标志,也不要在入场时无条件复位 —— 以连接标识变化为准**(§4)。
10. **不要把 Team 的 UI 状态或战斗匹配队伍与 friend 混用**,不要动 `TeamUiRoot.State` 的接线(那是留给组队服务的)。
11. **文案不要泄露拉黑方向**(§2.5 的 15007)。

---

## 8. 验收判据

**静态(可 grep):**

- `NotifyFriendEventHandler.MessageId` 在 `Assets/Scripts`(排除 `Net/Generated`)里只出现 1 次,且位于 `RegisterNotify(` 调用内。
- `HandlerRegistry.Register` 仍为零调用;`*.user.cs` 文件数仍为 0。
- `FriendClient.cs` 不含 `using UnityEngine`,不含数字形式的消息号字面量。
- `FriendClient.cs` 里没有 1003 / 1005 / 1008 / 150xx 这类 tip 数字字面量;tip 比较与文案 switch 全部经 `(uint)common_error.*` / `(uint)friend_error.*`。(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)
- `Friendpb.*Request` 的构造处没有 `PlayerId =`(只有 `TargetPlayerId` / `FromPlayerId`)。
- 重新生成后,客户端全部桩的 `MessageId` 以及 `MessageIds.cs` 与服务端 `proto/message_id.txt` 逐项一致。(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)
  - 基线 `120e2d8` 原有 92 个桩的 `MessageId` 和 `MessageIds.cs` 的 102 个常量,一个值都没变。
  - friend 只新增 11 个 `ClientPlayerFriend*Handler` 桩:`Handlers/` 从 92 变为 103,`HandlerRegistry.cs` 注册数也从 92 变为 103。
  - friend 用 (b) 写法、不改 `gen_messageids.ps1` 白名单,所以 `MessageIds.cs` 仍是 102 个常量、内容不变。如有任何变动,就是混入了别的改动,停下来查。

**EditMode(`FriendClientTests`,用方案 A 的 `FriendFakeTransport` 或方案 B 的 `FakeBattleTransport`,两者 API 相同;被测 Client 直接 `new FriendClient(_net, () => _conn)`,`_conn` 是测试持有的一个 object,不经 `Attach`,避免污染单例):**

1. `HandleSceneEntered` 发出且只发出 `GetPendingRequests` 和 `GetFriendList` 各 1 条;已有在途请求时再次调用不重复发。
2. `PushNotify(REQUEST_RECEIVED)` 后 `HasUnseenEvent == true`,并发出 1 条 `GetPendingRequests`;回包带 2 行时 `PendingIncomingCount == 2`,提示位被清。
3. `PushNotify(REQUEST_ACCEPTED)` 后发出 1 条 `GetFriendList`;未知 reason 不抛异常、不发请求。
4. 写操作单飞:第二个写请求被本地拒绝,不发网络请求。
5. 写成功后自动重拉;写回包带 tip(`friend_error.KFriendAlreadyFriends`)时 `OnError` 含对应文案,列表不变,`Busy` 复位。
6. 失败不重试,超时要上锁:(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)
   - `FailWith($"server tip={(uint)common_error.KRateLimitExceeded}")` 给出「操作太频繁」的文案,且**不产生任何新的 Call**(证明无自动重试);`FailWith("rpc timeout")` 同样不重试。
   - `FailWith("rpc timeout")` 之后 `RequiresReconnect == true`,再调任何读或写,`Calls.Count` 都不增加。把 `_conn` 换成 `new object()` 并调 `ObserveConnection()`(或触发 `RaiseDisconnected`)后解除隔离,读请求恢复发出。
   - `FailWith("server tip=1003")` 或 `FailWith("disconnected")` 之后不上锁,下一次请求照常发出。
7. 拉取失败时保留上一份列表和 N。
8. 断线后全部清空,迟到的回包不回填;`PlayerId` 变更后旧角色的回包被丢弃。
9. **重定向形态**:(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)
   - 基本形:不触发 `RaiseDisconnected`,让在途请求 `FailWith("disconnected")`,把 `_conn` 换成新对象后再调 `HandleSceneEntered`。断言 Loading / Busy 已复位、未进入 `RequiresReconnect`,并重新发出两条读请求;旧请求的迟到回包被丢弃。
   - 变体:先 `FailWith("rpc timeout")` 上锁,再换 `_conn`、调 `HandleSceneEntered`,断言解锁并发出两条读请求。

   9b. **同 zone 换图**:`_conn` 不变,一条读请求和一条写请求都在途时调 `HandleSceneEntered`。断言 Loading / Busy 不变,不重复发在途那一类请求,只补拉不在途的读请求;再调一次写操作仍被单飞拒绝。
10. `IsReady=false` 或 `PlayerId=0` 时不发任何请求。
11. `Calls` 里**永远没有** `NotifyFriendEvent` 的消息号。
12. `Dispose` 之后事件退订,回调失效。

可抄的样板用例名:`PetClientTests` 的 `RequestListAppliesServerList` / `ServerPushReplacesList` / `DisconnectDropsListAndInFlightWrite` / `TipInResponseBodyIsRejectionKeepsListAndClearsBusy` / `SecondWriteIsRejectedWhileFirstIsInFlight` / `NotReadyTransportRejectsBeforeSending`;`PlayerFeaturesClientTests` 的 `OlderListResponsesAndErrorsCannotOverrideNewerRequests` / `DisconnectClearsAllSnapshotsAndLateCallbacksCannotRepopulate` / `NewCharacterDropsPreviousDataAndIgnoresPreviousCharacterResponse` / `DisposeUnsubscribesAndInvalidatesCallbacks`;超时隔离与连接标识参照 `Assets/Tests/EditMode/Jubaozhai/JubaozhaiClientTests.cs` 与 `Assets/Tests/EditMode/Guild/GuildUiTests.cs`。

**联机(需服务端 friend + client_rpc_router 在线,且 gate 以路由服模式启动;全部未执行):**

- 两个账号互加,B 在线时红点在约 1 秒内亮起。
- B 离线期间被申请,B 登录后**不依赖推送**红点自亮。
- B 在重定向窗口内被申请,落地后红点自亮。
- 同意后双方好友页互见;拉黑后对方再次申请被拒,客户端展示中性文案。
- 连点「换一批」不掉线,gate 日志里的非法包计数不增长。

**给 Codex 的命令**(两步都过才算过,不能拿第 1 步的绿灯代替第 2 步):(2026-09-20 客户端基线 d2b165a → 120e2d8 后更正)
1. `pwsh -File <客户端仓根>\tools\client_compile_check.ps1`,期望退出码 0。**注意:它只用离线 Roslyn 编 `Assets/Scripts/**`(MmorpgClient 运行时程序集)**,不编 `Assets/Tests/**`(EditMode 程序集里的 `FriendClientTests` / `FriendWindowTests`、PlayMode 程序集里的 `FriendWindowPlayModeTests`),也不编 `Assets/Editor/**`(可选的 `FriendUiVerification.cs`)。退出码 0 只说明运行时代码能编译。
2. Unity Test Runner:EditMode 按名字过滤,方案 A 用 `MmorpgClient.Tests.EditMode.Friend`,方案 B 用 `MmorpgClient.Tests.EditMode.Battle.Friend`(两者都同时覆盖 FriendClientTests 与 FriendWindowTests);PlayMode 过滤 `FriendWindowPlayModeTests`。判据:测试程序集和 Editor 程序集编译成功(Console 没有编译错误),**过滤命中的用例数大于 0**,并且全部通过。测试或 Editor 脚本的编译错误只会在这一步出现,这时 Runner 一个用例都不跑,不能把"0 失败"当成通过。

---

## 9. 未核实清单

- `OnSceneEntered` 触发那一刻 friend 服务对该会话是否已可达(首拉会不会拿到 1003 / 1005)。
- 15007–15009 三个 tip 码的实际数值(导表器未跑)。〔2026-09-20 复核:仍未核实。服务端 `friend_error_tip.proto` 目前只到 15006。客户端改用 `friend_error` 枚举名后,这一条只影响 gen_proto 的时序(§1.2),不影响客户端代码。〕
- protoc 35.1 生成的 C# 与客户端 Google.Protobuf 3.28.3 运行时的兼容性;现有 `Proto/Generated` 当初是哪个 protoc 版本出的。
- ~~`Trade` / `Teampb` / `Friendpb` 三个 C# namespace 与现有手写代码是否撞名;`team.proto` / `jubaozhai.proto` 加进清单后是否还有缺失的 import。~~ **已关闭(客户端 `120e2d8` + 2026-09-20 工作区)**:`Trade`(`Proto/Generated/Jubaozhai.cs`)和 `Teampb`(`Proto/Generated/Team.cs`)已随新基线落地,手写层已经在用,例如 `JubaozhaiClient.cs` 里的 `using TradePb = Trade;`。team / jubaozhai 及各自的 tip proto 都已在 `tools/gen_proto.ps1` 的 `$files` 里,import 问题关闭。在 Assets 下搜索,手写代码里没有名为 `Friendpb`、`Friend` 的命名空间或类型,撞名风险可以排除。
  - **新增待办(工作区状态,未提交)**:handler 生成器已经跑过。`Net/Generated/Handlers/` 多了 11 个未跟踪的 `ClientPlayerFriend*Handler.cs`(没有 .meta),`HandlerRegistry.cs` 也多了 11 行注册,它们都引用 `Friendpb.*`。但 `Proto/Generated/Friend.cs` 还不存在,也就是 proto 消息类那一步(`tools/gen_proto.ps1`)还没跑或没落盘。在补齐之前,客户端工作区预计编译不过,会报 `Friendpb` 命名空间找不到。开工第一步要先把 `Friend.cs` 生成出来,再编译确认(未编译,待 Codex 验证)。
- 〔已核实〕(68,600) 已被帮会入口占用,左右两列都已排满,入口位置待用户/美术拍板(见 §3 HUD 入口),落位后截图确认。快捷键 `F` 在代码层无冲突,是否与策划规划的按键冲突仍需确认。
- 〔已核实(客户端 120e2d8 工作区)〕客户端运行时**没有加载任何配置表**。`MessageLimiterTableManager.Instance.Load*` 只在生成文件 `Assets/Scripts/Table/Generated/AllTable.cs` 的两个 `LoadTables` 重载里被调用(:98 / :172)。`AllTable.LoadTables` / `ReloadTables` / `OnTablesLoadSuccess` 在 `Assets/Scripts` 和 `Assets/Editor` 的手写代码里都没有调用方,客户端仓里也没有 MessageLimiter 表的 json/bin 数据文件。因此 `MessagelimiterTable` 不能直接用于本地节流;要用它,得先补上表加载入口(启动时调用 `AllTable.LoadTables`)并随包带上表数据,这不在本规格范围内。本规格不依赖它,客户端侧的节流以服务端返回的限流 tip 为准。
- 「新好友」是否需要红点;`REQUEST_ACCEPTED` 的 Toast 文案(没有昵称,只能用 id 或泛称)。
- 〔已核实,未重新 fetch〕客户端基线:`main 120e2d8 [origin/main]`,与上次 fetch 到的远端持平,以此为基线。Team(15 个 handler)、聚宝斋(4 个)和 TravelToZone 桩已随基线入库,git 跟踪的 handler 共 92 个。开工前再 `git fetch` 一次,确认远端没有更新的提交。
- 同批生成冲突:**尚未解除,且已有人动手**。2026-09-20 复核时,客户端工作区出现了一次未提交的生成产物:
  - `tools/gen_proto.ps1` 加了 friend.proto(+5/-1);
  - `HandlerRegistry.cs` 加了 11 行 Friend 注册;
  - 新增 11 个未跟踪的 `ClientPlayerFriend*Handler.cs`(还没有 .meta);
  - 20 个 Team/Jubaozhai/TravelToZone handler 被重写成 LF 行尾(内容不变)。

  但 `Assets/Scripts/Proto/Generated/Friend.cs` 还不存在,这批 handler 引用的 `Friendpb.*` 会导致 CS0246。开启 `enable_unity_client` 前,需要用户确认三件事:
  1. 这批工作区改动是谁生成的,是否归本任务,由其负责补齐 Friend.cs 或整体丢弃;
  2. 不再有别的会话同时改 `Net/Generated` / `Proto/Generated`;
  3. 那 20 个只有行尾变化的 handler 不应随 friend 一起提交。

  **〔已查清,2026-09-20 晚〕第 1 件**:这批产物来自服务端 **2026-09-20 22:10:40 的一次 proto-gen**(与服务端 `proto/message_id.txt` 同一秒写入)。那次用的是 09-09 的陈旧 `proto-gen.exe`(生成器因 proto2mysql 校验和没重编成功),在服务端吞掉了 `scene_node_service.cpp` 的 Agones 块,事故与恢复步骤见 `friend-handoff-20260920.md` §9.4 第 3 步。**对客户端的结论:留着,不用丢**。Unity handler 与 robot handler 的模板自 09-09 起没有变过,这批桩的内容与正确的生成器产出相同;Unity 桩和 `HandlerRegistry.cs` 每次运行都会整份重写,下一次正确的 `dev.bat proto` 会覆盖它们,消息号按服务端 `message_id.txt` 保留(Friend 的 11 个号已在 22:10 定下:2 / 7 / 11 / 12 / 119 与 230–238 之间的新号,以生成物为准)。`Friend.cs` 与 `FriendErrorTip.cs` 由客户端 `tools/gen_proto.ps1`(§1.4 第 6 步)生成,那一步本来就排在服务端导表与 proto-gen 之后。
  另:`generated/code/proto/tip/friend_error_tip.proto` 已照 trade / team 的形状补进客户端 `gen_proto.ps1`(在 `friend.proto` 之后,未提交),§1.2 / §5 的"要补"已完成。
