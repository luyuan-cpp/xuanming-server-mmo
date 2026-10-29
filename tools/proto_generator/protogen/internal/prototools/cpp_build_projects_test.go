package prototools

import (
	"fmt"
	"os"
	"path/filepath"
	"protogen/logger"
	"strings"
	"testing"

	"go.uber.org/zap"
)

func TestCppBuildRegistrationAddsNewDomainsWithoutDuplicates(t *testing.T) {
	for _, name := range []string{"CMakeLists.txt", "proto.vcxproj", "proto.vcxproj.filters"} {
		t.Run(name, func(t *testing.T) {
			text := "<Project>\r\n  <ItemGroup><ClCompile Include=\"chat\\chat.pb.cc\" /></ItemGroup>\r\n</Project>\r\n"
			if name == "CMakeLists.txt" {
				text = "# 保留工程配置\r\nset(SOURCE_FILES\r\n    chat/chat.pb.cc\r\n)\r\nadd_library(proto STATIC ${SOURCE_FILES})\r\n"
			}
			sources := []string{"chat/chat.pb.cc", "mail/mail.pb.cc", "rank/rank.pb.cc", "trade/commerce.pb.cc"}
			headers := []string{"mail/mail.pb.h"}
			first, err := appendCppProjectEntries(name, text, sources, headers)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.ReplaceAll(first, "\\", "/"), "mail/mail.pb.cc") {
				t.Fatal("新邮件协议未登记")
			}
			second, err := appendCppProjectEntries(name, first, sources, headers)
			if err != nil {
				t.Fatal(err)
			}
			if second != first {
				t.Fatal("重复运行改变清单或生成重复编译条目")
			}
			if !strings.HasPrefix(first, strings.Split(text, "\r\n")[0]) {
				t.Fatal("破坏已有配置")
			}
		})
	}
}

func TestCppBuildRegistrationRejectsBrokenManifestBeforeWriting(t *testing.T) {
	root := t.TempDir()
	protoRoot := filepath.Join(root, "proto")
	for _, project := range []string{"proto", "grpc_client"} {
		dir := filepath.Join(root, "cpp", "generated", project)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		for name, body := range map[string]string{"CMakeLists.txt": "set(SOURCE_FILES\n)\n", project + ".vcxproj": "<Project></Project>", project + ".vcxproj.filters": "<Project></Project>"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0644); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.MkdirAll(filepath.Join(protoRoot, "mail"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(protoRoot, "mail", "mail.proto"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "cpp", "generated", "proto", "mail")
	if err := os.MkdirAll(output, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(output, "mail.pb.cc"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(root, "cpp", "generated", "grpc_client", "grpc_client.vcxproj")
	if err := os.WriteFile(broken, []byte("<Project>"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := syncCppBuildProjects(protoRoot, root); err == nil {
		t.Fatal("损坏清单必须失败")
	}
	unchanged, err := os.ReadFile(filepath.Join(root, "cpp", "generated", "proto", "proto.vcxproj"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(unchanged), "mail.pb.cc") {
		t.Fatal("清单校验失败却写入半套登记")
	}
}

func TestCppBuildRegistrationRemovesDuplicatesOnlyFromGeneratedGroup(t *testing.T) {
	text := "<Project>\r\n  <ItemGroup>\r\n" +
		"    <ClCompile Include=\"mail\\mail.pb.cc\"><ObjectFileName>custom.obj</ObjectFileName></ClCompile>\r\n" +
		"  </ItemGroup>\r\n  <ItemGroup Label=\"ProtoGeneratorSources\">\r\n" +
		"    <ClCompile Include=\"mail\\mail.pb.cc\" />\r\n" +
		"    <ClCompile Include=\"rank\\rank.pb.cc\" />\r\n" +
		"    <ClCompile Include=\"rank\\rank.pb.cc\" />\r\n" +
		"  </ItemGroup>\r\n</Project>\r\n"
	got, err := appendCppProjectEntries("proto.vcxproj", text,
		[]string{"mail/mail.pb.cc", "rank/rank.pb.cc"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"mail\\mail.pb.cc", "rank\\rank.pb.cc"} {
		if strings.Count(got, `Include="`+path+`"`) != 1 {
			t.Fatalf("同一源文件必须只编译一次: %s\n%s", path, got)
		}
	}
	if !strings.Contains(got, "<ObjectFileName>custom.obj</ObjectFileName>") {
		t.Fatal("不能覆盖既有手工编译配置")
	}
	again, err := appendCppProjectEntries("proto.vcxproj", got,
		[]string{"mail/mail.pb.cc", "rank/rank.pb.cc"}, nil)
	if err != nil || again != got {
		t.Fatalf("去重后的重复运行应幂等: %v", err)
	}
}

func TestOfficialGrpcRegistrationRemovalPreservesMessagesAndIsIdempotent(t *testing.T) {
	for _, name := range []string{"CMakeLists.txt", "proto.vcxproj", "proto.vcxproj.filters"} {
		t.Run(name, func(t *testing.T) {
			text := "<Project>\r\n  <ItemGroup>\r\n    <ClCompile Include=\"mail\\mail.pb.cc\" />\r\n    <ClCompile Include=\"mail\\mail.grpc.pb.cc\" />\r\n    <ClInclude Include=\"mail\\mail.grpc.pb.h\" />\r\n  </ItemGroup>\r\n</Project>\r\n"
			if name == "CMakeLists.txt" {
				text = "# 保留配置\r\nset(SOURCE_FILES\r\n    mail/mail.pb.cc\r\n    mail/mail.grpc.pb.cc\r\n)\r\n"
			} else if strings.HasSuffix(name, ".filters") {
				text = "<Project>\r\n  <ItemGroup>\r\n    <ClCompile Include=\"mail\\mail.pb.cc\" />\r\n    <ClCompile Include=\"mail\\mail.grpc.pb.cc\">\r\n      <Filter>源文件</Filter>\r\n    </ClCompile>\r\n  </ItemGroup>\r\n</Project>\r\n"
			}
			got, err := removeOfficialGrpcEntries(name, text)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(got, ".grpc.pb.") || !strings.Contains(got, "mail.pb.cc") {
				t.Fatalf("应只移除官方桩，保留普通消息: %s", got)
			}
			again, err := removeOfficialGrpcEntries(name, got)
			if err != nil || again != got {
				t.Fatalf("退役清理必须幂等: %v", err)
			}
		})
	}
}

func TestOfficialGrpcRemovalRejectsCustomMetadataAndEscapingPaths(t *testing.T) {
	for _, entry := range []string{
		`<ClCompile Include="mail\mail.grpc.pb.cc" Condition="true" />`,
		`<ClCompile Include="mail\mail.grpc.pb.cc"><ObjectFileName>custom.obj</ObjectFileName></ClCompile>`,
		`<ClCompile Include="..\mail.grpc.pb.cc" />`,
	} {
		if _, err := removeOfficialGrpcEntries("proto.vcxproj", "<Project>\n"+entry+"\n</Project>\n"); err == nil {
			t.Fatalf("不能自动移除带条件、元数据或越界路径的条目: %s", entry)
		}
	}
}

func TestOfficialGrpcFilesAreRemovedOnlyAfterAllManifestsValidate(t *testing.T) {
	previousLogger := logger.Global
	logger.Global = zap.NewNop()
	t.Cleanup(func() { logger.Global = previousLogger })
	for _, broken := range []bool{false, true} {
		t.Run(fmt.Sprint(broken), func(t *testing.T) {
			root := t.TempDir()
			protoRoot := filepath.Join(root, "proto")
			if err := os.MkdirAll(filepath.Join(protoRoot, "mail"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(protoRoot, "mail", "mail.proto"), nil, 0644); err != nil {
				t.Fatal(err)
			}
			for _, project := range []string{"proto", "grpc_client"} {
				dir := filepath.Join(root, "cpp", "generated", project)
				if err := os.MkdirAll(dir, 0755); err != nil {
					t.Fatal(err)
				}
				for name, body := range map[string]string{"CMakeLists.txt": "set(SOURCE_FILES\n)\n", project + ".vcxproj": "<Project>\n</Project>\n", project + ".vcxproj.filters": "<Project>\n</Project>\n"} {
					if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0644); err != nil {
						t.Fatal(err)
					}
				}
			}
			output := filepath.Join(root, "cpp", "generated", "proto", "mail")
			if err := os.MkdirAll(output, 0755); err != nil {
				t.Fatal(err)
			}
			message := filepath.Join(output, "mail.pb.cc")
			stub := filepath.Join(output, "mail.grpc.pb.cc")
			if err := os.WriteFile(message, []byte("普通消息"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(stub, []byte("// Generated by the gRPC C++ plugin.\n"), 0644); err != nil {
				t.Fatal(err)
			}
			if broken {
				if err := os.WriteFile(filepath.Join(root, "cpp", "generated", "grpc_client", "grpc_client.vcxproj"), []byte("<Project>"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			err := syncCppBuildProjects(protoRoot, root)
			if broken {
				if err == nil {
					t.Fatal("损坏清单必须拒绝整次同步")
				}
				if _, err := os.Stat(stub); err != nil {
					t.Fatal("校验失败不能删除官方桩")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(stub); !os.IsNotExist(err) {
					t.Fatal("官方桩仍存在")
				}
			}
			if body, err := os.ReadFile(message); err != nil || string(body) != "普通消息" {
				t.Fatal("普通消息不能被清理")
			}
		})
	}
}

func TestGeneratedDependencyConfigurationUsesIndependentSdkPerConfiguration(t *testing.T) {
	for _, variant := range []struct{ configuration, prefix, protobuf string }{
		{"Debug|x64", "install_vs2026_dbg", "libprotobufd.lib"},
		{"Release|x64", "install_vs2026_rel", "libprotobuf.lib"},
		{"Debug|Win32", "install_vs2026_x86_dbg", "libprotobufd.lib"},
		{"Release|Win32", "install_vs2026_x86_rel", "libprotobuf.lib"},
	} {
		t.Run(variant.configuration, func(t *testing.T) {
			original := `<Project><ItemDefinitionGroup Condition="'$(Configuration)|$(Platform)'=='` + variant.configuration + `'">` +
				`<ClCompile><AdditionalIncludeDirectories>../../../third_party/grpc/include/;../../../third_party/grpc/third_party/protobuf/src/;../../../third_party/protobuf/third_party/abseil-cpp/;local/;%(AdditionalIncludeDirectories)</AdditionalIncludeDirectories></ClCompile>` +
				`<Link><AdditionalLibraryDirectories>custom-before/;../../../lib/;../../../third_party/grpc/install_vs2026_dbg/lib/;custom-after/;%(AdditionalLibraryDirectories)</AdditionalLibraryDirectories>` +
				`<AdditionalDependencies>grpc++.lib;grpc.lib;gpr.lib;grpc_client.lib;muduo_grpc.lib;libprotobufd.lib;utf8_range.lib;upb_base_lib.lib;custom.lib;%(AdditionalDependencies)</AdditionalDependencies></Link></ItemDefinitionGroup></Project>`
			got, err := migrateGeneratedDependencyConfiguration(original)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"third_party/protobuf/src/", "third_party/abseil-cpp/", "third_party/protobuf/" + variant.prefix + "/lib/", "third_party/boringssl/" + variant.prefix + "/lib/", variant.protobuf, "grpc_client.lib;muduo_grpc.lib;", "libutf8_range.lib", "custom.lib;%(AdditionalDependencies)"} {
				if !strings.Contains(got, want) {
					t.Fatalf("缺少独立依赖配置 %s: %s", want, got)
				}
			}
			for _, forbidden := range []string{"third_party/grpc/", "grpc++.lib", ";grpc.lib", "gpr.lib", "upb_base_lib.lib"} {
				if strings.Contains(got, forbidden) {
					t.Fatalf("残留官方 SDK 依赖 %s: %s", forbidden, got)
				}
			}
			wantDirectories := "<AdditionalLibraryDirectories>../../../third_party/protobuf/" + variant.prefix + "/lib/;../../../third_party/boringssl/" + variant.prefix + "/lib/;../../../third_party/googletest/" + variant.prefix + "/lib/;custom-before/;../../../lib/;custom-after/;%(AdditionalLibraryDirectories)</AdditionalLibraryDirectories>"
			if !strings.Contains(got, wantDirectories) {
				t.Fatalf("SDK 搜索目录必须前置，其他目录的相对顺序不变: %s", got)
			}
			again, err := migrateGeneratedDependencyConfiguration(got)
			if err != nil || again != got {
				t.Fatalf("依赖配置迁移必须幂等: %v", err)
			}
		})
	}
}

func TestGeneratedDependencyConfigurationKeepsProtoc35CompilerAndStaticZlib(t *testing.T) {
	for _, variant := range []struct{ configuration, prefix, postfix, zlib string }{
		{"Debug|x64", "install_vs2026_dbg", "d", "zlib-v1.3.2-dbg"},
		{"Release|x64", "install_vs2026_rel", "", "zlib-v1.3.2-rel"},
		{"Debug|Win32", "install_vs2026_x86_dbg", "d", "zlib-v1.3.2_x86-dbg"},
		{"Release|Win32", "install_vs2026_x86_rel", "", "zlib-v1.3.2_x86-rel"},
	} {
		t.Run(variant.configuration, func(t *testing.T) {
			for _, originalUpb := range []string{"", "libupb.lib;"} {
				original := `<Project><ItemDefinitionGroup Condition="'$(Configuration)|$(Platform)'=='` + variant.configuration + `'">` +
					`<Link><AdditionalLibraryDirectories>../../../third_party/grpc/install_vs2026_dbg/lib/;../../../lib/;%(AdditionalLibraryDirectories)</AdditionalLibraryDirectories>` +
					`<AdditionalDependencies>libprotocd.lib;` + originalUpb + `grpc.lib;upb_message_lib.lib;zlibd.lib;zlibstaticd.lib;custom.lib;%(AdditionalDependencies)</AdditionalDependencies></Link></ItemDefinitionGroup></Project>`
				got, err := migrateGeneratedDependencyConfiguration(original)
				if err != nil {
					t.Fatal(err)
				}
				for _, want := range []string{"libprotoc" + variant.postfix + ".lib", "libupb" + variant.postfix + ".lib", "zlibstatic" + variant.postfix + ".lib", "custom.lib", "../../../build/third_party/" + variant.zlib + "/install/lib/"} {
					if strings.Count(got, want) != 1 {
						t.Fatalf("独立编译器依赖和静态压缩库必须恰好一次: %s\n%s", want, got)
					}
				}
				if strings.Contains(got, ";grpc.lib") || strings.Contains(got, "upb_message_lib.lib") || strings.Contains(got, ";zlibd.lib") {
					t.Fatalf("不能保留退役运行库或动态 zlib: %s", got)
				}
				if strings.Index(got, "build/third_party/") > strings.Index(got, ";../../../lib/") {
					t.Fatal("canonical zlib 必须先于公共旧归档目录")
				}
				again, err := migrateGeneratedDependencyConfiguration(got)
				if err != nil || again != got {
					t.Fatalf("独立编译器依赖迁移必须幂等: %v", err)
				}
			}
		})
	}
}

func TestGeneratedZlibMigrationRejectsMissingCanonicalSdkRoot(t *testing.T) {
	group := `<Project><ItemDefinitionGroup Condition="'$(Configuration)|$(Platform)'=='Debug|x64'"><Link><AdditionalDependencies>zlibd.lib</AdditionalDependencies><AdditionalLibraryDirectories>../../../lib/</AdditionalLibraryDirectories></Link></ItemDefinitionGroup></Project>`
	if _, err := migrateGeneratedDependencyConfiguration(group); err == nil {
		t.Fatal("不能退回公共目录假装找到了与独立 SDK 匹配的 zlib")
	}
}

func TestCppBuildSdkMigrationCoversAllFiveManagedProjects(t *testing.T) {
	previousLogger := logger.Global
	logger.Global = zap.NewNop()
	t.Cleanup(func() { logger.Global = previousLogger })
	root := t.TempDir()
	protoRoot := filepath.Join(root, "proto")
	if err := os.MkdirAll(protoRoot, 0755); err != nil {
		t.Fatal(err)
	}
	group := `<Project><ItemDefinitionGroup Condition="'$(Configuration)|$(Platform)'=='Debug|x64'"><Link><AdditionalDependencies>libprotocd.lib;zlibd.lib;%(AdditionalDependencies)</AdditionalDependencies><AdditionalLibraryDirectories>../../../third_party/protobuf/install_vs2026_dbg/lib/;../../../lib/</AdditionalLibraryDirectories></Link></ItemDefinitionGroup></Project>`
	for _, project := range []string{"proto", "grpc_client", "rpc", "table", "proto_helpers"} {
		dir := filepath.Join(root, "cpp", "generated", project)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		for name, body := range map[string]string{project + ".vcxproj": group, project + ".vcxproj.filters": "<Project>\n</Project>\n", "CMakeLists.txt": "set(SOURCE_FILES\n)\n"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0644); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := syncCppBuildProjects(protoRoot, root); err != nil {
		t.Fatal(err)
	}
	for _, project := range []string{"proto", "grpc_client", "rpc", "table", "proto_helpers"} {
		body, err := os.ReadFile(filepath.Join(root, "cpp", "generated", project, project+".vcxproj"))
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"libupbd.lib", "zlibstaticd.lib", "../../../build/third_party/zlib-v1.3.2-dbg/install/lib/"} {
			if !strings.Contains(string(body), want) {
				t.Fatalf("受管生成工程 %s 漏掉依赖 %s", project, want)
			}
		}
	}
}
