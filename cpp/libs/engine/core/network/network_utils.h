#pragma once

#include <optional>
#include <string>
#include "core/network/grpc_client_call.h"
#include "entt/src/entt/entity/registry.hpp"
#include "type_define/type_define.h"
#include "core/utils/id/node_id_generator.h"
#include <utils/id/snow_flake.h>

using SessionIdGenerator = TransientNodeCompositeIdGenerator<uint32_t, kNodeBits>;

void ParseIpPort(const std::string &input, std::string &ip, uint16_t &port);

std::string FormatIpAndPort(const std::string &ip, uint32_t port);

NodeId GetGateNodeId(SessionId session_id);

// Resolve the entt::entity of a Gate node in THIS process's GateNodeService
// registry, given a session_id originated by some Gate.
//
// Post uuid-refactor, entt::entity is no longer numerically equal to node_id
// (ConnectToTcpNode now uses registry.create() and keys nodes by uuid), so
// callers in non-Gate processes (primarily Scene) must not do
// `entt::entity{GetGateNodeId(session_id)}` — that would treat a business
// node_id as an arbitrary entity slot and miss the real entity.
//
// Scene only talks to same-zone Gates (cross-zone flows are redirected at the
// SceneManager layer), so (self_zone, node_id) is a valid composite key here.
// Returns std::nullopt if no matching Gate has been discovered locally.
std::optional<entt::entity> ResolveLocalZoneGateEntity(SessionId sessionId);
