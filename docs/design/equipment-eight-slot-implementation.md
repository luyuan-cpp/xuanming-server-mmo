# 八部位装备：代码接入与验证

更新：2026-11-17。对应[养成与接入策划](equipment-eight-slot-planning.md)及[词条/法宝规格](equipment-v1-affixes-artifacts.md)。

首批打通服务器与客户端的槽位、实例和指定替换链路；第二批接入V1基础装模板与独立强化规则，供新UI后续直接调用。新界面图片、页面布局、打造、词条安装与法宝战斗效果另行接入。

八部位属于同一个 `PlayerBagsComp.bags[kEquipment]` 装备背包，复用 `ItemStore`、固定槽 `layout` 与放入规则。实例只记录物品身份和养成；布局记录槽位占用；准入规则按 `EquipSlot` 接受对应 `equip_kind`。没有八个独立背包，也不把左右槽位写进物品实例。2026-11-17 用户再次明确这一分层。

## 已落地的行为

| 可见位置 | 服务端slot | 接受的equip_kind |
|---|---:|---:|
| 武器 | 3 | 11 |
| 帽子 | 4 | 12 |
| 衣服 | 5 | 13 |
| 鞋子 | 6 | 14 |
| 首饰一 | 7 | 15 |
| 首饰二 | 8 | 15 |
| 法宝一 | 9 | 16 |
| 法宝二 | 10 | 16 |

- `EquipSlot.xlsx`追加四行，保留旧0—6行。0—2为无名称的旧夹具槽，空着不作为正式UI槽下发；原有物品占用时仍可显示并卸下。
- 两首饰位同类型，两法宝位同类型。穿戴按`item_id`实例操作，不按名称或模板唯一去重。同名不同实例可以各占一个槽，同一实例不能占两槽。
- `EquipItemRequest.target_slot`为新增的可选字段2；存在时只操作指定槽，0是合法显式值。容量外或不匹配类型在任何搬运前拒绝，使用既有`kEquipNoSlot`。
- 不传目标槽维持旧行为：按槽号先找空槽，全部占用后替换最小槽。RPC顺序、已有消息号和响应结构不变。
- 新角色装备容量至少11。旧存档还原先补足到`max(原容量,11,正式具名槽最大id+1)`，不缩容、不重排原位置，不改装备属性、强化或宝石。
- 成功仍回人物背包和装备栏全量快照，客户端沿用原有在途状态、失败处理与属性更新链路。

## 客户端给后续UI的入口

客户端仓库：`D:\luyuan\mmorpg-client`。

- `PlayerFeaturesClient.EquipItem(ulong itemId, uint targetSlot)`：明确目标槽；旧`EquipItem(ulong itemId)`保留默认选槽。
- `EquipBagModel.Slots(equipment)`：读取服务器槽定义及占用关系，构造所有可见槽。
- `EquipBagModel.FindEquipSlot(equipment,targetSlot)`：查找容量内、服务端实际定义的槽。
- `EquipBagModel.EquipCandidates(characterBag,equipment,targetSlot)`：列出人物背包内类型匹配的实例，保留同名同模板的多件，排除已穿实例；不在客户端复制人物等级/职业等最终校验。

UI点首饰二时保存目标slot8，选择候选实例后调用带目标槽重载。列表的选择键使用`item_id`，不可使用名称、config_id或数组下标。左右位置与美术布局的映射可以调整，协议slot不随画面位置改变。

本批未替换既有屏幕、美术、场景或Prefab；新UI完成后连接这些入口并消费服务器快照。服务器和客户端协议应成套发布；旧服务器会忽略新字段，因此不能在旧服务端上宣称已经支持指定第二槽。

## 本批尚未开放的内容

- 新版定向打造/词条安装的权威预览、材料扣除、创建与持久化防重。
- 36项词条的正式配置/效果；当前尚无正式kind15/16物品模板，槽位就绪不等于法宝可兑换。
- 八件法宝的战斗触发、状态和回放；同名双件的叠加与次数规则仍须补齐。
- 新UI、实际服务器部署、联网端到端和支付。当前构建/离线用例不替代线上验收。

后续顺序：定向打造与首次装词条 → 词条与法宝战斗效果 → 新UI绑定与联网验收。版本化模板、V1强化及基础属性已由本批接入；没有给未实现的写操作新增可扣款的假接口。

## 第二批：V1 本体与独立强化 profile

用户已授权本批包含生成同步的 70—110 文件范围。源码与生成同步已完成，运行验证结果见下节；本批不等于打造、36项词条与法宝战斗已经完成。

- `Item.equip_profile` 新增字段15：0保留旧规则；1为V1基础装；2为V1首饰；3为V1法宝。profile描述养成规则，不代替 `equip_kind`、不决定布局或目标槽。未知profile不回退旧规则。
- 本批只新增四基础装九档共36个模板，等级为1/10/20/30/40/50/60/70/80，ID为3101—3184中每十位的尾号1—4；运行时不按ID区间识别规则。旧Item行保持。新强化材料3002「百炼石」与旧强化石2001分开，不自动兑换、互扣或开放商城。
- `EquipEnhance.profile_id=8`、`level=9`：旧0号profile保持主键1—12及原费用/概率/加成，`level=0`仍按旧主键读等级。新1号profile用独立行1001—1010和显式等级1—10；每级必成、无金币、原始可强化项每级+200万分比，费用2/3/4/5/6/7/9/11/13/15，单件75石。
- 属性汇总、详情和下一级预览共用基础项规则：V1只放大攻击/防御，不放大速度、词条或宝石。旧装备全部基础项保持旧算法。首饰、法宝profile强化上限为0；本批没有为尚未接入效果的首饰/法宝制作空成品模板。
- V1新实例带有效的空 `equip`，即使误配随机池也不掷旧词条；重登不补条。已有实例的 `equip` 不重写。新打造与词条安装稍后走独立权威写入口。
- V1宝石开放规则未定，本批不沿相同kind自动开放新镶嵌、不显示空孔；已有宝石记录的效果、显示与取下保留，旧装备镶嵌不变。
- 客户端继续调用既有 `EnhanceEquip`，消费 `enhance_max_level`、`enhance_next` 和逐基础项当前/下一值，不复制2%公式、材料预算或profile判定。新UI、Prefab和场景未修改。

正式打造下一批必须新增严格扣足与成品创建的原子入口：当前 `ConsumeInventoryAndGrant` 为战斗结算按实际可扣量夹取，不能直接当兑换。拟用独立持久 `forge_revision` 配合最近结果缓存：缓存未命中仍比较预期revision，成功与扣料/造物同帧递增，防止32条结果淘汰后重造。此方案尚未落码，不宣称已有永久防重能力。

## 验证记录

首批服务端证据位于`build/verification/equipment-foundation-20261117/`：`bag_test-build-r3.log`编译及工程静态检查通过，`cpp-tests-r2.json`六套共285项全部通过。首次282/285结果保留；两项handler用例改为验证成功没有错误字段，另将已删除顶层背包的陈旧用例改为仍支持的旧ItemEntry格式，保留四件装备、属性和血蓝断言。

首批客户端证据位于`.codex-artifacts/equipment-eight-slot-20261117-032855/`：484个运行时源文件离线编译通过，新增16/16，关联回归273/274；唯一未通过项依赖Unity Test Runner日志作用域，离线runner报`No log scope is available`，未跳过或削弱断言。工程静态检查21994文件、0错误、6个既有类名警告。未执行Unity编辑器或联网验收。

第二批服务端证据位于`build/verification/equipment-v1-profile-20261117/`：table/modules/battle/scene库与bag_test按Debug x64、MSBuild `/m:1`串行构建通过，包含工程的no-raw-pointer-member检查；`cpp-tests.json`七套302项全部通过，其中V1新增17项。覆盖36模板、四基础装各75石必成、零金币、满级/材料不足/旧请求拒绝、鞋速度、旧0—12级数值、同名双槽、空词条存档和坏表拒绝。构建中的既有第三方与Protobuf警告保留。

scene与battle节点也按Debug x64串行编译链接通过；仅关闭工程PostBuildEvent中的部署复制，工程静态检查照常执行。产物留在`build/cpp/nodes/`，构建前后`bin/scene.exe`和`bin/battle.exe`的SHA256保持一致，未启动或重启服务。

Go共享表`go test -mod=readonly ./generated/pb/table ./generated/table`、robot vendor绑定`go test -mod=vendor proto/scene shared/generated/pb/table shared/generated/table`通过；Java config_node `mvn -o test`编译421个源文件通过，该工程无测试源码。相应日志及命令退出码保留于同一证据目录。数据生成校验在`build/verification/equipment-v1-tables-20261117/final-validation.json`：旧77物品/12强化行及样式保留，JSON与PB一致，单件75石/四件300石，manifest中其余47张表保留。

客户端最终证据位于`.codex-artifacts/equipment-v1-enhance-final-20261117/final-validation-summary.json`：同步生成表后的484个运行时源文件离线编译通过，新9项V1与原16项指定槽用例合计25/25通过，487个编译/测试输入哈希与最终源码一致。上述首批273/274的Unity日志作用域限制仍然保留，未执行Unity编辑器或联网验收。

独立Java服务器未在本批范围内实施，版本号未登记为已对齐。本仓`java/config_node`共享表投影的生成/编译不代表独立Java玩法已支持。
