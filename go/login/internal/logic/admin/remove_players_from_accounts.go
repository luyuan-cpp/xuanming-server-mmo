package admin

import (
	"context"
	"errors"
	"time"
	"unicode/utf8"

	"login/internal/svc"
	dspb "proto/data_service"
	loginpb "proto/login"
	"shared/accountdirectory"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	// MaxRemovePlayers 是一次请求的上限。每名角色一轮目录删角流程(问全部参与者),RPC 预算内做不了太多,没做完的回 RETRY。
	MaxRemovePlayers = 100
	maxOperatorBytes = 64
	maxReasonBytes   = 256
	// removeRoleBudget 是摘一名角色的全部下游调用的上限:核对读加一轮删角(目录客户端单次上限 2.5 秒)。超了回 RETRY,重跑幂等。
	removeRoleBudget = 3 * time.Second
	// removeReplyMargin 是回响应要留的时间。RPC 预算就是 login 的全局 Timeout(4 秒):login 在路由服的预算链里,门禁不许用 MethodTimeouts 放宽。
	// 剩余不够预算加余量就不开始下一名,回 RETRY:拖到 RPC 超时的话,已经摘掉的名单也回不到调用方。
	removeReplyMargin = 500 * time.Millisecond
)

// RemovePlayersFromAccountsReq 是一次摘孤儿。Operator、Reason 只进日志,谁在什么时候摘了谁要查得到。
type RemovePlayersFromAccountsReq struct {
	PlayerIDs []uint64
	Operator  string
	Reason    string
	DryRun    bool
	PeerAddr  string
}

// RemovePlayerResult 是一名角色的结局。
type RemovePlayerResult struct {
	PlayerID uint64
	Outcome  loginpb.RemovePlayerOutcome
}

// RemovePlayersFromAccountsResp 的计数和 Results 同一口径,FailedCount 是 CONFLICT + RETRY。
type RemovePlayersFromAccountsResp struct {
	RemovedCount   uint32
	NotFoundCount  uint32
	FailedCount    uint32
	NotOrphanCount uint32
	Results        []RemovePlayerResult
}

func (r *RemovePlayersFromAccountsResp) record(player uint64, outcome loginpb.RemovePlayerOutcome) {
	switch outcome {
	case loginpb.RemovePlayerOutcome_REMOVE_PLAYER_REMOVED:
		r.RemovedCount++
	case loginpb.RemovePlayerOutcome_REMOVE_PLAYER_NOT_FOUND:
		r.NotFoundCount++
	case loginpb.RemovePlayerOutcome_REMOVE_PLAYER_NOT_ORPHAN:
		r.NotOrphanCount++
	default:
		r.FailedCount++
	}
	r.Results = append(r.Results, RemovePlayerResult{PlayerID: player, Outcome: outcome})
}

func validText(text string, maxBytes int) bool {
	return text != "" && len(text) <= maxBytes && utf8.ValidString(text)
}

// RemovePlayersFromAccounts 摘孤儿角色:目录里有归属、player_store 里没有写权行的角色。它们进不了场,白占账号的角色名额。
//
// 判定和删除都走目录,不再碰 Redis。一名一名走:目录的删角是逐名两阶段提交,整组提交时中途失败已经删了前面几名,算不清谁成了。
//
// 墓碑不可逆,所以证明不了是孤儿的一律 NOT_ORPHAN:手滑输错 id 不能把在役角色删掉。有存档的不是孤儿;路由缺失时存档查不到,
// 也证明不了(路由和角色行同片,缺了只可能是库被人动过)。现行建角流程提交的角色(Lookup 回初始提交标记)只缺初始行也不是孤儿,
// 是多集群恢复点错开留下的建角残留,走 data_service 的 cmd/create_residual 修。没读到存档状态回 RETRY,不当成没有存档。
// 名字这里不碰:墓碑提交之后由 data_service 的删角收尾释放(核过角色行确是墓碑才删,guild-phase2/03-names.md §3.4a),
// 在役角色的名字到不了那一步。
//
// 重跑幂等:删过的回 NOT_FOUND。鉴权在拦截器(x-admin-token),本函数不判。
func RemovePlayersFromAccounts(ctx context.Context, sc *svc.ServiceContext, req *RemovePlayersFromAccountsReq) (*RemovePlayersFromAccountsResp, error) {
	out := &RemovePlayersFromAccountsResp{}
	if sc.AccountDirectory == nil || sc.DataServiceClient == nil {
		return out, accountdirectory.ErrUnavailable
	}
	if req == nil || len(req.PlayerIDs) > MaxRemovePlayers || !validText(req.Operator, maxOperatorBytes) || !validText(req.Reason, maxReasonBytes) {
		return out, accountdirectory.ErrInvalid
	}
	seen := make(map[uint64]bool, len(req.PlayerIDs))
	for _, player := range req.PlayerIDs {
		if seen[player] {
			continue
		}
		seen[player] = true
		if player == 0 {
			out.record(player, loginpb.RemovePlayerOutcome_REMOVE_PLAYER_NOT_FOUND)
			continue
		}
		outcome := removeOrphan(ctx, sc, req, player)
		out.record(player, outcome)
		logx.WithContext(ctx).Infof("[LoginAdmin] RemovePlayersFromAccounts player=%d outcome=%s dry_run=%t operator=%q reason=%q peer=%s",
			player, outcome, req.DryRun, req.Operator, req.Reason, req.PeerAddr)
	}
	return out, nil
}

func outOfBudget(ctx context.Context) bool {
	if ctx.Err() != nil {
		return true
	}
	deadline, ok := ctx.Deadline()
	return ok && time.Until(deadline) < removeRoleBudget+removeReplyMargin
}

func removeOrphan(ctx context.Context, sc *svc.ServiceContext, req *RemovePlayersFromAccountsReq, player uint64) loginpb.RemovePlayerOutcome {
	if outOfBudget(ctx) {
		return loginpb.RemovePlayerOutcome_REMOVE_PLAYER_RETRY
	}
	ctx, cancel := context.WithTimeout(ctx, removeRoleBudget)
	defer cancel()
	owned, err := sc.AccountDirectory.Lookup(ctx, player)
	if errors.Is(err, accountdirectory.ErrMissing) {
		return loginpb.RemovePlayerOutcome_REMOVE_PLAYER_NOT_FOUND
	}
	if err != nil {
		logx.WithContext(ctx).Errorf("[LoginAdmin] orphan ownership lookup failed: player=%d: %v", player, err)
		return loginpb.RemovePlayerOutcome_REMOVE_PLAYER_RETRY
	}
	ledger, err := sc.DataServiceClient.GetPlayerAssetOpLedger(ctx, &dspb.GetPlayerAssetOpLedgerRequest{PlayerId: player})
	if err != nil {
		logx.WithContext(ctx).Errorf("[LoginAdmin] orphan player_store check failed: player=%d: %v", player, err)
		return loginpb.RemovePlayerOutcome_REMOVE_PLAYER_RETRY
	}
	if ledger.GetFound() {
		return loginpb.RemovePlayerOutcome_REMOVE_PLAYER_NOT_ORPHAN
	}
	// 账本读在路由缺失时同样回 found=false。路由没了说明不了存档也没了,不能据此写不可逆的墓碑。
	if _, err = sc.DataServiceClient.GetPlayerHomeZone(ctx, &dspb.GetPlayerHomeZoneRequest{PlayerId: player}); err != nil {
		if status.Code(err) == codes.NotFound {
			logx.WithContext(ctx).Errorf("[LoginAdmin] role has an owner but no route; refusing to treat it as an orphan: player=%d", player)
			return loginpb.RemovePlayerOutcome_REMOVE_PLAYER_NOT_ORPHAN
		}
		logx.WithContext(ctx).Errorf("[LoginAdmin] orphan route check failed: player=%d: %v", player, err)
		return loginpb.RemovePlayerOutcome_REMOVE_PLAYER_RETRY
	}
	// 现行建角先把初始行写进存档库,再提交目录(初始提交标记)。这样的角色没有写权行,只可能是存档库的恢复点早于写初始行那一步
	// (多集群各自恢复到同一个 T 时恢复点错开,docs/ops/mysql-backup-pitr-runbook.md §4.1a):角色是真的建成了,该补初始行。
	// 墓碑不可逆,这里一律拒绝,交给 data_service 的 cmd/create_residual 重写初始行。
	if owned.GetInitialPlayerCommit() {
		logx.WithContext(ctx).Errorf("[LoginAdmin] role was committed by the current create flow and only lacks its initial player_store rows; "+
			"refusing to tombstone it, repair it with data_service cmd/create_residual instead: player=%d", player)
		return loginpb.RemovePlayerOutcome_REMOVE_PLAYER_NOT_ORPHAN
	}
	if req.DryRun {
		return loginpb.RemovePlayerOutcome_REMOVE_PLAYER_REMOVED
	}
	err = sc.AccountDirectory.Remove(ctx, owned.Account, []*dspb.AccountRoleVersion{{PlayerId: player, OwnerRevision: owned.OwnerRevision}})
	switch {
	case err == nil:
		return loginpb.RemovePlayerOutcome_REMOVE_PLAYER_REMOVED
	case errors.Is(err, accountdirectory.ErrMissing):
		return loginpb.RemovePlayerOutcome_REMOVE_PLAYER_NOT_FOUND
	case errors.Is(err, accountdirectory.ErrConflict):
		// 有未结束的业务,或归属在 Lookup 之后变了。目录已清令牌,不能按新归属偷偷重试。
		return loginpb.RemovePlayerOutcome_REMOVE_PLAYER_CONFLICT
	default:
		logx.WithContext(ctx).Errorf("[LoginAdmin] orphan removal not finished, safe to rerun: player=%d: %v", player, err)
		return loginpb.RemovePlayerOutcome_REMOVE_PLAYER_RETRY
	}
}
