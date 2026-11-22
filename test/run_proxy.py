import os
import sys
from urllib.parse import parse_qs, urlsplit

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "proxy"))

import pluto_proxy

passed = 0


def check(condition, label):
    global passed
    if not condition:
        raise AssertionError(label)
    passed += 1


def variant_links(text):
    return [
        line.strip()
        for line in text.splitlines()
        if line.strip().startswith("/variant.m3u8?")
    ]


master = "\n".join(
    [
        "#EXTM3U",
        "#EXT-X-VERSION:5",
        '#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",URI="subs.m3u8"',
        '#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",URI="audio.m3u8"',
        '#EXT-X-MEDIA:TYPE=CLOSED-CAPTIONS,GROUP-ID="cc",NAME="CC"',
        '#EXT-X-STREAM-INF:PROGRAM-ID=1,BANDWIDTH=1000,SUBTITLES="subs",AUDIO="audio"',
        "1000/playlist.m3u8?jwt=abc",
        '#EXT-X-STREAM-INF:BANDWIDTH=2000,AUDIO="audio"',
        "2000/playlist.m3u8",
    ]
)
master_base = "https://cdn.pluto.tv/v2/stitch/episode/1/master.m3u8?jwt=abc"
rewritten = pluto_proxy.rewrite_master(master, master_base, "74.83.52.188")
check(rewritten.startswith("#EXTM3U"), "master header")
check("TYPE=SUBTITLES" not in rewritten, "subtitle media dropped")
check("TYPE=AUDIO" not in rewritten, "audio media dropped")
check('SUBTITLES="' not in rewritten, "subtitle attrs dropped")
check('AUDIO="' not in rewritten, "audio attrs dropped")
check("TYPE=CLOSED-CAPTIONS" in rewritten, "closed captions kept")
links = variant_links(rewritten)
check(len(links) == 2, "variant link count")
expected_variants = [
    "https://cdn.pluto.tv/v2/stitch/episode/1/1000/playlist.m3u8?jwt=abc",
    "https://cdn.pluto.tv/v2/stitch/episode/1/2000/playlist.m3u8",
]
for link, expected in zip(links, expected_variants):
    query = parse_qs(urlsplit(link).query)
    check(query["url"][0] == expected, "variant url")
    check(query["xff"][0] == "74.83.52.188", "variant xff")

playlist = "\n".join(
    [
        "#EXTM3U",
        "#EXT-X-VERSION:5",
        '#EXT-X-KEY:METHOD=AES-128,URI="key1"',
        "#EXTINF:10,",
        "seg1.ts",
        "#EXT-X-DISCONTINUITY",
        "#EXT-X-PROGRAM-DATE-TIME:2026-01-01T00:00:00Z",
        "#EXT-X-KEY:METHOD=NONE",
        "#EXTINF:5,",
        "https://cdn.pluto.tv/1_ad/creative/ad1.ts",
        '#EXT-X-KEY:METHOD=AES-128,URI="key2"',
        "#EXTINF:6,",
        "https://cdn.pluto.tv/clip/_ad_bumper/ad2.ts",
        "#EXT-X-DISCONTINUITY",
        '#EXT-X-KEY:METHOD=AES-128,URI="key3"',
        "#EXTINF:10,",
        "relative/seg2.ts",
        "#EXT-X-ENDLIST",
        "#PLUTO-SESSION-ID:abc",
    ]
)
variant_base = (
    "https://cdn.pluto.tv/v2/stitch/episode/1/1000/playlist.m3u8"
)
cleaned = pluto_proxy.StripCleaner().clean(playlist, variant_base)
check("/1_ad/creative/" not in cleaned, "none-region ad dropped")
check("_ad_bumper" not in cleaned, "aes ad dropped")
check("METHOD=NONE" not in cleaned, "none key dropped")
check(cleaned.count("#EXT-X-ENDLIST") == 1, "single endlist")
check(cleaned.rstrip().endswith("#EXT-X-ENDLIST"), "endlist last")
check("#PLUTO-" not in cleaned, "trailer dropped")
check(cleaned.count("#EXTINF") == 2, "content extinf count")
check("#EXT-X-DISCONTINUITY" not in cleaned, "discontinuity dropped")
check("PROGRAM-DATE-TIME" not in cleaned, "dangling program time dropped")
check(
    "https://cdn.pluto.tv/v2/stitch/episode/1/1000/relative/seg2.ts" in cleaned,
    "relative segment resolved",
)
check(
    "https://cdn.pluto.tv/v2/stitch/episode/1/1000/key1" in cleaned,
    "relative key resolved",
)
check(
    "https://cdn.pluto.tv/v2/stitch/episode/1/1000/key3" in cleaned,
    "second relative key resolved",
)
check("key2" not in cleaned, "ad key dropped")

plain = "\n".join(
    [
        "#EXTM3U",
        "#EXTINF:4,",
        "https://cdn.pluto.tv/seg.ts",
        "#EXT-X-ENDLIST",
        "#PLUTO-VERSION:1",
    ]
)
plain_cleaned = pluto_proxy.StripCleaner().clean(plain, variant_base)
check(plain_cleaned.count("#EXT-X-ENDLIST") == 1, "plain endlist count")
check(plain_cleaned.rstrip().endswith("#EXT-X-ENDLIST"), "plain endlist last")
check("#PLUTO-" not in plain_cleaned, "plain trailer dropped")
check("https://cdn.pluto.tv/seg.ts" in plain_cleaned, "plain segment kept")

check(
    pluto_proxy.is_pluto_url(
        "https://cfd-v4-service-channel-stitcher-use1-1.prd.pluto.tv/master.m3u8"
    ),
    "pluto.tv allowed",
)
check(
    pluto_proxy.is_pluto_url("https://siloh-ns1.plutotv.net/seg.ts"),
    "plutotv.net allowed",
)
check(not pluto_proxy.is_pluto_url("http://192.168.1.1/"), "lan blocked")
check(not pluto_proxy.is_pluto_url("https://evilpluto.tv/"), "lookalike blocked")
check(not pluto_proxy.is_pluto_url("ftp://pluto.tv/seg.ts"), "ftp blocked")

check(
    pluto_proxy.resolve_xff({"xff": ["1.2.3.4"]}, "5.6.7.8", "9.9.9.9")
    == "1.2.3.4",
    "query xff wins",
)
check(
    pluto_proxy.resolve_xff({}, "5.6.7.8, 10.0.0.1", "9.9.9.9")
    == "5.6.7.8",
    "header xff",
)
check(pluto_proxy.resolve_xff({}, "bad", "9.9.9.9") == "9.9.9.9", "default xff")
check(
    pluto_proxy.resolve_xff({"xff": ["bad"]}, "", "9.9.9.9") == "9.9.9.9",
    "invalid query falls back",
)
check(pluto_proxy.resolve_xff({}, "", "") == "", "no xff")

print("All %d proxy tests passed" % passed)
