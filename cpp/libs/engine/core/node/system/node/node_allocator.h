#pragma once

#include <cstdint>
#include <unordered_set>

class NodeAllocator {
public:
	static void AcquireNode();

	// Returns false when no RPC port could be allocated. The caller must retry;
	// nothing is published to etcd on failure. Publishing port=0 would advertise
	// an endpoint peers cannot connect to and is never a valid outcome.
	static bool AcquireNodePort();

	static void ReRegisterExistingNode();

	// CAS losers: ids we tried and lost. Persists across the
	// allocateNodeTimer.RunAfter(1, AcquireNode) retry path so that the
	// next attempt skips ids we already know are taken globally, without
	// having to wait for the etcd watch stream to deliver the winning
	// peer's NodeInfo into our local ServiceNodeList snapshot.
	static void RecordLostId(uint32_t id);

private:
	// Process-local set of node_ids whose global allocation CAS we have
	// already lost during this boot. Cleared only on process exit.
	static std::unordered_set<uint32_t>& lostIds();
};