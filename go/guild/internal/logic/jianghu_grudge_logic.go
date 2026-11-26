package logic

import (
	"context"

	"guild/internal/constants"
	"guild/internal/data"

	kafkapb "proto/contracts/kafka"
	pb "proto/guild"
)

// WithJianghuGrudgeStore 允许单测为帮派生死状恩怨墙注入内存存储替身。
func WithJianghuGrudgeStore(store data.JianghuGrudgeStore) Option {
	return func(l *GuildLogic) {
		l.jianghuStore = store
	}
}

func (l *GuildLogic) activeJianghuGrudgeStore() data.JianghuGrudgeStore {
	if l.jianghuStore != nil {
		return l.jianghuStore
	}
	if l.repo != nil {
		return l.repo
	}
	return nil
}

// RecordJianghuDuelResult 消费 match-results 中的天墉生死状终局事件，仅对两支不同帮会的纯同帮小队幂等记入帮派恩怨墙。
func (l *GuildLogic) RecordJianghuDuelResult(ctx context.Context, ev *kafkapb.BattleResultEvent) error {
	store := l.activeJianghuGrudgeStore()
	if store == nil || ev == nil {
		return nil
	}
	_, err := store.RecordJianghuDuelResult(ctx, ev)
	return err
}

// GetGuildJianghuGrudgeWall 返回本帮天墉生死状恩怨墙（宿敌帮会汇总 + 最近 20 场战绩）；严格校验调用者必须为本帮在册成员。
func (l *GuildLogic) GetGuildJianghuGrudgeWall(ctx context.Context, _ *pb.GetGuildJianghuGrudgeWallRequest) (*pb.GetGuildJianghuGrudgeWallResponse, error) {
	who := callerOf(ctx, 0)
	if err := requireClientSession(who); err != nil {
		return nil, err
	}
	guildID, tip, err := l.operatorGuild(ctx, who.playerID)
	if err != nil {
		return nil, err
	}
	if tip != nil {
		return &pb.GetGuildJianghuGrudgeWallResponse{ErrorMessage: tip}, nil
	}
	if guildID == 0 {
		return &pb.GetGuildJianghuGrudgeWallResponse{ErrorMessage: tipErr(constants.ErrNotInGuild, "not in any guild")}, nil
	}
	store := l.activeJianghuGrudgeStore()
	if store == nil {
		return &pb.GetGuildJianghuGrudgeWallResponse{GuildId: guildID}, nil
	}
	rivals, records, err := store.GetGuildJianghuGrudgeWall(ctx, guildID)
	if err != nil {
		return nil, err
	}
	return &pb.GetGuildJianghuGrudgeWallResponse{
		GuildId:       guildID,
		Rivals:        rivals,
		RecentRecords: records,
	}, nil
}
