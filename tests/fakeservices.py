#!/usr/bin/env python3
"""Fake Telegram Bot API, ntfy and Pushover for tests/e2e.sh.

  fakeservices.py serve PORTFILE         run the fake on 127.0.0.1 (port written to PORTFILE)
  fakeservices.py URL update CHAT TEXT   a Telegram user (id CHAT) sends TEXT to the bot
  fakeservices.py URL button CHAT DATA   ... taps an inline button with callback DATA
  fakeservices.py URL has SVC TEXT [--now]   exit 0 once a request to SVC (tg, ntfy,
                                         pushover) contains TEXT (waits up to 10 s)
  fakeservices.py URL invite             print the last invite code the bot sent
  fakeservices.py URL callback PREFIX    print the last button callback starting with PREFIX

Telegram lives under /tg (Bot API URL), ntfy under /ntfy (server URL),
Pushover at /pushover.
"""
import json, re, sys, threading, time, urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs

lock = threading.Lock()
log = []          # every request: {"svc", "method", "body"}
updates = []      # pending Telegram updates
next_update = [1]
next_msg = [100]


def tg_result(method, body):
    if method == "getMe":
        return {"id": 1, "is_bot": True, "username": "testbot", "first_name": "test"}
    if method == "getUpdates":
        offset, timeout = body.get("offset", 0), min(body.get("timeout", 0), 2)
        deadline = time.time() + timeout
        while True:
            with lock:
                if offset:  # confirms everything before offset
                    updates[:] = [u for u in updates if u["update_id"] >= offset]
                if updates or time.time() >= deadline:
                    return list(updates)
            time.sleep(0.05)
    if method == "sendMessage":
        with lock:
            next_msg[0] += 1
            return {"message_id": next_msg[0], "chat": {"id": body.get("chat_id")}, "date": int(time.time())}
    return True


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def reply(self, code, obj):
        b = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)

    def do_GET(self):
        if self.path == "/test/log":
            with lock:
                return self.reply(200, log)
        self.reply(404, {})

    def do_POST(self):
        raw = self.rfile.read(int(self.headers.get("Content-Length", 0)))
        if self.path == "/test/update":
            u = json.loads(raw)
            user = {"id": u["chat"], "is_bot": False, "username": "u%d" % u["chat"], "first_name": "u"}
            msg = {"message_id": 1, "from": user, "chat": {"id": u["chat"], "type": "private"},
                   "date": int(time.time()), "text": u.get("text", "")}
            with lock:
                upd = {"update_id": next_update[0]}
                next_update[0] += 1
                if "button" in u:
                    upd["callback_query"] = {"id": "cb%d" % upd["update_id"], "from": user, "message": msg, "data": u["button"]}
                else:
                    upd["message"] = msg
                updates.append(upd)
            return self.reply(200, {})
        m = re.match(r"^/tg/bot[^/]+/(\w+)$", self.path)
        if m:
            body = json.loads(raw or b"{}")
            with lock:
                log.append({"svc": "tg", "method": m.group(1), "body": body})
            return self.reply(200, {"ok": True, "result": tg_result(m.group(1), body)})
        if self.path.startswith("/ntfy"):
            with lock:
                log.append({"svc": "ntfy", "method": "publish", "body": json.loads(raw)})
            return self.reply(200, {"id": "x"})
        if self.path == "/pushover":
            body = {k: v[0] for k, v in parse_qs(raw.decode()).items()}
            with lock:
                log.append({"svc": "pushover", "method": "messages", "body": body})
            return self.reply(200, {"status": 1})
        self.reply(404, {})


def get_log(url):
    return json.load(urllib.request.urlopen(url + "/test/log"))


def post(url, obj):
    req = urllib.request.Request(url + "/test/update", json.dumps(obj).encode(), {"Content-Type": "application/json"})
    urllib.request.urlopen(req).read()


def main():
    if sys.argv[1] == "serve":
        s = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        open(sys.argv[2], "w").write(str(s.server_address[1]))
        s.serve_forever()
    url, cmd, args = sys.argv[1], sys.argv[2], sys.argv[3:]
    if cmd == "update":
        post(url, {"chat": int(args[0]), "text": args[1]})
    elif cmd == "button":
        post(url, {"chat": int(args[0]), "button": args[1]})
    elif cmd == "has":
        svc, text = args[0], args[1]
        deadline = time.time() + (0 if "--now" in args else 10)
        while True:
            if any(e["svc"] == svc and text in json.dumps(e["body"], ensure_ascii=False) for e in get_log(url)):
                sys.exit(0)
            if time.time() >= deadline:
                sys.exit(1)
            time.sleep(0.1)
    elif cmd == "invite":
        codes = re.findall(r"cfi1-[^\s<\"]+", json.dumps([e["body"] for e in get_log(url) if e["svc"] == "tg"]))
        print(codes[-1] if codes else "")
    elif cmd == "callback":
        found = ""
        for e in get_log(url):
            for row in e["body"].get("reply_markup", {}).get("inline_keyboard", []) if e["svc"] == "tg" else []:
                for b in row:
                    if b["callback_data"].startswith(args[0]):
                        found = b["callback_data"]
        print(found)
    else:
        sys.exit("unknown command " + cmd)


main()
