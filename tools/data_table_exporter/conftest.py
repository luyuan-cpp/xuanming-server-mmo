"""pytest 根配置:把导出器包根放进 sys.path。

``core.*` 是按「以 tools/data_table_exporter 为根」的绝对导入写的(run.py 也是这么塞的),
不加这一行,从仓库根目录跑 pytest 会全部 ModuleNotFoundError。
"""

import sys
from pathlib import Path

_HERE = Path(__file__).resolve().parent
if str(_HERE) not in sys.path:
    sys.path.insert(0, str(_HERE))
