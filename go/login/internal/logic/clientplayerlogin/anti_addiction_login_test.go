package clientplayerloginlogic

// 防沉迷在 gate 上 Login 的拦截与会话账本(docs/design/real-name-anti-addiction.md §5.3~§5.5)。
// 沿用 login_commit_two_step_test.go 的夹具:登录域句柄是生产的构造器(键守卫生效),账本的键形或同槽出问题这些用例直接报错。
// 本包用例改写 config.AppConfig,不能加 t.Parallel()。

import (
	"context"
	"testing"
	"time"

	"login/internal/logic/pkg/loginsession"
	"login/internal/logic/pkg/nppa"
	"login/internal/svc"
	pbbase "proto/common/base"
	dspb "proto/data_service"
	"shared/antiaddiction"
	"shared/generated/pb/table"
	"shared/redisslot"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

const antiAddictionPISuffix = "09b57f3f8185f8cb5094ea3f26278efb"

var (
	antiAddictionAdultPI = antiaddiction.PIBirthPrefix(1990, 1, 1) + antiAddictionPISuffix
	antiAddictionMinorPI = antiaddiction.PIBirthPrefix(2012, 6, 1) + antiAddictionPISuffix
	// 2026-11-20 是周五:20:30 在未成年人的可玩时段里,12:00 不在。
	antiAddictionEvening  = time.Date(2026, time.November, 20, 20, 30, 0, 0, antiaddiction.Beijing)
	antiAddictionDeadline = time.Date(2026, time.November, 20, 21, 0, 0, 0, antiaddiction.Beijing)
	antiAddictionNoon     = time.Date(2026, time.November, 20, 12, 0, 0, 0, antiaddiction.Beijing)
)

// realNameTestDirectory 给夹具的账号目录加上实名列:account → pi。
type realNameTestDirectory struct {
	*testDirectory
	pi map[string]string
}

func (d *realNameTestDirectory) ReadWithLifecycle(ctx context.Context, account string, create bool) (*dspb.AccountDirectoryResponse, error) {
	players, err := d.testDirectory.Read(ctx, account, create)
	if err != nil {
		return nil, err
	}
	return &dspb.AccountDirectoryResponse{Players: players, RealNamePi: d.pi[account]}, nil
}

func (d *realNameTestDirectory) SetRealName(_ context.Context, account, pi string) (string, error) {
	d.pi[account] = pi
	return pi, nil
}

func (d *realNameTestDirectory) ResetRealName(_ context.Context, account string) (bool, error) {
	had := d.pi[account] != ""
	delete(d.pi, account)
	return had, nil
}

type antiAddictionEnv struct {
	*loginCommitEnv
	dir *realNameTestDirectory
	aa  *svc.AntiAddiction
	now time.Time
}

// newAntiAddictionEnv 在两步提交的夹具上打开防沉迷:夹具的登录方式是自有渠道(不在平台名单里),时钟固定在 now。
func newAntiAddictionEnv(t *testing.T, now time.Time) *antiAddictionEnv {
	t.Helper()
	e := &antiAddictionEnv{loginCommitEnv: newLoginCommitEnv(t), now: now}
	e.mr.SetTime(now) // 账本的时刻取自 Redis
	e.dir = &realNameTestDirectory{testDirectory: e.svcCtx.AccountDirectory.(*testDirectory), pi: map[string]string{}}
	e.svcCtx.AccountDirectory = e.dir
	e.aa = &svc.AntiAddiction{
		Ledger:          nppa.NewLedger(e.svcCtx.LoginRedis, loginsession.TTL()),
		PlatformManaged: map[string]bool{},
		Clock:           func() time.Time { return e.now },
	}
	e.svcCtx.AntiAddiction = e.aa
	return e
}

// watched 是账本里登记着的会话数(跨全部分片汇总)。
func (e *antiAddictionEnv) watched(t *testing.T) int64 {
	t.Helper()
	var total int64
	for shard := 0; shard < redisslot.NppaLedgerShardCount; shard++ {
		n, err := e.svcCtx.LoginRedis.ZCard(context.Background(), redisslot.NppaLeasesKey(shard)).Result()
		require.NoError(t, err)
		total += n
	}
	return total
}

// events 领走账本里待发的上下线事件。
func (e *antiAddictionEnv) events(t *testing.T) []nppa.Behavior {
	t.Helper()
	claimed, err := e.aa.Ledger.Claim(context.Background(), nppa.MaxBatch)
	require.NoError(t, err)
	out := make([]nppa.Behavior, len(claimed))
	for i, c := range claimed {
		require.True(t, c.Valid)
		out[i] = c.Behavior
	}
	return out
}

func (e *antiAddictionEnv) requireNoLoginState(t *testing.T, details *pbbase.SessionDetails) {
	t.Helper()
	require.False(t, e.mr.Exists(loginCommitRef(t, details).Key()), "被拒的登录不留登录态")
	require.Zero(t, e.watched(t), "被拒的登录不进账本")
	require.Empty(t, e.events(t), "被拒的登录不报上线")
}

// 自有渠道、账号没实名:不绑定会话,客户端先去 /api/real-name。
func TestGateLoginRequiresARealNameOnASelfManagedChannel(t *testing.T) {
	e := newAntiAddictionEnv(t, antiAddictionEvening)
	details := loginCommitDetails(1)

	resp := e.login(t, details)

	require.Equal(t, uint32(table.LoginError_kLoginRealNameRequired), resp.GetErrorMessage().GetId())
	require.True(t, resp.GetAntiAddiction().GetRealNameRequired())
	e.requireNoLoginState(t, details)
}

// 平台管的渠道(小游戏):不要求实名,成年人也不进账本,上下线由平台报。
func TestGateLoginOnAPlatformManagedChannelNeedsNoRealName(t *testing.T) {
	e := newAntiAddictionEnv(t, antiAddictionNoon)
	e.aa.PlatformManaged[e.authType] = true

	resp := e.login(t, loginCommitDetails(1))

	require.Zero(t, resp.GetErrorMessage().GetId())
	require.NotNil(t, resp.GetAntiAddiction(), "开了防沉迷的部署,应答总带状态")
	require.False(t, resp.GetAntiAddiction().GetRealNameRequired())
	require.Zero(t, e.watched(t))
	require.Empty(t, e.events(t))
}

// 自有渠道实名的成年人:绑定时报上线,断线时报下线,同一个会话标识。
func TestGateLoginReportsASelfManagedSessionFromBindToDisconnect(t *testing.T) {
	e := newAntiAddictionEnv(t, antiAddictionNoon)
	e.dir.pi[loginCommitAccount] = antiAddictionAdultPI
	details := loginCommitDetails(1)

	resp := e.login(t, details)
	require.Zero(t, resp.GetErrorMessage().GetId())
	require.Zero(t, resp.GetAntiAddiction().GetPlayDeadlineMs(), "成年人没有截止时刻")
	require.EqualValues(t, 1, e.watched(t))

	e.disconnect(t, details)

	events := e.events(t)
	require.Len(t, events, 2)
	require.Equal(t, nppa.BehaviorLogin, events[0].BT)
	require.Equal(t, nppa.BehaviorLogout, events[1].BT)
	require.Equal(t, events[0].SI, events[1].SI)
	require.Len(t, events[0].SI, 32)
	require.Equal(t, antiAddictionAdultPI, events[0].PI)
	require.Zero(t, e.watched(t))
}

// 可玩时段里的未成年人:放行,回包带当天的截止时刻(gate 凭它到点断开),账本里也记着(login 的兜底凭它)。
func TestGateLoginAdmitsAMinorInsideThePlayWindowWithADeadline(t *testing.T) {
	e := newAntiAddictionEnv(t, antiAddictionEvening)
	e.dir.pi[loginCommitAccount] = antiAddictionMinorPI
	details := loginCommitDetails(1)

	resp := e.login(t, details)

	require.Zero(t, resp.GetErrorMessage().GetId())
	require.True(t, resp.GetAntiAddiction().GetMinor())
	require.Equal(t, uint64(antiAddictionDeadline.UnixMilli()), resp.GetAntiAddiction().GetPlayDeadlineMs())
	deadline, err := e.aa.Ledger.Renew(context.Background(), loginCommitRef(t, details).DeviceMember())
	require.NoError(t, err)
	require.True(t, deadline.Equal(antiAddictionDeadline), "账本里的截止时刻 = %v", deadline)
}

// 不在可玩时段的未成年人:不绑定会话,什么都不写。
func TestGateLoginRejectsAMinorOutsideThePlayWindow(t *testing.T) {
	e := newAntiAddictionEnv(t, antiAddictionNoon)
	e.dir.pi[loginCommitAccount] = antiAddictionMinorPI
	details := loginCommitDetails(1)

	resp := e.login(t, details)

	require.Equal(t, uint32(table.LoginError_kLoginMinorNotInPlayTime), resp.GetErrorMessage().GetId())
	require.True(t, resp.GetAntiAddiction().GetMinor())
	e.requireNoLoginState(t, details)
}

// 走平台渠道、但账号在 App 上实名过的未成年人:照样按时段限,只是上下线不由我们报。
func TestGateLoginLimitsAKnownMinorOnAPlatformChannelWithoutReporting(t *testing.T) {
	e := newAntiAddictionEnv(t, antiAddictionEvening)
	e.aa.PlatformManaged[e.authType] = true
	e.dir.pi[loginCommitAccount] = antiAddictionMinorPI
	details := loginCommitDetails(1)

	resp := e.login(t, details)

	require.Zero(t, resp.GetErrorMessage().GetId())
	require.Equal(t, uint64(antiAddictionDeadline.UnixMilli()), resp.GetAntiAddiction().GetPlayDeadlineMs())
	require.EqualValues(t, 1, e.watched(t), "截止时刻记在账本里")
	require.Empty(t, e.events(t), "平台渠道的会话不由我们报上线")
}

// 断线通知抢在 Login 之前:连接已断,不会再有断线通知,账本里刚登记的那条当场收尾,不留只有上线的会话。
func TestGateLoginOnAClosedConnectionEndsTheLedgerSession(t *testing.T) {
	e := newAntiAddictionEnv(t, antiAddictionNoon)
	e.dir.pi[loginCommitAccount] = antiAddictionAdultPI
	details := loginCommitDetails(1)
	e.disconnect(t, details)

	resp := e.login(t, details)

	require.Equal(t, uint32(table.LoginError_kLoginSessionNotFound), resp.GetErrorMessage().GetId())
	require.Zero(t, e.watched(t))
	events := e.events(t)
	require.Len(t, events, 2)
	require.Equal(t, nppa.BehaviorLogin, events[0].BT)
	require.Equal(t, nppa.BehaviorLogout, events[1].BT)
}

// 到点断开由 gate 自己做。过了截止时刻还收到这条连接的续租,说明 gate 没断开它:login 兜底踢一次。
func TestRenewSessionKicksAMinorTheGateFailedToClose(t *testing.T) {
	e := newAntiAddictionEnv(t, antiAddictionEvening)
	e.dir.pi[loginCommitAccount] = antiAddictionMinorPI
	details := loginCommitDetails(1)
	require.Zero(t, e.login(t, details).GetErrorMessage().GetId())
	before := testutil.ToFloat64(backstopKickTotal)

	require.True(t, e.renew(t, details).GetRenewed())
	require.Equal(t, before, testutil.ToFloat64(backstopKickTotal), "没到截止时刻不踢")

	// 线上上报器每秒清扫一次账本,截止时刻一过,这条会话的租约就到期。清扫必须把截止时刻留在账本里:
	// 整条删掉的话,下面这次续租查不到它,兜底就不踢了(2026-11-26 复核出的缺陷)。
	e.now = antiAddictionDeadline.Add(time.Minute)
	e.mr.SetTime(e.now)
	_, err := e.aa.Ledger.Sweep(context.Background())
	require.NoError(t, err)
	require.True(t, e.renew(t, details).GetRenewed())
	require.Equal(t, before+1, testutil.ToFloat64(backstopKickTotal))
}

// 成年人的续租不踢,账本里的租约跟着顺延。
func TestRenewSessionNeverKicksAnAdult(t *testing.T) {
	e := newAntiAddictionEnv(t, antiAddictionNoon)
	e.dir.pi[loginCommitAccount] = antiAddictionAdultPI
	details := loginCommitDetails(1)
	require.Zero(t, e.login(t, details).GetErrorMessage().GetId())
	before := testutil.ToFloat64(backstopKickTotal)

	e.now = antiAddictionNoon.Add(24 * time.Hour)
	require.True(t, e.renew(t, details).GetRenewed())

	require.Equal(t, before, testutil.ToFloat64(backstopKickTotal))
	require.EqualValues(t, 1, e.watched(t))
}
