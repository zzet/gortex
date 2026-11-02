#!/usr/bin/env python3
"""Compare release-shape builds with checksum-pinned published tag binaries."""
import hashlib
import json
from pathlib import Path
import sys
import tarfile

root = Path(sys.argv[1])
baseline = root / 'baseline'
rows = []
expected = {
    'amd64': (308541560, '8dc85ed23aebdf2b8aaf8795e7a9070f4baad181b83ff13b2a764b0e9f48660b'),
    'arm64': (303859576, 'd36835c90ccdfcb6546757602bd82b520ce8d98593eda460faebdbe660d301a2'),
}

def require(condition, message):
    if not condition:
        raise ValueError(message)


def sha(path):
    h = hashlib.sha256()
    with path.open('rb') as stream:
        for chunk in iter(lambda: stream.read(1048576), b''):
            h.update(chunk)
    return h.hexdigest()

checksums = {}
for line in (baseline / 'checksums.txt').read_text().splitlines():
    digest, name = line.split(maxsplit=1)
    checksums[name.lstrip('*')] = digest
for arch, (tag_bytes, archive_sha) in expected.items():
    archive = baseline / f'gortex_linux_{arch}.tar.gz'
    require(checksums[archive.name] == archive_sha, 'published checksum pin changed')
    require(sha(archive) == archive_sha, 'archive checksum mismatch')
    with tarfile.open(archive, 'r:gz') as bundle:
        member = bundle.getmember('gortex')
        require(member.isfile() and member.size == tag_bytes, 'baseline binary shape changed')
        with bundle.extractfile(member) as source, (baseline / f'gortex-linux-{arch}').open('wb') as target:
            for chunk in iter(lambda: source.read(1048576), b''):
                target.write(chunk)
    tag = baseline / f'gortex-linux-{arch}'
    candidate = root / 'candidate' / f'gortex-linux-{arch}'
    control = root / 'control' / f'gortex-linux-{arch}'
    csize, msize = candidate.stat().st_size, control.stat().st_size
    rows.append({'arch': arch, 'published_tag_bytes': tag_bytes,
                 'candidate_bytes': csize, 'matched_control_bytes': msize,
                 'candidate_vs_tag_percent': (csize / tag_bytes - 1) * 100,
                 'candidate_minus_control_bytes': csize - msize,
                 'candidate_vs_control_percent': (csize / msize - 1) * 100,
                 'maximum_bytes_at_two_percent': tag_bytes * 102 // 100,
                 'two_percent_gate_passed': csize * 100 <= tag_bytes * 102,
                 'published_tag_sha256': sha(tag), 'candidate_sha256': sha(candidate),
                 'matched_control_sha256': sha(control), 'archive_sha256': archive_sha})
report = {'baseline': 'published v0.64.10', 'candidate_kind': 'prospective release-shape build',
          'official_release_artifact_equivalence_established': False,
          'scope': 'size only; no cold-index acceptance or revised budget', 'rows': rows}
(root / 'size-report.json').write_text(json.dumps(report, indent=2) + '\n')
text = ['Prospective release-shape builds versus checksum-verified published v0.64.10.',
        'These are measurement artifacts, not official release artifacts. Cold acceptance is unmeasured.', '',
        '| Arch | Tag bytes | Candidate bytes | Control bytes | vs tag | PR delta bytes | +2% |',
        '|---|---:|---:|---:|---:|---:|---|']
for row in rows:
    text.append(f"| {row['arch']} | {row['published_tag_bytes']} | {row['candidate_bytes']} | {row['matched_control_bytes']} | {row['candidate_vs_tag_percent']:+.4f}% | {row['candidate_minus_control_bytes']:+d} | {'PASS' if row['two_percent_gate_passed'] else 'FAIL'} |")
(root / 'size-report.md').write_text('\n'.join(text) + '\n')
print('\n'.join(text))
sys.exit(0 if all(row['two_percent_gate_passed'] for row in rows) else 1)
