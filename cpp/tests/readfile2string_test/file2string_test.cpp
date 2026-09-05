#include <gtest/gtest.h>

#include <filesystem>
#include <fstream>
#include <string>
#include <system_error>

#include "core/utils/file/file2string.h"

// File2String 把整个文件读成 std::string,是配表加载(*.pb / *.json)的唯一入口。
//
// 原用例只有一行 `File2String("test.txt")`:既不建文件也不断言。而 File2String 在文件
// 不存在时走 LOG_FATAL 直接中止进程 —— 表现为「跑了 1 个用例、没有 PASSED 行、退出码 3」,
// 只看有没有 [FAILED] 的脚本会把它当成通过(2026-09-04 实测踩到)。这里改成自带夹具的真断言。
//
// 文件不存在那条路径故意不测:LOG_FATAL 会 abort,只能用死亡测试覆盖,
// 而 Windows 上的死亡测试要起子进程、在这个链了 muduo 日志后端的工程里不稳,
// 收益不抵不确定性。若日后 File2String 改成返回错误而不是 abort,再补正常用例。

namespace {

class File2StringTest : public ::testing::Test {
protected:
    void SetUp() override {
        path = (std::filesystem::temp_directory_path() / "xuanming_file2string_test.bin").string();
    }

    void TearDown() override {
        std::error_code ec;
        std::filesystem::remove(path, ec);
    }

    void WriteFile(const std::string& content) const {
        std::ofstream out(path, std::ios::binary | std::ios::trunc);
        ASSERT_TRUE(out.is_open()) << "建临时文件失败: " << path;
        out.write(content.data(), static_cast<std::streamsize>(content.size()));
    }

    std::string path;
};

}  // namespace

TEST_F(File2StringTest, ReadsWholeFileContent) {
    const std::string content = "first line\nsecond line\n";
    WriteFile(content);
    EXPECT_EQ(File2String(path), content);
}

TEST_F(File2StringTest, ReadsEmptyFileAsEmptyString) {
    WriteFile(std::string{});
    EXPECT_TRUE(File2String(path).empty());
}

// 配表是二进制 pb:内嵌 \0 与高位字节必须原样读出,不能在中途被截断
TEST_F(File2StringTest, PreservesBinaryBytesIncludingEmbeddedNul) {
    const std::string content("head\0\x01\xff\xfetail", 13);
    ASSERT_EQ(content.size(), 13u);
    WriteFile(content);

    const auto readBack = File2String(path);
    EXPECT_EQ(readBack.size(), content.size()) << "长度不符说明被 \\0 截断或按文本模式读了";
    EXPECT_EQ(readBack, content);
}

// 以二进制模式读:CRLF 不能被悄悄折成 LF,否则配表字节数与磁盘不一致
TEST_F(File2StringTest, DoesNotTranslateCrlf) {
    const std::string content = "a\r\nb\r\n";
    WriteFile(content);

    const auto readBack = File2String(path);
    EXPECT_EQ(readBack.size(), content.size());
    EXPECT_EQ(readBack, content);
}

int32_t main(int argc, char** argv)
{
    testing::InitGoogleTest(&argc, argv);
    return RUN_ALL_TESTS();
}
