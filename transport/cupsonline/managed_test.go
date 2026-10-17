package cupsonline

import (
	"context"
	"testing"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport"
)

func TestCreateRoomsMakesAStripedList(t *testing.T) {
	f := newFakeCups(t)
	list, made, err := CreateRooms(context.Background(), 3)
	if err != nil {
		t.Fatal(err)
	}
	ids, striped, err := transport.UnpackRooms(list)
	if err != nil || !striped || len(ids) != 3 || made != 3 {
		t.Fatalf("list %q: ids %v striped %v made %d err %v", list, ids, striped, made, err)
	}
	if f.roomsCreated() != 3 {
		t.Fatalf("server created %d rooms, want 3", f.roomsCreated())
	}
	if _, _, err := CreateRooms(context.Background(), 0); err == nil {
		t.Fatal("0 rooms: want an error")
	}
}

func TestCheckRoomsCountsOnlyTheServersWord(t *testing.T) {
	f := newFakeCups(t)
	list, _, err := CreateRooms(context.Background(), 4)
	if err != nil {
		t.Fatal(err)
	}
	ids, _, _ := transport.UnpackRooms(list)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if gone, total, err := CheckRooms(ctx, list); gone != 0 || total != 4 || err != nil {
		t.Fatalf("all open: gone %d of %d, err %v", gone, total, err)
	}
	f.closeRoom(ids[1])
	if gone, _, err := CheckRooms(ctx, list); gone != 1 || err != nil {
		t.Fatalf("one closed (404): gone %d, err %v", gone, err)
	}
	// cups hands out a new room for a closed one.
	f.locked(func() { f.redirectGone = true })
	f.closeRoom(ids[2])
	if gone, _, err := CheckRooms(ctx, list); gone != 2 || err != nil {
		t.Fatalf("two closed, one redirected: gone %d, err %v", gone, err)
	}
	// A failing page says nothing about the room.
	f.locked(func() { f.failPages[ids[0]] = 1 })
	if gone, _, err := CheckRooms(ctx, list); gone != 2 || err == nil {
		t.Fatalf("a 502: gone %d, err %v; want 2 and an error", gone, err)
	}
	if _, _, err := CheckRooms(ctx, "not a list"); err == nil {
		t.Fatal("garbage: want an error")
	}
}
