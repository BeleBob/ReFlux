// Package mailru implements a transport that tunnels packets through
// Mail.ru's cloud document editor (docs.datacloudmail.ru), the same
// coauthoring backend family as Yandex.Docs. Two peers open the same
// public document and smuggle packets through the "cursor" field of the
// collaborative editing protocol.
package mailru

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/cookiejar"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/p1neappleXpress/OpenFlux/netbind"
	"github.com/p1neappleXpress/OpenFlux/transport"
	"github.com/p1neappleXpress/OpenFlux/utils"
)

// Mail.ru rejects the old Chrome/137 UA on the WebSocket upgrade with HTTP 409
// on some edge nodes; a current Firefox UA is accepted for both the API and the
// WebSocket. Reported by @Shiller70 (issue #164).
const mailruUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:155.0) Gecko/20100101 Firefox/155.0"

var cursorPayloadRe = regexp.MustCompile(`"cursor":"[^;]+;([^"]+)"`)

// mailruMaxFrame is the largest frame a cursor message carries: since
// 2026-10-12 the document server answers a cursor of about 3 KB and more
// with "access deny" (3060 bytes passed, 3072 did not), so batches stay at
// 3060 (transport.FrameLimiter). Two 1400-byte packets, the app's default
// MTU, fit in one with their Session envelopes; at 3000 only one did, and
// with the message allowance below every frame counts.
const mailruMaxFrame = 3060

// The document server allows a connection about five messages a second
// beyond a burst of about fifty and drops it with "access deny" past that
// (since 2026-10-14: 20 a second were denied after 70 messages, 8 after
// 133, 6 after 293; 5 a second were not in two minutes). It counts
// messages, not bytes, and each connection on its own. A connection keeps
// under it with room to spare; variables for tests.
var (
	sendRate  = 4.0
	sendBurst = 32.0
)

// budget is a connection's allowance of messages (sendRate, sendBurst): a
// token bucket that starts full, as the server's does for a new
// connection. Without a rate it allows everything.
type budget struct {
	rate, burst float64
	mu          sync.Mutex
	tokens      float64
	last        time.Time
}

func (b *budget) refillLocked(now time.Time) {
	if b.last.IsZero() {
		b.tokens = b.burst
	} else {
		b.tokens = min(b.burst, b.tokens+now.Sub(b.last).Seconds()*b.rate)
	}
	b.last = now
}

// take spends a message. A control message may overdraw the allowance; data
// then waits for longer.
func (b *budget) take() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked(time.Now())
	b.tokens--
}

// untilToken is how long until a message may go out (0: now).
func (b *budget) untilToken() time.Duration {
	if b.rate <= 0 {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refillLocked(time.Now())
	if b.tokens >= 1 {
		return 0
	}
	return time.Duration((1 - b.tokens) / b.rate * float64(time.Second))
}

// socketIOEvent starts every message the allowance counts: Socket.IO
// events, not Engine.IO pings and pongs.
var socketIOEvent = []byte("42")

// The document server also takes at most 1 MiB of cursors from a
// connection, whatever the rate, and then drops it with "access deny"
// (since 2026-10-14: after 257 cursors of 4085 characters at 2, 3 or 4 a
// second, after 523 of 2005). A new connection starts afresh. So a
// connection that has sent rotateAfter opens its successor, which takes
// over once the server has let it in; the old one then leaves. Variables
// for tests.
var (
	rotateAfter int64 = 640 << 10
	// rotateRetry is how long a connection waits to try again after its
	// successor failed to come up.
	rotateRetry = 5 * time.Second
	// retireAfter is how long a replaced connection stays: what it has
	// written is the server's already.
	retireAfter = time.Second
	// successorTimeout is how long a successor may wait to be let in.
	successorTimeout = 20 * time.Second
)

// keepAliveCursor is the keep-alive a quiet connection writes: a cursor as
// the editor sends it (its real length and payload), since the server now
// drops a connection on a malformed one, as it did on the old "18;---KA---".
// The receiving side skips its payload.
const (
	keepAlivePayload = "BgAAADYAMgA3AAMAAAA="
	keepAliveCursor  = "14;" + keepAlivePayload
)

// extendSessionMsg tells the document server the editor is in use, as the
// editor does while its user is active (sdkjs extendSession). The server
// closes a session it believes idle for an hour (disconnectReason 4002
// "idle session expires"), whatever the traffic: tunnel cursors do not
// count, so every connection dropped once an hour and the tunnel paused
// until it rejoined.
const extendSessionMsg = `42["message",{"type":"extendSession","idletime":0}]`

// extendSessionEvery is how often a connection renews its session, well
// inside the hour; a variable for tests.
var extendSessionEvery = 10 * time.Minute

// saveChangesMessageTemplate — шаблон saveChanges-сообщения в формате
// веб-клиента Mail.ru: две подстановки %s — UserId и UserShortId в
// excelAdditionalInfo. Остальные поля (op-ы в changes, анкоры, версия)
// оставлены ровно такими, какими их шлёт живой редактор; это шаблон, а не
// «правильная» структура — формат оп закрытый, и мы только подставляем свои
// значения участника.
//
// Поля isCoAuthoring/releaseLocks = false — особенность именно mail.ru
// (у yandex.docs там true/true).
//
// Since 2026-10-12 the server answers this template with "access deny" and
// drops the connection, so the transport no longer sends it; it stays for
// the signed JS port, which is compared with BuildSaveChanges.
const saveChangesMessageTemplate = `42["message",{"type":"saveChanges","changes":"[\"76;AgAAADEA//8BAOwbfF7pEAAALQEAAAMAAAAAAAAAAAAAAAAAAAAAAAAA9v///xoAAAAyADAAMgA2AC4AMgAuADEALgAyADIANgA4AA==\",\"35;BgAAADYAMgA3AAEAHAABAAAAAAAAAAEAAABhAAAAAAMAAAA=\",\"35;BgAAADYAMgA3AAEAHAABAAAAAQAAAAEAAABzAAAAAAMAAAA=\",\"35;BgAAADYAMgA3AAEAHAABAAAAAgAAAAEAAABkAAAAAAMAAAA=\"]","startSaveChanges":true,"endSaveChanges":true,"isCoAuthoring":false,"isExcel":false,"deleteIndex":null,"excelAdditionalInfo":"{\"UserId\":\"%s\",\"UserShortId\":\"%s\",\"CursorInfo\":\"14;BgAAADYAMgA3AAMAAAA=\"}","unlock":false,"releaseLocks":false}]`

type MailruDocsInfo struct {
	Token        string
	DocKey       string
	WsURL        string
	FileType     string
	DocURL       string
	DocTitle     string
	Permissions  map[string]interface{}
	CallbackURL  string
	EditorUserID string
}

type DocSession struct {
	Info       MailruDocsInfo
	Conn       *websocket.Conn
	WriteQueue chan []byte
	UserID     string
	writeMu    sync.Mutex
	lastWrite  atomic.Int64 // unix nanoseconds of the last successful write
	// budget is what the server still lets this connection send.
	budget budget
	// sent counts the bytes of the Socket.IO events written (rotateAfter).
	sent atomic.Int64
	// rotating is set once this connection has started opening its
	// successor; retired once the successor took over.
	rotating, retired atomic.Bool
	// successor is set on a connection opened to replace another, until
	// the server lets it in and it takes over (takeOver).
	successor atomic.Pointer[DocSession]
	// silence is how long the server may say nothing before the
	// connection counts as dead (engineIOSilence).
	silence time.Duration
}

func (s *DocSession) safeWrite(messageType int, data []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	// A write into a half-open connection (NAT dropped it, the network
	// changed) would otherwise block until the kernel gives up, minutes.
	_ = s.Conn.SetWriteDeadline(time.Now().Add(docWriteTimeout))
	err := s.Conn.WriteMessage(messageType, data)
	if err == nil {
		s.lastWrite.Store(time.Now().UnixNano())
		if bytes.HasPrefix(data, socketIOEvent) {
			s.budget.take()
			s.sent.Add(int64(len(data)))
		}
	}
	return err
}

// writtenWithin reports whether something went out on the connection in
// the last d.
func (s *DocSession) writtenWithin(d time.Duration) bool {
	return time.Since(time.Unix(0, s.lastWrite.Load())) < d
}

// docWriteTimeout bounds one WebSocket write to the document.
const docWriteTimeout = 20 * time.Second

// editAPIURL opens a public document for editing; a variable for tests.
var editAPIURL = "https://cloud.mail.ru/api/v4/r7/edit"

// unlockDocument releases the document's auth lock (see handleMessage),
// with the fields the editor sends (sdkjs DocsCoApi.unLockDocument).
const unlockDocument = `42["message",{"type":"unLockDocument","isSave":false,"unlock":true,"deleteIndex":null,"releaseLocks":false}]`

const mailruSocketIOHandshakeTimeout = 15 * time.Second

// waitMailruSocketIO completes the Engine.IO / Socket.IO handshake before
// editor authentication is sent. Mail.ru currently requires the Engine.IO
// open frame to be consumed before the Socket.IO connect packet is sent.
func waitMailruSocketIO(session *DocSession, token string) error {
	if session == nil || session.Conn == nil {
		return fmt.Errorf("mailru: websocket session is nil")
	}

	conn := session.Conn

	if err := conn.SetReadDeadline(time.Now().Add(mailruSocketIOHandshakeTimeout)); err != nil {
		return fmt.Errorf("mailru: set handshake deadline: %w", err)
	}
	defer conn.SetReadDeadline(time.Time{})

	waitFor := func(match func(string) bool) error {
		for {
			messageType, payload, err := conn.ReadMessage()
			if err != nil {
				return err
			}

			if messageType != websocket.TextMessage {
				continue
			}

			text := string(payload)

			// Engine.IO heartbeat may arrive while handshaking.
			if text == "2" {
				if err := session.safeWrite(websocket.TextMessage, []byte("3")); err != nil {
					return fmt.Errorf("mailru: send Engine.IO pong: %w", err)
				}
				continue
			}

			if match(text) {
				return nil
			}

			utils.Debugf("[M-DOCS] handshake: ignoring server frame %q", text)
		}
	}

	// Engine.IO open:
	//   0{"sid":"...", ...}
	if err := waitFor(func(text string) bool {
		if !strings.HasPrefix(text, "0{") {
			return false
		}
		session.silence = engineIOSilence(text[1:])
		return true
	}); err != nil {
		return fmt.Errorf("mailru: wait for Engine.IO open: %w", err)
	}

	utils.Debugf("[M-DOCS] Engine.IO open")

	socketConnect := fmt.Sprintf(`40{"token":"%s"}`, token)

	if err := session.safeWrite(websocket.TextMessage, []byte(socketConnect)); err != nil {
		return fmt.Errorf("mailru: send Socket.IO connect: %w", err)
	}

	// Socket.IO acknowledgement:
	//   40{"sid":"..."}
	if err := waitFor(func(text string) bool {
		return strings.HasPrefix(text, "40")
	}); err != nil {
		return fmt.Errorf("mailru: wait for Socket.IO connect: %w", err)
	}

	utils.Debugf("[M-DOCS] Socket.IO connected")

	return nil
}

type MailruDocsTransport struct {
	*transport.BaseTransport

	weblink string
	session *DocSession

	userCounter atomic.Int32
	baseUserID  string

	// queued counts the frames Send took that are not written yet.
	queued atomic.Int64
	// next is a successor not let in yet (rotate), for Stop.
	next atomic.Pointer[DocSession]

	// limits are the package's variables when the transport was made, so
	// a test may change them without racing a transport still running;
	// apiURL is editAPIURL the same way.
	limits limits
	apiURL string

	cookieJar *cookiejar.Jar
	jarMu     sync.RWMutex
	// csrf is the signed-in account's CSRF token for the editor API, ""
	// until taken (login.go); fileCookies are the account's cookies from
	// cookieFile (LoadCookieFile) as it was when cookieStamp was taken. All
	// under jarMu.
	csrf                    string
	fileCookies             map[string]string
	cookieFile, cookieStamp string

	// errNotifier asks for a login (login.go), at most once a minute
	// (loginAsked). Under errMu.
	errMu       sync.Mutex
	errNotifier func(err error, transportName, url, html, reason string)
	loginAsked  time.Time

	// reconnecting is set while a scheduled reconnect waits out its backoff:
	// the reader's error and ApplyCookies both schedule one when the cookies
	// are replaced under a live connection, and two reconnects open two
	// sessions to the document.
	reconnecting atomic.Bool
}

// NewMailruDocsTransport accepts either a bare weblink ("AbCdEfGh1/IjKlMnOp2")
// or a full public URL ("https://cloud.mail.ru/public/AbCdEfGh1/IjKlMnOp2"),
// normalizing the latter to the former.
func NewMailruDocsTransport(weblink string, config transport.TransportConfig) *MailruDocsTransport {
	t := &MailruDocsTransport{
		BaseTransport: transport.NewBaseTransport(config),
		weblink:       normalizeWeblink(weblink),
		limits:        currentLimits(),
		apiURL:        editAPIURL,
	}
	t.baseUserID = randUserID()
	jar, _ := cookiejar.New(nil)
	t.cookieJar = jar
	return t
}

func normalizeWeblink(weblink string) string {
	weblink = strings.TrimSpace(weblink)
	for _, prefix := range []string{
		"https://cloud.mail.ru/public/",
		"http://cloud.mail.ru/public/",
		"https://cloud.mail.ru/",
		"http://cloud.mail.ru/",
	} {
		if strings.HasPrefix(weblink, prefix) {
			return strings.Trim(strings.TrimPrefix(weblink, prefix), "/")
		}
	}
	return weblink
}

func (t *MailruDocsTransport) Start() error {
	if err := t.BaseTransport.Start(); err != nil {
		return err
	}

	t.baseUserID = randUserID()
	utils.SafeGo("mailru.keepAlive", t.keepAliveLoop)
	every := extendSessionEvery
	utils.SafeGo("mailru.extendSession", func() { t.extendSessionLoop(every) })
	t.connectToDoc(0)

	return nil
}

// Stop leaves the document before closing the connection. A participant
// that just disappears (the process exits, the socket closes without a
// word) keeps its place on Mail.ru's co-authoring server for minutes, and
// during that time the server closes every new connection to the document
// right after the WebSocket handshake: a restarted exit could not rejoin,
// so its clients stayed cut off for 3.5 minutes to over 10.
func (t *MailruDocsTransport) Stop() error {
	err := t.BaseTransport.Stop()
	t.Mu.Lock()
	session := t.session
	t.Mu.Unlock()
	if session != nil && session.Conn != nil {
		session.leave()
	}
	if next := t.next.Load(); next != nil && next.successor.Swap(nil) != nil {
		next.retired.Store(true)
		next.leave()
	}
	return err
}

// leave says goodbye the way the editor does when its tab closes: the
// co-authoring "close" message (the server drops the participant at once;
// a dropped socket alone leaves it listed for minutes, in case it comes
// back), a Socket.IO disconnect, then a normal WebSocket close.
func (s *DocSession) leave() {
	_ = s.safeWrite(websocket.TextMessage, []byte(`42["message",{"type":"close"}]`))
	_ = s.safeWrite(websocket.TextMessage, []byte("41"))
	s.writeMu.Lock()
	_ = s.Conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(2*time.Second))
	s.writeMu.Unlock()
	_ = s.Conn.Close()
}

func (t *MailruDocsTransport) Send(data []byte) error {
	if !t.IsConnected() {
		return fmt.Errorf("transport not connected")
	}

	t.Mu.RLock()
	session := t.session
	t.Mu.RUnlock()

	if session == nil {
		return fmt.Errorf("no active session")
	}

	select {
	case session.WriteQueue <- data:
		t.queued.Add(1)
		t.RecordSend(len(data))
		return nil
	default:
		return fmt.Errorf("write queue full")
	}
}

func (t *MailruDocsTransport) connectToDoc(attempt int) { t.connect(attempt, nil) }

// rotate opens old's successor (rotateAfter).
func (t *MailruDocsTransport) rotate(old *DocSession) {
	utils.Debugf("[M-DOCS] %d bytes sent on this connection: opening the next one", old.sent.Load())
	t.connect(0, old)
}

// rotationFailed lets old try again after rotateRetry.
func (t *MailruDocsTransport) rotationFailed(old *DocSession, err error) {
	utils.Debugf("[M-DOCS] the next connection did not come up: %v", err)
	time.AfterFunc(t.limits.rotateRetry, func() { old.rotating.Store(false) })
}

// takeOver makes next, which the server has just let in, the connection
// in use in place of old, which leaves. Nothing changes if old is no
// longer in use (it dropped and a reconnect replaced it): next leaves.
func (t *MailruDocsTransport) takeOver(old, next *DocSession) {
	t.next.CompareAndSwap(next, nil)
	t.Mu.Lock()
	current := t.session == old && t.IsRunning()
	if current {
		t.session = next
		t.SetConnected(true)
	}
	t.Mu.Unlock()
	if !current {
		next.retired.Store(true)
		next.leave()
		return
	}
	utils.Debugf("[M-DOCS] moved to the next connection after %d bytes", old.sent.Load())
	old.retired.Store(true)
	time.AfterFunc(t.limits.retireAfter, old.leave)
}

// limits are what the server allows a connection and how the transport
// keeps to it (sendRate, rotateAfter and the rest).
type limits struct {
	rate, burst                                float64
	rotateAfter                                int64
	rotateRetry, retireAfter, successorTimeout time.Duration
}

func currentLimits() limits {
	return limits{
		rate: sendRate, burst: sendBurst,
		rotateAfter: rotateAfter,
		rotateRetry: rotateRetry, retireAfter: retireAfter, successorTimeout: successorTimeout,
	}
}

// connect opens a connection to the document: the first one or one after
// a drop (replace nil), or replace's successor.
func (t *MailruDocsTransport) connect(attempt int, replace *DocSession) {
	if !t.IsRunning() {
		return
	}

	utils.Debugf("[M-DOCS] connectToDoc attempt %d", attempt)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				utils.Debugf("[PANIC] recovered in mailru.connect: %v", r)
			}
		}()
		t.Mu.Lock()
		existingSession := t.session
		t.Mu.Unlock()

		var userID string
		if existingSession != nil && replace == nil {
			userID = existingSession.UserID
		} else {
			suffix := fmt.Sprintf("%03d", t.userCounter.Add(1)%1000)
			userID = t.baseUserID + suffix
		}

		info, err := t.fetchDocInfo(t.weblink)
		if err != nil {
			utils.Debugf("[M-DOCS] fetchDocInfo failed: %v", err)
			if replace != nil {
				t.rotationFailed(replace, err)
				return
			}
			if errors.Is(err, ErrLoginRequired) {
				// A login that came in a new cookies file is tried at
				// once; otherwise one is asked for.
				changed, ferr := t.readCookieFile()
				if ferr != nil {
					utils.Debugf("[M-DOCS] %v", ferr)
				}
				if !changed {
					t.askLogin(err)
				}
			} else if utils.Throttled("m-docs.fetch", time.Minute) {
				utils.Infof("[M-DOCS] cannot open the document: %v; retrying", err)
			}
			t.scheduleReconnect(attempt)
			return
		}

		dialer := websocket.Dialer{
			// Mail.ru's nginx answers 409 Conflict to an upgrade that does
			// not offer permessage-deflate. gorilla/websocket only sends the
			// Sec-WebSocket-Extensions header when compression is enabled, so
			// without this the handshake was rejected before any auth and the
			// carrier never came up. Reported by @pirsasha (issue #163).
			EnableCompression: true,
			HandshakeTimeout:  15 * time.Second,
			NetDialContext: netbind.Wrap(&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		}
		headers := http.Header{}
		headers.Set("User-Agent", mailruUserAgent)
		// Some Mail.ru edge nodes still answer 409 to the upgrade when the
		// request looks less like a browser than the others; a real browser
		// always sends Accept-Language. Reported to still hit iOS (#164).
		headers.Set("Accept-Language", "en-US,en;q=0.9")
		headers.Set("Origin", "https://docs.datacloudmail.ru")

		utils.Debugf("[M-DOCS] WebSocket dial %s", info.WsURL)
		conn, resp, err := dialer.Dial(info.WsURL, headers)
		if err != nil {
			status := 0
			if resp != nil {
				status = resp.StatusCode
			}
			utils.Debugf("[M-DOCS] WebSocket dial failed (http %d): %v", status, err)
			if replace != nil {
				t.rotationFailed(replace, err)
				return
			}
			t.scheduleReconnect(attempt)
			return
		}
		utils.Debugf("[M-DOCS] WebSocket connected")

		writeQueue := make(chan []byte, t.GetConfig().MaxQueueSize)
		if existingSession != nil {
			writeQueue = existingSession.WriteQueue
		}

		session := &DocSession{
			Info:       info,
			Conn:       conn,
			WriteQueue: writeQueue,
			UserID:     userID,
			budget:     budget{rate: t.limits.rate, burst: t.limits.burst},
		}

		if err := waitMailruSocketIO(session, info.Token); err != nil {
			utils.Debugf("[M-DOCS] Socket.IO handshake failed: %v", err)
			_ = conn.Close()
			if replace != nil {
				t.rotationFailed(replace, err)
				return
			}
			t.scheduleReconnect(attempt)
			return
		}

		if replace != nil {
			// It takes over once the server lets it in (handleMessage); one
			// that is not let in gives up.
			session.successor.Store(replace)
			t.next.Store(session)
			time.AfterFunc(t.limits.successorTimeout, func() {
				if old := session.successor.Swap(nil); old != nil {
					t.next.CompareAndSwap(session, nil)
					session.retired.Store(true)
					session.leave()
					t.rotationFailed(old, errors.New("not let into the document"))
				}
			})
		} else {
			t.Mu.Lock()
			t.session = session
			t.SetConnected(true)
			t.Mu.Unlock()
		}

		if existingSession == nil {
			utils.SafeGo("mailru.writer", t.writerLoop)
		}

		authMsg := map[string]interface{}{
			"type":                "auth",
			"docid":               info.DocKey,
			"documentCallbackUrl": info.CallbackURL,
			"token":               "fghhfgsjdgfjs",
			"user": map[string]interface{}{
				"id":        info.EditorUserID,
				"username":  userID,
				"indexUser": -1,
			},
			"editorType":         0,
			"lastOtherSaveTime":  -1,
			"block":              []interface{}{},
			"documentFormatSave": 65,
			"view":               false,
			"isCloseCoAuthoring": false,
			"openCmd": map[string]interface{}{
				"c":               "open",
				"id":              info.DocKey,
				"userid":          info.EditorUserID,
				"format":          info.FileType,
				"url":             info.DocURL,
				"title":           info.DocTitle,
				"lcid":            25,
				"nobase64":        true,
				"convertToOrigin": ".pdf.xps.oxps.djvu",
			},
			"lang":                  "ru",
			"mode":                  "edit",
			"permissions":           info.Permissions,
			"IsAnonymousUser":       false,
			"timezoneOffset":        -180,
			"coEditingMode":         "fast",
			"jwtOpen":               info.Token,
			"time":                  1000,
			"supportAuthChangesAck": true,
		}
		messagePart, _ := json.Marshal([]interface{}{"message", authMsg})
		session.safeWrite(websocket.TextMessage, []byte(fmt.Sprintf("42%s", string(messagePart))))

		connectedAt := time.Now()
		for t.IsRunning() {
			if session.silence > 0 {
				_ = conn.SetReadDeadline(time.Now().Add(session.silence))
			}
			_, message, err := conn.ReadMessage()
			if err != nil {
				if session.retired.Load() {
					return // replaced, and closed on purpose
				}
				if old := session.successor.Swap(nil); old != nil {
					t.next.CompareAndSwap(session, nil)
					_ = conn.Close()
					t.rotationFailed(old, err)
					return
				}
				utils.Debugf("[M-DOCS] Read error: %v", err)
				if utils.Throttled("m-docs.drop", time.Minute) {
					utils.Infof("[M-DOCS] connection to the document dropped: %v; reconnecting", err)
				}
				t.SetConnected(false)
				conn.Close()

				next := attempt
				if time.Since(connectedAt) > 15*time.Second {
					next = -1
				}
				t.scheduleReconnect(next)
				return
			}
			t.handleMessage(session, message)
		}
	}()
}

func (t *MailruDocsTransport) writerLoop() {
	var queue chan []byte
	for t.IsRunning() && queue == nil {
		t.Mu.Lock()
		if t.session != nil {
			queue = t.session.WriteQueue
		}
		t.Mu.Unlock()
		if queue == nil {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if queue == nil {
		return
	}

	var pending []byte
	for t.IsRunning() {
		if pending == nil {
			packet, ok := <-queue
			if !ok {
				return
			}
			pending = packet
		}

		t.Mu.RLock()
		session := t.session
		t.Mu.RUnlock()
		if session == nil || session.Conn == nil {
			time.Sleep(15 * time.Millisecond)
			continue
		}
		if d := session.budget.untilToken(); d > 0 {
			time.Sleep(d)
			continue
		}

		payload := base64.StdEncoding.EncodeToString(pending)
		// The prefix is the packet's real length: since 2026-10-12 the
		// document server drops a connection ("access deny") whose cursor
		// declares another, as Yandex's has done for a while.
		msg := fmt.Sprintf(`42["message",{"type":"cursor","cursor":"%d;%s"}]`, len(pending), payload)
		if err := session.safeWrite(websocket.TextMessage, []byte(msg)); err != nil {
			utils.Debugf("[M-DOCS] Write error: %v", err)
			time.Sleep(15 * time.Millisecond)
			continue
		}
		pending = nil
		t.queued.Add(-1)
		if session.sent.Load() >= t.limits.rotateAfter && session.rotating.CompareAndSwap(false, true) {
			t.rotate(session)
		}
	}
}

// MaxFrame is transport.FrameLimiter: the batches above stay under the
// cursor size the document server accepts.
func (t *MailruDocsTransport) MaxFrame() int { return mailruMaxFrame }

// WaitSend is transport.Pacer: it returns once the connection is up, has
// no frame waiting and may send a message now, so the batch the caller
// closes next goes out at once and holds everything queued meanwhile.
func (t *MailruDocsTransport) WaitSend(stop <-chan struct{}) bool {
	for {
		wait := 20 * time.Millisecond
		t.Mu.RLock()
		session := t.session
		t.Mu.RUnlock()
		if session != nil && t.IsConnected() && t.queued.Load() == 0 {
			if wait = session.budget.untilToken(); wait <= 0 {
				return true
			}
		}
		timer := time.NewTimer(wait)
		select {
		case <-stop:
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
}

// QueueBytes is transport.Pacer: two seconds of full frames, one of them
// for a flow. Measured over a live document, one second held a download
// to 6 KB/s and three made new connections wait longer; two gave 9 KB/s.
func (t *MailruDocsTransport) QueueBytes() int { return int(2 * t.limits.rate * mailruMaxFrame) }

// keepAliveLoop writes a keep-alive only into a quiet connection: any
// write (tunnel data, a Session ping, a Socket.IO pong) does its job. Each
// keep-alive is relayed to every editor of the document, so one sent
// regardless of traffic woke both peers' radios every interval: on a
// phone the cellular modem then never went idle.
// extendSessionLoop renews the current connection's session every interval
// (see extendSessionMsg).
func (t *MailruDocsTransport) extendSessionLoop(every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for t.IsRunning() {
		select {
		case <-ticker.C:
		case <-t.Done():
			return
		}
		t.Mu.Lock()
		session := t.session
		t.Mu.Unlock()
		if session != nil && session.Conn != nil {
			if err := session.safeWrite(websocket.TextMessage, []byte(extendSessionMsg)); err != nil {
				utils.Debugf("[M-DOCS] extendSession write error: %v", err)
			}
		}
	}
}

func (t *MailruDocsTransport) keepAliveLoop() {
	interval := t.GetConfig().KeepAliveInterval
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	keepAliveMsg := `42["message",{"type":"cursor","cursor":"` + keepAliveCursor + `"}]`

	for t.IsRunning() {
		<-ticker.C
		t.Mu.Lock()
		session := t.session
		t.Mu.Unlock()

		if session != nil && session.Conn != nil && !session.writtenWithin(interval) {
			if err := session.safeWrite(websocket.TextMessage, []byte(keepAliveMsg)); err != nil {
				utils.Debugf("[M-DOCS] Keep-alive failed, closing the connection to reconnect: %v", err)
				t.SetConnected(false)
				_ = session.Conn.Close()
			}
		}
	}
}

// canEdit says whether the document may be edited through the link this session joined with.
// The server reports it in the document's permissions; a document that does not say is treated
// as editable, as the transport always did.
func canEdit(permissions map[string]interface{}) bool {
	edit, ok := permissions["edit"].(bool)
	return !ok || edit
}

// BuildSaveChanges is the saveChanges message for one participant: a pure
// function of the user id, exported so the JS port of this transport can be
// compared with it byte for byte.
func BuildSaveChanges(userID string) []byte {
	short := userID
	if len(short) > 1 {
		short = short[:len(short)-1]
	}
	return []byte(fmt.Sprintf(saveChangesMessageTemplate, userID, short))
}

func (t *MailruDocsTransport) handleMessage(session *DocSession, data []byte) {
	text := string(data)

	if text == "2" {
		if session != nil && session.Conn != nil {
			session.safeWrite(websocket.TextMessage, []byte("3"))
		}
		return
	}
	if text == "3" {
		return
	}

	// The server warns before it closes an idle session; the editor answers
	// by renewing it.
	if strings.Contains(text, `"type":"session"`) && strings.Contains(text, `"code":4002`) {
		utils.Debugf("[M-DOCS] the server says the session is about to expire: renewing it")
		if session != nil && session.Conn != nil {
			session.safeWrite(websocket.TextMessage, []byte(extendSessionMsg))
		}
		return
	}

	if strings.Contains(text, `"type":"auth"`) && strings.Contains(text, `"result":1`) {
		utils.Debugf("[M-DOCS] Auth OK for user %s", session.UserID)
		if old := session.successor.Swap(nil); old != nil {
			t.takeOver(old, session)
		}
		return
	}

	// When a second editor joins, the co-authoring server locks the
	// document in the name of the first one and tells it so (connectState
	// with waitAuth); the newcomer gets "waitAuth" and is let in only once
	// the first one releases the lock, as the editor does after switching
	// to co-editing. Nobody did: after 30 s the server dropped the lock
	// holder (disconnectReason 4007) to let the newcomer in, the dropped
	// side rejoined, and the peers kept knocking each other off the
	// document every ~30 s - a peer that rejoined could not stay.
	if strings.Contains(text, `"type":"connectState"`) && strings.Contains(text, `"waitAuth":true`) {
		utils.Debugf("[M-DOCS] a peer is waiting for the document lock: releasing it")
		if session != nil && session.Conn != nil {
			session.safeWrite(websocket.TextMessage, []byte(unlockDocument))
		}
		return
	}

	if strings.Contains(text, `"type":"disconnectReason"`) {
		utils.Infof("[M-DOCS] the document server dropped this connection: %s", text)
		return
	}

	if strings.Contains(text, "cursor") {
		for _, base64Str := range cursorPayloads(text) {
			if base64Str == keepAlivePayload {
				continue
			}
			decoded, err := base64.StdEncoding.DecodeString(base64Str)
			if err != nil {
				utils.Debugf("[M-DOCS] Base64 decode error: %v", err)
				continue
			}
			t.RecordReceive(len(decoded))
			t.CallReceive(decoded)
		}
		return
	}

	// Everything else the co-authoring server sends (participants, locks,
	// changes) is not needed for the tunnel, but it explains the server's
	// behaviour when something goes wrong.
	if len(text) > 300 {
		text = text[:300] + "..."
	}
	utils.Debugf("[M-DOCS] unhandled message: %s", text)
}

// cursorPayloads returns the base64 payload of every cursor entry in a server
// message, in order, without the keep-alive entries.
func cursorPayloads(text string) []string { return CursorPayloads(text) }

// CursorPayloads is the exported form of cursorPayloads (pure; the JS port is
// compared with it in tests).
func CursorPayloads(text string) []string {
	var out []string
	for _, m := range cursorPayloadRe.FindAllStringSubmatch(text, -1) {
		if len(m) > 1 && m[1] != "---KA---" {
			out = append(out, m[1])
		}
	}
	return out
}

func (t *MailruDocsTransport) scheduleReconnect(attempt int) {
	next := attempt + 1
	if !t.IsRunning() || next >= t.GetConfig().MaxReconnectAttempts {
		return
	}

	if !t.reconnecting.CompareAndSwap(false, true) {
		return // one is already waiting
	}
	d := reconnectBackoff(next)
	utils.Debugf("[M-DOCS] reconnecting in %v (attempt %d)", d, next)
	time.Sleep(d)
	t.reconnecting.Store(false) // a failed connect schedules the next one itself
	if !t.IsRunning() {
		return
	}

	t.RecordReconnect()
	t.connectToDoc(next)
}

// reconnectBackoff returns an exponential backoff with jitter, capped at 15s.
func reconnectBackoff(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	shift := n - 1
	if shift > 5 {
		shift = 5
	}
	d := 500 * time.Millisecond * time.Duration(1<<uint(shift))
	if d > 15*time.Second {
		d = 15 * time.Second
	}
	d += time.Duration(rand.Int63n(int64(d/2) + 1))
	return d
}

// fetchDocInfo POSTs to Mail.ru's public-document editor API and parses the
// response into the fields needed to open the collaborative WebSocket.
func (t *MailruDocsTransport) fetchDocInfo(weblink string) (MailruDocsInfo, error) {
	t.jarMu.RLock()
	jar := t.cookieJar
	t.jarMu.RUnlock()
	if jar == nil {
		var err error
		jar, err = cookiejar.New(nil)
		if err != nil {
			return MailruDocsInfo{}, err
		}
	}
	client := &http.Client{Jar: jar, Timeout: 15 * time.Second}

	email, csrf, err := t.signIn(client)
	if err != nil {
		return MailruDocsInfo{}, err
	}
	reqBody := map[string]string{
		"x-email":  email,
		"public":   "/" + weblink,
		"platform": "desktop_web",
	}
	jsonData, _ := json.Marshal(reqBody)

	utils.Debugf("[M-DOCS] fetchDocInfo POST %s", t.apiURL)

	req, _ := http.NewRequest("POST", t.apiURL, bytes.NewBuffer(jsonData))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("User-Agent", mailruUserAgent)
	req.Header.Set("X-Api-Version", "4")
	req.Header.Set("Referer", fmt.Sprintf("https://cloud.mail.ru/public/%s?weblink=%s", weblink, weblink))
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
		req.Header.Set("X-Email", email)
	}

	resp, err := client.Do(req)
	if err != nil {
		return MailruDocsInfo{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// The body names the error: {"error":"BAD/WEBLINK/URL",...}.
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&e)
		return MailruDocsInfo{}, t.refused(&StatusError{Code: resp.StatusCode, Reason: e.Error})
	}

	bodyBytes, _ := io.ReadAll(resp.Body)

	var res map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &res); err != nil {
		return MailruDocsInfo{}, fmt.Errorf("failed to parse JSON: %w", err)
	}

	apiBase, _ := res["api"].(string)
	token, _ := res["token"].(string)

	document, ok := res["document"].(map[string]interface{})
	if !ok || document == nil {
		return MailruDocsInfo{}, fmt.Errorf("document object missing")
	}

	docKey, _ := document["key"].(string)
	fileType, _ := document["fileType"].(string)
	docURL, _ := document["url"].(string)
	docTitle, _ := document["title"].(string)
	permissions, _ := document["permissions"].(map[string]interface{})
	if permissions == nil {
		permissions = make(map[string]interface{})
	}

	editorConfig, ok := res["editorConfig"].(map[string]interface{})
	if !ok || editorConfig == nil {
		return MailruDocsInfo{}, fmt.Errorf("editorConfig object missing")
	}
	callbackURL, _ := editorConfig["callbackUrl"].(string)

	userObj, _ := editorConfig["user"].(map[string]interface{})
	var editorUserID string
	if userObj != nil {
		editorUserID, _ = userObj["id"].(string)
	}

	// https -> wss (http -> ws for a test server).
	wsBase := strings.Replace(apiBase, "http", "ws", 1)
	wsURL := fmt.Sprintf("%s/doc/%s/c/?EIO=4&transport=websocket", wsBase, docKey)

	return MailruDocsInfo{
		Token:        token,
		DocKey:       docKey,
		WsURL:        wsURL,
		FileType:     fileType,
		DocURL:       docURL,
		DocTitle:     docTitle,
		Permissions:  permissions,
		CallbackURL:  callbackURL,
		EditorUserID: editorUserID,
	}, nil
}

func randUserID() string {
	return fmt.Sprintf("%010d", rand.New(rand.NewSource(time.Now().UnixNano())).Intn(1000000000))
}

// ---- CookieExchanger ----

// FetchCookies returns a snapshot of the transport's current cookie jar as
// name -> value. Used by the exit node to answer a SubtypeCookiesRequest.
func (t *MailruDocsTransport) FetchCookies() (map[string]string, error) {
	t.jarMu.RLock()
	jar := t.cookieJar
	t.jarMu.RUnlock()
	if jar == nil {
		return nil, fmt.Errorf("mailru: cookie jar is nil")
	}
	return jarValues(t.cloudURL(), jar), nil
}

// ApplyCookies replaces the transport's cookie jar with the provided values
// and forces the current session to reconnect. The account of a cookies
// file (LoadCookieFile) stays: its cookies win over the values.
func (t *MailruDocsTransport) ApplyCookies(values map[string]string) error {
	if len(values) == 0 {
		return nil
	}
	t.jarMu.Lock()
	merged := make(map[string]string, len(values)+len(t.fileCookies))
	for k, v := range values {
		merged[k] = v
	}
	for k, v := range t.fileCookies {
		merged[k] = v
	}
	t.cookieJar = newJar(t.cloudURL(), merged)
	t.csrf = ""
	t.jarMu.Unlock()
	t.errMu.Lock()
	t.loginAsked = time.Time{}
	t.errMu.Unlock()

	utils.Debugf("[M-DOCS] applied %d cookies, forcing reconnect", len(merged))

	t.Mu.Lock()
	session := t.session
	t.session = nil
	t.SetConnected(false)
	t.Mu.Unlock()
	if session != nil && session.Conn != nil {
		_ = session.Conn.Close()
	}
	if t.IsRunning() {
		t.scheduleReconnect(0)
	}
	return nil
}
