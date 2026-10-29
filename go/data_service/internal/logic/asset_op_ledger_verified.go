package logic

import (
	"context"
	"fmt"

	"data_service/internal/metrics"
	"data_service/internal/svc"
	componentpb "proto/common/component"
	"shared/savebarrier"

	goredis "github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
)

// 带副本核验的资产账本读(GetPlayerAssetOpLedger 的 verify_redis_copy=true 分支,
// docs/design/zone-redis-regression-guard-20261022.md §4.5「G5 资产自动放弃闸」)。
//
// 为什么要核验:自动放弃的条件 5「已落盘账本确定未见」读的是 zone Redis 父键 blob。Redis 崩溃丢尾、切主丢尾、
// 从备份恢复之后,blob 可能回到「未见」而 SQL 里其实已经应用过这一笔;拿这份副本放弃,对侧退账就成了复制或白扣。
// login 每次核验(SQL 与 Redis 对齐)之后会把当时的实例身份写进屏障 hash 的 verified_identity;
// 只要当前实例身份仍等于它,从核验到现在就没有丢过已确认的写,这份副本才可以当「确定未见」的证据。
//
// 身份、身份戳与 blob 必须在**同一段 Lua** 里取:分开读时,两次读之间实例可能已经换过(又一次回退),
// 「身份没变」的结论就不再覆盖 blob 那一次读。脚本的 KEYS 是父键与屏障 hash,沿用 scene 存盘 / login 加载
// 「父键、owner、屏障同一实例」的部署前提(redis_client.h、login parent_commit_barrier.go 头注释),
// 不新增跨键原子范围;Regions 的 Cluster 客户端下两把键不同 slot,脚本报 CROSSSLOT → Internal,调用方跳过(fail-closed)。
//
// 判据与 login 快路径相同:身份已知、role=master、身份 == 身份戳。身份读不到(INFO 在脚本里不可用、格式异常)
// 一律按未核验处理,不让整次读失败:blob 本身仍有效,「已见」分支照常可用。

// assetOpLedgerVerifiedScript 是生产用的核验读脚本,身份片段固定为 savebarrier.LuaInstanceIdentity。
// 构造后只读;miniredis 不支持 INFO server / replication,单测经 newVerifiedBlobReadScript 换成从普通键读身份的替身。
var assetOpLedgerVerifiedScript = newVerifiedBlobReadScript(savebarrier.LuaInstanceIdentity)

func newVerifiedBlobReadScript(instanceIdentityLua string) *goredis.Script {
	return goredis.NewScript(instanceIdentityLua + verifiedBlobReadLua)
}

// verifiedBlobReadLua 只读。
//
//	KEYS = 父键 `<PlayerAllData 全名>:<player_id>` / 屏障 hash player:{id}:save_barrier
//	返回:父键缺失 → nil(客户端得到 goredis.Nil,即语义表的 found=false);
//	      否则 4 个字符串:blob 原文、身份戳(缺失为空串)、本实例 identity(未知为空串)、role。
//
// 屏障 hash 类型不对时 HGET 报 WRONGTYPE,整段脚本失败 → Internal(数据已矛盾,不能当成「没核验过」放过)。
const verifiedBlobReadLua = `
local blob = redis.call('GET', KEYS[1])
if not blob then return false end
local identity, role = instance_identity()
local stamp = redis.call('HGET', KEYS[2], '` + savebarrier.VerifiedIdentityField + `') or ''
return {blob, stamp, identity, role}
`

// redisCopyIdentity 是核验读脚本带回的三项身份信息,与 blob 出自同一段 Lua。
type redisCopyIdentity struct {
	stamp    string // 屏障 hash 的 verified_identity:login 最近一次核验时的实例身份
	identity string // 本次脚本所在实例的身份;空串 = 身份未知
	role     string // 本次脚本所在实例的 role
}

// verified 报告这份副本能否当「确定未见」的证据。身份未知时绝不认作可信:
// 空身份与空身份戳(从未核验过的玩家)相等,不能据此放行。
func (c redisCopyIdentity) verified() bool {
	return c.identity != "" && c.role == "master" && c.stamp == c.identity
}

// GetVerifiedPlayerAssetOpLedger 读玩家已落盘的资产账本,并报告这份 Redis 副本是否已经过当前实例身份核验。
// 返回的 error 已经是 gRPC status;found / 账本 / 错误的语义与 GetPlayerAssetOpLedger 完全相同(同一张语义表),
// 多出的 verified 只在 found=true 时可能为 true。只读:不加锁、不写任何 key、不碰 MySQL。
func GetVerifiedPlayerAssetOpLedger(ctx context.Context, svcCtx *svc.ServiceContext, playerID uint64) (
	ledger *componentpb.PlayerAssetOpLedgerComp, found, verified bool, err error) {
	return getVerifiedPlayerAssetOpLedger(ctx, svcCtx, playerID, assetOpLedgerVerifiedScript)
}

func getVerifiedPlayerAssetOpLedger(ctx context.Context, svcCtx *svc.ServiceContext, playerID uint64, script *goredis.Script) (
	*componentpb.PlayerAssetOpLedgerComp, bool, bool, error) {
	result := assetOpLedgerReadError
	defer func() { metrics.ObserveAssetOpLedgerRead(result) }()

	var copyID redisCopyIdentity
	fetch := func(ctx context.Context, client goredis.Cmdable, playerID uint64) ([]byte, error) {
		keys := []string{playerAllDataKey(playerID), savebarrier.Key(playerID)}
		values, err := script.Run(ctx, client, keys).StringSlice()
		if err != nil {
			return nil, err // 含 goredis.Nil(父键缺失)
		}
		if len(values) != 4 {
			return nil, fmt.Errorf("verified blob read reply has %d values, want 4", len(values))
		}
		copyID = redisCopyIdentity{stamp: values[1], identity: values[2], role: values[3]}
		return []byte(values[0]), nil
	}
	player, found, err := readPersistedPlayerDatabaseWith(ctx, svcCtx, playerID, "GetPlayerAssetOpLedger", "asset-op-ledger", fetch)
	if err != nil {
		return nil, false, false, err
	}
	if !found {
		result = assetOpLedgerReadAbsent
		return nil, false, false, nil
	}
	if copyID.identity == "" || copyID.role != "master" {
		// 身份读不到或读到的不是主:每一次核验读都会是「未核验」,自动放弃全部停摆。这是部署问题
		// (脚本里 INFO 不可用、DevRedis 指到了从库),要人看。身份戳不等是回退之后的正常现象,不在这里打。
		logx.Errorf("[asset-op-ledger] Redis instance identity unavailable or instance is not master; copy reported unverified: player_id=%d identity_known=%t role=%q",
			playerID, copyID.identity != "", copyID.role)
	}
	result = assetOpLedgerReadFound
	return assetOpLedgerOf(player), true, copyID.verified(), nil
}
