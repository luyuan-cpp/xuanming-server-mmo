# gate 连接准入:P1-5 `resources.requests` 的落码规格(2026-11-26)

**2026-11-26 更新:本单不再交 Gemini。** 第 1 节由会话「gate 未验签连接预算、LRU 淘汰与按概率早拒(准入 P0-2)」落码;
原来的第 2 节(断线重连优先)核对后发现 login 早就做了(区级准入按 gate 水位封顶,重连免排队、不过区级准入),已删,
说明见 `docs/design/gate-connection-admission-control.md` P2-3 第 3 条。第 1 节落码后本单即可删除。

同一天落码的 G10 / §5 第 5 条 / G12 / P2-1 的验证命令不在这里,在 `docs/PROGRESS.md` 2026-11-26「gate 准入:验签前只收小帧」条目。

规矩照 AGENTS.md:只在 main 上改,不开分支;项目没上线;日志英文、注释与文档中文;改 Go 要同步 Java 版 `PARITY.md` 的待做行(不写 Java 代码);
动手前 `git status` / ListAgents 看清别的会话是否在改同一批文件,只用精确编辑。

## 1. P1-5:C++ 节点的 `resources.requests`(对应 G11)

**现状**:gate / scene / battle 的 Pod 模板都没有 `resources`,QoS 是 BestEffort,`oom_score_adj = 1000`,节点内存紧张时最先被驱逐,
被驱逐的是一整台的在线玩家。

**改哪里**:
- `tools/scripts/lib/k8s_client_entry.ps1`:集群外入口的 gate StatefulSet 模板、battle 的 hostPort Deployment 与 Agones Fleet 模板(搜 `terminationGracePeriodSeconds`,同一段容器定义里);
- `tools/scripts/k8s_deploy.ps1`:`New-NodeDeploymentYaml`(podip 形态的 gate / scene / battle Deployment)。
- 契约断言加在 `tools/scripts/tests/k8s_client_entry_contract.tests.ps1` 与 `tools/scripts/tests/k8s_deploy_contract.tests.ps1`:每个 C++ 节点容器都有 `resources.requests.cpu` 与 `.memory`,
  都**没有** `limits`。

**取值**(按 `-ReleaseProfile` 分档,`ValidateSet('dev','staging','prod')`):

| 角色 | dev | staging / prod |
| --- | --- | --- |
| gate | cpu `100m`,memory `128Mi` | cpu `1`,memory `512Mi` |
| battle | cpu `100m`,memory `128Mi` | cpu `1`,memory `512Mi` |
| scene | cpu `100m`,memory `256Mi` | cpu `1`,memory `1Gi` |

理由(写进生成器的注释与设计稿 P1-5 那张表):
- **cpu 1**:C++ 节点只有主 EventLoop 一个线程在干活(AGENTS §9 不开新线程),满载就是吃满一个核。request 1 让调度器真留一个核;
  **不配 cpu limit**,免得 CFS 限流把 tick 打出毛刺。
- **gate / battle 512Mi**:连接上限 2 万 × 每条约 10KB(§9.5 估的内核与用户态开销,加会话状态)约 200MB,加进程基线(配表、协议描述、代码)约 200MB。这是稳态的下限估计,只作 request。
- **scene 1Gi**:配表全量常驻加一台几千玩家的 ECS 状态,没有实测,先取 1Gi;压测后按 `process_resident_memory` 调。
- **dev 档取小值**:只为脱离 BestEffort;kind 这类本机集群一个节点要放下全部 Pod,按生产值会排不进去。
- **不配 memory limit**:limit 必须和「连接上限 × 每连接输出缓冲高水位(2MB)」对齐(设计稿 §9.5),这个乘积是理论值、要先实测;
  配错了就是 OOMKill 掉一整台在线玩家。只配 requests 已经把 QoS 抬到 Burstable,`oom_score_adj` 不再是 1000。

**通过标准**:两套契约测试全过;`-DryRun` 输出里三类 C++ 容器都带上表的 requests、不带 limits;kind 上 `kubectl get pod -o jsonpath='{.status.qosClass}'` 是 `Burstable`。

## 2. 本单之外仍要验证的

P1-1 ~ P1-4、P0-2、P2-2、battle 直连准入、部署参数 `-ConnectionAdmissionEnv` 与 2026-11-26 这批都还没编译、没运行,命令分别在 `docs/PROGRESS.md` 的对应条目里。
阈值都是推导值,校准步骤在设计稿 §9.7。
