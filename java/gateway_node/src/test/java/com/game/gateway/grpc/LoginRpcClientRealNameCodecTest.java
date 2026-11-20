package com.game.gateway.grpc;

import com.google.protobuf.CodedInputStream;
import com.google.protobuf.CodedOutputStream;
import org.junit.jupiter.api.Test;

import java.io.ByteArrayOutputStream;
import java.io.IOException;

import static org.junit.jupiter.api.Assertions.*;

/**
 * 手写的实名编解码与 proto/login/login.proto 对齐(docs/design/real-name-anti-addiction.md §5.2、§5.3):
 * SubmitRealNameRequest 1/2/3、SubmitRealNameResponse 1(tip)/2(状态)、LoginResponse 第 10 号 AntiAddictionStatus。
 */
class LoginRpcClientRealNameCodecTest {

    @Test
    void submitRealNameRequestWritesThreeStringsAndRedactsToString() throws IOException {
        var req = new LoginRpcClient.SubmitRealNameRequestProto("tok", "张三", "11010519491231002X");

        CodedInputStream in = CodedInputStream.newInstance(req.toBytes());
        assertEquals((1 << 3) | 2, in.readTag());
        assertEquals("tok", in.readString());
        assertEquals((2 << 3) | 2, in.readTag());
        assertEquals("张三", in.readString());
        assertEquals((3 << 3) | 2, in.readTag());
        assertEquals("11010519491231002X", in.readString());
        assertTrue(in.isAtEnd());

        assertFalse(req.toString().contains("张三"), "姓名不能出现在 toString 里");
        assertFalse(req.toString().contains("11010519491231002X"), "身份证号不能出现在 toString 里");
    }

    @Test
    void parsesSubmitRealNameResponseTipAndStatus() throws IOException {
        ByteArrayOutputStream tipBytes = new ByteArrayOutputStream();
        CodedOutputStream tip = CodedOutputStream.newInstance(tipBytes);
        tip.writeUInt32(1, 2083); // 任意码值,只测编解码
        tip.flush();
        ByteArrayOutputStream baos = new ByteArrayOutputStream();
        CodedOutputStream out = CodedOutputStream.newInstance(baos);
        out.writeByteArray(1, tipBytes.toByteArray());
        out.writeByteArray(2, antiAddiction(true, true, 1_763_982_000_000L));
        out.flush();

        var rsp = LoginRpcClient.parseSubmitRealNameResponse(baos.toByteArray());

        assertEquals(2083, rsp.errorCode);
        assertTrue(rsp.antiAddiction.realNameRequired);
        assertTrue(rsp.antiAddiction.minor);
        assertEquals(1_763_982_000_000L, rsp.antiAddiction.playDeadlineMs);
    }

    @Test
    void loginResponseCarriesAntiAddictionAtFieldTen() throws IOException {
        ByteArrayOutputStream baos = new ByteArrayOutputStream();
        CodedOutputStream out = CodedOutputStream.newInstance(baos);
        out.writeString(3, "access");
        out.writeByteArray(10, antiAddiction(false, true, 42L));
        out.flush();

        var rsp = LoginRpcClient.parseLoginResponse(baos.toByteArray());

        assertEquals("access", rsp.accessToken);
        assertNotNull(rsp.antiAddiction);
        assertFalse(rsp.antiAddiction.realNameRequired);
        assertTrue(rsp.antiAddiction.minor);
        assertEquals(42L, rsp.antiAddiction.playDeadlineMs);
    }

    @Test
    void loginResponseWithoutFieldTenHasNoStatus() throws IOException {
        ByteArrayOutputStream baos = new ByteArrayOutputStream();
        CodedOutputStream out = CodedOutputStream.newInstance(baos);
        out.writeString(3, "access");
        out.flush();

        assertNull(LoginRpcClient.parseLoginResponse(baos.toByteArray()).antiAddiction,
                "部署没开防沉迷时 login 不带第 10 号字段,网关应答里省略");
    }

    private static byte[] antiAddiction(boolean realNameRequired, boolean minor, long deadlineMs) throws IOException {
        ByteArrayOutputStream baos = new ByteArrayOutputStream();
        CodedOutputStream out = CodedOutputStream.newInstance(baos);
        out.writeBool(1, realNameRequired);
        out.writeBool(2, minor);
        out.writeUInt64(3, deadlineMs);
        out.flush();
        return baos.toByteArray();
    }
}
