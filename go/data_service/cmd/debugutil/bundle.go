package debugutil

import (
	"fmt"
	"strconv"

	"data_service/internal/routing"

	assetpb "proto/common/asset"
	databasepb "proto/common/database"
	rollbackpb "proto/common/rollback"
	dbpb "proto/db"
	locatorpb "proto/player_locator"
	smpb "proto/scene_manager"

	"shared/assetop"
	"shared/ownerepoch"
	"shared/placement"
	"shared/playersync"
	"shared/savebarrier"
	"shared/scenenode"

	"github.com/luyuancpp/proto2mysql"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// ── 单玩家导出包(player-bundle)──────────────────────────────────────
//
// debug_fetch -mode player-bundle 写、debug_import 读的同一份文件。一份包回答「这名玩家此刻在
// 线上的权威数据是什么」:核心 Redis 里的父快照 / 存盘屏障 / 归属与在线状态、落点库里的两张玩家表、
// 全局库里的快照 / 流水 / 名字,以及工具据此给出的判读(analysis)。
//
// 格式只增不改:导入端按 FormatVersion 拒绝看不懂的版本;新增字段必须带 omitempty 或有零值语义。

const (
	BundleFormat        = "player-bundle"
	BundleFormatVersion = 1
)

// Bundle 是导出包的根。玩家数据只来自固定的只读查询;口令一律不进包。
type Bundle struct {
	Format         string         `json:"format"`
	FormatVersion  int            `json:"format_version"`
	PlayerID       uint64         `json:"player_id"`
	ExportedAtUTC  string         `json:"exported_at_utc"`
	Operator       string         `json:"operator"`
	Reason         string         `json:"reason"`
	ServerRevision string         `json:"server_revision,omitempty"`
	ToolRevision   string         `json:"tool_revision,omitempty"`
	Sources        BundleSources  `json:"sources"`
	Routing        BundleRouting  `json:"routing"`
	Redis          []RedisEntry   `json:"redis"`
	MySQL          BundleMySQL    `json:"mysql"`
	Global         *BundleGlobal  `json:"global,omitempty"`
	Analysis       BundleAnalysis `json:"analysis"`
}

// BundleSources 记录数据从哪几台实例读出(host:port/库),不含口令。*_id 是实例身份(Redis run_id、
// MySQL @@server_uuid),导入端据此拒绝「写回导出来源」——port-forward 后地址都是 127.0.0.1,只看地址分不出来。
// 取不到身份的一侧留空(TiDB 一律留空,见 MySQLServerIdentity);导入端对空身份 fail-closed,不当成「不同实例」
// (2026-10-22,r2-ds#1)。Redis run_id 只用来拒绝「相同」:线上切主、重启后 run_id 就变,「不同」证明不了写目标
// 是本地,导入端对 Redis 写目标另要本地标记(HasLocalRedisMarker)。
type BundleSources struct {
	CoreRedis      string `json:"core_redis"`
	CoreRedisID    string `json:"core_redis_id,omitempty"`
	MappingRedis   string `json:"mapping_redis"`
	MappingRedisID string `json:"mapping_redis_id,omitempty"`
	ZoneMySQL      string `json:"zone_mysql"`
	ZoneMySQLID    string `json:"zone_mysql_id,omitempty"`
	GlobalMySQL    string `json:"global_mysql,omitempty"`
	GlobalMySQLID  string `json:"global_mysql_id,omitempty"`
}

// BundleRouting 是按 go/db 同一规则(placement_route.go parsePlacementSnapshot + EffectiveStorage)解析出的有效落点库。
type BundleRouting struct {
	HomeZone        uint32 `json:"home_zone"`
	HomeZonePresent bool   `json:"home_zone_present"`
	PlacementRaw    string `json:"placement_raw,omitempty"`
	PlacementFrozen bool   `json:"placement_frozen,omitempty"`
	StorageID       uint32 `json:"storage_id"`
	StorageDB       string `json:"storage_db"`
	// ResolvedBy:placement | home_zone | fallback_zone | override(-database 显式指定)。
	ResolvedBy string `json:"resolved_by"`
	// Malformed 非空:映射或落点值畸形,线上 go/db 对该玩家读写全部 fail-closed;此时只能用 -database 指定要读的库。
	Malformed string `json:"malformed,omitempty"`
}

// RedisEntry 是一把键在同一次 MULTI/EXEC 里读到的类型、TTL 与值。
type RedisEntry struct {
	Key      string `json:"key"`
	Instance string `json:"instance"`
	Purpose  string `json:"purpose"`
	// Type 为 Redis TYPE 的结果;none 表示键不存在。
	Type string `json:"type"`
	// PTTLMs:-1 表示无 TTL,-2 表示键不存在。
	PTTLMs int64            `json:"pttl_ms"`
	Value  *Value           `json:"value,omitempty"`
	Hash   map[string]Value `json:"hash,omitempty"`
	// Member / MemberPresent 只用于「玩家是否在某个 zset 索引里」这类成员检查,不导出整个集合。
	Member        string `json:"member,omitempty"`
	MemberPresent bool   `json:"member_present,omitempty"`
	Error         string `json:"error,omitempty"`
}

// BundleMySQL 是落点库里该玩家的表行,同一个可重复读快照内读出。
type BundleMySQL struct {
	Database string     `json:"database"`
	Tables   []TableRow `json:"tables"`
	Error    string     `json:"error,omitempty"`
}

// TableRow 是一张玩家表的一行。Row 的原字节是整行消息的 proto 序列化(proto2mysql 与 go/db 同一套
// 行 ↔ 消息映射),导入端据此逐字段还原,不经 JSON 数字。
type TableRow struct {
	Table        string `json:"table"`
	ProtoType    string `json:"proto_type"`
	Found        bool   `json:"found"`
	Row          *Value `json:"row,omitempty"`
	FenceVersion string `json:"fence_version,omitempty"`
	// LiveExtraColumns:线上表有、本地检出的 proto 没有的列。SELECT 只列本地 proto 字段,这些列的数据**没有导出**;
	// 非空说明本地检出比线上旧,导入端拒绝 -source=sql。
	LiveExtraColumns []string `json:"live_extra_columns,omitempty"`
	Error            string   `json:"error,omitempty"`
}

// BundleGlobal 是全局库(data_service SnapshotMySQL)里与该玩家有关的记录,只供查看,导入端不使用。
type BundleGlobal struct {
	AccountRole     *Value            `json:"account_role,omitempty"` // 全局 SQL 目录原行；不含密码，可保留角色 PB 未知字段。
	Database        string            `json:"database"`
	PlayerName      any               `json:"player_name,omitempty"`
	Snapshots       []SnapshotItem    `json:"snapshots,omitempty"`
	TransactionLog  []any             `json:"transaction_log,omitempty"`
	RollbackAudit   []any             `json:"rollback_audit_log,omitempty"`
	Errors          map[string]string `json:"errors,omitempty"`
	TruncatedAtRows int               `json:"truncated_at_rows"`
}

// SnapshotItem 是一条 player_snapshot 行。source=1(C++ scene 经 Kafka 落库)的 data 列是
// PlayerSnapshotEntry,其中再嵌两张玩家表的 blob,这里逐层解开;source=0 的 data 列是 JSON 字段图。
type SnapshotItem struct {
	Meta            any    `json:"meta"`
	Entry           *Value `json:"entry,omitempty"`
	PlayerDatabase  *Value `json:"player_database,omitempty"`
	PlayerDatabase1 *Value `json:"player_database_1,omitempty"`
	DataJSON        any    `json:"data_json,omitempty"`
	DecodeError     string `json:"decode_error,omitempty"`
}

// BundleAnalysis 是工具对上面原始数据的判读,方便直接回答「存盘落库了没有 / 玩家在不在线」。
type BundleAnalysis struct {
	Online      string       `json:"online"`
	Barrier     BarrierState `json:"barrier"`
	ParentVsSQL []TableDiff  `json:"parent_vs_sql,omitempty"`
	Warnings    []string     `json:"warnings,omitempty"`
}

// BarrierState 摘出存盘屏障三个令牌、归属计数器,以及 login 冷加载会复核的另外几项(SQL 行围栏、父快照水位)。
type BarrierState struct {
	OwnerEpoch           string `json:"owner_epoch,omitempty"`
	Pending              string `json:"pending,omitempty"`
	PlayerDatabaseAck    string `json:"player_database_ack,omitempty"`
	PlayerDatabase1Ack   string `json:"player_database_1_ack,omitempty"`
	SavePendingIndexed   bool   `json:"save_pending_indexed"`
	PlayerDatabaseFence  string `json:"player_database_fence,omitempty"`
	PlayerDatabase1Fence string `json:"player_database_1_fence,omitempty"`
	ParentPresent        bool   `json:"parent_present"`
	ParentLastSaveSeq    uint64 `json:"parent_last_save_seq,omitempty"`
	OutboxTerminal       string `json:"outbox_terminal,omitempty"`
	OutboxTerminalReason string `json:"outbox_terminal_reason,omitempty"`
	Conclusion           string `json:"conclusion"`
	// Anomalies 是 login 冷加载会拒绝、但正常写路径不会产生的状态(人工改键、部分恢复、库被回档)。
	Anomalies []string `json:"anomalies,omitempty"`
}

// TableDiff 对比 Redis 父快照里的子消息与 SQL 行(两侧都忽略行内围栏两列)。
type TableDiff struct {
	Table           string   `json:"table"`
	ParentPresent   bool     `json:"parent_present"`
	SQLPresent      bool     `json:"sql_present"`
	Equal           bool     `json:"equal"`
	DifferentFields []string `json:"different_fields,omitempty"`
	SQLFence        string   `json:"sql_fence,omitempty"`
}

// ── 玩家表 ─────────────────────────────────────────────────────────────

// PlayerTable 描述落点库里一张以 player_id 为主键的玩家表。
type PlayerTable struct {
	New func() proto.Message
	// LoadPath:登录冷加载(login dataloader)读这张表,并按行内围栏对齐存盘屏障。导入只写这类表。
	LoadPath bool
}

// PlayerTables 的顺序即导入时取锁的顺序(按物理表名升序),多表同事务时不得随意调换。
var PlayerTables = []PlayerTable{
	{New: func() proto.Message { return &databasepb.PlayerCentreDatabase{} }},
	{New: func() proto.Message { return &databasepb.PlayerDatabase{} }, LoadPath: true},
	{New: func() proto.Message { return &databasepb.PlayerDatabase_1{} }, LoadPath: true},
}

// PhysicalTableName 取 proto option 声明的物理表名(与 go/db 建表同源)。
func PhysicalTableName(msg proto.Message) (string, error) {
	name, ok := proto2mysql.TableNameFromDescriptor(msg.ProtoReflect().Descriptor())
	if !ok || name == "" {
		return "", fmt.Errorf("%s 没有声明表名", msg.ProtoReflect().Descriptor().FullName())
	}
	return name, nil
}

const (
	fenceOwnerEpochField = "fence_owner_epoch"
	fenceSaveSeqField    = "fence_save_seq"
)

// RowFence 读出行内围栏;表没有围栏列时返回 ok=false。
func RowFence(msg proto.Message) (ownerEpoch, saveSeq uint64, ok bool) {
	fields := msg.ProtoReflect().Descriptor().Fields()
	epochField := fields.ByName(fenceOwnerEpochField)
	seqField := fields.ByName(fenceSaveSeqField)
	if epochField == nil || seqField == nil {
		return 0, 0, false
	}
	m := msg.ProtoReflect()
	return m.Get(epochField).Uint(), m.Get(seqField).Uint(), true
}

// SetRowFence 改写行内围栏;表没有围栏列时返回错误(调用方不该对它调用)。
func SetRowFence(msg proto.Message, ownerEpoch, saveSeq uint64) error {
	fields := msg.ProtoReflect().Descriptor().Fields()
	epochField := fields.ByName(fenceOwnerEpochField)
	seqField := fields.ByName(fenceSaveSeqField)
	if epochField == nil || seqField == nil {
		return fmt.Errorf("%s 没有行内围栏列", msg.ProtoReflect().Descriptor().FullName())
	}
	m := msg.ProtoReflect()
	m.Set(epochField, protoreflect.ValueOfUint64(ownerEpoch))
	m.Set(seqField, protoreflect.ValueOfUint64(saveSeq))
	return nil
}

// RowFenceVersion 返回行内围栏的屏障令牌形式;无围栏列或围栏为零时返回空串。
func RowFenceVersion(msg proto.Message) string {
	epoch, seq, ok := RowFence(msg)
	if !ok || (epoch == 0 && seq == 0) {
		return ""
	}
	return savebarrier.Version(epoch, seq)
}

// ── Redis 键清单 ───────────────────────────────────────────────────────

// Redis 实例的两种角色。今天所有部署里两者是同一台实例(DB 0),G4a 按用途拆实例落地后才会分开:
// 核心 = C++ zoneredis / go/db 的 ServerConfig.RedisClient;映射 = go/db 的 Placement.Redis(缺省同核心)。
const (
	RedisCore    = "core"
	RedisMapping = "mapping"
)

// RedisKind 决定在 MULTI 里用哪条读命令。
type RedisKind uint8

const (
	RedisKindString RedisKind = iota
	RedisKindHash
	// RedisKindZSetMember 只查玩家是否在索引里(ZSCORE),不导出整个集合。
	RedisKindZSetMember
	RedisKindCount
)

// RedisKeySpec 是一把要读的键。Decode / HashDecode 显式给出类型,不靠名字猜。
type RedisKeySpec struct {
	Key        string
	Instance   string
	Kind       RedisKind
	Purpose    string
	Decode     func() proto.Message
	HashDecode map[string]func() proto.Message
	// Redact 非 nil:解码后脱敏且不导出原字节(见 RenderProto)。
	Redact func(proto.Message)
	Member string
}

// ParentKey 是 C++ 存盘写的父快照键 PlayerAllData:{<id>}(玩家域 hash tag),经 savebarrier.ParentKey 构造。
func ParentKey(playerID uint64) string {
	return savebarrier.ParentKey(playerID)
}

// SubTableCacheKey 是 go/db 落库后回写的子表缓存键 {msg_type}:{id}(key_ordered_consumer.buildCacheKey)。
func SubTableCacheKey(msg proto.Message, playerID uint64) string {
	return string(msg.ProtoReflect().Descriptor().FullName()) + ":" + strconv.FormatUint(playerID, 10)
}

// SavePendingIndexKey 是 C++ 存盘把玩家挂进的待落库索引(zset),桶 = player_id % 64(player_lifecycle.cpp)。
func SavePendingIndexKey(homeZone uint32, playerID uint64) string {
	return fmt.Sprintf("player:save_pending:%d:%d", homeZone, playerID%64)
}

// 下面这些键的写者在其他 Go module(login / player_locator / scene_manager)或 data_service 的未导出函数里,
// 这里只能照抄格式,单测也只能钉住这份副本——写者改格式时测试不会变红。改格式的人搜键前缀能搜到这里:
// login login_constants.go / sessionmanager/login_generation.go / entergamelogic.go;player_locator keys.go;
// scene_manager leave_release.go;data_service routing/router.go(lock:player:)。
func playerToAccountKey(playerID uint64) string { return "player_to_account:" + u64(playerID) }
func playerLockerKey(playerID uint64) string    { return "player_locker:" + u64(playerID) }
func loginGenerationKey(playerID uint64) string {
	return "player:" + u64(playerID) + ":login_generation"
}
func mergeNoticeKey(playerID uint64) string { return "player_merge_notice:" + u64(playerID) }
func forceRenameKey(playerID uint64) string { return "player_force_rename:" + u64(playerID) }
func leaveReleaseWaitKey(playerID uint64) string {
	return "player:" + u64(playerID) + ":leave_release_wait"
}
func LocatorSessionKey(playerID uint64) string  { return "player:session:" + u64(playerID) }
func locatorLocationKey(playerID uint64) string { return "player:location:" + u64(playerID) }
func mappingLockKey(playerID uint64) string     { return "lock:player:" + u64(playerID) }

// AccountKey 是旧账号目录缓存键；当前归属只认全局 SQL account_role。
func AccountKey(account string) string { return "account:" + account }

// PlayerToAccountKey 是历史反向缓存；不授权入场、自愈或删除。
func PlayerToAccountKey(playerID uint64) string { return playerToAccountKey(playerID) }

func u64(v uint64) string { return strconv.FormatUint(v, 10) }

func newMsg[T any, P interface {
	*T
	proto.Message
}]() func() proto.Message {
	return func() proto.Message { return P(new(T)) }
}

// PlayerRedisKeySpecs 列出排查一名玩家要看的全部 Redis 键。homeZone 为 0 时跳过只能按 home 拼的键。
func PlayerRedisKeySpecs(playerID uint64, homeZone uint32) []RedisKeySpec {
	specs := []RedisKeySpec{
		{Key: ParentKey(playerID), Instance: RedisCore, Kind: RedisKindString,
			Purpose: "父快照:scene 存盘写入,Kafka 落库前最新的权威态(无 TTL)", Decode: newMsg[databasepb.PlayerAllData]()},
		{Key: SubTableCacheKey(&databasepb.PlayerDatabase{}, playerID), Instance: RedisCore, Kind: RedisKindString,
			Purpose: "子表缓存:go/db 落库后回写(带 TTL,冷加载不采信)", Decode: newMsg[databasepb.PlayerDatabase]()},
		{Key: SubTableCacheKey(&databasepb.PlayerDatabase_1{}, playerID), Instance: RedisCore, Kind: RedisKindString,
			Purpose: "子表缓存:go/db 落库后回写(带 TTL,冷加载不采信)", Decode: newMsg[databasepb.PlayerDatabase_1]()},
		{Key: SubTableCacheKey(&databasepb.BagAllData{}, playerID), Instance: RedisCore, Kind: RedisKindString,
			Purpose: "旧版缓存键(合服清理清单里的历史键,通常不存在)", Decode: newMsg[databasepb.BagAllData]()},
		{Key: SubTableCacheKey(&databasepb.QuestAllData{}, playerID), Instance: RedisCore, Kind: RedisKindString,
			Purpose: "旧版缓存键(通常不存在)", Decode: newMsg[databasepb.QuestAllData]()},
		{Key: SubTableCacheKey(&databasepb.MailAllData{}, playerID), Instance: RedisCore, Kind: RedisKindString,
			Purpose: "旧版缓存键(通常不存在)", Decode: newMsg[databasepb.MailAllData]()},
		{Key: savebarrier.Key(playerID), Instance: RedisCore, Kind: RedisKindHash,
			Purpose: "存盘屏障:pending 与两张表的 SQL ACK 相等 = 最近一次存盘两表都已落库",
			HashDecode: map[string]func() proto.Message{
				"outbox_player_database":   newMsg[dbpb.DBTask](),
				"outbox_player_database_1": newMsg[dbpb.DBTask](),
			}},
		{Key: assetop.SQLCommitKey(playerID), Instance: RedisCore, Kind: RedisKindHash,
			Purpose:    "资产 SQL 提交证据:go/db 确认资产结局已随玩家行提交",
			HashDecode: map[string]func() proto.Message{"data": newMsg[assetpb.PlayerAssetSQLCommit]()}},
		{Key: ownerepoch.OwnerEpochKey(playerID), Instance: RedisCore, Kind: RedisKindString,
			Purpose: "归属代次:只经 scene_manager INCR 前进"},
		{Key: scenenode.LocationKey(playerID), Instance: RedisCore, Kind: RedisKindString,
			Purpose: "场景位置权威键:所在 scene 节点 / zone / 写入时的归属代次", Decode: newMsg[smpb.PlayerLocation]()},
		{Key: ownerepoch.HandoffKey(playerID), Instance: RedisCore, Kind: RedisKindString,
			Purpose: "跨节点交接落盘标记 {epoch}:{saved_at_ms}(EX 300)"},
		{Key: leaveReleaseWaitKey(playerID), Instance: RedisCore, Kind: RedisKindString,
			Purpose: "离场持有者释放等待标记"},
		{Key: playersync.Key(playerID), Instance: RedisCore, Kind: RedisKindHash,
			Purpose: "落地恢复同步摘要计数"},
		{Key: LocatorSessionKey(playerID), Instance: RedisCore, Kind: RedisKindString,
			Purpose: "player_locator 会话(在线 / 断线租约)", Decode: newMsg[locatorpb.PlayerSession]()},
		{Key: locatorLocationKey(playerID), Instance: RedisCore, Kind: RedisKindString,
			Purpose: "player_locator 位置", Decode: newMsg[locatorpb.PlayerLocation]()},
		{Key: loginGenerationKey(playerID), Instance: RedisCore, Kind: RedisKindString,
			Purpose: "登录代次(顶号栅栏)"},
		{Key: playerLockerKey(playerID), Instance: RedisCore, Kind: RedisKindString,
			Purpose: "login EnterGame 进场锁"},
		{Key: playerToAccountKey(playerID), Instance: RedisCore, Kind: RedisKindString,
			Purpose: "历史角色反向缓存(不授权入场)"},
		{Key: mergeNoticeKey(playerID), Instance: RedisCore, Kind: RedisKindString,
			Purpose: "合服通知标记"},
		{Key: forceRenameKey(playerID), Instance: RedisCore, Kind: RedisKindString,
			Purpose: "合服强制改名标记"},
		{Key: placement.HomeZoneKey(playerID), Instance: RedisMapping, Kind: RedisKindString,
			Purpose: "home_zone 映射(合服后以它为准,不以玩家看到的服号为准)"},
		{Key: placement.Key(playerID), Instance: RedisMapping, Kind: RedisKindString,
			Purpose: "存储落点记录 {storage}:{version}[:frozen:{run}];没有则落点 = home_zone 库"},
		{Key: placement.MapRevKey(playerID), Instance: RedisMapping, Kind: RedisKindString,
			Purpose: "映射修订号"},
		{Key: mappingLockKey(playerID), Instance: RedisMapping, Kind: RedisKindString,
			Purpose: "映射分布式锁"},
		{Key: routing.PlayerNameKey(playerID), Instance: RedisMapping, Kind: RedisKindString,
			Purpose: "名字缓存"},
	}
	if homeZone != 0 {
		specs = append(specs,
			RedisKeySpec{Key: SavePendingIndexKey(homeZone, playerID), Instance: RedisCore, Kind: RedisKindZSetMember,
				Member: u64(playerID), Purpose: "待落库索引:存盘已写父键、DBTask 尚未两表确认时在里面"},
			RedisKeySpec{Key: placement.MergeFenceKey(homeZone), Instance: RedisMapping, Kind: RedisKindString,
				Purpose: "home_zone 的合服闸门(存在 = 合服进行中)"},
		)
	}
	return specs
}

// AccountRedisKeySpec 是旧账号目录缓存键。值里的 password 一律清掉,且不导出原字节(只读查看)。
// debug_import 不读、不写这把键;角色归属只经全局 SQL 目录 ImportRole 按角色行写入(2026-10-22 核实,r2 账号目录疑点)。
func AccountRedisKeySpec(account string) RedisKeySpec {
	return RedisKeySpec{
		Key: AccountKey(account), Instance: RedisCore, Kind: RedisKindString,
		Purpose: "旧账号目录缓存(只读查看;当前归属只认全局 SQL account_role;password 已脱敏、不可导回)",
		Decode:  newMsg[databasepb.UserAccounts](),
		Redact: func(msg proto.Message) {
			if accounts, ok := msg.(*databasepb.UserAccounts); ok {
				accounts.Password = ""
			}
		},
	}
}

// SnapshotEntryDecoders 给 player_snapshot(source=1)解两层用。
func SnapshotEntryDecoders() (entry, db, db1 func() proto.Message) {
	return newMsg[rollbackpb.PlayerSnapshotEntry](), newMsg[databasepb.PlayerDatabase](), newMsg[databasepb.PlayerDatabase_1]()
}
