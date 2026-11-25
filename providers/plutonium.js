"use strict";

var BOOT_URL = "https://boot.pluto.tv/v4/start";
var SEARCH_URL = "https://service-media-search.clusters.pluto.tv";
var VOD_URL = "https://service-vod.clusters.pluto.tv";
var TMDB_BASE = "https://api.themoviedb.org/3";
var UA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36";
var LOG = "[Plutonium] ";
var MAX_PTV = 12;
var SEARCH_CANDIDATES = 8;
var FALLBACK_LIMIT = 18;

// Fallbacks for clients that have no per-scraper settings UI (for example
// Nuvio on Android TV). Any saved scraper settings override these wholesale,
// so desktop installs keep their own country/XFF/proxy values.
var LAN_DEFAULTS = {
  country: "US",
  forwardedFor: "74.83.52.188",
  proxyUrl: "https://nuvio-plutonium.marc-ballegeer.workers.dev"
};

function warn(msg) {
  console.log(LOG + "WARN " + msg);
}

function debugLog(cfg, msg) {
  if (cfg.debug) console.log(LOG + msg);
}

function geoHint(cfg) {
  if (!cfg.forwardedFor) {
    warn("hint: outside the US, set X-Forwarded-For to a US IP in the plugin settings");
  }
}

function readSettings() {
  var raw = globalThis.SCRAPER_SETTINGS;
  if (typeof raw === "string") {
    try {
      raw = JSON.parse(raw);
    } catch (e) {
      raw = {};
    }
  }
  if (!raw || typeof raw !== "object") raw = {};
  var hasSettings = false;
  for (var key in raw) {
    if (Object.prototype.hasOwnProperty.call(raw, key)) {
      hasSettings = true;
      break;
    }
  }
  if (!hasSettings) raw = LAN_DEFAULTS;
  var country = String(raw.country || "US").toUpperCase();
  if (country !== "US" && country !== "CA" && country !== "GB") country = "US";
  var forwardedFor = String(raw.forwardedFor || "").trim();
  if (/[\r\n]/.test(forwardedFor)) forwardedFor = "";
  var proxyUrl = String(raw.proxyUrl || "").trim();
  if (proxyUrl) {
    if (!/^https?:\/\//i.test(proxyUrl)) proxyUrl = "http://" + proxyUrl;
    proxyUrl = proxyUrl.replace(/\/+$/, "");
    if (!/^https?:\/\/[^\/\s]+/i.test(proxyUrl)) proxyUrl = "";
  }
  return {
    country: country,
    forwardedFor: forwardedFor,
    proxyUrl: proxyUrl,
    tmdbApiKey: String(raw.tmdbApiKey || "").trim(),
    debug: raw.debug === true || raw.debug === "true"
  };
}

function tmdbKey(cfg) {
  if (cfg.tmdbApiKey) return cfg.tmdbApiKey;
  var key = globalThis.TMDB_API_KEY;
  return key == null ? "" : String(key).trim();
}

function queryString(params) {
  var parts = [];
  for (var i = 0; i < params.length; i++) {
    var key = params[i][0];
    var value = params[i][1];
    parts.push(encodeURIComponent(key) + "=" + encodeURIComponent(value == null ? "" : String(value)));
  }
  return parts.join("&");
}

function tmdbRequest(path, apiKey, params) {
  var bearer = String(apiKey).indexOf("eyJ") === 0;
  var query = [];
  for (var i = 0; i < params.length; i++) query.push(params[i]);
  if (!bearer) query.push(["api_key", apiKey]);
  var headers = { "User-Agent": UA, "Accept": "application/json" };
  if (bearer) headers["Authorization"] = "Bearer " + apiKey;
  return {
    url: TMDB_BASE + path + "?" + queryString(query),
    headers: headers
  };
}

function uuid() {
  var out = "";
  for (var i = 0; i < 36; i++) {
    if (i === 8 || i === 13 || i === 18 || i === 23) out += "-";
    else if (i === 14) out += "4";
    else if (i === 19) out += (Math.floor(Math.random() * 4) + 8).toString(16);
    else out += Math.floor(Math.random() * 16).toString(16);
  }
  return out;
}

var ACCENTS = {
  "à": "a", "á": "a", "â": "a", "ã": "a", "ä": "a", "å": "a", "ā": "a", "ă": "a", "ą": "a",
  "ç": "c", "ć": "c", "č": "c",
  "ď": "d",
  "è": "e", "é": "e", "ê": "e", "ë": "e", "ē": "e", "ė": "e", "ę": "e", "ě": "e",
  "ğ": "g",
  "ì": "i", "í": "i", "î": "i", "ï": "i", "ī": "i", "į": "i",
  "ł": "l", "ń": "n", "ñ": "n", "ň": "n",
  "ò": "o", "ó": "o", "ô": "o", "õ": "o", "ö": "o", "ø": "o", "ō": "o", "ő": "o",
  "ř": "r", "ś": "s", "š": "s", "ş": "s",
  "ť": "t", "ţ": "t",
  "ù": "u", "ú": "u", "û": "u", "ü": "u", "ū": "u", "ů": "u", "ű": "u",
  "ý": "y", "ÿ": "y", "ž": "z", "ź": "z", "ż": "z",
  "æ": "ae", "œ": "oe", "ß": "ss"
};

function stripAccents(text) {
  var out = "";
  for (var i = 0; i < text.length; i++) {
    var ch = text.charAt(i);
    out += ACCENTS[ch] || ch;
  }
  return out;
}

function cleanTitle(text) {
  var s = String(text == null ? "" : text);
  s = s.replace(/\([^)]*\)/g, " ").replace(/\[[^\]]*\]/g, " ");
  return s.replace(/\s+/g, " ").trim();
}

function normalizeTitle(text) {
  var s = String(text == null ? "" : text);
  s = s.replace(/\([^)]*\)/g, " ").replace(/\[[^\]]*\]/g, " ");
  s = stripAccents(s.toLowerCase());
  s = s.replace(/&/g, " and ");
  s = s.replace(/['’`]/g, "");
  s = s.replace(/[^a-z0-9]+/g, " ").trim();
  if (s.indexOf("the ") === 0) s = s.slice(4);
  return s;
}

function toSlug(text) {
  var s = stripAccents(String(text == null ? "" : text).toLowerCase());
  s = s.replace(/&/g, " and ");
  s = s.replace(/['’`]/g, "");
  s = s.replace(/[^a-z0-9]+/g, "-");
  return s.replace(/-+/g, "-").replace(/^-+|-+$/g, "");
}

function pushUnique(arr, value) {
  if (value && arr.indexOf(value) < 0) arr.push(value);
}

function slugVariants(title) {
  var cleaned = cleanTitle(title);
  var out = [];
  pushUnique(out, toSlug(cleaned));
  if (cleaned.indexOf("&") >= 0) pushUnique(out, toSlug(cleaned.replace(/&/g, " ")));
  if (/['’`]/.test(cleaned)) pushUnique(out, toSlug(cleaned.replace(/['’`]/g, "-")));
  return out;
}

function yearFromDate(value) {
  var match = String(value == null ? "" : value).match(/(19|20)\d{2}/);
  return match ? parseInt(match[0], 10) : null;
}

function itemYear(item) {
  var clip = item.clip || {};
  var year = yearFromDate(clip.originalReleaseDate);
  if (!year) year = yearFromDate(item.releaseDate);
  if (!year && item.slug) {
    var parts = String(item.slug).split("-");
    for (var i = parts.length - 1; i >= 0; i--) {
      if (/^(19|20)\d{2}$/.test(parts[i])) {
        year = parseInt(parts[i], 10);
        break;
      }
    }
  }
  return year;
}

function seriesYear(item) {
  var seasons = item.seasons || [];
  var earliest = null;
  for (var i = 0; i < seasons.length; i++) {
    if (!seasons[i]) continue;
    var episodes = seasons[i].episodes || [];
    for (var j = 0; j < episodes.length; j++) {
      var clip = episodes[j] && episodes[j].clip ? episodes[j].clip : null;
      var year = clip ? yearFromDate(clip.originalReleaseDate) : null;
      if (year && (!earliest || year < earliest)) earliest = year;
    }
  }
  return earliest;
}

function normalizeMediaType(value) {
  var t = String(value == null ? "" : value).toLowerCase();
  if (t === "movie" || t === "film") return "movie";
  if (t === "tv" || t === "series" || t === "show") return "tv";
  return "";
}

function parseTmdbId(value) {
  var s = String(value == null ? "" : value).trim();
  s = s.replace(/^(tmdb|movie|tv|series|show):/i, "");
  if (/^\d+$/.test(s) && parseInt(s, 10) > 0) return s;
  return null;
}

function toInt(value) {
  if (value == null || value === "") return null;
  var n = parseInt(value, 10);
  return isNaN(n) ? null : n;
}

async function requestJson(url, headers, label) {
  var response;
  try {
    response = await fetch(url, { method: "GET", headers: headers });
  } catch (e) {
    warn(label + " network error: " + (e && e.message ? e.message : e));
    return null;
  }
  if (!response) {
    warn(label + " empty response");
    return null;
  }
  if (!response.ok) {
    warn(label + " HTTP " + response.status);
    return null;
  }
  var text;
  try {
    text = await response.text();
  } catch (e2) {
    warn(label + " read error: " + (e2 && e2.message ? e2.message : e2));
    return null;
  }
  if (!text) return null;
  try {
    return JSON.parse(text);
  } catch (e3) {
    warn(label + " invalid JSON (" + text.length + " bytes)");
    return null;
  }
}

function plutoHeaders(cfg) {
  var headers = {
    "User-Agent": UA,
    "Accept": "application/json, text/plain, */*"
  };
  if (cfg.forwardedFor) headers["X-Forwarded-For"] = cfg.forwardedFor;
  return headers;
}

function streamHeaders(cfg) {
  var headers = { "User-Agent": UA };
  if (cfg.forwardedFor) headers["X-Forwarded-For"] = cfg.forwardedFor;
  return headers;
}

function makeInfo(data, kind) {
  var title = kind === "movie" ? data.title : data.name;
  var original = kind === "movie" ? data.original_title : data.original_name;
  var year = yearFromDate(kind === "movie" ? data.release_date : data.first_air_date);
  var englishRegions = { US: 1, GB: 1, CA: 1, AU: 1, IE: 1, NZ: 1, "": 1 };
  var altTitles = [];
  var altBlock = data.alternative_titles || {};
  var altList = kind === "movie" ? (altBlock.titles || []) : (altBlock.results || []);
  for (var i = 0; i < altList.length; i++) {
    var entry = altList[i];
    if (!entry || !entry.title) continue;
    var countryCode = String(entry.iso_3166_1 || "");
    if (!englishRegions[countryCode]) continue;
    altTitles.push({ title: String(entry.title), country: countryCode });
  }
  var rank = { US: 0, GB: 1, CA: 2, AU: 3 };
  altTitles.sort(function (a, b) {
    var ra = rank[a.country] == null ? 4 : rank[a.country];
    var rb = rank[b.country] == null ? 4 : rank[b.country];
    return ra - rb;
  });
  var altNormalized = [];
  for (var j = 0; j < altTitles.length; j++) {
    var normalized = normalizeTitle(altTitles[j].title);
    if (normalized && altNormalized.indexOf(normalized) < 0) altNormalized.push(normalized);
  }
  return {
    kind: kind,
    title: String(title || ""),
    originalTitle: String(original || ""),
    year: year,
    alts: altTitles,
    altNormalized: altNormalized
  };
}

async function getTmdbInfo(tmdbId, kind, cfg) {
  var key = tmdbKey(cfg);
  if (!key) {
    warn("TMDB API key missing");
    return null;
  }
  var request = tmdbRequest("/" + kind + "/" + encodeURIComponent(tmdbId), key, [
    ["language", "en-US"],
    ["append_to_response", "alternative_titles"]
  ]);
  var data = await requestJson(request.url, request.headers, "tmdb-details");
  if (!data || !data.id) {
    warn("TMDB details failed for " + kind + " " + tmdbId);
    return null;
  }
  return makeInfo(data, kind);
}

async function resolveImdb(imdbId, kind, cfg) {
  var key = tmdbKey(cfg);
  if (!key) {
    warn("TMDB API key missing");
    return null;
  }
  var request = tmdbRequest("/find/" + encodeURIComponent(imdbId), key, [
    ["external_source", "imdb_id"],
    ["language", "en-US"]
  ]);
  var data = await requestJson(request.url, request.headers, "tmdb-find");
  if (!data) return null;
  var results = kind === "movie" ? data.movie_results : data.tv_results;
  if (!results || !results.length) return null;
  return String(results[0].id);
}

async function bootPluto(cfg) {
  var params = [
    ["appName", "web"],
    ["appVersion", "1"],
    ["deviceVersion", "0.0.0"],
    ["deviceModel", "web"],
    ["deviceMake", "firefox"],
    ["deviceType", "web"],
    ["clientID", uuid()],
    ["clientModelNumber", "1.0.0"],
    ["serverSideAds", "false"],
    ["country", cfg.country]
  ];
  var data = await requestJson(BOOT_URL + "?" + queryString(params), plutoHeaders(cfg), "boot");
  if (!data || !data.sessionToken) {
    warn("boot returned no session token");
    return null;
  }
  var stitcher = data.servers && data.servers.stitcher ? String(data.servers.stitcher) : "";
  if (!stitcher) {
    warn("boot returned no stitcher");
    return null;
  }
  var stitcherParams = data.stitcherParams ? String(data.stitcherParams) : "";
  stitcherParams = stitcherParams.replace(/^[?&]+/, "").replace(/&+$/, "");
  var servers = data.servers || {};
  return {
    token: String(data.sessionToken),
    stitcher: stitcher.replace(/\/+$/, ""),
    stitcherParams: stitcherParams,
    search: servers.search ? String(servers.search) : SEARCH_URL,
    vod: servers.vod ? String(servers.vod) : VOD_URL
  };
}

function vodBase(cfg) {
  return String(cfg.vod || VOD_URL).replace(/\/+$/, "");
}

async function lookupSlugs(slugs, token, cfg) {
  var url = vodBase(cfg) + "/v4/vod/slugs?slugs=" + slugs.map(encodeURIComponent).join(",");
  var headers = plutoHeaders(cfg);
  headers["Authorization"] = "Bearer " + token;
  var data = await requestJson(url, headers, "slugs");
  if (!Array.isArray(data)) {
    if (data) warn("slugs returned a non-array response");
    return [];
  }
  return data;
}

async function searchPluto(query, token, cfg) {
  var base = String(cfg.search || SEARCH_URL).replace(/\/+$/, "");
  var url = base + "/v1/search?q=" + encodeURIComponent(query);
  var headers = plutoHeaders(cfg);
  headers["Authorization"] = "Bearer " + token;
  var data = await requestJson(url, headers, "search");
  if (!data || !Array.isArray(data.data)) {
    if (data) warn("search returned an unexpected response for " + query);
    return [];
  }
  return data.data;
}

function titleList(info, includeAlternates, limit) {
  var out = [];
  pushTitle(out, info.title);
  pushTitle(out, info.originalTitle);
  if (!includeAlternates) return out;
  var max = limit || 6;
  for (var i = 0; i < info.alts.length && out.length < max; i++) {
    pushTitle(out, info.alts[i].title);
  }
  return out;
}

function pushTitle(out, title) {
  if (!title) return;
  var normalized = normalizeTitle(title);
  if (!normalized) return;
  for (var i = 0; i < out.length; i++) {
    if (normalizeTitle(out[i]) === normalized) return;
  }
  out.push(String(title));
}

function movieCandidatesFor(titles, years) {
  var out = [];
  for (var i = 0; i < titles.length; i++) {
    var variants = slugVariants(titles[i]);
    for (var j = 0; j < variants.length; j++) {
      var base = variants[j];
      for (var k = 0; k < years.length; k++) {
        var year = years[k];
        if (!year) continue;
        pushUnique(out, base + "-" + year + "-1-1");
        pushUnique(out, base + "-" + year + "-" + year + "-1-1");
      }
    }
  }
  return out;
}

function seriesCandidatesFor(titles, maxTitles) {
  var out = [];
  var used = 0;
  for (var i = 0; i < titles.length && used < maxTitles; i++) {
    var variants = slugVariants(titles[i]);
    if (!variants.length) continue;
    used++;
    for (var j = 0; j < variants.length; j++) {
      var base = variants[j];
      pushUnique(out, base);
      for (var n = 1; n <= MAX_PTV; n++) pushUnique(out, base + "-ptv" + n);
    }
  }
  return out;
}

function matchScore(name, target) {
  if (!target) return 0;
  if (name === target) return 100;
  if (name.indexOf(target) >= 0 || target.indexOf(name) >= 0) {
    var diff = Math.abs(name.length - target.length);
    if (diff <= 3) return 80;
    if (diff <= 8) return 60;
  }
  return 0;
}

function scoreItem(item, info) {
  if (!item || !item._id) return -1;
  var type = String(item.type || "");
  if (info.kind === "movie" && type !== "movie") return -1;
  if (info.kind === "tv" && type !== "series") return -1;
  var name = normalizeTitle(item.name);
  if (!name) return -1;
  var score = matchScore(name, normalizeTitle(info.title));
  if (!score) score = matchScore(name, normalizeTitle(info.originalTitle));
  if (!score) {
    for (var i = 0; i < info.altNormalized.length; i++) {
      var altScore = matchScore(name, info.altNormalized[i]);
      if (altScore > score) score = altScore;
    }
  }
  if (!score) return -1;
  if (info.kind === "movie" && info.year) {
    var year = itemYear(item);
    if (year) {
      var diff = Math.abs(year - info.year);
      if (diff === 0) score += 10;
      else if (diff === 1) score += 5;
      else return -1;
    }
  }
  if (info.kind === "tv" && info.year) {
    var seriesPremiere = seriesYear(item);
    if (seriesPremiere) {
      var showDiff = Math.abs(seriesPremiere - info.year);
      if (showDiff === 0) score += 10;
      else if (showDiff === 1) score += 5;
    }
  }
  return score;
}

function pickBest(items, info, seasonNumber, episodeNumber) {
  var best = null;
  var bestEpisode = null;
  var bestScore = -1;
  for (var i = 0; i < items.length; i++) {
    var score = scoreItem(items[i], info);
    if (score < 0) continue;
    var episodeItem = null;
    if (info.kind === "tv") {
      if (episodeNumber == null) continue;
      episodeItem = findEpisode(items[i], seasonNumber, episodeNumber);
      if (!episodeItem) continue;
      score += 20;
    }
    if (score > bestScore) {
      best = items[i];
      bestEpisode = episodeItem;
      bestScore = score;
    }
  }
  return bestScore >= 80 ? { item: best, episode: bestEpisode } : null;
}

function scoreSearchRow(row, info) {
  if (!row || !row.slug) return -1;
  var type = String(row.type || "");
  if (info.kind === "movie" && type !== "movie") return -1;
  if (info.kind === "tv" && type !== "series") return -1;
  var name = normalizeTitle(row.name);
  if (!name) return -1;
  var score = matchScore(name, normalizeTitle(info.title));
  if (!score) score = matchScore(name, normalizeTitle(info.originalTitle));
  if (!score) {
    for (var i = 0; i < info.altNormalized.length; i++) {
      var altScore = matchScore(name, info.altNormalized[i]);
      if (altScore > score) score = altScore;
    }
  }
  if (!score) return -1;
  var year = itemYear(row);
  if (year && info.year) {
    var diff = Math.abs(year - info.year);
    if (diff === 0) score += 10;
    else if (diff === 1) score += 5;
    else if (diff <= 3) score -= 5;
    else score -= 15;
  }
  return score;
}

function searchCandidates(rows, info) {
  var scored = [];
  var seen = Object.create(null);
  for (var i = 0; i < rows.length; i++) {
    var row = rows[i];
    var slug = row && row.slug ? String(row.slug) : "";
    if (!slug || seen[slug]) continue;
    var score = scoreSearchRow(row, info);
    if (score < 60) continue;
    seen[slug] = true;
    scored.push({ row: row, score: score });
  }
  scored.sort(function (a, b) { return b.score - a.score; });
  var out = [];
  for (var j = 0; j < scored.length && j < SEARCH_CANDIDATES; j++) out.push(scored[j].row);
  return out;
}

async function safeSearch(query, token, cfg) {
  try {
    return await searchPluto(query, token, cfg);
  } catch (e) {
    warn("search failed for " + query + ": " + (e && e.message ? e.message : e));
    return [];
  }
}

function fallbackSlugs(info) {
  var titles = titleList(info, true, 2);
  var out = [];
  if (info.kind === "movie") {
    if (!info.year) return out;
    var movies = movieCandidatesFor(titles, [info.year, info.year - 1, info.year + 1]);
    for (var i = 0; i < movies.length && out.length < FALLBACK_LIMIT; i++) pushUnique(out, movies[i]);
  } else {
    var series = seriesCandidatesFor(titles, 2);
    for (var j = 0; j < series.length && out.length < FALLBACK_LIMIT; j++) pushUnique(out, series[j]);
  }
  return out;
}

async function findBestMatch(info, seasonNumber, episodeNumber, token, cfg) {
  var queries = titleList(info, true, 3);
  var lists = await Promise.all(queries.map(function (query) {
    return safeSearch(query, token, cfg);
  }));
  var rows = [];
  for (var i = 0; i < lists.length; i++) {
    for (var j = 0; j < lists[i].length; j++) rows.push(lists[i][j]);
  }

  var candidates = searchCandidates(rows, info);
  var slugs = [];
  for (var c = 0; c < candidates.length; c++) slugs.push(String(candidates[c].slug));
  debugLog(cfg, "search " + info.title + " -> " + slugs.length + " candidate slug(s)");
  var items = slugs.length ? await lookupSlugs(slugs, token, cfg) : [];
  var match = pickBest(items, info, seasonNumber, episodeNumber);
  if (match) return match;

  var fallback = fallbackSlugs(info);
  if (fallback.length) {
    debugLog(cfg, "fallback " + info.kind + " slugs: " + fallback.join(","));
    var fallbackItems = await lookupSlugs(fallback, token, cfg);
    match = pickBest(fallbackItems, info, seasonNumber, episodeNumber);
    if (match) return match;
  }
  return null;
}

function sortedSeasons(series) {
  var seasons = [];
  var source = series && series.seasons ? series.seasons : [];
  for (var i = 0; i < source.length; i++) {
    if (source[i]) seasons.push(source[i]);
  }
  seasons.sort(function (a, b) {
    return (Number(a.number) || 0) - (Number(b.number) || 0);
  });
  return seasons;
}

function findEpisode(series, seasonNumber, episodeNumber) {
  var seasons = sortedSeasons(series);
  if (seasonNumber == null) {
    var all = [];
    for (var i = 0; i < seasons.length; i++) {
      var episodes = seasons[i].episodes || [];
      for (var j = 0; j < episodes.length; j++) all.push(episodes[j]);
    }
    return episodeNumber >= 1 && episodeNumber <= all.length ? all[episodeNumber - 1] : null;
  }
  var season = null;
  for (var k = 0; k < seasons.length; k++) {
    if (Number(seasons[k].number) === seasonNumber) {
      season = seasons[k];
      break;
    }
  }
  if (!season) return null;
  var list = season.episodes || [];
  for (var m = 0; m < list.length; m++) {
    if (list[m] && Number(list[m].number) === episodeNumber) return list[m];
  }
  if (episodeNumber >= 1 && episodeNumber <= list.length) return list[episodeNumber - 1];
  return null;
}

function buildStream(mediaId, cfg) {
  var path = "/v2/stitch/hls/episode/" + encodeURIComponent(mediaId) + "/master.m3u8";
  var parts = [];
  if (cfg.stitcherParams) parts.push(cfg.stitcherParams);
  parts.push("jwt=" + encodeURIComponent(cfg.token));
  parts.push("masterJWTPassthrough=1");
  var rawUrl = cfg.stitcher + path + "?" + parts.join("&");
  var url = rawUrl;
  if (cfg.proxyUrl) {
    url = cfg.proxyUrl + "/master.m3u8?url=" + encodeURIComponent(rawUrl);
    if (cfg.forwardedFor) url += "&xff=" + encodeURIComponent(cfg.forwardedFor);
    debugLog(cfg, "using ad-strip proxy " + cfg.proxyUrl);
  }
  var stream = {
    name: "Pluto TV",
    title: "Pluto TV (" + cfg.country + ") • Auto" + (cfg.proxyUrl ? " • ad-free" : ""),
    url: url,
    quality: "Auto",
    headers: streamHeaders(cfg),
    provider: "pluto",
    type: "hls"
  };
  if (cfg.country === "US" || cfg.country === "CA" || cfg.country === "GB") stream.language = "en";
  return stream;
}

async function getStreams(tmdbId, mediaType, season, episode) {
  var settings = readSettings();
  try {
    var kind = normalizeMediaType(mediaType);
    if (!kind) {
      warn("unsupported media type: " + mediaType);
      return [];
    }
    var id = parseTmdbId(tmdbId);
    if (!id) {
      var rawId = String(tmdbId == null ? "" : tmdbId).trim();
      if (/^tt\d+$/i.test(rawId)) {
        id = await resolveImdb(rawId, kind, settings);
      }
      if (!id) {
        warn("cannot resolve TMDB id: " + rawId);
        return [];
      }
    }
    var seasonNumber = kind === "tv" ? toInt(season) : null;
    var episodeNumber = kind === "tv" ? toInt(episode) : null;
    if (kind === "tv" && episodeNumber == null) {
      warn("missing episode number");
      return [];
    }

    // TMDb details and Pluto boot are independent, so start both at once.
    var startup = await Promise.all([
      getTmdbInfo(id, kind, settings),
      bootPluto(settings)
    ]);
    var info = startup[0];
    var boot = startup[1];
    if (!info) return [];
    if (!boot) {
      geoHint(settings);
      return [];
    }
    settings.token = boot.token;
    settings.stitcher = boot.stitcher;
    settings.stitcherParams = boot.stitcherParams;
    settings.search = boot.search;
    settings.vod = boot.vod;

    var match = await findBestMatch(info, seasonNumber, episodeNumber, boot.token, settings);
    if (!match) {
      warn("no Pluto match for " + info.title + " (" + kind + " " + id + ")");
      geoHint(settings);
      return [];
    }
    debugLog(settings, "matched " + match.item.type + " slug " + match.item.slug + " (" + match.item.name + ")");

    if (kind === "movie") {
      if (!match.item._id) return [];
      return [buildStream(match.item._id, settings)];
    }
    var episodeItem = match.episode;
    if (!episodeItem || !episodeItem._id) {
      warn("episode S" + seasonNumber + "E" + episodeNumber + " not found in " + match.item.slug);
      return [];
    }
    debugLog(settings, "episode " + episodeItem.slug + " (" + episodeItem.name + ")");
    return [buildStream(episodeItem._id, settings)];
  } catch (e) {
    warn("unexpected error: " + (e && e.stack ? e.stack : e));
    return [];
  }
}

function onSettings() {
  return [
    { type: "header", label: "Pluto TV" },
    { type: "info", label: "Streams come from Pluto TV's free on-demand catalog. Pluto ad breaks are embedded in the HLS playlist. The Cloudflare Worker ad-strip proxy is enabled by default; leave Proxy URL as-is to stream ad-free." },
    { type: "select", key: "country", label: "Catalog region", defaultValue: "US", options: [
      { label: "United States", value: "US" },
      { label: "Canada", value: "CA" },
      { label: "United Kingdom", value: "GB" }
    ], description: "Only these English-language Pluto TV catalogs are supported." },
    { type: "text", key: "forwardedFor", label: "X-Forwarded-For (optional)", defaultValue: "74.83.52.188", placeholder: "e.g. 203.0.113.10", description: "Pluto uses this header for geo-detection. A US IP is set by default; change it only if Pluto serves the wrong catalog." },
    { type: "text", key: "proxyUrl", label: "Ad-strip proxy URL (optional)", defaultValue: "https://nuvio-plutonium.marc-ballegeer.workers.dev", placeholder: "https://nuvio-plutonium.marc-ballegeer.workers.dev", description: "Cloudflare Worker that strips Pluto ad segments and resyncs captions. Leave as-is for ad-free streams; clear it to play the original stream with ads." },
    { type: "text", key: "tmdbApiKey", label: "TMDB API key (optional)", isPassword: true, placeholder: "Use the app's key", description: "Only needed if the app does not provide a TMDB API key." },
    { type: "toggle", key: "debug", label: "Verbose logs", defaultValue: false, description: "Log every slug batch and matched item." }
  ];
}

module.exports = {
  getStreams: getStreams,
  onSettings: onSettings
};
