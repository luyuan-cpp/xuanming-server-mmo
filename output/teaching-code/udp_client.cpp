#include "socket_common.h"

#include <array>
#include <iostream>
#include <string>

int main() {
    using namespace socket_demo;

    WsaSession wsa;
    if (!wsa.ok()) {
        std::cerr << "WSAStartup failed, error=" << wsa.result() << '\n';
        return 1;
    }

    SocketHandle socket_handle(socket(AF_INET, SOCK_DGRAM, IPPROTO_UDP));
    if (!socket_handle) {
        PrintSocketError("socket");
        return 1;
    }

    if (!SetSocketTimeouts(socket_handle.get(), 3000)) {
        PrintSocketError("set UDP timeouts");
        return 1;
    }

    sockaddr_in server_address{};
    if (!MakeIpv4Address("127.0.0.1", kUdpPort, server_address)) {
        std::cerr << "invalid server address\n";
        return 1;
    }

    const std::string request = "hello over UDP";
    const int sent = sendto(
        socket_handle.get(),
        request.data(),
        static_cast<int>(request.size()),
        0,
        reinterpret_cast<const sockaddr*>(&server_address),
        sizeof(server_address));
    if (sent == SOCKET_ERROR) {
        PrintSocketError("sendto");
        return 1;
    }

    std::array<char, kUdpBufferSize> buffer{};
    sockaddr_in response_address{};
    int response_address_size = sizeof(response_address);
    const int received = recvfrom(
        socket_handle.get(),
        buffer.data(),
        static_cast<int>(buffer.size()),
        0,
        reinterpret_cast<sockaddr*>(&response_address),
        &response_address_size);
    if (received == SOCKET_ERROR) {
        PrintSocketError("recvfrom");
        return 1;
    }
    if (response_address.sin_family != AF_INET ||
        response_address.sin_port != server_address.sin_port ||
        response_address.sin_addr.s_addr != server_address.sin_addr.s_addr) {
        std::cerr << "ignored response from unexpected UDP endpoint\n";
        return 1;
    }

    std::cout << "response: " << std::string(buffer.data(), received) << '\n';
    return 0;
}
