# GitHub 幸存者割草类游戏源码调研

> - 迁入说明：2026-09-07 从既有调研资料迁入；仓库活跃度、Star 和许可证结论仍是 2026-08-27 的快照。
> - 调研日期：2026-08-27
> - 调研状态：已完成
> - 数据口径：GitHub 官方仓库、README、LICENSE 与 GitHub API；Star 为调研时快照，会继续变化。

## 1. 结论

视频展示的不是《文明时代》式大地图策略，而是俯视角怪潮割草玩法：玩家在开放场地移动，敌人持续向玩家聚集，攻击由自动武器或右下角技能按钮驱动，核心体验是走位、清怪和构筑成长。通常可归为 **Survivors-like / Vampire Survivors-like / Bullet Heaven / Horde Survival**。

GitHub 上有相似源码，但不存在一个同时满足“高 Star、玩法完全一致、完整可玩、Unity、成熟多人、商业授权清晰”的项目。最实用的组合是：

- 用 [SikPang/Unity-VampireSurvivors-Copy](https://github.com/SikPang/Unity-VampireSurvivors-Copy) 和 [matthiasbroske/VampireSurvivorsClone](https://github.com/matthiasbroske/VampireSurvivorsClone)研究标准单人割草循环。
- 用 [Roo-Roo-Roo/survivors-roguelike-kit](https://github.com/Roo-Roo-Roo/survivors-roguelike-kit)研究较完整、数据驱动的 Unity 游戏模板。
- 用 [Cast43/Recycle_Rush](https://github.com/Cast43/Recycle_Rush)研究 Unity DOTS/ECS、Netcode、Relay 和队友复活。
- 用 [GameDev4Funs/void-pulse](https://github.com/GameDev4Funs/void-pulse)研究“房主权威、共享经验、各自选卡”的多人规则。

## 2. 视频玩法判定

对用户提供的本地视频抽帧观察：

- 画面为俯视角绿色场地，角色可自由移动。
- 蝙蝠等敌人从周围持续靠近，形成怪潮压力。
- 右下角存在多个圆形攻击或技能按钮。
- 战斗重心是移动躲避、清理大量追踪敌人，而不是省份地图、城池经营或回合外交。

视频只有约 11.35 秒，没有展示完整升级界面，因此“经验掉落、升级三选一、局外成长”属于同类玩法的筛选维度，不能当作视频中已经直接出现的事实。

## 3. 按 GitHub Star 降序主榜

相似度说明：

- **高**：具备俯视移动、怪潮、自动或技能攻击、经验/升级构筑中的大部分核心环节。
- **中**：战斗节奏或构筑接近，但核心控制、关卡结构或成长循环不同。
- **低**：主要是教学或极简技术验证，只覆盖少量机制。

| 排名 | 项目 | Star | 最近 push | 许可证 | 相似度 | 成熟度与可玩性 | 多人 | 主要相似点与差距 |
|---:|---|---:|---|---|---|---|---|---|
| 1 | [a327ex/SNKRX](https://github.com/a327ex/SNKRX) | 1,995 | 2022-07-01 | 代码 MIT；素材各自授权 | 中高 | 已发布的完整商业游戏源码，可运行 | README 未声明，按单人 | 自动攻击、无尽怪潮、英雄组合与职业加成很接近；差异是控制蛇形英雄队，主要在关卡间购买构筑，不是标准“经验宝石→即时升级三选一” |
| 2 | [a327ex/BYTEPATH](https://github.com/a327ex/BYTEPATH) | 1,539 | 2020-10-17 | README 声明代码 MIT、素材分别授权；API 为 `NOASSERTION` | 中 | 完整游戏源码，并提供从零构建教程 | README 未声明，按单人 | 大型技能树、职业、飞船与 Build 理论构筑很强；偏太空街机/双摇杆射击，不是标准自动攻击割草循环 |
| 3 | [MartinKral/Slash-The-Hordes](https://github.com/MartinKral/Slash-The-Hordes) | 537 | 2026-01-29 | **未发现 LICENSE** | 高 | 完整 Cocos Creator 游戏，有 Y8 在线版本 | README 未声明，按单人 | README 明确称深受 Vampire Survivors 启发，并包含对象池、事件、弹窗等系统；代码公开但没有明确复用授权 |
| 4 | [matthiasbroske/VampireSurvivorsClone](https://github.com/matthiasbroske/VampireSurvivorsClone) | 272 | 2024-04-29 | MIT | 高 | 完整可运行 Unity 项目，有网页演示 | README 未声明，按单人 | 20+ 可升级武器/能力、4 种敌人、2 个 Boss、升级系统、对象池、无限背景、移动端与 PC 输入；内容量仍偏模板/样例 |
| 5 | [bones-ai/c-blob-survival](https://github.com/bones-ai/c-blob-survival) | 184 | 2025-03-09 | **未发现 LICENSE** | 高 | 有可玩 WASM 成品，但源码不是开箱即跑 | README 未声明，按单人 | 数千敌人、四叉树碰撞、敌人掉落、经验进度、升级、不同攻击与敌人行为；作者明确部分素材不能再分发，仓库只提供占位素材，且核心集中在单个 C 文件 |
| 6 | [SikPang/Unity-VampireSurvivors-Copy](https://github.com/SikPang/Unity-VampireSurvivors-Copy) | 181 | 2026-05-29 | MIT；第三方素材另核 | 高 | 完整 Unity2D 仿作，并提供 Windows、macOS、Android、iOS 构建入口 | README 未声明，按单人 | 四向持续刷怪、6 种自动武器、击杀掉经验水晶、升级随机三选一、背包、死亡、移动端虚拟摇杆，与视频核心循环非常接近；Asset Store 素材需逐项核权利与署名条件 |
| 7 | [DarkRewar/SurvivorsStarterKit](https://github.com/DarkRewar/SurvivorsStarterKit) | 103 | 2026-04-27 | MIT | 高 | 可运行的 Godot 4 C# starter kit，不是完整内容游戏 | README 未声明，按单人 | 移动、属性、4 种法术、5 种敌人、Boss、成长曲线和升级齐全；UI 与内容较少，README 自报超过约 200 个敌人时性能明显下降 |
| 8 | [brannotaylor/SurvivorsClone_Complete](https://github.com/brannotaylor/SurvivorsClone_Complete) | 102 | 2024-06-09 | CC0-1.0 | 高 | 完整 Godot 教程配套工程，可用于学习和原型 | README 未声明，按单人 | 覆盖标准幸存者玩法骨架，授权宽松；定位是教程完成项目，不等于产品级内容、性能和工程治理 |
| 9 | [ptidejteam/ecs-survivors](https://github.com/ptidejteam/ecs-survivors) | 67 | 2026-02-04 | GPL-3.0 | 高 | 可玩学习原型，官方 README 链接最新 itch.io 构建 | README 未声明，按单人 | 使用 Flecs ECS 与 Raylib，覆盖移动、敌人、碰撞、战斗、升级和地图；更适合研究 ECS 与大量单位，GPL 衍生分发义务不适合直接并入闭源产品 |
| 10 | [PlayWithFurcifer/10LOCSurvivors](https://github.com/PlayWithFurcifer/10LOCSurvivors) | 55 | 2024-02-04 | MIT | 低 | 可玩的极简概念验证 | README 未声明，按单人 | 用 10 行 GDScript 还原最小“移动、怪物追踪、射击”循环；没有成熟的经验、升级、Build 和内容系统 |
| 11 | [Ninetyfiv3/ProjectVoid](https://github.com/Ninetyfiv3/ProjectVoid) | 47 | 2023-06-25 | MIT | 中高 | Qt/C++ 原型；README 极少，没有运行说明或 Release，不能确认开箱可玩 | README 未声明，按单人 | 源码可见多武器、精英/远程敌人、经验、商店和升级模块；只有 3 次提交，文档和维护风险较高 |
| 12 | [Roo-Roo-Roo/survivors-roguelike-kit](https://github.com/Roo-Roo-Roo/survivors-roguelike-kit) | 24 | 2026-07-26 | MIT；DOTween 与第三方内容另核 | 高 | 完整且活跃的 Unity 6 游戏模板，可直接运行 | README 未声明，按单人 | 技能与进化、Buff、近战/远程/冲锋敌人、程序刷怪/地图、经验/掉落/升级、角色/关卡选择、存档、对象池均已具备；Star 不高，但作为开发基底比许多高 Star 演示更完整 |
| 13 | [getsentry/sentaur-survivors](https://github.com/getsentry/sentaur-survivors) | 18 | 2026-08-21 | 游戏代码/美术/音效 Apache-2.0；音乐和依赖另有协议 | 高 | 完整的一周 Hackweek Unity 游戏；当前仓库软归档，新版迁移到 `sentry-demos/unity` | README 未声明，按单人 | 4 种武器升级路线、7 种敌人、经验、升级选择、键鼠/手柄/触屏支持；更像需要瞄准的射击版，不是纯移动自动攻击 |

## 4. 多人专项

高 Star 主榜几乎全部是单人项目。多人候选的 Star 很低，适合研究网络语义或作为原型参考，不能把低 Star 等同于低代码价值，也不能把“能联机”直接视为生产可用。

| 项目 | Star | 最近 push | 许可证 | 多人方式 | 玩法与成熟度判断 |
|---|---:|---|---|---|---|
| [Cast43/Recycle_Rush](https://github.com/Cast43/Recycle_Rush) | 1 | 2026-08-06 | MIT | Unity 6000、DOTS/ECS、Netcode、Relay 在线合作 | 有自动攻击、怪潮、材料升级、队友复活，和目标游戏的 Unity 联机方向最贴近；属于学术/低采用项目，应把它当实现案例而非成熟框架 |
| [sergiosanchezcustodio/emerita-survivors](https://github.com/sergiosanchezcustodio/emerita-survivors) | 0 | 2026-08-27 | GPL-3.0 | 浏览器本地 1–4 人 | 自动武器、经验三选一、57 种武器、3 个 Boss、队友复活和永久商店，内容较丰富；GPL 不宜直接并入闭源商业代码，且项目非常新 |
| [Cinereus/vampire-survivors-fusion-clone](https://github.com/Cinereus/vampire-survivors-fusion-clone) | 0 | 2026-01-12 | MIT | Unity Photon Fusion 2，client/host | 适合看 Photon Fusion 的房主/客户端接入；只是多人技术 Demo，不能当完整割草游戏基底 |
| [GameDev4Funs/void-pulse](https://github.com/GameDev4Funs/void-pulse) | 0 | 2026-08-04 | MIT | 浏览器 LAN 2–4 人，房主权威 | “共享经验、每名玩家独立选卡”以及房主权威模拟对合作割草很有参考价值；仍是低采用的新项目，需要自行验证断线、重连、作弊与大怪潮性能 |

多人设计上，最值得借鉴的不是某一个仓库的全部代码，而是四个明确语义：

1. **服务器或房主权威**：敌人生成、命中、掉落和升级结果不能由客户端自行决定。
2. **共享经验、独立构筑**：队伍共同推进等级，每个玩家分别选择升级，避免争抢经验破坏合作。
3. **倒地与复活**：比立即退场更适合持续怪潮合作，也要求权威侧处理复活进度和无敌窗口。
4. **大怪潮同步降维**：网络只同步必要状态或快照，不逐个广播全部表现对象和特效。

## 5. 许可证与复用边界

- `Slash-The-Hordes` 与 `c-blob-survival` 没有明确 LICENSE。GitHub 上能看到源码不等于获得复制、修改或商用许可，只能阅读思路或联系作者授权。
- `BYTEPATH` 的 GitHub API 没识别出 SPDX 许可证，但 README 明确写明代码 MIT、素材分别授权；使用时必须按文件来源核素材。
- `SNKRX` 同样是代码 MIT、素材分别授权，不能把仓库内所有资源一概视为 MIT。
- `SikPang`、`Roo kit`、`Sentaur Survivors` 都包含第三方素材或依赖，仓库代码许可证不能自动覆盖外部资产。
- `ecs-survivors` 与 `emerita-survivors` 是 GPL-3.0。学习算法和架构没有问题；复制或形成衍生作品并分发时需要评估 GPL 义务。
- `CC0-1.0` 的 `SurvivorsClone_Complete` 权利负担最低，但仍要确认仓库是否包含不受该声明覆盖的第三方资产。

## 6. 推荐顺序

### 只研究“视频这种玩法”

1. **SikPang/Unity-VampireSurvivors-Copy**：标准循环最直观，Unity、自动武器、经验水晶和升级三选一都具备。
2. **matthiasbroske/VampireSurvivorsClone**：内容、对象池、移动端/PC 输入和数据配置较完整。
3. **Roo-Roo-Roo/survivors-roguelike-kit**：Star 较低，但系统覆盖面、活跃度和二次开发价值最好。
4. **SNKRX**：用于研究英雄组合、职业协同、关卡节奏和 Build 反馈，不要把它当标准 Vampire Survivors 克隆。

### 研究大量怪物性能

- 先看 `c-blob-survival` 的四叉树和大量敌人处理思路，但不要复制无许可证代码。
- 再看 `ecs-survivors` 的 ECS 数据布局和更新方式，但不要忽略 GPL 边界。
- `DarkRewar/SurvivorsStarterKit` 适合理解普通节点/物理方案的性能上限和瓶颈，不适合作为大规模怪潮的最终方案。

### 研究多人合作

1. **Recycle_Rush**：最适合看 Unity Netcode/Relay 与 DOTS/ECS 如何结合玩法。
2. **void-pulse**：最值得看共享经验、独立选卡、房主权威的规则设计。
3. **Fusion clone**：只抽取 Photon Fusion 2 联机接入思路，不把技术 Demo 当成成品架构。
4. **Emerita Survivors**：适合研究本地多人、复活和内容量，但不直接合入闭源项目。

### 面向当前项目的建议

不要整仓照搬某一个项目。更稳妥的拆分参考是：

- 单人玩法骨架：SikPang 或 Matthias。
- 数据驱动、角色/关卡/技能内容组织：Roo kit。
- 高密度单位模拟：SNKRX、`c-blob-survival`、`ecs-survivors` 只取思路并自行实现。
- 多人权威与房间：Recycle Rush、Fusion clone。
- 合作升级语义：void-pulse。

本仓库是独立 MMORPG 服务端，最终实现仍应遵守服务器权威、客户端只提交意图与展示结果、
玩法状态由 Scene / Battle 等服务端运行空间推进、跨服务通过既有协议和生命周期协作的边界。
不能因为参考项目是单机或房主权威，就把命中、掉落、升级或胜负权威迁到客户端。

## 7. 筛选边界

本报告没有把以下项目混入主榜：

- 仅是素材列表、教程文章或游戏引擎，而没有可玩的同类工程。
- 名称含 `survivor`，但实际是战略、FPS、塔防或普通生存游戏。
- 仅公开反编译代码、商业游戏数据或模组，不具备明确再分发权利。
- 玩法只有题材相似，没有俯视移动、怪潮和战斗构筑循环。

因此，这份排序不是 GitHub 全站所有带 `survivor` 字样仓库的机械排序，而是经过玩法核验后的候选排序。
