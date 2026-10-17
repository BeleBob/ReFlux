package cupsonline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/p1neappleXpress/OpenFlux/transport"
)

// Rooms kept by whoever manages the channel (ReFlux) instead of the exit:
// it creates a channel's rooms once, writes their list into the exit's
// .conf and the client's link, and looks at them every so often, so it
// learns that rooms are gone and hands out new ones together with the new
// link. An exit left alone makes new rooms only at start, and only when
// all of its rooms are gone; nobody but its log learns their list.

// CreateRooms creates n rooms and returns their packed list in the striped
// format, what an exit started without --url prints, and how many rooms it
// holds: fewer than n when cups.online refused some of them.
func CreateRooms(ctx context.Context, n int) (list string, made int, err error) {
	if n < 1 || n > transport.MaxLanes() {
		return "", 0, fmt.Errorf("cups: %d rooms, want 1 to %d", n, transport.MaxLanes())
	}
	auths, err := createRooms(ctx, baseRoomURL, n, DefaultCupsonlineConfig().RoomCreatePause)
	if err != nil {
		return "", 0, err
	}
	ids := make([]string, len(auths))
	for i, a := range auths {
		ids[i] = a.roomUUID
	}
	return transport.PackRooms(ids, true), len(ids), nil
}

// CheckRooms says how many rooms of a packed list are gone, of how many.
// Only the server's word counts: a room page that answers 404 or 410, or
// renders without the room or with another room in its place. A room that
// could not be told (the network, a refusal) is not counted, and err says
// why. It only loads each room's page and never joins the room's channel.
func CheckRooms(ctx context.Context, list string) (gone, total int, err error) {
	ids, _ := parseRooms(list)
	if len(ids) == 0 {
		return 0, 0, errors.New("cups: not a room list")
	}
	client := &http.Client{Timeout: 30 * time.Second}
	for _, id := range ids {
		g, e := roomGone(ctx, client, baseRoomURL, id)
		switch {
		case e != nil:
			err = e
		case g:
			gone++
		}
	}
	return gone, len(ids), err
}

// roomGone loads one room's page.
func roomGone(ctx context.Context, client *http.Client, baseURL, id string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", joinURL(baseURL, id), nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("User-Agent", cupsUA)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "ru-RU,ru;q=0.9")
	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("room %s: %w", shortRoom(id), err)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return true, nil
	case resp.StatusCode != http.StatusOK:
		return false, fmt.Errorf("room %s: status %d", shortRoom(id), resp.StatusCode)
	case err != nil:
		return false, fmt.Errorf("room %s: %w", shortRoom(id), err)
	}
	// A closed room's page renders without the room, or with a new one.
	return firstMatch(reDataRoomUUID, string(body)) != id, nil
}
