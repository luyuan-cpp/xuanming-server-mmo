package scenenode

import (
	"context"
	"errors"
	"fmt"

	smpb "proto/scene_manager"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"
)

// SceneNodeRpcPrefix 是 scene 节点在 etcd 的注册前缀。与 C++ EtcdManager 拼的
// 路径、match 的 svc.SceneNodeRpcPrefix 必须一字不差。
const SceneNodeRpcPrefix = "SceneNodeService.rpc/"

// locationKeyFormat 是玩家位置键的格式串。
//
// **这不是仓内第 7 份字面量,而是 Go 侧 shared 的收口点**:同一串跨运行时契约
// 目前另有 6 份实现,改格式必须 7 处同改(改错的表现是静默定位不到人):
//
//	go/login/internal/logic/clientplayerlogin/player_class_backfill.go:57
//	                                                          backfillPlayerClassScript 的 KEYS[4]
//	go/match/internal/playercontract/playercontract.go:50     LocationKey
//	go/scene_manager/internal/logic/changesceneutil.go:18-20  getPlayerLocationKey(写者)
//	tools/merge_zone/scene_hot_state.go:51                    playerLocationKeyFmt
//	cpp/libs/services/scene/player/system/player_team.cpp:40  kPlayerLocationKeyFmt
//	cpp/nodes/battle/logic/battle_room_manager.cpp:1680       内联 GET
//
// login 那处不是顺带引用:它把这个键当作 Lua CAS 脚本的第 4 个 KEY,用来判断
// 「角色是否仍在场」,格式改错会把在场角色当成离线档回写存档。
//
// 三个 Go 服务(login / match / scene_manager)本可以改成引用本包,但它们不在本批
// 的文件归属里;后续批次把它们转调过来之后,Go 侧就只剩本处 + C++ 两份。
// 同族键 player:{id}:owner_epoch / player:{id}:handoff 的契约在 shared/ownerepoch。
const locationKeyFormat = "player:%d:location"

// LocationKey 返回 scene_manager 维护的玩家位置权威键(值为 PlayerLocation protobuf)。
func LocationKey(playerID uint64) string {
	return fmt.Sprintf(locationKeyFormat, playerID)
}

// LocationReader 读一个位置键的原始字节。键不存在必须返回 (nil, nil) —— 让
// 「玩家不在线」与「Redis 故障」在接口层就分开,调用方不必认各家客户端的哨兵值。
//
// 单独抽接口的原因:guild 用 go-redis,match / scene_manager 用 go-zero redis,
// 两者的"键不存在"表达方式不同(redis.Nil vs 空串);测试还要塞假数据。
type LocationReader interface {
	GetLocation(ctx context.Context, key string) ([]byte, error)
}

// GoRedisReader 用 go-redis 客户端实现 LocationReader。
//
// guild 直接复用 svcCtx.PlayerLocatorRedisClient:本地它与 SharedRedis 是同一个
// 实例,K8s 部署时必须指向 SharedRedis(位置键的写者 scene_manager 写在那儿),
// 指错库的表现是所有玩家都"不在线"。
type GoRedisReader struct {
	Client *redis.Client
}

// GetLocation 读键;键不存在返回 (nil, nil),其余错误原样返回(不吞、不降级)。
func (r GoRedisReader) GetLocation(ctx context.Context, key string) ([]byte, error) {
	if r.Client == nil {
		return nil, errors.New("scenenode: GoRedisReader.Client 未设置")
	}
	raw, err := r.Client.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return raw, nil
}

var (
	// ErrNotOnline:位置键不存在。玩家没上线,或还没进过场景。
	ErrNotOnline = errors.New("scenenode: player location absent")
	// ErrNodeUnknown:位置记录里的节点在镜像里找不到,或同身份多条注册被拒选,
	// 或镜像还没完成首次全量同步。共同点是「不知道该发给谁」。
	ErrNodeUnknown = errors.New("scenenode: scene node not registered or ambiguous")
	// ErrNodeAmbiguous:同一 (zone, node) 身份在镜像里有多条注册,任何一条都不能
	// 被安全选中。**包着 ErrNodeUnknown**,所以 errors.Is(err, ErrNodeUnknown) 与
	// IsNoHolder 的语义都不变,调用方不必跟着改;单独立一个哨兵只为把指标与日志
	// 分开 —— 这一档是部署/租约事故(两个进程抢同一个身份),不是普通的节点掉线。
	ErrNodeAmbiguous = fmt.Errorf("%w: 节点身份歧义", ErrNodeUnknown)
	// ErrAwaitingPlacement:跨 zone 交接已放行、目标 zone 还没落点
	// (node_id 为空且 owner_epoch != 0,见 proto/scene_manager/storage.proto:24-26
	// 与 scene_manager enterscenelogic.go 的 awaitingPlacement)。
	//
	// 此刻**没有任何节点持有该玩家**:源 scene 收到重定向应答后已销毁实体,目标
	// 节点还没被派到。所以这既不是"不在线"(人还在,票据在飞),也不是"节点未注册"
	// (根本没写节点号,不是发现层的问题)。调用方应当得到「稍后重试」,而不是把
	// 它当成故障告警,也绝不能据此认为操作失败。
	//
	// 对照 match 的同一处判断:go/match/internal/team/service.go:444 把它折成
	// 「成员未准备好」拒绝开战 —— 同样是重试语义,只是那边的调用方是玩家。
	ErrAwaitingPlacement = errors.New("scenenode: player awaiting cross-zone placement")
)

// IsNoHolder 报告 err 是否属于「当前没有节点持有这个玩家」这一族:不在线、
// 等待跨 zone 落点、节点未注册/歧义。三者的正确处置完全一样 —— 这次调用发不出去,
// 稍后重试;都不代表业务失败。
//
// 资产通道的 Caller 用它把这三种折成本地合成的 NOT_HERE 结局(04-asset-channel.md
// §4.19 caller.go 第 1 步)。单独给一个谓词,是为了以后再加同族哨兵时调用方不用改。
func IsNoHolder(err error) bool {
	return errors.Is(err, ErrNotOnline) ||
		errors.Is(err, ErrAwaitingPlacement) ||
		errors.Is(err, ErrNodeUnknown)
}

// Target 是一次定位的结果:玩家在哪、对应节点的地址与客户端。
type Target struct {
	Location *smpb.PlayerLocation
	Endpoint string
	Client   smpb.SceneNodeGrpcClient
}

// Locator 把「玩家 id」解析成「可以直接发 RPC 的 scene 节点客户端」。
//
// 装配契约(缺一即返回错误,不做隐式默认 —— 定位错节点的代价是资产操作发给
// 不持有该玩家的进程):
//   - Reader、Watcher 必须设置;
//   - Dial 与 Conns 至少设一个,Dial 优先(测试用它指向 bufconn);
//   - Metrics 可为 nil。
//
// 无自身可变状态,装配后可并发调用;字段不得在运行中改写。
type Locator struct {
	Reader  LocationReader
	Watcher *Watcher
	// Dial 为 nil 时用 Conns 拨号。给测试与自定义传输留的接缝。
	Dial    func(endpoint string) (smpb.SceneNodeGrpcClient, error)
	Conns   *ConnCache
	Metrics *Metrics
}

// Resolve 定位玩家当前所在的 scene 节点。
//
// 返回的错误分三档,调用方用 errors.Is / IsNoHolder 区分:
//   - ErrNotOnline / ErrAwaitingPlacement / ErrNodeUnknown:没人持有该玩家,重试;
//   - 其它错误(Redis 故障、反序列化失败、拨号失败):故障,按重试处理但要告警。
//
// 反序列化失败刻意**不**折成 ErrNotOnline:位置键写坏了是数据面事故,把它当成
// "玩家不在线"会让资产操作静悄悄地永远不落地。
//
// ctx 只约束这一次 Redis 往返;拨号是惰性的,不产生 I/O。
func (l *Locator) Resolve(ctx context.Context, playerID uint64) (Target, error) {
	if l.Reader == nil || l.Watcher == nil {
		l.Metrics.ObserveResolve(ResolveError)
		return Target{}, errors.New("scenenode: Locator 未装配 Reader / Watcher")
	}

	raw, err := l.Reader.GetLocation(ctx, LocationKey(playerID))
	if err != nil {
		l.Metrics.ObserveResolve(ResolveError)
		return Target{}, fmt.Errorf("scenenode: 读玩家位置失败 player=%d: %w", playerID, err)
	}
	// 键不存在(raw == nil)与空值(读到零长字节)都算不在线:位置记录是
	// scene_manager 整体写的 protobuf,空值只可能是残留或写坏的半成品。
	if len(raw) == 0 {
		l.Metrics.ObserveResolve(ResolveNotOnline)
		return Target{}, fmt.Errorf("%w: player=%d", ErrNotOnline, playerID)
	}

	loc := &smpb.PlayerLocation{}
	if err := proto.Unmarshal(raw, loc); err != nil {
		l.Metrics.ObserveResolve(ResolveError)
		return Target{}, fmt.Errorf("scenenode: 玩家位置反序列化失败 player=%d: %w", playerID, err)
	}

	if loc.GetNodeId() == "" {
		// scene_manager 只在跨 zone 交接放行时写空 node_id(placePlayerLocation)。
		// 带 epoch 的是那条正常的"等待落点";不带 epoch 的是更早的旧记录或写了一半,
		// 同样没有持有者 —— 按 fail-closed 归到"不知道发给谁",两者调用方都重试,
		// 只是指标口径分开,便于区分"传送在途"与"数据面异常"。
		if loc.GetOwnerEpoch() != 0 {
			l.Metrics.ObserveResolve(ResolveAwaitingPlacement)
			return Target{}, fmt.Errorf("%w: player=%d zone=%d epoch=%d",
				ErrAwaitingPlacement, playerID, loc.GetZoneId(), loc.GetOwnerEpoch())
		}
		l.Metrics.ObserveResolve(ResolveNodeUnknown)
		return Target{}, fmt.Errorf("%w: player=%d 位置记录无持有节点且无 owner_epoch", ErrNodeUnknown, playerID)
	}

	if !l.Watcher.Synced() {
		// 镜像还没建起来,查什么都是"未注册"。明确说出真实原因,别让运维把
		// 启动窗口里的正常现象当成节点掉线。
		l.Metrics.ObserveResolve(ResolveMirrorUnsynced)
		return Target{}, fmt.Errorf("%w: 节点镜像尚未完成首次全量同步", ErrNodeUnknown)
	}

	endpoint, err := l.Watcher.EndpointOf(loc.GetZoneId(), loc.GetNodeId())
	if err != nil {
		// 歧义(脑裂)与"查无此节点"(掉线)分开计数:两者的运维处置不同。
		// 错误仍按 §4.18 第 3 步的原样式包 ErrNodeUnknown,所以调用方侧
		// errors.Is / IsNoHolder 的行为一个字都没变。
		if errors.Is(err, ErrNodeAmbiguous) {
			l.Metrics.ObserveResolve(ResolveNodeAmbiguous)
		} else {
			l.Metrics.ObserveResolve(ResolveNodeUnknown)
		}
		return Target{}, fmt.Errorf("%w: %v", ErrNodeUnknown, err)
	}

	client, err := l.dial(endpoint)
	if err != nil {
		l.Metrics.ObserveResolve(ResolveError)
		return Target{}, fmt.Errorf("scenenode: 拨号失败 player=%d endpoint=%s: %w", playerID, endpoint, err)
	}

	l.Metrics.ObserveResolve(ResolveFound)
	return Target{Location: loc, Endpoint: endpoint, Client: client}, nil
}

func (l *Locator) dial(endpoint string) (smpb.SceneNodeGrpcClient, error) {
	if l.Dial != nil {
		return l.Dial(endpoint)
	}
	if l.Conns == nil {
		return nil, errors.New("scenenode: Locator 既没有 Dial 也没有 Conns")
	}
	conn, err := l.Conns.Dial(endpoint)
	if err != nil {
		return nil, err
	}
	return smpb.NewSceneNodeGrpcClient(conn), nil
}
