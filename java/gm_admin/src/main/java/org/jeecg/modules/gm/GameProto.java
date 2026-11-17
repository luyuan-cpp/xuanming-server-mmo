package org.jeecg.modules.gm;

import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.node.ObjectNode;
import com.fasterxml.jackson.databind.node.ArrayNode;
import com.fasterxml.jackson.databind.node.JsonNodeFactory;
import com.google.protobuf.CodedOutputStream;
import com.google.protobuf.DescriptorProtos.FileDescriptorProto;
import com.google.protobuf.DescriptorProtos.FileDescriptorSet;
import com.google.protobuf.Descriptors;
import com.google.protobuf.DynamicMessage;
import com.google.protobuf.util.JsonFormat;
import io.grpc.MethodDescriptor;
import io.grpc.protobuf.ProtoUtils;
import java.io.IOException;
import java.io.InputStream;
import java.nio.charset.StandardCharsets;
import java.security.GeneralSecurityException;
import java.security.MessageDigest;
import java.util.HashMap;
import java.util.HashSet;
import java.util.HexFormat;
import java.util.Map;
import java.util.Set;
import javax.crypto.Mac;
import javax.crypto.spec.SecretKeySpec;

/** 只消费权威 proto 的构建产物，不维护另一份消息字段或 RPC 映射。 */
public final class GameProto {
    private final Map<String, Descriptors.MethodDescriptor> methods;
    private final Descriptors.EnumDescriptor currencies;

    private GameProto(Map<String, Descriptors.MethodDescriptor> methods, Descriptors.EnumDescriptor currencies) {
        this.methods = Map.copyOf(methods);
        this.currencies = currencies;
    }

    public static GameProto load() {
        try (InputStream stream = GameProto.class.getResourceAsStream("/gm-contracts.pb")) {
            if (stream == null) throw new IllegalStateException("缺少构建生成的游戏协议描述文件");
            byte[] bytes = stream.readNBytes(32 * 1024 * 1024 + 1);
            if (bytes.length > 32 * 1024 * 1024) throw new IllegalStateException("游戏协议描述文件过大");
            FileDescriptorSet set = FileDescriptorSet.parseFrom(bytes);
            Map<String, FileDescriptorProto> sources = new HashMap<>();
            for (FileDescriptorProto source : set.getFileList()) {
                if (sources.put(source.getName(), source) != null) {
                    throw new IllegalStateException("游戏协议描述文件包含重复文件");
                }
            }
            Map<String, Descriptors.FileDescriptor> files = new HashMap<>();
            for (String name : sources.keySet()) build(name, sources, files, new HashSet<>());
            Map<String, Descriptors.MethodDescriptor> methods = new HashMap<>();
            for (Descriptors.FileDescriptor file : files.values()) {
                for (Descriptors.ServiceDescriptor service : file.getServices()) {
                    for (Descriptors.MethodDescriptor method : service.getMethods()) {
                        methods.put(service.getFullName() + "/" + method.getName(), method);
                    }
                }
            }
            Descriptors.FileDescriptor currencyFile = files.get("proto/common/base/currency_type.proto");
            if (currencyFile == null || currencyFile.findEnumTypeByName("CurrencyType") == null) {
                throw new IllegalStateException("游戏协议缺少权威币种目录");
            }
            return new GameProto(methods, currencyFile.findEnumTypeByName("CurrencyType"));
        } catch (IOException | Descriptors.DescriptorValidationException error) {
            throw new IllegalStateException("无法读取游戏协议描述文件", error);
        }
    }

    private static Descriptors.FileDescriptor build(String name,
            Map<String, FileDescriptorProto> sources, Map<String, Descriptors.FileDescriptor> files,
            Set<String> visiting) throws Descriptors.DescriptorValidationException {
        if (files.containsKey(name)) return files.get(name);
        FileDescriptorProto source = sources.get(name);
        if (source == null || !visiting.add(name)) {
            throw new IllegalStateException("游戏协议依赖缺失或存在循环：" + name);
        }
        Descriptors.FileDescriptor[] dependencies = new Descriptors.FileDescriptor[source.getDependencyCount()];
        for (int index = 0; index < dependencies.length; index++) {
            dependencies[index] = build(source.getDependency(index), sources, files, visiting);
        }
        Descriptors.FileDescriptor file = Descriptors.FileDescriptor.buildFrom(source, dependencies);
        files.put(name, file);
        visiting.remove(name);
        return file;
    }

    public Descriptors.MethodDescriptor descriptor(String name) {
        String normalized = name.startsWith("/") ? name.substring(1) : name;
        Descriptors.MethodDescriptor result = methods.get(normalized);
        if (result == null || result.isClientStreaming() || result.isServerStreaming()) {
            throw new IllegalArgumentException("游戏协议没有该一元 RPC");
        }
        return result;
    }

    public MethodDescriptor<DynamicMessage, DynamicMessage> method(String name) {
        Descriptors.MethodDescriptor descriptor = descriptor(name);
        return MethodDescriptor.<DynamicMessage, DynamicMessage>newBuilder()
                .setType(MethodDescriptor.MethodType.UNARY)
                .setFullMethodName(descriptor.getService().getFullName() + "/" + descriptor.getName())
                .setRequestMarshaller(ProtoUtils.marshaller(DynamicMessage.getDefaultInstance(descriptor.getInputType())))
                .setResponseMarshaller(ProtoUtils.marshaller(DynamicMessage.getDefaultInstance(descriptor.getOutputType())))
                .build();
    }

    public DynamicMessage request(String method, ObjectNode json) throws IOException {
        DynamicMessage.Builder builder = DynamicMessage.newBuilder(descriptor(method).getInputType());
        JsonFormat.parser().merge(json.toString(), builder);
        return builder.build();
    }

    public ObjectNode json(DynamicMessage message, ObjectMapper mapper) throws IOException {
        return (ObjectNode) mapper.readTree(JsonFormat.printer().print(message));
    }

    /** 币种编号只来自权威枚举，展示翻译由界面负责。 */
    public ArrayNode currencyTypes() {
        ArrayNode result = JsonNodeFactory.instance.arrayNode();
        for (Descriptors.EnumValueDescriptor value : currencies.getValues()) {
            result.addObject().put("id", value.getNumber()).put("label", value.getName());
        }
        return result;
    }

    boolean supportsCurrency(int value) { return currencies.findValueByNumber(value) != null; }

    /** 与 Scene 既有 GM nonce 信封相同，但 reason 绑定清空 auth 后的完整请求摘要。 */
    DynamicMessage signPlayerCommand(String method, DynamicMessage request, String secret, long timestampSeconds,
            String nonce) {
        Descriptors.Descriptor type = request.getDescriptorForType();
        Descriptors.FieldDescriptor auth = type.findFieldByName("auth");
        String actor = (String) request.getField(type.findFieldByName("operator"));
        int nodeId = ((Number) request.getField(type.findFieldByName("target_node_id"))).intValue();
        if (auth == null || auth.getJavaType() != Descriptors.FieldDescriptor.JavaType.STRING || nodeId == 0
                || timestampSeconds <= 0 || actor.isBlank() || actor.getBytes(StandardCharsets.UTF_8).length > 64
                || actor.contains("|") || actor.chars().anyMatch(Character::isISOControl)
                || nonce == null || !nonce.matches("[A-Za-z0-9_-]{1,128}")
                || secret == null || secret.getBytes(StandardCharsets.UTF_8).length < 32) {
            throw new IllegalArgumentException("场景管理签名参数非法");
        }
        try {
            DynamicMessage business = request.toBuilder().clearField(auth).build();
            byte[] bytes = new byte[business.getSerializedSize()];
            CodedOutputStream output = CodedOutputStream.newInstance(bytes);
            output.useDeterministicSerialization();
            business.writeTo(output);
            output.checkNoSpaceLeft();
            String digest = HexFormat.of().formatHex(MessageDigest.getInstance("SHA-256").digest(bytes));
            String reason = "gm-player-command-v1\n" + digest;
            String canonical = "/" + method(method).getFullMethodName() + "\n" + Integer.toUnsignedString(nodeId)
                    + "\n" + actor + "\n" + timestampSeconds + "\n" + nonce + "\n" + reason;
            Mac mac = Mac.getInstance("HmacSHA256");
            mac.init(new SecretKeySpec(secret.getBytes(StandardCharsets.UTF_8), "HmacSHA256"));
            String signature = HexFormat.of().formatHex(mac.doFinal(canonical.getBytes(StandardCharsets.UTF_8)));
            return business.toBuilder().setField(auth, actor + "|" + timestampSeconds + "|" + nonce + "|" + signature).build();
        } catch (IOException | GeneralSecurityException error) {
            throw new IllegalStateException("无法构造场景管理调用证明", error);
        }
    }

    /** 与 go/shared/serviceauth 完全相同：每次重投只刷新证明，不改业务请求。 */
    public DynamicMessage sign(String method, DynamicMessage request, String caller, String secret, long timestampMs) {
        if (timestampMs <= 0 || !caller.matches("[a-z][a-z0-9_-]{0,31}")) {
            throw new IllegalArgumentException("内部调用证明参数非法");
        }
        String key = secret == null ? "" : secret.strip();
        if (key.getBytes(StandardCharsets.UTF_8).length < 32) {
            throw new IllegalArgumentException("邮件管理签名密钥未配置或不足 32 字节");
        }
        Descriptors.FieldDescriptor authField = request.getDescriptorForType().findFieldByName("auth");
        if (authField == null) throw new IllegalArgumentException("游戏请求缺少调用证明字段");
        try {
            DynamicMessage business = request.toBuilder().clearField(authField).build();
            byte[] bytes = new byte[business.getSerializedSize()];
            CodedOutputStream output = CodedOutputStream.newInstance(bytes);
            output.useDeterministicSerialization();
            business.writeTo(output);
            output.checkNoSpaceLeft();
            String digest = HexFormat.of().formatHex(MessageDigest.getInstance("SHA-256").digest(bytes));
            String fullMethod = "/" + this.method(method).getFullMethodName();
            String canonical = "v1\n" + fullMethod + "\n" + caller + "\n" + timestampMs + "\n" + digest;
            Mac mac = Mac.getInstance("HmacSHA256");
            mac.init(new SecretKeySpec(key.getBytes(StandardCharsets.UTF_8), "HmacSHA256"));
            String signature = HexFormat.of().formatHex(mac.doFinal(canonical.getBytes(StandardCharsets.UTF_8)));
            Descriptors.Descriptor type = authField.getMessageType();
            DynamicMessage auth = DynamicMessage.newBuilder(type)
                    .setField(type.findFieldByName("caller"), caller)
                    .setField(type.findFieldByName("timestamp_ms"), timestampMs)
                    .setField(type.findFieldByName("signature_hex"), signature).build();
            return business.toBuilder().setField(authField, auth).build();
        } catch (IOException | GeneralSecurityException error) {
            throw new IllegalStateException("无法构造内部调用证明", error);
        }
    }
}
