# 玩家主动切线(分线列表 + 选线切换)

> 状态:2026-10-08 落码 + 8 视角对抗式评审(21 条意见全为 minor,已按区域修复),2026-10-09 并入本机 main(Claude)。
> **服务端未编译、未跑测试,待 Codex 验证**(清单见 §10);客户端离线 Roslyn 编译 0 error,Unity 内测试与截图未跑(本机 Unity 许可证离线过期)。
> 分支:服务端 `feat/channel-switch`(隔离工作树 `E:\work\xuanming-server-mmo-wt-channel`,基于 `5f4b968bcb`),
> 客户端 `feat/channel-switch`(`E:\work\mmorpg-client-wt-channel`,基于 `8280cb13`)。
> 前置阅读:[world-channel-system.md](world-channel-system.md)(分线是什么)、
> [world-channel-autoscale.md](world-channel-autoscale.md)(线的扩缩容与排空)。

## 1. 要解决的问题

一张大世界地图可以有 N 条线(每条线是一个独立的场景实例,互相看不见)。进场时 scene_manager 自动把人分到人数最少的线。
动工前的缺口:

| 缺口 | 后果 |
|---|---|
| 没有线号 | 玩家没法说「我在 3 线」 |
| 没有「这张图有哪些线」的接口 | 客户端画不出列表 |
| 客户端没有任何切线入口 | 两个号先后登录必被分到不同线,互相看不见,也没法凑到一起 |
| 指定 `scene_id` 进场时服务端不校验 | 能进回收中的线;没有人数上限;没有冷却 |

做完之后:主界面右上角显示「N线」,点开是线路列表(流畅 / 繁忙 / 爆满 / 回收中),点一条线就切过去。

## 2. 方案总览

```
客户端                     gate        scene 节点(C++)                 Redis              scene_manager(Go)
  │  SceneInfoC2S(43){with_channel_directory=true} ──► 读目录 ──GET world_channel_directory──►│◄─每 2s 发布目录(领导者)
  │◄─ NotifySceneInfo(31) ◄──────────── SceneInfoS2C{scene_info, channel_directory}
  │
  │  EnterScene(63){config, scene_id=目标线} ─► handler 置 client_channel_pick=true
  │                                              └─ RequestSceneChange ─────────────────► EnterScene:选线预检
  │                                                                                        (回收中 / 满 / 冷却)
  │◄─ NotifyEnterScene(79){scene_id=目标线}  ◄── 路由(同节点直接换;跨节点走既有交接)
```

三条设计决定,以及为什么:

1. **不新增客户端消息号、不新增 tip 码、不新增内部 RPC。**
   列线复用已有的 `SceneInfoC2S(43) → NotifySceneInfo(31)`,切线复用已有的 `EnterScene(63)`。
   原因:新增 RPC 必然占消息号(内部 gRPC 也占),而 `message_id.txt` 当时有 3 个别的会话的 RPC 等着发号,
   全量 proto-gen 又会顺手改掉别人未 regen 的 11 个 proto 的产物;Tip.xlsx 同理(本地 main 的 3028 还没推)。
   只给现有消息加字段可以用 protoc 做窄面重生成,基线逐字节可复现,不碰任何号。
2. **分线目录是 scene_manager 发布到 Redis 的读模型,scene 节点只读转发。**
   scene 节点只知道自己身上的线,完整列表只有 scene_manager 有。让 scene 节点读一个「scene_manager 写好的、
   带类型的快照」是最窄的跨语言契约:一个键名 + 一个 proto 消息。先例是 `player:{id}:location`(同样是
   scene_manager 写 proto、C++ 读)。
3. **放行权威仍在 scene_manager 的 EnterScene。** 目录只用于展示和客户端预判(秒级滞后),切线请求到了
   scene_manager 会重新校验。

`docs/notes/todo_zh.md` 第 112 条写的是「服务端场景没有线 / 频道概念 —— 分线是客户端的显示约定」。本设计偏离它:
线号由服务端持久化。理由是按 `scene_id` 排序推导出来的线号在缩容后会整体前移(「3线」悄悄变成「2线」),
两个玩家约「去 3 线」就会走岔。

## 3. 协议

### 3.1 客户端可见(`proto/scene/player_scene.proto`)

```protobuf
enum SceneChannelState {
  SCENE_CHANNEL_STATE_UNKNOWN = 0;
  SCENE_CHANNEL_STATE_SMOOTH = 1;      // 流畅
  SCENE_CHANNEL_STATE_BUSY = 2;        // 繁忙
  SCENE_CHANNEL_STATE_FULL = 3;        // 爆满
  SCENE_CHANNEL_STATE_CLOSING = 4;     // 回收中
  SCENE_CHANNEL_STATE_UNAVAILABLE = 5; // 所在节点暂不可用
}
message SceneChannelInfo {
  uint64 scene_id = 1; uint32 channel_no = 2; uint32 player_count = 3; SceneChannelState state = 4;
}
message SceneChannelDirectory {
  uint32 zone_id = 1; uint32 scene_config_id = 2;
  repeated SceneChannelInfo channels = 3;  // 按 channel_no 升序
  uint64 updated_at_ms = 4;
  uint32 switch_cooldown_seconds = 5;      // 0 = 无冷却
  uint32 max_players_per_channel = 6;      // 0 = 不限
  bool switch_enabled = 7;
}
message SceneInfoS2C {
  repeated SceneInfoComp scene_info = 1;
  SceneChannelDirectory channel_directory = 2;   // 新增;副本 / 镜像里或目录未发布时不带
}
message SceneInfoRequest {
  bool with_channel_directory = 1;               // 新增;true 才读目录、才带目录
}
```

`with_channel_directory` 是显式开关:压测机器人把 `SceneInfoC2S` 当高频的「动一下」在发
(`robot/logic/ai/robot_ai.go`),默认附带目录会让每条请求多一次 Redis 往返、推送变大,压测口径随之漂移。
不带开关的请求,scene 节点走的代码路径与开销和改动前相同。

- **列线**:客户端发 `SceneInfoC2S{with_channel_directory=true}`(消息号 43,应答类型 `Empty`,数据走推送),收 `NotifySceneInfo`(31)。
- **切线**:客户端发 `EnterScene`(63),`scene_info.scene_config_id = 当前地图`,`scene_info.scene_id = 目标线`。
  同步应答无错 = 已受理;到达 = 随后收到 `NotifyEnterScene`(79)且 `scene_id == 目标线`;
  受理后的失败 = 随后收到 tip(23 号消息)。
- 只有 `SMOOTH` / `BUSY` 可选。

### 3.2 内部(`proto/scene_manager/scene_manager_service.proto`)

`EnterSceneRequest.client_channel_pick = 11`(bool)。只有 scene 节点的客户端 EnterScene 入口在
`scene_id > 0` 时置位;队伍跟随、镜像、疏散、登录、跨节点交接的**重发**都不置位。

### 3.3 复用的现成 tip(不加新码)

| tip | 现在的发出点 | 切线场景下的含义 |
|---|---|---|
| 3008 `kEnterSceneYouInCurrentScene` | handler 同步 | 已经在这条线 |
| 3014 `kEnterSceneChangingScene` | handler 同步;受理后(拿 18 起交接时已有交接 / 冻结在途) | 上一次换场景还没结束 |
| 3023 `kEnterSceneFailed` | handler 同步(战斗中)/ 异步(scene_manager 任何非 18 的拒绝) | 切线失败 |
| 1003 `kServiceUnavailable` | handler 同步(没有 SceneManager);受理后(scene→scene_manager 传输失败,结果未知);gate 路由失败 | 服务暂不可用 |
| 1008 `kRateLimitExceeded` | gate 限流 | 发太快 |

客户端把受理后收到的 3023 / 3014 / 1003 都当作「这次切线没成」(`SceneChannelClient.IsSwitchFailureTip`);
其中 1003 结果未知,客户端另记一份,75 秒内真的到了目标线就补记成功(见 §7)。

scene_manager 的拒绝码(§4.4)在 C++ 侧一律变成 3023(`player_lifecycle.cpp` 的既有行为,本次不改:
该文件正被跨区会话 CPP-3 改写)。客户端因此拿不到「是满了还是冷却中」的细分原因 ——
用目录做事前预判(置灰 + 文案)来补,失败后自动刷新目录再给出具体原因。后续若要细分,在
`HandleSceneChangeEnterSceneReply` 里把 22 / 23 / 24 映射到 3007 / 3006 / 1008 即可,不需要新码 ——
但必须与客户端 `IsSwitchFailureTip` / 文案、robot channel-smoke 的 C4 / C6 断言**同批**修改,否则客户端认不出新码会等满 75 秒。

## 4. scene_manager

### 4.1 Redis 键(全部由 scene_manager 拥有)

| 键 | 类型 | 内容 | TTL |
|---|---|---|---|
| `world_channel_lineno:zone:{zone}:{conf}` | HASH | field = scene_id(十进制),value = 线号 | 无 |
| `world_channel_directory:zone:{zone}:{conf}` | STRING | `SceneChannelDirectory` 序列化字节 | `3 × 发布周期 + 竞选间隔`(竞选间隔 = 选主锁 TTL / 3 向上取整;默认 6 + 10 = 16s) |
| `player:{player_id}:channel_switch_cooldown` | STRING `"1"` | 存在 = 冷却中 | 冷却秒数 |

前两个键刻意不用 `world_channels:zone:` 前缀:孤儿清理按那个前缀 SCAN,会把它们当成频道集合。

### 4.2 线号

- 一条线第一次出现在目录里时拿到「最小的空闲正整数」;存量线按 `scene_id` 升序补号(snowflake 即创建先后)。
- 线在役或回收中期间线号不变;线彻底销毁后号被释放,之后新建的线可以复用。
- 分配在一段 Lua 里原子完成:输入 = 这张图当前「在役 ∪ 回收中」的全部 scene_id(Go 侧按数值升序排好,
  Lua 里只当字符串用 —— snowflake 超过 2^53,不能进 Lua number);脚本删掉不在输入里的 field,
  给没有号的成员按输入顺序补最小空闲号,返回整张表。
- **输入为空时不执行脚本**(Redis 抖动时 SMEMBERS 可能读空,执行会把整张表清掉,恢复后线号全部重排)。
- 表里的坏值(非数字 / 小于 1 / 非整数 / 超 uint32 / 重号)按「没有号」删掉重发;不修的话 Go 侧每轮解析失败,
  这张图的目录永远发布不出来。
- 只有领导者发布目录,所以分配实际是单写者;Lua 只是防并发副本的第二道保险。

### 4.3 目录发布

- `StartWorldChannelDirectoryPublisher`:`safego.Loop`,周期 `DirectoryRefreshSeconds`(默认 2s),只有领导者执行,
  遍历 `GetActiveZones() × worldConfIds()`。与 `WorldAutoscale.Enabled` 无关,自动伸缩关着也发布。
- 一张图的一轮:
  1. `SMEMBERS world_channels:zone:{z}:{conf}`(在役)与 `SMEMBERS world_channels:draining:zone:{z}:{conf}`(回收中);
     读失败 → 本轮跳过这张图(不发布、不动线号),记日志。读 `scene:{id}:node` 失败同样跳过这张图。
  2. 两个集合都空 → 不发布(旧目录自然过期)。
  3. 线号分配(§4.2)。
  4. 逐线读 `instance:{id}:player_count` 与 `scene:{id}:node`,判定状态(下表)。**只读**:
     不调用 `GetBestWorldChannel` / `ReserveBestWorldChannelForEnter`(它们会懒改派并发 CreateScene)。
  5. 按线号升序组 `SceneChannelDirectory`,`SETEX`。
  6. 每张图记一次 `scene_manager_world_channel_directory_publish_total{zone_id, outcome=ok|empty|error}`
     (按 zone 的发布结果计数,低基数;不是按线指标)。领导者在位而 `ok` 速率为 0 = 目录停摆。
- 状态判定(纯函数,自上而下第一条命中):

  | 条件 | 状态 |
  |---|---|
  | 在回收中集合里 | `CLOSING` |
  | `scene:{id}:node` 为空,或节点身份歧义,或节点不存活 | `UNAVAILABLE` |
  | 上限 > 0 且 人数 ≥ 上限 | `FULL` |
  | 上限 > 0 且 人数 × 100 ≥ 上限 × `BusyPercent` | `BUSY` |
  | 其它 | `SMOOTH` |

- **每线人数上限** = `ChannelSwitch.MaxPlayersPerChannel`;为 0 时取 `WorldAutoscale.ScaleOutPlayerThreshold`;
  两者都 ≤ 0 时取 2000。含义:自动伸缩认为「这条线该扩了」的人数,就是手动选线不再放人的人数。
  这是**软上限**:只拦玩家主动选线;自动选线、队伍跟随不看它;预检与预占不在同一原子步骤里,并发下可能超出几个人。

### 4.4 EnterScene 的选线预检

位置:跨 zone 重定向分支之后、解析目标场景(第 3 步)之前 —— 此时 location 已读、陈旧位置已过滤,
还没有任何预占、任何写入,也早于死节点改派和 18 换手门。条件:`in.ClientChannelPick && in.SceneId != 0`。

1. **目标是不是本 zone 的大世界分线**:先按 `in.SceneConfId` 查(非 0 时),查不到再遍历 `worldConfIds()`,
   分别 `SISMEMBER` 在役集合与回收中集合。
   - 在回收中集合里 → 拒 `ErrChannelUnavailable`(reason `channel_closing`)。
   - 都不在 → **不是分线**(按 id 进副本 / 镜像),预检到此为止,走原逻辑,不产生任何效果。
   - 在某张图 X 的在役集合里,但 `in.SceneConfId != 0 && in.SceneConfId != X` → 拒 `ErrChannelUnavailable`
     (reason `channel_conf_mismatch`;请求的地图会被死节点改派拿去建场景,不能信一个对不上的值)。
2. `ChannelSwitch.Disabled` → 拒 `ErrChannelUnavailable`(reason `channel_switch_disabled`)。
3. 人数 ≥ 上限 → 拒 `ErrChannelFull`(reason `channel_full`)。
4. **冷却**:仅当这是一次真正的切换(有位置记录、落在具体节点上、`location.scene_id != 0` 且不等于目标)。
   冷却键还在 → 拒 `ErrChannelSwitchCooldown`(reason `channel_switch_cooldown`)。预检只读,不占冷却。
5. Redis 读失败 → 拒 `ErrRedis`(fail-closed,与同文件其它读失败一致)。

**冷却什么时候开始计**:这条置位请求的最终应答是成功(0)或 `ErrHandoffPending`(18)时写冷却键。
18 也算,是因为跨节点切线的第一跳必拿 18,C++ 随即冻结 → 存盘 → 写标记 → **重发一条不带标记的 EnterScene**
(`PlayerSceneChangeInFlightComp` 只记 scene_id / scene_conf_id,重发时重新构造请求)。
所以规则是「预检只在第一跳做,重发直接放行」:若在入口就占冷却,重发会被自己挡住;若等成功再占,
重发那一跳不带标记、永远不会占。17(再入屏障)、19(epoch 冲突)这类可重试拒绝不计冷却。

新增内部错误码(`internal/constants/errors.go`,scene_manager 私有数轴,不是 tip):

| 值 | 名字 | 含义 | 算不算故障 |
|---|---|---|---|
| 22 | `ErrChannelUnavailable` | 玩家选的线不可进入(回收中 / 功能关闭 / 地图对不上) | 否,业务拒绝 |
| 23 | `ErrChannelFull` | 玩家选的线人数已到上限 | 否 |
| 24 | `ErrChannelSwitchCooldown` | 切线冷却未过 | 否 |

### 4.5 配置(`ChannelSwitch`,全部可缺省)

| 字段 | 0 值含义 | 说明 |
|---|---|---|
| `Disabled` | false | true = 关闭玩家主动切线(目录照发,`switch_enabled=false`) |
| `CooldownSeconds` | 取默认 10 | 负数 = 不设冷却 |
| `MaxPlayersPerChannel` | 跟随 `WorldAutoscale.ScaleOutPlayerThreshold` | 见 §4.3 |
| `BusyPercent` | 取默认 60 | 人数达到上限的这个百分比显示「繁忙」 |
| `DirectoryRefreshSeconds` | 取默认 2 | 负数 = 不发布目录(等于关掉列线) |

读配置一律走取值方法(零值安全),不直接读字段:`WorldAutoscale` 这类 `json:",optional"` 的嵌套块缺省时,
嵌套字段的 `default=` 标签是否生效没有保证。

| 取值方法 | 返回 |
|---|---|
| `ChannelSwitchConfig.EffectiveCooldownSeconds()` | 0 → 10;负数 → 0(= 不设冷却);上限一天 |
| `ChannelSwitchConfig.EffectiveBusyPercent()` | 0 → 60;钳到 [1, 100] |
| `ChannelSwitchConfig.EffectiveDirectoryRefreshSeconds()` | 0 → 2;负数 → 0(= 不发布);上限一天 |
| `Config.ChannelMaxPlayers()` | `MaxPlayersPerChannel` → `WorldAutoscale.ScaleOutPlayerThreshold` → 2000;恒为正,上限 uint32 |

`ChannelMaxPlayers` 挂在 `*Config` 上,因为它要同时读 `WorldAutoscale`。

### 4.6 清理

- 线彻底销毁:不用显式删线号,下一轮分配脚本会把它从表里摘掉。
- 地图从 World 表移除(孤儿清理 `deleteOrphanChannel`):顺手 `DEL` 这张图的线号表与目录键。
- 合服工具 `tools/merge_zone`:场景热状态清理模式里加上两个新前缀。

## 5. scene 节点(C++)

改动只有三个文件,不新增文件、不动工程文件:

- `player_scene_handler.cpp` 的 `EnterScene` 守护段:`req.set_client_channel_pick(scene_info.scene_id() > 0);`
  仍只经 `PlayerLifecycleSystem::RequestSceneChange` 这一个出口发出。
- `player_scene_handler.cpp` 的 `SceneInfoC2S` 守护段:改成一行委托
  `PlayerSceneSystem::SendSceneInfo(player, request->with_channel_directory())`。
- `player_scene.{h,cpp}` 新增 `PlayerSceneSystem::SendSceneInfo(player, withChannelDirectory)`:
  1. 取玩家当前场景的 `SceneInfoComp`;没有 → 记日志返回(与旧行为一致)。
  2. 请求要了目录(`withChannelDirectory`)、当前场景是大世界线
     (`mirror_config_id == 0 && dungeon_config_id == 0 && scene_config_id != 0`)、实体有 Guid、且 zone Redis
     已连接 → 异步 `GET world_channel_directory:zone:{GetZoneId()}:{scene_config_id}`。
  3. 回调里:实体仍有效且还是同一名玩家、仍在发起时的那个 `scene_id`(否则丢弃,客户端进新场景后会重新拉);
     应答是字符串且能解析成 `SceneChannelDirectory`、且 `scene_config_id` 与当前一致 → 放进 `channel_directory`;
     键不存在 / 读失败 / 解析失败 → 不带目录。**无论带不带目录都推一条 `SceneInfoS2C`**,客户端据此结束等待。
  4. 不满足第 2 步的条件(没要目录 / 副本 / 镜像 / Redis 未连接)→ 同步推不带目录的 `SceneInfoS2C`。
  5. 读命令没被 hiredis 收下(连接正在断开等,回调永远不会来)→ 同步补推不带目录的一条。
  6. hiredis 的异步命令没有单条超时:Redis 连着但不回时客户端收不到 31,由客户端自己的 5 秒等待上限兜底。

键名格式在 Go(`WorldChannelDirectoryKeyFmt`)与 C++(`kWorldChannelDirectoryKeyFmt`)各有一份,
两处注释互相指向;Go 侧有一条测试钉住字面量。

## 6. 组队

现状(既有行为,本次不改服务端):队长进场后会把**同节点**的队员拉到自己那条线;队员自己换场景后会被拉回队长那条线;
跨节点的队员不跟(team-system.md DV-6 / J-4,另立项)。

v1 规则:

- **队长可以切线**,同节点队员自动跟过去(既有扇出)。队伍跟随的请求不带 `client_channel_pick`,不受上限和冷却约束。
- **队员不能主动切线**:客户端置灰,文案「队伍中由队长切线,或先离队」。服务端不新增拦截 ——
  绕过客户端的后果就是现状(被拉回 / 与队伍分开),没有安全问题;硬拦要在 EnterScene 入口多一跳异步读队伍投影,
  而转让队长不发事件,缓存的队长身份会过期。
- 已知缺口:队长切到别的节点上的线时,留在原节点的队员不会跟。

## 7. 客户端(`mmorpg-client`)

| 文件 | 职责 |
|---|---|
| `Assets/Scripts/Game/WorldTravel/SceneChannelModels.cs` | 纯 C#:线路视图模型、状态文案、「这条线现在能不能点 / 为什么不能」 |
| `Assets/Scripts/Game/WorldTravel/SceneChannelClient.cs` | 纯 C#:发 43 / 收 31 / 发 63,单一发送出口 + 最小间隔,切线结果判定,本地冷却倒计时 |
| `Assets/Scripts/UI/Ugui/Gameplay/SceneChannelWindow.cs` | 线路面板(只发意图事件,不碰网络) |
| `Assets/Scripts/UI/Ugui/Gameplay/SceneChannelUiRoot.cs` | 右上角「N线」角标 + 面板宿主 + 接线,快捷键 L |
| `Assets/Editor/SceneChannelUiVerification.cs` | 离线截图验收 |
| `Assets/Tests/EditMode/Tianyong/SceneChannel*Tests.cs` | 模型 / 网络层 / 窗口 |
| `Docs/scene-channel-ui.md` | 客户端侧说明 |

`GameClient.cs` 不改:31 / 43 的处理器由 `SceneChannelClient` 经传输接口自己注册(与 `TeamClient` / `PetClient` 同款)。

要点:

- 每次进场(`OnSceneEntered`)清空线路缓存并拉一次目录,角标据此显示「N线 [L]」;目录里找不到当前 `scene_id` 时显示「线路 [L]」;
  没有目录时角标隐藏(L 键仍能打开面板)。副本 / 镜像里不列线(入场通知带镜像 / 副本配置号即判定不适用)。
- 面板关着而角标缺线号时补问目录,最多 3 次(15 / 30 / 60 秒后)。
- 面板打开时每 5 秒刷新一次;所有 43 号请求经同一出口,最小间隔 2.5 秒(gate 默认限流是 2 个整秒内 3 次,
  超了会回 1008 并累计非法包计数)。
- 63 号(EnterScene)本地最小间隔 1 秒,被 1008 限流后退避 2.5 秒(连点时同步拒绝回得很快,3 次以上会撞 gate 限流并记非法包)。
- 切线结果:同步应答带 tip → 立即失败;受理后只认 `NotifyEnterScene.scene_id == 目标`;同步应答之前到达的非目标入场通知
  不算结论;受理后收到失败 tip(3023 / 3014 / 1003)→ 失败;断线 → 复位。63 发出到同步应答之间有 30 秒兜底,受理后顺延到
  75 秒(沿用换图预算,目标线可能在别的节点,要走存盘交接)。
- 「结果未知」(1003 / 无码传输错误 / 本地等待超时)的失败另记一份:75 秒内真的进了目标线,按成功补记(起本地冷却)。
- 失败文案有寿命:能说出是哪条线且该线在目录里已满 / 回收中 → 显示具体原因,该线恢复后收掉;其它失败文案在第二次列线答复时收掉。
- 冷却:服务端不下发「还剩几秒」。客户端在自己发起的切线成功时按目录里的 `switch_cooldown_seconds` 本地倒计时;
  重登后倒计时丢失,此时若仍在冷却,服务端会拒,客户端显示通用失败文案。
- 同图切线不会重建地图,但 `NotifyEnterScene` 会清空全部角色再由 AOI 重建;切线期间面板留在屏上当遮罩(可手动关;
  关着时的失败用 toast 提示)。
- 战斗、观战、排队匹配中整面板置灰(「战斗或匹配中无法切线。」);队伍成员(非队长)置灰;跨区传送在途不打开面板。
- 列表只在内容变化时重建行按钮,仅底栏变化(冷却倒计时、状态行)不重建,避免每 5 秒刷新吞掉玩家的点击。

## 8. 没做的事(有意)

| 项 | 为什么 |
|---|---|
| 自动选线从「最少人数」改成「按线号填到软上限」 | 是策略变更:会让扩缩容条件、排空改派、压测的进场摊开形态一起变(world-channel-autoscale.md);需要拍板 |
| 把本地 yaml 里主城的 16 条线(压测遗留)降下来 | 配置只是种子,Redis 里的 desired 才是权威;init 只补不删,自动伸缩关着,没有现成的降线路径 |
| 重登回到上次那条线 | 登录不带 `scene_id`;干净登出后位置记录被删,需要另存「上次的线」 |
| 细分的失败提示(满 / 冷却) | 见 §3.3,等 `player_lifecycle.cpp` 的在途改动落地后加映射 |
| 跨节点队员跟随队长切线 | team-system.md J-4 |
| 服务端下发剩余冷却秒数 | 需要 scene 节点再读一个 scene_manager 的键;收益小 |
| 按线的 Prometheus 指标 | `scene_id` 不能做 label;线号可以,但本次没有消费者 |

## 9. 已知风险与依赖

1. **AOI 反向通知修复 `a1507aa41a` 不在 `origin/main`**(只在本机 main)。不带它,切到同一条线后「我能看见别人、
   站着不动的人看不见我」。合并本分支前先确认它已进主干。
2. **跨节点切线走的是 18 交接链路**(冻结 → 存盘 → 写标记 → 重发),本地 yaml 的 `AllowUnsafeCrossNodeHandoff: true`
   会绕过它;该链路此前同样未经编译与联调。单 scene 节点的本机环境只能验到同节点切线。
3. **人数计数没有对账**(既有问题):节点死亡后世界线的旧计数不清零,会显示假「繁忙 / 爆满」并误拒手动选线。
4. **目录滞后**:最多一个发布周期。领导者**优雅让位**(滚动发布)的最长无发布间隔约 14 秒,目录 TTL 16 秒盖得住;
   领导者**崩溃**切换最长约 44 秒无发布,目录会缺失最多约 28 秒 —— 有意取舍(不想让领导者全挂时陈旧目录留得更久)。
   目录缺失时客户端角标隐藏,面板显示获取失败的提示,L 键仍可打开面板。
5. **自动缩容与发布 / 预检的毫秒级窗口**:`beginDrainWorldChannel` 先 SREM 在役集合、隔几条命令才 SADD 回收中集合。
   发布恰好落在两步之间时,该线的线号会被释放、之后重发成别的号;预检两次读都落在两步之间时,这条线被当成「不是分线」,
   上限 / 冷却 / 地图校验都不做。根治是把两步合成一段 Lua(`world_autoscale.go`,本次未改);自动伸缩默认关闭。
6. **回收中的线**上的玩家会被服务端改派到默认大世界(既有行为,与 autoscale 文档写的「同图其它频道」不一致,非本次引入)。
7. **Java 版服务器**(AGENTS §12):本机没有该仓库,未做;客户端可见契约的变化是 `SceneInfoRequest.with_channel_directory`、
   `SceneInfoS2C.channel_directory` 与 `EnterScene` 指定 `scene_id` 的语义,Java 版需要同步。
8. **robot 通用的 EnterScene 回包处理器**(`robot/logic/handler/scene_scene_client_player_enter_scene.go:11`)判据是
   `GetErrorMessage() != nil`,而 C++ 每条应答都带 error_message(空 tip 也带),于是每次换场景都打一条误报的
   "enter scene rejected"。channel-smoke 自己绕开了;根治是改成 `GetId() != 0`(既有问题,本次未改)。
9. **线路面板开着时每 5 秒列一次线**,客户端消息会续活跃帧,玩家把面板开着挂机不会进入服务端的挂机态(组队窗同类)。

## 10. 给 Codex 的验证清单(全部未执行)

**编译位置**:C++ 必须在子模块齐全的检出里编(本机主仓 `E:\work\xuanming-server-mmo`,本分支已并入其 main);
隔离工作树的 `third_party` 子模块全空,直接编会以 C1083 失败。Go / robot 在主仓或工作树都行。

1. **scene_manager(Go)**,工作目录 `go/scene_manager`:
   ```powershell
   go build ./...
   go vet ./internal/logic/ ./internal/config/ .
   go test ./internal/logic/ -count=1 -run "TestChannelErrorCodes|TestChannelPick|TestChannelSwitchConfig|TestChannelSwitchStartsCooldown|TestConfig|TestEnterSceneFingerprint|TestIsChannelSwitch|TestLocateWorldChannel|TestOrphanCleanup|TestParseWorldChannelLineNumbers|TestPublishWorldChannelDirectory|TestWorldChannelDirectoryKeyFmt|TestWorldChannelDirectoryTTLSeconds|TestWorldChannelLineNumbers|TestWorldChannelState"
   go test ./... -count=1
   ```
   通过标准:全绿。全量里重点看 `TestEnterScene_*`、`TestIntegration_*`、`TestAutoscale_*` 没有回归(未置位请求的行为不应有任何变化)。
   静态评审时最没把握的点:miniredis 下线号 Lua 的行为、`mr.TTL` 精确秒数断言、`SetError` 之后的 fail-closed 用例。
2. **合服工具**,工作目录 `tools/merge_zone`:`go vet ./...`;`go test ./... -count=1`(重点 `TestSourceZonePatternsAndFixedKeys`、`TestSceneHotStateKeys`)。
3. **robot**,工作目录 `robot`(走 vendor):`go build -o robot.exe .`;`go vet ./...`。
4. **C++**(MSBuild 一律 `/m:1 /p:Configuration=Debug /p:Platform=x64`,串行):先 `cpp/libs/services/scene/scene.vcxproj`,
   再 `cpp/nodes/scene/scene.vcxproj`。只改了 `player_scene.{h,cpp}` 与 `player_scene_handler.cpp` 两个守护段,没有新文件、不动工程文件。
   编译器最可能挑的:lambda 按值捕获 `SceneInfoComp` 后转 `std::function`、`LOG << reply->len`(size_t)的重载、警告即错误。
   之后跑一遍 `pwsh tools/scripts/run_cpp_tests.ps1`(scene.lib 的链接方不应出现新的未解析符号)。
5. **起栈联调**(需要上面 1、4 的新产物部署到 `bin/`):`cd robot && .\robot.exe -c etc/channel_smoke.yaml`。
   期望日志有一行 `CHANNEL_SMOKE_OK`、退出码 0,约 20 秒;地图只有 1 条线时是 `CHANNEL_SMOKE_SKIP`(也是 0)。
   失败保留 `CHANNEL_SMOKE_FAIL` 那一行及其前 30 行,外加 scene_manager 日志里 `reason=channel_*` 的拒绝记录。
6. **客户端**(需要先在 Unity Hub 登录刷新许可证):
   ```powershell
   Unity.exe -batchmode -nographics -projectPath <客户端工程> -runTests -testPlatform EditMode -testFilter SceneChannel -testResults <xml> -logFile <log>
   Unity.exe -batchmode -projectPath <客户端工程> -executeMethod SceneChannelUiVerification.CaptureAll -logFile <log>
   ```
   第二条要图形(不加 `-nographics`),产物在 `<工程>/.codex-artifacts/scene-channel-ui/`,日志末行 `SCENE_CHANNEL_UI_CAPTURE_OK`;截图要目视。
   跑完看一眼 `Assets/Resources/Fonts/*.asset` 有没有被写入新字形,别提交成功能改动。
7. **双号联机验收**:两个客户端先后登录主城(本地 16 条线,必被分到不同线)→ 一方点右上角「N线 [L]」→ 选另一方所在的线 →
   两边互相可见。前置:main 已含 AOI 反向通知修复 `a1507aa41a` 或其等价移植 `0336e07c3e`
   (分别用 `git merge-base --is-ancestor <提交> HEAD` 核对,任一包含即可),
   不含时会得到「站着不动的人看不见我」;两个角色站近一些(视野 10、格边长 20,远处的人要等任一方跨格才建兴趣关系)。
