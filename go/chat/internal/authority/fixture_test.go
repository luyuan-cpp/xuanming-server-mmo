package authority

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"os"
	"shared/authoritysql"
	"shared/authoritysql/sqltest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/stretchr/testify/require"
	base "proto/common/base"
	dspb "proto/data_service"
	rfpb "proto/role_fence"
	"shared/rolefence"
	"shared/roleidentity"
)

func fixtureRules() Rules {
	return Rules{MaxContentBytes: 512, RateLimitPerSecond: 5, HistoryMaxEntries: 200, HistoryTTLSeconds: 604800, HistoryDefaultLimit: 20, HistoryMaxLimit: 50, SensitiveWords: []string{"广告"}}
}

// fixtureRoleStates 是目录 GetRoleFenceState 的替身:聊天只经这个 RPC 读角色归属,不碰目录的表。
// 没登记过的角色默认是 original-<id>、修订 7、未冻结。
type fixtureRoleStates struct {
	mu      sync.Mutex
	states  map[uint64]*rfpb.RoleFenceState
	missing map[uint64]bool
	err     error
	reads   int
}

func newFixtureRoleStates() *fixtureRoleStates {
	return &fixtureRoleStates{states: map[uint64]*rfpb.RoleFenceState{}, missing: map[uint64]bool{}}
}

func (f *fixtureRoleStates) GetRoleFenceState(_ context.Context, player uint64) (*rfpb.RoleFenceState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.err != nil {
		return nil, f.err
	}
	if f.missing[player] {
		return nil, rolefence.ErrRoleNotFound
	}
	if state, ok := f.states[player]; ok {
		return state, nil
	}
	return &rfpb.RoleFenceState{PlayerId: player, Account: "original-" + strconv.FormatUint(player, 10), OwnerRevision: 7, Version: 1, HomeZone: 1}, nil
}

// set 把角色改成给定的归属,token 非 0 表示被最终冻结。
func (f *fixtureRoleStates) set(player uint64, account string, revision, token uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states[player] = &rfpb.RoleFenceState{PlayerId: player, Account: account, OwnerRevision: revision, FreezeToken: token, FreezeFinal: token != 0, Version: 2, HomeZone: 1}
}

// setPreparing 把角色改成还在投票:有令牌,不是最终冻结。
func (f *fixtureRoleStates) setPreparing(player uint64, account string, revision, token uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states[player] = &rfpb.RoleFenceState{PlayerId: player, Account: account, OwnerRevision: revision, FreezeToken: token, Version: 2, HomeZone: 1}
}

func (f *fixtureRoleStates) remove(player uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.missing[player] = true
}

func (f *fixtureRoleStates) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

func nativeStore(t *testing.T, configured ...Rules) *Store {
	store, _ := nativeStoreFixture(t, false, configured...)
	return store
}

// requireLoopbackTiDBAddress:测试端点必须显式为数值 loopback,不跟随 DNS,不探测其它服务。端口由 runner 决定。
func requireLoopbackTiDBAddress(t *testing.T, address string) {
	t.Helper()
	host, portText, err := net.SplitHostPort(address)
	require.NoError(t, err)
	ip := net.ParseIP(host)
	require.NotNil(t, ip, "测试端点必须是数值loopback")
	require.True(t, ip.IsLoopback(), "测试端点必须是数值loopback")
	port, err := strconv.ParseUint(portText, 10, 16)
	require.NoError(t, err)
	require.NotZero(t, port)
}

// 同例副本显式共享immutable boot配置，不通过进程级池映射查找资格。
func nativeStoreFixture(t *testing.T, withGlobal bool, configured ...Rules) (*Store, *sqltest.Fixture) {
	t.Helper()
	address := os.Getenv("CHAT_IT_TIDB")
	if address == "" {
		if os.Getenv("CHAT_IT_REQUIRE_TIDB") == "1" {
			t.Fatal("真实聊天SQL验收缺CHAT_IT_TIDB")
		}
		t.Skip("未启用独立真实TiDB聊天验收")
	}
	requireLoopbackTiDBAddress(t, address)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	controlConfig := mysql.NewConfig()
	controlConfig.Net = "tcp"
	controlConfig.Addr = address
	controlConfig.User = "root"
	controlConfig.Timeout = time.Second
	controlConfig.ReadTimeout = 5 * time.Second
	controlConfig.WriteTimeout = 5 * time.Second
	var random [16]byte
	_, err := rand.Read(random[:])
	require.NoError(t, err)
	name := "chat_it_" + hex.EncodeToString(random[:])
	schemas := []string{name}
	if withGlobal {
		schemas = append(schemas, name+"_global")
	}
	opts, err := sqltest.OptionsFromEnv(controlConfig.FormatDSN(), name+"_writer", schemas...)
	require.NoError(t, err)
	fixture, err := sqltest.New(ctx, t, opts)
	require.NoError(t, err)
	db, err := fixture.OpenSchema(ctx, name)
	require.NoError(t, err)
	require.NoError(t, Migrate(ctx, db))
	require.NoError(t, fixture.GrantTables(ctx, name, "chat_send_authority", "chat_policy"))
	runtimeDB, err := fixture.Open(ctx, name, 8)
	require.NoError(t, err)
	rules := fixtureRules()
	require.LessOrEqual(t, len(configured), 1)
	if len(configured) == 1 {
		rules = configured[0]
	}
	s, err := New(ctx, runtimeDB, newFixtureRoleStates(), authoritysql.BackendTiDB, rules)
	require.NoError(t, err)
	return s, fixture
}

// roleStatesOf 取夹具给 Store 装的目录替身,用例用它改角色归属。
func roleStatesOf(t *testing.T, s *Store) *fixtureRoleStates {
	t.Helper()
	states, ok := s.states.(*fixtureRoleStates)
	require.True(t, ok, "Store 没装目录替身")
	return states
}

func storeSchema(t *testing.T, s *Store) string {
	t.Helper()
	var schema string
	require.NoError(t, s.db.QueryRow("SELECT DATABASE()").Scan(&schema))
	return schema
}

func actor(t *testing.T, id uint64, account string, revision uint64) context.Context {
	t.Helper()
	ctx, err := roleidentity.Capture(context.Background(), &dspb.AccountRoleOwnership{Account: account, OwnerRevision: revision, Role: &base.AccountSimplePlayer{PlayerId: id}})
	require.NoError(t, err)
	return ctx
}
