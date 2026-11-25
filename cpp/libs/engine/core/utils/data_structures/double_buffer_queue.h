#pragma once

#include "muduo/base/Mutex.h"

#include <new>
#include <utility>
#include <vector>

// Project-owned double-buffer queue.
//
// Characteristics:
// - multi-producer, single-consumer
// - non-blocking take()
// - consumer can process a fixed number of items per frame
// - read side is lock-free in the steady state
//
// Typical usage:
//   DoubleBufferQueue<Task> queue;
//
//   // producer threads
//   queue.put(task);
//
//   // main loop / frame tick
//   constexpr size_t kMaxPerFrame = 64;
//   for (size_t i = 0; i < kMaxPerFrame; ++i)
//   {
//       Task task;
//       if (!queue.take(task))
//       {
//           break;
//       }
//       task();
//   }
template <typename T>
class DoubleBufferQueue : noncopyable
{
public:
    using queue_type = std::vector<T>;

    DoubleBufferQueue() = default;

    void put(const T& x)
    {
        muduo::MutexLockGuard lock(mutex);
        buffers[writeIndex].push_back(x);
    }

    void put(T&& x)
    {
        muduo::MutexLockGuard lock(mutex);
        buffers[writeIndex].push_back(std::move(x));
    }

    template <typename... Args>
    void emplace(Args&&... args)
    {
        muduo::MutexLockGuard lock(mutex);
        buffers[writeIndex].emplace_back(std::forward<Args>(args)...);
    }

    // Non-blocking. Returns false immediately when no item is available.
    // Single-consumer only.  Zero atomic ops in the steady-state hot path.
    bool take(T& out)
    {
        if (readPos >= readEnd)
        {
            if (!swapReadBuffer())
            {
                return false;
            }
        }

        out = std::move(buffers[readIndex][readPos]);
        ++readPos;
        return true;
    }

private:
    bool swapReadBuffer()
    {
        buffers[readIndex].clear();
        readPos = 0;
        readEnd = 0;

        muduo::MutexLockGuard lock(mutex);
        if (buffers[writeIndex].empty())
        {
            return false;
        }

        std::swap(readIndex, writeIndex);
        readEnd = buffers[readIndex].size();
        return true;
    }

private:
    // -- Producer-side (written under mutex) --
    alignas(std::hardware_destructive_interference_size) mutable muduo::MutexLock mutex;
    queue_type buffers[2];
    int writeIndex GUARDED_BY(mutex) = 0;

    // -- Consumer-side (single-thread, no lock) --
    alignas(std::hardware_destructive_interference_size) int readIndex = 1;
    size_t readPos = 0;
    size_t readEnd = 0;
};
