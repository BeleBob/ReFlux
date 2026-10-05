package main

import "github.com/p1neappleXpress/OpenFlux/cmd/reflux/internal/i18n"

// The texts live in internal/i18n; these are the short names the rest of
// the package uses.

type lang = i18n.Lang

const (
	langEN = i18n.EN
	langRU = i18n.RU
)

type phrase = i18n.Phrase

func ph(id string, args ...any) phrase { return i18n.P(id, args...) }

func tr(l lang, id string, args ...any) string { return i18n.T(l, id, args...) }
