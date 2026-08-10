# gate 节点安全开关(运维必读)

本文只讲 **cpp/nodes/gate** 这一个进程的安全相关配置项。实现见
`cpp/nodes/gate/gate_security.h`,回归测试见
`cpp/nodes/gate/tests/gate_security_test.cpp`。

## 1. `GATE_RUN_MODE` —— 运行模式(唯一的 dev/prod 判据)

| 取值 | 含义 |
| --- | --- |
| 不设置 | **prod**(默认值就是安全的那一侧) |
| `prod` / `production` / `release` / `live` | 生产 |
| `dev` / `development` / `local` | 开发 |
| `test` / `testing` | 测试 |
| 其他任何值 | 按 **prod** 处理,并在启动时打一条 WARN |

大小写不敏感,前后空白会被去掉。

在此之前 gate **没有任何** dev/prod 判别:`gate_token_secret` 为空就直接把
每条客户端连接标成"已验证"。生产上只要 `GateTokenSecret` 忘配、或 ConfigMap
挂载失败读成空字符串,令牌校验整条链路就静默失效,而日志里一条异常都没有。

现在的行为:

| `GateTokenSecret` | `GATE_RUN_MODE` | 结果 |
| --- | --- | --- |
| 非空 | 任意 | 正常做 HMAC 令牌校验 |
| 空 / 纯空白 | `dev` / `test` | 放行,但启动时和首条连接各打一条 `SECURITY WARNING` |
| 空 / 纯空白 | `prod`(含未设置) | **拒绝启动**(`LOG_FATAL` -> abort);即使绕过启动门禁,连接层也会当场 `forceClose` |

> 本地开发:`bin/etc/base_deploy_config.yaml` 里 `GateTokenSecret` 本来就是
> 非空的占位串,所以默认路径不受影响。只有把它显式清空的场景才需要同时设
> `GATE_RUN_MODE=dev`。

## 2. `GATE_GM_ADMIN_SECRET` —— GM 面调用方鉴权(必配,否则 GM 停机不可用)

`Gate.GmGracefulShutdown` 会把本 gate 上**所有**在线会话 `forceClose` 再排队
停机。旧实现收到请求即执行,唯一的"身份"是请求体里自报的 `operator` 明文字符串
—— 任何能连到本节点 RPC 端口的进程都能停服。

现在必须带 HMAC-SHA256 签名。**未配置 `GATE_GM_ADMIN_SECRET` 时一律拒绝**,
没有"开发模式免签"的口子(开发环境停 gate 用 Ctrl+C / SIGTERM 即可)。

### 签名寄生在 `operator` 字段里(为什么不加 proto 字段)

`GmGracefulShutdownRequest` 只有 `operator` / `reason` 两个字段
(`proto/common/base/gm_admin.proto`),而这条 RPC 走 muduo `GameChannel` 通道:
`CallMethod` 的 `controller` 与 `done` 都恒为 `nullptr`(`game_channel.cpp:451`),
**没有任何 metadata 边信道**。改 proto 要连带重生成 C++/Go/Java 三侧产物,
不在本次可验证范围内,因此签名放进 `operator`:

```
operator = "<操作人>|<unix 秒>|<nonce>|<hmac-sha256 hex 小写>"
```

被签名的 canonical 串(字段间 `\n`,一段都不能省):

```
Gate.GmGracefulShutdown\n<目标 node_id>\n<操作人>\n<unix 秒原文>\n<nonce>\n<reason>
```

约束:

* `<目标 node_id>` 是要停的那个 gate 的节点号,**十进制、无前导零**(验证方固定
  用 `std::to_string(gNode->GetNodeId())` 生成)。node_id 在 gate 启动 banner 和
  `[gate_version]` 行里都有;
* 操作人名 1..64 字节,**不得含 `|`**;
* 时间戳必须是纯十进制数字,**原文**参与签名(不做解析后再格式化);
* nonce 1..128 字节,窗口内不可复用;
* 签名 64 位小写 hex;
* method / node_id / reason 都进 canonical 串 —— 分别防止把 Scene 那条 GM RPC
  的签名搬来调 Gate、把给 gate-17 的签名重放给 gate-18、以及中间人改写审计理由。

### 时间窗与重放

* 允许时钟偏差默认 300s,可用 `GATE_GM_AUTH_SKEW_SECONDS` 调整,**钳制**在
  `[30, 900]`;
* nonce 在 `2 × skew` 秒内去重,进程内表上限 1024 条,满则拒;去重表**不跨
  节点**,跨节点重放由 canonical 串里的 node_id 挡住;
* 校验顺序是**先验签名、后登记 nonce** —— 反过来的话未认证的进程能用垃圾
  nonce 灌满去重表,把合法 GM 请求挤掉,鉴权本身变成 DoS 面。

### 签一条请求(bash + openssl)

```bash
SECRET="$GATE_GM_ADMIN_SECRET"
NODE_ID="17"                 # 目标 gate 的 node_id
OP="gm_alice"
TS="$(date +%s)"
NONCE="$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')"
REASON="maintenance"

CANON=$(printf 'Gate.GmGracefulShutdown\n%s\n%s\n%s\n%s\n%s' \
        "$NODE_ID" "$OP" "$TS" "$NONCE" "$REASON")
SIG=$(printf '%s' "$CANON" | openssl dgst -sha256 -hmac "$SECRET" -hex | awk '{print $NF}')

echo "operator=${OP}|${TS}|${NONCE}|${SIG}"
echo "reason=${REASON}"
```

> `printf` 用 `%s` 拼而不是 `echo`:`echo` 会多加一个尾随换行,canonical 串
> 差一个字节签名就对不上。

拒绝时 gate 只打一条 `GM graceful shutdown REJECTED, reason=<原因>` 的
`LOG_ERROR`,不回错误码(响应 proto 里没有错误字段)。可能的 `reason`:
`secret_not_configured` / `malformed_envelope` / `timestamp_out_of_window` /
`signature_mismatch` / `replayed_nonce`。

## 3. 启动版本行

gate 启动会打两条 `[gate_version]` 单行记录(直写 stdout,不受 `LogLevel` 影响):

```
[gate_version] version=... commit=... build_time=... node_type=GATE node_id=pending zone_id=pending
[gate_version] version=... commit=... build_time=... node_type=GATE node_id=17 zone_id=1
```

第一条在 `main()` 第一句 —— 此时 `node_id` 还没做 etcd CAS、`zone_id` 还没读
配置,所以是 `pending`;哪怕进程在 etcd 阶段就崩了,版本三元组也已经落进容器
日志。第二条在 `SetAfterStart` 里,`node_id` / `zone_id` 都是终值。

版本三元组来源(逐级回落):

| 字段 | 来源 |
| --- | --- |
| `build_time` | `__DATE__ " " __TIME__`,编译器内建,一定有 |
| `commit` | 编译期宏 `BUILD_GIT_SHA` -> 环境变量 `GATE_BUILD_COMMIT` -> `unknown` |
| `version` | 环境变量 `GATE_BUILD_VERSION` -> 编译期宏 `GATE_BUILD_VERSION` -> `unknown` |

**为什么把环境变量排在编译期宏前面**:`cpp/nodes/gate/CMakeLists.txt` 是
`tools/archived/vcxproj2cmake.py` 从 `gate.vcxproj` **无条件重新生成**的
(`tools/scripts/build_linux.sh` 第 2 步),往里手加 `-D` 会在下一次构建时被抹掉。
所以能在部署侧一定生效的通道是环境变量(K8s 用镜像 tag / git sha 注入即可);
编译期宏留作构建流水线以后接入的入口 —— 真要接,得改
`tools/archived/vcxproj2cmake.py`,不能改生成出来的 `CMakeLists.txt`。
