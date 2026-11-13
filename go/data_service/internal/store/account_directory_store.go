package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"

	base "proto/common/base"
	dbpb "proto/common/database"
	dspb "proto/data_service"
	"shared/accountdirectory"
	"shared/authoritysql"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

const AccountDirectoryTableName = "account_directory"
const AccountRoleTableName = "account_role"
const accountDirectoryDDL = "CREATE TABLE IF NOT EXISTS account_directory (account VARBINARY(191) NOT NULL COMMENT 'pb:1',created_ms BIGINT UNSIGNED NOT NULL COMMENT 'pb:2',PRIMARY KEY(account)) ENGINE=InnoDB"
const accountRoleDDL = "CREATE TABLE IF NOT EXISTS account_role (player_id BIGINT UNSIGNED NOT NULL COMMENT 'pb:1',account VARBINARY(191) NOT NULL COMMENT 'pb:2',role_proto MEDIUMBLOB NOT NULL COMMENT 'pb:3',owner_revision BIGINT UNSIGNED NOT NULL COMMENT 'pb:4',updated_ms BIGINT UNSIGNED NOT NULL COMMENT 'pb:5',trade_listing_id BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT 'pb:6',initial_player_commit BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT 'pb:7',fence_version BIGINT UNSIGNED NOT NULL DEFAULT 1 COMMENT 'pb:8',prepare_token BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT 'pb:9',PRIMARY KEY(player_id) /*T![clustered_index] NONCLUSTERED */) ENGINE=InnoDB /*T! SHARD_ROW_ID_BITS=4 PRE_SPLIT_REGIONS=4 */"

// AccountDirectoryStore 拥有账号目录和角色归属(docs/design/horizontal-scale-logical-db.md §5)。不访问认证 password 或 Redis。
//
// 目录分片:账号目录和 ref 按账号,角色行、入场登记、过户收据按玩家。事务只开在一个逻辑库上,跨片一律分步,每步幂等。
// 真相永远是玩家分片的 account_role.account,ref 只是索引,见 account_role_ref.go。
type AccountDirectoryStore struct {
	shards   *shardPools
	steps    directorySteps
	backend  authoritysql.Backend
	maxRoles int
	fences   FenceCoordinator // 过户和删角的两阶段,nil 时都拒绝。见 ConfigureFenceCoordinator
	cfg      MySQLConfig
	stores   *PlayerStorePools // 按 storage_id 的存档库连接池,见 ConfigurePlayerStores
	// names 是名字登记表:建角提交之后绑定名字、删角写完墓碑之后释放名字(account_role_name.go)。nil 时两步都不做,见 ConfigurePlayerNames。
	names roleNameRegistry
	// nameCache 是名字读缓存:目录补回或删掉名字行之后删缓存,nil 时不删(account_directory_events.go,ConfigureNameCache)。
	nameCache NameCacheInvalidator
	// events 收「失败只记日志、不改结局」那几步的计数,nil 时只记日志(account_directory_events.go,ConfigureEvents)。
	events DirectoryEvents
	// 读路径发现的陈旧 ref,等后台清。
	staleRefs chan staleRefKey
	// directoryReads 是目录每一片留给账本、展示读的名额(readSlots),下标是片号。存档库那一侧的名额在 stores 里。
	directoryReads []readSlots
	// removalCursor 让删角收尾逐轮换起点。
	removalCursor atomic.Uint64
	// preparingCursor 让超时 PREPARING 的撤销逐轮换起点。
	preparingCursor atomic.Uint64
}

// maxOpenConn 是每片连接池的上限(config.AccountDirectoryConfig.PoolMaxOpenConn),显式传入而不读 cfg.MaxOpenConn:
// cfg 是共用的目录连接参数,那里的 MaxOpenConn 属于快照池。越界由 authoritysql.Open 拒绝。
func NewAccountDirectoryStore(cfg MySQLConfig, backend authoritysql.Backend, maxRoles, maxOpenConn int) (*AccountDirectoryStore, error) {
	if maxRoles < 1 || maxRoles > maxRefRows {
		return nil, accountdirectory.ErrInvalid
	}
	shards, err := openShardPools(cfg, func(one MySQLConfig) (*sql.DB, error) {
		openCtx, cancelOpen := context.WithTimeout(context.Background(), mysqlPingTimeout)
		db, err := authoritysql.Open(openCtx, buildDSN(one), one.DBName, backend, maxOpenConn, one.Permission, one.PoolOptions...)
		cancelOpen()
		if err != nil {
			return nil, err
		}
		// 权威连接池核对与业务表形状核对各用自己的有界启动预算。
		shapeCtx, cancelShape := context.WithTimeout(context.Background(), mysqlPingTimeout)
		defer cancelShape()
		if err = assertAccountDirectoryShape(shapeCtx, db, one.DBName); err != nil {
			_ = db.Close()
			return nil, err
		}
		return db, nil
	})
	if err != nil {
		return nil, err
	}
	// 每片一个池,每个池一份读名额;maxOpenConn 就是每片池的上限。
	directoryReads := make([]readSlots, shards.k)
	for slot := range directoryReads {
		directoryReads[slot] = newReadSlots(maxOpenConn)
	}
	s := &AccountDirectoryStore{shards: shards, backend: backend, maxRoles: maxRoles, cfg: cfg,
		staleRefs:      make(chan staleRefKey, staleRefQueueSize),
		directoryReads: directoryReads}
	s.steps = &sqlDirectorySteps{shards: shards, directory: s}
	return s, nil
}

// Close 关闭目录各片和已打开的存档库。
func (s *AccountDirectoryStore) Close() error {
	return errors.Join(s.shards.Close(), s.stores.Close())
}

func assertAccountDirectoryShape(ctx context.Context, db *sql.DB, name string) error {
	// 保持同一物理会话，避免目录形状的每条只读查询重复核验会话。
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := assertInitialPlayerCommitShape(ctx, conn, name); err != nil {
		return err
	}
	for _, shape := range []struct {
		model proto.Message
		table string
		keys  []string
	}{
		{&dbpb.AccountDirectory{}, AccountDirectoryTableName, []string{"account"}},
		{&dbpb.AccountRole{}, AccountRoleTableName, []string{"player_id"}},
		{&dbpb.AccountRoleRemoval{}, AccountRoleRemovalTableName, []string{"player_id"}},
		{&dbpb.AccountRoleRef{}, AccountRoleRefTableName, []string{"account", "player_id"}},
	} {
		if err := assertColumnsPresent(ctx, conn, name, shape.model, shape.table); err != nil {
			return err
		}
		keys, err := primaryKeyColumns(ctx, conn, name, shape.table)
		if err != nil {
			return err
		}
		if !slices.Equal(keys, shape.keys) {
			return fmt.Errorf("账号目录主键不符: %s", shape.table)
		}
		var typ string
		var width sql.NullInt64
		if err := conn.QueryRowContext(ctx, "SELECT DATA_TYPE,CHARACTER_MAXIMUM_LENGTH FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=? AND TABLE_NAME=? AND COLUMN_NAME='account'", name, shape.table).Scan(&typ, &width); err != nil {
			return err
		}
		if typ != "varbinary" || !width.Valid || width.Int64 != 191 {
			return fmt.Errorf("账号目录必须逐字节比较: %s", shape.table)
		}
	}
	return nil
}

// playerTx 在这名玩家所在的片上跑一个事务。
func (s *AccountDirectoryStore) playerTx(ctx context.Context, player uint64, action func(*sql.Tx) error) error {
	return shardTx(ctx, s.shards.forPlayer(player), action)
}

func lockDirectory(ctx context.Context, tx *sql.Tx, account string, create bool) error {
	if !accountdirectory.ValidAccount(account) {
		return accountdirectory.ErrInvalid
	}
	if create {
		if _, err := tx.ExecContext(ctx, "INSERT INTO account_directory(account,created_ms) VALUES (?,UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000) ON DUPLICATE KEY UPDATE account=account", []byte(account)); err != nil {
			return err
		}
	}
	var stored []byte
	err := tx.QueryRowContext(ctx, "SELECT account FROM account_directory WHERE account=? FOR UPDATE", []byte(account)).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return accountdirectory.ErrMissing
	}
	return err
}

type directoryQuery interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// Read 读账号的角色列表。登录每次都走这里,所以先无锁读,目录行已在就只读返回,不碰任何行锁
// (docs/design/player-data-tidb-scaling-plan.md §8.1 第 2 条)。
// 只有 create=true 且目录行不存在才进入加锁建行的路径。
//
// 读 ref,按玩家分片批量读 account_role,只留真相是这个账号的。ref 在提交点之前写,所以提交之后才开始的 Read 一定读得到
// (强读会话,TiDB 读 leader、MySQL 档连主库)。调用方不依赖和在途操作排队:列表只用来展示和选角,改归属的操作在各自事务里
// 按 owner_revision 复核,入场用 Lookup 强读复核;建角超时的恢复是先 Lookup 确认已提交,再 Read。
// 目录行只增不删,所以目录行与 ref 分两条语句读没有一致性问题。
func (s *AccountDirectoryStore) Read(ctx context.Context, account string, create bool) (*base.AccountSimplePlayerList, error) {
	if !accountdirectory.ValidAccount(account) {
		return nil, accountdirectory.ErrInvalid
	}
	exists, refs, err := s.steps.readAccount(ctx, account)
	if err != nil {
		return nil, err
	}
	if !exists {
		if !create {
			return nil, accountdirectory.ErrMissing
		}
		if err := s.steps.ensureAccount(ctx, account); err != nil {
			return nil, err
		}
		// 刚建的目录行上可能已经有并发建角写的 ref。
		if _, refs, err = s.steps.readAccount(ctx, account); err != nil {
			return nil, err
		}
	}
	return s.rolesOfRefs(ctx, account, refs)
}

// readRoleList 读当前列表。写操作提交之后用它回应答:读失败时调用方把结果当未知,按 player_id 强读消歧。
func (s *AccountDirectoryStore) readRoleList(ctx context.Context, account string) (*base.AccountSimplePlayerList, error) {
	_, refs, err := s.steps.readAccount(ctx, account)
	if err != nil {
		return nil, err
	}
	return s.rolesOfRefs(ctx, account, refs)
}

// rolesOfRefs 按真相过滤 ref。陈旧的 ref 不进列表,交给后台清。结果按 player_id 升序。
func (s *AccountDirectoryStore) rolesOfRefs(ctx context.Context, account string, refs []roleRef) (*base.AccountSimplePlayerList, error) {
	out := &base.AccountSimplePlayerList{}
	if len(refs) == 0 {
		return out, nil
	}
	ids := make([]uint64, 0, len(refs))
	for _, ref := range refs {
		ids = append(ids, ref.PlayerID)
	}
	owners, err := s.steps.readOwners(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, ref := range refs {
		if owned := owners[ref.PlayerID]; owned != nil && owned.Account == account {
			out.Players = append(out.Players, owned.Role)
			continue
		}
		// 已标保留的不再排队:它们在后台的复核名额里轮着重判,排进来只会占读路径队列的位置(SweepStaleRefs 第 1 段也会跳过)。
		if ref.HeldMs == 0 {
			s.noteStaleRef(account, ref.PlayerID)
		}
	}
	return out, nil
}

const ownershipColumns = "account,role_proto,owner_revision,trade_listing_id,prepare_token"

// lockOwnershipSQL 锁角色行,读归属和两个令牌。
const lockOwnershipSQL = "SELECT " + ownershipColumns + " FROM account_role WHERE player_id=? FOR UPDATE"

func scanOwnership(row interface{ Scan(...any) error }, id uint64) (*dspb.AccountRoleOwnership, error) {
	var account string
	var raw []byte
	var rev, listing, prepare uint64
	if err := row.Scan(&account, &raw, &rev, &listing, &prepare); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, accountdirectory.ErrMissing
		}
		return nil, err
	}
	return decodeOwnership(id, account, raw, rev, listing, prepare)
}

// decodeOwnership 还原一行归属。墓碑不带令牌,准备令牌进 FREEZING 时才换成最终令牌,两个不会同时非零。
func decodeOwnership(id uint64, account string, raw []byte, rev, listing, prepare uint64) (*dspb.AccountRoleOwnership, error) {
	role := &base.AccountSimplePlayer{}
	if len(raw) > 16384 {
		return nil, errors.New("角色归属原件越界")
	}
	if err := proto.Unmarshal(raw, role); err != nil || role.PlayerId != id || id == 0 || rev == 0 || (account != "" && !accountdirectory.ValidAccount(account)) || (account == "" && (listing != 0 || prepare != 0)) || (listing != 0 && prepare != 0) {
		return nil, errors.New("角色归属原件损坏")
	}
	return &dspb.AccountRoleOwnership{Account: account, Role: role, OwnerRevision: rev, TradeListingId: listing, PrepareToken: prepare}, nil
}

// Lookup 强读玩家所在分片。墓碑算不存在。InitialPlayerCommit 照实回:摘孤儿凭它分辨「现行建角流程建的、只缺初始行」(该修,不该删)。
//
// 入场和归属判定只看 trade_listing_id。准备令牌还在投票,撤销了角色就回来了,不出目录:调用方看到的是这名角色没有冻结,
// 玩家照常进场。这时进场的实例在转 FREEZING 的事务里按同一把行锁快照进 holder,此后 Register 才被挡,规格 §3.5。
func (s *AccountDirectoryStore) Lookup(ctx context.Context, id uint64) (*dspb.AccountRoleOwnership, error) {
	if id == 0 {
		return nil, accountdirectory.ErrInvalid
	}
	owned, err := s.steps.readOwner(ctx, id)
	if err != nil {
		return nil, err
	}
	if owned == nil || owned.Account == "" {
		return nil, accountdirectory.ErrMissing
	}
	owned.PrepareToken = 0
	return owned, nil
}

func validRole(role *base.AccountSimplePlayer) bool {
	return role != nil && role.PlayerId != 0 && proto.Size(role) <= 16384
}

func (s *AccountDirectoryStore) Create(ctx context.Context, account string, role *base.AccountSimplePlayer) (*base.AccountSimplePlayerList, error) {
	out, err := s.CreateWithInitialCommit(ctx, account, role)
	if err != nil {
		return nil, err
	}
	return out.Players, nil
}

// CreateWithInitialCommit 严格新建。提交点之前的失败回 ErrNotCommitted,未知提交由原 Lookup 恢复。
func (s *AccountDirectoryStore) CreateWithInitialCommit(ctx context.Context, account string, role *base.AccountSimplePlayer) (*dspb.AccountDirectoryResponse, error) {
	players, err := s.createRoleWithInitialRows(ctx, account, role)
	if err != nil {
		return nil, err
	}
	return &dspb.AccountDirectoryResponse{Players: players}, nil
}

// createRoleWithInitialRows 是建角的四步(规格 §5.3),每步幂等:
//
//  1. 账号分片事务:锁目录行,核上限,写 ref(PENDING)。
//  2. 存档库:写初始行。目录没提交时没人读得到,失败只留下孤儿行。
//  3. 玩家分片事务:核路由行,严格插入 account_role。这是提交点。
//     提交之后在名字片把名字行从预留置为已绑定(bindRoleNameBestEffort,先复核角色确已提交)。失败不改结局,重放或核账补上。
//  4. 账号分片:ref 改 ACTIVE。失败无害。
//
// 第 3 步之前的失败都回 ErrNotCommitted,第 3 步 BEGIN 就被拒绝也算:这次调用没有写角色行,这次写的 ref 当场删掉,立刻退还名额。
// 第 3 步之后的失败才是结局未知,login 按 player_id 强读消歧。两者不能混:login 收到 ErrNotCommitted 就释放名字。
//
// 第 3 步之后重放走预读那条路,不再碰存档:玩家可能已经存过盘。
// 同账号同角色的重放补绑名字、补第 4 步,读回列表;归属不同、已挂寄售或已删除(墓碑)一律冲突,删除后的角色不能复活。
func (s *AccountDirectoryStore) createRoleWithInitialRows(ctx context.Context, account string, role *base.AccountSimplePlayer) (*base.AccountSimplePlayerList, error) {
	if !validRole(role) || !accountdirectory.ValidAccount(account) {
		return nil, accountdirectory.ErrInvalid
	}
	raw, err := proto.Marshal(role)
	if err != nil {
		return nil, notCommitted(err)
	}
	old, err := s.steps.readOwner(ctx, role.PlayerId)
	if err != nil {
		return nil, notCommitted(err)
	}
	if old != nil {
		if err := checkCreateReplay(old, account, role); err != nil {
			return nil, err
		}
		s.bindRoleNameBestEffort(ctx, role)
		s.activateRefBestEffort(ctx, account, role.PlayerId)
		return s.readRoleList(ctx, account)
	}
	storage, err := s.initialStorage(ctx, role)
	if err != nil {
		return nil, notCommitted(err)
	}
	ref := roleRef{PlayerID: role.PlayerId, HomeZone: role.ZoneId, Nonce: newRefNonce()}
	if err := s.reserveRefWithEviction(ctx, account, ref); err != nil {
		if !errors.Is(err, accountdirectory.ErrFull) {
			// 这一步本身可能结局未知:ref 写没写成都按这次的 nonce 删。满位时什么都没写。
			s.releaseOwnRef(ctx, account, ref)
		}
		return nil, notCommitted(err)
	}
	if err := s.steps.writeInitialRows(ctx, storage, role); err != nil {
		s.releaseOwnRef(ctx, account, ref)
		return nil, notCommitted(err)
	}
	if err := s.steps.commitRole(ctx, account, role, raw, storage); err != nil {
		switch {
		case errors.Is(err, accountdirectory.ErrNotCommitted):
			s.releaseOwnRef(ctx, account, ref)
		case errors.Is(err, accountdirectory.ErrConflict):
			// 别人先建了或已删除:这条 ref 现在就能证明陈旧。证明不了的(路由不符)留给后台。
			s.reapOwnRef(ctx, account, role.PlayerId)
		}
		return nil, err
	}
	s.bindRoleNameBestEffort(ctx, role)
	s.activateRefBestEffort(ctx, account, role.PlayerId)
	return s.readRoleList(ctx, account)
}

// notCommitted 把提交点之前的失败标成确定没写。已经是确定拒绝的原样回:login 对它们有各自的处理。
func notCommitted(err error) error {
	if errors.Is(err, accountdirectory.ErrConflict) || errors.Is(err, accountdirectory.ErrFull) || errors.Is(err, accountdirectory.ErrInvalid) {
		return err
	}
	return fmt.Errorf("%w: %w", accountdirectory.ErrNotCommitted, err)
}

// definiteRejections 是 RPC 层映射成确定性拒绝的哨兵(directoryError):调用方看到它们就认为这笔被拒绝了。
var definiteRejections = []error{accountdirectory.ErrConflict, accountdirectory.ErrMissing, accountdirectory.ErrFull, accountdirectory.ErrInvalid, accountdirectory.ErrNotCommitted}

// notRejection 用在已经写了东西之后的失败上:调用方把确定性的拒绝当成「这笔没写」,所以这时改成 ErrRetryLater,让它重放(规格 §3.6)。
// 这些哨兵都是事务回滚之后才回的:这一步确定没写,前面的提交结局已知,排空证明不用 Hold。不留错误链:链上有哨兵的话 RPC 层还是按它映射。
// 别的错误(SQL 失败、提交结局未知)原样回,RPC 层回 Unavailable,按结局未知算。
func notRejection(err error) error {
	for _, rejection := range definiteRejections {
		if errors.Is(err, rejection) {
			return fmt.Errorf("%w: %v", accountdirectory.ErrRetryLater, err)
		}
	}
	return err
}

// readFailed 用在只读步骤的失败上,这一步没有写。确定性的拒绝原样回,其余回 ErrRetryLater:排空证明不用 Hold(规格 §3.6)。
// 这次调用前面写过东西也成立:走到这里说明那些提交的结局都已知,结局未知的提交在原地就返回了。原因留在错误链上。
func readFailed(err error) error {
	if err == nil {
		return nil
	}
	for _, rejection := range definiteRejections {
		if errors.Is(err, rejection) {
			return err
		}
	}
	return fmt.Errorf("%w: %w", accountdirectory.ErrRetryLater, err)
}

// activateRefBestEffort 是第 4 步:失败无害,ref 还在,读路径照常按真相过滤,后台会补。
func (s *AccountDirectoryStore) activateRefBestEffort(ctx context.Context, account string, player uint64) {
	if err := s.steps.activateRef(ctx, account, player, 0); err != nil {
		logx.Errorf("[account-role-ref] activate failed, the background sweep retries: player=%d: %v", player, err)
	}
}

// checkCreateReplay 判一个已有的角色行是不是这次建角的重放。令牌在投票也算有令牌:建角重放不该把这名角色当成空闲的回给 login。
func checkCreateReplay(old *dspb.AccountRoleOwnership, account string, role *base.AccountSimplePlayer) error {
	if old.Account == "" || old.TradeListingId != 0 || old.PrepareToken != 0 || old.Account != account || !proto.Equal(old.Role, role) {
		return accountdirectory.ErrConflict
	}
	return nil
}

// initialStorage 给建角选存档库:只认权威路由,和 go/db 一样不回落 home_zone,猜一个库写初始行比拒绝更糟。
func (s *AccountDirectoryStore) initialStorage(ctx context.Context, role *base.AccountSimplePlayer) (uint32, error) {
	if role.GetZoneId() == 0 {
		return 0, accountdirectory.ErrUnavailable
	}
	route, found, err := s.steps.readRoute(ctx, role.PlayerId)
	if err != nil {
		return 0, err
	}
	if !found || !route.HasHome() {
		return 0, fmt.Errorf("%w: player %d has no route", accountdirectory.ErrUnavailable, role.PlayerId)
	}
	if route.HomeZone != role.ZoneId || route.Rev == 0 || !route.HasPlacement() {
		return 0, accountdirectory.ErrConflict
	}
	if !s.steps.storageAllowed(route.StorageID) {
		return 0, accountdirectory.ErrUnavailable
	}
	return route.StorageID, nil
}

func mergeRole(old, patch *base.AccountSimplePlayer) (*base.AccountSimplePlayer, error) {
	copy := proto.Clone(old).(*base.AccountSimplePlayer)
	if patch.ClassId != 0 {
		if copy.ClassId != 0 && copy.ClassId != patch.ClassId {
			return nil, accountdirectory.ErrConflict
		}
		copy.ClassId = patch.ClassId
	}
	if patch.Gender != 0 {
		if copy.Gender != 0 && copy.Gender != patch.Gender {
			return nil, accountdirectory.ErrConflict
		}
		copy.Gender = patch.Gender
	}
	if patch.AppearanceId != "" {
		if copy.AppearanceId != "" && copy.AppearanceId != patch.AppearanceId {
			return nil, accountdirectory.ErrConflict
		}
		copy.AppearanceId = patch.AppearanceId
	}
	if patch.Name != "" {
		if copy.Name != "" && copy.Name != patch.Name {
			return nil, accountdirectory.ErrConflict
		}
		copy.Name = patch.Name
	}
	return copy, nil
}

// Repair 只补缺失的身份字段,不改归属,所以不碰账号分片的目录行。
func (s *AccountDirectoryStore) Repair(ctx context.Context, account string, role *base.AccountSimplePlayer, rev uint64) (*base.AccountSimplePlayerList, error) {
	if !validRole(role) || rev == 0 || !accountdirectory.ValidAccount(account) {
		return nil, accountdirectory.ErrInvalid
	}
	err := s.playerTx(ctx, role.PlayerId, func(tx *sql.Tx) error {
		old, err := scanOwnership(tx.QueryRowContext(ctx, lockOwnershipSQL, role.PlayerId), role.PlayerId)
		if err != nil {
			return err
		}
		if old.Account != account || old.OwnerRevision != rev || old.TradeListingId != 0 || old.PrepareToken != 0 {
			return accountdirectory.ErrConflict
		}
		merged, err := mergeRole(old.Role, role)
		if err != nil {
			return err
		}
		raw, err := proto.Marshal(merged)
		if err != nil {
			return err
		}
		before, err := proto.Marshal(old.Role)
		if err != nil {
			return err
		}
		if bytes.Equal(before, raw) {
			return nil
		}
		_, err = tx.ExecContext(ctx, "UPDATE account_role SET role_proto=?,updated_ms=UNIX_TIMESTAMP(CURRENT_TIMESTAMP(3))*1000 WHERE player_id=? AND account=? AND owner_revision=?", raw, role.PlayerId, []byte(account), rev)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.readRoleList(ctx, account)
}
