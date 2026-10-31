#include "node.h"
#include <regex>
#include <boost/uuid/uuid_io.hpp>
#include "table/code/all_table.h"
#include "node/system/etcd/etcd_helper.h"
#include "config.h"
#include "google/protobuf/util/json_util.h"
#include "google/protobuf/util/message_differencer.h"
#include "log/constants/log_constants.h"
#include "log/system/console_log.h"
#include "muduo/base/TimeZone.h"
#include "network/process_info.h"
#include "network/rpc_session.h"
#include "node/system/node/node_util.h"
#include "table/proto/tip/common_error_tip.pb.h"
#include "proto/common/base/node.pb.h"
#include "node/system/node/node_events.h"
#include "rpc/service_metadata/scene_service_metadata.h"
#include "rpc/service_metadata/gate_service_service_metadata.h"
#include "rpc/service_metadata/rpc_event_registry.h"
#include "thread_context/ecs_context.h"
#include "thread_context/redis_manager.h"
#include "infra/storage/redis_cluster/redis_cluster_client.h"
#include "time/system/time.h"
#include "core/utils/debug/stacktrace_system.h"
#include "build_info/build_info.h"
#include "error_reporter/error_reporter.h"
#include "network/node_utils.h"
#include "network/grpc_client_runtime.h"
#include "node/system/node/thread_observability.h"
#include "network/traffic_statistics.h"
#include "network/rpc_channel_stats.h"
#include "metrics/metrics_http_exporter.h"
#include "metrics/metrics_registry.h"
#include <boost/algorithm/string.hpp>
#include "node/system/etcd/etcd_service.h"
#include "node/system/grpc_call_deadline.h"
#include "node/system/node/node_connector.h"
#include "node/system/node/muduo_grpc_channel_slot.h"
#include "node/system/node/client_endpoint.h"
#include "node/system/node/open_file_limit.h"
#include "engine/config/node_log_level.h"
#include "thread_context/node_context_manager.h"
#include "muduo_grpc/address.h"
#include "muduo_grpc/method_table.h"
#include "muduo_grpc/server.h"
#include <node_config_manager.h>
#include <atomic>
#include <chrono>
#include <cassert>
#include <cerrno>
#include <csignal>
#include <cstdio>
#include <cstdlib>
#include <limits>
#include <optional>
#ifdef __linux__
#include <sys/resource.h>
#endif


#include <algorithm>
#include <filesystem>
#include <string>
#include <string_view>
#include <fstream>
#include <future>
#include <thread>
#include <stdexcept>
#include <array>
#include <boost/uuid/string_generator.hpp>
#include "utils/encode/sha256.h"
#include "proto/common/database/rollback_database_table.pb.h"
#include "google/protobuf/unknown_field_set.h"
#include "messaging/kafka/kafka_producer.h"
#include "muduo_grpc/channel.h"
#ifdef _WIN32
#include <windows.h>
#ifdef GetMessage
#undef GetMessage
#endif
#else
#include <fcntl.h>
#include <poll.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/wait.h>
#include <unistd.h>
extern char **environ;
#endif

namespace {
using MaintenanceReceipt = sql_writer_maintenance_receipt;
constexpr std::size_t kControlLimit = 16 * 1024 * 1024;
// 维护文件名按 UTF-8 解释，char8_t 字符串保留 Windows 路径编码语义。
std::filesystem::path PathFromUtf8(std::string_view value) {
    return std::filesystem::path(std::u8string(value.begin(), value.end()));
}
std::string ReadPinnedBytes(const std::string& file, std::size_t limit) {
    const std::filesystem::path p = PathFromUtf8(file);
    if (!p.is_absolute() || !std::filesystem::is_regular_file(std::filesystem::symlink_status(p))) throw std::runtime_error("维护原件无效");
    const auto size = std::filesystem::file_size(p);
    if (size == 0 || size > limit) throw std::runtime_error("维护原件大小无效");
    std::ifstream input(p, std::ios::binary);
    std::string out(static_cast<std::size_t>(size), '\0');
    if (!input.read(out.data(), static_cast<std::streamsize>(out.size())) || input.peek() != std::char_traits<char>::eof()) throw std::runtime_error("维护原件读取失败");
    return out;
}
std::string PathUtf8(const std::filesystem::path& path) { const auto raw = path.u8string(); return std::string(reinterpret_cast<const char*>(raw.data()), raw.size()); }
std::string ControlHex(const std::string& bytes) {
    constexpr char digits[] = "0123456789abcdef";
    std::string out; out.reserve(bytes.size() * 2);
    for (unsigned char b : bytes) { out.push_back(digits[b >> 4]); out.push_back(digits[b & 15]); }
    return out;
}
bool NoUnknownProto(const google::protobuf::Message& message) {
    const auto* reflection = message.GetReflection();
    if (reflection->GetUnknownFields(message).field_count() != 0) return false;
    std::vector<const google::protobuf::FieldDescriptor*> fields;
    reflection->ListFields(message, &fields);
    for (const auto* f : fields) if (f->cpp_type() == google::protobuf::FieldDescriptor::CPPTYPE_MESSAGE) {
        if (f->is_repeated()) { for (int i = 0; i < reflection->FieldSize(message, f); ++i) if (!NoUnknownProto(reflection->GetRepeatedMessage(message, f, i))) return false; }
        else if (!NoUnknownProto(reflection->GetMessage(message, f))) return false;
    }
    return true;
}
// 只执行明确 pin 的本地文件 owner；没有 shell、网络或 SQL。所有调用均在 std::async 工作线程上。
// stdout 只回传有界原字节；stderr 丢到空设备，路径/环境/原件正文不进入节点日志。
std::string PrivateControlCommand(const std::string& helper, const std::string& expectedHash,
                                  const std::string& command, const std::string& file,
                                  const std::string& input = {}) {
    if (!PathFromUtf8(helper).is_absolute() || !PathFromUtf8(file).is_absolute() ||
        expectedHash.size() != 64 || input.size() > kControlLimit) throw std::runtime_error("维护文件助手配置无效");
    std::string output;
    const auto deadline = std::chrono::steady_clock::now() + std::chrono::seconds(3);
#ifdef _WIN32
    const auto wide = [](const std::string& s) { return PathFromUtf8(s).wstring(); };
    const auto quote = [](const std::wstring& s) {
        std::wstring out = L"\""; std::size_t slashes = 0;
        for (wchar_t c : s) { if (c == L'\\') { ++slashes; continue; }
            if (c == L'\"') out.append(slashes * 2 + 1, L'\\'); else out.append(slashes, L'\\');
            slashes = 0; out.push_back(c); }
        out.append(slashes * 2, L'\\'); out.push_back(L'\"'); return out;
    };
    // 持有不共享写入/删除的二进制句柄直到 CreateProcess 返回，消除 SHA 检查到执行之间的替换窗口。
    HANDLE binary = CreateFileW(wide(helper).c_str(), GENERIC_READ, FILE_SHARE_READ, nullptr, OPEN_EXISTING,
                                FILE_ATTRIBUTE_NORMAL | FILE_FLAG_OPEN_REPARSE_POINT, nullptr);
    if (binary == INVALID_HANDLE_VALUE) throw std::runtime_error("维护文件助手不可用");
    BY_HANDLE_FILE_INFORMATION info{}; LARGE_INTEGER size{}; std::string executable;
    bool pinned = GetFileInformationByHandle(binary, &info) && !(info.dwFileAttributes & (FILE_ATTRIBUTE_DIRECTORY | FILE_ATTRIBUTE_REPARSE_POINT)) &&
                  GetFileSizeEx(binary, &size) && size.QuadPart > 0 && size.QuadPart <= 64 * 1024 * 1024;
    if (pinned) { executable.resize(static_cast<std::size_t>(size.QuadPart)); DWORD count = 0;
        pinned = ReadFile(binary, executable.data(), static_cast<DWORD>(executable.size()), &count, nullptr) && count == executable.size() && Sha256::HashToHex(executable) == expectedHash; }
    if (!pinned) { CloseHandle(binary); throw std::runtime_error("维护文件助手摘要不符"); }
    SECURITY_ATTRIBUTES sa{sizeof(sa), nullptr, TRUE};
    HANDLE childIn = nullptr, parentIn = nullptr, parentOut = nullptr, childOut = nullptr;
    if (!CreatePipe(&childIn, &parentIn, &sa, 0) || !CreatePipe(&parentOut, &childOut, &sa, 0)) {
        for (HANDLE h : {childIn, parentIn, parentOut, childOut}) if (h) CloseHandle(h);
        CloseHandle(binary); throw std::runtime_error("维护文件助手管道失败"); }
    SetHandleInformation(parentIn, HANDLE_FLAG_INHERIT, 0); SetHandleInformation(parentOut, HANDLE_FLAG_INHERIT, 0);
    HANDLE nullError = CreateFileW(L"NUL", GENERIC_WRITE, FILE_SHARE_READ | FILE_SHARE_WRITE, &sa, OPEN_EXISTING, 0, nullptr);
    STARTUPINFOEXW startup{}; startup.StartupInfo.cb = sizeof(startup);
    startup.StartupInfo.dwFlags = STARTF_USESTDHANDLES; startup.StartupInfo.hStdInput = childIn;
    startup.StartupInfo.hStdOutput = childOut; startup.StartupInfo.hStdError = nullError;
    SIZE_T attributeSize = 0; InitializeProcThreadAttributeList(nullptr, 1, 0, &attributeSize);
    std::vector<unsigned char> attributes(attributeSize); startup.lpAttributeList = reinterpret_cast<LPPROC_THREAD_ATTRIBUTE_LIST>(attributes.data());
    HANDLE inherited[] = {childIn, childOut, nullError};
    bool attributesReady = nullError != INVALID_HANDLE_VALUE && InitializeProcThreadAttributeList(startup.lpAttributeList, 1, 0, &attributeSize);
    bool ready = attributesReady && UpdateProcThreadAttribute(startup.lpAttributeList, 0, PROC_THREAD_ATTRIBUTE_HANDLE_LIST, inherited, sizeof(inherited), nullptr, nullptr);
    std::wstring arguments = quote(wide(helper)) + L" --command " + quote(wide(command)) + L" --path " + quote(wide(file)) + L" --max-bytes 16777216";
    PROCESS_INFORMATION process{};
    const bool started = ready && CreateProcessW(wide(helper).c_str(), arguments.data(), nullptr, nullptr, TRUE,
                                                 CREATE_NO_WINDOW | EXTENDED_STARTUPINFO_PRESENT, nullptr, nullptr, &startup.StartupInfo, &process);
    if (attributesReady) DeleteProcThreadAttributeList(startup.lpAttributeList);
    CloseHandle(binary); CloseHandle(childIn); CloseHandle(childOut); if (nullError != INVALID_HANDLE_VALUE) CloseHandle(nullError);
    if (!started) { CloseHandle(parentIn); CloseHandle(parentOut); throw std::runtime_error("维护文件助手启动失败"); }
    CloseHandle(process.hThread);
    std::atomic<bool> writeOk{true};
    std::thread writer;
    bool ok = true;
    try {
    writer = std::thread([&] { std::size_t offset = 0; while (offset < input.size()) { DWORD count = 0;
        if (!WriteFile(parentIn, input.data() + offset, static_cast<DWORD>((std::min)(input.size() - offset, std::size_t(4096))), &count, nullptr) || count == 0) { writeOk = false; break; } offset += count; }
        CloseHandle(parentIn); });
    for (;;) {
        DWORD available = 0;
        if (!PeekNamedPipe(parentOut, nullptr, 0, nullptr, &available, nullptr)) {
            if (GetLastError() != ERROR_BROKEN_PIPE) { ok = false; break; }
            // stdout 提前关闭不证明 child 已退出；仍按同一截止时间等实际进程终局。
            available = 0;
        }
        if (available) { char buffer[4096]; DWORD count = 0;
            if (!ReadFile(parentOut, buffer, (std::min)(available, DWORD(sizeof(buffer))), &count, nullptr)) { ok = false; break; }
            output.append(buffer, count); if (output.size() > kControlLimit) { ok = false; break; } continue; }
        if (WaitForSingleObject(process.hProcess, 0) == WAIT_OBJECT_0) break;
        if (std::chrono::steady_clock::now() >= deadline) { ok = false; break; }
        std::this_thread::sleep_for(std::chrono::milliseconds(5));
    }
    } catch (...) {
        TerminateProcess(process.hProcess, 1); WaitForSingleObject(process.hProcess, INFINITE);
        if (writer.joinable()) writer.join(); else CloseHandle(parentIn);
        CloseHandle(process.hProcess); CloseHandle(parentOut); throw;
    }
    if (!ok) TerminateProcess(process.hProcess, 1);
    WaitForSingleObject(process.hProcess, INFINITE); // 被终止的单个受控 helper 必须实际退出，不能遗留写原件的进程。
    writer.join(); DWORD exitCode = 1; GetExitCodeProcess(process.hProcess, &exitCode);
    CloseHandle(process.hProcess); CloseHandle(parentOut);
    if (!ok || !writeOk || exitCode != 0) throw std::runtime_error("维护文件助手拒绝或超时");
#else
    const int binary = ::open(helper.c_str(), O_RDONLY | O_CLOEXEC | O_NOFOLLOW);
    struct stat info{}; std::string executable;
    bool pinned = binary >= 0 && ::fstat(binary, &info) == 0 && S_ISREG(info.st_mode) && !(info.st_mode & 0022) &&
                  info.st_size > 0 && info.st_size <= 64 * 1024 * 1024;
    if (pinned) { executable.resize(static_cast<std::size_t>(info.st_size)); std::size_t n = 0;
        while (n < executable.size()) { const auto got = ::read(binary, executable.data() + n, executable.size() - n); if (got <= 0) { pinned = false; break; } n += static_cast<std::size_t>(got); }
        pinned = pinned && Sha256::HashToHex(executable) == expectedHash; }
    if (!pinned) { if (binary >= 0) ::close(binary); throw std::runtime_error("维护文件助手摘要不符"); }
    int channel[2]; if (::socketpair(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0, channel) != 0) { ::close(binary); throw std::runtime_error("维护文件助手管道失败"); }
    char* const argv[] = {const_cast<char*>(helper.c_str()), const_cast<char*>("--command"), const_cast<char*>(command.c_str()),
        const_cast<char*>("--path"), const_cast<char*>(file.c_str()), const_cast<char*>("--max-bytes"), const_cast<char*>("16777216"), nullptr};
    const pid_t child = ::fork();
    if (child == 0) {
        ::dup2(channel[1], STDIN_FILENO); ::dup2(channel[1], STDOUT_FILENO);
        const int nullError = ::open("/dev/null", O_WRONLY); if (nullError >= 0) ::dup2(nullError, STDERR_FILENO);
        ::close(channel[0]); ::close(channel[1]); if (nullError > STDERR_FILENO) ::close(nullError);
        ::fexecve(binary, argv, environ); ::_exit(127);
    }
    ::close(binary); ::close(channel[1]);
    if (child < 0) { ::close(channel[0]); throw std::runtime_error("维护文件助手启动失败"); }
    ::fcntl(channel[0], F_SETFL, ::fcntl(channel[0], F_GETFL) | O_NONBLOCK);
    std::size_t offset = 0; bool inputClosed = false, ok = true, exited = false; int status = 0;
    for (;;) {
        if (offset < input.size()) { const auto sent = ::send(channel[0], input.data() + offset, input.size() - offset, MSG_NOSIGNAL);
            if (sent > 0) offset += static_cast<std::size_t>(sent); else if (sent < 0 && errno != EAGAIN && errno != EINTR) { ok = false; break; } }
        if (offset == input.size() && !inputClosed) { ::shutdown(channel[0], SHUT_WR); inputClosed = true; }
        char buffer[4096]; const auto count = ::recv(channel[0], buffer, sizeof(buffer), 0);
        if (count > 0) { output.append(buffer, static_cast<std::size_t>(count)); if (output.size() > kControlLimit) { ok = false; break; } continue; }
        if (count < 0 && errno != EAGAIN && errno != EINTR) { ok = false; break; }
        const auto wait = ::waitpid(child, &status, WNOHANG); if (wait == child) exited = true;
        if (exited && count == 0) break;
        if (std::chrono::steady_clock::now() >= deadline) { ok = false; break; }
        pollfd pfd{channel[0], static_cast<short>(POLLIN | (offset < input.size() ? POLLOUT : 0)), 0}; ::poll(&pfd, 1, 10);
    }
    if (!exited) { ::kill(child, SIGKILL); while (::waitpid(child, &status, 0) < 0 && errno == EINTR) {} }
    ::close(channel[0]);
    if (!ok || offset != input.size() || !WIFEXITED(status) || WEXITSTATUS(status) != 0) throw std::runtime_error("维护文件助手拒绝或超时");
#endif
    if (command != "read-file" && !output.empty()) throw std::runtime_error("维护文件助手输出无效");
    return output;
}
}


// Node 独占生命周期状态，文件工作线程只持值副本；只有 loop 线程能封业务入口/读取 owner 前沿。
struct NodeMaintenanceControl {
    struct ReadResult {
        bool ok = true, active = false, sourceRequested = false, relayRequested = false;
        MaintenanceReceipt request;
        MaintenanceReceipt::Plan plan;
        std::optional<MaintenanceReceipt::IssuerClosure> source, relay;
        std::optional<MaintenanceReceipt::ManualConsumerEvidence> baseline;
        std::optional<MaintenanceReceipt::SourceOpenAck> open;
    };
    std::string directory, helper, helperHash, instance;
    std::future<ReadResult> reading;
    std::future<bool> writing;
    ReadResult runtime;
    bool loaded = false, held = false, sourceClosed = false, relayClosed = false;
    bool sourceWritten = false, relayWritten = false, writingSource = false, relayDraining = false;
    std::uint64_t sourceClosedMs = 0, relayClosedMs = 0;
    std::string sourceFrontier;
    bool prepareBoot=false, baselineWritten=false, manualWritten=false, preparedWritten=false, opened=false, identityWritten=false;
    int writingOther=0;
    std::optional<MaintenanceReceipt::ManualConsumerEvidence> baselineIntent;
    std::future<std::optional<MaintenanceReceipt::ManualConsumerEvidence>> manualReading;
    std::future<std::optional<MaintenanceReceipt::ConsumerReadiness>> preparing;
    std::future<bool> manualDraining;
    bool manualDrainReady=false, manualConsumersStopped=false;
    MaintenanceReceipt::ConsumerReadiness consumers;
    MaintenanceReceipt::IssuerReadiness readiness;
    std::string manualDigest;
    std::optional<MaintenanceReceipt::SourceOpenAck> openIntent;
    std::chrono::steady_clock::time_point stageDeadline{};
    static std::uint64_t NowMs() {
        const auto value = std::chrono::duration_cast<std::chrono::milliseconds>(std::chrono::system_clock::now().time_since_epoch()).count();
        return value > 0 ? static_cast<std::uint64_t>(value) : 0;
    }
    static void VerifyPins(const google::protobuf::RepeatedPtrField<MaintenanceReceipt::SourcePin>& pins) {
        if (pins.empty() || pins.size() > 128) throw std::runtime_error("维护原件 pin 数量无效");
        std::set<std::string> seen;
        for (const auto& pin : pins) if (pin.sha256().size() != 32 || !seen.insert(pin.absolute_path()).second ||
            Sha256::HashToBytes(ReadPinnedBytes(pin.absolute_path(), kControlLimit)) != pin.sha256()) throw std::runtime_error("维护原件 pin 不符");
    }
    static ReadResult Read(const std::string& directory, const std::string& helper, const std::string& helperHash,
                           const std::string& instance, bool loaded, bool prepareBoot, bool baselineWritten) {
        ReadResult out;
        try {
            const auto join = [&](const char* name) { return PathUtf8(PathFromUtf8(directory) / name); };
            PrivateControlCommand(helper, helperHash, "validate-directory", directory);
            if(!loaded&&!baselineWritten&&!prepareBoot){
                const auto file=join("manual-baseline.request.json");
                if(std::filesystem::exists(PathFromUtf8(file))){
                    MaintenanceReceipt::ManualConsumerEvidence intent;
                    if(!google::protobuf::util::JsonStringToMessage(PrivateControlCommand(helper,helperHash,"read-file",file),&intent).ok()||!NoUnknownProto(intent)||intent.operation_id().size()!=16||!intent.manifest_digest().empty()||intent.assignments_size()!=1||intent.accepting_closed()||intent.callbacks_joined()||intent.accepted()!=0||intent.settled()!=0||intent.unresolved_publications()!=0)throw std::runtime_error("manual P0意图无效");
                    auto identity=intent.assignments(0);if(identity.issuer_id().size()!=16||identity.instance_id()!=instance)throw std::runtime_error("manual P0 boot无效");
                    identity.clear_issuer_id();identity.clear_instance_id();if(identity.ByteSizeLong()!=0)throw std::runtime_error("manual P0不能预填native事实");
                    VerifyPins(intent.source_pins());
                    bool ownDirectory=false;for(const auto& pin:intent.source_pins())if(PathFromUtf8(pin.absolute_path()).parent_path()==PathFromUtf8(directory))ownDirectory=true;if(!ownDirectory)throw std::runtime_error("manual P0目录无效");
                    out.baseline=std::move(intent);
                }
            }
            if (!loaded) {
                const auto runtimePath = join("active.runtime.json"), hashPath = join("active.runtime.sha256");
                if (!std::filesystem::exists(PathFromUtf8(runtimePath)) || !std::filesystem::exists(PathFromUtf8(hashPath))) return out;
                const auto raw = PrivateControlCommand(helper, helperHash, "read-file", runtimePath);
                auto hash = PrivateControlCommand(helper, helperHash, "read-file", hashPath);
                if (hash.size() == 65 && hash.back() == '\n') hash.pop_back();
                if (hash.size() != 64 || Sha256::HashToHex(raw) != hash ||
                    !google::protobuf::util::JsonStringToMessage(raw, &out.request).ok() || !NoUnknownProto(out.request) ||
                    !out.plan.ParseFromString(out.request.plan_proto()) || !NoUnknownProto(out.plan)) throw std::runtime_error("维护 runtime 无效");
                const auto& request = out.request; const auto& plan = out.plan;
                if ((request.run_phase() == sql_writer_drain_run::SQL_RELEASED) != prepareBoot || request.run_id().size() != 16 ||
                    request.run_id() != request.evidence().operation_id() || plan.run_id() != request.run_id() ||
                    request.evidence().manifest_digest() != Sha256::HashToBytes(request.plan_proto()) ||
                    !request.evidence().has_publication() || plan.root_issuers().empty() || plan.queue_mutators().empty() ||
                    (plan.kafka_partitions().empty() && plan.manual_assignments().empty()) || plan.redis_targets().empty() || plan.launchers().empty()) throw std::runtime_error("维护 Manifest 绑定无效");
                VerifyPins(plan.source_pins());
                const auto& roles = request.evidence().publication().logical_roles();
                if (roles.empty() || roles.size() > 2) throw std::runtime_error("维护实际角色无效");
                for (const auto& role : roles) {
                    if (role.issuer_id().size() != 16 || role.instance_id() != instance || !role.physical_writer_key().empty() ||
                        role.permanent_revival_fence_id().size() != 16 || role.accepting_closed_ms() != 0 || role.accepted_settled_ms() != 0 ||
                        !role.accepted_frontier_digest().empty() || role.unresolved_publications() != 0) throw std::runtime_error("维护 boot 身份无效");
                    const auto* expected = role.logical_role() == "SOURCE" ? &plan.root_issuers() : role.logical_role() == "RELAY" ? &plan.queue_mutators() : nullptr;
                    if (!expected) throw std::runtime_error("维护角色无效");
                    int found = 0; for (const auto& seed : *expected) {
                        if(!prepareBoot&&google::protobuf::util::MessageDifferencer::Equals(seed,role))++found;
                        if(prepareBoot&&seed.issuer_id()==role.issuer_id()&&seed.logical_role()==role.logical_role()&&seed.physical_writer_key().empty()&&seed.instance_id()!=role.instance_id())++found;
                    }
                    if (found != 1) throw std::runtime_error("维护 P0 角色不匹配");
                    VerifyPins(role.source_evidence());
                    auto& selected = role.logical_role() == "SOURCE" ? out.source : out.relay;
                    if (selected) throw std::runtime_error("维护角色重复"); selected = role;
                }
                if (!out.source) throw std::runtime_error("C++ 节点缺少实际 SOURCE 身份");
                // 不允许旧 boot 借重启恢复；独占 durable marker 与 Go/Java 使用同一平台 owner。
                PrivateControlCommand(helper, helperHash, "create-file", join(("boot-" + ControlHex(instance) + ".consumed").c_str()), request.evidence().manifest_digest());
                out.active = true;
            }
            const auto requested = [&](const char* name, const char* value) {
                const auto file = join(name); if (!std::filesystem::exists(PathFromUtf8(file))) return false;
                const auto raw = PrivateControlCommand(helper, helperHash, "read-file", file);
                if (raw != std::string(value) + "\n") throw std::runtime_error("维护关闭请求无效"); return true;
            };
            if(prepareBoot){
                const auto file=join("open-source.request");if(std::filesystem::exists(PathFromUtf8(file))){MaintenanceReceipt::SourceOpenAck intent;
                    if(!google::protobuf::util::JsonStringToMessage(PrivateControlCommand(helper,helperHash,"read-file",file),&intent).ok()||!NoUnknownProto(intent))throw std::runtime_error("SOURCE开放意图无效");VerifyPins(intent.source_pins());out.open=std::move(intent);}
            }
            out.sourceRequested = requested("close-source.request", "CLOSE_SOURCE");
            out.relayRequested = requested("close-relay.request", "CLOSE_RELAY");
        } catch (...) { out.ok = false; }
        return out;
    }
    void PollFiles() {
        if (!reading.valid() && !held && !(runtime.sourceRequested && runtime.relayRequested)) {
            reading = std::async(std::launch::async, &NodeMaintenanceControl::Read, directory, helper, helperHash, instance, loaded, prepareBoot, baselineWritten);
        }
    }

    static std::string Json(const google::protobuf::Message& value){
        google::protobuf::util::JsonPrintOptions options;options.always_print_fields_with_no_presence=true;
        std::string raw;if(!google::protobuf::util::MessageToJsonString(value,&raw,options).ok())throw std::runtime_error("维护proto编码失败");return raw;
    }
    void WriteIdentity(const NodeInfo& info){
        const auto file=PathUtf8(PathFromUtf8(directory)/"boot-identity.nodeinfo.json"),raw=Json(info);writingOther=5;
        writing=std::async(std::launch::async,[helper=helper,hash=helperHash,file,raw]{try{PrivateControlCommand(helper,hash,"create-file",file,raw);return true;}catch(...){return false;}});
    }
    void WriteManual(MaintenanceReceipt::ManualConsumerEvidence report,bool baseline){
        if(writing.valid())return;
        const auto& pins=baseline?baselineIntent->source_pins():runtime.source->source_evidence();
        report.mutable_source_pins()->CopyFrom(pins);VerifyPins(pins);
        bool ownDirectory=false;for(const auto& pin:pins)if(PathFromUtf8(pin.absolute_path()).parent_path()==PathFromUtf8(directory))ownDirectory=true;if(!ownDirectory)throw std::runtime_error("manual原件不在SOURCE固定私有目录");
        const auto file=PathUtf8(PathFromUtf8(directory)/(baseline?"manual-baseline.report.json":"manual-closure.report.json"));
        const auto raw=Json(report);if(!baseline)manualDigest=Sha256::HashToBytes(raw);
        writingOther=baseline?1:4;
        writing=std::async(std::launch::async,[helper=helper,hash=helperHash,file,raw]{try{PrivateControlCommand(helper,hash,"create-file",file,raw);return true;}catch(...){return false;}});
    }
    void WritePrepared(MaintenanceReceipt::ConsumerReadiness actual){
        consumers=std::move(actual);readiness.Clear();readiness.set_issuer_id(runtime.source->issuer_id());readiness.set_instance_id(instance);readiness.set_logical_role("SOURCE");
        readiness.set_prepared_closed_ms(NowMs());readiness.mutable_source_pins()->CopyFrom(runtime.source->source_evidence());
        auto consumerProof=consumers;for(auto& manual:*consumerProof.mutable_manual_assignments())manual.mutable_source_pins()->CopyFrom(runtime.source->source_evidence());
        const auto consumerPath=PathUtf8(PathFromUtf8(directory)/"consumer-readiness.report.json");const auto consumerRaw=Json(consumerProof);
        readiness.set_consumer_ready_frontier_digest(Sha256::HashToBytes(consumerRaw));
        auto* pin=readiness.add_source_pins();pin->set_absolute_path(consumerPath);pin->set_sha256(readiness.consumer_ready_frontier_digest());
        const auto readyPath=PathUtf8(PathFromUtf8(directory)/"prepared.source.json"),readyRaw=Json(readiness);
        writingOther=2;writing=std::async(std::launch::async,[helper=helper,hash=helperHash,consumerPath,consumerRaw,readyPath,readyRaw,pins=runtime.source->source_evidence()]{try{VerifyPins(pins);PrivateControlCommand(helper,hash,"create-file",consumerPath,consumerRaw);PrivateControlCommand(helper,hash,"create-file",readyPath,readyRaw);return true;}catch(...){return false;}});
    }
    bool ValidOpen()const{
        if(!openIntent||!preparedWritten)return false;const auto& open=*openIntent;
        if(open.source_pins_size()!=readiness.source_pins_size())return false;
        for(const auto& pin:readiness.source_pins()){int found=0;for(const auto& actual:open.source_pins())if(google::protobuf::util::MessageDifferencer::Equals(pin,actual))++found;if(found!=1)return false;}
        return open.issuer_id()==readiness.issuer_id()&&open.instance_id()==instance&&open.physical_writer_key().empty()&&open.logical_role()=="SOURCE"&&open.registration_receipt_id().empty()&&open.registration_receipt_digest().empty()&&open.operation_id()==runtime.request.run_id()&&open.manifest_digest()==runtime.request.evidence().manifest_digest()&&open.open_request_id().size()==16&&open.opened_ms()==0&&open.accepted_frontier_digest().empty();
    }
    void WriteOpen(){
        auto ack=*openIntent;ack.set_opened_ms(NowMs());ack.mutable_source_pins()->CopyFrom(readiness.source_pins());
        ack.set_accepted_frontier_digest(Sha256::HashToBytes(ack.open_request_id()+ack.operation_id()+ack.manifest_digest()+instance+readiness.consumer_ready_frontier_digest()));
        const auto file=PathUtf8(PathFromUtf8(directory)/"source.open.ack.json"),raw=Json(ack);
        writingOther=3;writing=std::async(std::launch::async,[helper=helper,hash=helperHash,file,raw]{try{PrivateControlCommand(helper,hash,"create-file",file,raw);return true;}catch(...){return false;}});
    }

    void Write(bool source, const std::string& frontier) {
        const auto& seed = source ? runtime.source : runtime.relay;
        if (!seed || writing.valid()) return;
        auto closure = *seed;
        closure.set_accepting_closed_ms(source ? sourceClosedMs : relayClosedMs);
        closure.set_accepted_settled_ms(NowMs());
        closure.set_unresolved_publications(0);
        closure.set_accepted_frontier_digest(Sha256::HashToBytes(closure.issuer_id() + instance + closure.logical_role() +
            runtime.request.evidence().manifest_digest() + frontier));
        const auto runtimePath = PathUtf8(PathFromUtf8(directory) / "active.runtime.json");
        const auto outputPath = PathUtf8(PathFromUtf8(directory) / (source ? "source.issuer.json" : "relay.issuer.json"));
        writingOther=0;writingSource = source;
        writing = std::async(std::launch::async, [closure = std::move(closure), runtimePath, outputPath,
            helper = helper, helperHash = helperHash, expected = runtime.request]() mutable {
            try {
                VerifyPins(closure.source_evidence());
                const auto raw = PrivateControlCommand(helper, helperHash, "read-file", runtimePath);
                MaintenanceReceipt again;
                if (!google::protobuf::util::JsonStringToMessage(raw, &again).ok() || !google::protobuf::util::MessageDifferencer::Equals(again, expected)) return false;
                // P0固定source_evidence等集不可追加；新manual报告在NonSqlClosureEvidence独立pin。
                if (closure.accepting_closed_ms() == 0 || closure.accepted_settled_ms() < closure.accepting_closed_ms()) return false;
                const auto json=Json(closure);
                PrivateControlCommand(helper, helperHash, "create-file", outputPath, json); return true;
            } catch (...) { return false; }
        });
    }
};

namespace
{
	std::atomic<Node *> gNodeAtomic{nullptr};
	// gRPC 服务端排空预算(docs/design/grpc-muduo-transport.md §15.3 Q4、§15.10):从 BeginShutdown 起算 2s;
	// 第二段 GOAWAY 在 PING ACK 或满 1s 时发;排空到期后连接继续读写,已提交的应答(含被对端流控卡住的 DATA)写完、
	// 所有流关闭才半关;closeGrace 1s 从排空到期起算,是硬上限,到期强制关闭。总上限约 3s。
	constexpr std::chrono::milliseconds kGrpcDrainTimeout{2000};
	constexpr std::chrono::milliseconds kGrpcGoawayPingTimeout{1000};
	constexpr std::chrono::milliseconds kGrpcCloseGrace{1000};
	constexpr std::chrono::seconds kShutdownDrainBudget{15};
	constexpr std::chrono::seconds kShutdownCompletionWaitTimeout{20};

	// Diagnostic stack-dump signal handler (todo.md #216).
	//
	// Operators send the diagnostic signal to a running node:
	//   Linux:    kill -USR1 <pid>
	//   Windows:  Ctrl+Break in the console (delivered as SIGBREAK)
	//
	// Async-signal-safety strategy (Review R1 fix, 2026-05-17):
	//   The signal handler ONLY sets a sig_atomic_t flag — no mutex,
	//   no allocation, no stdio, no localtime, no LOG_* (muduo's
	//   logger acquires locks). The EventLoop poll function
	//   `DrainPendingDiagnosticWork` runs every 250 ms and consumes
	//   pending flags, executing the actually-heavy work (boost::stacktrace,
	//   error_reporter dump, file I/O) from normal main-thread context
	//   where mutex / fstream / localtime are fine.
	//
	// Cost of the 250 ms poll latency: acceptable for a diagnostic
	// trigger. Ops sending the signal is already a manual action; a
	// quarter-second delay before the dump file appears is invisible.
	//
	// Registration is idempotent and best-effort: failures only log a
	// warning.
	std::atomic<bool> gDiagnosticSignalInstalled{false};

	// sig_atomic_t guarantees writes are atomic from a signal handler.
	// volatile prevents the compiler from optimizing the read in the
	// poll function away even though it doesn't see who writes it.
	volatile std::sig_atomic_t gDumpStackPending{0};
	volatile std::sig_atomic_t gDumpErrorReporterPending{0};
	volatile std::sig_atomic_t gLastDumpSignum{0};

	// Async-signal-safe: only assigns to sig_atomic_t. Everything else
	// (re-install via std::signal, the actual dump work) is moved to
	// the EventLoop drain function.
	void HandleDiagnosticSignal(int signum)
	{
		gLastDumpSignum = static_cast<sig_atomic_t>(signum);
		gDumpStackPending = 1;
		gDumpErrorReporterPending = 1;
		// Re-install the handler; std::signal is technically not in the
		// POSIX async-signal-safe list, but is widely implemented as
		// safe-enough across glibc/musl/Windows MSVCRT and the
		// alternative (sigaction) needs platform-specific wrapping that
		// muduo doesn't have today. Re-install lets subsequent triggers
		// keep working without ourselves transitioning to sigaction.
		std::signal(signum, &HandleDiagnosticSignal);
	}

	// SIGUSR2 dump-only handler (Linux). Same async-signal-safe shape
	// as HandleDiagnosticSignal: only sets a flag.
#ifndef _WIN32
	std::atomic<bool> gErrorReporterSignalInstalled{false};

	void HandleErrorReporterDumpSignal(int signum)
	{
		gDumpErrorReporterPending = 1;
		std::signal(signum, &HandleErrorReporterDumpSignal);
	}

	void InstallErrorReporterDumpHandlerOnce()
	{
		bool expected = false;
		if (!gErrorReporterSignalInstalled.compare_exchange_strong(expected, true))
		{
			return;
		}
		if (std::signal(SIGUSR2, &HandleErrorReporterDumpSignal) == SIG_ERR)
		{
			LOG_WARN << "Failed to install SIGUSR2 error_reporter dump handler; errno=" << errno;
			gErrorReporterSignalInstalled.store(false);
			return;
		}
		LOG_INFO << "Error-reporter dump handler installed: send SIGUSR2 to PID (todo #250 slice D)";
	}
#else
	inline void InstallErrorReporterDumpHandlerOnce() {}
#endif

	// Drain function run from the EventLoop on a 250 ms cadence (set up
	// in Node::Initialize). Consumes the sig_atomic_t flags set by the
	// signal handlers and performs the actually-heavy work from a normal
	// context where mutex / fstream / localtime / LOG_* are all safe.
	//
	// We do NOT bother making the flag-check atomic vs the work — the
	// worst case is "signal arrives twice in 250 ms and we coalesce into
	// one dump", which is benign for a diagnostic dump that already
	// overwrites the previous file on each run.
	void DrainPendingDiagnosticWork()
	{
		// Snapshot + clear so a new signal arriving during the dump
		// schedules another pass on the next tick.
		const bool dumpStack = (gDumpStackPending != 0);
		const bool dumpReporter = (gDumpErrorReporterPending != 0);
		if (!dumpStack && !dumpReporter)
		{
			return;
		}
		const int signum = static_cast<int>(gLastDumpSignum);
		gDumpStackPending = 0;
		gDumpErrorReporterPending = 0;

		if (dumpStack)
		{
			DumpProcessStackTraceOnSignal(signum);
		}

		if (dumpReporter)
		{
			const auto path = error_reporter::DumpSnapshotToDefaultPath();
			if (!path.empty()) {
				LOG_INFO << "Diagnostic dump: error_reporter snapshot -> " << path;
			} else {
				LOG_WARN << "Diagnostic dump: error_reporter snapshot failed";
			}
		}
	}

	void InstallDiagnosticSignalHandlerOnce()
	{
		bool expected = false;
		if (!gDiagnosticSignalInstalled.compare_exchange_strong(expected, true))
		{
			return;
		}

#ifdef _WIN32
		const int diagSignum = SIGBREAK;
		const char *signalName = "SIGBREAK";
#else
		const int diagSignum = SIGUSR1;
		const char *signalName = "SIGUSR1";
#endif
		if (std::signal(diagSignum, &HandleDiagnosticSignal) == SIG_ERR)
		{
			LOG_WARN << "Failed to install diagnostic stack-dump signal handler ("
					 << signalName << "); errno=" << errno;
			gDiagnosticSignalInstalled.store(false);
			return;
		}
		LOG_INFO << "Diagnostic stack-dump handler installed: send " << signalName
				 << " to PID to trigger (todo #216)";
	}

	// Fatal-signal crash dump (todo.md #105).
	//
	// When the process is terminated abnormally (SIGSEGV / SIGABRT / SIGFPE /
	// SIGILL) all in-game players lose their session without going through
	// HandleExitGameNode, so there is no "logout time" recorded anywhere for
	// incident forensics. This handler stamps a wall-clock millisecond
	// timestamp + the surviving stack trace + the process thread count to
	// the log stream right before the process dies, giving on-call a "the
	// node was alive at T=…" anchor without depending on Redis writes
	// completing during a crash (they won't).
	//
	// What we DON'T do here:
	//   - Try to dump live player_id list. The actor registry is in an
	//     unknown state (we got here by SIGSEGV); walking it could deadlock
	//     or re-fault. Per-player time is recovered from the regular save
	//     trail; this handler covers "the node itself".
	//   - Re-raise / chain to a previous handler. We let the OS finish the
	//     job after logging. The default disposition for SIGSEGV is core
	//     dump + terminate, which is what we want for post-mortem.
	//
	// Cost: same async-signal-unsafe caveat as #216 — boost::stacktrace
	// allocates. This is the last-gasp triage path; we accept the risk.
	std::atomic<bool> gFatalSignalInstalled{false};
	std::atomic<bool> gFatalSignalFiring{false};

	void HandleFatalSignal(int signum)
	{
		// Guard against re-entry — if the handler itself faults the OS will
		// re-deliver the signal and we'd otherwise spin. compare_exchange to
		// "already firing" turns the second hit into a fast bail-out.
		bool expected = false;
		if (!gFatalSignalFiring.compare_exchange_strong(expected, true))
		{
			std::signal(signum, SIG_DFL);
			std::raise(signum);
			return;
		}

		const auto nowMs = std::chrono::duration_cast<std::chrono::milliseconds>(
			std::chrono::system_clock::now().time_since_epoch()).count();
		const int threadCount = GetProcessThreadCountForDiagnostic();

		LOG_ERROR << "=== FATAL SIGNAL (todo #105) === signal=" << signum
				  << " process_terminating_at_ms=" << nowMs
				  << " process_thread_count=" << threadCount;
		LOG_ERROR << GetCurrentStackTraceAsString(kMaxEntries);
		LOG_ERROR << "=== End fatal signal dump; restoring SIG_DFL and re-raising ===";

		// Restore default disposition and re-raise so the OS produces the
		// usual core dump / terminate behavior. Don't try to clean up — that
		// is exactly what post-mortem analysis of the core file is for.
		std::signal(signum, SIG_DFL);
		std::raise(signum);
	}

	void InstallFatalSignalHandlerOnce()
	{
		bool expected = false;
		if (!gFatalSignalInstalled.compare_exchange_strong(expected, true))
		{
			return;
		}

		// SIGSEGV / SIGABRT / SIGFPE / SIGILL are POSIX-portable and also
		// honored by Windows MSVCRT runtime, so the same install loop works
		// on both target platforms.
		const int fatalSignals[] = { SIGSEGV, SIGABRT, SIGFPE, SIGILL };
		for (int sig : fatalSignals)
		{
			if (std::signal(sig, &HandleFatalSignal) == SIG_ERR)
			{
				LOG_WARN << "Failed to install fatal-signal crash-dump handler for signal=" << sig
						 << "; errno=" << errno;
			}
		}
		LOG_INFO << "Fatal-signal crash-dump handler installed for SIGSEGV/SIGABRT/SIGFPE/SIGILL (todo #105)";
	}

	const char *GetNonEmptyEnv(const char *name)
	{
		const char *value = std::getenv(name);
		if (value == nullptr || value[0] == '\0')
		{
			return nullptr;
		}
		return value;
	}

	// GRPC_SERVER_MAX_POLLERS 已退役(docs/design/grpc-muduo-transport.md §15.11):第 4 批起 gRPC 服务端跑在节点
	// EventLoop 上,没有 poller 线程,这个变量不再生效。部署里残留的设置只提示一次,免得它看起来仍在起作用。
	void WarnRetiredGrpcServerMaxPollersOnce()
	{
		static std::atomic<bool> warned{false};
		const char *value = GetNonEmptyEnv("GRPC_SERVER_MAX_POLLERS");
		if (value == nullptr || warned.exchange(true))
		{
			return;
		}
		LOG_WARN << "GRPC_SERVER_MAX_POLLERS is retired (the gRPC server has no poller threads since batch 4); ignoring value="
				 << value;
	}

	std::string ResolveNodeIp()
	{
		// 这里返回的是节点的**集群内**身份地址:写进 NodeInfo.endpoint、发布到 etcd,
		// 供服务间互相拨号、端口 CAS、对端校验、FindNodeByPodIP。它不是 bind 地址
		// (bind 见 Node::StartRpcServer),也**不是**客户端可达地址。
		// 客户端可达地址是另一个字段 NodeInfo.client_endpoint(advertised address,语义同
		// Kafka advertised.listeners,D76):由进程按 CLIENT_ENDPOINT_* 在发布前自报
		// (见 Node::InitClientEndpoint),下发方只按 client_endpoint::ClientFacing 选址、不做翻译。
		// 两者互不替代:这里永远不读 CLIENT_ENDPOINT_*,那边也永远不读 POD_IP / NODE_IP(D92)。
		//
		// Prefer K8s Downward API pod IP, then explicit override, then legacy hostname resolve.
		if (const char *podIp = GetNonEmptyEnv("POD_IP"))
		{
			return podIp;
		}

		if (const char *nodeIp = GetNonEmptyEnv("NODE_IP"))
		{
			return nodeIp;
		}

		return localip();
	}

	std::optional<uint16_t> TryResolveNodePortFromEnv()
	{
		const char *rawPort = GetNonEmptyEnv("RPC_PORT");
		if (rawPort == nullptr)
		{
			rawPort = GetNonEmptyEnv("NODE_PORT");
		}

		if (rawPort == nullptr)
		{
			return std::nullopt;
		}

		errno = 0;
		char *endPtr = nullptr;
		const long parsedPort = std::strtol(rawPort, &endPtr, 10);
		if (errno != 0 || endPtr == rawPort || *endPtr != '\0' || parsedPort <= 0 || parsedPort > 65535)
		{
			LOG_WARN << "Ignore invalid env port value. RPC_PORT/NODE_PORT=" << rawPort;
			return std::nullopt;
		}

		return static_cast<uint16_t>(parsedPort);
	}

	std::string FormatKafkaPartitions(const std::vector<int32_t> &partitions)
	{
		if (partitions.empty())
		{
			return "all";
		}

		std::vector<std::string> partitionTokens;
		partitionTokens.reserve(partitions.size());
		for (int32_t partition : partitions)
		{
			partitionTokens.emplace_back(std::to_string(partition));
		}

		return boost::algorithm::join(partitionTokens, ",");
	}

	// 客户端地址配置 / 来源失败时的致命退出(D79 fail-closed)。
	// K8s 下 C++ 节点的业务日志只落 logs/cpp_nodes/*.log,而 muduo 的 LOG_FATAL 直接 abort,
	// 异步日志后端来不及把最后几行刷盘 —— CrashLoopBackOff 时运维会找不到原因。
	// 所以先同步写一行 stderr(kubectl logs --previous 可见),再 LOG_FATAL 以非 0 退出。
	[[noreturn]] void FatalClientEndpoint(const std::string &reason)
	{
		std::fprintf(stderr, "FATAL client endpoint: %s\n", reason.c_str());
		std::fflush(stderr);
		LOG_FATAL << "Client endpoint: " << reason;
		std::abort(); // LOG_FATAL 已 abort;这一行只为让 [[noreturn]] 对编译器成立
	}

	// 启动期配置错误:写法同 FatalClientEndpoint(先同步写一行 stderr,再 LOG_FATAL 非 0 退出)。
	// area 只用于日志,例如 "zone Redis" / "Kafka";reason 不得包含口令等敏感值。
	[[noreturn]] void FatalStartupConfig(const char *area, const std::string &reason)
	{
		std::fprintf(stderr, "FATAL %s config: %s\n", area, reason.c_str());
		std::fflush(stderr);
		LOG_FATAL << area << " config: " << reason;
		std::abort(); // LOG_FATAL 已 abort;这一行只为让 [[noreturn]] 对编译器成立
	}

	// 每个 C++ 节点都有的两条身份序列,排在各模块的采集结果之前。Pod 标签只能说明「哪个 Pod」,
	// zone / node_id 要靠这条关联;node_id 在 etcd 分配前为 0。
	std::string RenderNodeIdentityMetrics(const NodeInfo &info)
	{
		prometheus_text::Writer writer;
		writer.BeginFamily("cpp_node_info",
						   "Identity of this C++ node process (value is always 1); node_id is 0 before etcd allocation.",
						   prometheus_text::MetricType::kGauge);
		const auto nodeType = std::to_string(info.node_type());
		const auto zoneId = std::to_string(info.zone_id());
		const auto nodeId = std::to_string(info.node_id());
		const auto sceneNodeType = std::to_string(info.scene_node_type());
		writer.Sample({{"node_type", nodeType}, {"zone_id", zoneId}, {"node_id", nodeId},
					   {"scene_node_type", sceneNodeType}},
					  1);
		writer.BeginFamily("cpp_node_start_time_seconds",
						   "Unix time this C++ node process started (NodeInfo.launch_time).",
						   prometheus_text::MetricType::kGauge);
		writer.Sample({}, info.launch_time() / 1000000);
		return writer.Text();
	}

#ifdef __linux__
	// fd 限值行同时写 stdout 与日志:Linux 上 muduo 日志只进异步文件,stdout 是 kubectl logs 唯一看得到它的地方
	// (写法同 gate_version::PrintStartupLine,不受 LogLevel 过滤);日志文件那份由 sidecar 送 Loki。
	void EmitOpenFileLimitLine(const std::string &line, bool warn)
	{
		std::fputs(line.c_str(), stdout);
		std::fputc('\n', stdout);
		std::fflush(stdout);
		if (warn)
		{
			LOG_WARN << line;
		}
		else
		{
			LOG_INFO << line;
		}
	}

	uint64_t FromRlim(rlim_t value)
	{
		return value == RLIM_INFINITY ? open_file_limit::kUnlimited : static_cast<uint64_t>(value);
	}
#endif

	// fd 上限(RLIMIT_NOFILE)启动门槛,判定规则、余量与取舍见 open_file_limit.h。只在 Linux 上执行,Windows 本地开发不受影响。
	// 软限抬到 min(硬限, 1048576),只抬不降、不动硬限;抬完仍低于「客户端面准入上限 + 余量」时:
	// 缺省(enforce)经 FatalStartupConfig 先写 stderr 再拒启;NODE_OPEN_FILE_LIMIT_MODE=warn 时只打 WARN 继续启动。
	// C++ 节点没有 Prometheus 出口(gate 准入设计 G13),所以豁免与缺口只进 stdout 与日志,不出指标。
	void EnsureOpenFileLimitOrDie(uint32_t nodeType, const BaseDeployConfig &deploy)
	{
#ifdef __linux__
		const char *rawMode = std::getenv(open_file_limit::kModeEnv);
		const open_file_limit::ModeSetting mode = open_file_limit::ParseMode(rawMode);
		if (!mode.recognized)
		{
			EmitOpenFileLimitLine(open_file_limit::FormatInvalidModeLine(rawMode), true);
		}

		const std::string role = NodeUtils::NodeTypeToShortName(nodeType);
		const open_file_limit::ClientCap cap = open_file_limit::ClientConnectionCapFor(nodeType, deploy);
		const uint64_t required = open_file_limit::RequiredOpenFiles(cap.connections);

		struct rlimit current{};
		if (::getrlimit(RLIMIT_NOFILE, &current) != 0)
		{
			// 对合法指针 getrlimit 不会失败;真失败说明运行环境异常,无法核实即拒启。
			// 豁免开关只豁免「限值不够」,不豁免「无法核实」。
			const int err = errno;
			FatalStartupConfig("open file limit",
							   "role=" + role + " getrlimit(RLIMIT_NOFILE) failed errno=" + std::to_string(err));
		}

		const open_file_limit::Outcome outcome = open_file_limit::Evaluate(
			{FromRlim(current.rlim_cur), FromRlim(current.rlim_max)},
			required,
			[&current](uint64_t target) -> int
			{
				// 只改软限,硬限原样带回;target 恒为有限值(≤ max(kSoftLimitCeiling, required))。
				// 内核要求新的 rlim_max ≤ fs.nr_open:硬限是 RLIM_INFINITY、或设硬限之后 fs.nr_open 被调低时,
				// 原样带回会得到 EPERM。这里不降硬限重试,按「抬升失败」如实判定,errno 进启动行。
				struct rlimit raised = current;
				raised.rlim_cur = static_cast<rlim_t>(target);
				if (::setrlimit(RLIMIT_NOFILE, &raised) == 0)
				{
					return 0;
				}
				const int err = errno;
				return err != 0 ? err : EINVAL;
			});

		const bool clean = outcome.verdict == open_file_limit::Verdict::kOk && outcome.raiseErrno == 0;
		EmitOpenFileLimitLine(open_file_limit::FormatStartupLine(role, cap, mode.mode, outcome), !clean);

		switch (open_file_limit::ActionFor(outcome.verdict, mode.mode))
		{
		case open_file_limit::Action::kProceed:
			return;
		case open_file_limit::Action::kProceedExempted:
			EmitOpenFileLimitLine(open_file_limit::FormatExemptionWarning(role, outcome), true);
			return;
		case open_file_limit::Action::kRefuse:
			break;
		}
		FatalStartupConfig("open file limit", open_file_limit::FormatRefusalReason(role, cap, outcome));
#else
		(void)nodeType;
		(void)deploy;
		LOG_DEBUG << "Open file limit check skipped: RLIMIT_NOFILE handling is Linux-only";
#endif
	}

	std::string FormatEndpoint(const EndpointComp &endpoint)
	{
		return endpoint.ip() + ":" + std::to_string(endpoint.port());
	}

	// 指针入参保留原接口，先拒绝空值，再建立不会改变所有权的必需借用。
	muduo::net::EventLoop &RequireEventLoop(muduo::net::EventLoop *loop)
	{
		if (loop == nullptr)
		{
			LOG_FATAL << "Node requires a valid EventLoop pointer.";
			std::abort();
		}
		return *loop;
	}
}

std::unordered_map<std::string, std::unique_ptr<::google::protobuf::Service>> gNodeService;

Node *gNode;

Node::Node(muduo::net::EventLoop *loop, const std::string &logFilePath)
	: eventLoop(RequireEventLoop(loop)), logSystem(logFilePath, kMaxLogFileRollSize, 1)
{
	// Start async log system early so all constructor logs have correct formatting/colors.
	muduo::Logger::setOutput(AsyncOutput);
	logSystem.start();

	// Set timezone before any LOG_xxx call. Without a valid timezone, muduo appends 'Z'
	// to the timestamp (9 bytes instead of 8), shifting the log-level character past
	// kLoginInfoInex and causing LogToConsole to fall into the default (red) color.
	SetupTimeZone();

	LOG_INFO << "Node created, log file: " << logFilePath;
	if (gNode != nullptr && gNode != this)
	{
		LOG_FATAL << "Multiple Node instances detected. existing=" << gNode << ", new=" << this;
	}

	gNode = this;
	InstallLocalNodeInfo(&localNodeInfo_);
	gNodeAtomic.store(this, std::memory_order_release);
}

Node::Node(muduo::net::EventLoop *loop,
		   uint32_t nodeType,
		   CanConnectNodeTypeList connectTo,
		   ::google::protobuf::Service *replyService)
	: Node(loop, "logs/cpp_nodes/" + NodeUtils::NodeTypeToShortName(nodeType))
{
	if (replyService != nullptr)
	{
		replyService_ = std::ref(*replyService);
	}
	MutableNodeInfo().set_node_type(nodeType);
	targetNodeTypeWhitelist = std::move(connectTo);
	Initialize();
	node::observability::RegisterThreadObservability(eventLoop, "logs/" + NodeUtils::NodeTypeToShortName(nodeType));
	RegisterTrafficStatsReporter(eventLoop);
	RegisterRpcChannelStatsReporter(eventLoop);
}

Node::~Node()
{
	Shutdown();

	// Node 通常在 loop.loop() 返回后离开作用域。若该返回不是由正常收尾
	// (FinalizeTeardownInLoop 的两跳 quit)触发,loop 已异常退出:Shutdown() 只在本线程把
	// gRPC 排空(BeginShutdown)与业务 drain 启动起来,而排空、OnGrpcServerDrained 与收尾全部是
	// loop 内的定时器 / 排队任务,已经退出的 loop 不会再推进它们。此时重新驱动同一个 loop,
	// 跑完 loop 内的 gRPC 排空与收尾,直到既有 finalizer 调 quit(),避免捕获 this 的排队任务
	// 落到已销毁对象上。
	if (eventLoop.isInLoopThread())
	{
		eventLoop.assertInLoopThread();
		bool fallbackLogged = false;
		for (;;)
		{
			std::unique_lock<std::mutex> lock(shutdownCompletionMutex_);
			if (shutdownComplete_)
			{
				break;
			}
			lock.unlock();
			if (!fallbackLogged)
			{
				LOG_WARN << "EventLoop exited before node shutdown completed; running fallback drain loop";
				fallbackLogged = true;
			}
			eventLoop.loop();
		}
	}

	// EventLoop 外销毁时,Shutdown() 的公开等待预算只用于向调用方报告卡顿;
	// 析构本身不能在仍有捕获 this 的回调/worker 时继续释放成员。
	{
		std::unique_lock<std::mutex> lock(shutdownCompletionMutex_);
		shutdownCompletionCv_.wait(lock, [this]
								   { return shutdownComplete_; });
	}

	if (gNode == this)
	{
		InstallLocalNodeInfo(nullptr);
		gNodeAtomic.store(nullptr, std::memory_order_release);
		gNode = nullptr;
	}
}

int64_t Node::GetLeaseId() const
{
	return serviceDiscoveryManager.etcdService.GetLeaseId();
}

NodeInfo &Node::MutableNodeInfo()
{
	assert(eventLoop.isInLoopThread());
	return localNodeInfo_;
}

void Node::Initialize()
{
	eventLoop.assertInLoopThread();
	LOG_DEBUG << "Node initializing...";
	build_info::LogStartupBanner();
	InstallDiagnosticSignalHandlerOnce();
	InstallErrorReporterDumpHandlerOnce();
	InstallFatalSignalHandlerOnce();
	// Drain the sig_atomic_t flags set by the diagnostic / error_reporter
	// signal handlers (Review R1 fix). 250 ms cadence is fast enough that
	// ops doesn't notice the latency between sending SIGUSR1/SIGUSR2 and
	// the dump file appearing, slow enough to be free CPU overhead.
	eventLoop.runEvery(0.25, &DrainPendingDiagnosticWork);
	RegisterHandlers();
	LoadConfigs();
	InitLogSystem();
	// fd 上限门槛(容量模型 A0-2、gate 准入 G4 / P0-1)。这里是第一个配置已读、又还没开任何网络 fd 的位置:
	// 此后 InitRpcServer 连 Redis、InitKafka、InitEtcdService 都要 fd,软限必须在它们之前抬好,拒启也要在注册进 etcd、
	// 开始接流量之前。gate / scene / battle 都经过这里,以后新增的 C++ 节点自动覆盖。
	// 缺省 fail-closed:fd 不够时 gate 的唯一 IO 线程会 EMFILE 忙循环、Redis / Kafka / etcd 重连失败、日志滚动时崩溃,
	// 而且是运行到某个时刻才暴露、难以归因;宁可在发布时就暴露。拒启不是无代价的:影响面按工作负载形态不同,
	// 单副本 battle hostPort 会整体停服、scene 缺省 4 副本滚动时先少一台(docs/ops/gate-kernel-tuning-runbook.md §3.1),
	// 所以发布前必须先读各节点池硬限,不能指望「拒启只会让滚动停住」。运维豁免只认 NODE_OPEN_FILE_LIMIT_MODE=warn,
	// 与 *_RUN_MODE 无关(scene 的运行模式同时决定 GM 放行,不能为绕过这条检查去开 GM)。
	EnsureOpenFileLimitOrDie(GetNodeType(), tlsNodeConfigManager.GetBaseDeployConfig());
	// 生成的 gRPC 客户端的 deadline 必须在发出第一个请求(InitEtcdService 里的 etcd Range / LeaseGrant)之前就位;
	// 放在 InitLogSystem 之后,被忽略的配置项与生效值才进日志文件。
	grpc_call_deadline::Apply(tlsNodeConfigManager.GetBaseDeployConfig().grpc_client());
	// /metrics 出口排在任何业务网络之前:启动卡在 etcd / Redis 时也能抓到 cpp_node_info。
	StartMetricsExporter();
	InitRpcServer();
	InitMaintenanceControl();
	LoadAllConfigData();
	InitKafka();
	InitEtcdService();

	LOG_DEBUG << "Node initialization complete.";
}

void Node::StartMetricsExporter()
{
	eventLoop.assertInLoopThread();
	const auto port = ParseMetricsPortEnv(std::getenv(kMetricsPortEnv));
	if (!port)
	{
		FatalStartupConfig("metrics exporter",
						   std::string(kMetricsPortEnv) + " must be a port number 1..65535 (leave it unset to disable)");
	}
	if (*port == 0)
	{
		LOG_INFO << "[Metrics] " << kMetricsPortEnv << " is not set; Prometheus exporter disabled";
		return;
	}
	// 回调捕获 this:出口是本 Node 的成员,在 FinalizeTeardownInLoop 里先于 Node 释放,
	// 属于 AGENTS.md §11.7「与被绑对象同寿命的成员」的例外。只在 loop 线程上读 NodeInfo。
	metricsExporter_ = std::make_unique<MetricsHttpExporter>(&eventLoop, *port, [this]
	{
		return RenderNodeIdentityMetrics(GetNodeInfo()) + GlobalMetricsRegistry().Render();
	});
	metricsExporter_->Start();
}

void Node::InitRpcServer()
{
	NodeInfo &localNodeInfo = MutableNodeInfo();
	const std::string endpointIp = ResolveNodeIp();
	localNodeInfo.mutable_endpoint()->set_ip(endpointIp);

	// 显式给了 RPC_PORT / NODE_PORT 就以它为准:AcquireNodePort() 看到 endpoint 上
	// 已有非零端口时会尊重这个预设值,只做可用性校验、不再扫区间(见 node_allocator.cpp)。
	// 没给才留 0,由 AcquireNodePort() 从角色区间里扫一个出来。
	if (const auto envPort = TryResolveNodePortFromEnv(); envPort)
	{
		localNodeInfo.mutable_endpoint()->set_port(*envPort);
		LOG_INFO << "Node port from environment: " << *envPort;
	}

	LOG_INFO << "Node endpoint resolved. ip=" << endpointIp;

	// 客户端可达地址必须在这里(etcd 端口/node_id 分配与发布之前)同步定下来:
	// 发布出去的第一版 NodeInfo 就要带上它,match / login 看到节点时不能再有"地址待补"的窗口。
	InitClientEndpoint();

	localNodeInfo.set_node_type(GetNodeType());
	localNodeInfo.set_scene_node_type(tlsNodeConfigManager.GetGameConfig().scene_node_type());
	// battle 等纯 gRPC 节点必须以 PROTOCOL_GRPC 注册:C++ 发现方(node_connector.cpp)
	// 按 protocol_type 分派,标成 TCP 会让它们用 RpcCodec 去拨 battle 的 TCP 端口
	// (那里挂的是客户端战斗直连面 BattleClientEdge),而且永远建不出 gRPC stub;
	// Go match 只认 grpc_endpoint,不受 protocol_type 影响。其余 C++ 节点维持 TCP 注册不变。
	localNodeInfo.set_protocol_type(
		NodeUtils::IsGrpcOnlyNodeType(GetNodeType()) ? PROTOCOL_GRPC : PROTOCOL_TCP);
	localNodeInfo.set_launch_time(TimeSystem::NowMicrosecondsUTC());
	localNodeInfo.set_zone_id(tlsNodeConfigManager.GetGameConfig().zone_id());

	localNodeInfo.set_node_uuid(boost::uuids::to_string(uuidGenerator()));

	const auto &zoneRedisConfig = tlsNodeConfigManager.GetGameConfig().zone_redis();
	// 不实现 SELECT:跨运行时契约把库号钉在 0(Go / Java / C++ 读写同一组键,见 k8s_deploy.ps1 Go Redis 段
	// 「DB 号必须全仓一致」的注释)。非 0 说明配置写错了,带着它启动会把数据静默写进 0 号库,所以当场失败。
	if (zoneRedisConfig.db() != 0)
	{
		FatalStartupConfig("zone Redis", "zoneredis.db=" + std::to_string(zoneRedisConfig.db()) +
											  " is not supported; every runtime shares Redis db 0");
	}
	const auto &redisHost = zoneRedisConfig.host();
	const auto redisPort = static_cast<uint16_t>(zoneRedisConfig.port());
	muduo::net::InetAddress zoneRedisAddress(redisPort);

	// If the host looks like an IP address, use it directly without DNS resolve
	bool isIpLiteral = !redisHost.empty() && (std::isdigit(static_cast<unsigned char>(redisHost[0])) || redisHost.find(':') != std::string::npos);
	if (isIpLiteral)
	{
		zoneRedisAddress = muduo::net::InetAddress(redisHost, redisPort);
	}
	else if (!muduo::net::InetAddress::resolve(redisHost, &zoneRedisAddress))
	{
		LOG_WARN << "DNS resolve failed for Redis host '" << redisHost << "', treating as literal IP";
		zoneRedisAddress = muduo::net::InetAddress(redisHost, redisPort);
	}
	LOG_INFO << "Zone Redis address: " << zoneRedisAddress.toIpPort();
	// 只打开关,不打口令。口令来自 game_config.yaml 的 zoneredis.password,可被环境变量 ZONE_REDIS_PASSWORD 覆盖。
	LOG_INFO << "Zone Redis auth: " << (zoneRedisConfig.password().empty() ? "off" : "on");
	RedisConnectOptions redisOptions;
	redisOptions.password = zoneRedisConfig.password();
	tlsRedis.Connect(&eventLoop, zoneRedisAddress, std::move(redisOptions));

	LOG_DEBUG << "Node info: " << localNodeInfo.DebugString();
}

void Node::InitClientEndpoint()
{
	// 四步(D79):解析 env → (agones)取预构造钩子注册的来源 → Resolve → 写 NodeInfo。
	// 任何一步失败都 fail-closed 致命退出,由 K8s / Agones 重建;绝不回落成"不自报",
	// 否则 match 已经把房间派给本节点、玩家已经凑齐,到下发地址时才发现不可达就太晚了。
	client_endpoint::Settings settings;
	std::string error;
	const client_endpoint::EnvLookup env = [](const char *name) -> const char *
	{ return std::getenv(name); };
	if (!client_endpoint::ParseSettings(env, settings, error))
	{
		FatalClientEndpoint("invalid settings: " + error);
	}

	// none(含未设置)下 ParseSettings 按契约忽略 HOST / PORT 并放行:podip 形态的 Fleet 模板可能仍带 HOST(WP9)。
	// 但"给了地址却漏写 SOURCE=static"也落在这条路上 —— 节点不自报、消费方回落 PodIP、集群外客户端连不上,
	// 且全程没有任何报错。这是 fail-open 路径(AGENTS §11.3),必须留痕:放行语义不变,
	// 但同步写一行 stderr(kubectl logs 可见)再 LOG_WARN,带上两个原值,排障时一眼能看出配置被丢弃了。
	if (settings.source == client_endpoint::SourceKind::kNone)
	{
		const char *ignoredHost = GetNonEmptyEnv(client_endpoint::kEnvHost);
		const char *ignoredPort = GetNonEmptyEnv(client_endpoint::kEnvPort);
		if (ignoredHost != nullptr || ignoredPort != nullptr)
		{
			const std::string message = std::string("CLIENT_ENDPOINT_HOST/PORT set but CLIENT_ENDPOINT_SOURCE=none; ignored. ") +
										client_endpoint::kEnvHost + "='" + (ignoredHost != nullptr ? ignoredHost : "") + "' " +
										client_endpoint::kEnvPort + "='" + (ignoredPort != nullptr ? ignoredPort : "") + "'";
			std::fprintf(stderr, "WARN client endpoint: %s\n", message.c_str());
			std::fflush(stderr);
			LOG_WARN << "Client endpoint: " << message;
		}
	}

	std::unique_ptr<client_endpoint::ExternalSource> external;
	if (settings.source == client_endpoint::SourceKind::kAgones)
	{
		const auto factory = client_endpoint::GetExternalSourceFactory();
		if (factory == nullptr)
		{
			FatalClientEndpoint(std::string(client_endpoint::kEnvSource) +
								"=agones but this node registered no ClientEndpointSourceFactory hook");
		}
		external = factory();
		if (external == nullptr)
		{
			FatalClientEndpoint(std::string(client_endpoint::kEnvSource) +
								"=agones but the source factory returned null (Agones disabled or unsupported in this build)");
		}
	}

	// agones 来源在这里有界阻塞(来源内部重试,最坏约 60s);此时 EventLoop 尚未运行,阻塞只推迟启动。
	std::optional<EndpointComp> clientEndpoint;
	if (!client_endpoint::Resolve(settings, external.get(), clientEndpoint, error))
	{
		FatalClientEndpoint("resolve failed: source=" + std::string(client_endpoint::SourceKindName(settings.source)) +
							" error=" + error);
	}

	NodeInfo &localNodeInfo = MutableNodeInfo();
	if (clientEndpoint)
	{
		*localNodeInfo.mutable_client_endpoint() = *clientEndpoint;
	}
	clientEndpointRequired_ = settings.required;

	// endpoint 的端口可能还是 0(扫描区间的节点要到 etcd 分配后才定),照实打印。
	LOG_INFO << "Client endpoint resolved. source=" << std::string(client_endpoint::SourceKindName(settings.source))
			 << " endpoint=" << FormatEndpoint(localNodeInfo.endpoint())
			 << " client_endpoint=" << (clientEndpoint ? FormatEndpoint(*clientEndpoint) : std::string("(unset, clients use endpoint)"))
			 << " required=" << (settings.required ? 1 : 0);
}

void Node::InitKafka()
{
	// 三类节点都离不开 Kafka:gate / scene 收控制面命令,battle 发票据公告 / 确认 /
	// 结算 / 对局结果。带着发不出消息的生产者启动,等于结算静默丢失,所以 Brokers 为空当场失败。
	// broker 填了但暂时连不上不在这里拒启:librdkafka 后台重连,发送失败有 ERROR。
	if (!kafkaManager.Init(tlsNodeConfigManager.GetBaseDeployConfig().kafka()))
	{
		FatalStartupConfig("Kafka", "Kafka.Brokers is empty in etc/base_deploy_config.yaml; refusing to start");
	}
}

void Node::StartKafkaPolling()
{
	// Drive Kafka consumption from a dedicated thread, dispatching decoded
	// messages back into the muduo EventLoop. The legacy 100ms timer model
	// shared the loop with RPC / client TCP / scene-response forwarding;
	// during the 45k stress run (stress-1zone-45k-2026-05-30) the loop
	// stalled long enough for librdkafka to leave the group, leaving
	// gate-group-1 with no active members and Kafka command lag at 17k.
	//
	// queueInLoop preserves the single-threaded callback invariant the rest
	// of the codebase relies on — the consumer thread never touches ECS or
	// node state directly, only enqueues the existing callback.
	kafkaManager.StartBackgroundPolling(&eventLoop);
}

bool Node::RegisterKafkaMessageHandler(const std::vector<std::string> &topics,
									   const std::string &groupId,
									   KafkaMessageHandler handler,
									   const std::vector<int32_t> &partitions,
									   const KafkaPartitionAssignPolicy &assignPolicy)
{
	if (topics.empty())
	{
		LOG_ERROR << "RegisterKafkaMessageHandler failed: topics is empty.";
		return false;
	}

	if (!handler)
	{
		LOG_ERROR << "RegisterKafkaMessageHandler failed: handler is null.";
		return false;
	}

	auto &kafkaConfig = tlsNodeConfigManager.GetBaseDeployConfig().kafka();
    MaintenanceReceipt::ManualConsumerAssignment manual;
    if(!partitions.empty()&&maintenanceControl_){
        if(topics.size()!=1||partitions.size()!=1||assignPolicy.expectedTopicPartitions<=0||!assignPolicy.ownOffsets||!assignPolicy.startAtEnd)return false;
        manual.set_consumer_id(groupId);manual.set_instance_id(maintenanceControl_->instance);manual.set_target_instance_id(maintenanceControl_->instance);
        manual.set_target_node_id(GetNodeId());manual.set_partition_count(assignPolicy.expectedTopicPartitions);
    }
	if (!GetKafkaManager().Subscribe(kafkaConfig, topics, groupId, partitions, std::move(handler), assignPolicy, manual))
	{
		LOG_ERROR << "Kafka subscribe failed. group_id=" << groupId;
		return false;
	}

	LOG_INFO << "Kafka subscribe succeeded. group_id=" << groupId
			 << ", topics=" << boost::algorithm::join(topics, ",")
			 << ", partitions=" << FormatKafkaPartitions(partitions);

	StartKafkaPolling();
	kafkaPollingStarted = true;

	return true;
}

void Node::InitEtcdService()
{
	serviceDiscoveryManager.Init();
}

void Node::RegisterGrpcService(muduo_grpc::Service &service)
{
	if (grpcServer_)
	{
		// 方法表在 Server::Start 之前定型;启动之后再注册不会生效,不能静默接受。
		LOG_ERROR << "RegisterGrpcService called after the gRPC server started; ignored.";
		return;
	}
	grpcServices_.push_back(std::ref(service));
}

void Node::StartGrpcServer()
{
	eventLoop.assertInLoopThread();
	// etcd 端口分配可能晚于关机开始。此时不再启动服务端,也不发布发现键:ShutdownInLoop 调 ShutdownGrpcServer 时还没有
	// 服务端,已把 gRPC 排空记为完成;这里再起服务端,它会在排空之后才开始监听,只能被 Finalize 的防御分支不发 GOAWAY
	// 地销毁,而发布发现键会让 Go 侧在收尾期间拨号进来(docs/design/grpc-muduo-transport.md §15.16 第 4 条)。
	if (shutdownStarted.load(std::memory_order_acquire) || (maintenanceControl_ && maintenanceControl_->sourceClosed))
	{
		LOG_WARN << "Shutdown already started; not starting the gRPC server and not publishing discovery.";
		return;
	}
	if (grpcServices_.empty())
	{
		// No gRPC server to start. The discovery publish was deferred from
		// OnTxnSucceeded(allocKey), so we must publish now or this node will
		// never become discoverable to peers.
		serviceDiscoveryManager.etcdService.PublishDiscoveryAfterGrpcReady();
		return;
	}

	const auto &grpcEp = GetNodeInfo().grpc_endpoint();
	if (grpcEp.port() == 0)
	{
		LOG_ERROR << "gRPC server port not allocated, skipping gRPC server start.";
		// Same as above: publish now so the node still becomes discoverable
		// even when gRPC startup is skipped.
		serviceDiscoveryManager.etcdService.PublishDiscoveryAfterGrpcReady();
		return;
	}

	// 以前这里按 GRPC_SERVER_MAX_POLLERS 配官方 sync server 的 poller 线程数;第 4 批起服务端
	// (muduo_grpc::Server,docs/design/grpc-muduo-transport.md §15)整个跑在本节点 EventLoop 上,
	// 接入、HTTP/2、派发、定时器、关机都不创建线程。变量已退役,设了只提示一次。
	WarnRetiredGrpcServerMaxPollersOnce();

	const std::string serverAddress = grpcEp.ip() + ":" + std::to_string(grpcEp.port());
	if (grpcEp.port() > std::numeric_limits<uint16_t>::max())
	{
		LOG_FATAL << "gRPC server port out of range: " << serverAddress;
	}
	// 只 bind 注册的 IPv4(grpc_endpoint.ip),不读 BIND_IP、不开 SO_REUSEPORT(§15.3 Q8):暴露面与旧的官方
	// 服务端一致。muduo InetAddress 遇到非法 IP 会静默落成 0.0.0.0(监听全部网卡),所以必须先校验;
	// IPv6 的 POD_IP 在这里启动失败,与客户端侧(§8)同一限制。
	if (!muduo_grpc::IsCanonicalIpv4(grpcEp.ip()))
	{
		LOG_FATAL << "gRPC server bind address must be a canonical IPv4 literal: " << serverAddress;
	}

	const muduo_grpc::ServerOptions serverOptions{};
	auto server = muduo_grpc::Server::Create(eventLoop, grpcEp.ip(), static_cast<uint16_t>(grpcEp.port()),
											 serverOptions);
	if (!server)
	{
		LOG_FATAL << "Failed to create gRPC server on " << serverAddress;
		return;
	}
	for (auto &service : grpcServices_)
	{
		// 路径非法或与已注册的方法重复:fail-closed,不带着残缺的方法表上线。
		if (!server->RegisterService(service.get()))
		{
			LOG_FATAL << "Failed to register gRPC service methods on " << serverAddress;
			return;
		}
	}
	// Start 建 Acceptor 并 listen;bind 失败由 muduo 按 LOG_SYSFATAL 退出(与旧服务端 BuildAndStart 失败同样 fail-fast)。
	// §15.11 的启动日志("gRPC server (muduo_grpc) listening on ... threads=0")由 Server::Start 自己打,这里不重复。
	grpcServer_ = std::move(server);
	grpcServer_->Start();

	// gRPC port is now bound and accepting connections (Start() returns after
	// the listener is bound and listening). Safe to advertise this node in etcd
	// so peers can discover and dial us. Without this deferral, scene_manager
	// would see the node via etcd watch and dial gRPC before the port is open,
	// triggering "connection refused" + dead-node markings during cold start.
	serviceDiscoveryManager.etcdService.PublishDiscoveryAfterGrpcReady();
}

void Node::ShutdownGrpcServer()
{
	eventLoop.assertInLoopThread();
	if (!grpcServer_)
	{
		shutdownGrpcDrainComplete_ = true;
		MaybeFinalizeShutdownInLoop();
		return;
	}

	// 排空全部在 loop 内完成(docs/design/grpc-muduo-transport.md §15.10),不再起关机线程:Handle* 都是同步的,
	// 没有谁阻塞着等 loop。BeginShutdown 依次:置排空标志(此后 ServerCall::ServerDraining() 为真)→ 把 Acceptor
	// 移进排队任务销毁(停听)→ 每条连接发第一段 GOAWAY + PING(没收到过字节的直接关)→ 上弦排空定时器。
	// 已收完的调用照常派发;PING ACK 或满 1s 发最终 GOAWAY;2s 到期未开始的(含请求体还在上传的)回 REFUSED_STREAM、
	// 已开始的回 UNAVAILABLE,之后连接继续读写到已提交的帧全部写出、所有流关闭才半关,从到期起最多再宽限 1s 强制关闭。
	// 连接表为空时 onDrained 经 queueInLoop 投递,按 FIFO 排在最后一个 connectDestroyed 之后。
	// onDrained 恰好一次、永不在本调用栈内执行,所以 ShutdownInLoop 随后的 MaybeFinalize 必然看到「未排空」直接返回。
	// 回调捕获 this:与本类其它排队任务同理,shutdownComplete_ 只在收尾的两跳 quit 之后置位,~Node 会等它
	// (loop 异常退出时重新驱动 loop),所以投递到时 Node 一定还在。
	// "gRPC server draining: ..." 由 BeginShutdown 自己打;这里只记起点,给 "gRPC server drained. drain_ms=" 用。
	grpcDrainStart_ = std::chrono::steady_clock::now();

	muduo_grpc::ServerShutdownOptions shutdownOptions;
	shutdownOptions.drainTimeout = kGrpcDrainTimeout;
	shutdownOptions.goawayPingTimeout = kGrpcGoawayPingTimeout;
	shutdownOptions.closeGrace = kGrpcCloseGrace;
	grpcServer_->BeginShutdown(shutdownOptions, [this]
							   { OnGrpcServerDrained(); });
}

void Node::OnGrpcServerDrained()
{
	eventLoop.assertInLoopThread();
	if (shutdownGrpcDrainComplete_ || !grpcServer_)
	{
		// 已记为排空,或收尾已防御销毁了 Server(FinalizeShutdownInLoop 开头):迟到 / 重复的通知,忽略。
		return;
	}

	const auto drainMs = std::chrono::duration_cast<std::chrono::milliseconds>(
							 std::chrono::steady_clock::now() - grpcDrainStart_)
							 .count();
	const auto stats = grpcServer_->Stats();
	if (stats.deadlineAfterStart != 0 || stats.abortedAfterStart != 0 || stats.unavailableOnShutdown != 0 ||
		stats.lateFinish != 0 || stats.dropped != 0 || stats.droppedOffLoop != 0)
	{
		shutdownFailure_.store(true, std::memory_order_release);
		LOG_ERROR << "gRPC 生命周期 HOLD：已接收业务存在超时、丢弃或未知完成；连接清空不是成功闭合";
	}
	LOG_INFO << "gRPC server drained. drain_ms=" << drainMs << " "
			 << muduo_grpc::FormatServerStats(stats);
	// 这里是排队任务:不在 Server 自己的栈上,也不在 I/O 事件处理期间,可以同步销毁(server.h 的析构前置条件)。
	// 此时连接表已空、Acceptor 早已销毁;~Server 万一再排进 forceClose / connectDestroyed,由收尾末尾的两跳 quit 覆盖。
	grpcServer_.reset();

	shutdownGrpcDrainComplete_ = true;
	MaybeFinalizeShutdownInLoop();
}

void Node::OnNodeIdConflictShutdown(NodeIdConflictReason reason)
{
	// 四个触发点(keepalive TTL=0 / 本地租约 deadline / 重注册 CAS 失败 / Watch 发现
	// 身份被抢)都可能重复触发,而且 keepalive 定时器还在跑,必须幂等。
	if (conflictShutdownStarted_)
	{
		return;
	}
	conflictShutdownStarted_ = true;

	LOG_ERROR << "Node identity conflict detected (reason=" << static_cast<int>(reason)
			  << "), node_id=" << GetNodeId()
			  << ". Draining before exit.";

	// 1) 发号不用管:永久 guid(item / tx / snapshot)走号段(GuidSegmentRegistry),号段在
	//    AllocateIdSegment 提交时就已被数据库判定为用掉,与路由 node_id 无关 —— 失去路由身份
	//    不会让手里的号变成别人的号,收尾期间照常可以发完(§7.5 第 1 / 6 条)。

	// 2) 停掉所有会再次触发注册 / 冲突判定的重试,避免收尾期间又跑一遍分配流程 ——
	//    这台节点已经不是 node_id 的合法持有者,重抢端口 / 重占 node_id 只会干扰接手的进程。
	//    注意重试定时器住在 EtcdService 里,不是 Node 里。
	serviceHealthMonitorTimer.Cancel();
	serviceDiscoveryManager.etcdService.StopRegistrationRetries();

	// 3) 业务收尾:scene 节点在这里存盘并把玩家改派到别的 scene node。
	if (onConflictShutdownFn_)
	{
		LOG_INFO << "Running conflict-shutdown hook...";
		onConflictShutdownFn_(*this, reason);
	}

	// 4) 有界等待收尾落地后再退出。
	//    旧实现是 hook 返回后立刻 LOG_FATAL(muduo 的 FATAL 会 abort),而存盘是异步的
	//    (提交请求还在发往 go/db 的路上),abort 等于把这一
	//    批玩家数据直接丢掉。
	StartConflictDrainWatchdog();
}

void Node::StartConflictDrainWatchdog()
{
	// 收尾预算。推导:收尾要等的是"每个在线玩家一次 Redis 存盘往返 + 一次
	// SceneManager EnterScene 改派";正常情况下几百玩家在百毫秒级完成,15s 是给
	// Redis / SceneManager 抖动留的保守上限。到期即退出,不无限等下去 ——
	// 这是有界兜底,不是"等一会儿就当成功"(到期会明确打 ERROR 并报告未落地的数量)。
	// TODO: 待压测实测复核该预算。
	constexpr double kDrainPollIntervalSec = 0.1;
	constexpr auto kDrainBudget = std::chrono::seconds(15);

	conflictDrainDeadline_ = std::chrono::steady_clock::now() + kDrainBudget;

	conflictDrainTimer.RunEvery(kDrainPollIntervalSec, [this]
								{
		const bool drained = !conflictDrainCompleteFn_ || conflictDrainCompleteFn_(*this);
		const bool expired = std::chrono::steady_clock::now() >= conflictDrainDeadline_;
		if (!drained && !expired)
		{
			return;
		}

		conflictDrainTimer.Cancel();
		if (drained)
		{
			LOG_INFO << "Conflict drain complete, shutting down. node_id=" << GetNodeId();
		}
		else
		{
			LOG_ERROR << "Conflict drain budget exceeded; shutting down with work still in flight. node_id="
					  << GetNodeId();
		}

		// 走正常关闭路径:它会跑 before-shutdown hook、关 gRPC、flush Kafka producer、
		// 释放 etcd 租约,最后 quit 事件循环 —— 这些正是 abort 会跳过的东西。
		Shutdown(); });
}

void Node::StartRpcServer()
{
	eventLoop.assertInLoopThread();
    if (shutdownStarted.load(std::memory_order_acquire) || (maintenanceControl_ && maintenanceControl_->sourceClosed)) return;
	if (rpcServer)
	{
		LOG_TRACE << "RPC server already started, skipping.";
		return;
	}

	const NodeInfo &localNodeInfo = GetNodeInfo();

	// bind 地址与注册地址是两回事。endpoint().ip() 是**注册**地址(见 ResolveNodeIp),
	// 拿它去 bind 就等于要求"我只收发到这一个 IP 上的包" —— 在 K8s 里这既没必要
	// (pod 网络命名空间本来就只有它自己),又挡掉了经 hostPort / NodePort 之类
	// 转发进来的流量。默认 bind 0.0.0.0 收全部网卡;裸机多网卡确实要钉一张时,
	// 用 BIND_IP 显式指定。
	const auto listenPort = static_cast<uint16_t>(localNodeInfo.endpoint().port());
	const char *bindIp = GetNonEmptyEnv("BIND_IP");
	const muduo::net::InetAddress rpcListenAddress =
		bindIp != nullptr ? muduo::net::InetAddress(bindIp, listenPort)
						  : muduo::net::InetAddress(listenPort);
	LOG_INFO << "RPC server listen addr=" << rpcListenAddress.toIpPort()
			 << " registered endpoint=" << localNodeInfo.endpoint().ip() << ":" << listenPort;

	rpcServer = std::make_unique<RpcServerPtr::element_type>(&eventLoop, rpcListenAddress);
	if(!maintenancePrepareBoot_)rpcServer->start();
	auto *nodeReplyService = GetNodeReplyService();
	if (nodeReplyService != nullptr)
	{
		rpcServer->registerService(nodeReplyService);
	}
	else
	{
		LOG_WARN << "Node reply service is null, skip registerService for node_type=" << GetNodeInfo().node_type();
	}

	for (auto &[serviceName, service] : gNodeService)
	{
		if (service == nullptr)
		{
			LOG_WARN << "Skip null node service registration for key=" << serviceName;
			continue;
		}

		rpcServer->registerService(service.get());
	}

	NodeConnector::ConnectAllNodes();

	if (!RegisterKafkaHandlers())
	{
		LOG_FATAL << "RegisterKafkaHandlers failed for node_type=" << GetNodeInfo().node_type();
	}

	if(!maintenancePrepareBoot_)StartNodeRegistrationHealthMonitor();

	if(!maintenancePrepareBoot_)StartGrpcServer();


	auto nodeTypeName = boost::to_upper_copy(eNodeType_Name(GetNodeInfo().node_type()));
	const auto &ep = GetNodeInfo().endpoint();
	const auto &deployConfig = tlsNodeConfigManager.GetBaseDeployConfig();
	const auto &gameConfig = tlsNodeConfigManager.GetGameConfig();

	// Build connects-to list
	std::string connectsTo;
	for (auto nodeType : targetNodeTypeWhitelist)
	{
		if (!connectsTo.empty())
			connectsTo += ", ";
		connectsTo += eNodeType_Name(nodeType);
	}
	if (connectsTo.empty())
		connectsTo = "(none)";

	// Build Kafka brokers string
	std::string kafkaBrokers = boost::algorithm::join(
		std::vector<std::string>(deployConfig.kafka().brokers().begin(), deployConfig.kafka().brokers().end()), ", ");
	if (kafkaBrokers.empty())
		kafkaBrokers = "(not configured)";

	// Build etcd hosts string
	std::string etcdHosts = boost::algorithm::join(
		std::vector<std::string>(deployConfig.etcd_hosts().begin(), deployConfig.etcd_hosts().end()), ", ");
	if (etcdHosts.empty())
		etcdHosts = "(not configured)";

	// Build gRPC listen string
	std::string grpcListen = "(disabled)";
	if (GetNodeInfo().grpc_endpoint().port() > 0)
	{
		grpcListen = GetNodeInfo().grpc_endpoint().ip() + ":" + std::to_string(GetNodeInfo().grpc_endpoint().port());
	}

	// Print startup banner to both log file and stdout/console so it is always visible.
	const std::string banner =
		"\n\n"
		"=============================================================\n"
		"  " +
		nodeTypeName + " NODE STARTED SUCCESSFULLY\n"
					   "=============================================================\n"
					   "  Listen:      " +
		ep.ip() + ":" + std::to_string(ep.port()) + "\n"
													"  gRPC:        " +
		grpcListen + "\n"
					 "  node_id:     " +
		std::to_string(GetNodeId()) + "\n"
									  "  node_uuid:   " +
		GetNodeInfo().node_uuid() + "\n"
									"  zone_id:     " +
		std::to_string(gameConfig.zone_id()) + "\n"
											   "  etcd:        " +
		etcdHosts + "\n"
					"  kafka:       " +
		kafkaBrokers + "\n"
					   "  redis:       " +
		gameConfig.zone_redis().host() + ":" + std::to_string(gameConfig.zone_redis().port()) + "\n"
																								"  connects_to: " +
		connectsTo + "\n"
					 "  log_level:   " +
		std::to_string(deployConfig.log_level()) + "\n"
												   "=============================================================\n";
	LOG_INFO << banner;

    maintenancePreparedStart_=true;
	if (afterStartFn_&&!maintenancePrepareBoot_)afterStartFn_(*this);
}


void Node::StartPreparedSourceInLoop(){
    eventLoop.assertInLoopThread();
    if(!maintenancePrepareBoot_||!maintenancePreparedStart_||!rpcServer||!afterStartFn_||shutdownStarted.load())throw std::runtime_error("SOURCE未实际准备");
    // consumer owner lease中的提交只切准入闸，不在可失败的校验回调中发SQL/Kafka/Redis。
    // 此函数在提交成功后、同一loop调用栈尚未让出时完成已准备listener和域启动；失败保UNKNOWN，不重放open。
    if(!maintenanceSourceOpened_)throw std::runtime_error("SOURCE尚未原子提交");
    afterStartFn_(*this);StartGrpcServer();rpcServer->start();StartNodeRegistrationHealthMonitor();
}

void Node::InitMaintenanceControl()
{
    const char* directory = GetNonEmptyEnv("MMORPG_MAINTENANCE_CONTROL_DIR");
    const char* prepare=GetNonEmptyEnv("MMORPG_MAINTENANCE_PREPARE_ONLY");
    if(prepare&&std::string(prepare)!="1")FatalStartupConfig("maintenance","prepare-only只能显式为1");
    if (!directory) {if(prepare)FatalStartupConfig("maintenance","prepare-only缺私有控制目录");return;}
    const char* helper = GetNonEmptyEnv("MMORPG_SQL_PRIVATE_FILE_HELPER");
    const char* helperHash = GetNonEmptyEnv("MMORPG_SQL_PRIVATE_FILE_HELPER_SHA256");
    if (!helper || !helperHash || !PathFromUtf8(directory).is_absolute() ||
        !PathFromUtf8(helper).is_absolute() || !std::regex_match(helperHash, std::regex("[0-9a-fA-F]{64}")) ||
        !RpcChannel::ProcessMaintenance()->EnableAcknowledgedSends()) {
        FatalStartupConfig("maintenance", "维护控制目录/固定文件助手配置无效，或首次发布已经发生");
    }
    auto control = std::make_unique<NodeMaintenanceControl>();
    control->directory = directory; control->helper = helper;
    control->prepareBoot=prepare!=nullptr;maintenancePrepareBoot_=control->prepareBoot;
    if(!kafkaManager.PrepareOnlyBeforeSubscribe(maintenancePrepareBoot_)||(maintenancePrepareBoot_&&!RpcChannel::ProcessMaintenance()->PrepareSources()))FatalStartupConfig("maintenance","prepare-only必须早于首次接收");
    control->helperHash = boost::to_lower_copy(std::string(helperHash));
    try {
        const auto uuid = boost::uuids::string_generator()(GetNodeInfo().node_uuid());
        control->instance.assign(reinterpret_cast<const char*>(uuid.data()), uuid.size());
    } catch (...) { FatalStartupConfig("maintenance", "实际节点 boot UUID 无效"); }
    maintenanceControl_ = std::move(control);
    maintenanceControlTimer_.RunEvery(0.1, [this] { PollMaintenanceControl(); });
}

void Node::CloseMaintenanceSource()
{
    auto& control = *maintenanceControl_;
    if (control.sourceClosed) return;
    control.sourceClosed = true; control.sourceClosedMs = NodeMaintenanceControl::NowMs();
    control.stageDeadline = std::chrono::steady_clock::now() + std::chrono::seconds(60);
    if(!maintenanceInboundRelay_) RpcChannel::ProcessMaintenance()->CloseSources();
    if (!maintenanceSourceClose_ || !shutdownDrainCompleteFn_ || !maintenanceRelayStop_) {
        control.held = true; shutdownFailure_.store(true, std::memory_order_release);
        LOG_ERROR << "维护 HOLD：实际节点未装配 source/relay 生命周期钩子";
    } else if (!sourceBusinessClosed_) {
        sourceBusinessClosed_ = true; maintenanceSourceClose_(*this);
    }
    if(!maintenanceInboundRelay_) ShutdownGrpcServer();
    // Kafka consumers、Redis 和出站 RPC 在 P1 继续推动已接收工作；不能以 Shutdown 代替关闭根。
}

void Node::PollMaintenanceControl()
{
    eventLoop.assertInLoopThread();
    if (!maintenanceControl_ || shutdownStarted.load(std::memory_order_acquire)) return;
    auto& control = *maintenanceControl_;
    const auto hold = [&] {
        if (!control.held) LOG_ERROR << "维护 HOLD：私有原件、实际 owner 结果或生命周期门禁未满足";
        control.held = true; shutdownFailure_.store(true, std::memory_order_release);
        CloseMaintenanceSource();
    };
    if (control.held) return;
    if (control.reading.valid() && control.reading.wait_for(std::chrono::seconds(0)) == std::future_status::ready) {
        auto result = control.reading.get();
        if (!result.ok) { hold(); return; }
        if(result.baseline&&!control.baselineWritten&&!control.baselineIntent){control.baselineIntent=std::move(result.baseline);control.stageDeadline=std::chrono::steady_clock::now()+std::chrono::seconds(60);}
        if(result.open){if(control.openIntent&&!google::protobuf::util::MessageDifferencer::Equals(*control.openIntent,*result.open)){hold();return;}control.openIntent=std::move(result.open);}
        if (result.active) { control.runtime = std::move(result); control.loaded = true; control.stageDeadline=std::chrono::steady_clock::now()+std::chrono::seconds(60); }
        else {
            control.runtime.sourceRequested = control.runtime.sourceRequested || result.sourceRequested;
            control.runtime.relayRequested = control.runtime.relayRequested || result.relayRequested;
        }
    }
    if (control.writing.valid() && control.writing.wait_for(std::chrono::seconds(0)) == std::future_status::ready) {
        if (!control.writing.get()) { hold(); return; }
        if(control.writingOther==1)control.baselineWritten=true;
        else if(control.writingOther==2)control.preparedWritten=true;
        else if(control.writingOther==3)control.opened=true;
        else if(control.writingOther==4)control.manualWritten=true;
        else if(control.writingOther==5)control.identityWritten=true;
        else if (control.writingSource) control.sourceWritten = true; else control.relayWritten = true;
        control.writingOther=0;
    }
    control.PollFiles();
    if(maintenancePreparedStart_&&!control.identityWritten){if(!control.writing.valid())try{control.WriteIdentity(GetNodeInfo());}catch(...){hold();}return;}
    if(control.baselineIntent&&!control.baselineWritten&&!control.loaded&&maintenancePreparedStart_){
        if(!control.manualReading.valid()&&!control.writing.valid())control.manualReading=kafkaManager.ManualEvidence(control.baselineIntent->operation_id(),"",control.baselineIntent->assignments(0).issuer_id(),control.instance);
        if(control.manualReading.valid()&&control.manualReading.wait_for(std::chrono::seconds(0))==std::future_status::ready){auto actual=control.manualReading.get();if(!actual){if(std::chrono::steady_clock::now()>=control.stageDeadline)hold();return;}if(actual->assignments().empty()){hold();return;}try{control.WriteManual(std::move(*actual),true);}catch(...){hold();return;}}
    }
    if (!control.loaded) return;
    if(control.prepareBoot){
        if(control.runtime.sourceRequested||control.runtime.relayRequested){hold();return;}
        if(control.opened)return;
        if(!control.preparedWritten&&std::chrono::steady_clock::now()>=control.stageDeadline){hold();return;}
        if(!maintenancePreparedStart_||!afterStartFn_)return;
        if(!control.preparedWritten&&!control.writing.valid()){
            if(!control.preparing.valid())control.preparing=kafkaManager.PrepareReadiness(control.runtime.plan,control.runtime.request.evidence().manifest_digest(),control.runtime.source->issuer_id(),control.instance);
            if(control.preparing.wait_for(std::chrono::seconds(0))==std::future_status::ready){auto actual=control.preparing.get();if(actual){try{control.WritePrepared(std::move(*actual));}catch(...){hold();return;}}}
            return;
        }
        if(control.preparedWritten&&control.openIntent&&!control.writing.valid()){
            if(!control.ValidOpen()){hold();return;}
            try{
                if(!kafkaManager.OpenPrepared(control.consumers,[&]{
                    if(maintenanceSourceOpened_||shutdownStarted.load()||!rpcServer||!afterStartFn_)return false;
                    if(!RpcChannel::ProcessMaintenance()->OpenPreparedSources())return false;
                    maintenanceSourceOpened_=true;return true;
                })){hold();return;}
                StartPreparedSourceInLoop();control.WriteOpen();
            }catch(...){hold();return;}
        }
        return;
    }
    if (control.runtime.sourceRequested) CloseMaintenanceSource();
    if (!control.sourceClosed || control.held) return;
    if (HasKnownShutdownFailure()) { hold(); return; }
    if (!control.sourceWritten && std::chrono::steady_clock::now() >= control.stageDeadline) { hold(); return; }
    const auto ingressSettled=[&](std::string& frontier) {
        const bool rpcReady=maintenanceInboundRelay_?RpcChannel::ProcessMaintenance()->SettledFrontier(frontier):RpcChannel::ProcessMaintenance()->Frontier(true,frontier);
        if(!rpcReady)return false;
        if(grpcServer_){const auto stats=grpcServer_->Stats();
            if(stats.inFlight!=0||stats.deadlineAfterStart!=0||stats.abortedAfterStart!=0||stats.unavailableOnShutdown!=0||stats.lateFinish!=0||stats.dropped!=0||stats.droppedOffLoop!=0)return false;
            frontier+="\ngrpc-server:"+std::to_string(stats.dispatched)+":"+std::to_string(stats.inFlight);
        }else if(!grpcServices_.empty()&&!shutdownGrpcDrainComplete_)return false;
        return true;
    };
    if (!control.sourceWritten && !control.writing.valid() && shutdownDrainCompleteFn_(*this)) {
        std::string rpc, grpc, redis;
        const auto kafka = KafkaProducer::ProcessPublicationFrontier();
        if (ingressSettled(rpc) && muduo_grpc::Channel::ProcessMaintenance()->SettledFrontier(grpc) &&
            hiredis::Hiredis::ProcessMaintenance()->SettledFrontier(redis) && !kafka.unknown && kafka.activeSends == 0 && kafka.activeCallbacks == 0 && kafka.accepted == kafka.confirmed) {
            control.sourceFrontier = rpc + "\n" + grpc + "\n" + redis + "\nkafka-accepted:" + std::to_string(kafka.accepted) + ":" + std::to_string(kafka.confirmed);
            control.Write(true, control.sourceFrontier);
        }
    }
    if (!control.runtime.relayRequested || !control.runtime.relay || !control.sourceWritten || control.relayWritten) return;
    if (!control.relayDraining) { control.relayDraining = true; control.stageDeadline = std::chrono::steady_clock::now() + std::chrono::seconds(60);
        RpcChannel::ProcessMaintenance()->CloseSources(); ShutdownGrpcServer();
        control.manualDraining=kafkaManager.BeginManualDrain();
    }
    if (std::chrono::steady_clock::now() >= control.stageDeadline) { hold(); return; }
    if(!control.manualDrainReady){
        if(control.manualDraining.valid()&&control.manualDraining.wait_for(std::chrono::seconds(0))==std::future_status::ready){if(!control.manualDraining.get()){hold();return;}control.manualDrainReady=true;}
        if(!control.manualDrainReady)return;
    }
    if (!control.relayClosed) {
        if(!control.manualConsumersStopped){if(!kafkaManager.ManualDrainReady())return;kafkaManager.StopConsumers();control.manualConsumersStopped=true;}
        if (!shutdownGrpcDrainComplete_ || !kafkaManager.ConsumersJoined() || !shutdownDrainCompleteFn_(*this)) return;
        std::string rpc, grpc, redis;
        const auto kafka = KafkaProducer::ProcessPublicationFrontier();
        if (!RpcChannel::ProcessMaintenance()->Frontier(true, rpc) || !muduo_grpc::Channel::ProcessMaintenance()->SettledFrontier(grpc) ||
            !hiredis::Hiredis::ProcessMaintenance()->SettledFrontier(redis) || kafka.unknown || kafka.activeSends != 0 || kafka.activeCallbacks != 0 || kafka.accepted != kafka.confirmed) return;
        control.relayClosed = true; control.relayClosedMs = NodeMaintenanceControl::NowMs();
        maintenanceRelayStop_(*this); beforeTeardownRan_ = true;
        CloseTransportOwnersInLoop();
        ReleaseLocalOwnersInLoop();
        kafkaManager.Shutdown(std::chrono::milliseconds(0));
    }
    if (std::chrono::steady_clock::now() >= control.stageDeadline) { hold(); return; }
    bool ownsManual=false;for(const auto& row:control.runtime.plan.manual_assignments())if(row.issuer_id()==control.runtime.source->issuer_id()&&row.instance_id()==control.instance)ownsManual=true;
    if(ownsManual&&!control.manualWritten){
        if(!control.manualReading.valid()&&!control.writing.valid())control.manualReading=kafkaManager.ManualEvidence(control.runtime.request.run_id(),control.runtime.request.evidence().manifest_digest(),control.runtime.source->issuer_id(),control.instance);
        if(control.manualReading.valid()&&control.manualReading.wait_for(std::chrono::seconds(0))==std::future_status::ready){auto actual=control.manualReading.get();if(!actual||actual->assignments().empty()||!actual->accepting_closed()||!actual->callbacks_joined()||actual->unresolved_publications()!=0||actual->accepted()!=actual->settled()){hold();return;}try{control.WriteManual(std::move(*actual),false);}catch(...){hold();return;}}
        return;
    }
    std::string rpc, grpc, redis;
    const auto kafka = kafkaManager.ClosedPublicationFrontier();
    if (!control.writing.valid() && rpcNativeJoined_ && kafkaManager.LocalWorkJoined() && RpcChannel::ProcessMaintenance()->LocalCallbacksJoined() &&
        muduo_grpc::Channel::ProcessMaintenance()->Joined() && muduo_grpc::Channel::ProcessMaintenance()->NativeJoined() &&
        muduo_grpc::Channel::ProcessControlMaintenance()->Joined() && muduo_grpc::Channel::ProcessControlMaintenance()->NativeJoined() &&
        hiredis::Hiredis::ProcessMaintenance()->Joined() && RpcChannel::ProcessMaintenance()->Frontier(false, rpc) &&
        muduo_grpc::Channel::ProcessMaintenance()->Frontier(grpc) && hiredis::Hiredis::ProcessMaintenance()->Frontier(redis) && kafka) {
        control.Write(false, control.sourceFrontier + "\n" + rpc + "\n" + grpc + "\n" + redis + "\n" + *kafka + "\nmanual:" + ControlHex(control.manualDigest));
    }
}

void Node::CloseTransportOwnersInLoop()
{
    eventLoop.assertInLoopThread();
    if (transportCloseStarted_) return;
    transportCloseStarted_ = true;
    ReleaseNodeId();
    serviceDiscoveryManager.Shutdown();
    serviceHealthMonitorTimer.Cancel(); conflictDrainTimer.Cancel();
    RpcChannel::ProcessMaintenance()->ClosePublications();
    muduo_grpc::Channel::ProcessMaintenance()->Close();
    muduo_grpc::Channel::ProcessControlMaintenance()->Close();
    NodeUtils::CloseAllRpcChannels();
    for (auto& registry : tlsNodeContextManager.GetAllRegistries()) registry.clear<MuduoGrpcChannelPtr>();
}

void Node::ReleaseLocalOwnersInLoop()
{
    eventLoop.assertInLoopThread();
    if (localOwnerReleaseStarted_) return;
    localOwnerReleaseStarted_ = true;
    hiredis::Hiredis::ProcessMaintenance()->Close();
    tlsRedis.Shutdown();
    tlsPlayerRedis.Shutdown(); // 只有 scene 连过玩家域;其它节点这里是空操作
    // TcpServer 析构不能发生在 I/O 分发栈；所有 TCP owner 都在原 loop 释放，随后两跳覆盖 forceClose/connectDestroyed。
    eventLoop.queueInLoop([this] {
        rpcServer.reset(); tlsNodeContextManager.Clear();
        eventLoop.queueInLoop([this] { eventLoop.queueInLoop([this] { rpcNativeJoined_ = true; }); });
    });
}

void Node::RequestShutdown()
{
	// 已开始(包括已完成)时不要再向可能已经退出的 EventLoop 投递捕获 this 的
	// 重复任务。EventLoop 外的 Shutdown() 仍会在本调用返回后等待完成条件。
	if (shutdownStarted.load(std::memory_order_acquire))
	{
		return;
	}

	if (eventLoop.isInLoopThread())
	{
		ShutdownInLoop();
		return;
	}

	eventLoop.queueInLoop([this]
						   { ShutdownInLoop(); });
}

void Node::Shutdown()
{
	RequestShutdown();
	if (eventLoop.isInLoopThread())
	{
		return;
	}

	std::unique_lock<std::mutex> lock(shutdownCompletionMutex_);
	if (!shutdownCompletionCv_.wait_for(lock, kShutdownCompletionWaitTimeout,
										[this]
										{ return shutdownComplete_; }))
	{
		LOG_ERROR << "Node shutdown timed out waiting for loop thread. timeout_s="
				  << std::chrono::duration_cast<std::chrono::seconds>(kShutdownCompletionWaitTimeout).count();
	}
}

void Node::ShutdownInLoop()
{
	eventLoop.assertInLoopThread();
	if (shutdownStarted.exchange(true, std::memory_order_acq_rel))
	{
		return;
	}

	LOG_DEBUG << "Node shutting down...";

	// 先封住 Kafka 入站并 join 后台 poller,避免 drain 期间继续向 loop 投递
	// command(player_migrate 搬数据链已删,入站不再有 migration 消息)。
	// producer 保持可用,收尾阶段还要发事件。
	kafkaManager.StopConsumers();

	RpcChannel::ProcessMaintenance()->CloseSources();
	if (!sourceBusinessClosed_ && beforeShutdownFn_)
	{
		sourceBusinessClosed_ = true;
		LOG_INFO << "Running before-shutdown hook...";
		beforeShutdownFn_(*this);
	}

	// 业务 barrier 与 gRPC 排空并行推进。先执行 hook 生成待落地工作,随后立刻进入
	// gRPC 排空(停听、两段 GOAWAY,已收完的调用照常派发);EventLoop 保持运行,让这些调用
	// 与 Redis 回调完成。同一个 loop 任务里 hook 与 BeginShutdown 之间不可能派发任何调用,
	// 保持这个顺序是为了不让 hook 的阻塞时间吃掉排空预算(grpc-muduo-transport.md §15.3)。
	StartShutdownDrainWatchdog();
	ShutdownGrpcServer();
	MaybeFinalizeShutdownInLoop();
}

void Node::StartShutdownDrainWatchdog()
{
	eventLoop.assertInLoopThread();

	// 即使业务 hook 同步完成，Kafka 已排入 loop 的回调仍须真实返回。
	constexpr double kDrainPollIntervalSec = 0.1;
	shutdownDrainDeadline_ = std::chrono::steady_clock::now() + kShutdownDrainBudget;
	LOG_INFO << "Waiting for before-shutdown work to drain. timeout_s="
			 << std::chrono::duration_cast<std::chrono::seconds>(kShutdownDrainBudget).count();

	// 即使 predicate 暂时为 true 也不能锁存:在 gRPC 排空完成前,排空期间派发的
	// 调用仍可能向业务侧追加工作。每 100ms 都现场重查,OnGrpcServerDrained 也会重查。
	shutdownDrainTimer.RunEvery(kDrainPollIntervalSec, [this]
								{ MaybeFinalizeShutdownInLoop(); });
}

void Node::MaybeFinalizeShutdownInLoop()
{
	eventLoop.assertInLoopThread();
	if (!shutdownStarted.load(std::memory_order_acquire) || shutdownFinalizationStarted_)
	{
		return;
	}

	const bool businessDrained = !shutdownDrainCompleteFn_ || shutdownDrainCompleteFn_(*this);
	const bool businessExpired = std::chrono::steady_clock::now() >= shutdownDrainDeadline_;
	const bool kafkaCallbacksJoined = kafkaManager.ConsumersJoined();
	if (!shutdownGrpcDrainComplete_ || !kafkaCallbacksJoined || (!businessDrained && !businessExpired))
	{
		if (businessExpired) shutdownFailure_.store(true, std::memory_order_release);
		return;
	}

	shutdownFinalizationStarted_ = true;
	shutdownDrainTimer.Cancel();
	if (businessDrained && kafkaCallbacksJoined)
	{
		LOG_INFO << "Before-shutdown work and queued Kafka callbacks joined.";
	}
	else
	{
		shutdownFailure_.store(true, std::memory_order_release);
		LOG_ERROR << "Lifecycle HOLD: shutdown budget expired with unresolved work; teardown is not a closure receipt. timeout_s="
				  << std::chrono::duration_cast<std::chrono::seconds>(kShutdownDrainBudget).count();
	}
	FinalizeShutdownInLoop();
}

void Node::FinalizeShutdownInLoop()
{
	eventLoop.assertInLoopThread();
	// 防御(grpc-muduo-transport.md §15.10):正常路径下 gRPC 服务端已在 OnGrpcServerDrained 里销毁,
	// 仍在说明它没走完排空就进了收尾。不能在这里同步 reset:本函数可能从看门狗定时器进入,Linux 的定时器
	// 回调就跑在事件处理期间,此时销毁 Acceptor 会撞上 EventLoop removeChannel 的断言。移进排队任务销毁,
	// FIFO 排在下面的 FinalizeTeardownInLoop 之前;~Server 排进的 forceClose / connectDestroyed 由拆除末尾的
	// 两跳 quit 覆盖。
	if (grpcServer_)
	{
		LOG_ERROR << "gRPC server still alive at shutdown finalization; destroying it in a queued task. listen="
				  << grpcServer_->listenAddress();
		eventLoop.queueInLoop([server = std::move(grpcServer_)]() mutable
							  { server.reset(); });
	}
	shutdownDrainTimer.Cancel();
	maintenanceControlTimer_.Cancel();
	CloseTransportOwnersInLoop();
	eventLoop.queueInLoop([this]
	{
		FinalizeTeardownInLoop();
	});
}

void Node::FinalizeTeardownInLoop()
{
	eventLoop.assertInLoopThread();

	// node_id / 端口的重试定时器由 serviceDiscoveryManager.Shutdown() -> EtcdService::Shutdown()
	// 取消,不在这里。
	if (!kafkaManager.Shutdown())
	{
		shutdownFailure_.store(true, std::memory_order_release);
		LOG_ERROR << "Kafka 生命周期 HOLD：native producer 或已接收回调尚未确认，禁止以进程退出签闭合";
	}
	// 拆除前钩子(SetBeforeTeardown):排在 producer 最后一次有界 flush 之后(重入时 kafkaManager.Shutdown 只返回缓存结果,
	// 不再 flush),排在 Redis / ECS / 注册表拆除与 logSystem.stop() 之前 —— 业务在这里停掉自己的定时器并把未落定的事
	// 打进日志(scene:未落定玩家清单)。只在第一次进入时调用:下面的让轮会重入本函数,
	// 先停掉的定时器也就不会在让轮期间、在半拆的节点上再推业务。
	if (!beforeTeardownRan_)
	{
		beforeTeardownRan_ = true;
		if (beforeTeardownFn_)
		{
			beforeTeardownFn_(*this);
		}
	}
	// Hiredis 持有 muduo Channel 和待回调命令;必须在 EventLoop 与回调目标都存活时释放。
	ReleaseLocalOwnersInLoop();
	// 阶段 0 与本函数之间、以及 Redis 取消回调里，都可能向已关闭节点 Send:那是排在本函数之后的异步本地失败。
	// 让一轮,等它们在 ECS / 日志仍活着时执行完再拆(Kafka / Redis 的 Shutdown 可重入)。
	if (!rpcNativeJoined_ || !GrpcClientLocalCompletionsDrained() || !kafkaManager.LocalWorkJoined() ||
        !RpcChannel::ProcessMaintenance()->LocalCallbacksJoined() ||
        !muduo_grpc::Channel::ProcessMaintenance()->Joined() || !muduo_grpc::Channel::ProcessMaintenance()->NativeJoined() ||
        !muduo_grpc::Channel::ProcessControlMaintenance()->Joined() || !muduo_grpc::Channel::ProcessControlMaintenance()->NativeJoined() ||
        !hiredis::Hiredis::ProcessMaintenance()->Joined())
	{
		// 等的只是仍引用业务对象的本机工作；UNKNOWN 已退出的路径不会卡在此处。
		shutdownDrainTimer.RunAfter(0.05, [this] { FinalizeTeardownInLoop(); });
		return;
	}
	// 节点 registry 里的出站 TCP 客户端必须趁 EventLoop 还活着释放:RpcClient 按值持有
	// muduo::net::TcpClient,~TcpClient 会经 loop_ runInLoop / forceClose。以前没人清
	// tlsNodeContextManager,它们要等 thread_local 析构才释放,那时 RunNodeMain 栈上的
	// EventLoop 早已析构(use-after-free)。这里释放后排进队列的 forceCloseInLoop /
	// connectDestroyed 由下面的两跳 queueInLoop 排空(AGENTS.md §11.7)。
	// gRPC 与 RPC0 的在途调用都已失败完成；现在才允许清除整个节点池。
	// 同时释放 protobuf，避免拖到 EventLoop/protobuf 关闭后的 TLS 析构。
	tlsNodeContextManager.Clear();
	// /metrics 出口同理趁 EventLoop 还活着释放(~TcpServer 要在 loop 线程上关掉已有连接);它的采集回调读 tlsEcs,
	// 必须排在 tlsEcs.Clear() 之前。关闭排进队列的连接收尾由下面的两跳 queueInLoop 排空。
	metricsExporter_.reset();
	// Cleared by tlsEcs.Clear() below.
	tlsEcs.Clear();
#ifdef WIN32
	muduo::Logger::setOutput(LogToConsole);
#endif
	logSystem.stop();

	// 不能在这里直接 quit()。Windows 控制台 Ctrl+C / 关窗走 SetConsoleCtrlHandler →
	// Shutdown() → queueInLoop(ShutdownInLoop),于是本函数是在 doPendingFunctors 里跑的:
	// beforeShutdownFn_ 刚对每个客户端会话 forceClose(),它们的 forceCloseInLoop 被排进
	// 了**刚被 swap 出来的新队列**;没有 gRPC server 的节点(gate)又在同一栈里同步走到
	// 这里 —— 此时 quit() 会让 loop() 在 while(!quit_) 处退出,muduo_windows 的 ~EventLoop
	// 不排空 pendingFunctors_,forceCloseInLoop 永不执行,连接停在 kDisconnecting,最后在
	// thread_local(gate 的 tlsSessionManager)析构、EventLoop 已死之后释放最后一根引用,
	// 撞 TcpConnection.cc:71 assert(state_ == kDisconnected)。
	//
	// 需要排空的不是一趟而是**两趟**:第 N+1 趟 forceCloseInLoop → handleClose(它把
	// connectDestroyed 再排进队列),第 N+2 趟 connectDestroyed → channel_->remove() 清掉
	// addedToLoop_;少一趟,~Channel 会在 EventLoop 析构时撞 assert(!addedToLoop_)。
	//
	// 用**两跳 queueInLoop**,靠队列的 FIFO 把 quit() 排到两趟之后:
	//   本趟(N)  :forceCloseInLoop×k 已在队,再排入外跳;
	//   N+1      :forceCloseInLoop×k 各自把 connectDestroyed 排入队列,然后外跳跑、
	//              把内跳排在它们之后;
	//   N+2      :connectDestroyed×k 先跑,内跳最后 quit()。
	// 顺序由队列保证,与 poll 何时返回无关。**不能用定时器**(09-14 评审推翻过一版
	// runAfter(0.05) 的写法):Windows 的 loop 是 poll → handleEvents → timerQueue_->loop()
	// → doPendingFunctors,定时器若恰在 N+1 趟到期,quit_ 先置位、本趟只跑完
	// forceCloseInLoop 就退出,connectDestroyed 永远不跑,assert 照撞。
	// 完成标记必须等最后一跳执行完 quit() 才置位,否则析构的 fallback 会跳过排空,
	// EventLoop 外的析构也可能提前释放仍被这两跳捕获的 Node。notify 在锁内完成,
	// 等待方取得锁时已没有后续 Node 访问;解锁后不得再访问任何成员。
	// 见 docs/ops/incident-gate-tcpconnection-dtor-assert-2026-09-13.md §7.11。
	auto *const loop = &eventLoop;
	loop->queueInLoop([this, loop]
	{
		loop->queueInLoop([this, loop]
		{
			loop->quit();
			if (shutdownFailure_.load(std::memory_order_acquire)) { LOG_ERROR << "Node teardown complete with lifecycle HOLD"; }
			else { LOG_DEBUG << "Node teardown complete; maintenance closure requires original owner receipts"; }
			std::lock_guard<std::mutex> lock(shutdownCompletionMutex_);
			shutdownComplete_ = true;
			shutdownCompletionCv_.notify_all();
		});
	});
}

void Node::InitLogSystem()
{
	// Log system already started in constructor; just apply config-driven log level.
	// 数值即 muduo 枚举(node_log_level.h,没有映射表);readBaseDeployConfig 已对缺键 / 越界拒启。
	// 转换前再核一次:越界值转成枚举是未定义行为,不依赖「只有读配置会写这个字段」。
	static_assert(static_cast<uint32_t>(muduo::Logger::TRACE) == 0 &&
					  static_cast<uint32_t>(muduo::Logger::FATAL) == node_log_level::kMaxValue,
				  "LogLevel 配置值直接按 muduo 枚举解释;枚举顺序变了要同步 node_log_level.h 与 bin/etc 注释");
	const uint32_t configured = tlsNodeConfigManager.GetBaseDeployConfig().log_level();
	if (!node_log_level::IsValidValue(configured))
	{
		FatalStartupConfig("LogLevel", "log_level=" + std::to_string(configured) +
										   " is not a muduo Logger::LogLevel (0=TRACE .. 5=FATAL)");
	}
	muduo::Logger::setLogLevel(static_cast<muduo::Logger::LogLevel>(configured));
}

void Node::LoadConfigs()
{
	tlsNodeConfigManager = NodeConfigManager();
	readBaseDeployConfig("etc/base_deploy_config.yaml", tlsNodeConfigManager.GetBaseDeployConfig());

	// GAME_CONFIG_PATH lets a shared container image load a role-specific
	// game_config (for example game_config_instance.yaml) without mounting
	// a different ConfigMap. Falls back to etc/game_config.yaml.
	std::string gameConfigPath = "etc/game_config.yaml";
	if (const char *envPath = std::getenv("GAME_CONFIG_PATH");
	    envPath != nullptr && envPath[0] != '\0')
	{
		gameConfigPath = envPath;
		LOG_INFO << "GAME_CONFIG_PATH env override applied: " << gameConfigPath;
	}
	readGameConfig(gameConfigPath, tlsNodeConfigManager.GetGameConfig());
	gNodeConfigManager = tlsNodeConfigManager;
}

void Node::LoadAllConfigData()
{
	LoadTablesAsync();
}

void Node::SetupTimeZone()
{
#ifdef __linux__
	const muduo::TimeZone hkTz("zoneinfo/Asia/Hong_Kong");
#else
	const muduo::TimeZone hkTz(8 * 3600, "zoneinfo/Asia/Hong_Kong");
#endif // __linux__
	muduo::Logger::setTimeZone(hkTz);
}

void Node::StopWatchingServiceNodes()
{
	EtcdHelper::StopAllWatching();
}

void Node::ReleaseNodeId()
{
	EtcdHelper::RevokeLeaseAndCleanup(serviceDiscoveryManager.etcdService.GetLeaseId());
}

// These functions are provided by each node binary; the linker resolves them.
// 每个节点二进制都要提供下面两个链接符号(玩家回包链 InitPlayerServiceReplied 已随第 6 批删除)。按 message_id 的全局回包表(InitReply)已退场:节点间 RPC0 的回包
// 在调用点的每次调用回调里处理(docs/design/muduo-rpc-channel.md §9.2)。
void InitPlayerService();
void InitServiceHandler();
void Node::RegisterHandlers()
{
	InitMessageInfo();
	InitPlayerService();
	InitServiceHandler();
}

void Node::AsyncOutput(const char *msg, int len)
{
	Node *activeNode = gNodeAtomic.load(std::memory_order_acquire);
	if (activeNode != nullptr)
	{
		activeNode->Log().append(msg, len);
	}
#ifdef WIN32
	LogToConsole(msg, len);
#endif
}

bool Node::IsCurrentNode(const NodeInfo &candidateNode) const
{
	return NodeUtils::IsSameNode(candidateNode.node_uuid(), GetNodeInfo().node_uuid());
}

void Node::HandleServiceNodeStop(const std::string &key, const std::string &nodeJson)
{
	eventLoop.assertInLoopThread();
	LOG_INFO << "Service node stop, key: " << key << ", value: " << nodeJson;

	// Mirror HandleServiceNodeStart: allocation-slot keys store a raw uuid,
	// not a NodeInfo JSON. Etcd lease revoke fires DELETE for both the
	// allocation key AND the discovery key in the same shutdown, so without
	// this guard every clean stop produces a spurious "Parse node JSON
	// failed" error line per node type. The real removal is driven by the
	// matching discovery-key DELETE that comes through alongside it.
	if (key.find("/allocated/") != std::string::npos)
	{
		LOG_TRACE << "Skip allocation-slot delete for service discovery: " << key;
		return;
	}

	NodeInfo stoppedNode;
	// D77:忽略未知字段且永久保留。新版本节点会多带字段(如 clientEndpoint),严格解析会让
	// 滚动期间的旧解析方把整条记录当坏数据丢掉;与 Go 侧 shared/nodeinfo.Unmarshal 同一口径。
	google::protobuf::util::JsonParseOptions parseOptions;
	parseOptions.ignore_unknown_fields = true;
	auto parseResult = google::protobuf::util::JsonStringToMessage(nodeJson, &stoppedNode, parseOptions);
	if (!parseResult.ok())
	{
		LOG_ERROR << "Parse node JSON failed, key: " << key
				  << ", JSON: " << nodeJson
				  << ", Error: " << parseResult.message().data();
		return;
	}

	if (!eNodeType_IsValid(stoppedNode.node_type()))
	{
		LOG_TRACE << "Unknown service type for key: " << key;
		return;
	}

	if (stoppedNode.node_uuid().empty())
	{
		LOG_WARN << "Ignore service node stop with empty node_uuid. key=" << key;
		return;
	}

	// Mirror the AddServiceNode zone filter: zone-scoped services never make
	// it into this node's registry when they come from a foreign zone, so a
	// stop event for them must not trigger ExecuteNodeRemoval — that path
	// keys removal by (node_type, node_id) for TCP nodes and would otherwise
	// destroy the local entity at the same node_id slot even though it
	// belongs to a different zone.
	if (NodeUtils::IsZoneScopedNodeType(stoppedNode.node_type()) &&
		stoppedNode.zone_id() != GetNodeInfo().zone_id())
	{
		LOG_INFO << "Skip zone-scoped stop event for foreign zone. key=" << key
				 << ", node_zone=" << stoppedNode.zone_id()
				 << ", self_zone=" << GetNodeInfo().zone_id();
		return;
	}

	const auto graceSeconds = tlsNodeConfigManager.GetBaseDeployConfig().node_removal_grace_seconds();

	// --- Grace period path: defer removal so breakpoint-paused nodes can re-register ---
	if (graceSeconds > 0)
	{
		const auto &nodeUuid = stoppedNode.node_uuid();

		// Already pending? Reset timer.
		if (pendingNodeRemovals_.count(nodeUuid))
		{
			LOG_INFO << "Node removal already pending, resetting grace timer. uuid=" << nodeUuid;
			pendingNodeRemovals_.erase(nodeUuid);
		}

		auto pending = std::make_unique<PendingNodeRemoval>();
		pending->nodeInfo.CopyFrom(stoppedNode);

		// Capture uuid by value for the timer callback.
		std::string capturedUuid = nodeUuid;
		pending->timer.RunAfter(static_cast<double>(graceSeconds), [this, capturedUuid]()
								{
			auto it = pendingNodeRemovals_.find(capturedUuid);
			if (it == pendingNodeRemovals_.end())
			{
				return; // Already cancelled by a re-register PUT event.
			}

			LOG_WARN << "Grace period expired, removing node. uuid=" << capturedUuid;
			NodeInfo expiredNode;
			expiredNode.CopyFrom(it->second->nodeInfo);
			pendingNodeRemovals_.erase(it);

			// Execute the actual removal (same logic as the immediate path below).
			ExecuteNodeRemoval(expiredNode); });

		pendingNodeRemovals_[nodeUuid] = std::move(pending);
		LOG_INFO << "Node removal deferred for " << graceSeconds << "s grace period. uuid=" << nodeUuid;
		return;
	}

	// --- Immediate removal path (production: grace_seconds == 0) ---
	ExecuteNodeRemoval(stoppedNode);
}

void Node::CancelPendingNodeRemoval(const std::string &nodeUuid)
{
	auto it = pendingNodeRemovals_.find(nodeUuid);
	if (it == pendingNodeRemovals_.end())
	{
		return;
	}

	LOG_INFO << "Node re-registered during grace period, cancelling pending removal. uuid=" << nodeUuid;
	it->second->timer.Cancel();
	pendingNodeRemovals_.erase(it);
}

void Node::ExecuteNodeRemoval(const NodeInfo &stoppedNode)
{
	// 先移除发现快照,网络实体的关闭仍走下面既有的延后收尾。
	const int removedSnapshotCount = serviceDiscoveryManager.RemoveServiceNode(
		stoppedNode.node_type(), stoppedNode.node_uuid());

	if (removedSnapshotCount == 0)
	{
		LOG_WARN << "Service node stop did not match local cache. node_id=" << stoppedNode.node_id()
				 << ", node_uuid=" << stoppedNode.node_uuid()
				 << ", node_type=" << stoppedNode.node_type();
	}
	else
	{
		LOG_INFO << "Removed " << removedSnapshotCount << " stale node record(s) for uuid=" << stoppedNode.node_uuid();
	}

	const auto stoppedNodeType = stoppedNode.node_type();
	const auto stoppedNodeId = stoppedNode.node_id();
	const auto stoppedNodeUuid = stoppedNode.node_uuid();
	const auto stoppedProtocolType = stoppedNode.protocol_type();

	// Important: destroy network entities after current channel dispatch cycle.
	// Direct destroy inside etcd watch callback can invalidate activeChannels_.
	GetLoop()->queueInLoop([stoppedNodeType, stoppedNodeId, stoppedNodeUuid, stoppedProtocolType]()
						   {
		entt::registry& nodeRegistry = tlsNodeContextManager.GetRegistry(stoppedNodeType);

		// Both TCP and gRPC nodes are now keyed by uuid: TCP entities used to
		// be allocated as `entt::entity{node_id}` (see ConnectToTcpNode pre-
		// uuid-refactor) but that scheme aliased nodes from different zones
		// onto the same slot and required a parallel removal path. With both
		// node_connector paths using auto-assigned entity ids and uuid for
		// dedup, removal is uniformly a uuid scan.
		(void)stoppedProtocolType;
		(void)stoppedNodeId;
		for (const auto& [entity, nodeInfo] : nodeRegistry.view<NodeInfo>().each()) {
			if (!NodeUtils::IsSameNode(nodeInfo.node_uuid(), stoppedNodeUuid)) {
				continue;
			}

			OnNodeRemoveEvent nodeRemovedEvent;
			nodeRemovedEvent.node = entity;
			nodeRemovedEvent.nodeType = stoppedNodeType;
			// 先摘掉 gRPC 通道:通知里的后续选点不能再挑中正在摘除的节点(HasUsableGrpcClient 看通道)。
			// ~Channel 把在途调用以 UNAVAILABLE 排进队列,完成回调在实体销毁之后才执行,不捕获实体(§14.2)。
			nodeRegistry.remove<MuduoGrpcChannelPtr>(entity);
			tlsEcs.dispatcher.trigger(nodeRemovedEvent);

			if (nodeRegistry.valid(entity))
			{
				DestroyEntity(nodeRegistry, entity);
			}
			return;
		}
		LOG_WARN << "Service node stop: entity not found for uuid=" << stoppedNodeUuid
			<< ", node_type=" << stoppedNodeType
			<< ", node_id=" << stoppedNodeId; });
	LOG_INFO << "Service node stopped : " << stoppedNode.DebugString();
}

void Node::StartNodeRegistrationHealthMonitor()
{
	serviceHealthMonitorTimer.RunEvery(tlsNodeConfigManager.GetBaseDeployConfig().health_check_interval(),
									   [this, reRegistrationRequested = false]() mutable
									   {
										   if (rpcServer == nullptr)
										   {
											   return;
										   }

										   // 先检查租约截止时间,覆盖 Watch 流中断而本地 ServiceNodeSnapshot 尚未更新的网络分区。
										   //
										   // 失租**不再**直接走冲突关停:拿新 lease 重注册,分配键的 CAS 才是身份的裁判
										   // (被别的 uuid 持有 → OnTxnFailed → OnNodeIdConflictShutdown)。
										   // 永久 guid 走号段(GuidSegmentRegistry),唯一性由数据库的 CAS 领段保证,与 lease 无关。
										   if (serviceDiscoveryManager.etcdService.IsLeasePresumablyExpired())
										   {
											   if (!reRegistrationRequested)
											   {
												   LOG_ERROR << "Lease deadline exceeded: no keepalive ACK from etcd within TTL. "
																"node_id="
															 << GetNodeInfo().node_id()
															 << ". Re-granting a lease and re-attaching routing keys (no fence: identity is decided by the CAS).";
												   serviceDiscoveryManager.etcdService.RequestReRegistration("lease deadline exceeded locally");
												   reRegistrationRequested = true;
											   }
											   return;
										   }

										   const auto &currentNode = GetNodeInfo();

										   for (const auto &registeredNode : serviceDiscoveryManager.ServiceNodes().NodesOfType(currentNode.node_type()))
										   {
											   if (IsCurrentNode(registeredNode))
											   {
												   reRegistrationRequested = false;
												   return;
											   }
										   }

										   // Node snapshot disappeared from service discovery.
										   // Re-register with the same node_id; do not run full re-allocation flow.
										   if (reRegistrationRequested)
										   {
											   return;
										   }

										   serviceDiscoveryManager.etcdService.RequestReRegistration("health monitor missing local node snapshot");
										   reRegistrationRequested = true;
									   });
}
