package clientplayerloginlogic

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"login/internal/config"
	"login/internal/logic/pkg/ctxkeys"
	"login/internal/logic/pkg/locker"
	"login/internal/logic/pkg/loginsession"
	"login/internal/logic/pkg/playernamereg"
	"login/internal/svc"
	login_proto_common "proto/common/base"
	login_data_base "proto/common/database"
	login_proto "proto/login"
	"shared/accountdirectory"
	"shared/generated/pb/table"
	gametable "shared/generated/table"
	"shared/playername"
	"strconv"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/zeromicro/go-zero/core/logx"
)

// 以下包变量只为单测替换;生产路径不改它们(没有锁,运行期改会与在途建角产生数据竞争)。
var (
	// loadNameRules 读 RoleNameRule 配表(每次建角现查,配表热更后立即生效)。
	loadNameRules = playernamereg.Rules
	// afterFunc 安排「登记结果未知」时的第二次补偿释放;单测替换成同步执行。
	afterFunc = func(d time.Duration, f func()) { time.AfterFunc(d, f) }
	// delayedNameReleaseAfter 是第二次补偿释放的延迟,理由见 playernamereg.DelayedReleaseAfter。
	delayedNameReleaseAfter = playernamereg.DelayedReleaseAfter
)

const (
	// reserveNameBudget 是名字登记这一步的总预算(含同名重试、生成名换名重试)。
	// 它计入 login.yaml Locker.AccountLockTTL 的持锁耗时估算,改它要同步改那边的注释与取值。
	reserveNameBudget = 3 * time.Second
	// SQL提交未知后的独立强读预算，不继承已取消的请求。
	accountBlobReadBackTimeout = time.Second
	// createLockReleaseTimeout:释放建角锁的预算。释放不跟请求 ctx 走(见 CreatePlayer 第 3 步的 defer),
	// 所以自带上界。含义同 accountBlobReadBackTimeout:掐掉的是 go-redis 的内部重发,
	// 单次 EVAL 最长仍会等满约 3s 的 socket 读超时。
	createLockReleaseTimeout = time.Second
)

type CreatePlayerLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCreatePlayerLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreatePlayerLogic {
	return &CreatePlayerLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *CreatePlayerLogic) CreatePlayer(in *login_proto.CreatePlayerRequest) (*login_proto.CreatePlayerResponse, error) {
	resp := &login_proto.CreatePlayerResponse{
		Players: make([]*login_proto.AccountSimplePlayerWrapper, 0),
	}

	// 1. Get Session details
	sessionDetails, sessionFound := ctxkeys.GetSessionDetails(l.ctx)
	if !sessionFound || sessionDetails.SessionId <= 0 {
		logx.Error("SessionId not found or invalid during player creation")
		resp.ErrorMessage = &login_proto_common.TipInfoMessage{Id: uint32(table.LoginError_kLoginSessionIdNotFound)}
		return resp, nil
	}

	// 2. Get account —— 只认本条 gate 连接的登录态(loginsession.RefOf):没在这条连接上成功 Login 过就拒绝,
	//    不会因 gate 重启撞号而读到上一个 gate 进程同号连接的账号(审计 data-overwrite-rollback-audit-20261018.md §2.3)。
	account, err := loginsession.GetAccount(l.ctx, l.svcCtx.RedisClient, loginsession.RefOf(sessionDetails))
	if err != nil {
		logGetAccountFailure("CreatePlayer", sessionDetails, err)
		resp.ErrorMessage = &login_proto_common.TipInfoMessage{Id: uint32(table.LoginError_kLoginSessionNotFound)}
		return resp, nil
	}

	// 3. Lock (UUID + Lua safe release — prevents concurrent character creation)
	createLock, err := locker.NewRedisLocker(l.svcCtx.RedisClient).TryLock(
		l.ctx, "account_lock:create:"+account,
		time.Duration(config.AppConfig.Locker.AccountLockTTL)*time.Second,
	)
	if err != nil || !createLock.IsLocked() {
		logx.Errorf("CreatePlayer lock acquire failed for account=%s: %v", account, err)
		resp.ErrorMessage = &login_proto_common.TipInfoMessage{Id: uint32(table.LoginError_kLoginInProgress)}
		return resp, nil
	}
	defer func() {
		// 释放用 WithoutCancel:走到这里时请求 ctx 可能已经超时(zrpc 服务端 Timeout)或被客户端取消。
		// go-redis 在取连接之前先看 ctx,拿请求 ctx 去释放只会原地失败、命令根本不发出,锁要等
		// AccountLockTTL 自然过期,期间同账号重试建角一律 kLoginInProgress。
		// 释放是按令牌比对的 Lua 删除:锁若已过期并被另一次建角拿走,这里删不到别人的锁。
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(l.ctx), createLockReleaseTimeout)
		defer cancel()
		if _, err := createLock.Release(releaseCtx); err != nil {
			logx.Errorf("CreatePlayer lock release failed for account=%s: %v", account, err)
		}
	}()

	// 账号目录只强读全局 SQL；Redis 锁仍用于限制单账号本地在途，不裁决角色上限。
	userAccount, err := readAccountDirectory(l.ctx, l.svcCtx, account, false)
	if err != nil {
		resp.ErrorMessage = loginTip(table.LoginError_kLoginDataSerializeFailed)
		return resp, nil
	}

	// 6a. 职业/性别参数(class_id=0 / gender=0 兼容旧客户端与 robot:取配表第一个职业、默认男)
	classId := in.ClassId
	if classId == 0 {
		if rows := gametable.ClassTableManagerInstance.FindAll(); len(rows) > 0 {
			classId = rows[0].Id
		}
	} else if !gametable.ClassTableManagerInstance.Exists(classId) {
		resp.ErrorMessage = &login_proto_common.TipInfoMessage{Id: uint32(table.LoginError_kLoginUnknownError)}
		logx.Errorf("CreatePlayer rejected: class_id=%d not in Class table (account=%s)", classId, account)
		return resp, nil
	}
	gender := in.Gender
	if gender == 0 {
		gender = 1
	}
	if gender > 2 {
		resp.ErrorMessage = &login_proto_common.TipInfoMessage{Id: uint32(table.LoginError_kLoginUnknownError)}
		logx.Errorf("CreatePlayer rejected: gender=%d invalid (account=%s)", in.Gender, account)
		return resp, nil
	}
	if !validCharacterAppearance(in.GetAppearanceId()) {
		resp.ErrorMessage = loginTip(table.LoginError_kLoginUnknownError)
		logx.Infof("CreatePlayer rejected: invalid appearance_id (bytes=%d)", len(in.GetAppearanceId()))
		return resp, nil
	}

	// 6a'. 名字预检:纯计算、无副作用,所以必须排在发号**之前** —— 名字不合规是玩家手滑的
	// 常态,不该为它烧掉一个 PlayerId,更不该碰登记表。规则读不出来(配表缺行 / 不合法)时
	// fail-closed 拒绝建角:带着坏规则放行,写进唯一键的就是一个不合规的名字。
	// 空名不拒绝:正式客户端界面必填,空名只来自机器人 / 无 UI 路径,由 6c 生成默认名。
	rules, spec, attempts, err := loadNameRules()
	if err != nil {
		logx.Errorf("Refusing to create player: RoleNameRule unavailable (account=%s): %v", account, err)
		resp.ErrorMessage = loginTip(table.LoginError_kLoginDataSerializeFailed)
		return resp, nil
	}
	display, _, verdict := playername.Normalize(in.GetName(), rules)
	requested := ""
	switch verdict {
	case playername.VerdictOK:
		requested = display
	case playername.VerdictEmpty:
		// requested 留空,6c 生成默认名。
	case playername.VerdictSensitive:
		logx.Infof("CreatePlayer rejected: role name %q hits the sensitive word list (account=%s)", display, account)
		resp.ErrorMessage = loginTip(table.LoginError_kRoleNameSensitive)
		return resp, nil
	default:
		// VerdictInvalid,以及将来新增而这里还不认识的判定:一律按不合规拒绝(fail-closed)。
		// 只记长度不记原文:原文没过字符集校验,可能是任意字节。
		logx.Infof("CreatePlayer rejected: role name invalid (verdict=%s raw_bytes=%d account=%s)",
			verdict, len(in.GetName()), account)
		resp.ErrorMessage = roleNameInvalidTip(rules)
		return resp, nil
	}

	// 6b. 铸 PlayerId:号段优先(设计稿 §6),是否回退 snowflake 由 IdSegment 配置决定。
	// 发号失败建角整体失败。绝不能吞掉错误再用 0 或自造 id —— snowflake 那条路上,
	// 那会与接管了同一 worker id 的进程发出逐位相同的 PlayerId,而 player_database
	// 的写路径是 INSERT ... ON DUPLICATE KEY UPDATE,撞号不报错而是静默串档。
	stageDone := observeStage(createPlayerStageSeconds, createStageMint)
	newPlayerId, tip := l.mintPlayerID(account)
	stageDone()
	if tip != nil {
		resp.ErrorMessage = tip
		return resp, nil
	}

	// 6b'. 新铸 id 的只读探测(docs/design/new-player-first-enter-no-cold-read.md §10 P7,负责人 2026-10-15 定):
	// PlayerAllData 父键或 player:{id}:owner_epoch 已在 = 这个 id 上已有既有状态(id 复用),探测出错 = 探不清,
	// 两者都拒绝建角(fail-closed)。排在 6c 之前:拒绝发生在任何持久写之前,只烧掉这一个 id,名字与 player:zone
	// 都不登记,无需补偿。与种子开关无关,每次建角都探。它只是预检,不是提交点:开种子时 6d' 的种子脚本对「探测 → 种子」
	// 这一段做写时原子复核;关种子(默认)时探测之后、开种子时种子之后,到第 7 步围栏写都没有复核(已知残余窗口,
	// 接受理由见设计 §13.2 的 2026-10-16 补注;6c 名字登记的 FailedPrecondition 是与时序无关的另一道拦截)。
	stageDone = observeStage(createPlayerStageSeconds, createStageIDProbe)
	tip = l.probeMintedID(account, newPlayerId)
	stageDone()
	if tip != nil {
		resp.ErrorMessage = tip
		return resp, nil
	}

	// 6c. 登记名字(全服唯一,真源是 data_service 的 player_name 表),**fail-closed**:
	// 登记不成功就不建角。顺序固定为 mint → 探测(6b')→ 名字 → player:zone → SQL 账号目录:
	// 名字排在 player:zone 之前,是因为撞名是玩家侧的常态失败,而 player:zone 映射是
	// SET NX 且没有 TTL、也没有删除入口 —— 先登记映射再撞名,每次撞名都会留下一条永久的
	// 幽灵映射,回档孤儿报告与合服重映射都会扫到它。名字登记失败只烧掉一个号段 id,
	// 不留任何持久状态。
	stageDone = observeStage(createPlayerStageSeconds, createStageName)
	name, owner, tip := l.reservePlayerName(account, newPlayerId, requested, rules, spec, attempts)
	stageDone()
	if tip != nil {
		// 6c'. 「名字被占」且占用者就是本账号里一个职业、性别都相同的角色 = 上一次建角其实
		// 成功了、只是响应丢了,客户端拿同样的参数重试。这时不建新角色、也不报「已被使用」
		// (玩家会被自己的名字挡住),直接按第 8 步返回当前全量列表:客户端在响应里 diff
		// 不出新 id 时取最后一个角色,正好进入那个已经建好的角色。
		// owner 只在这里用于比对,**不下发客户端**。本次铸出的 id 已经烧掉,无害。
		if tip.Id == uint32(table.LoginError_kRoleNameTaken) &&
			isLostResponseRetry(userAccount.SimplePlayers.Players, owner, classId, gender, in.GetAppearanceId()) {
			logx.Infof("[player-name] create retry matched existing player_id=%d (burned id=%d) account=%s",
				owner, newPlayerId, account)
			appendRoleList(resp, userAccount)
			return resp, nil
		}
		resp.ErrorMessage = tip
		return resp, nil
	}

	// 6c''. 从这里起每一次写之前都先查请求 ctx(docs/design/client-request-ordering.md §6.1 / O11)。
	// 请求一过截止,go-zero 超时拦截器立刻给路由服回 DeadlineExceeded,路由服转成带 id 的 1003,
	// gate 随即放行同一会话的下一条请求,而本 handler 的 goroutine 还在跑:它此后再发出的写都可能
	// 晚于下一条落地,所以截止之后不再发任何新的写,只做补偿。走到这里名字已确定登记成功,
	// 放弃时按「确定已登记」释放一次(补偿走 WithoutCancel,见 releaseName)。
	// 不只靠 go-redis / grpc-go 在发命令前看 ctx:那是传输层的实现细节,而且 go-redis 那条路会
	// 先把围栏写当成「未确认」去回读一次;这里显式查,保证与结论都收在本函数里。
	// 放弃点在 6d 之后时,player:zone 映射会留下一条(SET NX、无 TTL),与第 7 步失败留下的同类,见 6d 注释。
	// tip 复用「服务端此刻建不了角,稍后重试」那条 kLoginDataSerializeFailed:截止之后这份回包
	// 已经没有接收方(拦截器早已返回),不为它申请新码。
	abandonAfterDeadline := func(nextStep string) bool {
		err := l.ctx.Err()
		if err == nil {
			return false
		}
		logx.Infof("[create-player] request ctx done before %s, sending no further writes "+
			"(account=%s player_id=%d): %v", nextStep, account, newPlayerId, err)
		l.releaseName(account, newPlayerId, name, false)
		resp.ErrorMessage = loginTip(table.LoginError_kLoginDataSerializeFailed)
		return true
	}
	if abandonAfterDeadline("player:zone registration") {
		return resp, nil
	}

	// 6d. 登记 player:zone 归属映射,**fail-closed**:登记失败整体拒绝建角,一个字节都不落盘。
	// 这条映射是数据路由 / 合服重映射 / 公会 / 榜的归属权威,而生产侧**只有这一个写入点**
	// (其余写入者只有 data_service 的 debug_import 与合服重映射)。先落 SQL 账号目录再登记失败,
	// 造出的是「账号里有、映射里没有」的玩家:tools/merge_zone 的重映射按映射扫,扫不到他,
	// 合服后他的数据就烂在已下线的源 zone —— 这正是修复前线上的状态(映射零写入,
	// 合服重映射找到 0 个玩家)。建角失败玩家重试一次就好,代价远小于一个静默错路由的角色。
	// 顺序也因此固定:先登记(幂等、可重试、无本地副作用),后落盘。
	// HomeZone 为 nil / DataServiceRpc 未配置时 RegisterPlayerZone 返回 ErrUnavailable,
	// 同样走这条拒绝路径 —— 接线缺失在启动时已有 ERROR 日志(svc/home_zone.go)。
	// 走到这里名字已经登记成功,所以失败时要先把名字释放掉再返回(确定已登记,一次即可)。
	stageDone = observeStage(createPlayerStageSeconds, createStageRegister)
	tip = l.registerHomeZone(account, newPlayerId)
	stageDone()
	if tip != nil {
		l.releaseName(account, newPlayerId, name, false)
		resp.ErrorMessage = tip
		return resp, nil
	}

	// 6e. Name 是登记表的只读副本(真源 data_service player_name);读侧遇到空名会回源。
	newPlayer := &login_proto_common.AccountSimplePlayer{
		PlayerId:     newPlayerId,
		ClassId:      classId,
		Gender:       gender,
		ZoneId:       config.AppConfig.Node.ZoneId,
		Name:         name,
		AppearanceId: in.GetAppearanceId(),
	}
	userAccount.SimplePlayers.Players = append(userAccount.SimplePlayers.Players, newPlayer)

	// 全局 SQL 事务再次复核账号角色上限；提交前取消则不发送写请求。
	if abandonAfterDeadline("account directory SQL commit") {
		return resp, nil
	}
	// 保留原 ID 复用的提交前拒绝；这里只读，不在 SQL 成功前写可信缓存。
	if config.AppConfig.NewPlayerSeed.Enabled {
		if tip = l.checkInitialSeedState(account, newPlayerId); tip != nil {
			l.releaseName(account, newPlayerId, name, false)
			resp.ErrorMessage = tip
			return resp, nil
		}
	}
	stageDone = observeStage(createPlayerStageSeconds, createStageAccountWrite)
	initial, tip := l.persistAccountRoleWithInitial(account, createLock, newPlayer, userAccount)
	stageDone()
	if tip != nil {
		resp.ErrorMessage = tip
		return resp, nil
	}

	// 已提交的目录不能因可选缓存优化失败再释放名字或假装建角失败。
	// 仅本次严格新建的真实 SQL 回执可缓存；未知提交/旧目录重放没有这种回执。
	if config.AppConfig.NewPlayerSeed.Enabled && initial != nil && l.ctx.Err() == nil {
		stageDone = observeStage(createPlayerStageSeconds, createStageSeed)
		l.cacheCommittedNewPlayer(account, initial)
		stageDone()
	}

	// 7b. Write reverse mapping: player_id → account (for rollback orphan cleanup)
	// 归属已经随角色目录在 SQL 提交，不再写 Redis 反向索引。

	// 8. Return player info
	appendRoleList(resp, userAccount)

	logx.Infof("Player created successfully, account: %s, playerId: %d, name: %q", account, newPlayerId, name)
	return resp, nil
}

// appendRoleList 是第 8 步:把账号当前的全量角色列表放进响应。
// 正常建角与 6c'(丢响应后的重试)两条成功路径共用。
func appendRoleList(resp *login_proto.CreatePlayerResponse, userAccount *login_data_base.UserAccounts) {
	for _, p := range userAccount.GetSimplePlayers().GetPlayers() {
		resp.Players = append(resp.Players, &login_proto.AccountSimplePlayerWrapper{Player: p})
	}
}

// loginTip 组一条 login 段的 tip;params 为空时与直接写 TipInfoMessage{Id: ...} 等价。
func loginTip(code table.LoginError, params ...string) *login_proto_common.TipInfoMessage {
	return &login_proto_common.TipInfoMessage{Id: uint32(code), Parameters: params}
}

// roleNameInvalidTip:kRoleNameInvalid 的文案不写死数字(长度是配表数值),
// 由 parameters = [min_chars, max_chars](十进制字符串)带给客户端回显。
func roleNameInvalidTip(rules playername.Rules) *login_proto_common.TipInfoMessage {
	return loginTip(table.LoginError_kRoleNameInvalid, strconv.Itoa(rules.MinRunes), strconv.Itoa(rules.MaxRunes))
}

// isLostResponseRetry 判定「名字被占」是不是本账号上一次建角丢了响应之后的重试(见 6c')。
// 三个条件缺一不可:占用者非 0、就在本账号的角色列表里、职业/性别/外观都和请求相同。
// 不同外观也不是同一次重试，即使职业、性别相同也不能误回旧人物。
func isLostResponseRetry(players []*login_proto_common.AccountSimplePlayer, owner uint64, classID, gender uint32, appearanceID string) bool {
	if owner == 0 {
		return false
	}
	for _, p := range players {
		if p.GetPlayerId() == owner {
			return p.GetClassId() == classID && p.GetGender() == gender && p.GetAppearanceId() == appearanceID
		}
	}
	return false
}

// mintPlayerID 是建角的发号步骤。单独抽出来只为可测:CreatePlayer 其余每一步都要 Redis。
//
// 失败返回的 tip 沿用发号器不可用时一直在用的 kLoginDataSerializeFailed —— 不另发新码:
// 客户端对它的处理(提示后重试)正是我们要的行为,而 Tip 码轴按段由导表器发号,
// 这里不手写数字。
func (l *CreatePlayerLogic) mintPlayerID(account string) (uint64, *login_proto_common.TipInfoMessage) {
	minter := l.svcCtx.PlayerIDMinter
	if minter == nil {
		// 接线错误(NewServiceContext 一定会装上它),按不可用处理而不是 nil 解引用崩掉整个进程。
		logx.Errorf("Refusing to create player: PlayerIDMinter not wired (account=%s)", account)
		return 0, &login_proto_common.TipInfoMessage{Id: uint32(table.LoginError_kLoginDataSerializeFailed)}
	}
	id, err := minter.Mint(l.ctx)
	if err != nil {
		logx.Errorf("Refusing to create player: id generator unavailable (account=%s): %v", account, err)
		return 0, &login_proto_common.TipInfoMessage{Id: uint32(table.LoginError_kLoginDataSerializeFailed)}
	}
	return id, nil
}

// nameReserveOutcome 是对**一个名字**尝试登记(含结果未知时的同名重试)之后的结论。
type nameReserveOutcome uint8

const (
	nameReserveFailed nameReserveOutcome = iota // 故障;需要的补偿已在 tryReserveName 里做完
	nameReserved                                // 登记成功(含同 id 同名的幂等命中)
	nameTaken                                   // 被别的 player_id 占用,确定没写
	nameRejected                                // data_service 复检判不合规,确定没写
)

// reservePlayerName 为新角色登记名字(调用点 6c)。
//
// requested 非空 = 玩家输入的名字(已过 6a' 预检),只试这一个;
// requested 为空 = 服务端生成「前缀 + 随机后缀」,撞名就换一个,最多 attempts 次。
//
// 返回 (name, 0, nil) 表示 name 已登记在 playerID 名下;否则返回 tip,此时登记表里
// **没有**留下本次的登记(结果未知的那种失败已在内部安排了补偿释放),调用方不用再释放。
// owner 只在 tip 为 kRoleNameTaken 时非 0,供 6c' 判重试,不得下发客户端。
//
// 故障类 tip 复用 kLoginDataSerializeFailed,理由同 mintPlayerID / registerHomeZone:
// 语义同档(「服务端此刻建不了角,稍后重试」),不为一个分支去申请新码。
func (l *CreatePlayerLogic) reservePlayerName(account string, playerID uint64, requested string,
	rules playername.Rules, spec playername.GenerateSpec, attempts int) (string, uint64, *login_proto_common.TipInfoMessage) {

	ctx, cancel := context.WithTimeout(l.ctx, reserveNameBudget)
	defer cancel()

	if requested != "" {
		outcome, owner := l.tryReserveName(ctx, account, playerID, requested)
		switch outcome {
		case nameReserved:
			return requested, 0, nil
		case nameTaken:
			return "", owner, loginTip(table.LoginError_kRoleNameTaken)
		case nameRejected:
			// login 预检放行、data_service 复检拒绝:两边跑的 shared/playername 不是同一个版本
			// (滚动发布中途,或有一边漏发)。以服务端为准拒绝,但必须有人看到。
			logx.Errorf("[player-name] data_service rejected a name login had validated "+
				"(shared/playername version skew?) player_id=%d name=%q account=%s", playerID, requested, account)
			return "", 0, roleNameInvalidTip(rules)
		default:
			return "", 0, loginTip(table.LoginError_kLoginDataSerializeFailed)
		}
	}

	for i := 0; i < attempts; i++ {
		// 前几次撞名可能已经把预算吃掉了:预算不够就别再发,一个 RPC 都没发也就无需补偿。
		if !hasBudget(ctx, playernamereg.DefaultReserveTimeout) {
			logx.Errorf("[player-name] generated-name budget exhausted after %d attempt(s) player_id=%d account=%s",
				i, playerID, account)
			return "", 0, loginTip(table.LoginError_kLoginDataSerializeFailed)
		}
		// crypto/rand:生成名要不可预测,否则能提前把下一个默认名抢注掉。
		candidate, err := playername.Generate(rand.Reader, spec)
		if err != nil {
			logx.Errorf("[player-name] generate default name failed player_id=%d account=%s: %v", playerID, account, err)
			return "", 0, loginTip(table.LoginError_kLoginDataSerializeFailed)
		}
		outcome, _ := l.tryReserveName(ctx, account, playerID, candidate)
		switch outcome {
		case nameReserved:
			return candidate, 0, nil
		case nameTaken:
			continue // 随机后缀撞了别人,换一个
		case nameRejected:
			// 生成名由配表前缀 + [a-z0-9] 拼成,Rules() 已自检过;还被拒只能是两边规则版本错配。
			logx.Errorf("[player-name] data_service rejected a server-generated name "+
				"(shared/playername version skew?) player_id=%d name=%q account=%s", playerID, candidate, account)
			return "", 0, loginTip(table.LoginError_kLoginDataSerializeFailed)
		default:
			return "", 0, loginTip(table.LoginError_kLoginDataSerializeFailed)
		}
	}
	logx.Errorf("[player-name] all %d generated names were taken player_id=%d account=%s "+
		"(raise RoleNameRule.generated_suffix_len?)", attempts, playerID, account)
	return "", 0, loginTip(table.LoginError_kLoginDataSerializeFailed)
}

// tryReserveName 对**同一个名字**做「一次,结果未知时再同名重试一次」。
//
// 错误分类(设计 §3.11)的核心是区分「确定没写」与「结果未知」,两者的补偿动作相反:
//   - ErrUnavailable:客户端没接线,一个 RPC 都没发 → 不释放、不计孤儿;
//   - FailedPrecondition:这个 player_id 名下已有**另一个**名字(发号器把在役 id 又发了一次)
//     → 确定没写,而且**绝不能释放** —— 条件删除虽然按 name 匹配,但这是 ID 安全事件,
//     login 不该再对这个 id 发任何写;
//   - 其它错误(超时 / Unavailable / 连接断 / 熔断):INSERT 可能已经提交。剩余预算够就同名
//     重试一次 —— 同 id 同名在服务端幂等:那条在途 INSERT 若已提交,重试得到成功;
//     若连接被杀、语句回滚,重试直接插入;重试得到「被占」则说明先前那次必未插入
//     (否则占用者就是自己,会得到成功)。重试后仍未知才放弃,并按 uncertain 补偿释放。
func (l *CreatePlayerLogic) tryReserveName(ctx context.Context, account string, playerID uint64, name string) (nameReserveOutcome, uint64) {
	// 发出之前先查 ctx(请求截止 / 名字总预算用完 / 已取消):此刻一个 RPC 都没发,是「确定没写」,
	// 不补偿。不查的话,grpc-go 会以 Canceled / DeadlineExceeded 拒掉这次调用,而这两个码在下面
	// 归入「结果未知」,于是对一个根本没发出的登记补发立即 + 延迟两次释放,还记一条误导的
	// uncertain reservation ERROR。截止之后不再发写,见 client-request-ordering.md §6.1。
	if err := ctx.Err(); err != nil {
		logx.Infof("[player-name] request ctx done before reserving, nothing sent player_id=%d account=%s: %v",
			playerID, account, err)
		return nameReserveFailed, 0
	}
	res, err := l.reserveNameOnce(ctx, playerID, name)
	if err != nil && isReserveOutcomeUnknown(err) && hasBudget(ctx, playernamereg.DefaultReserveTimeout) {
		logx.Infof("[player-name] reserve outcome unknown, retrying the same name once player_id=%d name=%q account=%s: %v",
			playerID, name, account, err)
		res, err = l.reserveNameOnce(ctx, playerID, name)
	}
	if err != nil {
		switch {
		case errors.Is(err, playernamereg.ErrUnavailable):
			logx.Errorf("Refusing to create player: player name registry not wired (account=%s player_id=%d): %v",
				account, playerID, err)
		case status.Code(err) == codes.FailedPrecondition:
			logx.Errorf("[player-name] player_id=%d already holds a different name — id reuse? "+
				"refusing to create and NOT releasing (account=%s name=%q): %v", playerID, account, name, err)
		default:
			logx.Errorf("[player-name] reserve outcome still unknown, giving up and releasing "+
				"player_id=%d name=%q account=%s: %v", playerID, name, account, err)
			l.releaseName(account, playerID, name, true)
		}
		return nameReserveFailed, 0
	}

	switch res.Code {
	case playername.ReserveOK:
		return nameReserved, 0
	case playername.ReserveTaken:
		return nameTaken, res.Owner
	case playername.ReserveInvalid:
		return nameRejected, 0
	default:
		// 不认识的结果码(data_service 比 login 新):不能当成功,按故障拒绝。
		// 同时补偿释放一次:我们读不懂这个码,也就**不知道**它是否意味着「已登记」。若已登记而不释放,
		// 名字会成为一个没有孤儿日志、也没有孤儿计数的静默孤儿。释放是按 (player_id, name) 的条件删除,
		// 而 playerID 是本次新铸、即将放弃的 id —— 删了不可能误伤任何在役角色;没登记则删个空,无害。
		// 用 uncertain=false:这是一条确定的应答,没有在途 INSERT,不需要延迟的第二次;
		// 这一次释放失败会照常记孤儿(有日志、可手工处理)。
		logx.Errorf("[player-name] unknown ReservePlayerName result=%d, refusing to create and releasing "+
			"player_id=%d name=%q account=%s", res.Code, playerID, name, account)
		l.releaseName(account, playerID, name, false)
		return nameReserveFailed, 0
	}
}

// reserveNameOnce 发一次 ReservePlayerName,套单次 RPC 预算(外层 ctx 已带总预算,取两者较早者)。
func (l *CreatePlayerLogic) reserveNameOnce(ctx context.Context, playerID uint64, name string) (playernamereg.ReserveResult, error) {
	rpcCtx, cancel := context.WithTimeout(ctx, playernamereg.DefaultReserveTimeout)
	defer cancel()
	return l.svcCtx.PlayerNames.Reserve(rpcCtx, playerID, name)
}

// isReserveOutcomeUnknown:err 是否意味着「INSERT 可能已经提交」。分类见 tryReserveName。
func isReserveOutcomeUnknown(err error) bool {
	if errors.Is(err, playernamereg.ErrUnavailable) {
		return false
	}
	return status.Code(err) != codes.FailedPrecondition
}

// hasBudget 判断 ctx 是否还剩至少 need 的时间预算(已取消 / 已过期算没有)。
// 用截止时间而不是「已重试几次 × 单次超时」:前面的调用可能提前返回,也可能拖满。
func hasBudget(ctx context.Context, need time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	deadline, ok := ctx.Deadline()
	return !ok || time.Until(deadline) >= need
}

// releaseName 是建角失败时对名字登记的补偿。
//
// uncertain=false:名字**确定**已登记在 playerID 名下(Reserve 明确成功过),后续步骤失败。
// 立即释放一次;失败即记孤儿。
//
// uncertain=true:Reserve 的结果未知(两次都超时 / 断连)。立即释放一次之后,还要延迟再发
// 第二次 —— 第一次可能跑在那条在途 INSERT 提交**之前**(删了个空,随后 INSERT 落地,
// 名字就成了没人要的孤儿)。playerID 是本次新铸、已经放弃的 id,晚删不会误伤任何人;
// 两次都落在 data_service 免 token 的释放窗口(10 分钟)内。孤儿只在延迟那次失败时才记:
// 立即那次失败还有第二次兜底。
//
// 释放用 WithoutCancel:走到补偿往往正是因为请求 ctx 已经超时 / 被客户端取消,
// 拿它去发释放只会原地再失败一次;延迟那次更是在请求返回之后才跑。
func (l *CreatePlayerLogic) releaseName(account string, playerID uint64, name string, uncertain bool) {
	names := l.svcCtx.PlayerNames
	err := releaseNameOnce(l.ctx, names, playerID, name)
	recordNameRelease(nameReleasePhaseImmediate, err)
	if !uncertain {
		if err != nil {
			recordNameOrphan(account, playerID, name, err)
		}
		return
	}

	// 这条 ERROR 同时是运维线索:login 若在延迟释放之前退出,下面的回调不会跑、也不会有
	// 孤儿日志,只能靠这一行找到 player_id 与名字去手工核对。
	logx.Errorf("[player-name] uncertain reservation: immediate release err=%v, second release in %s "+
		"player_id=%d name=%q account=%s", err, delayedNameReleaseAfter, playerID, name, account)
	parent := l.ctx
	afterFunc(delayedNameReleaseAfter, func() {
		// 回调跑在定时器 goroutine 上,panic 会带走整个 login 进程。
		defer func() {
			if r := recover(); r != nil {
				recordNameOrphan(account, playerID, name, fmt.Errorf("delayed release panicked: %v", r))
			}
		}()
		err := releaseNameOnce(parent, names, playerID, name)
		recordNameRelease(nameReleasePhaseDelayed, err)
		if err != nil {
			recordNameOrphan(account, playerID, name, err)
		}
	})
}

// releaseNameOnce 发一次补偿释放:丢掉 parent 的取消信号(保留 trace 等值),套单次预算。
func releaseNameOnce(parent context.Context, names *playernamereg.Client, playerID uint64, name string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), playernamereg.DefaultReleaseTimeout)
	defer cancel()
	return names.Release(ctx, playerID, name)
}

func recordNameRelease(phase string, err error) {
	result := nameReleaseResultOK
	if err != nil {
		result = nameReleaseResultError
	}
	createPlayerNameReleaseTotal.Inc(phase, result)
}

// recordNameOrphan 记一次「名字可能留在登记表里、却没有对应角色」。日志格式是运维手工释放
// (带 x-admin-token 调 ReleasePlayerName{player_id,name})的输入,改格式要同步改运维文档。
// 不做自动清扫:远端提交仍可能在途，目录暂时缺行不能证明可释放；误释放会拿走在役角色的名字。
func recordNameOrphan(account string, playerID uint64, name string, err error) {
	createPlayerNameOrphanTotal.Inc()
	logx.Errorf("[player-name] orphan reservation player_id=%d name=%q account=%s: %v", playerID, name, account, err)
}

// persistAccountRole 把单个原角色提交到全局目录；SQL 账号锁裁决上限与唯一归属。
// 已失去本次入口锁时不再发新写；SQL 提交结果未知时只强读原 player_id 恢复。
func (l *CreatePlayerLogic) persistAccountRole(account string, createLock *locker.Lock, role *login_proto_common.AccountSimplePlayer, directory *login_data_base.UserAccounts) *login_proto_common.TipInfoMessage {
	_, tip := l.persistAccountRoleWithInitial(account, createLock, role, directory)
	return tip
}

func (l *CreatePlayerLogic) persistAccountRoleWithInitial(account string, createLock *locker.Lock, role *login_proto_common.AccountSimplePlayer, directory *login_data_base.UserAccounts) (*login_data_base.PlayerAllData, *login_proto_common.TipInfoMessage) {
	if role == nil || l.svcCtx.AccountDirectory == nil {
		return nil, loginTip(table.LoginError_kLoginDataSerializeFailed)
	}
	playerID, name := role.PlayerId, role.Name
	if token, err := l.svcCtx.RedisClient.Get(l.ctx, createLock.Key).Result(); err != nil || token != createLock.Value {
		l.releaseName(account, playerID, name, false)
		return nil, loginTip(table.LoginError_kLoginInProgress)
	}
	created, err := accountdirectory.CreateWithInitialCommit(l.ctx, l.svcCtx.AccountDirectory, account, role)
	if err == nil && created != nil && created.Players != nil {
		directory.SimplePlayers = created.Players
		return created.InitialPlayerData, nil
	}
	if errors.Is(err, accountdirectory.ErrFull) || errors.Is(err, accountdirectory.ErrConflict) || errors.Is(err, accountdirectory.ErrInvalid) {
		l.releaseName(account, playerID, name, false)
		if errors.Is(err, accountdirectory.ErrFull) {
			return nil, loginTip(table.LoginError_kLoginAccountPlayerFull)
		}
		return nil, loginTip(table.LoginError_kLoginDataSerializeFailed)
	}
	// 超时可能发生在提交后；仅原账号、原 player_id、原载荷一致才恢复成功。
	ctx, cancel := context.WithTimeout(context.WithoutCancel(l.ctx), accountBlobReadBackTimeout)
	defer cancel()
	stored, readErr := l.svcCtx.AccountDirectory.Lookup(ctx, playerID)
	if readErr == nil && stored.Account == account && proto.Equal(stored.Role, role) {
		if roles, err := l.svcCtx.AccountDirectory.Read(ctx, account, false); err == nil {
			directory.SimplePlayers = roles
			return nil, nil
		}
	}
	// RPC 仍可能在远端提交；缺行也不能据此释放还在途的名字登记。
	recordNameOrphan(account, playerID, name, fmt.Errorf("SQL目录提交未知: write=%v read=%v", err, readErr))
	return nil, loginTip(table.LoginError_kLoginDataSerializeFailed)
}

// registerHomeZone 把新角色登记进 data_service 的 player:zone 映射(见调用点 6d 的理由)。
// 与 mintPlayerID 一样单独抽出来只为可测:CreatePlayer 其余每一步都要 Redis。
// 返回 nil 表示登记成功,可以继续落盘;返回 tip 表示必须拒绝建角。
//
// tip 复用发号失败那条 kLoginDataSerializeFailed:两者语义同档(「服务端此刻建不了角,
// 稍后重试」),客户端对它的处理正是我们要的;而 Tip 码轴按段由导表器发号,
// 这里不手写数字、也不为这一个分支去申请新码。
//
// 落点(docs/design/player-storage-placement.md §8.3):Placement.PinOnCreate 打开时,
// 同一次 RPC 带上 storage_id,由 data_service 与 player:zone 原子一起钉落点;关着时 storage_id=0,
// 请求与落点设计之前逐字节相同。钉落点失败与登记失败是同一次写,自然同样 fail-closed。
func (l *CreatePlayerLogic) registerHomeZone(account string, playerID uint64) *login_proto_common.TipInfoMessage {
	zone := config.AppConfig.Node.ZoneId
	storageID := config.AppConfig.Placement.StorageIDForNewPlayer(zone)
	if err := l.svcCtx.HomeZone.RegisterPlayerZone(l.ctx, playerID, zone, storageID); err != nil {
		logx.Errorf("Refusing to create player: player:zone registration failed "+
			"(account=%s player_id=%d zone=%d storage_id=%d): %v", account, playerID, zone, storageID, err)
		return &login_proto_common.TipInfoMessage{Id: uint32(table.LoginError_kLoginDataSerializeFailed)}
	}
	logx.Infof("[home-zone] registered player:zone player_id=%d zone=%d storage_id=%d (account=%s)",
		playerID, zone, storageID, account)
	return nil
}
