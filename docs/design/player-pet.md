# 宝宝(宠物)系统 —— 核心线

> 落地日期:2026-09-09(**未编译,待 Codex 验证**,见 §9)。实现分布:
> `proto/common/component/player_pet_comp.proto`、`proto/scene/player_pet.proto`、
> `proto/battle/battle_data.proto`(宝宝快照 / 参战单位 / 结算)、
> `cpp/libs/services/scene/player/system/player_pet.{h,cpp}`、
> `cpp/libs/services/scene/player/system/pet_rules.h`(纯规则)、
> `cpp/nodes/scene/handler/rpc/player/player_pet_handler.cpp`(守护段)、
> `cpp/libs/services/battle/system/turn_battle_engine.cpp`(`InitPets`)、
> 策划表 `data/Pet.xlsx` / `data/PetRule.xlsx` + `AttributePool/AttributeDimension/AttributeAutoPlan`
> 中 `owner_type=1` 的新行。

## 1. 目标与非目标

**目标**(用户 2026-09-09 选定的"核心线"):宝宝数据与携带槽位、四个一级属性
(体质 / 灵力 / 力量 / 敏捷)+ 资质、按宝宝等级发点的手动加点与自动加点、
召唤 / 收回、以及**作为独立行动单位参加回合制战斗**。

**非目标(核心线不做,二期见 §8)**:战斗中捕捉、宠物店购买、合宠、洗资质 / 洗成长、
放生、宝宝技能书学习、宝宝独立经验曲线、宝宝的服务器权威跟随位置(AOI 实体)。

## 2. 与角色属性加点的关系:复用同一套表和同一套纯规则

用户明确选择"复用角色那套"。落地方式是给 `AttributePool` 加一列 **`owner_type`**
(0 = 角色,1 = 宝宝),两边共用同一份表、同一份 `attributerules`:

| 复用的东西 | 在哪 | 宝宝这边怎么用 |
|---|---|---|
| 点数总量换算 | `attributerules::TotalPoints` | 传**宝宝等级**而不是主人等级 |
| "目标已分配"校验(只增不减 / 单项上限 / 不超剩余) | `attributerules::ValidateAllocation` | 逐字节同一套 |
| 自动加点分配 | `attributerules::DistributePoints` | 同一套,方案取 `AttributeAutoPlan` 里 `class_id=0 && pool_id=宝宝池` 的行 |
| HP/MP 随上限变化按比例保持 | `attributerules::RescaleCurrent` | 同一个函数(本次把角色侧的私有副本提到共享头,两边口径不会再分叉) |
| 维度系数矩阵 | `AttributeDimension` 的六列 | 新增 401-404 四行,`pool_id=4` |

**互不可见是强制的**:`PlayerAttributeSystem` 的面板与三个写入口(Allocate / Reset /
AutoAllocate)现在都过 `IsPlayerPool()`,拿到宝宝池一律按"池不存在"拒绝;
`Recalculate` 也跳过宝宝维度。反过来 `PetSystem` 只认 `owner_type=1` 的池。
不加这道闸的话,角色面板会凭空多出一个"宝宝属性点"池,而角色的加点接口能直接改宝宝的维度。

## 3. 计算口径

### 3.1 数据分层(与角色同构)

| 层 | 载体 | 是否落库 |
|---|---|---|
| 宝宝等级 | `PetInstance.level` | ✅(派生自主人等级,存下来只为离线展示与结算夹取) |
| 已分配点 | `PetInstance.allocated` | ✅ |
| 资质 | `PetInstance.aptitude` | ✅(出生随机,终身不变) |
| 当前 HP/MP | `PetInstance.health / mana` | ✅(残血跨登录保留) |
| 点数总量 | 无 | ❌ 按宝宝等级 + `AttributePool` 实时算 |
| 二级属性 | 无 | ❌ **每次现算**(`PetSystem::ComputeDerived`) |

二级属性连缓存都没有:宝宝数量以 `PetRule.max_pets` 计(默认 10),现算成本可忽略,
换来的是"已分配点 + 资质 + 主人等级"三个输入之外**没有第二份真相**。

**上线前置:DB 新列**。`player_database.pet_component`(字段 12)是一列新的 MEDIUMBLOB,
与 `attribute_component` 当初同一个坑:proto2mysql 的 SELECT/REPLACE 枚举 descriptor 全部字段,
存量库缺这一列时**整行玩家数据读写都报 Unknown column**,不只是宝宝丢失。部署前必须
`cd go/db && go run ./cmd/migrate -command up`;`go/db/model`、`go/login/model`、
`go/player_locator` 三份 `mysql_database_table.sql` 已同步加列。

### 3.2 等级:跟随主人,不设第二个等级真相

```
宝宝等级 = min(主人等级, PetTable.level_cap)
```

经验系统全仓未接(角色等级也只能 `GmSetPlayerLevel`),给宝宝单开一条经验曲线就是造第二份
"等级从哪来"的真相。因此核心线让宝宝等级完全派生:主人升级 → `PlayerUpgradeEvent` →
`PetSystem::RecalculateAll(kLevelChanged)` → 推列表。宝宝的独立经验是二期(§8)。

### 3.3 一级 → 二级

```
维度值(面板显示) = AttributeDimension.base_per_level × 宝宝等级 + 已分配点
二级属性 = PetTable 初值(init_health / init_mana / init_speed)
           + Σ(维度值 × 该维度系数 × 资质[该维度] / 10000)
```

**资质只放大贡献,不放大面板上的维度值** —— 玩家看到的"力量 50"是实打实的 50 点,
资质高低体现在这 50 点换来多少物伤。这样"我加了 5 点力量,物伤涨了多少"始终线性可预期,
而不同资质的同种宝宝差在斜率上。

**"成长率"是派生展示量**,`= 四维资质的算术平均`,由服务器算好塞进 `PetInfo.growth`,
**不落库**。存第二份就会和 `aptitude` 分叉,而它只是给玩家一眼看的概括数。

当前 HP/MP 随上限变化的规则与角色**逐条对齐**(`PetSystem::RecalcReason`):
- **升级**(`kLevelChanged` 且等级真的涨了):上限抬高按绝对增量补当前值;降级只夹。
- **加载 / 加点 / 洗点**:按比例保持 `hp × newMax / oldMax`(活着至少留 1)。
  不这么做的话"洗点降上限 → 再把点加回来"就是宝宝的免费回血,和角色那条 2026-09-04
  被堵掉的白嫖路径一模一样。
- **已分配 > 总量的收敛**:主人降级(GM / 回档)或改表缩点后整池清零返还并打 WARN。

## 4. 协议(`proto/scene/player_pet.proto`,service `ScenePetClientPlayer`)

| rpc | 语义 |
|---|---|
| `GetPetList` | 拉全量列表(客户端零配表:维度名 / 说明 / 上限 / 剩余点 / 资质 / 成长率全在里面) |
| `SummonPet` / `RecallPet` | 出战 / 收回(同时只能出战 1 只) |
| `AllocatePetPoints` | 确认加点:提交该宝宝的"目标已分配值"(全量、幂等) |
| `ResetPetPoints` | 洗点,按 `AttributePool.reset_cost_gold` 扣金币 |
| `AutoAllocatePetPoints` | 算推荐分配,**只算不落** |
| `RenamePet` | 改名,按 `PetRule.rename_cost_gold` 扣金币 |
| `NotifyPetListChanged` | 服务器主动推列表(主人升级 / 结算回写) |
| `GmGrantPet` | GM 发宝宝(核心线唯一获取入口;上线走 GM 鉴权白名单) |

**写口径与角色一致**:每个写操作成功后回全量列表,客户端整体覆盖,不做增量合并。
所有写操作在**跨 zone 冻结(`PlayerFrozenComp`)与战斗在途(`InBattleComp`)期间一律拒绝** ——
快照已经出去了,这时候改宝宝会让局内单位和面板分叉。

**消息号与限流**:消息号由导表/生成流程发号(`proto/message_id.txt`,预计 180+)。
新协议上线前必须给 `data/MessageLimiter.xlsx` 加行,否则吃默认档(3 次/窗口),
UI 连点第 4 个包就 `kRateLimitExceeded(1008)`,现象是"请求无响应"而不是错误提示。
改表后要重启 gate。**这一步依赖生成后的实际号,见 §9 交接清单第 3 条。**

## 5. 战斗:宝宝是独立行动单位

用户选定"独立参战单位"。实现只在三处接缝:

1. **scene 组快照**:`BuildBattleSnapshot` 在主人快照里挂 `BattlePlayerSnapshot.pets`
   (repeated,核心线恒为 0 或 1 只)。属性逐项对齐玩家快照:`base_attributes` 带 speed
   与当前 HP/MP,`max_*` 与物伤 / 法伤 / 防御来自现算的二级属性。
   **死宝宝(health==0)不参战** —— 带进去就是开局即倒的空单位。
2. **引擎 `InitPets`**:在 `InitPlayers` 之后追加(阵位按插入序,主人先站前排),
   `actor_id = pet_id`。**pet_id 沿用 item 身份域,与 player_id 的独立域可能数值相交**。
   scene 经 `tlsGuidSegmentRegistry.Get(GuidKind::kItem).TryNext` 从 item 号段取得 pet_id,
   不再依赖 SnowFlake 发号器;两种身份域仍不能假设数值互斥,
   所以 `InitPets` 显式查重并在相撞时**拒绝开局**(fail-closed),而不是假设不会撞;
   同理宝宝的归属只认所在快照的 `player_id`,快照自带的 `owner_player_id` 只用于对账,
   `team_index` 跟随主人,`actor_type = BATTLE_ACTOR_TYPE_PET`。
3. **结算**:`BuildSettlement` 把该主人名下的宝宝终值塞进
   `BattleSettlementData.pets`;scene 的 `ApplySettlementToEntity` 按现算上限夹后落实例。

**宝宝没有客户端行动权**,这是核心线最重要的一条取舍:

- `is_auto = true`,每回合由 `FillDefaultActions` 代打普攻;
- `SubmitAction` 本来就只收 `BATTLE_ACTOR_TYPE_PLAYER`,`SetActorAuto` 同样只认玩家 ——
  所以客户端既不能指挥宝宝,也不能把它的自动关掉;
- `AllPlayersReady` 只数玩家单位,**带宝宝不会拖慢任何一个回合**。

代价:玩家不能给宝宝下指令(问道里可以)。指挥宝宝需要一条"主人代宝宝提交行动 + 归属校验"
的新路径,留给二期(§8)。确定性不受影响:默认行动路径不额外消耗 RNG 分支。

已知语义(有意为之,不是缺陷):
- 主人逃跑 / 阵亡后宝宝仍在场,直到本方全灭 —— `SideWiped` 数的是全部单位;
- 宝宝阵亡后由结算回满(`ApplyBattleSettlement`)。不回满的话 0 血宝宝会被
  `BuildBattleSnapshot` 一直挡在门外,玩家再也带不出去且没有任何提示。

## 6. 表

- **`Pet`** —— 种类:`name` / `desc` / `model_id` / `quality` / `unlock_level`(可携带的最低主人等级)/
  `level_cap` / `init_health` / `init_mana` / `init_speed` / `aptitude_min[4]` / `aptitude_max[4]` / `skill[4]`。
  资质区间按维度顺序 401 体质 / 402 灵力 / 403 力量 / 404 敏捷 排。当前四行:灵狐(快 / 法)、
  石灵(肉)、金猊(物理)、云鹤(法系)。
- **`PetRule`** —— 单行全局:`max_pets` / `name_max_len` / `rename_cost_gold` / `summon_cooldown_seconds`。
- **`AttributePool` 新增行 4**:宝宝属性点,1 级解锁、每级 5 点、`owner_type=1`、洗点 300 金、30 级以下免费。
- **`AttributeDimension` 新增 401-404**:宝宝的体质 / 灵力 / 力量 / 敏捷,系数比角色低一档
  (体质 25 血 + 1.5 防 / 灵力 15 蓝 + 2.5 法伤 / 力量 4 物伤 / 敏捷 2.5 速度)。
  ⚠ 2026-09-10 角色系数上调(体质 50 血 + 5 防 / 灵力 10 蓝 + 40 法伤 / 力量 50 物伤 / 敏捷 3 速度)后宝宝未同步,
  "低一档"已不成立:宝宝单点物伤 4 对 50、法伤 2.5 对 40,只有主人的约 1/12~1/16。是否同比上调待策划决定
  (`player-attribute-allocation.md` §8)。
- **`AttributeAutoPlan` 新增行 4**:宝宝通用方案(力量 3 : 体质 2 : 敏捷 1)。
  **刻意不投灵力**:核心线宝宝只普攻(`PetTable.skill` 全 0),灵力给的法伤与法力上限
  对它零收益,推荐方案往那里投点等于白扔。宝宝接技能书之后要把 402 加回来。
- **`Tip` 新增组** `//pet_error base=26000 width=1000`,17 个码。

**加一种宝宝 = 加一行 `Pet.xlsx`,不改任何代码**;
**改宝宝的属性收益 = 改 `AttributeDimension` 的 401-404 四行,不改任何代码**。

## 7. 不变量

1. `PlayerPetComp` 的任何写入只经 `PetSystem` 的静态函数,每次写完都 `RecalculateAll`。
2. `pet_id` 只由 scene 经 `tlsGuidSegmentRegistry.Get(GuidKind::kItem).TryNext` 铸,
   沿用 item 号段,且**铸号成功后才新增宠物**。号段未就绪或已耗尽等发号失败时
   返回 `kPetIdGenerateFailed`,不新增任何宠物。
3. 宝宝的二级属性任何时候都不落库、不缓存。
4. 战斗在途 / 跨 zone 冻结期间拒绝一切宝宝写操作。
5. 种类行缺失(改表 / 删行)时**保留实例**、召唤时拒绝、日志 WARN ——
   玩家资产不能因为策划删了一行表就悄悄消失。

## 8. 已知缺口 / 二期

1. **获取渠道只有 GM**(`GmGrantPet`):战斗中捕捉、宠物店购买未做。
   `PetSystem::GrantPet` 就是为这两条路准备的公共入口,接的时候不需要改内部逻辑。
   **`GmGrantPet` 今天没有任何鉴权**,任何客户端都能自助发宝宝 —— 这不是本系统独有的口子,
   既有的 `GmSetPlayerLevel` / `GmAddCurrency` 同样敞着,gate 侧目前不存在「按消息号的 GM 白名单」
   (`gate_security.h` 管的是带签名的 GM admin RPC,不覆盖客户端 GM 消息)。
   上线前必须三条一起收口,单独堵宝宝这条没有意义。
2. **宝宝不可指挥**:见 §5。
3. **宝宝没有服务器权威位置**:召唤只是一个状态位 + 下发给客户端,场景里不生 AOI 实体,
   跟随表现由各自客户端推算。别的玩家看到的宝宝位置因此不是权威的。
4. **合宠 / 洗资质 / 洗成长 / 放生 / 技能书**:全部未做。资质字段已经就位,洗资质接上去
   只需要一条改 `aptitude` 的写入口。
5. **宝宝独立经验与升级**:见 §3.2。
6. **`summon_cooldown_seconds` 表列已在,但核心线未实施冷却**(召唤/收回是纯状态位切换,
   没有可被刷的副作用)。接跟随实体后需要补。
7. **宝宝在场景里没有跟随实体**:见缺口 3,客户端自行在主人身边挂模型。
8. **反复跑冒烟会把槽位塞满**:核心线没有放生,`pet_smoke` 每跑一次多一只宝宝;
   脚本在满槽时自动改为复用既有宝宝(所以永远可重复),但 `GmGrantPet` 那条断言会被跳过。
   接了放生之后应改成"跑完自己收拾"。

## 9. 交接给 Codex 的验证清单(§10.1)

Claude 未执行任何编译 / 测试命令,以下全部**未验证**:

1. **导表**(必须最先跑,后面全都依赖它):`dev.bat gen`。
   产出应包含 `pet_table` / `petrule_table` 的三语言管理器、
   `attributepool_table` 多出 `owner_type` 列与索引、`pet_error_tip` 的 pb。
   失败多半是 xlsx 列与 `data/schema/*.proto` 对不上,导表器会指出是哪一列。
   跑完补 `py tools/data_table_exporter/tools/gen_schema_index.py`。
2. **proto 生成**:按本机现行流程重生成 proto 产物 —— Go 侧走 `cd go && build.bat`,
   `proto/message_id.txt` 的发号与 scene handler 骨架由 `tools/proto_generator`(proto-gen)产出。
   **判据看产物而不是命令**:`message_id.txt` 出现 9 个 `ScenePetClientPlayer*` 新号、
   `cpp/nodes/scene/handler/rpc/player/player_pet_handler.{h,cpp}` 被重写且守护段内容仍在。生成器会重写
   `cpp/nodes/scene/handler/rpc/player/player_pet_handler.{h,cpp}` 与
   `player_service_interface.cpp` —— 守护段(`///<<< BEGIN WRITING YOUR CODE`)里的实现会被保留,
   若生成后守护段内容丢失请立刻停下报告。
3. **限流表**:第 2 步产出 `message_id.txt` 后,把 `ScenePetClientPlayer*` 的 9 个号
   加进 `data/MessageLimiter.xlsx`(建议:读类 10 次/秒,写类 5 次/秒,tip 1000),
   再跑一次 `dev.bat gen`,重启 gate。**漏了这步的现象是"点了没反应",不是报错。**
4. **DB 迁移**:`cd go/db && go run ./cmd/migrate -command up`,确认
   `player_database` 多出 `pet_component` 列。
5. **C++ 编译**(MSBuild 必须串行 `/m:1`):`table` → `scene`(lib)→ `scene`(node)→
   `battle` → `turn_battle_engine_test`。
6. **单测**:`build/cpp/tests/turn_battle_engine_test.exe`,重点看新增三条
   `PetJoinsOwnerTeamAndActsWithoutClientAction` /
   `PetIsNotControllableByClient` / `PetFinalStateGoesIntoOwnerSettlement`,
   以及既有全部用例仍为绿(宝宝改动动了 `Initialize` 与 `BuildSettlement`)。
7. **robot 端到端冒烟**:`cd robot && go build ./...` 后
   `robot.exe -c etc/pet_smoke.yaml`(账号 robot_9102,12 步,通过打 `PET_SMOKE_OK`)。
   **必须在第 3 步限流表补完并重启 gate 之后跑**,否则第 4 个包起就吃默认限流(3 次/窗口),
   现象是"等响应超时"而不是拒绝。冒烟里 `pool-isolation-char` 那两条断言守的是
   §2 的 owner_type 分流(角色面板不得出现宝宝池)。
8. **客户端**(独立仓库 `../mmorpg-client/`,已获用户 2026-09-09 授权改动):
   `pwsh -File tools/gen_proto.ps1`(已把两个 pet proto 加进列表)+
   `pwsh -File tools/gen_messageids.ps1`(要在服务端 `message_id.txt` 发号之后跑),
   再 `pwsh -File tools/client_compile_check.ps1`;EditMode 用例
   `Assets/Tests/EditMode/Battle/PetClientTests.cs`(11 条)。
   注意 Unity 命令行跑测试**不能带 `-quit`**(`-runTests` 自己会退出)。
9. **手动端到端**(最后的观感确认):登录 → HUD 右侧「宝宝」入口 → GM 发一只 →
   看资质/成长率/剩余点 → 自动加点 → 确认 → 出战 → 排一场 PVE,
   确认战报里有宝宝的普攻事件、战后面板里宝宝血量已回写。
