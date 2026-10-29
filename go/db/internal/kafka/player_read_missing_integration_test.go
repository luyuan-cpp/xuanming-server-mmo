package kafka

import (
	"context"
	"db/internal/metrics"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	playerpb "proto/common/database"
	dbpb "proto/db"
	"shared/savebarrier"
)

// 真实 SELECT 无行会保留请求体中的 player_id。必须在发布之前辨别此结局，
// 不能只给 Login 一个手造的空 Data 负例而遗漏实际生产回显。
func TestIntegrationPlayerReadMissingRowNeverEchoesRequest(t *testing.T) {
	model := openAssetFenceFixture(t)
	ctx := context.Background()
	initial := savebarrier.InitialPlayerData(runDBOpTestKey)
	for _, row := range []proto.Message{initial.PlayerDatabaseData, initial.PlayerDatabase_1Data} {
		t.Run(string(row.ProtoReflect().Descriptor().Name()), func(t *testing.T) {
			f := newRunDBOpFixture(t)
			route := f.route("read")
			route.store = model
			request := row.ProtoReflect().Type().New().Interface()
			id := request.ProtoReflect().Descriptor().Fields().ByName("player_id")
			request.ProtoReflect().Set(id, row.ProtoReflect().Get(id))
			task := &dbpb.DBTask{Key: runDBOpTestKey, Op: dbpb.DBOp_DB_OP_READ, MsgType: string(row.ProtoReflect().Descriptor().FullName()), WhereCase: fmt.Sprintf("player_id='%d'", runDBOpTestKey), TaskId: "missing-original-row"}
			before := f.mr.Dump()
			outcome, err := runDBOp(ctx, f.rc, task, request, handleDBReadOp, route, nil)
			require.ErrorIs(t, err, errMissingPlayerSQLRow)
			require.False(t, outcome.publishes())
			require.Equal(t, before, f.mr.Dump(), "无行不得写缓存或成功回执")
			// 复用既有确定失败回执接缝，不进重试，不伪造成功的 Data。
			w := &worker{ctx: ctx, redisClient: f.rc}
			w.failReadAuthority(&workerTask{}, task, metrics.ReadAbandonedMissingPlayerRow, errMissingPlayerSQLRow)
			result, ok := readTaskResult(t, w, task.TaskId)
			require.True(t, ok)
			require.False(t, result.Success)
			require.Empty(t, result.Data)
			require.Equal(t, metrics.ReadAbandonedMissingPlayerRow, result.Error)
			require.Zero(t, f.rc.Exists(ctx, buildCacheKey(task)).Val())

			// 真正存在的初始空行可以读出完整 1:1 元组，不能把合法空资产也误拒。
			require.NoError(t, model.Insert(row))
			request = row.ProtoReflect().Type().New().Interface()
			request.ProtoReflect().Set(id, row.ProtoReflect().Get(id))
			task.TaskId = "existing-initial-row"
			_, err = runDBOp(ctx, f.rc, task, request, handleDBReadOp, route, nil)
			require.NoError(t, err)
			result, ok = readTaskResult(t, w, task.TaskId)
			require.True(t, ok)
			require.True(t, result.Success)
			loaded := row.ProtoReflect().Type().New().Interface()
			require.NoError(t, proto.Unmarshal(result.Data, loaded))
			require.True(t, proto.Equal(row, loaded))
		})
	}
}

func TestIntegrationOtherReadMissingRowKeepsExistingContract(t *testing.T) {
	model := openAssetFenceFixture(t)
	row := &playerpb.PlayerCentreDatabase{PlayerId: runDBOpTestKey}
	model.RegisterTable(row)
	require.NoError(t, model.CreateOrUpdateTable(row))
	_, err := handleDBReadOp(context.Background(), model, &dbpb.DBTask{WhereCase: fmt.Sprintf("player_id='%d'", runDBOpTestKey)}, row)
	require.NoError(t, err, "本次只修改正式玩家双表缺行，不能扩展为所有查询的兼容破坏")
}
