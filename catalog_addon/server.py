#!/usr/bin/env python3
"""Pluto TV catalog addon for Nuvio.

The addon exposes Pluto's on-demand categories as Nuvio/Stremio catalogs and
serves the matching metadata and HLS streams.  Pluto does not publish TMDB or
IMDb IDs for VOD items, so the addon uses stable ``pluto:`` IDs throughout.
That keeps the catalog independent of TMDB and avoids a second lookup service.
"""

from __future__ import annotations

import argparse
import gzip
import json
import os
import re
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import zlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any

UA = (
    "Mozilla/5.0 (Windows NT 10.0; Win64; x64) "
    "AppleWebKit/537.36 (KHTML, like Gecko) "
    "Chrome/125.0.0.0 Safari/537.36"
)
CATEGORIES_URL = (
    "https://api.pluto.tv/v3/vod/categories"
    "?includeItems=true&deviceType=web&country=US"
)
SEASONS_URL = (
    "https://api.pluto.tv/v3/vod/series/{series_id}/seasons"
    "?includeItems=true&deviceType=web&country=US"
)
BOOT_URL = "https://boot.pluto.tv/v4/start"
CATALOG_TTL = 15 * 60
SERIES_TTL = 15 * 60
BOOT_TTL = 5 * 60

ROWS: dict[str, dict[str, str]] = {
    "pluto-movies": {
        "name": "Pluto Movies",
        "category": "Most Popular Movies",
        "type": "movie",
    },
    "pluto-series": {
        "name": "Pluto TV Series",
        "category": "Top TV Series",
        "type": "series",
    },
    "pluto-top-movies": {
        "name": "Top Movies on Pluto",
        "category": "Top Titles",
        "type": "movie",
    },
    "pluto-top-series": {
        "name": "Top Series on Pluto",
        "category": "Top Titles",
        "type": "series",
    },
    "pluto-new-movies": {
        "name": "New Movies on Pluto",
        "category": "New Movies This Month",
        "type": "movie",
    },
    "pluto-leaving-movies": {
        "name": "Leaving Pluto Soon",
        "category": "Leaving Soon",
        "type": "movie",
    },
    "pluto-leaving-series": {
        "name": "Leaving Pluto Soon (TV)",
        "category": "Leaving Soon",
        "type": "series",
    },
}

MANIFEST = {
    "id": "org.maba.pluto-catalog",
    "version": "0.1.0",
    "name": "Pluto TV Catalog",
    "description": (
        "Browse Pluto TV's free on-demand movies and series in Nuvio. "
        "Select a title to open its Pluto metadata and play the direct HLS stream."
    ),
    "types": ["movie", "series"],
    "resources": [
        {"name": "catalog", "types": ["movie", "series"]},
        {
            "name": "meta",
            "types": ["movie", "series"],
            "idPrefixes": ["pluto:"],
        },
        {
            "name": "stream",
            "types": ["movie", "series"],
            "idPrefixes": ["pluto:"],
        },
    ],
    "catalogs": [
        {"type": row["type"], "id": row_id, "name": row["name"]}
        for row_id, row in ROWS.items()
    ],
}

CACHE: dict[str, tuple[float, Any]] = {}
CACHE_LOCK = threading.Lock()


class UpstreamError(RuntimeError):
    pass


def cache_get(key: str) -> Any | None:
    now = time.time()
    with CACHE_LOCK:
        entry = CACHE.get(key)
        if not entry:
            return None
        expires, value = entry
        if expires <= now:
            CACHE.pop(key, None)
            return None
        return value


def cache_put(key: str, value: Any, ttl: int) -> None:
    with CACHE_LOCK:
        CACHE[key] = (time.time() + ttl, value)


def fetch_json(url: str, xff: str, timeout: int = 30) -> Any:
    headers = {
        "User-Agent": UA,
        "Accept": "application/json, text/plain, */*",
        "Accept-Encoding": "identity",
    }
    if xff:
        headers["X-Forwarded-For"] = xff
    request = urllib.request.Request(url, headers=headers)
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            body = response.read()
            encoding = (response.headers.get("Content-Encoding") or "").lower()
            if encoding == "gzip":
                body = gzip.decompress(body)
            elif encoding == "deflate":
                try:
                    body = zlib.decompress(body)
                except zlib.error:
                    body = zlib.decompress(body, -zlib.MAX_WBITS)
            return json.loads(body.decode("utf-8", "replace"))
    except urllib.error.HTTPError as error:
        detail = ""
        try:
            detail = error.read().decode("utf-8", "replace")[:300]
        except Exception:
            pass
        raise UpstreamError(f"upstream HTTP {error.code}: {detail}") from error
    except (urllib.error.URLError, TimeoutError, json.JSONDecodeError) as error:
        raise UpstreamError(f"upstream request failed: {error}") from error


def categories(xff: str) -> dict[str, Any]:
    cached = cache_get("categories")
    if cached is not None:
        return cached
    data = fetch_json(CATEGORIES_URL, xff, timeout=45)
    if not isinstance(data, dict) or not isinstance(data.get("categories"), list):
        raise UpstreamError("Pluto categories response had an unexpected shape")
    cache_put("categories", data, CATALOG_TTL)
    return data


def find_category(data: dict[str, Any], name: str) -> list[dict[str, Any]]:
    wanted = name.casefold()
    for category in data.get("categories") or []:
        if str(category.get("name", "")).casefold() == wanted:
            items = category.get("items")
            return items if isinstance(items, list) else []
    return []


def image_url(value: Any) -> str | None:
    if isinstance(value, str):
        return value or None
    if isinstance(value, dict):
        path = value.get("path") or value.get("url")
        return str(path) if path else None
    return None


def cover_url(item: dict[str, Any], aspect: str) -> str | None:
    covers = item.get("covers")
    if isinstance(covers, list):
        for cover in covers:
            if not isinstance(cover, dict):
                continue
            if str(cover.get("aspectRatio", "")) == aspect:
                url = cover.get("url")
                if url:
                    return str(url)
    return None


def poster_for(item: dict[str, Any]) -> str | None:
    return (
        cover_url(item, "347:500")
        or image_url(item.get("featuredImage"))
        or image_url(item.get("poster16_9"))
    )


def background_for(item: dict[str, Any]) -> str | None:
    return (
        cover_url(item, "16:9")
        or image_url(item.get("featuredImage"))
        or image_url(item.get("poster16_9"))
    )


def first_year(item: dict[str, Any]) -> str | None:
    clip = item.get("clip") if isinstance(item.get("clip"), dict) else {}
    for value in (
        clip.get("originalReleaseDate"),
        item.get("releaseDate"),
        item.get("slug"),
    ):
        if not value:
            continue
        match = re.search(r"(19|20)\d{2}", str(value))
        if match:
            return match.group(0)
    return None


def runtime_minutes(duration: Any) -> str | None:
    try:
        ms = int(duration)
    except (TypeError, ValueError):
        return None
    if ms <= 0:
        return None
    return f"{round(ms / 60000)} min"


def genres_for(item: dict[str, Any]) -> list[str]:
    genre = item.get("genre")
    if isinstance(genre, list):
        return [str(value) for value in genre if value]
    if isinstance(genre, str) and genre.strip():
        return [genre.strip()]
    return []


def people_for(item: dict[str, Any], key: str) -> list[dict[str, str]]:
    clip = item.get("clip") if isinstance(item.get("clip"), dict) else {}
    values = clip.get(key)
    if not isinstance(values, list):
        return []
    return [{"name": str(value)} for value in values if value]


def item_year(item: dict[str, Any]) -> str | None:
    year = first_year(item)
    if year:
        return year
    seasons = item.get("seasons")
    if isinstance(seasons, list):
        years = []
        for season in seasons:
            if not isinstance(season, dict):
                continue
            for episode in season.get("episodes") or []:
                if isinstance(episode, dict):
                    episode_year = first_year(episode)
                    if episode_year:
                        years.append(int(episode_year))
        if years:
            return str(min(years))
    return None


def catalog_item(item: dict[str, Any], media_type: str) -> dict[str, Any] | None:
    item_id = item.get("_id")
    name = item.get("name")
    if not item_id or not name:
        return None
    prefix = "pluto:movie:" if media_type == "movie" else "pluto:series:"
    result: dict[str, Any] = {
        "id": prefix + str(item_id),
        "type": media_type,
        "name": str(name),
        "poster": poster_for(item),
        "background": background_for(item),
        "description": item.get("summary") or item.get("description") or "",
        "genres": genres_for(item),
        "posterShape": "poster",
    }
    year = item_year(item)
    if year:
        result["releaseInfo"] = year
    rating = item.get("rating")
    if rating:
        result["ageRating"] = str(rating)
    runtime = runtime_minutes(item.get("duration"))
    if runtime:
        result["runtime"] = runtime
    return result


def meta_for_item(item: dict[str, Any], media_type: str) -> dict[str, Any]:
    item_id = str(item["_id"])
    meta: dict[str, Any] = {
        "id": ("pluto:movie:" if media_type == "movie" else "pluto:series:") + item_id,
        "type": media_type,
        "name": str(item.get("name") or "Untitled"),
        "poster": poster_for(item),
        "background": background_for(item),
        "description": item.get("description") or item.get("summary") or "",
        "genres": genres_for(item),
        "cast": [p["name"] for p in people_for(item, "actors")],
        "director": [p["name"] for p in people_for(item, "directors")],
    }
    year = item_year(item)
    if year:
        meta["releaseInfo"] = year
    rating = item.get("rating")
    if rating:
        meta["ageRating"] = str(rating)
    runtime = runtime_minutes(item.get("duration"))
    if runtime:
        meta["runtime"] = runtime
    return meta


def find_item(data: dict[str, Any], item_id: str) -> dict[str, Any] | None:
    for category in data.get("categories") or []:
        for item in category.get("items") or []:
            if isinstance(item, dict) and str(item.get("_id")) == item_id:
                return item
    return None


def series_seasons(series_id: str, xff: str) -> dict[str, Any]:
    key = f"series:{series_id}"
    cached = cache_get(key)
    if cached is not None:
        return cached
    data = fetch_json(SEASONS_URL.format(series_id=urllib.parse.quote(series_id)), xff)
    if not isinstance(data, dict):
        raise UpstreamError("Pluto seasons response had an unexpected shape")
    cache_put(key, data, SERIES_TTL)
    return data


def episode_meta(episode: dict[str, Any]) -> dict[str, Any] | None:
    episode_id = episode.get("_id")
    if not episode_id:
        return None
    year = first_year(episode)
    result: dict[str, Any] = {
        "id": "pluto:episode:" + str(episode_id),
        "title": str(episode.get("name") or "Episode"),
        "overview": episode.get("description") or episode.get("summary") or "",
        "thumbnail": poster_for(episode) or background_for(episode),
        "season": episode.get("season"),
        "episode": episode.get("number"),
    }
    if year:
        result["released"] = year
    runtime = runtime_minutes(episode.get("duration"))
    if runtime:
        result["runtime"] = runtime
    return result


def series_meta(item: dict[str, Any], xff: str) -> dict[str, Any]:
    meta = meta_for_item(item, "series")
    data = series_seasons(str(item["_id"]), xff)
    videos: list[dict[str, Any]] = []
    for season in data.get("seasons") or []:
        if not isinstance(season, dict):
            continue
        season_number = season.get("number")
        for episode in season.get("episodes") or []:
            if not isinstance(episode, dict):
                continue
            episode["season"] = episode.get("season", season_number)
            video = episode_meta(episode)
            if video:
                videos.append(video)
    videos.sort(
        key=lambda video: (
            video.get("season") if isinstance(video.get("season"), int) else 0,
            video.get("episode") if isinstance(video.get("episode"), int) else 0,
        )
    )
    meta["videos"] = videos
    if videos:
        meta.setdefault("behaviorHints", {})["defaultVideoId"] = videos[0]["id"]
    return meta


def boot(xff: str) -> dict[str, Any]:
    cached = cache_get("boot")
    if cached is not None:
        return cached
    params = [
        ("appName", "web"),
        ("appVersion", "1"),
        ("deviceVersion", "0.0.0"),
        ("deviceModel", "web"),
        ("deviceMake", "firefox"),
        ("deviceType", "web"),
        ("clientID", str(__import__("uuid").uuid4())),
        ("clientModelNumber", "1.0.0"),
        ("serverSideAds", "false"),
        ("country", "US"),
    ]
    data = fetch_json(BOOT_URL + "?" + urllib.parse.urlencode(params), xff)
    if not isinstance(data, dict) or not data.get("sessionToken"):
        raise UpstreamError("Pluto boot response had no session token")
    stitcher = str((data.get("servers") or {}).get("stitcher") or "").rstrip("/")
    if not stitcher:
        raise UpstreamError("Pluto boot response had no stitcher")
    result = {
        "token": str(data["sessionToken"]),
        "stitcher": stitcher,
        "stitcherParams": str(data.get("stitcherParams") or "")
        .lstrip("?&")
        .rstrip("&"),
    }
    cache_put("boot", result, BOOT_TTL)
    return result


def stream_url(media_id: str, xff: str) -> str:
    session = boot(xff)
    path = "/v2/stitch/hls/episode/" + urllib.parse.quote(media_id, safe="") + "/master.m3u8"
    parts = []
    if session["stitcherParams"]:
        parts.append(session["stitcherParams"])
    parts.append("jwt=" + urllib.parse.quote(session["token"], safe=""))
    parts.append("masterJWTPassthrough=1")
    return session["stitcher"] + path + "?" + "&".join(parts)


def parse_media_id(raw_id: str) -> tuple[str, str] | None:
    raw_id = urllib.parse.unquote(raw_id)
    if raw_id.startswith("pluto:movie:"):
        return "movie", raw_id[len("pluto:movie:") :]
    if raw_id.startswith("pluto:episode:"):
        return "episode", raw_id[len("pluto:episode:") :]
    return None


class AddonHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "PlutoCatalog/0.1"
    sys_version = ""

    def log_message(self, fmt: str, *args: Any) -> None:
        # Keep journald quiet for normal catalog requests.
        pass

    def do_GET(self) -> None:
        try:
            self.handle_get()
        except (BrokenPipeError, ConnectionResetError):
            pass
        except UpstreamError as error:
            self.send_json(502, {"error": str(error)})
        except Exception as error:  # pragma: no cover - defensive
            self.server.error(f"request failed: {type(error).__name__}: {error}")
            self.send_json(500, {"error": "internal error"})

    def do_HEAD(self) -> None:
        self.send_response(200)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def handle_get(self) -> None:
        parsed = urllib.parse.urlsplit(self.path)
        path = parsed.path.rstrip("/") or "/"
        if path == "/health":
            self.send_json(200, {"ok": True, "cache": len(CACHE)})
            return
        if path == "/manifest.json":
            self.send_json(200, MANIFEST)
            return
        if path == "/":
            self.send_text(200, json.dumps(MANIFEST, indent=2) + "\n", "application/json")
            return

        parts = [urllib.parse.unquote(part) for part in path.split("/") if part]
        if len(parts) < 3:
            self.send_json(404, {"error": "not found"})
            return
        resource, media_type, raw_id = parts[0], parts[1], parts[2]
        if raw_id.endswith(".json"):
            raw_id = raw_id[:-5]
        if media_type not in ("movie", "series"):
            self.send_json(404, {"error": "unsupported type"})
            return

        xff = self.server.xff
        if resource == "catalog":
            self.handle_catalog(media_type, raw_id, xff)
        elif resource == "meta":
            self.handle_meta(media_type, raw_id, xff)
        elif resource == "stream":
            self.handle_stream(raw_id, xff)
        else:
            self.send_json(404, {"error": "not found"})

    def handle_catalog(self, media_type: str, row_id: str, xff: str) -> None:
        row = ROWS.get(row_id)
        if not row or row["type"] != media_type:
            self.send_json(200, {"metas": []})
            return
        data = categories(xff)
        items = find_category(data, row["category"])
        metas = []
        seen = set()
        for item in items:
            if not isinstance(item, dict) or item.get("type") != media_type:
                continue
            meta = catalog_item(item, media_type)
            if not meta or meta["id"] in seen:
                continue
            seen.add(meta["id"])
            metas.append(meta)
        self.send_json(200, {"metas": metas})

    def handle_meta(self, media_type: str, raw_id: str, xff: str) -> None:
        raw_id = urllib.parse.unquote(raw_id)
        prefix = "pluto:movie:" if media_type == "movie" else "pluto:series:"
        if not raw_id.startswith(prefix):
            self.send_json(404, {"error": "unsupported id"})
            return
        item_id = raw_id[len(prefix) :]
        data = categories(xff)
        item = find_item(data, item_id)
        if not item:
            self.send_json(404, {"error": "item not found"})
            return
        if media_type == "series":
            meta = series_meta(item, xff)
        else:
            meta = meta_for_item(item, media_type)
        self.send_json(200, {"meta": meta})

    def handle_stream(self, raw_id: str, xff: str) -> None:
        parsed = parse_media_id(raw_id)
        if not parsed:
            self.send_json(200, {"streams": []})
            return
        _, media_id = parsed
        if not media_id:
            self.send_json(200, {"streams": []})
            return
        url = stream_url(media_id, xff)
        self.send_json(
            200,
            {
                "streams": [
                    {
                        "name": "Pluto TV",
                        "title": "Pluto TV • Auto",
                        "url": url,
                        "type": "hls",
                        "behaviorHints": {
                            "proxyHeaders": {
                                "request": {
                                    "User-Agent": UA,
                                    "X-Forwarded-For": xff,
                                }
                            }
                        },
                    }
                ]
            },
        )

    def send_text(self, status: int, text: str, content_type: str) -> None:
        body = text.encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", content_type + "; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Access-Control-Allow-Origin", "*")
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(body)

    def send_json(self, status: int, payload: Any) -> None:
        self.send_text(status, json.dumps(payload), "application/json")


class AddonServer(ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True

    def __init__(self, address: tuple[str, int], xff: str):
        super().__init__(address, AddonHandler)
        self.xff = xff
        self.started = time.time()

    def error(self, message: str) -> None:
        print(message, flush=True)


def main() -> None:
    parser = argparse.ArgumentParser(description="Pluto TV catalog addon for Nuvio")
    parser.add_argument("--host", default="0.0.0.0")
    parser.add_argument("--port", type=int, default=8789)
    parser.add_argument(
        "--xff",
        default=os.environ.get("PLUTO_XFF", "74.83.52.188"),
        help="US X-Forwarded-For value sent to Pluto",
    )
    args = parser.parse_args()
    server = AddonServer((args.host, args.port), args.xff.strip())
    print(
        f"Pluto catalog addon listening on http://{args.host}:{args.port}/manifest.json",
        flush=True,
    )
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
