"""Two `catflap proxy` sessions at once: both must keep working.

usage: concurrent.py CATFLAP ALIAS [ALIAS2]   (env selects the client dir;
       with ALIAS2, session B goes to that homelab)
Prints "ok" or what failed. The far end is the e2e fake sshd (banner + echo).
"""
import subprocess, sys, threading

cf, alias = sys.argv[1], sys.argv[2]
alias_b = sys.argv[3] if len(sys.argv) > 3 else alias

def start(alias=alias):
    p = subprocess.Popen([cf, "proxy", alias, "22"], stdin=subprocess.PIPE,
                         stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
    return p

def readline(p, timeout=30):
    out = []
    t = threading.Thread(target=lambda: out.append(p.stdout.readline()))
    t.start(); t.join(timeout)
    return out[0].decode().strip() if out else None

def echo(p, word):
    p.stdin.write(word.encode() + b"\n"); p.stdin.flush()
    return readline(p)

a = start()
if not (readline(a) or "").startswith("SSH-"):
    print("session A got no banner"); sys.exit(1)
b = start(alias_b)
if not (readline(b) or "").startswith("SSH-"):
    print("session B got no banner"); sys.exit(1)
res = (echo(b, "bee"), echo(a, "ay"), echo(b, "bee2"), echo(a, "ay2"))
for p in (a, b):
    p.kill()
print("ok" if res == ("bee", "ay", "bee2", "ay2") else f"echo mismatch {res}")
