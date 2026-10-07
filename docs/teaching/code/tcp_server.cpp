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

    SocketHandle listen_socket(socket(AF_INET, SOCK_STREAM, IPPROTO_TCP));
    if (!listen_socket) {
        PrintSocketError("socket");
        return 1;
    }

    sockaddr_in server_address{};
    if (!MakeIpv4Address("127.0.0.1", kTcpPort, server_address)) {
        std::cerr << "invalid server address\n";
        return 1;
    }

    if (bind(
            listen_socket.get(),
            reinterpret_cast<const sockaddr*>(&server_address),
            sizeof(server_address)) == SOCKET_ERROR) {
        PrintSocketError("bind");
        return 1;
    }

    if (listen(listen_socket.get(), SOMAXCONN) == SOCKET_ERROR) {
        PrintSocketError("listen");
        return 1;
    }

    std::cout << "TCP server listening on 127.0.0.1:" << kTcpPort << '\n';

    sockaddr_in client_address{};
    int client_address_size = sizeof(client_address);
    SocketHandle client_socket(accept(
        listen_socket.get(),
        reinterpret_cast<sockaddr*>(&client_address),
        &client_address_size));
    if (!client_socket) {
        PrintSocketError("accept");
        return 1;
    }
    if (!SetSocketTimeouts(client_socket.get())) {
        PrintSocketError("set TCP timeouts");
        return 1;
    }

    std::string request;
    if (!ReceiveFrame(client_socket.get(), request)) {
        std::cerr << "failed to receive a complete TCP frame\n";
        return 1;
    }

    std::cout << "request: " << request << '\n';
    const std::string response = "server received: " + request;
    if (!SendFrame(client_socket.get(), response)) {
        PrintSocketError("send frame");
        return 1;
    }

    ShutdownSendAndDrain(client_socket);
    return 0;
}
