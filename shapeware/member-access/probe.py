#!/usr/bin/env python3
"""Loopback-only synthetic grant server and upstream for a disposable reference run."""
import json, os, pathlib, socket, subprocess, sys, tempfile, threading, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.request import Request, urlopen
from urllib.error import HTTPError

binary, fixture = sys.argv[1:]
data = pathlib.Path(fixture).read_bytes()
fetched = threading.Event()
class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args): pass
    def do_GET(self):
        if self.path == '/api/v1/hosts/self/grants':
            if self.headers.get('Authorization') != 'Bearer synthetic-token':
                self.send_error(401); return
            body = data; fetched.set()
        else:
            body = json.dumps(dict(self.headers)).encode()
        self.send_response(200); self.send_header('Content-Length', str(len(body))); self.end_headers(); self.wfile.write(body)
def server():
    s = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
    threading.Thread(target=s.serve_forever, daemon=True).start()
    return s
fixture_server, upstream = server(), server()
def freeport():
    s = socket.socket(); s.bind(('127.0.0.1', 0)); port = s.getsockname()[1]; s.close(); return port
front, admin, relay = freeport(), freeport(), freeport()
with tempfile.TemporaryDirectory(prefix='member-access-') as tmp:
    home = pathlib.Path(tmp); cfg = home / '.config/matrix/agent.json'; cfg.parent.mkdir(parents=True)
    app = lambda name, login: {'name': name, 'scheme': 'http', 'host': '127.0.0.1', 'port': upstream.server_port, 'enabled': True, 'require_login': login, 'trust_cloud_identity': True, 'sso_secret': 'synthetic-secret'}
    cfg.write_text(json.dumps({'agent_name':'test-host','oauth2_email':'owner@example.test','portal_url':f'http://127.0.0.1:{fixture_server.server_port}','access_token':'synthetic-token','token':'synthetic-tunnel-token','server_addr':'127.0.0.1','server_port':relay,'remote_port':freeport(),'local_addr':f'127.0.0.1:{front}','apps':[app('demo', True), app('public', False)]})); cfg.chmod(0o600)
    env = dict(os.environ, HOME=tmp, TMPDIR=tmp, OUTPOST_ADMIN_ADDR=f'127.0.0.1:{admin}', OUTPOST_INGRESS_TRUST_LOOPBACK='1')
    proc = subprocess.Popen([binary, 'start'], env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        if not fetched.wait(20): raise RuntimeError('grant copy not fetched')
        def request(app, user):
            hdr = {'X-Forwarded-Prefix':'/app/'+app}
            if user: hdr['X-Periscope-User'] = user
            try:
                with urlopen(Request(f'http://127.0.0.1:{front}/app/{app}/', headers=hdr), timeout=8) as resp: return resp.status, json.loads(resp.read())
            except HTTPError as e: return e.code, {}
        for label, app, user, expected in [('owner','demo','owner@example.test',200),('member','demo','member@example.test',200),('account','demo','account@example.test',403),('anonymous','demo',None,403),('public','public',None,200)]:
            status, headers = request(app, user)
            if status != expected: raise AssertionError(f'{label}: expected {expected}, got {status}')
            print(f'{label}: {status}', flush=True)
        status, headers = request('public', 'account@example.test')
        if status != 200 or headers.get('X-Outpost-Relationship') not in ('account', 'signed-in') or headers.get('X-Periscope-User') or not headers.get('X-Outpost-Relationship-Sig') or not headers.get('X-Outpost-Visitor'): raise AssertionError(f'identity: {status} {headers}')
        print('account identity: pseudonym and signature; address withheld', flush=True)
        fixture_server.shutdown()
        status, _ = request('demo', 'member@example.test')
        if status != 200 or proc.poll() is not None: raise AssertionError('offline copy or daemon failed')
        print('relay unavailable: daemon running, member allowed', flush=True)
    finally:
        proc.terminate()
        try: proc.wait(timeout=10)
        except subprocess.TimeoutExpired: proc.kill(); proc.wait()
        upstream.shutdown()
