package team

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"match/internal/authority"

	teampb "proto/team"
)

// 整队开战的持久意图(team_launch 原件,sql_launch.go createLaunch)在请求截止之后不落
// (docs/design/client-request-ordering.md §6.1 / O11)。不连库:请求 ctx 已结束时连连接都不取,
// 它抓的回归是开战原件这条路径改用 context.Background() / WithoutCancel 读写。
// 真库上「截止之后名册一个字节都不变」由 TestTeamTiDBExpiredRequestDeadlineWritesNothing 证明(要 TiDB);
// 原件之后的两步提交(ReserveTeamLaunch / EnqueuePreparedTeamLaunch)与按原请求号撤销(CancelTeamLaunchForActor)
// 在 authority 包的 client_write_deadline_test.go 里,那里还覆盖「最后一条写发出后跑过截止 → 不发 COMMIT」。

var errLaunchDeadlineFixture = errors.New("launch deadline fixture: no statement may reach the driver")

// launchDeadlineConnector 记下取连接的次数,一律失败。
type launchDeadlineConnector struct{ connects atomic.Int32 }

func (c *launchDeadlineConnector) Connect(context.Context) (driver.Conn, error) {
	c.connects.Add(1)
	return nil, errLaunchDeadlineFixture
}
func (c *launchDeadlineConnector) Driver() driver.Driver { return launchDeadlineDriver{} }

type launchDeadlineDriver struct{}

func (launchDeadlineDriver) Open(string) (driver.Conn, error) { return nil, errLaunchDeadlineFixture }

func TestStartLaunchAfterRequestDeadlineSendsNothing(t *testing.T) {
	connector := &launchDeadlineConnector{}
	db := sql.OpenDB(connector)
	t.Cleanup(func() { _ = db.Close() })
	store := &sqlTeamStore{authority: &authority.Store{DB: db}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	request := &teampb.StartTeamMatchRequest{RequestId: uuid.NewString(), ExpectedTeamId: 101, BattleConfigId: 1}
	pinned, launch, err := store.createLaunch(ctx, &Snapshot{TeamId: 101, Version: 1}, 1, request, uuid.NewString(), []uint64{1, 2}, 60)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, pinned)
	require.Nil(t, launch)
	require.Zero(t, connector.connects.Load(), "no connection may be taken after the request deadline")
}
