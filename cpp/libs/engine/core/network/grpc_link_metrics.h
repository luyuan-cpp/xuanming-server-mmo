#pragma once

#include "../metrics/dependency_link_metrics.h"
#include "muduo_grpc/channel.h"

// 把 muduo_grpc 通道的连接事件接到依赖连接指标上(断线重连 R17)。etcd 传输与调 Go 服务的业务通道在建通道时
// 把它塞进 ChannelOptions::linkObserver;muduo_grpc 只报事件,不知道指标的存在。
inline muduo_grpc::ChannelLinkObserver ObserveGrpcLink(dependency_link_metrics::Dependency dependency)
{
	return [dependency](muduo_grpc::ChannelLinkEvent event)
	{
		switch (event)
		{
		case muduo_grpc::ChannelLinkEvent::kReady:
			dependency_link_metrics::OnUp(dependency);
			break;
		case muduo_grpc::ChannelLinkEvent::kLost:
			dependency_link_metrics::OnDown(dependency, /*expected=*/false);
			break;
		case muduo_grpc::ChannelLinkEvent::kRetired:
		case muduo_grpc::ChannelLinkEvent::kClosed:
			dependency_link_metrics::OnDown(dependency, /*expected=*/true);
			break;
		case muduo_grpc::ChannelLinkEvent::kConnectFailed:
			dependency_link_metrics::OnConnectFailed(dependency, dependency_link_metrics::FailureReason::kConnect);
			break;
		}
	};
}
