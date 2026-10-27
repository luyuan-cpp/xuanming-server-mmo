package kafka

// 这里只控制本轮显式白名单的真实节点，不创建模拟 scene，不构造旧 DBTask。
// 普通单测默认跳过；专用编排必须 REQUIRE=1，并把跳过视为失败。
import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/protobuf/proto"
	"proto/common/component"
	plpb "proto/player_locator"
	smpb "proto/scene_manager"
	"shared/ownerepoch"
	"shared/savebarrier"
	"shared/scenenode"
)

type ownershipFaultPhase struct {
	Run      string `json:"run"`
	Player   uint64 `json:"player_id,omitempty"`
	Gold     uint64 `json:"gold,omitempty"`
	Base     uint64 `json:"base_gold,omitempty"`
	Epoch    uint64 `json:"epoch,omitempty"`
	Command  string `json:"command,omitempty"`
	Rejected bool   `json:"old_write_rejected,omitempty"`
}

type ownershipFaultSQL struct {
	Epoch, Seq  [2]uint64
	Rows        [2]string
	Gold        uint64
	Bag, Ledger []byte
}

type ownershipFaultRig struct {
	ctx      context.Context
	dir, run string
	db       *sql.DB
	rc       *redis.Client
	ec       *clientv3.Client
}

func TestFullstackOwnershipFaults(t *testing.T) {
	if os.Getenv("OWNERSHIP_FAULT_REQUIRE") != "1" {
		t.Skip("未开启专用真实节点故障演练")
	}
	if os.Getenv("OWNERSHIP_FAULT_MODE") != "dev" {
		t.Fatal("只允许显式 dev 专用节点")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 420*time.Second)
	defer cancel()
	r := &ownershipFaultRig{ctx: ctx, dir: os.Getenv("OWNERSHIP_FAULT_DIR"), run: os.Getenv("OWNERSHIP_FAULT_RUN")}
	if !filepath.IsAbs(r.dir) || len(r.run) != 32 {
		t.Fatal("独立运行目录或运行号无效")
	}
	cfg, err := mysql.ParseDSN(os.Getenv("OWNERSHIP_FAULT_PLAYER_DSN"))
	if err != nil || cfg.DBName != "zone_990017_db" || cfg.Net != "tcp" {
		t.Fatal("只允许显式专用玩家库")
	}
	ownershipFaultLocal(t, cfg.Addr)
	r.db, err = openFenceAdminPool(t, ctx, cfg.FormatDSN(), cfg.DBName, true, 4, false)
	if err != nil {
		t.Fatal("专用 TiDB 强会话校验失败")
	}
	defer r.db.Close()
	addr := os.Getenv("OWNERSHIP_FAULT_REDIS")
	ownershipFaultLocal(t, addr)
	r.rc = redis.NewClient(&redis.Options{Addr: addr, Password: os.Getenv("OWNERSHIP_FAULT_REDIS_PASSWORD"), MaxRetries: -1, PoolSize: 2, DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second, ContextTimeoutEnabled: true})
	defer r.rc.Close()
	addr = os.Getenv("OWNERSHIP_FAULT_ETCD")
	ownershipFaultLocal(t, addr)
	r.ec, err = clientv3.New(clientv3.Config{Endpoints: []string{addr}, DialTimeout: time.Second})
	if err != nil {
		t.Fatal("专用 etcd 配置无效")
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
	if !t.Run("第二SQL_ACK延迟时交接等待", func(t *testing.T) { r.delay(t) }) {
		return
	}
	if !t.Run("真实旧实体恢复后旧写被拒", func(t *testing.T) { r.zombie(t) }) {
		return
	}
	r.command(t, "done")
	complete = true
}

func (r *ownershipFaultRig) delay(t *testing.T) {
	r.command(t, "start-delay")
	p := r.phase(t, "delay-ready", 45*time.Second)
	before := r.stableSQL(t, p.Player, 20*time.Second)
	if before.Gold != p.Base {
		t.Fatal("准备角色 SQL 基线与真实客户端不同")
	}
	owner, location := r.ownerLocation(t, p.Player)
	if location.GetZoneId() != 990017 || owner < before.Epoch[0] {
		t.Fatal("出发归属不在专用源区")
	}
	locBefore, err := proto.Marshal(location)
	if err != nil {
		t.Fatal(err)
	}
	barrierBefore := r.barrier(t, p.Player)
	// 正式 C++ 固定投递 first→second，DB 同 player 的 partition/sub-shard 串行消费。
	// 锁 second，才能观察 first 已 ACK、second 未 ACK；不根据猜测调换或手写 ACK。
	lockCtx, cancel := context.WithTimeout(r.ctx, 5*time.Second)
	defer cancel()
	tx, err := r.db.BeginTx(lockCtx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatal("第二表点锁事务失败")
	}
	defer tx.Rollback()
	var id uint64
	if err := tx.QueryRowContext(lockCtx, "SELECT player_id FROM player_database_1 WHERE player_id=? FOR UPDATE", p.Player).Scan(&id); err != nil || id != p.Player {
		t.Fatal("第二表主键点锁失败")
	}
	phaseCtx, stop := context.WithTimeout(lockCtx, 4500*time.Millisecond)
	defer stop()
	r.signal(t, "delay-go", ownershipFaultPhase{})
	var pending string
	ownershipFaultWait(t, phaseCtx, "等待真实 first ACK 与 second 延迟窗口", func() bool {
		v := r.barrier(t, p.Player)
		pending = v[0]
		return pending != "" && pending != barrierBefore[0] && v[1] == pending && v[2] != pending
	})
	// 跨过至少三次真实 scene 的 250ms 重查；期间 SQL 锁必须始终未超时。
	for i := 0; i < 4; i++ {
		if phaseCtx.Err() != nil {
			t.Fatal("锁预算已过，不能把自动释放后的状态算负向证据")
		}
		v := r.barrier(t, p.Player)
		nowOwner, nowLoc := r.ownerLocation(t, p.Player)
		raw, err := proto.Marshal(nowLoc)
		mark, markErr := r.rc.Exists(phaseCtx, ownerepoch.HandoffKey(p.Player)).Result()
		if err != nil || markErr != nil || v[0] != pending || v[1] != pending || v[2] == pending || nowOwner != owner || !bytes.Equal(raw, locBefore) || mark != 0 {
			t.Fatal("第二 SQL ACK 未齐时交接标记、owner 或 location 已改变")
		}
		select {
		case <-phaseCtx.Done():
			t.Fatal("负向观察截止")
		case <-time.After(250 * time.Millisecond):
		}
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal("显式释放第二表锁失败")
	}
	done := r.phase(t, "delay-done", 100*time.Second)
	after := r.stableSQL(t, p.Player, 20*time.Second)
	if done.Player != p.Player || done.Gold != p.Base+18 || after.Gold != p.Base+18 || !bytes.Equal(before.Bag, after.Bag) || !bytes.Equal(before.Ledger, after.Ledger) {
		t.Fatal("释放后往返没有精确保持一次资产变更及原件/收据")
	}
	r.signal(t, "delay-finish", ownershipFaultPhase{})
	r.signal(t, "delay-proof", ownershipFaultPhase{Player: p.Player, Gold: after.Gold, Base: p.Base, Epoch: after.Epoch[0]})
}

func (r *ownershipFaultRig) zombie(t *testing.T) {
	r.command(t, "start-source")
	p := r.phase(t, "zombie-source-baseline", 45*time.Second)
	owner, loc := r.ownerLocation(t, p.Player)
	if loc.GetZoneId() != 990017 {
		t.Fatal("旧实体归属不符")
	}
	var before ownershipFaultSQL
	baselineCtx, baselineStop := context.WithTimeout(r.ctx, 40*time.Second)
	defer baselineStop()
	ownershipFaultWait(t, baselineCtx, "当前旧实体真实30秒周期存盘双ACK", func() bool {
		before = r.readSQL(t, p.Player)
		b := r.barrier(t, p.Player)
		v := savebarrier.Version(before.Epoch[0], before.Seq[0])
		return before.Gold == p.Base && before.Epoch[0] == owner && before.Epoch[1] == owner && before.Seq[0] > 0 && before.Seq[0] == before.Seq[1] && b[0] == v && b[1] == v && b[2] == v
	})
	oldSession := r.onlineSession(t, p.Player)
	registration := r.registration(t, loc.GetNodeId(), loc.GetZoneId())
	if registration == "" {
		t.Fatal("旧节点没有真实 etcd 注册")
	}
	r.signal(t, "zombie-source-dirty", ownershipFaultPhase{})
	p = r.phase(t, "zombie-source-ready", 10*time.Second)
	if p.Gold != p.Base+11 || r.readSQL(t, p.Player).Gold != p.Base {
		t.Fatal("旧实体11金币并非仅在脏内存")
	}
	r.signal(t, "suspend-input", ownershipFaultPhase{Player: p.Player, Epoch: owner})
	r.command(t, "suspend")
	r.phase(t, "control-suspend", 10*time.Second)
	// 暂停成功后再读一次，封住“预检之后、NtSuspend之前周期存盘碰巧发出”的竞态。
	suspendedSQL := r.readSQL(t, p.Player)
	if suspendedSQL.Epoch != before.Epoch || suspendedSQL.Seq != before.Seq || suspendedSQL.Rows != before.Rows {
		t.Fatal("暂停前旧脏内存已被保存，故障窗口不成立")
	}
	// 只等真实注册租约自然消失，不 DEL 注册、不铸 owner、不伪造死亡标记。
	deadCtx, stop := context.WithTimeout(r.ctx, 145*time.Second)
	defer stop()
	ownershipFaultWait(t, deadCtx, "旧节点真实租约消失及20秒再入屏障", func() bool {
		current, err := r.ec.Get(deadCtx, registration)
		if err != nil || len(current.Kvs) != 0 {
			return false
		}
		raw, err := r.rc.Get(deadCtx, fmt.Sprintf("node:zone:%d:%s:death_at", loc.GetZoneId(), loc.GetNodeId())).Result()
		if err != nil {
			return false
		}
		ms, err := strconv.ParseInt(raw, 10, 64)
		return err == nil && ms > 0 && time.Since(time.UnixMilli(ms)) >= 20*time.Second
	})
	// 替换前必须确有旧 ONLINE 会话。不能把冷首登或手动补 session 算 ReplaceLogin。
	currentSession := r.onlineSession(t, p.Player)
	if currentSession.GetLoginGeneration() != oldSession.GetLoginGeneration() {
		t.Fatal("暂停期间会话已被其他登录改变")
	}
	r.command(t, "start-target")
	target := r.phase(t, "zombie-target-written", 45*time.Second)
	newOwner, newLoc := r.ownerLocation(t, p.Player)
	newSession := r.onlineSession(t, p.Player)
	if target.Player != p.Player || target.Base != p.Base || target.Gold != p.Base+7 || newLoc.GetZoneId() != 990018 || newOwner <= owner || newSession.GetLoginGeneration() <= oldSession.GetLoginGeneration() {
		t.Fatal("B 设备没有通过真实新登录代次及新 owner 接管")
	}
	r.signal(t, "zombie-target-flush", ownershipFaultPhase{})
	r.phase(t, "zombie-target-done", 15*time.Second)
	var committed ownershipFaultSQL
	commitCtx, finish := context.WithTimeout(r.ctx, 20*time.Second)
	defer finish()
	ownershipFaultWait(t, commitCtx, "B设备正式退出落库", func() bool {
		committed = r.readSQL(t, p.Player)
		return committed.Epoch[0] == newOwner && committed.Epoch[1] == newOwner && committed.Seq[0] > 0 && committed.Seq[0] == committed.Seq[1] && committed.Gold == p.Base+7
	})
	if !bytes.Equal(before.Bag, committed.Bag) || !bytes.Equal(before.Ledger, committed.Ledger) {
		t.Fatal("接管期间原件或资产收据改变")
	}
	r.command(t, "resume")
	r.phase(t, "control-resume", 10*time.Second)
	evidence := r.phase(t, "old-write-evidence", 45*time.Second)
	if !evidence.Rejected || evidence.Player != p.Player || evidence.Epoch != owner {
		t.Fatal("缺少恢复后本次真实旧实体实际写入被拒的证据；安全退出不算通过")
	}
	// 真实旧写拒绝之后仍连续观察，不能只比较恢复前的一次 SQL。
	for i := 0; i < 12; i++ {
		after := r.readSQL(t, p.Player)
		if after.Epoch != committed.Epoch || after.Seq != committed.Seq || after.Rows != committed.Rows {
			t.Fatal("旧实例恢复后覆盖了新 owner 的 SQL 或资产")
		}
		select {
		case <-r.ctx.Done():
			t.Fatal("收尾观察截止")
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
		t.Fatal("只允许本机隔离端点")
	}
}

func (r *ownershipFaultRig) signal(t *testing.T, name string, p ownershipFaultPhase) {
	t.Helper()
	p.Run = r.run
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal("阶段编码失败")
	}
	path := filepath.Join(r.dir, name+".json")
	if existing, err := os.ReadFile(path); err == nil {
		if bytes.Equal(existing, b) {
			return
		}
		t.Fatal("阶段文件已有冲突内容")
	}
	f, err := os.OpenFile(path+".tmp", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal("阶段文件已有或不可写")
	}
	_, e := f.Write(b)
	closeErr := f.Close()
	if e != nil || closeErr != nil || os.Rename(path+".tmp", path) != nil {
		t.Fatal("阶段文件提交失败")
	}
}

func (r *ownershipFaultRig) command(t *testing.T, command string) {
	t.Helper()
	// 每个命令仅创建一次；不覆盖正被 Windows 编排器打开的文件。
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
			t.Fatal("阶段文件无效或不属于本轮")
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
			t.Fatalf("真实故障演练阶段截止: %s", stage)
		}
		if check() {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("真实故障演练阶段截止: %s", stage)
		case <-ticker.C:
		}
	}
}

func (r *ownershipFaultRig) barrier(t *testing.T, id uint64) [3]string {
	t.Helper()
	v, err := r.rc.HMGet(r.ctx, savebarrier.Key(id), "pending", "player_database", "player_database_1").Result()
	if err != nil {
		t.Fatal("屏障强读失败")
	}
	var out [3]string
	for i := range out {
		if v[i] != nil {
			s, ok := v[i].(string)
			if !ok {
				t.Fatal("屏障字段错型")
			}
			if _, _, err := savebarrier.Parse(s); err != nil {
				t.Fatal("屏障字段畸形")
			}
			out[i] = s
		}
	}
	return out
}

func (r *ownershipFaultRig) ownerLocation(t *testing.T, id uint64) (uint64, *smpb.PlayerLocation) {
	t.Helper()
	owner, err := r.rc.Get(r.ctx, ownerepoch.OwnerEpochKey(id)).Uint64()
	if err != nil || owner == 0 {
		t.Fatal("真实owner缺失")
	}
	b, err := r.rc.Get(r.ctx, scenenode.LocationKey(id)).Bytes()
	loc := &smpb.PlayerLocation{}
	if err != nil || proto.Unmarshal(b, loc) != nil || loc.GetOwnerEpoch() != owner {
		t.Fatal("真实位置与owner不一致")
	}
	return owner, loc
}

func (r *ownershipFaultRig) onlineSession(t *testing.T, id uint64) *plpb.PlayerSession {
	t.Helper()
	b, err := r.rc.Get(r.ctx, fmt.Sprintf("player:session:%d", id)).Bytes()
	s := &plpb.PlayerSession{}
	if err != nil || proto.Unmarshal(b, s) != nil || s.GetPlayerId() != id || s.GetState() != plpb.PlayerSessionState_SESSION_STATE_ONLINE || s.GetLoginGeneration() == 0 {
		t.Fatal("缺少真实旧/新ONLINE会话，不能宣称ReplaceLogin")
	}
	return s
}

func (r *ownershipFaultRig) registration(t *testing.T, node string, zone uint32) string {
	t.Helper()
	result, err := r.ec.Get(r.ctx, scenenode.SceneNodeRpcPrefix, clientv3.WithPrefix())
	if err != nil {
		t.Fatal("真实注册表读取失败")
	}
	key := ""
	for _, kv := range result.Kvs {
		var v struct {
			Node uint32 `json:"nodeId"`
			Zone uint32 `json:"zoneId"`
		}
		if json.Unmarshal(kv.Value, &v) == nil && strconv.FormatUint(uint64(v.Node), 10) == node && v.Zone == zone {
			if key != "" {
				t.Fatal("节点身份有歧义")
			}
			key = string(kv.Key)
		}
	}
	return key
}

func (r *ownershipFaultRig) stableSQL(t *testing.T, id uint64, budget time.Duration) ownershipFaultSQL {
	t.Helper()
	ctx, cancel := context.WithTimeout(r.ctx, budget)
	defer cancel()
	var s ownershipFaultSQL
	ownershipFaultWait(t, ctx, "完整双SQL与双ACK基线", func() bool {
		s = r.readSQL(t, id)
		b := r.barrier(t, id)
		v := savebarrier.Version(s.Epoch[0], s.Seq[0])
		return s.Epoch[0] > 0 && s.Seq[0] > 0 && s.Epoch[0] == s.Epoch[1] && s.Seq[0] == s.Seq[1] && b[0] == v && b[1] == v && b[2] == v
	})
	return s
}

func (r *ownershipFaultRig) readSQL(t *testing.T, id uint64) ownershipFaultSQL {
	t.Helper()
	var s ownershipFaultSQL
	for i, table := range []string{"player_database", "player_database_1"} {
		ctx, cancel := context.WithTimeout(r.ctx, 2*time.Second)
		rows, err := r.db.QueryContext(ctx, "SELECT * FROM "+table+" WHERE player_id=?", id)
		if err != nil {
			cancel()
			t.Fatal("真实玩家SQL强读失败")
		}
		columns, err := rows.Columns()
		if err != nil || !rows.Next() {
			rows.Close()
			cancel()
			t.Fatal("真实玩家行缺失")
		}
		v := make([][]byte, len(columns))
		args := make([]any, len(v))
		for k := range v {
			args[k] = &v[k]
		}
		if rows.Scan(args...) != nil {
			rows.Close()
			cancel()
			t.Fatal("真实玩家行扫描失败")
		}
		h := sha256.New()
		foundEpoch, foundSeq := false, false
		for k, col := range columns {
			switch col {
			case "fence_owner_epoch":
				s.Epoch[i], err = strconv.ParseUint(string(v[k]), 10, 64)
				foundEpoch = true
			case "fence_save_seq":
				s.Seq[i], err = strconv.ParseUint(string(v[k]), 10, 64)
				foundSeq = true
			default:
				b, _ := json.Marshal(struct {
					Name  string
					Value []byte
				}{col, v[k]})
				h.Write(b)
				if i == 0 {
					switch col {
					case "currency":
						var c component.CurrencyComp
						if proto.Unmarshal(v[k], &c) != nil || len(c.Values) == 0 {
							rows.Close()
							cancel()
							t.Fatal("SQL金币编码无效")
						}
						s.Gold = c.Values[0]
					case "bag_component":
						s.Bag = append([]byte(nil), v[k]...)
					case "asset_op_ledger":
						s.Ledger = append([]byte(nil), v[k]...)
					}
				}
			}
			if err != nil {
				rows.Close()
				cancel()
				t.Fatal("SQL围栏编码无效")
			}
		}
		if !foundEpoch || !foundSeq || rows.Next() || rows.Err() != nil {
			rows.Close()
			cancel()
			t.Fatal("SQL身份或围栏不完整")
		}
		s.Rows[i] = hex.EncodeToString(h.Sum(nil))
		rows.Close()
		cancel()
	}
	return s
}
