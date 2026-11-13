// Package faultdrill 放真实双区进程的故障控制器,只有测试文件,生产不引用。
package faultdrill

// 这里只控制本轮显式白名单的真实节点,不创建模拟 scene,不构造 Save 请求,不写任何业务行。
// 普通单测默认跳过;专用编排必须 REQUIRE=1,runner 把跳过视为失败。
// 存档只读 SQL player_store,Redis 只读协调态(会话、位置、代次、交接标记、死亡时间)。
import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/conf"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/protobuf/proto"
	databasepb "proto/common/database"
	plpb "proto/player_locator"
	pspb "proto/player_store"
	smpb "proto/scene_manager"
	"shared/authoritysql"
	"shared/clusterredis"
	"shared/ownerepoch"
	"shared/playerredis"
	"shared/redisslot"
	"shared/scenenode"
)

const ownershipFaultSourceZone uint32 = 990017

const ownershipFaultTargetZone uint32 = 990018

// 本机多区演练只有一个地域存档库(dev 地域 0、K=1):两个区的号都落在这里,zone 库已退役(player-data-tidb-scaling-plan.md §7.2 A17c)。
const ownershipFaultPlayerDB = "player_store_1000000_db"

const ownershipFaultKindOwner = uint32(pspb.PlayerStoreKind_PLAYER_STORE_KIND_OWNER)

const ownershipFaultKindWallet = uint32(pspb.PlayerStoreKind_PLAYER_STORE_KIND_WALLET) // 货币与账本所在的行(player-save-direct-commit.md §1.1)

// 周期上限:超过它,测试截止覆盖不了「等一次真实周期存盘」,runner 与这里都拒绝。
const ownershipFaultMaxIntervalSeconds = 120

// 总预算 = 基数 + 2 个真实周期(基线一次、恢复后旧写一次)。runner 用同一公式算自己的截止,改动要两边一起改。
const ownershipFaultBaseBudgetSeconds = 360

// 持锁上限必须短于 go/db 的 SQL 锁等待 10s(player-save-direct-commit.md §5.2),否则 Save 会先超时,测的就不是「提交受阻」。
const ownershipFaultLockHold = 8 * time.Second

const ownershipFaultBlockedWait = 6 * time.Second

// 控制器会话的锁等待是 1s(authoritysql.Open),撞上一笔正常 Save 时换新事务再试,次数有上限。
const ownershipFaultLockAttempts = 5

const ownershipFaultReadSQL = "SELECT kind,owner_epoch,save_seq,schema_gen,holder_instance,released,payload,claim_seq FROM player_store WHERE player_id=? ORDER BY kind"

const ownershipFaultLockSQL = "SELECT player_id FROM player_store WHERE player_id=? AND kind=? FOR UPDATE"

const ownershipFaultLockWaitsSQL = "SELECT COALESCE(KEY_INFO,''),TRX_ID,CURRENT_HOLDING_TRX_ID FROM INFORMATION_SCHEMA.DATA_LOCK_WAITS"

type ownershipFaultPhase struct {
	Run      string `json:"run"`
	Player   uint64 `json:"player_id,omitempty"`
	Gold     uint64 `json:"gold,omitempty"`
	Base     uint64 `json:"base_gold,omitempty"`
	Epoch    uint64 `json:"epoch,omitempty"`
	Command  string `json:"command,omitempty"`
	Rejected bool   `json:"old_write_rejected,omitempty"`
	HoldTrx  uint64 `json:"hold_trx,omitempty"`
	WaitTrx  uint64 `json:"wait_trx,omitempty"`
	WaitKey  string `json:"wait_key,omitempty"`
}

// ownershipFaultSQL 是一条语句读出的一个玩家全部 player_store 行。
// 版本只看写权行(kind=0);没变化的 kind 保留旧 epoch/seq,不要求各行同版本。
type ownershipFaultSQL struct {
	Epoch    uint64
	Seq      uint64
	Holder   string
	HasGold  bool
	Gold     uint64
	Bag      []byte
	Ledger   []byte
	AssetRow string
	Rows     string
}

// ownershipFaultManagerConfig 只取本轮真实 scene_manager 配置里控制器要观测的几段。缺段由 conf 报错,不补缺省端点。
type ownershipFaultManagerConfig struct {
	Etcd struct {
		Hosts []string
	}
	Redis struct {
		Host string
		Type string `json:",optional"`
		Pass string `json:",optional"`
	}

	PlayerRedis                playerredis.Config `json:",optional"`
	SceneReentryBarrierSeconds int64              `json:",optional"`
}

type ownershipFaultRig struct {
	ctx          context.Context
	dir          string
	run          string
	sourceUUID   string
	saveInterval time.Duration
	barrier      time.Duration
	db           *sql.DB
	control      redis.UniversalClient
	player       redis.UniversalClient
	ec           *clientv3.Client
}

func TestFullstackOwnershipFaults(t *testing.T) {
	if os.Getenv("OWNERSHIP_FAULT_REQUIRE") != "1" {
		t.Skip("real-node fault drill not enabled")
	}
	if os.Getenv("OWNERSHIP_FAULT_MODE") != "dev" {
		t.Fatal("only an explicit dev drill is allowed")
	}
	interval, err := strconv.Atoi(os.Getenv("OWNERSHIP_FAULT_SOURCE_SAVE_INTERVAL_SECONDS"))
	if err != nil || interval < 1 || interval > ownershipFaultMaxIntervalSeconds {
		t.Fatal("missing or out-of-range source_save_interval_seconds from the scene receipt")
	}
	sourceUUID := os.Getenv("OWNERSHIP_FAULT_SOURCE_NODE_UUID")
	if sourceUUID == "" {
		t.Fatal("missing source node_uuid from the scene receipt")
	}
	budget := time.Duration(ownershipFaultBaseBudgetSeconds+2*interval) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	r := &ownershipFaultRig{ctx: ctx, dir: os.Getenv("OWNERSHIP_FAULT_DIR"), run: os.Getenv("OWNERSHIP_FAULT_RUN"), sourceUUID: sourceUUID, saveInterval: time.Duration(interval) * time.Second}
	if !filepath.IsAbs(r.dir) || len(r.run) != 32 {
		t.Fatal("invalid run directory or run id")
	}
	dsn, err := mysql.ParseDSN(os.Getenv("OWNERSHIP_FAULT_PLAYER_DSN"))
	if err != nil || dsn.DBName != ownershipFaultPlayerDB || dsn.Net != "tcp" {
		t.Fatal("only the dedicated player schema over tcp is allowed")
	}
	ownershipFaultLocal(t, dsn.Addr)
	// 只有 TiDB 有 DATA_LOCK_WAITS,档位写死 tidb;连到别的引擎在这里失败,不降级成 sleep。
	r.db, err = authoritysql.Open(ctx, dsn.FormatDSN(), ownershipFaultPlayerDB, authoritysql.BackendTiDB, 4, authoritysql.Reader())
	if err != nil {
		t.Fatalf("player_store session rejected: %v", err)
	}
	defer r.db.Close()
	manager := ownershipFaultLoadManagerConfig(t)
	r.barrier = ownershipFaultBarrier(t, manager)
	var closeRedis func()
	r.control, r.player, closeRedis = ownershipFaultRedis(t, manager)
	defer closeRedis()
	for _, host := range manager.Etcd.Hosts {
		ownershipFaultLocal(t, host)
	}
	r.ec, err = clientv3.New(clientv3.Config{Endpoints: manager.Etcd.Hosts, DialTimeout: time.Second})
	if err != nil {
		t.Fatalf("etcd config rejected: %v", err)
	}
	defer r.ec.Close()
	complete := false
	defer func() {
		if !complete {
			r.command(t, "abort")
			r.signal(t, "delay-finish", ownershipFaultPhase{})
			r.signal(t, "zombie-source-finish", ownershipFaultPhase{})
			r.signal(t, "zombie-target-flush", ownershipFaultPhase{})
		}
	}()
	if !t.Run("commit_blocked_handoff_waits", func(t *testing.T) { r.delay(t) }) {
		return
	}
	if !t.Run("stale_scene_resume_write_rejected", func(t *testing.T) { r.zombie(t) }) {
		return
	}
	r.command(t, "done")
	complete = true
}

// delay:SQL 提交受阻时交接等待。只在资产行上持真实悲观锁,不改数据。只有 Save 的批量写会锁这一行,所以等待方就是一笔带资产行的真实 Save。
func (r *ownershipFaultRig) delay(t *testing.T) {
	r.command(t, "start-delay")
	p := r.phase(t, "delay-ready", 45*time.Second)
	before := r.waitSQL(t, 20*time.Second, "delay baseline in player_store", p.Player, func(s ownershipFaultSQL) bool {
		return s.Epoch > 0 && s.Holder == r.sourceUUID && s.HasGold && s.Gold == p.Base
	})
	owner, location := r.ownerLocation(t, p.Player)
	if location.GetZoneId() != ownershipFaultSourceZone || owner < before.Epoch {
		t.Fatal("departure ownership is not on the dedicated source zone")
	}
	locationBefore, err := proto.Marshal(location)
	if err != nil {
		t.Fatal(err)
	}
	if r.handoffMarked(t, r.ctx, p.Player) {
		t.Fatal("handoff mark already present before the blocked window")
	}
	tx, holdTrx, release := r.lockAssetRow(t, p.Player)
	defer release()
	defer tx.Rollback()
	r.signal(t, "delay-go", ownershipFaultPhase{})
	waitCtx, stopWait := context.WithTimeout(r.ctx, ownershipFaultBlockedWait)
	defer stopWait()
	var waitTrx uint64
	var waitKey string
	ownershipFaultWait(t, waitCtx, "real Save blocked by this run's lock", func() bool {
		var hit bool
		waitTrx, waitKey, hit = r.lockWait(t, waitCtx, p.Player, holdTrx)
		return hit
	})
	// 确认真实阻塞之后才开始负向观察,每一轮都要求这笔 Save 还在等本事务的锁。
	for i := 0; i < 4; i++ {
		if waitCtx.Err() != nil {
			t.Fatal("lock budget spent; state after auto release is not negative evidence")
		}
		currentWait, _, hit := r.lockWait(t, waitCtx, p.Player, holdTrx)
		if !hit || currentWait != waitTrx {
			t.Fatal("save stopped waiting while the lock is still held")
		}
		nowSQL := r.readSQL(t, p.Player)
		nowOwner, nowLocation := r.ownerLocation(t, p.Player)
		raw, err := proto.Marshal(nowLocation)
		if err != nil {
			t.Fatal(err)
		}
		if nowSQL.AssetRow != before.AssetRow || nowSQL.Gold != p.Base || nowSQL.Epoch != before.Epoch || nowOwner != owner || !bytes.Equal(raw, locationBefore) || r.handoffMarked(t, waitCtx, p.Player) {
			t.Fatal("sql, owner, location or handoff mark moved while the save commit is blocked")
		}
		select {
		case <-waitCtx.Done():
			t.Fatal("negative observation deadline")
		case <-time.After(250 * time.Millisecond):
		}
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("explicit lock release failed: %v", err)
	}
	r.signal(t, "save-blocked-evidence", ownershipFaultPhase{Player: p.Player, HoldTrx: holdTrx, WaitTrx: waitTrx, WaitKey: waitKey})
	done := r.phase(t, "delay-done", 100*time.Second)
	after := r.waitSQL(t, 20*time.Second, "round trip committed", p.Player, func(s ownershipFaultSQL) bool {
		return s.HasGold && s.Gold == p.Base+18
	})
	if done.Player != p.Player || done.Gold != p.Base+18 || !bytes.Equal(before.Bag, after.Bag) || !bytes.Equal(before.Ledger, after.Ledger) {
		t.Fatal("round trip after release did not keep exactly one asset change")
	}
	r.signal(t, "delay-finish", ownershipFaultPhase{})
	r.signal(t, "delay-proof", ownershipFaultPhase{Player: p.Player, Gold: after.Gold, Base: p.Base, Epoch: after.Epoch, HoldTrx: holdTrx, WaitTrx: waitTrx, WaitKey: waitKey})
}

// zombie:真实旧 scene 恢复后旧写被 SQL 拒绝。只等真实租约消失,不删注册、不伪造死亡、不抬 owner。
func (r *ownershipFaultRig) zombie(t *testing.T) {
	r.command(t, "start-source")
	p := r.phase(t, "zombie-source-baseline", 45*time.Second)
	owner, loc := r.ownerLocation(t, p.Player)
	if loc.GetZoneId() != ownershipFaultSourceZone {
		t.Fatal("stale entity ownership is not on the source zone")
	}
	// 等一次真实周期存盘把准备变更写进 SQL,之后才造脏内存,这样离下一次周期最远。
	before := r.waitSQL(t, r.saveInterval+15*time.Second, "real periodic save of the current entity", p.Player, func(s ownershipFaultSQL) bool {
		return s.HasGold && s.Gold == p.Base && s.Epoch == owner && s.Seq > 0
	})
	if before.Holder != r.sourceUUID {
		t.Fatal("sql holder_instance is not this run's source scene node_uuid")
	}
	oldSession := r.onlineSession(t, p.Player)
	registration, registeredUUID := r.registration(t, loc.GetNodeId(), loc.GetZoneId())
	if registeredUUID != r.sourceUUID {
		t.Fatal("etcd registration of the holding node is not this run's source scene")
	}
	r.signal(t, "zombie-source-dirty", ownershipFaultPhase{})
	p = r.phase(t, "zombie-source-ready", 10*time.Second)
	if p.Gold != p.Base+11 || r.readSQL(t, p.Player).Gold != p.Base {
		t.Fatal("the +11 is not memory-only")
	}
	r.signal(t, "suspend-input", ownershipFaultPhase{Player: p.Player, Epoch: owner})
	r.command(t, "suspend")
	r.phase(t, "control-suspend", 10*time.Second)
	// 暂停成功后再读一次,封住「预检之后、暂停之前周期存盘碰巧发出」的竞态。
	if r.readSQL(t, p.Player).Rows != before.Rows {
		t.Fatal("dirty memory was saved before the suspend; fault window not established")
	}
	deadCtx, stopDead := context.WithTimeout(r.ctx, 145*time.Second)
	defer stopDead()
	ownershipFaultWait(t, deadCtx, "real lease expiry and configured reentry barrier", func() bool {
		current, err := r.ec.Get(deadCtx, registration)
		if err != nil || len(current.Kvs) != 0 {
			return false
		}
		// 键形与 scene_manager logic.nodeDeathAtKey 相同(node:{ctl:<z>}:<n>:death_at,docs/design/redis-cluster-all-domains-20261110.md
		// §4.1),在控制域;那边是 internal 包,这里引用不到,tag 由同一张族表生成。
		raw, err := r.control.Get(deadCtx, fmt.Sprintf("node:%s:%s:death_at",
			redisslot.ZoneControlTag(redisslot.ControlFamilySceneControl, loc.GetZoneId()), loc.GetNodeId())).Result()
		if err != nil {
			return false
		}
		ms, err := strconv.ParseInt(raw, 10, 64)
		return err == nil && ms > 0 && time.Since(time.UnixMilli(ms)) >= r.barrier
	})
	// 替换前必须确有旧 ONLINE 会话。冷首登或手动补会话都不算 ReplaceLogin。
	if r.onlineSession(t, p.Player).GetLoginGeneration() != oldSession.GetLoginGeneration() {
		t.Fatal("session changed by another login during the suspend")
	}
	r.command(t, "start-target")
	target := r.phase(t, "zombie-target-written", 45*time.Second)
	newOwner, newLoc := r.ownerLocation(t, p.Player)
	newSession := r.onlineSession(t, p.Player)
	if target.Player != p.Player || target.Base != p.Base || target.Gold != p.Base+7 || newLoc.GetZoneId() != ownershipFaultTargetZone || newOwner <= owner || newSession.GetLoginGeneration() <= oldSession.GetLoginGeneration() {
		t.Fatal("device B did not take over through a real new login generation and owner")
	}
	r.signal(t, "zombie-target-flush", ownershipFaultPhase{})
	r.phase(t, "zombie-target-done", 15*time.Second)
	committed := r.waitSQL(t, 20*time.Second, "device B formal exit committed", p.Player, func(s ownershipFaultSQL) bool {
		return s.Epoch == newOwner && s.Holder != "" && s.Holder != r.sourceUUID && s.HasGold && s.Gold == p.Base+7
	})
	if !bytes.Equal(before.Bag, committed.Bag) || !bytes.Equal(before.Ledger, committed.Ledger) {
		t.Fatal("bag or asset ledger changed during the takeover")
	}
	r.command(t, "resume")
	r.phase(t, "control-resume", 10*time.Second)
	// 旧实体下一次周期存盘才会真的发出去,等待要覆盖一个真实周期。
	evidence := r.phase(t, "old-write-evidence", r.saveInterval+15*time.Second)
	if !evidence.Rejected || evidence.Player != p.Player || evidence.Epoch != owner {
		t.Fatal("no evidence of the real stale entity's save being rejected; a clean exit does not count")
	}
	// 拒写证据之后仍连续观察,不能只比较恢复前的一次 SQL。
	for i := 0; i < 12; i++ {
		if r.readSQL(t, p.Player).Rows != committed.Rows {
			t.Fatal("resumed stale scene overwrote the new owner's player_store rows")
		}
		select {
		case <-r.ctx.Done():
			t.Fatal("final observation deadline")
		case <-time.After(250 * time.Millisecond):
		}
	}
	r.signal(t, "zombie-source-finish", ownershipFaultPhase{})
	r.signal(t, "zombie-proof", ownershipFaultPhase{Player: p.Player, Base: p.Base, Gold: committed.Gold, Epoch: newOwner, Rejected: true})
}

func ownershipFaultLocal(t *testing.T, addr string) {
	t.Helper()
	host, port, err := net.SplitHostPort(addr)
	n, e := strconv.ParseUint(port, 10, 16)
	if err != nil || e != nil || n == 0 || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		t.Fatal("only isolated loopback endpoints are allowed")
	}
}

// ownershipFaultLoadManagerConfig 读本轮真实 scene_manager 配置。端点不接受单独传值,否则配置和实际进程可能是两套。
func ownershipFaultLoadManagerConfig(t *testing.T) ownershipFaultManagerConfig {
	t.Helper()
	path := os.Getenv("OWNERSHIP_FAULT_SM_CONFIG")
	if !filepath.IsAbs(path) {
		t.Fatal("OWNERSHIP_FAULT_SM_CONFIG must be the absolute path of this run's scene_manager config")
	}
	raw, err := authoritysql.ReadPrivateControlFile(path, 1<<20)
	if err != nil {
		t.Fatalf("scene_manager config unreadable: %v", err)
	}
	var cfg ownershipFaultManagerConfig
	if err := conf.LoadFromYamlBytes(raw, &cfg); err != nil {
		t.Fatalf("scene_manager config invalid: %v", err)
	}
	if len(cfg.Etcd.Hosts) == 0 || cfg.Redis.Host == "" {
		t.Fatal("scene_manager config has no etcd hosts or redis host")
	}
	return cfg
}

// ownershipFaultBarrier:配置没写屏障时 scene_manager 用内部常量(constants.SceneReentryBarrier),本模块引用不到,
// 由启动者把实际值显式传进来;两边都没有就失败,不在这里写第二份默认值。
func ownershipFaultBarrier(t *testing.T, cfg ownershipFaultManagerConfig) time.Duration {
	t.Helper()
	seconds := cfg.SceneReentryBarrierSeconds
	if seconds <= 0 {
		var err error
		seconds, err = strconv.ParseInt(os.Getenv("OWNERSHIP_FAULT_REENTRY_BARRIER_SECONDS"), 10, 64)
		if err != nil || seconds <= 0 {
			t.Fatal("reentry barrier missing from both scene_manager config and OWNERSHIP_FAULT_REENTRY_BARRIER_SECONDS")
		}
	}
	return time.Duration(seconds) * time.Second
}

// ownershipFaultRedis 按真实配置连控制域与玩家域。玩家域没写 Addrs 时指向控制域那一个实例,口径同 EffectivePlayerRedis。
func ownershipFaultRedis(t *testing.T, cfg ownershipFaultManagerConfig) (redis.UniversalClient, redis.UniversalClient, func()) {
	t.Helper()
	controlConfig := redis.UniversalOptions{
		Addrs:                 []string{cfg.Redis.Host},
		Password:              cfg.Redis.Pass,
		IsClusterMode:         cfg.Redis.Type == "cluster",
		MaxRetries:            -1,
		ContextTimeoutEnabled: true,
	}
	playerConfig := cfg.PlayerRedis
	if len(playerConfig.Addrs) == 0 {
		playerConfig.Addrs = controlConfig.Addrs
		playerConfig.Password = controlConfig.Password
		playerConfig.Cluster = controlConfig.IsClusterMode
	}
	for _, addr := range append(append([]string(nil), controlConfig.Addrs...), playerConfig.Addrs...) {
		ownershipFaultLocal(t, addr)
	}
	// 控制域的判死标记带控制集群的 tag {ctl:<z>}，不是玩家域的 tag，不能挂玩家域的键守卫。
	control := redis.NewUniversalClient(&controlConfig)
	player, err := playerredis.New(playerConfig)
	if err != nil {
		_ = control.Close()
		t.Fatalf("player redis config rejected: %v", err)
	}
	return control, player, func() {
		_ = player.Close()
		_ = control.Close()
	}
}

func TestOwnershipFaultControlRedisReadsDeathMark(t *testing.T) {
	server := miniredis.RunT(t)
	const deathKey = "node:{ctl:990017}:1:death_at"
	if err := server.Set(deathKey, "1793800000000"); err != nil {
		t.Fatal(err)
	}
	cfg := ownershipFaultManagerConfig{}
	cfg.Redis.Host = server.Addr()
	control, player, closeClients := ownershipFaultRedis(t, cfg)
	t.Cleanup(closeClients)

	value, err := control.Get(t.Context(), deathKey).Result()
	if err != nil || value != "1793800000000" {
		t.Fatalf("控制域应读取带控制集群 tag 的判死标记，实际 %q，错误 %v", value, err)
	}
	if err := player.Get(t.Context(), deathKey).Err(); !errors.Is(err, clusterredis.ErrWrongDomainTag) {
		t.Fatalf("玩家域应拒绝带控制集群 tag 的键，实际错误 %v", err)
	}
}

func (r *ownershipFaultRig) signal(t *testing.T, name string, p ownershipFaultPhase) {
	t.Helper()
	p.Run = r.run
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal("phase encoding failed")
	}
	path := filepath.Join(r.dir, name+".json")
	if existing, err := os.ReadFile(path); err == nil {
		if bytes.Equal(existing, b) {
			return
		}
		t.Fatal("phase file already has conflicting content")
	}
	f, err := os.OpenFile(path+".tmp", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal("phase file exists or is not writable")
	}
	_, e := f.Write(b)
	closeErr := f.Close()
	if e != nil || closeErr != nil || os.Rename(path+".tmp", path) != nil {
		t.Fatal("phase file commit failed")
	}
}

func (r *ownershipFaultRig) command(t *testing.T, command string) {
	t.Helper()
	// 每个命令只创建一次;不覆盖正被 Windows 编排器打开的文件。
	r.signal(t, "command-"+command, ownershipFaultPhase{Command: command})
}

func (r *ownershipFaultRig) phase(t *testing.T, name string, budget time.Duration) ownershipFaultPhase {
	t.Helper()
	ctx, cancel := context.WithTimeout(r.ctx, budget)
	defer cancel()
	var p ownershipFaultPhase
	ownershipFaultWait(t, ctx, name, func() bool {
		b, err := os.ReadFile(filepath.Join(r.dir, name+".json"))
		if os.IsNotExist(err) {
			return false
		}
		if err != nil || json.Unmarshal(b, &p) != nil || p.Run != r.run {
			t.Fatal("phase file invalid or not from this run")
		}
		return true
	})
	return p
}

func ownershipFaultWait(t *testing.T, ctx context.Context, stage string, check func() bool) {
	t.Helper()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			t.Fatalf("fault drill stage deadline: %s", stage)
		}
		if check() {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("fault drill stage deadline: %s", stage)
		case <-ticker.C:
		}
	}
}

// lockAssetRow 持锁的上限由事务 context 兜住:到期 database/sql 自动回滚,锁不会比预算活得久。
func (r *ownershipFaultRig) lockAssetRow(t *testing.T, id uint64) (*sql.Tx, uint64, context.CancelFunc) {
	t.Helper()
	for attempt := 0; attempt < ownershipFaultLockAttempts; attempt++ {
		holdCtx, stop := context.WithTimeout(r.ctx, ownershipFaultLockHold)
		tx, err := r.db.BeginTx(holdCtx, nil)
		if err != nil {
			stop()
			t.Fatalf("lock transaction failed: %v", err)
		}
		var locked uint64
		err = tx.QueryRowContext(holdCtx, ownershipFaultLockSQL, id, ownershipFaultKindWallet).Scan(&locked)
		if err == nil && locked == id {
			var holdTrx uint64
			// TRX_ID 就是事务 start_ts,悲观 RC 下整笔事务不变。
			if err := tx.QueryRowContext(holdCtx, "SELECT @@tidb_current_ts").Scan(&holdTrx); err != nil || holdTrx == 0 {
				_ = tx.Rollback()
				stop()
				t.Fatalf("lock transaction id unavailable: %v", err)
			}
			return tx, holdTrx, stop
		}
		_ = tx.Rollback()
		stop()
		var server *mysql.MySQLError
		if err == nil || !errors.As(err, &server) || server.Number != 1205 {
			t.Fatalf("asset row lock failed: %v", err)
		}
	}
	t.Fatal("asset row lock not acquired within the attempt limit")
	return nil, 0, nil
}

// lockWait 查本事务持有的锁上有没有等待者。查询失败(含缺 PROCESS 权限)和等待键不符都直接失败,不当成「还没等到」。
func (r *ownershipFaultRig) lockWait(t *testing.T, ctx context.Context, id, holdTrx uint64) (uint64, string, bool) {
	t.Helper()
	rows, err := r.db.QueryContext(ctx, ownershipFaultLockWaitsSQL)
	if err != nil {
		t.Fatalf("DATA_LOCK_WAITS unavailable (needs PROCESS on the dev observer account): %v", err)
	}
	defer rows.Close()
	var waiter uint64
	var key string
	hit := false
	for rows.Next() {
		var info string
		var waitTrx, holder uint64
		if err := rows.Scan(&info, &waitTrx, &holder); err != nil {
			t.Fatalf("DATA_LOCK_WAITS scan failed: %v", err)
		}
		if holder != holdTrx {
			continue
		}
		if waitTrx == holdTrx || !ownershipFaultKeyMatches(info, id) {
			t.Fatalf("lock wait on this run's lock has an unexpected key: %s", info)
		}
		waiter, key, hit = waitTrx, info, true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("DATA_LOCK_WAITS read failed: %v", err)
	}
	return waiter, key, hit
}

// ownershipFaultKeyMatches 核等待键确实是本轮玩家的资产行。聚簇复合主键的 handle_value 形如 {id, kind};非聚簇落在索引键上。
func ownershipFaultKeyMatches(info string, id uint64) bool {
	var decoded struct {
		DBName      string   `json:"db_name"`
		TableName   string   `json:"table_name"`
		HandleValue string   `json:"handle_value"`
		IndexValues []string `json:"index_values"`
	}
	if json.Unmarshal([]byte(info), &decoded) != nil || decoded.DBName != ownershipFaultPlayerDB || decoded.TableName != "player_store" {
		return false
	}
	if decoded.HandleValue == fmt.Sprintf("{%d, %d}", id, ownershipFaultKindWallet) {
		return true
	}
	return len(decoded.IndexValues) == 2 && decoded.IndexValues[0] == strconv.FormatUint(id, 10) && decoded.IndexValues[1] == strconv.FormatUint(uint64(ownershipFaultKindWallet), 10)
}

func (r *ownershipFaultRig) handoffMarked(t *testing.T, ctx context.Context, id uint64) bool {
	t.Helper()
	marked, err := r.player.Exists(ctx, ownerepoch.HandoffKey(id)).Result()
	if err != nil {
		t.Fatalf("handoff mark read failed: %v", err)
	}
	return marked != 0
}

func (r *ownershipFaultRig) ownerLocation(t *testing.T, id uint64) (uint64, *smpb.PlayerLocation) {
	t.Helper()
	owner, err := r.player.Get(r.ctx, ownerepoch.OwnerEpochKey(id)).Uint64()
	if err != nil || owner == 0 {
		t.Fatal("real owner epoch missing")
	}
	b, err := r.player.Get(r.ctx, redisslot.LocationKey(id)).Bytes()
	loc := &smpb.PlayerLocation{}
	if err != nil || proto.Unmarshal(b, loc) != nil || loc.GetOwnerEpoch() != owner {
		t.Fatal("real location disagrees with owner epoch")
	}
	return owner, loc
}

func (r *ownershipFaultRig) onlineSession(t *testing.T, id uint64) *plpb.PlayerSession {
	t.Helper()
	b, err := r.player.Get(r.ctx, redisslot.SessionKey(id)).Bytes()
	s := &plpb.PlayerSession{}
	if err != nil || proto.Unmarshal(b, s) != nil || s.GetPlayerId() != id || s.GetState() != plpb.PlayerSessionState_SESSION_STATE_ONLINE || s.GetLoginGeneration() == 0 {
		t.Fatal("no real ONLINE session; cannot claim ReplaceLogin")
	}
	return s
}

// registration 按位置里的节点身份找真实 etcd 注册,回键和 node_uuid。
func (r *ownershipFaultRig) registration(t *testing.T, node string, zone uint32) (string, string) {
	t.Helper()
	result, err := r.ec.Get(r.ctx, scenenode.SceneNodeRpcPrefix, clientv3.WithPrefix())
	if err != nil {
		t.Fatalf("etcd registry read failed: %v", err)
	}
	key, uuid := "", ""
	for _, kv := range result.Kvs {
		var v struct {
			Node uint32 `json:"nodeId"`
			Zone uint32 `json:"zoneId"`
			UUID string `json:"nodeUuid"`
		}
		if json.Unmarshal(kv.Value, &v) == nil && strconv.FormatUint(uint64(v.Node), 10) == node && v.Zone == zone {
			if key != "" {
				t.Fatal("node identity is ambiguous in etcd")
			}
			key, uuid = string(kv.Key), v.UUID
		}
	}
	if key == "" || uuid == "" {
		t.Fatal("holding node has no real etcd registration with node_uuid")
	}
	return key, uuid
}

func (r *ownershipFaultRig) waitSQL(t *testing.T, budget time.Duration, stage string, id uint64, ready func(ownershipFaultSQL) bool) ownershipFaultSQL {
	t.Helper()
	ctx, cancel := context.WithTimeout(r.ctx, budget)
	defer cancel()
	var s ownershipFaultSQL
	ownershipFaultWait(t, ctx, stage, func() bool {
		s = r.readSQL(t, id)
		return ready(s)
	})
	return s
}

// readSQL 一条语句强读本轮玩家各 kind。kind=0 是写权真相,payload 必须为空;kind=2 按 PlayerDatabase 片段解出金币、背包、账本。
func (r *ownershipFaultRig) readSQL(t *testing.T, id uint64) ownershipFaultSQL {
	t.Helper()
	ctx, cancel := context.WithTimeout(r.ctx, 2*time.Second)
	defer cancel()
	rows, err := r.db.QueryContext(ctx, ownershipFaultReadSQL, id)
	if err != nil {
		t.Fatalf("player_store strong read failed: %v", err)
	}
	defer rows.Close()
	var s ownershipFaultSQL
	foundOwner := false
	whole := sha256.New()
	for rows.Next() {
		var kind, schemaGen, released uint32
		var epoch, seq, claim uint64
		var holder, payload []byte
		if err := rows.Scan(&kind, &epoch, &seq, &schemaGen, &holder, &released, &payload, &claim); err != nil {
			t.Fatalf("player_store scan failed: %v", err)
		}
		fmt.Fprintf(whole, "%d,%d,%d,%d,%x,%d,%x,%d;", kind, epoch, seq, schemaGen, holder, released, payload, claim)
		switch kind {
		case ownershipFaultKindOwner:
			if len(payload) != 0 {
				t.Fatal("writer row payload must be empty")
			}
			s.Epoch, s.Seq, s.Holder = epoch, seq, string(holder)
			foundOwner = true
		case ownershipFaultKindWallet:
			asset := &databasepb.PlayerDatabase{}
			if err := proto.Unmarshal(payload, asset); err != nil {
				t.Fatalf("asset row payload invalid: %v", err)
			}
			if currency := asset.GetCurrency(); currency != nil && len(currency.Values) > 0 {
				s.HasGold, s.Gold = true, currency.Values[0]
			}
			s.Bag = ownershipFaultCanonical(t, asset.GetBagComponent())
			s.Ledger = ownershipFaultCanonical(t, asset.GetAssetOpLedger())
			digest := sha256.Sum256(payload)
			s.AssetRow = hex.EncodeToString(digest[:])
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("player_store read failed: %v", err)
	}
	if !foundOwner {
		t.Fatal("writer row missing for this run's player")
	}
	s.Rows = hex.EncodeToString(whole.Sum(nil))
	return s
}

func ownershipFaultCanonical(t *testing.T, m proto.Message) []byte {
	t.Helper()
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(m)
	if err != nil {
		t.Fatalf("canonical encoding failed: %v", err)
	}
	return b
}
