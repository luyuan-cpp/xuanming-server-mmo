// muduo Acceptor 的 accept 失败分类(本仓对 muduo 的本地改动,docs/design/gate-connection-admission-control.md G6)。
//
// 被测的是纯函数 Acceptor::classifyAcceptFailure:accept 返回 -1 之后,只凭 errno 决定是等下一次、跳过这一条、
// 让位排掉一条,还是暂停监听。原来 ENFILE / ENOBUFS / ENOMEM 会让整个进程 abort,gate 上的全部在线连接一起断;
// 这里钉住「资源耗尽一律归到可降级的两类」。真实的 fd 耗尽要靠 Linux 容器里压低 ulimit 复现
// (验证步骤见 docs/PROGRESS.md 2026-11-12 条目),单测里不制造。
// Windows 与 Linux 用的是两份源码(cpp/libs/engine/muduo_windows 与 tools/archived/muduo_linux_overlay),
// 分类表逐字相同;本工程在 Windows 上编的是前者。

#include <gtest/gtest.h>

#include <cerrno>

#include "muduo/net/Acceptor.h"

namespace
{
	using muduo::net::Acceptor;
	using AcceptFailure = muduo::net::Acceptor::AcceptFailure;

	TEST(AcceptorFailurePolicy, NothingReadyOrInterruptedWaitsForTheNextEvent)
	{
		EXPECT_EQ(Acceptor::classifyAcceptFailure(EAGAIN), AcceptFailure::kRetryLater);
		EXPECT_EQ(Acceptor::classifyAcceptFailure(EWOULDBLOCK), AcceptFailure::kRetryLater);
		EXPECT_EQ(Acceptor::classifyAcceptFailure(EINTR), AcceptFailure::kRetryLater);
	}

	// 进程级(EMFILE)与全机级(ENFILE)的 fd 用尽走同一条路:让位排掉一条,排不掉就暂停监听。
	TEST(AcceptorFailurePolicy, DescriptorExhaustionIsShedNotFatal)
	{
		EXPECT_EQ(Acceptor::classifyAcceptFailure(EMFILE), AcceptFailure::kDescriptorsExhausted);
		EXPECT_EQ(Acceptor::classifyAcceptFailure(ENFILE), AcceptFailure::kDescriptorsExhausted);
	}

	// 内核缓冲 / 内存不足没有可让的位,只能暂停监听。
	TEST(AcceptorFailurePolicy, MemoryExhaustionPausesNotFatal)
	{
		EXPECT_EQ(Acceptor::classifyAcceptFailure(ENOBUFS), AcceptFailure::kMemoryExhausted);
		EXPECT_EQ(Acceptor::classifyAcceptFailure(ENOMEM), AcceptFailure::kMemoryExhausted);
	}

	// 对端先断开、防火墙拒绝、已排队连接身上挂着网络错误:这一条已被内核移出队列,接着接下一条。
	TEST(AcceptorFailurePolicy, DeadPendingConnectionIsSkipped)
	{
		EXPECT_EQ(Acceptor::classifyAcceptFailure(ECONNABORTED), AcceptFailure::kConnectionGone);
		EXPECT_EQ(Acceptor::classifyAcceptFailure(EPROTO), AcceptFailure::kConnectionGone);
		EXPECT_EQ(Acceptor::classifyAcceptFailure(EPERM), AcceptFailure::kConnectionGone);
		EXPECT_EQ(Acceptor::classifyAcceptFailure(EHOSTUNREACH), AcceptFailure::kConnectionGone);
		EXPECT_EQ(Acceptor::classifyAcceptFailure(ENETDOWN), AcceptFailure::kConnectionGone);
	}

	// 计数只增不减,任意线程可读;没有监听在跑时读两次应当相同。
	TEST(AcceptorFailurePolicy, ExhaustionCountsAreReadableWithoutAnAcceptor)
	{
		const Acceptor::ExhaustionCounts first = Acceptor::exhaustionCounts();
		const Acceptor::ExhaustionCounts second = Acceptor::exhaustionCounts();
		EXPECT_EQ(first.descriptors, second.descriptors);
		EXPECT_EQ(first.memory, second.memory);
		EXPECT_EQ(first.pauses, second.pauses);
	}
} // namespace
