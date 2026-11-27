# Gate 内核调优 Runbook

**Date:** 2026-05-08
**适用:** 所有 cpp gate 节点机器(裸机 / VM / K8s 节点)
**前提:** Linux 4.12+

---

## 一、为什么要调

压测出现下面任意一条 → 你撞上了 ephemeral port / TIME_WAIT 墙:

- `ss -s` 看到 TIME_WAIT 几万,`Bound` 千级别堆积
- `nstat | grep -i drop` 出现 `TcpExtListenDrops` 增长
- 登录长尾(p99 / max) 从几十 ms 跳到秒级
- 客户端日志: `connect: cannot assign requested address`(EADDRNOTAVAIL)

**实测对比(扩 port + tw_reuse 后)**

| 指标 | 优化前 | 优化后 |
|---|---|---|
| login_fail | 3% | 0 |
| avg_login | 414-453ms | 77-79ms |
| max_login | 18-25s | 0.9-1.2s |
| SynSent | 1540 | 0 |
| Bound | 15470 | 2704 |

---

## 二、固化 sysctl(必做)

写入 `/etc/sysctl.d/99-gate.conf`:

```ini
# === ephemeral port 扩容 ===
net.ipv4.ip_local_port_range     = 1024 65535
# 如有固定监听端口冲突,在此排除
net.ipv4.ip_local_reserved_ports = 3306,6379,8080,9000

# === TIME_WAIT 复用 + 容量 ===
# 主动 connect 端复用 TIME_WAIT 端口(基于 PAWS,安全)
net.ipv4.tcp_tw_reuse            = 1
net.ipv4.tcp_max_tw_buckets      = 1048576
net.ipv4.tcp_fin_timeout         = 15
net.ipv4.tcp_timestamps          = 1   # tw_reuse 依赖

# === 半连接 + 全连接队列 ===
net.core.somaxconn               = 65535
net.ipv4.tcp_max_syn_backlog     = 65535
net.ipv4.tcp_syncookies          = 1

# === 连接 buffer ===
net.core.rmem_max                = 16777216
net.core.wmem_max                = 16777216
net.ipv4.tcp_rmem                = 4096 87380 16777216
net.ipv4.tcp_wmem                = 4096 65536 16777216

# === conntrack(如启用 nf) ===
net.netfilter.nf_conntrack_max   = 1048576

# === keepalive(防 NAT 踢长连) ===
net.ipv4.tcp_keepalive_time      = 300
net.ipv4.tcp_keepalive_intvl     = 30
net.ipv4.tcp_keepalive_probes    = 3
```

加载:
```bash
sudo sysctl --system
sudo sysctl -p /etc/sysctl.d/99-gate.conf
```

⚠️ **绝对不要开** `net.ipv4.tcp_tw_recycle`:
- NAT 环境(K8s / 公网)会丢连接
- 4.12+ 内核已删除该参数

---

## 三、进程级 ulimit

`/etc/security/limits.d/gate.conf`:

```
*  soft  nofile  1048576
*  hard  nofile  1048576
*  soft  nproc   65535
*  hard  nproc   65535
```

systemd 单元 `[Service]`:
```
LimitNOFILE=1048576
LimitNPROC=65535
```

K8s pod:
```yaml
securityContext:
  sysctls:
    - name: net.core.somaxconn
      value: "65535"
```

### 3.1 K8s / 容器:进程自抬软限、硬限不够就拒启(2026-10-18 补充)

上面的 limits.d 与 systemd 写法对裸机 / VM 仍然适用;容器里它们不生效,由下面这套接管。

**C++ 节点自己抬软限**(`cpp/libs/engine/core/node/system/node/open_file_limit.h`,`Node::Initialize` 在读完配置、开任何网络 fd 之前调用,仅 Linux;gate / scene / battle 三类都覆盖):

- getrlimit 后把软限抬到 min(硬限, 1048576),只抬不降、不动硬限(非 root 也能做)。
- 需求 = 客户端面准入上限 + 4096 余量:gate = `GateMaxConnections` + 4096(默认 24096),battle = `BattleMaxConnections` + 4096(默认 8192),scene = 4096。配 0(仅 dev/test 合法)按 0 算。
- 余量 4096 只是拒启门槛,进程实际可用的是抬起来的软限。2026-11-12 定案维持 4096:scene_manager 11-05 起按 zone 收窄,gate 到路由服只连按 zone 取的几台,非客户端连接数不再随全服 zone 数增长(推导见头文件注释)。运行期够不够不靠这个常数,看下面的指标。
- **运行期观测(2026-11-12,已落码、未编译)**:C++ 节点的 Prometheus 出口(`-CppMetricsPort`,缺省 9400)导出:
  - `process_open_fds` / `process_max_fds`:当前打开的 fd 数与软限。超过 80% 持续 5 分钟报 `ProcessOpenFilesNearLimit`;
  - `cpp_node_open_file_limit_required` / `cpp_node_open_file_limit_exempted`:启动门槛,以及是否带 `warn` 豁免在跑。带豁免超过 1 小时报 `CppNodeOpenFileLimitExempted`;
  - `cpp_node_accept_exhausted_total{reason}`、`cpp_node_accept_paused_total`:accept 因 fd 或内存耗尽失败、监听暂停的次数,对应 `CppNodeAcceptResourceExhausted` 与持续 5 分钟以上的 `CppNodeAcceptResourceExhaustedSustained`。
  - 告警文件 `deploy/k8s/cpp-node-resource-alerts.yaml`,手工 apply:`kubectl apply -n observability -f deploy/k8s/cpp-node-resource-alerts.yaml`。
- **fd 或内存耗尽时 accept 的行为(2026-11-12,已落码、未编译)**:进程不再崩溃(原来 `ENFILE` / `ENOBUFS` / `ENOMEM` 会让 muduo 直接 abort,这台节点上的全部在线连接一起断)。
  - fd 用尽:监听端接进排在最前的连接再立刻关掉,对端马上看到断开;做不到时暂停监听 100ms 再试。
  - 内核缓冲或内存不足:暂停监听 100ms 再试。
  - 已建立的连接不受影响。节点日志里每秒最多一行 `Acceptor: accept failed, errno=... action=... failures_since_last_report=...`。
  - 处置:先看上面的 fd 指标;fd 跟着连接数涨就调低准入上限或加台数,持续上涨而连接数不涨多半是泄漏,保留现场(`ls -l /proc/<pid>/fd | awk '{print $NF}' | sort | uniq -c | sort -rn | head`)后重启这台。
- **拒启与其他致命错误的最后一行(2026-11-12,已落码、未编译)**:C++ 节点任何一处 `LOG_FATAL` 在退出前都会把那一行同步写进 stderr,`kubectl logs --previous` 看得到。原来只有 fd 上限、客户端地址这几类拒启会写,gate / battle 的令牌密钥、连接上限等启动门禁只进异步日志文件,崩溃后找不到原因。
- 抬完仍低于需求:缺省**拒绝启动**,stderr 一行 `FATAL open file limit config: role=... soft=... hard=... required=... shortfall=...`(`kubectl logs --previous` 可见),Pod 进 CrashLoop。多副本形态下新 Pod 起不来只会让滚动停住、旧 Pod 继续服务;全新 zone-up 则整个 zone 起不来。
  - **2026-10-18 更正(上一句把影响面说小了,按工作负载形态分开看)**:「只停住」只对滚动时 maxUnavailable 为 0 的工作负载成立。C++ 节点的 Deployment 模板(`k8s_deploy.ps1` 的 `New-NodeDeploymentYaml`)没写 strategy,按 K8s 缺省 25% / 25% 取整:
    - gate Deployment(podip 形态,缺省 2 副本):maxUnavailable = 0,确实只是停在滚动中途,旧 Pod 继续服务;
    - scene Deployment(缺省 4 副本;拆池时按各池副本数):maxUnavailable = 1,滚动一开始就先缩掉 1 台旧 scene(该台玩家按关机流程退出),新 Pod 拒启后这份容量一直缺着,直到回滚;
    - battle Deployment(非 external,缺省 1 副本,`-OpsProfile managed-cloud / bare-metal` 抬到 2):maxUnavailable = 0,只停住;
    - battle hostPort Deployment(external + `-BattleOrchestrator deployment`,只用于验证与回退,D86):固定 maxSurge 0 / maxUnavailable 1,先删旧 Pod 再起新 Pod。单副本时战斗服务**全停**、在打的局全部作废;2 副本时少一台;
    - Agones Fleet(scene / battle 的 agones 编排):RollingUpdate 缺省 25%,会先缩掉一部分旧 GameServer,新 GameServer 反复失败重建(Ready 前崩溃是原地 CrashLoop 还是变 Unhealthy 后重建取决于 Agones 版本,未核实),要盯 `kubectl get gs`;
    - external gate StatefulSet(OnDelete):apply 不重建 Pod,换版走 `k8s_gate_drain.ps1`,每次只排空、删除一个序号,新 Pod 就绪超时即报错停下,所以一次只坏一台——被排空的那台空着,直到回滚(见下文「紧急回退」);
    - 全新 zone-up:整个 zone 起不来。

    所以「先读各节点池硬限」(下文上线前置第 1 步)不是可选项;对单副本 battle 它是唯一的防线。给 C++ Deployment 显式设 maxUnavailable 0 要改共享脚本 `k8s_deploy.ps1`,已列为后续项,本项不改。

    (**2026-10-22 更正**,已落码、未验证:`New-NodeDeploymentYaml` 现在显式写 `strategy.rollingUpdate.maxUnavailable: 0`,maxSurge 仍取 K8s 缺省 25%。从新的 `k8s_deploy.ps1` 部署起,gate / scene / battle 的 Deployment 拒启时只停在滚动中途,上面 scene「先缩掉 1 台」那条不再成立。battle hostPort 形态仍是 maxSurge 0 / maxUnavailable 1,单副本拒启仍整体停服;Agones Fleet 与 external gate 的行为不变。另外,`-WaitReady` 等 gate / scene / battle 失败时,脚本会对未就绪 Pod 读 `kubectl logs --previous`(没有上一个实例时读当前实例),把 `FATAL open file limit` 行打印出来;Agones scene 不等待,只打印对应的 logs 命令。见 PROGRESS「2026-10-22 B0-4 部署侧后续项」条目。)
- 每次启动 stdout 都有一行(`kubectl logs` 可见,日志文件里也有一份):
  `[open_file_limit] role=gate verdict=ok mode=enforce required=24096 client_cap=20000 cap_key=GateMaxConnections reserve=4096 soft_before=1024 soft=524288 hard=524288 raise=ok`
- 为什么不在清单里做:K8s 没有按容器的 ulimit 字段;initContainer 改不了别的进程的限值;特权 prlimit 已否决(见 [gate 准入设计](../design/gate-connection-admission-control.md) P0-1);启动壳里的 `ulimit -n` 与进程自抬是同一件事,不做两份。

**硬限只能在节点运行时层面改**(以下是外部常识,未在本仓实测):

- containerd / dockerd 的 systemd drop-in,例如 `/etc/systemd/system/containerd.service.d/limits.conf` 写 `[Service]` + `LimitNOFILE=1048576`,然后 `systemctl daemon-reload && systemctl restart containerd`;只影响之后新建的容器。
- 或在 CRI 的 base_runtime_spec 里写 `rlimits`(RLIMIT_NOFILE)。
- 托管集群按云厂商的节点镜像 / 节点池配置改。这些都是集群运维,不在本仓。

**上线前置(硬性:发布带本项的镜像之前先做,别默认「大家会先读硬限」)**:

**目标集群还没有旧 Pod 时(首次上线就是这种情况)**:容器的 nofile 由节点池的容器运行时决定,与跑的是哪个进程无关。在每个节点池上起一个一次性的探针 Pod 读一次,读完自动删除;镜像用要发布的节点镜像,不用另拉镜像:

```bash
kubectl -n <namespace> run nofile-probe --rm -i --restart=Never --image=<节点镜像> \
  --overrides='{"spec":{"nodeSelector":{"<节点池标签名>":"<节点池标签值>"}}}' \
  --command -- sh -c 'echo soft=$(ulimit -Sn) hard=$(ulimit -Hn)'
```

硬限不低于需求(gate 24096、battle 8192、scene 4096,改过准入上限的按「上限 + 4096」算)才发布;读数记进 `docs/PROGRESS.md`。已有旧 Pod 的集群按下面第 1 步读。

1. 在每个目标集群的每个节点池上,从现有旧 Pod 读一次容器 nofile。gate / scene 的启动命令不是 `exec`,PID 1 可能是 `sh`,**不要读 `/proc/1/limits`**,按进程名找 pid:
   ```bash
   kubectl -n <ns> exec <pod> -c <容器名> -- sh -c 'for p in /proc/[0-9]*; do n=$(cat $p/comm 2>/dev/null); case "$n" in gate|scene|battle) echo $n; grep "Max open files" $p/limits;; esac; done'
   ```
   容器名即节点名(gate / scene / battle);battle 在全局池的命名空间。把 soft / hard 连同集群、节点池、containerd 版本记进 `docs/PROGRESS.md`。
   (**2026-10-18 更正**:容器名 = 工作负载名,不总是节点名:gate 是 `gate`;scene 是**池名**——单池为 `scene`,拆池的 zone 上是 `scene-world` / `scene-instance` 等(`k8s_deploy.ps1` 的 `New-NodeDeploymentYaml` / `New-SceneFleetYaml` 都取池名),照写 `-c scene` 会报容器不存在;battle 是 `battle`。拿不准时先 `kubectl -n <ns> get pod <pod> -o jsonpath='{.spec.containers[*].name}'`。下面第 3 步的 `logs -c` 同此。)
2. 硬限 ≥ 需求(按该环境实际配置算)才发新镜像。不够时两条路:
   - 改节点运行时的 `LimitNOFILE`(见上;集群运维),或调低 `GateMaxConnections` / `BattleMaxConnections` 并加台数;
   - 临时把豁免开关设为 `warn`(见下),先起服,再排期改硬限。
3. 发布后核对:`kubectl -n <ns> logs <pod> -c <容器名> | grep '\[open_file_limit\]'`,每类都应是 `verdict=ok`;再跑一次第 1 步的 exec,进程软限应等于 min(旧 hard, 1048576)。
   (**2026-10-18 更正**:判据应为「进程软限 = max(旧 soft, min(旧 hard, 1048576))」,以启动行的 `soft=` 字段为准。实现只抬不降:旧软限本来就大于 1048576 的环境(例如运行时 `LimitNOFILE=infinity`、soft = hard = 1073741816)会保持原值,`raise=none`,这是正常结果,不是失败。)

**运维豁免开关 `NODE_OPEN_FILE_LIMIT_MODE`**:

- 取值 `enforce`(缺省;未设置、空串同)与 `warn`,只认小写。其他值一律按 `enforce` 处理,并打一行 `[open_file_limit] WARN invalid NODE_OPEN_FILE_LIMIT_MODE='...', falling back to enforce`——拼错开关不会意外放行。
- `warn`:不够也继续启动,stdout 与日志各打一行 `[open_file_limit] WARN exemption active (NODE_OPEN_FILE_LIMIT_MODE=warn): role=... soft=... required=... shortfall=...`。该节点能承载的连接数以实际软限为准,超出的连接在 accept 时 EMFILE(G7 / G8 那一串),所以只用于应急,不是常态配置。C++ 节点没有指标出口,豁免只能从这行日志查到。
  - **2026-10-18 更正(上一条把 warn 的后果说轻了)**:gate 的准入闸门在 accept 拿到 fd **之后**才判(`client_message_processor.cpp` 按 `GateMaxConnections` 计数后 forceClose),准入上限没调时,客户端连接会先把整个 fd 表吃满。此后凡是要新 fd 的路径都会 EMFILE:节点间断线重连(scene / scene_manager)、Redis / Kafka 重连、etcd 通道重连(租约续不上就过期,gate 被摘除,玩家全掉);muduo 异步日志按大小或按天滚动时 `FileUtil::AppendFile` 的 fopen 失败,Debug 下 assert 直接 abort,Release 下接着对空指针 setbuffer 段错误(`third_party/muduo-linux/muduo/base/FileUtil.cc`)。也就是说,warn 是把「启动时拒启」换成「运行到某个时刻被摘除或崩溃」,不只是新连接被拒。操作口径:
    - **gate / battle 的应急首选是调低准入上限,不需要豁免**:把 `GateMaxConnections` / `BattleMaxConnections` 调到 ≤ 实际软限 − 4096(经 ConfigMap 下发)并加台数补容量,此时需求 ≤ 软限,检查本身就通过,`verdict=ok`;
    - 不调上限、只开 warn:后果如上,只能作为「必须先起服、几小时内改硬限」的短时手段,并盯日志里的 `EMFILE` / `Too many open files` 与 etcd 摘除;
    - scene 没有客户端面,需求只是 4096 余量,对它开 warn 的风险小得多(fd 只会被节点间 / 基础设施连接与日志占用),但同样要尽快改硬限。
  - 不要把豁免做成「自动下调准入上限」:负责人 10-17 拍板是缺省拒启 + 显式豁免,下调上限由运维显式改配置。
- 与 `GATE_RUN_MODE` / `SCENE_RUN_MODE` / `BATTLE_RUN_MODE`、GM 开关无关(scene 的运行模式同时决定 GM 放行,两件事不绑在一起)。
- K8s 生成器(`tools/scripts/k8s_deploy.ps1`)**目前没有接线**这个变量。临时手设只对 Deployment 形态直接生效(gate StatefulSet 是 OnDelete,设完还要逐台删 Pod;Agones Fleet 要改 Fleet 模板):
  ```bash
  kubectl -n <ns> set env deployment/<名字> NODE_OPEN_FILE_LIMIT_MODE=warn
  # 撤销必须显式做:set env 加的变量不在 last-applied 里,k8s_deploy 的 apply 不会删它
  kubectl -n <ns> set env deployment/<名字> NODE_OPEN_FILE_LIMIT_MODE-
  ```
  - **2026-10-22 更正**(已落码、未验证):生成器已接线。
    - 用法:`k8s_deploy.ps1 -NodeOpenFileLimitMode enforce|warn`,只认小写,其余值在入口被拒。留空 = 不写,进程按 enforce。
    - 覆盖本次部署的全部 C++ 工作负载:`zone-up` 的 gate(Deployment / external StatefulSet)与 scene(Deployment / Agones Fleet),`infra-up` 的 battle(Deployment / hostPort / Agones Fleet)。
    - `warn` 每次部署打一条 `Write-Warning`。
    - 不粘滞:下一次部署不传就落回 enforce。external gate 是 OnDelete,模板改了还要按 `k8s_gate_drain.ps1` 逐台换 Pod。
    - `dev_tools.ps1` / `k8s_image.ps1` 尚未透传,要直接调 `k8s_deploy.ps1 -Command zone-up` / `infra-up`。
      - (**2026-10-22 再更正**,已落码、未验证)两个包装入口已透传 `-NodeOpenFileLimitMode`,不必再直接调 `k8s_deploy.ps1`。可用入口:
        - `dev_tools.ps1 -Command k8s-zone-up` / `k8s-infra-up` / `k8s-all-up`;
        - `k8s-release-zone` / `k8s-release-all`。
      - 包装层留空 = 不透传,同样只收小写。
      - 灾难恢复走 [PITR 手册](./mysql-backup-pitr-runbook.md) §4.0:以 `warn` 运行的 zone 起服时必须照传。漏传就落回 enforce,硬限仍不够时 gate / scene 拒启。
    - 上面的 `kubectl set env` 仍可作应急。手设的值不在 last-applied 里,撤销仍按上面显式删。
- 裸机 / staging 主机:启动节点前 `export NODE_OPEN_FILE_LIMIT_MODE=warn`(见 [linux-staging-stress-runbook.md](./linux-staging-stress-runbook.md) A.1)。

**紧急回退**:本项不改 proto、配置键、ConfigMap 与 K8s 模板,回退就是换回旧镜像 tag(已推送的不可变 git sha tag)。不要用 `k8s-release-zone`,它会从当前工作区重新构建并推送镜像:

```powershell
# zone 内 gate / scene;其余参数与本次发布完全一致(-ReleaseProfile / -OpsProfile / -GateRouterMode / -ClientEntryMode 等)
pwsh -File tools/scripts/dev_tools.ps1 -Command k8s-zone-up `
  -ZoneName <zone> -ZoneId <id> -ReleaseProfile <dev|staging|prod> -OpsProfile <同发布时> `
  -NodeImage ghcr.io/luyuancpp/mmorpg-node:<旧 git sha> -WaitReady
# battle 是全局池,只在 infra-up 路径部署:同样的参数改用 -Command k8s-infra-up,-NodeImage 同上
```

同一镜像若还带了别的会改 ConfigMap 的改动,按那项改动自己的回滚说明处理(通常是用旧 commit 重跑 zone-up)。

**2026-10-18 更正(紧急回退的四处补充;上面的命令块保留,按下面的口径用)**:

1. **口径**:上面 `-NodeImage ghcr.io/luyuancpp/mmorpg-node:<旧 git sha>` 的写法只适用于 dev / kind 的 sha tag 镜像。staging / prod 按 [release-checklist.md](./release-checklist.md) §4 回滚:镜像 tag 用上一版 manifest 的 `images.image_tag`(`vX.Y.Z-<12位commit>`),回滚前先用 `docker buildx imagetools inspect <ref>` 核对 tag 仍指向 manifest 记录的 digest,不一致就不许用这个 tag 回滚。
2. **在发布提交的检出上执行**:`k8s-zone-up` 从当前工作区渲染 ConfigMap 与 Pod 模板,`k8s-infra-up` 还会按当前工作区 apply etcd / Redis / Kafka / MySQL 等全部 infra 清单。工作区领先发布提交是常态(多会话整区快照),在当前工作区跑回滚会顺带把未发布的改动推上集群。先 `git worktree add <目录> <发布提交>`,在那份检出里跑回滚命令,跑完再删 worktree。
3. **external 形态的 gate(StatefulSet,`updateStrategy: OnDelete`)换回旧模板后还要手动删 Pod**:zone-up 只改模板,正在 CrashLoop 的 gate Pod 由 kubelet 原地重启,仍跑新镜像;而 `-WaitReady` 对它轮询 `readyReplicas`,不删 Pod,会一直等到超时。所以:
   ```bash
   # ① 先不带 -WaitReady 跑一次上面的 k8s-zone-up(参数同上),只为换回旧模板
   # ② 找出未就绪的 gate Pod(READY 0/1、CrashLoopBackOff)
   kubectl -n <ns> get pod -l app=gate
   # ③ 逐个删除未就绪的那几台;它们没注册进 etcd、没在服务,可以直接删,STS 会按旧模板重建
   kubectl -n <ns> delete pod gate-<序号>
   # ④ 确认就绪:readyReplicas 等于副本数;scene 按池名等滚动完成
   kubectl -n <ns> get statefulset gate -o jsonpath='{.status.readyReplicas}'
   kubectl -n <ns> rollout status deployment/<scene 池名>
   ```
   **在服务的旧 gate Pod 不要动**;之后再换版本仍走 `k8s_gate_drain.ps1` 逐台排空。
4. **battle 优先只换镜像,不要为回滚 battle 跑整套 `k8s-infra-up`**(battle 在 `mmorpg-infra` 这类 infra 命名空间,容器名 `battle`):
   - Deployment 形态(含 external 的 hostPort 形态):`kubectl -n <infra-ns> set image deployment/battle battle=<旧镜像>`,再 `kubectl -n <infra-ns> rollout status deployment/battle`;
   - Agones Fleet 形态:`kubectl -n <infra-ns> edit fleet battle`,把 `spec.template.spec.template.spec.containers` 里 `name: battle` 那一项的 `image` 改回旧镜像,然后盯 `kubectl -n <infra-ns> get gs -l app=battle`;
   - 这是相对清单的漂移:下一次 `k8s-infra-up` 会按它的 `-NodeImage` 参数覆盖回去,所以下一次 infra-up 必须带旧镜像或已修好的镜像。

### 3.2 gate 连接准入:指标、告警与速率闸的调整(2026-11-12,已落码、未编译;2026-11-19 补未验签预算与按来源前缀限速,未编译)

设计与取值依据见 [gate-connection-admission-control.md](../design/gate-connection-admission-control.md) §6 的 P0-2、P1-1 ~ P1-4、P2-2。这里只写运维要用到的部分。

**gate 在 accept 处做五道判定**,被拒的连接当场收到 RST,gate 不为它建任何对象、内核不留 TIME_WAIT:

| 判定 | 参数 | 缺省 | 被拒时的 `gate_rejected_total{reason}` |
| --- | --- | --- | --- |
| 关源(停机 / 维护) | 无 | — | `shutting_down` |
| 总量 | `GateMaxConnections`(`base_deploy_config.yaml`) | 20000 | `capacity` |
| 按来源前缀限速(缺省关闭) | 环境变量 `GATE_SOURCE_RATE_PER_SECOND`(设了才开)、`GATE_SOURCE_BURST` | 关闭;开了只在承压时生效,推荐值 = 全局持续速率的 1/10(上限 2 万时 20 条/秒、桶深 200) | `source_rate` |
| 未验签预算 | 环境变量 `GATE_PREAUTH_BUDGET`、`GATE_PREAUTH_EARLY_DROP_START`、`GATE_PREAUTH_EVICTABLE_AGE_MS` | 早拒起点 = 上限的 5%,预算 = 上限的 10%(上限不到 1 万时为 500 / 1000),可淘汰年龄 3000 毫秒 | `preauth_early_drop` / `preauth_budget` |
| 新建速率(令牌桶) | 环境变量 `GATE_ACCEPT_RATE_PER_SECOND`、`GATE_ACCEPT_BURST` | 速率 = 上限的 1%/秒(下限 100),桶深 = 上限 | `accept_rate` |

- 启动日志有一行 `Client connection admission ENFORCED at accept, max_connections=... accept_rate_per_second=... accept_burst=... preauth_early_drop_start=... preauth_budget=... preauth_evictable_age_ms=... source_rate_per_second=... source_burst=...`,以它为准(按来源限速关闭时后两项是 0,括号里打着推荐值)。
- 判定顺序是关源 → 总量 → 按来源限速 → 未验签预算 → 新建速率(上表按这个顺序排)。
- **怎么设上表的环境变量**(2026-11-20 起,未运行):部署时带 `k8s_deploy.ps1 -ConnectionAdmissionEnv`,逗号分隔的「名字=值」,例如
  `-ConnectionAdmissionEnv GATE_ACCEPT_RATE_PER_SECOND=2000,GATE_ACCEPT_BURST=20000`。`dev_tools.ps1` 的 `k8s-zone-up` / `k8s-all-up` 与
  `k8s_image.ps1` 的发布命令同名透传。
  - 名字只收上表的七个 `GATE_*` 与 §3.3 的七个 `BATTLE_*`,区分大小写;值只收规范十进制正整数。拼错在部署入口就拒,范围与相互关系由 gate 启动时校验,写错拒启。
  - `GATE_*` 只写进 zone-up 的 gate(Deployment / external StatefulSet)。external gate 是 OnDelete,模板改了还要按 `k8s_gate_drain.ps1` 逐台换 Pod。
  - **不粘滞**:脚本不从集群读回旧值,下一次部署不带就回到缺省推导。压测或应急调过的,之后每次部署都要照带;要长期改,改代码里的推导(设计稿 §9.7)。
  - 每次部署打一行 `[connection-admission] gate overrides: ...`,部署输出里看得出这次带了什么。
  - 等不及部署时仍可 `kubectl -n <ns> set env statefulset/gate <名字>=<值>`(deployment 形态换成 `deployment/gate`)。set env 加的变量不在 last-applied 里,
    之后的部署不会清掉,用完要显式撤销(`<名字>-`)。
- **未验签预算怎么工作**:
  - 还没验签的连接数(`gate_unauthenticated_connections`)过了早拒起点就算承压。
  - 承压时,建连满 3 秒还没验签的最老一条连接给新连接让位(被关掉,`gate_preauth_evicted_total` 加一)。
  - 池里全是刚建的连接、没有可以让位的,新连接按概率被拒,到预算全拒。
  - gate 自己的 loop 刚卡过(存活扫描的 1 秒节拍迟到)时不淘汰,只按概率拒或到预算拒。
  - 正常客户端毫秒级验签,正常洪峰碰不到起点;登录压测也碰不到,不用为它调参数。
- **要调预算时**(`GatePreauthBudgetRejecting` 响、握手 P99 也在变慢,而卡顿已经处理过):
  部署时带 `-ConnectionAdmissionEnv GATE_PREAUTH_BUDGET=<n>,GATE_PREAUTH_EARLY_DROP_START=<n>`(上面「怎么设」)。
  - 只设预算时起点按新预算取一半;起点不能大于预算;可淘汰年龄 1~9999 毫秒,不能到 10 秒握手期限。写错拒绝启动。
  - 撤销 = 下一次部署不带它们;用 set env 设的写 `GATE_PREAUTH_BUDGET- GATE_PREAUTH_EARLY_DROP_START- GATE_PREAUTH_EVICTABLE_AGE_MS-`。
- **按来源前缀限速**(IPv4 /24、IPv6 /64):
  - 只在 gate 看得到玩家真实源地址的形态里有用:external 入口、`externalTrafficPolicy: Local`(设计稿 §4.2.1)。
  - 只对公网源地址生效(2026-11-20 起):私网、回环、链路本地、100.64/10、IPv6 唯一本地 / 链路本地的来源一律不按来源限速。
    这类地址是代理、LB 或集群内调用方的,认不出后面的真实玩家。podip 形态、代理型 LB 后面开了也不会误伤,只是不起作用。
  - **经 ws-edge 进来的小游戏连接不受 gate 这一道管**:gate 看到的是边缘 Pod 的内网地址。要按来源限它们,就在 infra-up 时带
    `-WsEdgeSourceRatePerSecond`,由边缘 nginx 按 /24 做 `limit_req`。
  - 开启:部署时带 `-ConnectionAdmissionEnv GATE_SOURCE_RATE_PER_SECOND=<n>`。推荐值启动日志里有(`suggested ...`)。
    `GATE_SOURCE_BURST` 不设时取速率 × 10 秒。
  - 开了也只在承压时生效:未验签池过了早拒起点,或全局速率闸最近 10 秒内拒过连接。平时一条都不拦。
  - 撤销 = 下一次部署不带它们;用 set env 设的写 `GATE_SOURCE_RATE_PER_SECOND- GATE_SOURCE_BURST-`,之后的部署不会替你清掉。
- 两个环境变量只接受规范十进制正整数(1..1000000),写错拒绝启动;不设就是缺省推导。没有「0 = 关闭」,要放开就写一个大数。
- **压测要打超过缺省速率的建连时**(例如反复登录登出的登录压测,持续新建速率超过上限的 1%/秒),先给 gate 设 `GATE_ACCEPT_RATE_PER_SECOND`,否则桶用完之后压测机会看到连接被重置,结果失真。部署压测环境时带 `-ConnectionAdmissionEnv GATE_ACCEPT_RATE_PER_SECOND=<n>`;压测完下一次部署不带它就回到缺省。
- **校准这些阈值**:现在的缺省值都是推导的,还没有压测数据。步骤、要看的列与改动判据见设计稿 §9.7;未验签连接用 `tools/scripts/preauth_flood.py` 造,复盘读 `stress_summarize.ps1` 的 N2 表。

**指标**(gate 的 Prometheus 出口,`-CppMetricsPort`,缺省 9400):

- `gate_connections_current` / `gate_connections_max`:会话数与生效上限。
- `gate_unauthenticated_connections`:还没验签的连接数。握手期限 10 秒,正常客户端毫秒级验签,这个数平时接近 0。
- `gate_accepted_total`:接纳的连接;`rate(gate_accepted_total[1m])` 就是新建连接速率。
- `gate_rejected_total{reason}`:被拒的连接。`not_ready` / `no_token_secret` 只在启动配置有问题时出现。
- `gate_accept_rate_limit_per_second` / `gate_accept_burst`:生效的速率闸参数。
- `gate_handshake_duration_ms`:建连到验签成功的耗时直方图。握手期限要不要调,看它的 P99.9。
- `gate_preauth_reaped_total`:建连满 10 秒仍未验签、被关掉的连接。
- `gate_preauth_evicted_total`:未验签池承压时,建连满 3 秒还没验签、被淘汰给新连接让位的连接。
- `gate_preauth_budget` / `gate_preauth_early_drop_start` / `gate_preauth_evictable_age_ms`:生效的未验签预算、早拒起点与可淘汰年龄。
- `gate_source_rate_limit_per_second` / `gate_source_burst`:按来源限速的参数(关闭时为 0);`gate_source_rate_active`:最近一次判定时是否生效;
  `gate_source_tracked`:登记着的前缀数;`gate_source_untracked_total`:前缀表满、没登记就放行的连接。

**区分正常洪峰与攻击**(只看这几项,别看连接总数):

| | 正常洪峰(开服、维护后重连) | 只建连不验签 |
| --- | --- | --- |
| `gate_unauthenticated_connections` | 低,周转快,到不了早拒起点 | 高,不周转,停在早拒起点附近(超不过预算) |
| `gate_handshake_duration_ms` | 分布正常 | 样本少(大多数连接根本不验签) |
| `gate_preauth_reaped_total` / `gate_preauth_evicted_total` | 基本不涨 | 持续上涨(承压时多半记在淘汰里) |
| 处置 | `accept_rate` 在拒绝时调高速率闸;`capacity` 在拒绝时加 gate 台数 | 速率闸、未验签预算与握手期限在按设计工作;去上游(L4、防火墙、云清洗)处置来源 |

**告警**:`deploy/k8s/gate-connection-alerts.yaml`,手工 apply:`kubectl apply -n observability -f deploy/k8s/gate-connection-alerts.yaml`。

| 告警 | 含义 | 先看什么 |
| --- | --- | --- |
| `GateConnectionsNearCapacity` | 会话数超过上限的 90% 持续 5 分钟 | 同 zone 其余 gate 是否也高;login 本应在 90% 停止往这台派人 |
| `GateRejectingAtCapacity` | 因满员拒绝了新连接 | 未验签连接最多占预算(上限的 10%),满员时至少九成是验签过的会话:多半是真实玩家满了,加台数 |
| `GateAcceptRateLimited` | 新建连接超过速率上限 | 按上表区分洪峰与攻击 |
| `GateUnauthenticatedConnectionsHigh` | 未验签连接到早拒起点的八成以上,持续 2 分钟 | `gate_preauth_evicted_total` 是否在涨;采样日志 `Evicting the oldest unauthenticated connection ...` 与其余采样行里的 `latest_peer` |
| `GatePreauthBudgetRejecting` | 未验签预算拒绝了新连接(池里全是刚建的连接) | 握手 P99:正常 = 只建连不验签的突发,去上游处置;也在变慢 = gate 卡顿或网络变差,拒掉的多半是真实玩家,先处理卡顿,反复出现再按上面调预算 |
| `GateSourceRateLimited` | 按来源限速开着、承压期间有前缀超了它那一份 | 采样日志 `Per-source accept rate exceeded` 行的 `latest_peer`;是已知运营商出口、握手耗时又正常时调高 `GATE_SOURCE_RATE_PER_SECOND` / `GATE_SOURCE_BURST` |
| `GateSourceTableFull` | 前缀表(65536)满了,新前缀没按来源限速就放行 | 分布式洪水,去上游处置;gate 这一侧不用调 |
| `GatePreauthConnectionsReaped` | 每秒超过 1 条未验签连接被关,持续 5 分钟 | 握手 P99 是否也在变慢 |
| `GateHandshakeSlow` | 握手 P99 超过 3 秒持续 10 分钟 | 这台 gate 的 CPU 与 loop 延迟;多台同时变慢看入口网络 |

**客户端看到的现象**:被拒的连接在客户端是「连接被重置」(RST),不是「对端正常关闭」。被淘汰的未验签连接是已经建出来的连接被关,客户端看到的是对端正常关闭。两种都按普通断线处理、退避后重连即可;排查玩家「连不上」时,先在对应时段的 `gate_rejected_total` 与 `gate_preauth_evicted_total` 里找原因。

### 3.3 battle 直连准入:指标、告警与速率闸的调整(2026-11-19,已落码、未编译;2026-11-20 补按来源前缀限速与 O(1) 的未认证池,未编译)

battle 的客户端直连与 gate 同一套做法(设计见 [gate-connection-admission-control.md](../design/gate-connection-admission-control.md)「P1-1 ~ P1-4 · battle 直连面」与 [turn-based-battle-server.md](../design/turn-based-battle-server.md) D27),速率闸与缺省推导是同一份代码。这里只写与 gate 不同的地方,其余读法照 §3.2。

| 判定 | 参数 | 缺省 | 被拒时的 `battle_rejected_total{reason}` |
| --- | --- | --- | --- |
| 关源(停机 / 维护;计划排空不关源) | 无 | — | `shutting_down` |
| 总量(参战者与观众共用) | `BattleMaxConnections`(`base_deploy_config.yaml`) | 4096 | `capacity` |
| 按来源前缀限速(缺省关闭,2026-11-20 补) | 环境变量 `BATTLE_SOURCE_RATE_PER_SECOND`(设了才开)、`BATTLE_SOURCE_BURST` | 关闭;开了只在承压时、只对公网来源生效,推荐值 4096 时每个前缀 10 条/秒、桶深 100 | `source_rate` |
| 未验签预算(2026-11-19 补) | 环境变量 `BATTLE_PREAUTH_BUDGET`、`BATTLE_PREAUTH_EARLY_DROP_START`、`BATTLE_PREAUTH_EVICTABLE_AGE_MS` | 4096 时早拒起点 500、预算 1000,可淘汰年龄 3000 毫秒 | `preauth_early_drop` / `preauth_budget` |
| 新建速率(令牌桶) | 环境变量 `BATTLE_ACCEPT_RATE_PER_SECOND`、`BATTLE_ACCEPT_BURST` | 速率 = 上限的 1%/秒(下限 100),桶深 = 上限;4096 时是 100 条/秒、桶深 4096 | `accept_rate` |

- 启动日志有一行 `Battle client connection admission ENFORCED at accept, max_connections=... accept_rate_per_second=... accept_burst=...`,以它为准。两个变量的取值规则与 gate 相同,写错拒绝启动。
- **全服匹配的网络探测**:探测也连这个端口,探测有效期 120 秒、到期前重探。match 在每个机房的可探测 battle(自报 CLOSED 的不探)里按玩家分散目标,同一玩家总落同一台(2026-11-19 起,未编译;之前每个机房只探 NodeId 最小的一台;推导见 [global-match-hierarchical-pools.md](../design/global-match-hierarchical-pools.md) §5.1「探测目标」)。所以每台的探测建连速率约等于「全服有探测计划的玩家数 ÷(本机房可探测 battle 数 × 重探周期)」,与它的连接上限无关;机房里只有一台时,1 万人排队就到了缺省的 100 条/秒。**上线和 `match-stress` 压测前按这个式子估算**,接近缺省时给 battle 设 `BATTLE_ACCEPT_RATE_PER_SECOND`,取值按「排队人数峰值 ÷(可探测台数 × 重探周期)」再留一倍;台数按机房里可探测得最少的时候算(滚动升级、排空期间 CLOSED 的节点不分担探测);重探周期按 robot 的口径约 80 秒(有效期 120 秒减去提前量 40 秒,`robot/network_probe.go`),Unity 客户端的提前量以客户端为准。被拒的探测在客户端是这个机房探测失败。探测连接出示了 match 签发的授权,不计入 `battle_unauthenticated_connections`。
- 怎么设(2026-11-20 起,未运行):与 gate 相同,infra-up 时带 `-ConnectionAdmissionEnv BATTLE_ACCEPT_RATE_PER_SECOND=<n>`(§3.2「怎么设」)。
  - Deployment、hostPort、Agones Fleet 三种形态都写,含金丝雀池;同一个参数里的 `GATE_*` 只在 zone-up 生效。
  - Fleet 改了 env 是换代:已分配的房间打完才回收,新房间用新值。不粘滞,同 gate。
  - 等不及部署时,Deployment / hostPort 形态可以 `kubectl -n <ns> set env deployment/battle <名字>=<值>`,撤销写 `<名字>-`。
  - Fleet 是 CRD,`kubectl edit fleet battle` 手加的 env 在下一次部署时被生成物覆盖,所以 Fleet 一律走部署参数。

**指标**(battle 的 Prometheus 出口,`-CppMetricsPort`,缺省 9400;名字与 gate 一一对应,把前缀换成 `battle_`):`battle_connections_current` / `battle_connections_max`、`battle_unauthenticated_connections`、`battle_accepted_total`、`battle_rejected_total{reason}`(`no_token_secret` 只在启动配置有问题时出现)、`battle_accept_rate_limit_per_second` / `battle_accept_burst`、`battle_handshake_duration_ms`(建连到票据握手成功)、`battle_preauth_reaped_total`、`battle_preauth_evicted_total`、`battle_preauth_budget` / `battle_preauth_early_drop_start` / `battle_preauth_evictable_age_ms`、`battle_source_*`(按来源限速,见下)。

**未验签预算**(2026-11-19 补,未编译;规则与 gate 相同,见 §3.2):
- 池是没完成票据握手、也没出示探测授权的直连,探测量再大也不占它。
- 缺省(`BattleMaxConnections` 4096 时)早拒起点 500、预算 1000,可淘汰年龄 3 秒。
- 覆盖变量 `BATTLE_PREAUTH_BUDGET` / `BATTLE_PREAUTH_EARLY_DROP_START` / `BATTLE_PREAUTH_EVICTABLE_AGE_MS`,写错拒启。
  怎么设同上面的速率闸变量(`-ConnectionAdmissionEnv`)。
- 被拒的直连记在 `battle_rejected_total{reason="preauth_budget|preauth_early_drop"}`。
- battle 的 loop 刚卡过时不淘汰。
- 启动日志 `Battle client connection admission ENFORCED at accept, ...` 带 `preauth_early_drop_start=` / `preauth_budget=` / `preauth_evictable_age_ms=`。
- 池按 connId 有序存着,每次 accept 只读池大小,最老一条只在承压时才找。被拒的洪水不会再按直连数消耗 battle 的 loop(2026-11-20 前是每次扫一遍会话表)。

**按来源前缀限速**(2026-11-20 补,未编译;规则与 gate 相同,见 §3.2):
- 缺省关闭。开启:infra-up 时带 `-ConnectionAdmissionEnv BATTLE_SOURCE_RATE_PER_SECOND=<n>`。
  推荐值在启动日志里(`suggested per-source ...`);`BATTLE_SOURCE_BURST` 不设时取速率 × 10 秒。
- battle 的 hostPort / Agones 端口映射保源地址,直连看得到玩家的公网地址;经 ws-edge 进来的小游戏直连只看得到边缘的内网地址,
  不受这一道管(要限就在 infra-up 带 `-WsEdgeSourceRatePerSecond`)。
- 开了也只在承压时生效:未认证池过了早拒起点,或全局速率闸最近 10 秒内拒过直连。
- 指标 `battle_source_rate_limit_per_second` / `battle_source_burst` / `battle_source_rate_active` / `battle_source_tracked` /
  `battle_source_untracked_total`,被拒记在 `battle_rejected_total{reason="source_rate"}`。

**告警**:`deploy/k8s/battle-connection-alerts.yaml`,手工 apply:`kubectl apply -n observability -f deploy/k8s/battle-connection-alerts.yaml`。

| 告警 | 含义 | 先看什么 |
| --- | --- | --- |
| `BattleConnectionsNearCapacity` | 直连数超过上限的 90% 持续 5 分钟 | 其余 battle 是否也高;只有这一台高时看是不是热门观战房间集中在它上面 |
| `BattleRejectingAtCapacity` | 因满员拒绝了新直连 | 未认证连接占比:高 = 位置被占着;低 = 真实的参战者与观众满了,加台数 |
| `BattleAcceptRateLimited` | 新建直连超过速率上限 | 先看同机房的 battle 是不是一起在拒:探测均摊在机房里每台可探测的 battle 上,一起拒多半是探测量,按上面估算调高速率;只有这一台在拒,再按 §3.2 区分洪峰与攻击 |
| `BattleUnauthenticatedConnectionsHigh` | 未认证直连到早拒起点的八成以上,持续 2 分钟(2026-11-19 前是「超过上限的 5%」) | `battle_preauth_evicted_total` 是否在涨;采样日志 `reason=handshake_timeout` 与 `battle evicting the oldest unauthenticated direct connection` 行里的 `latest_peer` |
| `BattlePreauthBudgetRejecting` | 未验签预算拒绝了新直连(池里全是刚建的直连) | 握手 P99:正常 = 只建连不握手的突发,去上游处置;也在变慢 = battle 卡顿,先处理卡顿,反复出现再调 `BATTLE_PREAUTH_BUDGET` / `BATTLE_PREAUTH_EARLY_DROP_START` |
| `BattleSourceRateLimited` | 按来源限速开着、承压期间有公网前缀超了它那一份 | 采样日志 `reason=source_rate` 行的 `latest_peer`;是已知运营商出口、握手耗时又正常时调高 `BATTLE_SOURCE_RATE_PER_SECOND` / `BATTLE_SOURCE_BURST` |
| `BattleSourceTableFull` | 前缀表(65536)满了,新前缀没按来源限速就放行 | 分布式洪水,去上游处置;battle 这一侧不用调 |
| `BattlePreauthConnectionsReaped` | 每秒超过 1 条未握手直连被关,持续 5 分钟 | 握手 P99 是否也在变慢 |
| `BattleHandshakeSlow` | 握手 P99 超过 3 秒持续 10 分钟 | 这台 battle 的 CPU、loop 延迟与房间数;多台同时变慢看客户端入口 |

**客户端看到的现象**:与 gate 相同,被拒的直连是 RST。参战者连不上直连时本局每回合超时自动出手,客户端按断线重连流程重连(同票可重用,或经 `MatchService.RequestBattleTicket` 补签)。

---

## 四、多源 IP(单 IP 端口耗尽时)

ephemeral port 上限是 **(srcIP, dstIP, dstPort)** 维度的。单 IP 对单后端理论 64K。

加副 IP:
```bash
ip addr add 10.0.0.11/24 dev eth0
ip addr add 10.0.0.12/24 dev eth0
ip addr add 10.0.0.13/24 dev eth0

# 持久化(Ubuntu netplan / RHEL nmcli)
```

让 gate 用上多源 IP(零代码改动):
- 跑多 gate 进程 + `ip rule` 绑不同源 IP
- 或多开 gate 容器/VM 直接横向扩

---

## 五、故障定位 SOP

### 5.1 一条命令判断"谁在短连"

```bash
ss -tan state time-wait | awk '{print $4}' | sort | uniq -c | sort -rn | head
```

| TIME_WAIT 集中在 | 含义 | 修法 |
|---|---|---|
| `<login_ip>:<grpc_port>`（路由模式看 router 对端） | gRPC 对端连接频繁周转 | 按 [gRPC 传输设计](../design/grpc-muduo-transport.md) 核查断链、GOAWAY 与重连；业务通道默认关闭周期 keepalive |
| 自己监听端口 | 客户端/压测短连进来 | 客户端开 keep-alive |
| 压测机几万 TIME_WAIT | 是压测机的极限,**不是 gate 的** | 压测机也调 sysctl |

### 5.2 看 accept queue 是否溢出

```bash
ss -lnt              # 看 Recv-Q / Send-Q
nstat | grep -E 'ListenOverflows|ListenDrops|TCPSynRetrans'
```

`ListenOverflows` 增长 → backlog 不够,调 `somaxconn` + `tcp_max_syn_backlog`,并确保业务 `listen(fd, N)` 的 N 够大。

### 5.3 看是不是软中断单核打满

```bash
mpstat -P ALL 1
# %soft 一个核 100% → 网卡 RSS / 多队列没开
ethtool -l eth0
ethtool -L eth0 combined <核数>
```

### 5.4 拐点判断

加压时同时盯:p99 延迟 / CPU% / `ss -s` / `ListenOverflows`

- 延迟陡升 + accept queue 溢出 + CPU 没满 → **服务端真极限**(优化业务 / 扩 worker)
- 延迟陡升 + Bound + SynSent 高 + CPU 没满 → **客户端 ephemeral port 极限**(回到 §二)
- 延迟陡升 + CPU 满 → **算力极限**(横向扩 gate)

---

## 六、阶梯压测建议

```
5k → 10k → 15k → 20k → 30k → 45k → 60k → 80k → 100k
每档 3-5 分钟,记录:
  QPS / avg / p99 / max
  ss -s 各状态计数
  CPU% (us/sy/si)
  网络带宽 / pps
画曲线找拐点。
```

---

## 七、回滚

任何 sysctl 改动想回滚:
```bash
sudo rm /etc/sysctl.d/99-gate.conf
sudo sysctl --system
```

不需要重启机器,内核参数立即生效。

---

## 关联文档
- [docs/design/ARCH.md](../design/ARCH.md) §9 总体调优
- [docs/design/architecture-current-state-vs-gaps-2026-05.md](../design/architecture-current-state-vs-gaps-2026-05.md) 缺口清单
- [docs/design/gate-load-balancing-design.md](../design/gate-load-balancing-design.md) gate 有状态约束
