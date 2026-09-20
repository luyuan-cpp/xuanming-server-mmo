// Package playernamereg 是 login 对 data_service「角色名登记表」(全局库 player_name)的
// 唯一客户端,外加 RoleNameRule 配表的读取入口。三条链复用:
//
//   - CreatePlayer 铸出 PlayerId 之后、登记 home zone 之前,用 Reserve 占名;建角后续
//     步骤失败时用 Release 补偿(见 clientplayerlogin/createplayerlogic.go);
//   - EnterGame / 角色列表发现账号记录里缺名字时,用 Lookup 回源(读侧,fail-open);
//   - 建角校验名字长度、空名生成默认名时,用 Rules 读配表。
//
// 为什么单独成包而不是塞进 homezone:两者虽然共用同一条 data_service 连接,失败方向却
// 相反的地方不一样 —— homezone 的读是「查不到就保留存量 zone」,而这里的 Reserve 是
// 唯一性的提交点,**结果未知**(超时 / 连接断)与**确定没写**(未配置 / 被拒)必须能被
// 调用方区分开:前者要补偿释放,后者一释放就可能删掉别人的登记。所以本包刻意做得很薄:
// 不套超时、不重试、不吞错、不打日志、不出指标 —— 这些都属于调用方的编排
// (建角要 1s 单次 + 3s 总预算 + 同名重试一次,读侧只要 500ms 且失败即放行),
// 包在这里只会让调用方拿不到它需要的那一层错误信息。
//
// 设计:docs/design/guild-phase2/03-names.md §3.10 / §3.11。
package playernamereg

import (
	"context"
	"errors"
	"fmt"
	"time"

	dspb "proto/data_service"
	pbtable "shared/generated/pb/table"
	gametable "shared/generated/table"
	"shared/playername"
)

// ErrUnavailable 表示名字登记客户端没有接线(IdSegment 关闭且没配 DataServiceRpc 时 login
// 根本不拨号,或 ServiceContext.PlayerNames 为 nil)。它的关键语义是**一个 RPC 都没发出去**:
// 建角侧据此知道「确定没有登记」,不做补偿释放、也不计孤儿;读侧按「查不到名字」放行。
var ErrUnavailable = errors.New("player name registry unavailable")

const (
	// DefaultReserveTimeout 是单次 ReservePlayerName 的预算。一次登记 = 一条走唯一键的
	// INSERT,稳态远低于 1s;给到 1s 是为了让建角的 3s 总预算里放得下「一次 + 同名重试一次」。
	DefaultReserveTimeout = 1000 * time.Millisecond
	// DefaultReleaseTimeout 是单次 ReleasePlayerName(建角补偿)的预算。
	DefaultReleaseTimeout = 1000 * time.Millisecond
	// DefaultLookupTimeout 是读侧 BatchGetPlayerName 的预算。它串在登录 / 进游戏上,
	// 而且失败只是名字显示为空,所以要短(与 homezone.DefaultRoleListLookupTimeout 同档)。
	DefaultLookupTimeout = 500 * time.Millisecond
	// DelayedReleaseAfter 是「登记结果未知」时第二次补偿释放的延迟。第一次释放可能跑在
	// 那条在途 INSERT 提交**之前**(删了个空,随后 INSERT 落地,名字就成了孤儿),10s 远大于
	// data_service 单条语句的执行上限,又远小于它 10 分钟的免 token 释放窗口(PlayerName.ReleaseWindow)。
	DelayedReleaseAfter = 10 * time.Second

	// roleNameRuleRowID:RoleNameRule 是单行全局规则表,只取 id=1(与 PetRule 同套路)。
	roleNameRuleRowID = 1
	// 生成名撞名重试次数的合法区间。每次尝试都要打一次登记表,上限给到 10 是为了挡住
	// 配表把它填成天文数字后把建角延迟放大到总预算之外;0 会让空名建角必定失败。
	minGenerateAttempts = 1
	maxGenerateAttempts = 10
)

// ReserveResult 是 ReservePlayerName 的**业务**结果(RPC 成功返回时才有意义)。
type ReserveResult struct {
	// Code 取值见 playername.ReserveOK / ReserveTaken / ReserveInvalid。
	Code uint32
	// Owner 仅 Code==ReserveTaken 时非 0:占用者 player_id。只给 login 判「是不是我自己
	// 丢了响应后的重试」,**不得下发客户端**(等于给了一条按名字查人的旁路)。
	Owner uint64
}

// Client 包装 data_service 的三个名字 RPC。ds 为 nil(或 Client 自身为 nil)时所有方法
// 返回 ErrUnavailable,不 panic。
//
// 超时由调用方通过 ctx 给:本包不套默认超时,见包注释。
type Client struct {
	ds dspb.DataServiceClient
}

// New 构造 Client。ds 允许为 nil(未配置 DataServiceRpc),此时所有方法返回 ErrUnavailable。
func New(ds dspb.DataServiceClient) *Client {
	return &Client{ds: ds}
}

// Reserve 为 playerID 登记 name(调用方应已用 playername.Normalize 归一化并校验过)。
//
// 返回值三种形态,调用方必须分开处理:
//   - (result, nil):data_service 给了确定答案(成功 / 被占 / 不合规);
//   - (_, ErrUnavailable):没接线,一个 RPC 都没发,确定没有登记;
//   - (_, 其它 err):gRPC 错误原样返回。codes.FailedPrecondition 表示该 id 名下已有**另一个**
//     名字(确定没写);超时 / Unavailable / 连接断等则是**结果未知** —— INSERT 可能已提交。
func (c *Client) Reserve(ctx context.Context, playerID uint64, name string) (ReserveResult, error) {
	if c == nil || c.ds == nil {
		return ReserveResult{}, ErrUnavailable
	}
	resp, err := c.ds.ReservePlayerName(ctx, &dspb.ReservePlayerNameRequest{PlayerId: playerID, Name: name})
	if err != nil {
		return ReserveResult{}, err
	}
	return ReserveResult{Code: resp.GetResult(), Owner: resp.GetOwnerPlayerId()}, nil
}

// Release 条件释放:data_service 只在 player_id 与 name 归一化后的 name_norm 都对上时才删,
// 而且不带 x-admin-token 时只删 ReleaseWindow 内的登记 —— 所以它只能用于**建角补偿**,
// 清不掉历史孤儿(那是运维带 token 的路径)。删不到行不是错误(幂等)。
func (c *Client) Release(ctx context.Context, playerID uint64, name string) error {
	if c == nil || c.ds == nil {
		return ErrUnavailable
	}
	_, err := c.ds.ReleasePlayerName(ctx, &dspb.ReleasePlayerNameRequest{PlayerId: playerID, Name: name})
	return err
}

// Lookup 批量查展示名。返回的 map 只含登记表里存在的 id;缺席(早于本功能建的角色 / 已释放)
// 不是错误,调用方按空名展示。err 非 nil 表示整次没查成,调用方不得把它当成「这些人没名字」。
// 一次最多 500 个 id(data_service 侧上限,超了返回 InvalidArgument,不做截断)。
func (c *Client) Lookup(ctx context.Context, playerIDs []uint64) (map[uint64]string, error) {
	if c == nil || c.ds == nil {
		return nil, ErrUnavailable
	}
	if len(playerIDs) == 0 {
		return map[uint64]string{}, nil
	}
	resp, err := c.ds.BatchGetPlayerName(ctx, &dspb.BatchGetPlayerNameRequest{PlayerIds: playerIDs})
	if err != nil {
		return nil, err
	}
	names := resp.GetNames()
	if names == nil {
		names = map[uint64]string{}
	}
	return names, nil
}

// Rules 读 RoleNameRule 配表 id=1 并做完整自检,返回玩法长度规则、默认名生成参数、
// 生成名撞名时的最多尝试次数。
//
// 行缺失或任何一项不合法都返回 error,调用方必须 fail-closed(拒绝建角):规则坏了还放行,
// 要么写进唯一键一个不合规的名字,要么生成出过不了自己校验的默认名。
// 每次建角现查而不是启动时缓存:配表支持热更(见 gametable 各 Manager 的快照注释),
// 缓存住的规则会永远停在热更之前。
func Rules() (playername.Rules, playername.GenerateSpec, int, error) {
	row, _ := gametable.RoleNameRuleTableManagerInstance.FindById(roleNameRuleRowID)
	return rulesFromRow(row)
}

// rulesFromRow 是 Rules 的纯函数部分,单独抽出来只为可测(不依赖全局配表单例)。
func rulesFromRow(row *pbtable.RoleNameRuleTable) (playername.Rules, playername.GenerateSpec, int, error) {
	if row == nil {
		return playername.Rules{}, playername.GenerateSpec{}, 0,
			fmt.Errorf("playernamereg: RoleNameRule 表缺少 id=%d 的规则行", roleNameRuleRowID)
	}

	// 先在 uint32 上挡住越界值再转 int:表里填一个超大数时,直接 int() 在 32 位平台上会
	// 变成负数,恰好绕过 Rules.Validate 里「MaxRunes > 上限」那条判断。
	if row.GetMinChars() > playername.StructuralMaxRunes || row.GetMaxChars() > playername.StructuralMaxRunes {
		return playername.Rules{}, playername.GenerateSpec{}, 0,
			fmt.Errorf("%w: RoleNameRule min_chars=%d max_chars=%d, 超过结构上限 %d",
				playername.ErrInvalidRules, row.GetMinChars(), row.GetMaxChars(), playername.StructuralMaxRunes)
	}
	if row.GetGeneratedSuffixLen() > playername.MaxGeneratedSuffixLen {
		return playername.Rules{}, playername.GenerateSpec{}, 0,
			fmt.Errorf("%w: RoleNameRule generated_suffix_len=%d, 超过上限 %d",
				playername.ErrInvalidGenerateSpec, row.GetGeneratedSuffixLen(), playername.MaxGeneratedSuffixLen)
	}

	rules := playername.Rules{MinRunes: int(row.GetMinChars()), MaxRunes: int(row.GetMaxChars())}
	spec := playername.GenerateSpec{Prefix: row.GetGeneratedPrefix(), SuffixLen: int(row.GetGeneratedSuffixLen())}
	// GenerateSpec.Validate 内部会先跑 rules.Validate,一次调用覆盖两者。
	if err := spec.Validate(rules); err != nil {
		return playername.Rules{}, playername.GenerateSpec{}, 0, fmt.Errorf("playernamereg: RoleNameRule 不合法: %w", err)
	}

	attempts := row.GetMaxGenerateAttempts()
	if attempts < minGenerateAttempts || attempts > maxGenerateAttempts {
		return playername.Rules{}, playername.GenerateSpec{}, 0,
			fmt.Errorf("playernamereg: RoleNameRule max_generate_attempts=%d, 必须在 [%d, %d]",
				attempts, minGenerateAttempts, maxGenerateAttempts)
	}
	return rules, spec, int(attempts), nil
}
