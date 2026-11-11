# 交接:战报回放的客户端细节(2026-11-06)

> **状态**:服务端 C++ / Go 已落码,协议已生成,剩余验证由 Codex 按 [设计稿](../design/battle-report-replay.md) §17 执行(现状见本文 §8)。客户端还没做,本文是交给客户端的全部细节。
> **没看过客户端仓**:本次没有客户端任务授权,没读 `../mmorpg-client`。文中「复用观战播放」「GZipStream 可用」等都基于服务端协议推断,落码前请在客户端仓核对,§9 列了要核对的点。
> **服务端设计**:[battle-report-replay.md](../design/battle-report-replay.md)。负责人 2026-11-06 定:只录 PVP,每名玩家保留最近 100 条。

## 1. 要做的三件事

1. **PVP 战报列表界面**:列出本人最近 100 场 PVP 对局,每条能「看回放」。
2. **回放播放**:取回放、解压、按回合播放,支持倍速、跳过、从任一回合开始。
3. **(可选)从近期战斗结果跳回放**:[近期战斗结果](../design/battle-result-recovery.md) §7 的结果面板里,PVP 局可以加「看回放」,同样用 `battle_id` 取。

## 2. 协议

两条请求都走**大厅连接**(gate → match),和 `WatchBattle`、`RequestBattleTicket` 一样;不需要、也不要建 battle 直连。

**生成**:新消息在 `proto/battle/player_battle.proto` 和 `proto/match/match_service.proto` 末尾,两份文件客户端本来就在生成白名单里(请核对)。Codex 服务端验证用的是不写客户端仓的配置;客户端落码时按客户端的流程用默认配置重新生成 C#,消息号以生成结果为准。

| 请求 | 响应 | 说明 |
|---|---|---|
| `MatchService.ListBattleReports(ListBattleReportsRequest{})` | `ListBattleReportsResponse{ error_message, reports }` | 本人列表,新的在前,最多 100 条,一次全给,没有翻页 |
| `MatchService.GetBattleReport(GetBattleReportRequest{ battle_id })` | `GetBattleReportResponse{ error_message, replay }` | 一局的回放外壳 `BattleReplayEnvelope` |

**`BattleReportSummary`**(列表的一条,终局时生成,之后不变):

| 字段 | 用法 |
|---|---|
| `battle_id` | 取回放用;发号器发的打散值,**不能按大小排序**,列表顺序以服务端返回为准 |
| `mode` | `MatchMode`:`MATCH_MODE_1V1`、`MATCH_MODE_5V5`、`MATCH_MODE_PVP_TEAM`、`MATCH_MODE_PVP_CHALLENGE`(切磋)等,按客户端文案显示 |
| `battle_config_id` | 副本 / 玩法配置 id,需要时查配表显示名称 |
| `finished_at_ms` | 结束时刻(Unix 毫秒),按本地时区显示 |
| `outcome` | `BATTLE_OUTCOME_SIDE_A_WIN` / `SIDE_B_WIN` / `DRAW`。本人胜负见下 |
| `total_rounds` | 回合数 |
| `members` | 双方真人参战者(开战时的名字、等级、职业 `class_id`),按 `team_index`、`player_id` 升序;不含宝宝和怪物 |
| `has_replay` | false = 这一局回放没存下来(录制被丢弃),不显示「看回放」或置灰为「无回放」 |

**本人胜负**:在 `members` 里找 `player_id` 等于本人的那一项,取它的 `team_index`:`outcome == SIDE_A_WIN` 且 `team_index == 0`,或 `outcome == SIDE_B_WIN` 且 `team_index == 1` 为胜;`DRAW` 为平;其余为负。

**`BattleReplayEnvelope`**:`format_version`(现在恒为 1)、`codec`(`BATTLE_REPLAY_CODEC_GZIP = 1`;`BATTLE_REPLAY_CODEC_NONE = 0` 是服务端兜底用的未压缩)、`raw_bytes`(解压后的字节数)、`payload`。

**错误码**(`error_message.id`,文案按 tip 表显示):

| 码 | 什么时候 | 客户端怎么做 |
|---|---|---|
| `kMatchBattleReportNotFound` | 这一局不在本人列表里(不存在、已被新局挤出、不是参战者),或者没存回放 | 显示 tip 文案;从当前列表里去掉这一条,或重新拉一次列表 |
| `kMatchInternal` | 服务端读库失败、会话身份缺失 | 提示稍后重试;不要自动连续重试 |
| 限频拒绝(MessageLimiter 表里配的码) | `GetBattleReport` 每秒超过 1 次、`ListBattleReports` 每 2 秒超过 1 次 | 提示操作太频繁;按钮做防连点 |

## 3. 列表界面

- **什么时候拉**:打开界面时拉一次。服务端没有推送;列表条目在终局提交成功之后才写入,刚打完一局时可能要过一两秒才出现。不要轮询。
- **单飞**:同一时刻只发一个 `ListBattleReports`。
- **每条显示**:时间、模式、双方成员(名字、等级、职业图标)、本人胜负、回合数、「看回放」按钮(`has_replay` 为 false 时不显示或置灰)。
- **条数**:服务端最多给 100 条,第 101 局会把最旧一条挤掉;客户端不用自己截断。

## 4. 取回放与解码

1. 调 `GetBattleReport(battle_id)`,单飞,按钮防连点。
2. 校验外壳:
   - `format_version != 1`:提示「请更新客户端后查看」;
   - `codec == GZIP`:按下一步解压;`codec == NONE`:`payload` 直接就是 `BattleReplay` 的字节;其他值:提示无法播放。
3. 解压:C# `System.IO.Compression.GZipStream`(RFC 1952,标准 gzip)。读取时设上限 `min(raw_bytes, 1 MiB)`,超过就判坏、停止读取(防解压炸弹);读完后长度必须等于 `raw_bytes`。
4. 解析:`BattleReplay.Parser.ParseFrom(bytes)`,失败提示「回放数据损坏」并记日志(带 `battle_id`)。
5. 缓存(可选):回放在终局时就定死了,同一 `battle_id` 永远是同一份,可以在进程内缓存最近几份;单份解压后最大 1 MiB,注意内存。

## 5. 播放

`BattleReplay` 里的帧就是观战下发的那两种消息的观众版,建议直接复用观战的播放状态机:

| 回放字段 | 等同于观战里的 |
|---|---|
| `start_state`(`BattleStateS2C`) | 进入观战时的首帧状态(`SpectateStateS2C.state`) |
| `rounds[i]`(`TurnResultS2C`) | 第 i+1 个 `NotifySpectateTurnResult` |

- **只播不发**:播放期间不建直连,不发任何战斗上行(`SubmitBattleAction`、`SetAutoBattle`、`StopWatchBattle` 等)。
- **倍速与跳过**:按现有演出倍速做;「跳过」直接显示最后一帧状态和结局。
- **从任一回合开始**:每帧都带结算后的全量状态。从第 k 回合(从 1 数)开始播时,用 `rounds[k-2].state` 当起始状态(k = 1 时用 `start_state`),再播 `rounds[k-1]` 的事件。
- **观众视角**:帧里没有任何人的冷却、道具余量和装备概率,参战者回看自己的局也一样。界面上不要显示这几项,或者显示为「—」。
- **实时字段**:`action_deadline_ms` 恒为 0,`pending_actor_ids` 恒为空:隐藏行动倒计时和「等待出手」提示。
- **结局**(看 `end_reason`):
  - `BATTLE_REPLAY_END_FINISHED`:播完最后一帧后按 `outcome` 显示胜负;
  - `BATTLE_REPLAY_END_DEADLINE_DRAW`:播完 `rounds` 之后,把状态切到 `final_state`,提示「超时平局」(此时 `rounds` 可能为空);
  - `truncated == true`:最后一帧之后提示「回放只录到第 N 回合」(N = `rounds` 的条数),再按 `outcome` 显示结局。`total_rounds` 是实际打的回合数,可能比 N 大。
- **配表**:技能、buff、道具的名称和特效按客户端当前配表显示。`table_fingerprint` 是录制时服务端的战斗配表指纹;如果客户端能拿到当前指纹且两者不同,可以提示「游戏已更新,技能显示可能与当时不同」。拿不到就不提示。

## 6. 边界

- 回合数一般是几回合到几十回合,极端的长局可能上百回合;回放解压后最大 1 MiB,一般几十 KB 到几百 KB。
- 名字、等级、职业是开战时的快照,改名之后列表里仍显示旧名字。
- 列表和回放都只有参战者自己能看;别人的 `battle_id` 一律回 `kMatchBattleReportNotFound`。
- 掉线重连之后按大厅连接现有的重连流程走,重新拉列表即可;回放播放本身不依赖连接。

## 7. 测试(EditMode,假 transport)

| 类别 | 用例 |
|---|---|
| 列表 | 空列表;100 条按服务端顺序显示;四种胜负判定(A 胜我在 A、A 胜我在 B、B 胜、平);`has_replay = false` 不显示按钮;单飞 |
| 解码 | gzip 正常;`codec = NONE`;未知 codec;`format_version = 2`;解压长度与 `raw_bytes` 不符;解压超过 1 MiB;payload 损坏 |
| 播放 | 从第 1 回合播完;从中间回合开始;`DEADLINE_DRAW`(含 `rounds` 为空);`truncated`;跳过 |
| 错误码 | NotFound 移除条目;Internal 提示不自动重试;限频提示 |

**样例数据**:Codex 跑通服务端冒烟(设计稿 §17 第 5 步)后,用 robot 打一局 1v1,把 `GetBattleReport` 的回包原样存一份,给客户端当夹具;再造一份 `DEADLINE_DRAW`(整场期限到)的样例。

## 8. 服务端状态(2026-11-10)

- 协议已生成:`MatchServiceListBattleReports` 消息号 379、`MatchServiceGetBattleReport` 380,tip 码 `kMatchBattleReportNotFound = 16025`。客户端按自己的流程用默认配置重新生成 C# 即可拿到同样的号。
- C++ 编译与用例、Go 编译与纯逻辑用例已有运行证据;真 TiDB 用例、robot 冒烟和导表后的限频还没跑(交 Codex,设计稿 §17)。
- `data/MessageLimiter.xlsx` 已加两行限频(379 每 2 秒 1 次、380 每秒 1 次),导表后生效。
- Java 版服务端待做(设计稿 §16);客户端契约两版相同,客户端不用区分。

## 9. 落码前请在客户端仓核对

1. 观战的播放状态机能不能直接喂 `start_state` + `rounds`(是否依赖直连、是否在收到第一帧前要求别的消息)。
2. 目标平台的 `GZipStream` 是否可用(Unity 的 WebGL 平台没有 `System.IO.Compression`;只出 PC / 移动端就没问题)。不可用时告诉服务端,改成 match 解压后下发 `codec = NONE`。
3. 客户端能否拿到当前战斗配表指纹(第 5 节最后一条)。
4. 生成白名单是否包含两份 proto,`BattleReportSummary.mode` 用的 `MatchMode` 枚举是否已生成。
