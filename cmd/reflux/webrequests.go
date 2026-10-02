package main

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// The panel's requests page (requests.go): the waiting requests with the
// owner bot's choices, and the decided ones.

type requestRow struct {
	accessRequest
	Pending bool
}

func (w *webServer) requests(r *http.Request) (string, pageData, error) {
	all, err := w.s.listRequests()
	var rows []requestRow
	for _, q := range all {
		rows = append(rows, requestRow{accessRequest: q, Pending: q.State == reqPending})
	}
	var invs []map[string]string
	for _, inv := range w.s.listInvites(time.Now()) {
		invs = append(invs, map[string]string{"Code": inv.Code, "Link": w.s.inviteLink(inv.Code), "Note": inv.Note,
			"How": tr(w.lang(), "rq.how."+howKey(inv.How)), "Until": inv.Expires.Local().Format("02.01 15:04")})
	}
	return "requests", pageData{Title: tr(w.lang(), "web.nav.requests"), Active: "requests", Refresh: 30,
		Body: map[string]any{"Rows": rows, "Free": w.s.freeCount(), "Invites": invs}}, err
}

// requestAction decides a request: approve with +30, +90 or never, reject,
// block, forget.
func (w *webServer) requestAction(r *http.Request) (string, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		return "", err
	}
	unlock, err := w.s.Lock(lockWait)
	if err != nil {
		return "", err
	}
	defer unlock()
	now := time.Now()
	switch r.PathValue("action") {
	case "approve":
		q, c, err := w.s.approveRequest(id, r.FormValue("how"), now)
		if err != nil {
			return "", err
		}
		w.s.logEvents([]event{{At: now, Level: "ok", RU: tr(langRU, "ev.rq.ok", q.Who(), c.Name), EN: tr(langEN, "ev.rq.ok", q.Who(), c.Name)}})
		if err := apply(w.s, io.Discard); err != nil {
			return "", fmt.Errorf("%s: %w", tr(w.lang(), "ui.add.nostart", c.Name), err)
		}
		return "/requests?ok=rq_approved", nil
	case "reject", "block":
		state := reqRejected
		if r.PathValue("action") == "block" {
			state = reqBlocked
		}
		_, err := w.s.decideRequest(id, state, now)
		return "/requests?ok=rq_" + state, err
	case "forget":
		return "/requests?ok=rq_forgotten", w.s.removeRequest(id)
	}
	return "", fmt.Errorf("unknown action %q", r.PathValue("action"))
}

// inviteAction makes an invite (new) or revokes one.
func (w *webServer) inviteAction(r *http.Request) (string, error) {
	if r.PathValue("code") == "new" {
		_, err := w.s.newInvite(orDefault(r.FormValue("how"), "+30"), r.FormValue("note"), time.Now())
		return "/requests?ok=invite_made#invites", err
	}
	return "/requests?ok=invite_revoked#invites", w.s.revokeInvite(r.PathValue("code"))
}
