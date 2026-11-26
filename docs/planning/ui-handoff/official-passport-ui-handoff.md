# 官方通行证与账号注册 · Unity UI 异机交接

另一台电脑的 Codex 负责配套 UI 切图素材（按项目既有的统一 UI 美术风格）；客户端代码/Prefab/联调由 Gemini 与客户端研发负责。服务端独立部署栈（`deploy/passport/`）与 `go/login` 的 `auth_type = "passport"` 验签接缝已完成（详见 [官方通行证接入设计](../../design/official-passport-registration.md)）。

> **美术边界声明**：视觉风格、材质与配色完全按项目既有的统一 UI 美术风格执行，**本文档不指定任何画风词、材质词或色号**，只明确需要做什么 UI/切图、用途、尺寸、文件名、透明底、严禁带文字与拉伸规则。

本次**不修改任何 `.proto`**，禁止手改 `Assets/Scripts/Proto/Generated/*` 与 `*Rpc.cs`。

---

## 一、素材清单（复用为主，仅增少量切图）

- **排布与适配**：中文简体清晰可读，锚定 2560×1080 横屏安全区，兼容 16:9 与带刘海 21:9 细长屏；视觉风格与配色直接沿用项目既有统一 UI 美术体系，本文档不另定画风或色号。

| 素材项 | 处理方式 | 规格与位置说明 |
|---|---|---|
| 登录底图与账号弹窗底框 | **直接复用** | `login_shell`、`main_frame`（不重绘底图与窗口外框） |
| 按钮与输入框控件 | **直接复用** | `clean_primary`（主操作）、`clean_tab`（次操作/取消）、`clean_search`（输入框），以及既有 `AccountDialog` 通用提示窗 |
| Apple / Google 按钮 | **保持原样** | 沿用 `apple_signin_button` / `google_signin_button` 平台审核规范控件 |
| **官方通行证入口图标** | **新增 1 枚小图** | 命名 `passport_jade_seal.png`（放在 `Assets/Resources/UI/Ugui/CoreFidelity20261016/passport_jade_seal.png`），88×88（或 128×128，界面显示 44×44）PNG-32 透明底，严禁带任何文字/字母/数字；用途为「官方账号登录/注册」按钮左侧图标，小尺寸下轮廓清晰易辨认；未出图前先用无图 `clean_primary` 按钮占位并标明待替换 |
| Casdoor 浏览器页换肤（可选） | **配置即可，不改代码** | 在 Casdoor 应用后台上传游戏 Logo 与配套背景图，与游戏内既有界面保持协调 |

---

## 二、需要改造与新增的 3 个 UI 切面

优先扩展现有 `QdaoServerSelectView`（账号弹窗 + 通用弹窗），不另开独立场景。

| 界面 / 弹框 | 呈现内容与按钮 | 交互与显示规则 |
|---|---|---|
| **1. 账号登录弹窗（扩展 `_credentialPanel`）** | 在既有账号/密码输入框下方新增官方通行证入口按钮：「官方账号登录 / 注册」（左侧配玉牌图标 `passport_jade_seal`），底部副文案一行小字说明「支持邮箱注册、登录与找回密码」 | ① `PassportSignIn.Available` 为 true 且**不在微信/抖音小游戏宿主**内显示；② 桌面端无 Apple/Google 时居中摆在原渠道位（`y=900, 600×90`）；移动端同时有 Apple/Google 时上下或左右错开排版，严禁重叠；③ `_busy` 时禁用；④ 点击触发 `BeginChannelSignIn(LoginAuthTypes.Passport)` |
| **2. 通行证授权等待窗（复用 `UtilityPanel` / `_accountDialog`）** | 复用既有弹窗底框 `main_frame`，标题「通行证授权中」；正文「已在浏览器打开官方通行证，请在浏览器完成注册或登录后返回游戏。」+ 旋转等待标识；双按钮：**「重新打开页面」**（`clean_tab`）、**「取消授权」**（`clean_tab`） | ① 拉起浏览器后立即弹出并锁住后方点击；② 若玩家误关浏览器可点「重新打开页面」复开同一授权 URL；③ 点「取消授权」或按 `Esc` 立刻调用 `PassportSignIn.Cancel()`，关闭本地回调监听、恢复 `_busy = false`，绝不卡死在等待态 |
| **3. 注销账号重认证与结果提示（复用 `QdaoServerSelectView.LoginUtilities.cs`）** | 沿用现有设置页「注销账号」与冷静期撤销弹窗 | ① `ClientSettings.LastLoginMethod == LoginAuthTypes.Passport` 时，申请注销走 `PassportSignIn.Request` 重新拿一枚新的 `id_token` 提交 `/api/account-deletion`（不传 `authorization_code`）；② 冷静期撤销、已注销新号提示、国内实名窗全部沿用现有通道，零分叉 |

---

## 三、状态文案与异常恢复

| 触发条件 | 玩家看见什么 | 可做什么 |
|---|---|---|
| 正在等待浏览器完成注册/登录 | 弹窗「通行证授权中」+「已在浏览器打开官方通行证，请在浏览器完成注册或登录后返回游戏。」 | 可点「重新打开页面」或「取消授权」 |
| 玩家主动取消授权 | 收起等待窗，状态栏提示「已取消官方通行证登录」 | 可重新点击登录或切换其他方式 |
| 等待超过 180 秒未回调 | 收起等待窗，提示「授权已超时，请重新点击登录」 | 自动关闭本地监听，可重试 |
| 回调 `state` 不匹配或缺少 `code` | 收起等待窗，提示「授权校验失败，请重试」 | 丢弃本次上下文，可重新发起 |
| 换取 `id_token` 网络失败 / 网关 `500 (2035/2036)` | 提示「通行证服务暂时不可用，请稍后重试」 | 丢弃旧凭证（不重发），用户可重新发起 |
| 本地开发未配置 `PassportOrigin` / `PassportClientId` | 自动隐藏「官方账号登录 / 注册」按钮 | 正常使用现有账密/其他渠道入口 |
| 本地端口 `18765` 被占用 | 收起等待窗，提示「本地授权回调端口被占用，请关闭冲突程序后重试」 | 立即解锁界面，记 `Debug.LogWarning`（不含 token） |

---

## 四、客户端代码与协议接缝清单

1. **`Assets/Scripts/Net/LoginCredential.cs`**：在 `LoginAuthTypes` 新增 `public const string Passport = "passport";`。
2. **`Assets/Scripts/Core/ClientSettings.cs`**（及启动参数覆写）：新增 `PassportOrigin`、`PassportClientId`、`PassportRedirectUri`（默认空串=隐藏入口；本地开发默认对应 `http://127.0.0.1:8000`、`xuanming-game-client`、`http://127.0.0.1:18765/passport/callback`；移动端为 `xuanming://passport/callback`）。
3. **新增 `Assets/Scripts/Net/PassportSignIn.cs`**（对齐 `PlatformSignIn.cs` 风格，纯 C# 标准库实现，不引入第三方 Unity 插件）：
   - OAuth2 Authorization Code + **PKCE（S256）** + 至少 128-bit 随机 `state`；
   - Public Client，**绝不传 `client_secret`**；
   - 桌面/编辑器起单次 `HttpListener`（**只监听 `127.0.0.1:18765`，严禁 `0.0.0.0`**，180s 超时，支持 `Cancel()` 与 `ReopenBrowser()`），收到回调核对 `state` → 返回道家风格简短中文 HTML「通行证登录已完成，请返回游戏」并立即停监听 → `POST <PassportOrigin>/api/login/oauth/access_token` 换取 `id_token` → 主线程精确回调一次 `PlatformSignInResult`；
   - 移动端先挂 `Application.deepLinkActivated` 接 `xuanming://passport/callback`（未真机验证需在交付注明）；
   - `id_token` 仅内存一次性交给 `LoginCredential.ForThirdParty(LoginAuthTypes.Passport, idToken)`，**不写 `PlayerPrefs`、不落盘、不打日志**。
4. **`QdaoServerSelectView.Visuals.cs` / `QdaoServerSelectView.cs` / `QdaoServerSelectView.LoginUtilities.cs`**：按上表扩展按钮、授权等待窗、`RequestChannel` 分支、注销重认证分支；新增 `[SerializeField]` 字段后通过 `QdaoUguiBuilder` 重新烘焙生产 Prefab 并核对 `ValidatePrefabReferences()`。

---

## 五、可点演示与截图验收

1. **EditMode 单测（新建 `Assets/Tests/EditMode/Net/PassportSignInTests.cs`）**：
   - RFC 7636 Appendix B 固定向量验证 PKCE `S256`；
   - `state` 不匹配拒绝、缺 `code` 拒绝、重复回调幂等、`Cancel()` 仅触发一次 `ErrorCanceled`；
   - `LoginCredential.ForThirdParty(LoginAuthTypes.Passport, "tok")` 构造断言。
2. **截图与交互验收**：
   - 16:9 与带刘海 21:9 横屏分别截图：账号弹窗（含玉牌按钮）、授权等待窗（含重开/取消按钮）、超时/取消恢复态、大字号下无溢出与无遮挡；
   - 键盘导航（`Tab` / `Enter` / `Esc`）与鼠标/触控各走通一次确认与取消；
   - 跑通 `tools/client_compile_check.ps1` 与受影响 EditMode 测试；若接通本机 `deploy/passport/` 与服务端，补一次真实邮箱注册 → 回调进服截图证据。

---

## 六、可直接复制给 Codex 的纯功能作图清单（不含任何风格与色号）

```text
请为「官方通行证与账号注册」产出以下透明背景 PNG 切图（PNG-32，背景完全透明，严禁带任何文字、字母或数字），输出到图仓目录 D:\work\image\designs\official-passport-ui-v1\slices\：
1. passport_jade_seal.png | 128x128 | 「官方账号登录 / 注册」主按钮左侧的通行证徽章图标（界面显示尺寸 44x44，小尺寸轮廓清晰）
2. passport_button_normal.png | 600x90 | 官方通行证主按钮常态无字底图（支持横向拉伸，左侧预留图标位，中心留空用于叠放按钮文字）
3. passport_button_hover.png | 600x90 | 官方通行证主按钮高亮/悬停态无字底图（与常态同构，支持横向拉伸）
4. passport_button_disabled.png | 600x90 | 官方通行证主按钮禁用态无字底图（与常态同构，支持横向拉伸）
5. passport_waiting_badge.png | 160x160 | 浏览器授权等待弹窗中的旋转等待徽标（严格居中对称，便于做平滑旋转动画）
6. passport_web_logo.png | 256x256 | 外部浏览器注册/登录页顶部游戏徽标（透明底）
7. passport_web_bg.png | 1920x1080 | 外部浏览器注册/登录页整屏背景图（无文字，居中留出表单卡片区域）

注：弹窗底框、次级按钮、输入框直接复用既有通用控件，无需重绘。产出放到 D:\work\image\designs\official-passport-ui-v1\slices\ 并附尺寸与拉伸留白清单即可。
```
