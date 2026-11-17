# AI 开发流程管线第一版

本版把任务范围、验收条件、现有检查入口和本轮验证证据连接起来。适用仓库是 C++/Go 服务端、独立 Java 服务端与 Unity 客户端，不包含 Python 服务端版本。它是本地开发验证入口；发布与生产操作仍遵守各仓规则。

## 从需求到交付

1. 复制 `tools/scripts/ai_pipeline/task.template.json`，写清目标、涉及仓库、允许改动范围、检查项与可观察的验收条件。
2. 每项任务逐一填写静态检查、构建、单测、契约、集成、端到端、视觉、性能八类测试。适用时登记检查 ID，不适用时说明原因。不能用“不适用”掩盖实际缺失的验收。
3. 先运行 `plan`，审阅实际工作目录、参数数组、工具路径与检查顺序。先读各仓 `AGENTS.md` 和相关设计；这里的任务卡不覆盖仓库原有规则。
4. 实施任务后运行 `run`。检查串行执行，遇到失败停止后续检查；未运行的必需项不算通过。修复后新建验证目录，再运行受影响任务。
5. 交付时列出改动、验收证据、未验证项目、双版本状态及剩余限制。任何“完成”都应指向具体任务与本轮报告，不能只复述 AI 自己的判断。

任务清单与检查注册表分别是 `tools/scripts/ai_pipeline/tasks/*.json` 和 `tools/scripts/ai_pipeline/checks.json`。前者表达此次目标，后者把检查 ID 映射到复用的真实命令；避免把长 shell 命令藏在自然语言任务里。

## 使用方法

在 C++ 仓库根目录执行：

```powershell
python -X utf8 -B tools/scripts/ai_pipeline/runner.py plan --task tools/scripts/ai_pipeline/tasks/backend-smoke.json
python -X utf8 -B tools/scripts/ai_pipeline/runner.py run --task tools/scripts/ai_pipeline/tasks/backend-smoke.json --output build/verification/ai-pipeline/backend-smoke-first
```

`--output` 必须是新目录。再次验证使用新名字，保留先前失败证据。默认仓库布局为当前 C++ 仓以及同级的 `xuanming-server-mmo`、`mmorpg-client`；本机路径不同可用 `--repos <本机JSON配置>` 覆盖仓库与工具路径。任务卡本身不得保存机器绝对路径。

本机配置格式为 `{"repositories":{"server_cpp":"相对配置文件的仓库路径","server_java":"另一仓库路径","client":"客户端路径"},"tools":{"go":"Go可执行文件路径","jdk21":"JDK21根目录","unity_root":"Unity版本根目录"}}`。只需填写要覆盖的键；工具建议填写绝对路径，本机配置与任务卡分开保存。默认优先使用同级 `tools` 中匹配 `go.mod` 的 Go、JDK 21，以及匹配客户端 `ProjectVersion.txt` 的 Unity。

统一开发入口提供同一流程：

```powershell
pwsh -NoProfile -File tools/scripts/dev_tools.ps1 -Command ai-plan -TaskPath tools/scripts/ai_pipeline/tasks/backend-smoke.json
pwsh -NoProfile -File tools/scripts/dev_tools.ps1 -Command ai-verify -TaskPath tools/scripts/ai_pipeline/tasks/backend-smoke.json -PipelineOutput build/verification/ai-pipeline/backend-smoke-second
```

按需传入 `-RepoConfigPath`；它对应 runner 的 `--repos`。计划阶段只展示待运行内容，不代表任何检查已经通过。

客户端示范可直接使用 `-TaskPath tools/scripts/ai_pipeline/tasks/client-compile-isolation.json`。运行输出 `task.json`、`catalog.json`、`report.json`、`delivery.md`、`checksums.json` 以及各检查日志和产物。目录默认自动生成唯一名字；每次运行的 JSON 与 Markdown 均保存实际参数、结果与版本依据，`checksums.json` 可独立核对证据是否被改动。

退出码为：0＝运行任务通过（或仅计划成功），1＝检查执行/验收失败，2＝配置错误、前置条件缺失、资源占用或必需检查未执行。单项状态区分 `PASS / FAIL / SKIP / NOT_RUN`；计划模式的 `delivery_eligible` 始终为 false。`--checks` / `-CheckIds` 可用于诊断子集，未选的必需项仍使整单不可交付。

## 第一版已登记的验证

| 检查 | 实际覆盖 | 不能据此声明 |
|---|---|---|
| `pipeline-selftest` | 统一执行器的回归与失败判定 | 任意游戏功能完成 |
| `cpp-script-contract` | 现有 C++ 工程依赖与组件读取约束 | C++ 全量编译、运行或联网通过 |
| `log-guard-unit` | 日志语言守卫的单元测试 | 全仓源码已执行日志扫描 |
| `release-contract` | 发布版本、摘要与制品校验的临时夹具回归 | 已出正式包、推镜像或部署 |
| `table-schema-index` | 配置表 schema 索引无漂移 | 所有真实表数据、玩法数值正确 |
| `go-gameday` | Go 游戏日/周边界单测 | 全 Go 仓或真数据库验收 |
| `java-gameday` | Java 游戏日单测及所需模块构建 | Java 全量单测、集成或双版本完全对齐 |
| `cpp-aoi` | 重建并运行匹配 AOI 的 C++ 测试工程 | 所有 C++ 测试或客户端 AOI 视觉验收 |
| `client-compile-isolation-tests` | 使用真实 Roslyn 编译器验证工具的成功、失败、带空格路径、产物留存与并发隔离 | Unity 场景、PlayMode 或联网通过 |
| `client-compile-check` | 当前客户端运行时源码离线编译，保留参数、编译日志与产物 | Unity Player 出包、运行、画面或联网验收 |

`backend-smoke.json` 只串行运行其中五个低成本基础检查；每个都为必需项。它用于演练任务卡、命令执行、验收汇总的闭环，不启动服务，也不触碰真实数据库。

客户端示范任务的功能是“编译检查工具的临时产物隔离及证据留存”。新增回归真实构造正确/错误 C# 项目，覆盖失败不能假绿、并发结果互不覆盖及含空格路径，再对当前客户端源码执行离线编译。这个示范没有修改玩法，也不能据此记为 Unity Player 或联网测试完成。

## 防止验证“假绿”

- 进程退出码必须为零，测试类检查还须证明实际执行了非零数量的测试。失败、超时、跳过或缺失证据不能自动转成通过。
- unittest 读取测试计数和跳过结果；Go 使用 `-count=1 -json`，保留逐测试事件，避免缓存结果被误认为本轮运行。
- Java 适配器只接受白名单 `java-gameday`，在当前进程设置 JDK 21，并用参数数组调用仓库 `mvnw.cmd`。`-o` 禁止 Maven 依赖下载；启动前还检查 wrapper 发行包缓存，缺失直接失败。
- Java 对所选 reactor 执行 `clean test`，避免已删除/改名测试的旧 class 继续运行。通过 `surefire.reportNameSuffix` 生成唯一报告名，再将该文件与来源回执归档到本轮独立 `java-tests/`；不收集历史报告。XML 必须含实际 testcase、正确且非零的计数、无失败/错误/跳过。上游模块允许没有指定测试，不代表目标 `GameDayTest` 可以缺失。参数依据见 [Surefire 官方文档](https://maven.apache.org/surefire/maven-surefire-plugin/test-mojo.html#reportNameSuffix)。
- C++ 检查带 `-Build`，不直接运行旧 exe。已有脚本在 gtest 跳过时可能仍 exit 0，所以本入口额外拒绝实际跳过计数。
- 静态检查可只按退出码判定，但必须明确它覆盖的性质。单测、编译、联网、画面和性能分别记录，互不替代。

## 并行工作与资源锁

同一验证任务内的命令始终串行。执行器的锁只协调通过这个入口启动的任务；它无法阻止用户手动命令、IDE、Unity、其他聊天或外部 CI 同时修改文件、占用服务或构建目录。

各仓当前已有未提交修改时，先记录实际输入与改动范围，避免把别的任务进度纳入本次交付。源码或受检输入在验证期间变化，应重新验证。Java 当前续移植遵守其 `main` 工作目录约定，不因为引入 AI 管线就自动新建并行 worktree。

输入范围按 Git 的字面路径解析；目录包含受跟踪和未忽略的新文件，明确点名的文件（例如生成的 `MmorpgClient.csproj`）即使被忽略也参与哈希。空匹配声明会被拒绝；已跟踪但删除的文件保留缺失记录。范围外的并行改动仅记录工作区状态，不宣称也验证了它们。任务范围应覆盖实际检查输入，执行器无法推断遗漏的业务依赖。

锁位于每个仓库的 Git common directory 中的 `ai-pipeline.lock`，正常结束自动释放。进程被强制结束后若留锁，应先核对锁内 PID 和创建时间、确认所属任务已经停止，再由维护者移除这一个锁文件；执行器不会盲目抢锁或终止未知进程。

## 边界与后续接入

这些 JSON 是受信任的工程代码配置，不是安全沙箱。修改 `argv` 或检查脚本等价于修改可执行代码，需代码审查；本工具不负责隔离恶意命令。适配器限制参数只缩小误用范围，不把整个仓库变成不可信代码的安全执行环境。

本版不自动 commit、push、合并、打 tag、推镜像或部署，不安装/升级工具，不自动清理数据库、容器和玩家数据。Unity 编译检查与独立回归即使通过，也不等于 Editor PlayMode、真实 Player、联网、地图/美术或视觉验收。

Java 完整契约检查后续应从 `contract/SOURCE.properties` 指定的全局源提交和地图源提交分别准备冻结快照，再执行 `ContractSync --check`。当前脏 C++ 树不能冒充指定基线；第一版不登记一个缺少真实输入却看似通过的契约项。

下一步按任务需求接入现有真依赖集成测试、robot 登录/战斗链路、Unity PlayMode/Player、截图验收和有基线的性能测试。每新增一个入口都同时定义前置条件、真实退出码、跳过识别、超时、日志与验收边界，然后再允许任务引用。
