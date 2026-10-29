## CurrencyComp 崩溃窗口验证方案

> 创建日期: 2026-06-03
> 状态: 方案设计完成,未执行
> 上游: docs/notes/player-data-loading-and-sharding-pain.md 阶段 0.1
> 上下文: 已确认 CurrencyComp 全链路持久化通(proto + marshal + unmarshal +
> MySQL schema + lifecycle save)。本验证目的是**量化 kill -9 崩溃窗口的金币
> 丢失量**,不是验证"金钱能否落地"。

---

## 验证目的(必须明确,避免做歪)

不是: "金币能不能存到 MySQL"——已经从代码 100% 确认能。

是: 在 scene 进程崩溃(kill -9)的时候,**两次 SavePlayerToRedis 之间的金币变更**
会丢多少。这个数字决定 P2 阶段 dirty-save + currency delta 写要做到多严。

---

## 已知保存触发点(代码核对)

| 触发点 | 位置 | 触发条件 |
|---|---|---|
| 周期性全量 save | `cpp/libs/services/scene/core/system/redis.cpp:75-90` | 默认每 300s 扫一次 `tlsEcs.playerList`,每个在线玩家都 marshal 一次。`SCENE_PLAYER_SAVE_INTERVAL_SECONDS=0` 可关 |
| 玩家退出 | `player_lifecycle.cpp:438` | `HandleExitGameNode` 路径,正常下线必走 |
| 跨服迁移 | `player_lifecycle.cpp:671`(2026-10-22:现为 `StartTravelHandoff`) | 跨 zone 完成时。**2026-10-20 更正(10-22 补限定)**:这条搬数据链(player_migrate)已下线。<br>现在跨 zone 传送一定在交接开始时存一次盘。同 zone 跨节点换图只有被 scene_manager 以 18 拒绝时才进入同一条链,这要求 `AllowUnsafeCrossNodeHandoff=false`(生产);本机 `scene_manager_service.yaml` 默认是 `true`,所以本文"单 zone,本机环境"的验证里不会多这次存盘。<br>`StartTravelHandoff` 先冻结再存盘;盘上已是同一份、且没有在途存盘时,跳过这次存盘。Redis 存盘落地、且两张子表的 SQL ACK 与 pending 同版之后,才写 handoff 标记(`handoff_sql_barrier.h`)。<br>见 cross-zone-scene-travel.md §11.2、§11.3、§13.2 |
| GM 加金币 | ❌ **没有** | `GmAddCurrency` handler 调 `CurrencySystem::AddCurrency` 后**没有**主动 SavePlayerToRedis,变更只在内存里 |

**即默认 300s 周期**: 加金币 → 等 0~300s → 周期 save → 之间崩了就丢这段时间的 delta。
这跟 notes 主文档"P2 dirty-save + 独立 delta 写"的优化目标完全对得上——目标是把
这个窗口从 300s 收敛到事务级别。

---

## 验证步骤(单 zone,本机环境)

### 前置准备

1. **数据全清**(CLAUDE.md §9.6 强制):
   - `redis-cli FLUSHALL`
   - 清 MySQL `player_database` / `user_accounts` / `account_share_database` 等表
   - `etcdctl del --prefix /` 清 etcd 注册数据
   - kafka offset reset: `pwsh tools/scripts/dev_tools.ps1 -Command kafka-offset-reset`
2. **服务启停**:
   - go services: `pwsh tools/scripts/go_services.ps1 -Command start`
   - cpp 节点: `pwsh tools/scripts/cpp_nodes.ps1 -Command start -Nodes gate,scene -SceneCount 1 -GateCount 1`
3. **找到 scene PID**:
   - `cat run/pids/cpp_nodes.pid.json` 拿到 scene_1 PID

### 测试用例 A: 短窗口(预期丢失)

**目的: 证实存在崩溃窗口,量化默认配置下的丢失量。**

1. 机器人登录 1 个号(player_id=1),记录初始金币 = `B0`
2. 通过机器人发 `GmAddCurrencyRequest{ currency_type=GOLD, amount=10000 }`
3. 等 5 秒(远小于 300s 周期 save)
4. `Stop-Process -Id <scene_pid> -Force`(等价 kill -9)
5. 重启 scene: `pwsh tools/scripts/cpp_nodes.ps1 -Command start -Nodes scene`
6. 机器人重新登录 player_id=1,读取金币 = `B1`
7. **期望**: `B1 == B0`(变更丢失,这正是要量化的窗口)
   **如果 `B1 == B0 + 10000`**: 说明 GM 路径异常触发了 SavePlayerToRedis,
   要回头看 handler 是不是有同步 save(跟当前代码核对结论矛盾,需要重查)

### 测试用例 B: 跨周期窗口(预期不丢)

**目的: 证实周期 save 真的兜住了金币。**

1. 同样登录 player_id=2,记录 `B0`
2. GM 加 10000 金币
3. **等 350 秒**(>1 个周期 save 周期,默认 300s)
4. 观察 scene 日志看到 `[RedisSystem] Periodic save scanned N online players`
5. `Stop-Process -Id <scene_pid> -Force`
6. 重启,重新登录,读 `B1`
7. **期望**: `B1 == B0 + 10000`(周期 save 已落地)

### 测试用例 C: 缩短间隔加速验证

**目的: 不想等 300s 跑 case B 时使用。**

启动 scene 时加环境变量 `SCENE_PLAYER_SAVE_INTERVAL_SECONDS=10`,然后 case B
里"等 350s"改成"等 15s"。这样 case A + case B 一轮跑完 < 1 分钟。

### 测试用例 D: 正常下线(回归保护)

**目的: 确保现有"正常下线不丢钱"的契约还在。**

1. 登录 player_id=3,GM 加 10000 金币
2. 机器人发 ExitGame(正常下线,不 kill)
3. 重新登录,读 `B1`
4. **期望**: `B1 == B0 + 10000`(`HandleExitGameNode` 路径必走 SavePlayerToRedis)

---

## 工具栈(都是现成的,不需要写新工具)

| 用途 | 现成工具 | 缺什么 |
|---|---|---|
| 机器人登录 | `robot/` | 已有,直接用 |
| GM 加金币 | `SceneCurrencyClientPlayer/GmAddCurrency` RPC,proto 已生成 | ~~机器人没调用~~ ✅ 已加 scenario(2026-06-03) |
| kill scene | `Stop-Process -Id <pid> -Force` | 现成 |
| 重启 scene | `tools/scripts/cpp_nodes.ps1 -Command start -Nodes scene` | 现成 |
| 读金币 | `GetCurrencyList` RPC | ~~机器人没 handler~~ ✅ 已加 handler(2026-06-03) |

---

## 实施现状(2026-06-03)

### 机器人侧已就绪

- `robot/logic/handler/scene_currency_client_player_get_currency_list.go` — 读 balance 响应处理
- `robot/logic/handler/scene_currency_client_player_gm_add_currency.go` — GM 加金币响应处理
- `robot/logic/gameobject/player.go` — 加 `currencyValues` / `lastBalanceAfter` 字段
  + `SetCurrencyValues` / `GetCurrencyValue` / `SetLastBalanceAfter` /
  `GetLastBalanceAfter` / `Reset*` / `Wait*` 共 8 个方法
- `robot/currency_crash_window_scenario.go` — `testCurrencyCrashWindow`(in-process
  smoke,挂在 login-test 套件) + `runCurrencyCrashSnapshotMode`(独立 mode,
  外部脚本驱动)
- `robot/etc/robot.currency-crash.yaml` — 给 currency-crash-snapshot mode 的 yaml
- `robot/main.go` — 加 `mode == "currency-crash-snapshot"` 分支
- `robot/config/config.go` — 加 `CurrencyCrashConfig` + 校验白名单更新

### 驱动脚本已就绪

`tools/scripts/currency_crash_window.ps1`,一键跑 4 个 case,产出:
- 每个 leg 的 snapshot JSON(`case-X_leg_pre.json` / `case-X_leg_post.json`)
- `summary.json` 汇总
- `summary.md` 可直接粘到本文档"执行结果"章节的 Markdown 表

### 运行步骤

```pwsh
# 默认: 跑全部 4 个 case (A/B/C 每次都清数据,B 等 350s,C 用 10s save 间隔加速)
pwsh tools/scripts/currency_crash_window.ps1

# 只跑 case C (15s 加速版,便于反复迭代脚本本身)
pwsh tools/scripts/currency_crash_window.ps1 -Cases C

# 不重置数据 (调试用,正式跑务必保持默认开启)
pwsh tools/scripts/currency_crash_window.ps1 -NoReset
```

前置条件: gate / login / db / etcd / kafka / redis / mysql 已通过
`go_services.ps1 + cpp_nodes.ps1` 启动好;脚本会自己负责 scene 的
启停 + 数据清理。

---

## 执行结果

> 状态:**驱动已修复并能端到端跑通,但真实结果被一个服务端 bug 阻塞,已定位并修复 proto,
> 待重新编译 gate 后才能取到可信数据。在 gate 重编译前,不写入任何捏造的结果表。**

### 本轮做了什么

1. **修复驱动脚本** `tools/scripts/currency_crash_window.ps1`:
   - `Invoke-DataReset`:改为走 `docker exec` 操作 redis/mysql,库名 `zone_1_db`,
     mysql root 密码默认 `Mmorpg#2026db`,新增 `-Zone` 参数(原来用的 host `redis-cli`/`mysql`
     不存在、库名/密码也不对)。
   - `Start-Scene` **死锁修复**:`cpp_nodes.ps1` 用 `Start-Process -RedirectStandardOutput`
     拉起 `scene.exe`,Windows `CreateProcess` 的 `bInheritHandles=TRUE` 使 detached 的 scene
     继承并一直占着启动器 pwsh 的标准句柄;任何依赖该句柄 EOF 的等待(`... | Out-Host` 管道、
     或 `Start-Process -Wait` 等重定向流)都会被 scene 的整个生命周期阻塞 → 驱动卡死。
     改为 **fire-and-forget 启动 + 轮询就绪**(轮询 `Get-Process scene` 并在
     `run/logs/cpp_nodes/scene.*.log` 中匹配 `STARTED SUCCESSFULLY` 横幅,45s 超时重试 4 次)。
   - 备注:scene 端口是 etcd 动态分配(无固定 metrics 端口);9150 是 Go 的 scene_manager,
     **不是** scene,不要拿它探活。

2. **定位真正阻塞点(服务端 bug)**:机器人能正常登录并进入场景(scene 日志可见
   `HandleEnterScene ... saved to Redis`),但 `GetCurrencyList`(message id **54**)超时,
   导致币值快照全为 0、结果退化(A 假失败、C/D 假通过)。
   gate 日志:`ERROR Invalid or unauthorized message ID: 54`。
   - 拦截点:`cpp/nodes/gate/handler/rpc/client_message_processor.cpp` 的
     `IsClientMessageId(msg_id)` 白名单校验。
   - 白名单由生成代码 `cpp/generated/rpc/service_metadata/rpc_event_registry.cpp` 的
     `IsClientMessageId` 决定,只对带 `option (OptionIsClientProtocolService) = true;`
     的 service 放行。
   - **根因**:`proto/scene/player_currency.proto` 的 `service SceneCurrencyClientPlayer`
     只设了 `OptionIsPlayerService`,**漏了 `OptionIsClientProtocolService`**,而所有兄弟
     `Scene*ClientPlayer`(skill/scene/movement/common)都设了它 → 货币消息 id 被排除在 gate 白名单外。

3. **已修复 + 已重新生成**:
   - 给 `SceneCurrencyClientPlayer` 补上 `option (OptionIsClientProtocolService) = true;`。
   - 执行 `dev.bat proto` 重新生成,确认 `rpc_event_registry.cpp` 的 `IsClientMessageId`
     switch 现已包含 `SceneCurrencyClientPlayerGetCurrencyListMessageId` 等全部货币 case。

### 待办(取真实结果的前置条件)

- [ ] **重新编译 gate C++ 节点**(按仓库约定 C++ 编译由本人手动执行,Agent 不自动触发)。
- [ ] 用新 gate 重启栈,重跑驱动:
      `Start-Process pwsh -ArgumentList @('-NoProfile','-File','tools\scripts\currency_crash_window.ps1','-Cases','A,C,D') -RedirectStandardOutput run\logs\java\driver.log -RedirectStandardError run\logs\java\driver.err -WindowStyle Hidden`
      (case B = 350s 慢档,本轮按计划跳过)。
- [ ] 把生成的 `robot\logs\currency-crash-window-<ts>\summary.md` 的 markdown 表粘贴替换本节,
      并补:周期 save 实际触发时间戳(scene 日志 grep `Periodic ... save`)。
- 预期:A `Lost > 0`(kill 在两次 save 之间,金币回退),C/D `Lost == 0`(已落库,保住)。

---

## 跑前检查清单(给执行人)

### 必须先就位的服务

- [ ] redis 在 `127.0.0.1:6379`,`redis-cli ping` 返回 PONG
- [ ] mysql `mmo` 库已建,root 密码与 `$env:MYSQL_PASSWORD`(默认 root)一致
- [ ] etcd 启动(go services 依赖)
- [ ] kafka broker 启动 + 必要的 topic 已创建
- [ ] gate / login / db / centre / chat 等 go services 已启动:
      `pwsh tools/scripts/go_services.ps1 -Command start`
- [ ] gate 节点已启动(机器人需要它):
      `pwsh tools/scripts/cpp_nodes.ps1 -Command start -Nodes gate`
- [ ] **scene 不要预先启动**——驱动脚本会自己起停 scene

### 必须不在的东西

- [ ] 没有遗留的 `scene.exe` 进程在跑(`Get-Process scene` 应返回空)
- [ ] `run/pids/cpp_nodes.pid.json` 里没有 stale 的 scene 记录(脚本会清,
      但提前手清更稳)

### 启动命令

```pwsh
# 跑全部 4 个 case (默认会清数据)
pwsh tools/scripts/currency_crash_window.ps1

# 单 case 调试
pwsh tools/scripts/currency_crash_window.ps1 -Cases C
```

### 跑完后回填结果

1. 脚本会在 `robot/logs/currency-crash-window-<ts>/` 生成
   `summary.md` + `summary.json` + 8 个 `case-*_leg_*.json`(2 leg × 4 case)
2. 把 `summary.md` 的 markdown 表内容粘贴替换上面"## 执行结果"下的占位
3. 从 `robot/logs/cpp_nodes/scene_1.log` 抓 `[RedisSystem] Periodic save scanned`
   日志行,把实际触发时间戳贴在结果表下方,作为周期 save 间隔的实测值
4. 如果 case A 的 `Lost > 0`、case B/C/D 的 `Lost == 0`,Pass = ✅,这是 P2
   dirty-save 设计的 baseline 数据点

---

## 已知坑(执行时小心)

1. **case B 要等 350s**——耐心,中途不要 Ctrl+C,否则 leg2 不会跑;急用就跑 case C
2. **mysql 密码**通过 `$env:MYSQL_PASSWORD` 注入,默认 root;如果你的本地不是 root,
   先 `$env:MYSQL_PASSWORD = "你的密码"` 再跑
3. **kill 时机**: 脚本是 leg1 完整跑完(包括等响应)再 sleep N 秒后 kill。
   case A 的 N=5,意味着 GmAddCurrency 已经���回 + 等 5s 后 kill。
   这 5s 内可能因为别的因素(比如 leg1 退出时机器人侧的 Disconnect)
   触发了一次保存,实测可能丢失量小于 10000——这是观测点不是 bug
4. **scene 重启后 metrics port 9150 必须能 bind**——如果你改过端口,加 `-ScenePort`
5. **现在还没真跑过**——这是第一次端到端尝试,真跑出问题在 logs 目录里看
   `case-X_leg_*.json.cfg.yaml`(每个 leg 用的实际 yaml)和 `scene_1.log` 排查

---

## 改动清单(2026-06-03 实施)

为这次验证增加 / 修改的文件,跑完之后如果整套用不上可以一并撤:

**新文件**:
- `robot/currency_crash_window_scenario.go`
- `robot/etc/robot.currency-crash.yaml`
- `robot/logic/handler/scene_currency_client_player_get_currency_list.go`
- `robot/logic/handler/scene_currency_client_player_gm_add_currency.go`
- `tools/scripts/currency_crash_window.ps1`
- `docs/notes/currency-crash-window-verification.md`(本文件)

**修改文件**:
- `robot/main.go`(加 currency-crash-snapshot mode 分支)
- `robot/config/config.go`(加 CurrencyCrashConfig + 校���白名单)
- `robot/logic/gameobject/player.go`(加 currency 状态字段 + 8 个方法)
- `robot/logic/handler/message_body_handler.go`(注册 2 个 currency handler)
- `robot/login_test_scenarios.go`(scenarios 列表加 CurrencyCrashWindow)
- `docs/notes/player-data-loading-and-sharding-pain.md`(修正 currency
  持久化判断 + 引用本文档)

---

## 输出物(执行后要交的)

1. 一张表(同 case A/B/C/D 四行),记录每个 case 的 `B0`/`B1`/期望/实际/通过否
2. 周期 save 实际间隔(从 scene 日志 `[RedisSystem] Periodic save` 的时间戳算)
3. 写进 `docs/notes/player-data-loading-and-sharding-pain.md` 阶段 0.1 章节,
   作为 P2 dirty-save 设计的 baseline 数据

---

## 不在本验证范围内的事

- **dirty-save 接入**: 这是 P2 子任务,不要顺手做了。本验证只测量当前实际窗口
- **新建 currency event topic**: 同上,P2 子任务
- **测压性能影响**: 不是本验证目的,Round 17 的事
- **多 zone / 跨服**: 单 zone 单 scene 跑就够了

---

## 风险提醒

1. **kill -9 之后机器人可能挂着旧连接**——重新登录前要确认机器人侧的 client
   断开了,否则可能拿到 stale 连接复用导致结果误判
2. **DB sub-task Kafka 消息有 retry**——kill scene 后 db service 仍然会消费
   早先发出去的消息。要确认 kill 时机在"GmAddCurrency 发了消息但 DB 还没消费"
   的窗口,这个窗口非常短(单机 < 1s),case A 等 5 秒够安全(已经超过 db 消费延迟,
   所以 case A 的丢失量 = 5 秒区间内"通过周期 save 触发并被 db 消费"的部分,
   实际等于 0,符合预期)。
3. **CLAUDE.md §9.5 规定压期间不上传日志**——这次是验证不是压测,可以本地分析
   日志,但不要 push 到仓库
