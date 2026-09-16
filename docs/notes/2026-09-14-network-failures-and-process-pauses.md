# 游戏服务器网络故障、网络分区与进程暂停：一手来源调研

> 调研与访问日期：2026-09-14。
> 历史状态：2026-09-14 完成一手来源调研与独立复核，保留正文 24 条编号证据及当日复核结论。
> 迁入日期：2026-09-15。原 Pandora-Server 目录删除后，从保留的正式稿与复核记录恢复至 mmorpg；本次未重新联网复核外部来源，网页内容及产品文档可能已更新。
> 用途：为游戏服务器的失联判断、归属接管、旧实例隔离和恢复测试提供证据。
> 边界：这是外部资料调研，不是本项目故障率测量、代码审计、部署验收或运行时安全证明。

## 结论与阅读方法

**同机房部署和设备冗余不能排除通信失败；心跳超时也不能证明旧进程已经退出。** 网络恢复后，选主、复制、积压任务和应用状态仍可能需要恢复。上述结论分别由下文数据中心研究、ZooKeeper 官方机制和厂商事故复盘支持。

对游戏服务器的含义是：接管既要保证新实例能继续服务，也要保证旧实例恢复后不能继续控制同一玩家或提交过期写入。本项目阅读入口是 [场景所有权与再入屏障方案](../design/scene-owner-reentry-barrier.md)、[EnterScene 路由与跨节点交接边界](../design/enter-scene-zone-routing.md) 和 [项目规范](../../AGENTS.md)。这些设计文档中的方案、历史状态与已实现能力须分别核对；本报告不重设心跳、租约或迁移屏障参数。

数字按证据编号登记，旁边给出标题、年份或版本、链接、原文位置和统计对象。PDF 页码统一从文件首页开始计数，必要时另列印刷页码；网页用章节和可搜索词定位，避免依赖抓取工具的临时行号。

采用规则：

- 论文读取全文；厂商案例读取官方复盘；产品机制读取官方版本文档。搜索摘要和二手文章只用作寻找原文。
- 历史事故只说明发生过什么；研究样本比例不代表本项目发生概率。
- 配置默认值、典型时长、实测样例和硬上界分别表述，不互相替代。
- 原文无法读取或统计口径未对齐的数字不进入已核实正文，列在“查不到 / 暂不采用”。
- 草稿中的材料不因被收集就自动成为项目设计结论。外部证据与本报告推论分别标注。

## 机房内网络故障研究

### Microsoft：设备冗余改善流量承载，但不能保证业务无感

**来源**：Phillipa Gill、Navendu Jain、Nachiappan Nagappan，*Understanding Network Failures in Data Centers: Measurement, Analysis, and Implications*，SIGCOMM **2011**。[Microsoft Research 原论文](https://www.microsoft.com/en-us/research/wp-content/uploads/2017/01/sigcomm11netwiser.pdf)。

| 编号 | 核实事实 | 原文位置 | 口径与限制 |
|---|---|---|---|
| D-MS-A | 网络事件样本覆盖 **2009 年 10 月至 2010 年 9 月**。 | PDF 第 3 页，§3.1。 | 是该研究的数据窗口，不能视作今天的机房故障水平。 |
| D-MS-B | 故障期间流量相对故障前流量的中位数：单链路 **65%**，合并冗余组后 **93%**。 | [PDF 第 10 页](https://www.microsoft.com/en-us/research/wp-content/uploads/2017/01/sigcomm11netwiser.pdf#page=10)，§5.1、图 16。 | 流量统计是业务影响的代理，作者缺少应用性能数据。不是请求成功率，也不是“成功屏蔽了多少次故障”。 |

这篇论文支持检查冗余在真实流量下的效果，而不是只检查“是否配置了备用链路”。不能把摘要的冗余效果措辞改写成一个故障屏蔽概率。

### Google：网络事故与业务恢复需要分别计时

**来源**：Ramesh Govindan 等，*Evolve or Die: High-Availability Design Principles Drawn from Google's Network Infrastructure*，SIGCOMM **2016**。[Google 官方书目](https://research.google/pubs/evolve-or-die-high-availability-design-principles-drawn-from-googles-network-infrastructure/)；[Berkeley 托管原论文全文](https://people.eecs.berkeley.edu/~sylvia/cs268-2019/papers/ramesh16a.pdf)。这是本次核对的 Google 论文，不能与其他年份的数据中心网络论文混用。

| 编号 | 核实事实 | 原文位置 | 口径与限制 |
|---|---|---|---|
| D-G-A | 分析 **两年内 103 份事故复盘**。 | PDF 第 5 页，§4；去重规则见第 2 页脚注 1、第 5 页脚注 4；网络范围见第 2 页 §2 The Networks。 | 覆盖数据中心网络与 B2、B4；已知重复故障不另记。这不是全部网络故障次数，更不是单机房频率。 |
| D-G-B | 样本中约 **80%** 的事件持续 **10–100 分钟**。 | PDF 第 6 页，§5 的 Duration 段；图 5 见第 8 页。 | 从发现故障到网络故障修复；业务恢复可能更晚。不是所有用户连续完全断网的时长，也没有给恢复上界。 |
| D-G-C | **近 70 起**事件发生时，出故障网元正在进行管理操作。 | PDF 第 7 页，§5；图 6 见第 8 页。 | 作者明确提醒管理操作不一定是根因；不能写成“这些故障都由运维造成”。 |

### Facebook：软件可感知的事故与所有设备故障不是一个样本

**来源**：Justin Meza 等，*A Large Scale Study of Data Center Network Reliability*，IMC **2018**。[IMC 官方原论文](https://conferences.sigcomm.org/imc/2018/papers/imc18-final179.pdf)。

| 编号 | 核实事实 | 原文位置 | 口径与限制 |
|---|---|---|---|
| D-F-A | 机房内事故分析采用 **2011–2018 年、七年**的 SEV 记录。 | PDF 第 5 页，§4.1–4.3。 | 时间跨度按作者原文记录；本节聚焦自动修复未覆盖、影响软件系统的网络事故，不是所有硬件告警。 |
| D-F-B | 根因统计中，误配置和软件缺陷合计 **25%**，硬件问题 **13%**。 | [PDF 第 6 页](https://conferences.sigcomm.org/imc/2018/papers/imc18-final179.pdf#page=6)，§4.4、§5.1、表 2。 | 一个 SEV 可对应多个根因；记录也不保证穷尽所有事故。这些比例不是设备故障概率。 |

作者在 PDF 第 3 页 §3.2 表示，当时没有观察到灾难性的整个数据中心或整个区域分区。不能据此论文声称 Facebook 经常发生整机房分区，也不能把“当时未观察到”提升为“不可能发生”。

## 网络分区的后果

### The Network is Reliable：故障模型的综述入口

**来源**：Peter Bailis、Kyle Kingsbury，*The Network is Reliable: An informal survey of real-world communications failures*，**2014**。[作者托管全文](https://www.bailis.org/papers/partitions-queue2014.pdf)；[Berkeley 出版记录](https://amplab.cs.berkeley.edu/publication/the-network-is-reliable/)。

作者汇集真实通信故障，说明失联也可能来自软件、调度和过载；材料主要是依赖具体部署的个案，难以给出统一发生率。位置：PDF 第 1–2 页导言、第 10 页 APPLICATION-LEVEL FAILURES、第 12 页 CONCLUSIONS。

本报告采用它的分类和研究限制，不采用其中转引的事故数字。数字回溯到被引论文或厂商复盘。本节采用正式论文版本，不将同名早期网页与正式论文的年份混记。

### OSDI：连接恢复不等于错误状态消失

**来源**：Ahmed Alquraan 等，*An Analysis of Network-Partitioning Failures in Cloud Systems*，OSDI **2018**。[USENIX 原论文](https://www.usenix.org/system/files/osdi18-alquraan.pdf)。

| 编号 | 核实事实 | 原文位置 | 口径与限制 |
|---|---|---|---|
| N-O-A | 分析 **25 个分布式系统中的 136 项网络分区故障**。 | 摘要，印刷 p.51 / PDF 第 2 页；§3，印刷 p.53–54 / PDF 第 4–5 页。 | 样本含公开问题记录、Jepsen 报告及作者测试发现，不应全部称为生产事故。 |
| N-O-B | 样本中 **21%** 的故障在网络分区修复后仍留下错误状态。 | §4.1 Failure Impact，Finding 3，印刷 p.55 / PDF 第 6 页。 | 是所选问题样本比例；持续错误不等于永久不可修复，也不等于每次分区造成数据损失的概率。 |

§3 的筛选方法和 Limitations 说明该研究刻意选择高影响问题、排除低优先级问题，也排除了仅靠节点崩溃就能触发的故障。因此，不能用样本比例估算游戏服的年事故率。

### GitHub：短暂断链触发长时间服务降级

**来源**：Jason Warner，*October 21 post-incident analysis*，**2018**，页面标注发布于 **2018-10-30**。[GitHub 官方复盘](https://github.blog/news-insights/company-news/oct21-post-incident-analysis/)。

| 编号 | 核实事实 | 原文位置 | 口径与限制 |
|---|---|---|---|
| N-GH-A | 网络枢纽与主要东海岸数据中心之间的通信中断 **43 秒**。 | Background 第 2 段，搜索 `43 seconds`。 | 描述该链路的中断；切换跨地域进行，但故障链路本身不能因此被称作跨地域链路。 |
| N-GH-B | 整次事故服务降级 **24 小时 11 分钟**。 | 导言第 1 段、Background 第 2 段，搜索 `24 hours and 11 minutes`。 | 是服务降级总时长，不是全站完全不可用或持续断网时长。 |

分区触发跨地域数据库切换，恢复连接时两侧分别存在另一侧没有的写入；复制恢复和积压处理进一步延长影响。位置：Background 及事故时间线中讨论切换后数据分歧、复制与积压恢复的段落。该案例不能简化成“网络断了一整天”。

## 进程仍存活，但无法及时发送心跳

### GC 与 ZooKeeper 会话过期

**来源**：Apache Geode，*Troubleshooting Garbage Collection Pauses*，最后修改 **2016-06-08**。[官方排障文档](https://cwiki.apache.org/confluence/display/GEODE/Troubleshooting%2BGarbage%2BCollection%2BPauses)。

| 编号 | 核实事实 | 原文位置 | 口径与限制 |
|---|---|---|---|
| P-GC | 文档给出的 Full GC 日志样例，应用暂停 **21.73 秒**。 | Description、Determination；搜索 `21.73 seconds` 或日志字段 `real=21.73 secs`。 | 仅是排障样例，不是 JVM 平均值、分位数或上界，也不是本项目 C++/Go/Java 服务的暂停测量。 |

该文说明长暂停可引起超时或成员被移除。Apache 的 [Troubleshooting ZooKeeper Operating Environment](https://cwiki.apache.org/confluence/spaces/ZOOKEEPER/pages/24193439/Troubleshooting)（最后修改 **2013-12-06**）在 GC pressure 一节进一步指出，GC 可使心跳线程无法及时运行，引起断连和会话过期。此处只采用机制说明，不照搬旧 JVM 的调参建议。

**来源**：Apache ZooKeeper，*ZooKeeper Administrator’s Guide*，**3.9.0** 版。[官方文档](https://zookeeper.apache.org/doc/r3.9.0/zookeeperAdmin.html)。

| 编号 | 核实事实 | 原文位置 | 口径与限制 |
|---|---|---|---|
| P-ZK | 客户端可协商 session timeout 的默认下界为 **2 × tickTime**，默认上界为 **20 × tickTime**。 | Configuration Parameters → Advanced Configuration，`minSessionTimeout`、`maxSessionTimeout`。 | 这是可配置的默认协商范围，不是不可修改的协议限制或故障测量；必须看实际协商结果。 |

同版 [ZooKeeper Programmer’s Guide](https://zookeeper.apache.org/doc/r3.9.0/zookeeperProgrammers.html) 的 ZooKeeper Sessions 说明，会话过期由集群判断；客户端超过协商时间未通信，集群会使会话过期并删除临时节点。断连客户端可能直到重新连上才知道已过期。

**机制结论**：临时节点消失、会话过期和原进程退出，是不同事实。失联进程恢复执行时，不能仅凭本地还保存着旧身份就继续操作受保护资源。

### 云主机维护暂停：保留内存不代表持续执行

**来源**：Microsoft Azure，*Maintenance for virtual machines in Azure*，页面最后更新 **2026-08-13**。[官方文档](https://learn.microsoft.com/en-us/azure/virtual-machines/maintenance-and-updates)。

| 编号 | 核实事实 | 原文位置 | 口径与限制 |
|---|---|---|---|
| P-AZ-A | Live migration 暂停通常不超过 **5 秒**。 | Maintenance that doesn't require a reboot → Live migration；搜索 `typically lasting no more than 5 seconds`。 | 典型描述；迁移保留内存且不要求重启，但属于尽力而为，不是暂停 SLA 或硬上界。 |
| P-AZ-B | 少见维护机制会使 VM 暂停**约 30 秒**。 | Maintenance that doesn't require a reboot；搜索 `about 30 seconds`。 | 特定维护机制的约数，不能写成所有 Azure VM 的最大暂停时间。 |

**来源**：Google Cloud，*Live migration process during maintenance events*，页面最后更新 **2026-09-03 UTC**。[官方文档](https://docs.cloud.google.com/compute/docs/instances/live-migration-process)。

| 编号 | 核实事实 | 原文位置 | 口径与限制 |
|---|---|---|---|
| P-G-A | 支持 live migration 的场景中，短暂 disruption **通常远小于 1 秒**。 | How does the live migration process work?；搜索 `much less than 1 second`。 | 是典型 disruption，不是整个迁移耗时、统计分位数或所有机型的保证。 |
| P-G-B | 文档规定 blackout **超过 5 秒**时的时钟同步处理。 | 同节 Blackout；搜索 `exceeds 5 seconds`。 | 这是时钟处理阈值，不能反读成暂停上界，也不能据此估计长暂停频率。 |

Google 在 Blackout 段描述 VM 会暂停、暂时不在任何宿主机上运行，之后恢复。整机暂停可影响不同语言的进程；因此不能以游戏服不是 Java 为由排除该故障模型。

## 云厂商官方事故复盘

各案例分别标明 AZ、区域、跨区域或机架范围。**AZ 不自动等同单个物理机房；有业务影响也不自动等同脑裂。**

### AWS：可用区内变更与区域控制面拥塞

**来源**：AWS，*Summary of the Amazon EC2 and Amazon RDS Service Disruption in the US East Region*，**2011-04-29**。[官方复盘](https://aws.amazon.com/message/65648/)。

| 编号 | 核实事实 | 原文位置 | 口径与限制 |
|---|---|---|---|
| C-AWS-A | 副本重建风暴形成后，受影响可用区约 **13%** 的 EBS 卷当时处于 stuck 状态。 | Primary Outage，描述 re-mirroring storm 后的一段；搜索 `about 13%`。 | 分母为受影响 AZ 内的卷；这是该时点的状态，不是整次事故的累计受影响比例，不是 AWS 全部卷，也不是全部数据丢失。 |

Primary Outage 说明，错误流量迁移使部分 EBS 节点主备网络同时失去可用连接，网络恢复后又发生副本重建风暴。Overview of EBS System 说明，重新确定可写主副本期间会阻塞访问，控制面负责仲裁主副本。这里观察到的是可用性与恢复连锁问题，不能把“节点认为对方失效”直接写成“已发生双主写”。

**来源**：AWS，*Summary of the AWS Service Event in the Northern Virginia (US-EAST-1) Region*，**2021-12-10** 发布，事件发生于 **2021-12-07**。[官方复盘](https://aws.amazon.com/message/12721/)。

| 编号 | 核实事实 | 原文位置 | 口径与限制 |
|---|---|---|---|
| C-AWS-B | 内部网络拥塞事件从 **7:30 AM PST** 开始；受影响网络设备到 **2:22 PM PST** 全部恢复。 | Issue Summary，自动扩容触发段及 `all network devices fully recovered` 段。 | 是网络设备恢复时间线；各项业务恢复时间不同，不能称全部 AWS 业务已恢复。 |

主网络与内部基础服务网络之间的连接激增，引起延迟和重试放大；监控与部署工具也受到影响。AWS Service Impact 区分了仍在运行的实例与受损的创建/管理 API。这说明控制面故障可以使修复与扩容变慢，不能只检查游戏进程当前是否还在跑。

### Google Cloud：维护自动化把影响传播到多个位置

**来源**：Google Cloud，*Google Cloud Networking Incident #19009*，事件 **2019-06-02**，详细复盘条目 **2019-06-06**。[官方复盘](https://status.cloud.google.com/incident/cloud-networking/19009)。

| 编号 | 核实事实 | 原文位置 | 口径与限制 |
|---|---|---|---|
| C-GCP | 受影响区域网络拥塞、丢包升高持续 **3 小时 19 分钟至 4 小时 25 分钟**。 | ISSUE SUMMARY 第 1 段。 | 不同区域时长和丢包程度不同；不是全球所有服务完全中断的统一时长。 |

ROOT CAUSE AND REMEDIATION 描述维护配置与自动化缺陷叠加，使多个物理位置的网络控制任务被停止；网络拥塞也影响诊断工具，重建和分发配置进一步延长恢复。页面顶端的概括时间不能替代详细复盘的分区域口径。

### Azure：单路由器上的操作可以影响全局 WAN

**来源**：Microsoft Azure，*Post Incident Review (PIR) – Azure Networking – Global WAN issues*，**2023-01-25**，跟踪号 VSG1-B90。[官方复盘](https://azure.status.microsoft/status/history/?trackingId=VSG1-B90)。

| 编号 | 核实事实 | 原文位置 | 口径与限制 |
|---|---|---|---|
| C-AZ | 影响窗口为 **07:08–12:43 UTC**；大多数区域和服务到 **09:05 UTC** 已恢复。 | What happened? 前两段。 | 剩余路径仍可能丢包，不能把影响窗口写成每位客户持续完全断网。 |

What went wrong and why? 说明一个命令在不同厂商路由器上的作用范围不同，导致全网重新计算可达性。操作权限只限定在单设备，并不保证影响只停留在单设备。

### Cloudflare：机架内部分通信失败及网络配置共因故障

**来源**：Tom Lianza、Chris Snook，*A Byzantine failure in the real world*，**2020-11-27**，复盘事件 **2020-11-02**。[官方复盘](https://blog.cloudflare.com/a-byzantine-failure-in-the-real-world/)。

| 编号 | 核实事实 | 原文位置 | 口径与限制 |
|---|---|---|---|
| C-CF-A | 异常交换机 **6 分钟**后自行恢复；API 与 dashboard 可用性受影响 **6 小时 33 分钟**。 | Partial Switch Failure 末段；导言第 2 段。 | 前者是交换机异常时长，后者是控制面业务影响。边缘服务继续工作，不能称全网不可用。 |

机架交换机部分转发失效，使 etcd 节点看到不同连通关系；选举反复导致不能写入，触发数据库提升和副本重建，随后出现负载问题。Database system promotes a new primary database 明确描述此次提升没有错误或数据丢失。**这不是 Raft 已确认双主或已确认数据丢失的证据。** 标题保留原文名称；作者 Postscript 后记修正此场景应称 omission fault，本文用“部分通信失败”描述。

**来源**：Tom Strickx、Jeremy Hartman，*Cloudflare outage on June 21, 2022*，**2022-06-21**。[官方复盘](https://blog.cloudflare.com/cloudflare-outage-on-june-21-2022/)。

| 编号 | 核实事实 | 原文位置 | 口径与限制 |
|---|---|---|---|
| C-CF-B | 故障影响 **19 个数据中心**，从 **06:27 UTC** 开始，到 **07:42 UTC** 所有受影响机房恢复。 | Introduction 首段及 Incident timeline and impact。 | 是这次事故的机房恢复时间线，不是未来故障的时长上界。 |
| C-CF-C | 原文称这些位置约占其总网络 **4%**，事故影响 **50%** 的总请求。 | Incident timeline and impact，成功 HTTP 请求图之后一段。 | total network 的具体分母未在该句进一步定义；不能改写成设备比例。请求占比也不是客户占比或全球互联网占比。 |

网络前缀策略的顺序变更导致重要路由撤回，同时增加恢复操作的困难。该事件说明，设备/地点数量少不代表承载的业务流量少；分批变更应覆盖实际架构类型和流量权重。

## 对游戏服务器的设计推论与验证建议

以下是结合证据提出的应用解释，**不是论文对 mmorpg 实现的测试结论**。原报告的 DS 指游戏专用服务器；迁入本项目后，玩家权威状态的讨论对应 C++ scene 节点，不能把所有 battle 进程直接等同于玩家数据 owner。

### 本项目阅读入口与适用范围

- [跨服架构原则](../design/cross_server_architecture_principle.md) 的 single writer 要求用于约束同一玩家的写入者。
- [场景所有权与再入屏障方案](../design/scene-owner-reentry-barrier.md) 讨论 etcd 失联后的改派、旧节点存盘和 owner_epoch 校验。本报告不把其中的时间预算视为进程暂停或在途请求的硬上界，也不把方案落档视为已经实现。
- [EnterScene 路由与跨节点交接边界](../design/enter-scene-zone-routing.md) 记录已有位置的跨节点交接安全闸、持久化确认和 handoff epoch 前提。本次资料迁移不解除该安全闸。
- 项目相关参数、协议和实现状态仍以 [AGENTS.md](../../AGENTS.md)、对应设计、源码及运行证据为准。本次只做文档迁移与适配，未审计或验收这些链路。

### 同机房与单机 Demo 应如何理解

- 同机房仍可能有交换机部分失效、共享网络配置错误、存储不可达和过载；可参考 Microsoft、Facebook 和 Cloudflare 机架案例。跨区域复盘另外说明影响传播与恢复成本，不能混入单机房发生率。
- 同一宿主机上的不同进程也可能出现只有旧游戏进程暂停、协调进程仍可接管的情况；云维护还可能让整个 VM 暂停。具体是否形成旧新并存，取决于协调者所在位置和接管流程。
- 若确实只有单个进程、完全没有并发副本或接管者，双所有者问题的前提不同。但一旦支持自动重启、重新分配或旧实例恢复，就必须重新检查这些前提；“Demo”本身不是安全证明。

### 接管时要保证的业务条件

- **未知不等于离线**：心跳超时、locator 缺失、管理 API 报错可触发诊断或恢复流程，但不能单独证明旧 scene 实例已退出。
- **归属需要唯一权威**：归属更新、owner epoch 与精确实例身份应有明确的原子提交边界，租约和迁移屏障须与该权威一致；这是后续设计与验证条件，不代表本项目已完成整条协议。
- **旧执行者必须失去操作能力**：旧 scene 实例失租、到期或续租结果不确定时，需要按所有权协议隔离；暂停恢复后须在处理玩家输入和业务写前检查有效身份。只停止接收新玩家不能阻止已有玩家继续产生写入。
- **实际写入端拒绝旧身份**：资源写入必须校验有效 owner epoch/身份并与写操作保持原子关系；协调服务已经选出新 owner，不会自动撤销旧进程已经发出的请求。
- **恢复不重复生效**：奖励、资产、结算和迁移沿稳定操作标识幂等处理；请求超时后可能已成功，重试不能重复发奖或覆盖新状态。
- **恢复工具也要能工作**：监控、配置发布、扩容和回滚入口应纳入故障域分析；业务错误和重试需要有界退避与限流，避免恢复期间继续放大拥塞。

### 建议故障演练矩阵（本次未执行）

| 场景 | 注入顺序 | 通过条件 |
|---|---|---|
| 旧 scene 实例暂停后恢复 | 暂停旧实例，令其失租；新实例完成允许的接管；恢复旧实例并发送输入和写请求。 | 旧实例不能继续操控玩家；旧身份写入被拒；同一玩家不出现并行可玩实例。 |
| 部分网络分区 | 让旧 scene 实例无法访问 owner 权威，同时保留它到玩家或部分业务服务的连接。 | 不能因仍能收玩家输入就继续作为 owner；新实例遵守迁移屏障。 |
| 请求执行成功但回包丢失 | 对归属迁移、结算或奖励操作丢弃响应，随后以相同操作标识重试。 | 不重复占座、不重复创建 owner、不重复发奖；能查到真实结果。 |
| 网络恢复与积压重放 | 修复分区，恢复旧连接，送达迟到心跳、注销、写入及积压任务。 | 新状态不回退，旧身份不能删除或续租新身份；积压恢复可观察且不重放副作用。 |
| 控制面故障 | 令监控、配置或创建实例 API 不可用，再观察业务和恢复入口。 | 记录真实未知状态；不会把现存实例批量判死，也不会无限重试压垮依赖。 |

需要项目自己的时间线、运行时停顿数据、身份日志和持久化结果才能判断通过。引用云厂商典型暂停时间不能代替这些验证。

## 查不到 / 暂不采用

下表明确记录没有采用的主张；其中出现的数字是待否定或待澄清对象，不属于已核实正文结论。

| 主张或入口 | 查证结果 | 处理 |
|---|---|---|
| Microsoft“冗余只屏蔽 40% 的故障” | 原论文确有冗余效果措辞，但不能换成故障事件屏蔽率；§5.1 给出的是流量代理指标。 | 正文改用 D-MS-B，不写该屏蔽率。 |
| Google“70% 的故障由运维造成” | 原文是近 70 起事件发生时存在管理操作，明确不保证因果。 | 使用 D-G-C 的计数和相关性表述。 |
| OSDI“约 80% / 79.5% 灾难性故障” | §4.1 与表 2 的比例，和表 1 总计 104/136 未在本次核查中对齐；位置为印刷 p.54–55 / PDF 第 5–6 页。 | 暂不采用；不能自行拼分母或据此算本项目概率。 |
| “AWS EC2 live migration 最多暂停某个秒数” | [How host maintenance works for Amazon EC2 Dedicated Hosts](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/dedicated-hosts-maintenance-basics.html) 的 Live migration host maintenance 没有给出冻结秒数。 | 不写 AWS 暂停数字；迁移调度窗口不是冻结时长。 |
| “GC 普遍/最多会暂停若干分钟” | 取得官方长暂停机制和 P-GC 样例，但未取得支持统一分钟值的完整测量口径。 | 不采用统一时长或最大值。 |
| “云主机暂停最长只有 5 秒或 30 秒” | Azure 使用典型值、约数；Google 还明确描述超过阈值时的处理。 | 不提升为上界或项目租约建议。 |
| “MTBI 倒数年化后就是单设备年故障概率” | 这是事件率与概率混用；转换需要模型假设与明确暴露时间。 | 不照搬草稿的概率推导。 |
| “只有部分分区产生脑裂”“整个交换机失效不会脑裂” | 本次资料不能支持这种绝对命题；后果取决于拓扑、仲裁和写入隔离。 | 不采用。 |
| “本项目某个固定心跳租约足够安全” | 没有本项目停顿分布、故障注入及旧写入隔离验证。 | 不作参数安全结论，继续以现有设计和实际测试为准。 |
| Google 论文 ACM 全文入口 | 本次读取失败；已核官方书目，并打开 Berkeley 托管原论文。 | 不属于全文缺失；正文注明实际读取入口。 |
| The Network is Reliable 的 ACM 页面 | 本次读取受限；已打开作者托管完整 PDF 并核对出版记录。 | 不以搜索摘要代替全文，也不把入口失败写成原文不存在。 |

## 归档、草稿与复核记录

原项目曾有 `_notes-split-brain` 下的 `dc-network.md`、`partitions.md`、`process-pause.md`、`cloud-postmortems.md` 四份检索草稿；这些草稿未随本次恢复迁入，因此不保留指向已删除目录的链接。

这些是检索过程材料，不是已经统一口径的设计规范。**2026-09-14 的逐项复核范围是本报告采用的事实，不等于把四份草稿全部认证通过。** 草稿中的概率外推、脑裂案例归类、“仅不接新活即可”、以及绕开 owner 权威的建议不进入正式结论。

历史审计见 [独立证据复核记录](./2026-09-14-network-failures-and-process-pauses-verification.md)。该记录与本报告同时恢复；其中“重新打开原文”“通过”等措辞均指 2026-09-14 的工作。

复核方式：独立复核人员重新打开上述原文，逐项检查事实数字、单位、分母、时间范围、页节位置与措辞；有问题就更正，无法对齐则移出正文。研究作者、原文标题、出版年份、文档版本和网页更新时间也随来源核对。

复核结果（2026-09-14）：**24/24 条编号证据通过，11/11 项排除清单处理通过**；标题、作者、年份、版本及页面更新时间也已核对。这里的通过仅表示本报告的文献转述有原文支持。

| 复核分组 | 编号范围 | 结果 |
|---|---|---|
| 数据中心研究 | D-MS-A/B、D-G-A/B/C、D-F-A/B | 7 条通过 |
| 分区后果 | N-O-A/B、N-GH-A/B | 4 条通过 |
| 进程与 VM 暂停 | P-GC、P-ZK、P-AZ-A/B、P-G-A/B | 6 条通过 |
| 云厂商复盘 | C-AWS-A/B、C-GCP、C-AZ、C-CF-A/B/C | 7 条通过 |

复核过程中修正了以下问题，并重新核对修订结果：

- D-G-A：Google 论文关于去重的脚注曾被误标为网络范围出处，已分别列出正确位置。
- C-AWS-A：AWS 卷受影响比例明确为重建风暴后某一时点的状态，避免被误解为事故累计比例。

正文采用的数值没有剩余无出处项；无法读取的入口、未对齐的统计和不受支持的外推仍保留在排除清单中。

### 迁移记录（2026-09-15）

- 原归档路径：`D:/luyuan/Pandora-Server/docs/research/2026-09-14-network-failures-and-process-pauses.md`，原目录已删除。
- 恢复来源：`D:/luyuan/work/.research-staging/network-partition-pause-evidence-2026-09-14.md`；SHA256：`EF603BC5A3A3E7140D4FCB602DFBB58CDA6D22A25C68C07229010A129101F719`。
- 独立复核来源：同目录的 `verification-audit.md`；SHA256：`94645FE667AD2C239D87C500852D36D96615F8EEA26886F4E3F61CE01F617877`。
- 保留原报告的证据编号、数字、统计口径、外部来源及排除清单；调整项目设计链接、服务称谓和适用范围，移除未恢复草稿的失效链接。
- 本次验证范围为恢复内容一致性、24 条证据编号与复核记录对应关系、11 项排除清单和新增本地链接；外部来源的访问状态和内容未重新验证。

完成边界：本次仅添加研究文档、历史复核记录与文档入口；未修改游戏服务逻辑、未调整运行参数、未执行故障演练、未提交版本库。
