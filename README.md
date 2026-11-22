# Plutonium

A single cross-platform Go binary that serves Pluto TV's on-demand catalog to
Nuvio and plays it **ad-free**, with Pluto's embedded English captions retimed
to the ad-free timeline.

It replaces the earlier Python `catalog_addon/server.py` and
`proxy/pluto_proxy.py` processes with one executable:

- Nuvio/Stremio addon endpoints: `manifest.json`, `catalog`, `meta`, `stream`, `search`
- HLS ad stripping (`METHOD=NONE` segments and Pluto ad URL patterns)
- Subtitles merged from Pluto's chunked WebVTT renditions and retimed using the
  chunk base encoded in each segment URL
- A small on-disk cache for API responses and generated subtitle tracks

The Kodi Pluto addon (`plugin.video.plutovod`) is the reference implementation
for the ad-strip and subtitle behaviour.

## Requirements

- Go 1.23 or newer to build
- A US IP for Pluto's API. If the host itself is outside the US, pass a US
  `X-Forwarded-For` address with `--xff`.
- Nuvio (or any Stremio-compatible client) able to play HLS

## Build

```sh
go build -trimpath -ldflags "-s -w" -o bin/plutonium ./cmd/plutonium
```

Cross-compile every common target:

```sh
./scripts/build-all.sh
```

The output is a set of static binaries under `dist/`, for example
`plutonium-linux-amd64` and `plutonium-windows-amd64.exe`.

## Run

```sh
./bin/plutonium --port 8789 --xff 74.83.52.188
```

The addon URL is `http://<host>:8789/manifest.json`. The existing Nuvio
registration does not need to change.

Useful flags:

| Flag | Default | Description |
| --- | --- | --- |
| `--host` | `0.0.0.0` | Listen address |
| `--port` | `8789` | Listen port |
| `--xff` | `$PLUTO_XFF` | `X-Forwarded-For` sent to Pluto (US IP) |
| `--country` | `US` | Pluto country code |
| `--tmdb-api-key` | `$TMDB_API_KEY` | TMDb API key for cast profile photos (optional) |
| `--public-url` | request host | Public base URL used in stream links |
| `--cache-dir` | `$XDG_CACHE_HOME/nuvio-plutonium` | API/subtitle cache |
| `--session-ttl` | `24h` | Playback session lifetime |
| `--subtitle-cache-ttl` | `168h` | Generated subtitle cache lifetime |
| `--subtitle-prewarm-timeout` | `15s` | Deprecated and ignored; subtitles always generate in the background |
| `--subtitle-offset` | `2` | Caption sync delay in seconds; Pluto's captions run ahead of the video (matches the Kodi addon default) |
| `--subtitle-workers` | `48` | Parallel VTT segment downloads |
| `--log-level` | `info` | `debug`, `info`, `warn`, `error` |
| `--version` | | Print the version and exit |

### Environment

`PLUTO_XFF` is used when `--xff` is not supplied. Set it in a service
environment file rather than exposing it on a shared command line if you
prefer.

## Service examples

### Linux (systemd user unit)

Copy the binary somewhere stable, then install the example unit:

```sh
install -Dm755 bin/plutonium ~/.local/bin/plutonium
mkdir -p ~/.config/systemd/user
cp packaging/systemd/plutonium.service ~/.config/systemd/user/plutonium.service
# Edit the unit if your XFF address or paths differ.
systemctl --user daemon-reload
systemctl --user enable --now plutonium.service
```

Check it:

```sh
curl -sS http://127.0.0.1:8789/health
journalctl --user -u plutonium.service -f
```

If you are replacing the old Python units:

```sh
systemctl --user disable --now nuvio-pluto-catalog.service nuvio-pluto-proxy.service
```

### macOS (launchd)

Copy `packaging/launchd/org.maba.plutonium.plist` to
`~/Library/LaunchAgents/`, adjust the binary path and XFF value, then:

```sh
launchctl load ~/Library/LaunchAgents/org.maba.plutonium.plist
launchctl start org.maba.plutonium
```

### Windows

The binary is a normal console application. To run it as a service, use NSSM,
WinSW, or Task Scheduler with "Run whether user is logged on or not". Example
with NSSM:

```powershell
nssm install Plutonium C:\Tools\plutonium.exe
nssm set Plutonium AppParameters "--port 8789 --xff 74.83.52.188"
nssm set Plutonium Start SERVICE_AUTO_START
nssm start Plutonium
```

## How it works

1. `/stream` resolves Pluto's stitcher master playlist for a movie or episode.
2. The master is rewritten to remove Pluto's separate audio rendition (audio is
   muxed into the video TS variants) and to point at local variant playlists.
3. Each variant is cleaned: unencrypted `METHOD=NONE` ad segments and known
   Pluto ad URLs are removed, together with their keys, EXTINF entries and
   ad-boundary tags. Content AES-128 keys remain public CDN URLs, so no DRM
   license proxy is needed.
4. Pluto's subtitle playlist is parsed in the background (the `/stream`
   endpoint returns immediately). Placeholder ad segments are skipped; content
   VTT segments are fetched concurrently. Each chunk's absolute offset is
   parsed from its URL (`.../<base>-<end>/hls/text_en_cc/...`), added to the
   cue times, then duplicate cues are removed.
5. Before serving, cues are rebased onto the ad-free timeline using the ad
   windows detected while cleaning the video playlist, mirroring the Kodi
   addon's `rebase_after_ad_windows()` helper. Cues inside removed ad breaks
   are dropped and all later cues are shifted left by the removed duration.
   The on-disk cache stays on the original timeline so a title watched again
   with different ad breaks is retimed again.
6. The retimed WebVTT is served as a one-segment HLS subtitle rendition.

## Endpoints

| Path | Purpose |
| --- | --- |
| `/health` | Liveness, uptime, session/subtitle-job counts |
| `/manifest.json` | Nuvio addon manifest |
| `/catalog/{type}/{id}.json` | Catalog rows |
| `/search?q=term` | Title/genre/description search over the cached catalog |
| `/meta/{type}/{id}.json` | Metadata and series episodes |
| `/stream/{type}/{id}.json` | Ad-free HLS stream descriptor |
| `/hls/{session}/master.m3u8` | Rewritten master |
| `/hls/{session}/v/{n}.m3u8` | Cleaned variant |
| `/hls/{session}/subs.m3u8` | Subtitle media playlist |
| `/hls/{session}/subs.vtt` | Retimed merged WebVTT |

## Cast photos

Pluto's API returns cast as names only — no photos. To show cast faces (like other catalogs), get a free TMDb API key from [themoviedb.org](https://www.themoviedb.org/settings/api) and pass it on startup:

```sh
./bin/plutonium --port 8789 --xff 74.83.52.188 --tmdb-api-key <your-key>
```

Or set `TMDB_API_KEY` in the environment. When configured, the `/meta` endpoint returns cast as objects with `name` and optional `poster` (profile photo URL). Without a key, cast remains a list of names (unchanged behavior).

## Ad stripping and subtitles

Ad stripping and caption resyncing are handled by the Cloudflare Worker in the
companion [`nuvio-plutonium-cf`](https://github.com/maba72/nuvio-plutonium-cf)
project. The provider defaults to the deployed Worker, so both desktop Nuvio and
clients without a settings UI (such as Nuvio on Android TV) get ad-free HLS
without any device configuration:

```js
var LAN_DEFAULTS = {
  country: "US",
  forwardedFor: "74.83.52.188",
  proxyUrl: "https://nuvio-plutonium.marc-ballegeer.workers.dev"
};
```

The same URL is the default in the settings UI under **Ad-strip proxy URL**.
Saved settings still override the defaults wholesale. Clear the proxy URL only
if you want the original Pluto stream with ads. The Python
`proxy/pluto_proxy.py` endpoint remains supported if you point the proxy URL at
a local instance instead.

The Worker now also serves the full addon (catalogs, metadata, search and
streams), so running this Go server is no longer necessary. To install the
all-in-one version in Nuvio, use the addon URL instead of the scraper manifest:

```text
https://nuvio-plutonium.marc-ballegeer.workers.dev/manifest.json
```

The GitHub `manifest.json` remains a stream-only plugin for titles supplied by
other Nuvio catalogs.

## Notes and troubleshooting

- **Empty catalogs**: Pluto returns an empty category list for non-US callers.
  Set `--xff` to a US address.
- **Subtitles appear late/early**: Cues are built from Pluto's chunk offsets,
  shifted left by the removed ad windows, then delayed by a configurable sync
  offset (default 2s) because Pluto's captions run ahead of the picture — the
  same two corrections the Kodi addon applies. Tune `--subtitle-offset` (or the
  app's Subtitle sync delay setting) if captions still look early or late. The
  on-disk cache stays canonical so changing the offset needs no re-download.
  English captions are the only track available.
- **First playback start**: The stream endpoint returns as soon as the master
  is resolved; subtitles are merged in the background and attached when the
  player asks for them, so a cold cache no longer delays playback. Only the
  first play of a title requires the VTT downloads.
- **Cache**: Subtitles are cached under the cache directory. Delete the
  directory to force regeneration after changing subtitle logic (the cache key
  also carries a format version).
- **Ad-free only for VOD**: This project targets Pluto's on-demand stitcher,
  not live channels.

## Layout

```text
cmd/plutonium/        CLI and HTTP server startup
internal/addon/       Nuvio addon and HLS HTTP routes
internal/pluto/       Pluto API/boot client and catalog mapping
internal/hls/         Ad cleaner, sessions, subtitle generation
internal/vtt/         WebVTT parsing, dedupe, rendering
internal/cache/       TTL cache for upstream API responses
scripts/              Cross-platform build helper
packaging/            Example systemd, launchd and Windows service files
```

## Legacy files

`catalog_addon/server.py` and `proxy/pluto_proxy.py` are the previous Python
implementation. They are kept for reference but are no longer needed once the
Go service is running.
