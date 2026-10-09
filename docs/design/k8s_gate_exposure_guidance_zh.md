# K8s Gate 暴露方式指南

> **2026-09-29 重写为「每实例入口」口径**(集群外入口 D76–D93)。权威设计见 `docs/design/k8s-client-entry.md`(D87–D90、「运维手册」),
> 部署参数见 `deploy/k8s/README.md` Optional Flags「集群外客户端入口」。与 `k8s_gate_exposure_guidance.md` 同内容。
> 文末「原口径」保留旧四条原文并逐条标注。以下全部**未上集群验证**,待 kind 端到端验收(Codex / 用户执行)。

## 1. 结论:gate 入口必须是每个实例一个

- login 为玩家选定一台 gate,签发的票据绑定 `GateTokenPayload.gate_node_id`(`proto/common/base/message.proto:219-220`);
  gate 与自身 node_id 不符即以 `token_gate_node_mismatch` 断开(`cpp/nodes/gate/handler/rpc/client_message_processor.cpp:1052-1057`)。
  客户端必须连到被选中的那一台,任何在多台 gate 间分流的入口(单一 Service、L4 LB 后端池)都会让大量票据被拒。
- **单一 Service 只适用于单副本**:`gate-entry` 只在 `-ClientEntryMode podip` 且 gate 副本数恰为 1 时生成(D90;
  `tools/scripts/lib/k8s_client_entry.ps1:509`,`tools/scripts/k8s_deploy.ps1:4994-4999`)。podip 下 login 下发 PodIP,它也不能让集群外客户端连上。
- **集群外玩家一律用 `-ClientEntryMode external`**:gate 为 StatefulSet(不进 Agones,D87)+ 每序号 Service `gate-<i>`
  (selector 带 `statefulset.kubernetes.io/pod-name`,`lib/k8s_client_entry.ps1:864-909`);gate 在发布 etcd 前自报 `NodeInfo.client_endpoint`
  (D76,`proto/common/base/common.proto:33`),login / scene_manager 下发它(`go/shared/clientendpoint/select.go:41`)。

## 2. `-GateServiceType` 怎么选(external 下决定每个 `gate-<i>` 的类型)

| 集群 | `-GateServiceType` | gate 自报的客户端地址 | 要求 |
|---|---|---|---|
| 托管云,有成熟 LB | `LoadBalancer`(每个 gate 一个 LB) | `-GateClientHostTemplate` 渲染的 DNS 名 + 18000 | 模板必填;副本 >1 时必须含 `{ordinal}` |
| 自建 / 裸金属 | `NodePort` | 主机:模板 > `-ClientPublicHost` > 节点 `status.hostIP`;端口 `gateNodePortBase + 序号` | 每 zone 显式配 base,段不重叠,建议 30000–30085;多节点下自报地址须落到 Pod 所在节点(见下方 D89 前提) |

- 地址计算唯一实现:`lib/k8s_client_entry.ps1:408` `Resolve-GateClientEndpointPlan`(D88)。
- 不要把 LoadBalancer 当通用默认(external 下 LB 数随 gate 副本线性增长);没有成熟 LB 实现时用 NodePort。
- `-GateExternalTrafficPolicy` 默认 `Local`(D89),保留玩家源 IP,是 gate 按源限流(`gate-connection-admission-control.md` G9)的前提;`Cluster` 会 SNAT,preflight 警告。
- **NodePort + `Local` 的多节点前提(D89)**:每个序号自报的主机必须把流量送到该序号 Pod 所在的节点,否则到达节点上没有本地 Pod、流量被直接丢弃。
  主机取 `status.hostIP` 或按 `{ordinal}` 渲染的 DNS 模板时天然满足;**给了 `-ClientPublicHost`(单一主机)时所有序号自报同一个主机**,多节点下只有该主机所在节点、
  或其背后按 nodePort 逐序号正确转发到 Pod 所在节点的 gate 可达。preflight 对这种组合不告警,单节点(如 kind)之外请用 DNS 模板或 LoadBalancer
  (`gate-connection-admission-control.md` §4.2.1)。LoadBalancer 能否保留源 IP 还取决于云 LB 是否直通:代理型 LB 需要 PROXY protocol,gate 不支持。
- 仍建议显式写 `-OpsProfile`:managed-cloud 强制 LoadBalancer(external 必须给模板),bare-metal 强制 NodePort,custom 未显式给时为 NodePort。
- 防火墙只放行 gate nodePort 段(或各 LB)、battle 的 Agones 端口段、gateway Ingress 的 80/443;gRPC 端口永不对外。
- battle 同口径:直连地址每实例一个(Agones Dynamic hostPort,或验证用 hostPort 20000),不经 Service、不做 LB(D81 / D86)。
- external gate 滚动逐台走 `tools/scripts/k8s_gate_drain.ps1`(`OnDelete` + PDB `maxUnavailable: 0`)。

## 3. 原口径(2026-09-29 之前,原文保留)

- Gate 外部暴露规则：托管云 K8s 优先使用 LoadBalancer；自建/裸金属 K8s 优先使用 NodePort 加外部 L4 负载均衡器。
  —— **(2026-09-29 更正)** 类型取舍仍成立;外部 L4 只能按实例透传(每个 nodePort 一条监听),不得把多台 gate 放进同一后端池。
- 当集群没有成熟的 LB 实现时，不要将 LoadBalancer 作为通用默认推荐。 —— 仍成立。
- 在示例和运维文档中，优先使用明确的 OpsProfile 选择（`managed-cloud` 或 `bare-metal`）。 —— 仍成立;managed-cloud + external 须给模板。
- `custom` 配置的脚本默认基线为 `GateServiceType=NodePort`；`managed-cloud` 配置仍强制使用 `LoadBalancer`。
  —— 仍成立;`dev_tools.ps1` / `k8s_image.ps1` 的 `-GateServiceType` 默认已改为留空、跟随 `k8s_deploy.ps1`(原先 `k8s_image.ps1` 默认 LoadBalancer)。
