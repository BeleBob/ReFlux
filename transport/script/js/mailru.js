// Script-transport port of transport/mailru/mailru.go. Line-for-line the
// same protocol: tunnels packets through the "cursor" field of Mail.ru's
// public-document coauthoring WebSocket. Every constant, timing, and
// message shape below matches the native transport exactly - this file is
// a faithfulness test for the host API, not a rewrite of the protocol.
//
// The Go<->JS packet boundary (write()/emit()) carries raw bytes as
// ArrayBuffers; mailru's own wire format needs those bytes as base64 text
// spliced into the cursor field, so this script calls the host's
// base64.encode/decode explicitly at that one seam - fast (native Go
// codec, no hand-rolled JS base64) and the only place bytes<->text
// conversion happens at all.

var USER_AGENT =
  "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/137.0.0.0 Safari/537.36";
var CURSOR_RE = /"cursor":"[^;]+;([^"]+)"/;
var KEEPALIVE_MS = 10000; // transport.DefaultConfig().KeepAliveInterval
var MAX_RECONNECT_ATTEMPTS = 999999; // transport.DefaultConfig().MaxReconnectAttempts

var weblink = "";
var running = false;
var sock = null;
var baseUserID = "";
var userID = null;
var userCounter = 0;
var connectedAt = null;
var keepAliveTimer = null;

function pad(n, width) {
  var s = String(n);
  while (s.length < width) s = "0" + s;
  return s;
}

function randUserID() {
  return pad(Math.floor(Math.random() * 1000000000), 10);
}

// normalizeWeblink accepts either a bare weblink ("AbCdEfGh1/IjKlMnOp2") or
// a full public URL, same as NewMailruDocsTransport.
function normalizeWeblink(link) {
  link = String(link).trim();
  var prefixes = [
    "https://cloud.mail.ru/public/",
    "http://cloud.mail.ru/public/",
    "https://cloud.mail.ru/",
    "http://cloud.mail.ru/",
  ];
  for (var i = 0; i < prefixes.length; i++) {
    if (link.indexOf(prefixes[i]) === 0) {
      return link.slice(prefixes[i].length).replace(/^\/+|\/+$/g, "");
    }
  }
  return link;
}

// reconnectBackoff: exponential with jitter, capped at 15s - identical
// formula to reconnectBackoff() in mailru.go.
function reconnectBackoff(n) {
  if (n < 1) n = 1;
  var shift = n - 1;
  if (shift > 5) shift = 5;
  var d = 500 * Math.pow(2, shift);
  if (d > 15000) d = 15000;
  d += Math.floor(Math.random() * (d / 2 + 1));
  return d;
}

function scheduleReconnect(attempt) {
  var next = attempt + 1;
  if (!running || next >= MAX_RECONNECT_ATTEMPTS) return;
  var d = reconnectBackoff(next);
  setTimeout(function () {
    if (!running) return;
    connectToDoc(next);
  }, d);
}

// fetchDocInfo POSTs to Mail.ru's public-document editor API and returns
// the fields needed to open the collaborative WebSocket - same endpoint,
// headers and field extraction as fetchDocInfo() in mailru.go.
async function fetchDocInfo(link) {
  var body = JSON.stringify({
    "x-email": "anonym",
    public: "/" + link,
    platform: "desktop_web",
  });
  var res = await http.fetch({
    url: "https://cloud.mail.ru/api/v4/r7/edit",
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      Accept: "application/json, text/plain, */*",
      "User-Agent": USER_AGENT,
      "X-Api-Version": "4",
      Referer: "https://cloud.mail.ru/public/" + link + "?weblink=" + link,
    },
    body: body,
  });
  if (res.status !== 200) {
    throw new Error("mailru: api returned status " + res.status);
  }
  var data = JSON.parse(res.body);
  var doc = data.document;
  if (!doc) throw new Error("mailru: document object missing");
  var editorConfig = data.editorConfig;
  if (!editorConfig) throw new Error("mailru: editorConfig object missing");

  var apiBase = data.api || "";
  var wsBase = apiBase.replace("https://", "wss://");
  return {
    token: data.token || "",
    docKey: doc.key,
    wsURL: wsBase + "/doc/" + doc.key + "/c/?EIO=4&transport=websocket",
    fileType: doc.fileType,
    docUrl: doc.url,
    docTitle: doc.title,
    // document.permissions is an object of booleans, not a number -
    // sending it as anything else makes the editor server reject the auth
    // message with "access deny" (same caveat as the native transport).
    permissions: doc.permissions || {},
    callbackUrl: editorConfig.callbackUrl || "",
    editorUserId: (editorConfig.user && editorConfig.user.id) || "",
  };
}

function startKeepAlive() {
  if (keepAliveTimer) clearInterval(keepAliveTimer);
  keepAliveTimer = setInterval(function () {
    if (!sock) return;
    try {
      sock.send('42["message",{"type":"cursor","cursor":"18;---KA---"}]');
    } catch (e) {
      setState("degraded", String(e));
    }
  }, KEEPALIVE_MS);
}

function handleMessage(text) {
  if (text.indexOf("---KA---") !== -1) return;

  // Socket.IO ping/pong.
  if (text === "2") {
    if (sock) {
      try {
        sock.send("3");
      } catch (e) {}
    }
    return;
  }
  if (text === "3") return;

  if (text.indexOf('"type":"auth"') !== -1 && text.indexOf('"result":1') !== -1) {
    return; // auth ack - fire-and-forget, same as the native transport
  }

  if (text.indexOf("cursor") !== -1) {
    var m = CURSOR_RE.exec(text);
    if (!m || !m[1]) return;
    emit(base64.decode(m[1])); // cursor field is base64 text -> bytes
  }
}

function onSocketClose(attempt) {
  sock = null;
  var next = attempt;
  // A connection that had been up for >15s gets the fast (attempt=1)
  // backoff on its next try instead of continuing to climb - identical to
  // the `next = -1` branch in mailru.go's read-error handler.
  if (connectedAt !== null && Date.now() - connectedAt > 15000) {
    next = -1;
  }
  if (running) setState("reconnecting");
  scheduleReconnect(next);
}

function connectToDoc(attempt) {
  if (!running) return;

  (async function () {
    try {
      var info = await fetchDocInfo(weblink);
      var newSock = await ws.open(info.wsURL, {
        "User-Agent": USER_AGENT,
        Origin: "https://docs.datacloudmail.ru",
      });

      sock = newSock;
      if (userID === null) {
        userID = baseUserID + pad(userCounter++ % 1000, 3);
      }

      sock.onmessage = handleMessage;
      sock.onclose = function () {
        onSocketClose(attempt);
      };

      // Auth fires immediately, same as the native transport: Mail.ru's
      // coauthoring server buffers these until its own session state
      // catches up, and waiting for an explicit ack here only stretches
      // the outage window on every reconnect.
      sock.send('40{"token":"' + info.token + '"}');

      var authMsg = {
        type: "auth",
        docid: info.docKey,
        documentCallbackUrl: info.callbackUrl,
        token: "fghhfgsjdgfjs",
        user: { id: info.editorUserId, username: userID, indexUser: -1 },
        editorType: 0,
        lastOtherSaveTime: -1,
        block: [],
        documentFormatSave: 65,
        view: false,
        isCloseCoAuthoring: false,
        openCmd: {
          c: "open",
          id: info.docKey,
          userid: info.editorUserId,
          format: info.fileType,
          url: info.docUrl,
          title: info.docTitle,
          lcid: 25,
          nobase64: true,
          convertToOrigin: ".pdf.xps.oxps.djvu",
        },
        lang: "ru",
        mode: "edit",
        permissions: info.permissions,
        IsAnonymousUser: false,
        timezoneOffset: -180,
        coEditingMode: "fast",
        jwtOpen: info.token,
        time: 1000,
        supportAuthChangesAck: true,
      };
      sock.send('42' + JSON.stringify(["message", authMsg]));

      connectedAt = Date.now();
      setState("connected");
    } catch (e) {
      setState("reconnecting", String(e));
      scheduleReconnect(attempt);
    }
  })();
}

var Transport = {
  info: function () {
    return {
      name: "mailru",
      version: "1.0.0",
      cookieDomain: "https://cloud.mail.ru/",
      mtu: 0, // unbounded - native mailru never fragments either
      reliable: false,
      ordered: false,
      halfDuplex: false,
      minIntervalMs: 0,
      params: [
        {
          key: "url",
          label: "Mail.ru public document link or weblink",
          type: "url",
          required: true,
        },
      ],
    };
  },

  open: function (cfg) {
    var raw = (cfg.params && cfg.params.url) || cfg.url || "";
    weblink = normalizeWeblink(raw);
    baseUserID = randUserID();
    running = true;
    startKeepAlive();
    connectToDoc(0);
  },

  write: function (bytes) {
    if (!sock) throw new Error("mailru: not connected");
    sock.send('42["message",{"type":"cursor","cursor":"18;' + base64.encode(bytes) + '"}]');
  },

  close: function () {
    running = false;
    if (keepAliveTimer) clearInterval(keepAliveTimer);
    if (sock) {
      try {
        sock.close();
      } catch (e) {}
    }
    sock = null;
  },

  // Generic cookie exchange (see host.go / transport.go FetchCookies /
  // ApplyCookies): Go reads cookies straight out of the shared jar with no
  // help from this script, but only the script can make an already-open
  // socket pick up freshly applied cookies - so it reacts to this event by
  // forcing a reconnect, same as ApplyCookies forcing t.session = nil in
  // the native transport.
  onEvent: function (kind) {
    if (kind === "cookiesApplied") {
      userID = null;
      if (sock) {
        try {
          sock.close();
        } catch (e) {}
      }
      sock = null;
      setState("reconnecting");
      scheduleReconnect(0);
    }
  },
};
