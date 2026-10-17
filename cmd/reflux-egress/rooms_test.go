package main

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// create-rooms answers with the list and its size, or the error.
func TestCreateRoomsCmd(t *testing.T) {
	old := createRooms
	t.Cleanup(func() { createRooms = old })
	createRooms = func(_ context.Context, n int) (string, int, error) {
		if n == 9 {
			return "", 0, errors.New("could not create any room: 429")
		}
		return "LIST", n - 1, nil
	}
	for _, c := range []struct{ arg, want string }{
		{"4", `{"list":"LIST","rooms":3}`},
		{"9", `{"rooms":0,"error":"could not create any room: 429"}`},
	} {
		var out strings.Builder
		if err := createRoomsCmd([]string{c.arg}, &out); err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(out.String()); got != c.want {
			t.Errorf("create-rooms %s: got %s, want %s", c.arg, got, c.want)
		}
	}
	if err := createRoomsCmd([]string{"four"}, &strings.Builder{}); err == nil {
		t.Error("create-rooms four: want an error")
	}
	if err := createRoomsCmd(nil, &strings.Builder{}); err == nil {
		t.Error("create-rooms without a count: want an error")
	}
}

// A room list is dead once a room is gone, unknown when a room could not
// be told and none is gone.
func TestCheckDocsRoomLists(t *testing.T) {
	old := checkRooms
	t.Cleanup(func() { checkRooms = old })
	checkRooms = func(_ context.Context, list string) (int, int, error) {
		switch list {
		case "gone":
			return 1, 4, errors.New("room 1234: status 502")
		case "flaky":
			return 0, 4, errors.New("room 1234: status 502")
		}
		return 0, 4, nil
	}
	in := "cupsonline ok\ncupsonline gone\ncupsonline flaky\n"
	var out strings.Builder
	if err := checkDocs(strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	want := `{"line":1,"state":"ok"}
{"line":2,"state":"dead","error":"1 of 4 cups.online rooms are gone"}
{"line":3,"state":"unknown","error":"room 1234: status 502"}`
	if got := strings.TrimSpace(out.String()); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}
