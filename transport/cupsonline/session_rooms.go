package cupsonline

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport"
)

// Since November 2026 cups.online lets only the session that created a room
// into it: anyone else gets 403 "Отсутствуют права доступа на эту страницу".
// The creator's sessionid cookie still works from any address, and with it
// every visit is the same participant. So a room list in the third format
// carries that cookie, and both ends enter the rooms as the creator:
//
//	{"v":3,"rooms":[...],"sid":"<sessionid>","until":<unix seconds>}
//
// until is when the cookie expires (cups.online gives it 14 days and does
// not extend it); whoever manages the rooms replaces the list before then.
// A v3 list always stripes. Builds older than this format fail to parse it
// instead of joining rooms they cannot enter.
//
// Being one participant, the two ends cannot tell their messages apart by
// the sender's user uuid any more, and each hears its own messages echoed.
// On this wire every message's data starts with a 4-byte sender tag (see
// newSenderTag), and a message carrying one's own tag is dropped.

const sessionRoomsVersion = 3

// sessionLifetime is how long a sessionid lives when its cookie does not
// say (measured 2026-11-08: Max-Age 1209600).
const sessionLifetime = 14 * 24 * time.Hour

type roomListV3 struct {
	V     int      `json:"v"`
	Rooms []string `json:"rooms"`
	SID   string   `json:"sid"`
	Until int64    `json:"until"`
}

// roomList is a parsed --url: the rooms, whether they are striped and, for
// a v3 list, the session to enter them with.
type roomList struct {
	ids     []string
	striped bool
	sid     string
	until   time.Time
}

// PackRoomList packs rooms entered with session sid into a v3 list.
func PackRoomList(ids []string, sid string, until time.Time) string {
	return packSessionRooms(ids, sid, until)
}

// packSessionRooms packs a v3 list.
func packSessionRooms(ids []string, sid string, until time.Time) string {
	raw, _ := json.Marshal(roomListV3{V: sessionRoomsVersion, Rooms: ids, SID: sid, Until: until.Unix()})
	return base64.RawURLEncoding.EncodeToString(raw)
}

// unpackRoomList reads a packed list of any format.
func unpackRoomList(s string) (roomList, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return roomList{}, err
	}
	var v struct {
		V int `json:"v"`
	}
	if len(raw) > 0 && raw[0] == '{' && json.Unmarshal(raw, &v) == nil && v.V == sessionRoomsVersion {
		var l roomListV3
		if err := json.Unmarshal(raw, &l); err != nil {
			return roomList{}, err
		}
		if len(l.Rooms) == 0 || l.SID == "" {
			return roomList{}, errors.New("room list: no rooms or no session")
		}
		return roomList{ids: l.Rooms, striped: true, sid: l.SID, until: time.Unix(l.Until, 0)}, nil
	}
	ids, striped, err := transport.UnpackRooms(s)
	if err != nil {
		return roomList{}, err
	}
	return roomList{ids: ids, striped: striped}, nil
}

// RoomList describes a packed room list for whoever manages the rooms.
type RoomList struct {
	Rooms int
	// Until is when the rooms' session expires; zero for a list without one,
	// which cups.online no longer lets anyone but the creator into.
	Until time.Time
}

// ParseRoomList reads a packed room list of any format.
func ParseRoomList(list string) (RoomList, error) {
	l, err := unpackRoomList(list)
	if err != nil {
		return RoomList{}, err
	}
	return RoomList{Rooms: len(l.ids), Until: l.until}, nil
}

// newHTTPSession is a fresh cookie session for the room pages.
func newHTTPSession() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{
		Jar:     jar,
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}
}

// sessionFor is a session that enters rooms under baseURL as the holder of
// sid; with no sid, a fresh one.
func sessionFor(baseURL, sid string) *http.Client {
	c := newHTTPSession()
	if sid != "" {
		c.Jar.SetCookies(mustParseURL(baseURL), []*http.Cookie{{Name: "sessionid", Value: sid, Path: "/"}})
	}
	return c
}

// sessionOf is the sessionid a room was entered with and when it expires.
func sessionOf(a *cupsAuth, baseURL string) (sid string, until time.Time) {
	for _, c := range a.httpClient.Jar.Cookies(mustParseURL(baseURL)) {
		if c.Name == "sessionid" {
			sid = c.Value
		}
	}
	until = a.sessionUntil
	if until.IsZero() {
		until = time.Now().Add(sessionLifetime)
	}
	return sid, until
}

// noAccess reports the page cups.online answers a room's visitors other
// than its creator with.
func noAccess(status int, body []byte) bool {
	return status == http.StatusForbidden && bytes.Contains(body, []byte("Отсутствуют права доступа"))
}

// senderTagLen is the size of the sender tag on the v3 wire.
const senderTagLen = 4

// newSenderTag is a random tag whose top bit says the role, so a client's
// tag never equals its exit's.
func newSenderTag(isClient bool) []byte {
	tag := make([]byte, senderTagLen)
	rand.Read(tag)
	v := binary.BigEndian.Uint32(tag) &^ (1 << 31)
	if isClient {
		v |= 1 << 31
	}
	binary.BigEndian.PutUint32(tag, v)
	return tag
}
