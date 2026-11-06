package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	base "proto/common/base"
	"shared/accountdirectory"
	"shared/authoritysql"
	"shared/playername"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

// 多集群时间点恢复之后的建角残留:核账和修复(docs/ops/mysql-backup-pitr-runbook.md §4.1a,工具 cmd/create_residual)。
//
// 一次建角跨四类逻辑库分步写(docs/design/horizontal-scale-logical-db.md §5.3):名字片登记 player_name(副本 player_name_owner
// 在玩家片),玩家片登记 player_route,账号片写 PENDING ref,存档库写初始行,最后在玩家片的一个事务里锁路由行、插 account_role
// (提交点),再把 ref 改 ACTIVE。各库分在不同集群、各自恢复到同一个 T 时,实际恢复点前后错开,贴着 T 建的角会留下半截。
// 这里只处理「角色行在」的三种。角色行是提交点,它在就说明前面几步当时都做过,缺了只可能是那个库的恢复点早于那一步(或者库被人动过):
//
//   - ref 缺失:账号片早于写 ref。列表只按 ref 读,角色从账号的列表里消失,存档还在。修复:补一条 ACTIVE ref。
//   - 名字缺失:名字片早于登记名字。别的账号能再登记同一个名字。修复:按角色行里的原名重新登记并置为已绑定;
//     名字已经登记在别的角色名下时只报告,不覆盖任何一方(v1 没有改名,也就没有强制改名的标记可打,裁决交给人)。
//   - 名字未绑定:名字登记在本角色名下,却还是预留态(建角提交之后的绑定没做完,或者名字片恢复点早于绑定;
//     名字行「预留 → 绑定」状态机见 docs/design/guild-phase2/03-names.md §3.4a)。修复:置为已绑定,和建角提交之后同一步。
//   - 初始存档行缺失:存档库早于写初始行。go/db 认领回 NO_OWNER,永远进不了场,建角重放也不碰存档。修复:重写建角时那几行初始行。
//     只修现行建角流程建的角色(initial_player_commit=1):只有它们确定写过、而且只写过这几行。
//
// 反过来「有名字、有路由、有 ref,没有角色行」是没建成的建角,不在这里:PENDING ref 由后台判陈旧,名字和初始行没人读得到。
//
// 默认只报告。修复要显式选种类;每一项修之前重读角色行复核前提,变了就拒绝。ref 和初始行各在一个逻辑库的一个事务里写完,
// 名字修复复用建角的登记(PlayerNameStore.Reserve,两步各自幂等,提交点是名字行)再置为已绑定(PlayerNameStore.Bind),
// 写的时候持着两个库的闸门行锁。
// 维护身份不过写者闸门,所以每一笔写都要先锁本库闸门行、核它开着(requireGateOpenTx):冻结要拿同一把锁,只能排在这笔写之后。
// 每一项都可以重放:ref 和初始行在事务里核过不缺才写,名字登记本身幂等。按玩家分片分页扫,只做一致读,不锁表。

const (
	// CreateResidualDefaultBatch / CreateResidualMaxBatch 是每页扫的角色数。页内的 ref、名字、存档按目标库分组批量查:
	// 一页太小每个库只分到几行,太大一条 IN 太长。
	CreateResidualDefaultBatch = 256
	CreateResidualMaxBatch     = 1000
	// residualLookupChunk 是一条 IN 最多带的键数。
	residualLookupChunk = 500
)

// CreateResidualKind 是一项残留的种类。
type CreateResidualKind uint8

const (
	ResidualRefMissing         CreateResidualKind = iota + 1 // 账号片没有 (账号, 角色) 这条 ref:可修
	ResidualNameMissing                                      // 名字没有登记:可修
	ResidualNameIndexStale                                   // 名字登记在本角色名下,按玩家查名字的副本缺失或不符:可修
	ResidualNameTaken                                        // 名字登记在别的角色名下:只报告
	ResidualNameUnverifiable                                 // 角色行里的名字按现行字符集规则算不出判重键:只报告
	ResidualInitialRowsMissing                               // 存档库没有这名角色的任何行:现行建角流程建的、没过过户、没有挂着交易或删角的可修
	ResidualOwnerRowMissing                                  // 存档库有别的行,只缺写权行:只报告
	ResidualRouteMissing                                     // 没有路由行或没有落点:只报告
	ResidualStorageNotListed                                 // 落点不在本次允许访问的存档库里,没核:只报告
	ResidualRoleRowCorrupt                                   // 角色行原件解不开:只报告
	// ResidualNameUnbound 名字登记在本角色名下,却还是预留态:建角提交之后的绑定没做完(或者名字片恢复点早于绑定)。
	// 名字仍受保护(释放预留态之前要证明角色从未提交,而它提交了),但只有绑定之后才是「只有删角能释放」:可修。
	ResidualNameUnbound
)

// String 进工具输出,固定的英文词。
func (k CreateResidualKind) String() string {
	switch k {
	case ResidualRefMissing:
		return "ref_missing"
	case ResidualNameMissing:
		return "name_missing"
	case ResidualNameIndexStale:
		return "name_index_stale"
	case ResidualNameTaken:
		return "name_taken"
	case ResidualNameUnverifiable:
		return "name_unverifiable"
	case ResidualInitialRowsMissing:
		return "initial_rows_missing"
	case ResidualOwnerRowMissing:
		return "owner_row_missing"
	case ResidualRouteMissing:
		return "route_missing"
	case ResidualStorageNotListed:
		return "storage_not_listed"
	case ResidualRoleRowCorrupt:
		return "role_row_corrupt"
	case ResidualNameUnbound:
		return "name_unbound"
	default:
		return "unknown"
	}
}

// CreateResidualResult 是一项残留这一次的结局。
type CreateResidualResult uint8

const (
	ResidualPlanned   CreateResidualResult = iota + 1 // 能修,这一次没修:plan,或者 apply 没选这一类
	ResidualManual                                    // 不自动修,原因在 Reason
	ResidualRepaired                                  // 修了,写已提交
	ResidualUnchanged                                 // 复核时已经不缺了,什么都没写
	ResidualRefused                                   // 复核或写的时候前提不成立,什么都没写,原因在 Reason
	ResidualFailed                                    // 出错,写可能成了也可能没成;重跑 plan 再看
)

// String 进工具输出,固定的英文词。
func (r CreateResidualResult) String() string {
	switch r {
	case ResidualPlanned:
		return "planned"
	case ResidualManual:
		return "manual"
	case ResidualRepaired:
		return "repaired"
	case ResidualUnchanged:
		return "unchanged"
	case ResidualRefused:
		return "refused"
	case ResidualFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// CreateResidualFinding 是一名角色的一项残留。
type CreateResidualFinding struct {
	Slot     int
	PlayerID uint64
	Kind     CreateResidualKind
	Result   CreateResidualResult
	// Reason 是 Manual、Refused 的原因;Repaired 时是附注(比如补回之后超过角色上限)。固定的英文词,进输出。
	Reason string
	// 以下只有 ResidualNameTaken 填:名字现在登记在谁名下,那名角色还在不在,按「建角早者留名」该谁留名(0 = 判不出,人工定)。
	Holder     uint64
	HolderLive bool
	Keeper     uint64

	// seen 是扫到时的角色行,修之前拿来和重读的比。
	seen residualRole
}

// NeedsAttention 报告这一项还要人看:没修、不能自动修、被拒或出错。
func (f CreateResidualFinding) NeedsAttention() bool {
	return f.Result != ResidualRepaired && f.Result != ResidualUnchanged
}

// CreateResidualRepairs 选这一次修哪几类。全 false 就是只报告。
type CreateResidualRepairs struct {
	Refs        bool // ResidualRefMissing
	Names       bool // ResidualNameMissing、ResidualNameIndexStale、ResidualNameUnbound
	InitialRows bool // ResidualInitialRowsMissing
}

func (r CreateResidualRepairs) selects(kind CreateResidualKind) bool {
	switch kind {
	case ResidualRefMissing:
		return r.Refs
	case ResidualNameMissing, ResidualNameIndexStale, ResidualNameUnbound:
		return r.Names
	case ResidualInitialRowsMissing:
		return r.InitialRows
	default:
		return false
	}
}

// CreateResidualScan 是扫一片的一段。
type CreateResidualScan struct {
	Slot   int
	After  uint64 // 从这个 player_id 之后接着扫(不含);0 从头
	Limit  int    // 这一次最多扫多少名角色;0 扫到片尾
	Batch  int    // 每页角色数;0 用 CreateResidualDefaultBatch
	Repair CreateResidualRepairs
}

// CreateResidualReport 是扫完一段的结果。出错时只含已经核完的页,NextAfter 停在最后一页核完的末尾,从那里重跑不会漏。
type CreateResidualReport struct {
	Slot      int
	Scanned   int
	Findings  []CreateResidualFinding
	NextAfter uint64
	Done      bool // 这一片扫到头了
}

// CreateResidualAuditor 核对并修复建角残留。不持久化任何状态,多个实例可以同时跑(比如每片一个进程):每一项修复都在库里核过才写。
type CreateResidualAuditor struct {
	steps    residualSteps
	shards   int
	maxRoles int
	now      func() time.Time
}

// NewCreateResidualAuditor 用目录(必须已经 ConfigurePlayerStores)和名字库装出核账。两者必须开在同一组逻辑库上:
// 名字修复借目录的池锁闸门行、用名字库的池写(withGatesHeld),两边是同一个库,锁才有意义。
func NewCreateResidualAuditor(directory *AccountDirectoryStore, names *PlayerNameStore, now func() time.Time) (*CreateResidualAuditor, error) {
	if directory == nil || directory.shards == nil || directory.stores == nil || names == nil || names.pools == nil || now == nil {
		return nil, errors.New("建角残留核账缺少目录、存档库或名字库")
	}
	if !slices.Equal(directory.shards.names, names.pools.names) {
		return nil, errors.New("名字库和账号目录不是同一组逻辑库")
	}
	return &CreateResidualAuditor{
		steps:    &sqlResidualSteps{shards: directory.shards, stores: directory.stores, names: names},
		shards:   directory.shards.k,
		maxRoles: directory.maxRoles,
		now:      now,
	}, nil
}

// Shards 是目录分片数 K,片号 0..K-1。
func (a *CreateResidualAuditor) Shards() int { return a.shards }

// Run 扫一片的一段:按 player_id 分页读角色行,每页批量取 ref、名字、存档的证据逐名判,选了的种类当场修。
// 证据读失败时这一页的结论不报、游标不前进,回错误;单项修复失败记在那一项上,不中断。
func (a *CreateResidualAuditor) Run(ctx context.Context, scan CreateResidualScan) (CreateResidualReport, error) {
	report := CreateResidualReport{Slot: scan.Slot, NextAfter: scan.After}
	batch := scan.Batch
	if batch == 0 {
		batch = CreateResidualDefaultBatch
	}
	if scan.Slot < 0 || scan.Slot >= a.shards || batch < 1 || batch > CreateResidualMaxBatch || scan.Limit < 0 {
		return report, accountdirectory.ErrInvalid
	}
	for scan.Limit == 0 || report.Scanned < scan.Limit {
		n := batch
		if scan.Limit > 0 && scan.Limit-report.Scanned < n {
			n = scan.Limit - report.Scanned
		}
		page, err := a.steps.scanRoles(ctx, scan.Slot, report.NextAfter, n)
		if err != nil {
			return report, fmt.Errorf("scan account_role on shard %d: %w", scan.Slot, err)
		}
		findings, err := a.classify(ctx, scan.Slot, page)
		if err != nil {
			return report, fmt.Errorf("check shard %d after player %d: %w", scan.Slot, report.NextAfter, err)
		}
		for i := range findings {
			a.settle(ctx, &findings[i], scan.Repair)
		}
		report.Findings = append(report.Findings, findings...)
		report.Scanned += len(page)
		if len(page) > 0 {
			report.NextAfter = page[len(page)-1].PlayerID
		}
		if len(page) < n {
			report.Done = true
			break
		}
	}
	return report, nil
}

// classify 批量取一页的证据,逐名判。
func (a *CreateResidualAuditor) classify(ctx context.Context, slot int, page []residualRole) ([]CreateResidualFinding, error) {
	refKeys := make([]residualRefKey, 0, len(page))
	names := make(map[uint64]residualName, len(page))
	norms := make([]string, 0, len(page))
	byStorage := make(map[uint32][]uint64)
	for _, r := range page {
		if r.Role == nil {
			continue
		}
		refKeys = append(refKeys, residualRefKey{account: r.Account, player: r.PlayerID})
		if key, ok := residualNameKeyOf(r.Role.GetName()); ok {
			names[r.PlayerID] = key
			norms = append(norms, key.norm)
		}
		if r.Routed && r.Route.HasPlacement() && a.steps.storageAllowed(r.Route.StorageID) {
			byStorage[r.Route.StorageID] = append(byStorage[r.Route.StorageID], r.PlayerID)
		}
	}
	present, err := a.steps.presentRefs(ctx, refKeys)
	if err != nil {
		return nil, fmt.Errorf("read account_role_ref: %w", err)
	}
	holders, err := a.steps.nameHolders(ctx, norms)
	if err != nil {
		return nil, fmt.Errorf("read player_name: %w", err)
	}
	kinds := make(map[uint64][]int32)
	for storage, players := range byStorage {
		found, err := a.steps.storeKinds(ctx, storage, players)
		if err != nil {
			return nil, fmt.Errorf("read player_store of storage %d: %w", storage, err)
		}
		for player, rows := range found {
			kinds[player] = rows
		}
	}
	var out []CreateResidualFinding
	for _, r := range page {
		out = append(out, a.judge(slot, r, present, names, holders, kinds)...)
	}
	if err := a.describeHolders(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

// judge 判一名角色,顺序固定:ref、名字、存档。
func (a *CreateResidualAuditor) judge(slot int, r residualRole, present map[residualRefKey]bool, names map[uint64]residualName,
	holders map[string]residualNameRow, kinds map[uint64][]int32) []CreateResidualFinding {
	finding := func(kind CreateResidualKind) CreateResidualFinding {
		return CreateResidualFinding{Slot: slot, PlayerID: r.PlayerID, Kind: kind, seen: r}
	}
	manual := func(kind CreateResidualKind, reason string) CreateResidualFinding {
		f := finding(kind)
		f.Result, f.Reason = ResidualManual, reason
		return f
	}
	if r.Role == nil {
		return []CreateResidualFinding{manual(ResidualRoleRowCorrupt, "corrupt_role_row")}
	}
	var out []CreateResidualFinding
	if !present[residualRefKey{account: r.Account, player: r.PlayerID}] {
		out = append(out, finding(ResidualRefMissing))
	}
	// 名字的三种可修残留:删角进行中的只报告。删角收尾释放名字的时候补登记的行可能还没写,补回去的名字就绑在墓碑上,
	// 小表那一行也删了,再没有谁会放它(docs/design/guild-phase2/03-names.md §3.4a)。等删角走完(墓碑或撤销)再跑。
	nameFinding := func(kind CreateResidualKind) CreateResidualFinding {
		if r.Removing {
			return manual(kind, "role_removing")
		}
		return finding(kind)
	}
	// 角色行里没有名字的(早于名字功能的角色)没有可核的。
	if r.Role.GetName() != "" {
		key, computable := names[r.PlayerID]
		row, held := holders[key.norm]
		switch {
		case !computable:
			out = append(out, manual(ResidualNameUnverifiable, "name_rules_changed"))
		case !held:
			out = append(out, nameFinding(ResidualNameMissing))
		case row.Holder != r.PlayerID:
			f := manual(ResidualNameTaken, "name_held_by_other")
			f.Holder = row.Holder
			out = append(out, f)
		case !row.Bound:
			// 修法(restoreName:重新登记 + 置绑定)顺带把名字副本也补齐,所以和下一种同时成立时只报这一种。
			out = append(out, nameFinding(ResidualNameUnbound))
		case r.OwnedNorm != key.norm:
			out = append(out, nameFinding(ResidualNameIndexStale))
		}
	}
	switch {
	case !r.Routed || !r.Route.HasPlacement():
		// 路由行和角色行同片同事务提交,建角又恒写落点:缺了只可能是库被人动过,要人看。
		out = append(out, manual(ResidualRouteMissing, "no_route_or_placement"))
	case !a.steps.storageAllowed(r.Route.StorageID):
		out = append(out, manual(ResidualStorageNotListed, "storage_not_listed"))
	default:
		rows := kinds[r.PlayerID]
		switch {
		case slices.Contains(rows, playerStoreOwnerKind):
		case len(rows) > 0:
			out = append(out, manual(ResidualOwnerRowMissing, "partial_store_rows"))
		case !r.InitialCommit:
			// 早于初始行流程的角色,不知道它当年该有哪些行。
			out = append(out, manual(ResidualInitialRowsMissing, "legacy_role"))
		case r.OwnerRevision != 1:
			// 建角之后过过户:冻结时存档库一定有过它的行,丢的不只是建角那几行,买家付钱买的进度也在里面。
			// 重写初始行等于把角色悄悄重置,要人先看存档库的恢复点能不能再往 T 靠、过户要不要处理。
			out = append(out, manual(ResidualInitialRowsMissing, "transferred_role"))
		case r.Busy:
			// 挂着交易或删角的令牌:冻结证据和删角两阶段都要看存档,让那条流程先走完。
			out = append(out, manual(ResidualInitialRowsMissing, "role_busy"))
		default:
			out = append(out, finding(ResidualInitialRowsMissing))
		}
	}
	return out
}

// describeHolders 给名字被占的项补上占用者的情况和裁决。
func (a *CreateResidualAuditor) describeHolders(ctx context.Context, findings []CreateResidualFinding) error {
	var holders []uint64
	for _, f := range findings {
		if f.Kind == ResidualNameTaken && !slices.Contains(holders, f.Holder) {
			holders = append(holders, f.Holder)
		}
	}
	if len(holders) == 0 {
		return nil
	}
	info, err := a.steps.describePlayers(ctx, holders)
	if err != nil {
		return fmt.Errorf("read name holders: %w", err)
	}
	for i := range findings {
		f := &findings[i]
		if f.Kind != ResidualNameTaken {
			continue
		}
		holder := info[f.Holder]
		f.HolderLive = holder.Live
		f.Keeper = nameKeeper(f.PlayerID, f.seen.Route.CreatedAtMs, f.Holder, holder)
	}
	return nil
}

// nameKeeper 是「建角早者留名」:名字该归谁。建角时刻取路由行的 created_at_ms(建角第 ② 步写下,早于角色行提交)。
// 占用者不是活角色时归本角色(占用者的登记是孤儿,要人带 token 释放);两边时刻有一边缺或者相等时判不出,回 0。
func nameKeeper(self, selfCreatedMs, holder uint64, info residualPlayer) uint64 {
	switch {
	case !info.Live:
		return self
	case selfCreatedMs == 0 || info.CreatedAtMs == 0 || selfCreatedMs == info.CreatedAtMs:
		return 0
	case selfCreatedMs < info.CreatedAtMs:
		return self
	default:
		return holder
	}
}

// settle 给一项定结局:只报告的已经标好;能修的没选就是 Planned,选了就修。
func (a *CreateResidualAuditor) settle(ctx context.Context, f *CreateResidualFinding, repairs CreateResidualRepairs) {
	switch {
	case f.Result == ResidualManual:
	case !repairs.selects(f.Kind):
		f.Result = ResidualPlanned
	default:
		a.repair(ctx, f)
	}
}

// repair 修一项。先重读角色行:扫完之后过了户、删了角、改了身份,这一项的前提就不成立了,拒绝,重跑 plan 再看。
func (a *CreateResidualAuditor) repair(ctx context.Context, f *CreateResidualFinding) {
	fresh, err := a.steps.loadRole(ctx, f.PlayerID)
	if err != nil {
		a.fail(f, err)
		return
	}
	if !sameResidualRole(fresh, &f.seen) {
		f.Result, f.Reason = ResidualRefused, "role_changed"
		return
	}
	switch f.Kind {
	case ResidualRefMissing:
		a.restoreRef(ctx, f, fresh)
	case ResidualNameMissing, ResidualNameIndexStale, ResidualNameUnbound:
		a.restoreName(ctx, f, fresh)
	case ResidualInitialRowsMissing:
		a.restoreInitialRows(ctx, f, fresh)
	default:
		f.Result, f.Reason = ResidualManual, "not_repairable"
	}
}

// sameResidualRole 比角色行本身:归属和修订号、身份原件、初始提交标记、令牌、落点。名字副本不比:同一名角色前一项修名字会改它。
func sameResidualRole(fresh, seen *residualRole) bool {
	return fresh != nil && fresh.Role != nil && seen.Role != nil &&
		fresh.Account == seen.Account && fresh.OwnerRevision == seen.OwnerRevision && proto.Equal(fresh.Role, seen.Role) &&
		fresh.InitialCommit == seen.InitialCommit && fresh.Busy == seen.Busy && fresh.Removing == seen.Removing &&
		fresh.Routed == seen.Routed && fresh.Route.StorageID == seen.Route.StorageID
}

// fail 记一次修复出错。闸门冻结时事务里锁闸门行那一步就拒了,确定什么都没写,算拒绝:冻结结束再跑。别的错误结局未知。
func (a *CreateResidualAuditor) fail(f *CreateResidualFinding, err error) {
	if authoritysql.IsWriterFrozen(err) {
		f.Result, f.Reason = ResidualRefused, "writer_gate_frozen"
		return
	}
	f.Result = ResidualFailed
	logx.Errorf("[create-residual] repair outcome unknown, rerun plan to check: kind=%s player=%d: %v", f.Kind, f.PlayerID, err)
}

func (a *CreateResidualAuditor) restoreRef(ctx context.Context, f *CreateResidualFinding, role *residualRole) {
	outcome, err := a.steps.restoreRef(ctx, role.Account, role.PlayerID, a.maxRoles)
	if err != nil {
		a.fail(f, err)
		return
	}
	switch outcome {
	case refRestored:
		f.Result = ResidualRepaired
	case refRestoredOverCap:
		// 隐身的角色不占名额,恢复之后账号可能又建满了。角色是真的,补回来比藏着对;超出的名额只挡以后建角。
		f.Result, f.Reason = ResidualRepaired, "over_role_cap"
	case refPresent:
		f.Result = ResidualUnchanged
	default:
		// 再多一条,读列表就超过索引硬上限、整个账号读不出来了。
		f.Result, f.Reason = ResidualRefused, "ref_hard_cap"
	}
	if f.Result == ResidualRepaired {
		logx.Infof("[create-residual] restored account role ref: player=%d over_cap=%t", role.PlayerID, outcome == refRestoredOverCap)
	}
}

func (a *CreateResidualAuditor) restoreName(ctx context.Context, f *CreateResidualFinding, role *residualRole) {
	key, ok := residualNameKeyOf(role.Role.GetName())
	if !ok {
		f.Result, f.Reason = ResidualRefused, "role_changed"
		return
	}
	outcome, holder, err := a.steps.restoreName(ctx, role.PlayerID, key.display, key.norm, uint64(a.now().UnixMilli()))
	if err != nil {
		a.fail(f, err)
		return
	}
	switch outcome {
	case ReserveInserted, ReserveAlreadyOwned:
		f.Result = ResidualRepaired
		logx.Infof("[create-residual] restored player name registration: player=%d outcome=%s", role.PlayerID, outcome)
	case ReserveTaken:
		// 扫完之后有人抢先登记了这个名字。登记什么都没写,和扫到时被占一样交给人。
		f.Result, f.Reason, f.Holder = ResidualRefused, "name_held_by_other", holder
	default:
		// 这个 player_id 名下已经登记了另一个名字。v1 没有改名,只可能是 ID 复用这类事故,一个字节都不动。
		f.Result, f.Reason = ResidualRefused, "player_holds_other_name"
	}
}

func (a *CreateResidualAuditor) restoreInitialRows(ctx context.Context, f *CreateResidualFinding, role *residualRole) {
	outcome, err := a.steps.restoreInitialRows(ctx, role.Route.StorageID, role.Role)
	switch {
	case errors.Is(err, accountdirectory.ErrConflict):
		// 写的时候库里冒出了不是这几行初始行的行,事务回滚,一个字节没改。
		f.Result, f.Reason = ResidualRefused, "store_changed"
	case err != nil:
		a.fail(f, err)
	case outcome == storeRestored:
		f.Result = ResidualRepaired
		logx.Infof("[create-residual] restored initial player_store rows: player=%d storage=%d", role.PlayerID, role.Route.StorageID)
	case outcome == storePresent:
		f.Result = ResidualUnchanged
	default:
		f.Result, f.Reason = ResidualRefused, "partial_store_rows"
	}
}

// ── 证据与修复的接缝 ──────────────────────────────────────────────────────

// residualRole 是一名角色在玩家片上的三样东西:角色行、路由行、名字副本。三张表都按玩家分片,在同一个库。
type residualRole struct {
	PlayerID uint64
	Account  string                    // "" 是墓碑
	Role     *base.AccountSimplePlayer // nil 是原件解不开
	// OwnerRevision 建角时是 1,只有过户和删角会抬它。
	OwnerRevision uint64
	// InitialCommit 是 account_role.initial_player_commit=1:现行建角流程提交的,初始行先于角色行写进了存档库。
	InitialCommit bool
	// Busy 是挂着交易冻结或删角的令牌(trade_listing_id、prepare_token 非零)。
	Busy bool
	// Removing 是挂着删角令牌(prepare_token 是 rolefence.RemovalToken):删角进行中,名字一项只报告不修。
	Removing  bool
	Route     RouteRow
	Routed    bool
	OwnedNorm string // player_name_owner.name_norm,"" 是没有
}

type residualRefKey struct {
	account string
	player  uint64
}

// residualPlayer 是名字占用者的情况:是不是活角色,建角时刻(路由行 created_at_ms,0 是没有路由行)。
type residualPlayer struct {
	Live        bool
	CreatedAtMs uint64
}

type residualName struct{ display, norm string }

// residualNameKeyOf 按 data_service 登记名字时的规则(ReservePlayerName 的结构复检)算判重键。
// 敏感词照样算:建角时登记过的名字,词表后来变了也要能核、能补回。ok=false 是空名,或者按现行字符集规则算不出键。
func residualNameKeyOf(name string) (residualName, bool) {
	display, norm, verdict := playername.Normalize(name, playername.StructuralRules)
	if verdict != playername.VerdictOK && verdict != playername.VerdictSensitive {
		return residualName{}, false
	}
	return residualName{display: display, norm: norm}, true
}

type refRestore uint8

const (
	refRestored        refRestore = iota + 1 // 补上了
	refRestoredOverCap                       // 补上了,账号的 ref 数已经到了角色上限
	refPresent                               // 已经有这条 ref,没写
	refAtHardCap                             // 到了 ref 硬上限,没写
)

type storeRestore uint8

const (
	storeRestored storeRestore = iota + 1 // 写了初始行
	storePresent                          // 已经有写权行,没写
	storePartial                          // 有别的行没有写权行,没写
)

// residualSteps 是核账和修复要的每一步。读都是一致读不加锁;每个修复方法只写一个逻辑库(名字登记例外,见 restoreName),
// 事务里先锁本库闸门行。分开是为了能用内存替身把判据、复核和重放跑一遍,SQL 实现见 create_residual_sql.go。
type residualSteps interface {
	// scanRoles 按 player_id 升序读一片上 after 之后的活角色(墓碑跳过),最多 limit 名。
	scanRoles(ctx context.Context, slot int, after uint64, limit int) ([]residualRole, error)
	// loadRole 重读一名角色,墓碑也读出来;没有角色行回 nil。
	loadRole(ctx context.Context, player uint64) (*residualRole, error)
	presentRefs(ctx context.Context, keys []residualRefKey) (map[residualRefKey]bool, error)
	// nameHolders 回这些判重键现在登记在谁名下、绑没绑定,没登记的不出现。
	nameHolders(ctx context.Context, norms []string) (map[string]residualNameRow, error)
	storageAllowed(storage uint32) bool
	// storeKinds 回这些玩家在存档库里各有哪几种行,一行都没有的不出现。
	storeKinds(ctx context.Context, storage uint32, players []uint64) (map[uint64][]int32, error)
	describePlayers(ctx context.Context, players []uint64) (map[uint64]residualPlayer, error)

	restoreRef(ctx context.Context, account string, player uint64, maxRoles int) (refRestore, error)
	// restoreName 按原名重新登记(名字行、名字副本),再置为已绑定:修之前角色行刚复核过,确已提交。
	// 结局口径同 Reserve:Inserted / AlreadyOwned 是登记并绑定好了,Taken 带占用者,Conflict 是这个 id 名下另有名字。
	restoreName(ctx context.Context, player uint64, display, norm string, nowMs uint64) (ReserveOutcome, uint64, error)
	restoreInitialRows(ctx context.Context, storage uint32, role *base.AccountSimplePlayer) (storeRestore, error)
}

// residualNameRow 是名字行上核账要的两列:登记在谁名下,绑没绑定(bound_ms 非 0)。
type residualNameRow struct {
	Holder uint64
	Bound  bool
}
