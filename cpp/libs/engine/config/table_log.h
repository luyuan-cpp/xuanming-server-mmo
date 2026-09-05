#pragma once

#include <cstdint>

/// Log a "row not found" error for a config table lookup.
///
/// Declared here — and NOT as a muduo stream — so the generated per-table headers
/// (table/code/*_table.h) can carry their Lookup* guard macros without dragging
/// muduo/base/Logging.h into every translation unit that includes a table header.
/// muduo's LOG_ERROR is a stream macro, so it cannot be forward-declared; a plain
/// function is the only self-contained shape.
///
/// `file` / `line` are the CALL SITE (__FILE__ / __LINE__ at macro expansion), not
/// this function's own location.
///
/// Defined in table_log.cpp.
void TableLookupLogMissing(const char* tableName, uint64_t tableId,
                           const char* file, int line);
