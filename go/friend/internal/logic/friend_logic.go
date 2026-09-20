// Package logic 是 friend 的业务层。
//
// F2 已完成,剩余项见 F3。
//
// # 错误语义(F2-1,本包全部方法一律照此)
//
// 业务结果**一律 in-band**:`return &Resp{ErrorMessage: tipErr(码)}, nil`。
// 原因是 gate / 路由服的回包桥接只认「成功响应」—— 返回 gRPC status 时客户端只看到一个
// 信封级失败,TipInfoMessage 连同它的中文文案一起丢掉,玩家看到的是"网络错误"。
// MySQL / Redis 真故障也走 in-band,用本域唯一的 fault 码 constants.ErrStorage 表达并打
// 一条 Error 日志,由 serverbase 的定性函数记成 rpc_inband_fault(告警接在那里)。
// **本包不再有 `return nil, err`** —— 新增方法时照此,别把这条规矩开个例外。
//
// # 身份与预算
//
// 身份只从会话取(D-9,请求 message 里没有 player_id 字段),目标 id 才看请求体。
// 每个方法入口先套 config.RequestBudget()(= Timeout − InBandReplyReserve,F2-13):
// 本次请求的 MySQL / Redis / Kafka 全用这一个 ctx。单次调用各自的超时只限单次,串行几次
// 就会越过 go-zero 服务端的超时拦截器;拦截器一旦先到,回的是 gRPC DeadlineExceeded,
// 上面那套 in-band 故障码全部丢失。
//
// # 权威判定都在存储层
//
// 本层只做"纯校验 + 频控 + 编解码 + 推送",一切上限与关系判定都在 data 层的权威事务里
// (容量守卫内的当前读,规格 §2 的全局锁序)。**不要**在这里加"先查一下再写"的预检来省
// 一次开库:预检不持锁,两个并发请求都能通过(AGENTS.md §11.3),而且写在这里会让
// "上限在哪里生效"变成两个地方说话。
//
// # F2 之后仍然开着的口子(F3)
//
//  1. **sweep 没有启动点**:本包的 StartSweep(见 sweep.go)在 F2 的 19 文件清单里没有调用方
//     —— go/friend/friend.go 不属本批,不许改。所以终态申请清理目前是**死代码**,
//     friend_request 的终态行仍不回收。接法见 sweep.go 顶部注释。
//  2. **三个新 tip 码**(FriendBlocked / FriendBlockListFull / FriendTargetInboxFull)要等
//     合并回 main 时改 data/tip/Tip.xlsx + 跑导表器,本批刻意不动 xlsx(二进制无法 3-way 合并,
//     主工作区有并行会话在改同一个文件)。在那之前 constants 里那三个枚举常量不存在。
//  3. **推送消息号常量**待 proto-gen 重新发号后核对(见 push.go)。
package logic

import (
	"context"
	"errors"
	"time"

	"github.com/zeromicro/go-zero/core/logx"

	"friend/internal/constants"
	"friend/internal/data"
	"friend/internal/session"
	"friend/internal/svc"
	base "proto/common/base"
	pb "proto/friend"
)

func tipErr(id uint32, msg string) *base.TipInfoMessage {
	return &base.TipInfoMessage{Id: id, Parameters: []string{msg}}
}

// FriendStore 是本层用到的好友存储能力,由 *data.FriendRepo 实现。
//
// 为什么是接口而不是直接拿 *data.FriendRepo(F1 §3.8 当时钉的是具体类型):
// 本层的分支已经多到必须被单测钉住(拉黑拒绝、各类上限、存储故障 in-band、推送不影响
// RPC 结果),而这些分支的触发条件全在 MySQL 事务里 —— 用具体类型就只能起真 MySQL 才能测
// 一条 if。接口让 logic 的单测用假 store 注入每一种 sentinel,真 MySQL 的事务与锁序回归
// 留在 data 包的 *_mysql_test.go 里(§11.4 面向接口测试:接口既是调用面也是测试面)。
//
// **契约**:所有方法在依赖故障时返回**非 sentinel** 的普通 error,调用方一律定性为
// constants.ErrStorage;业务拒绝必须回下面这些 sentinel,新增拒绝语义 = 新增 sentinel,
// 不许用 error 文本判别(errors.Is 认的是值,不是字符串)。
//
// **目标为 0 / 目标是自己由本层在入口挡住**;真走到 data 层会拿到一个非 sentinel 的 error
// 并被定性成 ErrStorage —— 那是服务端缺陷的信号(客户端不该能触发它),
// 不要为它加 sentinel(见 data/friend_repo.go:66-72 的 errInvalidPlayerPair)。
type FriendStore interface {
	// AddFriendRequest 是 AddFriend 的权威事务(规格 §3.1):容量守卫内复核拉黑 / 好友边 /
	// 申请行 / 出站与入站 pending / 双方好友数,再 upsert 申请行。
	// 三个上限收在 data.AddFriendLimits 里而不是三个并排的 uint32 —— 出站与入站上限同类型、
	// 方向相反,位置传反不会报错,只会让上限张冠李戴。
	//
	// sentinel:ErrBlocked / ErrAlreadyFriends / ErrRequestAlreadySent /
	// ErrTooManyPending(我的出站满)/ ErrTargetInboxFull(对方收件箱满)/
	// ErrSenderFriendsFull / ErrAcceptorFriendsFull。
	AddFriendRequest(ctx context.Context, fromPlayerID, toPlayerID uint64, lim data.AddFriendLimits) error

	// AcceptFriend 参数顺序是 (申请人, 接受者)。sentinel:ErrRequestNotFound / ErrBlocked /
	// ErrSenderFriendsFull(申请人满)/ ErrAcceptorFriendsFull(接受者满)。
	//
	// ⚠ "好友数满"全域只有 Sender/Acceptor **一对**哨兵,按 RPC 分别映射:
	// AddFriend 的 me 是 Sender、AcceptFriend 的 me 是 Acceptor,所以同一个哨兵在两条路径上
	// 对应**相反**的 tip 码。映射表的唯一事实源在 data/friend_repo.go:27-40。
	AcceptFriend(ctx context.Context, fromPlayerID, toPlayerID uint64, maxFriends uint32) error

	// RejectFriend sentinel:ErrRequestNotFound。
	RejectFriend(ctx context.Context, fromPlayerID, toPlayerID uint64) error

	// RemoveFriend 幂等:不是好友时返回 nil(且不创建任何容量行,F2-15)。
	RemoveFriend(ctx context.Context, playerID, targetPlayerID uint64) error

	GetFriendList(ctx context.Context, playerID uint64) ([]data.FriendEntry, error)
	GetPendingRequests(ctx context.Context, playerID uint64) ([]data.FriendRequestEntry, error)

	// Block 幂等:已拉黑时提交且不占新名额。sentinel:ErrBlockListFull。
	Block(ctx context.Context, playerID, targetPlayerID uint64, maxBlocks uint32) error
	// Unblock 幂等,无上限可越,不加守卫。
	Unblock(ctx context.Context, playerID, targetPlayerID uint64) error
	// ListBlocks 不带 limit 参数:硬上限(Friend.ListReadHardLimit)在 NewFriendRepo 时
	// 交给 repo 保管,与 GetFriendList / GetPendingRequests 同源 —— 逐调用传值会让
	// "这份列表按谁的上限截的"取决于调用点,出现第二个事实源。
	ListBlocks(ctx context.Context, playerID uint64) ([]data.BlockEntry, error)

	// 推荐两条策略(纯读、不进事务、结果允许轻微陈旧,见 data/recommend_repo.go 的约束)。
	// 由 recommend.go 组策略链,放在同一个接口里是因为它们实现在同一个 repo 上。
	RecommendByMutual(ctx context.Context, playerID uint64, exclude []uint64, limit uint32) ([]data.RecommendCandidate, error)
	RecommendRandom(ctx context.Context, playerID uint64, exclude []uint64, limit uint32) ([]data.RecommendCandidate, error)
}

// SessionStore 读共享库的 player:session:{id} 判在线(规格 §3.9),由 *data.SessionReader 实现。
//
// 它是跨运行时契约 key 的唯一读法:friend 自己**不再**维护 friend:online(F1 已确认零调用方,
// 写者随 NotifyOnline/Offline 一起删了)。
//
// **只有一个返回值,没有 error**:分批(按 ListReadHardLimit 切 MGET)、限流日志、
// metrics.ObserveOnlineLookup 与"读失败降级为全部离线"全在实现内部。
// 在线态是展示字段,调用方拿到的"id 不在 map 里"就是最终答案(= 离线且无活跃记录),
// 没有需要它决策的失败分支 —— 把 error 抬到这里只会让每个调用点重复同一段降级代码,
// 还有重复计指标的风险。
type SessionStore interface {
	FillOnlineStatus(ctx context.Context, playerIDs []uint64) map[uint64]data.OnlineStatus
}

// Deps 是 logic 层的依赖集合。main 装配一次、整进程共用(有状态的东西都在 SvcCtx 里)。
//
// 保留 SvcCtx 整体而不是只拷需要的几个字段:推送要 KafkaWriter / GateCommandBuilder,
// 频控要 FriendRedis,预算要 Config —— 拆开只会让每加一处依赖就改一次 main 的装配。
type Deps struct {
	SvcCtx   *svc.ServiceContext
	Repo     FriendStore
	Sessions SessionStore
	// Sweeps 只被后台清理循环用(sweep.go),与请求路径无关,所以单独一个字段:
	// 假 store 测请求路径时不必连带实现清理方法。
	Sweeps SweepStore
	// Now 是时间源(推送的 ts_ms、sweep 的保留期截止点都取它)。
	// nil 时 NewFriendLogic / StartSweep 回落到 time.Now;单测注入固定时钟。
	Now func() time.Time
}

// 编译期接缝断言。三个接口都是为 logic 的可测性造的(假 store 注入每一种 sentinel),
// 而真实现只在 NewDeps 里被赋值一次 —— 没有这三行时,接口与实现漂移不会在编译期暴露,
// 只会在装配那一刻炸,或者更糟:假 store 全绿而生产路径走另一套语义。
// F2 的五处 B1↔B3 签名冲突(AddFriendLimits / OnlineStatus / FillOnlineStatus /
// ErrInvalidTarget / Self-Peer 哨兵)全部是缺了这三行的后果,别删。
var (
	_ FriendStore  = (*data.FriendRepo)(nil)
	_ SessionStore = (*data.SessionReader)(nil)
	_ SweepStore   = (*data.FriendRepo)(nil)
)

// NewDeps 由 main 调用,签名是 F1 批与 F2 批之间的接缝契约(F1 规格 §3.8)。
func NewDeps(svcCtx *svc.ServiceContext) *Deps {
	// 缓存句柄用 FriendRedis(friend 私有 key),不是 SharedRedis:
	// 共享库是 allkeys-lfu,而且那里只放跨运行时契约 key(契约 §4)。
	// 列表读的硬上限交给 repo 保管(F2 §4):缓存里存的是**已经截断过**的列表,
	// 逐调用传上限会让"缓存里那份按谁的上限截的"取决于谁先回填。
	repo := data.NewFriendRepo(svcCtx.FriendRedis, svcCtx.DB,
		svcCtx.Config.Friend.CacheTTL, svcCtx.Config.Friend.ListReadHardLimit)
	return &Deps{
		SvcCtx: svcCtx,
		Repo:   repo,
		// 在线状态必须走 SharedRedis:player:session:{id} 是 player_locator 写的跨运行时
		// 契约 key,FriendRedis 可能是另一套(甚至 Cluster)实例,读不到它。
		// 一次 MGET 的 key 数按 ListReadHardLimit 分批,上限与列表读同源。
		Sessions: data.NewSessionReader(svcCtx.SharedRedis, svcCtx.Config.Friend.ListReadHardLimit),
		Sweeps:   repo,
		Now:      time.Now,
	}
}

// now 取时间源;Deps 由单测手工构造时 Now 可能为 nil。
func (d *Deps) now() time.Time {
	if d.Now == nil {
		return time.Now()
	}
	return d.Now()
}

type FriendLogic struct {
	deps *Deps
}

func NewFriendLogic(deps *Deps) *FriendLogic {
	return &FriendLogic{deps: deps}
}

// 阈值一律从 l.deps.SvcCtx.Config.Friend 读。以前读的是包级全局 config.AppConfig ——
// 那个全局变量已随 F1 的配置重写删掉:它让单测无法并行(改一个阈值会波及别的用例),
// 也把"配置什么时候被加载"变成了隐式约定(谁先跑谁看到零值)。

// withRequestBudget 给一次请求套上整请求业务预算(F2-13);父 ctx 更早到期时以父 ctx 为准。
//
// 预算非正时**不套**:那只可能是单测手工构造的 Config(Timeout 为 0),
// 套上去会得到一个立刻过期的 ctx,让每个用例都在第一次 I/O 上失败,现象是"存储全挂"
// 而不是"配置没填",极难排查。生产路径不会走到这里 —— config.Validate 拒收
// Timeout < MinRpcTimeoutMs。
func (l *FriendLogic) withRequestBudget(ctx context.Context) (context.Context, context.CancelFunc) {
	budget := l.deps.SvcCtx.Config.RequestBudget()
	if budget <= 0 {
		logx.WithContext(ctx).Errorf("[friend] RequestBudget=%v 非正,本次请求不设预算(检查 Timeout 配置)", budget)
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, budget)
}

// requireCaller 取本次请求的权威身份(D-9:客户端请求体里没有自身 player_id,只能从会话取)。
//
// ok=false 只有两种来路:①内部调用者直接拨了客户端方法(没有会话元数据,会话拦截器按
// "内部调用"放行);②会话里 player_id 为 0。两种都属于调用方用错了,不是玩家的错,也不是
// 服务故障 —— 所以调用方拿到的是 ErrInvalidParameter 而**不是** kPlayerNotFoundInSession
// (那是 fault 码,会把误用打成故障告警)。这里打一条 Error 日志,因为它必然是代码缺陷。
func (l *FriendLogic) requireCaller(ctx context.Context, method string) (uint64, bool) {
	me, ok := session.ClientPlayerID(ctx)
	if !ok {
		logx.WithContext(ctx).Errorf("[friend] %s 取不到会话身份:该方法只接受经 gate → 路由服转发的客户端请求(D-9)", method)
	}
	return me, ok
}

// storageFailure 把依赖故障统一定性成 in-band 的 ErrStorage,并打一条 Error 日志。
//
// 日志是**唯一**能看到原始 err 的地方:回给客户端的只有 tip 码与一句固定文案 ——
// 库名、SQL、连接串都不能出信封(AGENTS.md §11.3 敏感信息最少暴露)。
func (l *FriendLogic) storageFailure(ctx context.Context, method string, err error) *base.TipInfoMessage {
	logx.WithContext(ctx).Errorf("[friend] %s 依赖故障: %v", method, err)
	return tipErr(constants.ErrStorage, "storage unavailable")
}

// onlineStatuses 批量取在线状态。句柄缺失时返回 nil ——
// Go 里读 nil map 合法且取到零值,零值恰好等于"离线 + 无活跃记录"。
//
// 在线状态是**展示字段**:查不到就全当离线继续返回列表,不让好友列表整个失败 ——
// 共享 Redis 抖一下就看不到好友,比看到一份"全部灰着"的列表更糟。
// 降级日志与 metrics.ObserveOnlineLookup 在 SessionStore 的实现里打,这里不重复计(会双计)。
func (l *FriendLogic) onlineStatuses(ctx context.Context, playerIDs []uint64) map[uint64]data.OnlineStatus {
	if len(playerIDs) == 0 || l.deps.Sessions == nil {
		return nil
	}
	return l.deps.Sessions.FillOnlineStatus(ctx, playerIDs)
}

func (l *FriendLogic) AddFriend(ctx context.Context, req *pb.AddFriendRequest) (*pb.AddFriendResponse, error) {
	const method = "AddFriend"
	ctx, cancel := l.withRequestBudget(ctx)
	defer cancel()
	reject := func(tip *base.TipInfoMessage) (*pb.AddFriendResponse, error) {
		return &pb.AddFriendResponse{ErrorMessage: tip}, nil
	}

	me, ok := l.requireCaller(ctx, method)
	if !ok {
		return reject(tipErr(constants.ErrInvalidParameter, "missing session identity"))
	}
	target := req.GetTargetPlayerId()
	// target=0 必须显式拒:身份改从会话取之后 me 永远非 0,原来那条 `req.PlayerId == req.TargetPlayerId`
	// 不再顺带挡住"目标为 0"(以前 0==0 会命中自加好友分支)。放过去就会给 player_id=0 建好友边。
	if target == 0 {
		return reject(tipErr(constants.ErrInvalidParameter, "target_player_id required"))
	}
	if me == target {
		return reject(tipErr(constants.ErrCannotAddSelf, "cannot add yourself"))
	}
	// 频控在一切副作用之前(§3.6):它挡的是"发一条撤一条"的高频刷 —— 那种刷法每条都合法,
	// MaxPendingRequests 这类"同时挂多少"的上限永远拦不住。fail-open,理由见 rate_quota.go。
	if !l.allowFriendRequest(ctx, me) {
		return reject(tipErr(constants.ErrRateLimited, "too many friend requests, retry later"))
	}

	// 一切上限与关系判定都在这一个事务里(§3.1)。这里刻意**不做**任何事务外预检:
	// 预检不持容量守卫锁,两个并发请求都能通过,却会让人误以为上限在本层生效。
	cfg := l.deps.SvcCtx.Config.Friend
	err := l.deps.Repo.AddFriendRequest(ctx, me, target, data.AddFriendLimits{
		MaxFriends:          cfg.MaxFriends,
		MaxPendingRequests:  cfg.MaxPendingRequests,  // 出站:我挂着的 pending
		MaxIncomingRequests: cfg.MaxIncomingRequests, // 入站:别人发给我的 pending
	})
	switch {
	case err == nil:
	case errors.Is(err, data.ErrBlocked):
		// 两个方向命中同一个码:告诉客户端"我拉黑了他"和"他拉黑了我"哪一种,等于
		// 把别人的黑名单泄露给申请人,对方会立刻知道自己被拉黑了。
		return reject(tipErr(constants.ErrBlocked, "cannot send friend request"))
	case errors.Is(err, data.ErrAlreadyFriends):
		return reject(tipErr(constants.ErrAlreadyFriends, "already friends"))
	case errors.Is(err, data.ErrRequestAlreadySent):
		return reject(tipErr(constants.ErrRequestAlreadySent, "request already sent"))
	case errors.Is(err, data.ErrTooManyPending):
		return reject(tipErr(constants.ErrTooManyPending, "too many pending friend requests"))
	case errors.Is(err, data.ErrTargetInboxFull):
		return reject(tipErr(constants.ErrTargetInboxFull, "target inbox full"))
	// AddFriend 的 me 是 Sender、target 是 Acceptor,所以 Sender→我的列表满、
	// Acceptor→对方列表满;AcceptFriend 的 me 是 Acceptor,同一对哨兵在那里的映射
	// **正好相反**(见下面 AcceptFriend 的 switch)。映射表的唯一事实源在
	// data/friend_repo.go:27-40,改这里先回去看那张表。
	case errors.Is(err, data.ErrSenderFriendsFull):
		return reject(tipErr(constants.ErrFriendListFull, "your friend list full"))
	case errors.Is(err, data.ErrAcceptorFriendsFull):
		return reject(tipErr(constants.ErrTargetFriendListFull, "target's friend list full"))
	default:
		return reject(l.storageFailure(ctx, method, err))
	}

	// 推送在提交之后、事务之外(§3.5),失败只打日志计指标,不影响本次 RPC 的结果:
	// 好友申请已经落库了,客户端拉 GetPendingRequests 一定能看到它。
	l.pushFriendEvent(ctx, target, pb.FriendEventReason_FRIEND_EVENT_REASON_REQUEST_RECEIVED, me)
	logx.WithContext(ctx).Infof("[friend] player %d 向 %d 发出好友申请", me, target)
	return &pb.AddFriendResponse{}, nil
}

func (l *FriendLogic) AcceptFriend(ctx context.Context, req *pb.AcceptFriendRequest) (*pb.AcceptFriendResponse, error) {
	const method = "AcceptFriend"
	ctx, cancel := l.withRequestBudget(ctx)
	defer cancel()
	reject := func(tip *base.TipInfoMessage) (*pb.AcceptFriendResponse, error) {
		return &pb.AcceptFriendResponse{ErrorMessage: tip}, nil
	}

	me, ok := l.requireCaller(ctx, method)
	if !ok {
		return reject(tipErr(constants.ErrInvalidParameter, "missing session identity"))
	}
	// from=0 显式拒(F2-12):放过去会拿 0 去查申请行,结果必然是"没有申请",
	// 客户端看到的是 ErrNoPendingRequest —— 一个把"请求体没填"伪装成"申请不存在"的码。
	from := req.GetFromPlayerId()
	if from == 0 {
		return reject(tipErr(constants.ErrInvalidParameter, "from_player_id required"))
	}
	if from == me {
		// 自己给自己的申请本来就不可能存在(AddFriend 挡了自加),显式拒是为了不让
		// 这种请求进事务白拿一次容量守卫锁。
		return reject(tipErr(constants.ErrInvalidParameter, "cannot accept your own request"))
	}

	// 参数顺序是 (发起者, 接受者):接受者永远是会话里的 me,不能让请求体指定 ——
	// 否则客户端可以替别人通过好友申请。
	err := l.deps.Repo.AcceptFriend(ctx, from, me, l.deps.SvcCtx.Config.Friend.MaxFriends)
	switch {
	case err == nil:
	case errors.Is(err, data.ErrBlocked):
		return reject(tipErr(constants.ErrBlocked, "cannot accept friend request"))
	case errors.Is(err, data.ErrRequestNotFound):
		return reject(tipErr(constants.ErrNoPendingRequest, "no pending friend request"))
	case errors.Is(err, data.ErrSenderFriendsFull):
		return reject(tipErr(constants.ErrTargetFriendListFull, "sender's friend list full"))
	case errors.Is(err, data.ErrAcceptorFriendsFull):
		return reject(tipErr(constants.ErrFriendListFull, "your friend list full"))
	default:
		return reject(l.storageFailure(ctx, method, err))
	}

	// 推给**原申请人**(from),不推给操作者本人:他自己刚点的按钮,响应里已经有结果了。
	l.pushFriendEvent(ctx, from, pb.FriendEventReason_FRIEND_EVENT_REASON_REQUEST_ACCEPTED, me)
	logx.WithContext(ctx).Infof("[friend] player %d 同意了 %d 的好友申请", me, from)
	return &pb.AcceptFriendResponse{}, nil
}

func (l *FriendLogic) RejectFriend(ctx context.Context, req *pb.RejectFriendRequest) (*pb.RejectFriendResponse, error) {
	const method = "RejectFriend"
	ctx, cancel := l.withRequestBudget(ctx)
	defer cancel()
	reject := func(tip *base.TipInfoMessage) (*pb.RejectFriendResponse, error) {
		return &pb.RejectFriendResponse{ErrorMessage: tip}, nil
	}

	me, ok := l.requireCaller(ctx, method)
	if !ok {
		return reject(tipErr(constants.ErrInvalidParameter, "missing session identity"))
	}
	from := req.GetFromPlayerId()
	if from == 0 {
		return reject(tipErr(constants.ErrInvalidParameter, "from_player_id required"))
	}
	if from == me {
		// 自己给自己的申请不可能存在(AddFriend 挡了自加)。显式拒是为了不让它落到 data 的
		// errInvalidPlayerPair —— 那条路径被刻意定性成 fault 码 ErrStorage(服务端缺陷的信号),
		// 客户端伪造一个 from=自己 的包就能刷出 rpc_inband_fault 告警。
		return reject(tipErr(constants.ErrInvalidParameter, "cannot reject your own request"))
	}

	err := l.deps.Repo.RejectFriend(ctx, from, me)
	switch {
	case err == nil:
	case errors.Is(err, data.ErrRequestNotFound):
		return reject(tipErr(constants.ErrNoPendingRequest, "no pending friend request"))
	default:
		return reject(l.storageFailure(ctx, method, err))
	}
	// 刻意**不推送**(照 A 仓):把"你被拒绝了"实时弹到申请人脸上只会制造社交尴尬,
	// 而且拒绝方会因此暴露自己"刚刚在线并处理了申请"。客户端下次拉列表自然发现申请没了。
	logx.WithContext(ctx).Infof("[friend] player %d 拒绝了 %d 的好友申请", me, from)
	return &pb.RejectFriendResponse{}, nil
}

func (l *FriendLogic) RemoveFriend(ctx context.Context, req *pb.RemoveFriendRequest) (*pb.RemoveFriendResponse, error) {
	const method = "RemoveFriend"
	ctx, cancel := l.withRequestBudget(ctx)
	defer cancel()
	reject := func(tip *base.TipInfoMessage) (*pb.RemoveFriendResponse, error) {
		return &pb.RemoveFriendResponse{ErrorMessage: tip}, nil
	}

	me, ok := l.requireCaller(ctx, method)
	if !ok {
		return reject(tipErr(constants.ErrInvalidParameter, "missing session identity"))
	}
	// target=0 显式拒(F2-12)。非 0 但不是好友时是**幂等成功**(不报错也不建容量行,
	// F2-15 由 repo 侧 fail-closed 保证):删一个已经不在列表里的人,结果就是"不在列表里"。
	target := req.GetTargetPlayerId()
	if target == 0 {
		return reject(tipErr(constants.ErrInvalidParameter, "target_player_id required"))
	}

	if err := l.deps.Repo.RemoveFriend(ctx, me, target); err != nil {
		return reject(l.storageFailure(ctx, method, err))
	}
	// 不推送:删好友是单方面动作,通知对方等于把"我把你删了"送到他屏幕上。
	logx.WithContext(ctx).Infof("[friend] player %d 删除好友 %d", me, target)
	return &pb.RemoveFriendResponse{}, nil
}

func (l *FriendLogic) GetFriendList(ctx context.Context, req *pb.GetFriendListRequest) (*pb.GetFriendListResponse, error) {
	const method = "GetFriendList"
	ctx, cancel := l.withRequestBudget(ctx)
	defer cancel()

	me, ok := l.requireCaller(ctx, method)
	if !ok {
		return &pb.GetFriendListResponse{
			ErrorMessage: tipErr(constants.ErrInvalidParameter, "missing session identity"),
		}, nil
	}

	friends, err := l.deps.Repo.GetFriendList(ctx, me)
	if err != nil {
		return &pb.GetFriendListResponse{ErrorMessage: l.storageFailure(ctx, method, err)}, nil
	}

	friendIDs := make([]uint64, 0, len(friends))
	for _, f := range friends {
		friendIDs = append(friendIDs, f.FriendPlayerID)
	}
	statuses := l.onlineStatuses(ctx, friendIDs)

	pbFriends := make([]*pb.FriendEntry, 0, len(friends))
	for _, f := range friends {
		entry := &pb.FriendEntry{
			FriendPlayerId: f.FriendPlayerID,
			SinceMs:        f.SinceMs,
		}
		// is_online 与 last_active_ms **都**来自共享库的会话(§3.9):friend 表里只有
		// player_id / friend_player_id / since_ms 三列,没有"最后活跃"这种显示态
		// (显示态不进持久化记录,AGENTS.md §11.6)。查不到就留零值 = 离线 + 未知活跃时间。
		// 也正因如此,data.FriendEntry.LastActiveMs 不进缓存 —— 每次请求都从会话现取。
		if st, hit := statuses[f.FriendPlayerID]; hit {
			entry.IsOnline = st.Online
			entry.LastActiveMs = st.LastActiveMs
		}
		pbFriends = append(pbFriends, entry)
	}
	return &pb.GetFriendListResponse{Friends: pbFriends}, nil
}

func (l *FriendLogic) GetPendingRequests(ctx context.Context, req *pb.GetPendingRequestsRequest) (*pb.GetPendingRequestsResponse, error) {
	const method = "GetPendingRequests"
	ctx, cancel := l.withRequestBudget(ctx)
	defer cancel()

	me, ok := l.requireCaller(ctx, method)
	if !ok {
		return &pb.GetPendingRequestsResponse{
			ErrorMessage: tipErr(constants.ErrInvalidParameter, "missing session identity"),
		}, nil
	}

	requests, err := l.deps.Repo.GetPendingRequests(ctx, me)
	if err != nil {
		return &pb.GetPendingRequestsResponse{ErrorMessage: l.storageFailure(ctx, method, err)}, nil
	}

	pbRequests := make([]*pb.FriendRequest, 0, len(requests))
	for _, r := range requests {
		pbRequests = append(pbRequests, &pb.FriendRequest{
			FromPlayerId:  r.FromPlayerID,
			ToPlayerId:    r.ToPlayerID,
			RequestTimeMs: r.RequestTimeMs,
			Status:        pb.FriendRequestStatus(r.Status),
		})
	}
	return &pb.GetPendingRequestsResponse{Requests: pbRequests}, nil
}

// Block 把某个玩家加入黑名单:拒掉两个方向的 pending 申请、删掉已有的好友边(§3.4)。
//
// 拉黑**不推送**任何事件:被拉黑者不该知道自己被拉黑了 —— 知道了就会换号骚扰,
// 而"悄悄失效"正是黑名单的价值所在。
func (l *FriendLogic) Block(ctx context.Context, req *pb.BlockRequest) (*pb.BlockResponse, error) {
	const method = "Block"
	ctx, cancel := l.withRequestBudget(ctx)
	defer cancel()
	reject := func(tip *base.TipInfoMessage) (*pb.BlockResponse, error) {
		return &pb.BlockResponse{ErrorMessage: tip}, nil
	}

	me, ok := l.requireCaller(ctx, method)
	if !ok {
		return reject(tipErr(constants.ErrInvalidParameter, "missing session identity"))
	}
	target := req.GetTargetPlayerId()
	// 拉黑自己走 ErrInvalidParameter 而不是 ErrCannotAddSelf:后者的文案是"不能添加自己为好友",
	// 弹在拉黑按钮上文不对题。也不能放过去 —— 那会把自己的好友边删掉。
	if target == 0 || target == me {
		return reject(tipErr(constants.ErrInvalidParameter, "target_player_id invalid"))
	}

	err := l.deps.Repo.Block(ctx, me, target, l.deps.SvcCtx.Config.Friend.MaxBlocks)
	switch {
	case err == nil:
	case errors.Is(err, data.ErrBlockListFull):
		return reject(tipErr(constants.ErrBlockListFull, "block list full"))
	default:
		return reject(l.storageFailure(ctx, method, err))
	}
	logx.WithContext(ctx).Infof("[friend] player %d 拉黑 %d", me, target)
	return &pb.BlockResponse{}, nil
}

// Unblock 解除拉黑。幂等:没拉黑过也回成功 —— 客户端重试、双击按钮都不该报错。
func (l *FriendLogic) Unblock(ctx context.Context, req *pb.UnblockRequest) (*pb.UnblockResponse, error) {
	const method = "Unblock"
	ctx, cancel := l.withRequestBudget(ctx)
	defer cancel()
	reject := func(tip *base.TipInfoMessage) (*pb.UnblockResponse, error) {
		return &pb.UnblockResponse{ErrorMessage: tip}, nil
	}

	me, ok := l.requireCaller(ctx, method)
	if !ok {
		return reject(tipErr(constants.ErrInvalidParameter, "missing session identity"))
	}
	target := req.GetTargetPlayerId()
	// target == me 与 RejectFriend 的 from == me 同理:data 的 Unblock 对 me==target 回
	// errInvalidPlayerPair(刻意的非哨兵),落到 default 就是 fault 码 ErrStorage ——
	// 客户端发一个 target=自己 的包就能按秒刷出 rpc_inband_fault 假告警。
	if target == 0 || target == me {
		return reject(tipErr(constants.ErrInvalidParameter, "target_player_id invalid"))
	}

	if err := l.deps.Repo.Unblock(ctx, me, target); err != nil {
		return reject(l.storageFailure(ctx, method, err))
	}
	// 解除拉黑**不会**自动恢复好友关系:拉黑时那条边已经删了,想加回来要重新走 AddFriend。
	logx.WithContext(ctx).Infof("[friend] player %d 解除拉黑 %d", me, target)
	return &pb.UnblockResponse{}, nil
}

// ListBlocks 返回调用者自己的黑名单。直接读 MySQL、不缓存(§3.4):
// 黑名单是低频读 + 写后必须立刻可见的小列表,缓存只会引入"刚拉黑却还没进列表"的窗口。
func (l *FriendLogic) ListBlocks(ctx context.Context, req *pb.ListBlocksRequest) (*pb.ListBlocksResponse, error) {
	const method = "ListBlocks"
	ctx, cancel := l.withRequestBudget(ctx)
	defer cancel()

	me, ok := l.requireCaller(ctx, method)
	if !ok {
		return &pb.ListBlocksResponse{
			ErrorMessage: tipErr(constants.ErrInvalidParameter, "missing session identity"),
		}, nil
	}

	blocks, err := l.deps.Repo.ListBlocks(ctx, me)
	if err != nil {
		return &pb.ListBlocksResponse{ErrorMessage: l.storageFailure(ctx, method, err)}, nil
	}

	pbBlocks := make([]*pb.BlockEntry, 0, len(blocks))
	for _, b := range blocks {
		pbBlocks = append(pbBlocks, &pb.BlockEntry{
			BlockedPlayerId: b.BlockedPlayerID,
			SinceMs:         b.SinceMs,
		})
	}
	// 黑名单里刻意不带在线状态:那会变成"查某人是否在线"的旁路(拉黑任意 id 即可探测)。
	return &pb.ListBlocksResponse{Blocks: pbBlocks}, nil
}

// RecommendFriends 在 recommend.go(策略链与限幅),NotifyFriendEvent 方向相反、
// 服务端不提供 C2S 语义(server 侧继承 Unimplemented,会话层白名单也不收它)。
//
// NotifyOnline / NotifyOffline 两个方法已删除:对应 rpc 与 message 都从 proto 里去掉了
// (在线状态改由共享库的 player:session:{id} 承载,契约 §4 / §3.9),留着编译不过。
