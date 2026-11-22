"use strict";

var path = require("path");

var tmdbKey = process.env.TMDB_API_KEY || "";
if (!tmdbKey) {
  console.error("Set TMDB_API_KEY to run the tests.");
  process.exit(2);
}

globalThis.TMDB_API_KEY = tmdbKey;
globalThis.SCRAPER_SETTINGS = {
  country: process.env.PLUTO_COUNTRY || "US",
  forwardedFor: process.env.PLUTO_XFF || "",
  proxyUrl: process.env.PLUTO_PROXY || "",
  debug: process.env.DEBUG === "1"
};

var plugin = require(path.join(__dirname, "..", "providers", "plutonium.js"));
if (typeof plugin.getStreams !== "function" || !Array.isArray(plugin.onSettings())) {
  console.error("plugin exports are invalid");
  process.exit(1);
}

var cases = [
  { label: "Pretty Woman", id: "114", type: "movie" },
  { label: "Titanic", id: "597", type: "movie" },
  { label: "A Star Is Born (2018)", id: "332562", type: "movie" },
  { label: "Baywatch S1E1", id: "4386", type: "tv", season: 1, episode: 1 },
  { label: "Forensic Files S1E1", id: "11105", type: "tv", season: 1, episode: 1 },
  { label: "Ghosts (2021) S1E1", id: "126027", type: "tv", season: 1, episode: 1 },
  { label: "What I Like About You S1E1", id: "33", type: "tv", season: 1, episode: 1 },
  { label: "Baywatch S2E1", id: "4386", type: "tv", season: 2, episode: 1 },
  { label: "Ghosts (2021) S2E3", id: "126027", type: "tv", season: 2, episode: 3 },
  { label: "Star Trek TNG S1E1", id: "655", type: "tv", season: 1, episode: 1 },
  { label: "Baywatch absolute episode 24", id: "4386", type: "tv", season: null, episode: 24 },
  { label: "Pretty Woman via IMDb", id: "tt0100405", type: "movie" },
  { label: "Breaking Bad (expected miss)", id: "1396", type: "tv", season: 1, episode: 1, expect: 0 }
];

function timeout(ms) {
  return new Promise(function (_, reject) {
    setTimeout(function () { reject(new Error("timeout after " + ms + "ms")); }, ms);
  });
}

async function checkMaster(stream) {
  var res = await fetch(stream.url, { headers: stream.headers });
  if (!res.ok) return "master HTTP " + res.status;
  var body = await res.text();
  if (body.indexOf("#EXTM3U") !== 0) return "master is not HLS";
  return "master " + body.length + " bytes";
}

(async function () {
  var failures = 0;
  for (var i = 0; i < cases.length; i++) {
    var item = cases[i];
    var expected = item.expect == null ? 1 : item.expect;
    var streams = [];
    var error = "";
    try {
      streams = await Promise.race([
        plugin.getStreams(item.id, item.type, item.season, item.episode),
        timeout(90000)
      ]);
    } catch (e) {
      error = e && e.message ? e.message : String(e);
    }
    var ok = !error && Array.isArray(streams) && streams.length >= expected;
    if (item.expect === 0) ok = !error && Array.isArray(streams) && streams.length === 0;
    var detail = error || (streams.length + " stream(s)");
    if (ok && streams.length) {
      try {
        detail += ", " + await checkMaster(streams[0]);
      } catch (e2) {
        ok = false;
        detail += ", " + (e2 && e2.message ? e2.message : String(e2));
      }
      var u = new URL(streams[0].url);
      detail += ", path " + u.pathname;
    }
    if (!ok) failures++;
    console.log((ok ? "PASS" : "FAIL") + " " + item.label + ": " + detail);
  }
  if (failures) {
    console.log(failures + " test(s) failed");
    process.exit(1);
  }
  console.log("All tests passed");
})();
