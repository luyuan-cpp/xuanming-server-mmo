package playerstore

import (
	"context"
	"errors"
	"io"
	"net"

	"github.com/redis/go-redis/v9"
)

// ResultUnknownError 表示命令发出后连接出错(EOF、超时、连接重置等),服务端可能已经执行,也可能没有。
// 调用方必须按「结果未知」处理:围栏脚本靠同版本重放幂等,不能把它当成「没执行」或「执行失败」。
//
// 之所以包一层:go-redis 的 ClusterClient 在 io.EOF 与超时(实现 Timeout() 的错误)上会自动重发同一命令
// (v9.16 osscluster.go 的 process 循环、error.go 的 shouldRetry),可能把已执行的 Lua 再跑一遍。
// 本类型既不等于 io.EOF,也不实现 Timeout(),集群层就不会重发;Unwrap 保留原因供 errors.Is 判断。
type ResultUnknownError struct {
	Err error
}

func (e *ResultUnknownError) Error() string {
	return "playerstore: 结果未知(连接在命令发出后出错): " + e.Err.Error()
}

func (e *ResultUnknownError) Unwrap() error { return e.Err }

// wrapResultUnknown 只包网络层错误;服务端应答的错误(含 MOVED / ASK / TRYAGAIN 与 redis.Nil)原样返回,
// 交给集群层按「未执行」重试或交给调用方。
func wrapResultUnknown(err error) error {
	if err == nil {
		return nil
	}
	var redisErr redis.Error
	if errors.As(err, &redisErr) {
		return err
	}
	var already *ResultUnknownError
	if errors.As(err, &already) {
		return err
	}
	var netErr net.Error
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, &netErr) {
		return &ResultUnknownError{Err: err}
	}
	return err
}

// noReplayHook 挂在 Cluster 的每个节点客户端上(OnNewNode),把网络层错误改成 ResultUnknownError。
// 拨号失败同样会被包住(此时命令其实没发出),代价是这种情况也不自动重试——保守但不会重放。
type noReplayHook struct{}

var _ redis.Hook = noReplayHook{}

func (noReplayHook) DialHook(next redis.DialHook) redis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		return next(ctx, network, addr)
	}
}

func (noReplayHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := wrapResultUnknown(next(ctx, cmd))
		if err != nil && cmd.Err() != nil {
			cmd.SetErr(wrapResultUnknown(cmd.Err()))
		}
		return err
	}
}

func (noReplayHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		err := wrapResultUnknown(next(ctx, cmds))
		for _, cmd := range cmds {
			if cmd.Err() != nil {
				cmd.SetErr(wrapResultUnknown(cmd.Err()))
			}
		}
		return err
	}
}
