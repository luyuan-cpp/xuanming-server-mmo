#include "core/tracing/otlp_http.h"

#include <cctype>
#include <limits>

namespace tracing {

namespace {

struct ParsedUrl {
    std::string host;
    uint16_t port{kOtlpHttpDefaultPort};
    std::string path;   // 原样:可能为空
};

char ToLowerAscii(char c) { return (c >= 'A' && c <= 'Z') ? static_cast<char>(c - 'A' + 'a') : c; }

bool EqualsIgnoreCase(std::string_view a, std::string_view b)
{
    if (a.size() != b.size()) {
        return false;
    }
    for (size_t i = 0; i < a.size(); ++i) {
        if (ToLowerAscii(a[i]) != ToLowerAscii(b[i])) {
            return false;
        }
    }
    return true;
}

bool StartsWithIgnoreCase(std::string_view text, std::string_view prefix)
{
    return text.size() >= prefix.size() && EqualsIgnoreCase(text.substr(0, prefix.size()), prefix);
}

std::string_view TrimOws(std::string_view text)
{
    while (!text.empty() && (text.front() == ' ' || text.front() == '\t')) {
        text.remove_prefix(1);
    }
    while (!text.empty() && (text.back() == ' ' || text.back() == '\t')) {
        text.remove_suffix(1);
    }
    return text;
}

bool IsHostChar(char c)
{
    return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '.';
}

// 规范十进制端口:1..65535,不带前导零。
std::optional<uint16_t> ParsePort(std::string_view digits)
{
    if (digits.empty() || digits.size() > 5 || digits.front() == '0') {
        return std::nullopt;
    }
    uint32_t value = 0;
    for (const char c : digits) {
        if (c < '0' || c > '9') {
            return std::nullopt;
        }
        value = value * 10 + static_cast<uint32_t>(c - '0');
    }
    if (value == 0 || value > 65535) {
        return std::nullopt;
    }
    return static_cast<uint16_t>(value);
}

std::optional<ParsedUrl> ParseHttpUrl(std::string_view url)
{
    constexpr std::string_view kScheme = "http://";
    if (!StartsWithIgnoreCase(url, kScheme)) {
        return std::nullopt;   // https 与别的 scheme 都不认
    }
    for (const char c : url) {
        // 查询串、片段、空白与控制字符一律不认:导出地址用不上它们,认了反而要决定怎么转义。
        if (c == '?' || c == '#' || static_cast<unsigned char>(c) <= 0x20 || static_cast<unsigned char>(c) >= 0x7F) {
            return std::nullopt;
        }
    }
    std::string_view rest = url.substr(kScheme.size());
    const size_t pathStart = rest.find('/');
    const std::string_view authority = rest.substr(0, pathStart);
    if (authority.empty() || authority.find('@') != std::string_view::npos || authority.front() == '[') {
        return std::nullopt;   // 没有主机、带凭据、IPv6 字面量
    }

    ParsedUrl parsed;
    std::string_view host = authority;
    if (const size_t colon = authority.find(':'); colon != std::string_view::npos) {
        host = authority.substr(0, colon);
        const auto port = ParsePort(authority.substr(colon + 1));
        if (!port) {
            return std::nullopt;
        }
        parsed.port = *port;
    }
    if (host.empty() || host.front() == '.' || host.front() == '-') {
        return std::nullopt;
    }
    for (const char c : host) {
        if (!IsHostChar(c)) {
            return std::nullopt;
        }
    }
    parsed.host.assign(host.data(), host.size());
    if (pathStart != std::string_view::npos) {
        const std::string_view path = rest.substr(pathStart);
        parsed.path.assign(path.data(), path.size());
    }
    return parsed;
}

} // namespace

std::string OtlpHttpEndpoint::ToUrl() const
{
    return "http://" + host + ":" + std::to_string(port) + path;
}

std::optional<OtlpHttpEndpoint> ParseOtlpTracesEndpoint(std::string_view url)
{
    auto parsed = ParseHttpUrl(url);
    if (!parsed) {
        return std::nullopt;
    }
    OtlpHttpEndpoint endpoint;
    endpoint.host = std::move(parsed->host);
    endpoint.port = parsed->port;
    endpoint.path = parsed->path.empty() ? std::string("/") : std::move(parsed->path);
    return endpoint;
}

std::optional<OtlpHttpEndpoint> ParseOtlpBaseEndpoint(std::string_view url)
{
    auto parsed = ParseHttpUrl(url);
    if (!parsed) {
        return std::nullopt;
    }
    OtlpHttpEndpoint endpoint;
    endpoint.host = std::move(parsed->host);
    endpoint.port = parsed->port;
    std::string base = std::move(parsed->path);
    if (base.empty() || base.back() != '/') {
        base.push_back('/');
    }
    endpoint.path = base + std::string(kOtlpTracesPath.substr(1));
    return endpoint;
}

OtlpExportSettings ResolveOtlpExportSettings(std::string_view configEndpoint, const EnvironmentLookup& environment)
{
    const auto get = [&environment](const char* name) -> std::string_view {
        const char* value = environment ? environment(name) : nullptr;
        return value != nullptr ? std::string_view(value) : std::string_view();
    };
    OtlpExportSettings settings;

    if (EqualsIgnoreCase(TrimOws(get("OTEL_SDK_DISABLED")), "true")) {
        settings.source = "OTEL_SDK_DISABLED";
        return settings;
    }
    if (const std::string_view exporter = TrimOws(get("OTEL_TRACES_EXPORTER")); !exporter.empty()) {
        if (EqualsIgnoreCase(exporter, "none")) {
            settings.source = "OTEL_TRACES_EXPORTER";
            return settings;
        }
        if (!EqualsIgnoreCase(exporter, "otlp")) {
            settings.error = "OTEL_TRACES_EXPORTER must be otlp or none";
            return settings;
        }
    }
    std::string_view protocol = TrimOws(get("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"));
    const char* protocolName = "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL";
    if (protocol.empty()) {
        protocol = TrimOws(get("OTEL_EXPORTER_OTLP_PROTOCOL"));
        protocolName = "OTEL_EXPORTER_OTLP_PROTOCOL";
    }
    if (!protocol.empty() && protocol != "http/protobuf") {
        settings.error = std::string(protocolName) + " must be http/protobuf (the only OTLP transport of the C++ exporter)";
        return settings;
    }

    // 报错不回显地址:地址里可能带着本不该出现的凭据。
    const auto take = [&settings](std::optional<OtlpHttpEndpoint> endpoint, const char* source) {
        settings.source = source;
        if (!endpoint) {
            settings.error = std::string(source) +
                             " must be an http://host[:port][/path] URL without credentials, query or fragment";
            return settings;
        }
        settings.endpoint = std::move(*endpoint);
        settings.enabled = true;
        return settings;
    };
    if (const std::string_view traces = get("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"); !traces.empty()) {
        return take(ParseOtlpTracesEndpoint(traces), "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT");
    }
    if (const std::string_view base = get("OTEL_EXPORTER_OTLP_ENDPOINT"); !base.empty()) {
        return take(ParseOtlpBaseEndpoint(base), "OTEL_EXPORTER_OTLP_ENDPOINT");
    }
    if (!configEndpoint.empty()) {
        return take(ParseOtlpTracesEndpoint(configEndpoint), "Tracing.OtlpTracesEndpoint");
    }
    return settings;
}

std::string BuildOtlpHttpRequest(const OtlpHttpEndpoint& endpoint, std::string_view body)
{
    std::string request;
    request.reserve(160 + endpoint.host.size() + endpoint.path.size() + body.size());
    request.append("POST ").append(endpoint.path).append(" HTTP/1.1\r\n");
    request.append("Host: ").append(endpoint.host);
    if (endpoint.port != 80) {
        request.append(":").append(std::to_string(endpoint.port));
    }
    request.append("\r\n");
    request.append("User-Agent: mmorpg-cpp-otlp/1.0\r\n");
    request.append("Content-Type: application/x-protobuf\r\n");
    request.append("Content-Length: ").append(std::to_string(body.size())).append("\r\n");
    request.append("\r\n");
    request.append(body.data(), body.size());
    return request;
}

namespace {

// 十进制非负整数;溢出或非数字返回 nullopt。
std::optional<size_t> ParseDecimalSize(std::string_view digits)
{
    if (digits.empty()) {
        return std::nullopt;
    }
    size_t value = 0;
    for (const char c : digits) {
        if (c < '0' || c > '9') {
            return std::nullopt;
        }
        const size_t digit = static_cast<size_t>(c - '0');
        if (value > (std::numeric_limits<size_t>::max() - digit) / 10) {
            return std::nullopt;
        }
        value = value * 10 + digit;
    }
    return value;
}

int HexDigit(char c)
{
    if (c >= '0' && c <= '9') return c - '0';
    if (c >= 'a' && c <= 'f') return c - 'a' + 10;
    if (c >= 'A' && c <= 'F') return c - 'A' + 10;
    return -1;
}

// 逗号分隔的头值里有没有某个记号(不区分大小写)。
bool HeaderHasToken(std::string_view value, std::string_view token)
{
    while (!value.empty()) {
        const size_t comma = value.find(',');
        if (EqualsIgnoreCase(TrimOws(value.substr(0, comma)), token)) {
            return true;
        }
        if (comma == std::string_view::npos) {
            break;
        }
        value.remove_prefix(comma + 1);
    }
    return false;
}

// 逗号分隔的最后一个记号是不是 chunked(RFC 9112:chunked 只能是最后一个传输编码)。
bool LastTokenIsChunked(std::string_view value)
{
    const size_t comma = value.rfind(',');
    const std::string_view last = comma == std::string_view::npos ? value : value.substr(comma + 1);
    return EqualsIgnoreCase(TrimOws(last), "chunked");
}

// 从 start 起解析 chunked 正文。返回 nullopt = 还不完整;-1(以 SIZE_MAX 表示)= 格式错误;否则是正文结束后的偏移。
constexpr size_t kChunkedError = std::numeric_limits<size_t>::max();

std::optional<size_t> ParseChunkedBody(std::string_view data, size_t start)
{
    size_t offset = start;
    for (;;) {
        const size_t lineEnd = data.find("\r\n", offset);
        if (lineEnd == std::string_view::npos) {
            return data.size() - offset > 1024 ? std::optional<size_t>(kChunkedError) : std::nullopt;
        }
        std::string_view sizeLine = data.substr(offset, lineEnd - offset);
        if (const size_t semicolon = sizeLine.find(';'); semicolon != std::string_view::npos) {
            sizeLine = sizeLine.substr(0, semicolon);   // 块扩展不认,跳过
        }
        sizeLine = TrimOws(sizeLine);
        if (sizeLine.empty() || sizeLine.size() > 15) {
            return kChunkedError;
        }
        size_t chunkSize = 0;
        for (const char c : sizeLine) {
            const int digit = HexDigit(c);
            if (digit < 0) {
                return kChunkedError;
            }
            chunkSize = chunkSize * 16 + static_cast<size_t>(digit);
        }
        offset = lineEnd + 2;
        if (chunkSize == 0) {
            // 最后一块之后是零到多行尾部字段,以一个空行结束。
            for (;;) {
                const size_t trailerEnd = data.find("\r\n", offset);
                if (trailerEnd == std::string_view::npos) {
                    return std::nullopt;
                }
                const bool emptyLine = trailerEnd == offset;
                offset = trailerEnd + 2;
                if (emptyLine) {
                    return offset;
                }
            }
        }
        if (data.size() - offset < chunkSize + 2) {
            return std::nullopt;
        }
        if (data.substr(offset + chunkSize, 2) != "\r\n") {
            return kChunkedError;
        }
        offset += chunkSize + 2;
    }
}

} // namespace

HttpResponseParse ParseHttpResponse(std::string_view data)
{
    HttpResponseParse result;
    size_t offset = 0;
    for (;;) {
        const size_t headerEnd = data.find("\r\n\r\n", offset);
        if (headerEnd == std::string_view::npos) {
            if (data.size() - offset > kMaxHttpResponseHeaderBytes) {
                result.state = HttpResponseParse::State::kError;
            }
            return result;
        }
        if (headerEnd - offset > kMaxHttpResponseHeaderBytes) {
            result.state = HttpResponseParse::State::kError;
            return result;
        }
        const std::string_view head = data.substr(offset, headerEnd - offset);
        const size_t bodyStart = headerEnd + 4;

        // 状态行:HTTP/1.x SSS[ 原因]
        const size_t statusLineEnd = head.find("\r\n");
        const std::string_view statusLine = head.substr(0, statusLineEnd);
        if (statusLine.size() < 12 || statusLine.substr(0, 7) != "HTTP/1." ||
            (statusLine[7] != '0' && statusLine[7] != '1') || statusLine[8] != ' ' ||
            (statusLine.size() > 12 && statusLine[12] != ' ')) {
            result.state = HttpResponseParse::State::kError;
            return result;
        }
        int status = 0;
        for (size_t i = 9; i < 12; ++i) {
            if (statusLine[i] < '0' || statusLine[i] > '9') {
                result.state = HttpResponseParse::State::kError;
                return result;
            }
            status = status * 10 + (statusLine[i] - '0');
        }
        const bool http11 = statusLine[7] == '1';

        std::optional<size_t> contentLength;
        bool chunked = false;
        bool connectionClose = false;
        bool connectionKeepAlive = false;
        std::string_view headers = statusLineEnd == std::string_view::npos ? std::string_view() : head.substr(statusLineEnd + 2);
        while (!headers.empty()) {
            const size_t lineEnd = headers.find("\r\n");
            const std::string_view line = headers.substr(0, lineEnd);
            headers = lineEnd == std::string_view::npos ? std::string_view() : headers.substr(lineEnd + 2);
            const size_t colon = line.find(':');
            if (colon == std::string_view::npos || colon == 0) {
                result.state = HttpResponseParse::State::kError;
                return result;
            }
            const std::string_view name = line.substr(0, colon);
            const std::string_view value = TrimOws(line.substr(colon + 1));
            if (EqualsIgnoreCase(name, "Content-Length")) {
                const auto length = ParseDecimalSize(value);
                if (!length || (contentLength && *contentLength != *length)) {
                    result.state = HttpResponseParse::State::kError;
                    return result;
                }
                contentLength = length;
            } else if (EqualsIgnoreCase(name, "Transfer-Encoding")) {
                chunked = LastTokenIsChunked(value);
            } else if (EqualsIgnoreCase(name, "Connection")) {
                connectionClose = connectionClose || HeaderHasToken(value, "close");
                connectionKeepAlive = connectionKeepAlive || HeaderHasToken(value, "keep-alive");
            }
        }

        if (status >= 100 && status < 200) {
            if (status == 101) {
                result.state = HttpResponseParse::State::kError;   // 没有请求升级
                return result;
            }
            offset = bodyStart;   // 临时响应,接着解析真正的那条
            continue;
        }

        result.headersComplete = true;
        result.statusCode = status;
        result.keepAlive = http11 ? !connectionClose : (connectionKeepAlive && !connectionClose);
        if (status == 204 || status == 304) {
            result.state = HttpResponseParse::State::kComplete;
            result.consumed = bodyStart;
            return result;
        }
        if (chunked) {
            const auto end = ParseChunkedBody(data, bodyStart);
            if (!end) {
                return result;
            }
            if (*end == kChunkedError) {
                result.state = HttpResponseParse::State::kError;
                return result;
            }
            result.state = HttpResponseParse::State::kComplete;
            result.consumed = *end;
            return result;
        }
        if (contentLength) {
            if (data.size() - bodyStart < *contentLength) {
                return result;
            }
            result.state = HttpResponseParse::State::kComplete;
            result.consumed = bodyStart + *contentLength;
            return result;
        }
        // 既没长度也不是 chunked:正文到连接关闭为止,连接不能复用。
        result.bodyUntilClose = true;
        result.keepAlive = false;
        return result;
    }
}

} // namespace tracing
