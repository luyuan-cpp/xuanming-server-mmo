#pragma once

#include <grpcpp/grpcpp.h>
#include <grpcpp/resource_quota.h>

#include <memory>
#include <string>
#include <unordered_map>

namespace grpc_channel_cache {

inline int GetGrpcMaxThreads() {
    const char* env = std::getenv("GRPC_MAX_THREADS");
    if (env == nullptr || env[0] == '\0') {
        return 2;
    }
    const int parsed = std::atoi(env);
    return parsed > 0 ? parsed : 2;
}

inline int GetGrpcBackupPollIntervalMs() {
    const char* env = std::getenv("GRPC_CLIENT_CHANNEL_BACKUP_POLL_INTERVAL_MS");
    if (env == nullptr || env[0] == '\0') {
        return 5000;
    }
    const int parsed = std::atoi(env);
    return parsed >= 0 ? parsed : 5000;
}

// ── HTTP/2 keepalive tunables ─────────────────────────────────────────
// Defaults chosen for long-lived cross-service gRPC channels on Linux
// hosts behind NAT / cloud LBs (AWS NLB idles at 350s, most k8s Service
// meshes at 60–120s).
//   GRPC_KEEPALIVE_TIME_MS           - how often the client sends PING when
//                                      no active RPC; 30s is safely below
//                                      common NAT idle thresholds.
//   GRPC_KEEPALIVE_TIMEOUT_MS        - how long to wait for the PING ACK
//                                      before marking the connection dead.
//   GRPC_KEEPALIVE_PERMIT_WITHOUT_CALLS - 1 = ping even when idle. Requires
//                                      the server to opt-in via
//                                      KeepalivePolicy::MinRecvPingInterval.
// Set GRPC_KEEPALIVE_TIME_MS=0 to disable client-side keepalive entirely
// (useful if the remote server is stricter about ping cadence).
inline int GetGrpcKeepaliveTimeMs() {
    const char* env = std::getenv("GRPC_KEEPALIVE_TIME_MS");
    if (env == nullptr || env[0] == '\0') {
        return 30000;
    }
    const int parsed = std::atoi(env);
    return parsed >= 0 ? parsed : 30000;
}

inline int GetGrpcKeepaliveTimeoutMs() {
    const char* env = std::getenv("GRPC_KEEPALIVE_TIMEOUT_MS");
    if (env == nullptr || env[0] == '\0') {
        return 10000;
    }
    const int parsed = std::atoi(env);
    return parsed > 0 ? parsed : 10000;
}

inline int GetGrpcKeepalivePermitWithoutCalls() {
    const char* env = std::getenv("GRPC_KEEPALIVE_PERMIT_WITHOUT_CALLS");
    if (env == nullptr || env[0] == '\0') {
        return 1;
    }
    const int parsed = std::atoi(env);
    return parsed > 0 ? 1 : 0;
}

inline int ConfiguredMaxThreads() {
    return GetGrpcMaxThreads();
}

inline int ConfiguredBackupPollIntervalMs() {
    return GetGrpcBackupPollIntervalMs();
}

class GrpcChannelCache {
public:
    GrpcChannelCache()
        : resourceQuota_("node-grpc-client-rq"),
          maxThreads_(GetGrpcMaxThreads()),
          backupPollIntervalMs_(GetGrpcBackupPollIntervalMs()),
          keepaliveTimeMs_(GetGrpcKeepaliveTimeMs()),
          keepaliveTimeoutMs_(GetGrpcKeepaliveTimeoutMs()),
          keepalivePermitWithoutCalls_(GetGrpcKeepalivePermitWithoutCalls()) {
        resourceQuota_.SetMaxThreads(maxThreads_);
    }

    int ConfiguredMaxThreads() const {
        return maxThreads_;
    }

    int ConfiguredBackupPollIntervalMs() const {
        return backupPollIntervalMs_;
    }

    size_t CachedTargetCount() {
        for (auto it = channelCache_.begin(); it != channelCache_.end();) {
            if (it->second.expired()) {
                it = channelCache_.erase(it);
            } else {
                ++it;
            }
        }
        return channelCache_.size();
    }

    std::shared_ptr<grpc::Channel> GetOrCreateChannel(const std::string& target) {
        auto it = channelCache_.find(target);
        if (it != channelCache_.end()) {
            if (auto existing = it->second.lock()) {
                return existing;
            }
        }

        grpc::ChannelArguments args;
        args.SetResourceQuota(resourceQuota_);
        args.SetInt("grpc.client_channel_backup_poll_interval_ms", backupPollIntervalMs_);

        // HTTP/2 keepalive: prevents NAT / LB idle-timeout from silently killing
        // the long-lived channel and forcing a fresh TCP handshake on the next
        // call (which would briefly consume an ephemeral port and briefly look
        // like a "short connection" under pressure).
        //
        // Tunable via env so ops can disable if the login-side server refuses
        // frequent pings (gRPC servers default to rejecting >2 pings per hour
        // when no active RPC is in flight; KEEPALIVE_PERMIT_WITHOUT_CALLS=1
        // on the client requires the server to enable MinRecvPingInterval).
        args.SetInt(GRPC_ARG_KEEPALIVE_TIME_MS,                keepaliveTimeMs_);
        args.SetInt(GRPC_ARG_KEEPALIVE_TIMEOUT_MS,             keepaliveTimeoutMs_);
        args.SetInt(GRPC_ARG_KEEPALIVE_PERMIT_WITHOUT_CALLS,   keepalivePermitWithoutCalls_);
        args.SetInt(GRPC_ARG_HTTP2_MAX_PINGS_WITHOUT_DATA,     0);

        auto channel = grpc::CreateCustomChannel(target, grpc::InsecureChannelCredentials(), args);
        channelCache_[target] = channel;
        return channel;
    }

private:
    std::unordered_map<std::string, std::weak_ptr<grpc::Channel>> channelCache_;
    grpc::ResourceQuota resourceQuota_;
    int maxThreads_;
    int backupPollIntervalMs_;
    int keepaliveTimeMs_;
    int keepaliveTimeoutMs_;
    int keepalivePermitWithoutCalls_;
};

}  // namespace grpc_channel_cache
