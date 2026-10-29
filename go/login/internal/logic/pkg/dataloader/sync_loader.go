package dataloader

import (
	"context"
	"fmt"
	"login/internal/kafka"
	login_proto "proto/db"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

// dbReadTask tracks a sub-message that must be fetched from the DB.
type dbReadTask struct {
	taskID  string
	message proto.Message // updated in-place when the DB result arrives
	subKey  string        // Redis cache key for this individual sub-message
}

type dbTaskProducer interface {
	SendTask(context.Context, *login_proto.DBTask, string) error
	SendTasks(context.Context, []*login_proto.DBTask, string) error
}

// TriggerPlayerDataPreload fires Kafka DB-read requests for any data not already cached in Redis,
// but does NOT wait for the results (fire-and-forget).
//
// Use this during login to warm the Redis cache without blocking the EnterGame RPC.
// The scene node will load the data asynchronously via its own HandlePlayerAsyncLoaded path
// regardless — this is just a hint to pre-populate Redis before the scene asks for it.
//
// Steps:
//  1. Check parent-level Redis cache — skip entirely if cached.
//  2. Check individual sub-message Redis caches.
//  3. Send Kafka DB-read requests for sub-messages not in cache (fire-and-forget).
//
// replyInstance 传本进程 dispatcher 的 ReplyInstance():没人等结果,但传空串会让这些读回执
// 退回全服旧频道广播给所有 login。
func TriggerPlayerDataPreload(
	ctx context.Context,
	redisClient redis.Cmdable,
	kafkaProducer *kafka.KeyOrderedKafkaProducer,
	playerId uint64,
	messages []proto.Message,
	replyInstance string,
) error {
	playerIdStr := strconv.FormatUint(playerId, 10)

	var tasks []*dbReadTask

	for _, msg := range messages {
		parentKey := buildParentKey(msg, playerId)

		if found, _ := LoadProtoFromRedis(ctx, redisClient, parentKey, proto.Clone(msg)); found {
			logx.Debugf("TriggerPreload: parent cache hit key=%s", parentKey)
			continue
		}

		subMsgs := collectSubMessages(msg)
		for _, sub := range subMsgs {
			subKey := fmt.Sprintf("%s:%s", sub.ProtoReflect().Descriptor().FullName(), playerIdStr)

			if val, _ := redisClient.Get(ctx, subKey).Bytes(); val != nil {
				loaded := proto.Clone(sub)
				if proto.Unmarshal(val, loaded) == nil {
					logx.Debugf("TriggerPreload: sub cache hit key=%s", subKey)
					continue
				}
			}

			taskID := uuid.NewString()
			tasks = append(tasks, &dbReadTask{taskID: taskID, message: sub, subKey: subKey})
		}
	}

	if len(tasks) == 0 {
		logx.Debugf("TriggerPreload: all cached for playerId=%d", playerId)
		return nil
	}

	logx.Infof("TriggerPreload: firing %d DB read tasks (no wait) for playerId=%d", len(tasks), playerId)
	return sendDBReadRequests(ctx, kafkaProducer, tasks, playerId, playerIdStr, replyInstance)
}

// newDBReadTask 构造一条读任务。replyInstance 必须是登记了这条任务回调的那个
// TaskResultDispatcher 的 ReplyInstance():go/db 按它把到达通知只发到该进程的实例频道。
func newDBReadTask(t *dbReadTask, playerId uint64, playerIdStr, replyInstance string) (*login_proto.DBTask, error) {
	body, err := proto.Marshal(t.message)
	if err != nil {
		return nil, fmt.Errorf("marshal DB task body (type=%s): %w",
			t.message.ProtoReflect().Descriptor().FullName(), err)
	}
	return &login_proto.DBTask{
		Key:           playerId,
		WhereCase:     "player_id='" + playerIdStr + "'",
		Op:            login_proto.DBOp_DB_OP_READ,
		MsgType:       string(t.message.ProtoReflect().Descriptor().FullName()),
		Body:          body,
		TaskId:        t.taskID,
		ReplyInstance: replyInstance,
	}, nil
}

// sendDBReadRequests sends DB read tasks to Kafka (single or batch).
// replyInstance 写进每条任务,约束同 newDBReadTask。
func sendDBReadRequests(
	ctx context.Context,
	producer dbTaskProducer,
	tasks []*dbReadTask,
	playerId uint64,
	playerIdStr string,
	replyInstance string,
) error {
	dbTasks := make([]*login_proto.DBTask, len(tasks))
	for i, t := range tasks {
		task, err := newDBReadTask(t, playerId, playerIdStr, replyInstance)
		if err != nil {
			return err
		}
		dbTasks[i] = task
	}

	logx.Infof("LoadSync: sending %d DB read tasks for playerId=%d", len(dbTasks), playerId)
	if len(dbTasks) == 1 {
		return producer.SendTask(ctx, dbTasks[0], playerIdStr)
	}
	return producer.SendTasks(ctx, dbTasks, playerIdStr)
}

// playerAllDataParentTTL bounds how long the assembled parent-key blob may
// stay in Redis if Scene never takes ownership (e.g. login -> immediate
// disconnect before EnterScene). Once Scene executes SavePlayerToRedis, it
// overwrites this entry persistently via its own Lua script.
const playerAllDataParentTTL = 24 * time.Hour
