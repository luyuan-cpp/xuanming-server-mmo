#pragma once

#include "engine/core/type_define/type_define.h"
#include <muduo/contrib/hiredis/Hiredis.h>
#include <muduo/net/InetAddress.h>
#include <muduo/net/TimerId.h>

class RedisManager
{
public:
	using HiredisPtr = std::unique_ptr<hiredis::Hiredis>;
	using ReconnectCallback = std::function<void()>;

	RedisManager() = default;
	~RedisManager();

	RedisManager(const RedisManager &) = delete;
	RedisManager &operator=(const RedisManager &) = delete;
	RedisManager(RedisManager &&) = delete;
	RedisManager &operator=(RedisManager &&) = delete;

	HiredisPtr &GetZoneRedis()
	{
		return zoneRedis_;
	}

	// Create the Hiredis instance, register connect/disconnect callbacks, and
	// kick off the initial connect. Idempotent within a single thread; safe to
	// re-invoke (will tear down any existing instance first).
	//
	// Use this instead of constructing Hiredis manually + calling SetupReconnect:
	// the manual sequence loses the very first disconnect event because callbacks
	// were not yet registered.
	void Connect(muduo::net::EventLoop *loop, const muduo::net::InetAddress &addr);

	// Deprecated. Kept for source compatibility. Equivalent to Connect()
	// but requires the caller to have already created and ->connect()'d zoneRedis_.
	// Prefer Connect() in new code.
	void SetupReconnect(muduo::net::EventLoop *loop, const muduo::net::InetAddress &addr);

	// 显式取消重连并释放 Hiredis Channel。必须在 EventLoop 析构前调用;
	// 幂等,Node 正常停机与入口函数兜底可以重复执行。
	void Shutdown();

	// Register a callback to be invoked after a successful reconnect.
	void SetReconnectCallback(const ReconnectCallback &cb) { reconnectCb_ = cb; }

private:
	void ScheduleReconnect();
	void CancelReconnectTimer();
	void DoReconnect();
	void InstallCallbacks();
	void ResetConnection();

	HiredisPtr zoneRedis_;
	muduo::net::EventLoop *loop_ = nullptr;
	muduo::net::InetAddress redisAddr_;
	muduo::net::TimerId reconnectTimerId_;
	bool reconnectTimerActive_ = false;
	ReconnectCallback reconnectCb_;
};

extern thread_local RedisManager tlsRedis;
