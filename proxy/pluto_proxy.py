import argparse
import gzip
import ipaddress
import json
import os
import re
import sys
import threading
import time
import urllib.error
import urllib.request
import zlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, quote, urlencode, urljoin, urlsplit

UA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36"
MASTER_TTL = 60
VARIANT_TTL = 300
MAX_CACHE = 24
CACHE = {}
CACHE_LOCK = threading.Lock()

EXTINF_RE = re.compile(r"#EXTINF:\s*([0-9.]+)")
AD_RE = re.compile(
    r"(/1_ad/creative/|plutotv_kidsbumpers|plutotv_filler|"
    r"_ad(?:/|%2F|_bumper)|dai\.google\.com)",
    re.IGNORECASE,
)
URI_ATTR_RE = re.compile(r'URI="([^"]*)"')
MEDIA_TYPE_RE = re.compile(r"TYPE=(?:AUDIO|SUBTITLES)", re.IGNORECASE)
STREAM_ATTR_RE = re.compile(r',(?:AUDIO|SUBTITLES)="[^"]*"', re.IGNORECASE)
STREAM_ATTR_HEAD_RE = re.compile(
    r'^(#EXT-X-STREAM-INF:)(?:AUDIO|SUBTITLES)="[^"]*",?', re.IGNORECASE
)


def is_ip(value):
    try:
        ipaddress.ip_address(value)
        return True
    except ValueError:
        return False


def resolve_xff(query, header, default):
    candidates = []
    query_xff = (query.get("xff") or [""])[0].strip()
    if query_xff:
        candidates.append(query_xff)
    header_xff = (header or "").split(",")[0].strip()
    if header_xff:
        candidates.append(header_xff)
    if default:
        candidates.append(default)
    for candidate in candidates:
        if is_ip(candidate):
            return candidate
    return ""


def is_pluto_url(url):
    try:
        parsed = urlsplit(url)
    except ValueError:
        return False
    if parsed.scheme not in ("http", "https") or not parsed.hostname:
        return False
    host = parsed.hostname.lower()
    return (
        host == "pluto.tv"
        or host.endswith(".pluto.tv")
        or host == "plutotv.net"
        or host.endswith(".plutotv.net")
    )


def cache_get(key):
    now = time.time()
    with CACHE_LOCK:
        item = CACHE.get(key)
        if not item:
            return None
        expires, value = item
        if expires <= now:
            CACHE.pop(key, None)
            return None
        return value


def cache_put(key, value, ttl):
    now = time.time()
    with CACHE_LOCK:
        if len(CACHE) >= MAX_CACHE:
            expired = [k for k, (expires, _) in CACHE.items() if expires <= now]
            for old_key in expired:
                CACHE.pop(old_key, None)
            if len(CACHE) >= MAX_CACHE:
                CACHE.clear()
        CACHE[key] = (now + ttl, value)


def cache_size():
    with CACHE_LOCK:
        return len(CACHE)


def fetch(url, xff):
    headers = {
        "User-Agent": UA,
        "Accept": "*/*",
        "Accept-Encoding": "identity",
    }
    if xff:
        headers["X-Forwarded-For"] = xff
    request = urllib.request.Request(url, headers=headers)
    try:
        with urllib.request.urlopen(request, timeout=25) as response:
            body = response.read()
            encoding = (response.headers.get("Content-Encoding") or "").strip().lower()
            if encoding == "gzip":
                body = gzip.decompress(body)
            elif encoding == "deflate":
                try:
                    body = zlib.decompress(body)
                except zlib.error:
                    body = zlib.decompress(body, -zlib.MAX_WBITS)
            charset = response.headers.get_content_charset() or "utf-8"
            return body.decode(charset, "replace"), response.status, response.geturl()
    except urllib.error.HTTPError as error:
        body = ""
        try:
            body = error.read().decode("utf-8", "replace")
        except Exception:
            pass
        return body, error.code, error.geturl()


def absolute(uri, base_url):
    uri = (uri or "").strip()
    if not uri:
        return uri
    try:
        scheme = urlsplit(uri).scheme.lower()
    except ValueError:
        return uri
    if scheme:
        return uri
    if base_url:
        return urljoin(base_url, uri)
    return uri


def rewrite_master(text, base_url, xff):
    output = []
    for raw_line in text.splitlines():
        line = raw_line.strip()
        if not line:
            continue
        if line.startswith("#"):
            if line.upper().startswith("#EXT-X-MEDIA:") and MEDIA_TYPE_RE.search(line):
                continue
            if line.upper().startswith("#EXT-X-STREAM-INF:"):
                line = STREAM_ATTR_RE.sub("", line)
                line = STREAM_ATTR_HEAD_RE.sub(r"\1", line)
            output.append(line)
            continue
        variant_url = urljoin(base_url, line) if base_url else line
        params = [("url", variant_url)]
        if xff:
            params.append(("xff", xff))
        output.append("/variant.m3u8?" + urlencode(params, quote_via=quote))
    return "\n".join(output) + "\n"


class StripCleaner:
    def clean(self, text, base_url=""):
        if not text:
            return text
        text = text.lstrip("\ufeff")
        use_none = "METHOD=AES-128" in text
        if use_none or AD_RE.search(text):
            return self._strip(text, base_url, use_none)
        return self._ensure_endlist(text, base_url)

    def _strip(self, text, base_url, use_none):
        output = []
        pending = []
        pending_duration = 0.0
        none_region = False
        last_was_ad = False
        ended = False

        for raw_line in text.splitlines():
            line = raw_line.strip()
            if ended or not line:
                continue
            if line.startswith("#EXT-X-ENDLIST"):
                ended = True
                continue
            if line.startswith("#PLUTO-"):
                continue
            if line.startswith("#EXT-X-KEY:"):
                if "METHOD=NONE" in line:
                    if use_none:
                        none_region = True
                elif "METHOD=AES-128" in line:
                    none_region = False
                pending.append(self._uri_attrs(line, base_url))
                continue
            if line.startswith("#EXTINF"):
                match = EXTINF_RE.match(line)
                try:
                    pending_duration = float(match.group(1)) if match else 0.0
                except ValueError:
                    pending_duration = 0.0
                pending.append(line)
                continue
            if line.startswith("#EXT-X-MAP:"):
                pending.append(self._uri_attrs(line, base_url))
                continue
            if line.startswith("#EXT-X-PROGRAM-DATE-TIME"):
                pending.append(line)
                continue
            if line == "#EXT-X-DISCONTINUITY":
                if not last_was_ad:
                    pending.append(line)
                continue
            if not line.startswith("#"):
                if none_region or AD_RE.search(line):
                    pending = []
                    pending_duration = 0.0
                    last_was_ad = True
                    continue
                output.extend(pending)
                pending = []
                output.append(absolute(line, base_url))
                pending_duration = 0.0
                last_was_ad = False
                continue
            output.append(line)

        return "\n".join(output).rstrip() + "\n#EXT-X-ENDLIST\n"

    def _ensure_endlist(self, text, base_url):
        output = []
        for raw_line in text.splitlines():
            line = raw_line.strip()
            if not line:
                continue
            if line.startswith("#EXT-X-ENDLIST"):
                break
            if line.startswith("#PLUTO-"):
                continue
            if line.startswith("#EXT-X-KEY:") or line.startswith("#EXT-X-MAP:"):
                output.append(self._uri_attrs(line, base_url))
            elif line.startswith("#"):
                output.append(line)
            else:
                output.append(absolute(line, base_url))
        return "\n".join(output).rstrip() + "\n#EXT-X-ENDLIST\n"

    def _uri_attrs(self, line, base_url):
        if 'URI="' not in line:
            return line
        return URI_ATTR_RE.sub(
            lambda match: 'URI="' + absolute(match.group(1), base_url) + '"', line
        )


def route(path):
    normalized = (path or "/").rstrip("/").lower()
    if normalized == "/health":
        return "/health"
    if normalized in ("/master", "/master.m3u8"):
        return "/master"
    if normalized in ("/variant", "/variant.m3u8"):
        return "/variant"
    return None


class ProxyHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "PlutoProxy/1.0"
    sys_version = ""

    def log_message(self, format, *args):
        pass

    def do_GET(self):
        try:
            self._handle_get()
        except (BrokenPipeError, ConnectionResetError):
            pass
        except Exception as error:
            self.server.log_error("GET failed: " + type(error).__name__)
            try:
                self.send_json(500, {"error": "internal error"})
            except Exception:
                pass

    def _handle_get(self):
        started = time.time()
        try:
            parsed = urlsplit(self.path)
        except ValueError:
            self.send_json(400, {"error": "bad request"})
            return
        path = route(parsed.path)
        if path is None:
            self.send_json(404, {"error": "not found"})
            return
        if path == "/health":
            self.send_json(
                200,
                {
                    "ok": True,
                    "uptime": int(time.time() - self.server.started),
                    "cache": cache_size(),
                    "xff": bool(self.server.default_xff),
                },
            )
            return
        query = parse_qs(parsed.query)
        raw_url = (query.get("url") or [""])[0].strip()
        if not is_pluto_url(raw_url):
            self.send_json(400, {"error": "url must be a Pluto TV http(s) URL"})
            return
        xff = resolve_xff(
            query, self.headers.get("X-Forwarded-For"), self.server.default_xff
        )
        host = urlsplit(raw_url).netloc
        key = (path, raw_url, xff)
        cached = cache_get(key)
        if cached is not None:
            length = self.send_text(200, cached)
            self.server.log_request(
                "GET %s host=%s xff=%s cache=hit bytes=%d %.3fs"
                % (path, host, xff or "-", length, time.time() - started)
            )
            return
        try:
            text, status, final_url = fetch(raw_url, xff)
        except Exception as error:
            self.send_json(502, {"error": "upstream fetch failed"})
            self.server.log_error(
                "GET %s host=%s xff=%s error=%s"
                % (path, host, xff or "-", type(error).__name__)
            )
            return
        if status != 200:
            self.send_json(502, {"error": "upstream error", "status": status})
            self.server.log_error(
                "GET %s host=%s xff=%s upstream=%d"
                % (path, host, xff or "-", status)
            )
            return
        text = text.lstrip("\ufeff") if text else ""
        if path == "/master":
            output = rewrite_master(text, final_url, xff)
            ttl = MASTER_TTL
        else:
            output = StripCleaner().clean(text, final_url)
            ttl = VARIANT_TTL
        if not output.lstrip().startswith("#EXTM3U"):
            self.send_json(
                502, {"error": "upstream did not return an HLS playlist"}
            )
            self.server.log_error(
                "GET %s host=%s xff=%s returned a non-playlist" % (path, host, xff or "-")
            )
            return
        cache_put(key, output, ttl)
        length = self.send_text(200, output)
        self.server.log_request(
            "GET %s host=%s xff=%s cache=miss bytes=%d %.3fs"
            % (path, host, xff or "-", length, time.time() - started)
        )

    def do_HEAD(self):
        try:
            parsed = urlsplit(self.path)
            path = route(parsed.path)
            if path is None:
                self.send_head(404)
                return
            if path == "/health":
                self.send_head(200)
                return
            query = parse_qs(parsed.query)
            raw_url = (query.get("url") or [""])[0].strip()
            self.send_head(200 if is_pluto_url(raw_url) else 400)
        except Exception:
            try:
                self.send_head(500)
            except Exception:
                pass

    def send_text(self, status, text):
        data = text.encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/vnd.apple.mpegurl")
        self.send_header("Content-Length", str(len(data)))
        self.send_header("Cache-Control", "no-store")
        self.send_header("Access-Control-Allow-Origin", "*")
        self.end_headers()
        try:
            self.wfile.write(data)
            self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError):
            pass
        return len(data)

    def send_json(self, status, payload):
        data = json.dumps(payload, separators=(",", ":")).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.send_header("Cache-Control", "no-store")
        self.send_header("Access-Control-Allow-Origin", "*")
        self.end_headers()
        try:
            self.wfile.write(data)
            self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError):
            pass

    def send_head(self, status):
        self.send_response(status)
        self.send_header("Content-Type", "application/vnd.apple.mpegurl")
        self.send_header("Content-Length", "0")
        self.send_header("Cache-Control", "no-store")
        self.send_header("Access-Control-Allow-Origin", "*")
        self.end_headers()


class ProxyServer(ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True

    def __init__(self, address, handler, default_xff, verbose):
        self.default_xff = default_xff
        self.verbose = verbose
        self.started = time.time()
        super().__init__(address, handler)

    def log_request(self, message):
        if self.verbose:
            print(message, flush=True)

    def log_error(self, message):
        print("error: " + message, file=sys.stderr, flush=True)


def main():
    parser = argparse.ArgumentParser(
        description="LAN HLS proxy that strips ad segments from Pluto TV VOD playlists."
    )
    parser.add_argument(
        "--host", default="0.0.0.0", help="bind address (default: %(default)s)"
    )
    parser.add_argument(
        "--port", type=int, default=8787, help="bind port (default: %(default)s)"
    )
    parser.add_argument(
        "--xff",
        default=os.environ.get("PLUTO_XFF", ""),
        help="default X-Forwarded-For IP (or set PLUTO_XFF)",
    )
    parser.add_argument("--verbose", action="store_true", help="log requests")
    args = parser.parse_args()
    xff = args.xff.strip()
    if xff and not is_ip(xff):
        print("warning: ignoring invalid --xff value", file=sys.stderr)
        xff = ""
    try:
        server = ProxyServer(
            (args.host, args.port), ProxyHandler, xff, args.verbose
        )
    except OSError as error:
        print(
            "error: cannot listen on %s:%d: %s" % (args.host, args.port, error),
            file=sys.stderr,
        )
        return 1
    display_host = (
        args.host if args.host not in ("0.0.0.0", "::") else "<this-LAN-IP>"
    )
    suffix = " (XFF " + xff + ")" if xff else ""
    print(
        "Pluto ad-strip proxy listening on http://%s:%d%s"
        % (display_host, args.port, suffix),
        flush=True,
    )
    print(
        "Nuvio plugin setting: Proxy URL = http://%s:%d"
        % (display_host, args.port),
        flush=True,
    )
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()
    return 0


if __name__ == "__main__":
    sys.exit(main())
