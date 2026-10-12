package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Each node tells its client the client's current link (the core's
// ConfigOffer, --config-offer-file): reflux keeps clients/<name>/offer with
// the link and a version that grows whenever the link changes, so an app
// reached over a document that still works picks up the channel's new
// documents on its own. The file holds the key, like the link: 0600.

const offerFile = "offer"

type offer struct {
	Link    string `json:"link"`
	Version int64  `json:"version"`
}

func (s Store) offerPath(name string) string { return filepath.Join(s.clientDir(name), offerFile) }

// writeOffer brings c's offer up to date; the version moves only with the
// link.
func (s Store) writeOffer(c Client, now time.Time) error {
	key, err := s.Key(c.Name)
	if err != nil {
		return err
	}
	link, err := shownLink(c, key)
	if err != nil {
		return err
	}
	var old offer
	b, err := os.ReadFile(s.offerPath(c.Name))
	switch {
	case err == nil:
		json.Unmarshal(b, &old)
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	if old.Link == link {
		return nil
	}
	return writeJSON(s.offerPath(c.Name), offer{Link: link, Version: max(now.Unix(), old.Version+1)})
}
