#!/usr/bin/env python3
"""Verify the complete source overlay before compiling the IPv6 candidate."""
import hashlib,json
from pathlib import Path
root=Path(__file__).resolve().parents[1]/'vendor'
manifest=json.loads((root/'SOURCES.json').read_text())['files']
actual={str(p.relative_to(root)):hashlib.sha256(p.read_bytes()).hexdigest()
        for name in ('applib',) for p in (root/name).rglob('*') if p.is_file()}
if actual!=manifest:
    changed=sorted(k for k in actual.keys()|manifest.keys() if actual.get(k)!=manifest.get(k))
    raise SystemExit('IPv6 source snapshot changed: '+', '.join(changed))
print('IPv6 SDK source hashes verified')
