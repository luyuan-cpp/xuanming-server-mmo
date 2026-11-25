#include <boost/asio.hpp>
#include <memory>
#include <functional>
#include <chrono>

class TimerTaskBoostComp : public std::enable_shared_from_this<TimerTaskBoostComp> {
public:
	using TimerCallback = std::function<void()>;

	explicit TimerTaskBoostComp(boost::asio::io_context& io)
		: io(io) {
	}

	~TimerTaskBoostComp() {
		Cancel();
	}

	void RunAt(std::chrono::steady_clock::time_point time, const TimerCallback& cb) {
		Cancel();
		callback = cb;
		timer = std::make_shared<boost::asio::steady_timer>(io, time - std::chrono::steady_clock::now());
		timer->async_wait([this, self = shared_from_this()](const boost::system::error_code& ec) {
			if (!ec) OnTimer();
			});
	}

	void RunAfter(double delaySeconds, const TimerCallback& cb) {
		RunAt(std::chrono::steady_clock::now() + std::chrono::milliseconds((int)(delaySeconds * 1000)), cb);
	}

	void RunEvery(double intervalSeconds, const TimerCallback& cb) {
		Cancel();
		interval = std::chrono::milliseconds((int)(intervalSeconds * 1000));
		callback = cb;
		timer = std::make_shared<boost::asio::steady_timer>(io, interval);
		timer->async_wait([this, self = shared_from_this()](const boost::system::error_code& ec) {
			if (!ec) OnRecurringTimer();
			});
	}

	void Cancel() {
		if (timer) {
			timer->cancel();
			timer.reset();
		}
		callback = nullptr;
	}

	bool IsActive() const {
		return timer != nullptr;
	}

	uint64_t GetEndTime() const {
		// Return expiration time as epoch seconds
		if (timer) {
			auto tp = timer->expiry();
			return std::chrono::duration_cast<std::chrono::seconds>(
				tp.time_since_epoch()).count();
		}
		return 0;
	}

	void SetCallback(const TimerCallback& cb) {
		callback = cb;
	}

private:
	void OnTimer() {
		if (callback) {
			callback();
		}
	}

	void OnRecurringTimer() {
		if (callback) {
			callback();
			timer->expires_after(interval);
			timer->async_wait([this, self = shared_from_this()](const boost::system::error_code& ec) {
				if (!ec) OnRecurringTimer();
				});
		}
	}

	boost::asio::io_context& io;
	std::shared_ptr<boost::asio::steady_timer> timer;
	std::chrono::milliseconds interval;
	TimerCallback callback;
};
