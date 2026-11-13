#pragma once

#include <cmath>
#include <fstream>
#include <string>

// 手绘平面导航的有效范围；只约束原图世界矩形和原始地面，不再引入旧位图真相。
// 伴随文件格式：ANAV 1 换行 minX minZ width height groundY 换行；SHA 由装载边界核对。
struct AuthoredNavBounds
{
    double minX = 0, minZ = 0, width = 0, height = 0, groundY = 0;

    bool Valid() const
    {
        return std::isfinite(minX) && std::isfinite(minZ) && std::isfinite(width) &&
            std::isfinite(height) && width > 0 && height > 0 && groundY == 0 &&
            std::isfinite(minX + width) && std::isfinite(minZ + height) &&
            minX + width > minX && minZ + height > minZ;
    }

    bool Contains(double x, double z) const
    {
        return std::isfinite(x) && std::isfinite(z) && x >= minX && z >= minZ &&
            x < minX + width && z < minZ + height;
    }

    bool Load(const std::string& path)
    {
        std::ifstream input(path, std::ios::binary | std::ios::ate);
        if (!input || input.tellg() <= 0 || input.tellg() > 4096) return false;
        input.seekg(0);
        std::string magic, extra;
        int version = 0;
        return static_cast<bool>(input >> magic >> version >> minX >> minZ >> width >> height >> groundY) &&
            !(input >> extra) && magic == "ANAV" && version == 1 && Valid();
    }
};
