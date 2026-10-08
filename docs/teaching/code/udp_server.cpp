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

    sockaddr_in server_address{};
    if (!MakeIpv4Address("127.0.0.1", kUdpPort, server_address)) {
        std::cerr << "invalid server address\n";
        return 1;
    }

    if (bind(
            socket_handle.get(),
            reinterpret_cast<const sockaddr*>(&server_address),
            sizeof(server_address)) == SOCKET_ERROR) {
        PrintSocketError("bind");
        return 1;
    }

    std::cout << "UDP server bound to 127.0.0.1:" << kUdpPort << '\n';

    std::array<char, kUdpBufferSize> buffer{};
    sockaddr_in client_address{};
    int client_address_size = sizeof(client_address);
    const int received = recvfrom(
        socket_handle.get(),
        buffer.data(),
        static_cast<int>(buffer.size()),
        0,
        reinterpret_cast<sockaddr*>(&client_address),
        &client_address_size);
    if (received == SOCKET_ERROR) {
        PrintSocketError("recvfrom");
        return 1;
    }

    const std::string request(buffer.data(), received);
    std::cout << "request: " << request << '\n';

    const std::string response = "server received: " + request;
    const int sent = sendto(
        socket_handle.get(),
        response.data(),
        static_cast<int>(response.size()),
        0,
        reinterpret_cast<const sockaddr*>(&client_address),
        client_address_size);
    if (sent == SOCKET_ERROR) {
        PrintSocketError("sendto");
        return 1;
    }

    return 0;
}
