#pragma once

#include <algorithm>
#include <cmath>
#include <cstdint>
#include <fstream>
#include <iomanip>
#include <string>
#include <vector>

// 节庆导航的原始半开边界与位图合同；Recast 体素取整不能扩大允许移动的范围。
// 烘焙器与运行端共用此数据格式，坐标只在 NavQuerySystem 转换一次。
struct NavMask
{
    int columns = 150, rows = 150;
    double cellSize = 2, minX = 50, minZ = 0, width = 300, height = 300;
    double effectiveX = 50, effectiveZ = 0, effectiveWidth = 300, effectiveHeight = 300;
    std::vector<uint8_t> packed;

    bool ValidGeometry() const
    {
        const double values[] = {cellSize, minX, minZ, width, height, effectiveX,
            effectiveZ, effectiveWidth, effectiveHeight};
        for (double v : values) if (!std::isfinite(v)) return false;
        return columns > 0 && rows > 0 && columns <= 4096 && rows <= 4096 &&
            cellSize > 0 && width > 0 && height > 0 && effectiveWidth > 0 && effectiveHeight > 0 &&
            columns == std::ceil(width / cellSize) && rows == std::ceil(height / cellSize) &&
            effectiveX >= minX && effectiveZ >= minZ &&
            effectiveX + effectiveWidth <= minX + width + 1e-9 &&
            effectiveZ + effectiveHeight <= minZ + height + 1e-9;
    }

    bool Valid() const
    {
        const size_t cells = static_cast<size_t>(columns) * rows;
        const int tail = static_cast<int>((8 - cells % 8) % 8);
        return ValidGeometry() && packed.size() == (cells + 7) / 8 &&
            (packed.back() & ((1 << tail) - 1)) == 0;
    }

    bool Cell(int col, int row) const
    {
        if (col < 0 || row < 0 || col >= columns || row >= rows) return false;
        const size_t i = static_cast<size_t>(row) * columns + col;
        return i / 8 < packed.size() && (packed[i / 8] & (0x80 >> (i % 8))) != 0;
    }

    bool Contains(double x, double z) const
    {
        if (!std::isfinite(x) || !std::isfinite(z) || x < effectiveX || z < effectiveZ ||
            x >= effectiveX + effectiveWidth || z >= effectiveZ + effectiveHeight) return false;
        return Cell(static_cast<int>(std::floor((x - minX) / cellSize)),
            rows - 1 - static_cast<int>(std::floor((z - minZ) / cellSize)));
    }

    bool CellBounds(int col, int row, double& x0, double& x1, double& z0, double& z1) const
    {
        x0 = std::max(minX + col * cellSize, effectiveX);
        x1 = std::min(minX + (col + 1) * cellSize, effectiveX + effectiveWidth);
        z0 = std::max(minZ + (rows - 1 - row) * cellSize, effectiveZ);
        z1 = std::min(minZ + (rows - row) * cellSize, effectiveZ + effectiveHeight);
        return x0 < x1 && z0 < z1;
    }

    // 列/行格线切分直线，检查每个开区间以及格线交点。对角穿角同时检查两个正交格。
    bool Segment(double x0, double z0, double x1, double z1) const
    {
        if (!Contains(x0, z0) || !Contains(x1, z1)) return false;
        std::vector<double> cuts{0, 1};
        auto addCuts = [&cuts](double a, double b, double origin, double cell) {
            if (a == b) return;
            int first = static_cast<int>(std::floor((std::min(a,b) - origin) / cell)) + 1;
            int last = static_cast<int>(std::floor((std::max(a,b) - origin) / cell));
            for (int i = first; i <= last; ++i) {
                double t = (origin + i * cell - a) / (b - a);
                if (t > 0 && t < 1) cuts.push_back(t);
            }
        };
        addCuts(x0, x1, minX, cellSize);
        addCuts(z0, z1, minZ, cellSize);
        std::sort(cuts.begin(), cuts.end());
        // 同时跨 X/Z 格线只保留一个时刻，零长度区间不能伪装为正交中转格。
        cuts.erase(std::unique(cuts.begin(), cuts.end(), [](double a, double b) {
            return std::abs(a - b) <= 1e-12;
        }), cuts.end());
        int prevCol = static_cast<int>(std::floor((x0 - minX) / cellSize));
        int prevRow = rows - 1 - static_cast<int>(std::floor((z0 - minZ) / cellSize));
        for (size_t i = 1; i < cuts.size(); ++i) {
            double t = (cuts[i - 1] + cuts[i]) * 0.5;
            double x = x0 + (x1 - x0) * t, z = z0 + (z1 - z0) * t;
            if (!Contains(x, z)) return false;
            int col = static_cast<int>(std::floor((x - minX) / cellSize));
            int row = rows - 1 - static_cast<int>(std::floor((z - minZ) / cellSize));
            if (col != prevCol && row != prevRow && (!Cell(col, prevRow) || !Cell(prevCol, row))) return false;
            prevCol = col; prevRow = row;
        }
        const int endCol = static_cast<int>(std::floor((x1 - minX) / cellSize));
        const int endRow = rows - 1 - static_cast<int>(std::floor((z1 - minZ) / cellSize));
        return endCol == prevCol || endRow == prevRow || (Cell(endCol, prevRow) && Cell(prevCol, endRow));
    }

    bool Save(const std::string& path) const
    {
        if (!Valid()) return false;
        std::ofstream out(path, std::ios::binary);
        out << "FMASK 1\n" << columns << ' ' << rows << '\n' << std::setprecision(17)
            << cellSize << ' ' << minX << ' ' << minZ << ' ' << width << ' ' << height << '\n'
            << effectiveX << ' ' << effectiveZ << ' ' << effectiveWidth << ' ' << effectiveHeight << '\n';
        for (uint8_t b : packed) out << std::hex << std::setfill('0') << std::setw(2) << static_cast<int>(b);
        out << '\n';
        return static_cast<bool>(out);
    }

    bool Load(const std::string& path)
    {
        std::ifstream in(path, std::ios::binary);
        std::string magic, hex, extra;
        int version = 0;
        if (!(in >> magic >> version >> columns >> rows >> cellSize >> minX >> minZ >> width >> height
            >> effectiveX >> effectiveZ >> effectiveWidth >> effectiveHeight) ||
            magic != "FMASK" || version != 1 || !ValidGeometry()) return false;
        if (!(in >> hex) || (in >> extra) || hex.size() != ((static_cast<size_t>(columns) * rows + 7) / 8) * 2) return false;
        auto nibble = [](char c) { return c >= '0' && c <= '9' ? c - '0' : c >= 'a' && c <= 'f' ? c - 'a' + 10 : -1; };
        packed.clear();
        for (size_t i = 0; i < hex.size(); i += 2) {
            int hi = nibble(hex[i]), lo = nibble(hex[i+1]);
            if (hi < 0 || lo < 0) return false;
            packed.push_back(static_cast<uint8_t>(hi * 16 + lo));
        }
        return Valid();
    }
};
