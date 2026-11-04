# d256 node

**RU** · [EN](#english)

Консольный узел для [d256-panel](https://github.com/dest1256/d256-panel). Запускает Xray-core (как Go-библиотеку), применяет конфиг от панели,
считает трафик и подключения, отправляет статистику. GUI нет. Основан на идеях [3x-ui](https://github.com/MHSanaei/3x-ui) (MHSanaei и контрибьюторы), изменения © dest1256, GPL-3.0 (см. [LICENSE](LICENSE), [NOTICE](NOTICE)).
Архитектура: [d256-panel/docs/ARCHITECTURE.md](https://github.com/dest1256/d256-panel/blob/main/docs/ARCHITECTURE.md).

## Что делает
- Сам подключается к панели (исходящее gRPC + mTLS), держит двунаправленный стрим, шлёт метрики сервера и состояние Xray.
- Применяет желаемое состояние атомарно, с откатом при ошибке; пользователей и inbound'ы меняет без перезапуска Xray.
- Для каждого соединения знает клиента, inbound, IP источника, домен/IP назначения, порт и байты (обёртка над диспетчером xray-core, без патчей) и шлёт список активных соединений и почасовые агрегаты.
- **Офлайн:** без панели работает из зашифрованного кэша конфига, пишет журнал трафика на диск (только пока связи нет), сам применяет лимиты клиентов (трафик, срок, число устройств). После возврата связи досылает журнал пачками; панель принимает каждую пачку один раз. Приоритет у состояния панели.

## Команды
`run`, `enroll <panel-url> <token>`, `status`, `logs`, `reload`, `reset`, `version`.

## Установка и регистрация
Нужен только Docker. Клонировать репозитории не нужно: узел собирается с GitHub одним compose-файлом.
1. В панели: **Узлы → Добавить** (имя, публичный адрес) → одноразовый токен (действует 1 час).
2. На сервере узла:
```
mkdir node && cd node
curl -O https://raw.githubusercontent.com/dest1256/d256-node/main/docker-compose.yml
echo '<токен>' | docker compose run --rm -T d256-node enroll https://panel.example.com -
docker compose up -d --build
docker compose exec d256-node d256-node status
```
Рядом с compose-файлом появится папка `data` (ключ и сертификат узла, зашифрованный кэш, офлайн-журнал). Ключ создаётся на самом узле и никуда не передаётся. Порт gRPC панели — `8444` (должен быть открыт на панели), порты inbound'ов узла задаются в панели и открываются в файрволе узла (узел работает в сетевом режиме `host`).
Обновление: `docker compose up -d --build` (подтянет свежий код; конкретная версия — `D256_NODE_SRC=https://github.com/dest1256/d256-node.git#v1.0.0` и `D256_PANEL_SRC=...d256-panel.git#v1.0.0` в `.env`).
Бэкап не нужен: при потере папки `data` узел регистрируется заново; недоставленный журнал при этом теряется.
Отзыв: в панели «Новый токен» (старый сертификат перестаёт приниматься) или удаление узла.

### Настройка
Переменные окружения (`.env`, см. `.env.example`): `D256_LOG_FORMAT` (`text` или `json`; логи идут в stdout), `TZ`. Лимит журнала по умолчанию 256 МиБ: при 80 % старые записи сливаются по часам (трафик не теряется, теряется временное разрешение).

## Разработка (только Docker)
Для разработки репозитории `d256-node` и `d256-panel` должны лежать рядом (общий proto-модуль); образ — `make image`.
```
./dev make verify     # линтеры + тесты + сборка + govulncheck
./dev bash
```

## Обновление узла из панели (в один клик)
Узел умеет обновлять сам себя (вместе со встроенным Xray) по команде из панели: на странице «Узлы» рядом с версией появляется метка «Обновление vX.Y.Z», а в меню действий — «Обновить узел». Узел сам скачивает подписанный релиз, проверяет подпись, ставит его в `data/bin` и перезапускается; клиенты отключаются на несколько секунд, учёт сохраняется в журнале.

**Разовая настройка (владелец репозитория):**
1. Создайте ключ подписи релизов: `./dev go run ./cmd/d256-sign keygen`. Публичный ключ запишется в `internal/update/signing.pub` — закоммитьте его. Приватный ключ команда выведет на экран: сохраните его как секрет репозитория GitHub `D256_SIGNING_KEY` (Settings → Secrets and variables → Actions) и нигде больше не храните.
2. **Один раз пересоберите все узлы** (`docker compose up -d --build`) уже с этим публичным ключом: только такие узлы смогут проверять обновления. Образ из ветки `main` показывает версию `dev`.

**Выпуск релиза:** `git tag v1.0.1 && git push origin v1.0.1`. Workflow `Release` соберёт статические бинарники (linux amd64 и arm64), подпишет их и опубликует в GitHub Releases. Через 30 минут (или кнопкой обновления списка) панель увидит новую версию.

**Как это защищено.** Панель присылает узлу только номер версии (строго `vX.Y.Z`). Адрес загрузки узел строит сам, и он всегда ведёт в репозиторий релизов. Бинарник принимается, только если подпись Ed25519 (она покрывает версию, платформу и хеш файла) проверена вшитым в узел публичным ключом; затем он один раз запускается (`version`) и должен назвать ожидаемую версию. Новая версия, которая не сумела подключиться к панели за 3 запуска, откатывается к бинарнику из образа. Сервер с релизом без вашей подписи обновить узлы не может.
Geo-файлы обновляются только пересборкой образа.

## Безопасность и ограничения
- Кэш и журнал зашифрованы (XChaCha20-Poly1305), но ключ выводится из приватного ключа узла, лежащего на том же диске. Это защита от подмены и случайного чтения, **не** от root на сервере узла. Отдельного «защищённого режима» нет: ставьте узел на доверенный сервер.
- Токен регистрации не пишется в логи; его удобнее передавать через stdin.
- Аварийное завершение при живой связи может потерять не более одного интервала (5 с) ещё не отправленного учёта.
- Лимит устройств считает одновременно активные IP источника.

---

## English
Console node for [d256-panel](https://github.com/dest1256/d256-panel). Runs Xray-core as a Go library, applies the panel's config, accounts traffic and connections and reports statistics. No GUI.
Derived from ideas of [3x-ui](https://github.com/MHSanaei/3x-ui); changes © dest1256; GPL-3.0 (see [LICENSE](LICENSE), [NOTICE](NOTICE)).

**What it does.** Dials the panel (outbound gRPC + mTLS), reports host metrics and Xray state; applies the desired state atomically with rollback and changes users/inbounds without restarting Xray; knows client, inbound, source IP, destination domain/IP, port and bytes per connection (wrapper over the xray-core dispatcher, no patches) and reports live connections plus hourly aggregates. **Offline:** runs from an encrypted config cache, journals usage on disk (only while disconnected), enforces client limits itself (quota, expiry, device count), then redelivers the journal; the panel accepts each batch exactly once and its state wins.

**Commands:** `run`, `enroll <panel-url> <token>`, `status`, `logs`, `reload`, `reset`, `version`.
**Install and enroll** (Docker only, no clone): *Nodes → Add* in the panel gives a one-time token (1 hour); on the node server `mkdir node && cd node`, `curl -O https://raw.githubusercontent.com/dest1256/d256-node/main/docker-compose.yml`, `echo '<token>' | docker compose run --rm -T d256-node enroll https://panel.example.com -`, `docker compose up -d --build`. The compose file builds the node and the panel's proto module from GitHub and keeps the state in `./data` (key, certificate, sealed cache, journal); the key is generated on the node and never leaves it. Host networking: open the inbound ports you configure in the panel; the panel's gRPC port is `8444`. Logs go to stdout (`D256_LOG_FORMAT=text|json`).
**Update:** `docker compose up -d --build` (pin versions with `D256_NODE_SRC` / `D256_PANEL_SRC`, e.g. `https://github.com/dest1256/d256-node.git#v1.0.0`). **Backup:** none needed — re-enroll if `data` is lost (an undelivered journal is lost with it).
**Development (Docker only):** keep `d256-node` and `d256-panel` side by side, then `./dev make verify`.

**Updating nodes from the panel (one click).** A node can replace its own binary (including the embedded Xray) when the panel asks: the Nodes page shows an "Update vX.Y.Z" tag and an "Update node" action. The node downloads the signed release, verifies it, installs it under `data/bin` and restarts; clients reconnect after a few seconds and usage is kept in the journal. *One-time setup (repository owner):* run `./dev go run ./cmd/d256-sign keygen`, commit the public key written to `internal/update/signing.pub`, store the printed private key as the GitHub secret `D256_SIGNING_KEY`, and rebuild every node once (`docker compose up -d --build`) so it carries the public key. *Release:* `git tag v1.0.1 && git push origin v1.0.1` (the `Release` workflow builds linux amd64/arm64, signs and publishes). *Safety:* the panel only sends a version string; the node builds the download URL itself, accepts a binary only with a valid Ed25519 signature (covering version, platform and hash) from the key built into it, test-runs it, and falls back to the image's binary if the new one fails to reach the panel in 3 starts. Geo files are updated by rebuilding the image.

**Security and limitations.** The cache and journal are encrypted (XChaCha20-Poly1305) with a key derived from the node's private key on the same disk: protection against tampering and casual reading, not against root on the node server; there is no separate "protected mode" — use a trusted server. A crash while online may lose at most one 5 s interval of not-yet-sent usage. The device limit counts simultaneously active source IPs.
