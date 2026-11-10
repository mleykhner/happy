#!/usr/bin/env python3
"""Create a deterministic, offline Go dependency archive for Nix and Docker."""
import gzip
from pathlib import Path
import tarfile

root = Path(__file__).resolve().parent.parent
with (root / 'packaging/vendor.tar.gz').open('wb') as output:
    with gzip.GzipFile(filename='', mode='wb', fileobj=output, mtime=0, compresslevel=9) as zipped:
        with tarfile.open(fileobj=zipped, mode='w|', format=tarfile.PAX_FORMAT) as archive:
            for path in sorted((root / 'vendor').rglob('*')):
                info = archive.gettarinfo(str(path), arcname=str(path.relative_to(root)))
                info.uid = info.gid = info.mtime = 0
                info.uname = info.gname = ''
                info.pax_headers = {}
                info.mode = 0o755 if path.is_dir() else 0o644
                if path.is_file():
                    with path.open('rb') as source:
                        archive.addfile(info, source)
                else:
                    archive.addfile(info)
