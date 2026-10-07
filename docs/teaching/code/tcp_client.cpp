#include "socket_common.h"

#include <iostream>
#include <string>

int main() {
    using namespace socket_demo;

    WsaSession wsa;
    if (!wsa.ok()) {
        std::cerr << "WSAStartup failed, error=" << wsa.result() << '\n';
        return 1;
    }

    SocketHandle socket_handle(socket(AF_INET, SOCK_STREAM, IPPROTO_TCP));
    if (!socket_handle) {
        PrintSocketError("socket");
        return 1;
    }
    if (!SetSocketTimeouts(socket_handle.get())) {
        PrintSocketError("set TCP timeouts");
        return 1;
    }

    sockaddr_in server_address{};
    if (!MakeIpv4Address("127.0.0.1", kTcpPort, server_address)) {
        std::cerr << "invalid server address\n";
        return 1;
    }

    if (connect(
            socket_handle.get(),
            reinterpret_cast<const sockaddr*>(&server_address),
            sizeof(server_address)) == SOCKET_ERROR) {
        PrintSocketError("connect");
        return 1;
    }

    const std::string request = "hello over TCP";
    if (!SendFrame(socket_handle.get(), request)) {
        PrintSocketError("send frame");
        return 1;
    }

    std::string response;
    if (!ReceiveFrame(socket_handle.get(), response)) {
        std::cerr << "failed to receive a complete TCP frame\n";
        return 1;
    }

    std::cout << "response: " << response << '\n';
    ShutdownSendAndDrain(socket_handle);
    return 0;
}
