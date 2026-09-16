# 网络故障与进程暂停调研：独立证据复核

> 历史复核记录，2026-09-15 从保留的 `verification-audit.md` 恢复至 mmorpg。下文结论与访问状态均截至 2026-09-14，本次未重新联网复核；不包含迁移时新增的 mmorpg 阅读入口与适用范围说明。

复核日期：2026-09-14。原复核对象：`network-partition-pause-evidence-2026-09-14.md`，原归档目标为 Pandora-Server 的 `docs/research/2026-09-14-network-failures-and-process-pauses.md`。迁入后的 [正式报告](./2026-09-14-network-failures-and-process-pauses.md) 保留原有证据与排除清单。

结论：正式稿中的 **24 个编号证据均有可读原文支持**。首轮发现一处脚注定位错误、一处瞬时比例可能被误读为累计比例；汇总者已修正，复核人员已重新读取修订行确认。其余事实数字、单位、分母、时间范围及引用位置通过。没有发现需要删除的无出处正文数字。

本复核重新打开正式稿使用的原文，逐项读取对应段落、表格文字与图注；没有只依据其他 agent 的摘要判断。额外核对了标题、作者、年份、产品版本、网页更新时间、定性论述与排除清单。这里的“通过”仅表示文献转述得到证据支持，不表示四份原始草稿、游戏服务实现或故障演练已通过。

## 编号证据清单

| 编号 | 复核结果 | 原文定位及核查重点 |
|---|---|---|
| D-MS-A | 通过 | Microsoft 托管 PDF 第 3 页 §3.1：2009 年 10 月至 2010 年 9 月。 |
| D-MS-B | 通过 | 同 PDF 第 10 页 §5.1、图 16：先按事件计算故障期间/故障前的中位流量比，再呈现归一化字节量分布；单链路 65%、冗余组 93%。不是事件屏蔽率。 |
| D-G-A | 修正后通过 | Berkeley 原文 PDF 第 5 页 §4 给两年和 103；第 2 页脚注 1、第 5 页脚注 4 给唯一故障/不完整频率样本；第 2 页 §2 给网络范围。原稿将脚注 1 误标为网络范围，已改正。 |
| D-G-B | 通过 | 同 PDF 第 6 页 §5 Duration 给约 80%、10–100 分钟；图 5 确在第 8 页。原文明确某些情况下业务恢复不计入事件时长。 |
| D-G-C | 通过 | 同 PDF 第 7 页 §5 给近 70 起及不必然为根因；图 6 在第 8 页。计数、比例和因果没有混用。 |
| D-F-A | 通过 | IMC 官方 PDF 第 5 页 §4.2、§4.3 给 2011–2018 年、七年；同页描述 SEV 样本。保留作者时间跨度原话，不另作年数推算。 |
| D-F-B | 通过 | 同 PDF 第 6 页 §5.1 直接写 25% 与硬件 13%；表 2 同时列误配置 13%、bug 12%。该节明确一个 SEV 可计入多个根因类别；§4.4 说明报告可能不穷尽。 |
| N-O-A | 通过 | OSDI 原文摘要 PDF 第 2 页/印刷 p.51，§3 PDF 第 4–5 页/印刷 p.53–54：25 个系统、136 项故障；有问题记录、Jepsen 和作者测试发现。 |
| N-O-B | 通过 | 同 PDF 第 6 页/印刷 p.55，§4.1 Finding 3：21% 留下网络恢复后仍持续的错误状态。正文没有擅自声称永久不可修复。 |
| N-GH-A | 通过 | GitHub 复盘 Background 第 2 段：43 秒是东海岸网络枢纽与主要数据中心间连接恢复耗时。 |
| N-GH-B | 通过 | 同文导语第 1 段和 Background 第 2 段：24 小时 11 分钟为服务降级时长。 |
| P-GC | 通过 | Apache Geode Determination：21.73 秒是 Full GC 日志样例，日志 real 字段与正文一致。 |
| P-ZK | 通过 | ZooKeeper 3.9.0 Administrator’s Guide 的 minSessionTimeout/maxSessionTimeout：默认 2×/20×tickTime，属于可配置协商范围。 |
| P-AZ-A | 通过 | Azure Maintenance 文档 Live migration：通常不超过 5 秒，明确是典型值及尽力而为。 |
| P-AZ-B | 通过 | 同文 Maintenance that doesn't require a reboot：少见机制暂停约 30 秒，不能改为上界。 |
| P-G-A | 通过 | GCP live migration 文档 How does the live migration process work?：短暂 disruption 通常远小于 1 秒。 |
| P-G-B | 通过 | 同节 Blackout：超过 5 秒触发时钟同步处理；不是允许暂停的上界。 |
| C-AWS-A | 修正后通过 | AWS 2011 Primary Outage：重建风暴后该时点约 13% 的受影响 AZ 卷 stuck。后文同时有新增 stuck 与恢复，不能用“曾”表示整个事故累计占比；已改为时点快照。 |
| C-AWS-B | 通过 | AWS 2021 Issue Summary：7:30 AM PST 触发拥塞，2:22 PM PST 网络设备全恢复。AWS Service Impact 另述各业务恢复，不是全业务同时恢复。 |
| C-GCP | 通过 | GCP #19009 的 2019-06-06 详细更新，ISSUE SUMMARY 首段：3 小时 19 分钟至 4 小时 25 分钟，因区域而异。 |
| C-AZ | 通过 | Azure VSG1-B90 What happened? 前两段：07:08–12:43 UTC；多数区域与服务到 09:05 UTC 恢复。 |
| C-CF-A | 通过 | Cloudflare 2020 导语第 2 段及 Partial Switch Failure 末段：6 小时 33 分钟为 API/dashboard 影响，6 分钟为交换机自行恢复。 |
| C-CF-B | 通过 | Cloudflare 2022 Introduction 前两段及时间线：19 个数据中心，06:27 UTC 开始，07:42 UTC 所有受影响机房恢复。 |
| C-CF-C | 通过 | 同文 Incident timeline and impact，HTTP 请求图后：4% total network、50% total requests。正文保留了 total network 分母未细定义的限制。 |

## 定性论述和元数据

- Microsoft 论文题名、作者与 SIGCOMM 2011 见 PDF 首页；研究者明确缺少应用性能数据，支持正文采用“流量代理”的限制。
- Google 论文题名、Ramesh Govindan 等作者及 SIGCOMM 2016 与 Google 官方书目一致。两年/独特事故样本、网络范围和恢复时长定义均已核对。
- Facebook 论文题名、Justin Meza 等作者及 IMC 2018 与官方 PDF 一致；第 3 页 §3.2 的当时未观察到灾难性整机房/区域分区表述得到原文支持。
- The Network is Reliable 正式 PDF 首页题名和作者、尾页版权年份，以及 Berkeley 出版记录均支持 2014 年版本。导言在第 1–2 页，APPLICATION-LEVEL FAILURES 在第 10 页，CONCLUSIONS 在第 12 页；调度、程序错误、过载和个案不易推广的转述均得到对应段落支持。
- OSDI 2018 的题名、作者和年份，以及 §3 的高影响问题筛选、排除仅节点崩溃即可触发问题、非生产案例组成均已核对。
- GitHub 复盘发布日 2018-10-30 正确，页面另有 2021-12-19 更新日期。时间线确实描述两侧各有对方未持有的写入，正文没有把它归结为持续断网。
- Geode 修改日 2016-06-08、ZooKeeper 排障修改日 2013-12-06，以及 ZooKeeper Admin/Programmer 文档 3.9.0 路径和机制均正确。Programmer’s Guide 明确会话由集群过期、临时节点删除及失联客户端可能延后获知。
- Azure VM 文档题名和 2026-08-13 更新日期、GCP live migration 文档题名和 2026-09-03 UTC 更新日期均已核对。
- AWS 两篇、GCP #19009、Azure VSG1-B90、Cloudflare 两篇的题名、发布/事件日期及作者均核对。AWS 主备网络失联和重建风暴、AWS 控制面重试/监控受损、GCP 跨位置维护自动化、Azure 路由器命令传播的定性转述均有相应官方复盘段落支持。
- Cloudflare 2020 原文记录 etcd 写不可用，数据库提升没有错误或数据丢失；Postscript 明确将场景从 Byzantine fault 更正为 omission fault。正式稿已保留这一更正，没有把可用性问题写成已证实 Raft 双主或数据丢失。
- 设计推论与故障矩阵均明确标记为本报告推论/本次未执行；不属于外部论文已验证的项目行为。

## “查不到 / 暂不采用”清单

11 行处理均合理。Microsoft 40% 不可改作故障屏蔽率；Google 近 70 起不等于 70% 也不证明运维因果；OSDI 表 1 的 104/136 与表 2 的 79.5% 确实未在报告中给出统一解释，暂不采用合理。

AWS Dedicated Hosts 的 Live migration host maintenance 节未给冻结秒数；其中迁移时间窗不可当暂停时长。统一 GC 分钟上界、云 VM 5/30 秒硬上界、把 MTBI 倒数视为概率、按分区类型绝对判定脑裂、直接判定本项目租约安全，都不应由现有证据推出。这里的否定是“不得到所引用证据支持”，不是证明所有其他来源都不存在。

复核人员另从 Google 官方书目尝试打开 ACM 下载入口、从 Berkeley 书目尝试打开 ACM Queue 入口，均未成功取得正文；替代的 Berkeley/作者托管 PDF 可读，正式稿对入口失败与全文可读的区分正确。

## 已修正问题

1. D-G-A 脚注 1 的含义与网络范围出处混淆：已修正并重读。
2. C-AWS-A 时点 13% 的表述可能被理解为整事故累计受影响卷比例：已修正并重读。

最终统计：24/24 编号证据通过，11/11 排除清单处理通过；未留待修订的证据问题。复核未修改正式报告或仓库文件，仅新增此审计文件。
