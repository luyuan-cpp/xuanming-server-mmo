#pragma once

// Proto-compare-driven dirty detection (todo.md #204 / #226).
//
// The "save when changed" pattern in the codebase today relies on hand-rolled
// `bool dirty{false}` flags scattered across components (e.g.
// `PlayerCurrencyComp::dirty`). The flags are easy to forget to set on a new
// mutation path, which means data either gets written every save (wasteful)
// or never gets written when the flag was missed (silent loss).
//
// A more robust pattern is to keep the last persisted serialization as a
// reference, then compare the current serialization against it via
// google::protobuf::util::MessageDifferencer. This is more expensive per save
// but eliminates the "did I forget to set dirty?" bug class entirely.
//
// This header is the small, dependency-free building block. The rollout into
// SavePlayer itself is staged separately (see
// docs/design/proto-compare-dirty-save.md) because it interacts with the
// per-player snapshot retention strategy and that decision needs more
// thought than a header-only utility.

#include <google/protobuf/message.h>
#include <google/protobuf/util/message_differencer.h>

namespace dirty_save {

// Equality check that treats two protobuf messages as identical when their
// effective content is the same. Wraps MessageDifferencer::Equals with the
// settings appropriate for "did anything that matters for persistence
// change?":
//   - default field comparison (proto3 default values are equal to "unset")
//   - default repeated-field comparison (order matters — fine for our
//     repeated lists today; if we add unordered sets we'll need a per-call
//     override)
//   - default map comparison (MessageDifferencer treats map<K,V> as
//     unordered automatically since proto 3.5)
//
// Returns true iff the two messages are byte-equivalent in semantics. Cost
// is roughly proportional to message size — for a typical player snapshot
// (~few KB) it's well under 100µs on modern x86, dominated by descriptor
// walking rather than comparison work.
inline bool IsEqual(const ::google::protobuf::Message& a,
                    const ::google::protobuf::Message& b)
{
    return ::google::protobuf::util::MessageDifferencer::Equals(a, b);
}

// Convenience: returns true when `current` differs from `lastPersisted` and
// thus needs to be written. The verbose name discourages confusing this
// with `==`. `lastPersisted == nullptr` is treated as "first save" → dirty.
inline bool ShouldPersist(const ::google::protobuf::Message& current,
                          const ::google::protobuf::Message* lastPersisted)
{
    if (lastPersisted == nullptr)
    {
        return true;
    }
    return !IsEqual(current, *lastPersisted);
}

} // namespace dirty_save
