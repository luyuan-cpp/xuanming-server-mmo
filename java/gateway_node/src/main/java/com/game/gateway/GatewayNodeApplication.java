package com.game.gateway;

import com.game.startup.ClockCheck;
import org.springframework.boot.SpringApplication;
import org.springframework.boot.autoconfigure.SpringBootApplication;
import org.springframework.scheduling.annotation.EnableScheduling;

@SpringBootApplication
@EnableScheduling
public class GatewayNodeApplication {

    public static void main(String[] args) {
        ClockCheck.mustPass("gateway");
        SpringApplication.run(GatewayNodeApplication.class, args);
    }
}
