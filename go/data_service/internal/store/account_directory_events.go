package store

import (
	"context"
	"errors"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
)

// 目录里「失败只记日志、不改结局」那几步对外的两个接缝(docs/design/guild-phase2/03-names.md §3.4a「失败的计数与告警」):
//   - DirectoryEvents:建角提交之后的名字绑定、删角墓碑之后的收尾没做完时计一次。装配层(svc)接到 internal/metrics,
//     store 不依赖指标包;没接时只记日志(单测、工具)。告警在 deploy/k8s/data-service-account-directory-alerts.yaml。
//   - NameCacheInvalidator:目录自己补回或删掉名字行之后删名字读缓存(player:name:{id},正、负缓存同一个键),
//     补回的名字马上查得到,删掉的名字不再展示。缓存只是展示用的读缓存,删失败只记日志。

// DirectoryEvents 收目录里那几步的失败。实现必须并发安全、不阻塞:建角和删角的请求路径上会调它。
type DirectoryEvents interface {
	// NameBindFailed 记一次建角提交之后名字没绑上:名字行停在预留态,或者名字在别人名下。reason 取 NameBindFailure* 常量。
	NameBindFailed(reason string)
	// RemovalSettleFailed 记一次删角墓碑之后的收尾没做完:小表那一行留着,后台 ResumeRemovals 接着做。step 取 RemovalSettle* 常量。
	RemovalSettleFailed(step string)
}

// 名字绑定没做完的原因(DirectoryEvents.NameBindFailed 的 reason),封闭集合,进指标 label。
const (
	// NameBindFailureError 读角色行或写名字行出错(库不可达、超时、闸门冻结):同一建角重放或核账工具补上。
	NameBindFailureError = "error"
	// NameBindFailureTaken 名字登记在别的角色名下:一名两角,v1 没有改名,要人裁决。
	NameBindFailureTaken = "taken"
	// NameBindFailureConflict 名字行不在,而这个 player_id 名下登记着另一个名字:只有 ID 复用这类事故到得了。
	NameBindFailureConflict = "conflict"
	// NameBindFailureRoleMismatch 复核时角色行不在、已是墓碑,或者名字不是这个:不绑,留给人看。
	NameBindFailureRoleMismatch = "role_mismatch"
	// NameBindFailureRoleRemoved 补登记撞上并发删角:本次补写的那一行已撤掉(或撤销失败,错误日志里有)。
	NameBindFailureRoleRemoved = "role_removed"
	// NameBindFailureRulesChanged 现行字符集规则算不出这个名字的判重键:当年登记的那一行认不出来。
	NameBindFailureRulesChanged = "rules_changed"
)

// NameBindFailureReasons 是 NameBindFailed 的全部取值,装配层按它预置指标序列。
func NameBindFailureReasons() []string {
	return []string{NameBindFailureError, NameBindFailureTaken, NameBindFailureConflict,
		NameBindFailureRoleMismatch, NameBindFailureRoleRemoved, NameBindFailureRulesChanged}
}

// 删角墓碑之后的收尾里没做完的那一步(DirectoryEvents.RemovalSettleFailed 的 step),封闭集合,进指标 label。
const (
	// RemovalSettleReleaseName 释放这名角色登记的名字失败(名字片不可达、核墓碑失败)。名字留在已删的角色名下,别人登记不了。
	RemovalSettleReleaseName = "release_name"
	// RemovalSettleDropRow 名字放掉了,删小表 account_role_removal 那一行失败。下一轮收尾重放一遍,释放是幂等的。
	RemovalSettleDropRow = "drop_removal_row"
)

// RemovalSettleSteps 是 RemovalSettleFailed 的全部取值,装配层按它预置指标序列。
func RemovalSettleSteps() []string {
	return []string{RemovalSettleReleaseName, RemovalSettleDropRow}
}

// NameCacheInvalidator 删掉一名玩家的名字缓存(正、负缓存是同一个键)。*routing.PlayerNameCache 满足它。
// 由消费方(本包)定义:routing 依赖 store,store 不能反过来依赖 routing。
type NameCacheInvalidator interface {
	DelPlayerName(ctx context.Context, playerID uint64) error
}

// ConfigureEvents 接上失败计数,在装配完成、对外服务之前调。nil 不改现状。
func (s *AccountDirectoryStore) ConfigureEvents(events DirectoryEvents) {
	if events != nil {
		s.events = events
	}
}

// ConfigureNameCache 接上名字读缓存,在装配完成、对外服务之前调。nil 不改现状(不删缓存,名字最多按缓存 TTL 晚一点看得到)。
func (s *AccountDirectoryStore) ConfigureNameCache(cache NameCacheInvalidator) {
	if cache != nil {
		s.nameCache = cache
	}
}

func (s *AccountDirectoryStore) noteNameBindFailed(reason string) {
	if s.events != nil {
		s.events.NameBindFailed(reason)
	}
}

func (s *AccountDirectoryStore) noteRemovalSettleFailed(step string) {
	if s.events != nil {
		s.events.RemovalSettleFailed(step)
	}
}

const (
	// nameCacheInvalidateAttempts / nameCacheInvalidateBackoff:删名字缓存的重试。DEL 幂等,可以重来;次数和等待都有上限,
	// 走到这里库已经改好,缓存只是展示用的读缓存,不值得为它拖长请求。
	nameCacheInvalidateAttempts = 3
	nameCacheInvalidateBackoff  = 50 * time.Millisecond
)

// invalidateNameCache 在库里补回或删掉一名玩家的名字之后删它的名字缓存。没接缓存时什么都不做;失败只记日志,
// 补回的名字最多晚负缓存的 TTL(PlayerName.NegativeCacheTTL)才查得到,删掉的名字最多再展示正缓存的 TTL。
func (s *AccountDirectoryStore) invalidateNameCache(ctx context.Context, player uint64) {
	if s.nameCache == nil {
		return
	}
	if err := delNameCacheWithRetry(ctx, s.nameCache, player); err != nil {
		logx.Errorf("[player-name] name cache not invalidated after the name row changed, it stays stale until its TTL: player=%d: %v", player, err)
	}
}

// delNameCacheWithRetry 删一名玩家的名字缓存,失败按固定间隔重来,次数有上限,ctx 结束即停。
func delNameCacheWithRetry(ctx context.Context, cache NameCacheInvalidator, player uint64) error {
	var failure error
	for attempt := 1; attempt <= nameCacheInvalidateAttempts; attempt++ {
		err := cache.DelPlayerName(ctx, player)
		if err == nil {
			return nil
		}
		failure = errors.Join(failure, err)
		if attempt == nameCacheInvalidateAttempts {
			break
		}
		if err := sleepCtx(ctx, nameCacheInvalidateBackoff); err != nil {
			return errors.Join(failure, err)
		}
	}
	return failure
}
