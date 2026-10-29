export const meta = {
  name: 'audit-r2-wave34',
  description: '第二轮只读审计第三、四波：补覆盖查找、low 复核、换视角查找、对抗复核（按 args 选路，单次 ≤6 代理）',
  phases: [
    { title: 'Wave3', detail: '补覆盖与余项增量查找' },
    { title: 'Wave4', detail: '第一轮 22 条 low 复核' },
    { title: 'Dsweep', detail: '换视角查找（每次运行一轮）' },
    { title: 'Verify', detail: 'high / medium 对抗复核' },
  ],
}

const A = args || {}
const WAVE3_KEYS = A.wave3Keys || []
const LOW_REVIEW_ON = !!A.lowReview
const D_ROUND = (typeof A.dRound === 'number') ? A.dRound : null
const SEEN_TITLES = A.seenTitles || []
const VERIFY = A.verify || []
const MEDIUM_LENSES = A.mediumLenses || 2
const VERIFY_NOTE = A.verifyNote || ''

async function batched(thunks, size) {
  const out = []
  for (let i = 0; i < thunks.length; i += size) {
    const part = await parallel(thunks.slice(i, i + size))
    for (const p of part) out.push(p)
  }
  return out
}

const PRE = [
  '你是只读审计员，参与 mmorpg 服务器仓库（D:\\luyuan\\xuanming-server-mmo-cpp，main 分支，HEAD 190d9f2ea，2026-10-22）上线前第二轮「数据问题 / 覆盖回档」审计。用中文作答。',
  '',
  '【只读约束，必须遵守】',
  '- 不创建、不修改、不删除任何文件（包括 %TEMP%、会话草稿目录、/tmp）。',
  '- 按提交读文件只用 git show <rev>:<path> 直接输出到终端，或用 git grep <rev>；git diff 也只直接输出。不得用 > 、>> 、Out-File、Set-Content、tee 之类重定向成文件。',
  '- 不跑任何构建、测试、静态检查（go build / go test / go vet / msbuild / cmake / mvn 等一律不跑，AGENTS.md §10.1）。只读文件、grep、git log/show/diff。',
  '- 不给其他会话或代理发消息。',
  '',
  '【在途文件】工作区有多个会话在并行改代码。读到与 HEAD 不同的文件时以工作区为准，但注明是未提交改动；读到明显是半截的改动时注明"半成品"。',
  '已落码、待 Codex 验证的修复，本轮不复审修复本身（留给代码冻结后的增量审计）：第一波 6 条（r2-gate#1、r2-sm#1/#2/#3、r2-db#1/#3，在 go/scene_manager、cpp/nodes/gate、go/db/internal/saveoutbox）；r2-battle#1 / r2-battle#4（battle_room_manager、result_commit.h、settlement_outbox.h，按成员记落库结论、轮次上限 kMaxStagingRounds）；r2-mz#1（tools/merge_zone 前向 copy 保留 save_barrier、save_barrier_verify.go，go/db/internal/barrierrestore 的 home 回退）；第一轮 go-asset-services#0/#1（go/trade 的 tick_isolation.go、腿结局）；字段保护 3a′（r2-ps#2、r2-ps#5、宝石拆卸预检）；r2-ps#1（只改了 docs/ops/release-checklist.md §2.1 ⑦、§6.2 与设计 §8.1 第 6 条，机器门禁待办）。若发现这些修复与别的模块交互出新问题，照常报，related 写编号。',
  '审计基线说明：第三波第一批（r2 文档 §3.6）是在旧基线 c63b22dca 上查的；之后主干合入大量提交（含四职业技能、组队、全局路由、聚宝斋、账号角色权威、chat/data_service 全球权威等）。',
  '',
  '【要找的问题】',
  '数据问题：丢失、重复（资产复制）、部分提交、ID 冲突、跨存储不一致。',
  '覆盖回档：旧数据盖新数据、双写者、乱序或重试旧载荷、加载旧快照后再存盘。',
  '项目尚未上线（负责人 2026-10-18 确认），首次上线以 go/db dual + scene/login 严格提交屏障整批起步。只在混版、滚动升级、降级窗口出现的问题照报，但标明"仅混版窗口"，严重度降一级。',
  '',
  '【已知问题不要重报】开工前先读两份文档：docs/design/data-overwrite-rollback-audit-20261018.md 的 §2–§4（第一轮，约 140 行）；docs/design/data-overwrite-rollback-audit-r2-20261019.md 的 §3–§4（本轮第一、二波与第三波第一批，含 §3.5 附带观察、§3.6 与 §4 已排除）；docs/design/data-consistency-guarantee-map-20261020.md 的 §2–§4（2026-10-20 另一次一致性核对的发现，含 Redis 回退一族与 cross-service-assets#1）。已登记条目不重报；若发现已登记条目的修复本身有缺陷，或是已登记条目的新变种，要报，并在 related 里写关联编号。',
  '',
  '【严重度】high = 默认配置下可达的资产复制，或回档 / 丢失资产；medium = 默认可达的丢进度，或需特定故障才可达的资产问题，或门控功能里的严重问题；low = 其余。运维工具需要人为误用才触发的，最高 low，除非缺省参数就会出错。',
  '',
  '【方法】每条先自己尝试推翻（读调用方、配置缺省值、上游守卫、Lua 原子性）再报；推翻成功的不进 findings，只在 excluded 里各写一行。locations 写真实的 文件:行（当前工作区）。trigger 一步一步写谁先谁后。reachability 写默认配置能否触发、受哪个开关门控（配置键、缺省值与出处 文件:行）。registered 写设计文档里是否已登记为已知残余（文档:行 或"未登记"）。refute_attempt 写你尝试推翻它为什么没成功。只写结论和证据，不贴大段代码。unread 列出没读完的部分。',
].join('\n')

const FINDING = {
  type: 'object',
  properties: {
    title: { type: 'string' },
    severity: { type: 'string', enum: ['high', 'medium', 'low'] },
    kind: { type: 'string', enum: ['修复缺陷', '新问题', '已登记变种'] },
    related: { type: 'string' },
    locations: { type: 'array', items: { type: 'string' } },
    trigger: { type: 'string' },
    reachability: { type: 'string' },
    registered: { type: 'string' },
    fix: { type: 'string' },
    confidence: { type: 'string', enum: ['确定', '很可能', '存疑'] },
    refute_attempt: { type: 'string' },
  },
  required: ['title', 'severity', 'kind', 'locations', 'trigger', 'reachability', 'registered', 'fix', 'confidence', 'refute_attempt'],
}
const FIND_SCHEMA = {
  type: 'object',
  properties: {
    findings: { type: 'array', items: FINDING },
    excluded: { type: 'array', items: { type: 'string' } },
    unread: { type: 'array', items: { type: 'string' } },
    coverage: { type: 'string' },
  },
  required: ['findings', 'excluded', 'unread'],
}
const REVIEW_SCHEMA = {
  type: 'object',
  properties: {
    items: {
      type: 'array',
      items: {
        type: 'object',
        properties: {
          no: { type: 'integer' },
          title: { type: 'string' },
          verdict: { type: 'string', enum: ['成立', '部分成立', '推翻', '已修复'] },
          severity_after: { type: 'string', enum: ['high', 'medium', 'low', 'none'] },
          locations: { type: 'array', items: { type: 'string' } },
          reason: { type: 'string' },
          fix: { type: 'string' },
        },
        required: ['no', 'title', 'verdict', 'severity_after', 'locations', 'reason'],
      },
    },
    new_findings: { type: 'array', items: FINDING },
  },
  required: ['items'],
}
const VERDICT_SCHEMA = {
  type: 'object',
  properties: {
    verdict: { type: 'string', enum: ['成立', '部分成立', '推翻'] },
    severity_after: { type: 'string', enum: ['high', 'medium', 'low', 'none'] },
    reason: { type: 'string' },
    evidence: { type: 'array', items: { type: 'string' } },
  },
  required: ['verdict', 'severity_after', 'reason', 'evidence'],
}

const WAVE3 = [
  { key: 'assets-misc', scope: [
    '【本次范围：资产相关的其余增量（收件箱、号段引导、交易、好友、邮件），增量对抗评审】',
    '第一轮已登记 go-asset-services#0/#1（交易）、收件箱 Ack 以 Redis 落盘为依据（low），不要重报。',
    '代码（git diff bf3c60632 HEAD -- <路径> 加 git diff HEAD -- <路径>，再按需读全文）：',
    '- cpp/libs/services/scene/player/system/player_inbox_system.cpp/.h；cpp/nodes/scene/rpc_replies/player_inbox_response_handler.cpp/.h',
    '- cpp/nodes/scene/id_segment_bootstrap.cpp/.h；cpp/nodes/scene/main.cpp 中的接线；cpp/libs/modules/id_segment/',
    '- cpp/libs/services/scene/player/system/asset_op_system.cpp',
    '- go/trade/internal/data/asset_op_repo.go、go/trade/internal/commerce/engine.go、go/trade/internal/svc/assetchannel.go',
    '- go/friend/internal/data/sweep_repo.go、friend_repo.go、block_repo.go',
    '- go/mail/：internal/data/sweep.go、policy.go、repo.go、campaign.go，internal/maintenance/sweep.go，internal/server/server.go，cmd/mailadmin/main.go，etc/mail.yaml；设计 docs/design/mail-system.md',
    '重点问题：',
    '1. 收件箱：应答乱序、重复、超时后实际成功时，资产会不会发两次或丢一次？与玩家退出、交接并发时怎样？',
    '2. 号段引导：节点重启、号段 Redis / 库回退、多个 scene 并发引导时，item / mail 等 guid 会不会重复发号？引导失败时是 fail-closed 吗？',
    '3. 邮件清扫：会不会删掉带未领附件的邮件（资产丢失）？清扫与领取并发时会不会附件已发、邮件未删（可重复领取），或邮件已删、附件未发？mailadmin 的批量操作有无 dry-run 与鉴权？',
    '4. 交易 asset_op_repo 改动：恢复路径会不会重复执行已完成的资产腿，或把已终结的合同重新打开？',
    '5. 好友清扫：会不会误删在用关系；双向关系一边删了另一边没删（跨存储不一致）。',
  ].join('\n') },
  { key: 'lifecycle-unread', scope: [
    '【本次范围：第一轮未读完的 player_lifecycle 段落与存盘链底层】',
    'cpp/libs/services/scene/player/system/player_lifecycle.cpp 很大，只读以下段落及其直接调用链（用 grep 定位函数）：',
    '- 交接取证（handoff 证据的写入与读取）；',
    '- 改派待确认表（relocate pending confirm，含 relocate_confirm.h、travel_freeze_cap.h）；',
    '- EnterScene 应答分派；',
    '- HandleSessionEndedRelease、DecideSessionEndedBinding、WriteExitReleaseMarkIfEligible、HandlePlayerAsyncSaved（第一波只读了注释）。',
    '另读：cpp/libs/engine/thread_context/redis_manager.cpp/.h（2026-10-19 改动约 160 行）、cpp/libs/engine/infra/storage/redis_client/redis_client.h（存盘 Lua）、cpp/libs/services/scene/player/system/player_scene.cpp 改动、cpp/libs/services/scene/player/comp/player_ownership_comp.h。',
    '相关设计：docs/design/location-release-binding.md、docs/design/player-async-save-loss-windows.md、docs/design/scene-owner-reentry-barrier.md（按需读）。',
    '重点问题：',
    '1. 交接取证：证据写入失败或丢失时，新节点会不会在旧节点数据未落地时加载旧快照？证据与存盘的先后是否保证？',
    '2. 改派待确认表：确认丢失、超时、重复时，玩家会不会被两个节点同时持有（双写者），或被放弃导致未落盘数据丢失？',
    '3. EnterScene 应答分派：应答到达时实体已被销毁重建、或属于旧会话时，会不会对新实体执行旧操作？',
    '4. HandlePlayerAsyncSaved：存盘回执乱序（旧 save_seq 的回执晚到）会不会把"已落地"标记推到错误版本，导致后续以为已落地而删除实体或写 A1′？',
    '5. redis_manager 改动：连接切换、重连、主从切换期间排队的存盘命令会不会丢失、重发（旧载荷重放）或跨连接乱序？',
  ].join('\n') },
  { key: 'write-entries-gates', scope: [
    '【本次范围：补覆盖——各业务系统修改持久组件的写入口，是否绕过冻结闸或退出闸】',
    '背景：玩家跨场景交接时有"冻结"（旧节点停止修改、等新节点接手），退出流程有"退出闸"（开始退出后不再接受修改，先存盘后销毁）。若某个写入口在冻结或退出中仍修改持久组件，修改会落在已经快照或已经交出的实体上：要么丢失，要么与新节点双写。',
    '步骤：',
    '1. 先找出冻结闸与退出闸的实现与判定函数（grep travel_freeze、freeze、Exiting、exit_state、IsPlayerExiting、kExiting、handoff、relocate_confirm、IsCrossZoneFrozen 等；player_lifecycle.cpp、travel_freeze_cap.h、player_gm_guard.h、客户端消息分发处的统一守卫、scene_handler.cpp）。确认统一守卫覆盖了哪些入口（客户端消息？RPC？Kafka 事件？定时器？）。',
    '2. 列出修改持久组件的全部写入口：客户端请求 handler（cpp/nodes/scene/handler/rpc/player/*）、节点间 RPC、gRPC（scene_node_service.cpp）、Kafka 消费（收件箱、资产通道、GM、帮会、邮件通知）、定时器 / tick 系统（任务、buff、体力恢复、限时商人刷新）、战斗结算回调。持久组件以 proto 里 player_database / player_database_1 的字段和 cpp/libs/services/scene 下 *Comp 为准，参考 docs/design/ecs-component-access-rules.md。',
    '3. 对每个统一守卫没覆盖的入口，检查它自己有没有判冻结与退出；没有就报。',
    '重点：资产类（货币、背包、装备、宠物、邮件附件领取、神秘商人购买、任务奖励、战斗结算、GM 发放）优先；非资产的进度类其次。在 coverage 字段里附一张"入口 → 守卫"的覆盖表（每行一句：入口名、是否被守卫、靠哪个守卫）。',
  ].join('\n') },
  { key: 'persist-schema-c3c4', scope: [
    '【本次范围：混版本持久字段保护第 3b 批（C++ 严格屏障 Lua 的 C3、写方令牌）与第 4 批（login C4），增量对抗评审】',
    '第二波只审了 go/db 侧与第 1、2 批；3b、4 是 2026-10-19～20 落码的（未编译、未测试，是否已有 Codex 验证以 PROGRESS 为准）。C++ 携带（3a / 3a′）另有一路在审，不重点看。',
    '先读：docs/design/mixed-version-persist-field-protection.md 的 §5.2（v0.6 写方令牌）、§9 各批状态、§15 的 v0.6 / v0.7；docs/PROGRESS.md「混版本持久字段保护 §5.2 改为写方令牌（设计 v0.6），第 3b 批（C++ C3）与第 4 批（login C4）落码」条目（grep 标题定位）。',
    '代码（git log --oneline -5 -- <路径> 找落码提交，git show <提交> -- <路径> 或 git diff <落码前一提交> HEAD -- <路径> 看改动，再按需读全文）：',
    '- cpp/libs/engine/infra/storage/redis_client/redis_client.h（严格屏障 Lua：参数个数、schema_gen、schema_writer、返回码 2 / 3 两种新结局、OnSaved 返回值）与它的调用方（player_lifecycle.cpp 的 SavePlayerToRedisImpl、HandlePlayerSaveRejected 等，grep schema_writer / kSchema）',
    '- 写方令牌在实体上的生成、保存与交接：grep schema_writer、SchemaWriter、writer_token',
    '- go/login/internal/logic/pkg/dataloader/parent_commit_barrier.go、ensure_player_all_data_async.go；go/login 的 config 与 persist_schema 相关文件（C4）',
    '- go/db/internal/barrierrestore/scripts.go、restore.go（字段白名单加 schema_writer 后恢复是否仍正确）；tools/merge_zone 中处理 save_barrier 的代码（player_rows.go、save_barrier_verify.go 若存在）',
    '- deploy/k8s/persist-schema-alerts.yaml 与 C3 / C4 相关的告警',
    '重点问题：',
    '1. C3：新的屏障 Lua 会不会把合法存盘永久拒绝或无限延后（丢进度）？返回码 2 / 3 的调用方处理是否正确（重试、延后、销毁实体），会不会把"版本错位"误当"已废黜"销毁实体丢进度，或反过来放行旧版本？',
    '2. 写方令牌：同节点重载、跨节点交接、断线重连复用实体、首存在途时排队的后继存盘、首存被顶替、回包丢失后重发时，令牌比较是否都正确？会不会两个写者持有同一令牌（双写者）？',
    '3. C4：login 父提交屏障比较世代时，会不会把合法玩家永久拒在登录外？与 r2 文档 §3.6 的 r2-login#1（新号铸 epoch 后首存前故障永久拒登）是否叠加或缓解？',
    '4. save_barrier 新增两个字段后，屏障恢复工具、合服 copy（r2-mz#1）、debug_import、data_consistency_check 等读写屏障的工具是否都认得这两个字段，会不会误拒或把字段丢掉？',
    '5. 混版窗口：旧 scene（不写 schema_gen / schema_writer）与新 login / go/db 并存时的行为（按规则降一级）。',
  ].join('\n') },
  { key: 'persist-schema-cpp', scope: [
    '【本次范围：混版本持久字段保护第 3a / 3a′ 批（C++ 携带、统一存盘入口、盖戳），增量对抗评审】',
    '第二波审字段保护时第 3a 批还是未跟踪半成品，只审了 go/db 侧与已提交部分。本路补审 C++ 携带侧。3a′ 已修了交接时登记的三处缺陷（① 收件箱执行器把 AttachDebt 版本错位记成终态 = r2-ps#2；② Bag::AddItems 规划只按 has_equip 分流 = r2-ps#5；③ 宝石拆卸预检判据）；3b 的 C3 / 写方令牌另有一路在审，不重点看。',
    '先读：docs/design/mixed-version-persist-field-protection.md 的 §3.3（盖戳）、§4（机制一）、§9 各批状态、§15；docs/handoff/mixed-version-persist-handoff-20261019.md；docs/PROGRESS.md「混版本持久字段保护第 3a 批落码」及之后 3a′ 的条目（grep 标题定位）。',
    '代码（git log --oneline -5 -- <路径> 找落码提交，git diff HEAD -- <路径> 看工作区改动，再按需读全文）：',
    '- cpp/libs/engine/core/utils/proto/unknown_field_carry.h；cpp/libs/services/scene/player/comp/player_persist_carry_comp.h；cpp/libs/services/scene/player/system/player_persist_marshal.h',
    '- cpp/libs/services/scene/player/system/player_database_loader.cpp、player_database_1_loader.cpp 的手写 Unmarshal / Marshal；player_lifecycle.cpp 的 MarshalPlayerForSave；cpp/libs/modules/snapshot/snapshot_system.cpp 的 CaptureAndSend',
    '- 对象内携带：cpp/libs/modules/currency/comp/player_currency_comp.h、cpp/libs/modules/currency/system/currency_system.cpp（AttachDebt 守卫）、mission_marshal.cpp、player_skill.cpp（SanitizeSkillList）、asset_op_ledger.cpp（ResetAssetOpStreamForEpoch）、cpp/libs/modules/bag/item_store.{h,cpp}（IsIndependentInstance）、bag_marshal.cpp/.h',
    '- 运行期整体替换持久组件的路径：grep 对持久组件（两张玩家表字段对应的 *Comp 或 proto 消息）做 emplace_or_replace、整份赋值、Clear()、CopyFrom、重建的地方，例如 player_mission.cpp 日常 / 周常重置、player_attribute.cpp 属性重建、宠物重建、player_feature_snapshot.cpp、player_battle.cpp 结算回写、GM 重置；逐个判断是否丢掉加载时携带的未知字段',
    '- 测试：cpp/tests/bag_test/persist_round_trip_test.cpp（覆盖率断言到底覆盖了哪些节点、漏了哪些）',
    '- 生成器：tools/proto_generator/protogen/internal/persistschema/generate.go、render.go、ledger.go、apply.go、schema.go；产物 cpp/generated/persist_schema/、go/shared/generated/persistschema/',
    '重点问题：',
    '1. 每个持久节点（两张玩家表的顶层与嵌套消息、repeated 元素、map 值）加载→内存→存盘是否完整保留未知字段？哪些节点没有携带、又不在拒存或延后路径上（静默丢字段）？往返测试的覆盖率断言能否发现这些漏网节点？',
    '2. 运行期整体替换或 Clear 持久组件的路径会不会丢掉携带的未知字段（混版窗口里新版字段被旧二进制抹掉）？',
    '3. 盖戳 max(本二进制世代, 两表加载世代最小值)：新建角色（无加载世代）、只加载了一张表、跨场景交接（快照来自另一节点）、离线快照、回档快照时戳是否正确？会不会让新版本数据盖上旧戳而被旧 go/db 接受，或旧数据盖上新戳？',
    '4. "缺载体整份拒存"：哪些正常路径会缺载体（新建角色、镜像实体、断线重连复用实体、跨场景交接后建出的实体），拒存后是否丢进度（存盘永久失败 → 回档到上次成功存盘）？拒存有无告警与重试？',
    '5. 核对 3a′ 对 r2-ps#2、r2-ps#5 与 ③ 的修复是否到位（只在 coverage 里写结论；修复本身若有新缺陷，照常报，related 写编号）。另：第一轮 low「SanitizeSkillList 按当前技能表永久删除玩家技能」在四职业技能合入后（含"保留未知技能的存盘保护"，提交 94dd15382）是否仍成立，写进 coverage。',
    '6. 生成器：bootstrap / active 切换、世代号分配、字段增删时台账的一致性。r2-ps#1 已登记不重报，但若发现其他发号缺陷要报。',
  ].join('\n') },
  { key: 'login-rest', scope: [
    '【本次范围：go/login 第二波未读完的部分，增量对抗评审】',
    '第二波 login 路已审 loginsession、sessionfence、账号目录四个写者（结论：四个写者同锁加令牌 CAS，第一轮 §4「账号 blob 有两个写者不走建角锁」不成立，见 r2 文档 §4）、kill switch、deprecation。本路补读其余部分。',
    '代码（git diff bf3c60632 HEAD -- <路径> 加 git diff HEAD -- <路径> 看改动，再按需读全文）：',
    '- go/login/internal/logic/pkg/loginqueue/：queue.go、gatetoken.go、dispatcher.go、capacity.go、gatedrain.go、gatedrain_monitor.go',
    '- go/login/internal/logic/pkg/dataloader/：ensure_player_all_data_async.go、parent_commit_barrier.go、data_loader.go、sync_loader.go、new_player_seed.go',
    '- go/login/internal/logic/pkg/node/：node_cache.go、watcher.go、node_allocator.go',
    '- go/login/internal/logic/clientplayerlogin/entergamelogic.go 第 420 行之后（第二波只读到这里）；createplayerlogic.go 的 writeAccountBlob（约 :698-745）与 writeAccountBlobScript（约 :61-90）',
    '- 疑点：go/data_service/cmd/debugutil/bundle.go 约 :397 的 AccountRedisKeySpec 把 account:{account} 列为 bundle 条目（注释称不可导回），核对 go/data_service/cmd/debug_import 导入时是否会不拿 account_lock:create:{account} 就整块写回账号目录、盖掉并发建出的角色（bundle_import.go 的身份闸与重铸 epoch 两处有其他会话在改，那两处不审）',
    '重点问题：',
    '1. loginqueue：同一账号或同一角色能否被并发放行两次（两条登录链同时进场、出现两个持有者）？gate token 重复、过期、被重放时怎样？排队中断线、换 gate、gate 排空（gatedrain）时的状态清理会不会误删另一个连接的票？',
    '2. dataloader：异步加载与在途存盘并发时，会不会把旧快照装进缓存，随后被当作最新数据进场（加载旧快照后再存盘）？父提交屏障判定（pending / ACK / owner_epoch）有无 TOCTOU：检查通过后、读快照前屏障被推进？新玩家播种（new_player_seed）会不会覆盖已有数据？',
    '3. 父提交屏障与字段保护的世代口径是否一致（第二波结论是 login 没有 schema_gen 的读者，确认这是否会导致问题）。',
    '4. node_cache / watcher：etcd 事件乱序、租约丢失恢复、watch 重建时，缓存会不会给出已下线或已换实例的节点，把玩家派到旧节点（双持有者）？',
    '5. entergamelogic 后半段：Bind 下发、location 写入、会话写入的先后与失败回滚；中途失败会不会留下指向旧节点或旧代次的残留状态。',
  ].join('\n') },
  { key: 'guild-rollback', scope: [
    '【本次范围：补覆盖——帮会逻辑层正文与区服回档脚本（第一轮未读）】',
    '代码：',
    '- go/guild/internal/logic/ 全部业务逻辑（资金、贡献、成员进出、职位、仓库 / 捐献、帮会战或历练结算入账、解散），go/guild/internal/data/（存储与事务），go/guild 的 Kafka 或事件消费（若有）。合服围栏 merge_fence.go 另有人审，不重点看。',
    '- tools/scripts/k8s_zone_rollback.ps1 正文（第一轮未读），以及 docs/design/zone_data_rollback.md、docs/design/zone_data_rollback_discard_tool.md。第一轮已登记 ops-tools-java#1（PITR 只回退 zone 库），不重报。',
    '重点问题：',
    '1. 帮会资金与贡献：入账与扣款是否幂等（Kafka 重投、RPC 重试会不会重复入账或重复扣款）？玩家侧与帮会侧双边记账（玩家捐献扣钱、帮会加资金）是否原子，失败时有无补偿，会不会钱扣了帮会没加或反之？',
    '2. 成员进出与解散：玩家退帮 / 被踢 / 解散时，帮会侧与玩家侧（玩家持久组件里的帮会 id）会不会不一致？',
    '3. 帮会数据存储的并发写：多个 guild 实例或多协程写同一帮会时有无锁或版本，会不会旧盖新？',
    '4. 回档脚本：缺省参数是否 dry-run？会不会只回退部分存储（zone 库回退而 Redis 缓存、Kafka 位点、全局库、帮会库不回退），造成复制或白亏？执行前是否强制停服、排空？',
  ].join('\n') },
  { key: 'match-rank-db', scope: [
    '【本次范围：组队匹配、评分排行与 go/db 剩余未读部分】',
    '代码：',
    '- go/match/internal/logic/：party.go、party_match.go、queue.go、rating.go、matcher.go、gather.go、cancelqueuelogic.go、keys.go；go/match/internal/kafka/rating_snapshot.go；go/match/internal/team/；go/match/etc/match_service.yaml、internal/config/config.go',
    '- go/rank/（读写与消费评分快照的部分）',
    '- go/db/internal/kafka/retry_stranded.go、durable_enqueue.go 主体（第一轮和本轮第一波都只读了文件头与 flush 主路径）',
    '第一轮已登记：评级来源停写后结果消费卡死（rating.go，low）、全球匹配票据卡死（§2.5，GlobalMatch.Enabled 门控）、已打完的战斗被恢复误判（low）。不重报。',
    '重点问题：',
    '1. 评分：同一场战斗结果重复消费会不会重复加减分？乱序消费会不会旧分数盖新分数？评分快照出口给 rank 的数据与 match 内部是否会不一致？',
    '2. 组队：队伍状态在 Redis 的并发修改（入队、退队、解散、队长变更）有无 CAS？会不会留下幽灵队员，使玩家永远进不了匹配（只记 low，除非涉及资产）？',
    '3. durable_enqueue / retry_stranded：持久入队与 Kafka ack 的先后，崩溃时会不会既没入队又已 ack（丢存盘）？搬移搁浅任务时会不会把旧任务搬到新任务之后执行（旧盖新，dual 下行内围栏能否兜住）？',
  ].join('\n') },
  { key: 'merge-zone-rest', scope: [
    '【本次范围：合服工具第二波未读完的部分，增量对抗评审】',
    '第二波 merge-zone 路只读完主流程（merge_run、preflight、player_rows、unmerge 调用点），已登记 r2-mz#1、r2-mz#2；第一轮已登记 ops-tools-java#0 与 §4 的 4 条 merge_zone low。均不重报。',
    'r2-mz#1 截至 2026-10-22 仍未修。D1 落点第 2 批写入点等改动已随主干提交，未必已验证。设计与手册：docs/ops/merge-zone-runbook.md、docs/design/player-storage-placement.md（按需读）。',
    '代码（git diff bf3c60632 HEAD -- <路径> 加 git diff HEAD -- <路径>，再按需读全文）：',
    '- tools/merge_zone/route_store.go、route_publish.go、fence.go、relocate.go、relocate_run.go、relocate_manifest.go、unmerge.go 正文、merged_into.go、post_merge_stamp.go、trade_step.go、guild_step.go、placement_ops.go、pin_placement.go',
    '- go/guild/internal/logic/merge_fence.go 及调用它的帮会写路径（economy_logic.go、guild_manage_logic.go、guild_logic.go 里资金 / 成员 / 贡献写入）',
    '- 合服后 ID 撞号与号段续接：go/data_service/internal/store/id_segment_store.go；player / item / mail / guild / trade id 的号源（grep AllocateIdSegment、SnowFlake、guild id mint）；tools/merge_zone 是否检查或续接号段',
    '- tools/data_consistency_check/（main.go、route_source.go）',
    '重点问题：',
    '1. relocate_run（单玩家迁库）：拷行、发布新落点、删旧行之间，玩家登录或 go/db 迟到写会不会落到旧库（双写者），或拷到旧快照？中断后重跑是否幂等？',
    '2. route_store / route_publish / fence：路由发布与数据拷贝的先后、围栏的加解时序；发布失败或部分成功（多条路由）时的状态；重跑会不会把路由指回旧库？',
    '3. unmerge 正文：撤销时会不会删掉合服后新产生的数据（玩家在合服后登录、交易、入帮）？',
    '4. 帮会合服围栏：围栏期间资金、成员、贡献写入是被拒绝（客户端可重试）还是被静默丢弃？围栏解除与路由切换不同步时，写入会不会落到旧区帮会库？',
    '5. ID 撞号：两区 player_id、item guid、mail id、帮会 id、交易单号合到一起会不会撞号？合服后号段从哪里续，会不会重发已用号？',
    '6. data_consistency_check：能否发现上面这些不一致；它自己的读是否会误报而诱导运维误操作。',
  ].join('\n') },
  { key: 'route-reconcile-rest', scope: [
    '【本次范围：data_service 路由读写其余部分（第二波路由对账路提前收尾未读），增量对抗评审】',
    '第二波已登记 r2-rr#1~#4 与附带观察「catch_up 收尾不查未决就置 VI」，第一轮已登记 data-service#3。均不重报，但本路要回答 r2-rr#1 的受害面（见问题 1）。',
    '设计：docs/design/player-route-mapping-durability-proposal.md、docs/design/player-storage-placement.md、docs/design/data_service_role_and_scope.md（按需读与读写路由相关的章节）。',
    '代码（git diff 8767c5027 HEAD -- <路径> 加 git diff HEAD -- <路径>，再按需读全文）：',
    '- go/data_service/internal/routing/route_publish.go（W1 发布）、route_read.go、router.go（ClientForPlayer 等落点解析）、player_name_cache.go',
    '- go/data_service/internal/store/route_reads.go、route_store.go（RegisterHome / ScanRoutes / GetRoutes 正文）、route_tidb.go、route_reconcile.go 中被读写路径调用的部分',
    '- go/shared/placement/route.go、route_ready.go（读脚本与 VerifyRouteReady）、placement.go',
    '- 读方：go/db/internal/kafka/placement_route.go、route_ready_gate.go；login / scene_manager 里解析落点的调用点（grep placement.、RouteReady、ResolvePlacement）',
    '重点问题：',
    '1. W1 发布失败语义：库写成功、Redis 发布失败时，读方读到什么？之后谁来补发布？据此给出结论：r2-rr#1 里"缓存缺键或错值"时读方是 fail-closed 还是写错库。',
    '2. 读路径：缓存缺键、值陈旧、rev 回退时，读方（go/db 写 SQL、data_service 读写、login 加载）会不会落到旧库或默认 home 库，读到空数据后当成新玩家或覆盖？',
    '3. RegisterHome / ScanRoutes / GetRoutes：并发注册、重复注册、扫描分页时的漏读或重复；注册与合服 / 迁移并发时会不会把玩家钉回旧区。',
    '4. VerifyRouteReady 与 route_ready 读脚本：就绪标记的判定是否原子，标记陈旧时会不会放行。',
    '5. 缺省模式（RouteStore.Mode 缺省 redis）与 db_first 下各自的行为差异；缺省配置下哪些路径走不到。',
  ].join('\n') },
]

const LOW_REVIEW = [
  { key: 'low-1-12', range: '第 1–12 条（C++ 存盘与加载 4 条、C++ 资产 3 条、login 2 条、资产服务 1 条、匹配 2 条）' },
  { key: 'low-13-22', range: '第 13–22 条（运维工具与 Java 7 条、身份原语 3 条）' },
]

const D_ROUNDS = [
  [
    { key: 'asset-currency', lens: '按资产追踪：货币的完整生命周期。产出（战斗结算、任务、GM、邮件附件、收件箱、交易、神秘商人退款）、消耗（商店、神秘商人、强化、交易）、存储（CurrencyComp、欠款 owed / paid）、跨服与合服。逐个产出点和消耗点追幂等、原子性、存盘归属与回档后的状态。' },
    { key: 'fault-redis', lens: '按故障：Redis 丢尾或主从切换。逐个列出 zone Redis、核心 Redis、mapping Redis、共享 Redis 各自持有哪些不可再生状态（屏障、owner_epoch、location、会话、号段、意图桶、标记），丢一段尾巴或切到落后的从库后，各条链路（存盘、登录、交接、补投、资产通道、号段）的行为。' },
    { key: 'entry-scene-client', lens: '按入口：C++ scene 的客户端请求 handler 全量走一遍（cpp/nodes/scene/handler/rpc/player/ 下全部），每个改动玩家数据的 handler 是否校验请求者身份与会话代次、冻结与退出状态、幂等（重发会不会做两次）。' },
  ],
  [
    { key: 'asset-item', lens: '按资产追踪：物品、装备、宠物的完整生命周期。铸号、入包、堆叠拆分、临时格、交易托管、邮件附件、销毁与分解、合服搬迁。追 guid 唯一性、数量守恒、跨存储一致。' },
    { key: 'fault-kafka-crash', lens: '按故障：Kafka 重投、乱序、消费组重平衡、位点回退，以及节点（scene / gate / battle / 各 Go 服务）在关键两步之间崩溃。逐个 topic 列消费者，判断是否幂等、key 是否为业务实体 id、重投旧载荷能否被挡住。' },
    { key: 'entry-go-admin', lens: '按入口：Go 服务的 gRPC / HTTP 入口与 admin / GM 接口（login、data_service、scene_manager、player_locator、guild、mail、trade、match、rank，以及 java/gateway_node 的 admin API），关注鉴权、幂等、对玩家数据或资产的直接写。' },
  ],
  [
    { key: 'asset-guild-mail-reward', lens: '按资产追踪：帮会资金与玩家帮会贡献、邮件附件、任务奖励、成就与活动奖励的完整生命周期。' },
    { key: 'fault-clock-upgrade', lens: '按故障：时钟回拨与跳变（所有用墙钟做比较、过期、排序、去重、限购刷新的地方），以及滚动升级混版（新旧 scene / go/db / login / gate / scene_manager 并存时各协议字段的语义）。混版问题按规则降一级。' },
    { key: 'entry-consumers-timers', lens: '按入口：Kafka 消费者与周期任务入口（每个 consumer、每个 sweeper / reconciler / leader 任务 / 定时器），关注重复执行、并发执行（多副本）、与玩家在线写的竞态。' },
  ],
  [
    { key: 'asset-crosszone-idseg', lens: '按资产追踪：跨区与合服、号段（item / mail / player / snapshot / txlog）的全链路，关注撞号、重发号、跨区资产复制。' },
    { key: 'fault-partition-lease', lens: '按故障：网络分区、etcd 租约丢失与恢复、gRPC 超时后实际成功、半开连接。关注双持有者、超时重试导致的重复执行。' },
    { key: 'entry-ops-cli', lens: '按入口：运维与调试 CLI（tools/ 下全部 Go 工具与 tools/scripts 下改数据的 ps1），关注缺省参数是否安全、是否校验目标环境、是否会在在线时写入。' },
  ],
  [
    { key: 'fresh-eyes-save', lens: '自由视角：从 C++ 存盘快照组装到 go/db 写 SQL 的整条链，挑你认为最可能出错的三个接缝深挖。' },
    { key: 'fresh-eyes-load', lens: '自由视角：从 login 加载、scene_manager 落点到 scene 建实体的整条链，挑你认为最可能出错的三个接缝深挖。' },
    { key: 'fresh-eyes-asset', lens: '自由视角：所有跨服务资产流转（交易、邮件、收件箱、帮会、商城），挑你认为最可能出错的三个接缝深挖。' },
  ],
]

// 本次运行要跑的代理，全部收进一张表后按 ≤6 一批执行
const jobs = []

for (const f of WAVE3.filter(w => WAVE3_KEYS.includes(w.key))) {
  jobs.push({ kind: 'find', key: f.key, run: () => agent(PRE + '\n\n' + f.scope, {
    label: 'find:' + f.key, phase: 'Wave3', schema: FIND_SCHEMA,
  }) })
}

if (LOW_REVIEW_ON) {
  for (const r of LOW_REVIEW) {
    jobs.push({ kind: 'review', key: r.key, run: () => agent(PRE + '\n\n' + [
      '【本次任务：复核第一轮审计 §4 的 low 条目，逐条尝试推翻】',
      '第一轮审计 docs/design/data-overwrite-rollback-audit-20261018.md §4 在"下面 23 条没有复核"之后列了一组 low（SanitizeSkillList 两个区域重复报告，算 1 条，共 22 条），按出现顺序编号 1–22。你负责' + r.range + '。',
      '对每条：按当前工作区代码核对（行号可能已漂移，用 grep 重新定位）；判断它现在是否仍成立（可能已被后续改动修掉）；尝试推翻；给出复核后的严重度与修复方向。',
      '口径补充：第 4 条「存盘不保留未知字段」——混版本字段保护第 3a 批（unknown_field_carry.h 等三个头文件）与 3a′、3b、第 4 批现已随主干提交（c35e1133d 起），是否已编译测试以 docs/PROGRESS.md 的 Codex 条目为准；按当前 HEAD 判，并写明它依赖的字段保护批次尚未运行验证（若 PROGRESS 无 Codex 通过记录）。第 8 条「账号 blob 两个写者不走建角锁」已由第二波证实不成立（r2 文档 §4），核对后判推翻即可。第 1 条 SanitizeSkillList 在 3a 里改为替换前并回未知字段，注意那只解决未知字段，不解决"按当前技能表永久删除"本身。',
      'items 里每条一项，no 用 1–22 的编号。复核过程中若发现与该条相关、但不同的新问题，放进 new_findings（格式同发现）。',
    ].join('\n'), { label: 'review:' + r.key, phase: 'Wave4', schema: REVIEW_SCHEMA, model: 'sonnet' }) })
  }
}

if (D_ROUND !== null && D_ROUND < D_ROUNDS.length) {
  const seenText = SEEN_TITLES.map((t, i) => (i + 1) + '. ' + t).join('\n')
  for (const p of D_ROUNDS[D_ROUND]) {
    jobs.push({ kind: 'sweep', key: p.key, run: () => agent(PRE + '\n\n' + [
      '【本次任务：换视角查找（第 ' + (D_ROUND + 1) + ' 轮）】',
      '前面各波已经按子系统和按改动查过。这一轮换一个视角查全仓，找前面漏掉的问题。',
      '你的视角：' + p.lens,
      '',
      '【本轮之前已发现的问题（不要重报，只报这个列表和两份审计文档里都没有的新问题）】',
      seenText,
      '',
      '如果按你的视角查完确实没有新问题，findings 返回空数组，并在 excluded 里写你查过并排除了什么。',
    ].join('\n'), { label: 'sweep:' + p.key, phase: 'Dsweep', schema: FIND_SCHEMA }) })
  }
}

VERIFY.forEach((f, idx) => {
  const lenses = (f.severity === 'high' || MEDIUM_LENSES >= 2)
    ? ['代码路径与时序：逐步核对每个 文件:行 与触发时序的每一步是否真的成立', '可达性与上游守卫：缺省配置、开关、上游校验、Lua 原子性、已登记的设计残余是否已挡住或已接受；并据此裁定严重度']
    : ['代码路径、时序与可达性：逐步核对每个 文件:行 与触发时序，并检查缺省配置、上游守卫和已登记残余']
  lenses.forEach((lens, li) => {
    jobs.push({ kind: 'verify', key: (f.id || f.source || 'v') + '.' + li, idx: idx, run: () => agent([
      '你是对抗复核员，任务是尽力推翻下面这条审计发现。用中文作答。',
      '只读：不创建、修改、删除任何文件（包括 %TEMP%、/tmp）；git show 只直接输出不重定向；不跑任何构建、测试、静态检查；不给其他会话发消息。行号以当前工作区（D:\\luyuan\\xuanming-server-mmo-cpp）为准。',
      '项目尚未上线；只在混版窗口出现的问题严重度降一级。已在设计文档登记为接受的残余，按"已登记"处理，不算推翻但要写明。',
      '严重度口径：high = 默认配置下可达的资产复制，或回档 / 丢失资产；medium = 默认可达的丢进度，或需特定故障才可达的资产问题，或门控功能里的严重问题；low = 其余。',
      VERIFY_NOTE,
      '',
      '【待复核发现】',
      JSON.stringify(f, null, 2),
      '',
      '【你的视角】' + lens,
      '读真实代码核对；拿不准时倾向判"推翻"或降级，但必须给出 文件:行 证据。输出 verdict（成立 / 部分成立 / 推翻）、复核后严重度（high / medium / low / none）、理由与证据。',
    ].join('\n'), { label: 'verify:' + (f.id || 'v' + idx) + '.' + li, phase: 'Verify', schema: VERDICT_SCHEMA }) })
  })
})

log('本次运行代理数：' + jobs.length + '（' + jobs.map(j => j.kind + ':' + j.key).join('，') + '）')
const res = await batched(jobs.map(j => j.run), 6)

const out = { finders: [], lowReview: [], sweep: [], verified: [] }
jobs.forEach((j, i) => {
  const r = res[i]
  if (!r) { log('代理失败或被跳过：' + j.kind + ':' + j.key); return }
  if (j.kind === 'find') out.finders.push({ key: j.key, result: r })
  else if (j.kind === 'review') out.lowReview.push({ key: j.key, result: r })
  else if (j.kind === 'sweep') out.sweep.push({ key: j.key, result: r })
  else out.verified.push({ key: j.key, idx: j.idx, finding: VERIFY[j.idx].id || '', verdict: r })
})
const nFind = out.finders.concat(out.sweep).reduce((s, x) => s + ((x.result.findings || []).length), 0)
log('完成：新发现 ' + nFind + ' 条；复核 ' + out.verified.length + ' 份')
return out
