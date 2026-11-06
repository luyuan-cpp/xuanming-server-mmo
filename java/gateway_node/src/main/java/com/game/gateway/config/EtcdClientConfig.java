package com.game.gateway.config;

import io.etcd.jetcd.Client;
import jakarta.annotation.PreDestroy;
import org.springframework.context.annotation.Bean;
import org.springframework.context.annotation.Configuration;

import java.time.Duration;

@Configuration
public class EtcdClientConfig {

    private Client client;

    @Bean
    public Client etcdClient(EtcdProperties props) {
        // connectTimeout 取 etcd.dial-timeout-ms:配置项早就有,之前没传给客户端,建连超时一直是 jetcd 的缺省值
        // (docs/design/infra-reconnect-overview.md §4 R19)。每次查询另有 gate.discovery-timeout-ms 的截止。
        client = Client.builder()
                .endpoints(props.getEndpoints().toArray(String[]::new))
                .connectTimeout(Duration.ofMillis(props.getDialTimeoutMs()))
                .build();
        return client;
    }

    @PreDestroy
    public void close() {
        if (client != null) {
            client.close();
        }
    }
}
