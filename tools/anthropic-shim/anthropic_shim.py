#!/usr/bin/env python3
"""anthropic-shim: make 9router's /v1/messages answer non-streaming requests in
the Anthropic Messages format.

Why. Measured 2026-09-13 with 9router 0.5.75: for oc/muse-spark-1.3-contributor-free,
a non-streaming /v1/messages request returns an OpenAI `chat.completion` object with
empty content and no `usage`, while the same request with `stream: true` returns
correct Anthropic SSE. oc/mimo-v2.5-free behind the same router answers both, so
the fault is specific to muse-spark. Claude Code's auto-mode safety classifier sends
non-streaming requests and reads `usage.input_tokens`; with muse it fails, and auto
mode then denies every tool call that needs classification.

What. A non-streaming POST to /v1/messages is sent upstream with `stream: true`, and
the SSE events are assembled into one Anthropic message. Every other request passes
through unchanged, streamed as it arrives.

    run:   python3 anthropic_shim.py --upstream http://localhost:20128 --port 20198
    test:  python3 anthropic_shim.py --self-test
    use:   export ANTHROPIC_BASE_URL=http://127.0.0.1:20198

It listens on 127.0.0.1 only, forwards the client's own credentials untouched, and
logs request shape and sizes to stderr — never message content, never credentials.
"""

import argparse
import http.client
import http.server
import json
import sys
import threading
import time
from urllib.parse import urlsplit

# Hop-by-hop and length headers are recomputed here. accept-encoding is dropped so
# the upstream answers uncompressed and the SSE can be read.
HOP = {"connection", "keep-alive", "transfer-encoding", "content-length", "host", "accept-encoding"}


def log(**fields):
    fields["t"] = time.strftime("%Y-%m-%dT%H:%M:%S")
    print(json.dumps(fields, ensure_ascii=False), file=sys.stderr, flush=True)


def assemble(sse_text):
    """Assemble Anthropic SSE events into one message.

    Returns (message, None), or (None, error) when the stream carried an error
    event, so a failure upstream is reported as a failure rather than as an
    empty answer.
    """
    message = {"type": "message", "role": "assistant", "content": [],
               "stop_reason": None, "stop_sequence": None,
               "usage": {"input_tokens": 0, "output_tokens": 0}}
    blocks, partial_json = {}, {}
    for line in sse_text.splitlines():
        if not line.startswith("data:"):
            continue
        payload = line[5:].strip()
        if not payload.startswith("{"):
            continue
        try:
            ev = json.loads(payload)
        except ValueError:
            continue
        kind = ev.get("type")
        if kind == "error":
            return None, ev.get("error") or {"type": "api_error", "message": "error event in upstream stream"}
        if kind == "message_start":
            start = ev.get("message") or {}
            for key in ("id", "model"):
                if key in start:
                    message[key] = start[key]
            message["usage"].update(start.get("usage") or {})
        elif kind == "content_block_start":
            blocks[ev.get("index", len(blocks))] = dict(ev.get("content_block") or {})
        elif kind == "content_block_delta":
            index = ev.get("index", 0)
            block = blocks.setdefault(index, {"type": "text", "text": ""})
            delta = ev.get("delta") or {}
            dt = delta.get("type")
            if dt == "text_delta":
                block["text"] = block.get("text", "") + delta.get("text", "")
            elif dt == "thinking_delta":
                block["thinking"] = block.get("thinking", "") + delta.get("thinking", "")
            elif dt == "signature_delta":
                block["signature"] = block.get("signature", "") + delta.get("signature", "")
            elif dt == "input_json_delta":
                partial_json[index] = partial_json.get(index, "") + delta.get("partial_json", "")
        elif kind == "message_delta":
            delta = ev.get("delta") or {}
            message["stop_reason"] = delta.get("stop_reason", message["stop_reason"])
            message["stop_sequence"] = delta.get("stop_sequence", message["stop_sequence"])
            message["usage"].update(ev.get("usage") or {})
    for index, raw in partial_json.items():
        if index in blocks:
            try:
                blocks[index]["input"] = json.loads(raw) if raw else {}
            except ValueError:
                blocks[index]["input"] = {}
    message["content"] = [blocks[i] for i in sorted(blocks)]
    return message, None


def is_empty_reply(message):
    """A complete reply with nothing in it: no text, no tool call, and no usage at all.

    muse-spark returns this for about 1 in 8 calls. A real answer always carries
    usage, so zero usage is what separates this from a legitimately short reply.
    """
    has_answer = any(b.get("type") in ("text", "tool_use") for b in message["content"])
    usage = message["usage"]
    return not has_answer and not usage.get("input_tokens") and not usage.get("output_tokens")


def anthropic_error(message, kind="api_error"):
    return {"type": "error", "error": {"type": kind, "message": f"anthropic-shim: {message}"}}


class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def _send_json(self, status, obj):
        data = json.dumps(obj).encode()
        self.send_response(status)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def _connect(self):
        up = self.server.upstream
        cls = http.client.HTTPSConnection if up.scheme == "https" else http.client.HTTPConnection
        return cls(up.hostname, up.port, timeout=self.server.timeout_s)

    def _handle(self):
        length = int(self.headers.get("content-length") or 0)
        body = self.rfile.read(length) if length else b""
        headers = {k: v for k, v in self.headers.items() if k.lower() not in HOP}

        convert, model = False, None
        if self.command == "POST" and self.path.split("?", 1)[0].rstrip("/").endswith("/v1/messages") and body:
            try:
                req = json.loads(body)
            except ValueError:
                req = None
            if isinstance(req, dict) and not req.get("stream"):
                req["stream"] = True
                body = json.dumps(req).encode()
                convert, model = True, req.get("model")

        started = time.monotonic()
        if convert:
            try:
                self._convert(body, headers, model, started)
            except (BrokenPipeError, ConnectionResetError):
                pass  # the client went away; nothing left to answer
            return

        conn = None
        try:
            conn = self._connect()
            conn.request(self.command, self.server.upstream_prefix + self.path, body=body, headers=headers)
            resp = conn.getresponse()
        except Exception as exc:  # the upstream is down or unreachable
            log(event="upstream_unreachable", error=str(exc))
            if conn:
                conn.close()
            return self._send_json(502, anthropic_error(f"upstream unreachable: {exc}"))

        try:
            self._pass_through(resp)
        except (BrokenPipeError, ConnectionResetError):
            pass  # the client went away; nothing left to answer
        finally:
            conn.close()

    def _fetch(self, body, headers):
        """One buffered upstream round trip: (status, headers, body)."""
        conn = self._connect()
        try:
            conn.request("POST", self.server.upstream_prefix + self.path, body=body, headers=headers)
            resp = conn.getresponse()
            return resp.status, resp.getheaders(), resp.read()
        finally:
            conn.close()

    def _convert(self, body, headers, model, started):
        # One retry of an empty reply (is_empty_reply). Without it Claude Code's
        # classifier gets no verdict from its first stage and runs a second one,
        # which costs several seconds on every classified action.
        message = error = None
        for attempt in (1, 2):
            try:
                status, up_headers, raw = self._fetch(body, headers)
            except Exception as exc:  # the upstream is down or unreachable
                log(event="upstream_unreachable", error=str(exc))
                return self._send_json(502, anthropic_error(f"upstream unreachable: {exc}"))
            if status != 200 or raw.lstrip().startswith(b"{"):
                break
            message, error = assemble(raw.decode("utf-8", "replace"))
            if error is None and attempt == 1 and is_empty_reply(message):
                log(event="retry", reason="empty reply with zero usage", model=model)
                continue
            break
        ms = int((time.monotonic() - started) * 1000)

        if status != 200 or raw.lstrip().startswith(b"{"):
            # An upstream error, or an upstream that ignored stream:true and
            # answered JSON itself: hand it back as it is rather than guess.
            log(event="converted", status=status, ms=ms, model=model, passthrough=True)
            self.send_response(status)
            for k, v in up_headers:
                if k.lower() not in HOP:
                    self.send_header(k, v)
            self.send_header("content-length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)
            return

        if error is not None:
            log(event="converted", status=500, ms=ms, model=model, upstream_error=error.get("type"))
            return self._send_json(500, {"type": "error", "error": error})
        if "id" not in message and not message["content"]:
            log(event="converted", status=502, ms=ms, model=model, note="no stream events")
            return self._send_json(502, anthropic_error("upstream returned 200 without Anthropic stream events"))

        log(event="converted", status=200, ms=ms, attempts=attempt, model=message.get("model", model),
            blocks=[b.get("type") for b in message["content"]],
            text_chars=sum(len(b.get("text", "")) for b in message["content"] if b.get("type") == "text"),
            stop_reason=message["stop_reason"], output_tokens=message["usage"].get("output_tokens"))
        self._send_json(200, message)

    def _pass_through(self, resp):
        self.send_response(resp.status)
        for k, v in resp.getheaders():
            if k.lower() not in HOP:
                self.send_header(k, v)
        self.send_header("transfer-encoding", "chunked")
        self.end_headers()
        while True:
            chunk = resp.read1(65536)
            if not chunk:
                break
            self.wfile.write(b"%x\r\n%s\r\n" % (len(chunk), chunk))
            self.wfile.flush()
        self.wfile.write(b"0\r\n\r\n")
        self.wfile.flush()

    do_GET = do_POST = do_PUT = do_PATCH = do_DELETE = _handle


class Server(http.server.ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self, address, upstream, timeout_s):
        super().__init__(address, Handler)
        self.upstream = urlsplit(upstream)
        self.upstream_prefix = self.upstream.path.rstrip("/")
        self.timeout_s = timeout_s


# ---------------------------------------------------------------------------
# Self-test


def _sse(events):
    return "".join(f"event: {e['type']}\ndata: {json.dumps(e)}\n\n" for e in events)


# The shape of a real 9router stream for muse-spark (captured 2026-09-13).
TEXT_STREAM = _sse([
    {"type": "message_start", "message": {"id": "msg_1", "type": "message", "role": "assistant",
                                          "model": "muse-spark-1.3-contributor-free(xhigh)", "content": [],
                                          "usage": {"input_tokens": 2010, "output_tokens": 0}}},
    {"type": "content_block_start", "index": 0, "content_block": {"type": "text", "text": ""}},
    {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": "O"}},
    {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": "K"}},
    {"type": "content_block_stop", "index": 0},
    {"type": "message_delta", "delta": {"stop_reason": "end_turn", "stop_sequence": None},
     "usage": {"output_tokens": 12}},
    {"type": "message_stop"},
])


# A complete stream with only a thinking block and zero usage: what muse-spark
# returns for about 1 in 8 direct calls (measured 2026-09-14).
EMPTY_STREAM = _sse([
    {"type": "message_start", "message": {"id": "msg_empty", "type": "message", "role": "assistant",
                                          "model": "muse-spark-1.3-contributor-free(xhigh)", "content": [],
                                          "usage": {"input_tokens": 0, "output_tokens": 0}}},
    {"type": "content_block_start", "index": 0, "content_block": {"type": "thinking", "thinking": ""}},
    {"type": "content_block_delta", "index": 0, "delta": {"type": "thinking_delta", "thinking": "The user wants OK."}},
    {"type": "content_block_stop", "index": 0},
    {"type": "message_delta", "delta": {"stop_reason": "end_turn"}, "usage": {"input_tokens": 0, "output_tokens": 0}},
    {"type": "message_stop"},
])


def self_test():
    failures = []

    def check(name, condition):
        print(("ok    " if condition else "FAIL  ") + name)
        if not condition:
            failures.append(name)

    msg, err = assemble(TEXT_STREAM)
    check("text stream assembles to one text block", err is None and msg["content"] == [{"type": "text", "text": "OK"}])
    check("usage combines message_start and message_delta",
          msg["usage"] == {"input_tokens": 2010, "output_tokens": 12})
    check("id, model and stop_reason are carried",
          msg.get("id") == "msg_1" and msg.get("model", "").startswith("muse") and msg["stop_reason"] == "end_turn")

    msg, _ = assemble(_sse([
        {"type": "message_start", "message": {"id": "m"}},
        {"type": "content_block_start", "index": 0, "content_block": {"type": "thinking", "thinking": ""}},
        {"type": "content_block_delta", "index": 0, "delta": {"type": "thinking_delta", "thinking": "hmm"}},
        {"type": "content_block_delta", "index": 0, "delta": {"type": "signature_delta", "signature": "sig"}},
        {"type": "content_block_start", "index": 1, "content_block": {"type": "text", "text": ""}},
        {"type": "content_block_delta", "index": 1, "delta": {"type": "text_delta", "text": "<block>no</block>"}},
        {"type": "message_delta", "delta": {"stop_reason": "end_turn"}, "usage": {"output_tokens": 5}},
    ]))
    check("thinking with signature, then text, in order",
          msg["content"] == [{"type": "thinking", "thinking": "hmm", "signature": "sig"},
                             {"type": "text", "text": "<block>no</block>"}])
    check("missing usage in message_start defaults to zero input tokens", msg["usage"]["input_tokens"] == 0)

    msg, _ = assemble(_sse([
        {"type": "message_start", "message": {"id": "m"}},
        {"type": "content_block_start", "index": 0,
         "content_block": {"type": "tool_use", "id": "tu", "name": "Bash", "input": {}}},
        {"type": "content_block_delta", "index": 0, "delta": {"type": "input_json_delta", "partial_json": "{\"command\": "}},
        {"type": "content_block_delta", "index": 0, "delta": {"type": "input_json_delta", "partial_json": "\"ls\"}"}},
        {"type": "message_delta", "delta": {"stop_reason": "tool_use"}},
    ]))
    check("tool_use input is rebuilt from partial JSON",
          msg["content"][0].get("input") == {"command": "ls"} and msg["stop_reason"] == "tool_use")

    msg, err = assemble(_sse([{"type": "message_start", "message": {"id": "m"}},
                              {"type": "error", "error": {"type": "overloaded_error", "message": "busy"}}]))
    check("an error event is reported as an error, not an empty answer",
          msg is None and err == {"type": "overloaded_error", "message": "busy"})

    check("an empty reply with zero usage is recognised", is_empty_reply(assemble(EMPTY_STREAM)[0]))
    check("a real answer is not an empty reply", not is_empty_reply(assemble(TEXT_STREAM)[0]))
    tool_only, _ = assemble(_sse([
        {"type": "content_block_start", "index": 0, "content_block": {"type": "tool_use", "id": "t", "name": "Bash"}},
    ]))
    check("a tool call with zero usage is still an answer", not is_empty_reply(tool_only))

    # End to end, against a fake upstream that reproduces muse-spark's fault:
    # proper SSE when streaming, an empty OpenAI object when not.
    seen = []
    script = []  # streams to return first, before falling back to TEXT_STREAM

    class FakeUpstream(http.server.BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, *args):
            pass

        def do_POST(self):
            req = json.loads(self.rfile.read(int(self.headers["content-length"])))
            seen.append(req.get("stream"))
            if req.get("stream"):
                data, ctype = (script.pop(0) if script else TEXT_STREAM).encode(), "text/event-stream"
            else:
                data = json.dumps({"object": "chat.completion", "choices": [{"message": {"content": ""}}]}).encode()
                ctype = "application/json"
            self.send_response(200)
            self.send_header("content-type", ctype)
            self.send_header("content-length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

    fake = http.server.ThreadingHTTPServer(("127.0.0.1", 0), FakeUpstream)
    threading.Thread(target=fake.serve_forever, daemon=True).start()
    shim = Server(("127.0.0.1", 0), f"http://127.0.0.1:{fake.server_address[1]}", 30)
    threading.Thread(target=shim.serve_forever, daemon=True).start()

    def post(port, payload):
        conn = http.client.HTTPConnection("127.0.0.1", port, timeout=30)
        conn.request("POST", "/v1/messages?beta=true", body=json.dumps(payload),
                     headers={"content-type": "application/json", "x-api-key": "test"})
        resp = conn.getresponse()
        return resp.status, resp.getheader("content-type") or "", resp.read()

    status, ctype, body = post(shim.server_address[1], {"model": "m", "max_tokens": 64, "messages": []})
    reply = json.loads(body) if ctype.startswith("application/json") else {}
    check("non-streaming request is sent upstream as streaming", seen[-1:] == [True])
    check("non-streaming reply is an Anthropic message with usage",
          status == 200 and reply.get("type") == "message" and reply.get("usage", {}).get("input_tokens") == 2010
          and reply.get("content") == [{"type": "text", "text": "OK"}])

    status, ctype, body = post(shim.server_address[1], {"model": "m", "max_tokens": 64, "stream": True, "messages": []})
    check("streaming request passes through byte for byte",
          status == 200 and ctype.startswith("text/event-stream") and body == TEXT_STREAM.encode())

    script[:] = [EMPTY_STREAM]
    before = len(seen)
    status, ctype, body = post(shim.server_address[1], {"model": "m", "max_tokens": 64, "messages": []})
    reply = json.loads(body)
    check("an empty reply is retried once and the real answer returned",
          len(seen) - before == 2 and reply.get("content") == [{"type": "text", "text": "OK"}])

    script[:] = [EMPTY_STREAM, EMPTY_STREAM]
    before = len(seen)
    status, ctype, body = post(shim.server_address[1], {"model": "m", "max_tokens": 64, "messages": []})
    reply = json.loads(body)
    check("a second empty reply is returned as it is, after exactly two calls",
          len(seen) - before == 2 and status == 200 and [b.get("type") for b in reply.get("content", [])] == ["thinking"])

    dead = Server(("127.0.0.1", 0), "http://127.0.0.1:9", 5)  # port 9: nothing listens
    threading.Thread(target=dead.serve_forever, daemon=True).start()
    status, ctype, body = post(dead.server_address[1], {"model": "m", "max_tokens": 64, "messages": []})
    check("unreachable upstream answers 502 in Anthropic error format",
          status == 502 and json.loads(body).get("type") == "error")

    for server in (fake, shim, dead):
        server.shutdown()
    print(f"\n{'all passed' if not failures else f'{len(failures)} failed'}")
    return 1 if failures else 0


def main():
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--upstream", default="http://localhost:20128", help="router base URL (http or https)")
    parser.add_argument("--port", type=int, default=20198, help="local port, bound to 127.0.0.1")
    parser.add_argument("--timeout", type=float, default=600, help="seconds to wait on the upstream")
    parser.add_argument("--self-test", action="store_true", help="run the built-in tests and exit")
    args = parser.parse_args()
    if args.self_test:
        sys.exit(self_test())
    server = Server(("127.0.0.1", args.port), args.upstream, args.timeout)
    log(event="listening", address=f"127.0.0.1:{args.port}", upstream=args.upstream)
    server.serve_forever()


if __name__ == "__main__":
    main()
