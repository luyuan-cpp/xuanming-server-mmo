package org.jeecg.modules.gm;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.sun.net.httpserver.HttpServer;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.List;
import org.apache.shiro.authz.UnauthorizedException;
import org.apache.shiro.subject.Subject;
import org.apache.shiro.util.ThreadContext;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.BeforeEach;
import org.junit.jupiter.api.Test;
import org.springframework.web.server.ResponseStatusException;
import static org.junit.jupiter.api.Assertions.*;
import static org.mockito.Mockito.*;

/** 网关管理面写操作只经后台转发:白名单之外不转,操作人取登录会话,网关口令不出服务端(设计 §8 批 6)。 */
class GmGatewayAdminTest {
    private static final String KEY = "gateway-admin-test-only-key-00000000001";
    private final ObjectMapper json = new ObjectMapper();
    private final Subject subject = mock(Subject.class);
    private final List<String> seen = new ArrayList<>();
    private HttpServer gateway;
    private GmController controller;

    public static final class Principal { public String getUsername() { return "alice@corp"; } }

    @BeforeEach void setup() throws Exception {
        ThreadContext.bind(subject);
        when(subject.isAuthenticated()).thenReturn(true);
        when(subject.getPrincipal()).thenReturn(new Principal());
        gateway = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        gateway.createContext("/admin/", exchange -> {
            String body = new String(exchange.getRequestBody().readAllBytes(), StandardCharsets.UTF_8);
            seen.add(exchange.getRequestMethod() + " " + exchange.getRequestURI() + " key=" + exchange.getRequestHeaders().getFirst("X-Admin-Key")
                    + " operator=" + exchange.getRequestHeaders().getFirst("X-Admin-Operator") + " body=" + body);
            byte[] reply = "{\"zoneId\":7}".getBytes(StandardCharsets.UTF_8);
            exchange.sendResponseHeaders(200, reply.length);
            exchange.getResponseBody().write(reply);
            exchange.close();
        });
        gateway.start();
        controller = new GmController(json, mock(GameServices.class), mock(GmCommandJournal.class),
                "http://127.0.0.1:" + gateway.getAddress().getPort(), KEY, "", "");
    }

    @AfterEach void clear() {
        gateway.stop(0);
        ThreadContext.unbindSubject();
    }

    @Test void anAllowedWriteCarriesTheSessionOperatorAndTheServerSideKey() throws Exception {
        String reply = controller.gatewayAdmin("{\"method\":\"POST\",\"path\":\"/admin/zones/7/maintenance\",\"body\":{\"maintenanceMsg\":\"维护\"}}");
        assertEquals(200, json.readTree(reply).path("result").path("status").asInt());
        assertEquals(7, json.readTree(reply).path("result").path("body").path("zoneId").asInt());
        assertEquals(List.of("POST /admin/zones/7/maintenance key=" + KEY + " operator=alice@corp body={\"maintenanceMsg\":\"维护\"}"), seen);
        verify(subject).checkPermission(GmController.GATEWAY_WRITE);
    }

    @Test void routesOutsideTheWhitelistAndMissingPermissionAreRefusedBeforeAnyCall() {
        for (String rejected : List.of(
                "{\"method\":\"POST\",\"path\":\"/admin/tables/pointer\"}",
                "{\"method\":\"GET\",\"path\":\"/admin/zones\"}",
                "{\"method\":\"POST\",\"path\":\"/admin/zones/7/../../tables\"}",
                "{\"method\":\"DELETE\",\"path\":\"/admin/killswitch?key=a&key=b\"}",
                "{\"method\":\"POST\",\"path\":\"/admin/zones\",\"operator\":\"bob\"}")) {
            assertEquals(400, assertThrows(ResponseStatusException.class, () -> controller.gatewayAdmin(rejected)).getStatusCode().value(), rejected);
        }
        doThrow(new UnauthorizedException()).when(subject).checkPermission(GmController.GATEWAY_WRITE);
        assertThrows(UnauthorizedException.class, () -> controller.gatewayAdmin("{\"method\":\"POST\",\"path\":\"/admin/zones\"}"));
        assertEquals(List.of(), seen);
    }

    @Test void plainHttpGatewayIsAllowedOnlyForLoopbackOrClusterServiceNames() {
        // 集群内服务间本来就是明文(设计 §11.4):回环与 <service>.<namespace>.svc[.cluster.local] 上的 http 放行,别的照旧拒绝。
        GameServices game = mock(GameServices.class);
        GmCommandJournal journal = mock(GmCommandJournal.class);
        for (String url : List.of("http://gateway.mmorpg.svc:8081", "http://gateway.mmorpg.svc.cluster.local:8081",
                "http://127.0.0.1:8081", "http://localhost:8081", "http://[::1]:8081")) {
            assertDoesNotThrow(() -> new GmController(json, game, journal, url, KEY, "", ""), url);
        }
        for (String url : List.of("http://gateway.mmorpg:8081", "http://a.b.c.svc:8081", "http://x.svc.example.com:8081",
                "http://Gateway.mmorpg.svc:8081", "http://10.0.0.8:8081", "http://[fe80::1]:8081", "http://user@gateway.mmorpg.svc:8081",
                "http://gateway.mmorpg.svc:8081?x=1", "http://gateway.mmorpg.svc:8081#f")) {
            assertThrows(IllegalArgumentException.class, () -> new GmController(json, game, journal, url, KEY, "", ""), url);
        }
    }
}
