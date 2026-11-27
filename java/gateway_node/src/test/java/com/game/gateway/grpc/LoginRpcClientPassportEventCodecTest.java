package com.game.gateway.grpc;

import com.google.protobuf.CodedOutputStream;
import org.junit.jupiter.api.Test;

import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.nio.charset.StandardCharsets;

import static org.junit.jupiter.api.Assertions.assertArrayEquals;
import static org.junit.jupiter.api.Assertions.assertEquals;

/**
 * 官方通行证回调两条消息的手写 wire 编解码(docs/design/official-passport-registration.md §3.7)。
 * 同 {@code LoginRpcClientAccountDeletionCodecTest}:字段号写错没有编译期报错,只能靠 {@link CodedOutputStream} 手编字节守住。
 *
 * <p>字段号来自 {@code proto/login/login.proto}:PassportEventRequest payload = 1;PassportEventResponse checked_users = 1。
 */
class LoginRpcClientPassportEventCodecTest {

    @FunctionalInterface
    private interface Fields {
        void writeTo(CodedOutputStream out) throws IOException;
    }

    @Test
    void requestWritesThePayloadVerbatimAsField1() throws IOException {
        byte[] payload = "{\"action\":\"set-password\",\"organization\":\"xuanming\",\"user\":\"alice\"}"
                .getBytes(StandardCharsets.UTF_8);

        assertArrayEquals(encode(out -> out.writeByteArray(1, payload)),
                new LoginRpcClient.PassportEventRequestProto(payload).toBytes());
        assertArrayEquals(new byte[0], new LoginRpcClient.PassportEventRequestProto(null).toBytes(),
                "proto3: empty bytes are not written");
    }

    @Test
    void responseParsesCheckedUsersSkippingUnknownFields() throws IOException {
        byte[] bytes = encode(out -> {
            out.writeString(9, "future field");
            out.writeUInt32(1, 3);
        });

        assertEquals(3, LoginRpcClient.parsePassportEventResponse(bytes).checkedUsers);
        assertEquals(0, LoginRpcClient.parsePassportEventResponse(new byte[0]).checkedUsers);
    }

    private static byte[] encode(Fields fields) throws IOException {
        ByteArrayOutputStream baos = new ByteArrayOutputStream();
        CodedOutputStream out = CodedOutputStream.newInstance(baos);
        fields.writeTo(out);
        out.flush();
        return baos.toByteArray();
    }
}
