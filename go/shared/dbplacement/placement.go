// Package dbplacement 是逻辑库到物理集群的映射:etcd 键 /mmorpg/db_placement/<逻辑库>,值 {cluster, version, state}。
//
// 进程按映射拨号(authoritysql.WithPlacement):Watcher 跟着 etcd 走,映射翻转之后旧连接被连接池丢弃,*sql.DB 句柄全程不变。
// 映射只由 tools/dbmove 写。设计见 docs/design/horizontal-scale-logical-db.md §8,操作见 docs/ops/db-move-runbook.md。
package dbplacement

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"regexp"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"

	"shared/authoritysql"
)

// KeyPrefix 是映射键的前缀,后面接逻辑库名。
const KeyPrefix = "/mmorpg/db_placement/"

// EnvConfig 是一次性工具读配置的环境变量,值是 Config 的 JSON。常驻服务读自己 yaml 的 DbPlacement 段。
const EnvConfig = "MMORPG_DB_PLACEMENT"

// DefaultClusterKey 记默认集群。首次部署时写入,之后配置和它不一致就拒启:没有映射键的库都在这个集群上,
// 配置里换一个默认集群等于让这些库的连接全部指向别处,而且没有人冻结过它们。
// 不在 KeyPrefix 下面:Watcher 和 List 按前缀读映射,这个键不是逻辑库。
const DefaultClusterKey = "/mmorpg/db_placement_default"

// EnvRequired 设成 1 时,没有 DbPlacement 段的 -migrate 拒绝运行。staging、prod 的 schema Job 由 k8s_deploy.ps1 注入。
// schema Job 的配置来自运维预置的 Secret,漏了这一段的话 -migrate 看不到 state=MOVING,会在搬迁中做 DDL。
const EnvRequired = "MMORPG_DB_PLACEMENT_REQUIRED"

// State 是一个逻辑库的搬迁状态。
type State string

const (
	// StateActive 是常态。
	StateActive State = "ACTIVE"
	// StateMoving:tools/dbmove 的 prepare 到翻转或回滚之间。这段时间复制在跑,-migrate 拒绝 DDL。
	StateMoving State = "MOVING"
)

// ErrMoving 表示这个逻辑库正在搬迁,此刻不能做 DDL。
var ErrMoving = errors.New("db placement: the logical database is being moved (state=MOVING); run schema migrations after the move")

// ErrConflict 表示 CompareAndPut 的版本条件不成立,这次写确定没有落地。
var ErrConflict = errors.New("db placement: the mapping changed concurrently")

// ErrPlacementRequired 表示这次 -migrate 必须能查到搬迁状态,但配置里没有 DbPlacement 段。
var ErrPlacementRequired = errors.New("db placement: the schema config has no DbPlacement section, so this migration cannot see state=MOVING; add the section to the schema config")

// ErrDefaultClusterChanged 表示配置的默认集群和 etcd 里记下的不一致。
var ErrDefaultClusterChanged = errors.New("db placement: DefaultCluster differs from the one recorded in etcd; logical databases without a mapping key live on the recorded cluster, move them with tools/dbmove instead of changing the config")

// FeedIdentity 是一条复制流的身份。复制断过之后重建,身份就变了:verify 比过的数据和之后复制过来的数据之间可能有一段缺口,
// 延迟回到 0 也看不出来。值相等才算同一条流,TiDB 档比 changefeed,MySQL 档比通道和它的来源。
type FeedIdentity struct {
	Changefeed string `json:"changefeed,omitempty"`
	StartTs    uint64 `json:"start_ts,omitempty"`
	CreateTime string `json:"create_time,omitempty"`
	Channel    string `json:"channel,omitempty"`
	SourceUUID string `json:"source_uuid,omitempty"`
	SourceHost string `json:"source_host,omitempty"`
	SourcePort uint32 `json:"source_port,omitempty"`
	// TargetCreated 是目标库闸门表的建表时间。从全量重来会删库重建,这时上面的字段可能都不变
	// (MySQL 的通道名和来源、TiDB 用 pause 和 resume 时的 changefeed),只有它会变。
	TargetCreated string `json:"target_created,omitempty"`
}

// Move 是 tools/dbmove 这一次搬迁的记录,写在映射值里。进程不读它。
type Move struct {
	RunID         string `json:"run_id"`
	SourceCluster string `json:"source_cluster"`
	TargetCluster string `json:"target_cluster"`
	PreparedMs    uint64 `json:"prepared_ms,omitempty"`
	// VerifiedMs 是 verify 比对的那个位置的时刻。TiDB 档取 primary_ts 的物理时间,MySQL 档取 verify 开始的时刻。
	VerifiedMs uint64 `json:"verified_ms,omitempty"`
	// VerifiedFeed 是 verify 时的复制流。cutover 比它,变了就要重跑 verify。
	VerifiedFeed *FeedIdentity `json:"verified_feed,omitempty"`
	// VerifiedPrimaryTs 是 verify 站的 TiCDC primary_ts,MySQL 档为 0。只用于排查。
	VerifiedPrimaryTs uint64 `json:"verified_primary_ts,omitempty"`
	// LockedAccounts 是 prepare 在目标上 ACCOUNT LOCK 的写者账号,cutover 按它解锁,rollback 按它再锁一遍。
	LockedAccounts []string `json:"locked_accounts,omitempty"`
	// SharedAccounts 是还在写目标集群上别的库、所以没锁的写者账号(-allow-shared-writer-accounts)。
	SharedAccounts []string `json:"shared_accounts,omitempty"`
	// AbortedMs 是最近一次切换窗口回滚的时刻。写它同时让窗口里还没落地的翻转 CAS 失效。
	AbortedMs    uint64 `json:"aborted_ms,omitempty"`
	RolledBackMs uint64 `json:"rolled_back_ms,omitempty"`
	FlippedMs    uint64 `json:"flipped_ms,omitempty"`
	AuditedMs    uint64 `json:"audited_ms,omitempty"`
	CleanedMs    uint64 `json:"cleaned_ms,omitempty"`
	// TargetPoint 是目标库闸门打开之前的位置(TiDB:TSO;MySQL:gtid_executed),audit 在这个位置上比校验和。
	// 打开闸门之前就写进映射:窗口崩在打开之后时,重跑要复用它,重读的话位置已经包含了那次打开。
	TargetPoint string `json:"target_point,omitempty"`
}

// Placement 是映射值。没有键时等于 {配置的默认集群, 0, ACTIVE}。
// Version 只在翻转时加 1,状态变化不动它:连接按版本判过期,prepare 和 rollback 不该让连接池换连接。
type Placement struct {
	Cluster string `json:"cluster"`
	Version uint64 `json:"version"`
	State   State  `json:"state"`
	Move    *Move  `json:"move,omitempty"`
}

var clusterNameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func (p Placement) validate() error {
	if !clusterNameRe.MatchString(p.Cluster) {
		return fmt.Errorf("db placement: bad cluster name %q", p.Cluster)
	}
	if p.State != StateActive && p.State != StateMoving {
		return fmt.Errorf("db placement: bad state %q", p.State)
	}
	return nil
}

// Decode 解析映射值。多余字段不拒绝(工具以后加字段时旧进程照常读),形状不对才报错。
func Decode(raw []byte) (Placement, error) {
	var p Placement
	if err := json.Unmarshal(raw, &p); err != nil {
		return Placement{}, fmt.Errorf("db placement: value is not valid JSON: %w", err)
	}
	if err := p.validate(); err != nil {
		return Placement{}, err
	}
	return p, nil
}

// Encode 校验并序列化映射值。
func Encode(p Placement) ([]byte, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	return json.Marshal(p)
}

// Key 返回逻辑库的映射键。
func Key(logicalDB string) string { return KeyPrefix + logicalDB }

// LogicalDBOfKey 从映射键取逻辑库名。
func LogicalDBOfKey(key string) (string, bool) {
	name, ok := strings.CutPrefix(key, KeyPrefix)
	return name, ok && name != ""
}

// SqlCluster 是一个物理集群。账号口令不在这里:沿用写者启动目录给的连接串,账号在目标集群提前建好。
type SqlCluster struct {
	// Hosts 是连接地址 host:port。多个 TiDB server 前面放负载均衡,这里写一个地址。
	Hosts string `json:"Hosts"`
}

// Config 是服务 yaml 的 DbPlacement 段。SqlClusters 为空 = 不按映射拨号,行为和没有这一段完全一样。
type Config struct {
	// DefaultCluster 是没有映射键的逻辑库所在的集群。
	DefaultCluster string                `json:"DefaultCluster,optional"`
	SqlClusters    map[string]SqlCluster `json:"SqlClusters,optional"`
	EtcdHosts      []string              `json:"EtcdHosts,optional"`
}

// Enabled 报告这个进程是否按映射拨号。
func (c Config) Enabled() bool { return len(c.SqlClusters) > 0 }

// Validate 校验配置形状,不连 etcd。没启用时恒为 nil。
func (c Config) Validate() error {
	if !c.Enabled() {
		return nil
	}
	if _, ok := c.SqlClusters[c.DefaultCluster]; !ok {
		return fmt.Errorf("db placement: DefaultCluster %q is not in SqlClusters", c.DefaultCluster)
	}
	if len(c.EtcdHosts) == 0 {
		return errors.New("db placement: EtcdHosts is empty")
	}
	for name, cluster := range c.SqlClusters {
		if !clusterNameRe.MatchString(name) {
			return fmt.Errorf("db placement: bad cluster name %q", name)
		}
		if _, _, err := net.SplitHostPort(cluster.Hosts); err != nil {
			return fmt.Errorf("db placement: cluster %s: Hosts must be host:port", name)
		}
	}
	return nil
}

// ConfigFromEnv 读一次性工具的配置。没设环境变量时返回零值(不按映射拨号)。
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	raw := getenv(EnvConfig)
	if raw == "" {
		return Config{}, nil
	}
	var c Config
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return Config{}, fmt.Errorf("db placement: %s is not valid JSON", EnvConfig)
	}
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

const etcdDialTimeout = 5 * time.Second

// Dial 建 etcd 客户端。工具和 Watcher 共用。
func Dial(cfg Config) (*clientv3.Client, error) {
	return clientv3.New(clientv3.Config{Endpoints: cfg.EtcdHosts, DialTimeout: etcdDialTimeout})
}

// Record 是读到的映射键。ModRevision 为 0 表示键不存在,这时 Placement 是零值。
type Record struct {
	Placement   Placement
	ModRevision int64
}

// Store 读写映射键。写只有 CompareAndPut 一种:翻转和状态变化都要带上读到的版本。
type Store struct {
	kv clientv3.KV
}

// NewStore 用 etcd 客户端建 Store。
func NewStore(kv clientv3.KV) *Store { return &Store{kv: kv} }

// Get 读一个逻辑库的映射。键不存在不是错误;值解析不了是错误。
func (s *Store) Get(ctx context.Context, logicalDB string) (Record, error) {
	resp, err := s.kv.Get(ctx, Key(logicalDB))
	if err != nil {
		return Record{}, err
	}
	if len(resp.Kvs) == 0 {
		return Record{}, nil
	}
	p, err := Decode(resp.Kvs[0].Value)
	if err != nil {
		return Record{}, err
	}
	return Record{Placement: p, ModRevision: resp.Kvs[0].ModRevision}, nil
}

// List 读全部映射。
func (s *Store) List(ctx context.Context) (map[string]Record, error) {
	resp, err := s.kv.Get(ctx, KeyPrefix, clientv3.WithPrefix())
	if err != nil {
		return nil, err
	}
	out := make(map[string]Record, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		name, ok := LogicalDBOfKey(string(kv.Key))
		if !ok {
			continue
		}
		p, err := Decode(kv.Value)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		out[name] = Record{Placement: p, ModRevision: kv.ModRevision}
	}
	return out, nil
}

// DefaultCluster 读 etcd 里记下的默认集群。还没有部署写过时 found=false。
func (s *Store) DefaultCluster(ctx context.Context) (cluster string, found bool, err error) {
	resp, err := s.kv.Get(ctx, DefaultClusterKey)
	if err != nil {
		return "", false, err
	}
	if len(resp.Kvs) == 0 {
		return "", false, nil
	}
	return string(resp.Kvs[0].Value), true, nil
}

// EnsureDefaultCluster 第一次部署时把配置的默认集群写进 etcd,之后只比对:不一致回 ErrDefaultClusterChanged。
// 并发的第一次部署由事务串行,写到不同值的那个拒启。配置没启用时什么都不做。
func EnsureDefaultCluster(ctx context.Context, kv clientv3.KV, cfg Config) error {
	if !cfg.Enabled() {
		return nil
	}
	resp, err := kv.Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(DefaultClusterKey), "=", 0)).
		Then(clientv3.OpPut(DefaultClusterKey, cfg.DefaultCluster)).
		Else(clientv3.OpGet(DefaultClusterKey)).
		Commit()
	if err != nil {
		return fmt.Errorf("db placement: record the default cluster: %w", err)
	}
	if resp.Succeeded {
		return nil
	}
	if len(resp.Responses) != 1 || resp.Responses[0].GetResponseRange() == nil || len(resp.Responses[0].GetResponseRange().Kvs) != 1 {
		return errors.New("db placement: unexpected answer while reading the default cluster")
	}
	return checkDefaultCluster(string(resp.Responses[0].GetResponseRange().Kvs[0].Value), cfg.DefaultCluster)
}

func checkDefaultCluster(recorded, configured string) error {
	if recorded != configured {
		return fmt.Errorf("%w (recorded %q, configured %q)", ErrDefaultClusterChanged, recorded, configured)
	}
	return nil
}

// RequireForMigration 在部署要求能查到搬迁状态时,拒绝没有 DbPlacement 段的 -migrate。
func RequireForMigration(cfg Config, getenv func(string) string) error {
	if !cfg.Enabled() && getenv(EnvRequired) == "1" {
		return ErrPlacementRequired
	}
	return nil
}

// CompareAndPut 在键的 ModRevision 等于 expected 时写入(0 = 键必须不存在)。
// 条件不成立回 ErrConflict,这次写确定没有落地;其余错误结局未知,可能已经落地。
func (s *Store) CompareAndPut(ctx context.Context, logicalDB string, expected int64, p Placement) error {
	raw, err := Encode(p)
	if err != nil {
		return err
	}
	key := Key(logicalDB)
	resp, err := s.kv.Txn(ctx).
		If(clientv3.Compare(clientv3.ModRevision(key), "=", expected)).
		Then(clientv3.OpPut(key, string(raw))).
		Commit()
	if err != nil {
		return err
	}
	if !resp.Succeeded {
		return ErrConflict
	}
	return nil
}

// fixedResolver 是读一次映射的解析器:一次性工具和 -migrate 用,不起 Watch。
type fixedResolver struct {
	addr    string
	version uint64
}

func (r fixedResolver) ResolvePlacement(string) (string, uint64, error) {
	return r.addr, r.version, nil
}

// Resolved 是读一次映射的结果。Addr 为空表示没启用映射。
type Resolved struct {
	Addr      string
	Placement Placement
}

// OpenOptions 给 authoritysql.Open 用:没启用映射时是空的。
func (r Resolved) OpenOptions() []authoritysql.OpenOption {
	if r.Addr == "" {
		return nil
	}
	return []authoritysql.OpenOption{authoritysql.WithPlacement(fixedResolver{addr: r.Addr, version: r.Placement.Version})}
}

type keyReader interface {
	Get(ctx context.Context, key string, opts ...clientv3.OpOption) (*clientv3.GetResponse, error)
}

// ResolveOnce 读一次映射。etcd 读不到时报错,不回退到默认集群:库可能已经搬走了。
func ResolveOnce(ctx context.Context, cfg Config, logicalDB string) (Resolved, error) {
	if !cfg.Enabled() {
		return Resolved{}, nil
	}
	if err := cfg.Validate(); err != nil {
		return Resolved{}, err
	}
	client, err := Dial(cfg)
	if err != nil {
		return Resolved{}, fmt.Errorf("db placement: connect etcd: %w", err)
	}
	defer client.Close()
	readCtx, cancel := context.WithTimeout(ctx, etcdDialTimeout)
	defer cancel()
	if err := EnsureDefaultCluster(readCtx, client, cfg); err != nil {
		return Resolved{}, err
	}
	return resolveWith(readCtx, client, cfg, logicalDB)
}

func resolveWith(ctx context.Context, reader keyReader, cfg Config, logicalDB string) (Resolved, error) {
	resp, err := reader.Get(ctx, Key(logicalDB))
	if err != nil {
		return Resolved{}, fmt.Errorf("db placement: read %s: %w", logicalDB, err)
	}
	p := Placement{Cluster: cfg.DefaultCluster, State: StateActive}
	if len(resp.Kvs) != 0 {
		if p, err = Decode(resp.Kvs[0].Value); err != nil {
			return Resolved{}, fmt.Errorf("db placement: %s: %w", logicalDB, err)
		}
	}
	cluster, ok := cfg.SqlClusters[p.Cluster]
	if !ok {
		return Resolved{}, fmt.Errorf("db placement: %s is on cluster %q, which SqlClusters does not list", logicalDB, p.Cluster)
	}
	return Resolved{Addr: cluster.Hosts, Placement: p}, nil
}

// ForMigration 是 -migrate 入口用的:state=MOVING 回 ErrMoving(设计 §8.3),否则返回连到当前集群的选项。
// 部署要求能查到搬迁状态(EnvRequired)而配置没有 DbPlacement 段时回 ErrPlacementRequired。
func ForMigration(ctx context.Context, cfg Config, logicalDB string) ([]authoritysql.OpenOption, error) {
	if err := RequireForMigration(cfg, os.Getenv); err != nil {
		return nil, err
	}
	resolved, err := ResolveOnce(ctx, cfg, logicalDB)
	if err != nil {
		return nil, err
	}
	if err := resolved.RefuseMoving(logicalDB); err != nil {
		return nil, err
	}
	return resolved.OpenOptions(), nil
}

// RefuseMoving 在 state=MOVING 时回 ErrMoving。
func (r Resolved) RefuseMoving(logicalDB string) error {
	if r.Placement.State == StateMoving {
		return fmt.Errorf("%w: %s", ErrMoving, logicalDB)
	}
	return nil
}

// ForOneShot 给一次性工具和启动期的短连接池用:连到当前集群,不看 state。
func ForOneShot(ctx context.Context, cfg Config, logicalDB string) ([]authoritysql.OpenOption, error) {
	resolved, err := ResolveOnce(ctx, cfg, logicalDB)
	if err != nil {
		return nil, err
	}
	return resolved.OpenOptions(), nil
}
