#!/usr/bin/env python3
"""Keep local verification material out of the fork's tracked tree."""
from pathlib import PurePosixPath
import subprocess

BASIC_DOCS = {'README.md', 'API.md', 'CONFIGURATION.md', 'DEPLOYMENT.md', 'USAGE.md', 'TROUBLESHOOTING.md'}

def excluded(name):
    path = PurePosixPath(name)
    parts = {p.lower() for p in path.parts}
    filename = path.name.lower()
    if parts & {'tests', 'testdata', 'fixtures', '__snapshots__', 'screenshots', 'recordings', '.claude'}:
        return True
    if filename.endswith(('_test.go', '.tsbuildinfo', '.log', '.test.mjs', '.test.ts', '.test.tsx', '.spec.ts', '.spec.tsx')):
        return True
    if name in {'DESIGN.md', 'CHANGELOG.md', 'AGENT_IDENTITY_IMPORT.md', '.github/workflows/security-scan.yml'}:
        return True
    if name.startswith('docs/') and (len(path.parts) != 2 or path.name not in BASIC_DOCS):
        return True
    if name.startswith(('.github/scripts/', 'frontend/scripts/', 'scripts/')) and any(term in filename for term in ['test', 'verify', 'smoke', 'audit-gate', 'race-packages']):
        return True
    return False

def clean(repo):
    result = subprocess.check_output(['git', '-C', str(repo), 'ls-files', '-z']).decode().split('\0')
    names = [name for name in result if name and excluded(name)]
    # --cached preserves existing local files, including verification sources.
    for offset in range(0, len(names), 100):
        subprocess.run(['git', '-C', str(repo), 'rm', '--cached', '-f', '--ignore-unmatch', '--', *names[offset:offset+100]], check=True, stdout=subprocess.DEVNULL)
    return len(names)

if __name__ == '__main__':
    import sys
    from pathlib import Path
    print('Untracked local-only files:', clean(Path(sys.argv[1] if len(sys.argv)>1 else '.')))
