#include "table_log.h"

#include "muduo/base/Logging.h"

// 必须用 LOG_ERROR 宏本身,不能手写 muduo::Logger(..., muduo::Logger::ERROR)。
// 仓里有两份 muduo,错误级别的枚举名不一样:
//   Windows  third_party/muduo/muduo/base/Logging.h:26        -> ERROR_
//   Linux    third_party/muduo-linux/muduo/base/Logging.h:26  -> ERROR
// (Windows 那份改名是因为 ERROR 在 wingdi.h 里是宏。)
// 这个文件两端都要编,写死任何一个名字都会挂掉另一端。
//
// 代价:muduo 自己的 SourceFile 字段显示本文件,调用点位置放在消息正文里。
void TableLookupLogMissing(const char* tableName, uint64_t tableId,
                           const char* file, int line) {
    LOG_ERROR << tableName << " row not found for ID: " << tableId
              << " @ " << file << ':' << line;
}
