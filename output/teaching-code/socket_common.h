#pragma once

#ifndef WIN32_LEAN_AND_MEAN
#define WIN32_LEAN_AND_MEAN
#endif

#include <winsock2.h>
#include <ws2tcpip.h>

#include <cstdint>
#include <iostream>
#include <string>

#pragma comment(lib, "Ws2_32.lib")

namespace socket_demo {

constexpr std::uint16_t kTcpPort = 9000;
constexpr std::uint16_t kUdpPort = 9001;
constexpr std::uint32_t kMaxTcpMessageSize = 1024 * 1024;
constexpr int kUdpBufferSize = 1200;
constexpr DWORD kIoTimeoutMs = 5000;

class WsaSession {
public:
    WsaSession() : result_(WSAStartup(MAKEWORD(2, 2), &data_)), started_(result_ == 0) {}

    ~WsaSession() {
        if (started_) {
            WSACleanup();
        }
    }

    WsaSession(const WsaSession&) = delete;
    WsaSession& operator=(const WsaSession&) = delete;

    bool ok() const { return started_; }
    int result() const { return result_; }

private:
    WSADATA data_{};
    int result_ = 0;
    bool started_ = false;
};

class SocketHandle {
public:
    explicit SocketHandle(SOCKET socket = INVALID_SOCKET) : socket_(socket) {}

    ~SocketHandle() {
        reset();
    }

    SocketHandle(const SocketHandle&) = delete;
    SocketHandle& operator=(const SocketHandle&) = delete;

    SocketHandle(SocketHandle&& other) noexcept : socket_(other.socket_) {
        other.socket_ = INVALID_SOCKET;
    }

    SocketHandle& operator=(SocketHandle&& other) noexcept {
        if (this != &other) {
            reset();
            socket_ = other.socket_;
            other.socket_ = INVALID_SOCKET;
        }
        return *this;
    }

    SOCKET get() const { return socket_; }
    explicit operator bool() const { return socket_ != INVALID_SOCKET; }

    void reset(SOCKET socket = INVALID_SOCKET) {
        if (socket_ != INVALID_SOCKET) {
            closesocket(socket_);
        }
        socket_ = socket;
    }

private:
    SOCKET socket_ = INVALID_SOCKET;
};

inline void PrintSocketError(const char* operation) {
    std::cerr << operation << " failed, WSA error=" << WSAGetLastError() << '\n';
}

inline bool SetSocketTimeouts(SOCKET socket, DWORD timeout_ms = kIoTimeoutMs) {
    return setsockopt(
               socket,
               SOL_SOCKET,
               SO_RCVTIMEO,
               reinterpret_cast<const char*>(&timeout_ms),
               sizeof(timeout_ms)) != SOCKET_ERROR &&
           setsockopt(
               socket,
               SOL_SOCKET,
               SO_SNDTIMEO,
               reinterpret_cast<const char*>(&timeout_ms),
               sizeof(timeout_ms)) != SOCKET_ERROR;
}

inline bool MakeIpv4Address(const char* ip, std::uint16_t port, sockaddr_in& address) {
    address = {};
    address.sin_family = AF_INET;
    address.sin_port = htons(port);
    return InetPtonA(AF_INET, ip, &address.sin_addr) == 1;
}

inline bool SendAll(SOCKET socket, const char* data, int size) {
    int total = 0;
    while (total < size) {
        const int sent = send(socket, data + total, size - total, 0);
        if (sent == SOCKET_ERROR || sent == 0) {
            return false;
        }
        total += sent;
    }
    return true;
}

inline bool ReceiveAll(SOCKET socket, char* data, int size) {
    int total = 0;
    while (total < size) {
        const int received = recv(socket, data + total, size - total, 0);
        if (received == SOCKET_ERROR || received == 0) {
            return false;
        }
        total += received;
    }
    return true;
}

inline bool SendFrame(SOCKET socket, const std::string& message) {
    if (message.size() > kMaxTcpMessageSize) {
        return false;
    }

    const auto length = static_cast<std::uint32_t>(message.size());
    const std::uint32_t network_length = htonl(length);
    return SendAll(
               socket,
               reinterpret_cast<const char*>(&network_length),
               static_cast<int>(sizeof(network_length))) &&
           SendAll(socket, message.data(), static_cast<int>(message.size()));
}

inline bool ReceiveFrame(SOCKET socket, std::string& message) {
    std::uint32_t network_length = 0;
    if (!ReceiveAll(
            socket,
            reinterpret_cast<char*>(&network_length),
            static_cast<int>(sizeof(network_length)))) {
        return false;
    }

    const std::uint32_t length = ntohl(network_length);
    if (length > kMaxTcpMessageSize) {
        std::cerr << "message too large: " << length << '\n';
        return false;
    }

    message.resize(length);
    return length == 0 || ReceiveAll(socket, message.data(), static_cast<int>(length));
}

inline void ShutdownSendAndDrain(SocketHandle& socket) {
    if (!socket) {
        return;
    }

    shutdown(socket.get(), SD_SEND);
    char buffer[256];
    while (recv(socket.get(), buffer, sizeof(buffer), 0) > 0) {
    }
    socket.reset();
}

}
