#include "redis_manager.h"

#include "muduo/base/Logging.h"
#include "muduo/net/EventLoop.h"

namespace
{
	// Mirrors hiredis's REDIS_OK constant. Using a local literal avoids relying on
	// hiredis.h being transitively included from muduo's Hiredis.h.
	constexpr int kRedisOk = 0;

	// 保持原先"传空 loop 就等于未绑定"的语义,不对空指针解引用。
	std::optional<std::reference_wrapper<muduo::net::EventLoop>> ToLoopRef(muduo::net::EventLoop *loop)
	{
		if (loop == nullptr)
		{
			return std::nullopt;
		}
		return std::ref(*loop);
	}
}

thread_local RedisManager tlsRedis;

RedisManager::~RedisManager()
{
	Shutdown();
}

void RedisManager::Connect(muduo::net::EventLoop *loop, const muduo::net::InetAddress &addr)
{
	if (loop_)
	{
		CancelReconnectTimer();
	}
	ResetConnection();
	loop_ = ToLoopRef(loop);
	redisAddr_ = addr;

	// Re-create the Hiredis instance under our control so callbacks are
	// guaranteed registered BEFORE the first connect attempt. This closes the
	// hole where the very first SYN failure could go unnoticed (no disconnect
	// callback registered yet -> reconnect timer never started).
	zoneRedis_ = std::make_unique<HiredisPtr::element_type>(loop, redisAddr_);
	InstallCallbacks();
	zoneRedis_->connect();
}

void RedisManager::Shutdown()
{
	if (loop_)
	{
		CancelReconnectTimer();
	}

	// 先切断 RedisManager 自身的回调,否则 redisAsyncFree 触发 disconnect
	// callback 时会在停机过程中重新挂一条重连定时器。
	reconnectCb_ = {};
	ResetConnection();
	loop_.reset();
}

void RedisManager::SetupReconnect(muduo::net::EventLoop *loop, const muduo::net::InetAddress &addr)
{
	loop_ = ToLoopRef(loop);
	redisAddr_ = addr;

	if (!zoneRedis_)
	{
		// Caller did not pre-create the instance — fall through to Connect()
		// which will create + register callbacks + connect atomically.
		Connect(loop, addr);
		return;
	}

	InstallCallbacks();

	// If the externally-issued connect() already failed before callbacks were
	// in place, schedule a reconnect now so we still recover from cold start.
	if (!zoneRedis_->connected())
	{
		LOG_WARN << "Redis not yet connected at SetupReconnect time, scheduling reconnect to "
				 << redisAddr_.toIpPort();
		ScheduleReconnect();
	}
}

void RedisManager::InstallCallbacks()
{
	if (!zoneRedis_)
	{
		return;
	}

	zoneRedis_->setConnectCallback([this](hiredis::Hiredis *, int status)
								   {
		if (status == kRedisOk)
		{
			LOG_INFO << "Redis connected to " << redisAddr_.toIpPort();
			CancelReconnectTimer();
			if (reconnectCb_)
			{
				reconnectCb_();
			}
		}
		else
		{
			LOG_WARN << "Redis connect attempt failed (status=" << status
					 << ") to " << redisAddr_.toIpPort() << ", will retry";
			ScheduleReconnect();
		} });

	zoneRedis_->setDisconnectCallback([this](hiredis::Hiredis *, int status)
									  {
		LOG_ERROR << "Redis disconnected (status=" << status << "), scheduling reconnect to "
				  << redisAddr_.toIpPort();
		ScheduleReconnect(); });
}

void RedisManager::ScheduleReconnect()
{
	static constexpr double kReconnectIntervalSec = 3.0;
	if (!loop_ || reconnectTimerActive_)
	{
		return; // Timer already running; avoid stacking duplicate timers.
	}
	reconnectTimerId_ = loop_->get().runEvery(kReconnectIntervalSec, [this]
										{ DoReconnect(); });
	reconnectTimerActive_ = true;
}

void RedisManager::CancelReconnectTimer()
{
	if (loop_ && reconnectTimerActive_)
	{
		loop_->get().cancel(reconnectTimerId_);
		reconnectTimerActive_ = false;
	}
}

void RedisManager::DoReconnect()
{
	LOG_INFO << "Attempting Redis reconnect to " << redisAddr_.toIpPort();

	// 销毁旧连接前清掉 connect/disconnect 回调,避免显式替换又触发重连;
	// command 回调保留,redisAsyncFree 会用 null reply 让上层把写入放回重试队列。
	ResetConnection();
	if (!loop_)
	{
		return;
	}

	// Create a new Hiredis, install callbacks BEFORE connect(), then connect.
	zoneRedis_ = std::make_unique<HiredisPtr::element_type>(&loop_->get(), redisAddr_);
	InstallCallbacks();
	zoneRedis_->connect();
}

void RedisManager::ResetConnection()
{
	if (!zoneRedis_)
	{
		return;
	}
	zoneRedis_->setConnectCallback({});
	zoneRedis_->setDisconnectCallback({});
	zoneRedis_.reset();
}
