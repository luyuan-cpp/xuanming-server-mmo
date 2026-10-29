#pragma once

#include "muduo/base/Logging.h"
#include "muduo/net/EventLoop.h"

#ifdef _WIN32
#include <Windows.h>
#include <TlHelp32.h>
#else
#include <dirent.h>
#endif

#include <cctype>
#include <chrono>
#include <cstdlib>
#include <cstdint>
#include <cstring>
#include <memory>
#include <optional>
#include <string>

namespace node {
namespace observability {

inline int GetEnvIntWithDefaultAndMin(const char* name, int defaultValue, int minValue)
{
    const char* env = std::getenv(name);
    if (env == nullptr || env[0] == '\0') {
        return defaultValue;
    }

    const int parsed = std::atoi(env);
    return parsed >= minValue ? parsed : defaultValue;
}

inline bool IsDisabledEnvValue(const char* env)
{
    if (env == nullptr || env[0] == '\0') {
        return false;
    }

    if ((env[0] == '0' || env[0] == 'f' || env[0] == 'F' || env[0] == 'n' || env[0] == 'N' || env[0] == 'o' || env[0] == 'O')
        && env[1] == '\0') {
        return true;
    }

    return std::strcmp(env, "false") == 0
        || std::strcmp(env, "FALSE") == 0
        || std::strcmp(env, "off") == 0
        || std::strcmp(env, "OFF") == 0
        || std::strcmp(env, "no") == 0
        || std::strcmp(env, "NO") == 0;
}

inline bool ConfiguredThreadMonitorEnabled()
{
    const char* env = std::getenv("NODE_THREAD_MONITOR_ENABLED");
    return !IsDisabledEnvValue(env);
}

inline std::string NormalizeNodeDisplayName(const std::string& rawName)
{
    if (rawName.empty()) {
        return "Node";
    }

    size_t start = rawName.find_last_of("/\\");
    std::string name = (start == std::string::npos) ? rawName : rawName.substr(start + 1);
    if (name.empty()) {
        name = "Node";
    }

    name[0] = static_cast<char>(std::toupper(static_cast<unsigned char>(name[0])));
    return name;
}

struct ThreadMonitorConfig
{
    int stableWindowSeconds = 60;
    int growthWarnConsecutiveSamples = 6;
    int growthWarnAbsoluteIncrease = 16;
};

inline int ConfiguredThreadMonitorStableWindowSeconds()
{
    return GetEnvIntWithDefaultAndMin("NODE_THREAD_MONITOR_STABLE_WINDOW_SECONDS", 60, 1);
}

inline int ConfiguredThreadMonitorGrowthWarnConsecutiveSamples()
{
    return GetEnvIntWithDefaultAndMin("NODE_THREAD_MONITOR_GROWTH_WARN_CONSECUTIVE_SAMPLES", 6, 1);
}

inline int ConfiguredThreadMonitorGrowthWarnAbsoluteIncrease()
{
    return GetEnvIntWithDefaultAndMin("NODE_THREAD_MONITOR_GROWTH_WARN_ABSOLUTE_INCREASE", 16, 1);
}

inline int ConfiguredThreadMonitorSampleIntervalSeconds()
{
    return GetEnvIntWithDefaultAndMin("NODE_THREAD_MONITOR_SAMPLE_INTERVAL_SECONDS", 10, 1);
}

inline ThreadMonitorConfig GetThreadMonitorConfigFromEnv()
{
    ThreadMonitorConfig config;
    config.stableWindowSeconds = ConfiguredThreadMonitorStableWindowSeconds();
    config.growthWarnConsecutiveSamples = ConfiguredThreadMonitorGrowthWarnConsecutiveSamples();
    config.growthWarnAbsoluteIncrease = ConfiguredThreadMonitorGrowthWarnAbsoluteIncrease();
    return config;
}

struct ThreadMonitorSnapshot
{
    int totalThreads = -1;
    int deltaThreads = 0;
    int peakThreads = -1;
    int baselineThreads = -1;
    int increaseFromBaseline = 0;
    std::int64_t uptimeSeconds = 0;
    int consecutiveGrowthSamples = 0;
    bool baselineReady = false;
    bool baselineJustReady = false;
    bool shouldWarn = false;
};

inline int GetCurrentProcessThreadCount()
{
#ifdef _WIN32
    const DWORD pid = GetCurrentProcessId();
    HANDLE snapshot = CreateToolhelp32Snapshot(TH32CS_SNAPTHREAD, 0);
    if (snapshot == INVALID_HANDLE_VALUE) {
        return -1;
    }

    THREADENTRY32 entry;
    entry.dwSize = sizeof(THREADENTRY32);
    int threadCount = 0;
    if (Thread32First(snapshot, &entry)) {
        do {
            if (entry.th32OwnerProcessID == pid) {
                ++threadCount;
            }
            entry.dwSize = sizeof(THREADENTRY32);
        } while (Thread32Next(snapshot, &entry));
    }

    CloseHandle(snapshot);
    return threadCount;
#else
    DIR* dir = opendir("/proc/self/task");
    if (dir == nullptr) {
        return -1;
    }

    int threadCount = 0;
    while (dirent* entry = readdir(dir)) {
        if (entry->d_name[0] == '.') {
            continue;
        }

        bool isNumeric = true;
        for (const char* p = entry->d_name; *p != '\0'; ++p) {
            if (!std::isdigit(static_cast<unsigned char>(*p))) {
                isNumeric = false;
                break;
            }
        }

        if (isNumeric) {
            ++threadCount;
        }
    }

    closedir(dir);
    return threadCount;
#endif
}

class ThreadMonitor
{
public:
    explicit ThreadMonitor(ThreadMonitorConfig config = {})
        : config_(config)
        , bootTime_(std::chrono::steady_clock::now())
    {
    }

    ThreadMonitorSnapshot Sample()
    {
        ThreadMonitorSnapshot snapshot;
        snapshot.totalThreads = GetCurrentProcessThreadCount();
        snapshot.uptimeSeconds = std::chrono::duration_cast<std::chrono::seconds>(
            std::chrono::steady_clock::now() - bootTime_).count();

        if (lastThreadCount_ >= 0 && snapshot.totalThreads >= 0) {
            snapshot.deltaThreads = snapshot.totalThreads - lastThreadCount_;
        }

        if (snapshot.totalThreads >= 0 && snapshot.totalThreads > peakThreadCount_) {
            peakThreadCount_ = snapshot.totalThreads;
        }
        snapshot.peakThreads = peakThreadCount_;

        if (snapshot.deltaThreads > 0) {
            ++consecutiveGrowthSamples_;
        } else {
            consecutiveGrowthSamples_ = 0;
        }
        snapshot.consecutiveGrowthSamples = consecutiveGrowthSamples_;

        if (!baselineReady_ && snapshot.uptimeSeconds >= config_.stableWindowSeconds) {
            baselineReady_ = true;
            baselineThreadCount_ = snapshot.totalThreads;
            snapshot.baselineJustReady = true;
        }

        snapshot.baselineReady = baselineReady_;
        snapshot.baselineThreads = baselineThreadCount_;

        if (baselineThreadCount_ >= 0 && snapshot.totalThreads >= 0) {
            snapshot.increaseFromBaseline = snapshot.totalThreads - baselineThreadCount_;
        }

        snapshot.shouldWarn = baselineReady_
            && snapshot.totalThreads >= 0
            && consecutiveGrowthSamples_ >= config_.growthWarnConsecutiveSamples
            && snapshot.increaseFromBaseline >= config_.growthWarnAbsoluteIncrease;

        lastThreadCount_ = snapshot.totalThreads;
        return snapshot;
    }

private:
    ThreadMonitorConfig config_;
    std::chrono::steady_clock::time_point bootTime_;
    int lastThreadCount_ = -1;
    int peakThreadCount_ = -1;
    int baselineThreadCount_ = -1;
    int consecutiveGrowthSamples_ = 0;
    bool baselineReady_ = false;
};

inline void RegisterThreadObservability(muduo::net::EventLoop& loop, const std::string& nodeName)
{
    const std::string displayName = NormalizeNodeDisplayName(nodeName);
    if (!ConfiguredThreadMonitorEnabled()) {
        LOG_INFO << displayName << " thread monitor disabled by env NODE_THREAD_MONITOR_ENABLED";
        return;
    }

    const auto config = GetThreadMonitorConfigFromEnv();
    const int sampleIntervalSeconds = ConfiguredThreadMonitorSampleIntervalSeconds();
    auto threadMonitor = std::make_shared<ThreadMonitor>(config);

    LOG_INFO << displayName << " thread monitor config: sample_interval_sec=" << sampleIntervalSeconds
        << ", stable_window_sec=" << config.stableWindowSeconds
        << ", growth_warn_consecutive_samples=" << config.growthWarnConsecutiveSamples
        << ", growth_warn_absolute_increase=" << config.growthWarnAbsoluteIncrease;

    // 空 = 还没打过指标行:第一次采样就打一行,此后每小时一次。不能用 time_point::min() 表示「从未打过」:
    // steady_clock 是 int64 纳秒,now - min() 有符号溢出,实际回绕成负数,指标行就永远不会打出。
    auto lastInfoLogTime = std::make_shared<std::optional<std::chrono::steady_clock::time_point>>();
    loop.runEvery(static_cast<double>(sampleIntervalSeconds), [threadMonitor, displayName, lastInfoLogTime] {
        const auto snapshot = threadMonitor->Sample();
        const auto now = std::chrono::steady_clock::now();
        constexpr int kInfoLogIntervalSeconds = 3600;
        if (!lastInfoLogTime->has_value()
            || std::chrono::duration_cast<std::chrono::seconds>(now - **lastInfoLogTime).count() >= kInfoLogIntervalSeconds) {
            *lastInfoLogTime = now;
            LOG_INFO << displayName << " thread metrics: total_threads=" << snapshot.totalThreads
                << ", delta_threads=" << snapshot.deltaThreads
                << ", peak_threads=" << snapshot.peakThreads
                << ", uptime_sec=" << snapshot.uptimeSeconds;
        }

        if (snapshot.baselineJustReady) {
            LOG_INFO << displayName << " thread baseline (stable window reached): total_threads=" << snapshot.totalThreads
                << ", delta_threads=" << snapshot.deltaThreads
                << ", peak_threads=" << snapshot.peakThreads
                << ", baseline_threads=" << snapshot.baselineThreads
                << ", uptime_sec=" << snapshot.uptimeSeconds;
        }

        if (snapshot.shouldWarn) {
            LOG_WARN << displayName << " thread growth warning: total_threads=" << snapshot.totalThreads
                << ", baseline_threads=" << snapshot.baselineThreads
                << ", increase_from_baseline=" << snapshot.increaseFromBaseline
                << ", consecutive_growth_samples=" << snapshot.consecutiveGrowthSamples
                << ", peak_threads=" << snapshot.peakThreads
                << ", uptime_sec=" << snapshot.uptimeSeconds;
        }
    });
}

} // namespace observability
} // namespace node
