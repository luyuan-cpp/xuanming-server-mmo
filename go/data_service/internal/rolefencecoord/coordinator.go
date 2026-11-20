// Package rolefencecoord 是角色护栏协调者对参与者的 RPC 一侧,实现 store.FenceCoordinator。
// 规格见 docs/design/horizontal-scale-logical-db.md §3.5。
package rolefencecoord

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"data_service/internal/config"
	"data_service/internal/metrics"
	rfpb "proto/role_fence"
	"shared/grpckeepalive"
	"shared/rolefence"
	"shared/safego"
	"shared/serviceauth"

	"github.com/zeromicro/go-zero/core/conf"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/zrpc"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

const (
	// prepareTimeout 是一轮 Prepare 的上限。要给调用方的 2.5 秒留出前后两个目录事务;
	// 参与者那边等行锁最多 1 秒(会话 innodb_lock_wait_timeout=1)。超时算不可达,用同一 listing 重放。
	prepareTimeout = 1500 * time.Millisecond
	// dryRunTimeout 是一轮试探的上限。它排在 Prepare 前面,用的是同一份 2.5 秒,所以比 Prepare 短。
	// 超时算不可达,什么都没写,下一次重放再试。
	dryRunTimeout = 800 * time.Millisecond
	// pushTimeout 是一次推送的上限。丢了由副本下一次访问自愈。
	pushTimeout = 2 * time.Second
	// purgeTimeout 是一轮清数据的上限。清理跑在删角收尾里(前台删角或后台 ResumeRemovals),超时算没清完,下一轮整组再调。
	purgeTimeout = 2 * time.Second
)

// errPushRetry 表示参与者这一次没写成,带内回的 retry。
var errPushRetry = errors.New("participant answered retry")

type participant struct {
	name   string
	client rfpb.RoleFenceParticipantClient
}

// Coordinator 并行调全部参与者。并发安全,字段构造后不再改。
type Coordinator struct {
	participants []participant
	secret       string
	closers      []zrpc.Client
	pushCtx      context.Context
	cancelPushes context.CancelFunc
	pushes       sync.WaitGroup
}

// New 按配置连全部参与者。连接是非阻塞的:参与者没起来不影响 data_service 启动,调用时算不可达。
func New(cfg config.RoleFenceConfig, secret string) (*Coordinator, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if _, err := serviceauth.New(map[string]string{rolefence.CoordinatorCaller: secret}); err != nil {
		return nil, fmt.Errorf("%s: %w", rolefence.CoordinatorSecretEnv, err)
	}
	c := newCoordinator(secret)
	for _, p := range cfg.Participants {
		client, err := dial(p)
		if err != nil {
			return nil, errors.Join(fmt.Errorf("participant %s: %w", p.Name, err), c.Close())
		}
		c.closers = append(c.closers, client)
		c.participants = append(c.participants, participant{name: p.Name, client: rfpb.NewRoleFenceParticipantClient(client.Conn())})
	}
	metrics.PrimeRoleFence(rolefence.Participants)
	return c, nil
}

// LocalParticipant 是同进程的参与者,调用不走网络。data_service 自己的流水库有未结束的 GM 回收时也不能过户、删角。
// 名字不进 rolefence.Participants:那张表是要配地址的远端服务。
const LocalParticipant = "data_service_recall"

// localClient 把参与者的服务端实现包成客户端接口。签名照常验,进程内外走同一条路。
type localClient struct {
	server rfpb.RoleFenceParticipantServer
}

func (l localClient) PrepareRoleFreeze(ctx context.Context, in *rfpb.PrepareRoleFreezeRequest, _ ...grpc.CallOption) (*rfpb.PrepareRoleFreezeResponse, error) {
	return l.server.PrepareRoleFreeze(ctx, in)
}

func (l localClient) ApplyRoleFence(ctx context.Context, in *rfpb.ApplyRoleFenceRequest, _ ...grpc.CallOption) (*rfpb.ApplyRoleFenceResponse, error) {
	return l.server.ApplyRoleFence(ctx, in)
}

func (l localClient) PurgeRemovedRole(ctx context.Context, in *rfpb.PurgeRemovedRoleRequest, _ ...grpc.CallOption) (*rfpb.PurgeRemovedRoleResponse, error) {
	return l.server.PurgeRemovedRole(ctx, in)
}

// AddLocalParticipant 接上同进程的参与者。只在交给目录之前调,之后字段不再改。
func (c *Coordinator) AddLocalParticipant(server rfpb.RoleFenceParticipantServer) {
	c.participants = append(c.participants, participant{name: LocalParticipant, client: localClient{server: server}})
	metrics.PrimeRoleFence([]string{LocalParticipant})
}

func newCoordinator(secret string) *Coordinator {
	ctx, cancel := context.WithCancel(context.Background())
	return &Coordinator{secret: secret, pushCtx: ctx, cancelPushes: cancel}
}

func dial(p config.RoleFenceParticipantConfig) (zrpc.Client, error) {
	var c zrpc.RpcClientConf
	if err := conf.FillDefault(&c); err != nil {
		return nil, err
	}
	c.Endpoints, c.Etcd = p.Endpoints, p.Etcd
	c.NonBlock = true
	// K8s 上参与者都是 Service 地址,整条连接落在一个 Pod 上:keepalive 让对端整机消失后的半开连接
	// 在一次调用后 20 秒内断开重连,否则冻结一直不可达、过户删角被挡约 15 分钟(shared/grpckeepalive)。
	return zrpc.NewClient(c, zrpc.WithDialOption(grpckeepalive.DialOption()))
}

func (c *Coordinator) sign(method string, in proto.Message) error {
	return serviceauth.Sign(method, in, rolefence.CoordinatorCaller, c.secret, uint64(time.Now().UnixMilli()))
}

// Prepare 并行调全部参与者 PrepareRoleFreeze。有不通过就是 Blocked,其次有不可达或过时就是 Retry,全部通过才是 Clean。
func (c *Coordinator) Prepare(ctx context.Context, state *rfpb.RoleFenceState) rolefence.PrepareOutcome {
	outcome, _ := c.askAll(ctx, prepareTimeout, state, false)
	return outcome
}

// DryRun 并行试探全部参与者有没有挂起,参与者不写副本。结论的合并规则和 Prepare 相同,问不齐就是 Retry。
func (c *Coordinator) DryRun(ctx context.Context, state *rfpb.RoleFenceState) rolefence.PrepareOutcome {
	outcome, _ := c.askAll(ctx, dryRunTimeout, state, true)
	return outcome
}

// DryRunBlockers 同 DryRun,另回各参与者说的挂起原因(低基数枚举串,按参与者顺序,一个参与者至多一个)。
func (c *Coordinator) DryRunBlockers(ctx context.Context, state *rfpb.RoleFenceState) (rolefence.PrepareOutcome, []string) {
	return c.askAll(ctx, dryRunTimeout, state, true)
}

func (c *Coordinator) askAll(ctx context.Context, timeout time.Duration, state *rfpb.RoleFenceState, dryRun bool) (rolefence.PrepareOutcome, []string) {
	call, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	outcomes := make([]rolefence.PrepareOutcome, len(c.participants))
	blockers := make([]string, len(c.participants))
	var wg sync.WaitGroup
	for i, p := range c.participants {
		wg.Add(1)
		safego.Go("data_service.role_fence.prepare", func() {
			defer wg.Done()
			outcomes[i], blockers[i] = c.prepareOne(call, p, state, dryRun)
		})
	}
	wg.Wait()
	var reasons []string
	for _, b := range blockers {
		if b != "" {
			reasons = append(reasons, b)
		}
	}
	return mergeOutcomes(outcomes), reasons
}

// Purge 并行调全部参与者 PurgeRemovedRole(docs/design/account-deletion.md §6.1),state 必须是墓碑。全部回成功才是 true;
// 有一个没清完就回 false,删角收尾的小表留着,下一轮(ResumeRemovals)整组再调一遍:每个参与者的清理都可重复。
func (c *Coordinator) Purge(ctx context.Context, state *rfpb.RoleFenceState) bool {
	call, cancel := context.WithTimeout(ctx, purgeTimeout)
	defer cancel()
	done := make([]bool, len(c.participants))
	var wg sync.WaitGroup
	for i, p := range c.participants {
		wg.Add(1)
		safego.Go("data_service.role_fence.purge", func() {
			defer wg.Done()
			done[i] = c.purgeOne(call, p, state)
		})
	}
	wg.Wait()
	for _, ok := range done {
		if !ok {
			return false
		}
	}
	return true
}

func (c *Coordinator) purgeOne(ctx context.Context, p participant, state *rfpb.RoleFenceState) bool {
	in := &rfpb.PurgeRemovedRoleRequest{State: state}
	if err := c.sign(rfpb.RoleFenceParticipant_PurgeRemovedRole_FullMethodName, in); err != nil {
		logx.Errorf("[role-fence] sign purge failed: participant=%s err=%v", p.name, err)
		return false
	}
	out, err := p.client.PurgeRemovedRole(ctx, in)
	switch {
	case err != nil:
		logx.Errorf("[role-fence] purge unreachable: participant=%s player=%d version=%d err=%v", p.name, state.PlayerId, state.Version, err)
		return false
	case out.GetRetry():
		logx.Errorf("[role-fence] purge answered retry: participant=%s player=%d version=%d", p.name, state.PlayerId, state.Version)
		return false
	}
	return true
}

func mergeOutcomes(outcomes []rolefence.PrepareOutcome) rolefence.PrepareOutcome {
	merged := rolefence.PrepareClean
	for _, outcome := range outcomes {
		switch outcome {
		case rolefence.PrepareClean:
		case rolefence.PrepareBlocked:
			return rolefence.PrepareBlocked
		default:
			merged = rolefence.PrepareRetry
		}
	}
	return merged
}

// prepareOne 问一个参与者,回结论与它说的挂起原因(只有 Blocked 时非空)。
func (c *Coordinator) prepareOne(ctx context.Context, p participant, state *rfpb.RoleFenceState, dryRun bool) (rolefence.PrepareOutcome, string) {
	in := &rfpb.PrepareRoleFreezeRequest{State: state, DryRun: dryRun}
	if err := c.sign(rfpb.RoleFenceParticipant_PrepareRoleFreeze_FullMethodName, in); err != nil {
		logx.Errorf("[role-fence] sign prepare failed: participant=%s err=%v", p.name, err)
		metrics.ObserveRoleFencePrepare(p.name, metrics.RoleFenceResultUnreachable)
		return rolefence.PrepareRetry, ""
	}
	out, err := p.client.PrepareRoleFreeze(ctx, in)
	switch {
	case err != nil:
		logx.Errorf("[role-fence] prepare unreachable: participant=%s player=%d version=%d err=%v", p.name, state.PlayerId, state.Version, err)
		metrics.ObserveRoleFencePrepare(p.name, metrics.RoleFenceResultUnreachable)
		return rolefence.PrepareRetry, ""
	case out.GetRetry():
		logx.Errorf("[role-fence] prepare answered retry: participant=%s player=%d version=%d", p.name, state.PlayerId, state.Version)
		metrics.ObserveRoleFencePrepare(p.name, metrics.RoleFenceResultUnreachable)
		return rolefence.PrepareRetry, ""
	case out.GetStale():
		logx.Infof("[role-fence] prepare stale: participant=%s player=%d version=%d", p.name, state.PlayerId, state.Version)
		metrics.ObserveRoleFencePrepare(p.name, metrics.RoleFenceResultStale)
		return rolefence.PrepareRetry, ""
	case out.GetClean():
		metrics.ObserveRoleFencePrepare(p.name, metrics.RoleFenceResultClean)
		return rolefence.PrepareClean, ""
	case out.GetBlocker() != "":
		logx.Infof("[role-fence] prepare blocked: participant=%s player=%d version=%d blocker=%s", p.name, state.PlayerId, state.Version, out.GetBlocker())
		metrics.ObserveRoleFencePrepare(p.name, metrics.RoleFenceResultBlocked)
		metrics.ObserveRoleFenceBlocker(p.name, out.GetBlocker())
		return rolefence.PrepareBlocked, out.GetBlocker()
	default:
		// 既没通过也没说原因,不算通过,也不清令牌。
		logx.Errorf("[role-fence] prepare answered nothing: participant=%s player=%d version=%d", p.name, state.PlayerId, state.Version)
		metrics.ObserveRoleFencePrepare(p.name, metrics.RoleFenceResultUnreachable)
		return rolefence.PrepareRetry, ""
	}
}

// Push 尽力推送,不等结果,也不跟着请求的 ctx 走:请求回给调用方之后推送还要继续。
func (c *Coordinator) Push(_ context.Context, state *rfpb.RoleFenceState) {
	for _, p := range c.participants {
		c.pushes.Add(1)
		safego.Go("data_service.role_fence.push", func() {
			defer c.pushes.Done()
			c.pushOne(p, state)
		})
	}
}

func (c *Coordinator) pushOne(p participant, state *rfpb.RoleFenceState) {
	in := &rfpb.ApplyRoleFenceRequest{State: state}
	err := c.sign(rfpb.RoleFenceParticipant_ApplyRoleFence_FullMethodName, in)
	if err == nil {
		call, cancel := context.WithTimeout(c.pushCtx, pushTimeout)
		defer cancel()
		var out *rfpb.ApplyRoleFenceResponse
		if out, err = p.client.ApplyRoleFence(call, in); err == nil && out.GetRetry() {
			err = errPushRetry
		}
	}
	if err != nil {
		logx.Infof("[role-fence] push failed, the replica will self-heal: participant=%s player=%d version=%d err=%v", p.name, state.PlayerId, state.Version, err)
		metrics.ObserveRoleFencePush(p.name, metrics.RoleFencePushFailed)
		return
	}
	metrics.ObserveRoleFencePush(p.name, metrics.RoleFencePushOK)
}

// Close 取消在途推送,等它们结束,关连接。调用之前业务入口已经排空,不会再有新的推送。
func (c *Coordinator) Close() error {
	c.cancelPushes()
	c.pushes.Wait()
	var result error
	for _, client := range c.closers {
		result = errors.Join(result, client.Conn().Close())
	}
	return result
}
