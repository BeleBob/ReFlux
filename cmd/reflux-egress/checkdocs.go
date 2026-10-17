package main

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
	"sync"

	"github.com/p1neappleXpress/OpenFlux/transport/mailru"
)

// check-docs: reflux asks whether its clients' and the pool's documents
// still work. The check runs here, in the egress namespace, so the document
// service sees the address the nodes come from, never the host's. Documents
// arrive on stdin ("type url" or "url" per line), never as arguments, which
// any process on the host may read; the answers name them by line number.
// The first line may be "cookies <the account's cookies>": the mail.ru
// account the nodes sign in with, which the documents are opened as (Mail.ru
// opens them for a signed-in account only). It counts as line 1.

// docCheck is the answer for line N (from 1).
type docCheck struct {
	Line  int    `json:"line"`
	State string `json:"state"` // "ok", "dead" or "unknown" (could not tell now)
	Error string `json:"error,omitempty"`
}

// checkDocument checks one document; tests replace it.
var checkDocument = func(transport, url string, cookies map[string]string) (dead bool, err error) {
	return mailru.CheckDocument(url, cookies)
}

const checkParallel = 4

func checkDocs(in io.Reader, out io.Writer) error {
	type job struct {
		line           int
		transport, url string
	}
	var jobs []job
	var cookies map[string]string
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 4096), 1<<20)
	for n := 1; sc.Scan(); n++ {
		if rest, ok := strings.CutPrefix(sc.Text(), "cookies "); ok && n == 1 {
			cookies, _ = mailru.ParseCookies(rest) // none: anonymously
			continue
		}
		f := strings.Fields(sc.Text())
		switch len(f) {
		case 1:
			jobs = append(jobs, job{n, "mailru", f[0]})
		case 2:
			jobs = append(jobs, job{n, f[0], f[1]})
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	results := make([]docCheck, len(jobs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, checkParallel)
	for i, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r := docCheck{Line: j.line, State: "unknown"}
			if j.transport != "mailru" {
				r.Error = "no check for " + j.transport + " documents"
				results[i] = r
				return
			}
			dead, err := checkDocument(j.transport, j.url, cookies)
			switch {
			case dead:
				r.State = "dead"
			case err == nil:
				r.State = "ok"
			}
			if err != nil {
				r.Error = err.Error()
			}
			results[i] = r
		}()
	}
	wg.Wait()
	enc := json.NewEncoder(out)
	for _, r := range results {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	return nil
}
