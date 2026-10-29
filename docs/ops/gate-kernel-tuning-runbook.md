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
- 余量 4096 是推算,只是拒启门槛,进程实际可用的是抬起来的软限:gate 直连模式约 2Z 条到全服 scene_manager,scene 约 4Z 条到全服 SM / DS(Z ≈ 200~445,2000 万在线口径),都在 4096 以内;Z 超过约 900 或副本数变多时要重新核这个常数(推导见头文件注释)。
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
    - `dev_tools.ps1` / `k8s_image.ps1` / `k8s_zone_rollback.ps1` 尚未透传,要直接调 `k8s_deploy.ps1 -Command zone-up` / `infra-up`。
      - (**2026-10-22 再更正**,已落码、未验证)三个包装入口已透传 `-NodeOpenFileLimitMode`,不必再直接调 `k8s_deploy.ps1`。可用入口:
        - `dev_tools.ps1 -Command k8s-zone-up` / `k8s-infra-up` / `k8s-all-up`;
        - `k8s-release-zone` / `k8s-release-all`;
        - `k8s-zone-rollback`。
      - 包装层留空 = 不透传,同样只收小写。
      - 回档时以 `warn` 运行的 zone 必须照传。`k8s_zone_rollback.ps1` 第 0 步不核对这个 env;漏传时 gate / scene 在 Step 6 拒启,那时已停服、已回档,只能补上参数单独重跑 Step 6。
        - (**2026-10-22 三更正**,已落码、未验证)第 0 步 a 现在核对。`-NodeOpenFileLimitMode` 留空时,`-Apply` 在停服之前读 gate(Deployment / external StatefulSet)与 scene Deployment 的模板;装了 Agones 时再读 scene Fleet。上面 `kubectl set env` 手设在单个工作负载上的值也算。
        - 任一带着 `warn`,或读不到(kubectl / Agones 探测失败、没有 gate),都在 Step 1 之前拒绝,报错给出照传的 `-NodeOpenFileLimitMode warn`。
          - (**2026-10-23 更正上一句**)两种拒绝给的出路不同。有工作负载带着 `warn` 时,报错要求照传 `-NodeOpenFileLimitMode warn`;硬限已修好就显式传 `enforce`。读不到时(还包括 `valueFrom`、变量重复、存档早于这项核对),报错不替操作者选值:先确认该 zone 实际以哪种模式运行,再显式传 `warn` 或 `enforce`。实际以 enforce 运行的 zone 不要为了过检查传 `warn`,那会让 Step 6 拉起的 gate / scene 带上豁免,不再拒启保护。
        - 显式给值不核对:照传 `warn`,或确认硬限已修好后显式传 `enforce`。
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
