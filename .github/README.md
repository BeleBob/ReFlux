# ReFlux

ReFlux — набор инструментов для развёртывания и обслуживания exit-нод
[OpenFlux](https://github.com/p1neappleXpress/OpenFlux) на Linux-сервере.
Каждый клиент получает отдельную ноду со своим ключом шифрования. Исходящий
трафик клиентов проходит только через туннели AmneziaWG. Управление
выполняется из командной строки, через Telegram-бот и веб-панель.

[![CI](https://github.com/BeleBob/ReFlux/actions/workflows/ci.yml/badge.svg)](https://github.com/BeleBob/ReFlux/actions/workflows/ci.yml)
[![Images](https://github.com/BeleBob/ReFlux/actions/workflows/reflux-images.yml/badge.svg)](https://github.com/BeleBob/ReFlux/actions/workflows/reflux-images.yml)
[![Release](https://img.shields.io/github/v/release/BeleBob/ReFlux?filter=reflux-v*&label=release)](https://github.com/BeleBob/ReFlux/releases)
[![License: GPL v3](https://img.shields.io/badge/license-GPL--3.0--or--later-blue.svg)](../LICENSE)

## Содержание
- [Технологии](#технологии)
- [Возможности](#возможности)
- [Использование](#использование)
- [Разработка](#разработка)
- [Тестирование](#тестирование)
- [Deploy и CI/CD](#deploy-и-cicd)
- [Contributing](#contributing)
- [To do](#to-do)
- [Команда проекта](#команда-проекта)
- [Источники](#источники)
- [Лицензия](#лицензия)

## Технологии
- [Go](https://go.dev/)
- [Docker](https://www.docker.com/) и Docker Compose
- [AmneziaWG](https://github.com/amnezia-vpn/amneziawg-linux-kernel-module)
- [nftables](https://netfilter.org/projects/nftables/) и [Unbound](https://nlnetlabs.nl/projects/unbound/)
- [Telegram Bot API](https://core.telegram.org/bots/api)
- [GitHub Actions](https://docs.github.com/actions) и [GitHub Container Registry](https://docs.github.com/packages)

## Возможности
- **Изоляция клиентов.** Для каждого клиента запускается отдельный контейнер
  с собственным ключом и документом. Доступ выдаётся, приостанавливается,
  ограничивается по сроку и отзывается независимо.
- **Контролируемый выход в интернет.** Контейнер egress выпускает трафик только
  через туннели AmneziaWG. Российские адреса и остальной мир обслуживаются
  разными туннелями, для мирового поддерживаются резервные серверы.
- **Диагностика и восстановление.** `reflux doctor` проверяет состояние сервера,
  туннелей и нод. `reflux heal` автоматически восстанавливает остановленные
  ноды.
- **Telegram-боты.** Бот администратора присылает уведомления о сбоях и
  позволяет управлять клиентами. Бот для клиентов принимает заявки на доступ и
  выдаёт данные для подключения.
- **Веб-панель.** Состояние сервера, туннелей и клиентов, графики нагрузки и
  трафика. Доступна только из локальной сети.
- **Резервное копирование.** Ежедневные архивы данных и восстановление
  командой `reflux restore`.

## Использование

### Требования
- Linux (Debian, Ubuntu или совместимый дистрибутив), x86_64 или arm64.
- Модуль ядра AmneziaWG.
- Конфигурационные файлы AmneziaWG для исходящих туннелей.

### Установка
Выполните от имени пользователя, который будет управлять сервером (не root):

```sh
curl -fsSL https://github.com/BeleBob/ReFlux/releases/latest/download/install.sh | sh
```

Скрипт загружает утилиту `reflux` из последнего релиза, проверяет её
контрольную сумму и запускает мастер настройки `reflux setup`. Мастер
устанавливает недостающие компоненты и перед каждым действием с правами root
запрашивает подтверждение. Его можно запускать повторно; `reflux setup --check`
только проверяет текущее состояние.

### Основные команды
```sh
reflux add <name> --url <document>   # добавить клиента
reflux list                          # список клиентов и их состояние
reflux show <name>                   # данные для подключения и QR-код
reflux revoke <name>                 # отозвать доступ
reflux doctor                        # проверить сервер
reflux update                        # обновить образы
```

Полное описание команд и ручной установки — в
[руководстве администратора](../docs/reflux/SERVER.ru.md).

## Разработка

### Требования
Для сборки необходим [Go](https://go.dev/dl/) версии, указанной в `go.mod`.

### Сборка
```sh
go build ./cmd/reflux
```

### Структура
- `cmd/reflux` — утилита управления, боты и веб-панель.
- `cmd/reflux-egress` — контроллер контейнера egress.
- `deploy/reflux` — Dockerfile, установщик и системные unit-файлы.
- `docs/reflux` — документация и история изменений.

Остальные каталоги содержат ядро OpenFlux.

## Тестирование
Проект покрыт модульными тестами Go. Для запуска выполните:

```sh
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
```

## Deploy и CI/CD
- **CI** (`ci.yml`) запускает тесты, проверку гонок, `go vet` и сборку под все
  поддерживаемые платформы при каждом push и pull request.
- **Образы** (`reflux-images.yml`) `reflux-node` и `reflux-egress` публикуются
  в GHCR при изменениях в `main` и для каждого релиза.
- **Релизы** (`reflux-release.yml`) создаются тегом `reflux-vX.Y.Z`: собираются
  бинарные файлы для linux/amd64 и linux/arm64, публикуются установщик и
  `SHA256SUMS`, описание берётся из [CHANGELOG](../docs/reflux/CHANGELOG.md).

## Contributing
Сообщения об ошибках и предложения принимаются через
[Issues](https://github.com/BeleBob/ReFlux/issues). Изменения предлагаются
через pull request в ветку `main`; перед отправкой убедитесь, что проходят
тесты и `go vet`. Пользовательские изменения описываются в разделе
«Не выпущено» файла [CHANGELOG](../docs/reflux/CHANGELOG.md).

Исправления ядра OpenFlux оформляются отдельными коммитами с регрессионным
тестом, чтобы их можно было предложить в основной проект.

## To do
- [x] Отдельная нода и ключ для каждого клиента
- [x] Установка одной командой и мастер настройки
- [x] Telegram-боты и веб-панель
- [ ] Обслуживание нескольких клиентов одной нодой с аутентификацией по ключу
- [ ] Доработанное Android-приложение

## Источники
ReFlux основан на [OpenFlux](https://github.com/p1neappleXpress/OpenFlux) и
является его изменённой версией. Документация ядра —
[README OpenFlux](../README.ru.md).

## Лицензия
Распространяется по лицензии GNU GPL v3.0 или более поздней версии. См.
[LICENSE](../LICENSE), [COPYRIGHT](../COPYRIGHT) и [NOTICE](../NOTICE).
