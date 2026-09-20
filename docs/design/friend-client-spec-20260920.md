# friend 客户端任务规格(Unity,2026-09-20)

> **这是什么**:`docs/design/friend-handoff-20260920.md` §4.3 的展开 —— friend 功能在 Unity 客户端(同级独立仓库 `../mmorpg-client/`)要做的事,分「生成管线」和「手写业务层」两部分。
> **依据**:2026-09-20 对客户端仓(HEAD `d2b165a`)与服务端仓(HEAD `d9e471b80`)的**只读**核对,用户已授权只读客户端仓。全程没有跑任何构建 / 生成 / 测试命令,两个仓都没有被修改。
> **还没有的授权**:**修改**客户端仓。本文落在服务端仓正是因为这个原因;开工前先向用户要客户端任务授权(`AGENTS.md §9`)。客户端仓自己的惯例是每个功能在 `Docs/` 下一篇(如 `Docs/team-ui.md`),开工后应把本文的 UI 部分沉淀成 `Docs/friend-ui.md`。
> **行号只是辅助坐标,定位一律按符号名 grep。** 标「未核实」的条目没有读到证据,接手人需自行确认。
> **纪律**:按服务端 `AGENTS.md §10.1`,Claude 不执行编译 / 生成 / 测试命令,这些交给 Codex 或人工;没有运行结果之前不得声称"编译通过""测试绿"。

---

## 0. 结论速览(先读这七条)

1. **生成的 handler 桩不是接线点。** `HandlerRegistry.Register` 在客户端**全仓零调用**(唯一出现处是它自己的定义);`GameClient.WireSceneNotifyHandlers` 里的注释明确警告:`OnNotify` 是**覆盖语义**,一旦有人把 `HandlerRegistry.Register` 接上,生成的空壳会**静默盖掉**手写的 `RedirectToGate` / `NotifyEnterScene` 处理器。桩唯一被用到的是常量 `XxxHandler.MessageId` 与 `Parser`。所以"proto-gen 生成出 `ClientPlayerFriendNotifyFriendEventHandler.cs`" ≠ "客户端接上了推送" —— 接线要手写。
2. **客户端 `tools/gen_proto.ps1` 缺的不止 friend。** `$files`(32 项)里没有 `proto/friend/friend.proto`,也没有 `proto/team/team.proto` 与 `proto/trade/jubaozhai.proto`。服务端用默认配置(`enable_unity_client: true`,全局开关)跑 proto-gen,客户端一次会新增 **31** 个桩(Friend 11 + Team 15 + Jubaozhai 4 + `SceneSceneClientPlayerTravelToZone` 1;现有 72 → 103),**三个 proto 必须同批补进清单**,否则 CS0246。
3. **两个生成脚本的默认 `-ProtoRoot` 是错的。** `gen_proto.ps1` 与 `gen_messageids.ps1` 的默认值都是 `$PSScriptRoot/../../..`(按"客户端是 `<服务端>/client/unity` 子模块"的旧布局写的;客户端 `README.md` 这一段同样过期)。两仓同级布局下它解析到两个仓共同父目录的再上一级,目录存在所以参数绑定不报错,要到 protoc 找不到 proto 才失败。**必须显式传 `-ProtoRoot <服务端仓根>`。**
4. **样板用 Pet(宝宝)+ PlayerFeatures(背包/任务/活动)+ Team(组队 UI)。** 客户端仓里 Guild、Chat、Jubaozhai/Trade 都没有手写层(Chat 只有两个空桩,Guild / Trade 连桩都没有);Team 只有纯 UI 和视图状态(`Docs/team-ui.md`:「服务端尚无接口」),没有网络层。「C2S + 1 条 S2C 推送 + 列表」形态最接近 friend 的是 `PetClient`;防旧回包的三重过滤照抄 `PlayerFeaturesClient`;同意/拒绝列表、分页、入口文字带计数照抄 `TeamWindow` / `TeamUiRoot`。
5. **客户端没有这些东西,不要去找,也不要为 friend 新造**:红点系统、全局 tip 文案表(`README.md`:「Localization: tip table loader not yet wired」)、UIManager / 面板注册表、「登录后首拉」的先例(现有功能全是开面板才拉)、自动重连(断线后回选服界面)。
6. **friend 在客户端零存量。** 没有手写代码、UI 资源、生成桩、`Friendpb` 类型、消息号常量。`git grep -i friend` 只命中无关的战斗特效素材名与 `ProjectSettings.asset`。
7. **硬前置**:服务端先跑完导表器与全量 proto-gen(交接文档 §2 第 2、3 步)。当前 `proto/message_id.txt` 里 friend 仍是旧的 8 个 `FriendService*`,11 个 `ClientPlayerFriend*` 的号还不存在。

---

## 1. 生成管线

### 1.1 客户端仓的规范

- 仓内没有 `AGENTS.md` / `CLAUDE.md`;成文规范只有 `README.md` 和 `Docs/*.md`。
- **生成物纳入版本管理**(`.gitignore` 注释原文:「Generated/*.cs IS committed … Re-run that script after .proto changes and commit the result.」)。已跟踪:`Proto/Generated` 65 个、`Net/Generated` 147 个、`Table/Generated` 133 个。**`.meta` 文件要一并提交**(先例:`1d3a0a7 补齐宝宝系统 7 个缺失的 .meta`)。
- **UI 是原生 uGUI + TextMeshPro。** FairyGUI 已在 `69e22a0` 移除;仓根的 `FairyGUI.csproj` / `FairyGUI-Editor.csproj` 是未跟踪的旧 IDE 残留,忽略。
- 客户端仓现状(2026-09-20):分支 `main`,本地与 `origin/main` 持平(未 fetch,远端是否更新未核实);工作区只有 1 条 `M ProjectSettings/ProjectSettings.asset`,**不是这批的,不要夹带**。生成目录上次改动:`Net/Generated` 与 `Proto/Generated` 是 `cfa737b`(09-12),`Table/Generated` 是 `d2b165a`(09-14)。

### 1.2 `tools/gen_proto.ps1`

- proto 源:`--proto_path=$ProtoRoot`,`$ProtoRoot` 默认值见上(错的,必须显式传)。
- protoc:`[string]$Protoc = "protoc"`,走 PATH;仓内 `tools/` 下没有 `protoc.exe`。服务端仓自带一份:`third_party/grpc/install_vs2026_dbg/bin/protoc.exe`(`libprotoc 35.1`),可用 `-Protoc <绝对路径>` 传入。⚠ **protoc 35.1 生成的 C# 与客户端内置的 Google.Protobuf 3.28.3 运行时是否兼容,未核实**(现有 `Proto/Generated` 是用哪个版本的 protoc 生成的也未核实)—— 重生成后若出现大面积无关 diff 或编译错误,先查这一条。
- 输出:`Assets/Scripts/Proto/Generated`,平铺,文件名由 proto 文件名转 PascalCase。
- `$files` 末尾的注释原文:「handler 生成器…是按服务端 proto 全量出 handler 的,这份清单漏配哪个 proto,就会多出一批引用不存在类型的 handler(CS0246)。服务端新增 proto 时,这里必须同步加,否则下次重生成必红。」
- **要补的三行**(沿用文件里"注释 + 路径"的写法;注意现在的末项 `"proto/scene/player_currency.proto"` 后面要补逗号,新末项后面不能有逗号):
  ```powershell
      # 组队(docs/design/team-system.md,service ClientPlayerTeam,C# namespace Teampb)
      "proto/team/team.proto",
      # 聚宝斋(service ClientPlayerJubaozhai,package trade → C# namespace Trade)
      "proto/trade/jubaozhai.proto",
      # 好友(docs/design/friend-handoff-20260920.md,service ClientPlayerFriend,namespace Friendpb)
      "proto/friend/friend.proto"
  ```
  三个 proto 的 import 只有 `proto_option` / `empty` / `tip`,都已在清单里。`Trade` / `Teampb` / `Friendpb` 这几个 C# namespace 与现有手写代码是否撞名,**未核实**(全 `Assets` 下 grep 这三个词零命中,大概率不撞)。
- `friend_error_tip.proto` **不用加**:清单里没有任何域的 `*_error_tip.proto`(无先例),handler 模板只引用响应类型,编译不需要它;tip proto 没有 package,生成的类型会进全局 namespace,是否撞名未核实。客户端目前也没有任何 tip 枚举(导表器的 `compile_proto_csharp` 刻意只编根目录下的表 proto,tip 不进客户端)。

### 1.3 `Assets/Scripts/Net/Generated`

- 一个 `HandlerRegistry.cs` + 平铺的 `Handlers/`。现有 72 个桩:BattleClientPlayer 12 / ClientPlayerChat 2 / ClientPlayerLogin 6 / Scene\* 52(Activity 1、Attribute 9、Bag 2、ClientPlayerCommon 3、Currency 5、Mission 3、Movement 8、Pet 9、Scene 8、Skill 4)。Team / Guild / Jubaozhai / Friend 一个都没有。
- 生成器过滤规则(服务端 `tools/proto_generator/protogen/internal/generator/unity/unity_client_handler.go`):服务带 `OptionIsClientProtocolService`,**且**服务名含 `ClientPlayer` 或 `GamePlayer`。所以 `MatchService`、`GuildService` 不出桩;`SceneRollbackClientPlayer` 的 12 个方法只标了 `OptionIsPlayerService`、没标客户端协议,也不出桩。
- S2C 方法的响应是 `Empty` 时,桩的 `Parser` 回落用请求类型 —— `NotifyFriendEvent` 对应 `Friendpb.FriendEventS2C`,正好可用。
- 生成器只创建或覆盖文件,**不会删除旧文件**;`HandlerRegistry.cs` 每次整体重写。
- **消息号一致性基线**:现有 72 个桩的 `MessageId`、以及 `Net/MessageIds.cs` 的 73 个常量,与服务端 `proto/message_id.txt` 逐一比对,**当前零漂移**。重生成后请再比一次 —— 按交接文档 §2 第 3 步的分析,已有方法的号保留不变,这 72 + 73 个值**应当一个都不变**;有任何一个变了就是出了意外,停下来查。
- 期望新增的 friend 桩(按生成器规则 `Service+Method+"Handler"` 推导,文件尚不存在):`ClientPlayerFriendGetFriendListHandler.cs` … `ClientPlayerFriendNotifyFriendEventHandler.cs`,共 11 个。

### 1.4 生成侧操作清单(交给 Codex / 人工执行)

0. **先让用户拍板**:这批要不要把 Team / 聚宝斋的桩一起带进客户端(`proto_gen.yaml` 的 team 块注释就是这道门禁;是否与别的正在改客户端协议的会话冲突也在此时确认)。不带 → 服务端用 `enable_unity_client: false` 的配置副本跑 proto-gen,本节全部推迟。
1. 环境:PATH 上要有 `protoc`(或给脚本传 `-Protoc`);导表器要 Python。缺哪个先停下来报告。
2. 客户端仓 `git status --porcelain` 存基线。
3. 服务端先跑导表器(会经 deploy 覆盖客户端 `Table/Generated`;tip 不进客户端)。跑完核对客户端 diff 应只有表相关变化(会包含新表 `RoleNameRule` —— 帮会二期 B3a 的内容,见交接文档 §2 第 2 步)。
4. 编辑客户端 `tools/gen_proto.ps1` 的 `$files`,补 1.2 的三行。
5. 服务端跑一次全量 proto-gen(默认配置)。期望客户端:`Handlers/` 新增 31 个 `.cs`、`HandlerRegistry.cs` 重写为 103 行注册、现有 72 个桩的 `MessageId` 不变。
6. 客户端:`pwsh -File tools/gen_proto.ps1 -ProtoRoot <服务端仓根>`(期望新增 `Friend.cs` / `Team.cs` / `Jubaozhai.cs`,`PlayerScene.cs` 等落后于服务端的文件一并刷新,补上 `TravelToZoneResponse`);`pwsh -File tools/gen_messageids.ps1 -ProtoRoot <服务端仓根>`。
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

数据模型没有单独的 Model 类,状态就是 Client 单例上的属性,类型直接用 proto 生成类。

### 2.2 链路 A:收到 S2C → UI 刷新

`GateTcpClient` 读线程入 inbox → `AppBootstrap.Update` → `GameClient.Tick` → `_gate.Poll()` → `GameClient.DispatchInbound`:`Id != 0` 且命中 pending 走回包;`Id == 0` 且该 `message_id` 有 FIFO pending 走回包;否则查 `_notifyHandlers[mc.MessageId]` → Client 构造时经 `_net.RegisterNotify(...)` 登记的处理器 → `Parser.ParseFrom(content.SerializedMessage)` → 更新属性、触发事件 → UiRoot 订阅者 → 面板重绘。

### 2.3 链路 B:点按钮 → C2S → 回包 / tip

面板按钮 → Client 方法 → `BeginWrite`(检查 `_net.IsReady`、本地校验、`Busy` 单飞)→ `_net.Call(messageId, req, Parser, onResp, onErr)` → `GameClient.Call` 包 `ClientRequest{Id=自增, MessageId, Body}` 发出,协程等待,**15 秒超时** → 回包分三种:

- **信封级**错误(`mc.ErrorMessage.Id != 0`)→ `onError("server tip=N")`;
- 正常 → 解析响应体 → `onResp`;
- 本地失败 → `onError`,字符串为 `"not connected"` / `"send failed: …"` / `"disconnected"` / `"rpc timeout"` / `"parse response: …"`。

`onResp` 里再看**响应体**的 `resp.ErrorMessage`(`HasTip`),有 tip 就 `Fail(DescribeTip(...))`。

经 Go 服务(gate → client_rpc_router → gRPC)的请求,先例是 `BattleClient` 调 `MatchService*`;friend 走同一条路。回包关联是两级:先按 `MessageContent.id` 精确匹配(scene 路径会回显 id),`id == 0` 时按 `message_id` FIFO 匹配(gRPC 路径)—— 所以同一个 message_id 并发多发时按先进先出配对,**friend 每类请求单飞即可规避错配**。

### 2.4 消息号怎么取

两种写法并存:(a) `MessageIds.X`(要改 `gen_messageids.ps1` 的白名单;Pet / Attribute / Battle 用);(b) 直接用桩常量 `SceneBagClientPlayerGetBagHandler.MessageId`(最近落地的 `PlayerFeaturesClient` 的用法,不用改白名单)。**friend 用 (b)**:`Block` / `Unblock` 这种短名放进 `MessageIds` 有歧义;桩常量与路由表出自同一次 proto-gen,天然满足"同批生成"。

### 2.5 tip 与限频

- **tip 转文案没有全局机制。** 信封级 tip 统一变成 `"server tip=N"`;响应体 tip 由各 Client 自己翻译 —— `PetClient.DescribeTip` 是一个手抄 `data/tip/Tip.xlsx` 的 `switch`(未收录退回 `"tip=N"`,注释写着「表改了要同步这里」)。friend 照此做镜像 `switch`。
- **限频被拒(`kRateLimitExceeded` = 1008)客户端现在没有任何专门处理**,而它有两条到达路径,friend 都要处理:
  1. **gate 限流器**:`cpp/nodes/gate/handler/rpc/client_message_processor.cpp` 的 `CheckMessageLimit` 被拒时回**信封级** 1008 → 客户端表现为 `onError("server tip=1008")`。⚠ **同一函数里每次被拒还会计入该会话的非法包计数(`IllegalPacketCounter::RegisterAndShouldKill`),到阈值 gate 直接 `forceClose`。所以被限频后不得自动重试。**
  2. **friend 服务自己的每分钟配额**(仅 `AddFriend`)回**响应体** 1008(`go/friend/internal/constants/constants.go` 的 `ErrRateLimited`)。
- 其他会出现的 common 码:1005 `kInvalidParameter`;1003 `kServiceUnavailable`(服务端存储故障,以及路由服把一切 gRPC 错误翻成的信封码 —— 客户端调 `NotifyFriendEvent` 被拒也是它)。
- friend 段的码(15007–15009 是按 `Tip.xlsx` 行序推断的,**导表器跑完后核对**):

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

- **面板不是预制体,全部用代码搭。** 仓里唯一的 UI 预制体是选服界面。功能窗口用 `QdaoUguiFactory`(`CreateRect` / `CreateCenteredRect` / `CreateStretch` / `CreateImage` / `CreateText` / `CreateInputField` / `ConfigureHudCanvas`)与 `GameplayUiArt`(`Art` / `Text` / `Button` / `Clear`,`Assets/Scripts/UI/Ugui/Gameplay/GameplayUiArt.cs`)。素材在 `Assets/Resources/UI/Ugui/GameplayV1/`(`main_frame`、`title_plate`、`content_panel`、`tab_normal`、`tab_selected`、`button_primary`、`button_secondary`、`portrait_frame`、`close` 等)。Team 直接复用这套,**friend 同样复用,不需要新美术**。
- **没有 UIManager。** 每个功能一个 `XxxUiRoot : MonoBehaviour`,`[RuntimeInitializeOnLoadMethod(AfterSceneLoad)]` 自举,`DontDestroyOnLoad`,自带 Canvas。CanvasScaler:`ScaleWithScreenSize`,参考分辨率 2560×1080,`ScreenMatchMode.Expand`。现有 `sortingOrder`:属性 160 / 宝宝 170 / 背包任务活动 180 / 地图 185 / 组队 186 / 战斗 200 → **friend 建议 187**。
- **打开用 `Toggle()`,打开前手动互斥**:调其他根的 `Instance?.HidePanel()`。新增 friend 后,其余各根的 `Open` / `Toggle` 里也要各补一行 `FriendUiRoot.Instance?.HidePanel()`。
- 可用性判定:`game.InGame && game.IsGateReady && !(BattleUiRoot.Instance?.IsBattleLayerVisible ?? false)`,不可用时自动 `HidePanel`。快捷键在 `Update` 里读,先判 `IsTyping()` 和 `GameplayInputGate.IsKeyboardBlocked`。窗口根节点挂 `GameplayInputBlocker`(`Assets/Scripts/World/Tianyong/GameplayInputGate.cs`)阻挡地图移动。
- **HUD 入口**:右侧入口列 `BattleUiStyle.HudEntryY(0..7)` 已排满。`TeamUiRoot` 改放左侧(`EntryX=68, EntryY=496`);friend 建议紧随其下约 (68, 600)。与任务追踪框不重叠是按数值推的,**视觉上未核实,需截图确认**;快捷键 `F` 是否与既有按键冲突**未核实**。
- **红点:无现成系统。** 最接近的先例是 `TeamUiRoot.Changed` 把入口文字改成 `组队 · N条申请`;friend 照此做「好友 · N」(规则见 §6)。
- **列表:无虚拟列表。** 两种现成做法:定长分页重建(`TeamWindow.RenderList`,`RowsPerPage=4`,先 `Clear(_body)` 再逐行建,配上一页/下一页)与原生 `ScrollRect` + `RectMask2D`(`PetPanel`)。friend 的四个列表统一用分页重建 —— 行内「同意/拒绝」与 `TeamWindow` 完全同形,它的焦点恢复逻辑可直接照抄。
- **文本安全**:凡来自服务器或玩家的字符串一律 `richText = false`(`GameplayUiArt.Text` 已默认如此)。

---

## 4. 生命周期钩子 —— 三个拉取时机挂在哪

**可用的钩子只有 `GameClient.OnSceneEntered`**(`event Action<SceneInfoComp>`,在 `WireSceneNotifyHandlers` 的 `NotifyEnterScene` 处理器里触发)。它覆盖全部三种入场:

1. **首次登录**:`EnterZone` → `ConnectAndEnter` → `EnterGameAndWaitScene`,等的就是这条推送。UI 路(`QdaoServerSelectView`)与 `DevAutoPilot` 直连路都经过这里;`QdaoServerSelectView` 的 `onSuccess` 回调只覆盖 UI 路,所以不要挂它。
2. **跨区 / 换 gate 落地**:客户端**没有** TravelToZone 的调用代码(226 号在客户端既无桩也无白名单)。跨区只有被动的 `RedirectToGate`(124 号),由 `GameClient.RedirectFlow` 处理:探测 → 连新 gate → `ResetConnectionState` → **完整重跑** `ConnectAndEnter`。落地同样以 `NotifyEnterScene` 为准,所以 `OnSceneEntered` 会再触发一次。`RedirectFlow` 的 `onSuccess` 是私有闭包、没有对外事件、且晚于 `OnSceneEntered`,不要挂它。
3. **断线后重登**:没有自动重连,`OnDisconnected` 后 UI 回选服,玩家重新走 `EnterZone`,同 1。
- 同 zone 切场景(`CityTravelUiRoot.RequestTravel`)也会触发它。客户端分辨不出这次入场是否跨区,所以统一「入场即拉」并去重 —— 两条读请求,在建议限频档位之内。

| 时机 | 钩子 | 动作 |
|---|---|---|
| 登录后 | `GameClient.OnSceneEntered` | `FriendClient.HandleSceneEntered()`:请求序号 +1,清在途标志,`RequestPendingRequests()` + `RequestFriendList()` |
| 开面板 | `FriendUiRoot.Toggle` → `FriendWindow.Show(page)`(先例:`PetPanel.Show` 调 `RequestList`) | 拉当前页对应的列表;申请页和好友页**总是**拉 `GetPendingRequests`;「黑名单」「推荐」只在切到该页时拉 |
| 跨区 / 换 gate 落地 | 同一个 `GameClient.OnSceneEntered` | 同「登录后」 |

**这才是"传送窗口内推送会丢"的真正兜底**(交接文档 §4.3 / 服务端已知限制):传送期间 gate 绑定在换,窗口内发出的推送会丢(最坏约 60 秒),只有主动拉取能补回来。

**三个必须知道的时序细节:**

1. `OnSceneEntered` 触发时 **`InGame` 仍为 false**(要等 `EnterGameAndWaitScene` 协程下一帧才置位),但 `IsGateReady == true` 且 `PlayerId != 0` 已成立。所以 friend 的就绪判据照 `PlayerFeaturesClient.PrepareRequest` 用 `_net.IsReady && _net.PlayerId != 0`,**不要判 `InGame`**。
2. **重定向期间不触发 `OnDisconnected`**(`ResetConnectionState` 走的是 `notify:false`):在途 `Call` 以 `onError("disconnected")` 收场,`PlayerId` 先被清成 0、随后恢复成同一个 id。所以 friend **不能只靠 `Disconnected` 事件复位在途标志**,入场钩子里必须自己把请求序号 +1 并清 Loading / Busy。
3. 此刻 friend 服务对该会话是否已可达(gate 会话是否已绑定玩家并能注入会话 metadata)**未核实** —— 服务端 robot `friend-smoke` 是在 EnterGame 之后才调 friend 的,需要联机确认。若确有时间窗口,备选方案是改在 `InGame` 由 false 变 true 的边沿拉取(在 `FriendUiRoot.Update` 里检测,照 `GameplayUiRoot`)。

断线清理:Client 订阅 `_net.Disconnected` 清快照(epoch +1,照 `PlayerFeaturesClient.ClearSnapshots`);UI 根另有 `_playerId` 变化检测(`GameplayUiRoot.Update` / `TeamUiRoot.Update` 的 `ResetSession`)。

---

## 5. 建议新增 / 修改的文件(相对客户端仓根)

**新增(手写):**

| 路径 | 职责 |
|---|---|
| `Assets/Scripts/Game/Friend/FriendClient.cs` | 网络层 + 数据模型 + 红点真相,**不引用 UnityEngine**。`FriendClient.Attach(IBattleTransport)`、`static Instance`。属性:`Friends` / `PendingRequests` / `Blocks` / `Candidates`,各自的 Loading / Error,`Busy`(写单飞),`PendingIncomingCount`,`HasUnseenEvent`。事件:`OnChanged` / `OnError` / `OnFriendEvent(reason, byPlayerId)`。方法:`RequestFriendList` / `RequestPendingRequests` / `RequestBlocks` / `RequestRecommend(exclude)` / `AddFriend` / `Accept` / `Reject` / `Remove` / `Block` / `Unblock` / `HandleSceneEntered()` / `Dispose()` |
| `Assets/Scripts/UI/Ugui/Friend/FriendUiRoot.cs` | 照 `TeamUiRoot` + `GameplayUiRoot`。自举单例,自带 Canvas(187),左侧入口「好友 [F]」。`Update` 里绑定 `AppBootstrap.Instance?.GameClient?.Friends`,订阅事件,检测 `_playerId` 变化并 `ResetSession`。负责互斥 `HidePanel`、Toast、入口文字「好友 · N」 |
| `Assets/Scripts/UI/Ugui/Friend/FriendWindow.cs` | 照 `TeamWindow`。纯 C# 类,四个页签(好友 / 申请 / 黑名单 / 推荐),分页重建。**只抛意图事件**(`AcceptRequested(id)` 等),`SetState(...)` 渲染,`ResetSession()`。根节点挂 `GameplayInputBlocker`。加好友的 id 输入框照 `BattleQueuePanel._targetInput`(`CreateInputField` + `ContentType.IntegerNumber` + `ulong.TryParse`)。控件 name 要稳定(`FriendAccept_<id>` 等),测试按 name 找按钮 |
| `Assets/Tests/EditMode/Battle/FriendClientTests.cs` | **必须放 Battle 程序集** —— 假传输 `FakeBattleTransport` 是 `internal`,只在该程序集可见(现有 `PetClientTests` / `PlayerFeaturesClientTests` 都因此放在这里,与功能名无关)。用例见 §8 |
| `Assets/Tests/EditMode/Battle/FriendWindowTests.cs` | 照 `TeamWindowTests`:点击只抛意图事件、不改列表;空态 / 加载 / 错误态;分页 |
| `Assets/Tests/PlayMode/FriendWindowPlayModeTests.cs` | 照 `TeamWindowPlayModeTests`:打开、关闭、销毁时输入阻挡器正确释放 |
| `Docs/friend-ui.md` | 照 `Docs/team-ui.md` 的体例 |
| `Assets/Editor/FriendUiVerification.cs`(可选) | 照 `TeamUiVerification.cs` 做离线截图 |

**修改(手写):**

- `Assets/Scripts/Game/GameClient.cs`:构造函数里(`Features = …` 那行之后)加 `Friends = FriendClient.Attach(new GameClientBattleTransport(this));`;加属性 `public FriendClient Friends { get; }`;登记入场首拉 `OnSceneEntered += _ => Friends.HandleSceneEntered();`。最后这一行**无先例**,是本规格的建议:拉取属于数据正确性,不应依赖 UI 根存活,而且可以用假传输直接测。备选:照 `CityTravelUiRoot.Bind` 在 `FriendUiRoot` 里订阅。
- `Assets/Scripts/UI/AppBootstrap.cs`:`OnDestroy` 里照 `GameClient.Features?.Dispose()` 加 `GameClient.Friends?.Dispose()`。
- 互斥补行:`GameplayUiRoot.Open`、`TeamUiRoot.Toggle`、`CityTravelUiRoot.Toggle`、`PetPanel.Show`、`AttributePanel` 对应的打开处,各加一行 `Friend.FriendUiRoot.Instance?.HidePanel();`。
- `tools/gen_proto.ps1`:补 `$files`(§1.2)。

**生成(不手改):** `Assets/Scripts/Proto/Generated/Friend.cs`、11 个 `ClientPlayerFriend*Handler.cs`、`HandlerRegistry.cs`、`MessageIds.cs`。**不要写 `.user.cs` partial**(仓里 `*.user.cs` 为零,因为 `Dispatch` 从未被调用)。

**接线要点:**

- **S2C**:`FriendClient` 构造函数里 `_net.RegisterNotify(ClientPlayerFriendNotifyFriendEventHandler.MessageId, HandleFriendEvent)`。按 `reason` 分路:`REQUEST_RECEIVED` → 置 `HasUnseenEvent = true`、立即触发 `OnChanged`(红点先亮)、再 `RequestPendingRequests()`;`REQUEST_ACCEPTED` → `RequestFriendList()` + 触发 `OnFriendEvent` 供 Toast;未知 reason(含 0)只记日志、不报错(前向兼容)。**这个消息号全仓只许注册这一处**(`OnNotify` 是覆盖语义)。
- **C2S**:写操作的响应只带 `error_message`,**不回全量列表**(与 Pet 不同)—— 成功后必须**自己再拉**一次(`Accept` 成功 → `RequestPendingRequests()` + `RequestFriendList()`),不允许本地增删行去猜结果。回调先用 `IsCurrent(epoch, playerId)` 过滤旧回包。
- 传输错误映射(`DescribeTransportError`):`"server tip=1008"` →「操作太频繁,请稍后再试」;`"server tip=1003"` →「好友服务暂不可用,请稍后重试」;`"rpc timeout"` / `"disconnected"` / `"not connected"` →「网络不稳定,请重试」;其余原样。响应体的 1008 / 1005 / 1003 也要收进 `DescribeTip`。
- **请求体不带自己的 `player_id`**:proto 里已 `reserved 1`,生成类里不会有这个字段(D-9:身份只从会话取)。
- 服务端推荐参数(核自 `go/friend/internal/config/config.go`):`RecommendDefaultLimit` 10、`RecommendMaxLimit` 20、**`RecommendMaxExclude` 64**(超出回 1005)—— 客户端「换一批」回传的 `exclude_player_ids` 只保留最近若干批,别无限累加。

---

## 6. 红点规则

- **真相**:`PendingIncomingCount` = 最近一次**成功**的 `GetPendingRequestsResponse.requests.Count`。服务端只返回 `to_player_id = 我` 且 PENDING 的行 —— **只有收到的申请,没有「我发出的」列表**,UI 不要做「已发送」页。
- **提示位**:`HasUnseenEvent` 由 `REQUEST_RECEIVED` 推送置位,让红点在拉取返回之前先亮。
- **显示条件**:`PendingIncomingCount > 0 || HasUnseenEvent`。入口文字「好友 · N」;提示位为真但 N 还是 0 时显示「好友 · 新」。
- **熄灭**:一次成功的 `GetPendingRequests` 返回后清 `HasUnseenEvent`,N 以真相为准;同意 / 拒绝成功后重拉,N 自然下降。**打开面板不直接清 N** —— 申请还在,红点就该在。
- **拉取失败**:保留上一次的 N 与提示位,不清零也不自增(照 `PlayerFeaturesClient`「保留失败前的有效快照」)。
- **`REQUEST_ACCEPTED`**:只拉好友列表并弹 Toast。是否给「新好友」亮红点**产品口径未定**。
- 推送是 at-most-once。**红点的正确性只建立在 §4 的三次拉取上,推送只是加速手段。**
- 断线、换角色、`Dispose` 时清零。

---

## 7. 不要做的事

1. **不要调用 `NotifyFriendEvent`。** 它的消息号只许出现在 `RegisterNotify` 的第一个参数里,不许传给 `Call` / `SendOneWay`。它和 C2S 同 service,服务端在会话拦截器的方法白名单里刻意排除了它(否则客户端可以伪造好友事件),调了只会拿到信封码 1003。
2. **不要调用 `HandlerRegistry.Register`,也不要写 `*.user.cs` partial** —— 会静默盖掉 `GameClient` 手写的 `RedirectToGate` / `NotifyEnterScene` 处理器。
3. **不要在请求里带自己的 player_id**,也不要用客户端推断"我是谁"。
4. **不要本地推算列表**(点了同意就把行挪到好友页、点了删除就删行)。只认拉回来的快照。
5. **不要在被限频或失败后自动重试、轮询或定时刷新** —— gate 会把每次被拒计入非法包,累计到阈值就断开连接。「换一批」推荐按钮要有单飞和冷却(建议档位 1 次/秒)。
6. **不要手改生成物**(`MessageIds.cs`、`Net/Generated/*`、`Proto/Generated/*`),不要手填消息号数字,不要混用两次 proto-gen 的产物。
7. **不要用 FairyGUI,不要新建预制体式面板,不要新造 UIManager 或红点框架**(只有一个功能要用,YAGNI),不要新增美术资源。
8. **不要伪造昵称、等级或头像。** 协议里只有 id、在线状态、时间戳、共同好友数(`display_name` 留给玩家档案二期)。用中性占位如「道友 · {id}」(照 `TeamWindow.DisplayName` 的口径)。
9. **不要用 `InGame` 作为首拉的放行条件;不要只靠 `Disconnected` 复位在途标志**(§4)。
10. **不要把 Team 的 UI 状态或战斗匹配队伍与 friend 混用**,不要动 `TeamUiRoot.State` 的接线(那是留给组队服务的)。
11. **文案不要泄露拉黑方向**(§2.5 的 15007)。

---

## 8. 验收判据

**静态(可 grep):**

- `NotifyFriendEventHandler.MessageId` 在 `Assets/Scripts`(排除 `Net/Generated`)里只出现 1 次,且位于 `RegisterNotify(` 调用内。
- `HandlerRegistry.Register` 仍为零调用;`*.user.cs` 文件数仍为 0。
- `FriendClient.cs` 不含 `using UnityEngine`,不含数字形式的消息号字面量。
- `Friendpb.*Request` 的构造处没有 `PlayerId =`(只有 `TargetPlayerId` / `FromPlayerId`)。
- 重新生成后,客户端全部桩的 `MessageId` 以及 `MessageIds.cs` 与服务端 `proto/message_id.txt` 逐项一致;原有 72 + 73 个值一个都没变。

**EditMode(`FriendClientTests`,用 `FakeBattleTransport`;被测 Client 直接 `new FriendClient(_net)`,不经 `Attach`,避免污染单例):**

1. `HandleSceneEntered` 发出且只发出 `GetPendingRequests` 和 `GetFriendList` 各 1 条;已有在途请求时再次调用不重复发。
2. `PushNotify(REQUEST_RECEIVED)` 后 `HasUnseenEvent == true`,并发出 1 条 `GetPendingRequests`;回包带 2 行时 `PendingIncomingCount == 2`,提示位被清。
3. `PushNotify(REQUEST_ACCEPTED)` 后发出 1 条 `GetFriendList`;未知 reason 不抛异常、不发请求。
4. 写操作单飞:第二个写请求被本地拒绝,不发网络请求。
5. 写成功后自动重拉;写回包带 tip(15001)时 `OnError` 含对应文案,列表不变,`Busy` 复位。
6. `FailWith("server tip=1008")` 给出「操作太频繁」的文案,且**不产生任何新的 Call**(证明无自动重试);`FailWith("rpc timeout")` 同样不重试。
7. 拉取失败时保留上一份列表和 N。
8. 断线后全部清空,迟到的回包不回填;`PlayerId` 变更后旧角色的回包被丢弃。
9. **重定向形态**:不触发 `RaiseDisconnected`,让在途请求 `FailWith("disconnected")`,随后调 `HandleSceneEntered`;断言 Loading / Busy 已复位,并重新发出两条读请求。
10. `IsReady=false` 或 `PlayerId=0` 时不发任何请求。
11. `Calls` 里**永远没有** `NotifyFriendEvent` 的消息号。
12. `Dispose` 之后事件退订,回调失效。

可抄的样板用例名:`PetClientTests` 的 `RequestListAppliesServerList` / `ServerPushReplacesList` / `DisconnectDropsListAndInFlightWrite` / `TipInResponseBodyIsRejectionKeepsListAndClearsBusy` / `SecondWriteIsRejectedWhileFirstIsInFlight` / `NotReadyTransportRejectsBeforeSending`;`PlayerFeaturesClientTests` 的 `OlderListResponsesAndErrorsCannotOverrideNewerRequests` / `DisconnectClearsAllSnapshotsAndLateCallbacksCannotRepopulate` / `NewCharacterDropsPreviousDataAndIgnoresPreviousCharacterResponse` / `DisposeUnsubscribesAndInvalidatesCallbacks`。

**联机(需服务端 friend + client_rpc_router 在线,且 gate 以路由服模式启动;全部未执行):**

- 两个账号互加,B 在线时红点在约 1 秒内亮起。
- B 离线期间被申请,B 登录后**不依赖推送**红点自亮。
- B 在重定向窗口内被申请,落地后红点自亮。
- 同意后双方好友页互见;拉黑后对方再次申请被拒,客户端展示中性文案。
- 连点「换一批」不掉线,gate 日志里的非法包计数不增长。

**给 Codex 的命令**:`pwsh -File <客户端仓根>\tools\client_compile_check.ps1`(期望退出码 0);Unity EditMode 按名字过滤 `MmorpgClient.Tests.EditMode.Battle.Friend`,PlayMode 过滤 `FriendWindowPlayModeTests`。

---

## 9. 未核实清单

- `OnSceneEntered` 触发那一刻 friend 服务对该会话是否已可达(首拉会不会拿到 1003 / 1005)。
- 15007–15009 三个 tip 码的实际数值(导表器未跑)。
- protoc 35.1 生成的 C# 与客户端 Google.Protobuf 3.28.3 运行时的兼容性;现有 `Proto/Generated` 当初是哪个 protoc 版本出的。
- `Trade` / `Teampb` / `Friendpb` 三个 C# namespace 与现有手写代码是否撞名;`team.proto` / `jubaozhai.proto` 加进清单后是否还有缺失的 import。
- 左侧入口 (68, 600) 的视觉是否遮挡;快捷键 `F` 是否与既有按键冲突。
- 客户端的 `MessagelimiterTable` 生成类是否在运行时加载了数据(可用于本地节流;本规格不依赖它)。
- 「新好友」是否需要红点;`REQUEST_ACCEPTED` 的 Toast 文案(没有昵称,只能用 id 或泛称)。
- 客户端远端是否有新提交(摸底时未 fetch)。
- 与「另一个正在改客户端协议的会话」是否存在同批生成冲突 —— 需要用户确认之后再开启 `enable_unity_client`。
