// Package saveoutbox 补投场景已原子保存、但尚未取得两表 SQL ACK 的原始 DBTask。
// 它只恢复现有存盘管道，不生成玩家资产、归属 epoch 或 save_seq。
package saveoutbox

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	dbconfig "db/internal/config"
	"google.golang.org/protobuf/proto"
	databasepb "proto/common/database"
	dbpb "proto/db"
	"shared/kafkautil"
	"shared/placement"
	"shared/savebarrier"
)

// BucketCount 与 C++ player_lifecycle 的持久索引契约相同；修改必须双写迁移，不能直接换桶。
const BucketCount uint64 = 64

const (
	fieldVersion = "outbox_version"
	fieldTopic   = "outbox_topic"
	fieldFirst   = "outbox_player_database"
	fieldSecond  = "outbox_player_database_1"
)

// 补投状态字段只由 go/db 写(scripts.go 头注释)。两表 ACK 齐全时与上面四个字段一起删除，屏障回到 pending 加两表 ACK 三个字段。
const (
	fieldAttemptVersion = "outbox_attempt_version"
	fieldAttempts       = "outbox_attempts"
	fieldTerminal       = "outbox_terminal"
	fieldTerminalReason = "outbox_terminal_reason"
	fieldRetryStamp     = "outbox_retry_ms"
)

func pendingIndexKey(zone uint32, bucket uint64) string {
	return fmt.Sprintf("player:save_pending:%d:%d", zone, bucket)
}

func topicFor(zone, generation uint32) string {
	return dbconfig.DbTaskTopicForGeneration(zone, generation)
}

func topicZone(topic string) (uint32, error) {
	if !strings.HasPrefix(topic, kafkautil.DBTaskTopicPrefix) {
		return 0, errors.New("存盘意图 topic 前缀不合法")
	}
	parts := strings.Split(strings.TrimPrefix(topic, kafkautil.DBTaskTopicPrefix), "_g")
	if len(parts) > 2 {
		return 0, errors.New("存盘意图 topic 世代不合法")
	}
	z, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil || !placement.IsZoneStorage(uint32(z)) || strconv.FormatUint(z, 10) != parts[0] {
		return 0, errors.New("存盘意图 topic zone 不合法")
	}
	if len(parts) == 2 {
		gen, err := strconv.ParseUint(parts[1], 10, 32)
		if err != nil || gen < 2 || strconv.FormatUint(gen, 10) != parts[1] {
			return 0, errors.New("存盘意图 topic 世代不合法")
		}
	}
	return uint32(z), nil
}

type intent struct {
	version string
	topic   string
	first   string
	second  string
}

func validateIntent(playerID uint64, in intent) (uint32, error) {
	zone, err := topicZone(in.topic)
	if err != nil {
		return 0, err
	}
	first := &databasepb.PlayerDatabase{}
	second := &databasepb.PlayerDatabase_1{}
	for i, body := range []string{in.first, in.second} {
		task := &dbpb.DBTask{}
		if err := proto.Unmarshal([]byte(body), task); err != nil {
			return 0, errors.New("存盘意图 DBTask 无法解码")
		}
		var row proto.Message = first
		if i == 1 {
			row = second
		}
		if task.GetKey() != playerID || task.GetOp() != dbpb.DBOp_DB_OP_WRITE || task.GetMsgType() != string(row.ProtoReflect().Descriptor().FullName()) ||
			task.GetOwnerEpoch() == 0 || task.GetSaveSeq() == 0 || savebarrier.Version(task.GetOwnerEpoch(), task.GetSaveSeq()) != in.version {
			return 0, errors.New("存盘意图 DBTask 身份或围栏不匹配")
		}
		if err := proto.Unmarshal(task.GetBody(), row); err != nil {
			return 0, errors.New("存盘意图玩家行无法解码")
		}
	}
	if first.GetPlayerId() != playerID || second.GetPlayerId() != playerID {
		return 0, errors.New("存盘意图玩家行主键不匹配")
	}
	return zone, nil
}
