#!/usr/bin/env python3
"""Isolated Docker stop-deadline test for the M3.1 safety closure.

Runs a SEPARATE container (never the operator's deployment) against a loopback
mock backend, with the same restrictions as compose.yaml, keeps requests active
(a shared query that never finishes, a client that never reads, a client that
reads very slowly), sends Docker's normal stop (SIGTERM) with the 45 s
stop_grace_period, and records the actual stop duration and exit status.
A SIGKILL (exit status 137) caused by exceeding the deadline is a failure.

Requires Docker, the image given as argv[1] (default hextet-lg:closure-test)
and free loopback ports 18081 and 19098.
"""
import http.server, json, os, pathlib, socket, subprocess, sys, tempfile, threading, time, urllib.request, uuid

IMAGE = sys.argv[1] if len(sys.argv) > 1 else 'hextet-lg:closure-test'
NAME = 'hextet-stop-' + uuid.uuid4().hex[:12]
STOP_TIMEOUT = 45          # compose.yaml stop_grace_period
LISTEN, MOCK = 18081, 19098
LONG_PATH = ' '.join(['64500'] * 2000)
BIG = json.dumps({'rib': [{'prefix': f'10.{i >> 8}.{i & 255}.0/24', 'aspath': LONG_PATH, 'true_nexthop': '192.0.2.1',
                           'exit_nexthop': '192.0.2.1', 'valid': True, 'best': True, 'ovs': 'valid', 'avs': 'unknown',
                           'origin': 'igp', 'localpref': 100, 'communities': ['64500:1']} for i in range(1000)]}).encode()
events = []
events_lock = threading.Lock()
T0 = time.monotonic()


def note(msg):
    with events_lock:
        events.append((time.monotonic() - T0, msg))
    print('%7.2fs  %s' % (time.monotonic() - T0, msg), flush=True)


class Mock(http.server.BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'

    def do_GET(self):
        if self.path.startswith('/rib?as=64500'):
            # The shared query that never finishes: stream the opening and hold.
            self.send_response(200); self.send_header('Content-Type', 'application/json'); self.end_headers()
            self.wfile.write(b'{"rib":['); self.wfile.flush()
            note('mock: hung shared query started')
            try:
                while True:
                    time.sleep(0.25)
                    self.wfile.write(b' '); self.wfile.flush()
            except (BrokenPipeError, ConnectionResetError, OSError):
                note('mock: hung shared query was CANCELED by the application (connection closed)')
            return
        body = BIG if self.path.startswith('/rib') else b'{}'
        self.send_response(200); self.send_header('Content-Length', str(len(body))); self.end_headers()
        try:
            self.wfile.write(body)
        except OSError:
            pass

    def log_message(self, *a):
        pass


class Server(http.server.ThreadingHTTPServer):
    daemon_threads = True


def http_get(path, timeout=60):
    try:
        with urllib.request.urlopen('http://127.0.0.1:%d%s' % (LISTEN, path), timeout=timeout) as r:
            return r.status, r.read()
    except Exception as e:
        return 'error', repr(e).encode()


def raw_connection(path, rcvbuf):
    s = socket.socket()
    s.setsockopt(socket.SOL_SOCKET, socket.SO_RCVBUF, rcvbuf)
    s.connect(('127.0.0.1', LISTEN))
    s.sendall(('GET %s HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n' % path).encode())
    return s


def main():
    with socket.socket() as probe:
        probe.bind(('127.0.0.1', LISTEN))
    srv = Server(('127.0.0.1', MOCK), Mock)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    cfg = f'''server:
  listen: "127.0.0.1:{LISTEN}"
  title: "Stop test"
  rate_per_second: 10000
  rate_burst: 10000
  live_rate_per_second: 10000
  live_rate_burst: 10000
  shutdown_grace: 40s
looking_glass:
  default_backend: all
  query_timeout: 120s
  cache_ttl: 120s
  page_size: 1000
backends:
  - id: mock
    name: Mock
    location: Test
    url: "http://127.0.0.1:{MOCK}"
    enabled: true
'''
    verdict = 'FAIL'
    with tempfile.TemporaryDirectory(prefix='hextet-stop-') as directory:
        os.chmod(directory, 0o755)
        path = pathlib.Path(directory) / 'config.yaml'
        path.write_text(cfg); path.chmod(0o644)
        try:
            subprocess.run(['docker', 'run', '-d', '--name', NAME, '--network', 'host', '--no-healthcheck',
                            '--memory', '2g', '--pids-limit', '128', '--read-only', '--tmpfs', '/tmp:size=8m,mode=1777',
                            '--security-opt', 'no-new-privileges:true', '--cap-drop', 'ALL',
                            '-v', str(path) + ':/etc/hextet-lg/config.yaml:ro', IMAGE], check=True, stdout=subprocess.DEVNULL)
            for _ in range(100):
                if http_get('/healthz', 1)[0] == 200:
                    break
                time.sleep(0.1)
            else:
                raise RuntimeError('test container failed to start')
            # Warm the cache with a large complete result so renders hit backpressure.
            status, body = http_get('/?as=64501&view=detail&output=plain')
            note('warm request: status=%s bytes=%d' % (status, len(body)))
            assert status == 200 and len(body) > 4 << 20, 'warm request must be a large complete response'

            # Active requests: two clients sharing the hung query, one blocked
            # reader, one extremely slow reader (progress, but never finishing).
            for target in ('/?as=64500&view=text&output=plain', '/?as=64500&view=table'):
                threading.Thread(target=lambda t=target: note('shared client ended: %s' % str(http_get(t, 120)[0])), daemon=True).start()
            blocked = raw_connection('/?as=64501&view=detail&output=plain', 2048)
            slow = raw_connection('/?as=64501&view=detail', 4096)
            stop_slow = threading.Event()

            def drip():
                n = 0
                while not stop_slow.is_set():
                    try:
                        d = slow.recv(512); n += len(d)
                        if not d:
                            break
                    except OSError:
                        break
                    time.sleep(0.5)
                note('slow client ended after %d bytes' % n)
            threading.Thread(target=drip, daemon=True).start()
            time.sleep(2)
            note('active requests established (2 shared, 1 blocked reader, 1 slow reader); sending docker stop -t %d' % STOP_TIMEOUT)

            start = time.monotonic()
            subprocess.run(['docker', 'stop', '-t', str(STOP_TIMEOUT), NAME], check=True, stdout=subprocess.DEVNULL)
            duration = time.monotonic() - start
            stop_slow.set()
            inspect = json.loads(subprocess.check_output(['docker', 'inspect', NAME]))[0]['State']
            logs = subprocess.run(['docker', 'logs', '--tail', '5', NAME], capture_output=True, text=True)
            note('docker stop returned after %.1fs' % duration)
            print('exit_code', inspect['ExitCode'], 'oom_killed', inspect['OOMKilled'], 'status', inspect['Status'], flush=True)
            print('container log tail:\n' + (logs.stdout + logs.stderr).strip(), flush=True)
            grace = 40
            ok = inspect['ExitCode'] == 0 and not inspect['OOMKilled'] and duration < STOP_TIMEOUT
            print('stop_duration_seconds %.1f configured_stop_grace %d app_shutdown_grace %d' % (duration, STOP_TIMEOUT, grace), flush=True)
            if inspect['ExitCode'] == 137:
                print('SIGKILL: the application did not exit within the stop deadline', flush=True)
            verdict = 'PASS' if ok else 'FAIL'
        finally:
            subprocess.run(['docker', 'rm', '-f', NAME], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
            srv.shutdown(); srv.server_close()
    print('docker_stop_verdict', verdict, flush=True)
    sys.exit(0 if verdict == 'PASS' else 1)


if __name__ == '__main__':
    main()
