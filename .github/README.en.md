<div align="center">
  <h1>
    <img src="https://raw.githubusercontent.com/BeleBob/ReFlux-Android/main/design/logo/logo.svg" width="64" align="middle" alt="">
    ReFlux
  </h1>
  <p>
    <a href="https://github.com/BeleBob/ReFlux/releases/latest"><img src="https://img.shields.io/github/v/release/BeleBob/ReFlux?filter=reflux-v*&amp;display_name=tag&amp;style=flat-square&amp;color=EA1A1A&amp;label=release" alt="Latest release"></a>
    <a href="https://github.com/BeleBob/ReFlux/actions/workflows/ci.yml"><img src="https://github.com/BeleBob/ReFlux/actions/workflows/ci.yml/badge.svg" alt="CI status"></a>
    <a href="https://github.com/BeleBob/ReFlux/actions/workflows/reflux-images.yml"><img src="https://github.com/BeleBob/ReFlux/actions/workflows/reflux-images.yml/badge.svg" alt="Images build"></a>
    <a href="../LICENSE"><img src="https://img.shields.io/badge/license-GPL--3.0--or--later-blue?style=flat-square" alt="GPL-3.0-or-later license"></a>
  </p>
  <p>
    <img src="https://img.shields.io/badge/Linux-amd64%20%7C%20arm64-455a64?style=flat-square&amp;logo=linux&amp;logoColor=white" alt="Linux amd64 and arm64">
    <img src="https://img.shields.io/badge/Docker-Compose-2496ED?style=flat-square&amp;logo=docker&amp;logoColor=white" alt="Docker Compose">
    <img src="https://img.shields.io/badge/AmneziaWG-tunnels-455a64?style=flat-square" alt="AmneziaWG tunnels">
    <img src="https://img.shields.io/badge/Go-1.26%2B-00ADD8?style=flat-square&amp;logo=go&amp;logoColor=white" alt="Go 1.26 or newer">
    <a href="https://github.com/BeleBob/ReFlux-Android"><img src="https://img.shields.io/badge/client-Android-3DDC84?style=flat-square&amp;logo=android&amp;logoColor=white" alt="Android client"></a>
  </p>
  <p><a href="README.md">Русский</a> · <strong>English</strong></p>
</div>

ReFlux is a tunnel server: it deploys and maintains exit nodes (their core
is [OpenFlux](https://github.com/p1neappleXpress/OpenFlux)) on a Linux server. Every client gets a node of its own with its
own encryption key. Clients' traffic leaves only through AmneziaWG tunnels.
It is managed from the command line, a Telegram bot and a web panel.

**[Download the latest release](https://github.com/BeleBob/ReFlux/releases/latest)** ·
**[The Android app](https://github.com/BeleBob/ReFlux-Android)**

## Contents
- [Features](#features)
- [Usage](#usage)
- [Development](#development)
- [Testing](#testing)
- [Deploy and CI/CD](#deploy-and-cicd)
- [Contributing](#contributing)
- [To do](#to-do)
- [Origin and license](#origin-and-license)

## Features
- **Isolated clients.** Every client runs in a container of its own with its
  own key and document. Access is given, paused, limited in time and revoked
  per client.
- **Controlled way out.** The egress container lets traffic out only through
  AmneziaWG tunnels. Russian addresses and the rest of the world go through
  different tunnels; the world one fails over to backup servers.
- **Checks and recovery.** `reflux doctor` checks the server, the tunnels and
  the nodes. `reflux heal` brings stopped nodes back on its own.
- **Telegram bots.** The owner's bot reports failures and manages clients.
  The client bot takes access requests and hands out the connection details.
- **Web panel.** The server, the tunnels and the clients, with load and
  traffic charts. Reachable from the local network only.
- **Backups.** Daily archives of the data and `reflux restore`.

## Usage

### Requirements
- Linux (Debian, Ubuntu or alike), x86_64 or arm64.
- The AmneziaWG kernel module.
- AmneziaWG configuration files for the outgoing tunnels.

### Install
Run as the user who will manage the server:

```sh
curl -fsSL https://github.com/BeleBob/ReFlux/releases/latest/download/install.sh | sh
```

The script downloads `reflux` from the latest release, checks its checksum
and starts the `reflux setup` wizard. The wizard installs what is missing and
asks before every step that needs root. It is safe to run again;
`reflux setup --check` only reports.

### Main commands
```sh
reflux add <name> --url <document>   # add a client
reflux list                          # the clients and their state
reflux show <name>                   # connection details and a QR code
reflux revoke <name>                 # revoke access
reflux doctor                        # check the server
reflux update                        # update the images
```

Clients connect with the [ReFlux for Android](https://github.com/BeleBob/ReFlux-Android)
app using an `openflux://` link or a QR code. Every command and the manual
install are in the [operator guide](../docs/reflux/SERVER.ru.md) (Russian).

## Development

### Requirements
[Go](https://go.dev/dl/) of the version in `go.mod`.

### Build
```sh
go build ./cmd/reflux
```

### Layout
```
ReFlux/
  cmd/reflux/          # the management tool, the bots and the web panel
  cmd/reflux-egress/   # the egress container's controller
  deploy/reflux/       # Dockerfile, installer and system units
  docs/reflux/         # documentation and the changelog
```

The other directories hold the OpenFlux core.

## Testing
The project has Go unit tests:

```sh
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
```

## Deploy and CI/CD
- **CI** (`ci.yml`) runs the tests, the race detector, `go vet` and builds
  for every supported platform on each push and pull request.
- **Images** (`reflux-images.yml`): `reflux-node` and `reflux-egress` go to
  GHCR on changes in `main` and for every release.
- **Releases** (`reflux-release.yml`) come from a `reflux-vX.Y.Z` tag:
  binaries for linux/amd64 and linux/arm64, the installer and `SHA256SUMS`;
  the notes come from the [CHANGELOG](../docs/reflux/CHANGELOG.md).

## Contributing
Bugs and ideas go to [Issues](https://github.com/BeleBob/ReFlux/issues).
Changes come as pull requests to `main`; the tests and `go vet` must pass.
User-facing changes go under «Не выпущено» in the
[CHANGELOG](../docs/reflux/CHANGELOG.md).

Fixes to the OpenFlux core are separate commits with a regression test, so
they can be offered to the original project.

## To do
- [x] A node and a key per client
- [x] One-command install and a setup wizard
- [x] Telegram bots and a web panel
- [x] An Android app of its own: [ReFlux for Android](https://github.com/BeleBob/ReFlux-Android)
- [x] Subscription self-update through the document
- [ ] Several clients on one node with key authentication

## Origin and license
ReFlux is a modified version of [OpenFlux](https://github.com/p1neappleXpress/OpenFlux)
(p1neappleXpress): since September 2026 it adds the management tool, the
egress container, the bots and the web panel, and fixes in the core. The
core's documentation is the [OpenFlux README](../README.md).

It is distributed under the GNU GPL v3.0 or any later version: see
[LICENSE](../LICENSE), [COPYRIGHT](../COPYRIGHT) and [NOTICE](../NOTICE).
