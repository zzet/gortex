#!/usr/bin/env python3
"""Compare release-shape builds with checksum-pinned published tag binaries."""
import hashlib
import json
import os
import re
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


def source_identity(role):
    parts = (root / role / 'source.txt').read_text().strip().split(maxsplit=2)
    require(len(parts) == 3, 'missing exact source commit/tree/time: ' + role)
    commit, tree, committed_at = parts
    require(re.fullmatch(r'[0-9a-f]{40}', commit) and
            re.fullmatch(r'[0-9a-f]{40}', tree), 'invalid source identity: ' + role)
    provenance = {}
    for line in (root / role / 'driver-provenance.txt').read_text().splitlines():
        key, separator, value = line.partition('=')
        if separator:
            provenance[key] = value
    require(provenance.get('source') == commit, 'driver/source identity mismatch: ' + role)
    return {'commit': commit, 'tree': tree, 'committed_at': committed_at,
            'image': provenance.get('image'), 'platform': provenance.get('platform'),
            'version': provenance.get('version'), 'date': provenance.get('date')}


sources = {role: source_identity(role) for role in ('candidate', 'control')}
for key, environment in (('commit', 'RELEASE_SIZE_CANDIDATE_SHA'),
                         ('tree', 'RELEASE_SIZE_CANDIDATE_TREE')):
    pin = os.environ.get(environment, '')
    require(re.fullmatch(r'[0-9a-f]{40}', pin), 'unbound final candidate: ' + environment)
    require(sources['candidate'][key] == pin, 'candidate artifact source differs: ' + key)
require(sources['control']['commit'] == 'defa9f8e7a670792b5b693744d88488f741a8160' and
        sources['control']['tree'] == '655071eb061f2647bd819ef30e0f42116f1dfcbf',
        'same-image tag-source control differs')
for role in sources:
    require(sources[role]['image'] == 'ghcr.io/goreleaser/goreleaser-cross@sha256:33b3cc035e333460086a631f87b3b898e86e2cd9971ea0250e012d46d8b0c643' and
            sources[role]['platform'] == 'linux/amd64', 'source producer differs: ' + role)
    require(sources[role]['version'] and sources[role]['date'], 'missing common build metadata: ' + role)
for key in ('version', 'date'):
    require(sources['candidate'][key] == sources['control'][key], 'role build metadata differs: ' + key)


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
                 'maximum_bytes_at_limit': tag_bytes * 105 // 100,
                 'size_gate_passed': csize * 100 <= tag_bytes * 105,
                 'published_tag_sha256': sha(tag), 'candidate_sha256': sha(candidate),
                 'matched_control_sha256': sha(control), 'archive_sha256': archive_sha})
report = {'baseline': 'published v0.64.10', 'size_limit_percent': 5, 'sources': sources,
          'control_kind': 'v0.64.10 source rebuilt with the same image and common metadata',
          'candidate_kind': 'prospective release-shape build',
          'official_release_artifact_equivalence_established': False,
          'scope': 'size only; approved 5% limit; cold-index acceptance remains separate', 'rows': rows}
(root / 'size-report.json').write_text(json.dumps(report, indent=2) + '\n')
text = ['Prospective release-shape builds versus checksum-verified published v0.64.10.',
        'These are measurement artifacts, not official release artifacts. Cold acceptance is unmeasured.',
        'Control is v0.64.10 source rebuilt with common metadata; the approved 5% gate uses the published tag.',
        f"Candidate source: {sources['candidate']['commit']} / tree {sources['candidate']['tree']}.",
        f"Tag-source control: {sources['control']['commit']} / tree {sources['control']['tree']}.", '',
        '| Arch | Published tag bytes | Final candidate bytes | Same-image tag-source bytes | vs published tag | Candidate minus tag-source bytes | +5% published |',
        '|---|---:|---:|---:|---:|---:|---|']
for row in rows:
    text.append(f"| {row['arch']} | {row['published_tag_bytes']} | {row['candidate_bytes']} | {row['matched_control_bytes']} | {row['candidate_vs_tag_percent']:+.4f}% | {row['candidate_minus_control_bytes']:+d} | {'PASS' if row['size_gate_passed'] else 'FAIL'} |")
(root / 'size-report.md').write_text('\n'.join(text) + '\n')
print('\n'.join(text))
sys.exit(0 if all(row['size_gate_passed'] for row in rows) else 1)
