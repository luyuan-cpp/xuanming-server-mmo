"""校验并恢复本仓归档的 Boost 当前源码，不覆盖已有目录。"""
import argparse
import hashlib
import io
import json
from pathlib import Path
import zipfile

parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument('--output', required=True, help='尚不存在的独立恢复目录')
args = parser.parse_args()
root = Path(__file__).resolve().parents[3]
manifest = json.loads((root / 'tools/patches/local-worktree-20260929.json').read_text(encoding='utf-8'))
entry = next(item for item in manifest['dependencies'] if item['path'] == 'third_party/boost')
pieces = []
for part in entry['recovery_parts']:
    path = (root / part['path']).resolve()
    if not path.is_relative_to((root / 'tools/patches/boost').resolve()):
        raise ValueError('分卷路径超出归档目录')
    data = path.read_bytes()
    if len(data) != part['bytes'] or hashlib.sha256(data).hexdigest() != part['sha256']:
        raise ValueError('分卷校验失败: ' + part['path'])
    pieces.append(data)
archive = b''.join(pieces)
if len(archive) != entry['recovery_archive_bytes'] or hashlib.sha256(archive).hexdigest() != entry['recovery_archive_sha256']:
    raise ValueError('完整归档校验失败')
output = Path(args.output).resolve()
if output.exists():
    raise FileExistsError('恢复目录已存在，拒绝覆盖: ' + str(output))
with zipfile.ZipFile(io.BytesIO(archive)) as source:
    for member in source.infolist():
        if not (output / member.filename).resolve().is_relative_to(output):
            raise ValueError('归档成员路径超出恢复目录')
    if source.testzip() is not None:
        raise ValueError('ZIP 完整性检查失败')
    output.mkdir(parents=True, exist_ok=False)
    source.extractall(output)
print('源码已恢复至: ' + str(output))
