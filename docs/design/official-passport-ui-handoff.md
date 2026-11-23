# 官方通行证与账号注册 · 道家 Q 版 Unity UI 异机交接

另一台电脑的 Codex 负责 `mmorpg-client` 的道家 Q 版 UI、Prefab 烘焙、PKCE 授权接入与本地/联机演示验收。服务端独立部署栈（`deploy/passport/`）与 `go/login` 的 `auth_type = "passport"` 验签接缝已完成（详见 [官方通行证接入设计](official-passport-registration.md)）。

本次**不修改任何 `.proto`**，禁止手改 `Assets/Scripts/Proto/Generated/*` 与 `*Rpc.cs`。

---

## 一、道家 Q 版视觉风格与素材清单（复用为主，仅增 1 枚小图标）

严格沿用项目现有道家 Q 版横屏视觉体系（`QdaoCoreFidelityArt`）：
- **色板**：墨玉绿 `#244C3E`（`QdaoCoreFidelityArt.Ink`）、象牙暖米白 `#FFF3D3`（`QdaoCoreFidelityArt.Ivory`）、淡金标题 `#E5CC80`、深松绿描边 `#28412B`、玉青高亮 `#419B76`、半透明玄青遮罩 `rgba(6, 18, 15, 0.78)`。
- **字效与排布**：中文简体清晰可读，重要标题走 `CoreGoldText` 金字描边，常规正文与按钮走 `QdaoCoreFidelityArt.ApplyText`；锚定 2560×1080 横屏安全区，兼容 16:9 与带刘海 21:9 细长屏。

| 素材项 | 处理方式 | 规格与位置说明 |
|---|---|---|
| 登录底图与账号弹窗底框 | **直接复用** | `login_shell`、`main_frame`（不重绘底图与窗口外框） |
| 按钮与输入框控件 | **直接复用** | `clean_primary`（主操作）、`clean_tab`（次操作/取消）、`clean_search`（输入框），以及既有 `AccountDialog` 通用提示窗 |
| Apple / Google 按钮 | **保持原样** | 沿用 `apple_signin_button` / `google_signin_button` 平台审核规范控件，不套古风皮肤 |
| **官方通行证玉牌图标** | **新增 1 枚小图** | 建议命名 `passport_jade_seal`（放在 `Assets/Resources/UI/Ugui/CoreFidelity20261016/passport_jade_seal.png`），88×88（显示 44×44）透明底 PNG：Q 版圆角玉佩/道纹令符造型，暖金细描边 + 玉绿或朱砂印纹点缀，小尺寸高对比易辨认；若美术暂未出图，允许先用无图 `clean_primary` 文字按钮占位并标明待替换 |
| Casdoor 浏览器页换肤（可选） | **配置即可，不改代码** | 在 Casdoor 应用后台配置游戏 Logo + 主题色 `#244C3E`（玉绿）/ 暖米底 `#FFF3D3`，让外部注册/找回密码网页与游戏内道家画风衔接 |

---

## 二、需要改造与新增的 3 个 UI 切面

优先扩展现有 `QdaoServerSelectView`（账号弹窗 + 通用弹窗），不另开独立场景。

| 界面 / 弹框 | 呈现内容与按钮 | 交互与显示规则 |
|---|---|---|
| **1. 账号登录弹窗（扩展 `_credentialPanel`）** | 在既有账号/密码输入框下方新增官方通行证入口按钮：「官方账号登录 / 注册」（左侧配玉牌图标 `passport_jade_seal`），底部副文案一行小字说明「支持邮箱注册、登录与找回密码」 | ① `PassportSignIn.Available` 为 true 且**不在微信/抖音小游戏宿主**内显示；② 桌面端无 Apple/Google 时居中摆在原渠道位（`y=900, 600×90`）；移动端同时有 Apple/Google 时上下或左右错开排版，严禁重叠；③ `_busy` 时禁用；④ 点击触发 `BeginChannelSignIn(LoginAuthTypes.Passport)` |
| **2. 通行证授权等待窗（复用 `UtilityPanel` / `_accountDialog`）** | 暖米纸框 `main_frame`，标题「通行证授权中」；正文「已在浏览器打开官方通行证，请在浏览器完成注册或登录后返回游戏。」+ 旋转八卦/罗盘或静态等待标识；双按钮：**「重新打开页面」**（`clean_tab`）、**「取消授权」**（`clean_tab`） | ① 拉起浏览器后立即弹出并锁住后方点击；② 若玩家误关浏览器可点「重新打开页面」复开同一授权 URL；③ 点「取消授权」或按 `Esc` 立刻调用 `PassportSignIn.Cancel()`，关闭本地回调监听、恢复 `_busy = false`，绝不卡死在等待态 |
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

## 六、直接复制给另一台电脑 Codex 的指令（无客户端目录，仅出道家 Q 版 UI 与切图）

```text
请参照本机 Image 目录下 design 里的现有道家 Q 版横屏风格，制作「官方通行证与账号注册」配套的 UI 设计预览图和无字透明底切图素材包（本机无需改动游戏客户端代码）。

一、风格要求（严格对齐 Image/design 既有道家 Q 版规范）
- 色板参考：墨玉绿 #244C3E、深松绿描边 #28412B、象牙暖米纸 #FFF3D3、淡金标题/细边 #E5CC80、玉青高亮 #419B76，搭配圆润 Q 版云纹、玉佩、篆印等道家元素。
- 基准画布：横屏 2560×1080（兼顾 16:9 与带刘海 21:9 细长横屏安全区）。
- 分层铁律：所有可复用按钮、框体、图标的切图必须为 RGBA 透明底且**不烤死任何中文文字**（文字由客户端动态渲染）；带文案的只放在“整页设计预览图”里。

二、需要交付的 4 张整页设计预览图（2560×1080）
1. 01_account_login_with_passport.png（账号登录弹窗）
   - 在道家 Q 版登录弹窗中保留原有「账号 / 密码」输入框与「取消 / 登录」按钮；
   - 在弹窗下方新增「官方账号登录 / 注册」玉绿镶金主按钮（左侧配玉牌小图标），按钮下方附一行小字副文案「支持邮箱注册、登录与找回密码」；
   - 同时给出一版兼容移动端 Apple / Google 官方按钮时的错开避让布局（不重叠、不裁切）。
2. 02_passport_auth_waiting.png（通行证授权等待弹窗）
   - 玩家点击后拉起外部浏览器，游戏内弹出半透明玄青遮罩 + 暖米纸细金边道家弹窗；
   - 标题「通行证授权中」，中间放一枚可旋转的道纹玉盘/罗盘等待徽标，正文「已在浏览器打开官方通行证，请在浏览器完成注册或登录后返回游戏。」；
   - 底部双按钮：「重新打开页面」（次级暖纸金边按钮）与「取消授权」（次级按钮）。
3. 03_passport_status_dialogs.png（状态与恢复提示窗合集）
   - 同一道家弹窗规范下的 3 种状态示意：授权取消/180秒超时重试提示、账号注销重认证确认提示、7 天注销冷静期「撤销注销 / 退出」提示。
4. 04_passport_web_register_theme.png（浏览器通行证注册/登录页换肤参考，供 Casdoor 托管页配主题）
   - 简洁的网页端注册/登录卡片（邮箱、密码、确认密码、验证码输入框 + 主按钮 + 忘记密码链接），延用同款暖米纸卡片 + 玉绿主按钮 + 淡金边框与顶部道家徽章，让跳出浏览器注册时不割裂。

三、需要交付的无字透明底切图素材（PNG，RGBA 透明背景）
1. passport_jade_seal.png（128×128）：官方通行证玉牌 / 道纹令符图标，缩小到 44×44 时轮廓与细节仍清晰。
2. passport_button_normal.png / passport_button_hover.png / passport_button_disabled.png（600×90 基准，支持 9-slice 拉伸）：官方通行证主按钮三态无字底图（玉绿镶细金边，左侧预留图标位）。
3. passport_waiting_badge.png（160×160）：授权等待窗用的道纹罗盘 / 八卦玉盘透明徽标（居中对称，便于程序做平滑旋转动画）。
4. passport_web_logo.png（256×256 透明）与 passport_web_bg.png（1920×1080 柔和青绿山水祥云底图）：供 Casdoor 网页端直接上传配置。
（弹窗底框、次级按钮、输入框若与 Image/design 已有通用控件完全一致，可注明复用路径，无需重复重绘。）

四、交付目录与验收清单
- 统一输出到本仓 designs/official-passport-ui-v1/（下分 previews/ 放整页预览图、slices/ 放无字透明切图）；
- 附一份 MANIFEST.md，标明每张图的尺寸、透明通道核验结果、9-slice 建议边距、色值与用途，方便客户端后续直接导入切图与对照排版。
```
