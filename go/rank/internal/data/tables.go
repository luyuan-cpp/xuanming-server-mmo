package data

import (
	"google.golang.org/protobuf/proto"
	rankpb "proto/rank"
)

const DatabaseName = "mmorpg_rank"

func Tables() []proto.Message {
	return []proto.Message{&rankpb.RankBoardRecord{}, &rankpb.RankBoardShardRecord{}, &rankpb.RankScoreRecord{}, &rankpb.RankInputReceiptRecord{}, &rankpb.RankSettlementRecord{}, &rankpb.RankSnapshotRecord{}, &rankpb.RankMailRewardRecord{}, &rankpb.RankMutationRecord{}, &rankpb.RankRemovedEntryRecord{}}
}
