# 战斗剧本(battle script)

> **状态(2026-11-12)**:服务端(C++ battle 引擎 + Go match)与客户端代码已落码,**未生成协议、未编译、未测试**,待 Codex 按 §11 验证(AGENTS §10.1)。Java 版只登记待做(§8)。
> **来源**:负责人 2026-11-11 提出「战斗剧本:不同 PVP / PVE 有不同剧本,剧本自己定逻辑(先打哪个怪、先封、谁毒、谁冰、组合什么技能),玩家可以上传、由我们审核」;同日按推荐拍板:剧本是 protobuf 定义的规则表,不嵌脚本语言;整队一份、按角色位分工;只管挂机 / 托管 / 超时的人,手动出手永远优先;评分 PVP 先只开官方剧本(§12 第 3 条)。
> **取代**:[configurable-templates.md](configurable-templates.md) §4(战斗策略模板)与 §8.6.5(自动战斗策略)的方案,该文对应正文已改成指向本文。
> **相关**:[turn-based-battle-server.md](turn-based-battle-server.md)(确定性引擎)、[class-skills.md](class-skills.md) §6.5(挂机职业选技,剧本落空时的兜底)、[battle-report-replay.md](battle-report-replay.md)(建房原件即回放材料)。

## 0. 结论

1. **剧本是数据,不是代码。** 一份剧本 = 若干「角色位」,每个角色位一串「规则」(条件 → 目标 → 行动)。格式由 `proto/battle/battle_script.proto` 定义;存储与服务间传输用 protobuf 二进制;要给人看、策划手写时用 protobuf 自带的 JSON 映射(C++ `JsonStringToMessage`、Go `protojson`、C# `JsonParser`),不自己写语法解析器,不嵌 Lua / JS / WASM。
2. **唯一决策点是 match。** 每一局每一方用哪份剧本由 match 在开局时解析(队长的选用 > 官方绑定 > 不带),按值冻结进 `CreateBattleRequest.team_scripts`;battle 不读任何剧本配置。剧本因此是建房原件的一部分:随原件落库、恢复重放、回放与按指令流重算的输入都在原件里。
3. **剧本只能做手动能做的事。** 选出的行动照样过 `CheckActionPrerequisites`;剧本只读出手方本来就看得到的状态;不消耗随机数;不循环。开局时 battle 按自己的配表再校验一遍(第二道闸),不合法整份忽略、该队出手照旧并记 ERROR。
4. **管谁**:怪物、宝宝、挂机玩家、超时没提交的玩家。手动提交的行动一律不动,而且算作该角色位的「认领」,跟随的角色位可以接着打队友手选的目标。
5. **玩家剧本**:上传 → 机器校验(与引擎同一套规则)→ 待审 → 运营审核(通过 / 驳回)→ 才能选用。改一次就是新版本、重新待审,审过之前对局一直用上一份通过版。审核按 (script_id, revision, 待审) 做 CAS,流水同事务落库。

## 1. 需求与边界

| 要的 | 怎么落 |
|---|---|
| 不同 PVP / PVE 有不同剧本 | 官方绑定按 (MatchMode, 副本, 哪一方) 配;玩家选用按 (MatchMode, 副本) 配,0 表示任意,查找从具体到通用(§5.3) |
| 先打哪个怪 | 目标筛选 `IS_MONSTER`(怪物表 id)+ 排序 |
| 先封住怪 | 规则顺序 + `HAS_BUFF_TYPE`(封 = 30 眩晕 / 31 沉默,取反 = 还没被封) |
| 谁毒、谁冰 | 角色位按职业 / 单位类型认领单位,每个角色位一串自己的规则 |
| 组合什么技能 | 决策按角色位顺序,先决策的定下目标(本回合认领),后决策的用 `CLAIMED_BY_ROLE` 跟同一个目标;跨回合组合用状态条件(「已被冰 → 放毒」) |
| 玩家上传、我们审核 | §6:存储、版本、审核入口与运营工具 |

不做:剧本自动用道具(会替玩家消耗资产,选用要本人确认,另立);战斗中切换剧本;剧本的公开分享与广场(先私有);队长在队伍界面手动指派队员角色位(现在按角色位条件自动认领,§3.3)。

## 2. 选型:为什么是 proto 规则表

| 方案 | 结论 | 决定性理由 |
|---|---|---|
| **proto 规则表(采用)** | 两版各写一个几百行的求值器,共用对拍用例 | 不能循环,上传时就能算出最坏开销;确定性;只读可见状态;客户端可以做下拉编辑器;审核员读的是条文 |
| Lua / Luau | 不采用 | 玩家上传 = 不可信代码,人工审核看不出死循环、内存炸弹、沙箱逃逸;`pairs` 顺序、GC 时机破坏逐字节重放;Java 只有 5.2 语义的 LuaJ,与 C++ 5.4 整数语义不同;Lua 运行时也没接进构建 |
| JS(QuickJS 等) | 不采用 | 同 Lua |
| WASM | 不采用 | 沙箱好,但审核员看不懂二进制,玩家也写不了 |
| 行为树 / 节点图 | 不采用 | 回合制每回合只做一次瞬时决策,用不到 Running 与黑板;优先级规则最好读、最好 diff(configurable-templates.md §2 同结论) |

代价:表达力有边界,每加一种条件 / 筛选 / 排序要在 C++、Go(、Java)各写一份。边界是有意的:程序决定能做什么,策划组合,玩家在这个范围里选。新原子只在枚举末尾追加,编号上线后不复用。

**JSON 只是文本形式**:官方剧本写成 `BattleScriptConfig` 的 protobuf JSON(`go/match/internal/battlescript/official_scripts.json`,随 match 构建内置);玩家在客户端可以粘贴同样格式的一份剧本导入。字段名、类型、枚举名都由 protobuf 按定义检查,未知字段直接拒绝。

## 3. 剧本模型(`proto/battle/battle_script.proto`)

### 3.1 结构

```
BattleScript { schema_version = 1, script_id(官方)/0(玩家), name, description, roles[] }
  BattleScriptRole { role_id, order, name, actor_type, class_id, monster_id, max_members, rules[] }
    BattleScriptRule { priority, conditions[≤2], action(ATTACK/SKILL/DEFEND), skill_id, target, note }
      BattleScriptTarget { side(SELF/ENEMY/ALLY/ENGINE_PICK), filters[≤3], order }
```

集合顺序不靠下标(AGENTS §11.6):角色位按 `order`、规则按 `priority`,都校验唯一;规范化后按它们排好再交给引擎。

### 3.2 原子

| 类别 | 原子 | 参数 |
|---|---|---|
| 条件(看自己和战局) | `SELF_HP_BELOW` | p1 万分比 |
| | `ROUND_AT_LEAST` | p1 回合 |
| | `ROUND_EVERY` | 从第 p2 回合起每 p1 回合 |
| | `ENEMY_COUNT_AT_LEAST` | p1 |
| 目标筛选 | `HP_BELOW` | p1 万分比 |
| | `HAS_BUFF_TYPE` | p1 = Buff.buff_type(只收持续状态类:增减益 10/11/13、眩晕 30、沉默 31、无敌 32、免疫 34、回血回蓝 40–42、毒 50、灼烧 51、冰冻 52) |
| | `HAS_BUFF` | p1 = Buff 表 id |
| | `IS_MONSTER` / `IS_CLASS` / `IS_ACTOR_TYPE` | 怪物表 id / 职业 / 单位类型 |
| | `CLAIMED_BY_ROLE` | p1 = 角色位(0 = 自己的角色位):本回合该角色位已选它当目标 |
| 排序 | 阵位 / 气血最少 / 最多 / 比例最低 / 速度最快 | 同分按阵位、再按 actor_id 无符号升序 |
| 行动 | 普攻 / 技能 / 防御 | 技能填 skill_id |

条件与筛选都可以 `negate` 取反。比例判定全是整数交叉相乘(饱和,不回绕),C++ / Go / Java 逐位一致。

### 3.3 角色位认领与决策顺序

- **认领**(开局一次):本方单位按阵位逐个,找第一个接收它(单位类型、职业、怪物 id 都匹配)且没满员的角色位(按 `order`)。具体的角色位(按职业、按怪物)排前面,「其余单位」放最后。没认领到的单位出手照旧。
- **决策**(每回合):按角色位 `order`,同一角色位内按阵位,逐个替还没有行动的单位选:规则按 `priority` 逐条,条件全成立 → 按 side 收集候选、筛选、排序 → 依次试候选直到过 `CheckActionPrerequisites`;一条都不行 → 留给现行默认(挂机玩家先试 class-skills §6.5 的职业选技,其余普攻)。
- **认领表**:每回合每队一张 (role_id, target_id)。先登记本队已提交的手动行动,再随决策追加。`CLAIMED_BY_ROLE` 读它:组合技、集火、同一角色位不重复封同一个(`CLAIMED_BY_ROLE 0` 取反)。

### 3.4 例子(写成规则)

```
剧本「血魔副本·先封后杀」,玩家方
角色位 1「封系」(职业 11)   p10 放封印 → 敌方[是怪物 102、没被封(HAS_BUFF_TYPE 30 取反)]
                             p20 普攻   → 敌方 气血最少
角色位 2「冰系」(职业 12)   p10 放冰冻 → 敌方[封系本回合没选的、没被冰]
角色位 3「毒系」(职业 13)   p10 放毒   → 敌方[冰系本回合选的、没中毒]        ← 组合:跟冰系同一个
角色位 4「输出」(其余玩家) p10 普攻   → 敌方[冰系本回合选的]                ← 集火
                             p20 普攻   → 敌方 气血最少
```

`cpp/tests/turn_battle_engine_test/battle_script_test.cpp` 的 `RolesSealFreezePoisonAndFocusTogether` 就是这一份。随 match 内置的官方剧本(`official_scripts.json`)用现有四职业技能写了一份「集火控速」和一份怪物剧本「首领·先打治疗」(绑在副本 3 的怪物方);现有技能表里还没有封 / 冰 / 毒类技能,策划补技能后剧本直接能用。

## 4. 引擎(C++,`cpp/libs/services/battle`)

- **输入**:`CreateBattleRequest.team_scripts`。`Initialize` 末尾 `BindBattleScripts`:每队至多一份;按队里有没有怪物判定是怪物方还是玩家方;`NormalizeBattleScript` + `CheckBattleScriptSide` 按本节点配表复核,不合法的记进 `ScriptRejections()`(节点 `HandleCreateBattle` 逐条记 ERROR),该队出手照旧,对局照常进行(剧本只影响出招、不碰资产,按 AGENTS §11.3 选 fail-open)。
- **接缝**:只有 `FillDefaultActions` 开头的 `FillScriptActions` 一处。手动提交的行动不动。
- **怪物技能**:怪物表没有技能列;认领了角色位的怪物,技能集 = 该角色位规则引用的技能(升序去重,开局状态里客户端可见)。怪物方只许零耗蓝的老技能(怪物没有法力列),在 match 与 battle 两道校验都拦。
- **老技能只许打敌方**:老技能没有敌我语义(`ValidateSkillTarget` 不分阵营,伤害照打);职业技能的目标方必须与 `target_team` 一致。
- **确定性**:选择不消耗随机数;候选排序是全序;认领表按固定顺序追加。没有剧本的对局与改动前逐字节相同;被忽略的剧本对事件流零影响(用例 `InvalidScriptsAreIgnoredWithoutChangingTheBattle`)。
- **对敌施加异常**:configurable-templates.md 旧稿的 H3(禁止剧本对敌施毒 / 冰 / 封)取消。剧本自己不掷骰,抗异常的随机数消耗发生在出手时,与手动放同一技能完全一样;Java 引擎已有抗异常(`BuffEngine`、`EquipmentAilmentTest`),两版逐位一致由跨语言金样钉住(§8)。
- **公开函数**:`TurnBattleEngine::IsTurnBattleCastableSkill`、`LegacySkillManaCost` 改为 public static,剧本校验与出手用同一份判据。
- **热路径**:每回合每队至多 角色位 8 × 单位 10 × 规则 16 × 候选 20 × 筛选 3 次整数比较,不分配大块内存。

## 5. 解析与冻结(Go,match)

### 5.1 官方剧本

`go/match/internal/battlescript`:`official_scripts.json` 用 `//go:embed` 内置(K8s 上 `/app/etc` 被配置卷覆盖,放 etc 读不到),启动时 `LoadWithTables` 读 Skill / Buff / Monster / Dungeon 表后整份校验,不合法 match 拒绝启动(`svc.NewServiceContextWithAdmission` panic,与模板目录同口径)。改官方剧本 = 改这份 JSON + 重新发布 match。真表契约用例 `TestEmbeddedOfficialConfigValidatesAgainstExportedTables`:策划删了被剧本引用的行,这条先红。

### 5.2 每一方用哪份

`runFormationGather` 组建房原件时 `resolveTeamScripts`(`logic/battle_script_gather.go`):

- **怪物方**(PVE 的 1 队):官方绑定(MONSTER)。
- **玩家方**:选用者 = 该方**队长**(整队开局的发起人,`MatchFormationMember.captain`,撮合在成局时按 `TeamLaunchRecord.actor_id` 写入);没有队长(单人、散人匹配)取阵位第一的成员。选用者对这一局的选用(具体 → 通用)> 官方绑定(PLAYER)> 不带。
- **评分局只开官方剧本**(负责人 2026-11-11 定):`isRatedMode` 的模式里,选用中指向玩家剧本的条目当作不存在,继续往通用找官方的选用或绑定;选用接口对明确指定评分模式的玩家剧本直接拒绝(`kMatchBattleScriptLoadoutInvalid`),「任意模式」的玩家剧本照收、只在非评分局生效。
- 选用的玩家剧本取**审核通过版**,并按当前配表再校验;选用失效(读库失败、剧本已删、配表改了以后不再合法)退回官方绑定,记 ERROR 与 `fell_back` 计数,不影响开局。
- 解析只在成局第一次 gather 时做一次:原件整份落库(`SaveSupportedIntent`),恢复重放用的就是落库的那一份。预算 500ms(只读本库),与建房前其它调用一起由 `matchedTicketTTLFor` 的余量吸收。

### 5.3 查找顺序

官方绑定与玩家选用同一口径:(模式, 副本) → (模式, 任意) → (任意, 副本) → (任意, 任意),取第一条。怪物方绑定只许 PVE 模式或任意模式。

## 6. 玩家剧本:存储与审核

### 6.1 表(match 库,`proto/match/match_authority.proto`,由 `match -migrate` 建)

| 表 | 主键 / 索引 | 内容 |
|---|---|---|
| `match_battle_script` | `script_id`;`(owner_player_id, script_id)`、`(review_state, submitted_ms, script_id)` | 当前提交版正文 + 审核状态;审核通过版正文与版本号(建房只用它) |
| `match_battle_script_review` | `(script_id, revision)` | 审核流水:谁、通过 / 驳回、原因、审的那一版正文;只增不改 |
| `match_battle_script_loadout` | `(player_id, match_mode, battle_config_id)` | 选用:官方剧本 id 或本人剧本 id |

`script_id` 向发号器领,与 battle_id 同一号段(`BattleIDBizTag`),事务外领,领不到整体失败。

### 6.2 一致性

- 玩家写入(提交、删除、选用)先锁本人 `role_fence`,同一玩家串行;份数(每人 10 份)与选用条目(每人 32 条)在锁内数,不会并发超限。
- 剧本行加行锁:改写按 `expected_revision` CAS;审核按 (revision, 待审) CAS,作者改了、删了、或已被审过一律 STALE,不会审错版本。
- 不变量:选用里的玩家剧本一定存在、属于本人、至少通过过一次 —— 选用时锁内核对,删除剧本与清掉指向它的选用同一事务。
- 事务里不调 RPC(AGENTS §7.6)。

### 6.3 审核入口与运营工具

- `MatchInternal.ListPendingBattleScripts` / `ReviewBattleScript`:serviceauth 调用方 `ops`,与 `SetSpectateFeatured` 同一把运营密钥(`Spectate.OpsCallerSecret`,K8s 由 Secret `match-ops-caller` 注入);带会话、入口未开、验签失败一律带内 DENIED。请求体不进日志(`IgnoreContentMethods`)。
- 通过前在审核事务里按当前配表再校验一次,已不合法的只能驳回(INVALID)。驳回必须写原因(作者会看到)。
- 工具 `go/match/cmd/battlescriptreview`:`-list` 打印待审剧本的完整正文(protobuf JSON),`-script -revision -approve|-reason -reviewer` 审一版。密钥只从环境变量 `MMORPG_MATCH_OPS_CALLER_SECRET` 读。

## 7. 客户端协议(`MatchService`,只认会话本人)

| RPC | 说明 |
|---|---|
| `ListBattleScripts` | 官方剧本、本人剧本(含审核状态、驳回原因、生效版本)、本人选用、上限 |
| `SubmitBattleScript` | 新建(`script_id = 0`)或改写(带 `expected_revision`);不合法回 `kMatchBattleScriptInvalid`,位置在 `invalid_detail` |
| `DeleteBattleScript` | 带 `expected_revision` |
| `SetBattleScriptLoadout` | 选官方剧本或本人已通过的剧本;`origin = UNSPECIFIED` 清除 |

- **包长**:gate 对所有客户端消息有 1KB 硬上限(`CheckMessageSize`,超限还计非法包),所以单份玩家剧本序列化后不超过 900 字节(`max_script_bytes` 回给客户端,客户端提交前先自查)。参数一律 uint32 也是为此。
- **tip 码**(`match_error` 段,导表器发号):`kMatchBattleScriptInvalid / NotFound / LimitReached / RevisionConflict / NotApproved / TooLarge / LoadoutInvalid / LoadoutFull`;库故障回 `kMatchInternal`。
- 限频:缺省额度(MessageLimiter 无专门行)。

## 8. 两版与对拍

- 本仓 C++ 校验(`battle_script.{h,cpp}`)与 Go 校验(`battlescript/validate.go`)逐条对应,同一批非法写法两边的单测都要拒(`BattleScriptValidationTest.RejectsMalformedScripts` ↔ `TestNormalizeRejectsMalformedScripts`)。
- **Java 版待做**(已登记 PARITY.md):game-battle-engine 的同一求值器与 `fillDefaultActions` 接入;game-match 的解析、存储、四个客户端协议与审核入口;跨语言金样(C++ 导出带剧本对局的事件 hex,Java 回放一致)。客户端契约(`battle_script.proto`、MatchService 新消息、tip 码)两版共享。

## 9. 可观测性

- match:`match_battle_script_resolve_total{side, outcome}`(chosen_official / chosen_player / bound / none / fell_back)、`match_battle_script_request_total{rpc, result}`;label 不带玩家与剧本 id。
- battle:被忽略的剧本逐条 ERROR(`CreateBattle battle script ignored`)。
- 日志只记 script_id、revision、玩家 id,不记正文。

## 10. 改动清单

- 协议:`proto/battle/battle_script.proto`(新)、`battle_node.proto`(`team_scripts = 17`)、`proto/match/match_authority.proto`(`MatchFormationMember.captain = 4` 与三张表)、`match_service.proto`(四个 RPC)、`match_internal.proto`(两个审核 RPC)、`data/tip/Tip.xlsx`(8 个码)。
- C++:`cpp/libs/services/battle/system/battle_script.{h,cpp}`(新)、`turn_battle_engine.{h,cpp}`、`battle.vcxproj(.filters)`、`cpp/nodes/battle/logic/battle_room_manager.cpp`、`cpp/tests/turn_battle_engine_test/battle_script_test.cpp`(新)与工程文件。
- Go:`go/match/internal/battlescript/*`(新)、`internal/authority/battle_script.go`(新)与 `schema.go`、`store.go`、`internal/logic/battle_script_gather.go` / `battlescriptlogic.go` / `battlescriptreviewlogic.go`(新)与 `gather.go`、`global_pool_matcher.go`、`queue.go`(注释)、`internal/server/*`、`internal/svc/servicecontext.go`、`battle_id.go`(注释)、`internal/metrics/battle_script.go`(新)与 `metrics.go`、`internal/constants/errors.go(_test)`、`cmd/battlescriptreview`(新)、`etc/match_service.yaml`。
- 客户端:见客户端仓 `Docs/BattleScriptUI.md`。

## 11. 验证(交 Codex,AGENTS §10.1;任一步红即停,保留首个错误)

1. 导表(tip 码):`dev.bat gen`;核对 `generated/code/proto/tip/match_error_tip.proto` 出现 8 个 `kMatchBattleScript*`,已有码号不变。
2. 协议:`pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run -ConfigPath tools/proto_generator/protogen/etc/proto_gen.no-unity.yaml`(客户端另按客户端文档用带 Unity 的配置生成)。核对生成 `cpp/generated/proto/battle/battle_script.pb.{h,cc}`、`go/proto/battle/battle_script.pb.go`,proto.vcxproj / CMakeLists 已登记新文件,`shared/generated/clientrpc/methods.go` 含四个新方法。
3. C++(串行 `/m:1`):先 battle 库与 battle 节点,再 `turn_battle_engine_test`(测试工程链接预编好的库,改了库要先重编库);全部用例绿,重点新增 18 条:`BattleScriptRulesTest.*`(3)、`BattleScriptValidationTest.*`(3)、`BattleScriptEngineTest.*`(12),以及既有 `turn_battle_engine_test` 全量不变(没有剧本的对局逐字节不变)。
4. Go:`cd go/match && go build ./... && go vet ./... && go test ./internal/battlescript/... ./internal/constants/... ./internal/logic/... ./internal/metrics/...`;`TestEmbeddedOfficialConfigValidatesAgainstExportedTables` 必须真读到 `generated/tables`。成局原件现在给整队发起人带 `captain`:比对成局成员的既有用例如因此变化,按新字段更新期望(不是回归)。
5. 真 TiDB:`MATCH_IT_TIDB=<端口> MATCH_IT_REQUIRE_TIDB=1 go test ./internal/authority/ -run 'TestBattleScript' -count=1`(两条);迁移:`match -migrate` 建出三张新表。
6. 冒烟(本机全栈):用 `battlescriptreview` 与客户端(或 robot 直调)走一遍:提交 → 列表待审 → 审核通过 → 选用 → 打副本 3 与选用的模式,battle 日志无 `battle script ignored`,怪物 / 挂机出招符合剧本;改写后仍用通过版;删除后选用消失。

## 12. 已知限制与待定

1. 选用者是队长;队员在队伍界面手动指派角色位没做(现在按职业等条件自动认领)。要做时在组队界面加「本局角色位」并随成局原件冻结。
2. 剧本不自动用道具(会替玩家消耗资产,要单独的本人确认流程)。
3. 角色删除后其剧本与选用不清理(每人至多 10 + 32 行,不阻塞角色过户;合服 / 删角口径另定)。
4. 客户端编辑器先做「粘贴 protobuf JSON 导入」,可视化逐条编辑另立(客户端文档)。
