# K8s Gate Exposure Guidance

> **Rewritten 2026-09-29 around "one entry per gate instance"** (cluster-external client entry, D76–D93).
> Authoritative design: `docs/design/k8s-client-entry.md` (D87–D90 and its "运维手册" section); deploy flags: `deploy/k8s/README.md`,
> Optional Flags, "集群外客户端入口". The Chinese versions (`k8s_gate_exposure_guidance.md` / `_zh.md`) are authoritative.
> The four original bullets are kept verbatim at the end, each annotated. Nothing here has run on a cluster yet;
> it is pending the kind end-to-end check (Codex / the user).

## 1. Rule: every gate instance needs its own entry

- Login picks **one** gate for the player and signs a token bound to it (`GateTokenPayload.gate_node_id`,
  `proto/common/base/message.proto:219-220`). A gate whose node id differs drops the connection with
  `token_gate_node_mismatch` (`cpp/nodes/gate/handler/rpc/client_message_processor.cpp:1052-1057`).
  Any entry that spreads connections across several gates (one Service, one L4 LB backend pool) rejects a large share of tokens.
- **A single Service only works for a single replica.** `gate-entry` is generated only with `-ClientEntryMode podip`
  and exactly one gate replica (D90; `tools/scripts/lib/k8s_client_entry.ps1:509`, `tools/scripts/k8s_deploy.ps1:4994-4999`).
  In podip mode login hands out the PodIP anyway, so `gate-entry` does not make gates reachable from outside the cluster.
- **External players need `-ClientEntryMode external`**: gate runs as a StatefulSet (never in Agones, D87) with one Service
  `gate-<i>` per ordinal whose selector pins `statefulset.kubernetes.io/pod-name` (`lib/k8s_client_entry.ps1:864-909`).
  Before publishing to etcd, each gate advertises its client-reachable address in `NodeInfo.client_endpoint`
  (D76, `proto/common/base/common.proto:33`); login and scene_manager hand that out (`go/shared/clientendpoint/select.go:41`).

## 2. Choosing `-GateServiceType` (in external mode it sets the type of every `gate-<i>`)

| Cluster | `-GateServiceType` | Address the gate advertises | Requirements |
|---|---|---|---|
| Managed cloud with a mature LB | `LoadBalancer` (one LB per gate) | DNS name rendered from `-GateClientHostTemplate`, port 18000 | Template required; must contain `{ordinal}` when replicas > 1 |
| Self-hosted / bare metal | `NodePort` | Host: template > `-ClientPublicHost` > node `status.hostIP`; port `gateNodePortBase + ordinal` | Explicit `gateNodePortBase` per zone, no overlapping ranges, 30000–30085 recommended; on multiple nodes the advertised host must reach the Pod's node (D89 premise below) |

- Single implementation of the address plan: `lib/k8s_client_entry.ps1:408` `Resolve-GateClientEndpointPlan` (D88).
- Do not make LoadBalancer a universal default: in external mode the LB count grows with the gate replica count. Without a mature LB implementation, use NodePort.
- `-GateExternalTrafficPolicy` defaults to `Local` (D89) to keep the player's source IP, which per-source limiting at the gate needs
  (`gate-connection-admission-control.md`, G9). `Cluster` SNATs traffic; the preflight warns.
- **Multi-node premise for NodePort + `Local` (D89)**: the host each ordinal advertises must deliver its traffic to the node running that ordinal's Pod;
  otherwise the receiving node has no local endpoint and drops the traffic. This holds when the host is the node's `status.hostIP` or a DNS template
  rendered per `{ordinal}`. **With `-ClientPublicHost` (a single host), every ordinal advertises the same host**, so on a multi-node cluster only the gates on
  that host's node, or behind it with correct per-nodePort forwarding to each Pod's node, are reachable. The preflight does not warn about this combination;
  outside a single-node cluster (such as kind) use a DNS template or LoadBalancer (`gate-connection-admission-control.md` §4.2.1).
  Whether LoadBalancer keeps the source IP also depends on the cloud LB being passthrough: a proxying LB needs PROXY protocol, which the gate does not support.
- Still name `-OpsProfile` explicitly: `managed-cloud` forces LoadBalancer (so external needs a template), `bare-metal` forces NodePort, and `custom` resolves to NodePort unless set.
- Firewall: open only the gate nodePort range (or the LBs), battle's Agones port range and the gateway Ingress on 80/443. gRPC ports are never exposed.
- battle follows the same rule: one direct address per instance (Agones `portPolicy: Dynamic` hostPort, or hostPort 20000 for verification only), with no Service and no LB (D81 / D86).
- Roll external gates one at a time with `tools/scripts/k8s_gate_drain.ps1` (`OnDelete` + PDB `maxUnavailable: 0`).

## 3. Original guidance (before 2026-09-29, kept verbatim)

- Gate external exposure rule: managed cloud K8s prefers LoadBalancer; self-hosted / bare metal K8s prefers NodePort plus external L4 load balancer.
  — **(Corrected 2026-09-29)** The type choice still holds. An external L4 balancer is usable only as per-instance passthrough (one listener per nodePort); never put several gates in one backend pool.
- Do not recommend LoadBalancer as a universal default when the cluster lacks a mature LB implementation. — Still holds.
- Prefer explicit OpsProfile choices (`managed-cloud` or `bare-metal`) in examples and operational docs. — Still holds; managed-cloud plus external requires a host template.
- Script baseline default for `custom` profile is `GateServiceType=NodePort`; `managed-cloud` profile still forces `LoadBalancer`.
  — Still holds. `-GateServiceType` in `dev_tools.ps1` and `k8s_image.ps1` now defaults to empty and follows `k8s_deploy.ps1` (`k8s_image.ps1` used to default to LoadBalancer).
