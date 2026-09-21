#pragma once
#ifdef _WIN32
// Windows SDK 的 winsock.h 与 winsock2.h 不能混用；必须先于 Boost/Windows.h。
#include <winsock2.h>
#endif
#include <boost/stacktrace.hpp>
#include <sstream>
#include <string>
#include <algorithm>
#include <cctype>
#include <muduo/base/Logging.h>

#ifdef _WIN32
#include <Windows.h>
#include <TlHelp32.h>
#else
#include <dirent.h>
#endif

constexpr size_t kMaxEntries = 10;

inline std::string GetStackTraceAsString()
{
	boost::stacktrace::stacktrace stack = boost::stacktrace::stacktrace();
	std::ostringstream oss;
	oss << stack;
	return oss.str();
}


// Return current stack trace, limited to maxEntries frames.
inline std::string GetCurrentStackTraceAsString(size_t maxEntries) {
    boost::stacktrace::stacktrace stack = boost::stacktrace::stacktrace();
    size_t stackSize = stack.size();
    size_t numToPrint = std::min(stackSize, maxEntries);

    std::ostringstream oss;
    oss << "Current Stack Trace (last " << numToPrint << " entries):" << std::endl;

    for (size_t i = stackSize - numToPrint; i < stackSize; ++i) {
        oss << stack[i] << std::endl;
    }

    return oss.str();
}

inline void PrintCurrentStackTrace(size_t maxEntries) {
    LOG_ERROR << GetCurrentStackTraceAsString(maxEntries);
}

inline void PrintDefaultStackTrace() {
    PrintCurrentStackTrace(kMaxEntries);
}

inline void PrintStackTraceWithLimit(size_t entries) {
    PrintCurrentStackTrace(entries);
}

// Diagnostic stack trace for signal-driven dumps (todo.md #216).
//
// Operators trigger this by sending the diagnostic signal to a node:
//   Linux:    kill -USR1 <pid>
//   Windows:  GenerateConsoleCtrlEvent(CTRL_BREAK_EVENT, <pid>)  (or Ctrl+Break)
//
// What it prints today:
//   - process thread count (cheap, OS-level)
//   - the calling thread's stack trace (boost::stacktrace, up to kMaxEntries)
//
// Limitation: this dumps only the SIGNAL-HANDLING thread's stack, not every
// thread in the process. A true "all threads" dump portably is substantially
// harder (Linux: pthread_kill broadcast + per-thread async-safe stacktrace;
// Windows: SuspendThread + StackWalk64 with CONTEXT) and is tracked as a
// follow-up to #216. For now this is enough to:
//   - confirm the node is alive and responsive to signals,
//   - capture where the main / signal-handling thread is when ops triggered,
//   - report the process-wide thread count so a leak is visible at a glance.
//
// boost::stacktrace::stacktrace() is not strictly async-signal-safe, so this
// helper should be used as a last-resort diagnostic — not on a hot path.
inline int GetProcessThreadCountForDiagnostic()
{
#ifdef _WIN32
    const DWORD pid = GetCurrentProcessId();
    HANDLE snapshot = CreateToolhelp32Snapshot(TH32CS_SNAPTHREAD, 0);
    if (snapshot == INVALID_HANDLE_VALUE) {
        return -1;
    }
    THREADENTRY32 entry{};
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

inline void DumpProcessStackTraceOnSignal(int signum) {
    const int threadCount = GetProcessThreadCountForDiagnostic();
    LOG_ERROR << "=== Diagnostic stack dump (signal=" << signum
              << ", process_thread_count=" << threadCount
              << ") ===";
    LOG_ERROR << GetCurrentStackTraceAsString(kMaxEntries);
    LOG_ERROR << "=== End diagnostic stack dump ===";
}
