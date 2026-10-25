<div align="center">
  <h1>
    <img src="https://raw.githubusercontent.com/BeleBob/ReFlux-Android/main/design/logo/icon.svg" width="40" align="middle" alt="">
    ReFlux
  </h1>
  <p>
    <a href="https://github.com/BeleBob/ReFlux/releases/latest"><img src="https://img.shields.io/github/v/release/BeleBob/ReFlux?filter=reflux-v*&amp;display_name=tag&amp;style=flat-square&amp;color=EA1A1A&amp;label=release" alt="Последний релиз"></a>
    <a href="https://github.com/BeleBob/ReFlux/actions/workflows/ci.yml"><img src="https://github.com/BeleBob/ReFlux/actions/workflows/ci.yml/badge.svg" alt="Статус CI"></a>
    <a href="https://github.com/BeleBob/ReFlux/actions/workflows/reflux-images.yml"><img src="https://github.com/BeleBob/ReFlux/actions/workflows/reflux-images.yml/badge.svg" alt="Сборка образов"></a>
    <a href="../LICENSE"><img src="https://img.shields.io/badge/license-GPL--3.0--or--later-blue?style=flat-square" alt="Лицензия GPL-3.0 или новее"></a>
  </p>
  <p>
    <img src="https://img.shields.io/badge/Linux-amd64%20%7C%20arm64-455a64?style=flat-square&amp;logo=linux&amp;logoColor=white" alt="Linux amd64 и arm64">
    <img src="https://img.shields.io/badge/Docker-Compose-2496ED?style=flat-square&amp;logo=docker&amp;logoColor=white" alt="Docker Compose">
    <img src="https://img.shields.io/badge/AmneziaWG-tunnels-455a64?style=flat-square" alt="Туннели AmneziaWG">
    <img src="https://img.shields.io/badge/Go-1.26%2B-00ADD8?style=flat-square&amp;logo=go&amp;logoColor=white" alt="Go 1.26 и новее">
    <a href="https://github.com/BeleBob/ReFlux-Android"><img src="https://img.shields.io/badge/client-Android-3DDC84?style=flat-square&amp;logo=android&amp;logoColor=white" alt="Клиент для Android"></a>
  </p>
  <p><strong>Русский</strong> · <a href="README.en.md">English</a></p>
</div>

ReFlux — сервер туннелей: разворачивает и обслуживает exit-ноды (ядро —
[OpenFlux](https://github.com/p1neappleXpress/OpenFlux)) на Linux-сервере.
Каждый клиент получает отдельную ноду со своим ключом шифрования. Исходящий
трафик клиентов проходит только через туннели AmneziaWG. Управление
выполняется из командной строки, через Telegram-бот и веб-панель.

**[Скачать последний релиз](https://github.com/BeleBob/ReFlux/releases/latest)** ·
**[Приложение для Android](https://github.com/BeleBob/ReFlux-Android)**

## Содержание
- [Возможности](#возможности)
- [Использование](#использование)
- [Разработка](#разработка)
- [Тестирование](#тестирование)
- [Deploy и CI/CD](#deploy-и-cicd)
- [Contributing](#contributing)
- [To do](#to-do)
- [Происхождение и лицензия](#происхождение-и-лицензия)

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
Выполните от имени пользователя, который будет управлять сервером:

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

Клиенты подключаются приложением
[ReFlux для Android](https://github.com/BeleBob/ReFlux-Android) по ссылке
`openflux://` или QR-коду. Полное описание команд и ручной установки — в
[руководстве администратора](../docs/reflux/SERVER.ru.md).

## Разработка

### Требования
Для сборки необходим [Go](https://go.dev/dl/) версии, указанной в `go.mod`.

### Сборка
```sh
go build ./cmd/reflux
```

### Структура
```
ReFlux/
  cmd/reflux/          # утилита управления, боты и веб-панель
  cmd/reflux-egress/   # контроллер контейнера egress
  deploy/reflux/       # Dockerfile, установщик и системные unit-файлы
  docs/reflux/         # документация и история изменений
```

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
- [x] Своё приложение для Android — [ReFlux для Android](https://github.com/BeleBob/ReFlux-Android)
- [x] Самообновление подписки через документ
- [ ] Обслуживание нескольких клиентов одной нодой с аутентификацией по ключу

## Происхождение и лицензия
ReFlux — изменённая версия [OpenFlux](https://github.com/p1neappleXpress/OpenFlux)
(p1neappleXpress): с сентября 2026 года добавлены утилита управления,
контейнер egress, боты и веб-панель, в ядро внесены исправления. Документация
ядра — [README OpenFlux](../README.ru.md).

Распространяется по лицензии GNU GPL v3.0 или более поздней версии: см.
[LICENSE](../LICENSE), [COPYRIGHT](../COPYRIGHT) и [NOTICE](../NOTICE).
