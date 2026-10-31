// Package drain是DB的维护排空装配：只有gRPC入口和SQL池，没有消费者、队列或补投；它不重新注册writer或建立第二SQL连接。
package drain

import (
	"context"
	"db/internal/logic/pkg/proto_sql"
	"db/internal/maintenancephase"
	"errors"
	"shared/drainproof"
	"time"
)

// Run只复用调用方当前boot已guard的真实stores。专用维护boot从未serve，没有已接收RPC。
func Run(ctx context.Context, runtime *drainproof.Runtime, stores *proto_sql.StoreRegistry) error {
	return runOwners(ctx, runtime, stores, nil)
}

// RunExisting在正常boot上排空：closeSource关闭gRPC入口、等在途RPC结束并给出前沿。
func RunExisting(ctx context.Context, runtime *drainproof.Runtime, stores *proto_sql.StoreRegistry, closeSource func(context.Context) ([]byte, error)) error {
	if closeSource == nil {
		return drainproof.ErrHold
	}
	return runOwners(ctx, runtime, stores, closeSource)
}

func runOwners(ctx context.Context, runtime *drainproof.Runtime, stores *proto_sql.StoreRegistry, closeSource func(context.Context) ([]byte, error)) error {
	if runtime == nil || stores == nil {
		return drainproof.ErrHold
	}
	if err := maintenancephase.ValidateAuthorityPlan(runtime.Plan); err != nil {
		return err
	}
	source, relay := drainproof.New(), drainproof.New()
	sourceWritten := false
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var drainCtx context.Context
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		requested, err := runtime.Requested("SOURCE")
		if err != nil {
			return err
		}
		if requested && !sourceWritten {
			sourceDrainCtx, cancelDrain := context.WithTimeout(ctx, 60*time.Second)
			defer cancelDrain()
			drainCtx = sourceDrainCtx
			if closeSource != nil {
				digest, err := closeSource(drainCtx)
				if err != nil {
					return err
				}
				if err = source.MergeSettled(digest); err != nil {
					return err
				}
			}
			source.CloseAdmission()
			if err = runtime.Write("SOURCE", source); err != nil {
				return err
			}
			sourceWritten = true
		}
		relayRequested, err := runtime.Requested("RELAY")
		if err != nil {
			return err
		}
		if relayRequested && !sourceWritten {
			return errors.New("RELAY关闭请求先于SOURCE收敛")
		}
		if !sourceWritten || !relayRequested {
			continue
		}
		if err = drainCtx.Err(); err != nil {
			return err
		}
		relay.CloseAdmission()
		if err = relay.Wait(drainCtx); err != nil {
			return err
		}
		// 所有同boot SQL调用已确定join，永久禁用原grant，防止同进程重开物理池。
		writer, err := proto_sql.CurrentWriterConfig()
		if err != nil {
			return err
		}
		writer.Seal()
		if err = stores.Close(); err != nil {
			return err
		}
		return runtime.Write("RELAY", relay)
	}
}
