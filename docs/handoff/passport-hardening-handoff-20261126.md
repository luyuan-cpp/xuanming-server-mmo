# 官方通行证加固：验证交接（2026-11-26，交 Gemini；代码已改完，Gemini 只验证）

## 0. 范围与现状

- **内容**：负责人 2026-11-26 批准全做的四批，设计与取舍都在 [official-passport-registration.md](../design/official-passport-registration.md)：
  - §3.6 通行证改密 / 封禁 / 删除之后，登录与刷新时吊销改密之前的游戏会话并踢下线（新提示码 `kLoginPassportCredentialChanged` = 2059）；
  - §3.7 通行证回调（webhook）→ 网关 `POST /internal/passport/events` → login `LoginPreGate.HandlePassportEvent`，改密、封禁的那一刻就踢；
  - §3.8 客户端正式包锁地址、桌面五个回调端口；手机回调用自定义协议头 + PKCE（安卓 11-27 复查后撤回 App Links，原因见设计稿 §3.8 第 3 条）；
  - §3.9 首启隐私弹窗与注册页协议勾选；
  - §3.10 生产部署清单 `deploy/k8s/passport/`（白名单入口、页面请求带静默登录参数 `prompt` / `silentSignin` 的回 403、网络隔离、验证码记录清理、按环境渲染初始化数据）。
- **代码状态**：
  - 服务端：一部分已被别的会话的批量提交收进 HEAD（3eb57a09a、d77a458ee、76e2b59bb、a0b3d195e），其余在 main 工作区**未提交**：`deploy/passport/init_data.json`、`deploy/k8s/passport/`（新目录）、`go/login` 的 `pkg/auth/passport_provider.go`、`pkg/passportsession/`、`logic/loginpregate/handlepassporteventlogic.go`（新）、`server/loginpregate/loginpregateserver.go`、`rpclogging/config*.go`，网关的 `LoginRpcClient.java`、`PassportEventController.java`（新）、`application.yaml` 与两份新用例，`tools/scripts/k8s_deploy.ps1`、`tests/k8s_deploy_contract.tests.ps1`、`tests/passport_k8s_contract.tests.ps1`（新），`proto/login/login.proto`、`data/tip/Tip.xlsx`。
  - 客户端（`mmorpg-client`）：`GameClient.cs`、`AntiAddictionTests.cs` 已在 d9902efc；其余未提交：`Assets/Scripts/Core/ClientEndpoints.cs`、`PrivacyConsent.cs`、`ClientSettings.cs`，`Assets/Resources/ClientEndpoints.json`，`Assets/Editor/ClientEndpointsBuildGuard.cs`、`ClientPlayerBuild.cs`、`PassportMobileDeepLinkBuildPostProcess.cs`，`Assets/Scripts/Net/PassportOAuthProtocol.cs`、`PassportSignIn.cs`，`QdaoServerSelectView.cs` / `.LoginUtilities.cs` / `.Visuals.cs` / `.Privacy.cs`（新），`tools/build_client.ps1`，两份 EditMode 用例（新 `ClientEndpointsTests.cs`、改 `PassportSignInTests.cs`）；新文件都带 `.meta`。
  - `k8s_deploy.ps1` 与它的契约测试里还有别的会话未提交的改动（match 的 FriendRpc / ChatRpc / GuildRpc 等），红在那些地方的不归本单。
- **生成状态**：工作区里的生成产物已经含本轮的协议（`HandlePassportEvent`、`PassportEventRequest/Response`）与提示码（Go `LoginError_kLoginPassportCredentialChanged = 2059`、C# `KLoginPassportCredentialChanged`），是有人跑过导表与 proto-gen，没有核对是否与当前源一致：照样重跑一遍。
- **验证状态**：全部没有编译、没有运行。
- **Java 独立仓**：只在它的 `PARITY.md` 登记待做，不写代码；本仓 `java/gateway_node` 照常验证。

## 1. 规矩

与 [third-party-login-and-anti-addiction-handoff-20261126.md](third-party-login-and-anti-addiction-handoff-20261126.md) §1 相同，摘要：

- 生成、编译、测试、冒烟用 `Carbon Medium`。要改代码（含测试代码、部署脚本、清单）时保留第一条错误原文与复现命令，这一步停下，交回 Claude（新开会话读本单与设计稿 §3.6~§3.10）；和它无关的后续步骤可以接着跑，结果里写明哪些是在红的基础上跑的。
- 只在 main 工作区做；不开分支、不 push、不 tag；不 `git add -A` / `git reset` / `git checkout --`；负责人要求时才提交，且只提交自己的改动。
- 失败时不注释断言、不放宽、不跳用例、不连续自动重试。
- 游戏没上线，数据可以全清。冒烟前按 `docs/rules/stress-testing.md` 清场；**通行证的库也要清**（`init_data.json` 改了，Casdoor 只在全新初始化时读它）。
- 结果在 `docs/PROGRESS.md` 文末追加「Gemini：官方通行证加固验证」，列命令、通过数与失败原文摘要；日志放 `build/verification/<运行名>/`。

## 2. 生成

1. 客户端仓根跑 `.\gen.bat`（服务端导表 + 服务端 proto-gen + 客户端同步与编译检查，见 `docs/rules/proto-and-tables.md` §2）。
2. 核对（都应能 grep 到）：
   - `go/shared/generated/pb/table/login_error_tip.pb.go`：`LoginError_kLoginPassportCredentialChanged`；
   - `go/proto/login/login_grpc.pb.go`：`HandlePassportEvent`；
   - 客户端 `Assets/Scripts/Proto/Generated/LoginErrorTip.cs`：`KLoginPassportCredentialChanged`。
3. `git diff --stat` 看生成目录：除了本轮的两处，别的改动来自别的会话的表或协议，原样记录，不回退。

## 3. 服务端

### 3.1 go/login（工作目录 `go/login`）

1. `go build ./...`、`go vet ./...`。
2. `go test ./internal/logic/pkg/token/ ./internal/logic/pkg/auth/ ./internal/logic/pkg/passportsession/ ./internal/logic/clientplayerlogin/ ./internal/logic/loginpregate/ ./internal/rpclogging/ ./internal/svc/ ./internal/config/ ./cmd/sandbox_mock/ . -count=1 -v`，逐个点名看到通过：
   - token：`TestIdentityIssuedAtSurvivesRotation`、`TestRevokeIssuedBeforeKeepsLaterLogins`；
   - auth：`TestPassportAdminLookupUserAndSessionVerdict`、`TestPassportAdminLookupUserByName`、`TestPassportAdminDeleteUser*`、`TestNewPassportAdminRequiresAllSettings`、`TestPassportSubject`，以及 `passport_provider_test.go` 的全部；
   - passportsession：`TestCheckLoginRefusesIdentityTokensFromBeforeThePasswordChange`、`TestCheckLoginAfterThePasswordChangeRevokesOnlyEarlierSessions`、`TestBannedOrDeletedUsersLoseEverySession`、`TestPassportOutagesLetLoginsAndRefreshesThrough`、`TestCheckRefresh`、`TestNilEnforcerLetsEverythingThrough`、`TestHandleEventChecksTheNamedUsersAgainstThePassport`、`TestHandleEventRevokesEverythingForBannedUsers`、`TestHandleEventSkipsAppPseudoUsers`（11-27 补：应用凭据发起的回调不报错）、`TestKickRunsInTheBackground`（11-27 补：踢线在后台、登录不等它）；
   - clientplayerlogin：`TestRefreshRevokesPassportSessionsLoggedInBeforePasswordChange`、`TestRefreshRenewsPassportSessionsLoggedInAfterPasswordChange`、`TestRefreshRenewsWhenPassportIsUnreachable`、`TestRefreshDoesNotAskPassportForOtherLogins`；
   - rpclogging：`config_test.go` 全部（屏蔽名单 7 个方法；11-27 补了回调请求体按 base64 出现在日志里也算泄漏）；
   - clientplayerlogin 的 `TestAuthFailureTip`（11-27 补：重新认证被通行证核对拒绝时回 2059）；
   - sandbox_mock：`TestPassportAdminCheckSessionAgainstSandboxMock`、`TestPassportAdmin*AgainstSandboxMock`、`TestPassportProviderAgainstSandboxMock`；
   - 包根：`TestGoctlServerWrappersImplementEveryRPC`（`goctl_wrappers_test.go`，查 `HandlePassportEvent` 的手写转发）。
3. 并发：`go test ./internal/logic/pkg/passportsession/ ./internal/logic/pkg/token/ -race -count=3`。
4. 全量：`go test ./... -count=1`。

### 3.2 C++（`login.proto` 变了，生成的 login gRPC 客户端与服务元数据跟着变）

`msbuild game.sln /m:1 /p:Configuration=Debug /p:Platform=x64` 整解编过即可；本轮没有 C++ 手写代码。

### 3.3 网关（仓库根）

1. 定向（`PassportEventControllerTest` 11-27 改成按 `HttpServletRequest` 读请求体，新增 `readsNothingBeforeTheSecretChecksOut`）：`mvn -B -f java/pom.xml -pl gateway_node -am test "-Dtest=PassportEventControllerTest,LoginRpcClientPassportEventCodecTest,LoginRpcClientParseTest" -Dsurefire.failIfNoSpecifiedTests=false`。
2. 全量：`mvn -B -f java/pom.xml -pl gateway_node -am test`。

### 3.4 部署契约与脚本（仓库根）

1. `pwsh -NoProfile -File tools/scripts/tests/passport_k8s_contract.tests.ps1`（白名单、集群内入口、镜像摘要、初始化数据缺省、渲染脚本）。
2. `pwsh -NoProfile -File tools/scripts/tests/gate_pick_alerts_contract.tests.ps1`（`login.passport-session` 告警组那一条：11-27 起组里两条规则，`LoginPassportSessionCheckFailing` 与 `LoginPassportSessionMassRevocation`）。
3. `pwsh -NoProfile -File tools/scripts/tests/k8s_deploy_contract.tests.ps1`（`gateway-passport-webhook` 密钥条目与「没设环境变量不注入」那一条）。
4. 语法：对 `deploy/k8s/passport/render-init-data.ps1`、`tools/scripts/k8s_deploy.ps1`、客户端 `tools/build_client.ps1` 跑 `[System.Management.Automation.Language.Parser]::ParseFile(<路径>, [ref]$null, [ref]$errors)`，`$errors` 应为空。
5. nginx 配置能被 nginx 自己认（`public-entry.yaml` 的 ConfigMap 第一段）：
   ```powershell
   $run = 'build/verification/<运行名>'; New-Item -ItemType Directory -Force $run | Out-Null
   $lines = Get-Content deploy/k8s/passport/public-entry.yaml
   $start = [Array]::IndexOf($lines, '  nginx.conf: |') + 1; $end = [Array]::IndexOf($lines, '---')
   $lines[$start..($end - 1)] | ForEach-Object { if ($_.Length -ge 4) { $_.Substring(4) } else { '' } } | Set-Content "$run/nginx.conf"
   docker run --rm --add-host casdoor.passport.svc.cluster.local:127.0.0.1 -v "${PWD}/${run}/nginx.conf:/etc/nginx/nginx.conf:ro" nginx:1.30.8-alpine@sha256:8eb04fe3b385090e9aa02961f77398ad09b6bf33ab07f6669a5bb7acce22c0d3 nginx -t
   ```
   期望 `syntax is ok` / `test is successful`。
6. 有 kind 集群时：`kubectl apply --dry-run=server -f deploy/k8s/passport/`；渲染脚本的输出也过一遍：
   ```powershell
   $env:MMORPG_GATEWAY_PASSPORT_WEBHOOK_SECRET = 'x' * 40
   pwsh -File deploy/k8s/passport/render-init-data.ps1 -PublicOrigin https://passport.example.com -TermsUrl https://www.example.com/terms -PrivacyUrl https://www.example.com/privacy -WebhookUrl http://gateway.zone-1.svc.cluster.local:8081/internal/passport/events | kubectl apply --dry-run=server -f -
   ```
   没有集群就跳过并写明。

## 4. 客户端（客户端仓根）

1. `pwsh -File tools/client_compile_check.ps1 -ShowErrors 40`，期望 0 error（运行时代码、Editor 程序集、EditMode 测试程序集都要编过）。
2. EditMode 跑 `MmorpgClient.Tests.EditMode.Net`，点名：`ClientEndpointsTests` 的 4 个（`ParseTrimsAndToleratesBadJson`、`ReleaseBuildsNeedExternalHttpsEndpoints`、`ReleaseOverridesAreCompiledOutOfPlayerBuilds`、`PrivacyConsentVersionOnlyGoesUp`），`PassportSignInTests` 新增的 `LoopbackRedirectCandidates_FiveConsecutivePortsOnlyForLoopback` 与原有的全部，`AntiAddictionTests.PassportCredentialChangedHasTexts`。
3. 出包检查（Windows 包即可）：
   - `pwsh -File tools/build_client.ps1 -OutDir <新目录>`（正式包、不给地址）：脚本直接报 `A release build needs -GatewayUrl, -TermsUrl and -PrivacyUrl …`；
   - 正式包给回环或 http 地址（如 `-GatewayUrl http://127.0.0.1:8081`，协议地址给 https）：Unity 构建前检查 `ClientEndpointsBuildGuard` 让构建失败，日志里有原因；
   - 正式包给全套 https 地址：出包成功，`build-manifest.json` 记着这四个地址，出包后 `git status Assets/Resources/ClientEndpoints.json` 无改动（构建完已还原）；
   - `-Development`：不给地址也照常出包。
   - 有安卓出包环境时：正式包与开发包的 `AndroidManifest.xml` 主 Activity 都有 `xuanming://passport/callback` 的 intent-filter（VIEW / DEFAULT / BROWSABLE），都没有 https 的 `autoVerify` 过滤器。有真机时在两三台国产手机上用它们自带的浏览器各走一次「官方账号登录」，能回到游戏并进入。没有环境就跳过并写明。

## 5. 冒烟

前提：§2~§4 全绿；按清场规矩清空；通行证从空库起：`docker compose -f deploy/passport/docker-compose.yml down -v`，再按设计稿 §5.2 第 1~6 步起 Casdoor、建测试用户 alice、取服务端应用密钥，login 按第 4 步配 `Passport`（含 `AdminClientId` / `AdminClientSecret`）。

1. **起栈**：login（Mode dev）、网关（启动前设 `$env:MMORPG_GATEWAY_PASSPORT_WEBHOOK_SECRET` 为 40 个字符）、gate / scene 等本机常规栈。
2. **回调接通**：Casdoor 后台（`http://127.0.0.1:8000`，built-in/admin）→ Webhooks → `webhook-xuanming-game-sessions`：URL 改 `http://host.docker.internal:8081/internal/passport/events`，请求头 `X-Passport-Webhook-Secret` 填同一个值，勾上启用，保存。
3. **回调入口自检**（`curl.exe -X POST http://127.0.0.1:8081/internal/passport/events -H "Content-Type: application/json"`，同 Casdoor 发的内容类型）：
   - 不带密钥请求头 POST `{}` → 401；
   - 带对的请求头 POST `not json` → 400；POST `{}` → 200（没有要核对的人，`login_passport_session_check_total{phase="event",result="ignored"}` +1）；
   - 网关不设密钥重启 → 同一请求 404（入口关着）；测完设回去。
4. **首启隐私弹窗**（Unity 编辑器；先删本机设置键 `mmorpg.privacy.accepted_version`）：
   - 弹窗盖住登录界面，选服、登录、注册都点不了，Esc 关不掉；
   - 本机 `ClientEndpoints.json` 的协议地址是空的：点「阅读《用户协议》」状态栏提示「这个包没有配置《用户协议》的地址」；
   - 「不同意」第一次只说明后果，再点退出（编辑器里看日志）；
   - 「同意并继续」后正常登录；重启不再弹。
5. **回调踢线（改密）**：编辑器「官方账号登录」用 alice 进游戏 → 后台改 alice 的密码：几秒内客户端提示「账号在官方通行证修改了密码或已被停用，请重新登录」并回到登录页、不自动重连；login 日志有 `[passport] sessions revoked: phase=event`；`{phase="event",result="password_changed"}` +1；网关回 Casdoor 200。用新密码重新登录成功（`{phase="login",result="valid"}`），之后不被踢。
6. **回调踢线（封禁）**：后台把 alice 设为禁用并保存 → 同样被踢，`{phase="event",result="user_forbidden"}` +1；再登录时 Casdoor 登录页自己拒绝（被封用户拿不到身份令牌）；解除禁用后能登录。
7. **登录时吊销（不靠回调）**：后台关掉回调 → alice 在编辑器（A）在线 → 后台改密：A 不掉线（没有回调）→ 另起一个开发包客户端（B）用新密码登录 alice：A 当场被踢并显示同一句提示，login 日志 `[passport] sessions revoked: phase=login`，B 不受影响。
8. 刷新时吊销、通行证故障放行、旧身份令牌被拒这几条，冒烟里不好造，由 §3.1 的 `TestRefresh*`、`TestPassportOutages*`、`TestCheckLogin*` 与 sandbox_mock 用例覆盖。
9. **可选：白名单入口走一遍**（预发前最好做，README 第 7 步的本机版）：查 Casdoor 容器所在网络与地址 `docker inspect mmorpg-passport-casdoor -f '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{$v.IPAddress}}{{end}}'`，用 §3.4 第 5 步抽出的 `nginx.conf` 起一个 nginx：`--network <网络> --add-host casdoor.passport.svc.cluster.local:<地址> -p 127.0.0.1:8090:8080`。浏览器开 `http://127.0.0.1:8090/login/xuanming` 走登录、注册页（勾选协议）、找回密码；开发者工具里看有没有被 404 的 `/api/*`。有就记下路径交回 Claude 补进白名单与契约测试，不要自己加。再核对静默登录拦截：`curl.exe -s -o NUL -w "%{http_code}" "http://127.0.0.1:8090/login/oauth/authorize?client_id=xuanming-game-client&response_type=code&prompt=none"` 回 403，把 `prompt` 换成 `%70rompt`、`silentSignin=1` 也是 403，去掉这个参数回 200。
