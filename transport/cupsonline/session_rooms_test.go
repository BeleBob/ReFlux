package cupsonline

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport"
)

// noEcho checks that nothing p sent itself came back to it.
func noEcho(t *testing.T, p *peer, sent [][]byte) {
	t.Helper()
	time.Sleep(200 * time.Millisecond)
	for {
		select {
		case got := <-p.got:
			for _, s := range sent {
				if string(got) == string(s) {
					t.Fatalf("got its own %q back", s)
				}
			}
		default:
			return
		}
	}
}

// exchange checks traffic both ways between exit and client, and that
// neither end takes its own messages, which both hear as the same
// participant, for the peer's.
func exchange(t *testing.T, exit, client *peer) {
	t.Helper()
	waitFor(t, 5*time.Second, "all channels up", func() bool {
		return allConnected(exit) && allConnected(client)
	})
	deliver(t, client, exit, []byte("up"), 3*time.Second)
	deliver(t, exit, client, []byte("down"), 3*time.Second)
	drain(exit)
	drain(client)
	var up, down [][]byte
	for i := range 20 {
		up = append(up, seqFrame(i))
		down = append(down, seqFrame(100+i))
	}
	for i := range up {
		client.Send(up[i])
		exit.Send(down[i])
	}
	expectSet(t, exit, up)
	expectSet(t, client, down)
	noEcho(t, exit, down)
	noEcho(t, client, up)
}

// Since November 2026 cups.online lets only a room's creator in. The rooms
// the egress makes for a channel carry their session in the list, and the
// node and the app both enter them with it.
func TestSessionRoomsWorkWhenOnlyTheCreatorMayEnter(t *testing.T) {
	f := newFakeCups(t)
	t.Setenv("CUPS_STRIPE", "")
	f.locked(func() { f.ownerOnly = true })
	list, _, err := CreateRooms(context.Background(), 3)
	if err != nil {
		t.Fatal(err)
	}
	cfg := fastConfig()
	exit := mustStart(t, list, false, cfg)
	client := mustStart(t, list, true, cfg)
	exchange(t, exit, client)
	if got := exit.RoomList(); got != list {
		t.Fatalf("exit room list %q, want %q", got, list)
	}
	// A reconnect enters again with the same session.
	for _, id := range exit.RoomUUIDs() {
		f.dropConns(id)
	}
	deliver(t, client, exit, []byte("again"), 5*time.Second)
}

// An exit started without rooms makes them in one session and prints the
// list with it.
func TestExitMakesSessionRooms(t *testing.T) {
	f := newFakeCups(t)
	t.Setenv("CUPS_STRIPE", "")
	f.locked(func() { f.ownerOnly = true })
	cfg := fastConfig()
	exit := mustStart(t, "", false, cfg)
	list := exit.RoomList()
	if l, err := unpackRoomList(list); err != nil || l.sid == "" || len(l.ids) != cfg.NumRooms {
		t.Fatalf("exit printed %q: %+v %v", list, l, err)
	}
	client := mustStart(t, list, true, cfg)
	exchange(t, exit, client)
}

// A list without the session cannot get the client in any more; it fails
// as rooms that are gone, and the list's format stays what it was.
func TestListWithoutSessionIsRefused(t *testing.T) {
	f := newFakeCups(t)
	t.Setenv("CUPS_STRIPE", "")
	list, _, err := CreateRooms(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	f.locked(func() { f.ownerOnly = true })
	old := transport.PackRooms(parseRoomList(list), true)
	_, err = startPeer(t, old, true, fastConfig())
	if err == nil || !strings.Contains(err.Error(), "gone") {
		t.Fatalf("start with an old list: %v, want the rooms gone", err)
	}
}

func TestRoomListFormats(t *testing.T) {
	ids := []string{"11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"}
	until := time.Unix(1795000000, 0)
	l, err := unpackRoomList(packSessionRooms(ids, "sid", until))
	if err != nil || !l.striped || l.sid != "sid" || !l.until.Equal(until) || len(l.ids) != 2 {
		t.Fatalf("v3: %+v %v", l, err)
	}
	// Older builds refuse a v3 list rather than join rooms they can't enter.
	if _, _, err := transport.UnpackRooms(packSessionRooms(ids, "sid", until)); err == nil {
		t.Fatal("transport.UnpackRooms took a v3 list")
	}
	for _, striped := range []bool{false, true} {
		l, err := unpackRoomList(transport.PackRooms(ids, striped))
		if err != nil || l.striped != striped || l.sid != "" || len(l.ids) != 2 {
			t.Fatalf("v1/v2 (striped %v): %+v %v", striped, l, err)
		}
	}
	if _, err := unpackRoomList(packSessionRooms(ids, "", until)); err == nil {
		t.Fatal("a v3 list without a session must not parse")
	}
	if p, err := ParseRoomList(transport.PackRooms(ids, true)); err != nil || p.Rooms != 2 || !p.Until.IsZero() {
		t.Fatalf("ParseRoomList v2: %+v %v", p, err)
	}
}

func TestSenderTagsTellTheRolesApart(t *testing.T) {
	for range 100 {
		e, c := newSenderTag(false), newSenderTag(true)
		if e[0]&0x80 != 0 || c[0]&0x80 == 0 {
			t.Fatalf("exit tag %x, client tag %x", e, c)
		}
	}
}
