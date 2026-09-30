# K8s Gate Exposure Guidance / K8s gate 暴露方式指南

> **2026-09-29 重写为「每实例入口」口径**(集群外入口 D76–D93)。权威设计见 `docs/design/k8s-client-entry.md`(D87–D90、「运维手册」),
> 部署参数见 `deploy/k8s/README.md` Optional Flags「集群外客户端入口」。本文件与 `_zh.md` 同内容(中文为准),`_en.md` 为英文版。
> 文末「原口径」保留 2026-09-29 之前的四条原文并逐条标注。以下全部**未上集群验证**,待 kind 端到端验收(Codex / 用户执行)。

## 1. 结论:gate 入口必须是每个实例一个

- gate 是有状态长连接,login 为玩家选定**一台** gate 并签发绑定该 gate 的票据:`GateTokenPayload.gate_node_id`
  (`proto/common/base/message.proto:219-220`)。gate 验票时与自身 node_id 不符即以 `token_gate_node_mismatch` 断开
  (`cpp/nodes/gate/handler/rpc/client_message_processor.cpp:1052-1057`)。所以客户端**必须连到 login 选中的那一台 gate**,
  任何在多台 gate 之间分流的入口(单一 Service、L4 LB 的后端池)都会让约一半票据被拒(副本数为 2 时)。
- **单一 Service 只适用于单副本**:`gate-entry` 只在 `-ClientEntryMode podip` 且 gate 副本数恰为 1 时生成(D90;
  `tools/scripts/lib/k8s_client_entry.ps1:509` `Test-GateEntryServiceWanted`,`tools/scripts/k8s_deploy.ps1:4994-4999`),其余情形不生成并清掉残留。
  而且 podip 下 login 下发的是 gate 注册在 etcd 里的 PodIP,`gate-entry` 本身并不能让集群外客户端连上(`k8s_deploy.ps1:865-870` 的警告)。
- **集群外玩家一律用 `-ClientEntryMode external`**:gate 为 StatefulSet(不进 Agones,D87),每个序号一个 Service `gate-<i>`,
  selector 带 `statefulset.kubernetes.io/pod-name`,只命中同序号 Pod(`lib/k8s_client_entry.ps1:864-909` `New-GateOrdinalServicesYaml`)。
  gate 在发布 etcd 之前自报客户端可达地址 `NodeInfo.client_endpoint`(D76,`proto/common/base/common.proto:33`),
  login / scene_manager 下发的就是它(`go/shared/clientendpoint/select.go:41` `Select`,按有效地址去重只留 launch_time 最大者)。
- 不要写"给 gate 挂一个 LoadBalancer / Service 就能对外"。这句话在单副本之外都是错的。

## 2. `-GateServiceType` 怎么选(external 下决定每个 `gate-<i>` 的类型)

| 集群 | `-GateServiceType` | gate 自报的客户端地址 | 要求 |
|---|---|---|---|
| 托管云,有成熟 LB 实现 | `LoadBalancer`(每个 gate 一个 LB) | `-GateClientHostTemplate` 渲染出的 DNS 名 + 18000 | 模板必填;副本 >1 时必须含 `{ordinal}`;生成 external-dns 注解 |
| 自建 / 裸金属 | `NodePort` | 主机:模板 > `-ClientPublicHost` > 节点 `status.hostIP`;端口:`gateNodePortBase + 序号` | 每个 zone 显式配 `gateNodePortBase`,段不重叠,建议 30000–30085;多节点下自报地址须落到 Pod 所在节点(见下方 D89 前提) |

- 地址计算的唯一实现:`lib/k8s_client_entry.ps1:408` `Resolve-GateClientEndpointPlan`(D88)。
- 不要把 `LoadBalancer` 当通用默认:external 下它是"每个 gate 一个 LB",成本与配额随 gate 副本数线性增长;集群没有成熟 LB 实现时用 `NodePort`。
- `-GateExternalTrafficPolicy` 默认 `Local`(D89):保留玩家真实源 IP,这是 gate 按源限流的前提(`gate-connection-admission-control.md` G9);
  `Cluster` 会 SNAT,preflight 警告。`Local` + NodePort 要求 gate 自报的节点地址对客户端可达,多节点下做不到就改用 LoadBalancer + DNS 模板。
- **NodePort + `Local` 的多节点前提(D89)**:每个序号自报的主机必须把流量送到**该序号 Pod 所在的节点**,否则到达节点上没有本地 Pod、流量被直接丢弃。
  主机取 `status.hostIP` 或按 `{ordinal}` 渲染的 DNS 模板时天然满足;**给了 `-ClientPublicHost`(单一主机)时所有序号自报同一个主机**,多节点下只有该主机所在节点、
  或其背后按 nodePort 逐序号正确转发到 Pod 所在节点的 gate 可达。preflight 对这种组合不告警,单节点(如 kind)之外请用 DNS 模板或 LoadBalancer
  (`gate-connection-admission-control.md` §4.2.1)。LoadBalancer 能否保留源 IP 还取决于云 LB 是否直通:代理型 LB 需要 PROXY protocol,gate 不支持。
- 命令里仍建议显式写 `-OpsProfile`:`managed-cloud` 强制 LoadBalancer(因此 external 必须给模板),`bare-metal` 强制 NodePort,`custom` 未显式给时解析为 NodePort。
- 防火墙只放行 gate 的 nodePort 段(或各 LB)、battle 的 Agones 端口段与 gateway Ingress 的 80/443;gRPC 端口(TCP+30000)永不对外。
- battle 同一口径:直连地址也是每实例一个(Agones Fleet `portPolicy: Dynamic` 分到的 hostPort,或验证用的 hostPort 20000 Deployment),不经 Service、不做 LB(D81 / D86)。
- 滚动与排空:external 的 gate StatefulSet 是 `OnDelete` + PDB `maxUnavailable: 0`,逐台走 `tools/scripts/k8s_gate_drain.ps1`,不要直接删 Pod。

## 3. 原口径(2026-09-29 之前,原文保留)

- Gate external exposure rule: managed cloud K8s prefers LoadBalancer; self-hosted / bare metal K8s prefers NodePort plus external L4 load balancer.
  —— **(2026-09-29 更正)** Service 类型的取舍仍成立,但"NodePort 前面再挂一个外部 L4 负载均衡器"只在它**按实例透传**(每个 nodePort 一条监听、不跨 gate 分流)时才可用;
  把多台 gate 放进同一个后端池会触发 `token_gate_node_mismatch`。该类型在 external 下作用于每序号 Service,podip 下只决定单副本 `gate-entry` 的类型。
- Do not recommend LoadBalancer as a universal default when the cluster lacks a mature LB implementation.
  —— 仍成立。
- Prefer explicit OpsProfile choices (`managed-cloud` or `bare-metal`) in examples and operational docs.
  —— 仍成立;注意 managed-cloud + external 必须给 `-GateClientHostTemplate`。
- Script baseline default for `custom` profile is `GateServiceType=NodePort`; `managed-cloud` profile still forces `LoadBalancer`.
  —— 仍成立;`dev_tools.ps1` 与 `k8s_image.ps1` 的 `-GateServiceType` 默认已改为留空、跟随 `k8s_deploy.ps1`(原先 `k8s_image.ps1` 默认 LoadBalancer)。
