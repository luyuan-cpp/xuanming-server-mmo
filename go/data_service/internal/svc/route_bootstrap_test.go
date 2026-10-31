package svc

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

type bootstrapFixture struct {
	routes, history bool
	failure         error
}

func (f *bootstrapFixture) HasAnyRoute(context.Context) (bool, error) { return f.routes, f.failure }
func (f *bootstrapFixture) HasHistoricalPlayers(context.Context, []uint32) (bool, error) {
	return f.history, f.failure
}

func TestGlobalEmptyRouteBootstrapRequiresEmptySQLHistory(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fixture bootstrapFixture
		wantErr bool
	}{
		{name: "新空环境"},
		{name: "路由表非空不查历史", fixture: bootstrapFixture{routes: true, history: true}},
		{name: "路由表空而有角色历史", fixture: bootstrapFixture{history: true}, wantErr: true},
		{name: "SQL读失败不算空", fixture: bootstrapFixture{failure: errors.New("SQL unknown")}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.fixture
			err := checkEmptyRouteBootstrap(context.Background(), &f, []uint32{1})
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}
