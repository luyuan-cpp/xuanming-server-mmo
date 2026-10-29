package clientplayerloginlogic

import (
	"context"
	"errors"
	"login/internal/svc"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/redis/go-redis/v9"
	"google.golang.org/protobuf/proto"
	base "proto/common/base"
	dbpb "proto/common/database"
	dspb "proto/data_service"
	"shared/accountdirectory"
	"shared/savebarrier"
)

// 明确注入的目录接口替身。只在 fixture 创建时从其输入快照播种；运行期间不读取 Redis。
type testDirectory struct {
	mu                                            sync.Mutex
	accounts                                      map[string]bool
	owners                                        map[uint64]*dspb.AccountRoleOwnership
	readErr, lookupErr, createErr, afterCreateErr error
	afterCommit                                   func()
	max                                           int
}

func newTestDirectory() *testDirectory {
	return &testDirectory{accounts: map[string]bool{}, owners: map[uint64]*dspb.AccountRoleOwnership{}, max: 5}
}
func (d *testDirectory) seed(account string, roles ...*base.AccountSimplePlayer) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.accounts[account] = true
	for _, r := range roles {
		d.owners[r.PlayerId] = &dspb.AccountRoleOwnership{Account: account, Role: proto.Clone(r).(*base.AccountSimplePlayer), OwnerRevision: 1}
	}
}
func (d *testDirectory) list(account string) *base.AccountSimplePlayerList {
	out := &base.AccountSimplePlayerList{}
	for _, r := range d.owners {
		if r.Account == account {
			out.Players = append(out.Players, proto.Clone(r.Role).(*base.AccountSimplePlayer))
		}
	}
	sort.Slice(out.Players, func(i, j int) bool { return out.Players[i].PlayerId < out.Players[j].PlayerId })
	return out
}
func (d *testDirectory) Read(ctx context.Context, a string, create bool) (*base.AccountSimplePlayerList, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if d.readErr != nil {
		return nil, d.readErr
	}
	if !d.accounts[a] && !create {
		return nil, accountdirectory.ErrMissing
	}
	d.accounts[a] = true
	return d.list(a), nil
}
func (d *testDirectory) Create(ctx context.Context, a string, r *base.AccountSimplePlayer) (*base.AccountSimplePlayerList, error) {
	out, err := d.CreateWithInitialCommit(ctx, a, r)
	if err != nil {
		return nil, err
	}
	return out.Players, nil
}
func (d *testDirectory) CreateWithInitialCommit(ctx context.Context, a string, r *base.AccountSimplePlayer) (*dspb.AccountDirectoryResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if d.createErr != nil {
		return nil, d.createErr
	}
	if old := d.owners[r.GetPlayerId()]; old != nil {
		if old.Account != a || old.TradeListingId != 0 || !proto.Equal(old.Role, r) {
			return nil, accountdirectory.ErrConflict
		}
		if d.afterCreateErr != nil {
			return nil, d.afterCreateErr
		}
		return &dspb.AccountDirectoryResponse{Players: d.list(a)}, nil
	}
	if len(d.list(a).Players) >= d.max {
		return nil, accountdirectory.ErrFull
	}
	d.accounts[a] = true
	d.owners[r.PlayerId] = &dspb.AccountRoleOwnership{Account: a, Role: proto.Clone(r).(*base.AccountSimplePlayer), OwnerRevision: 1}
	if d.afterCreateErr != nil {
		return nil, d.afterCreateErr
	}
	if d.afterCommit != nil {
		d.afterCommit()
	}
	// 替身通过同一能力接口承诺 SQL 提交；未知应答和旧角色重放均不返回初始凭据。
	return &dspb.AccountDirectoryResponse{Players: d.list(a), InitialPlayerData: savebarrier.InitialPlayerData(r.PlayerId)}, nil
}
func (d *testDirectory) Lookup(ctx context.Context, id uint64) (*dspb.AccountRoleOwnership, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if d.lookupErr != nil {
		return nil, d.lookupErr
	}
	if o := d.owners[id]; o != nil {
		return proto.Clone(o).(*dspb.AccountRoleOwnership), nil
	}
	return nil, accountdirectory.ErrMissing
}
func (d *testDirectory) Repair(ctx context.Context, a string, p *base.AccountSimplePlayer, rev uint64) (*base.AccountSimplePlayerList, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	o := d.owners[p.GetPlayerId()]
	if o == nil {
		return nil, accountdirectory.ErrMissing
	}
	if o.Account != a || rev == 0 || o.OwnerRevision != rev || o.TradeListingId != 0 {
		return nil, accountdirectory.ErrConflict
	}
	r := proto.Clone(o.Role).(*base.AccountSimplePlayer)
	if p.AppearanceId != "" {
		if r.AppearanceId != "" && r.AppearanceId != p.AppearanceId {
			return nil, accountdirectory.ErrConflict
		}
		r.AppearanceId = p.AppearanceId
	}
	if p.ClassId != 0 {
		if r.ClassId != 0 && r.ClassId != p.ClassId {
			return nil, accountdirectory.ErrConflict
		}
		r.ClassId = p.ClassId
	}
	if p.Gender != 0 {
		if r.Gender != 0 && r.Gender != p.Gender {
			return nil, accountdirectory.ErrConflict
		}
		r.Gender = p.Gender
	}
	if p.Name != "" {
		if r.Name != "" && r.Name != p.Name {
			return nil, accountdirectory.ErrConflict
		}
		r.Name = p.Name
	}
	o.Role = r
	return d.list(a), nil
}
func (d *testDirectory) Remove(ctx context.Context, a string, refs []*dspb.AccountRoleVersion) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, r := range refs {
		if o := d.owners[r.PlayerId]; o != nil && (o.Account != a || o.OwnerRevision != r.OwnerRevision || o.TradeListingId != 0) {
			return accountdirectory.ErrConflict
		}
	}
	for _, r := range refs {
		delete(d.owners, r.PlayerId)
	}
	return nil
}

var fixtureDirectories sync.Map // *redis.Client -> 显式注入的 testDirectory；只供旧 fixture 的观测 helper。
func fixtureDirectory(t *testing.T, rdb *redis.Client) *testDirectory {
	t.Helper()
	if old, ok := fixtureDirectories.Load(rdb); ok {
		return old.(*testDirectory)
	}
	d := newTestDirectory()
	ctx := context.Background()
	keys, err := rdb.Keys(ctx, "account:*").Result()
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		raw, err := rdb.Get(ctx, key).Bytes()
		if err != nil {
			t.Fatal(err)
		}
		a := &dbpb.UserAccounts{}
		if err = proto.Unmarshal(raw, a); err != nil {
			d.readErr = err
			continue
		}
		d.seed(strings.TrimPrefix(key, "account:"), a.GetSimplePlayers().GetPlayers()...)
	}
	fixtureDirectories.Store(rdb, d)
	t.Cleanup(func() { fixtureDirectories.Delete(rdb) })
	return d
}
func fixtureRoles(t *testing.T, rdb *redis.Client, account string) *base.AccountSimplePlayerList {
	t.Helper()
	value, ok := fixtureDirectories.Load(rdb)
	if !ok {
		t.Fatal("fixture未显式装配SQL目录替身")
	}
	out, err := value.(*testDirectory).Read(context.Background(), account, false)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSQLDirectoryUnavailableNeverUsesRedisOwnership(t *testing.T) {
	d := newTestDirectory()
	d.readErr = errors.New("SQL down")
	if _, err := readAccountDirectory(context.Background(), &svc.ServiceContext{AccountDirectory: d}, "buyer", true); err == nil {
		t.Fatal("SQL依赖错误不得伪装空目录")
	}
	if _, err := readAccountDirectory(context.Background(), &svc.ServiceContext{}, "buyer", true); !errors.Is(err, accountdirectory.ErrUnavailable) {
		t.Fatalf("未配置目录不能读Redis替身: %v", err)
	}
}
