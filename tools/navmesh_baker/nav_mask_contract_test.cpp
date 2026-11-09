#include "../../cpp/libs/services/scene/spatial/comp/nav_mask.h"

#include <iostream>
#include <limits>

// 通过公开 Contains/Segment 接口测试玩家路径，不重复其格线算法。
int main(int argc, char** argv)
{
    int failures = 0, checks = 0;
    auto expect = [&](bool ok, const char* message) {
        ++checks;
        if (!ok) { ++failures; std::cerr << "FAIL: " << message << '\n'; }
    };
    NavMask mask;
    mask.columns = mask.rows = 2;
    mask.minX = mask.minZ = mask.effectiveX = mask.effectiveZ = 0;
    mask.width = mask.height = mask.effectiveWidth = mask.effectiveHeight = 4;
    mask.packed = {0x60}; // 西南与东北两个对角平台。
    expect(mask.Valid(), "two-island fixture valid");
    expect(mask.Contains(1,1) && mask.Contains(3,3), "island endpoints valid");
    expect(!mask.Segment(1,1,3,3), "cannot cut a blocked diagonal corner");
    expect(!mask.Segment(3,3,1,1), "reverse diagonal cannot cut corner");
    expect(mask.Segment(0.1,0.1,1.9,1.9), "ordinary in-cell movement valid");
    mask.packed = {0xd0}; // 西南阻挡，其余可走；混合方向穿角也必须同时查两个正交格。
    expect(!mask.Segment(1,3,3,1), "mixed-direction diagonal cannot cut one blocked corner");
    expect(!mask.Segment(3,1,1,3), "reverse mixed-direction diagonal cannot cut one blocked corner");
    mask.packed = {0x70};
    expect(!mask.Segment(1,1,3,3), "northeast move cannot cut one blocked corner");
    expect(!mask.Segment(3,3,1,1), "southwest move cannot cut one blocked corner");
    mask.packed = {0x60};
    expect(!mask.Contains(4,3) && !mask.Contains(3,4), "east and north half-open limits blocked");
    expect(mask.Contains(0,0), "west and south limits included");
    expect(!mask.Contains(std::numeric_limits<double>::quiet_NaN(),1), "NaN rejected");
    expect(!mask.Contains(1,std::numeric_limits<double>::infinity()), "infinity rejected");
    mask.packed = {0xf0};
    expect(mask.Segment(0,0,3.9,3.9), "connected diagonal valid with orthogonal support");
    mask.effectiveX = 0.5; mask.effectiveWidth = 3;
    expect(!mask.Contains(0.49,1) && mask.Contains(0.5,1), "fractional west crop respected");
    expect(!mask.Contains(3.5,1) && mask.Contains(3.499,1), "fractional east crop half open");
    mask.packed = {0xf1};
    expect(!mask.Valid(), "nonzero tail rejected");
    if (argc == 2) {
        const std::string root = argv[1];
        const char* ids[] = {"01001","01002","05008","10000","19004","21003","23006","28007","35004"};
        const int counts[] = {250,309,2313,7268,48,3218,833,1481,2566};
        for (int m = 0; m < 9; ++m) {
            NavMask actual;
            expect(actual.Load(root + "/" + ids[m] + "/scene.bin.mask"), "delivered mask loads");
            int walkable = 0;
            for (int row=0;row<actual.rows;++row) for(int col=0;col<actual.columns;++col) {
                if (!actual.Cell(col,row)) continue;
                double x0,x1,z0,z1;
                expect(actual.CellBounds(col,row,x0,x1,z0,z1), "walkable cell intersects content");
                expect(actual.Contains((x0+x1)/2,(z0+z1)/2), "clipped cell center is walkable");
                ++walkable;
            }
            expect(walkable == counts[m], "delivered safe-cell count preserved");
            expect(!actual.Contains(actual.effectiveX+actual.effectiveWidth,actual.effectiveZ), "delivered east limit blocked");
        }
    }
    std::cout << "NavMask contract checks=" << checks << " failures=" << failures << '\n';
    return failures ? 1 : 0;
}
