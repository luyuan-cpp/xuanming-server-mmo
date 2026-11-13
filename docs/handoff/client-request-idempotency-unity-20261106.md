# 交接：客户端请求幂等键——Unity 客户端（交 Codex 实施，2026-11-06）

**授权：** 负责人 2026-11-06：「unity 你可以出细节让 codex 去写」。由 Codex 修改客户端仓 `D:\luyuan\mmorpg-client`（服务端仓的同级目录 `../mmorpg-client/`）。Claude 只读过客户端仓，没有改动。
**服务端契约：** [client-request-idempotency.md](../design/client-request-idempotency.md) §2、§6。服务端三处已落码，2026-11-08 Codex 跑过设计稿 §8 的单测（`bag_test` 932 条通过、0 条失败，`cross_zone_test` 458 条通过，见设计稿状态行）。
**执行状态（2026-11-13 核对）：** §0~§7 的 Unity 改动 2026-11-12 已在客户端仓 main 落码，未生成协议、未编译、未运行（`docs/PROGRESS.md` 2026-11-12「客户端·强化期望等级与幂等键、临时格取回、进场失败后换角」条）。§8 的 robot 三个场景 2026-11-13 已改完，未编译、未运行（`docs/PROGRESS.md` 2026-11-13 条目）。§9 没做。
**客户端现状（2026-11-06 读码，客户端仓 `b624bad2c`）：** 三个调用点都已经有单飞，回包没到时再点不会发第二个包。但请求里都没带键，结果不明时也只报错、重拉或锁住，从不重发。强化请求还没有填 R2 会话 2026-11-05 加的 `expected_level`。

## 0. 先做

1. 在客户端仓执行 `git status`，查看别的会话有没有未提交的改动。下面每个文件都只改列出的那几处，不覆盖别人的改动。
2. 服务端仓先按 `docs/PROGRESS.md` 2026-11-06「客户端请求幂等键」条目完成导表和协议生成。然后用**默认配置**（会写客户端仓）再生成一次 C#：在服务端仓根执行 `pwsh -File tools/scripts/dev_tools.ps1 -Command proto-gen-run`，不带 `-ConfigPath`。生成后核对客户端仓的 `Assets/Scripts/Proto/Generated/`：
   - `PlayerBag.cs` 有 `EnhanceEquipRequest.ClientRequestId`、`ExpectedLevel` 和 `EnhanceEquipResponse.Replayed`。
   - `PlayerMysteryMerchant.cs` 有 `BuyMysteryMerchantGoodsRequest.ClientRequestId` 和应答的 `Replayed`。
   - `PlayerAttribute.cs` 有 `CreateAttributeSchemeRequest.ClientRequestId` 和应答的 `Replayed`。
   - 这次生成也会带进 R2 那批的 `RetrieveTemporaryItems` 和新 tip 码，它们的客户端接入不在本文范围内。

## 1. 统一规则（三个调用点都一样）

| 时机 | 做法 |
|---|---|
| 玩家点一次，本地检查都过了，准备发包 | 生成一个新键（§2），写进请求的 `ClientRequestId` |
| 回包没到时又点了同一个按钮 | 保持现有单飞：不发包、不报错。**不要**为第二次点击生成新键再发 |
| 收到业务应答（成功，或响应体里带 tip） | 结论明确，这个键作废 |
| 应答 `Replayed == true` | 按成功处理，这就是首次那一次的结果，玩家之前没看到过。照常发「成功 / 已购买 / 强化结果」通知，不提示失败 |
| 结果不明：`"rpc timeout"`，或信封错误 `server tip=1003` | 连接对象没变、这个请求还没重发过时，**用同一个请求对象原样重发一次**（同一个键、其余字段都不变），单飞标志保持为真。重发拿到明确结论就照常收尾；重发也是结果不明，就走现有的「结果未知」收尾（见各调用点），不再重发 |
| `"disconnected"`，或连接对象变了 | 第一版不重发，走现有收尾（重拉快照）。服务端其实还记得这个键（随存档落盘），跨连接重发是安全的，见 §9 |
| 信封错误是别的码（1008 限流、1006 等） | 请求没有执行，结论明确，照现有逻辑处理 |
| 带键请求收到 `KInvalidParameter` | 说明客户端把旧键用在了新操作上，属于客户端缺陷。照现有逻辑报错、重拉，另外 `Debug.LogError("[client-request] key reused for another operation: " + 方法名)` |

为什么只自动重发一次、只在同一条连接上：服务端的键随玩家存档落盘（2026-11-06 起），键还在账本里时带原键重发只会拿到首次结果或首次执行，不会扣两次。键会在两种情况下不在：同一玩家之后又成功做了 32 次以上带键的操作，把它挤了出去；账本坏了被人工清空（设计稿 §5.2）。第一版只在同一条连接上重发，是为了让客户端少做一段「跨连接保留待发请求」的逻辑：连接换了就重拉快照，让玩家看过结果再决定。跨连接、跨 App 重启的自动恢复见 §9，是可选后续。

## 2. 新文件 `Assets/Scripts/Net/ClientRequestKeys.cs`（含 `.meta`）

```csharp
namespace MmorpgClient.Net
{
    /// <summary>业务幂等键(服务端 docs/design/client-request-idempotency.md)。与信封的 ClientRequest.id 是两回事:
    /// 信封 id 每次发送都换号,幂等键在一次操作的所有重发里不变。</summary>
    public static class ClientRequestKeys
    {
        /// <summary>非 0 的随机 64 位数,来自 RandomNumberGenerator。拿不到安全随机数时返回 false,调用方不发包、提示玩家。</summary>
        public static bool TryCreate(out ulong key);

        /// <summary>旧字符串错误契约下,这个失败是否「结果不明、可以带原键重发一次」:
        /// "rpc timeout" 或 "server tip=1003" 为真;"disconnected"、其它 tip、解析失败为假。</summary>
        public static bool ShouldResendOnce(string legacyError);
    }
}
```

- `TryCreate` 照 `GuildClient.EconomyRequestId`（`GuildClient.cs:971-982`）的写法：循环到非 0 为止，捕获 `CryptographicException` 返回 false。
- `GuildClient.EconomyRequestId` 改成调 `TryCreate`，失败文案不变。
- `ShouldResendOnce` 判 1003 时，用的必须是客户端现有的信封解析函数（`server tip=` 前缀，`RpcStatus.ToLegacyError` 唯一的翻译点），不要另写一套字符串匹配。

## 3. 强化：`Assets/Scripts/Game/PlayerFeatures/PlayerFeaturesClient.cs`

`EnhanceEquip(ulong itemId)`（:296）：

1. `RejectLocally` 里找到目标时，把 `target.EnhanceLevel` 记下来作期望等级。这是 R2 的契约，见服务端 `equipment-enhance-inlay.md` §6.1。快照缺失时按那边的规则直接发包，期望等级填 0。
2. 本地检查过了以后、进入 `BusyEquip` 之前调 `ClientRequestKeys.TryCreate`。失败就走 `FailEquipAction("无法安全生成请求编号，本次操作未发送")`，不发包。
3. 请求改成 `new EnhanceEquipRequest { ItemId = itemId, ExpectedLevel = expectedLevel, ClientRequestId = key }`。这个对象构造一次，重发时复用。
4. `PerformEquipAction` 的 `Fail` 分支（:474-483），只对强化生效：
   - `ClientRequestKeys.ShouldResendOnce(error)`、`IsCurrent(epoch, playerId)`、请求序号没变、这一次还没重发过，四个条件都满足时，原样再 `send(Complete, Fail)` 一次（不改 `BusyEquip`，也不改 `_lastForgeSendMs`）。
   - 其余情况维持现状：`Reject(..., true)` 重拉两份快照。
   - 实现方式：给 `PerformEquipAction` 加一个可选参数 `bool resendOnceOnUnknown = false`，只有强化传 true；穿脱、镶嵌、取下的行为与文案逐字不变（现有用例守着）。
5. 把 :479 的注释「绝不自动重发:强化不是幂等操作」改成新规则：带键的强化只在同一条连接上、结果不明时原样重发一次，服务端按键去重。
6. `Replayed` 不需要特殊处理：`Complete` 用的就是应答里的 `Success` / `EnhanceLevel`，重放带回的是首次结果，`LastEnhanceOutcome` 与 `OnEnhanceResult` 照常发布。
7. `KEquipEnhanceLevelChanged` 按 R2 的契约处理：一律重拉两份快照，不提示「失败」。这一条不属于本文的改动，但同一个请求要一起做对，不然强化过一次的装备会一直被拒。

## 4. 神秘商人：`Assets/Scripts/Game/MysteryMerchant/MysteryMerchantClient.cs`

`Buy(quote, count)`（:321）：

1. 本地检查都过了以后、`BuyPending = true` 之前调 `TryCreate`，失败就 `RejectLocally("无法安全生成请求编号，本次操作未发送")`。
2. 请求对象先构造好、带上 `ClientRequestId = key`，再交给 `_net.Call`。把调用包成一个本地函数，重发时复用同一个请求对象。
3. `error` 回调（:378 起）：
   - `ShouldResendOnce(error)`、`IsCurrent(generation)`、这一次还没重发过，三个条件都满足时：保持 `BuyPending`、不调 `EndBuy`，原样重发一次。
   - 信封错误是 1003 以外的码：维持 `FailBuy(tipId)`，请求没有执行。
   - 重发之后又收到 1003，或者收到超时 / 断线：按结果不明处理，即现有的 `RequiresReconnect = true` 加 `RecoveryMessage`，**不要**调 `FailBuy`。现状把所有信封错误都当成「没买成」，而 gate 回 1003 也可能是 gate → scene 超时，那时结果其实不明。这一处按本条改正。
4. 成功回调：`Replayed == true` 和普通成功走同一条路径（`ApplyInfo`、`Currency`、`Purchased?.Invoke(seen, count)`）。
5. :385 的注释「gate 层带码拒绝…结果是确定的『没买成』」改成：1003 结果不明，其余信封码确定没买成。

## 5. 新建加点方案：`Assets/Scripts/Game/Attribute/AttributeClient.cs`

`CreateScheme(string name)`（:235）：

1. `BeginWrite` 成功之后调 `TryCreate`。失败时 `FinishWrite(stamp, false)`，并 `OnError?.Invoke("无法安全生成请求编号，本次操作未发送")`。
2. 请求 `new CreateAttributeSchemeRequest { Name = name ?? string.Empty, ClientRequestId = key }` 构造一次，重发复用。
3. `err` 回调：`ShouldResendOnce(err)`、`CurrentWrite(stamp)`、这一次还没重发过，三个条件都满足时，原样重发一次，`Busy` 保持为真。其余情况走 `FailWrite(stamp, err)`。只要是结果不明（`ShouldResendOnce` 为真，或 `"disconnected"`），收尾时都要 `RequestPanel()` 重拉面板：服务端可能已经建好了方案。只改 `CreateScheme` 这一条路径，其它写请求不动。
4. 成功回调照常 `CompleteWrite`。`Replayed == true` 时面板就是当前面板，`SchemeId` 是首次建出来的方案号。

## 6. 用例（EditMode，沿用各自现有的夹具与假传输）

| 文件 | 用例 |
|---|---|
| `Assets/Tests/EditMode/Net/ClientRequestKeysTests.cs`（新建，含 `.meta`） | `TryCreate` 连取 1000 次都非 0、互不相同；`ShouldResendOnce` 的分类表：`rpc timeout` 真、`server tip=1003` 真、`server tip=1008` 假、`disconnected` 假、`parse response: x` 假 |
| `Assets/Tests/EditMode/Battle/EquipForgeClientTests.cs` | ① 强化请求带非 0 的 `ClientRequestId`，`ExpectedLevel` 等于快照里的等级；② 两次各自完成的强化，键不同；③ `rpc timeout` 之后恰好重发一次，请求逐字节相同，`BusyEquip` 仍为真；喂重放应答（`Replayed = true`、`Success = false`）后，`LastEnhanceOutcome` 与 `OnEnhanceResult` 都是首次结果；④ 第二次又超时：不再重发，按现状重拉两份快照；⑤ `disconnected`：不重发；⑥ 信封 1008：不重发；⑦ 信封 1003：重发一次；⑧ 穿脱、镶嵌、取下遇到 `rpc timeout` 都不重发（回归） |
| `Assets/Tests/EditMode/Tianyong/MysteryMerchantClientTests.cs` | ① 购买请求带非 0 键；② `rpc timeout` 之后原样重发一次，`BuyPending` 仍为真；重放应答之后 `Purchased` 只触发一次；③ 1003 两次：进入 `RequiresReconnect`，不显示「没买成」的文案；④ 1008：`FailBuy`，不重发；⑤ `disconnected`：不重发 |
| `Assets/Tests/EditMode/Battle/AttributeClientTests.cs` | ① `CreateScheme` 带非 0 键；② 超时之后原样重发一次，`Busy` 仍为真；重放应答里的面板被应用；③ 两次结果都不明：报错，并且重拉一次面板；④ 别的写请求（加点、洗点、切换、改名）超时不重发（回归） |
| `Assets/Tests/EditMode/Guild/` 现有帮会用例 | 改用 `ClientRequestKeys.TryCreate` 之后全部照旧通过 |

断言「原样重发」时，比较两次发出请求的 `ToByteArray()` 是否完全相同；只比较键不够，其余字段也必须一样。

## 7. 验证（客户端仓）

1. `pwsh -File tools/client_compile_check.ps1`：退出码 0。
2. Unity EditMode 测试按客户端仓现有方式跑（batchmode `-runTests -testPlatform EditMode`），先跑上表涉及的四个测试类，再跑一遍 EditMode 全量。编辑器开着工程时 batchmode 进不去，按 `client_compile_check.ps1` 头部的说明处理。
3. 通过标准：全绿。失败时保留第一个失败用例名和断言原文，不改断言、不连续重试。
4. 联调（服务端验证通过之后，可选）：dev 环境里连点强化、购买、新建方案，各点两次以上，只扣一次。在 scene 日志里能看到 `[ClientRequest] metric=client_request_replay` 的前提是确实发生了重发；单飞拦下的点击不会发包，也不会有这行日志。

## 8. robot 冒烟（2026-11-06 补；2026-11-13 Claude 已改完，未编译、未运行，Codex 编译并在 dev 环境跑）

robot（本仓 `robot/`）也是客户端。三个现有冒烟各加一步，在真实环境里验证「同一个键只执行一次」与「随存档落盘」。

| 场景（在 `robot/` 下执行） | 改了哪里 | 加的那一步 |
|---|---|---|
| `.\robot.exe -c etc/equip_forge_smoke.yaml` | `equip_forge_smoke_scenario.go`：`equipForgeEnhance` 签名不变（`asset_arrival_prepare.go` 也在调它），每次调用自带一个新键；新增 `equipForgeSendEnhance` 发调用方组好的请求；第 3a 步自己组请求、留住对象；对账函数 `checkEquipForgeReplay` | 背包里那次强化之后，用**同一个请求对象**原样再发一次（`step=replay`）。断言：tip 为 0，`Replayed == true`，`Success` / `EnhanceLevel` 与第一次相同，两份快照与第一次的应答逐项相同（金币、强化石、目标都没再动）。原来那条「按强化前的等级重发」改成用新键发（`step=enhance-bag-stale-level`），验的是快照过期或旧键被挤出账本时由 `expected_level` 兜住。之后的强化每次用新键 |
| `.\robot.exe -c etc/merchant_smoke.yaml` | `mystery_merchant_smoke_scenario.go`：`merchantBuy` 改为接收请求对象，`merchantBuyRequest(quote, count, key)` 组请求；对账函数 `merchantSmokeCheckReplay` | M6 买成之后原样重发（`step=replay`）：tip 为 0，`Replayed == true`，余额与物品数不变，应答货架里这件的 `bought_count` 不变。**落盘验证**：M8 之后正常下线、等 2 秒、重新登录进场（`channelSmokeLogin`），再把 M6 的请求原样发一次（`step=replay-after-relogin`）：仍然 tip 为 0、`Replayed == true`，余额与物品数同下线前最后一份成功应答。重登后多半不在商人那条线，看不到货架时只核钱和物 |
| `.\robot.exe -c etc/attribute_smoke.yaml` | `attribute_smoke_scenario.go` 步骤 10：`CreateAttributeSchemeRequest` 带键，请求与应答对象留住 | 新建成功后原样重发（`step=replay`）：tip 为 0，`Replayed == true`，`SchemeId` 与第一次相同，方案数与金币都没变。走「上限已满」分支时跳过这一步并打一行说明，`ATTRIBUTE_SMOKE_OK` 行带 `create_replay=checked` 或 `create_replay=skipped_limit` |

- 键由新文件 `robot/smoke_client_request_key.go` 的 `smokeClientRequestKey()` 生成：`crypto/rand` 取的非 0 uint64，三个场景共用，不留时间戳退路。
- 现有的 `*_SMOKE_OK` / `*_SMOKE_FAIL step=… reason=…` 输出格式不变，新步骤失败时 `step=` 写 `replay` 或 `replay-after-relogin`。
- 纯函数单测：`TestSmokeClientRequestKeyIsNonZeroAndDistinct`、`TestEquipForgeReplay*`（2 个）、`TestMerchantSmoke*`（3 个，新文件 `mystery_merchant_smoke_scenario_test.go`）。
- 前提：robot 的 vendored proto 里已经有这几个字段（2026-11-13 核对过，`robot/vendor/proto/scene/` 与 `go/proto/scene/` 的三份 `.pb.go` 逐字节相同），本批不需要重新生成协议；dev 环境已起。项目未上线，跑之前清空玩家数据，按 `AGENTS.md` §6.2 的清理步骤做：属性冒烟的账号开满方案之后就验不到新建方案的重放。
- 通过标准：三个场景的退出码都是 0，日志里各有一行 `*_SMOKE_OK`，属性那一行是 `create_replay=checked`。scene 日志里每个场景至少有一行 `[ClientRequest] metric=client_request_replay`，`client_request_id=` 与 robot 日志里打出的键相同；神秘商人场景至少两行（重登前后各一次）。

## 9. 不在本文范围

- R2 那批的客户端部分（`RetrieveTemporaryItems` 的界面与调用）。`expected_level` 和本文的同一个请求有关，§3 第 1、7 条已经顺带要求。
- 跨连接、跨 App 重启的自动恢复（可选后续）：服务端的键已经随存档落盘（2026-11-06），客户端可以照帮会的做法，用 `DurableRequestFileStore` 把待确认的请求存到本地，重连或 App 重启后带原键重发一次。第一版不做，只在同一条连接上重发。做的时候要同时改设计稿 §5.2：那里允许在玩家离线时整体清空账本，前提正是「客户端只在同一条连接上重发」，有了跨连接恢复，清空会让待确认的那次再执行一次。
