#!/usr/bin/env python3
"""Merge upstream into the fork, build it, and deploy with database rollback."""
import argparse
import fcntl
import json
import logging
from logging.handlers import RotatingFileHandler
import os
from pathlib import Path
import shutil
import socket
import sqlite3
import subprocess
import tempfile
import time
from datetime import datetime
import urllib.request

from repo_policy import clean, excluded

SOURCE = Path(__file__).resolve().parents[1]
INSTALL = Path('/home/aleafy/code/codex-api')
STATE = Path('/home/aleafy/.local/state/codex2api/updater')
SERVICE = 'codex2api.service'


def run(args, cwd=SOURCE, timeout=180, capture=False, env=None):
    result = subprocess.run(args, cwd=cwd, timeout=timeout, check=True,
                            stdout=subprocess.PIPE if capture else subprocess.DEVNULL,
                            stderr=subprocess.PIPE, text=True, env=env)
    return result.stdout.strip() if capture else ''


def git(*args, cwd=SOURCE):
    return run(['git', *args], cwd=cwd, capture=True)


def http_json(url, secret=None, data=None):
    headers = {'Content-Type': 'application/json'}
    if secret:
        headers['X-Admin-Key'] = secret
    request = urllib.request.Request(url, data=json.dumps(data).encode() if data is not None else None, headers=headers)
    with urllib.request.build_opener(urllib.request.ProxyHandler({})).open(request, timeout=5) as response:
        return json.load(response)


def healthy(version, port=8080):
    for _ in range(60):
        try:
            value = http_json(f'http://127.0.0.1:{port}/health')
            if value.get('status') == 'ok' and value.get('build_version') == version:
                return
        except Exception:
            pass
        time.sleep(1)
    raise RuntimeError('Health/version verification failed: ' + version)


def smoke(binary, version, directory):
    directory.mkdir()
    with socket.socket() as listener:
        listener.bind(('127.0.0.1', 0))
        port = listener.getsockname()[1]
    secret = os.urandom(24).hex()
    env = dict(os.environ, CODEX_PORT=str(port), CODEX_BIND='127.0.0.1', ADMIN_SECRET=secret,
               DATABASE_DRIVER='sqlite', DATABASE_PATH=str(directory/'data.db'), CACHE_DRIVER='memory',
               IMAGE_ASSET_DIR=str(directory/'images'), LOG_DIR=str(directory/'logs'), CODEX_ALLOW_ANONYMOUS='false')
    with (directory/'startup.log').open('w') as output:
        process = subprocess.Popen([str(binary)], cwd=directory, env=env, stdout=output, stderr=output)
        try:
            healthy(version, port)
            result = http_json(f'http://127.0.0.1:{port}/api/admin/usage/conversation-retention', secret)
            if result.get('retention_days') != 7:
                raise RuntimeError('Conversation retention default is not 7 days')
        finally:
            process.terminate()
            try:
                process.wait(timeout=20)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()


def wait_for_idle(secret, timeout=600, quiet_seconds=5):
    if not secret:
        raise RuntimeError('Admin authentication is required to check active requests; deployment skipped')
    deadline = time.monotonic() + timeout
    quiet_since = None
    previous_active = None
    while time.monotonic() < deadline:
        accounts = http_json('http://127.0.0.1:8080/api/admin/runtime-status', secret).get('accounts', {})
        active = accounts.get('active_requests')
        if accounts.get('status') == 'error' or not isinstance(active, int) or active < 0:
            raise RuntimeError('Active request count is unavailable; deployment skipped')
        now = time.monotonic()
        if active != previous_active:
            logging.info('Waiting for idle deployment window: active_requests=%d', active)
            previous_active = active
        if active:
            quiet_since = None
        elif quiet_since is None:
            quiet_since = now
        elif now - quiet_since >= quiet_seconds:
            logging.info('No active model requests for %s seconds; proceeding with deployment', quiet_seconds)
            return
        time.sleep(1)
    raise RuntimeError('No idle deployment window within 10 minutes; running service preserved')


def deploy(binary, version):
    current = http_json('http://127.0.0.1:8080/health')['build_version']
    env = dict(line.split('=', 1) for line in (INSTALL/'.env').read_text().splitlines() if line and not line.startswith('#') and '=' in line)
    if env.get('DATABASE_DRIVER') != 'sqlite':
        raise RuntimeError('This deployment is configured for SQLite only')
    database = Path(env['DATABASE_PATH'])
    if not database.is_absolute():
        database = INSTALL/database
    wait_for_idle(env.get('ADMIN_SECRET'))
    backup = STATE/'backups'/(datetime.now().strftime('%Y%m%d-%H%M%S') + '-' + current)
    backup.mkdir(parents=True, mode=0o700)
    shutil.copy2(INSTALL/'codex2api', backup/'codex2api')
    shutil.copy2(INSTALL/'.env', backup/'.env')
    staged = INSTALL/'.codex2api-new'
    shutil.copy2(binary, staged)
    staged.chmod(0o755)
    stopped = False
    replaced = False
    try:
        run(['systemctl', '--user', 'stop', SERVICE], timeout=90)
        stopped = True
        with sqlite3.connect(str(database)) as source, sqlite3.connect(str(backup/'database.db')) as destination:
            source.backup(destination)
            if destination.execute('PRAGMA integrity_check').fetchone()[0] != 'ok':
                raise RuntimeError('Database backup integrity check failed')
        os.replace(staged, INSTALL/'codex2api')
        replaced = True
        run(['systemctl', '--user', 'start', SERVICE], timeout=90)
        healthy(version)
    except Exception:
        logging.exception('Deployment failed; restoring %s', current)
        if replaced:
            run(['systemctl', '--user', 'stop', SERVICE], timeout=90)
            failed = backup/'failed-database'
            failed.mkdir()
            for path in [database, Path(str(database)+'-wal'), Path(str(database)+'-shm')]:
                if path.exists():
                    shutil.move(str(path), str(failed/path.name))
            shutil.copy2(backup/'database.db', database)
            shutil.copy2(backup/'codex2api', staged)
            os.replace(staged, INSTALL/'codex2api')
        if stopped:
            run(['systemctl', '--user', 'start', SERVICE], timeout=90)
            healthy(current)
        raise
    finally:
        staged.unlink(missing_ok=True)
    (STATE/'deployed.json').write_text(json.dumps({'version':version, 'updated_at':datetime.now().isoformat()}))
    backups = sorted(path for path in (STATE/'backups').iterdir() if path.is_dir())
    for old in backups[:-2]:
        shutil.rmtree(old)
    logging.info('Deployed %s; retained at most 2 backups', version)


def sync(deploy_only=False):
    if git('status', '--porcelain', '--untracked-files=no'):
        raise RuntimeError('Source tree has uncommitted changes; refusing automatic merge')
    if not deploy_only:
        git('fetch', 'origin', 'main')
        git('fetch', 'upstream', 'main', '--tags')
        git('merge', '--ff-only', 'origin/main')
    with tempfile.TemporaryDirectory(prefix='fork-sync-', dir=STATE) as tmp:
        temp = Path(tmp)
        work = temp/'source'
        git('worktree', 'add', '--detach', str(work), 'HEAD')
        try:
            if not deploy_only:
                try:
                    git('merge', '--no-commit', '--no-ff', 'upstream/main', cwd=work)
                except subprocess.CalledProcessError:
                    conflicts = git('diff', '--name-only', '--diff-filter=U', cwd=work).splitlines()
                    if not conflicts or any(not excluded(name) for name in conflicts):
                        raise RuntimeError('Upstream merge needs manual resolution: ' + ', '.join(conflicts))
                clean(work)
                if git('diff', '--name-only', '--diff-filter=U', cwd=work):
                    raise RuntimeError('Unresolved merge conflicts')
                if (work/'.git').exists() and git('rev-parse', '--git-path', 'MERGE_HEAD', cwd=work):
                    merge_head = Path(git('rev-parse', '--git-path', 'MERGE_HEAD', cwd=work))
                    if not merge_head.is_absolute():
                        merge_head = work/merge_head
                    if merge_head.exists():
                        git('commit', '-m', 'Merge upstream main with fork repository policy', cwd=work)
            revision = git('rev-parse', 'HEAD', cwd=work)
            base = git('describe', '--tags', '--abbrev=0', '--match', 'v*', cwd=work)
            version = base+'-aleafy.'+revision[:12]
            if http_json('http://127.0.0.1:8080/health').get('build_version') == version:
                logging.info('Already current: %s', version)
                return
            logging.info('Building fork %s', version)
            run(['npm', 'ci', '--no-audit', '--no-fund'], cwd=work/'frontend', timeout=600)
            run(['npm', 'run', 'typecheck'], cwd=work/'frontend', timeout=300)
            build_env = dict(os.environ, VITE_APP_VERSION=version, GOTOOLCHAIN='auto')
            run(['npm', 'run', 'build'], cwd=work/'frontend', timeout=600, env=build_env)
            binary = temp/'codex2api'
            run(['go', 'build', '-trimpath', '-ldflags=-s -w -X github.com/codex2api/internal/version.Version='+version,
                 '-o', str(binary), '.'], cwd=work, timeout=1200, env=build_env)
            smoke(binary, version, temp/'smoke')
            # Publish only a buildable, validated integration. No force push.
            git('push', 'origin', 'HEAD:main', cwd=work)
            git('merge', '--ff-only', revision)
            deploy(binary, version)
        finally:
            git('worktree', 'remove', '--force', str(work))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--deploy-only', action='store_true', help='Build/deploy the committed fork without fetching upstream')
    args = parser.parse_args()
    os.umask(0o077)
    # Cron has a minimal PATH; use the installed Node runtime explicitly.
    node_versions = Path.home()/'.nvm/versions/node'
    if node_versions.exists():
        candidates = sorted(node_versions.glob('v*/bin'), key=lambda p: tuple(int(n) for n in p.parent.name.lstrip('v').split('.')))
        if candidates:
            os.environ['PATH'] = str(candidates[-1])+':'+os.environ.get('PATH', '/usr/bin:/bin')

    os.environ.setdefault('XDG_RUNTIME_DIR', '/run/user/'+str(os.getuid()))
    os.environ.setdefault('DBUS_SESSION_BUS_ADDRESS', 'unix:path='+os.environ['XDG_RUNTIME_DIR']+'/bus')
    STATE.mkdir(parents=True, exist_ok=True, mode=0o700)
    handler = RotatingFileHandler(STATE/'update.log', maxBytes=2*1024*1024, backupCount=3)
    handler.setFormatter(logging.Formatter('%(asctime)s %(levelname)s %(message)s'))
    logging.basicConfig(level=logging.INFO, handlers=[handler])
    with (STATE/'update.lock').open('w') as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX|fcntl.LOCK_NB)
        except BlockingIOError:
            logging.info('Another update is running; skipped')
            return
        try:
            sync(args.deploy_only)
        except Exception as error:
            if isinstance(error, subprocess.CalledProcessError):
                logging.error('Command failed: %s\n%s', error.cmd, error.stderr)
            else:
                logging.exception('Fork update stopped; current service preserved')
            raise SystemExit(1)

if __name__ == '__main__':
    main()
