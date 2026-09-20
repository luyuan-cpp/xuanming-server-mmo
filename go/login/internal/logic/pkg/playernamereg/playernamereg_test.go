package playernamereg

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	dspb "proto/data_service"
	pbtable "shared/generated/pb/table"
	gametable "shared/generated/table"
	"shared/playername"
)

// fakeNameService 只实现三个名字 RPC;其余方法由内嵌的 nil 接口兜底(调到即 panic,
// 正好暴露「名字客户端碰了不该碰的 RPC」)。
type fakeNameService struct {
	dspb.DataServiceClient
	reserveResp *dspb.ReservePlayerNameResponse
	names       map[uint64]string
	err         error

	reserveReqs []*dspb.ReservePlayerNameRequest
	releaseReqs []*dspb.ReleasePlayerNameRequest
	lookupReqs  []*dspb.BatchGetPlayerNameRequest
}

func (f *fakeNameService) ReservePlayerName(_ context.Context, in *dspb.ReservePlayerNameRequest, _ ...grpc.CallOption) (*dspb.ReservePlayerNameResponse, error) {
	f.reserveReqs = append(f.reserveReqs, in)
	if f.err != nil {
		return nil, f.err
	}
	return f.reserveResp, nil
}

func (f *fakeNameService) ReleasePlayerName(_ context.Context, in *dspb.ReleasePlayerNameRequest, _ ...grpc.CallOption) (*emptypb.Empty, error) {
	f.releaseReqs = append(f.releaseReqs, in)
	if f.err != nil {
		return nil, f.err
	}
	return &emptypb.Empty{}, nil
}

func (f *fakeNameService) BatchGetPlayerName(_ context.Context, in *dspb.BatchGetPlayerNameRequest, _ ...grpc.CallOption) (*dspb.BatchGetPlayerNameResponse, error) {
	f.lookupReqs = append(f.lookupReqs, in)
	if f.err != nil {
		return nil, f.err
	}
	return &dspb.BatchGetPlayerNameResponse{Names: f.names}, nil
}

// 未接线(nil *Client / nil ds)时三个方法都必须回 ErrUnavailable 而不是 panic:
// 建角侧靠这个哨兵判定「一个 RPC 都没发、确定没登记」,读侧靠它走放行。
func TestClient_UnavailableWhenNotWired(t *testing.T) {
	var nilClient *Client
	cases := map[string]*Client{
		"nil client": nilClient,
		"nil ds":     New(nil),
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := c.Reserve(context.Background(), 1, "云中君"); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("Reserve err = %v, want ErrUnavailable", err)
			}
			if err := c.Release(context.Background(), 1, "云中君"); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("Release err = %v, want ErrUnavailable", err)
			}
			names, err := c.Lookup(context.Background(), []uint64{1})
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("Lookup err = %v, want ErrUnavailable", err)
			}
			if names != nil {
				t.Fatalf("Lookup names = %v alongside ErrUnavailable, want nil", names)
			}
		})
	}
}

// Reserve 把业务结果(含占用者 id)原样带出来;gRPC 错误**原样**返回 —— 建角侧要靠
// status code 区分「确定没写」(FailedPrecondition)与「结果未知」(超时等),不能被包一层吞掉。
func TestClient_ReservePassesThroughResultAndError(t *testing.T) {
	fake := &fakeNameService{reserveResp: &dspb.ReservePlayerNameResponse{Result: playername.ReserveTaken, OwnerPlayerId: 77}}
	c := New(fake)

	got, err := c.Reserve(context.Background(), 42, "云中君")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if got.Code != playername.ReserveTaken || got.Owner != 77 {
		t.Fatalf("result = %+v, want {Code:1 Owner:77}", got)
	}
	if len(fake.reserveReqs) != 1 || fake.reserveReqs[0].GetPlayerId() != 42 || fake.reserveReqs[0].GetName() != "云中君" {
		t.Fatalf("request = %v, want player_id=42 name=云中君", fake.reserveReqs)
	}

	fake.err = status.Error(codes.FailedPrecondition, "conflict")
	if _, err := c.Reserve(context.Background(), 42, "云中君"); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("err = %v, want the raw FailedPrecondition status", err)
	}
}

func TestClient_ReleaseSendsIdAndName(t *testing.T) {
	fake := &fakeNameService{}
	c := New(fake)

	if err := c.Release(context.Background(), 42, "云中君"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if len(fake.releaseReqs) != 1 || fake.releaseReqs[0].GetPlayerId() != 42 || fake.releaseReqs[0].GetName() != "云中君" {
		t.Fatalf("request = %v, want player_id=42 name=云中君", fake.releaseReqs)
	}

	fake.err = errors.New("down")
	if err := c.Release(context.Background(), 42, "云中君"); err == nil {
		t.Fatal("Release swallowed the RPC error")
	}
}

// Lookup:空 id 清单不发 RPC;缺席的 id 不在 map 里(不是错误);RPC 出错时不回半份结果。
func TestClient_Lookup(t *testing.T) {
	fake := &fakeNameService{names: map[uint64]string{42: "云中君"}}
	c := New(fake)

	empty, err := c.Lookup(context.Background(), nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("Lookup(nil) = %v/%v, want empty map and nil error", empty, err)
	}
	if len(fake.lookupReqs) != 0 {
		t.Fatalf("Lookup(nil) sent %d RPC(s), want 0", len(fake.lookupReqs))
	}

	names, err := c.Lookup(context.Background(), []uint64{42, 43})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if names[42] != "云中君" {
		t.Fatalf("names[42] = %q, want 云中君", names[42])
	}
	if _, ok := names[43]; ok {
		t.Fatal("absent id 43 must not appear in the result")
	}
	if len(fake.lookupReqs) != 1 || len(fake.lookupReqs[0].GetPlayerIds()) != 2 {
		t.Fatalf("lookup requests = %v, want one RPC carrying 2 ids", fake.lookupReqs)
	}

	fake.err = status.Error(codes.Unavailable, "db down")
	if names, err := c.Lookup(context.Background(), []uint64{42}); err == nil || names != nil {
		t.Fatalf("Lookup on RPC error = %v/%v, want nil map and the error", names, err)
	}
}

// validRow 是一行合法规则(与 data/RoleNameRule.xlsx 的数据行一致),各用例在它上面改一项。
func validRow() *pbtable.RoleNameRuleTable {
	return &pbtable.RoleNameRuleTable{
		Id:                  1,
		MinChars:            2,
		MaxChars:            12,
		GeneratedPrefix:     "道友",
		GeneratedSuffixLen:  6,
		MaxGenerateAttempts: 5,
	}
}

func TestRulesFromRow_Valid(t *testing.T) {
	rules, spec, attempts, err := rulesFromRow(validRow())
	if err != nil {
		t.Fatalf("rulesFromRow: %v", err)
	}
	if rules != (playername.Rules{MinRunes: 2, MaxRunes: 12}) {
		t.Fatalf("rules = %+v, want {2 12}", rules)
	}
	if spec != (playername.GenerateSpec{Prefix: "道友", SuffixLen: 6}) {
		t.Fatalf("spec = %+v, want {道友 6}", spec)
	}
	if attempts != 5 {
		t.Fatalf("attempts = %d, want 5", attempts)
	}
}

// 规则行坏了必须报错(调用方 fail-closed 拒绝建角),不能带着一份半合法的规则继续跑。
func TestRulesFromRow_Rejects(t *testing.T) {
	type row = pbtable.RoleNameRuleTable
	cases := []struct {
		name   string
		mutate func(*row)
	}{
		{"min=0(空串会变合法)", func(r *row) { r.MinChars = 0 }},
		{"max=33(超过结构上限 32)", func(r *row) { r.MaxChars = 33 }},
		{"max 为超大值", func(r *row) { r.MaxChars = 1 << 31 }},
		{"min > max", func(r *row) { r.MinChars = 13 }},
		{"attempts=0", func(r *row) { r.MaxGenerateAttempts = 0 }},
		{"attempts=11", func(r *row) { r.MaxGenerateAttempts = 11 }},
		{"前缀含下划线(字符集不放行)", func(r *row) { r.GeneratedPrefix = "道_友" }},
		{"前缀为空", func(r *row) { r.GeneratedPrefix = "" }},
		{"前缀命中敏感词", func(r *row) { r.GeneratedPrefix = "官方" }},
		{"后缀位数为 0", func(r *row) { r.GeneratedSuffixLen = 0 }},
		{"后缀位数超上限", func(r *row) { r.GeneratedSuffixLen = 17 }},
		{"前缀+后缀超过 max(2+11>12)", func(r *row) { r.GeneratedSuffixLen = 11 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := validRow()
			tc.mutate(r)
			if _, _, _, err := rulesFromRow(r); err == nil {
				t.Fatalf("rulesFromRow accepted an invalid row: %v", r)
			}
		})
	}

	t.Run("nil 行(表里没有 id=1)", func(t *testing.T) {
		if _, _, _, err := rulesFromRow(nil); err == nil {
			t.Fatal("rulesFromRow accepted a missing row")
		}
	})
}

// Rules 读真实导表产物,锁住「代码读的列」与「表里配的值」没有脱节。
//
// 只 Load 这一张表而不是 gametable.LoadTables:后者任何一张表缺文件都是 log.Fatalf,
// 整个测试进程直接退出、拿不到是哪条断言失败;单表 Load 会把错误还给我们。
// 前置:导表器已跑过(generated/tables/rolenamerule.json 存在)。
func TestRules_FromGeneratedTable(t *testing.T) {
	const tableDir = "../../../../../../generated/tables"
	if err := gametable.RoleNameRuleTableManagerInstance.Load(tableDir, false); err != nil {
		t.Fatalf("load RoleNameRule from %s (run the table exporter first): %v", tableDir, err)
	}

	rules, spec, attempts, err := Rules()
	if err != nil {
		t.Fatalf("Rules: %v", err)
	}
	if rules != (playername.Rules{MinRunes: 2, MaxRunes: 12}) {
		t.Fatalf("rules = %+v, want {2 12}", rules)
	}
	if spec != (playername.GenerateSpec{Prefix: "道友", SuffixLen: 6}) {
		t.Fatalf("spec = %+v, want {道友 6}", spec)
	}
	if attempts != 5 {
		t.Fatalf("attempts = %d, want 5", attempts)
	}
}

// 表没加载(管理器里是空快照)时 Rules 必须报错,而不是返回零值规则让调用方自己踩。
func TestRules_MissingRowFails(t *testing.T) {
	saved := gametable.RoleNameRuleTableManagerInstance
	gametable.RoleNameRuleTableManagerInstance = gametable.NewRoleNameRuleTableManager()
	t.Cleanup(func() { gametable.RoleNameRuleTableManagerInstance = saved })

	if _, _, _, err := Rules(); err == nil {
		t.Fatal("Rules succeeded without a RoleNameRule row")
	}
}
