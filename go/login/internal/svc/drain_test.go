package svc

import (
	"context"
	"errors"
	"shared/drainproof"
	"testing"
)

func TestRootClosureRejectsAndWaitsForAcceptedHandler(t *testing.T) {
	service := &ServiceContext{}
	done, err := service.EnterRoot()
	if err != nil {
		t.Fatal(err)
	}
	service.CloseRootAdmission()
	if _, err = service.EnterRoot(); !errors.Is(err, drainproof.ErrClosed) {
		t.Fatal("根入口没有永久关闭")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = service.StopAndReport(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("活动handler被当成排空: %v", err)
	}
	done()
	if err = service.StopAndReport(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = service.EnterRoot(); !errors.Is(err, drainproof.ErrClosed) {
		t.Fatal("排空后入口重新打开")
	}
}
