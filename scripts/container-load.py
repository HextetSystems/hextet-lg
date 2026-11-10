#!/usr/bin/env python3
"""Isolated Linux host-network mock load; requires Docker and free ports 18080/19099.

Runs a separate container with a configuration containing only the loopback mock.
Never points the workload at the operator's deployment.
"""
import collections, concurrent.futures, http.server, json, os, pathlib, socket, subprocess, tempfile, threading, time, urllib.error, urllib.request, uuid

DURATION = 305
NAME = 'hextet-load-' + uuid.uuid4().hex[:12]
DOC = json.dumps({'rib': [{'prefix': f'10.{i >> 8}.{i & 255}.0/24', 'aspath': '64500 65000', 'true_nexthop': '192.0.2.1', 'exit_nexthop': '192.0.2.1', 'valid': True, 'best': True, 'ovs': 'valid', 'avs': 'unknown', 'origin': 'igp', 'localpref': 100, 'communities': ['64500:1'], 'large_communities': ['64500:1:1']} for i in range(10000)]}).encode()
backend_counts = collections.Counter()
backend_lock = threading.Lock()

class Mock(http.server.BaseHTTPRequestHandler):
 def do_GET(self):
  with backend_lock: backend_counts['requests'] += 1
  body = DOC if self.path.startswith('/rib') else b'{}'
  self.send_response(200); self.send_header('Content-Length', str(len(body))); self.end_headers()
  try: self.wfile.write(body)
  except (BrokenPipeError, ConnectionResetError):
   with backend_lock: backend_counts['disconnected'] += 1
 def log_message(self, *args): pass
class Server(http.server.ThreadingHTTPServer): daemon_threads = True

def request(i):
 targets = ['/?as=64500&view=text&output=plain', '/?as=%d&view=table' % (64501 + i % 80), '/summary', '/neighbors', '/?as=64500&view=table']
 try:
  with urllib.request.urlopen('http://127.0.0.1:18080' + targets[i % len(targets)], timeout=12) as r:
   completeness = r.headers.get('X-Hextet-LG-Completeness', '')
   if i % 12 == 0:
    r.read(128)
    return str(r.status), completeness, 'client_cancelled'
   if i % 12 == 1:
    while r.read(8192): time.sleep(.02)
   else:
    while r.read(65536): pass
   return str(r.status), completeness, r.headers.get('Retry-After', '')
 except urllib.error.HTTPError as e: return str(e.code), '', e.headers.get('Retry-After','')
 except Exception: return 'error', '', ''

def main():
 # Refuse to run if the test listener is already in use.
 with socket.socket() as probe:
  probe.bind(('127.0.0.1', 18080))
 server = Server(('127.0.0.1', 19099), Mock)
 threading.Thread(target=server.serve_forever, daemon=True).start()
 cfg = '''server:\n  listen: "127.0.0.1:18080"\n  title: "Load test"\n  rate_per_second: 10000\n  rate_burst: 10000\n  live_rate_per_second: 10000\n  live_rate_burst: 10000\nlooking_glass:\n  default_backend: all\nbackends:\n  - id: mock\n    name: Mock\n    location: Test\n    url: "http://127.0.0.1:19099"\n    enabled: true\n'''
 with tempfile.TemporaryDirectory(prefix='hextet-load-') as directory:
  os.chmod(directory, 0o755)
  path = pathlib.Path(directory)/'config.yaml'; path.write_text(cfg); path.chmod(0o644)
  try:
   subprocess.run(['docker','run','-d','--name',NAME,'--network','host','--no-healthcheck','--memory','2g','--pids-limit','128','--read-only','--tmpfs','/tmp:size=8m,mode=1777','-v',str(path)+':/etc/hextet-lg/config.yaml:ro','hextet-lg:local'],check=True,stdout=subprocess.DEVNULL)
   for _ in range(100):
    try:
     with urllib.request.urlopen('http://127.0.0.1:18080/healthz',timeout=1) as r:
      if r.status == 200: break
    except Exception: time.sleep(.1)
   else: raise RuntimeError('test container failed to start')
   start=time.monotonic(); counts=collections.Counter(); peak=0; samples=[]; i=0
   with concurrent.futures.ThreadPoolExecutor(max_workers=24) as pool:
    pending=set()
    while time.monotonic()-start < DURATION:
     while len(pending)<24:
      pending.add(pool.submit(request,i)); i+=1
     done,pending=concurrent.futures.wait(pending,timeout=1,return_when=concurrent.futures.FIRST_COMPLETED)
     for f in done:
      status, completeness, retry = f.result(); counts[status]+=1
      if completeness: counts['completeness_'+completeness]+=1
      if status=='503' and retry: counts['503_retry_after']+=1
      if retry=='client_cancelled': counts['client_cancellations']+=1
     if not samples or time.monotonic()-samples[-1][0]>=5:
      raw=subprocess.check_output(['docker','stats','--no-stream','--format','{{.MemUsage}}',NAME],text=True).strip(); samples.append((time.monotonic(),raw))
    for f in concurrent.futures.as_completed(pending):
     status, completeness, retry=f.result(); counts[status]+=1
     if completeness: counts['completeness_'+completeness]+=1
     if status=='503' and retry: counts['503_retry_after']+=1
     if retry=='client_cancelled': counts['client_cancellations']+=1
   print('duration_seconds',round(time.monotonic()-start,1),'requests',sum(v for k,v in counts.items() if k.isdigit() or k=='error'), 'counts',dict(counts),flush=True)
   print('container_memory_samples', [x[1] for x in samples],flush=True)
   print('process_memory',subprocess.check_output(['docker','exec',NAME,'sh','-c',"grep -E 'VmRSS|VmHWM' /proc/1/status"],text=True).strip(),flush=True)
   print('backend_counts', dict(backend_counts), flush=True)
   print('unavailable_metrics: active decodes, admitted work, admission waiters, accounted retained memory, cache memory and Go heap are not exported by the production process; backend failures beyond observed disconnects are not instrumented', flush=True)
   print('container_state',subprocess.check_output(['docker','inspect','--format','{{.State.OOMKilled}} {{.State.Status}}',NAME],text=True).strip(),flush=True)
   with urllib.request.urlopen('http://127.0.0.1:18080/healthz',timeout=3) as r: print('healthz',r.status,flush=True)
  finally:
   subprocess.run(['docker','rm','-f',NAME],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
   server.shutdown(); server.server_close()
if __name__=='__main__': main()
