package data

import (
	"context"
	"fmt"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	guildpb "proto/guild"
	table "shared/generated/pb/table"

	"mail/internal/metrics"
)

// guildLookupTimeout 是读一次所在帮会的上限。批量领取时每封邮件读一次,不能让帮会服务卡住整个请求预算。
const guildLookupTimeout = 300 * time.Millisecond

// GuildDirectory 读玩家当前所在的帮,0 表示没有帮。帮会表归 guild 服务,邮件库不和它同事务,在事务外读。
type GuildDirectory interface {
	GuildOf(ctx context.Context, player uint64) (uint64, error)
}

// GuildRPCDirectory 经 guild 服务的 GetPlayerGuild 读。不带会话元数据,按内部调用走。
type GuildRPCDirectory struct {
	Client guildpb.GuildServiceClient
}

func (d GuildRPCDirectory) GuildOf(ctx context.Context, player uint64) (uint64, error) {
	resp, err := d.Client.GetPlayerGuild(ctx, &guildpb.GetPlayerGuildRequest{PlayerId: player})
	if err != nil {
		return 0, err
	}
	switch tip := resp.GetErrorMessage().GetId(); tip {
	case 0:
		return resp.GetGuild().GetGuildId(), nil
	case uint32(table.GuildError_kGuildNotInGuild):
		return 0, nil
	default:
		return 0, fmt.Errorf("guild.GetPlayerGuild rejected: tip=%d", tip)
	}
}

// guildOf 读玩家所在的帮,给帮会文本邮件判可见。
//
// 读不到时按没有帮算:这类邮件不带资产(带附件的帮会邮件走收件人名单),只是这一次看不到;
// 不能因为帮会服务抖动,让玩家打不开邮箱、领不了别的附件。失败有日志和 mail_guild_lookup_failures_total。
func (r *Repo) guildOf(ctx context.Context, player uint64) uint64 {
	call, cancel := context.WithTimeout(ctx, guildLookupTimeout)
	defer cancel()
	guild, err := r.guilds.GuildOf(call, player)
	if err != nil {
		metrics.ObserveGuildLookupFailure()
		logx.WithContext(ctx).Errorf("[mail] Guild lookup failed, hiding guild text mails for this request player=%d: %v", player, err)
		return 0
	}
	return guild
}
