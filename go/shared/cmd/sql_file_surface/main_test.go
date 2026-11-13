package main

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestFixtureEndpointCannotTargetAnotherDatabaseServer(t *testing.T) {
	for _, endpoint := range []string{"127.0.0.1:24900", "127.0.0.1:3306", "localhost:24000", "0.0.0.0:24000", "127.0.0.1:024000", "127.0.0.1:24000?x"} {
		if validEndpoint(endpoint) {
			t.Fatalf("accepted non-fixture endpoint %q", endpoint)
		}
	}
	if !validEndpoint("127.0.0.1:24000") {
		t.Fatal("rejected formal fixture endpoint")
	}
}

func TestPrivateDSNRejectsRoutingAndSessionOverrides(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, dsn := range []string{"root@tcp(127.0.0.1:3306)/", "root@tcp(127.0.0.1:24900)/", "root@tcp(127.0.0.1:24000)/?multiStatements=true", "root@tcp(127.0.0.1:24000)/?tidb_snapshot=1", "root@tcp(127.0.0.1:24000)/?allowAllFiles=true"} {
		if _, issue := configure(ctx, []byte(dsn), "", "127.0.0.1:24000"); issue == nil {
			t.Fatal("accepted unsafe private DSN")
		}
	}
	cfg, issue := configure(ctx, []byte("root@tcp(127.0.0.1:24000)/"), "mall_it_unit", "127.0.0.1:24000")
	if issue != nil || cfg.DBName != "mall_it_unit" || cfg.MultiStatements || cfg.Timeout > time.Second {
		t.Fatal("qualified DSN configuration failed")
	}
}

func TestFailureDoesNotSerializeDriverSecret(t *testing.T) {
	issue := classify("target", errors.New("PRIVATE_PASSWORD jdbc:mysql://private/ SQL SECRET"))
	raw, err := json.Marshal(issue)
	if err != nil || strings.Contains(string(raw), "PRIVATE") || strings.Contains(string(raw), "SECRET") {
		t.Fatal("raw driver error escaped")
	}
}

type fixtureRows struct {
	closeError error
	multiple   bool
}

func (fixtureRows) Columns() []string         { return []string{"result"} }
func (r fixtureRows) Close() error            { return r.closeError }
func (fixtureRows) Next([]driver.Value) error { return io.EOF }
func (r fixtureRows) HasNextResultSet() bool  { return r.multiple }
func (fixtureRows) NextResultSet() error      { return io.EOF }

func TestResultRequiresSingleResultAndConfirmedClose(t *testing.T) {
	for _, rows := range []fixtureRows{{multiple: true}, {closeError: errors.New("PRIVATE_CLOSE_ERROR")}} {
		_, _, issue := readRows(context.Background(), rows)
		if issue == nil {
			t.Fatal("ambiguous result was confirmed")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, issue := readRows(ctx, fixtureRows{}); issue == nil {
		t.Fatal("expired read was confirmed")
	}
}
