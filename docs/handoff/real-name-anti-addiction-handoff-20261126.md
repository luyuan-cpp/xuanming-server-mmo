# 实名认证与防沉迷：剩余代码与全部验证交 Gemini（2026-11-26）

> 写代码、复核本单的分析：Gemini 4 Neon Extra；生成、编译、跑测、冒烟：Carbon Medium（AGENTS.md §10.3）。
> 本单由 Claude（Opus 5.5，非最高档）整理。§2.1 的缺陷分析请先按代码复核，确认了再改。
> 只在 main 上改，不开分支；负责人没说提交就不提交，不 push。工作区里有很多别的会话的未提交改动：只碰本单列出的文件和段落，不要 `git add -A`。
> 本单是总入口 [third-party-login-and-anti-addiction-handoff-20261126.md](third-party-login-and-anti-addiction-handoff-20261126.md) §3 的细节。清场、生成以及几条线一起跑的顺序，按总入口 §4；实名这条线写什么、怎么验，以本单为准。

## 1. 现状

- 设计稿：[docs/design/real-name-anti-addiction.md](../design/real-name-anti-addiction.md)。§5 是 App 形态的方案，§5.3~§5.5 已于 11-26 按当前代码重写，并写明了 §2.1 修正后的行为。
- 代码都在 main 上（被 b6a08148c、3aea2802d 等批量提交带入），**从没编译、运行过**。各批的文件清单与命令看 `docs/PROGRESS.md` 的这些条目：2026-11-19「实名认证与未成年人防沉迷落码」、同日 K8s 部署两条和 go/payment 限额闸两条、2026-11-20「收掉以后再做的四处」、2026-11-21 Gemini「NPPA 账本 16 分片」。
- 2026-11-20 第二批在 PROGRESS 里没有单独条目，内容如下：
  - **上报改成 Redis 持久账本**：`go/login/internal/logic/pkg/nppa/ledger.go`（新增）和 `reporter.go`（重写，去掉了内存队列）；`go/shared/redisslot/login_keys.go`（账本键，删了 `LoginAntiAddictionKey`）。login 侧接线在 `clientplayerlogin/anti_addiction.go`（`beginWatchedSession` / `endWatchedSession` / `renewWatchedSession`）、`loginlogic.go`、`renewsessionlogic.go`、`session_lifecycle.go`；`svc/anti_addiction.go` 加了 `Ledger`、`Clock`、`Now()`。
  - **到点断开改由 gate 本地做**：`cpp/nodes/gate/handler/rpc/minor_play_deadline.{h,cpp}`（新增）；`cpp/libs/services/gate/session/comp/session_info_comp.h`（`minorPlayDeadlineMs`）、`login_admission.h`（`SuccessfulLoginResponse`）、`client_request_lane.cpp`、`rpc_reply_bridge.cpp`、`client_session_handler.cpp`（`CloseMinorAtPlayDeadline`）、`main.cpp`（`minorDeadlineTimer`）、`gate.vcxproj(.filters)`；用例 `cpp/nodes/gate/tests/minor_play_deadline_test.cpp`，登记在 `cpp/tests/routing_identity_test/routing_identity_test.vcxproj`。CMakeLists 由 vcxproj 生成，不要手改。
  - `go/shared/antiaddiction` 加了 `PIBirthPrefix`；新增单测 `go/login/internal/svc/anti_addiction_test.go` 和 `clientplayerlogin/anti_addiction_login_test.go`。
- 生成物：11-26 核对过，`go/proto`、`cpp/generated` 里已有 `ResetAccountRealName` 和 `kLoginMinorPlayTimeOver`。本单的改动不碰 proto 和 Tip.xlsx。
- Java 版：11-24 已对齐（Java 仓 `PARITY.md` 的 2026-11-24 行）。本仓不写 Java 代码。

## 2. 要改的代码（按顺序）

### 2.1 【缺陷】截止时刻一到，清扫就删掉账本条目，login 兜底踢线失效

- **位置**：`go/login/internal/logic/pkg/nppa/ledger.go` 里 `luaPrelude` 的 `lease_end`，以及 `sweepScript`。
- **现象**：Begin / Renew 把未成年人的租约封顶在截止时刻。上报器每秒清扫一次，截止那一刻这条到期，被 `HDEL` 删掉。之后 `Renew` 找不到条目，回 -1，`renewWatchedSession` 拿到零值就不踢。gate 本地没关掉的未成年人（主防线出问题时）会一直在线。`clientplayerlogin/anti_addiction_login_test.go` 的 `TestRenewSessionKicksAMinorTheGateFailedToClose` 没跑清扫，所以测不出来。
- **改法**（与设计稿 §5.5「未成年人的租约」一致）：
  1. `lease_end` 只在截止时刻还没到时封顶：`if cap_ms > now_ms and cap_ms < e then e = cap_ms end`。
  2. `sweepScript` 处理一条到期条目时，如果 `cap > 0`，并且租约分数等于 `cap`（说明是因截止时刻到期的，连接可能还活着）：si 非空就照原规则补一条下线（发生时刻取 cap，照旧夹在 [now - 回填, now] 之内）；然后**不删**，改写成 `'||' .. num(cap)`，租约设为 `now_ms + lease`。其余到期条目照旧删除。
  3. 结果应当是：连接还在，下一次续租 `Renew` 仍回截止时刻，兜底踢线；之后断线时 End 删掉条目、不再报下线（si 已空）；连接早没了的，再过一个租约被清扫删掉，不发事件。
- **测试**（先写成红，再改代码转绿）：
  - `anti_addiction_login_test.go` 的 `TestRenewSessionKicksAMinorTheGateFailedToClose`：续租之前把 miniredis 的时间也拨到截止之后（`e.mr.SetTime(antiAddictionDeadline.Add(time.Minute))`），先跑一次 `e.aa.Ledger.Sweep(ctx)`，再断言兜底计数加 1。没修之前它应当是红的。
  - `ledger_test.go`：`TestLedgerCapsAMinorsLeaseAtThePlayDeadline` 原有断言保留（截止时刻清扫 1 条，补一条发生时刻为截止的下线），后面补上：`Renew` 仍回截止时刻；随后 `End` 不再产生事件；另起一条不续租的，再过一个租约被清扫删掉且不发事件。推进时间用文件里的 `runTo`（每 5 分钟清扫一次，避开暂停门槛）。
  - Java 版账本是照本仓移植的，可能有同样的问题：看一下 Java 仓 `com.game.login.compliance` 的账本。如果有，就在 Java 仓 `PARITY.md` 登记待做，不写 Java 代码。

### 2.2 国家系统沙箱 mock（冒烟用）

- 新增 `go/login/cmd/sandbox_mock/nppa.go` 和 `nppa_test.go`，在 `main.go` 的 `newMux()`（约 112 行）登记以下路由。路径与 `nppa.DefaultCheckURL` / `DefaultQueryURL` / `DefaultLoginoutURL` 相同：
  - `POST /idcard/authentication/check`、`GET /idcard/authentication/query`、`POST /behavior/collection/loginout`：先验签、解密，再按规范回 `{"errcode":0,"errmsg":"OK","data":{...}}`。返回结构看 `pkg/nppa/client.go` 的 `authResult` 和 `Loginout`。
  - `GET /mock/nppa/behaviors`：返回收到的全部上下线，冒烟用来核对上线、下线成对，且 si 一致。
- 在 `pkg/nppa` 里加一个导出的服务端校验函数，例如 `VerifyRequest(secretKey string, r *http.Request) ([]byte, error)`：核对 appId、bizId、timestamps、sign，并解出报文体。mock 和 `client_test.go` 共用它，删掉 `client_test.go` 里测试私有的 `decrypt`（第 28 行）和 `verifyRequest`（第 86 行）。
- 结果按姓名和身份证号决定：
  - 身份证号过不了 `antiaddiction.ValidIDNumber` → `errcode 2001`；
  - 姓名含「不匹配」→ `status 2`；
  - 姓名含「认证中」→ check 回 `status 1`，用同一个 ai 再 query 时回 `status 0` 加 pi；
  - 其余 → `status 0` 加 pi。
  - 签名错或报文解不开 → 回规范里对应的错误码；上报批次里只要带了某个固定 pi（在 mock 里写成常量），整批失败，用来走重试路径。
  - pi 的算法：`antiaddiction.PIBirthPrefix(身份证号第 7~14 位的出生日期)` 后接 SHA-256(身份证号) 的前 32 个十六进制字符，同一个人每次拿到同一个 pi。
- 单测照 `idtoken_test.go` 的写法：用真的 `nppa.Client` 打 `newMux()`，覆盖成功、认证中、认证失败、签名错四种，再加一条上报。
- 默认凭据用 `client_test.go` 里规范样例的 secretKey、appId、bizId，允许用环境变量覆盖（照 `main.go` 里其它 mock 读配置的方式）。mock 也不在日志里打姓名和身份证号。
- `deploy/login-stack.linux/login.yaml` 加 `AntiAddiction` 段，三个地址指向 `http://sandbox-mock:18090`（字段见 `go/login/internal/config/anti_addiction.go`）。但 `Enabled` 缺省为 `false`：robot 走口令登录，打开后会被要求先实名。冒烟时再打开。
- 在 `docs/ops/wechat-qq-sandbox-runbook.md` 末尾加 §J「国家防沉迷系统 mock」，写清楚怎么用：`AntiAddiction.Enabled: true`；`Nppa.CheckUrl` / `QueryUrl` / `LoginoutUrl` 指到 mock；secretKey 放进 `Nppa.SecretKeyEnv` 指向的环境变量（读法见 `go/login/internal/config/secrets.go` 的 `NppaSecretKey`）；再附上 §3 第 4 步的冒烟步骤。

## 3. 生成、编译、测试、冒烟（Carbon Medium，按顺序）

1. **静态**：本单碰过的 Go 目录跑 `gofmt -l`，输出为空；`python tools/scripts/tests/test_cpp_no_raw_pointer_member.py`、`python tools/scripts/tests/test_cpp_thread_sources.py`、`python tools/scripts/check_log_text_english.py` 都要过。
2. **Go**：
   ```
   cd go/login && go build ./... && go vet ./internal/logic/pkg/nppa/... ./internal/logic/clientplayerlogin/... ./internal/svc/... ./cmd/sandbox_mock/...
   cd go/login && go test -count=1 -v ./internal/logic/pkg/nppa/... ./internal/logic/pkg/realname/... ./internal/svc/... ./internal/logic/clientplayerlogin/... ./internal/logic/loginadmin/... ./internal/logic/loginpregate/... ./cmd/sandbox_mock/...
   cd go/shared && go test -count=1 ./antiaddiction/... ./redisslot/... ./accountdirectory/...
   cd go/data_service && go test -count=1 ./internal/store/... ./internal/server/...
   cd go/payment && go test -count=1 ./...
   ```
   通过标准：全部 PASS，下面这些用例在 `-v` 输出里都是 PASS，不是 SKIP：
   - `nppa` 包：`TestLedger*` 13 条、`TestTick*` 7 条、`TestQuota*` 2 条，加上 `client_test.go` 原有用例；
   - `svc` 包：`TestEvaluate*` 3 条、`TestNowUsesTheInjectedClock`；
   - `clientplayerlogin` 包：`TestGateLogin*` 7 条、`TestRenewSession*Kicks*`、`TestRenewSessionNeverKicksAnAdult`；
   - `loginadmin` 包：`TestResetAccountRealName*` 3 条；
   - `redisslot` 包：`TestNppaLedgerKeysShareSlotPerShardAndSpreadAcrossSlots`；
   - 加上 §2 新写的用例。
   data_service 里带 TiDB 的实名用例要真库，没库时如实记「跳过」。
3. **C++**（Windows，MSBuild 串行 `/m:1`）：命令见 `docs/rules/cpp-runtime-and-build.md`。先编 gate 依赖的库（测试工程链接的是预编好的 lib），再编 `cpp/nodes/gate/gate.vcxproj` 和 `cpp/tests/routing_identity_test/routing_identity_test.vcxproj`。然后运行 `build\cpp\tests\routing_identity_test.exe --gtest_filter=MinorPlayDeadline*`，9 条都要 PASS（`MinorPlayDeadlineTest.*` 7 条、`MinorPlayDeadlineReplyTest.*` 2 条）。Linux 再用 `build_linux.sh` 编一遍 gate。
4. **冒烟**（本机栈：真 Redis，login 打开防沉迷并指向 §2.2 的 mock，加上 gateway、gate、robot）：
   1. 口令账号调 `/api/login`，应答带 `anti_addiction.real_name_required=true`；直接用 robot 进 gate 时，Login 回 `kLoginRealNameRequired`。
   2. 用 curl 凭 access token 调 `/api/real-name`，提交一个成年人身份证号，成功。之后 robot 登录成功；`/mock/nppa/behaviors` 里出现一条 `bt=1`。robot 断开后约 2 秒内出现 si 相同的一条 `bt=0`。
   3. 用一个出生在 2012 年的身份证号实名：不在可玩时段时，Login 回 `kLoginMinorNotInPlayTime`。在时段内的到点断开由单测覆盖；如果正好在周五到周日的 20:00~21:00 跑，可以顺带看 gate 在 20:59:55 左右断开。
   4. 登录后立刻 `kill -9` 这个 login 副本，再起一个：事件不能丢，下线由清扫补上（发生时刻往回最多 120 秒）。
   5. 用这个未成年账号在 payment 下单：超过限额（8~16 岁单笔 50 元）时被拒。
   6. 账本的 Lua 用了 `redis.call('TIME')` 和 `string.format('%.0f')`，此前只在 miniredis 下测过。冒烟要在真 Redis 上跑：K8s 钉的 7.2 和本机 compose 的 8.x 各跑一轮，记下版本。
5. **记录**：在 `docs/PROGRESS.md` 追加一条（只追加），写清楚改了哪些文件、新增了哪些用例名、每一步的实际结果，失败的保留首错原文。设计稿状态行和 §5.5 那段「代码待改」按结果更新。

## 4. 已定案，不要再改

- 姓名和身份证号不存、不打日志，只存 pi。
- `wechat_minigame`、`douyin_minigame` 由平台实名和执行限制，其余渠道一律自管、fail-closed。判定按本次登录的渠道（access_token 带着签发时的 auth_type）。账号上有 pi 时，走任何渠道都按我们的规则限。
- 单日充值上限 = 单次上限，只在 `antiaddiction.SpendLimitAt` 一处定义。
- 国家接口的全服共用额度（GCRA，放在登录域 Redis）：check 80/s，query 240/s，loginout 8/s。
- Disconnect 不为上报重放：End 失败靠清扫补。
- `play_deadline_ms` 是绝对 Unix 毫秒，gate 按自己的墙钟提前 5 秒关；时钟偏差由启动核对的 30 秒门槛兜住。
- 账本 16 分片（11-21）。将来要改分片数，必须同时清掉账本键。

## 5. 没核实的（不是代码问题，留给负责人）

- 国家系统的报文格式只对过规范样例，还没连过真接口。真接口要等负责人拿到测试用的 appId、bizId、secretKey（设计稿 §9）。
- 微信 App 渠道的绑定方式、抖音小游戏是否由平台执行付费限额：见设计稿 §3、§9。
