package svc

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"shared/authoritysql"
	"shared/drainproof"
	"shared/etcdwatch"
	"shared/safego"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"match/internal/authority"
	"match/internal/battleplace"
	"match/internal/battlescript"
	"match/internal/catalog"
	"match/internal/config"
	"match/internal/discovery"
	matchkafka "match/internal/kafka"
	"match/internal/metrics"
	"match/internal/poolowner"
	"match/internal/ranked"
	"match/internal/scope"
	"match/internal/spectatechat"
	"match/internal/tournament"

	chatpb "proto/chat"
	dspb "proto/data_service"
	friendpb "proto/friend"
	guildpb "proto/guild"
	mailpb "proto/mail"
	matchpb "proto/match"
	inboxpb "proto/player_inbox"

	"shared/clusterredis"
	"shared/contentguard"
	"shared/dsclient"
	"shared/generated/table"
	"shared/idsegment"
	"shared/kafkacmd"
	"shared/kafkautil"
	"shared/placement"
	"shared/playerredis"
	"shared/playersync"
	"shared/rolefence"
	"shared/serviceauth"
	"shared/tablebundle"
	"shared/tracing/kafkagotrace"

	redv9 "github.com/redis/go-redis/v9"
	"github.com/segmentio/kafka-go"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/zrpc"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// SceneNodeRpcPrefix / BattleNodeRpcPrefix / MatchNodeRpcPrefix 是 match 侧 watch 的发现前缀。
// 与 load_reporter 的 sceneNodePrefix 同写法:字符串字面量,不依赖生成的枚举名。
const (
	SceneNodeRpcPrefix   = "SceneNodeService.rpc/"
	BattleNodeRpcPrefix  = "BattleNodeService.rpc/"
	MatchNodeRpcPrefix   = "MatchNodeService.rpc/"
	PlayerInboxRpcPrefix = "PlayerInboxNodeService.rpc/"
)

// roleFenceCaller 是 match 读目录权威状态时的 ServiceCallAuth.caller。
const roleFenceCaller = "match"

type ServiceContext struct {
	lifecycleMu      sync.Mutex
	sourceMu         sync.Mutex
	sourceStops      []context.CancelFunc
	sourceWorkers    sync.WaitGroup
	sourceClosed     bool
	stopBackground   context.CancelFunc
	resourcesClosed  bool
	resourceCloseErr error
	ConsumerOwner    *kafkautil.BoundConsumerGroup
	WriterConfig     authoritysql.WriterConfig
	RootAdmission    *drainproof.Session
	RelayAdmission   *drainproof.Session
	RelayIngress     *drainproof.Session
	DrainRuntime     *drainproof.Runtime
	Config           config.Config
	Authority        *authority.Store
	// guild 调 MatchInternal 的签名(开局、取消、占用、终局读)。仅main显式装配，缺失时这些入口拒绝；不复用网络探测或其它服务秘密。
	GuildCallAuth       *serviceauth.Verifier
	GuildCallSecret     string
	// scene 调 MatchInternal.StartActivityBattle(瑞兽迎春单人活动开局)的独立调用方证明，与 guild 严格隔离。
	SceneCallAuth       *serviceauth.Verifier
	BattleOutcomeAuth   *serviceauth.Verifier
	BattleOutcomeSecret string
	// SpectateOpsAuth 校验运营工具调 SetSpectateFeatured 的调用证明。Spectate.OpsCallerSecret 为空时保持 nil,入口一律回 SPECTATE_FEATURED_REJECT_DENIED。
	SpectateOpsAuth *serviceauth.Verifier
	// 启动时装配，随后只读；调用方必须在取得SQL/P锁之前调用。
	RefreshBattleSettlement func(context.Context, *matchpb.MatchAuthorityTicketRecord) error
	resultPublisher         *kafka.Writer
	// GlobalGatherSlots 限制本副本同时备战的成局数(GlobalMatch.GatherSlots)。按池撮合先占槽再提交成局。
	// 到期终局恢复的翻页游标不挂在这里:它是 match.global.outcome.recover 循环的局部状态(logic/global_outcome_recover.go)。
	GlobalGatherSlots chan struct{}

	// Catalog 是启动时由 PlayMode / Dungeon / ZoneGroup 三张表编译的模板目录(热更时通过 liveCatalog 原子切换)。
	// 手工构造的单测 ServiceContext 可直接写 Catalog 或留 nil;业务读路径一律经 ActiveCatalog()。
	Catalog     *catalog.Catalog
	liveCatalog atomic.Pointer[catalog.Catalog]
	// BattleScripts 是启动时解析、按配表校验过的内置官方战斗剧本目录(热更时通过 liveBattleScripts 原子切换)。
	// 手工构造的单测 ServiceContext 可直接写 BattleScripts 或留 nil;业务读路径一律经 ActiveBattleScripts()。
	BattleScripts     *battlescript.Catalog
	liveBattleScripts atomic.Pointer[battlescript.Catalog]
	// Ranked 是启动时由 RankedRule / RankedPreset 等表编译校验的论剑排位目录(热更时经 liveRanked 原子切换)。
	Ranked     *ranked.Catalog
	liveRanked atomic.Pointer[ranked.Catalog]
	// Tournament 是启动时由 TournamentRule / TournamentReward 编译校验的全服赛季战队锦标赛目录(热更时经 liveTournament 原子切换)。
	Tournament       *tournament.Catalog
	liveTournament   atomic.Pointer[tournament.Catalog]
	TournamentCheers *tournament.SpectateCheerHub
	cheerHubOnce     sync.Once
	SpectateChat     *spectatechat.Hub
	spectateChatOnce sync.Once
	// ContentGuard 用于战队名称等 UGC 文本审核。生产由 match_service.go 装配(没配目标拒启);nil 只出现在单测里,那时不送检。
	ContentGuard contentguard.Checker
	// MailAdmin / MailSecret 只用于投递锦标赛赛季奖励的附件邮件(生死状退费 2026-11-26 起只由 scene 退,match 不发信)。
	// 没装配时带附件的奖励保持可领取;非 dev / test 档开着锦标赛却没装配,启动即拒(requireTournamentMail)。
	MailAdmin  mailpb.MailAdminClient
	MailSecret string
	// FriendClient / ChatClient / GuildClient 用于江湖恩怨·天墉生死状一期跨服务只读校验与系统播报(可选装配)。
	FriendClient friendpb.ClientPlayerFriendClient
	ChatClient   chatpb.ClientPlayerChatClient
	GuildClient  guildpb.GuildServiceClient
	// JianghuStore 允许逻辑单测注入内存存储替身；生产为 nil 时默认走 Authority(*authority.Store)。
	JianghuStore authority.JianghuStore
	// TableBundle 负责表包启动校验与运行期两阶段热更(未设 MMORPG_TABLE_BUNDLE_ROOT 时仅执行启动校验)。
	TableBundle *tablebundle.Manager
	// MatchNodes 是 match 实例自身注册(MatchNodeService)的 etcd 镜像。按池撮合用它的实例 UUID 作成员表,
	// 以最高随机权重哈希决定每个池由哪个副本负责(internal/poolowner)。
	MatchNodes *discovery.NodeWatcher
	// poolMember 是本实例 MatchNodeService 注册的 NodeUuid,注册成功后由 match_service 设置;未设置时不负责任何池。
	poolMember atomic.Pointer[string]
	// Placement 记录本副本在各 battle 节点上的在途备战数与临时熔断,用于机房内选点(internal/battleplace)。
	Placement *battleplace.Picker

	// 双存储(设计文档 cross-zone-matchmaking.md D2)。两者都只放协调态,权威都在 SQL:
	//   - MatchRedis:match 私有集群的句柄(shared/clusterredis,生产是 Redis Cluster),只放 spectate:* 观战索引、档位与配额。
	//     守卫只放行 spectate: 开头、带 hash tag 的键;结果未知的命令不由客户端重发,原样报给调用方。
	//     没写 MatchRedis.Addrs 时落到 Redis 段那一个实例(本地单库形态),见 NewMatchRedis。
	//   - PlayerRedis:玩家域句柄(shared/playerredis,生产可以是 Redis Cluster):跨运行时契约 key(player:{id}:location /
	//     player:session:{id} / battle:lock:{id}),写者是 scene_manager / player_locator / C++ scene,match 只读;
	//     player:{id}:sync_digest 由组队 bump。键一律带 {player_id},发送前经守卫核对。
	MatchRedis  redv9.UniversalClient
	PlayerRedis redv9.UniversalClient

	// SpectateLobby 是观战列表读的 1 秒进程内缓存:三把全服观战索引拆成 16 桶之后,列表读要跨桶合并(spectate_lobby_cache.go)。
	// 手工构造的单测 ServiceContext 不填时为 nil,列表每次回源。
	SpectateLobby *SpectateLobbyCache

	// SyncDigest 组队名册提交后给受影响玩家的落地摘要 bump TEAM / TEAM_INVITE
	// (docs/design/landing-recovery-sync-digest.md §3.3),由 team.NewService 取用。
	// 句柄必须是 PlayerRedis:player:{id}:sync_digest 是 C++ scene 读的玩家域键,MatchRedis C++ 读不到。
	// 指标 label service 固定为 "team":排队 / 切磋 / 观战不 bump 摘要。
	// 手工构造的 ServiceContext(单测)不填时为 nil,bump 全部是无操作。
	SyncDigest *playersync.Bumper

	Etcd *clientv3.Client
	// Kafka 只用于挑战(切磋)S2C 经 gate-{id} 的推送,同步 RequireOne
	// (与 friend/guild 的 servicecontext 同口径)。
	Kafka              *kafka.Writer
	GateCommandBuilder kafkautil.GateCommandBuilder

	// BattleIDGen 发 battle_id / challenge_id / team_id(biz_tag = battle,见 battle_id.go)。
	// 同进程第二次注册 TeamNodeService 不影响发号(team-system.md §C.3)。
	BattleIDGen     BattleIDGenerator
	battleIDSegment *idsegment.Client

	// SceneNodes / BattleNodes 是 etcd list-watch 内存镜像:
	//   - SceneNodes:按 player:{id}:location 里的 (zone_id, node_id) 定位
	//     scene 节点 gRPC 地址(PrepareBattle / CancelBattlePrepare 对端);
	//   - BattleNodes:全局池,机房内按负载选点(internal/battleplace)后写进成局原件。只收 stable 节点;
	//   - CanaryBattleNodes:同一前缀下只收 canary 节点(发布轨道设计 §2.6)。全员都是金丝雀会话的新成局先在这里、
	//     在按 stable 节点选定的机房里选点,选不到退回 BattleNodes;按节点号确认已有对局的实例时两份都查。
	//     探测目标、机房集合与节点计数只看 BattleNodes,所以 canary battle 节点要和 stable 的部署在同一批机房。
	SceneNodes        *discovery.NodeWatcher
	BattleNodes       *discovery.NodeWatcher
	CanaryBattleNodes *discovery.NodeWatcher

	// BattleInstanceLoss 是原执行进程失效的提前作废(docs/design/battle-instance-loss-early-abort.md §4):补签问不到原实例时
	// 把这一局记成可疑,取证循环按实例两次确认原进程已不在之后写 ABORT。由 logic.BindDeadlineAbortEvidence 装配(与截止作废共用
	// 一个复核记录器),取证循环在 stable 与 canary 副本上都启动(logic.StartBattleInstanceLossWatcher)。nil = 没装配,补签不记可疑。
	BattleInstanceLoss BattleInstanceLossWatcher

	// DataServiceClient 是 data_service 的 gRPC 客户端:领号段、查玩家昵称与展示资料。
	// DataServiceRpc 没配任何目标时为 nil,号段装配随之拒绝启动。连接是 NonBlock 的:data_service 没起
	// 不阻塞 match 起服,调用时才返回 Unavailable。
	DataServiceClient dspb.DataServiceClient

	// MergedZones 是合服映射的进程内副本,经 data_service 读,Authority 按它解析同区(authority/merged_zones.go)。
	// 启动时读不到拒绝启动;StopAndReport 在关库之前停它的定时重读。
	MergedZones *placement.MergedZoneCache

	stopOnce            sync.Once
	rankPublisherCancel context.CancelFunc
	rankPublisherDone   chan struct{}
	// resultLagProbeDone 在结果消费组 lag 探针 goroutine 退出(已关掉自己的 broker 连接)时关闭;探针跟着 stopBackground 的 ctx 退出,
	// StopAndReport 停后台之后等它。canary 副本不起结果消费者,保持 nil。
	resultLagProbeDone <-chan struct{}
	// partyTeam 整队排队里 logic 反过来问 / 通知 team 域的端口(见 PartyTeamPort),
	// 装配时由 SetPartyTeam 注入。用 atomic:matcher 循环比组队服务先启动。
	partyTeam atomic.Pointer[partyTeamHolder]
}

// PartyTeamPort 整队排队里 logic 需要 team 域回答 / 接收的两件事,
// 由 team.Service 实现、match_service.go 装配时注入。接口定义在 svc:logic 与 team 互不 import
// (team 依赖 proto/team 生成物),方法签名只用内建类型;原因码见 constants.PartyEnd*。
type PartyTeamPort interface {
	// CheckParty 整队成局前的复核:队伍记录还在、成员集合与 members 相等、全员在线。
	// 返回 (constants.PartyEndNone, 0, nil) 表示仍然有效;否则是失效原因与相关成员(可为 0)。
	// err 非 nil = 读不到,调用方不做失效判定。
	CheckParty(ctx context.Context, teamId uint64, members []uint64) (reason uint32, culprit uint64, err error)
	// PartyEnded 一支排队中的整队已被撤出(不是成功开战)之后的通知:team 给 members 推当前视图与原因。
	// 实现方自己异步执行,不阻塞调用方;members 是这一单的名单。
	PartyEnded(teamId uint64, members []uint64, reason uint32, culprit uint64)
}

type partyTeamHolder struct{ port PartyTeamPort }

// SetPartyTeam 注入补位端口(match_service.go 装配、测试);传 nil 清空。
func (s *ServiceContext) SetPartyTeam(port PartyTeamPort) {
	if port == nil {
		s.partyTeam.Store(nil)
		return
	}
	s.partyTeam.Store(&partyTeamHolder{port: port})
}

// SetPoolMember 记录本实例 MatchNodeService 注册的 NodeUuid(注册成功后调用;注销前传空串)。
func (s *ServiceContext) SetPoolMember(nodeUUID string) {
	s.poolMember.Store(&nodeUUID)
}

// PoolMember 返回本实例在池归属成员表里的身份;尚未注册返回空串,此时不负责任何池。
func (s *ServiceContext) PoolMember() string {
	if p := s.poolMember.Load(); p != nil {
		return *p
	}
	return ""
}

// OwnsWork 报告本实例是否负责 key 这份后台工作:撮合段(poolowner.Segment.Key)、成局恢复片、整队维护片、投榜片
// (authority/work_owner.go)。全部用同一条规则:在 etcd 注册的 match 实例上做最高随机权重哈希(internal/poolowner)。
// 它只管分工,正确性在各自的 SQL 提交点。成员表为空(watch 还没同步,或 etcd 一直起不来)时全部副本都做,保活优先;
// 成员表非空、但本实例不在其中(尚未注册或已注销)时什么都不负责,交给表里的副本。
func (s *ServiceContext) OwnsWork(key string) bool {
	members := s.PoolMembers()
	if len(members) == 0 {
		return true
	}
	return poolowner.Owns(s.PoolMember(), key, members)
}

// PoolMembers 返回当前注册在 etcd 的全部 match 实例 UUID(按池归属的成员表)。未装配 watcher 时为空。
func (s *ServiceContext) PoolMembers() []string {
	if s.MatchNodes == nil {
		return nil
	}
	nodes := s.MatchNodes.Snapshot()
	out := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if node.NodeUuid != "" {
			out = append(out, node.NodeUuid)
		}
	}
	return out
}

// PartyTeam 取补位端口;未注入返回 nil。端口缺失时 matcher 不让整队成组(核不了就本轮搁置)。
// 生产装配在 matcher 循环启动之前注入(match_service.go)。
func (s *ServiceContext) PartyTeam() PartyTeamPort {
	if h := s.partyTeam.Load(); h != nil {
		return h.port
	}
	return nil
}

func NewServiceContext(c config.Config) *ServiceContext {
	writer, err := authoritysql.LoadWriterConfig(os.Getenv)
	if err != nil {
		panic(err)
	}
	return NewServiceContextWithAdmission(c, writer, drainproof.New(), drainproof.New(), true)
}

// globalBackground 为 false(发布轨道 canary,docs/design/release-track-gray-canary.md §2.5)时不起排名发布与终局发件箱;
// 其余装配(收件箱节点镜像、权威库、合服映射等数据面)照旧。match_service 按 releasetrack.FromEnv 传入,NewServiceContext 固定 stable。
func NewServiceContextWithAdmission(c config.Config, writer authoritysql.WriterConfig, root, relay *drainproof.Session, globalBackground bool) *ServiceContext {
	if root == nil || relay == nil || root == relay {
		panic("match real role admission gates not wired")
	}
	// keepalive:节点镜像的 watch 不会因为半开连接静默停住(etcdwatch 包注释)。
	etcdCli, err := clientv3.New(etcdwatch.WithKeepAlive(clientv3.Config{
		Endpoints:   c.Etcd.Hosts,
		DialTimeout: 5 * time.Second,
	}))
	if err != nil {
		panic("failed to create etcd client: " + err.Error())
	}

	matchRds, err := NewMatchRedis(c)
	if err != nil {
		_ = etcdCli.Close()
		panic(err)
	}
	playerRedisConf := c.EffectivePlayerRedis()
	playerRedis, err := playerredis.New(playerRedisConf)
	if err != nil {
		_ = matchRds.Close()
		_ = etcdCli.Close()
		panic(fmt.Errorf("invalid PlayerRedis config: %w", err))
	}
	logx.Infof("[match] player redis %s", playerRedisConf.Describe())
	sc := &ServiceContext{
		WriterConfig: writer, RootAdmission: root, RelayAdmission: relay, RelayIngress: drainproof.New(),
		Config:             c,
		MatchRedis:         matchRds,
		PlayerRedis:        playerRedis,
		SpectateLobby:      NewSpectateLobbyCache(time.Now),
		SyncDigest:         playersync.NewBumper(playersync.NewGoRedisEvaler(playerRedis), "team"),
		Etcd:               etcdCli,
		GateCommandBuilder: matchkafka.NewGateCommandBuilder(),
	}

	kafkaWriteTimeout := time.Duration(c.KafkaWriteTimeoutSeconds) * time.Second
	if kafkaWriteTimeout <= 0 {
		kafkaWriteTimeout = 5 * time.Second
	}
	sc.Kafka = &kafka.Writer{
		Completion: func(_ []kafka.Message, err error) { sc.HoldRelay(err) },
		Addr:       kafka.TCP(c.Kafka.Brokers...),
		// 控制面命令 topic(gate-cmd_gN)必须按 node_id % P 落到**指定分区**:
		// 消费端 assign 的就是那一个分区,落错分区 = 目标 gate 永远收不到,
		// 而 Kafka 一个错都不报(docs/design/control-plane-topic-partitioning-20260908.md)。
		// kafka-go 的 Writer 在写入路径上忽略 Message.Partition、只问 Balancer。
		//
		// 其它 topic 仍走 Hash 按消息 Key(player_id)选分区(项目不变量:
		// kafka key = 业务实体 ID,同一玩家的推送必须有序)。
		Balancer:               &kafkacmd.CommandPartitionBalancer{Fallback: &kafka.Hash{}},
		AllowAutoTopicCreation: true,
		BatchTimeout:           10 * time.Millisecond,
		MaxAttempts:            1,
		WriteTimeout:           kafkaWriteTimeout,
		// kafka.Writer 的 RequiredAcks 零值是 RequireNone(fire-and-forget),
		// 显式 RequireOne:挑战弹窗丢了对端只能干等 60s 超时。
		RequiredAcks: kafka.RequireOne,
		Async:        false,
	}

	sc.SceneNodes = discovery.NewNodeWatcher("scene", SceneNodeRpcPrefix,
		metrics.SetDiscoveredNodes,
		func(entry discovery.NodeEntry) { discovery.RemoveEndpointConn(entry.Endpoint) })
	// battle、match、player_inbox 的镜像只收 stable(发布轨道设计 §2.5,见 NodeWatcher.StableOnly):
	// 放战斗与调收件箱都是服务间调用;匹配池的 HRW 成员表若算进 canary,落到它名下的池会停摆。scene 按 zone 选,不过滤。
	sc.BattleNodes = discovery.NewNodeWatcher("battle", BattleNodeRpcPrefix,
		metrics.SetDiscoveredNodes,
		func(entry discovery.NodeEntry) { discovery.RemoveEndpointConn(entry.Endpoint) }).StableOnly()
	sc.CanaryBattleNodes = discovery.NewNodeWatcher("battle-canary", BattleNodeRpcPrefix,
		metrics.SetDiscoveredNodes,
		func(entry discovery.NodeEntry) { discovery.RemoveEndpointConn(entry.Endpoint) }).CanaryOnly()
	sc.MatchNodes = discovery.NewNodeWatcher("match", MatchNodeRpcPrefix, metrics.SetDiscoveredNodes, nil).StableOnly()
	sc.Placement = battleplace.New()

	// 模板目录与内置官方剧本经 tablebundle 两阶段加载与校验:
	// 启动失败直接拒启,运行期热更先在候选 TableSet 上编译校验再原子替换 liveCatalog / liveBattleScripts / Authority.Catalog。
	tableMgr, err := tablebundle.LoadAtStartup(tablebundle.Options{
		ServiceName:           "match",
		ImageTableDir:         c.TableDir,
		UseBinary:             false,
		StrictStartupValidate: true,
		PrepareHook: func(_ string, ts *table.TableSet) (func(), error) {
			nextCatalog, err := catalog.BuildFromTableSet(ts)
			if err != nil {
				return nil, fmt.Errorf("build match template catalog: %w", err)
			}
			nextScripts, err := battlescript.LoadFromTableSet(ts)
			if err != nil {
				return nil, fmt.Errorf("build official battle scripts: %w", err)
			}
			nextRanked, err := ranked.BuildFromTableSet(ts)
			if err != nil {
				return nil, fmt.Errorf("build ranked play catalog: %w", err)
			}
			nextTournament, err := tournament.BuildFromTableSet(ts)
			if err != nil {
				return nil, fmt.Errorf("build season tournament catalog: %w", err)
			}
			return func() {
				sc.liveCatalog.Store(nextCatalog)
				sc.liveBattleScripts.Store(nextScripts)
				sc.liveRanked.Store(nextRanked)
				sc.liveTournament.Store(nextTournament)
				if sc.Authority != nil {
					sc.Authority.SwapCatalog(nextCatalog)
					sc.Authority.SwapRankedCatalog(nextRanked)
					sc.Authority.SwapTournamentCatalog(nextTournament)
				}
			}, nil
		},
	})
	if err != nil {
		sc.Stop()
		panic(fmt.Errorf("failed to load match tables (TableDir=%s): %w", c.TableDir, err))
	}
	sc.TableBundle = tableMgr
	matchCatalog := sc.ActiveCatalog()
	sc.Catalog = matchCatalog
	sc.BattleScripts = sc.ActiveBattleScripts()
	sc.Ranked = sc.ActiveRanked()
	sc.Tournament = sc.ActiveTournament()

	sc.DataServiceClient = NewDataServiceClient(c.DataServiceRpc)
	sc.FriendClient = newOptionalFriendClient(c.FriendRpc)
	sc.ChatClient = newOptionalChatClient(c.ChatRpc)
	sc.GuildClient = newOptionalGuildClient(c.GuildRpc)
	sc.MailAdmin = newOptionalMailAdminClient(c.MailRpc)
	if envName := strings.TrimSpace(c.MailSecretEnv); envName != "" {
		sc.MailSecret = strings.TrimSpace(os.Getenv(envName))
	} else {
		sc.MailSecret = strings.TrimSpace(os.Getenv("MAIL_PRODUCER_SECRET_MATCH"))
	}
	if err := requireTournamentMail(c.Mode, sc.Tournament.Rule().GetEnabled(), sc.MailAdmin != nil, sc.MailSecret); err != nil {
		sc.Stop()
		panic(err)
	}
	battleIDSegment, err := NewBattleIDSegment(c.IdSegment, sc.DataServiceClient)
	if err != nil {
		sc.Stop()
		panic(fmt.Errorf("battle id segment: %w", err))
	}
	sc.battleIDSegment = battleIDSegment
	sc.BattleIDGen = segmentBattleIDs{segment: battleIDSegment}
	// 观战运营入口与凭证密钥(battle-spectate-tiered-fanout.md §15.6、§15.7)。密钥长度已由 Config.Validate 在加载期校验。
	if c.Spectate.OpsEntryEnabled() {
		opsAuth, err := serviceauth.New(map[string]string{config.SpectateOpsCaller: c.Spectate.OpsCallerSecret})
		if err != nil {
			sc.Stop()
			panic(fmt.Errorf("invalid Spectate.OpsCallerSecret: %w", err))
		}
		sc.SpectateOpsAuth = opsAuth
	}
	if c.Spectate.RelayEnabled && !c.Spectate.RelayUsable() {
		logx.Error("[match] Spectate.RelayEnabled is on but Spectate.TicketSecret is empty (dev/test only); spectator relay stays off and full direct seats return 16021")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := authority.OpenWithPermission(ctx, c.GlobalMatch, authoritysql.Writer(writer))
	if err != nil {
		sc.Stop()
		panic(fmt.Errorf("failed to initialize global match authority: %w", err))
	}
	sc.Authority = store
	store.SwapRankedCatalog(sc.ActiveRanked())
	store.SwapTournamentCatalog(sc.ActiveTournament())
	if sc.DataServiceClient != nil {
		dsClient := sc.DataServiceClient
		store.BindRankedNameLookup(func(lookupCtx context.Context, playerID uint64) string {
			if playerID == 0 {
				return ""
			}
			callCtx, cancelLookup := context.WithTimeout(lookupCtx, 2*time.Second)
			defer cancelLookup()
			resp, lookupErr := dsClient.BatchGetPlayerName(callCtx, &dspb.BatchGetPlayerNameRequest{PlayerIds: []uint64{playerID}})
			if lookupErr != nil || resp == nil {
				return ""
			}
			return resp.GetNames()[playerID]
		})
	}
	schedule := scope.Schedule{
		GroupAfterMs:  uint64(c.GlobalMatch.ScopeGroupAfterSeconds) * 1000,
		GlobalAfterMs: uint64(c.GlobalMatch.ScopeGlobalAfterSeconds) * 1000,
	}
	if err := store.ConfigureMatchCatalog(matchCatalog, schedule); err != nil {
		sc.Stop()
		panic(fmt.Errorf("invalid global match scope rules: %w", err))
	}
	// Authority 已就绪后再启动后台热更轮询,避免首拍回调看到 Authority==nil。
	sc.TableBundle.Start()
	// 成局恢复、整队维护、闲置队伍到期、投榜按片分工,与撮合池同一条归属规则(OwnsWork)。
	store.BindWorkOwner(sc.OwnsWork)
	// 角色归属锁本库的护栏副本,自愈、事务外的读和入场校验走 data_service(docs/design/horizontal-scale-logical-db.md §3)。
	if err := sc.configureRoleFence(ctx, store); err != nil {
		sc.Stop()
		panic(fmt.Errorf("global match role fence unavailable: %w", err))
	}
	// 分不清「没有合服」和「读不到」的实例不能对外服务,同区判定全靠这份映射。
	if err := sc.loadMergedZones(context.Background(), store); err != nil {
		sc.Stop()
		panic(fmt.Errorf("match: cannot read merged zones from data_service, refusing to start: %w", err))
	}
	if err := sc.RunRelay(func() error { return store.EnsureGlobalPolicy(ctx, c) }); err != nil {
		sc.Stop()
		panic(err)
	}
	secret := os.Getenv(c.GlobalMatch.RankSecretEnv)
	if len(secret) < 32 || c.GlobalMatch.RankEndpoint == "" {
		sc.Stop()
		panic("global rating rank publishing requires RANK_PRODUCER_SECRET_MATCH and RankEndpoint")
	}
	// 终局发件箱的批量与并发(GlobalMatch.OutcomeOutbox,2026-10-30):越界拒启、不悄悄夹紧;逐行结局计入 metrics。
	outboxOptions, err := authority.OutcomeOutboxOptions{
		ResultBatch:  c.GlobalMatch.OutcomeOutbox.ResultBatch,
		FirstBatch:   c.GlobalMatch.OutcomeOutbox.FirstBatch,
		WaitingBatch: c.GlobalMatch.OutcomeOutbox.WaitingBatch,
		Concurrency:  c.GlobalMatch.OutcomeOutbox.Concurrency,
		Observe:      metrics.ObserveOutcomeOutboxDelivery,
	}.Normalized()
	if err != nil {
		sc.Stop()
		panic(fmt.Errorf("invalid GlobalMatch.OutcomeOutbox parameters: %w", err))
	}
	pubctx, pubcancel := context.WithCancel(context.Background())
	sc.rankPublisherCancel = pubcancel
	sc.rankPublisherDone = make(chan struct{})
	outcomeSecret := os.Getenv("MMORPG_MATCH_OUTCOME_SECRET_BATTLE")
	outcomeVerifier, err := serviceauth.New(map[string]string{"battle": outcomeSecret})
	inboxSecret := os.Getenv("MMORPG_PLAYER_INBOX_SECRET_MATCH")
	if err != nil || len(inboxSecret) < 32 {
		pubcancel()
		close(sc.rankPublisherDone)
		sc.Stop()
		panic("global outcomes require dedicated Battle/Match inbox signing secrets")
	}
	sc.BattleOutcomeAuth, sc.BattleOutcomeSecret = outcomeVerifier, outcomeSecret
	inboxNodes := discovery.NewNodeWatcher("player_inbox", PlayerInboxRpcPrefix, nil, func(entry discovery.NodeEntry) { discovery.RemoveEndpointConn(entry.Endpoint) }).StableOnly()
	sc.resultPublisher = &kafka.Writer{Completion: func(_ []kafka.Message, err error) { sc.HoldRelay(err) }, Addr: kafka.TCP(c.Kafka.Brokers...), Balancer: &kafka.Hash{}, RequiredAcks: kafka.RequireAll, Async: false, MaxAttempts: 1, WriteTimeout: 2 * time.Second, AllowAutoTopicCreation: false}
	resolveInbox := func(ctx context.Context) (inboxpb.PlayerInboxClient, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		node, ok := inboxNodes.PickRandom()
		if !ok {
			return nil, fmt.Errorf("收件箱尚无正式发现端点")
		}
		conn, err := discovery.DialEndpoint(node.Endpoint)
		if err != nil {
			return nil, err
		}
		return inboxpb.NewPlayerInboxClient(conn), nil
	}
	// ctx 里有 trace 就写进消息头,guild 等结果消费者接上同一条 trace(docs/design/distributed-tracing.md §6.2);
	// 后台 outbox 中继发的没有 trace,不带头。
	publishResult := func(ctx context.Context, battle uint64, raw []byte) error {
		return sc.resultPublisher.WriteMessages(ctx, kafka.Message{Topic: c.ResultTopic, Key: []byte(fmt.Sprint(battle)), Value: raw,
			Headers: kafkagotrace.Headers(ctx)})
	}
	sc.RefreshBattleSettlement = func(ctx context.Context, ticket *matchpb.MatchAuthorityTicketRecord) error {
		return store.RefreshBattleSettlementReceipt(ctx, ticket, resolveInbox, inboxSecret)
	}
	var publishers sync.WaitGroup
	runRankPublisher := func() {
		defer publishers.Done()
		authority.RunRankPublisher(pubctx, store, c.GlobalMatch.RankEndpoint, secret, relay)
	}
	// 投递循环本身(节拍、每轮 recover、活性)在 outcome_outbox_loop.go。
	runOutcomeOutbox := func() {
		defer publishers.Done()
		runOutcomeOutboxLoop(pubctx, relay, c.GlobalMatch.OutcomeOutboxInterval(), func(call context.Context) error {
			return store.PublishBattleOutcomes(call, resolveInbox, inboxSecret, publishResult, outboxOptions)
		})
	}
	// 收件箱节点镜像是数据面(JoinQueue、GetQueueStatus 经 RefreshBattleSettlement 要找收件箱),canary 也起。
	// 排名发布与终局发件箱是全局作用域的后台,只在 stable 起(发布轨道设计 §2.5),canary 写下的终局由 stable 投出。
	// 按实际起的数量 Add:rankPublisherDone 要在它们都退出后关闭,少算会让 StopAndReport 等到超时变成 HOLD。
	publishers.Add(1)
	go func() { defer publishers.Done(); inboxNodes.Run(pubctx, etcdCli) }()
	if globalBackground {
		publishers.Add(2)
		go runRankPublisher()
		go runOutcomeOutbox()
	}
	go func() { publishers.Wait(); close(sc.rankPublisherDone) }()

	return sc
}

// configureRoleFence 给 Authority 接上目录客户端。调用方密钥缺失就拒绝启动:没有它读不到权威状态,角色副本永远自愈不了。
func (sc *ServiceContext) configureRoleFence(ctx context.Context, store *authority.Store) error {
	secret := os.Getenv(rolefence.SecretEnv(roleFenceCaller))
	states, err := rolefence.NewDirectoryReader(sc.DataServiceClient, roleFenceCaller, secret)
	if err != nil {
		return err
	}
	admissions, err := rolefence.NewAdmissionVerifier(sc.DataServiceClient, roleFenceCaller, secret)
	if err != nil {
		return err
	}
	return store.ConfigureRoleFence(ctx, states, admissions)
}

// NewDataServiceClient 按配置建 data_service 客户端;配置里没有任何目标(etcd key /
// 直连端点 / target)返回 nil,由 team 按 fail-closed 处理。
//
// 强制 NonBlock:本函数在 NewServiceContext 里调用,早于节点注册与 s.Start()。阻塞建连
// 会把 data_service 从"组队的请求期依赖"升级成"整个 match 的启动依赖"(排队 / 切磋 / 观战
// 一起不可用),所以 NonBlock=false 记错误日志后改为 true,而不是照配置阻塞。
// 拆成独立函数是为了不起 etcd 也能测试。
func NewDataServiceClient(c zrpc.RpcClientConf) dspb.DataServiceClient {
	if c.Etcd.Key == "" && len(c.Endpoints) == 0 && c.Target == "" {
		logx.Error("[match] DataServiceRpc not configured: id segments and player display lookups are unavailable")
		return nil
	}
	if !c.NonBlock {
		logx.Error("[match] DataServiceRpc.NonBlock=false is not allowed; forced to true (blocking connection would prevent match startup when data_service is unavailable)")
		c.NonBlock = true
	}
	conn := zrpc.MustNewClient(dsclient.Conf(c))
	return dspb.NewDataServiceClient(conn.Conn())
}

func newOptionalFriendClient(c zrpc.RpcClientConf) friendpb.ClientPlayerFriendClient {
	if c.Etcd.Key == "" && len(c.Endpoints) == 0 && c.Target == "" {
		return nil
	}
	c.NonBlock = true
	return friendpb.NewClientPlayerFriendClient(zrpc.MustNewClient(c).Conn())
}

func newOptionalChatClient(c zrpc.RpcClientConf) chatpb.ClientPlayerChatClient {
	if c.Etcd.Key == "" && len(c.Endpoints) == 0 && c.Target == "" {
		return nil
	}
	c.NonBlock = true
	return chatpb.NewClientPlayerChatClient(zrpc.MustNewClient(c).Conn())
}

func newOptionalGuildClient(c zrpc.RpcClientConf) guildpb.GuildServiceClient {
	if c.Etcd.Key == "" && len(c.Endpoints) == 0 && c.Target == "" {
		return nil
	}
	c.NonBlock = true
	return guildpb.NewGuildServiceClient(zrpc.MustNewClient(c).Conn())
}

func newOptionalMailAdminClient(c zrpc.RpcClientConf) mailpb.MailAdminClient {
	if c.Etcd.Key == "" && len(c.Endpoints) == 0 && c.Target == "" {
		return nil
	}
	c.NonBlock = true
	return mailpb.NewMailAdminClient(zrpc.MustNewClient(c).Conn())
}

// requireTournamentMail:锦标赛奖励的附件只经邮件发(群仙问道赛设计 V1 第七节,数据覆写审计 r2 第 4 轮)。
// dev / test 以外的档位开着锦标赛却没配 MailRpc 或发件密钥(MAIL_PRODUCER_SECRET_MATCH,不足 32 字节同样算没配),
// 领奖只会一直回内部错误,启动即拒;dev / test 放行,领奖照样不把带附件的奖励落成已领取。
func requireTournamentMail(mode string, tournamentEnabled, mailRpcConfigured bool, mailSecret string) error {
	if mode == "dev" || mode == "test" || !tournamentEnabled {
		return nil
	}
	if !mailRpcConfigured || len(mailSecret) < 32 {
		return errors.New("tournament rewards need MailRpc and a mail producer secret (MailSecretEnv, default MAIL_PRODUCER_SECRET_MATCH, at least 32 bytes); refusing to start")
	}
	return nil
}

// NewMatchRedis 按配置建立 match 私有集群的句柄。没写 MatchRedis.Addrs 时私有 key 落到 Redis 段那一个实例
// (config.EffectiveMatchRedis)。拆成独立函数是为了不依赖 etcd 就能测试回落逻辑。
// 构造时不探活(go-redis 用到才连):观战索引只是协调数据,它连不上不该拦住排队与成局,命令各自报错。
func NewMatchRedis(c config.Config) (redv9.UniversalClient, error) {
	conf := c.EffectiveMatchRedis()
	client, err := clusterredis.New(conf, clusterredis.MatchPrivateKeys)
	if err != nil {
		return nil, fmt.Errorf("invalid MatchRedis config: %w", err)
	}
	logx.Infof("[match] match redis %s", conf.Describe())
	return client, nil
}

func (sc *ServiceContext) BindDrainRuntime(runtime *drainproof.Runtime) error {
	sc.lifecycleMu.Lock()
	defer sc.lifecycleMu.Unlock()
	if runtime == nil || sc.DrainRuntime != nil || runtime.Seed("SOURCE") == nil || runtime.Seed("RELAY") == nil {
		return drainproof.ErrHold
	}
	if err := runtime.BindBoot(sc.WriterConfig.InstanceID(), sc.WriterConfig.WriterKey()); err != nil {
		return err
	}
	sc.DrainRuntime = runtime
	return nil
}

// BindBackgroundStop 由 main 交付已有后台上下文；控制面使用独立 context，不会取消自己的关闭请求。
func (sc *ServiceContext) BindBackgroundStop(stop context.CancelFunc) error {
	sc.lifecycleMu.Lock()
	defer sc.lifecycleMu.Unlock()
	if stop == nil || sc.stopBackground != nil || sc.resourcesClosed {
		return drainproof.ErrHold
	}
	sc.stopBackground = stop
	return nil
}
func (sc *ServiceContext) CloseSource(ctx context.Context) error {
	sc.sourceMu.Lock()
	defer sc.sourceMu.Unlock()
	if sc.RootAdmission == nil {
		return drainproof.ErrHold
	}
	sc.RootAdmission.CloseAdmission()
	sc.sourceClosed = true
	for _, stop := range sc.sourceStops {
		stop()
	}
	done := make(chan struct{})
	go func() { sc.sourceWorkers.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		sc.RootAdmission.Hold()
		return errors.Join(drainproof.ErrHold, ctx.Err())
	}
	return sc.RootAdmission.Wait(ctx)
}
func (sc *ServiceContext) StopAndReport(ctx context.Context) error {
	sc.lifecycleMu.Lock()
	defer sc.lifecycleMu.Unlock()
	if sc.resourcesClosed {
		return sc.resourceCloseErr
	}
	if err := sc.CloseSource(ctx); err != nil {
		return err
	}
	if sc.RelayAdmission == nil {
		return drainproof.ErrHold
	}
	if sc.RelayIngress != nil {
		sc.RelayIngress.CloseAdmission()
		if err := sc.RelayIngress.Wait(ctx); err != nil {
			return err
		}
	}
	if sc.stopBackground != nil {
		sc.stopBackground()
	}
	if sc.rankPublisherCancel != nil {
		sc.rankPublisherCancel()
	}
	if sc.ConsumerOwner != nil {
		raw, err := sc.ConsumerOwner.CloseAndReport(ctx)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(raw)
		if err = sc.RelayAdmission.MergeSettled(digest[:]); err != nil {
			return err
		}
	}
	if sc.rankPublisherDone != nil {
		select {
		case <-sc.rankPublisherDone:
		case <-ctx.Done():
			sc.RelayAdmission.Hold()
			return errors.Join(drainproof.ErrHold, ctx.Err())
		}
	}
	// 结果消费组 lag 探针随上面 stopBackground 取消的 ctx 退出;等它真正退出、关掉自己的 broker 连接。
	// 探针只读,但这里等不到就说不清后台是否都停了,与上面的等待同样按 HOLD 报(sarama 每次调用 5s 超时,一轮有界)。
	if sc.resultLagProbeDone != nil {
		select {
		case <-sc.resultLagProbeDone:
		case <-ctx.Done():
			sc.RelayAdmission.Hold()
			return errors.Join(drainproof.ErrHold, ctx.Err())
		}
	}
	// 循环、已发出的 RPC、队伍异步通知均持有同一 RELAY；停止新轮次后再等所有实际后继。
	if err := sc.RelayAdmission.WaitIdle(ctx); err != nil {
		return err
	}
	// 发号的调用方都已停下。领段结局不明时排空不了,与 guild、trade 同口径。
	if sc.battleIDSegment != nil {
		digest, err := sc.battleIDSegment.DrainAndReport(ctx)
		if err != nil {
			sc.RelayAdmission.Hold()
			return err
		}
		if err = sc.RelayAdmission.MergeSettled(digest); err != nil {
			return err
		}
	}
	if sc.resultPublisher != nil {
		if err := sc.resultPublisher.Close(); err != nil {
			sc.RelayAdmission.Hold()
			return err
		}
	}
	if sc.Kafka != nil {
		if err := sc.Kafka.Close(); err != nil {
			sc.RelayAdmission.Hold()
			return err
		}
	}
	sc.RelayAdmission.CloseAdmission()
	if err := sc.RelayAdmission.Wait(ctx); err != nil {
		return err
	}
	// 最后一个用同区判定的调用方已经退出。必须早于封写者资格和关库,否则最后一拍重读会报假故障。
	sc.MergedZones.Stop()
	sc.TableBundle.Stop()
	sc.WriterConfig.Seal()
	var result error
	if sc.Authority != nil {
		result = errors.Join(result, sc.Authority.DB.Close())
	}
	if sc.PlayerRedis != nil {
		result = errors.Join(result, sc.PlayerRedis.Close())
	}
	if sc.MatchRedis != nil {
		result = errors.Join(result, sc.MatchRedis.Close())
	}
	if sc.Etcd != nil {
		result = errors.Join(result, sc.Etcd.Close())
	}
	sc.resourcesClosed = true
	sc.resourceCloseErr = result
	if result != nil {
		sc.RelayAdmission.Hold()
	}
	return result
}

// ActiveCatalog 返回当前生效的匹配模板目录(运行期热更经 liveCatalog 原子切换;未走 LoadAtStartup 的单测回落到 Catalog)。
func (sc *ServiceContext) ActiveCatalog() *catalog.Catalog {
	if sc == nil {
		return nil
	}
	if c := sc.liveCatalog.Load(); c != nil {
		return c
	}
	return sc.Catalog
}

// ActiveBattleScripts 返回当前生效的内置官方战斗剧本目录(同 ActiveCatalog 口径)。
func (sc *ServiceContext) ActiveBattleScripts() *battlescript.Catalog {
	if sc == nil {
		return nil
	}
	if b := sc.liveBattleScripts.Load(); b != nil {
		return b
	}
	return sc.BattleScripts
}

// ActiveRanked 返回当前生效的论剑排位目录(运行期热更经 liveRanked 原子切换;单测未装配时回落默认目录)。
func (sc *ServiceContext) ActiveRanked() *ranked.Catalog {
	if sc == nil {
		return ranked.DefaultFallback()
	}
	if r := sc.liveRanked.Load(); r != nil {
		return r
	}
	if sc.Ranked != nil {
		return sc.Ranked
	}
	return ranked.DefaultFallback()
}

// ActiveTournament 返回当前生效的锦标赛配置目录(运行期热更经 liveTournament 原子切换;单测未装配时回落默认目录)。
func (sc *ServiceContext) ActiveTournament() *tournament.Catalog {
	if sc == nil {
		return tournament.DefaultFallback()
	}
	if t := sc.liveTournament.Load(); t != nil {
		return t
	}
	if sc.Tournament != nil {
		return sc.Tournament
	}
	return tournament.DefaultFallback()
}

// ActiveTournamentCheers 返回锦标赛观战预设喝彩聚合器(首次访问懒初始化,并发安全)。
func (sc *ServiceContext) ActiveTournamentCheers() *tournament.SpectateCheerHub {
	if sc == nil {
		return tournament.NewSpectateCheerHub()
	}
	sc.cheerHubOnce.Do(func() {
		if sc.TournamentCheers == nil {
			sc.TournamentCheers = tournament.NewSpectateCheerHub()
		}
	})
	return sc.TournamentCheers
}

// ActiveSpectateChat 返回局外人观战交流中枢(优先复用 MatchRedis 跨副本共享,单测未配 Redis 自动退化内存实现)。
func (sc *ServiceContext) ActiveSpectateChat() *spectatechat.Hub {
	if sc == nil {
		return spectatechat.NewHub(nil)
	}
	sc.spectateChatOnce.Do(func() {
		if sc.SpectateChat == nil {
			sc.SpectateChat = spectatechat.NewHub(sc.MatchRedis)
		}
	})
	return sc.SpectateChat
}

// ValidateTables 是 match 的 --validate-tables 预检钩子:与生产运行期热更使用完全相同的目录与剧本编译校验。
func ValidateTables(_ string, ts *table.TableSet) (func(), error) {
	if _, err := catalog.BuildFromTableSet(ts); err != nil {
		return nil, fmt.Errorf("build match template catalog: %w", err)
	}
	if _, err := battlescript.LoadFromTableSet(ts); err != nil {
		return nil, fmt.Errorf("build official battle scripts: %w", err)
	}
	if _, err := ranked.BuildFromTableSet(ts); err != nil {
		return nil, fmt.Errorf("build ranked play catalog: %w", err)
	}
	if _, err := tournament.BuildFromTableSet(ts); err != nil {
		return nil, fmt.Errorf("build season tournament catalog: %w", err)
	}
	return nil, nil
}

// Stop 是带时限的 StopAndReport(排空号段、flush Kafka 在途批次)。幂等,正常退出与启动失败路径可安全共用。
func (sc *ServiceContext) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := sc.StopAndReport(ctx); err != nil {
		logx.Errorf("match shutdown HOLD: %v", err)
	}
}

// StartSourceLoop 拥有新撮合发起者；prepared 阶段不执行新轮次，P1 真正停止并 join。
//
// 每一轮各自 recover(2026-11-12,与 StartRelayLoop 同一写法):一轮 panic 打 HOLD(结局未知),只丢这一轮,循环接着跑。
// 原来 panic 一路抛到最外层的 safego.Go 才被兜住,循环就此退出,进程却还活着、还在成员表里:它名下的撮合段从此没人撮合,
// 而且没有任何信号。兜住的 panic 计 safego_panic_total{point},告警 GoBackgroundPanicRecovered 按点位报
// (deploy/k8s/go-background-alerts.yaml)。撮合的内存状态只用于调度,提交决策都在 SQL 提交点,丢一轮不影响正确性。
//
// 活性(metrics.LoopLive 里 point 那一条;缺口清单第 25 条第六批,docs/design/assetop-redelivery-sharding-20261110.md §14.10,
// 口径在 shared/looplive 的包注释):起来时写一次;每一轮正常返回(成败都算)就写这一刻,panic 的一轮不写;入口已关(停机排空中)
// 有意跳过的一拍也写,循环活着、只是不开新一轮;ctx 结束时删掉序列。入口报出别的错误时循环退出、序列留着:这是意料之外的退出,
// 停摆告警 MatchBackgroundLoopStalled 报出来。告警按 300 秒判,所以间隔加一轮预算要远小于它(现有循环最长 1 分钟一拍、一轮 30 秒)。
func (sc *ServiceContext) StartSourceLoop(parent context.Context, point string, interval time.Duration, round func(context.Context)) {
	if interval <= 0 || round == nil {
		sc.RootAdmission.Hold()
		return
	}
	sc.sourceMu.Lock()
	defer sc.sourceMu.Unlock()
	if sc.sourceClosed {
		return
	}
	ctx, stop := context.WithCancel(parent)
	sc.sourceStops = append(sc.sourceStops, stop)
	sc.sourceWorkers.Add(1)
	safego.Go(point, func() {
		defer sc.sourceWorkers.Done()
		defer stop()
		defer func() {
			if recovered := recover(); recovered != nil {
				sc.RootAdmission.Hold()
				panic(recovered)
			}
		}()
		live := metrics.LoopLive.For(point)
		live.Mark(time.Now())
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				live.Stop()
				return
			case <-ticker.C:
			}
			finish, err := sc.RootAdmission.Enter()
			if err != nil {
				if errors.Is(err, drainproof.ErrClosed) {
					live.Mark(time.Now())
					continue
				}
				sc.RootAdmission.Hold()
				return
			}
			if !live.Round(point, func() { defer finish(); round(context.WithoutCancel(ctx)) }) {
				sc.RootAdmission.Hold()
			}
		}
	})
}

// LaunchRelay 在派生 goroutine 之前接纳；调用方的 RPC 结束不影响已接纳工作。
// fn 必须保留原有有界预算，未知 I/O 由返回值或 HoldRelay 明确报告。
func (sc *ServiceContext) LaunchRelay(point string, fn func() error) bool {
	if sc.RelayAdmission == nil {
		return false
	}
	finish, err := sc.RelayAdmission.Enter()
	if err != nil {
		return false
	}
	safego.Go(point, func() {
		defer finish()
		defer func() {
			if recovered := recover(); recovered != nil {
				sc.RelayAdmission.Hold()
				panic(recovered)
			}
		}()
		sc.HoldRelay(fn())
	})
	return true
}

// RunRelay 用于已有 owner 负责 join 的同步一轮，不能替代 reader/轮次自身的真实停止。
func (sc *ServiceContext) RunRelay(fn func() error) error {
	if sc.RelayAdmission == nil {
		return drainproof.ErrHold
	}
	finish, err := sc.RelayAdmission.Enter()
	if err != nil {
		return err
	}
	defer finish()
	defer func() {
		if recovered := recover(); recovered != nil {
			sc.RelayAdmission.Hold()
			panic(recovered)
		}
	}()
	err = fn()
	sc.HoldRelay(err)
	return err
}

// HoldRelay 把失败记成排空 HOLD。闸门冻结不算:搬库窗口里这笔确定没写,持久待办留给下一轮。
func (sc *ServiceContext) HoldRelay(err error) {
	if err != nil && sc.RelayAdmission != nil && !authoritysql.IsWriterFrozen(err) {
		sc.RelayAdmission.Hold()
	}
}

// LaunchRelayTask 供已有 void 工作体接纳；业务错误必须在原观察点 HoldRelay，panic 不会当作完成。
func (sc *ServiceContext) LaunchRelayTask(point string, fn func()) bool {
	return sc.LaunchRelay(point, func() error { fn(); return nil })
}

// StartRelayLoop 拥有计时器及整个循环的生命周期；每轮保留原有超时，停止只阻止新轮。
//
// 活性同 StartSourceLoop(metrics.LoopLive 里 point 那一条):起来时写一次,每一轮正常返回就写,panic 的一轮不写,ctx 结束时删掉序列;
// 间隔加一轮预算同样要远小于停摆告警的 300 秒。
func (sc *ServiceContext) StartRelayLoop(ctx context.Context, point string, interval time.Duration, round func(context.Context)) {
	if interval <= 0 {
		sc.HoldRelay(drainproof.ErrHold)
		return
	}
	sc.LaunchRelayTask(point, func() {
		live := metrics.LoopLive.For(point)
		live.Mark(time.Now())
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				live.Stop()
				return
			case <-ticker.C:
			}
			// 让已接受的一轮用其自身有界预算结束，不能把主循环取消当作 SQL 终局。
			if !live.Round(point, func() { round(context.WithoutCancel(ctx)) }) {
				sc.HoldRelay(drainproof.ErrHold)
			}
		}
	})
}

// BindConsumerOwner 只登记本服务真实消费组，不能在维护时换成另一个代次 owner。
func (sc *ServiceContext) BindConsumerOwner(owner *kafkautil.BoundConsumerGroup) error {
	sc.lifecycleMu.Lock()
	defer sc.lifecycleMu.Unlock()
	if owner == nil || sc.ConsumerOwner != nil {
		return drainproof.ErrHold
	}
	sc.ConsumerOwner = owner
	return nil
}

// BindResultLagProbe 登记结果消费组 lag 探针的退出信号(kafka.StartResultConsumer 起探针 goroutine 之前调用),只能登记一次。
// 探针只读、不进排空的在途集合,但关停时同样要等它退出,不能从 join 里漏掉(同 data_service 的探针)。
func (sc *ServiceContext) BindResultLagProbe(done <-chan struct{}) error {
	sc.lifecycleMu.Lock()
	defer sc.lifecycleMu.Unlock()
	if done == nil || sc.resultLagProbeDone != nil || sc.resourcesClosed {
		return drainproof.ErrHold
	}
	sc.resultLagProbeDone = done
	return nil
}
