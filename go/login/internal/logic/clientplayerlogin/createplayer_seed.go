package clientplayerloginlogic

import (
	"context"
	"errors"
	"login/internal/logic/pkg/dataloader"
	login_proto_common "proto/common/base"
	databasepb "proto/common/database"
	"shared/generated/pb/table"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
)

// newPlayerSeedTimeout 是种子写的 ctx 预算。理由同 accountBlobReadBackTimeout:它**封不住单次调用**
// (login 的 Redis 客户端没开 ContextTimeoutEnabled,单次 socket 读仍可等满 go-redis 默认的 3s),
// 真正限制的是 go-redis 的内部重发(默认 MaxRetries=3)。种子只是优化,不值得在账号锁里多等几轮。
const newPlayerSeedTimeout = time.Second

// probeMintedID 是 CreatePlayer 的 6b' 步:在登记名字(6c)之前,只读探测新铸 id 上是否已有既有状态
// (dataloader.ProbeMintedPlayerState:PlayerAllData 父键、player:{id}:owner_epoch;
// docs/design/new-player-first-enter-no-cold-read.md §10 P7,负责人 2026-10-15 按「最标准」定为采用)。
//
// 返回 nil = 两把键都不在,可以继续;返回 tip = 必须拒绝建角。命中(id 复用)与探测出错一律拒绝(fail-closed):
// 号段 id 不复用(设计 I1),新铸 id 上有状态说明发号器把在役 id 又发了一次,或 id_segment 表被重置而 Redis 没清,
// 把它交给新账号就是串档;探不清时放行,同样可能把一个在役 id 交出去。拒绝发生在任何持久写之前:名字、
// player:zone 都还没登记,只烧掉这一个 id,不需要补偿。玩家重试会铸出下一个 id。
//
// 与种子开关无关,每次建角都探:关着种子时,id 复用同样会让新账号首次入场读到那份旧缓存。
// 探测与 6d' 种子写之间仍有窗口:开种子时,种子脚本的条件判断保留为这一段的写时原子复核(见 seedNewPlayer);
// 关种子(默认)时探测之后、开种子时种子之后,到第 7 步围栏写都没有复核,是已知残余窗口(设计 §13.2 2026-10-16 补注)。
//
// tip 复用 6c / 6d 故障时返回的 kLoginDataSerializeFailed(「服务端此刻建不了角,稍后重试」),理由同
// registerHomeZone:不为一个分支申请新码,Tip 码轴由导表器发号。
//
// 预算(2026-10-16):探测直接用请求 ctx,不另设子预算(原先的 1s mintedIDProbeTimeout 已删)。login 的 Redis
// 客户端没开 ContextTimeoutEnabled,ctx 截止不约束 socket 读写,但 go-redis 取连接(连接池排队)与拨号认 ctx:
// 1s 子预算会让连接池等满超过 1s 的探测以 DeadlineExceeded 失败,而探测 fail-closed,它就成了唯一一个预算短于
// handler、又会拒绝建角的 Redis 步骤(读 blob、围栏写都用请求 ctx)。用请求 ctx 后它对池等待的容忍度与读 blob
// 一致,上界是 handler 截止;6c 之后的写仍由 6c” 的 abandonAfterDeadline 把关,login.yaml 锁 TTL 的推导不变。
// 只读 EXISTS 在预算内被 go-redis 重发是无害的。
func (l *CreatePlayerLogic) probeMintedID(account string, playerID uint64) *login_proto_common.TipInfoMessage {
	// 发出之前先查请求 ctx:截止之后不再发任何命令(docs/design/client-request-ordering.md §6.1)。
	// 此刻名字、映射都还没登记,直接拒绝、无需补偿;不计探测指标、不打 ERROR —— 探测没有发生,
	// 也不是 Redis 故障。不查的话,go-redis 会以 ctx 错误原地拒掉 pipeline,被误记成一次探测出错。
	if err := l.ctx.Err(); err != nil {
		logx.Infof("[minted-id-probe] request ctx done before probing, nothing sent player_id=%d account=%s: %v",
			playerID, account, err)
		return loginTip(table.LoginError_kLoginDataSerializeFailed)
	}
	result, err := dataloader.ProbeMintedPlayerState(l.ctx, l.svcCtx.RedisClient, playerID)
	if result == dataloader.MintedIDProbeError {
		// 请求在探测途中结束(截止 / 取消):go-redis 在取连接或拨号时以 ctx 错误返回,探测没有结论,也不是
		// Redis 故障。与上面的前置检查同一口径:仍拒绝,不计探测指标、不打 ERROR(metrics.go id_probe_total 注释)。
		// 只认「错误就是请求 ctx 的错误」:不会被 go-redis 重发的 Redis 错误(错误回包、重发用完后的拨号失败等)
		// 即使恰好在截止之后才返回,也照常计 error。读超时、连接池超时这类可重发的错误则不同:截止之后,go-redis
		// 重发前的退避等待会把它换成 ctx 错误,落进本分支、不计数 —— Redis 卡顿只体现在 stage_seconds{stage="id_probe"}。
		if ctxErr := l.ctx.Err(); ctxErr != nil && errors.Is(err, ctxErr) {
			logx.Infof("[minted-id-probe] request ctx done while probing, refusing to create player_id=%d account=%s: %v",
				playerID, account, err)
			return loginTip(table.LoginError_kLoginDataSerializeFailed)
		}
	}
	createPlayerIDProbeTotal.Inc(result.String())
	switch result {
	case dataloader.MintedIDClear:
		return nil
	case dataloader.MintedIDParentExists, dataloader.MintedIDOwnerEpochExists:
		// 与名字登记的 FailedPrecondition 同属 ID 安全事件:运维据此排查发号器 / id_segment 表。
		logx.Errorf("[minted-id-probe] freshly minted player_id=%d already has %s — id reuse? "+
			"refusing to create before reserving the name or registering player:zone (account=%s)",
			playerID, result, account)
	default:
		// MintedIDProbeError,以及将来新增而这里还不认识的结论:探不清,按拒绝处理。
		logx.Errorf("[minted-id-probe] probe failed, refusing to create (fail-closed) player_id=%d account=%s: %v",
			playerID, account, err)
	}
	return loginTip(table.LoginError_kLoginDataSerializeFailed)
}

// cacheCommittedNewPlayer 只在目录与两张 SQL 行提交成功后发布本次初始快照。
// 缓存未确认或被并发新版本拒绝时保留已提交角色，首次登录沿 SQL 权威核验，不执行名字补偿。
func (l *CreatePlayerLogic) cacheCommittedNewPlayer(account string, parent *databasepb.PlayerAllData) {
	ctx, cancel := context.WithTimeout(l.ctx, newPlayerSeedTimeout)
	defer cancel()
	result, err := dataloader.SeedCommittedNewPlayerAllData(ctx, l.svcCtx.RedisClient, parent)
	createPlayerSeedTotal.Inc(result.String())
	if result != dataloader.SeedWritten && result != dataloader.SeedIdempotent {
		logx.Errorf("[new-player-seed] 已提交角色的初始缓存未确认或被拒绝，保留 SQL 原件并回落权威读取 account=%s result=%s: %v", account, result, err)
	}
}

// 原种子写入具有 ID 复用拒绝语义；移到 SQL 提交后不能再释放在役名字，故提交前保留只读检查。
func (l *CreatePlayerLogic) checkInitialSeedState(account string, playerID uint64) *login_proto_common.TipInfoMessage {
	ctx, cancel := context.WithTimeout(l.ctx, newPlayerSeedTimeout)
	defer cancel()
	state, err := dataloader.ProbeMintedPlayerState(ctx, l.svcCtx.RedisClient, playerID)
	switch state {
	case dataloader.MintedIDClear:
		return nil
	case dataloader.MintedIDParentExists:
		createPlayerSeedTotal.Inc(dataloader.SeedParentExists.String())
	case dataloader.MintedIDOwnerEpochExists:
		createPlayerSeedTotal.Inc(dataloader.SeedOwnerEpochExists.String())
	default:
		createPlayerSeedTotal.Inc(dataloader.SeedError.String())
	}
	logx.Errorf("[new-player-seed] 提交前缓存身份不再是本次全新 ID，拒绝创建 account=%s: %v", account, err)
	return loginTip(table.LoginError_kLoginDataSerializeFailed)
}
