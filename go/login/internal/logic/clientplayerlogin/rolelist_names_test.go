package clientplayerloginlogic

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc"

	"login/internal/logic/pkg/playernamereg"
	"login/internal/svc"
	pbbase "proto/common/base"
	pbdb "proto/common/database"
	dspb "proto/data_service"
	loginpb "proto/login"
)

// stubRoleNameLookup 是 roleNameLookup 的假实现:记录每次 Lookup 的 id 与 ctx 期限,
// 按 names 应答(只回存在的 id,与 BatchGetPlayerName 同口径)。
// block=true 时一直等到 ctx 结束再返回 ctx.Err(),用来验超时预算确实套上了。
type stubRoleNameLookup struct {
	names map[uint64]string
	err   error
	block bool

	calls       int
	lastIDs     []uint64
	hadDeadline bool
	remaining   time.Duration
}

func (s *stubRoleNameLookup) Lookup(ctx context.Context, ids []uint64) (map[uint64]string, error) {
	s.calls++
	s.lastIDs = append([]uint64(nil), ids...)
	if deadline, ok := ctx.Deadline(); ok {
		s.hadDeadline = true
		s.remaining = time.Until(deadline)
	}
	if s.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if s.err != nil {
		return nil, s.err
	}
	out := map[uint64]string{}
	for _, id := range ids {
		if name, ok := s.names[id]; ok {
			out[id] = name
		}
	}
	return out, nil
}

// fakeNameRegistryClient 是 data_service 名字注册表的假客户端,只实现读侧的 BatchGetPlayerName
// (只回存在的 id);别的 RPC 走内嵌的 nil 接口,调到即 panic —— 读侧不该碰登记 / 释放。
//
// stubRoleNameLookup 替换的是 roleNameLookup 这个接口,只能打纯函数;而 ServiceContext.PlayerNames
// 是具体类型 *playernamereg.Client,验「Login / EnterGame 真的把它接上了」只能从它底下的
// gRPC 客户端换起:playernamereg.New(&fakeNameRegistryClient{...})。
// 不加锁:两条调用链上的回源都发生在调用方 goroutine 里(EnterGame 的回源早于异步入场链)。
type fakeNameRegistryClient struct {
	dspb.DataServiceClient
	names map[uint64]string

	calls   int
	lastIDs []uint64
}

func (f *fakeNameRegistryClient) BatchGetPlayerName(_ context.Context, in *dspb.BatchGetPlayerNameRequest, _ ...grpc.CallOption) (*dspb.BatchGetPlayerNameResponse, error) {
	f.calls++
	f.lastIDs = append([]uint64(nil), in.GetPlayerIds()...)
	out := map[uint64]string{}
	for _, id := range in.GetPlayerIds() {
		if name, ok := f.names[id]; ok {
			out[id] = name
		}
	}
	return &dspb.BatchGetPlayerNameResponse{Names: out}, nil
}

// rolesFromAccount 走真实的 buildRoleList(resolver=nil → 透传账号对象的指针),
// 这正是 fillMissingRoleNames 必须先克隆再写的那种输入。
func rolesFromAccount(players []*pbbase.AccountSimplePlayer) []*loginpb.AccountSimplePlayerWrapper {
	return buildRoleList(context.Background(), nil, false, players)
}

// 全部有名:一次 RPC 都不发 —— 这是绝大多数登录,不能为名字多付一跳。
func TestFillMissingRoleNames_AllNamedSkipsLookup(t *testing.T) {
	players := []*pbbase.AccountSimplePlayer{
		{PlayerId: 100, Name: "云中君"},
		{PlayerId: 101, Name: "湘夫人"},
	}
	roles := rolesFromAccount(players)
	stub := &stubRoleNameLookup{names: map[uint64]string{100: "不该被用到"}}

	fillMissingRoleNames(context.Background(), stub, roles, 0)

	if stub.calls != 0 {
		t.Fatalf("Lookup calls = %d, want 0 when every role already has a name", stub.calls)
	}
	for i, w := range roles {
		if w.Player != players[i] {
			t.Fatalf("role %d was cloned although nothing had to be filled", players[i].PlayerId)
		}
	}
	if roles[0].Player.Name != "云中君" || roles[1].Player.Name != "湘夫人" {
		t.Fatalf("names changed: %q / %q", roles[0].Player.Name, roles[1].Player.Name)
	}
}

// 两个缺名:恰好一次 Lookup、带的就是那两个 id;名字补在克隆上,账号对象一个字节不变。
func TestFillMissingRoleNames_FillsOnCloneWithOneLookup(t *testing.T) {
	players := []*pbbase.AccountSimplePlayer{
		{PlayerId: 100, ZoneId: 1, ClassId: 2},
		{PlayerId: 101, ZoneId: 1, ClassId: 4, Name: "湘夫人"},
		{PlayerId: 102, ZoneId: 1, ClassId: 3},
	}
	roles := rolesFromAccount(players)
	stub := &stubRoleNameLookup{names: map[uint64]string{100: "云中君", 102: "东皇太一"}}

	fillMissingRoleNames(context.Background(), stub, roles, 0)

	if stub.calls != 1 {
		t.Fatalf("Lookup calls = %d, want exactly 1 batched call", stub.calls)
	}
	if len(stub.lastIDs) != 2 || stub.lastIDs[0] != 100 || stub.lastIDs[1] != 102 {
		t.Fatalf("Lookup ids = %v, want [100 102] (only the nameless roles)", stub.lastIDs)
	}
	if !stub.hadDeadline || stub.remaining <= 0 || stub.remaining > playernamereg.DefaultLookupTimeout {
		t.Fatalf("timeout=0 must fall back to DefaultLookupTimeout, got deadline=%v remaining=%v",
			stub.hadDeadline, stub.remaining)
	}

	if got := roles[0].Player.Name; got != "云中君" {
		t.Fatalf("role 100 name = %q, want filled from registry", got)
	}
	if got := roles[2].Player.Name; got != "东皇太一" {
		t.Fatalf("role 102 name = %q, want filled from registry", got)
	}
	// 克隆要带上其余字段,不能只剩 id + name。
	if roles[0].Player.PlayerId != 100 || roles[0].Player.ZoneId != 1 || roles[0].Player.ClassId != 2 {
		t.Fatalf("clone lost fields: %v", roles[0].Player)
	}

	// 账号对象不变:之后无论谁 Marshal(userAccount),回源结果都不会被带进 blob。
	if players[0].Name != "" || players[2].Name != "" {
		t.Fatalf("account objects were mutated: %q / %q", players[0].Name, players[2].Name)
	}
	if roles[0].Player == players[0] || roles[2].Player == players[2] {
		t.Fatal("filled roles still alias the account objects")
	}
	// 本来就有名的角色不受影响,也不必克隆。
	if roles[1].Player != players[1] || roles[1].Player.Name != "湘夫人" {
		t.Fatalf("named role was touched: %v", roles[1].Player)
	}
}

// 注册表里没有的 id(早于名字功能的旧角色)保持空名,不是错误。
func TestFillMissingRoleNames_AbsentFromRegistryStaysEmpty(t *testing.T) {
	players := []*pbbase.AccountSimplePlayer{{PlayerId: 100}, {PlayerId: 101}}
	roles := rolesFromAccount(players)
	stub := &stubRoleNameLookup{names: map[uint64]string{101: "湘夫人"}}

	fillMissingRoleNames(context.Background(), stub, roles, 0)

	if roles[0].Player.Name != "" {
		t.Fatalf("role 100 name = %q, want empty (absent from registry)", roles[0].Player.Name)
	}
	if roles[0].Player != players[0] {
		t.Fatal("role without a registry hit must not be cloned")
	}
	if roles[1].Player.Name != "湘夫人" {
		t.Fatalf("role 101 name = %q, want 湘夫人", roles[1].Player.Name)
	}
}

// 读侧 fail-open:Lookup 报错 / 超时 / 注册表没接线,列表都原样返回,不 panic。
func TestFillMissingRoleNames_FailOpen(t *testing.T) {
	// nil 指针装进接口后不等于 nil 接口:"nil *Client" 必须靠 Client.Lookup 的 nil 接收者
	// 分支兜住,"nil interface" 才是 fillMissingRoleNames 自己的 names == nil 判断。
	var nilClient *playernamereg.Client
	cases := map[string]struct {
		names   roleNameLookup
		timeout time.Duration
	}{
		"lookup error":   {names: &stubRoleNameLookup{err: errors.New("data_service down")}},
		"lookup timeout": {names: &stubRoleNameLookup{block: true}, timeout: 20 * time.Millisecond},
		"nil *Client":    {names: nilClient},
		"nil interface":  {names: nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			players := []*pbbase.AccountSimplePlayer{
				{PlayerId: 100, ZoneId: 1},
				{PlayerId: 101, ZoneId: 1, Name: "湘夫人"},
			}
			roles := rolesFromAccount(players)

			fillMissingRoleNames(context.Background(), tc.names, roles, tc.timeout)

			if len(roles) != 2 {
				t.Fatalf("len=%d, want 2 (login must not lose roles)", len(roles))
			}
			if roles[0].Player != players[0] || roles[1].Player != players[1] {
				t.Fatal("roles must be returned untouched on lookup failure")
			}
			if roles[0].Player.Name != "" || roles[1].Player.Name != "湘夫人" {
				t.Fatalf("names changed on failure: %q / %q", roles[0].Player.Name, roles[1].Player.Name)
			}
		})
	}

	// 超时用例:预算确实套到了 Lookup 的 ctx 上,而不是拿调用方的无期限 ctx 直接等。
	blocked := &stubRoleNameLookup{block: true}
	fillMissingRoleNames(context.Background(), blocked,
		rolesFromAccount([]*pbbase.AccountSimplePlayer{{PlayerId: 100}}), 20*time.Millisecond)
	if blocked.calls != 1 || !blocked.hadDeadline || blocked.remaining > 20*time.Millisecond {
		t.Fatalf("configured timeout not applied: calls=%d deadline=%v remaining=%v",
			blocked.calls, blocked.hadDeadline, blocked.remaining)
	}

	// 空列表:不发 RPC,也不因 nil 切片出错。
	idle := &stubRoleNameLookup{}
	fillMissingRoleNames(context.Background(), idle, nil, 0)
	if idle.calls != 0 {
		t.Fatalf("Lookup calls = %d for an empty role list, want 0", idle.calls)
	}
}

// 接线:Login 的角色列表真的把 ServiceContext.PlayerNames 交给了 fillMissingRoleNames。
// 上面的用例全是直接打纯函数,roleListWithCurrentHomeZone 里那一行调用被删掉它们照样全绿;
// 下面 nil PlayerNames 的用例也拦不住(接没接上名字都是空)。所以这里接一个真的会应答的注册表。
func TestRoleListWithCurrentHomeZone_FillsNamesFromRegistry(t *testing.T) {
	registry := &fakeNameRegistryClient{names: map[uint64]string{100: "云中君"}}
	l := NewLoginLogic(context.Background(), &svc.ServiceContext{PlayerNames: playernamereg.New(registry)})
	players := []*pbbase.AccountSimplePlayer{
		{PlayerId: 100, ZoneId: 1},
		{PlayerId: 101, ZoneId: 1, Name: "湘夫人"},
	}
	acct := &pbdb.UserAccounts{SimplePlayers: &pbbase.AccountSimplePlayerList{Players: players}}

	got := l.roleListWithCurrentHomeZone(acct)

	if len(got) != 2 {
		t.Fatalf("len=%d, want 2", len(got))
	}
	if name := got[0].Player.GetName(); name != "云中君" {
		t.Fatalf("role 100 name = %q, want 云中君 from the registry", name)
	}
	if name := got[1].Player.GetName(); name != "湘夫人" {
		t.Fatalf("role 101 name = %q, want 湘夫人", name)
	}
	if registry.calls != 1 || len(registry.lastIDs) != 1 || registry.lastIDs[0] != 100 {
		t.Fatalf("BatchGetPlayerName calls=%d ids=%v, want exactly one call for [100]", registry.calls, registry.lastIDs)
	}
	// 回源结果只补在返回的那一份上:账号对象不变,之后 Marshal(userAccount) 不会把它带进 blob。
	if players[0].GetName() != "" {
		t.Fatalf("account object was mutated: name = %q", players[0].GetName())
	}
}

// ServiceContext.PlayerNames 没配(nil *Client)时照常返回列表 —— 名字注册表是弱依赖,
// 不能因为它登不进去。
func TestRoleListWithCurrentHomeZone_NilPlayerNamesKeepsList(t *testing.T) {
	l := NewLoginLogic(context.Background(), &svc.ServiceContext{})
	acct := &pbdb.UserAccounts{SimplePlayers: &pbbase.AccountSimplePlayerList{
		Players: []*pbbase.AccountSimplePlayer{
			{PlayerId: 100, ZoneId: 1},
			{PlayerId: 101, ZoneId: 1, Name: "湘夫人"},
		},
	}}

	got := l.roleListWithCurrentHomeZone(acct)

	if len(got) != 2 {
		t.Fatalf("len=%d, want 2", len(got))
	}
	if got[0].Player.GetPlayerId() != 100 || got[0].Player.GetName() != "" {
		t.Fatalf("role 100 = %v, want id kept and name still empty", got[0].Player)
	}
	if got[1].Player.GetName() != "湘夫人" {
		t.Fatalf("role 101 name = %q, want 湘夫人", got[1].Player.GetName())
	}
}
