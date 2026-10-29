# 账号角色目录的全局 SQL 权威

状态：2026-10-21 P0-b 账号目录源码收口，P4 目录登记／holder／SQL 证明与原生接线实施中。尚未执行本批协议生成、构建和运行验收。SQL 确认器已有源码装配，但缺显式强读配置或原生排空证据时仍拒绝；不能据此声明角色交易可用。认证密码与钱包不在本文模块内。

## 职责与权威

data_service 的账号目录模块负责“角色属于哪个账号”和账号角色数上限。所有 zone 读取同一个逻辑 TiDB 全局库。认证继续使用原密码／第三方认证模块；玩家玩法数据继续由 scene 与现有 SQL 存盘围栏负责。`user_accounts.password` 不迁、不复制。

目录由 `account_directory` 账号 guard 与 `account_role` 角色行组成。角色行的 `player_id` 唯一，保存账号、独立 `owner_revision` 和原 `AccountSimplePlayer` protobuf 字节；未知字段原样保留。角色名和 home_zone 仍分别以名字注册表、路由表为准。Redis 的 `account:*`、`player_to_account:*` 不再授予角色所有权，也不能用于 SQL 缺失时自动恢复。

全局库使用 data_service 现有 schema 迁移入口与 proto 源；短字符串主键沿用现有 bootstrap DDL＋proto 形状守卫模式。独立目录池复用 `shared/authoritysql`，生产要求真实 TiDB、RC、pessimistic、leader 和禁陈旧读，不能混入快照批处理池。

## 接口与并发

内部 RPC 提供读／初始化账号目录、创建角色、查角色归属、补齐缺失身份、删除角色目录项。请求复用 `ServiceCallAuth`，服务端限制调用方为 login；证明覆盖完整方法与确定性请求摘要。没有配置密钥或目录存储时拒绝，不回退 Redis。

账号创建不接受密码。角色上限只由 data_service 的 AccountDirectory.MaxPlayersPerAccount 决定，缺省 5、配置范围 1..100；不接收各 zone caller 传来的上限。账号按原认证字节比较，长度 1..191 UTF-8 字节，不 trim、不 casefold。建角事务先锁账号 guard，再读角色列表、复核数量上限、插入角色；同 player_id／同账号／相同角色字节幂等，不同归属或条款拒绝。提交后的回包丢失只强读同一 player_id 判定，不新发角色、不释放仍在役的名字。

补齐与删除同样先锁账号 guard，再按 player_id 升序锁角色，提交点复核调用链捕获的 expected_owner_revision；它不等于 scene owner_epoch。删除留下空账号墓碑并递增修订，普通建角重放不能复活墓碑。补齐只填缺失的 class/gender/appearance/name；已有非空身份与请求冲突即拒绝，保留未知字段。删除按 SQL 归属校验，不能凭 Redis 反向键删角色。目录错误显式返回，日志／指标只使用低基数结局；不输出密码或签名。

SQL 事务内不访问 Redis、Kafka 或 scene。RPC 和事务共享可取消截止时间。未知提交结果按原角色强读恢复；无法确认就保留名字登记并报告错误。角色目录提交与名字、路由、玩家种子不宣称跨系统 ACID，沿用现有补偿和孤儿告警。

## 登录与所有调用者

登录列表、建角上限、EnterGame 归属、身份补齐和孤儿清理接缝均走 SQL 目录；既有无生产调用者的 LoginAdmin 删除 RPC 继续明确 Unimplemented，不借迁移扩大破坏性权限。旧账号 self-heal 不再根据 Redis 反向键制造角色。AssignGate 的“已有会话”优化也查询同一目录。P0-b 原始批次只覆盖这些目录入口；随后获授权的 P4 增加下节的交易锁与过户收据，但不改变玩家种子、父快照及 scene owner_epoch 的事实源。

调试导出显示全局 SQL 目录，旧 Redis 目录仅标为历史缓存。调试导入的 `-local-account` 必须在目标本地全局目录登记角色；该受本地目标、来源隔离与离线检查约束的 CLI 直接调用 SQL store，不走仅允许 login 的 RPC。墓碑恢复还须显式 -replace，并继续递增归属修订号；不再只写反向 Redis 键冒充归属。工具不得改变认证口令。

导入先在原 Redis 原子闸复核 location、locator 会话和 `player_locker` 均不存在，并安装 pending≠ACK。随后先以稳定维护请求提交 SQL 目录：同角色行锁下拒绝 OPEN admission，首次导入递增 owner_revision 并写永久维护收据；此后才写两张玩家 SQL，最后补齐 ACK。已登记的旧入场先使导入拒绝，尚未登记的旧路由则因修订变化拒绝。目录失败零玩家 SQL；玩家 SQL 失败保留新目录修订与未完成屏障，不能 ACK。重铸 scene owner_epoch 同时覆盖来源、本地 Redis 与实际两张 SQL 行的最高版本，不回退 SQL 围栏。

`account_role_import_receipt` 以原 bundle、所选数据源、目标 zone 和原账号选择器的稳定 SHA-256 判重，正文摘要还绑定解析后的账号与原角色 PB。空选择器仍保留为空：首次从本地 SQL Lookup 取原账号，缺目录／空账号拒绝，不能在后续过户后用新账号生成另一维护请求。显式新建账号仍须给 local-account。相同维护请求只在当前角色仍为收据原修订且原身份相同的情况下继续恢复，不反复增加修订；后来删除、过户或另一导入使旧请求拒绝。`-replace` 是首次墓碑恢复许可与失败恢复控制，收据保存首次许可；已提交请求从 false 改为 true 不产生新目录副作用，数据条款与原修订仍严格相等。工具未执行过任何实际导入。

两玩家 SQL 替换使用同事务围栏 CAS：仅删除围栏不高于本次版本且覆盖世代不高于来源的行，再以完整原件插入。已有较新行不删除，插入撞键使整个双表事务回滚；先获目录授权但延迟写入的旧工具不能把后来导入的较新 SQL 围栏删回去。较新覆盖世代同样保留，不能用旧模型吞掉新列；PB blob 中的未知字段随原件保留。legacy 显式工具维持独立旧入口。

## 上线前部署与既有 Redis 数据

项目尚未上线。先执行正式 proto 生成和各启用模块构建，再对全局库运行 data_service 迁移，部署共享同库及独立目录签名配置的 data_service/login，最后开放登录。不支持新 login 缺 SQL 时使用旧 Redis 权威。

既有开发 Redis 角色不能在普通登录时自动导入：这种恢复会把已删除／后续已转移角色重新授权。需要保留的开发角色只能在停写维护窗口由受控调试导入显式指定目标账号，并逐项核对源玩家与目标 SQL；不需要保留的数据从 SQL 空目录正常建角。不读取或迁移密码。本文不执行任何数据导入或系统配置动作。

## 验收

必须验证清空 Redis 后目录仍在、跨实例建角上限竞争、相同角色幂等／不同账号冲突、SQL 超时和未配置拒绝、未知提交恢复、身份补齐保留未知字段、删除后旧反向键不能 self-heal、调试工具 SQL 归属及鉴权负例。H2／替身不能替代真实 TiDB；缺测试配置造成的 Skip 不算通过。生成、编译、单测、真实 SQL 与实际登录证据分别登记。

现有全量本地 reset 脚本把全局 account_role/account_directory 纳入原显式清理清单；角色目录与玩家数据必须在同一个停写维护窗口重置。数据一致性工具仅在显式全局 SQL 句柄上核 player_route→account_role，未配置报告 NOT CHECKED，不再按 zone user_accounts 行数猜测玩家数。没有执行任何清理。

## 角色寄售冻结与过户接缝（P4，2026-10-21）

本节覆盖聚宝斋设计 §6.3 的旧 Redis 账号锁／反向键草案。账户字符串仍逐字节比较；账号归属只由全局 SQL 目录裁决。角色存档、场景归属 `owner_epoch` 与目录 `owner_revision` 是三个不同问题，过户只递增最后一个，不凭目录修订号重置场景围栏，也不复制玩家存档。

角色冻结分两步。`FREEZING` 表示 SQL 已持久阻止新入场，尚未证明当前在线实体与早先受理的入场链已排空；`FROZEN` 才表示原实体已冻结、严格 SQL 存盘确认并退出，且不能有更晚的旧登录链重新落点。目录锁本身不等于资产已托管。商品只在第二步完成后公示、收款或过户；依赖失败保留 `FREEZING`，通过同一 listing 重试，不自动按时间清锁。

仅在 EnterGame 入口与异步末尾强读锁字段仍存在“最后一次读后才冻结”的竞态。因此两次强读只能作为调用方闸门，不能用来伪造冻结完成证据。完整完成点必须同时接上场景端拒写／卸载与入场提交点的冻结围栏；SQL 确认者验证实际持久证据，不能接受 trade 自报 `offline=true`、任意 owner_epoch/save_seq 或客户端快照。该证据接缝在正式落完整角色链前必须明确，不以新增字段代替验收。

目录事务的最小状态为角色行上的当前 `trade_listing_id`，以及逐 listing 的冻结原请求与终局收据。收据永久区分 `FREEZING/FROZEN/CANCEL_PENDING/CANCELLED/TRANSFERRED`，保留原 `player_id/seller_account/owner_revision` 与请求摘要；已取消的旧 Lock 重放只返回原终局，不能再次冻结角色。补齐、删除与调试恢复均在提交点拒绝当前有交易锁的角色，普通建角重放不能改变锁。

内部写面按四个动作收口：发起冻结、确认实际冻结、取消冻结、原子过户。请求均使用现有 `ServiceCallAuth`，独立 trade 调用者密钥，完整方法与确定性正文绑定；login 的目录读写权限不能代替交易过户权限。确认冻结的实现必须验证上述场景／SQL 证据，未装配验证者拒绝，不能 nil 降级。失败不给部分成功应答，也不写假的过户收据。

每笔过户以唯一 `tx_id` 保存永久 SQL 收据及原请求摘要。原请求包含 listing、角色、卖家、买家与卖家原归属修订；同 tx_id 仅全部业务参数一致时返回原收据，之后再次过户也不改变旧收据。不同参数拒绝；买家满位属于可重试条件，不写成功收据。事务提交点复核卖家仍拥有原修订、角色与冻结收据都锁定同一 listing、冻结已完成、买卖账号不同、买家未超过全局角色上限，然后只改角色行归属、递增 owner_revision、清当前交易锁，并把 listing 收据标为 TRANSFERRED；这些变化与 tx_id 收据同事务提交。

所有账号 guard 按 UTF-8 原始字节升序锁定，去重后最多两条；然后锁 listing 收据、tx_id 收据、角色行（角色有多条时按 player_id 升序）。不能先锁角色再回头锁第二个账号。现有建角／补齐／删除同样先账号后角色，避免双向过户与建角互锁。事务内不调用 Redis、scene、支付或 Kafka；超时／未知提交只用原 tx_id 强读消歧，禁止发新 tx_id 补做。home_zone、名字注册与玩法资产保持原记录，不重建反向 Redis 权威。

目录交易实现为 `BeginAccountRoleTrade/ConfirmAccountRoleTradeFreeze/CancelAccountRoleTrade/TransferAccountRole` 四 RPC。`account_role.trade_listing_id` 是当前锁；`account_role_trade` 保留逐 listing 状态与原请求摘要；`account_role_transfer_receipt` 保存逐 tx_id 的原角色与原修订。P4 初始原语批次追加默认 0 的交易锁并新建这两张收据表；随后本批再加下节的入场／holder／维护收据与历史 SQL 下限。字段、主键、逐字节账号和摘要列漂移均拒绝。运行期复用目录权威池，trade 证明只读 `MMORPG_ACCOUNT_DIRECTORY_SECRET_TRADE`，与 login 的目录证明分开。

`RoleFreezeVerifier.VerifyFrozen(ctx, tx, 原listing收据)` 接受服务端注入的 SQL 验证器，持有同一目录事务，不接受请求中的任意证明。生产仅在 `AccountDirectory.RoleTradeSQLProof.Enabled=true`、配置非空 AllowedStorageIDs 且路由为 db_first 时装配 `SQLRoleFreezeVerifier`；目录池同时强制真实 TiDB。未配、白名单非法或路由模式不符保持 nil 并拒绝 Confirm。测试验证器仅隔离旧状态机回归，不能作为原生证据。EnterGame 的重复 Lookup 仍只是前置闸，最终授权依赖下一节的原生 Register，不能以重读代替该提交点。

Cancel 已增加 `CANCEL_PENDING`：本轮任一 holder 未 ACK 时继续保留目录锁，全部 holder 安装原世代墓碑并排空后，原请求重试才清锁为 CANCELLED。取消与冻结共用 seal≤原 upper 的排空方式，允许取消导致重新登录；不实现会与迟到 Freeze 竞争的异步 resume。旧 seal 只能封原 instance/revision 的原上界，不能误伤取消后新世代或买家修订。Transfer 清锁仍不能代替实际原生与市场整链验收。

## P4 原生入场登记与 SQL 证明（源码候选）

内部共用消息源为 `proto/common/base/role_admission.proto`。`RoleAdmissionIdentity` 绑定 player、scene 进程 UUID、进程单调 generation、login 捕获的 owner_revision、scene owner_epoch、node/scene/zone/session/login_generation。修订经 login→SceneManager→Kafka Route→gate→scene 原样传播；0 拒绝、修订参与 request_id 指纹，不能给旧卖家路由反查并补上买家修订。客户端协议不增加字段。

`RegisterRoleAdmission/CloseRoleAdmissions/AckRoleTradeDrain` 仅允许 scene 的完整方法及正文签名，固定环境变量 `MMORPG_ACCOUNT_DIRECTORY_SECRET_SCENE`，不复用 login、trade 或资产通道证明。`account_role_admission` 每 (player_id,scene_instance_id) 一行，保留最高 generation、closed_through、原身份／请求摘要、最后 Close 摘要及最大所需 SQL (owner_epoch,save_seq)。每次登录不追加历史行。同 generation 只有原完整业务正文重放；较旧或已关闭 generation 拒绝。新 generation 不表示旧实体与异步回调已经结束，scene 必须排空整个连续前缀后才能 Close。

Register 与 Begin 在同 account_role 行锁裁决。Begin 先持久 FREEZING，再固定所有未关闭实例的 holder 上界；`account_role_trade_holder` 逐 listing/instance 保存原 `RoleTradeSeal` 与 ACK 摘要。全局锁序为账号 guard 原字节升序→listing／维护收据→角色行→入场登记按实例字节序→holder；Register/Close 只取角色→登记，不回头拿账号。待处理 holder 超过 256 明确拒绝，不截断全集。失联实例不按 TTL 假装关闭；恢复或可验证撤销尚未完成时交易保持未完成，不自动删持久水位。

scene 必须在任何实体复用／激活副作用之前成功 Register；请求未知结果只能重试原 generation，不能先激活。Seal 在逻辑线程安装原≤upper 的墓碑，取消 pending 并挡住 SQL load 完成后的迟到回调；实体严格完整存盘、两表 ACK 与本次版本完全相等后卸载，才发原 holder 的 ACK。现有普通退出或 Kafka broker ACK 不等于这份证据，不能直接复用 Destroy 的名字推断排空；新 drain 不写跨场景 handoff，不采用 35 秒自动解冻。原生钩子与故障回归由同批 C++ 接线提供，本文源码候选不冒其已运行。

Close/Ack 的 0/0 只表示本次没有新的激活实体存盘，不清除历史所需 SQL 下限。普通退出即使 SQL 暂未追上，也必须先持久记录最后完整存盘下限；Begin 包括已关闭登记在内捕获全历史最大下限。Confirm 在同 TiDB 事务锁读原 holder 全部 drained、权威 player_route 有效存储以及实际两张玩家 SQL 行；不查 Redis 资产证明，不接受 caller 自报离线。库名只从合法 storage_id 派生，且必须在白名单；冻结落点、缺行、SQL 错误、两行围栏或覆盖世代不齐／超前、SQL 低于所需下限均拒绝。摘要按列名排序并包含全部 SQL 原字节，保留未知字段所在 blob；每行超 16MiB 拒绝。

冻结证据 kind 显式区分 NO_ADMISSION 与 SCENE。只有实际两行 SQL 都为 0/0、无历史所需下限且本轮无 holder 时允许 NO_ADMISSION；SCENE 必须为非零完整围栏。未分类的证据拒绝。Remove 与受控 Import 在同角色行锁下同样拒绝 OPEN admission，关闭水位与历史 SQL 下限保留。

新增回归源码覆盖并发 Register/Begin 提交竞态、八次原请求重放、换载荷拒绝、取消前保持锁、旧 ACK 不关闭新世代、两 SQL 落后／错版、NO_ADMISSION 边界、历史下限不归零、过户后原卖家拒绝、维护导入仅首次换修订和错误阶段零后续副作用。真 TiDB 夹具要求本线程独有 `ACCOUNT_DIRECTORY_TEST_DSN`，只创建／清理本例随机全局库和随机合法 player_store 库，通过正式 proto2mysql 建两张完整玩家表；Require 门控下缺 DSN 为失败，普通 Skip 不算通过。本批只执行格式化与静态检查，尚未生成、编译或运行这些回归。

P4 完整验收至少包括：冻结持久化后新登录拒绝、冻结前已受理的异步登录不能在确认后重新进场、SQL 确认未到时不公示或过户、清 Redis 后锁仍在、同 listing 解冻／旧 Lock 重放不复冻、同 tx_id 原请求重放保持原修订、换载荷拒绝、双向并发过户遵守锁序、买家满位后腾位重试、旧卖家与旧 owner_revision 均不能补齐／删除／入场、未知提交重读无双过户。两版实现与真库／在线故障证据分别登记；本节不是角色链已经实现或运行通过的声明。

## 业务原会话与资产合同提交互锁（2026-10-21，源码候选）

gate发出的`SessionDetails.owner_revision=9`是原会话修订。交易和帮会只读部署显式指定的全局LoginRedis，以原gate进程UUID与连接incarnation找到原账号；键规则由`shared/loginsession`与login共用。缺账号、旧连接、修订0、未声明全局同一登录态实例均拒绝。SQL Lookup只复核这份原账号／修订，不能用当前owner填补旧会话。`shared/roleidentity`复制原件进context；后台处理则从持久合同取原件，不能以后台context没有actor为由绕过归属检查。冻结踢原会话是通知，不作为在途提交的安全证据。

trade普通交易、寄售、拍卖、撮合和人民币合同的所有新增参与者／资产腿，先在同一实际TiDB强会话池按player_id升序锁全体`account_role`，核原actor账号／修订与非交易锁，再取原guard、commerce、seq、outbox锁。普通合同持久`seller_identity=21/buyer_identity=22`；后台退款、撮合子单和原最高竞价退款沿用各自持久身份。缺原件、已换主或修订ABA均拒发，保留托管证据。私有历史按原账号与修订筛选，角色易主不会把旧合同身份授予新主。

人民币角色寄售先提交本地`rmb_freezing`，再调用目录Begin，逐个原holder UUID发送完整正文HMAC Seal，目录Confirm实际SQL证明后才登记商城。登记意图先持久为`rmb_registering`，未知HTTP结果重复原register消歧后才能取消退还，避免迟到上架空托管。购买绑定商城可信原buyerAccount、十进制字符串buyerOwnerRevision及完整交付意图摘要；角色过户固定持久tx_id，未知结果只能重试该tx。取消早于Begin时目录先写永久CANCELLED墓碑，后到Begin只重放终局。取消有未排空holder仍保持锁，不用TTL解冻。好友记录随角色保留。

非角色人民币托管、官方SKU与普通合同均使用角色首锁：新合同／首次buyer绑定先赢则目录冻结拒绝，目录冻结先赢则资产提交拒绝。官方SKU原买家角色在首次受理前已换主且尚无资产op时，提交持久`official_refund`再回商城；人民币交付同理提交`rmb_refund`，不能向当前新账号发放。官方零新增资产的退款决定可不阻止后续出售，但真实pending／manual资产op仍阻止；商城掌管原款退款状态，这不等于已经退款。已发或结局未知的资产指令不能按该例外放行。

目录Begin、Remove与受控Import在同一P行锁下，除原生admission、默认离帮检查外，锁读未终局trade合同、trade/guild pending或manual outbox、mail pending或manual outbox、guild owed及历练ACTIVE参与原件。库名由显式`RoleTradeTradeSchema/RoleTradeGuildSchema/RoleTradeMailSchema`给定，禁止从DSN猜。交易合同超过单角色256条或任一schema／表／查询不可用均拒绝，不能截断全集后宣称无挂起合同。仅自己的CHARACTER `rmb_freezing`原账号／修订例外，不允许借另一listing或资产合同绕过。

帮会客户端由显式全局LoginRedis捕获原actor。`runTxBody`在任何G／state／member／seq／outbox之前，合并原actor和本次所有资产／入帮目标，按player_id升序取P首锁并复核原actor。建帮与审批通过也走此统一前缀，审批不在G锁后回头拿申请人P。目录Begin不取G，只在P之后锁state／原member，所以它不会与原G→state锁序成环。捐献、商店、活动领奖、历练结算全候选和owed转换均声明目标前缀；被冻结owed保留原行，不重建奖励。邮件与好友的同类入口由各自实现批次接线，不能用trade的完成代替它们的门禁。

历练邀请的同意身份是服务端私有`GuildTrialLobbyConsent`，按(lobby,player)在Redis保存不可覆盖原件；发起人建房捕获本人，被邀者亲自同意时捕获本人。缺件或不同账号／修订要求重新组队，不向客户端LobbyMember加入账号。launch前读取全体原同意，在全员P升序与G锁下核原账号／修订及本帮成员，持久`guild_trial_launch`完整原件和`guild_trial_launch_role` ACTIVE索引，再调用原request_id的Match Start。未知结果保留PREPARED，恢复轮转重放原名单、周期、配置与身份，不再从Redis或当前owner捕获。结果通过服务端`BattleActivityContext.launch_request_id=7`定位原件，结算和POISON同事务终结参与索引；owed／真实op继续独立保护资产。原有EXPIRED允许迟到结果，故不是安全撤销证明，不能清ACTIVE。PREPARED释放还要求Match同原意图锁的持久取消墓碑，或真实formation安全中止终局；RPC负答和房间过期都不够。

2026-10-23 更正（第二轮审计 r2-guild#1）：上段没有覆盖「开局成功（Match formation=4）之后战斗不出结果」，即 battle 停机作废、崩溃或活动结果没发出。那时既无结算也无标毒，原先 launch 与 ACTIVE 占用永不解除，冻结、删除、导入与全球匹配入口的挂起守卫一直拒绝这批角色。现在的出口是 Match 的 SQL 终局：权威截止之后，`RecoverExpiredOutcomes` 或帮会重放原 Start 的签名 `CancelActivityBattle`，在原 P→formation 锁内与 COMPLETE 争同一主键，原子提交 ABORT，formation 4→5、全员原票置 4。帮会拿到 formation=5、TerminalDecision=ABORT 的回执后，`FinishCancelledTrialLaunch` 同事务置 launch3、role2、lobby4、slot0，本文挂起守卫随之放行。截止前的 formation=4、一次 RPC 超时、EXPIRED、帮会解散都不是终局，占用照旧保留；formation=5 却带 COMPLETE 的矛盾回执同样拒绝。释放规则只在 `go/guild/internal/data/trial_launch_repo.go` 的 `TrialCancelReceiptIsTerminal` 一处，推导与回归见 cross-zone-matchmaking.md §13.6「2026-10-23 修复」。

装配还必须显式配置trade／guild的`AccountRoleSchema`、`GlobalLoginSessions`和`LoginRedis`；进程实际断言TiDB pessimistic／RC／leader／strict／无快照／无staleness，不能只看配置布尔值。普通独立交易状态机测试可显式注入fixture角色接口，旧目录隔离测试可显式关闭跨域guard；它们不能当原生验收。新增真TiDB回归分别使用独占`ACCOUNT_DIRECTORY_TEST_DSN`、`COMMERCE_TEST_DSN`与`GUILD_ROLE_TEST_DSN`及对应REQUIRE开关，fullstack还必须显式给`TRADE_ASSET_FULLSTACK_GLOBAL_SCHEMA`。本批只执行源码格式化和静态检查，正式生成、编译、真实SQL与双区在线验收须另记实际证据。

## 跨域挂起守卫的终态白名单（2026-10-21 源码补充）

data_service 的 `AccountDirectory.RoleTradeTradeSchema`、`RoleTradeGuildSchema`、`RoleTradeMailSchema`、`RoleTradeMatchSchema` 必须显式指定同一实际 TiDB 事务域内的权威 schema，不从 DSN、当前默认库或玩家区号猜测。启动装配先检查实际强读会话，再对所有守卫所需表列执行只读零行查询；不替其他服务建表。任一 schema、表、列或查询不可用则撤掉挂起守卫配置，Begin、Remove 与受控 Import 继续拒绝，不能沿用旧的不完整装配。目录普通读取不因此停用。

这三个归属变更入口均在同一 `account_role` 首锁内核挂起全集。trade/guild outbox 只允许 APPLIED=2、REJECTED=3、ABORTED=4、ABANDONED=6；其中 6 是各域已确认 scene 未见过该 seq 的终态，不能与 APPLIED_PARTIAL=5 混同。mail outbox 只允许 APPLIED=2、REJECTED=3、ABANDONED=5，MANUAL=4 仍阻塞。未识别状态、0 和 NULL 均阻塞；guild owed 任一行以及 launch_role 非精确 TERMINAL=2 也阻塞。commerce 合同保留仅 CANCELLED=7／SETTLED=11 完成的更严格规则；官方零新增资产的 `official_refund` 例外必须同时为 status=8，本次 CHARACTER 自身 `rmb_freezing` 例外必须同时为 status=3，未知状态不能借 stage 字符串绕过。真实 pending／manual outbox 不受上述合同例外豁免。

Match 使用显式 `RoleTradeMatchSchema` 下的 `match_authority_ticket` 原件。state=1／2／3 即使票据墙钟过期仍阻止冻结、删除和导入，只有 Match 自己提交 state=4 后才释放；本域不取消、不改票、不按 TTL 猜终局。先 Cancel 后迟到 Join 的 state=0 占位，仅在 ticket、request_id 均为空且 formation_id=0 时允许；占位可以已记录原账号身份，但这不等于已有有效票。state=0 带原票、请求或 formation，未知状态和 NULL 均拒绝。新匹配参与者也先持同一 P，故票先赢则冻结拒绝，冻结先赢则新参与者拒绝，避免冻结后旧 Cancel 又被归属围栏拒绝而形成死结。

同一 Match schema 下的 `match_challenge_invitation` 同时保护挑战双方：state=1 且按数据库当前时间尚未过期的原件、未知状态、NULL 均阻止双方角色变更归属。精确过期 PENDING 不能再接受，state=2 ACCEPTED 在同 P 事务已交接到真实票／formation，state=3 DECLINED 已拒绝；后二者的后续占用由 ticket 与 admission 继续保护。通知投递失败不删除或改写邀请原件；data_service 不依赖 Redis 通知是否送达来裁决。缺邀请表或所需列与缺票表同样在启动装配和实际提交时拒绝。

新增回归源码覆盖 trade/guild/mail 的 pending、manual、0、未知、NULL、已知安全终态，Match 三类在途、精确空占位与伪空占位，Begin／Remove／Import 的零副作用，以及 Match 新参与与冻结提交竞争。配置解析与缺 schema／缺列装配失败也有回归源。当前只完成源码和静态检查，未执行本轮生成、编译、单测、真实 TiDB 或双区进程验收；测试入口与环境约束登记在本轮构建目录清单，不引用历史测试结果作为本轮通过证据。

## 旧 Scene 入场登记的受控恢复（2026-10-23，已授权实施，待验收）

恢复模块只负责已经固定 listing 的原失联入场登记：调用者是受控维护 CLI，变化原因是旧 Scene 崩溃后无法补发原排空 ACK；它不授予新 Scene 替旧实例宣称排空的权限。正常 Register／Close／Ack 与角色交易提交仍沿上述接口和锁序。负责人先授权两版最多 24 个手写路径及必要正规生成，随后明确授权与历练终局恢复的 76 路径合并，按最多 100 个唯一手写路径统一管理；业务范围和总上限不增加。具体源码、生成、真库与故障结果分开登记。

仅有新 UUID、TTL 到期或 SQL 围栏大于登记身份都不够。主 C++ 的 B5 可以在同一入场 generation 内采纳 E+2，SQL admission.identity 不随本地 owner 改写。例如原登记 E1、实际两 SQL 已为 E3/q1，而原 E3/q2 仍在 pending；以 3>1 退休登记会放过仍合法的 q2。恢复必须核旧实例所有可能已发及未来可发 epoch 的审核上界、旧写资格已撤销的永久证据，以及原完整父内容、pending、outbox 的实际收敛；不能清除未确认义务来制造可恢复条件。

维护正向链必须实际可达：先阻止原写者和迟到 B5 继续取得资格，真实完成或明确拒绝原待投，再在权威 route 和两张完整玩家 SQL 的行锁下建立严格高于旧写上界的永久围栏。现有屏障恢复对非零 SQL 通常保留原 E/q，不能假定它已自动完成这一抬升。若需要只抬围栏，必须有独立的审定计划、原 SQL 全列摘要与版本 CAS，保持全部业务列，原子提交两表并保留提交未知时的原件消歧；不能仅在测试夹具手工抬 E 后声称生产入口成立。

最终退休按账号 guard→原 listing→角色→原 admission／holder→权威 route→两 SQL 的既有顺序，在同一权威事务内复核原身份、修订、连续世代上界、撤销证明及 SQL 版本／覆盖世代／全列摘要，保存永久 SQL 恢复原件和摘要后才退休指定前缀。不能先核 SQL、解锁后再裸调 Close。相同请求重放返回原收据；换 UUID、世代、revision、route 或正文拒绝，迟到旧请求不得推进新登记。恢复原件不是业务效果 APPLIED，也不能代替其他 holder 的排空。

维护退休写下的是专属 recovery 域摘要和永久 SQL 收据，不是旧 Scene 发出的 ACK。既有 `AccountRoleFreezeEvidenceKind.SCENE` 原本分类非初始的实际玩家 SQL 存档，即使没有本轮 holder 也可能返回该值；它不证明收到过某个实例的回包。本分支沿用该分类，具体退休来源必须查看永久恢复原件，不能只凭枚举把维护退休记成真实 Scene 排空回包。

Redis 维护撤销与 SQL 提交不宣称跨存储 ACID：任一阶段失败保留维护闸和原证据，未知结果复用同一原件消歧，不签恢复成功。CLI 默认只生成可审计划，显式 apply 绑定精确摘要；连接、命名空间和目标范围沿现有本地维护工具约束，秘密仅取环境变量。缺任一行、SQL 两版不齐、世代超前、route 变化、待投仍高于已提交版本、撤销资格不明或权限不足时均拒绝且不关闭原登记。

Java 版按自己的 SQL owner 与最终存盘模型实现。旧 OPEN 只有准入、没有最终快照载荷；现有七天清理的安全依据是同 P／route 锁内的持久更高 E 与最终状态条件，七天本身只是保留期。新工具只退休已固定 listing 的指定旧 OPEN，并持久保存恢复原件；PENDING、未知状态、Match／Inbox／资产待投不进入退休集合，不伪造 EXACT_APPLIED。更高 E 已入场时要求原 SCENE_ACK 与实际 SQL APPLIED；更高 E 仅 claim、尚未 load 时，要求该 E 的最终准入实际缺席及非 NULL 原存档，计划固定剩余旧 OPEN 集合、数量和最后一条语义，仅最后一条退休时复用正式封闭形成真实 SQL_NO_ADMISSION。load 与冻结以同 P 决定先后，不能借无准入分支替已经入场的实体造 ACK。两版必须分别验证真实崩溃后阻断、合法恢复、旧写者迟到拒绝、B5 反例、未确认 pending 拒绝、并发 Cancel／Register／过户、提交未知及原件重放。
