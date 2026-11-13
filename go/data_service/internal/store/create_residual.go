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
//     名字这几项对挂着删角令牌的角色只报告(role_removing):补回去的名字可能赶在删角收尾放名字之后,绑在墓碑上没人放。
//     ref 不受这条影响:多出来的陈旧 ref 由读路径过滤、后台判陈旧清掉;初始行本来就要求没有令牌。
//   - 初始存档行缺失:存档库早于写初始行。go/db 认领回 NO_OWNER,永远进不了场,建角重放也不碰存档。修复:重写建角时那几行初始行。
//     只修现行建角流程建的角色(initial_player_commit=1):只有它们确定写过、而且只写过这几行。
//
// 名字登记表另有两种残留,也在这里收(2026-11-12):
//
//   - 删角的名字还登记着(removed_role_name):名字片恢复点早于删角释放,名字行(或名字副本)还在已删的角色名下,小表那一行
//     已经删了,没有谁会再放它。角色行扫描连墓碑一起扫:墓碑里留着原名,按它查名字行。修复:先复核墓碑(不可逆),再按
//     (player_id, 判重键) 条件删除名字行和名字副本(PlayerNameStore.ReleaseRemoved,和删角收尾同一步)。
//   - 孤儿名字(orphan_name):名字行还是预留态、登记超过 ResidualOrphanNameMinAge,这个 player_id 却没有活角色(没有角色行,
//     或者是不带名字的墓碑)。来历是建角回滚的释放没做成:玩家片冻结或不可达时释放之前的证明写不了墓碑,名字就留下了;
//     login 判不出结局、来不及补偿时也会留下。按名字片分页扫(Target = ResidualTargetNames)。修复同运维带 token 的
//     ReleasePlayerName:先在玩家片证明这名角色永远不会提交(有路由行、没有角色行时写墓碑),再只删预留态的名字行。
//     证明之前先找提交过的证据,有就只报告(committed_evidence):存档库进过场或存过盘、这名角色在某个账号上有 ACTIVE 的 ref
//     (多集群恢复点错开时角色行可能丢了,墓碑不可逆);落点不在本次的存档库名单里也只报告。
//
// 反过来「有名字、有路由、有 ref,没有角色行」是没建成的建角(或者玩家片恢复点早于提交):PENDING ref 由后台判陈旧,
// 名字由上面的孤儿名字一项收,初始行没人读得到。
//
// 默认只报告。修复要显式选种类;每一项修之前重读角色行复核前提,变了就拒绝。ref 和初始行各在一个逻辑库的一个事务里写完,
// 名字修复复用建角的登记(PlayerNameStore.Reserve,两步各自幂等,提交点是名字行)再置为已绑定(PlayerNameStore.Bind),
// 写的时候持着两个库的闸门行锁。名字每改一处(补回、置绑定、释放)都删这名玩家的名字缓存(NameCacheInvalidator):
// 补回的名字马上查得到,不用等 60 秒的负缓存过期;没接缓存时名字的修复一律拒绝(name_cache_unavailable)。
// 维护身份不过写者闸门,所以每一笔写都要先锁本库闸门行、核它开着(requireGateOpenTx):冻结要拿同一把锁,只能排在这笔写之后。
// 每一项都可以重放:ref 和初始行在事务里核过不缺才写,名字登记和释放本身幂等。按片分页扫,只做一致读,不锁表。

const (
	// CreateResidualDefaultBatch / CreateResidualMaxBatch 是每页扫的角色数。页内的 ref、名字、存档按目标库分组批量查:
	// 一页太小每个库只分到几行,太大一条 IN 太长。
	CreateResidualDefaultBatch = 256
	CreateResidualMaxBatch     = 1000
	// residualLookupChunk 是一条 IN 最多带的键数。
	residualLookupChunk = 500

	// ResidualOrphanNameMinAge 是预留态名字行登记多久之后才当孤儿看:远大于 login 一次建角的预算和它两次补偿释放的间隔
	// (立即一次、约 10 秒后一次),也大于无 token 释放的窗口(PlayerName.ReleaseWindow,10 分钟)。更新的预留态可能是在途的建角。
	ResidualOrphanNameMinAge = 30 * time.Minute
)

// CreateResidualTarget 是一次扫描扫哪张表。
type CreateResidualTarget uint8

const (
	// ResidualTargetRoles 按玩家片扫角色行(含墓碑),核 ref、名字、存档。零值就是它。
	ResidualTargetRoles CreateResidualTarget = iota
	// ResidualTargetNames 按名字片扫名字行,找孤儿名字(预留态、没有活角色)。
	ResidualTargetNames
)

// String 进工具输出,固定的英文词。
func (t CreateResidualTarget) String() string {
	switch t {
	case ResidualTargetRoles:
		return "roles"
	case ResidualTargetNames:
		return "names"
	default:
		return "unknown"
	}
}

// 修好了、但留下要人定的后果:Result 是 Repaired,Reason 是下面之一时 NeedsAttention 仍回 true(退出码非 0)。
const (
	// residualReasonOverRoleCap 补回 ref 之后这个账号的角色数超过了上限:角色是真的,补回比藏着对;超出的名额只挡以后建角,
	// 要不要清掉哪名角色由人定。
	residualReasonOverRoleCap = "over_role_cap"
	// residualReasonNameCacheStale 名字在库里修好了,删名字缓存失败:按玩家查名字最多还要等缓存的 TTL(负缓存 60 秒、正缓存 24 小时),
	// 要人手工删 player:name:<player>。
	residualReasonNameCacheStale = "name_cache_not_invalidated"
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
	// ResidualRemovedRoleName 角色已是墓碑,名字行(或名字副本)还在它名下:名字片恢复点早于删角释放。可修(复核墓碑之后释放)。
	ResidualRemovedRoleName
	// ResidualOrphanName 预留态的名字行登记很久了,这个 player_id 没有活角色(没有角色行,或者是不带这个名字的墓碑):
	// 建角回滚的释放没做成。没有提交过的证据时可修(先证明永远不会提交,再删预留态),有证据只报告。
	ResidualOrphanName
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
	case ResidualRemovedRoleName:
		return "removed_role_name"
	case ResidualOrphanName:
		return "orphan_name"
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

	// seen 是扫到时的角色行,修之前拿来和重读的比。孤儿名字一项扫到时可能没有角色行,这时只有 PlayerID。
	seen residualRole
	// norm 是名字两项(removed_role_name、orphan_name)要释放的那个判重键:墓碑原名算出来的,或者名字行本身的。不进输出。
	norm string
}

// NeedsAttention 报告这一项还要人看:没修、不能自动修、被拒或出错;修好了但留下要人定的后果(补回 ref 之后超过角色上限、
// 名字缓存没删掉)也算。工具的退出码按它定。
func (f CreateResidualFinding) NeedsAttention() bool {
	switch f.Result {
	case ResidualRepaired:
		return f.Reason == residualReasonOverRoleCap || f.Reason == residualReasonNameCacheStale
	case ResidualUnchanged:
		return false
	default:
		return true
	}
}

// CreateResidualRepairs 选这一次修哪几类。全 false 就是只报告。
type CreateResidualRepairs struct {
	Refs        bool // ResidualRefMissing
	Names       bool // ResidualNameMissing、ResidualNameIndexStale、ResidualNameUnbound、ResidualRemovedRoleName、ResidualOrphanName
	InitialRows bool // ResidualInitialRowsMissing
}

func (r CreateResidualRepairs) selects(kind CreateResidualKind) bool {
	switch kind {
	case ResidualRefMissing:
		return r.Refs
	case ResidualNameMissing, ResidualNameIndexStale, ResidualNameUnbound, ResidualRemovedRoleName, ResidualOrphanName:
		return r.Names
	case ResidualInitialRowsMissing:
		return r.InitialRows
	default:
		return false
	}
}

// CreateResidualScan 是扫一片的一段。角色行按 player_id 分页(After),名字行按判重键分页(AfterName)。
type CreateResidualScan struct {
	Slot      int
	Target    CreateResidualTarget
	After     uint64 // 角色行:从这个 player_id 之后接着扫(不含);0 从头
	AfterName string // 名字行:从这个判重键之后接着扫(不含);"" 从头
	Limit     int    // 这一次最多扫多少行;0 扫到片尾
	Batch     int    // 每页行数;0 用 CreateResidualDefaultBatch
	Repair    CreateResidualRepairs
}

// CreateResidualReport 是扫完一段的结果。出错时只含已经核完的页,NextAfter / NextAfterName 停在最后一页核完的末尾,从那里重跑不会漏。
type CreateResidualReport struct {
	Slot          int
	Target        CreateResidualTarget
	Scanned       int
	Findings      []CreateResidualFinding
	NextAfter     uint64 // 角色行扫描的下一段起点
	NextAfterName string // 名字行扫描的下一段起点
	Done          bool   // 这一片扫到头了
}

// CreateResidualAuditor 核对并修复建角残留。不持久化任何状态,多个实例可以同时跑(比如每片一个进程):每一项修复都在库里核过才写。
type CreateResidualAuditor struct {
	steps    residualSteps
	shards   int
	maxRoles int
	// cache 是名字读缓存:名字每修一处就删这名玩家的缓存。nil 时名字的修复一律拒绝(修好了却查不到,不如不修)。
	cache NameCacheInvalidator
	now   func() time.Time
}

// NewCreateResidualAuditor 用目录(必须已经 ConfigurePlayerStores)、名字库和名字读缓存装出核账。目录和名字库必须开在同一组逻辑库上:
// 名字修复借目录的池锁闸门行、用名字库的池写(withGatesHeld),两边是同一个库,锁才有意义。cache 可以是 nil:只报告、不修名字时用不上它。
func NewCreateResidualAuditor(directory *AccountDirectoryStore, names *PlayerNameStore, cache NameCacheInvalidator, now func() time.Time) (*CreateResidualAuditor, error) {
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
		cache:    cache,
		now:      now,
	}, nil
}

// Shards 是目录分片数 K,片号 0..K-1。
func (a *CreateResidualAuditor) Shards() int { return a.shards }

// Run 扫一片的一段。角色行(ResidualTargetRoles):按 player_id 分页读角色行(含墓碑),每页批量取 ref、名字、存档的证据逐名判;
// 名字行(ResidualTargetNames):按判重键分页读名字行,老的预留态逐个去玩家片核角色。选了的种类当场修。
// 证据读失败时这一页的结论不报、游标不前进,回错误;单项修复失败记在那一项上,不中断。
func (a *CreateResidualAuditor) Run(ctx context.Context, scan CreateResidualScan) (CreateResidualReport, error) {
	report := CreateResidualReport{Slot: scan.Slot, Target: scan.Target, NextAfter: scan.After, NextAfterName: scan.AfterName}
	batch := scan.Batch
	if batch == 0 {
		batch = CreateResidualDefaultBatch
	}
	if scan.Slot < 0 || scan.Slot >= a.shards || batch < 1 || batch > CreateResidualMaxBatch || scan.Limit < 0 {
		return report, accountdirectory.ErrInvalid
	}
	switch {
	case scan.Target == ResidualTargetNames && scan.After == 0:
		return a.runNames(ctx, scan, batch, report)
	case scan.Target != ResidualTargetRoles || scan.AfterName != "":
		// 游标和扫描对象对不上(角色行带了名字游标、名字行带了 player_id 游标),说明调用方拼错了。
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

// classify 批量取一页的证据,逐名判。墓碑只核名字:ref 由后台判陈旧清,存档不再有人读。
func (a *CreateResidualAuditor) classify(ctx context.Context, slot int, page []residualRole) ([]CreateResidualFinding, error) {
	refKeys := make([]residualRefKey, 0, len(page))
	names := make(map[uint64]residualName, len(page))
	norms := make([]string, 0, len(page))
	byStorage := make(map[uint32][]uint64)
	for _, r := range page {
		if r.Role == nil {
			continue
		}
		if key, ok := residualNameKeyOf(r.Role.GetName()); ok {
			names[r.PlayerID] = key
			norms = append(norms, key.norm)
		}
		if r.Account == "" {
			continue
		}
		refKeys = append(refKeys, residualRefKey{account: r.Account, player: r.PlayerID})
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
	if r.Account == "" {
		return judgeRemoved(slot, r, names, holders)
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

// judgeRemoved 判一块墓碑:只核名字。墓碑不可逆,这个 player_id 再也不会有角色,名字行或名字副本还在它名下就是删角释放没做完
// (名字片恢复点早于释放;小表那一行已删,没有谁会再放它)。按墓碑里留着的原名查;原名算不出判重键时按名字副本里的判重键放。
// 墓碑里没有名字的(证明、判陈旧写的)没有可核的,它们留下的预留态由名字行扫描当孤儿名字报。
func judgeRemoved(slot int, r residualRole, names map[uint64]residualName, holders map[string]residualNameRow) []CreateResidualFinding {
	if r.Role.GetName() == "" {
		return nil
	}
	f := CreateResidualFinding{Slot: slot, PlayerID: r.PlayerID, Kind: ResidualRemovedRoleName, seen: r}
	key, computable := names[r.PlayerID]
	switch {
	case !computable && r.OwnedNorm == "":
		// 名字行按判重键分片,算不出键就找不到它;名字副本也没有,没有可放的线索。
		return nil
	case !computable:
		f.norm = r.OwnedNorm
	case r.OwnedNorm != "" && r.OwnedNorm != key.norm:
		// 名字副本指着别的名字:只有 ID 复用这类事故到得了,一个字节都不动,交给人。
		f.Result, f.Reason = ResidualManual, "player_holds_other_name"
		return []CreateResidualFinding{f}
	default:
		row, held := holders[key.norm]
		if !(held && row.Holder == r.PlayerID) && r.OwnedNorm == "" {
			return nil
		}
		f.norm = key.norm
	}
	return []CreateResidualFinding{f}
}

// runNames 扫一片名字行的一段:按判重键分页读,预留态、登记早于 ResidualOrphanNameMinAge 的逐个去玩家片核角色(孤儿名字很少,
// 逐个点查),当场判、选了就修。Scanned 数的是名字行。
func (a *CreateResidualAuditor) runNames(ctx context.Context, scan CreateResidualScan, batch int, report CreateResidualReport) (CreateResidualReport, error) {
	cutoffMs := uint64(a.now().Add(-ResidualOrphanNameMinAge).UnixMilli())
	for scan.Limit == 0 || report.Scanned < scan.Limit {
		n := batch
		if scan.Limit > 0 && scan.Limit-report.Scanned < n {
			n = scan.Limit - report.Scanned
		}
		page, err := a.steps.scanNames(ctx, scan.Slot, report.NextAfterName, n)
		if err != nil {
			return report, fmt.Errorf("scan player_name on shard %d: %w", scan.Slot, err)
		}
		var findings []CreateResidualFinding
		for _, entry := range page {
			if entry.BoundMs != 0 || entry.CreatedMs >= cutoffMs {
				continue
			}
			f, reported, err := a.judgeOrphan(ctx, scan.Slot, entry)
			if err != nil {
				// 这一页的结论不报、游标不前进:重跑从这一页开始,不会跳过。
				return report, fmt.Errorf("check reserved names on shard %d: %w", scan.Slot, err)
			}
			if reported {
				findings = append(findings, f)
			}
		}
		for i := range findings {
			a.settle(ctx, &findings[i], scan.Repair)
		}
		report.Findings = append(report.Findings, findings...)
		report.Scanned += len(page)
		if len(page) > 0 {
			report.NextAfterName = page[len(page)-1].Norm
		}
		if len(page) < n {
			report.Done = true
			break
		}
	}
	return report, nil
}

// judgeOrphan 判一条老的预留态名字行,回这一项和要不要报。活角色、带着这个名字的墓碑不在这里报:角色行扫描按 name_unbound、
// removed_role_name 报它们。没有角色行时先找提交过的证据(orphanEvidence),有就只报告。
func (a *CreateResidualAuditor) judgeOrphan(ctx context.Context, slot int, entry residualNameEntry) (CreateResidualFinding, bool, error) {
	f := CreateResidualFinding{Slot: slot, PlayerID: entry.Holder, Kind: ResidualOrphanName, norm: entry.Norm, seen: residualRole{PlayerID: entry.Holder}}
	role, err := a.steps.loadRole(ctx, entry.Holder)
	if err != nil {
		return f, false, fmt.Errorf("read the role of a reserved name: %w", err)
	}
	if role != nil {
		f.seen = *role
		if role.Role == nil {
			f.Result, f.Reason = ResidualManual, "corrupt_role_row"
			return f, true, nil
		}
		key, ok := residualNameKeyOf(role.Role.GetName())
		ownName := ok && key.norm == entry.Norm
		switch {
		case ownName:
			return f, false, nil
		case role.Account != "":
			// 活角色名下登记着另一个名字的预留态:只有 ID 复用这类事故到得了,交给人。
			f.Result, f.Reason = ResidualManual, "live_role_holds_other_name"
		}
		// 不带这个名字的墓碑:永远不会提交,可修。
		return f, true, nil
	}
	evidence, err := a.steps.orphanEvidence(ctx, entry.Holder)
	if err != nil {
		return f, false, fmt.Errorf("read the commit evidence of a reserved name: %w", err)
	}
	if evidence != "" {
		f.Result, f.Reason = ResidualManual, evidence
	}
	return f, true, nil
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
// 孤儿名字扫到时可能没有角色行,前提是「没有活角色」,由 settleOrphanName 自己复核。
// 名字几项要删名字缓存:没接缓存就不修(修好了却查不到,补回的名字要等负缓存过期)。
func (a *CreateResidualAuditor) repair(ctx context.Context, f *CreateResidualFinding) {
	if residualNameKind(f.Kind) && a.cache == nil {
		f.Result, f.Reason = ResidualRefused, "name_cache_unavailable"
		return
	}
	fresh, err := a.steps.loadRole(ctx, f.PlayerID)
	if err != nil {
		a.fail(f, err)
		return
	}
	if f.Kind == ResidualOrphanName {
		a.settleOrphanName(ctx, f, fresh)
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
	case ResidualRemovedRoleName:
		a.releaseRemovedName(ctx, f)
	case ResidualInitialRowsMissing:
		a.restoreInitialRows(ctx, f, fresh)
	default:
		f.Result, f.Reason = ResidualManual, "not_repairable"
	}
}

// residualNameKind 报告这一类修的是名字登记(修完要删名字缓存)。
func residualNameKind(kind CreateResidualKind) bool {
	switch kind {
	case ResidualNameMissing, ResidualNameIndexStale, ResidualNameUnbound, ResidualRemovedRoleName, ResidualOrphanName:
		return true
	default:
		return false
	}
}

// invalidateNameCache 名字在库里修好之后删这名玩家的名字缓存(正、负缓存同一个键),有上限地重来。还是删不掉就把这一项标成
// 要人看(name_cache_not_invalidated):库里已经修好,只是按玩家查名字最多还要等缓存的 TTL。
func (a *CreateResidualAuditor) invalidateNameCache(ctx context.Context, f *CreateResidualFinding) {
	if err := delNameCacheWithRetry(ctx, a.cache, f.PlayerID); err != nil {
		f.Reason = residualReasonNameCacheStale
		logx.Errorf("[create-residual] name repaired but its cache entry was not deleted, delete player:name:<player> by hand: player=%d: %v", f.PlayerID, err)
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
		// 隐身的角色不占名额,恢复之后账号可能又建满了。角色是真的,补回来比藏着对;超出的名额只挡以后建角,
		// 要不要处理由人定:算成要人看的项(NeedsAttention),工具以 1 退出。
		f.Result, f.Reason = ResidualRepaired, residualReasonOverRoleCap
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
		// 名字缺着的那段时间读路径可能记了「没有名字」的负缓存(60 秒),名字副本不对时还可能缓存着错的名字:删掉,修完马上查得到。
		a.invalidateNameCache(ctx, f)
	case ReserveTaken:
		// 扫完之后有人抢先登记了这个名字。登记什么都没写,和扫到时被占一样交给人。
		f.Result, f.Reason, f.Holder = ResidualRefused, "name_held_by_other", holder
	default:
		// 这个 player_id 名下已经登记了另一个名字。v1 没有改名,只可能是 ID 复用这类事故,一个字节都不动。
		f.Result, f.Reason = ResidualRefused, "player_holds_other_name"
	}
}

// releaseRemovedName 释放一块墓碑名下的名字(removed_role_name):repair 刚重读过角色行,确是同一块墓碑(不可逆)。
// 持着名字片和玩家片的闸门行锁,按 (player_id, 判重键) 条件删名字行和名字副本,和删角收尾同一步(PlayerNameStore.ReleaseRemoved);
// 名字已经归了别人(player_id 对不上)不动。可以重放。
func (a *CreateResidualAuditor) releaseRemovedName(ctx context.Context, f *CreateResidualFinding) {
	outcome, err := a.steps.releaseRemovedName(ctx, f.PlayerID, f.norm)
	if err != nil {
		a.fail(f, err)
		return
	}
	f.Result = ResidualRepaired
	logx.Infof("[create-residual] released the name of a removed role: player=%d outcome=%s", f.PlayerID, outcome)
	a.invalidateNameCache(ctx, f)
}

// settleOrphanName 收一条孤儿名字(orphan_name),做的事同运维带 token 的 ReleasePlayerName:
//  1. 复核这时还没有活角色(扫完之后建成了就拒绝,role_committed);没有角色行时重新找一遍提交过的证据,有就拒绝。
//  2. 在玩家片证明这名角色永远不会提交(sealOrphanRole:锁闸门行、路由行、角色行;有路由行、没有角色行时写墓碑,迟到的建角提交不了)。
//  3. 持着名字片和玩家片的闸门行锁,只删预留态的名字行(和名字副本)。证明在前、删除在后:反过来的话删完名字之后在途的建角还能提交。
//
// 每一步都可以重放:墓碑写过就读到墓碑,名字删过就是 Absent。
func (a *CreateResidualAuditor) settleOrphanName(ctx context.Context, f *CreateResidualFinding, fresh *residualRole) {
	switch {
	case fresh != nil && fresh.Account != "":
		f.Result, f.Reason = ResidualRefused, "role_committed"
		return
	case fresh == nil:
		evidence, err := a.steps.orphanEvidence(ctx, f.PlayerID)
		if err != nil {
			a.fail(f, err)
			return
		}
		if evidence != "" {
			f.Result, f.Reason = ResidualRefused, evidence
			return
		}
	}
	seal, err := a.steps.sealOrphanRole(ctx, f.PlayerID)
	if err != nil {
		a.fail(f, err)
		return
	}
	if seal == RoleSealCommitted {
		// 证明的事务里读到在役角色:复核之后才提交的。名字属于它,下一轮角色行扫描按 name_unbound 报。
		f.Result, f.Reason = ResidualRefused, "role_committed"
		return
	}
	outcome, err := a.steps.releaseReservedName(ctx, f.PlayerID, f.norm)
	if err != nil {
		a.fail(f, err)
		return
	}
	switch outcome {
	case ReleaseDeleted:
		f.Result = ResidualRepaired
		logx.Infof("[create-residual] released an orphan name reservation: player=%d seal=%s", f.PlayerID, seal)
		a.invalidateNameCache(ctx, f)
	case ReleaseAbsent:
		f.Result = ResidualUnchanged
	default:
		// 已绑定:证明之后角色提交不了,也就绑定不了;读到它说明库被人动过。一个字节都不动。
		f.Result, f.Reason = ResidualRefused, "name_bound"
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

// residualNameEntry 是名字行上核孤儿名字要的四列:判重键、登记在谁名下、登记时刻、绑定时刻(0 = 预留态)。
type residualNameEntry struct {
	Norm      string
	Holder    uint64
	CreatedMs uint64
	BoundMs   uint64
}

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

// residualSteps 是核账和修复要的每一步。读都是一致读不加锁;每个修复方法只写一个逻辑库(名字登记、释放例外,见 restoreName),
// 事务里先锁本库闸门行。分开是为了能用内存替身把判据、复核和重放跑一遍,SQL 实现见 create_residual_sql.go。
type residualSteps interface {
	// scanRoles 按 player_id 升序读一片上 after 之后的角色行(墓碑也读,Account 为空),最多 limit 行。
	scanRoles(ctx context.Context, slot int, after uint64, limit int) ([]residualRole, error)
	// scanNames 按判重键升序读一片名字行里 after 之后的,最多 limit 行(不筛状态,筛选在编排里)。
	scanNames(ctx context.Context, slot int, after string, limit int) ([]residualNameEntry, error)
	// orphanEvidence 找一个没有角色行的 player_id 提交过的证据:存档库进过场或存过盘(store_saved)、某个账号上有 ACTIVE 的 ref
	// (ref_active)、落点不在本次的存档库名单里(storage_not_listed,核不全)。没有路由行或没有落点回空:提交要求路由行和落点,提交不了。
	orphanEvidence(ctx context.Context, player uint64) (string, error)
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
	// releaseRemovedName 删墓碑名下的名字行和名字副本(只认这个 player_id、这个判重键),持着两个库的闸门行锁。调用方已复核墓碑。
	releaseRemovedName(ctx context.Context, player uint64, norm string) (ReleaseOutcome, error)
	// sealOrphanRole 在玩家片一个事务里锁闸门行、核它开着,再证明这名角色永远不会提交(sealRoleTx:必要时写墓碑)。
	sealOrphanRole(ctx context.Context, player uint64) (RoleSeal, error)
	// releaseReservedName 只删预留态的名字行和名字副本(PlayerNameStore.Release,不限登记时刻),持着两个库的闸门行锁。
	// 调用方已证明角色永远不会提交。
	releaseReservedName(ctx context.Context, player uint64, norm string) (ReleaseOutcome, error)
}

// residualNameRow 是名字行上核账要的两列:登记在谁名下,绑没绑定(bound_ms 非 0)。
type residualNameRow struct {
	Holder uint64
	Bound  bool
}
