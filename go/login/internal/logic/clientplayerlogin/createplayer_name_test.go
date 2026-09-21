package clientplayerloginlogic

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	gzprom "github.com/zeromicro/go-zero/core/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	"login/internal/constants"
	"login/internal/logic/pkg/homezone"
	"login/internal/logic/pkg/playernamereg"
	pbbase "proto/common/base"
	pbdb "proto/common/database"
	dspb "proto/data_service"
	loginpb "proto/login"
	"shared/generated/pb/table"
	gametable "shared/generated/table"
	"shared/idsegment"
	"shared/playername"
)

// 建角名字登记(设计 docs/design/guild-phase2/03-names.md §3.11 / §3.19)的单测。
// harness 沿用 createplayer_homezone_test.go:miniredis + 假 data_service 客户端,
// 同一个 fakeRegisterClient 同时充当 home zone 与名字登记的后端(生产上就是同一条连接)。

const (
	nameOwnerID   = uint64(8000) // 「名字已被占用」用例里的占用者
	nameCreateKey = "account_lock:create:" + hzAccount
)

var generatedNamePattern = regexp.MustCompile(`^道友[a-z0-9]{6}$`)

// countingSegment 是会数调用次数的发号源:用来断言「名字不合规在发号之前就被拒」。
type countingSegment struct {
	id    uint64
	calls int
}

func (s *countingSegment) Next(context.Context) (uint64, error) {
	s.calls++
	return s.id, nil
}

type nameHarness struct {
	*createPlayerHarness
	fake    *fakeRegisterClient
	segment *countingSegment
	// scheduled 记下每一次 afterFunc 被安排的延迟;回调本身已被同步执行。
	scheduled []time.Duration
}

func newNameHarness(t *testing.T) *nameHarness {
	t.Helper()
	fake := &fakeRegisterClient{}
	n := &nameHarness{
		createPlayerHarness: newCreatePlayerHarnessWithNames(t, homezone.New(fake, 0, 0, 0), fake),
		fake:                fake,
		segment:             &countingSegment{id: hzPlayerID},
	}
	n.l.svcCtx.PlayerIDMinter = &idsegment.Minter{Name: "player_id", Segment: n.segment}

	// 延迟释放换成同步执行:用例不等 10s,也不引入并发(fake 没有锁)。
	saved := afterFunc
	afterFunc = func(d time.Duration, f func()) {
		n.scheduled = append(n.scheduled, d)
		f()
	}
	t.Cleanup(func() { afterFunc = saved })
	return n
}

func (n *nameHarness) create(t *testing.T, name string) *loginpb.CreatePlayerResponse {
	t.Helper()
	resp, err := n.l.CreatePlayer(&loginpb.CreatePlayerRequest{Name: name})
	if err != nil {
		t.Fatalf("CreatePlayer must report failures via tip, not transport error: %v", err)
	}
	return resp
}

// seedPlayers 把账号 blob 改写成已有这些角色,返回写进去的原始字节(供「blob 未变」断言)。
func (n *nameHarness) seedPlayers(t *testing.T, players ...*pbbase.AccountSimplePlayer) []byte {
	t.Helper()
	blob, err := proto.Marshal(&pbdb.UserAccounts{SimplePlayers: &pbbase.AccountSimplePlayerList{Players: players}})
	if err != nil {
		t.Fatalf("marshal seeded account: %v", err)
	}
	if err := n.rdb.Set(n.ctx, constants.GetAccountDataKey(hzAccount), blob, 0).Err(); err != nil {
		t.Fatalf("seed account blob: %v", err)
	}
	return blob
}

func (n *nameHarness) accountBlob(t *testing.T) []byte {
	t.Helper()
	raw, err := n.rdb.Get(n.ctx, constants.GetAccountDataKey(hzAccount)).Bytes()
	if err != nil {
		t.Fatalf("read account blob: %v", err)
	}
	return raw
}

// defaultClassID 复刻 CreatePlayer 对 class_id=0 的解析(取配表第一个职业;单测进程里
// 通常没加载配表,得到 0)。写成函数而不是常量 0:将来同包有用例加载了配表也不会悄悄失效。
func defaultClassID() uint32 {
	if rows := gametable.ClassTableManagerInstance.FindAll(); len(rows) > 0 {
		return rows[0].Id
	}
	return 0
}

func requireTip(t *testing.T, resp *loginpb.CreatePlayerResponse, code table.LoginError) {
	t.Helper()
	if resp.ErrorMessage == nil {
		t.Fatalf("create succeeded, want tip %v", code)
	}
	if resp.ErrorMessage.GetId() != uint32(code) {
		t.Fatalf("tip = %d, want %v(%d)", resp.ErrorMessage.GetId(), code, uint32(code))
	}
	if len(resp.Players) != 0 {
		t.Fatalf("response carries %d player(s) alongside a failure tip", len(resp.Players))
	}
}

// createPlayerCounterValue 从默认 registry gather 一次,读出某个建角计数器当前的值;
// 没有这条时间序列时返回 0。计数是进程级累加的,用例一律比较**增量**。
//
// 为什么要真的 gather:本包指标走 go-zero 的 core/metric,每次更新都先过
// prometheus.Enabled() 这个全局开关(生产上由 login.yaml 的 Prometheus 块经
// ServiceConf.SetUp 打开)。B3a-1 在 data_service 上踩过「指标一次都没落地且零报错」的坑,
// 只断言「代码走到了哪个分支」看不出那种缺陷,必须去问 registry。单测里开关默认是关的,
// 所以这里先 Enable。
func createPlayerCounterValue(t *testing.T, metricName string, labels map[string]string) float64 {
	t.Helper()
	gzprom.Enable()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != metricName {
			continue
		}
		for _, m := range mf.GetMetric() {
			matched := 0
			for _, lp := range m.GetLabel() {
				if want, ok := labels[lp.GetName()]; ok && want == lp.GetValue() {
					matched++
				}
			}
			if matched == len(labels) {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func orphanCount(t *testing.T) float64 {
	t.Helper()
	return createPlayerCounterValue(t, "login_create_player_name_orphan_total", nil)
}

func releaseCount(t *testing.T, phase, result string) float64 {
	t.Helper()
	return createPlayerCounterValue(t, "login_create_player_name_release_total",
		map[string]string{"phase": phase, "result": result})
}

// 用例 1:不合规的名字在**发号之前**就被拒 —— 不烧 id、不碰登记表;
// tip 带 [min_chars, max_chars] 供客户端回显具体字数(文案里不写死数字)。
func TestCreatePlayerName_InvalidRejectedBeforeMint(t *testing.T) {
	cases := []struct{ label, raw string }{
		{"少于 2 字", "云"},
		{"多于 12 字", strings.Repeat("云", 13)},
		{"含标点", "云中君!"},
		{"含空格", "云 中君"},
		{"非法 UTF-8", "云\xff君"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			n := newNameHarness(t)
			resp := n.create(t, tc.raw)

			requireTip(t, resp, table.LoginError_kRoleNameInvalid)
			if got := resp.ErrorMessage.GetParameters(); !slices.Equal(got, []string{"2", "12"}) {
				t.Fatalf("tip parameters = %v, want [2 12]", got)
			}
			if n.segment.calls != 0 {
				t.Fatalf("minted %d id(s) for an invalid name, want 0", n.segment.calls)
			}
			if len(n.fake.reserveReqs) != 0 || n.fake.calls != 0 {
				t.Fatalf("reserve/register calls = %d/%d, want 0/0", len(n.fake.reserveReqs), n.fake.calls)
			}
		})
	}
}

// 用例 2:敏感词同样在发号之前被拒,用单独的 tip(客户端提示语不同)。
func TestCreatePlayerName_SensitiveRejectedBeforeMint(t *testing.T) {
	n := newNameHarness(t)
	resp := n.create(t, "官方小助手")

	requireTip(t, resp, table.LoginError_kRoleNameSensitive)
	if n.segment.calls != 0 || len(n.fake.reserveReqs) != 0 {
		t.Fatalf("mint/reserve calls = %d/%d, want 0/0", n.segment.calls, len(n.fake.reserveReqs))
	}
}

// 规则读不出来(配表缺行 / 不合法)必须 fail-closed,同样不烧 id。
func TestCreatePlayerName_RulesUnavailableFailsClosed(t *testing.T) {
	n := newNameHarness(t)
	loadNameRules = func() (playername.Rules, playername.GenerateSpec, int, error) {
		return playername.Rules{}, playername.GenerateSpec{}, 0, errors.New("RoleNameRule row missing")
	}

	resp := n.create(t, "云中君")

	requireTip(t, resp, table.LoginError_kLoginDataSerializeFailed)
	if n.segment.calls != 0 || len(n.fake.reserveReqs) != 0 {
		t.Fatalf("mint/reserve calls = %d/%d, want 0/0", n.segment.calls, len(n.fake.reserveReqs))
	}
}

// 玩家输入的名字经归一化后登记、落盘、回给客户端(全角 → 半角,去首尾空白)。
func TestCreatePlayerName_RequestedNameIsNormalizedAndPersisted(t *testing.T) {
	n := newNameHarness(t)
	resp := n.create(t, "  云中君ＡＢ ")

	if resp.ErrorMessage != nil {
		t.Fatalf("unexpected tip %v", resp.ErrorMessage)
	}
	const want = "云中君AB"
	if len(n.fake.reserveReqs) != 1 || n.fake.reserveReqs[0].GetName() != want {
		t.Fatalf("reserved = %v, want exactly one reservation of %q", n.fake.reserveReqs, want)
	}
	players := storedPlayers(t, n.ctx, n.rdb)
	if len(players) != 1 || players[0].GetName() != want {
		t.Fatalf("persisted players = %v, want one player named %q", players, want)
	}
	if len(resp.Players) != 1 || resp.Players[0].GetPlayer().GetName() != want {
		t.Fatalf("response players = %v, want one player named %q", resp.Players, want)
	}
}

// 用例 3:名字被别的账号占用 → kRoleNameTaken。确定没写,所以不释放;
// 而且发生在 player:zone 登记之前,不留幽灵映射。
func TestCreatePlayerName_TakenByStranger(t *testing.T) {
	n := newNameHarness(t)
	n.fake.reserve = func(int, *dspb.ReservePlayerNameRequest) (*dspb.ReservePlayerNameResponse, error) {
		return &dspb.ReservePlayerNameResponse{Result: playername.ReserveTaken, OwnerPlayerId: nameOwnerID}, nil
	}

	resp := n.create(t, "云中君")

	requireTip(t, resp, table.LoginError_kRoleNameTaken)
	if n.fake.calls != 0 || len(n.fake.releaseReqs) != 0 {
		t.Fatalf("register/release calls = %d/%d, want 0/0", n.fake.calls, len(n.fake.releaseReqs))
	}
	if players := storedPlayers(t, n.ctx, n.rdb); len(players) != 0 {
		t.Fatalf("account blob persisted %d player(s) despite the name being taken", len(players))
	}
}

// 用例 4:占用者就是本账号里职业、性别都相同的角色 = 上次建角成功但响应丢了。
// 不建新角色、不报错,原样返回现有列表(客户端取最后一个角色进入)。
func TestCreatePlayerName_TakenByOwnRoleIsLostResponseRetry(t *testing.T) {
	n := newNameHarness(t)
	n.seedPlayers(t, &pbbase.AccountSimplePlayer{
		PlayerId: nameOwnerID, ClassId: defaultClassID(), Gender: 1, ZoneId: hzZoneID, Name: "云中君",
	})
	n.fake.reserve = func(int, *dspb.ReservePlayerNameRequest) (*dspb.ReservePlayerNameResponse, error) {
		return &dspb.ReservePlayerNameResponse{Result: playername.ReserveTaken, OwnerPlayerId: nameOwnerID}, nil
	}

	resp := n.create(t, "云中君")

	if resp.ErrorMessage != nil {
		t.Fatalf("lost-response retry must succeed silently, got tip %v", resp.ErrorMessage)
	}
	if len(resp.Players) != 1 || resp.Players[0].GetPlayer().GetPlayerId() != nameOwnerID {
		t.Fatalf("response players = %v, want exactly the existing player %d", resp.Players, nameOwnerID)
	}
	if players := storedPlayers(t, n.ctx, n.rdb); len(players) != 1 || players[0].GetPlayerId() != nameOwnerID {
		t.Fatalf("account blob = %v, want it untouched with the single existing player", players)
	}
	if n.fake.calls != 0 || len(n.fake.releaseReqs) != 0 {
		t.Fatalf("register/release calls = %d/%d, want 0/0", n.fake.calls, len(n.fake.releaseReqs))
	}
}

// 用例 5:占用者在本账号里,但职业(或性别)不同 = 玩家想再建一个不同的角色却撞上自己的
// 旧名字,如实报被占,不能被当成重试吞掉。
func TestCreatePlayerName_TakenByOwnRoleWithDifferentIdentity(t *testing.T) {
	cases := []struct {
		label    string
		existing *pbbase.AccountSimplePlayer
	}{
		{"职业不同", &pbbase.AccountSimplePlayer{PlayerId: nameOwnerID, ClassId: defaultClassID() + 1, Gender: 1, Name: "云中君"}},
		{"性别不同", &pbbase.AccountSimplePlayer{PlayerId: nameOwnerID, ClassId: defaultClassID(), Gender: 2, Name: "云中君"}},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			n := newNameHarness(t)
			n.seedPlayers(t, tc.existing)
			n.fake.reserve = func(int, *dspb.ReservePlayerNameRequest) (*dspb.ReservePlayerNameResponse, error) {
				return &dspb.ReservePlayerNameResponse{Result: playername.ReserveTaken, OwnerPlayerId: nameOwnerID}, nil
			}

			resp := n.create(t, "云中君")

			requireTip(t, resp, table.LoginError_kRoleNameTaken)
			if players := storedPlayers(t, n.ctx, n.rdb); len(players) != 1 {
				t.Fatalf("account blob has %d player(s), want the single existing one", len(players))
			}
		})
	}
}

// 用例 6:空名(机器人 / 无 UI 路径)由服务端生成;撞名就换一个再试。
// 生成名要写进账号 blob,也要回给客户端。
func TestCreatePlayerName_EmptyNameGeneratesAndRetriesOnCollision(t *testing.T) {
	n := newNameHarness(t)
	n.fake.reserve = func(call int, _ *dspb.ReservePlayerNameRequest) (*dspb.ReservePlayerNameResponse, error) {
		if call == 1 {
			return &dspb.ReservePlayerNameResponse{Result: playername.ReserveTaken, OwnerPlayerId: nameOwnerID}, nil
		}
		return &dspb.ReservePlayerNameResponse{Result: playername.ReserveOK}, nil
	}

	resp := n.create(t, "")

	if resp.ErrorMessage != nil {
		t.Fatalf("unexpected tip %v", resp.ErrorMessage)
	}
	if len(n.fake.reserveReqs) != 2 {
		t.Fatalf("reserve calls = %d, want 2 (collision, then success)", len(n.fake.reserveReqs))
	}
	for i, req := range n.fake.reserveReqs {
		if !generatedNamePattern.MatchString(req.GetName()) {
			t.Fatalf("reserve #%d name = %q, want %s", i+1, req.GetName(), generatedNamePattern)
		}
	}
	want := n.fake.reserveReqs[1].GetName()
	players := storedPlayers(t, n.ctx, n.rdb)
	if len(players) != 1 || players[0].GetName() != want {
		t.Fatalf("persisted players = %v, want one player named %q", players, want)
	}
	if len(resp.Players) != 1 || resp.Players[0].GetPlayer().GetName() != want {
		t.Fatalf("response players = %v, want one player named %q", resp.Players, want)
	}
	if len(n.fake.releaseReqs) != 0 {
		t.Fatalf("release calls = %d, want 0 (a definite Taken needs no compensation)", len(n.fake.releaseReqs))
	}
}

// 空名且每次都撞名:尝试次数用尽后按故障失败,不降级成重名;全程没有需要释放的登记。
func TestCreatePlayerName_EmptyNameExhaustsAttempts(t *testing.T) {
	n := newNameHarness(t)
	n.fake.reserve = func(int, *dspb.ReservePlayerNameRequest) (*dspb.ReservePlayerNameResponse, error) {
		return &dspb.ReservePlayerNameResponse{Result: playername.ReserveTaken, OwnerPlayerId: nameOwnerID}, nil
	}

	resp := n.create(t, "")

	requireTip(t, resp, table.LoginError_kLoginDataSerializeFailed)
	if len(n.fake.reserveReqs) != 5 {
		t.Fatalf("reserve calls = %d, want max_generate_attempts=5", len(n.fake.reserveReqs))
	}
	if n.fake.calls != 0 || len(n.fake.releaseReqs) != 0 {
		t.Fatalf("register/release calls = %d/%d, want 0/0", n.fake.calls, len(n.fake.releaseReqs))
	}
}

// 空名路径上登记结果未知:同名重试一次后仍未知 → 按 uncertain 补偿并**结束**(设计 §3.11),
// 不能像撞名那样换一个名字接着试 —— 那会让同一个 player_id 身后挂上好几条结果未知的在途
// INSERT,每条都要各自补偿两次,还把 3s 预算全花在一个多半已经不通的 data_service 上。
func TestCreatePlayerName_EmptyNameUnknownOutcomeStopsGenerating(t *testing.T) {
	n := newNameHarness(t)
	n.fake.reserve = func(int, *dspb.ReservePlayerNameRequest) (*dspb.ReservePlayerNameResponse, error) {
		return nil, status.Error(codes.Unavailable, "connection reset")
	}

	resp := n.create(t, "")

	requireTip(t, resp, table.LoginError_kLoginDataSerializeFailed)
	if len(n.fake.reserveReqs) != 2 {
		t.Fatalf("reserve calls = %d, want 2 (one generated name + its same-name retry, no second name)",
			len(n.fake.reserveReqs))
	}
	generated := n.fake.reserveReqs[0].GetName()
	if !generatedNamePattern.MatchString(generated) || n.fake.reserveReqs[1].GetName() != generated {
		t.Fatalf("reserve names = %q then %q, want the same generated name (%s) twice",
			generated, n.fake.reserveReqs[1].GetName(), generatedNamePattern)
	}
	if len(n.fake.releaseReqs) != 2 {
		t.Fatalf("release calls = %d, want 2 (immediate + delayed)", len(n.fake.releaseReqs))
	}
	for i, req := range n.fake.releaseReqs {
		if req.GetPlayerId() != hzPlayerID || req.GetName() != generated {
			t.Fatalf("release #%d = %d/%q, want %d/%q", i+1, req.GetPlayerId(), req.GetName(), hzPlayerID, generated)
		}
	}
	if !slices.Equal(n.scheduled, []time.Duration{delayedNameReleaseAfter}) {
		t.Fatalf("delayed releases scheduled = %v, want exactly one after %s", n.scheduled, delayedNameReleaseAfter)
	}
	if n.fake.calls != 0 {
		t.Fatalf("RegisterPlayerZone calls = %d, want 0", n.fake.calls)
	}
}

// 用例 7:第一次登记超时(结果未知)→ 同名重试一次(服务端对同 id 同名幂等)→ 成功。
// 成功了就不需要任何补偿。
func TestCreatePlayerName_UnknownOutcomeRetriesSameNameOnce(t *testing.T) {
	n := newNameHarness(t)
	n.fake.reserve = func(call int, _ *dspb.ReservePlayerNameRequest) (*dspb.ReservePlayerNameResponse, error) {
		if call == 1 {
			return nil, status.Error(codes.DeadlineExceeded, "reserve timed out")
		}
		return &dspb.ReservePlayerNameResponse{Result: playername.ReserveOK}, nil
	}

	resp := n.create(t, "云中君")

	if resp.ErrorMessage != nil {
		t.Fatalf("unexpected tip %v", resp.ErrorMessage)
	}
	if len(n.fake.reserveReqs) != 2 ||
		n.fake.reserveReqs[0].GetName() != "云中君" || n.fake.reserveReqs[1].GetName() != "云中君" {
		t.Fatalf("reserve requests = %v, want the same name twice", n.fake.reserveReqs)
	}
	if len(n.fake.releaseReqs) != 0 || len(n.scheduled) != 0 {
		t.Fatalf("release calls = %d, delayed = %d, want 0/0", len(n.fake.releaseReqs), len(n.scheduled))
	}
	if players := storedPlayers(t, n.ctx, n.rdb); len(players) != 1 || players[0].GetName() != "云中君" {
		t.Fatalf("persisted players = %v, want one player named 云中君", players)
	}
}

// 重试得到「被占」:说明先前那次必未插入(否则占用者是自己,会得到成功),如实报被占、不释放。
func TestCreatePlayerName_UnknownOutcomeThenTaken(t *testing.T) {
	n := newNameHarness(t)
	n.fake.reserve = func(call int, _ *dspb.ReservePlayerNameRequest) (*dspb.ReservePlayerNameResponse, error) {
		if call == 1 {
			return nil, status.Error(codes.Unavailable, "connection reset")
		}
		return &dspb.ReservePlayerNameResponse{Result: playername.ReserveTaken, OwnerPlayerId: nameOwnerID}, nil
	}

	resp := n.create(t, "云中君")

	requireTip(t, resp, table.LoginError_kRoleNameTaken)
	if len(n.fake.releaseReqs) != 0 || len(n.scheduled) != 0 {
		t.Fatalf("release calls = %d, delayed = %d, want 0/0", len(n.fake.releaseReqs), len(n.scheduled))
	}
}

// 用例 8:两次都超时 → 放弃建角,立即释放一次 + 延迟再释放一次(同 id 同名),
// 覆盖「第一次释放跑在在途 INSERT 提交之前」的竞态。不登记 player:zone。
func TestCreatePlayerName_StillUnknownReleasesTwice(t *testing.T) {
	n := newNameHarness(t)
	n.fake.reserve = func(int, *dspb.ReservePlayerNameRequest) (*dspb.ReservePlayerNameResponse, error) {
		return nil, status.Error(codes.DeadlineExceeded, "reserve timed out")
	}
	orphansBefore := orphanCount(t)
	immediateBefore := releaseCount(t, nameReleasePhaseImmediate, nameReleaseResultOK)
	delayedBefore := releaseCount(t, nameReleasePhaseDelayed, nameReleaseResultOK)

	resp := n.create(t, "云中君")

	requireTip(t, resp, table.LoginError_kLoginDataSerializeFailed)
	if len(n.fake.reserveReqs) != 2 {
		t.Fatalf("reserve calls = %d, want 2 (first + one same-name retry)", len(n.fake.reserveReqs))
	}
	if len(n.fake.releaseReqs) != 2 {
		t.Fatalf("release calls = %d, want 2 (immediate + delayed)", len(n.fake.releaseReqs))
	}
	for i, req := range n.fake.releaseReqs {
		if req.GetPlayerId() != hzPlayerID || req.GetName() != "云中君" {
			t.Fatalf("release #%d = %d/%q, want %d/云中君", i+1, req.GetPlayerId(), req.GetName(), hzPlayerID)
		}
	}
	if !slices.Equal(n.scheduled, []time.Duration{delayedNameReleaseAfter}) {
		t.Fatalf("delayed releases scheduled = %v, want exactly one after %s", n.scheduled, delayedNameReleaseAfter)
	}
	if n.fake.calls != 0 {
		t.Fatalf("RegisterPlayerZone calls = %d, want 0", n.fake.calls)
	}
	if players := storedPlayers(t, n.ctx, n.rdb); len(players) != 0 {
		t.Fatalf("account blob persisted %d player(s) despite the failure", len(players))
	}
	if got := releaseCount(t, nameReleasePhaseImmediate, nameReleaseResultOK) - immediateBefore; got != 1 {
		t.Fatalf("release_total{immediate,ok} delta = %v, want 1", got)
	}
	if got := releaseCount(t, nameReleasePhaseDelayed, nameReleaseResultOK) - delayedBefore; got != 1 {
		t.Fatalf("release_total{delayed,ok} delta = %v, want 1", got)
	}
	if got := orphanCount(t) - orphansBefore; got != 0 {
		t.Fatalf("orphan_total delta = %v, want 0 (both releases succeeded)", got)
	}
}

// 预算闸(设计 §3.11「剩余预算 ≥1s 则同名重试一次」):名字登记剩下的预算已经放不下一次完整的
// Reserve(playernamereg.DefaultReserveTimeout)时 ——
//   - 输入名:第一次照发(发之前不看预算);结果未知也**不再**同名重试,直接按 uncertain 补偿
//     (立即 + 延迟各释放一次)。预算不够还硬发,多半只是再添一条结果未知的在途 INSERT;
//   - 空名:一个 Reserve 都不发,也就没有任何需要补偿的登记。
//
// 直接调 reservePlayerName 而不走 CreatePlayer:这一步不碰 Redis;走 CreatePlayer 的话前面几步
// 要在同一个短预算里跑完 miniredis 往返,慢机器上会先超时、测到别的分支。这里的结论不随机器
// 快慢变化:预算取单次 Reserve 的一半,恒小于所需;就算它在调用途中耗尽,hasBudget 同样判
// false(ctx.Err() != nil),各项计数不变 —— 假客户端不看 ctx,补偿释放走 WithoutCancel。
func TestReservePlayerName_BudgetBelowOneReserveRPC(t *testing.T) {
	cases := []struct {
		label        string
		requested    string
		wantReserves int
		wantReleases int
		wantDelayed  []time.Duration
	}{
		{"输入名:不重试,按 uncertain 补偿", "云中君", 1, 2, []time.Duration{delayedNameReleaseAfter}},
		{"空名:一个 RPC 都不发", "", 0, 0, nil},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			n := newNameHarness(t)
			n.fake.reserve = func(int, *dspb.ReservePlayerNameRequest) (*dspb.ReservePlayerNameResponse, error) {
				return nil, status.Error(codes.DeadlineExceeded, "reserve timed out")
			}
			rules, spec, attempts, err := loadNameRules()
			if err != nil {
				t.Fatalf("stubbed name rules: %v", err)
			}
			ctx, cancel := context.WithTimeout(n.ctx, playernamereg.DefaultReserveTimeout/2)
			defer cancel()
			n.l.ctx = ctx

			name, owner, tip := n.l.reservePlayerName(hzAccount, hzPlayerID, tc.requested, rules, spec, attempts)

			if tip.GetId() != uint32(table.LoginError_kLoginDataSerializeFailed) {
				t.Fatalf("tip = %v, want %v", tip, table.LoginError_kLoginDataSerializeFailed)
			}
			if name != "" || owner != 0 {
				t.Fatalf("name/owner = %q/%d alongside a failure tip, want empty/0", name, owner)
			}
			if len(n.fake.reserveReqs) != tc.wantReserves {
				t.Fatalf("reserve calls = %d, want %d", len(n.fake.reserveReqs), tc.wantReserves)
			}
			if len(n.fake.releaseReqs) != tc.wantReleases {
				t.Fatalf("release calls = %d, want %d", len(n.fake.releaseReqs), tc.wantReleases)
			}
			for i, req := range n.fake.releaseReqs {
				if req.GetPlayerId() != hzPlayerID || req.GetName() != tc.requested {
					t.Fatalf("release #%d = %d/%q, want %d/%q", i+1, req.GetPlayerId(), req.GetName(), hzPlayerID, tc.requested)
				}
			}
			if !slices.Equal(n.scheduled, tc.wantDelayed) {
				t.Fatalf("delayed releases scheduled = %v, want %v", n.scheduled, tc.wantDelayed)
			}
		})
	}
}

// 用例 9:FailedPrecondition = 这个 player_id 名下已有另一个名字(发号器重发了在役 id)。
// 确定没写,不重试;而且绝不能释放 —— login 不该再对这个 id 发任何写。
func TestCreatePlayerName_IdConflictNeverReleases(t *testing.T) {
	n := newNameHarness(t)
	n.fake.reserve = func(int, *dspb.ReservePlayerNameRequest) (*dspb.ReservePlayerNameResponse, error) {
		return nil, status.Error(codes.FailedPrecondition, "error_code=26: player already holds a different name")
	}

	resp := n.create(t, "云中君")

	requireTip(t, resp, table.LoginError_kLoginDataSerializeFailed)
	if len(n.fake.reserveReqs) != 1 {
		t.Fatalf("reserve calls = %d, want 1 (a definite refusal is not retried)", len(n.fake.reserveReqs))
	}
	if len(n.fake.releaseReqs) != 0 || len(n.scheduled) != 0 {
		t.Fatalf("release calls = %d, delayed = %d, want 0/0", len(n.fake.releaseReqs), len(n.scheduled))
	}
	if n.fake.calls != 0 {
		t.Fatalf("RegisterPlayerZone calls = %d, want 0", n.fake.calls)
	}
}

// 用例 10:名字登记没接线(nil *Client / 未配 DataServiceRpc)→ 一个 RPC 都没发:
// 拒绝建角,不释放、不计孤儿。
func TestCreatePlayerName_RegistryNotWired(t *testing.T) {
	cases := map[string]*playernamereg.Client{
		"nil client": nil,
		"nil ds":     playernamereg.New(nil),
	}
	for label, client := range cases {
		t.Run(label, func(t *testing.T) {
			n := newNameHarness(t)
			n.l.svcCtx.PlayerNames = client
			orphansBefore := orphanCount(t)

			resp := n.create(t, "云中君")

			requireTip(t, resp, table.LoginError_kLoginDataSerializeFailed)
			if len(n.fake.reserveReqs) != 0 || len(n.fake.releaseReqs) != 0 || len(n.scheduled) != 0 {
				t.Fatalf("reserve/release/delayed = %d/%d/%d, want 0/0/0",
					len(n.fake.reserveReqs), len(n.fake.releaseReqs), len(n.scheduled))
			}
			if n.fake.calls != 0 {
				t.Fatalf("RegisterPlayerZone calls = %d, want 0", n.fake.calls)
			}
			if got := orphanCount(t) - orphansBefore; got != 0 {
				t.Fatalf("orphan_total delta = %v, want 0 (nothing was ever reserved)", got)
			}
		})
	}
}

// data_service 复检判不合规(login 预检却放行了)= 两边规则包版本错配:以服务端为准拒绝。
func TestCreatePlayerName_RejectedByDataService(t *testing.T) {
	n := newNameHarness(t)
	n.fake.reserve = func(int, *dspb.ReservePlayerNameRequest) (*dspb.ReservePlayerNameResponse, error) {
		return &dspb.ReservePlayerNameResponse{Result: playername.ReserveInvalid}, nil
	}

	resp := n.create(t, "云中君")

	requireTip(t, resp, table.LoginError_kRoleNameInvalid)
	if got := resp.ErrorMessage.GetParameters(); !slices.Equal(got, []string{"2", "12"}) {
		t.Fatalf("tip parameters = %v, want [2 12]", got)
	}
	if n.fake.calls != 0 || len(n.fake.releaseReqs) != 0 {
		t.Fatalf("register/release calls = %d/%d, want 0/0", n.fake.calls, len(n.fake.releaseReqs))
	}
}

// data_service 回了一个 login 不认识的结果码(对端比 login 新):不能当成功继续建角 ——
// 那等于拿一个不知道登没登记上的名字去落盘。按故障拒绝,不登记 player:zone、不落盘。
func TestCreatePlayerName_UnknownReserveResultFailsClosed(t *testing.T) {
	n := newNameHarness(t)
	n.fake.reserve = func(int, *dspb.ReservePlayerNameRequest) (*dspb.ReservePlayerNameResponse, error) {
		return &dspb.ReservePlayerNameResponse{Result: 99}, nil
	}

	resp := n.create(t, "云中君")

	requireTip(t, resp, table.LoginError_kLoginDataSerializeFailed)
	if len(n.fake.reserveReqs) != 1 {
		t.Fatalf("reserve calls = %d, want 1 (a definite answer is not retried)", len(n.fake.reserveReqs))
	}
	if n.fake.calls != 0 {
		t.Fatalf("RegisterPlayerZone calls = %d, want 0", n.fake.calls)
	}
	if players := storedPlayers(t, n.ctx, n.rdb); len(players) != 0 {
		t.Fatalf("account blob persisted %d player(s) despite the failure", len(players))
	}
	// 读不懂这个码就不知道它是否意味着「已登记」→ 补偿释放一次(条件删除,playerID 是已放弃的新 id,
	// 删了不会误伤)。这是一条确定的应答、没有在途 INSERT,所以只有立即那一次,没有延迟的第二次。
	if len(n.fake.releaseReqs) != 1 {
		t.Fatalf("release calls = %d, want 1 (an unreadable result may still mean the name was stored)",
			len(n.fake.releaseReqs))
	}
	if req := n.fake.releaseReqs[0]; req.GetPlayerId() != hzPlayerID || req.GetName() != "云中君" {
		t.Fatalf("released %d/%q, want %d/云中君", req.GetPlayerId(), req.GetName(), hzPlayerID)
	}
	if len(n.scheduled) != 0 {
		t.Fatalf("delayed releases scheduled = %v, want none", n.scheduled)
	}
}

// 用例 11:名字已登记、player:zone 登记失败 → 立即释放一次。名字是**确定**登记上的,
// 不存在「释放跑在 INSERT 之前」的竞态,所以没有延迟的第二次。
func TestCreatePlayerName_RegisterFailureReleasesOnce(t *testing.T) {
	n := newNameHarness(t)
	n.fake.err = status.Error(codes.Unavailable, "data_service down")

	resp := n.create(t, "云中君")

	requireTip(t, resp, table.LoginError_kLoginDataSerializeFailed)
	if got := strings.Join(n.fake.events, ","); got != "reserve,register,release" {
		t.Fatalf("data_service call order = %q, want reserve,register,release", got)
	}
	if len(n.scheduled) != 0 {
		t.Fatalf("delayed releases scheduled = %v, want none", n.scheduled)
	}
	if req := n.fake.releaseReqs[0]; req.GetPlayerId() != hzPlayerID || req.GetName() != "云中君" {
		t.Fatalf("released %d/%q, want %d/云中君", req.GetPlayerId(), req.GetName(), hzPlayerID)
	}
}

// 用例 12:建角锁在持锁期间丢了(这里在 Reserve 回调里直接删掉锁来模拟过期后易主)→
// 围栏脚本拒写:账号 blob 一个字节都不变;回读确认 blob 里没有新角色之后名字释放,
// 回 kLoginInProgress。(「-1 但其实写进去了」见 TestCreatePlayerName_LockLostButEarlierAttemptLanded。)
func TestCreatePlayerName_LostLockFencesAccountWrite(t *testing.T) {
	n := newNameHarness(t)
	before := n.seedPlayers(t, &pbbase.AccountSimplePlayer{PlayerId: 7000, Gender: 2, Name: "旧角色"})
	n.fake.reserve = func(int, *dspb.ReservePlayerNameRequest) (*dspb.ReservePlayerNameResponse, error) {
		if err := n.rdb.Del(n.ctx, nameCreateKey).Err(); err != nil {
			t.Errorf("drop create lock: %v", err)
		}
		return &dspb.ReservePlayerNameResponse{Result: playername.ReserveOK}, nil
	}

	resp := n.create(t, "云中君")

	requireTip(t, resp, table.LoginError_kLoginInProgress)
	if len(n.fake.releaseReqs) != 1 || len(n.scheduled) != 0 {
		t.Fatalf("release calls = %d, delayed = %d, want 1/0", len(n.fake.releaseReqs), len(n.scheduled))
	}
	if after := n.accountBlob(t); !bytes.Equal(before, after) {
		t.Fatal("account blob changed although the create lock was lost")
	}
	if err := n.rdb.Get(n.ctx, constants.PlayerToAccountKey(hzPlayerID)).Err(); !errors.Is(err, redis.Nil) {
		t.Fatalf("reverse mapping written despite the failure (err=%v)", err)
	}
}

// 围栏写成功时 blob 带上配置的过期时间(原来的裸 SET 就带,换成脚本后不能丢)。
func TestCreatePlayerName_AccountBlobKeepsCacheExpire(t *testing.T) {
	n := newNameHarness(t)
	if resp := n.create(t, "云中君"); resp.ErrorMessage != nil {
		t.Fatalf("unexpected tip %v", resp.ErrorMessage)
	}
	// harness 把 Account.CacheExpire 设成 1h。
	if ttl := n.mr.TTL(constants.GetAccountDataKey(hzAccount)); ttl != time.Hour {
		t.Fatalf("account blob TTL = %s, want 1h", ttl)
	}
}

// swapWriteScript 把围栏写脚本换成替身(报错,或回 -1),用来走「写未确认」的回读分支。
func swapWriteScript(t *testing.T, src string) {
	t.Helper()
	saved := writeAccountBlobScript
	writeAccountBlobScript = redis.NewScript(src)
	t.Cleanup(func() { writeAccountBlobScript = saved })
}

// 写 blob 报错但其实写进去了(回包丢了):回读看到新角色 → 当成功,名字保留。
func TestCreatePlayerName_WriteErrorButReadBackFindsPlayer(t *testing.T) {
	n := newNameHarness(t)
	swapWriteScript(t, `
redis.call("SET", KEYS[1], ARGV[2])
return redis.error_reply("simulated reply loss")
`)

	resp := n.create(t, "云中君")

	if resp.ErrorMessage != nil {
		t.Fatalf("the write did land, create must succeed; got tip %v", resp.ErrorMessage)
	}
	if len(resp.Players) != 1 || resp.Players[0].GetPlayer().GetPlayerId() != hzPlayerID {
		t.Fatalf("response players = %v, want the new player %d", resp.Players, hzPlayerID)
	}
	if len(n.fake.releaseReqs) != 0 {
		t.Fatalf("release calls = %d, want 0", len(n.fake.releaseReqs))
	}
	if got, err := n.rdb.Get(n.ctx, constants.PlayerToAccountKey(hzPlayerID)).Result(); err != nil || got != hzAccount {
		t.Fatalf("reverse mapping = %q/%v, want %q", got, err, hzAccount)
	}
}

// 写 blob 报错且回读确认没写进去:释放名字(确定已登记,一次即可),回 kLoginRedisSetFailed。
func TestCreatePlayerName_WriteErrorAndReadBackMissesPlayer(t *testing.T) {
	n := newNameHarness(t)
	swapWriteScript(t, `return redis.error_reply("simulated write failure")`)

	resp := n.create(t, "云中君")

	requireTip(t, resp, table.LoginError_kLoginRedisSetFailed)
	if len(n.fake.releaseReqs) != 1 || len(n.scheduled) != 0 {
		t.Fatalf("release calls = %d, delayed = %d, want 1/0", len(n.fake.releaseReqs), len(n.scheduled))
	}
	if players := storedPlayers(t, n.ctx, n.rdb); len(players) != 0 {
		t.Fatalf("account blob persisted %d player(s) despite the failure", len(players))
	}
}

// 写 blob 报错、回读也失败:分不清角色落没落盘 → **保留登记**(宁可孤儿不可重名),
// 记孤儿(日志 + 计数),回 kLoginRedisSetFailed。
func TestCreatePlayerName_WriteAndReadBackBothFailKeepsReservation(t *testing.T) {
	n := newNameHarness(t)
	n.fake.reserve = func(int, *dspb.ReservePlayerNameRequest) (*dspb.ReservePlayerNameResponse, error) {
		// 名字登记成功之后 Redis 整个不可用:后面的围栏写与回读都会失败。
		n.mr.SetError("simulated redis outage")
		return &dspb.ReservePlayerNameResponse{Result: playername.ReserveOK}, nil
	}
	t.Cleanup(func() { n.mr.SetError("") })
	orphansBefore := orphanCount(t)

	resp := n.create(t, "云中君")

	requireTip(t, resp, table.LoginError_kLoginRedisSetFailed)
	if len(n.fake.releaseReqs) != 0 || len(n.scheduled) != 0 {
		t.Fatalf("release calls = %d, delayed = %d, want 0/0 (reservation must be kept)",
			len(n.fake.releaseReqs), len(n.scheduled))
	}
	if got := orphanCount(t) - orphansBefore; got != 1 {
		t.Fatalf("orphan_total delta = %v, want 1", got)
	}
}

// 回归:脚本回 -1,但 blob 里其实已经有新角色。生产上的成因是 go-redis 把读超时的 EVALSHA
// 原样重发(默认 MaxRetries=3):第一次执行时锁还在、已经写成功,只是回包没等到;重发时锁
// 已过期才得到 -1,login 只看得到最后这个 -1。这里用「先写再回 -1」的替身复现这个局面。
// 必须当成功:角色已经落盘,这时释放名字就是放别人来占同名 —— 两个在役角色重名,无法事后修复。
func TestCreatePlayerName_LockLostButEarlierAttemptLanded(t *testing.T) {
	n := newNameHarness(t)
	swapWriteScript(t, `
redis.call("SET", KEYS[1], ARGV[2])
return -1
`)

	resp := n.create(t, "云中君")

	if resp.ErrorMessage != nil {
		t.Fatalf("the write did land, create must succeed; got tip %v", resp.ErrorMessage)
	}
	if len(resp.Players) != 1 || resp.Players[0].GetPlayer().GetPlayerId() != hzPlayerID {
		t.Fatalf("response players = %v, want the new player %d", resp.Players, hzPlayerID)
	}
	if len(n.fake.releaseReqs) != 0 || len(n.scheduled) != 0 {
		t.Fatalf("release calls = %d, delayed = %d, want 0/0 (the character exists, its name must stay)",
			len(n.fake.releaseReqs), len(n.scheduled))
	}
	if players := storedPlayers(t, n.ctx, n.rdb); len(players) != 1 || players[0].GetName() != "云中君" {
		t.Fatalf("persisted players = %v, want one player named 云中君", players)
	}
	if got, err := n.rdb.Get(n.ctx, constants.PlayerToAccountKey(hzPlayerID)).Result(); err != nil || got != hzAccount {
		t.Fatalf("reverse mapping = %q/%v, want %q", got, err, hzAccount)
	}
}

// 脚本回 -1 且回读出来的字节解析不了:分不清角色落没落盘 → 与「脚本报错」那条路径同样
// **保留登记**并记孤儿;tip 仍是 -1 这条路径的 kLoginInProgress。
func TestCreatePlayerName_LockLostAndReadBackUnparsableKeepsReservation(t *testing.T) {
	n := newNameHarness(t)
	// 'g' = 0x67 → wire type 7(protobuf 里不存在),Unmarshal 必报错。
	swapWriteScript(t, `
redis.call("SET", KEYS[1], "garbage")
return -1
`)
	orphansBefore := orphanCount(t)

	resp := n.create(t, "云中君")

	requireTip(t, resp, table.LoginError_kLoginInProgress)
	if len(n.fake.releaseReqs) != 0 || len(n.scheduled) != 0 {
		t.Fatalf("release calls = %d, delayed = %d, want 0/0 (reservation must be kept)",
			len(n.fake.releaseReqs), len(n.scheduled))
	}
	if got := orphanCount(t) - orphansBefore; got != 1 {
		t.Fatalf("orphan_total delta = %v, want 1", got)
	}
	if err := n.rdb.Get(n.ctx, constants.PlayerToAccountKey(hzPlayerID)).Err(); !errors.Is(err, redis.Nil) {
		t.Fatalf("reverse mapping written despite the failure (err=%v)", err)
	}
}

// releaseName 的孤儿计数口径(设计 §3.11):
//   - 确定已登记(uncertain=false):立即那次失败就是孤儿;
//   - 结果未知(uncertain=true):立即那次失败还有第二次兜底,只有延迟那次失败才算孤儿;
//   - 延迟回调 panic 不能带走进程,且按孤儿处理(释放结果未知)。
func TestReleaseName_OrphanAccounting(t *testing.T) {
	down := status.Error(codes.Unavailable, "data_service down")
	cases := []struct {
		name         string
		uncertain    bool
		release      func(call int, _ *dspb.ReleasePlayerNameRequest) error
		wantReleases int
		wantDelayed  int
		wantOrphans  float64
	}{
		{"确定已登记,释放成功", false, nil, 1, 0, 0},
		{"确定已登记,释放失败 → 孤儿", false,
			func(int, *dspb.ReleasePlayerNameRequest) error { return down }, 1, 0, 1},
		{"结果未知,两次都成功", true, nil, 2, 1, 0},
		{"结果未知,立即失败、延迟成功 → 不算孤儿", true,
			func(call int, _ *dspb.ReleasePlayerNameRequest) error {
				if call == 1 {
					return down
				}
				return nil
			}, 2, 1, 0},
		{"结果未知,立即成功、延迟失败 → 孤儿", true,
			func(call int, _ *dspb.ReleasePlayerNameRequest) error {
				if call == 2 {
					return down
				}
				return nil
			}, 2, 1, 1},
		{"结果未知,延迟回调 panic → 被兜住并记孤儿", true,
			func(call int, _ *dspb.ReleasePlayerNameRequest) error {
				if call == 2 {
					panic("simulated panic inside delayed release")
				}
				return nil
			}, 2, 1, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := newNameHarness(t)
			n.fake.release = tc.release
			orphansBefore := orphanCount(t)

			n.l.releaseName(hzAccount, hzPlayerID, "云中君", tc.uncertain)

			if len(n.fake.releaseReqs) != tc.wantReleases || len(n.scheduled) != tc.wantDelayed {
				t.Fatalf("release calls = %d, delayed = %d, want %d/%d",
					len(n.fake.releaseReqs), len(n.scheduled), tc.wantReleases, tc.wantDelayed)
			}
			if got := orphanCount(t) - orphansBefore; got != tc.wantOrphans {
				t.Fatalf("orphan_total delta = %v, want %v", got, tc.wantOrphans)
			}
		})
	}
}

// 补偿释放必须丢掉请求 ctx 的取消信号:走到补偿往往正是因为请求已经超时 / 被客户端取消,
// 带着它去发释放只会原地再失败一次,名字就成了孤儿。
func TestReleaseName_IgnoresRequestCancellation(t *testing.T) {
	n := newNameHarness(t)
	ctx, cancel := context.WithCancel(n.ctx)
	cancel()
	n.l.ctx = ctx

	var releaseCtxErr error
	n.l.svcCtx.PlayerNames = playernamereg.New(&ctxProbeClient{fakeRegisterClient: n.fake, onRelease: func(c context.Context) {
		releaseCtxErr = c.Err()
	}})

	n.l.releaseName(hzAccount, hzPlayerID, "云中君", false)

	if len(n.fake.releaseReqs) != 1 {
		t.Fatalf("release calls = %d, want 1", len(n.fake.releaseReqs))
	}
	if releaseCtxErr != nil {
		t.Fatalf("release ran on a cancelled context: %v", releaseCtxErr)
	}
}

// ctxProbeClient 在 ReleasePlayerName 上多探一眼 ctx,其余原样交给 fakeRegisterClient。
type ctxProbeClient struct {
	*fakeRegisterClient
	onRelease func(context.Context)
}

func (c *ctxProbeClient) ReleasePlayerName(ctx context.Context, in *dspb.ReleasePlayerNameRequest, opts ...grpc.CallOption) (*emptypb.Empty, error) {
	c.onRelease(ctx)
	return c.fakeRegisterClient.ReleasePlayerName(ctx, in, opts...)
}

// hasBudget 对各种 ctx 形态的结论。时长差都取得很开(need/2 对 need、1h 对 need),结论不随机器快慢变化。
func TestHasBudget(t *testing.T) {
	const need = time.Second

	ample, cancelAmple := context.WithTimeout(context.Background(), time.Hour)
	defer cancelAmple()
	short, cancelShort := context.WithTimeout(context.Background(), need/2)
	defer cancelShort()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	ampleButCancelled, cancelBoth := context.WithTimeout(context.Background(), time.Hour)
	cancelBoth()

	cases := []struct {
		name string
		ctx  context.Context
		want bool
	}{
		{"没有截止时间", context.Background(), true},
		{"剩余预算充足", ample, true},
		{"剩余预算不足一次调用", short, false},
		{"已取消(无截止时间)", cancelled, false},
		{"截止时间还远但已取消", ampleButCancelled, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasBudget(tc.ctx, need); got != tc.want {
				t.Fatalf("hasBudget = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsLostResponseRetry(t *testing.T) {
	players := []*pbbase.AccountSimplePlayer{
		{PlayerId: 1, ClassId: 3, Gender: 1},
		{PlayerId: 2, ClassId: 4, Gender: 2},
	}
	cases := []struct {
		name    string
		owner   uint64
		classID uint32
		gender  uint32
		want    bool
	}{
		{"占用者在账号里且职业性别相同", 2, 4, 2, true},
		{"owner=0(data_service 没给占用者)", 0, 3, 1, false},
		{"占用者不在账号里", 9, 3, 1, false},
		{"职业不同", 1, 4, 1, false},
		{"性别不同", 1, 3, 2, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isLostResponseRetry(players, tc.owner, tc.classID, tc.gender, ""); got != tc.want {
				t.Fatalf("isLostResponseRetry = %v, want %v", got, tc.want)
			}
		})
	}
}
