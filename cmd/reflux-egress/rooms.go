package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport/cupsonline"
)

// create-rooms N: reflux asks for N new cups.online rooms for a channel.
// They are made here, from the address the nodes come from. The answer is
// one JSON line with the packed list (which reflux writes into the node's
// .conf and the client's link) and how many rooms it holds.

// roomsMade is the answer.
type roomsMade struct {
	List  string `json:"list,omitempty"`
	Rooms int    `json:"rooms"`
	Error string `json:"error,omitempty"`
}

// createRooms asks cups.online; tests replace it.
var createRooms = cupsonline.CreateRooms

// checkRooms looks at a list's rooms; tests replace it.
var checkRooms = cupsonline.CheckRooms

// roomsTimeout bounds a command: cups.online makes a room in about a
// second, and createRooms waits longer between tries when refused.
const roomsTimeout = 4 * time.Minute

func createRoomsCmd(args []string, out io.Writer) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: reflux-egress create-rooms <n>")
	}
	n, err := strconv.Atoi(args[0])
	if err != nil {
		return fmt.Errorf("create-rooms: %q is not a number", args[0])
	}
	ctx, cancel := context.WithTimeout(context.Background(), roomsTimeout)
	defer cancel()
	var r roomsMade
	if r.List, r.Rooms, err = createRooms(ctx, n); err != nil {
		r.Error = err.Error()
	}
	return json.NewEncoder(out).Encode(r)
}

// checkRoomList is check-docs for a room list: dead when any room is gone,
// since the channel then runs slower over the rest and gets new rooms.
func checkRoomList(list string) (dead bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	gone, total, err := checkRooms(ctx, list)
	if gone > 0 {
		return true, fmt.Errorf("%d of %d cups.online rooms are gone", gone, total)
	}
	return false, err
}
