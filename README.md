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

## Регистрация узла
1. В панели: **Узлы → Добавить** (имя, публичный адрес) → одноразовый токен (действует 1 час).
2. На сервере узла (токен через stdin, чтобы не попал в историю и `ps`):
```
echo '<token>' | docker run --rm -i -v node-data:/var/lib/d256-node d256-node:local enroll https://panel.example.com -
docker compose up -d
```
Ключ узла создаётся на самом узле и никуда не передаётся. Порт gRPC панели — `8444`. `d256-node status` показывает связь, версию конфига, состояние Xray и размер журнала.
Отзыв: в панели «Новый токен» (старый сертификат перестаёт приниматься) или удаление узла.

### Настройка
Переменные окружения (см. `.env.example`): `D256_LOG_FORMAT` (`text` или `json`; логи идут в stdout), `D256_STATE_DIR` (по умолчанию `/var/lib/d256-node`). Лимит журнала по умолчанию 256 МиБ: при 80 % старые записи сливаются по часам (трафик не теряется, теряется временное разрешение).

## Запуск (Docker)
```
cp .env.example .env
make image && docker compose up -d
```
Узел работает в сетевом режиме `host`: порты inbound'ов задаются в панели. Для портов ниже 1024 нужен `NET_BIND_SERVICE` (уже в compose).
Обновление: пересобрать образ и `docker compose up -d`. Бэкап не нужен: при потере тома узел регистрируется заново (`enroll`); недоставленный журнал при этом теряется.

## Разработка (только Docker)
Репозитории `d256-node` и `d256-panel` должны лежать рядом (общий proto-модуль).
```
./dev make verify     # линтеры + тесты + сборка + govulncheck
./dev bash
```

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
**Enroll:** *Nodes → Add* in the panel gives a one-time token (1 hour); on the node server run `echo '<token>' | docker run --rm -i -v node-data:/var/lib/d256-node d256-node:local enroll https://panel.example.com -`, then `docker compose up -d`. The key is generated on the node and never leaves it.
**Run:** `cp .env.example .env`, `make image && docker compose up -d` (host networking; inbound ports are set in the panel). Logs go to stdout (`D256_LOG_FORMAT=text|json`).
**Update:** rebuild the image and `docker compose up -d`. **Backup:** none needed — re-enroll if the volume is lost (an undelivered journal is lost with it).
**Development (Docker only):** keep `d256-node` and `d256-panel` side by side, then `./dev make verify`.

**Security and limitations.** The cache and journal are encrypted (XChaCha20-Poly1305) with a key derived from the node's private key on the same disk: protection against tampering and casual reading, not against root on the node server; there is no separate "protected mode" — use a trusted server. A crash while online may lose at most one 5 s interval of not-yet-sent usage. The device limit counts simultaneously active source IPs.
