#include "process_info.h"

#include <cstdint>

// ===== WIN32 =====
#ifdef __linux__
#include <sys/socket.h>
#include <netinet/in.h>
#include <arpa/inet.h>
#include <netdb.h>
#include <unistd.h>

#include "muduo/base/ProcessInfo.h"
#endif // __linux__

#ifdef  WIN32

// WinSock2.h 必须排在 Windows.h 前面:Windows.h 会带出 winsock.h(v1),
// 两者的 sockaddr / FD_SET 等定义冲突,顺序反了就是一屏重定义错误。
#include <WinSock2.h>
#include <WS2tcpip.h>
#include <Windows.h>
#include <process.h>
#pragma comment(lib, "ws2_32.lib")

namespace muduo
{
	namespace ProcessInfo
	{

		std::string hostname()
		{
			// HOST_NAME_MAX 64
			// _POSIX_HOST_NAME_MAX 255
			char buf[256];
			if (::gethostname(buf, sizeof buf) == 0)
			{
				buf[sizeof(buf) - 1] = '\0';
				return buf;
			}
			else
			{
				return "unknownhost";
			}
		}

	}

}//namespace muduo


#endif//WIN32

namespace
{

	// 探测目标只用来让内核查路由表,**不会真的发包**(见 ProbeLocalIpByRoute)。
	// 选一个公网地址是为了匹配默认路由;目标可不可达、有没有网都不影响结果。
	constexpr const char *kRouteProbeTarget = "8.8.8.8";
	constexpr uint16_t kRouteProbePort = 53;

	void CloseProbeSocket(
#ifdef WIN32
		SOCKET sock
#else
		int sock
#endif
	)
	{
#ifdef WIN32
		::closesocket(sock);
#else
		::close(sock);
#endif
	}

	// 用"路由表探测"选出本机对外那张网卡的 IPv4。
	//
	// 做法:给一个 UDP socket 调 connect()。UDP 的 connect 只是让内核按路由表选定
	// 出口接口并绑定本地地址,**一个字节都不会发出去**,随后 getsockname() 就能读出
	// 该接口上的本机 IPv4。所以它离线可用、不要求目标可达,只要求本机有一条匹配得上
	// 的路由。
	//
	// 为什么不枚举网卡:开发机上 VMware / Hyper-V / WSL / Docker / VPN 的虚拟网卡
	// 全都算"本机网卡的 IPv4",枚举完还是要猜哪张是对的。路由表里本来就存着答案,
	// 而且它给出的正是**别人看到我们时的那个地址**。顺带也避免了
	// GetAdaptersAddresses / getifaddrs 两套平台 API 的 #ifdef 分叉。
	//
	// 失败返回空串,由调用方决定怎么兜底。
	std::string ProbeLocalIpByRoute()
	{
#ifdef WIN32
		const SOCKET sock = ::socket(AF_INET, SOCK_DGRAM, IPPROTO_UDP);
		if (sock == INVALID_SOCKET)
		{
			return {};
		}
#else
		const int sock = ::socket(AF_INET, SOCK_DGRAM, IPPROTO_UDP);
		if (sock < 0)
		{
			return {};
		}
#endif

		sockaddr_in target{};
		target.sin_family = AF_INET;
		target.sin_port = htons(kRouteProbePort);
		if (::inet_pton(AF_INET, kRouteProbeTarget, &target.sin_addr) != 1)
		{
			CloseProbeSocket(sock);
			return {};
		}

		if (::connect(sock, reinterpret_cast<const sockaddr *>(&target), sizeof(target)) != 0)
		{
			// 没有可匹配的路由(比如完全没配网络)。不是错误,交给调用方兜底。
			CloseProbeSocket(sock);
			return {};
		}

		sockaddr_in local{};
#ifdef WIN32
		int localLen = static_cast<int>(sizeof(local));
#else
		socklen_t localLen = sizeof(local);
#endif
		if (::getsockname(sock, reinterpret_cast<sockaddr *>(&local), &localLen) != 0)
		{
			CloseProbeSocket(sock);
			return {};
		}

		char text[INET_ADDRSTRLEN] = {};
		const char *converted = ::inet_ntop(AF_INET, &local.sin_addr, text, sizeof(text));
		CloseProbeSocket(sock);
		return converted != nullptr ? std::string(converted) : std::string();
	}

	// 旧实现:gethostbyname(主机名) 取地址列表的第一个。
	// 保留为二档兜底 —— 顺序由接口 metric / 绑定顺序决定,多网卡开发机上经常选中
	// 一张连不通的虚拟网卡,所以不再作为首选。
	// 原实现没有空指针检查:gethostbyname 失败返回 nullptr,直接解引用 h_addr 会崩,
	// 主机名解析不了时表现为进程无日志退出。这里补上。
	std::string LocalIpByHostname()
	{
		const auto host = muduo::ProcessInfo::hostname();
		const struct hostent *hostEntry = ::gethostbyname(host.c_str());
		if (hostEntry == nullptr || hostEntry->h_addr == nullptr || hostEntry->h_addrtype != AF_INET)
		{
			return {};
		}

		char text[INET_ADDRSTRLEN] = {};
		const char *converted =
			::inet_ntop(AF_INET, hostEntry->h_addr, text, sizeof(text));
		return converted != nullptr ? std::string(converted) : std::string();
	}

} // namespace

std::string localip()
{
	// 三档兜底,任何一档都不会崩:
	//   1. 路由表探测 —— 多网卡下唯一靠谱的选法
	//   2. 主机名解析 —— 旧行为,留给没有默认路由的隔离环境
	//   3. 127.0.0.1  —— 单机自测仍然可用,总比空串或崩溃强
	// 想绕开全部自动判断就设 NODE_IP 环境变量(见 Node 的 ResolveNodeIp)。
	if (std::string routed = ProbeLocalIpByRoute(); !routed.empty())
	{
		return routed;
	}

	if (std::string byHostname = LocalIpByHostname(); !byHostname.empty())
	{
		return byHostname;
	}

	return "127.0.0.1";
}
