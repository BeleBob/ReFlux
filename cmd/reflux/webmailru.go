package main

import (
	"fmt"
	"net/http"
	"time"
)

// The panel's mail.ru page: the account the nodes sign in with
// (mailrulogin.go), a check now, and a field for new cookies. The page
// never shows the cookies, and they go nowhere but the egress's check and
// mailru/cookies.

type mailruData struct {
	Have  bool
	Level string // ok, warn, FAIL: the doctor's verdict
	Text  string
}

func (w *webServer) mailruPage(r *http.Request) (string, pageData, error) {
	l := w.lang()
	_, have, err := w.s.mailruLogin()
	d := mailruData{Have: have}
	dr := &doctor{}
	dr.mailru(w.s)
	f := finding{Level: levelWarn, Msg: "mailru.none"} // no clients on mail.ru yet
	if len(dr.findings) > 0 {
		f = dr.findings[0]
	}
	d.Level, d.Text = f.Level.String(), f.text(l)
	return "mailru", pageData{Title: tr(l, "web.mailru.title"), Active: "mailru", Body: d}, err
}

// mailruAction saves new cookies (login) or checks the login now (check).
func (w *webServer) mailruAction(r *http.Request) (string, error) {
	unlock, err := w.s.Lock(lockWait)
	if err != nil {
		return "", err
	}
	defer unlock()
	switch r.PathValue("what") {
	case "login":
		if err := w.s.Init(); err != nil {
			return "", err
		}
		if _, err := w.s.SaveMailruLogin(r.FormValue("cookies"), time.Now()); err != nil {
			return "", err
		}
		return "/mailru?ok=mailru", nil
	case "check":
		if _, err := w.s.CheckMailruLogin(time.Now()); err != nil {
			return "", err
		}
		return "/mailru?ok=mailrucheck", nil
	}
	return "", fmt.Errorf("unknown action %q", r.PathValue("what"))
}
