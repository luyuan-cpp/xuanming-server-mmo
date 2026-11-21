package com.game.mall;

import com.game.startup.ClockCheck;
import org.springframework.boot.SpringApplication;
import org.springframework.boot.autoconfigure.SpringBootApplication;
import org.springframework.boot.context.properties.EnableConfigurationProperties;
import org.springframework.scheduling.annotation.EnableScheduling;

@SpringBootApplication
@EnableScheduling
@EnableConfigurationProperties(CommerceProperties.class)
public class MallGameApplication {
    public static void main(String[] args) {
        ClockCheck.mustPass("mall");
        SpringApplication.run(MallGameApplication.class, args);
    }
}
