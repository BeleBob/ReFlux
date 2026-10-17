package main

import (
	"encoding/json"
	"errors"
	"io"

	"github.com/p1neappleXpress/OpenFlux/transport/mailru"
)

// check-login: reflux asks whether Mail.ru takes the account the nodes
// sign in with, from the address the nodes come from. The cookies come on
// stdin (any form mailru.ParseCookies reads), never as arguments; the
// answer is one JSON line, without them.

// loginCheck is the answer.
type loginCheck struct {
	OK      bool   `json:"ok"`
	Account string `json:"account,omitempty"`
	// Refused: the cookies hold no login, or Mail.ru no longer takes it.
	// Otherwise a failed check says nothing about the login (the network,
	// the service having a bad moment).
	Refused bool   `json:"refused,omitempty"`
	Error   string `json:"error,omitempty"`
}

// checkLogin asks Mail.ru; tests replace it.
var checkLogin = mailru.CheckLogin

func checkLoginCmd(in io.Reader, out io.Writer) error {
	raw, err := io.ReadAll(io.LimitReader(in, 1<<20))
	if err != nil {
		return err
	}
	var r loginCheck
	values, err := mailru.ParseCookies(string(raw))
	if err != nil {
		r.Refused, r.Error = true, "the cookies: "+err.Error()
	} else if r.Account, err = checkLogin(values); err != nil {
		r.Refused, r.Error = errors.Is(err, mailru.ErrLoginRequired), err.Error()
	} else {
		r.OK = true
	}
	return json.NewEncoder(out).Encode(r)
}
