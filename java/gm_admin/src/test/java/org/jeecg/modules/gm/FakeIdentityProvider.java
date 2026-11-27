package org.jeecg.modules.gm;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.nimbusds.jose.JOSEException;
import com.nimbusds.jose.JWSAlgorithm;
import com.nimbusds.jose.JWSHeader;
import com.nimbusds.jose.crypto.RSASSASigner;
import com.nimbusds.jose.jwk.JWKSet;
import com.nimbusds.jose.jwk.RSAKey;
import com.nimbusds.jwt.JWTClaimsSet;
import com.nimbusds.jwt.SignedJWT;
import com.sun.net.httpserver.HttpExchange;
import com.sun.net.httpserver.HttpServer;
import java.io.IOException;
import java.net.InetAddress;
import java.net.InetSocketAddress;
import java.net.http.HttpClient;
import java.nio.charset.StandardCharsets;
import java.security.KeyPair;
import java.security.KeyPairGenerator;
import java.security.NoSuchAlgorithmException;
import java.security.interfaces.RSAPrivateKey;
import java.security.interfaces.RSAPublicKey;
import java.time.Duration;
import java.util.Date;
import java.util.List;

/** 只在本机回环上应答的测试身份提供方:发现文档、JWKS、令牌端点三个地址,ID Token 由测试现签。 */
final class FakeIdentityProvider implements AutoCloseable {
    static final String CLIENT_ID = "gm-console";
    static final String CLIENT_SECRET = "client-secret-for-tests-only-0123456789";
    static final String REDIRECT_URI = "https://gm.example.test/login";
    static final String KEY_ID = "test-key";

    private final HttpServer server;
    private final KeyPair signingKey = newKey();
    /** 令牌端点下一次要返回的 ID Token;为 null 时回 400 invalid_grant。 */
    volatile String nextIdToken;
    /** 发现文档自报的发行方;测试可以改成别的值来模拟被指到另一家。 */
    volatile String advertisedIssuer;
    volatile String lastTokenRequestBody = "";
    volatile String lastTokenRequestAuthorization = "";
    volatile int tokenRequests;

    FakeIdentityProvider() throws IOException {
        server = HttpServer.create(new InetSocketAddress(InetAddress.getLoopbackAddress(), 0), 0);
        advertisedIssuer = issuer();
        server.createContext("/.well-known/openid-configuration", exchange -> reply(exchange, 200, new ObjectMapper().createObjectNode()
                .put("issuer", advertisedIssuer).put("authorization_endpoint", issuer() + "/authorize")
                .put("token_endpoint", issuer() + "/token").put("jwks_uri", issuer() + "/jwks").toString()));
        server.createContext("/jwks", exchange -> reply(exchange, 200,
                new JWKSet(new RSAKey.Builder((RSAPublicKey) signingKey.getPublic()).keyID(KEY_ID).build()).toString()));
        server.createContext("/token", exchange -> {
            tokenRequests++;
            lastTokenRequestBody = new String(exchange.getRequestBody().readAllBytes(), StandardCharsets.UTF_8);
            lastTokenRequestAuthorization = String.valueOf(exchange.getRequestHeaders().getFirst("Authorization"));
            String idToken = nextIdToken;
            if (idToken == null) reply(exchange, 400, "{\"error\":\"invalid_grant\"}");
            else reply(exchange, 200, new ObjectMapper().createObjectNode().put("id_token", idToken).put("token_type", "Bearer").toString());
        });
        server.start();
    }

    String issuer() { return "http://127.0.0.1:" + server.getAddress().getPort(); }

    GmOidcProvider provider() { return provider("sub"); }

    GmOidcProvider provider(String subjectClaim) {
        return new GmOidcProvider(issuer(), CLIENT_ID, CLIENT_SECRET, REDIRECT_URI, "openid profile", subjectClaim, "groups", "name",
                new ObjectMapper(), HttpClient.newBuilder().connectTimeout(Duration.ofSeconds(2)).build());
    }

    /** 一份各项都合格的声明;测试在它上面改一处来造不合格的令牌。 */
    JWTClaimsSet.Builder claims(String subject, String nonce) {
        long now = System.currentTimeMillis();
        return new JWTClaimsSet.Builder().issuer(issuer()).audience(CLIENT_ID).subject(subject).claim("nonce", nonce)
                .claim("name", "测试员").claim("groups", List.of("gm-admins"))
                .issueTime(new Date(now)).expirationTime(new Date(now + 300_000));
    }

    String sign(JWTClaimsSet claims) { return sign(claims, signingKey); }

    static String sign(JWTClaimsSet claims, KeyPair key) {
        try {
            SignedJWT token = new SignedJWT(new JWSHeader.Builder(JWSAlgorithm.RS256).keyID(KEY_ID).build(), claims);
            token.sign(new RSASSASigner((RSAPrivateKey) key.getPrivate()));
            return token.serialize();
        } catch (JOSEException failed) {
            throw new IllegalStateException(failed);
        }
    }

    static KeyPair newKey() {
        try {
            KeyPairGenerator generator = KeyPairGenerator.getInstance("RSA");
            generator.initialize(2048);
            return generator.generateKeyPair();
        } catch (NoSuchAlgorithmException impossible) {
            throw new IllegalStateException(impossible);
        }
    }

    private static void reply(HttpExchange exchange, int status, String body) throws IOException {
        byte[] bytes = body.getBytes(StandardCharsets.UTF_8);
        exchange.getResponseHeaders().add("Content-Type", "application/json");
        exchange.sendResponseHeaders(status, bytes.length);
        try (var out = exchange.getResponseBody()) { out.write(bytes); }
    }

    @Override public void close() { server.stop(0); }
}
