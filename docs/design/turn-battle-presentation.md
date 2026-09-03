# 回合制战斗表现规格(问道 / 梦幻西游式演出)v1

> 需求(2026-09-03):参考问道战斗录像,把回合制战斗做成"问道那样"的最终效果;服务器 + 客户端代码实现完整;
> 角色用现有 image(2D 图片)角色,需要的新图由本轮产出(程序化生成 + 生图提示词包)。
> 关联:`turn-based-battle-server.md`(引擎/协议)、`cross-zone-matchmaking.md`(匹配)。
> 客户端现状地图(2026-09-03 调研):战斗 UI 为纯代码 UGUI 色块,单位无图片,事件固定 0.55s 一拍抖动/闪色/飘字;
> 可用资产:1 套 8 方向×8 帧跑步帧条(QdaoHeadbandBoy)、22 张 1024 立绘、4 张已生成未接线的战斗 Fx/入场图、
> FairyGUI 的 GTween(未使用)。

## 0. 目标效果(一句话)

上敌下我两阵交错站位、近大远小;每回合按出手序**逐个演出**:出手者冲到目标前 → 挥击/施法动作 →
命中特效 + 伤害数字弹出 + 受击后仰/闪白 → 回位;群攻一次挥砍多目标同时飙血;暴击顿帧震屏;
死亡倒地渐隐;回合开始/结束有节拍;底栏为问道式命令环(攻击/法术/防御/道具/召唤/逃跑/自动)。

## 1. 参考录像观察(抽帧结论)

录像:36s,2656×1220,10v10 PVE 自动战斗,两个回合。0.5fps 全程 18 帧 + 4fps 局部 24 帧,结论:

- **视角与站位**:固定 45° 等距俯视,场景是 3D 地形 + 2D 单位。敌阵在画面**左上**、我方在**右下**,各自沿
  "右上→左下"的对角线排成**两排交错**(每排 5 个,前排靠中线,后排外侧且略高),单位近大远小、后排被前排轻微遮挡
  (按脚底 y 排序)。整个战场占屏幕中央约 60% 宽度,上下留出 HUD 带。
- **单位**:立绘尺寸约屏幕高的 18-22%(1220 高下约 240px),脚下投影阴影;**头顶**两条细条(红 HP、蓝 MP,宽约单位宽的 60%,
  高 4px,叠放,左侧有小 buff 图标格);**脚下**名字(玩家蓝色、宠物青色、怪物浅蓝,带黑描边);可有聊天气泡(白底圆角,
  上方偏右)。宠物站在主人斜前方同一排。
- **伤害数字**:超大(高约 36-40px)红色粗体、深色厚描边,从受击者头顶上方弹出并**上飘约 60px 后淡出**(约 1.2s);
  群攻时每个目标各自一串,错位不重叠;治疗为绿色 "+N";数字用等宽像素感字体,带轻微缩放弹出。
- **技能演出**:群攻 = 施法者原地施法光效(绿/紫爆裂环)→ 全屏大范围特效扫过敌阵(蓝色雷弧、金色光环)→ 所有目标
  同一拍飙血 + 受击闪白;单体普攻 = 出手者冲到目标身前挥击后回位(录像中被自动战斗节奏带过,约 1s)。
  防御类 buff = 我方每个单位套金色透明护盾泡(持续显示)。特效尺寸大,可覆盖 2-4 个单位。
- **节奏**(4fps 局部帧):群体技能 = 施法者起手光效 0.25-0.4s → 范围特效铺满目标区 0.4-0.6s(雷柱从天而降 / 光环扩散)→
  所有目标**同一拍**受击闪白 + 伤害数字弹出(数字 1.0-1.2s 上飘淡出,多目标错位)→ 特效余烬 0.3s;整段约 1.5-2s。
  单体普攻约 0.8-1.0s。一回合 10v10 约 20-24s;回合切换时左上"第 N 回合"翻数,无额外停顿。
- **受击反馈**:目标整体闪白一帧 + 轻微后仰位移(约 8px,0.15s 回弹);死亡为倒地 + 渐隐(约 1s),尸体不保留。
- **HUD**:左上"第 N 回合"金框 + "战斗记录"眼睛图标;右上两张角色卡(头像 + 等级 149/139 + 红蓝条,自己与队长/宠物);
  右下三键"角色 / 宠物 / 取消自动"(金框方形图标,自动战斗中);左下世界聊天面板 + 语音键 + 好友;手动模式下应为
  命令环(本录像未出现,按问道惯例:攻击/法术/防御/道具/召唤/逃跑/自动,圆形排布在右下)。
- **结算**:录像未包含;按问道惯例做"胜利"大字 + 奖励列表飞入。

## 2. 数据层(服务端 → 客户端)缺口与补齐

现有 `TurnResultS2C.events[]`(BattleEventItem:type/source/target/skill/buff/value/is_critical/success/
target_health_after/item)能表达顺序与数值,缺以下演出必需信息(全部为**增量字段**,客户端旧版本忽略即可):

| # | 缺口 | 补齐(proto) | 引擎侧 |
|---|---|---|---|
| D1 | 闪避/未命中/格挡 | `eBattleEventType` 新增 `BATTLE_EVENT_MISS=12`、`BATTLE_EVENT_BLOCK=13`(预留) | 引擎命中判定产出 MISS 事件(当前无闪避公式:一期按 `dodge = 0`,事件先有语义,数值公式二期) |
| D2 | 群攻/连击分组 | `BattleEventItem.group_id`(同一次施法的所有事件同 id)、`hit_index`(多段攻击序号) | 引擎在一次行动内递增 group_id |
| D3 | 出手顺序 | `TurnResultS2C.action_order[]`(本回合按速度排定的 actor_id 序,含被跳过者) | 引擎回合结算时已排好序,直接透传 |
| D4 | 阵型槽位 | `BattleActorState.formation_slot`(0-9:0-4 前排左→右,5-9 后排) | 引擎按加入序/队伍分配,怪物按 DungeonTable 怪物组序 |
| D5 | MP 变化 | `BattleEventItem` 新增 `BATTLE_EVENT_MANA=14`(value=变化量,负为消耗) | 技能耗蓝时产出 |
| D6 | 演出提示 | 客户端表数据(技能→特效 id/动作类型/命中帧,怪物→外观 id)走**客户端表管线**,不进协议 | 导表工具增加客户端 json 导出 |

以上属"表现数据",不影响判定与结算;字段编号只增不改。

## 3. 客户端表现层架构

```
BattleClient(纯逻辑,已有) ──OnTurnResult──▶ BattlePresenter
                                              ├─ TurnPlan 生成器(纯 C#,可 EditMode 测):events[] → 演出指令序列
                                              │    · 合并 ATTACK+DAMAGE(+MISS) 为一拍;同 group_id 的 DAMAGE 并行
                                              │    · 事件类型化时长:攻击 0.9s / 技能 1.4s / buff_tick 0.25s / 死亡 1.2s
                                              ├─ BattleStage(阵型):10 个槽位的世界坐标(上敌下我,梯形交错,近大远小 0.85~1.0)
                                              ├─ BattleUnitView:Image 角色 + 动作机(idle/attack/cast/hit/die/win)+ 名字牌/血条/buff 行
                                              ├─ BattleFx:特效播放器(序列帧 Image / 程序化粒子),按 target/source/中点定位
                                              ├─ DamageNumber:对象池,弹出缩放 + 上飘 + 淡出,暴击放大描边
                                              ├─ Camera/Screen FX:震屏、暴击顿帧(realtime 0.08s)、黑边
                                              └─ Sequencer:基于 GTween(Plugins/FairyGUI/Runtime/Tween)的并行轨 + 回调,可中断(观战抢占)
BattleScreen(已有):改为只负责命令环/回合计时/目标选择;单位槽换成 BattleUnitView
```

- **动作机来源**:现有跑步帧条按方向取 S/E/W 帧做 idle(呼吸缩放 ±2%)与"冲刺"(E/W 帧循环);攻击/受击/施法/死亡
  用**位移 + 缩放 + 闪白 + 残影**的程序化动作(无需新帧);若 `Resources/Battle/Characters/<id>/{attack,cast,hit,die}_strip.png`
  存在则自动切换为帧动画(契约见 §5)。
- **怪物**:按 `MonsterTableId` 找 `Resources/Battle/Monsters/<id>.png`,缺图用程序化"剪影 + 名字"占位。
- **资源路径契约**均集中在 `BattleArtCatalog`(一处),缺图不报错。

## 4. UI(问道式)

- 底栏命令环:攻击 / 法术 / 防御 / 道具 / 召唤(预留)/ 逃跑 / 自动,圆形排布,当前可用高亮,PVP 逃跑置灰。
- 回合计时:环形倒计时 + 数字;行动预告条(§2 D3)在顶部横向显示头像序列。
- 单位名牌:名字/等级/血条/蓝条 + buff 图标行(表驱动图标,缺图用字母块)。
- 结算:胜/负/平大字入场 → 奖励逐条飞入(经验/金钱/道具)→ 确认。

## 5. 美术契约与产出计划

### 5.1 本轮程序化生成(Go `tools/battle_art_gen`,直接出 PNG)
- 技能特效序列帧:斩击弧、刺击线、火球/冰锥/雷击(3 系各 1)、治疗光圈、buff 上升光柱、命中星爆、死亡消散 —— 8 帧 256×256 横条。
- UI:九宫格面板/按钮底板/血条槽/命令环底、伤害数字字集(0-9 + "-" "+" "闪" "暴击",两套颜色描边)。
- buff 图标 24 个(增益/减益/控制三色底 + 简单符号)。
- 出生光环已有(spawn_ring_friendly/enemy)、入场云层已有 —— 接线。

### 5.2 需要生图模型的资产(提示词包,沿用 walk v1 的规格与抠图/打包流程)
- 角色战斗动作(每角色):idle 4 帧、attack 6 帧、cast 6 帧、hit 3 帧、die 6 帧、win 4 帧,**只做 E 与 W 两向**(战斗侧视),
  512×512 单格横条,chroma-magenta 背景,pivot (0.5, 0.08)。
- 怪物:首批 6 只(DungeonTable 首几组用到的 MonsterTable id),各 idle 4 + attack 6 + hit 3 + die 6,E/W 两向。
- 提示词与命名契约见 `docs/design/battle-art-prompts.md`(本轮产出)。

## 6. 验证

- EditMode:TurnPlan 生成器用例(合并/分组/时长/中断)。
- 播放器:robot 对手 + 自动驾驶客户端跑一局,录屏(Unity Recorder 不在包里 → 用 `-screenshotEvery` 命令行截图序列)对照本规格逐条勾。

- 服务端数据层(2026-09-03 已验证,`cpp/tests/turn_battle_engine_test`):
  - `PresentationGroupIdSharedWithinActionDistinctAcrossActions`:同一行动的 ATTACK+DAMAGE 共用非 0 `group_id`,跨行动不同;单目标 `hit_index`=0;基础命中率 100 不产出 MISS;`LastActionOrder()` 为速度序(battle 节点原样填进 `TurnResultS2C.action_order`)。
  - `PresentationFormationSlotIncrementsPerTeamInSnapshotOrder`:同队 `formation_slot` 按快照顺序 0.. 递增,怪物队独立从 0 起。
  - `PresentationSkillManaCostEmitsManaEventInSkillGroup`:耗蓝技能产出 MANA 事件(value=消耗、`target_mana_after`=剩余、与 SKILL 同 group),状态快照法力同步扣减,无耗蓝技能不产出。
  - 引擎测试合计 50/50 全绿;客户端侧 `TurnPlan` 直接消费上述字段,旧服务端(无 group_id)按"紧随其后的 DAMAGE 归入前一个 ATTACK/SKILL"回退。
## 7. 与二期并行线的关系

- 服务端 §2 的 proto 增量在 `phase2-cpp-scene-battle` 线完成后再改(避免与其同时改 battle 引擎/regen)。
- 客户端表现层在 `unity-crosszone-autopilot` 线完成后开工(BattleUiStyle/Bootstrap 有重叠)。
