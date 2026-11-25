#include "test_h2_client.h"

// nghttp2.h 必须先于任何 muduo 头文件(本文件不包含 muduo)。工程里定义了
// NGHTTP2_STATICLIB(静态库)与 NGHTTP2_NO_SSIZE_T(MSVC 没有 ssize_t,只隐藏已废弃的 ssize_t 接口)。
#include <nghttp2/nghttp2.h>

#include <algorithm>
#include <cstring>

#include "muduo_grpc/server_session.h"
#include "muduo_grpc/status.h"

namespace muduo_grpc_test
{

namespace
{

std::string BytesToString(const uint8_t *data, size_t len)
{
    return len == 0 ? std::string() : std::string(reinterpret_cast<const char *>(data), len);
}

std::vector<nghttp2_nv> MakeNva(const HeaderList &headers, uint8_t nameFlags)
{
    std::vector<nghttp2_nv> nva;
    nva.reserve(headers.size());
    for (const auto &[name, value] : headers)
    {
        nghttp2_nv nv;
        nv.name = reinterpret_cast<uint8_t *>(const_cast<char *>(name.data()));
        nv.value = reinterpret_cast<uint8_t *>(const_cast<char *>(value.data()));
        nv.namelen = name.size();
        nv.valuelen = value.size();
        nv.flags = nameFlags; // 普通入口拷贝名和值;非法名称入口只借用已经稳定持有的名称
        nva.push_back(nv);
    }
    return nva;
}

std::string LibError(const char *what, int code)
{
    return std::string(what) + " failed: " + nghttp2_strerror(code);
}

const std::string *FindIn(const HeaderList &headers, std::string_view name)
{
    for (const auto &[key, value] : headers)
    {
        if (key == name)
        {
            return &value;
        }
    }
    return nullptr;
}

} // namespace

// nghttp2 客户端回调:只记状态。全部只会在 TestH2Client 的方法(mem_recv2 / mem_send2)调用栈里发生。
struct TestH2ClientCallbacks
{
    static TestH2Client &Self(void *userData) { return *static_cast<TestH2Client *>(userData); }

    static int OnBeginHeaders(nghttp2_session *, const nghttp2_frame *frame, void *userData)
    {
        if (frame->hd.type == NGHTTP2_HEADERS)
        {
            Self(userData).StreamFor(frame->hd.stream_id);
        }
        return 0;
    }

    static int OnHeader(nghttp2_session *, const nghttp2_frame *frame, const uint8_t *name, size_t nameLen,
                        const uint8_t *value, size_t valueLen, uint8_t, void *userData)
    {
        if (frame->hd.type != NGHTTP2_HEADERS)
        {
            return 0;
        }
        ClientStream &stream = Self(userData).StreamFor(frame->hd.stream_id);
        HeaderList &target = frame->headers.cat == NGHTTP2_HCAT_RESPONSE ? stream.responseHeaders : stream.trailers;
        target.emplace_back(BytesToString(name, nameLen), BytesToString(value, valueLen));
        return 0;
    }

    static int OnFrameRecv(nghttp2_session *, const nghttp2_frame *frame, void *userData)
    {
        auto &self = Self(userData);
        self.frameTypes.push_back(frame->hd.type);
        switch (frame->hd.type)
        {
        case NGHTTP2_HEADERS:
        case NGHTTP2_DATA:
        {
            ClientStream &stream = self.StreamFor(frame->hd.stream_id);
            stream.frames.push_back(ClientFrame{frame->hd.type, frame->hd.flags, 0});
            if ((frame->hd.flags & NGHTTP2_FLAG_END_STREAM) != 0)
            {
                stream.endStreamReceived = true;
            }
            break;
        }
        case NGHTTP2_RST_STREAM:
        {
            const auto it = self.streams.find(frame->hd.stream_id);
            if (it != self.streams.end())
            {
                it->second.frames.push_back(ClientFrame{frame->hd.type, frame->hd.flags, frame->rst_stream.error_code});
                it->second.rstReceived = true;
                it->second.rstErrorCode = frame->rst_stream.error_code;
            }
            break;
        }
        case NGHTTP2_SETTINGS:
            if ((frame->hd.flags & NGHTTP2_FLAG_ACK) == 0)
            {
                ++self.settingsFramesReceived;
                self.lastPeerSettings.clear();
                for (size_t i = 0; i < frame->settings.niv; ++i)
                {
                    self.lastPeerSettings.push_back(
                        SettingEntry{frame->settings.iv[i].settings_id, frame->settings.iv[i].value});
                }
            }
            break;
        case NGHTTP2_PING:
            if ((frame->hd.flags & NGHTTP2_FLAG_ACK) != 0)
            {
                ++self.pingAcksReceived;
            }
            else
            {
                ++self.pingsReceived; // ACK 由 nghttp2 自动回
            }
            break;
        case NGHTTP2_GOAWAY:
            self.goawaysReceived.push_back(ReceivedGoaway{
                frame->goaway.last_stream_id, frame->goaway.error_code,
                BytesToString(frame->goaway.opaque_data, frame->goaway.opaque_data_len)});
            break;
        default:
            break;
        }
        return 0;
    }

    static int OnDataChunkRecv(nghttp2_session *, uint8_t, int32_t streamId, const uint8_t *data, size_t len,
                               void *userData)
    {
        Self(userData).StreamFor(streamId).body.append(BytesToString(data, len));
        return 0;
    }

    static int OnStreamClose(nghttp2_session *, int32_t streamId, uint32_t errorCode, void *userData)
    {
        auto &self = Self(userData);
        const auto it = self.streams.find(streamId);
        if (it != self.streams.end())
        {
            it->second.closed = true;
            it->second.closeErrorCode = errorCode;
        }
        self.pendingData.erase(streamId);
        return 0;
    }

    static nghttp2_ssize ReadData(nghttp2_session *, int32_t streamId, uint8_t *buf, size_t length,
                                  uint32_t *dataFlags, nghttp2_data_source *, void *userData)
    {
        auto &self = Self(userData);
        const auto it = self.pendingData.find(streamId);
        if (it == self.pendingData.end())
        {
            *dataFlags |= NGHTTP2_DATA_FLAG_EOF;
            return 0;
        }
        TestH2Client::PendingData &pending = it->second;
        const size_t chunk = std::min(length, pending.bytes.size() - pending.offset);
        if (chunk > 0)
        {
            std::memcpy(buf, pending.bytes.data() + pending.offset, chunk);
        }
        pending.offset += chunk;
        if (pending.offset == pending.bytes.size())
        {
            *dataFlags |= NGHTTP2_DATA_FLAG_EOF;
            self.pendingData.erase(it);
        }
        return static_cast<nghttp2_ssize>(chunk);
    }
};

const std::string *ClientStream::FindResponseHeader(std::string_view name) const
{
    return FindIn(responseHeaders, name);
}

const std::string *ClientStream::FindTrailer(std::string_view name) const
{
    return FindIn(trailers, name);
}

void TestH2Client::SessionDeleter::operator()(nghttp2_session *session) const
{
    nghttp2_session_del(session);
}

TestH2Client::TestH2Client() : TestH2Client(TestH2ClientOptions{})
{
}

TestH2Client::TestH2Client(TestH2ClientOptions options)
{
    nghttp2_session_callbacks *rawCallbacks = nullptr;
    if (nghttp2_session_callbacks_new(&rawCallbacks) != 0)
    {
        Fail("nghttp2_session_callbacks_new failed");
        return;
    }
    const std::unique_ptr<nghttp2_session_callbacks, void (*)(nghttp2_session_callbacks *)> callbacks(
        rawCallbacks, &nghttp2_session_callbacks_del);
    nghttp2_session_callbacks_set_on_begin_headers_callback(callbacks.get(), &TestH2ClientCallbacks::OnBeginHeaders);
    nghttp2_session_callbacks_set_on_header_callback(callbacks.get(), &TestH2ClientCallbacks::OnHeader);
    nghttp2_session_callbacks_set_on_frame_recv_callback(callbacks.get(), &TestH2ClientCallbacks::OnFrameRecv);
    nghttp2_session_callbacks_set_on_data_chunk_recv_callback(callbacks.get(),
                                                              &TestH2ClientCallbacks::OnDataChunkRecv);
    nghttp2_session_callbacks_set_on_stream_close_callback(callbacks.get(), &TestH2ClientCallbacks::OnStreamClose);

    nghttp2_session *rawSession = nullptr;
    // 默认选项:自动回 SETTINGS ACK / PING ACK、自动 WINDOW_UPDATE、开启 HTTP 语义校验(只校验收到的响应,
    // 不校验发出去的请求)。
    const int created = nghttp2_session_client_new2(&rawSession, callbacks.get(), this, nullptr);
    if (created != 0)
    {
        Fail(LibError("nghttp2_session_client_new2", created));
        return;
    }
    session.reset(rawSession);
    if (!SendSettings(options.initialSettings))
    {
        return;
    }
    if (options.localConnectionWindowBytes > 0)
    {
        const int rv = nghttp2_session_set_local_window_size(session.get(), NGHTTP2_FLAG_NONE, 0,
                                                             options.localConnectionWindowBytes);
        if (rv != 0)
        {
            Fail(LibError("nghttp2_session_set_local_window_size", rv));
            return;
        }
        Flush(); // 与其余脚本调用一致:立即序列化进输出缓冲
    }
}

TestH2Client::~TestH2Client() = default;

bool TestH2Client::Receive(std::string_view bytes)
{
    if (!session)
    {
        return Fail("client session not created");
    }
    const nghttp2_ssize consumed = nghttp2_session_mem_recv2(
        session.get(), reinterpret_cast<const uint8_t *>(bytes.data()), bytes.size());
    if (consumed < 0)
    {
        return Fail(LibError("nghttp2_session_mem_recv2", static_cast<int>(consumed)));
    }
    if (static_cast<size_t>(consumed) != bytes.size())
    {
        return Fail("nghttp2_session_mem_recv2 consumed " + std::to_string(consumed) + " of " +
                    std::to_string(bytes.size()) + " bytes");
    }
    return true;
}

std::string TestH2Client::TakeOutput()
{
    Flush();
    std::string out;
    out.swap(output);
    return out;
}

bool TestH2Client::Pump(muduo_grpc::ServerSession &server, int maxRounds)
{
    for (int round = 0; round < maxRounds; ++round)
    {
        if (!Flush())
        {
            return false;
        }
        std::string toServer;
        toServer.swap(output);
        if (!toServer.empty())
        {
            const muduo_grpc::Status received =
                server.Receive(reinterpret_cast<const uint8_t *>(toServer.data()), toServer.size());
            if (!received.ok())
            {
                return Fail("server Receive failed: " + received.ToString());
            }
        }
        std::string toClient;
        const muduo_grpc::Status drained = server.DrainOutput([&toClient](std::string_view chunk)
                                                              {
            toClient.append(chunk.data(), chunk.size());
            return true; });
        if (!drained.ok())
        {
            if (!toClient.empty())
            {
                Receive(toClient);
            }
            return Fail("server DrainOutput failed: " + drained.ToString());
        }
        if (!toClient.empty() && !Receive(toClient))
        {
            return false;
        }
        if (toServer.empty() && toClient.empty())
        {
            return true;
        }
    }
    return Fail("Pump did not settle within " + std::to_string(maxRounds) + " rounds");
}

const ClientStream *TestH2Client::FindStream(int32_t streamId) const
{
    const auto it = streams.find(streamId);
    return it == streams.end() ? nullptr : &it->second;
}

int32_t TestH2Client::RemoteConnectionWindow() const
{
    return session ? nghttp2_session_get_remote_window_size(session.get()) : 0;
}

int32_t TestH2Client::OpenStream(const HeaderList &headers, bool endStream)
{
    return OpenStreamWithNameFlags(headers, endStream, NGHTTP2_NV_FLAG_NONE);
}

int32_t TestH2Client::OpenStreamWithUnnormalizedNames(const HeaderList &headers, bool endStream)
{
    // 只改变名称的提交方式;同一会话的 HPACK 动态表、流编号与响应观察完全沿用普通入口。
    auto retained = std::make_unique<HeaderList>(headers);
    const HeaderList &stableHeaders = *retained;
    retainedHeaderNames.push_back(std::move(retained));
    return OpenStreamWithNameFlags(stableHeaders, endStream, NGHTTP2_NV_FLAG_NO_COPY_NAME);
}

int32_t TestH2Client::OpenStreamWithNameFlags(const HeaderList &headers, bool endStream, uint8_t nameFlags)
{
    if (!session)
    {
        Fail("client session not created");
        return -1;
    }
    const std::vector<nghttp2_nv> nva = MakeNva(headers, nameFlags);
    const int32_t streamId = nghttp2_submit_headers(session.get(),
                                                    endStream ? NGHTTP2_FLAG_END_STREAM : NGHTTP2_FLAG_NONE, -1,
                                                    nullptr, nva.data(), nva.size(), nullptr);
    if (streamId < 0)
    {
        Fail(LibError("nghttp2_submit_headers", streamId));
        return -1;
    }
    StreamFor(streamId);
    if (!Flush())
    {
        return -1;
    }
    return streamId;
}

bool TestH2Client::SendData(int32_t streamId, std::string_view bytes, bool endStream)
{
    if (!session)
    {
        return Fail("client session not created");
    }
    if (pendingData.count(streamId) != 0)
    {
        return Fail("previous DATA on stream " + std::to_string(streamId) + " is still queued");
    }
    pendingData[streamId] = PendingData{std::string(bytes), 0};

    nghttp2_data_provider2 provider;
    provider.source.ptr = nullptr;
    provider.read_callback = &TestH2ClientCallbacks::ReadData;
    const int rv = nghttp2_submit_data2(session.get(), endStream ? NGHTTP2_FLAG_END_STREAM : NGHTTP2_FLAG_NONE,
                                        streamId, &provider);
    if (rv != 0)
    {
        pendingData.erase(streamId);
        return Fail(LibError("nghttp2_submit_data2", rv));
    }
    if (!Flush())
    {
        return false;
    }
    if (pendingData.count(streamId) != 0)
    {
        // 流控窗口不够,DATA 被推迟:脚本顺序就保证不了了,让用例显式失败而不是悄悄乱序。
        pendingData.erase(streamId);
        return Fail("DATA on stream " + std::to_string(streamId) + " was deferred by flow control");
    }
    return true;
}

bool TestH2Client::SendRstStream(int32_t streamId, uint32_t errorCode)
{
    if (!session)
    {
        return Fail("client session not created");
    }
    const int rv = nghttp2_submit_rst_stream(session.get(), NGHTTP2_FLAG_NONE, streamId, errorCode);
    if (rv != 0)
    {
        return Fail(LibError("nghttp2_submit_rst_stream", rv));
    }
    return Flush();
}

bool TestH2Client::SendSettings(const std::vector<SettingEntry> &entries)
{
    if (!session)
    {
        return Fail("client session not created");
    }
    std::vector<nghttp2_settings_entry> iv;
    iv.reserve(entries.size());
    for (const auto &entry : entries)
    {
        iv.push_back(nghttp2_settings_entry{entry.id, entry.value});
    }
    const int rv = nghttp2_submit_settings(session.get(), NGHTTP2_FLAG_NONE, iv.data(), iv.size());
    if (rv != 0)
    {
        return Fail(LibError("nghttp2_submit_settings", rv));
    }
    return Flush();
}

bool TestH2Client::SendPing()
{
    if (!session)
    {
        return Fail("client session not created");
    }
    const int rv = nghttp2_submit_ping(session.get(), NGHTTP2_FLAG_NONE, nullptr);
    if (rv != 0)
    {
        return Fail(LibError("nghttp2_submit_ping", rv));
    }
    return Flush();
}

bool TestH2Client::Flush()
{
    if (!session)
    {
        return Fail("client session not created");
    }
    for (;;)
    {
        const uint8_t *data = nullptr;
        const nghttp2_ssize produced = nghttp2_session_mem_send2(session.get(), &data);
        if (produced < 0)
        {
            return Fail(LibError("nghttp2_session_mem_send2", static_cast<int>(produced)));
        }
        if (produced == 0)
        {
            return true;
        }
        output.append(reinterpret_cast<const char *>(data), static_cast<size_t>(produced));
    }
}

bool TestH2Client::Fail(std::string message)
{
    lastError = std::move(message);
    return false;
}

ClientStream &TestH2Client::StreamFor(int32_t streamId)
{
    const auto [it, inserted] = streams.try_emplace(streamId);
    if (inserted)
    {
        it->second.streamId = streamId;
    }
    return it->second;
}

} // namespace muduo_grpc_test
