package com.game.gateway.config;

import org.junit.jupiter.api.Test;
import org.springframework.beans.factory.config.YamlPropertiesFactoryBean;
import org.springframework.core.io.ClassPathResource;
import org.springframework.core.io.Resource;
import org.springframework.core.io.support.PathMatchingResourcePatternResolver;
import org.springframework.core.type.MethodMetadata;
import org.springframework.core.type.classreading.CachingMetadataReaderFactory;
import org.springframework.core.type.classreading.MetadataReader;
import org.springframework.scheduling.annotation.Scheduled;

import java.io.IOException;
import java.util.Properties;
import java.util.Set;
import java.util.TreeSet;

import static org.junit.jupiter.api.Assertions.assertNotNull;
import static org.junit.jupiter.api.Assertions.assertTrue;

/**
 * 后台定时任务的线程数不少于任务数(2026-11-12,服务器缺口清单第 29 条收尾)。
 *
 * <p>Spring 缺省只有 1 个调度线程,全部 {@code @Scheduled} 任务排队跑。网关的定时任务各自阻塞在不同的依赖上
 * (etcd、MySQL、login 的 gRPC,超时都是秒级),共用一个线程时,一个依赖卡住会拖住其余任务。其中健康汇总的发布者
 * ({@code service.ZoneHealthPublisher})每拍续一次 etcd 租约,被拖过租约时长,它写的记录就消失,全部网关把这个区显示成「不知道」。
 * 所以 {@code application.yaml} 把 {@code spring.task.scheduling.pool.size} 设成任务数;这里钉住:以后加了定时任务却没加线程,
 * 本用例先红。
 *
 * <p>只读编译产物的注解元数据与主配置文本,不启动 Spring 上下文。同一个任务本来就不会与自己并发(调度器保证),
 * 线程数只决定不同任务之间要不要互相等。
 */
class ScheduledTaskThreadsTest {

    private static final String POOL_SIZE_KEY = "spring.task.scheduling.pool.size";

    @Test
    void schedulerHasAThreadForEveryScheduledTask() throws IOException {
        Set<String> tasks = scheduledMethods();
        assertTrue(tasks.contains("com.game.gateway.service.ZoneHealthPublisher#publish"),
                "扫描应当找到健康汇总发布者,实际找到:" + tasks);

        YamlPropertiesFactoryBean yaml = new YamlPropertiesFactoryBean();
        yaml.setResources(new ClassPathResource("application.yaml"));
        Properties properties = yaml.getObject();
        assertNotNull(properties, "application.yaml 应当能读到");
        String configured = properties.getProperty(POOL_SIZE_KEY);
        assertNotNull(configured, "application.yaml 要写 " + POOL_SIZE_KEY + ":缺省是 1,全部定时任务会互相排队");

        int poolSize = Integer.parseInt(configured.trim());
        assertTrue(poolSize >= tasks.size(),
                POOL_SIZE_KEY + " = " + poolSize + ",但有 " + tasks.size() + " 个定时任务:" + tasks
                        + "。加定时任务时同步加大线程数,否则任务之间互相排队,健康汇总的续租会被别的任务拖住");
    }

    /** com.game.gateway 下全部带 {@code @Scheduled} 的方法,形如 {@code 类全名#方法名}。 */
    private static Set<String> scheduledMethods() throws IOException {
        PathMatchingResourcePatternResolver resolver = new PathMatchingResourcePatternResolver();
        CachingMetadataReaderFactory readers = new CachingMetadataReaderFactory(resolver);
        Set<String> methods = new TreeSet<>();
        for (Resource resource : resolver.getResources("classpath*:com/game/gateway/**/*.class")) {
            MetadataReader reader = readers.getMetadataReader(resource);
            for (MethodMetadata method : reader.getAnnotationMetadata().getAnnotatedMethods(Scheduled.class.getName())) {
                methods.add(method.getDeclaringClassName() + "#" + method.getMethodName());
            }
        }
        return methods;
    }
}
