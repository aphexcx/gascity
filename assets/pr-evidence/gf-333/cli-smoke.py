"""Disposable-city acceptance proof; never opens or changes the live city."""
import hashlib
import os
from pathlib import Path
import subprocess
import tempfile

binary = Path('bin/gc').resolve()
with tempfile.TemporaryDirectory(prefix='gf-333-smoke-', dir='/var/tmp') as temp:
    root = Path(temp)
    city = root / 'city'
    city.mkdir()
    (city / '.gc').mkdir()
    home = root / 'home'
    home.mkdir()
    env = {'PATH': os.environ['PATH'], 'HOME': str(home), 'GC_HOME': str(home / '.gc'),
           'GC_BEADS': 'file', 'GC_DOLT': 'skip', 'GC_SESSION': 'fake',
           'GIT_CONFIG_NOSYSTEM': '1', 'GIT_CONFIG_GLOBAL': '/dev/null',
           'TMPDIR': str(root), 'USER': os.environ['USER'], 'SHELL': '/bin/sh'}
    source = 'https://example.invalid/license-fixture.git'
    repo = root / 'repo'
    repo.mkdir()
    (repo / 'pack.toml').write_text('[pack]\nname="fixture"\nschema=2\n')
    git = '/Library/Developer/CommandLineTools/usr/bin/git'
    def git_run(*args, cwd=repo):
        return subprocess.check_output([git, '-c', 'core.hooksPath=/dev/null',
            '-c', 'user.name=Fixture', '-c', 'user.email=fixture@example.invalid',
            *args], cwd=cwd, env=env, text=True, stderr=subprocess.STDOUT).strip()
    git_run('init')
    git_run('add', '.')
    git_run('commit', '-m', 'fixture')
    commit = git_run('rev-parse', 'HEAD')
    key = hashlib.sha256((source + commit).encode()).hexdigest()
    cache = home / '.gc/cache/repos' / key
    cache.parent.mkdir(parents=True)
    git_run('clone', str(repo), str(cache))
    (city / 'city.toml').write_text('[workspace]\n')
    (city / '.gc/site.toml').write_text('workspace_name="git-license-smoke"\n')
    (city / 'pack.toml').write_text('[pack]\nname="smoke"\nschema=2\n[imports.fixture]\nsource="' + source + '"\n')
    (city / 'packs.lock').write_text('schema=1\n[packs."' + source + '"]\nversion="sha:' + commit + '"\ncommit="' + commit + '"\n')
    badbin = root / 'badbin'
    badbin.mkdir()
    fake = badbin / 'git'
    fake.write_text("#!/bin/sh\necho 'You have not agreed to the Xcode license agreements.' >&2\nexit 69\n")
    fake.chmod(0o755)
    env['PATH'] = str(badbin) + ':' + env['PATH']
    def run(*args, success=True):
        p = subprocess.run([str(binary), *args], cwd=city, env=env, capture_output=True, text=True, timeout=40)
        print('COMMAND gc', *args, 'EXIT', p.returncode)
        print(p.stdout, end='')
        print(p.stderr, end='')
        assert (p.returncode == 0) == success, (args, p.returncode)
        assert p.stderr.count('warning: git on PATH is unusable') == 1, p.stderr
        return p
    run('config', 'show', '--validate')
    run('session', 'list')
    run('mail', 'inbox')
    doctor = run('doctor', '--check', 'git-binary')
    assert 'Command Line Tools fallback' in doctor.stdout
    git_run('commit', '--allow-empty', '-m', 'wrong head', cwd=cache)
    rejected = run('config', 'show', '--validate', success=False)
    assert 'expected ' + commit in rejected.stderr
    print('PASS: session list and mail inbox succeed; exactly one warning each; doctor warns; wrong commit rejected.')
