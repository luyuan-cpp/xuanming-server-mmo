#include "redis_manager.h"

#include "muduo/base/Logging.h"
#include "muduo/net/EventLoop.h"

#include <chrono>
#include <memory>
#include <string_view>

namespace
{
	// AUTH 失败时服务端错误文本最多打印这么多字节。
	constexpr std::size_t kMaxAuthErrorLogBytes = 200;

	// 保持原先"传空 loop 就等于未绑定"的语义,不对空指针解引用。
	std::optional<std::reference_wrapper<muduo::net::EventLoop>> ToLoopRef(muduo::net::EventLoop *loop)
	{
		if (loop == nullptr)
		{
			return std::nullopt;
		}
		return std::ref(*loop);
	}

	// 服务端给的错误文本进日志前的处理。口令绝不进日志:文本里若含口令原文(某些代理会回显),
	// 只保留第一个空格前的错误码(WRONGPASS / ERR / NOAUTH ...)供运维区分原因,其余换成 <redacted>;
	// 错误码本身也含口令(口令很短或恰是错误码的一部分)时整段换掉。
	// 必须先在完整文本上查口令、再截断:先截断会把跨截断点的口令前半段放进日志。
	std::string RedactServerErrorText(std::string_view text, const std::string &password)
	{
		if (password.empty() || text.find(password) == std::string_view::npos)
		{
			return std::string(text.substr(0, kMaxAuthErrorLogBytes));
		}
		const std::string_view code = text.substr(0, text.find(' '));
		if (code.empty() || code.size() > kMaxAuthErrorLogBytes || code.find(password) != std::string_view::npos)
		{
			return "<redacted>";
		}
		return std::string(code) + " <redacted>";
	}

	// AUTH 失败时给日志用的描述。
	std::string DescribeAuthFailure(const redisReply *reply, const std::string &password)
	{
		if (reply->type != REDIS_REPLY_ERROR || reply->str == nullptr)
		{
			return "unexpected reply type " + std::to_string(reply->type);
		}
		return RedactServerErrorText(std::string_view(reply->str, reply->len), password);
	}

	// 建连失败 / 断线时 hiredis 记下的原因(redisAsyncContext::errstr,P0-E 第 3 批):I/O 错误的系统文本、
	// "Server closed the connection"(读到 EOF)、协议错误,或建连后、发出第一条命令前服务端主动回的错误
	// (到线时的 "ERR max number of clients reached":hiredis async.c 把这种回复当错误、写进 errstr 后断开)。
	// 适配层没有暴露数值的 err,errstr 是它对应的文本;显式拆除(redisAsyncDisconnect)时为空串,打成 none。
	// 只能在该连接自己的连接 / 断线回调栈内调用:此时 context 仍有效(hiredis 在回调返回后才释放它,
	// 各平台适配层随后清空 context_)。
	// errstr 可能是服务端原文,同 AUTH 失败一样过一遍口令遮挡。
	std::string DescribeConnectionError(const hiredis::Hiredis &connection, const std::string &password)
	{
		const char *errstr = connection.errstr();
		if (errstr == nullptr || *errstr == '\0')
		{
			return "none";
		}
		return RedactServerErrorText(errstr, password);
	}

	// 先清连接与断线回调再释放:避免显式拆除又走一遍断线 / 重连路径。
	// 命令回调保留:redisAsyncFree 用空应答回调挂起命令,上层据此把写入放回重试队列。
	void ReleaseConnection(RedisManager::HiredisPtr &connection)
	{
		if (!connection)
		{
			return;
		}
		connection->setConnectCallback({});
		connection->setDisconnectCallback({});
		connection.reset();
	}

	// 非正数(含 NaN)回落到默认值并打 WARN。what 拼进日志:"Redis <what> <value>s is not positive, using <fallback>s"。
	double PositiveOrDefault(double value, double fallback, const char *what)
	{
		if (value > 0)
		{
			return value;
		}
		LOG_WARN << "Redis " << what << " " << value << "s is not positive, using " << fallback << "s";
		return fallback;
	}

	std::chrono::duration<double> Seconds(double seconds)
	{
		return std::chrono::duration<double>(seconds);
	}
}

thread_local RedisManager tlsRedis;

RedisManager::~RedisManager()
{
	Shutdown();
}

void RedisManager::Connect(muduo::net::EventLoop *loop, const muduo::net::InetAddress &addr, RedisConnectOptions options)
{
	if (loop_)
	{
		CancelReconnectTimer();
		CancelLivenessTimer();
	}
	// 连接可能仍被当前 Channel 分发保住；先让旧连接的管理器回调失效，再拆连接。
	lifetimeToken_.reset();
	// 旧连接留在局部变量中，等新周期完整建立后再收尾。旧业务命令的空应答允许重入 Connect / Shutdown
	// 或销毁管理器，因此释放旧业务连接之后，当前调用栈不能再访问任何成员。
	HiredisPtr oldHandshake = std::move(handshakeRedis_);
	HiredisPtr oldZone = std::move(zoneRedis_);
	authAwaitingReply_ = false;
	ResetLivenessProbe();
	loop_ = ToLoopRef(loop);
	redisAddr_ = addr;
	options_ = std::move(options);
	options_.reconnectIntervalSec = PositiveOrDefault(
		options_.reconnectIntervalSec, RedisConnectOptions::kDefaultReconnectIntervalSec, "reconnect interval");
	options_.livenessProbeIntervalSec = PositiveOrDefault(
		options_.livenessProbeIntervalSec, RedisConnectOptions::kDefaultLivenessProbeIntervalSec, "liveness probe interval");
	options_.livenessTimeoutSec = PositiveOrDefault(
		options_.livenessTimeoutSec, RedisConnectOptions::kDefaultLivenessTimeoutSec, "liveness timeout");
	// 新的 Connect 周期换一枚令牌:上一周期排队的摘下任务与遗留的定时器闭包一律失效。
	lifetimeToken_ = std::make_shared<LifetimeToken>();

	// 占位对象:只构造、从不 connect()。它的 connected() 恒为 false、command() 直接返回 REDIS_ERR 且不调回调,
	// context 为空,析构什么也不做(Hiredis.cc 的 connected() / command() / ~Hiredis)。有了它,GetZoneRedis()
	// 在第一次握手完成前就非空,直接 `GetZoneRedis()->command(` 的调用点不会解引用空指针。
	zoneRedis_ = hiredis::Hiredis::Create(loop, redisAddr_);
	StartLivenessTimer();
	StartHandshake();
	ReleaseConnection(oldHandshake);
	ReleaseConnection(oldZone);
}

void RedisManager::Shutdown()
{
	if (loop_)
	{
		CancelReconnectTimer();
		CancelLivenessTimer();
	}

	// 先切断 RedisManager 自身的回调,否则 redisAsyncFree 触发 disconnect
	// callback 时会在停机过程中重新挂一条重连定时器。
	reconnectCb_ = {};
	// 已排队、还没跑的摘下任务随令牌一起作废；ResetConnections 释放管理器持有的连接引用。
	// 若仍在 I/O 分发中，Hiredis 工厂保证实际析构在 pending 阶段完成。
	lifetimeToken_.reset();
	loop_.reset();
	// 释放业务连接可能同步回调并重入或销毁管理器，必须作为最后一步。
	ResetConnections();
}

void RedisManager::StartHandshake()
{
	if (!loop_)
	{
		return;
	}

	// 回调在 connect() 之前装好:首个 SYN 就失败也能进 OnHandshakeConnected 挂上重连定时器。
	handshakeRedis_ = hiredis::Hiredis::Create(&loop_->get(), redisAddr_);
	handshakeRedis_->setConnectCallback(
		[token = std::weak_ptr<LifetimeToken>(lifetimeToken_), this](hiredis::Hiredis *me, int status)
		{
			if (!token.lock()) return;
			OnHandshakeConnected(me, status);
		});
	// 断线回调装在连接对象上,移交后继续有效:握手期间断开与移交后断开都走这里。
	// reason 是 hiredis 记下的断线原因(见 DescribeConnectionError):到线、被服务端踢、网络断开在日志里分得开。
	handshakeRedis_->setDisconnectCallback(
		[token = std::weak_ptr<LifetimeToken>(lifetimeToken_), this](hiredis::Hiredis *me, int status)
		{
			if (!token.lock()) return;
			LOG_ERROR << "Redis disconnected (status=" << status << ", reason=" << DescribeConnectionError(*me, options_.password)
					  << "), scheduling reconnect to " << redisAddr_.toIpPort();
			ScheduleReconnect();
		});
	handshakeRedis_->connect();
}

void RedisManager::OnHandshakeConnected(hiredis::Hiredis *me, int status)
{
	if (status != REDIS_OK)
	{
		LOG_WARN << "Redis connect attempt failed (status=" << status << ", reason="
				 << DescribeConnectionError(*me, options_.password) << ") to " << redisAddr_.toIpPort() << ", will retry";
		ScheduleReconnect();
		return;
	}

	LOG_INFO << "Redis connected to " << redisAddr_.toIpPort();
	CancelReconnectTimer();
	if (options_.password.empty())
	{
		// 不认证:连上即移交,重连回调与改动前同一时机。
		PromoteHandshake();
		return;
	}

	// 握手截止:从连上起一个重连间隔内没有 +OK,就由 DoReconnect 在回调栈外重建握手连接。
	// AUTH 被拒、发不出去时也靠这条定时器重试。
	ScheduleReconnect();
	SendAuth();
}

void RedisManager::SendAuth()
{
	// 握手连接上的第一条也是唯一一条命令。%b 按长度传参:口令里的空格、引号或二进制字节都不会被拆成多个参数。
	const int ret = handshakeRedis_->command(
		[token = std::weak_ptr<LifetimeToken>(lifetimeToken_), this](hiredis::Hiredis *me, redisReply *reply)
		{
			if (!token.lock()) return;
			OnAuthReply(me, reply);
		},
		"AUTH %b", options_.password.data(), options_.password.size());
	if (ret == REDIS_OK)
	{
		authAwaitingReply_ = true;
		return;
	}
	// 连接回调里这条连接必然可用(hiredis 回调前已置 REDIS_CONNECTED,不会处在 DISCONNECTING / FREEING),
	// 所以只在命令格式化失败或内存不足时发生。不移交,截止定时器到点重建。
	LOG_ERROR << "Redis AUTH could not be sent to " << redisAddr_.toIpPort() << " (ret=" << ret
			  << ") -- zone Redis stays unavailable, retrying every " << options_.reconnectIntervalSec
			  << "s (metric=zone_redis_auth_failed)";
}

void RedisManager::OnAuthReply(hiredis::Hiredis *me, const redisReply *reply)
{
	// 空应答:这条连接正在释放或已断开,后续由断线回调 / 重连定时器负责;不移交,也不清 authAwaitingReply_ ——
	// 应答前被服务端断开的握手由截止时的 DoReconnect 按 AUTH 超时报告。
	// me 不是当前握手连接:被替换掉的旧握手连接的迟到回调。两种情况都不处理。
	if (reply == nullptr || me != handshakeRedis_.get())
	{
		return;
	}
	authAwaitingReply_ = false; // 有了应答,这次握手的结局在下面当场报告,截止时不再按超时报
	if (reply->type == REDIS_REPLY_STATUS)
	{
		LOG_INFO << "Redis AUTH ok to " << redisAddr_.toIpPort();
		PromoteHandshake();
		return;
	}

	// 失败一律不移交(fail-closed,见头文件契约)。多数情况下服务端会以 NOAUTH 拒绝这条连接上的后续命令,
	// 但服务端没开口令时命令会照常执行,所以不能把这条连接交给业务。
	// 截止定时器已在跑，到点按既有间隔重建；Channel tie 与工厂删除器另外保证回调内释放连接也安全。
	LOG_ERROR << "Redis AUTH rejected by " << redisAddr_.toIpPort() << ": " << DescribeAuthFailure(reply, options_.password)
			  << " -- zone Redis stays unavailable, retrying every " << options_.reconnectIntervalSec
			  << "s (metric=zone_redis_auth_failed)";
	ScheduleReconnect(); // 截止定时器已在跑时是空操作;兜底保证一定会重试
}

void RedisManager::PromoteHandshake()
{
	CancelReconnectTimer();
	// shared_ptr 移动赋值:先让 zoneRedis_ 指向握手连接,再释放被替换的对象。被替换的只会是占位对象或已断开的旧连接
	// (见头文件不变量),析构不触发任何回调。握手连接本身没有被销毁,所以在它自己的连接 / 命令回调栈里移交是安全的。
	zoneRedis_ = std::move(handshakeRedis_);
	// 新连接从零开始探测;旧连接上迟到的 PING 回调因代号不符被忽略。
	ResetLivenessProbe();
	const ReconnectCallback callback = reconnectCb_;
	if (callback)
	{
		callback();
	}
}

void RedisManager::ScheduleReconnect()
{
	if (!loop_ || reconnectTimerActive_)
	{
		return; // Timer already running; avoid stacking duplicate timers.
	}
	const std::uint64_t generation = ++reconnectGeneration_;
	reconnectTimerId_ = loop_->get().runEvery(options_.reconnectIntervalSec,
		[token = std::weak_ptr<LifetimeToken>(lifetimeToken_), this, generation]
		{
			const auto alive = token.lock();
			if (!alive)
			{
				return;
			}
			QueueReconnect(generation);
		});
	reconnectTimerActive_ = true;
}

void RedisManager::CancelReconnectTimer()
{
	// cancel 只能阻止后续定时器回调，已投递的重建任务必须另用代次作废。
	++reconnectGeneration_;
	if (loop_ && reconnectTimerActive_)
	{
		loop_->get().cancel(reconnectTimerId_);
	}
	reconnectTimerActive_ = false;
}

void RedisManager::QueueReconnect(std::uint64_t generation)
{
	if (!reconnectTimerActive_ || generation != reconnectGeneration_)
	{
		return;
	}
	// timerfd 与 socket 可能同批就绪。重建会释放 Hiredis，只能在本轮 I/O 分发完的 pending 阶段执行。
	loop_->get().queueInLoop(
		[token = std::weak_ptr<LifetimeToken>(lifetimeToken_), this, generation]
		{
			const auto alive = token.lock();
			if (!alive)
			{
				return;
			}
			// 同批 AUTH 成功会取消旧代次；建连成功还可能立即启动新 AUTH 截止，不能只看 active。
			if (!reconnectTimerActive_ || generation != reconnectGeneration_)
			{
				return;
			}
			DoReconnect();
		});
}

void RedisManager::DoReconnect()
{
	if (authAwaitingReply_)
	{
		// AUTH 已发出,截止前没有任何应答:服务端卡住、中间代理只接受连接不转发,或应答前连接就被关了
		// (后者此前已有一条 "Redis disconnected" ERROR)。与被拒同样是握手失败、zone Redis 同样不可用,
		// 必须带 metric 打 ERROR,不能只剩下面那条 INFO(按 metric 标记的告警会漏掉)。
		LOG_ERROR << "Redis AUTH timed out on " << redisAddr_.toIpPort() << ": no reply within "
				  << options_.reconnectIntervalSec << "s of connecting -- zone Redis stays unavailable, retrying every "
				  << options_.reconnectIntervalSec << "s (metric=zone_redis_auth_failed)";
		authAwaitingReply_ = false;
	}
	LOG_INFO << "Attempting Redis reconnect to " << redisAddr_.toIpPort();

	// 只重建握手连接:建连失败、握手超时或 AUTH 被拒的旧握手连接在这里(pending 阶段)销毁,
	// 挂起的 AUTH 以空应答回调,OnAuthReply 开头就返回。zoneRedis_ 里已断开的旧连接(或占位对象)保持不动,
	// 业务继续看到 connected()==false,直到新连接完成握手、移交时才被替换。每条新连接都会重新 AUTH。
	ReleaseConnection(handshakeRedis_);
	StartHandshake();
}

void RedisManager::ResetConnections()
{
	// 先清空成员，再释放局部连接：业务空应答即使重入或销毁管理器，也不会再次拆掉同一条连接。
	HiredisPtr oldHandshake = std::move(handshakeRedis_);
	HiredisPtr oldZone = std::move(zoneRedis_);
	authAwaitingReply_ = false;
	ResetLivenessProbe();
	// 显式拆除不是握手失败，不报 AUTH 超时。释放 oldZone 以后不得再访问管理器成员。
	ReleaseConnection(oldHandshake);
	ReleaseConnection(oldZone);
}

void RedisManager::StartLivenessTimer()
{
	if (!loop_ || livenessTimerActive_)
	{
		return;
	}
	livenessTimerId_ = loop_->get().runEvery(
		options_.livenessProbeIntervalSec,
		[token = std::weak_ptr<LifetimeToken>(lifetimeToken_), this]
		{
			if (!token.lock())
			{
				return;
			}
			OnLivenessTick();
		});
	livenessTimerActive_ = true;
}

void RedisManager::CancelLivenessTimer()
{
	if (loop_ && livenessTimerActive_)
	{
		loop_->get().cancel(livenessTimerId_);
		livenessTimerActive_ = false;
	}
}

void RedisManager::OnLivenessTick()
{
	// 只探测对业务可见、仍连着的连接。握手中、断线待重连期间 zoneRedis_ 是占位对象或已断开的旧连接,
	// 由 AUTH 截止与重连定时器负责;握手连接上绝不发 PING(AUTH 必须是它唯一的命令)。
	if (!zoneRedis_ || !zoneRedis_->connected())
	{
		return;
	}

	const auto now = std::chrono::steady_clock::now();
	if (livenessPingOutstanding_)
	{
		if (now - livenessPingSentAt_ >= Seconds(options_.livenessTimeoutSec))
		{
			QueueDropUnresponsive();
		}
		// 截止前继续等;不叠加第二条 PING —— 它只会排在第一条后面,不提供任何新信息。
		return;
	}

	const std::uint64_t generation = livenessGeneration_;
	const int ret = zoneRedis_->command(
		[token = std::weak_ptr<LifetimeToken>(lifetimeToken_), this, generation](hiredis::Hiredis *, redisReply *reply)
		{
			if (!token.lock()) return;
			OnLivenessPingReply(generation, reply);
		},
		"PING");
	if (ret != REDIS_OK)
	{
		// connected() 为真时只在命令格式化失败或内存不足时发生;不算判定依据,下一跳再试。
		LOG_WARN << "Redis liveness PING could not be sent to " << redisAddr_.toIpPort() << " (ret=" << ret
				 << "), retrying in " << options_.livenessProbeIntervalSec << "s";
		return;
	}
	livenessPingOutstanding_ = true;
	livenessPingSentAt_ = now;
}

void RedisManager::OnLivenessPingReply(std::uint64_t generation, const redisReply *reply)
{
	// 空应答:连接正在释放或已断开(含本探测把它摘下),由断线 / 重连路径负责。
	// 代号不符:已被替换掉的旧连接上的迟到回调。两种情况都不碰探测状态。
	if (reply == nullptr || generation != livenessGeneration_)
	{
		return;
	}
	// 任何非空应答(+PONG,或 -BUSY / -LOADING 之类的错误应答)都说明这条连接仍在往返数据。
	livenessPingOutstanding_ = false;
}

void RedisManager::QueueDropUnresponsive()
{
	// 不在这里摘下:本函数运行在定时器回调里,与 socket 事件同属一轮分发(Linux 的 timerfd 与 socket 在同一批
	// activeChannels_ 里;Windows 在 I/O 之后、eventHandling_ 仍为真时跑定时器)。在这里销毁一条本轮也有 I/O 的连接,
	// Linux 是释放后使用(Hiredis 的 Channel 回调绑的是裸指针),Windows Debug 撞 EventLoop::removeChannel 的断言。
	// 排到本轮的 pending functor 阶段再做:那时本轮 Channel 事件已全部分发完。
	loop_->get().queueInLoop(
		[token = std::weak_ptr<LifetimeToken>(lifetimeToken_), this, generation = livenessGeneration_]
		{
			if (!token.lock())
			{
				return; // 已 Shutdown 或重新 Connect:连接已由它们释放
			}
			DropUnresponsive(generation);
		});
}

void RedisManager::DropUnresponsive(std::uint64_t generation)
{
	// 排队期间情况可能已经变了:这条连接自己断开了(断线回调已接手重连),或已被移交 / 显式拆除替换。都不再处理。
	if (!loop_ || generation != livenessGeneration_ || !zoneRedis_ || !zoneRedis_->connected())
	{
		return;
	}

	LOG_ERROR << "Redis liveness probe timed out on " << redisAddr_.toIpPort() << ": PING got no reply within "
			  << options_.livenessTimeoutSec << "s -- dropping the connection (in-flight commands complete with null replies), "
			  << "reconnecting every " << options_.reconnectIntervalSec << "s (metric=zone_redis_liveness_timeout)";
	ResetLivenessProbe();

	// 空应答 = 结果未知,不是"没执行":close 之后内核仍会重传已写出的命令,它们可能晚于重连后新连接上的命令到达 Redis
	// 并被执行(第二轮审计 r2-lc#1 / r2-lc#2)。上层不得假设"摘连接前发出的命令先于重连后发出的命令";需要这种保证的写
	// 必须在 Redis 端用围栏令牌原子判定(例:scene 的 handoff 标记围栏 save_barrier.handoff_fence)。

	// 先换上占位对象再释放:释放时在途命令以空应答回调,回调里若再取 GetZoneRedis(),看到的是 connected()==false 的
	// 占位对象,新命令当场 REDIS_ERR,不会发到正在释放的连接上。释放发生在本类的成员函数里、不在任何 hiredis 回调栈内;
	// 被摘下连接的断线回调先被清掉,不会再走一遍断线日志与 ScheduleReconnect。
	HiredisPtr unresponsive = std::move(zoneRedis_);
	zoneRedis_ = hiredis::Hiredis::Create(&loop_->get(), redisAddr_);
	const std::weak_ptr<LifetimeToken> oldCycle = lifetimeToken_;
	ReleaseConnection(unresponsive);
	// 空应答业务回调可能销毁管理器或开始新 Connect 周期；只检查局部弱令牌，失效后不能再碰 this。
	if (oldCycle.expired())
	{
		return;
	}
	// 走既有重连:此刻 zoneRedis_ 是占位对象,满足「重连定时器只在 zoneRedis_ 不可用时运行」。
	ScheduleReconnect();
}

void RedisManager::ResetLivenessProbe()
{
	++livenessGeneration_;
	livenessPingOutstanding_ = false;
}
