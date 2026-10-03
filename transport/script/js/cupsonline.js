// Script-transport port of transport/cupsonline/cupsonline.go. Smuggles
// packets through a real Centrifugo pub/sub channel: data rides as
// {row, column} integer pairs in a "shared_editor_change_cursors" RPC
// (cups.online rejects a non-numeric column but relays a cursor array of
// integers verbatim, in order), split across N=4 independent rooms for
// throughput and redundancy, round-robined on send.
//
// Each room needs its OWN cookie jar (its own csrftoken/session) - see
// http.newSession() in transport/script/host.go, added specifically for
// this: cookie names collide across sessions on one domain, so a single
// shared jar would let the last room's authorize() call silently clobber
// every earlier room's cookies. With isolated sessions, joining all rooms
// at once (Promise.all, matching native joinListed) is both safe and
// faster - no race to avoid, so no reason to serialize it.
//
// The exit-side "no rooms given -> create N fresh ones and print the
// packed list for the operator to copy" convenience is not ported: it's
// an operational nicety (first-time setup), not a wire-protocol
// requirement, and every real use of this transport joins existing rooms.

var UA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/137.0.0.0 Safari/537.36";
var BASE_ROOM_URL = "https://interview.cups.online/live-coding/";

var NUM_ROOMS = 4;
var WS_HANDSHAKE_MS = 15000;
var WS_READ_TIMEOUT_MS = 90000;
var KEEPALIVE_MS = 20000;
var RECONNECT_MIN_MS = 100;
var RECONNECT_MAX_MS = 10000;
var RECONNECT_MULTIPLIER = 1.3;
var ROOM_GONE_RETRY_MIN_MS = 60000;
var ROOM_GONE_RETRY_MAX_MS = 30 * 60000;
var ROOM_DEAD_AFTER_FAILS = 5;

var BATCH_MAX_PACKETS = 256;
var BATCH_MAX_BYTES = 11000;
var MAX_MESSAGE_DATA = 4096;
var BATCH_TIMEOUT_MS = 2;
var SEND_INTERVAL_MS = 18;
var MAX_PAYLOAD_BYTES = 65535;
var BYTES_PER_NUMBER = 6;

var ROOM_RE = /data-room="\{&quot;uuid&quot;:\s*&quot;([0-9a-f-]{36})&quot;/;
var USER_RE = /data-user="\{&quot;uuid&quot;:\s*&quot;([0-9a-f-]{36})&quot;/;
var CONN_TOKEN_RE = /<meta[^>]+name="centrifuge-connection-token"[^>]+content="([^"]+)"/;
var CONN_URL_RE = /<meta[^>]+name="centrifuge-connection-url"[^>]+content="([^"]+)"/;
var SUB_URL_RE = /<meta[^>]+name="centrifuge-subscription-token-url"[^>]+content="([^"]+)"/;

var running = false;
var rooms = [];
var rrIndex = 0;

// ---- room list packing (base64.RawURLEncoding of a JSON array, native's
// packRooms/unpackRooms) - no host primitive for that alphabet, so it's a
// thin substitution wrapper around base64.encode/decode. ----

function b64urlDecode(s) {
  var b64 = s.replace(/-/g, "+").replace(/_/g, "/");
  var pad = (4 - (b64.length % 4)) % 4;
  for (var i = 0; i < pad; i++) b64 += "=";
  return base64.decode(b64);
}

function b64urlEncode(bytes) {
  return base64.encode(bytes).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

function parseRoomList(rawURL) {
  rawURL = (rawURL || "").trim();
  if (!rawURL) return [];
  try {
    var p = url.parse(rawURL);
    var qs = parseQuery(p.search);
    if (qs.rooms) {
      var ids = JSON.parse(text.decode(b64urlDecode(qs.rooms)));
      if (Array.isArray(ids)) return ids;
    } else if (qs.room) {
      return [qs.room];
    }
  } catch (e) { /* not a URL with query params - fall through */ }
  try {
    var direct = JSON.parse(text.decode(b64urlDecode(rawURL)));
    if (Array.isArray(direct)) return direct;
  } catch (e) {}
  return [];
}

function parseQuery(qs) {
  var out = {};
  if (!qs) return out;
  var parts = qs.split("&");
  for (var i = 0; i < parts.length; i++) {
    if (!parts[i]) continue;
    var kv = parts[i].split("=");
    out[decodeURIComponent(kv[0])] = decodeURIComponent((kv[1] || "").replace(/\+/g, "%20"));
  }
  return out;
}

function originOf(u) {
  var p = url.parse(u);
  return p.protocol + "//" + p.host;
}

// ---- per-room object ----

function Room(idx, id) {
  this.idx = idx;
  this.id = id;
  this.session = http.newSession();
  this.auth = null; // { roomUUID, userUUID, connToken, connURL, subURL, subToken, channel, csrfToken, cookieHeader }
  this.sock = null;
  this.connected = false;
  this.dead = false;
  this.goneDelay = ROOM_GONE_RETRY_MIN_MS;
  this.reconnectDelay = RECONNECT_MIN_MS;
  this.fails = 0;
  this.rpcId = 2;
  this.pendingWaiters = [];
  this.sendQueue = [];
  this.batch = [];
  this.batchBytes = 0;
  this.batchTimer = null;
  this.lastSend = 0;
  this.recvBuf = new Uint8Array(0);
  this.needJoin = true;
}

Room.prototype.nextID = function () { return ++this.rpcId; };

Room.prototype.joinURL = function () {
  return BASE_ROOM_URL + "?room=" + this.id;
};

// authorize (this room's own session -> own jar, own csrftoken).
Room.prototype.authorize = async function () {
  var res = await this.session.fetch({
    url: this.joinURL(),
    headers: {
      "User-Agent": UA,
      Accept: "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
      "Accept-Language": "ru-RU,ru;q=0.9",
    },
  });
  if (res.status === 404 || res.status === 410) {
    throw new RoomGoneError("GET room status " + res.status);
  }
  if (res.status !== 200) throw new Error("GET room status " + res.status);

  var html = res.body;
  var roomUUID = firstMatch(ROOM_RE, html);
  var userUUID = firstMatch(USER_RE, html);
  var connToken = firstMatch(CONN_TOKEN_RE, html);
  var connURL = firstMatch(CONN_URL_RE, html);
  var subURL = firstMatch(SUB_URL_RE, html);

  if (!roomUUID || !userUUID) throw new RoomGoneError("room/user uuid missing");
  if (roomUUID !== this.id) throw new RoomGoneError("вместо неё выдана новая " + roomUUID.slice(0, 8));
  if (!connToken || !connURL || !subURL) throw new Error("centrifuge meta missing");

  var cookies = this.session.cookies.get(this.joinURL());
  var csrfToken = cookies["csrftoken"] || "";
  if (!csrfToken) throw new Error("csrftoken missing");

  var channel = "$shared_editor:room-" + roomUUID;

  var subRes = await this.session.fetch({
    url: subURL,
    method: "POST",
    headers: {
      "User-Agent": UA,
      "Content-Type": "application/json",
      "X-CSRFToken": csrfToken,
      Origin: originOf(this.joinURL()),
      Referer: this.joinURL(),
    },
    body: JSON.stringify({ channel: channel }),
  });
  if (subRes.status !== 200) throw new Error("sub token status " + subRes.status);
  var subToken = (JSON.parse(subRes.body) || {}).token;
  if (!subToken) throw new Error("empty sub token");

  var finalCookies = this.session.cookies.get(this.joinURL());
  var cookieHeader = Object.keys(finalCookies).map(function (k) { return k + "=" + finalCookies[k]; }).join("; ");

  this.auth = {
    roomUUID: roomUUID, userUUID: userUUID, connToken: connToken,
    connURL: connURL, subURL: subURL, subToken: subToken,
    channel: channel, cookieHeader: cookieHeader,
  };
};

function RoomGoneError(msg) {
  this.message = msg;
  this.isRoomGone = true;
}
RoomGoneError.prototype = Object.create(Error.prototype);

function firstMatch(re, s) {
  var m = re.exec(s);
  return m ? m[1] : "";
}

// ---- reply waiting (a frame may pack several JSON lines; a ping "{}"
// answers itself; anything else is either a reply to a pending id or a
// push) ----

function isPing(line) { return line === "{}"; }

Room.prototype.waitForReply = function (id, timeoutMs) {
  var room = this;
  return new Promise(function (resolve, reject) {
    var waiter = { done: false };
    waiter.pred = function (line) {
      if (isPing(line)) return false;
      var reply;
      try { reply = JSON.parse(line); } catch (e) { return false; }
      return reply && reply.id === id;
    };
    var timer = setTimeout(function () {
      if (waiter.done) return;
      waiter.done = true;
      var i = room.pendingWaiters.indexOf(waiter);
      if (i >= 0) room.pendingWaiters.splice(i, 1);
      reject(new Error("timeout waiting for reply " + id));
    }, timeoutMs);
    waiter.settle = function (line) {
      if (waiter.done) return;
      waiter.done = true;
      clearTimeout(timer);
      var reply = JSON.parse(line);
      if (reply.error) {
        var err = new Error(reply.error.code + " " + reply.error.message);
        err.isRefused = true; // Centrifugo turned the token down - only a fresh join fixes that
        reject(err);
      } else {
        resolve(reply);
      }
    };
    room.pendingWaiters.push(waiter);
  });
};

Room.prototype.dispatchToWaiters = function (line) {
  for (var i = 0; i < this.pendingWaiters.length; i++) {
    var w = this.pendingWaiters[i];
    if (w.pred(line)) {
      this.pendingWaiters.splice(i, 1);
      w.settle(line);
      return true;
    }
  }
  return false;
};

Room.prototype.writeRaw = function (data) {
  if (!this.sock) throw new Error("ws not connected");
  this.sock.send(data);
};

Room.prototype.writeJSON = function (v) {
  this.writeRaw(JSON.stringify(v));
};

// ---- connect + handshake ----

Room.prototype.wsURL = function () {
  var u = this.auth.connURL.replace("https://", "wss://").replace("http://", "ws://");
  return u.replace(/\/+$/, "") + "/websocket";
};

Room.prototype.connectAndServe = async function () {
  var room = this;
  var sock = await ws.open(this.wsURL(), {
    Origin: originOf(this.auth.connURL),
    "User-Agent": UA,
    Cookie: this.auth.cookieHeader,
  }, { readTimeoutMs: WS_READ_TIMEOUT_MS });

  this.sock = sock;
  this.recvBuf = new Uint8Array(0);

  sock.onmessage = function (raw) { room.onFrame(raw); };
  var closed = new Promise(function (resolve) {
    sock.onclose = function (reason) { resolve(reason); };
  });

  this.writeJSON({ id: 1, connect: { token: this.auth.connToken, name: "js" } });
  await this.waitForReply(1, WS_HANDSHAKE_MS);

  this.writeJSON({ id: 2, subscribe: { channel: this.auth.channel, token: this.auth.subToken } });
  await this.waitForReply(2, WS_HANDSHAKE_MS);

  this.connected = true;
  this.markAlive();
  this.reconnectDelay = RECONNECT_MIN_MS;
  this.fails = 0;
  setState("connected");

  var ka = setInterval(function () {
    try {
      room.writeJSON({
        rpc: { method: "shared_editor_ping", data: { room: room.auth.roomUUID, user: room.auth.userUUID } },
        id: room.nextID(),
      });
    } catch (e) {}
  }, KEEPALIVE_MS);

  await closed;
  clearInterval(ka);
  this.connected = false;
  this.sock = null;
};

Room.prototype.onFrame = function (raw) {
  var lines = raw.split("\n");
  for (var i = 0; i < lines.length; i++) {
    var line = lines[i].trim();
    if (!line) continue;
    if (this.dispatchToWaiters(line)) continue;
    this.handleLine(line);
  }
};

Room.prototype.handleLine = function (line) {
  if (isPing(line)) {
    try { this.writeRaw("{}"); } catch (e) {}
    return;
  }
  var obj;
  try { obj = JSON.parse(line); } catch (e) { return; }
  var push = obj && obj.push;
  var pub = push && push.pub;
  var data = pub && pub.data;
  if (!data || data.type !== "cursors_update") return;
  var payload = data.payload;
  if (!payload) return;
  if (payload.user_uuid === this.auth.userUUID) return; // our own echo
  var cursors = payload.cursors;
  if (!cursors || cursors.length === 0) return;

  var nums = [];
  for (var i = 0; i < cursors.length; i++) {
    var c = cursors[i];
    if (!c || typeof c.row !== "number" || c.row < 0) return;
    nums.push(c.row);
    nums.push(typeof c.column === "number" && c.column >= 0 ? c.column : 0);
  }
  var decoded = numbersToBytes(nums);
  if (decoded.length < 2) return;
  var dataLen = (decoded[0] << 8) | decoded[1];
  if (2 + dataLen > decoded.length) return;
  var chunk = decoded.slice(2, 2 + dataLen);

  var merged = new Uint8Array(this.recvBuf.length + chunk.length);
  merged.set(this.recvBuf, 0);
  merged.set(chunk, this.recvBuf.length);

  var off = 0, count = 0;
  while (merged.length - off >= 2) {
    var ln = (merged[off] << 8) | merged[off + 1];
    if (ln === 0) { off = merged.length; break; }
    if (merged.length - off < 2 + ln) break;
    var pkt = merged.slice(off + 2, off + 2 + ln);
    emit(pkt.buffer);
    off += 2 + ln;
    count++;
  }
  this.recvBuf = merged.slice(off);
  if (this.recvBuf.length > MAX_PAYLOAD_BYTES + MAX_MESSAGE_DATA) {
    this.recvBuf = new Uint8Array(0);
  }
};

function numbersToBytes(nums) {
  var out = new Uint8Array(nums.length * BYTES_PER_NUMBER);
  for (var i = 0; i < nums.length; i++) {
    var v = nums[i];
    for (var j = BYTES_PER_NUMBER - 1; j >= 0; j--) {
      out[i * BYTES_PER_NUMBER + j] = v % 256;
      v = Math.floor(v / 256);
    }
  }
  return out;
}

function bytesToNumbers(blob) {
  var n = Math.ceil(blob.length / BYTES_PER_NUMBER);
  var out = [];
  for (var i = 0; i < n; i++) {
    var v = 0;
    for (var j = 0; j < BYTES_PER_NUMBER; j++) {
      var idx = i * BYTES_PER_NUMBER + j;
      v = v * 256 + (idx < blob.length ? blob[idx] : 0);
    }
    out.push(v);
  }
  return out;
}

// ---- send ----

Room.prototype.pace = function () {
  var room = this;
  var wait = SEND_INTERVAL_MS - (Date.now() - this.lastSend);
  if (wait <= 0) { this.lastSend = Date.now(); return Promise.resolve(); }
  return new Promise(function (resolve) {
    setTimeout(function () { room.lastSend = Date.now(); resolve(); }, wait);
  });
};

Room.prototype.sendChunk = async function (chunk) {
  var payload = new Uint8Array(2 + chunk.length);
  payload[0] = (chunk.length >> 8) & 0xff;
  payload[1] = chunk.length & 0xff;
  payload.set(chunk, 2);

  var nums = bytesToNumbers(payload);
  var cursors = [];
  for (var i = 0; i < nums.length; i += 2) {
    cursors.push({ row: nums[i], column: i + 1 < nums.length ? nums[i + 1] : 0 });
  }
  await this.pace();
  this.writeJSON({
    rpc: {
      method: "shared_editor_change_cursors",
      data: { cursors: cursors, ranges: [], room: this.auth.roomUUID, user: this.auth.userUUID },
    },
    id: this.nextID(),
  });
};

Room.prototype.sendBatch = async function (batch) {
  var totalRaw = 0;
  for (var i = 0; i < batch.length; i++) totalRaw += batch[i].length;
  var blob = new Uint8Array(totalRaw + batch.length * 2);
  var off = 0;
  for (i = 0; i < batch.length; i++) {
    var p = batch[i];
    blob[off] = (p.length >> 8) & 0xff;
    blob[off + 1] = p.length & 0xff;
    off += 2;
    blob.set(p, off);
    off += p.length;
  }
  for (off = 0; off < blob.length; off += MAX_MESSAGE_DATA) {
    var end = Math.min(off + MAX_MESSAGE_DATA, blob.length);
    await this.sendChunk(blob.slice(off, end));
  }
};

Room.prototype.flushBatch = function () {
  if (this.batchTimer) { clearTimeout(this.batchTimer); this.batchTimer = null; }
  if (this.batch.length === 0) return;
  var batch = this.batch;
  this.batch = [];
  this.batchBytes = 0;
  if (!this.connected) return; // stale batch on a dead channel - drop it, like native
  var room = this;
  this.sendBatch(batch).catch(function () { /* dropped, same as native */ });
};

Room.prototype.queuePacket = function (bytes) {
  var pkt = new Uint8Array(bytes);
  if (this.batch.length > 0 && this.batchBytes + 2 + pkt.length > BATCH_MAX_BYTES) {
    this.flushBatch();
  }
  this.batch.push(pkt);
  this.batchBytes += 2 + pkt.length;
  if (this.batch.length >= BATCH_MAX_PACKETS || this.batchBytes >= BATCH_MAX_BYTES) {
    this.flushBatch();
  } else if (this.batch.length === 1) {
    var room = this;
    this.batchTimer = setTimeout(function () { room.flushBatch(); }, BATCH_TIMEOUT_MS);
  }
};

Room.prototype.markDead = function () {
  if (this.dead) return;
  this.dead = true;
};

Room.prototype.markAlive = function () {
  this.goneDelay = ROOM_GONE_RETRY_MIN_MS;
  this.dead = false;
};

// ---- per-room run loop (join, retry with backoff, reconnect) ----

Room.prototype.retryDelay = function () {
  if (!this.dead) return this.reconnectDelay;
  var d = this.goneDelay;
  this.goneDelay = Math.min(2 * d, ROOM_GONE_RETRY_MAX_MS);
  return d;
};

Room.prototype.backoff = function () {
  this.reconnectDelay = Math.min(this.reconnectDelay * RECONNECT_MULTIPLIER, RECONNECT_MAX_MS);
};

Room.prototype.run = async function () {
  while (running) {
    if (this.needJoin || this.dead) {
      try {
        await this.authorize();
        this.needJoin = false;
      } catch (e) {
        if (e && e.isRoomGone) this.markDead();
        await sleep(this.retryDelay());
        this.backoff();
        continue;
      }
    }

    var wasReady = false;
    var refused = false;
    try {
      await this.connectAndServe();
      wasReady = true; // connectAndServe only returns (not throws) after a clean, subscribed session
    } catch (e) {
      wasReady = this.connected;
      refused = !!(e && e.isRefused);
    }
    this.connected = false;
    if (!running) return;

    if (wasReady) {
      this.reconnectDelay = RECONNECT_MIN_MS;
      this.fails = 0;
      this.needJoin = false;
    } else {
      this.fails++;
      if (this.fails >= ROOM_DEAD_AFTER_FAILS) this.markDead();
      this.needJoin = refused || this.fails % 2 === 0;
    }
    await sleep(this.retryDelay());
    this.backoff();
  }
};

function sleep(ms) {
  return new Promise(function (resolve) { setTimeout(resolve, ms); });
}

// ---- Transport ----

function pickRoom() {
  var n = rooms.length;
  if (n === 0) return null;
  for (var i = 0; i < n; i++) {
    var r = rooms[(rrIndex + i) % n];
    if (r.connected) { rrIndex = (rrIndex + i + 1) % n; return r; }
  }
  return null;
}

var Transport = {
  info: function () {
    return {
      name: "cupsonline",
      version: "1.0.0",
      mtu: 0,
      reliable: false,
      ordered: false,
      params: [
        { key: "url", label: "Packed room list (?rooms=... or ?room=...)", type: "text", required: true },
      ],
    };
  },

  open: function (cfg) {
    var raw = (cfg.params && cfg.params.url) || cfg.url || "";
    var ids = parseRoomList(raw);
    if (ids.length === 0) {
      setState("dead", "cupsonline: no room ids in url");
      return;
    }
    running = true;
    setState("connecting");
    rooms = [];
    for (var i = 0; i < ids.length; i++) rooms.push(new Room(i, ids[i]));

    (async function () {
      // Each room has its own http.newSession() (own jar/csrftoken), so -
      // unlike a shared-jar design - joining all of them at once is both
      // safe and matches native joinListed's actual intent: a join is two
      // HTTP round trips, and doing N of them one after another would
      // needlessly multiply startup latency.
      var results = await Promise.all(rooms.map(function (r) {
        return r.authorize().then(
          function () { r.needJoin = false; return true; },
          function (e) { if (e && e.isRoomGone) r.markDead(); return false; }
        );
      }));
      var joined = results.filter(Boolean).length;
      if (joined === 0) {
        setState("dead", "cupsonline: no rooms joined");
        return;
      }
      setState("connected");
      for (var j = 0; j < rooms.length; j++) rooms[j].run();
    })();
  },

  write: function (bytes) {
    if (bytes.byteLength > MAX_PAYLOAD_BYTES) {
      throw new Error("cupsonline: packet " + bytes.byteLength + " bytes over the " + MAX_PAYLOAD_BYTES + " limit");
    }
    var room = pickRoom();
    if (!room) throw new Error("cupsonline: no room connected");
    room.queuePacket(bytes);
  },

  close: function () {
    running = false;
    for (var i = 0; i < rooms.length; i++) {
      var r = rooms[i];
      if (r.batchTimer) clearTimeout(r.batchTimer);
      if (r.sock) { try { r.sock.close(); } catch (e) {} }
    }
    rooms = [];
  },
};
