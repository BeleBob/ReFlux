# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

OpenFlux: an IPv4 TCP/UDP tunnel core in Go (module `openflux`). One binary is
client, exit node, or benchmark depending on `--role`. Packets travel over
pluggable "carrier" transports (Yandex.Docs/Volga/Board, MAX WebRTC,
Cups.online, Mail.ru, direct TCP). README.md is the user-facing reference for
flags and usage; PROTOCOL_NEGOTIATION.md is the byte-level wire spec.

## ReFlux: контекст, цели, правила

Общаться с владельцем на русском.

ReFlux (github.com/BeleBob/ReFlux, remote `origin`) — форк OpenFlux
(github.com/p1neappleXpress/OpenFlux, remote `upstream`), GPL-3.0. Обновления из
upstream должны сливаться легко.

Окружение владельца: разработка на CachyOS (VSCodium, Go, Docker); exit-нода на
домашнем сервере Debian 13, деплой через Docker Compose; основной клиент —
Android (OpenFluxAndroid, отдельный репозиторий, форк позже).

Цели:
1. Управление сервером: развёртывание и обслуживание exit-ноды на Linux через
   Docker одной-двумя командами (скрипт управления или Makefile, позже —
   возможно, простая веб-панель).
2. Доступ для нескольких клиентов: у каждого свой ключ; ключи можно выдавать,
   отзывать, просматривать списком. Этап 1 — отдельный контейнер exit-ноды на
   клиента со своим ключом шифрования и своим URL транспорта. Этап 2 (позже) —
   одна нода с аутентификацией по ключу на уровне протокола.
3. Доработанное Android-приложение (позже, отдельным этапом).
4. Ведение на GitHub: CI (сборка и проверка), Docker-образ в GHCR, задачи в Issues.

Правила:
- Перед крупными изменениями — сначала план, ждать подтверждения владельца.
- Небольшие логичные коммиты с понятными сообщениями.
- Комментарии в коде и сообщения коммитов — только на английском.
- После изменений Go-кода проверять, что проходит `go build ./...`.
- Баги в коде OpenFlux (`transport/`, `tunnel/`, корень) можно исправлять по
  своему решению (владелец разрешил); прочие изменения логики транспортов и
  туннеля — только после обсуждения. Такие правки — отдельными коммитами с
  регрессионным тестом, чтобы их можно было предложить в upstream.
- Никаких секретов (ключей, токенов, URL документов, cookies) в репозитории —
  только через `.env` (в `.gitignore`) и `.env.example`.
- Не трогать LICENSE, COPYRIGHT, NOTICE и копирайты в коде. В README должно
  быть указано, что проект основан на OpenFlux и изменён.
- Сохранять совместимость с upstream: своё — предпочтительно в новых файлах и
  каталогах, а не правками upstream-файлов.

## ReFlux components (all new files; no upstream file is changed)

- `cmd/reflux` — host CLI (`add/list/show/pause/resume/expire/revoke/apply/update/
  restart/status/logs/heal/doctor/bot`). Nodes serve the core's IPC bridge
  (`IPCSocket = /state/ipc.sock`); `list`/`status`/`doctor` read who is online and
  traffic from it. `render` re-syncs `node.conf` from `client.json`; `apply`
  recreates nodes started before their `node.conf` changed. `reflux bot` is a
  Telegram bot (systemd user service, token in `~/reflux/telegram.json`): it runs
  the doctor checks every minute and reports findings whose `Sig` changed for
  two runs; it talks to the Bot API from the host, not through egress. It also
  manages clients through screens with inline buttons (`botui.go`; one message per
  screen, edited on each press) and the same commands. Texts are in `i18n.go`
  (`messages`: English and Russian with the same arguments, checked by a test); the
  CLI prints English, the bot the owner's language (`lang` in `telegram.json`).
  Doctor findings carry a message id and arguments; the egress state comes from
  its `/run/reflux-egress/status.json`. `reflux web` is the same panel for the LAN
  (`web.go`, `web/panel.html` embedded; systemd user service `reflux-web`): it
  listens on the LAN address in `~/reflux/web.json`, answers private addresses
  only, signs in `trusted` addresses or browsers with a one-time link (`/web`
  in the bot, `reflux web login`; token and session hashes in `web-*.json`),
  and takes POSTs from its own origin only. Commands that change clients or containers hold the flock
  `~/reflux/.lock` (`Store.Lock`; `heal` skips when busy).
  Data in `~/reflux` (`REFLUX_HOME`): `clients/<name>/{client.json,key,node.conf}`,
  `state/<name>/`, `egress/{ru-1.conf,world-N.conf}`, `revoked/`; renders
  `compose.yml` (JSON, valid YAML). Each node is a **classic exit with a key**
  (`.conf` without `[Transport]` sections) so classic and Session apps both
  connect; `network_mode: service:egress`, no ports, proxy env blanked.
  `apply` refuses while `hostRuleProblem` finds the egress `ip rule` missing.
- `cmd/reflux-egress` — controller inside the egress container: kill switch
  (nftables, only UDP to active AWG endpoints leaves `eth0`, DNS redirected to
  local unbound), `awg-ru` + `awg-world` on the host's amneziawg kernel module,
  RU prefixes (RIPE delegated, baked in image, refreshed daily) routed to
  `awg-ru` (or, with `egress/ru-direct`, out of `eth0` with the kill switch
  opening only nft set `ru4`; with `egress/ru-fallback-direct`, out of `eth0` only
  while `awg-ru` fails `failLimit` rounds, back after `recoverRounds`),
  with `egress/carrier-direct` the carriers' servers (mail.ru hosts, resolved every
  minute into nft set `carrier4` and /32 routes) out of `eth0` while other RU
  traffic stays in the tunnel,
  the owner's world server choice in `egress/world-select` (`reflux gateway`,
  applied once per change, failover still moves on), world failover in file order without automatic
  return; after a restart it starts with the owner's choice, else the last world
  server that held `rememberAfter` (`world-last` in the `egress-state` volume). Never exits on errors: nodes share its netns; after an egress
  restart `reflux heal` (cron) recreates the stranded nodes.
- `deploy/reflux/egress/Dockerfile` (build from repo root),
  `deploy/reflux/host/reflux-egress-route.service` (host `ip rule`, tied to awg0),
  `docs/reflux/SERVER.ru.md` (operator guide).
- Workflows: `reflux-images.yml` (reflux-node + reflux-egress → GHCR),
  `reflux-release.yml` (`reflux-v*` tag → reflux binaries).
- Mail.ru carrier = OnlyOffice co-authoring over Socket.IO. When a second
  editor joins, the server locks the document for the first one
  (`connectState` with `waitAuth: true`); it must answer `unLockDocument`
  (`unlock: true`) or it is dropped after 30 s (`disconnectReason` 4007) —
  without that, peers kept knocking each other off. `Stop()` leaves with the
  editor's `close` message; exits and SOCKS clients stop transports on SIGTERM
  (`shutdown.go`). `-dd` logs unhandled server messages.
- Local image builds on the dev machine need `docker build --network host`
  (its DNS blocks the Alpine CDN inside containers).

## Commands

The CI gate (`.github/workflows/ci.yml`), run from the repo root:

```sh
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./...   # CI also builds linux/amd64, linux/arm64, darwin/amd64, darwin/arm64
git diff --check
```

- One test: `go test ./transport -run '^TestSessionAcceptsRestartedClient$' -count=1 -v`
- Platform-specific files (`tun_darwin.go`, `tun_windows.go`, `tunnel/l3/backend_*.go`, `netbind_*`) only compile on their GOOS, so run the cross-builds after touching them.
- `mobile/` is a **separate module** (`openflux-mobile`, `replace openflux => ..`). Root `go test ./...` does not cover it, and neither does CI: `cd mobile && go test ./...`. `mobile/ios` is `//go:build ios` and is built by `./build_ios.sh` (macOS + Xcode only).
- Exit-node release binaries use `-tags exitnode`, which swaps `node_wizard.go` for a stub. Check that with `go build -tags exitnode .`.
- Linux raw L3 integration tests need root in a throwaway network namespace:
  ```sh
  go test -race -c -o /tmp/openflux-l3.test ./tunnel/l3
  sudo unshare --net sh -ec 'ip link set lo up; ip link set lo mtu 1280; OPENFLUX_L3_INTEGRATION=1 /tmp/openflux-l3.test -test.run "^TestLinuxRaw" -test.v -test.timeout=45s'
  ```
- Live/network tests skip unless their env vars are set: `OPENFLUX_CUPS_LIVE`, `OPENFLUX_TEST_DOC`/`_KEY`/`_DIRECT`/`_EXPECT` (mobile live node), `OPENFLUX_TEST_SSH`/`_USER`/`_ROOT_PASSWORD`/`_PINNED`/`_CORE_SHA` (provision integration).

## Architecture

Data path: local inbound (macOS utun, Windows Wintun, SOCKS5/HTTP proxy with a
gVisor stack in `tunnel/`, or the iOS packet tunnel) → raw IPv4 packets →
transport stack → carrier → exit node → Internet. The exit backend is picked
on the exit with `--mode`: `l3` (`tunnel/l3`: raw SOCK_RAW SNAT/DNAT with
conntrack, UDP NAT, ICMP/PMTU and reassembly; Linux+root only) or `l4`
(`tunnel/proxy_exit.go`: gVisor terminates TCP/UDP and re-dials). `proxy` is a
deprecated alias for `l4`. Clients never choose the backend.

Transport stack (in `transport/`), from the bottom up:
- `Transport` interface (`transport.go`): Start/Stop/Send/Receive/IsConnected/Stats. Carriers embed `BaseTransport` and live in subpackages (`yandex/`, `oneme/`, `cupsonline/`, `mailru/`), plus `direct.go`.
- Codecs: `BatchedTransport` (`batched.go`/`framing.go`, batch-v2 + zstd, the default) and the legacy per-packet LZ4 (`compressor.go`). They are wire-incompatible, but the classic codec decodes both formats.
- `encrypted.go`: AES-256-GCM records. Keys come from the secret plus a KDF **context** (`kdfcontext.go`, `KDFContexts`). The context defaults to `--url`, which is why both peers need the same `--url`.
- `Session` (`session.go`): a multi-transport authenticated session with a hello/challenge handshake (`control/` envelopes), a replay window, priority failover and liveness pings. Whenever a key is configured, a Session runs, including for classic `--transport=X` setups, which then fall back to "classic" framing against old peers (`SetClassic`, `SetAlternateContexts`). The Session and classic modes layer encryption and framing in **opposite orders**. PROTOCOL_NEGOTIATION.md documents this, and changes must match it byte for byte.
- `manager/`: owns a session's transports, cookie stores, captcha/login checks and background retries. `ipc/`: the Unix-socket protocol the apps use for `CookiesRequest`/`CookiesOffer`. Captchas the exit hits are relayed to the client over control messages, and `auth_proxy.go` lets the app pass them from the exit's IP.

Root package wiring: `main.go` parses flags (short-flag expansion, deprecated
aliases) and `.conf` files (`conf.go`, flags override), then dispatches on
role. `transport_spec.go` parses `--transports=name:prio,...` and bootstraps
the manager. `transport_factory.go` builds a carrier by type.

Adding a transport: implement `Transport`, register it in
`transport_factory.go` (sessions) **and** in the `--transport` switch in
`main.go` (single-transport mode). For captcha support, also implement
`transport.ErrorNotifier` and `transport.CookieExchanger`. `transport/mailru/`
is the reference example.

Shared by every client:
- `share/`: `openflux://` links and QR codes. `--parse-link` and `--make-link` expose them as JSON so the external apps (Android, desktop, iOS) behave identically. Keep the output shape stable.
- `node_wizard.go` + `provision/`: the `--node-wizard` JSON-lines stdin/stdout protocol that provisions an exit over SSH. Secrets travel only on stdin.
- `mobile/`: the gomobile (Android) API and the iOS C library `liboflux.a` (`mobile/ios`) used by `ios-app/` (SwiftUI + NetworkExtension, XcodeGen `project.yml`).

## Release coupling (easy to break)

- `deploy/node-install.sh` pins `CORE_VERSION` and `SHA_<arch>` for the `node-v*` core release. `node-release.yml` rebuilds reproducibly (`-trimpath -buildvcs=false -tags exitnode -ldflags="-s -w -buildid="`) and fails when the hashes differ.
- `provision/pin.go` pins `node-install.sh` by commit and SHA-256. `TestPinnedScriptHash` fails when the script changes without an update there. The sequence is: commit the script, then point `PinnedCommit` at that commit in a follow-up commit (see git history).
- `v*` tags publish the CLI (`release.yml`). CHANGELOG.md follows Keep a Changelog, and the release notes are taken from it.

## Conventions

- Commit subjects are short and lowercase-prefixed by area (`release: ...`, `provision: ...`), or a plain sentence describing the behavior.
- README.md and README.ru.md are both maintained. Update both when user-facing behavior or flags change.
- REVIEW.md is a historical review log with open limitations (IPv6, DPLPMTUD, rekey/forward secrecy, WinDivert L3 not wired). Do not claim those are solved.
- Logging goes through `utils/logging.go` with debug levels `-d`/`-dd`/`-ddd`. Key material and plaintext frames are logged only with `--sensitive`.
- Secret length is counted in UTF-16 code units (`utils.SecretChars`, minimum 16) to match the Kotlin/Java apps. Do not switch to counting bytes.
