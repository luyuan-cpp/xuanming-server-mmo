package com.game.startup;

import java.io.InputStream;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.file.Files;
import java.nio.file.Path;
import java.security.KeyStore;
import java.security.cert.Certificate;
import java.security.cert.CertificateFactory;
import java.time.Duration;
import java.time.ZonedDateTime;
import java.time.format.DateTimeFormatter;
import javax.net.ssl.SSLContext;
import javax.net.ssl.TrustManagerFactory;

/**
 * 启动时核对本机时钟:和 Kubernetes API server 的时钟差 30 秒以上就拒绝启动。
 *
 * <p>规则与 Go 服务(go/shared/clockcheck)、C++ 节点(clock_check.h)相同,开关同为 MMORPG_CLOCK_CHECK_MODE;
 * 为什么要这一道、运维怎么处置见 docs/ops/clock-sync-runbook.md。参照是 API server 回应里的 Date 头
 * (GET /version 不要任何权限,回应状态码不论)。不在 Kubernetes 里不核;参照读不到只告警、照常启动:拿不到参照不等于时钟错了。
 */
public final class ClockCheck {

    /** 允许的最大偏差。Date 头只到秒,加上往返,测量误差在两秒以内;30 秒只拦明显错的时钟。 */
    static final long MAX_OFFSET_MILLIS = 30_000;

    /** 设成 warn 时超限只告警、照常启动(API server 自己的时钟错了时的应急开关);其余取值都按拒启。 */
    static final String MODE_ENV = "MMORPG_CLOCK_CHECK_MODE";

    private static final String CLUSTER_CA = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt";
    private static final Duration TIMEOUT = Duration.ofSeconds(3);

    private ClockCheck() {
    }

    /** 偏差超限时向 stderr 写一行原因并以 1 退出,其余情形写一行结果。在 SpringApplication.run 之前调。 */
    public static void mustPass(String service) {
        String host = System.getenv("KUBERNETES_SERVICE_HOST");
        String port = System.getenv("KUBERNETES_SERVICE_PORT");
        if (host == null || host.isEmpty() || port == null || port.isEmpty()) {
            return; // 不在 Kubernetes 里:没有参照,不核
        }
        long offset;
        try {
            offset = measure(host, port);
        } catch (Exception e) {
            System.err.println("clock check skipped: cannot read the Kubernetes API server clock: " + e);
            return;
        }
        String refusal = judge(offset, System.getenv(MODE_ENV));
        if (refusal != null) {
            System.err.println(service + " refuses to start: " + refusal);
            System.exit(1);
        }
    }

    private static long measure(String host, String port) throws Exception {
        HttpClient client = HttpClient.newBuilder().sslContext(clusterContext()).connectTimeout(TIMEOUT).build();
        String authority = host.contains(":") ? "[" + host + "]:" + port : host + ":" + port;
        HttpRequest request = HttpRequest.newBuilder(URI.create("https://" + authority + "/version")).timeout(TIMEOUT).GET().build();
        long sent = System.currentTimeMillis();
        HttpResponse<Void> response = client.send(request, HttpResponse.BodyHandlers.discarding());
        long received = System.currentTimeMillis();
        String date = response.headers().firstValue("date").orElseThrow(() -> new IllegalStateException("no Date header"));
        long dateSeconds = ZonedDateTime.parse(date, DateTimeFormatter.RFC_1123_DATE_TIME).toEpochSecond();
        return offsetMillis(sent, received, dateSeconds);
    }

    /** 本机时钟减参照时钟(毫秒)。参照是回应的 Date 头,只到秒:本机取请求前后的中点,参照取那一秒的中点。 */
    static long offsetMillis(long sentMillis, long receivedMillis, long dateSeconds) {
        return sentMillis + (receivedMillis - sentMillis) / 2 - (dateSeconds * 1000 + 500);
    }

    /** 能启动返回 null 并写一行结果,要拒启返回原因。超限时 mode 为 warn 只告警,其余取值都拒启。 */
    static String judge(long offsetMillis, String mode) {
        String measured = "the local clock is " + offsetMillis + " ms off the Kubernetes API server clock";
        if (offsetMillis >= -MAX_OFFSET_MILLIS && offsetMillis <= MAX_OFFSET_MILLIS) {
            System.err.println("clock check passed: " + measured);
            return null;
        }
        String reason = measured + ", more than the " + MAX_OFFSET_MILLIS + " ms allowed; fix the node clock "
                + "(docs/ops/clock-sync-runbook.md), or set " + MODE_ENV + "=warn to start anyway";
        if ("warn".equals(mode)) {
            System.err.println("clock check failed but " + MODE_ENV + "=warn: " + reason);
            return null;
        }
        return reason;
    }

    private static SSLContext clusterContext() throws Exception {
        KeyStore trust = KeyStore.getInstance(KeyStore.getDefaultType());
        trust.load(null, null);
        try (InputStream in = Files.newInputStream(Path.of(CLUSTER_CA))) {
            int n = 0;
            for (Certificate cert : CertificateFactory.getInstance("X.509").generateCertificates(in)) {
                trust.setCertificateEntry("cluster-ca-" + n++, cert);
            }
        }
        TrustManagerFactory factory = TrustManagerFactory.getInstance(TrustManagerFactory.getDefaultAlgorithm());
        factory.init(trust);
        SSLContext context = SSLContext.getInstance("TLS");
        context.init(null, factory.getTrustManagers(), null);
        return context;
    }
}
