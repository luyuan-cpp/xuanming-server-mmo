# Gate 登录身份修复与 Claude Code 审核说明

日期：2026-11-05（美东）。本轮代码尚未提交；精确源码基线、文件摘要和补丁见审核包清单。仅包含本任务的改动，其他窗口的 AOI、场景广播等改动不归入本包。

## 用户疑问与结论

用户问的是：login 已认证成功、gate 已知道这条连接登录成功，下游还要再次登录认证吗？以及 gate 灰度更新或重启后怎么办？

结论：**login 验凭据，gate 保存可信成功结果；在线下游复用原身份，不为每条操作重新查登录 Redis 或验密码。下游仍验证服务间身份声明的来源，并在业务提交点校验角色权限。** TCP 接通或拿到 gate 入口票据，都不等于完成账号登录。

权限复核不只防止角色转移：还包括冻结、删除和归属修订；连接退出、踢线和 gate 重启则由连接生命周期处理。角色转出再转回同一账号，旧 revision 仍须拒绝。业务必须核原账号，不能反查当前 owner 再把旧请求补成新主人的身份。

## 本轮实现

1. login 在完整入场链中把原认证账号、原 owner_revision、连接 incarnation 与原有 player_id/login_generation 一起发给 gate。BindSessionEvent 属于内部 Kafka 协议。
2. gate 将认证账号与原修订单独保存在 SessionInfo，和会变化的场景路由版本分开；缺字段、错连接代次、旧登录代次或同代身份冲突不能绑定。离场清理同时清除认证原件。
3. 所有面向 Go 的在线身份由 gate 从服务端会话构造。SessionDetails 整体字节摘要、实际目标 RPC 全名、时间戳和随机 nonce 参与既有 v1 HMAC。直连与 client_rpc_router 转发都签实际业务方法。生成器增加 grpcFullMethod，生成产物通过正式 no-unity 流程产生。
4. Go 共用 shared/callerauth 验签接缝，覆盖 chat/friend/guild/trade/match 与 domainhost 装配的 mail/rank。通过后在 context 保存不可变原件。shared/loginsession 只有在该证明与请求身份完全一致时直接取原账号，不读 Redis；伪造账号或修改 revision/gate UUID 拒绝。
5. login 保留原适配器的强制/开发宽松配置和敏感 metadata 清理。未带原账号的旧内部请求保留原 Redis 路径，但有账号声明、没有验签证明时禁止回落补权。该兼容分支不是允许旧 gate 绕过新服务的验签拦截器。
6. 已排队请求保存准入时的身份，发送前复核当前绑定；离场后残留请求不能借后来的身份继续执行。角色归属检查和本库 role_fence 提交锁未删除，事务内不新增 RPC。
7. 本机启动器只对需要它的进程注入同源内部密钥；生产部署通过 Secret 引用注入。未配密钥拒启，不把真实密钥写入仓库或日志。协议可使用额外验钥环境变量分阶段轮换。
8. Java 独立实现中，绑定角色的 SessionContext 使用入场时 ownerAccount，而不是后续可能变化的 account。新连接仍从空身份开始。Java 内部使用既有 Dubbo 认证机制，不宣称与 C++/Go 新签名格式相同。

## Gate 重启与客户端

新 gate 使用新进程 UUID，新 TCP 使用新 incarnation，不恢复旧连接的登录标记。客户端必须重新获取入口票据、完成 Login、选择原角色 EnterGame，并等 NotifyEnterScene。可用的 access token 可以用于重新认证；令牌失效则完整登录。仅提供 player_id 不能恢复权限。

**本轮不需要修改或重编译客户端。** 客户端消息号和请求字段未变；本轮新增字段只用于内部身份传递。本轮验证的是现有客户端协议下的重新登录，未把客户端 UI 自动重连/自动选回原角色做成新功能，也不宣称无感重连。此前已交付的客户端 PVE 入口补丁在历史包中，与本轮强制升级无关。

项目尚未上线；新 gate 绑定依赖新 login，下游又要求签名，因此本次按协调更新服务器验收。旧 gate/新在线服务混跑和生产滚动升级尚未验收。客户端兼容不代表任意新旧服务可混跑。

## 此前相关故障修复也纳入审核

- 建角 1003：本机 MySQL 驱动参数与目录查询整理；相应实际对比证据保留在旧公开验收收据中。
- 进场后 PVE 失败：EnterGame 成功时不再提前删除连接登录态，LeaveGame/Disconnect 与 TTL 仍负责既有清理。此次身份传递使在线请求不再依赖这条临时记录。
- 本机 battle 公网探测端点、启动写者目录保留、机器人等待 Scene 个人结算终局等已修复项与历史证据保留。旧证据不能替代本次身份方案测试。

## 已取得的验证

| 范围 | 实际结果 |
|---|---|
| Go shared/login/chat/friend/guild/trade/match/mail/rank/client_rpc_router | 各模块 go test ./...、go vet ./... 通过，9 个服务最终程序已重编译 |
| 身份回归 | 先红后绿：已签在线身份无需 Redis；改 revision/UUID、未验账号和篡改签名拒绝；login 完整入场事件携带原件 |
| C++ | proto/rpc/gate 库、routing_identity_test、隔离 gate 工程构建通过；路由与身份测试 224/224 |
| 生成器 | cpp 生成包测试通过，正式 proto-gen-build / proto-gen-run(no-unity) 通过 |
| PowerShell | go_services 30/30；身份环境注入 3/3；k8s 部署契约 283/283 |
| Java reactor | game-gate 及依赖 BUILD SUCCESS；Gate 348/348；discovery 224 通过、134 条件跳过；真 TiDB AuthorityPoolTiDbTest 4/4 |
| 新程序本机端到端 | 正在验收，最终以本包公开收据为准，未有收据不算通过 |

Java 阻塞修复有明确失败证据：三项 Windows 控制台中文断言改为显式 UTF-8；两项缺真实夹具恢复为正式本机 TiDB 夹具运行；继续运行发现的两项旧 GM 契约测试改为检查 reserved 墓碑和不可路由。未注释断言、跳过失败测试或改成可路由。

本机 TiKV 曾 OOM 停止，已启动原容器且保留数据。Java 使用 sql_fixture_env.ps1 的本地隔离夹具，记录运行前后容器 ID、启动时间、内存配置相同；**这不是旧的全系列 SQL 资格续签，也不是双区 8/8。** 本轮未重新执行历史全部系列。

## 未覆盖与已有失败

- 附加 lobby_authority_render 测试仍在既有 AssetOpsOn=false 夹具与 guild.yaml 的 AssetOp.Enabled=true 比较上失败。本轮未修改资产开关；独立的 k8s 部署契约 283 项通过。这个失败没有隐藏，也未通过改断言掩盖。
- C++ 正常输出目录首次构建受另一个窗口并行 MSBuild 的 PDB 占用影响，出现 C1041；使用独立 IntDir 与 TargetName 后构建通过。没有终止其他窗口的编译。
- 第一次本机启动因已有 Docker 容器属于 player-store-r2-infra 而重名失败；指定该现有 Compose 项目继续启动，不删数据。再次启动须保留这一项目选择。
- HMAC 绑定身份和方法，不覆盖业务正文；nonce 去重仍是现有进程内窗口，不是跨副本永久撤销账本。内部网络仍应限制接入；共享验钥服务处在同一信任域。
- 本轮没有实现所有已受理请求在踢线瞬间跨服务原子撤销。撤销前受理的请求按既有业务契约完成或拒绝，冻结/过户正确性由提交护栏保证。
- 未执行新压测，不声明性能提升；未进行生产部署或全游戏功能验收。

## 请 Claude Code 审核

优先检查：原账号的唯一来源；BindSession 与连接/退出栅栏的先后关系；排队请求是否能借新身份；两种转发模式的签名目标和编码；所有在线域是否经过验签；原身份是否在业务提交时仍核 role_fence；gate 重启的新连接是否只能重新认证；Java ownerAccount 生命周期。

再检查部署密钥同源、缺钥拒启、回滚协调范围以及新增生成器字段的完整性。工作区有其他窗口改动，按清单与补丁逐文件审，不把无关改动算成本轮修复。当前工作区已包含补丁，勿重复应用。Claude 的执行权限按各仓 AGENTS.md：可评审/改代码，编译测试交 Codex 执行。包内包含实际证据，未执行项不能改写为通过。