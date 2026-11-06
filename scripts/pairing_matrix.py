#!/usr/bin/env python3
"""Pairing and access matrix against a scratch yoakeland server.

    go build -o /tmp/yoakeland ./cmd/yoakeland && python3 -I scripts/pairing_matrix.py /tmp/yoakeland

Starts the binary with its own state dir, socket and lock (XDG_CACHE_HOME), so it never touches a
running server, then runs every case in parallel over real TLS and WebSocket connections: who a
device is for x the tier asked for x the device ceiling x allow / allow-full / decline, token rules,
what each tier may and may not do, delegation by a Trusted person, revoking, live lowering, and the
profile picture bucket. Needs only the Python standard library. JOBS sets the parallelism (12).
Exit status is 1 if any case fails. Not covered: the WebRTC transport.
Usage: pairing_matrix.py <yoakeland-binary> [workdir]"""
import base64, collections, concurrent.futures as cf, hashlib, json, os, re, secrets, socket, ssl, struct, subprocess, sys, tempfile, threading, time, traceback, uuid

BIN = sys.argv[1]
WORK = sys.argv[2] if len(sys.argv) > 2 else tempfile.mkdtemp(prefix="matrix-")
DENIED = re.compile(r"needs (Trusted|Full) access|isn't allowed|not allowed|local socket only|only available on the computer")
TIERS = {"limited": 0, "trusted": 1, "full": 2}

def free_port():
    s = socket.socket(); s.bind(("127.0.0.1", 0)); p = s.getsockname()[1]; s.close(); return p

class Conn:
    """A connection with a reader thread; events are kept so any thread can wait for the next match."""
    def __init__(self, kind, target):
        self.kind, self.events, self.cv, self.closed = kind, [], threading.Condition(), False
        self.on_event = None
        if kind == "unix":
            self.sock = socket.socket(socket.AF_UNIX); self.sock.connect(target)
        else:
            ctx = ssl.create_default_context(); ctx.check_hostname = False; ctx.verify_mode = ssl.CERT_NONE
            self.sock = ctx.wrap_socket(socket.create_connection(("127.0.0.1", target), timeout=10), server_hostname="localhost")
            if kind == "ws": self._ws_handshake()
        self.iolock = threading.Lock()   # an SSLSocket must not be read and written at the same moment
        self.sock.settimeout(0.05)
        self.buf = b""
        threading.Thread(target=self._read, daemon=True).start()

    def _ws_handshake(self):
        key = base64.b64encode(os.urandom(16)).decode()
        self.sock.sendall(f"GET /ws HTTP/1.1\r\nHost: localhost\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n\r\n".encode())
        head = b""
        while b"\r\n\r\n" not in head:
            try: head += self.sock.recv(1)
            except (socket.timeout, ssl.SSLWantReadError): pass
        if b" 101 " not in head.split(b"\r\n")[0]: raise RuntimeError("ws upgrade refused: " + head[:80].decode())

    def _recv(self, n):
        while True:
            if self.closed: raise EOFError
            with self.iolock:
                try:
                    c = self.sock.recv(n)
                except (socket.timeout, ssl.SSLWantReadError, TimeoutError):
                    c = None
            if c is None:
                time.sleep(0.005); continue
            if not c: raise EOFError
            return c

    def _recvn(self, n):
        out = b""
        while len(out) < n: out += self._recv(n - len(out))
        return out

    def _messages(self):
        if self.kind != "ws":
            while True:
                try: yield self._recv(1 << 16)
                except EOFError: return
        while True:
            b1, b2 = self._recvn(2); op, n = b1 & 15, b2 & 127
            if n == 126: n = struct.unpack(">H", self._recvn(2))[0]
            elif n == 127: n = struct.unpack(">Q", self._recvn(8))[0]
            data = self._recvn(n)
            if op == 8: return
            if op == 1 or op == 0: yield data

    def _read(self):
        try:
            for chunk in self._messages():
                self.buf += chunk
                while b"\n" in self.buf:
                    line, self.buf = self.buf.split(b"\n", 1)
                    try: env = json.loads(line)
                    except ValueError: continue
                    with self.cv:
                        self.events.append(env); self.cv.notify_all()
                    if self.on_event:
                        try: self.on_event(self, env)
                        except Exception: traceback.print_exc()
        except Exception: pass
        with self.cv:
            self.closed = True; self.cv.notify_all()

    def send(self, typ, data=None, sid=None):
        env = {"type": typ}
        if data is not None: env["data"] = data
        if sid: env["sessionId"] = sid
        raw = (json.dumps(env) + "\n").encode()
        if self.kind == "ws":
            mask = os.urandom(4); n = len(raw)
            hdr = bytes([0x81]) + (bytes([0x80 | n]) if n < 126 else bytes([0x80 | 126]) + struct.pack(">H", n) if n < 65536 else bytes([0x80 | 127]) + struct.pack(">Q", n))
            raw = hdr + mask + bytes(b ^ mask[i % 4] for i, b in enumerate(raw))
        with self.iolock:
            view = memoryview(raw)
            while view:
                try: view = view[self.sock.send(view):]
                except (socket.timeout, ssl.SSLWantWriteError, ssl.SSLWantReadError): time.sleep(0.005)

    def mark(self):
        with self.cv: return len(self.events)

    def wait(self, pred, since, timeout=8):
        end = time.time() + timeout
        with self.cv:
            while True:
                for e in self.events[since:]:
                    if pred(e): return e
                if self.closed or time.time() > end: return None
                self.cv.wait(0.2)

    def ask(self, typ, data, want, timeout=8, sid=None):
        """Send and return (reply_data, error_message): the first `want` event or error after sending."""
        m = self.mark(); self.send(typ, data, sid)
        w = {want} if isinstance(want, str) else set(want)
        e = self.wait(lambda e: e["type"] in w or e["type"] == "error", m, timeout)
        if e is None: return None, "no reply" + (" (connection closed)" if self.closed else "")
        if e["type"] == "error": return None, (e.get("data") or {}).get("message", "error")
        return e.get("data") or {}, None

    def close(self):
        self.closed = True
        with self.iolock:
            try: self.sock.close()
            except Exception: pass

class Server:
    def __init__(self):
        self.dir = WORK; os.makedirs(self.dir, exist_ok=True)
        # a unix socket path must be short, so it lives in the runtime dir rather than the work dir
        self.sock_dir = tempfile.mkdtemp(prefix="mx", dir=os.environ.get("XDG_RUNTIME_DIR", "/tmp"))
        self.sock_path = os.path.join(self.sock_dir, "s.sock"); self.port = free_port()
        env = dict(os.environ, XDG_CACHE_HOME=os.path.join(self.dir, "cache"))
        self.proc = subprocess.Popen([BIN, "-state-dir", os.path.join(self.dir, "state"), "-listen", "unix:" + self.sock_path, "-project", self.dir],
                                     env=env, stdout=open(os.path.join(self.dir, "server.log"), "w"), stderr=subprocess.STDOUT)
        for _ in range(100):
            if os.path.exists(self.sock_path): break
            time.sleep(0.1)
        self.decisions = {}            # credential -> allow | full | decline | ignore
        self.admin_lock = threading.Lock()
        self.admin = Conn("unix", self.sock_path); self.admin.on_event = self._approver
        m = self.admin.mark()
        self.admin.send("set_remote_access", {"enabled": True, "addr": f"127.0.0.1:{self.port}"})
        for _ in range(100):
            try: Conn("tls", self.port).close(); break
            except Exception: time.sleep(0.1)

    def _approver(self, conn, env):
        if env["type"] != "pairing_request": return
        cred = env["data"]["credential"]; how = self.decisions.get(cred, "allow")
        if how == "ignore": return
        conn.send("approve_pairing", {"credential": cred, "allow": how != "decline", "full": how == "full"})

    def admin_ask(self, *a, **k):
        with self.admin_lock: return self.admin.ask(*a, **k)

    def stop(self):
        self.proc.terminate()
        try: self.proc.wait(5)
        except Exception: self.proc.kill()
        import shutil; shutil.rmtree(self.sock_dir, ignore_errors=True)

SRV = None
def uniq(p): return f"{p}-{uuid.uuid4().hex[:6]}"

def register(sender, body, decision="allow"):
    """Register a pairing token through `sender` (a Conn, or None for the owner on the local socket)."""
    body = dict(body); body.setdefault("name", uniq("dev")); body.setdefault("pairing", True)
    SRV.decisions[body["name"]] = decision
    data, err = (SRV.admin_ask("register_token", body, "token_registered") if sender is None else sender.ask("register_token", body, "token_registered"))
    return body["name"], data, err

# The server drops connections beyond 32 that haven't authenticated yet (a deliberate guard), so the
# harness keeps its own sign-ins under that, and tests the guard separately.
PREAUTH = threading.Semaphore(16)

def signin(kind, token, device=None, wait=12):
    with PREAUTH:
        c = Conn(kind, SRV.port); m = c.mark()
        c.send("auth", {"token": token, "deviceId": device or uniq("dv"), "deviceType": "web", "name": uniq("name")})
        e = c.wait(lambda e: e["type"] in ("peer_list", "error"), m, wait)
    if e is None: return c, None, "no answer" + (" (closed)" if c.closed else "")
    if e["type"] == "error": return c, None, e["data"]["message"]
    me = next(p for p in e["data"]["peers"] if p["peerId"] == e["data"]["you"])
    return c, me, None

def pair(kind, sender, body, decision="allow"):
    name, data, err = register(sender, body, decision)
    if err: return None, None, None, "register: " + err
    c, me, err = signin(kind, data["token"])
    return c, me, data, err

def probe(c, ev, data):
    """Send one event and return the error it drew, or None. Success replies can be broadcasts that
    other tests set off, so instead of matching one, a cheap request after it marks the end: the
    server handles a connection's frames in order, so any error for the probe arrives before it."""
    m = c.mark(); c.send(ev, data); c.send("list_sessions", {})
    c.wait(lambda e: e["type"] == "session_list", m, 8)
    for e in c.events[m:]:
        if e["type"] == "error": return (e.get("data") or {}).get("message", "error")
    return None

def check(cond, msg):
    if not cond: raise AssertionError(msg)

# ---- cases -------------------------------------------------------------------------------------
CASES, SERIAL = [], []   # SERIAL cases run alone, after the parallel ones (they disturb the server's shared limits)
def case(name, serial=False):
    def deco(f): (SERIAL if serial else CASES).append((name, f)); return f
    return deco

def add_matrix():
    existing_holder = {}
    regs = [
        ("me", {"user": "me"}, "limited", "owner"),
        ("me+full", {"user": "me", "full": True}, "full", "owner"),
        ("me+ceiling-trusted", {"user": "me", "maxTier": "trusted"}, "trusted", "owner"),
        ("new-limited", {"user": "new", "newUserName": "Ada", "tier": "limited"}, "limited", None),
        ("new-trusted", {"user": "new", "newUserName": "Bea", "tier": "trusted"}, "trusted", None),
        ("new-full", {"user": "new", "newUserName": "Cy", "tier": "full"}, "full", None),
        ("existing-person", None, None, "existing"),
    ]
    for kind in ("tls", "ws"):
        for rname, body, tier, who in regs:
            for decision in ("allow", "full", "decline"):
                def run(kind=kind, rname=rname, body=body, tier=tier, who=who, decision=decision):
                    b = body
                    if rname == "existing-person":
                        n0, d0, e0 = register(None, {"user": "new", "newUserName": uniq("Dee"), "tier": "trusted"}, "allow")
                        check(not e0, "setup: " + str(e0))
                        b, tier, who = {"user": d0["userId"]}, "trusted", d0["userId"]
                    c, me, data, err = pair(kind, None, b, decision)
                    try:
                        if decision == "decline":
                            check(err and "declined" in err, f"decline should refuse with 'declined', got me={me} err={err}")
                            return
                        check(not err, f"sign-in refused: {err}")
                        want = "full" if decision == "full" else tier
                        check(me["tier"] == want, f"tier {me['tier']}, want {want}")
                        if who == "owner": check(me["userId"] == "owner", f"person {me['userId']}, want owner")
                        elif who: check(me["userId"] == who, f"person {me['userId']}, want {who}")
                        else: check(me["userId"] not in ("owner", ""), f"a new person was expected, got {me['userId']}")
                    finally:
                        if c: c.close()
                case(f"pair/{kind}/{rname}/{decision}")(run)

add_matrix()

@case("pair/no-approver-answer -> times out, then refuses")
def _():
    c, me, data, err = pair("tls", None, {"user": "new", "newUserName": "Slow", "tier": "limited"}, "ignore")
    check(me is None and err, f"an unanswered pairing must not admit: me={me} err={err}")
    c.close()

@case("guard/an idle flood doesn't lock out a real pairing afterwards", serial=True)
def _():
    idle = []
    try:
        for _ in range(40):
            try: idle.append(Conn("tls", SRV.port))
            except Exception: pass
        time.sleep(0.5)
    finally:
        for c in idle: c.close()
    time.sleep(1.5)
    c, me, data, err = pair("tls", None, {"user": "new", "newUserName": uniq("After"), "tier": "limited"})
    check(me, f"pairing after the flood: {err}")
    c.close()

@case("token/wrong token refused (tls)")
def _():
    c, me, err = signin("tls", secrets.token_hex(16), wait=5); check(me is None and err, f"admitted {me}"); c.close()

@case("token/wrong token refused (ws)")
def _():
    c, me, err = signin("ws", secrets.token_hex(16), wait=5); check(me is None and err, f"admitted {me}"); c.close()

@case("token/empty token refused")
def _():
    c, me, err = signin("tls", "", wait=5); check(me is None and err, f"admitted {me}"); c.close()

@case("token/same device may reconnect, another device may not")
def _():
    name, data, err = register(None, {"user": "new", "newUserName": uniq("Re"), "tier": "limited"}); check(not err, err)
    c1, me1, e1 = signin("tls", data["token"], device="dev-A"); check(me1, f"first device refused: {e1}"); c1.close()
    c2, me2, e2 = signin("tls", data["token"], device="dev-A"); check(me2, f"same device refused on reconnect: {e2}"); c2.close()
    c3, me3, e3 = signin("ws", data["token"], device="dev-B"); check(me3 is None and e3 and "already" in e3, f"second device: me={me3} err={e3}"); c3.close()

@case("token/one code, two devices at once: exactly one pairs")
def _():
    name, data, err = register(None, {"user": "new", "newUserName": uniq("Race"), "tier": "limited"}); check(not err, err)
    with cf.ThreadPoolExecutor(4) as ex:
        res = list(ex.map(lambda i: signin("tls" if i % 2 else "ws", data["token"], device=f"race-{i}"), range(4)))
    ok = [r for r in res if r[1]]
    for r in res: r[0].close()
    check(len(ok) == 1, f"{len(ok)} devices paired with one code: " + str([r[2] for r in res]))

@case("storm/24 pairings at once all succeed")
def _():
    def one(i):
        c, me, data, err = pair("tls" if i % 2 else "ws", None, {"user": "new", "newUserName": uniq("S"), "tier": "limited"})
        if c: c.close()
        return me, err
    with cf.ThreadPoolExecutor(24) as ex: res = list(ex.map(one, range(24)))
    bad = [e for m, e in res if not m]
    check(not bad, f"{len(bad)} of 24 failed: {bad[:3]}")

# Who may do what, by tier, over a real connection.
PROBES = [  # (label, event, data, wanted reply, minimum tier allowed)
    ("list_people", "list_people", {}, "people", "trusted"),
    ("list_problems", "list_problems", {}, "problem_list", "trusted"),
    ("set_user_permissions", "set_user_permissions", {"userId": "u-nobody", "tier": "limited"}, "people", "full"),
    ("remove_user", "remove_user", {"userId": "u-nobody"}, "people", "full"),
    ("set_remote_access", "set_remote_access", {"enabled": True}, "remote_access_set", "never"),
    ("terminal_open", "terminal_open", {"projectPath": "/tmp"}, "terminal_opened", "full"),
    ("get_avatar", "get_avatar", {"avatar": "upload:" + "a" * 64}, "avatar_data", "limited"),
    ("set_profile", "set_profile", {"name": "Probe"}, "profile_updated", "limited"),
    ("list_sessions", "list_sessions", {}, "session_list", "limited"),
]
for tier in ("limited", "trusted", "full"):
    def run(tier=tier):
        c, me, data, err = pair("tls", None, {"user": "new", "newUserName": uniq("P" + tier), "tier": tier})
        check(me, f"setup: {err}")
        try:
            wrong = []
            for label, ev, d, want, min_tier in PROBES:
                if tier == "full" and label in ("restart_server",): continue
                e = probe(c, ev, d)
                refused = bool(e and DENIED.search(e))
                should_allow = min_tier != "never" and TIERS[tier] >= TIERS.get(min_tier, 9)
                if should_allow == refused: wrong.append(f"{label}: {'refused' if refused else 'allowed'} ({e}), wanted {'allowed' if should_allow else 'refused'}")
            if tier == "limited":  # only the refusal is safe to probe: an allowed restart_server really restarts
                ev = c.ask("restart_server", {}, "x", timeout=3)[1]
                check(ev and DENIED.search(ev), f"restart_server not refused: {ev}")
            check(not wrong, "; ".join(wrong))
        finally: c.close()
    case(f"access/{tier} person over tls")(run)

@case("delegation/trusted may add limited people only; limited may add nobody")
def _():
    tc, tme, d, err = pair("tls", None, {"user": "new", "newUserName": uniq("Trustee"), "tier": "trusted"}); check(tme, err)
    lc, lme, d2, err2 = pair("tls", None, {"user": "new", "newUserName": uniq("Lim"), "tier": "limited"}); check(lme, err2)
    tc.on_event = lambda conn, env: conn.send("approve_pairing", {"credential": env["data"]["credential"], "allow": True}) if env["type"] == "pairing_request" else None
    try:
        res = {}
        for label, body in [("new-limited", {"user": "new", "newUserName": uniq("Kid"), "tier": "limited"}), ("new-trusted", {"user": "new", "newUserName": uniq("Peer"), "tier": "trusted"}),
                            ("new-full", {"user": "new", "newUserName": uniq("Boss"), "tier": "full"}), ("me", {"user": "me"}), ("full-flag", {"user": "new", "newUserName": uniq("F"), "full": True})]:
            nm, data, e = register(tc, body); res[label] = (data, e)
        check(res["new-limited"][0], f"trusted could not add a limited person: {res['new-limited'][1]}")
        for label in ("new-trusted", "new-full", "me", "full-flag"):
            check(res[label][1] and DENIED.search(res[label][1] or "") or res[label][1], f"trusted was allowed to register {label}: {res[label]}")
        # the new device pairs, and the Trusted person (not only the owner) can approve it
        c, me, e = signin("tls", res["new-limited"][0]["token"])
        check(me and me["tier"] == "limited", f"the limited person's device: {me} {e}"); c.close()
        nm, data, e = register(lc, {"user": "new", "newUserName": uniq("X")})
        check(e and DENIED.search(e), f"a limited person registered a device: {data} {e}")
    finally: tc.close(); lc.close()

@case("revoke/remove_token drops the device at once (tls and ws) and its token stops working")
def _():
    for kind in ("tls", "ws"):
        name, data, err = register(None, {"user": "new", "newUserName": uniq("Rv"), "tier": "limited"}); check(not err, err)
        c, me, e = signin(kind, data["token"]); check(me, e)
        SRV.admin_ask("remove_token", {"name": name}, "token_removed")
        end = time.time() + 4
        while not c.closed and time.time() < end: time.sleep(0.1)
        check(c.closed, f"{kind}: the revoked device stayed connected")
        c2, me2, e2 = signin(kind, data["token"], wait=4); check(me2 is None, f"{kind}: a revoked token signed in"); c2.close(); c.close()

@case("revoke/remove_user drops every device of that person")
def _():
    n1, d1, e1 = register(None, {"user": "new", "newUserName": uniq("Two"), "tier": "limited"}); check(not e1, e1)
    n2, d2, e2 = register(None, {"user": d1["userId"]}); check(not e2, e2)
    c1, m1, x1 = signin("tls", d1["token"]); c2, m2, x2 = signin("ws", d2["token"]); check(m1 and m2, f"{x1} {x2}")
    SRV.admin_ask("remove_user", {"userId": d1["userId"]}, "people")
    end = time.time() + 4
    while not (c1.closed and c2.closed) and time.time() < end: time.sleep(0.1)
    check(c1.closed and c2.closed, f"tls closed={c1.closed} ws closed={c2.closed}")

@case("live/lowering a device ceiling takes effect on the open connection")
def _():
    name, data, err = register(None, {"user": "new", "newUserName": uniq("Lo"), "tier": "trusted"}); check(not err, err)
    c, me, e = signin("tls", data["token"]); check(me and me["tier"] == "trusted", f"{me} {e}")
    reply, err = c.ask("list_people", {}, "people"); check(not err, f"trusted could not list people: {err}")
    SRV.admin_ask("set_device_restrictions", {"credential": name, "maxTier": "limited"}, "people")
    time.sleep(0.5)
    reply, err = c.ask("list_people", {}, "people"); check(err and DENIED.search(err), f"still allowed after lowering: {err}")
    c.close()

@case("avatar/set_profile stores a picture; get_avatar returns it on a second device")
def _():
    png = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="
    n, d, e = register(None, {"user": "new", "newUserName": uniq("Pic"), "tier": "limited"}); check(not e, e)
    n2, d2, e2 = register(None, {"user": d["userId"]}); check(not e2, e2)
    c1, m1, x1 = signin("tls", d["token"]); c2, m2, x2 = signin("ws", d2["token"]); check(m1 and m2, f"{x1} {x2}")
    m = c2.mark()
    prof, err = c1.ask("set_profile", {"avatar": "data:image/png;base64," + png}, "profile_updated"); check(not err, err)
    check(prof["avatar"].startswith("upload:"), f"avatar not in the bucket: {prof['avatar'][:30]}")
    pu = c2.wait(lambda e: e["type"] == "profile_updated", m, 4); check(pu, "the person's other device wasn't told")
    got, err = c2.ask("get_avatar", {"avatar": prof["avatar"]}, "avatar_data"); check(not err and got.get("data", "").endswith(png), f"get_avatar: {err} {str(got)[:60]}")
    bad, err = c1.ask("set_profile", {"avatar": "data:text/html;base64,PHNjcmlwdD4="}, "profile_updated"); check(err, "a non-image avatar was accepted")
    c1.close(); c2.close()

# ---- run ---------------------------------------------------------------------------------------
def main():
    global SRV
    SRV = Server(); t0 = time.time(); results = []
    def run(item):
        name, fn = item; t = time.time()
        try: fn(); return name, True, "", time.time() - t
        except AssertionError as e: return name, False, str(e), time.time() - t
        except Exception as e: return name, False, f"{type(e).__name__}: {e}", time.time() - t
    try:
        with cf.ThreadPoolExecutor(int(os.environ.get("JOBS", "12"))) as ex: results = list(ex.map(run, CASES))
        results += [run(c) for c in SERIAL]
    finally: SRV.stop()
    bad = [r for r in results if not r[1]]
    for n, ok, msg, dt in sorted(results, key=lambda r: (r[1], r[0])):
        print(("PASS " if ok else "FAIL ") + n + (f"  [{dt:.1f}s]" if ok else f"\n      -> {msg}"))
    print(f"\n{len(results) - len(bad)}/{len(results)} passed in {time.time() - t0:.0f}s   (server log: {WORK}/server.log)")
    sys.exit(1 if bad else 0)

if __name__ == "__main__": main()
