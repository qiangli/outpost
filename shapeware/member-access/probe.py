#!/usr/bin/env python3
"""Loopback-only synthetic grant server and upstream for a disposable reference run."""
import hashlib, hmac, json, os, pathlib, socket, subprocess, sys, tempfile, threading, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.request import Request, urlopen
from urllib.error import HTTPError

binary, fixture = sys.argv[1:3]
only = sys.argv[3] if len(sys.argv) > 3 else None
def say(label, text):
    if only is None or only == label: print(text, flush=True)
data = pathlib.Path(fixture).read_bytes()
fetched = threading.Event()
SECRET = b'synthetic-secret'
def mac(payload): return hmac.new(SECRET, payload.encode(), hashlib.sha256).hexdigest()
def verify_rel(h, app, user):
    kind = h.get('X-Outpost-Relationship', '')
    ts = h.get('X-Outpost-Identity-Ts', '')
    sig = h.get('X-Outpost-Relationship-Sig', '')
    visitor = h.get('X-Outpost-Visitor', '')
    payload = f"v2\n{kind}\n{h.get('X-Outpost-Circle','')}\n{h.get('X-Outpost-Groups','')}\n{visitor}\n{user}\n/app/{app}\n{ts}"
    if not kind or not ts or not sig or not hmac.compare_digest(mac(payload), sig):
        raise AssertionError(f'relationship signature does not verify: {h}')
    return kind, visitor

class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args): pass
    def do_GET(self):
        if self.path == '/api/v1/hosts/self/grants':
            if self.headers.get('Authorization') != 'Bearer ' + 'synthetic-token':
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
    env = dict(os.environ, HOME=tmp, TMPDIR=tmp, XDG_CONFIG_HOME=f'{tmp}/.config', XDG_CACHE_HOME=f'{tmp}/.cache', XDG_STATE_HOME=f'{tmp}/.local/state', XDG_DATA_HOME=f'{tmp}/.local/share', OUTPOST_ADMIN_ADDR=f'127.0.0.1:{admin}', OUTPOST_INGRESS_TRUST_LOOPBACK='1')
    proc = subprocess.Popen([binary, 'start'], env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        if not fetched.wait(60): raise RuntimeError('grant copy not fetched from examples/grants.json fixture')
        def request(app, user):
            hdr = {'X-Forwarded-Prefix':'/app/'+app}
            if user: hdr['X-Periscope-User'] = user
            try:
                with urlopen(Request(f'http://127.0.0.1:{front}/app/{app}/', headers=hdr), timeout=8) as resp: return resp.status, json.loads(resp.read())
            except HTTPError as e: return e.code, {}
        cases = [('owner','demo','owner@example.test',200),('member','demo','member@example.test',200),('member-outside-circle','demo','outsider@example.test',403),('member-outside-group','demo','nogroup@example.test',403),('account','demo','account@example.test',403),('anonymous','demo',None,403),('public','public',None,200)]
        for label, app, user, expected in cases:
            status, headers = request(app, user)
            if status != expected: raise AssertionError(f'{label}: expected {expected}, got {status}')
            say(label, f'{label}: {status}')
        # Member identity: signed, verifiable, address present.
        status, h = request('demo', 'member@example.test')
        kind, visitor = verify_rel(h, 'demo', 'member@example.test')
        if kind != 'member' or h.get('X-Outpost-Circle') != 'company' or h.get('X-Outpost-Groups') != 'support' or h.get('X-Periscope-User') != 'member@example.test' or visitor:
            raise AssertionError(f'member identity: {h}')
        say('member-identity', 'member identity: signed and verified (member/company/support)')
        # Account identity on public app: pseudonym, address withheld, signature verifies.
        status, h = request('public', 'account@example.test')
        kind, visitor = verify_rel(h, 'public', '')
        want_visitor = mac('visitor\npublic\naccount@example.test')[:32]
        if status != 200 or kind != 'account' or h.get('X-Periscope-User') or h.get('Remote-User') or visitor != want_visitor:
            raise AssertionError(f'account identity: {status} {h}')
        say('account-identity', f'account identity: {kind}, pseudonym and signature verified; address withheld')
        fixture_server.shutdown()
        status, _ = request('demo', 'member@example.test')
        if status != 200 or proc.poll() is not None: raise AssertionError('offline copy or daemon failed')
        say('relay-unavailable', 'relay unavailable: daemon running, member allowed')
    finally:
        proc.terminate()
        try: proc.wait(timeout=10)
        except subprocess.TimeoutExpired: proc.kill(); proc.wait()
        upstream.shutdown()
