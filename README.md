# d256 node

**RU** · [EN](#english)

Консольный узел для [d256-panel](https://github.com/dest1256/d256-panel). Запускает Xray-core (как Go-библиотеку), применяет конфиг от панели,
отправляет статистику. GUI нет. Основан на идеях [3x-ui](https://github.com/MHSanaei/3x-ui) (MHSanaei и контрибьюторы), изменения © dest1256, GPL-3.0.

> Статус: разработка (этап 5 из 8): Xray встроен; без панели узел работает из зашифрованного кэша, пишет журнал трафика на диск и сам применяет лимиты клиентов. Архитектура: [d256-panel/docs/ARCHITECTURE.md](https://github.com/dest1256/d256-panel/blob/main/docs/ARCHITECTURE.md).

## Команды
`run`, `enroll <panel-url> <token>`, `status`, `logs`, `reload`, `reset`, `version`.

## Регистрация узла
1. В панели: **Узлы → Добавить** (имя, публичный адрес) → панель покажет одноразовый токен (1 час).
2. На сервере узла (токен через stdin, чтобы не попал в историю и `ps`):
```
echo '<token>' | docker run --rm -i -v node-data:/var/lib/d256-node d256-node:local enroll https://panel.example.com -
docker compose up -d
```
Ключ узла создаётся на самом узле и никуда не передаётся. `d256-node status` показывает состояние связи.
Отзыв: в панели «Новый токен» (старый сертификат перестаёт приниматься) или удаление узла.

## Разработка (только Docker)
Репозитории `d256-node` и `d256-panel` должны лежать рядом (общий proto-модуль).
```
./dev make verify     # линтеры + тесты + сборка + govulncheck
./dev bash
```

## Запуск (Docker)
```
cp .env.example .env
make image && docker compose up -d
```
Обновление: пересобрать образ и `docker compose up -d`. Бэкап: том `node-data` не нужен для восстановления — узел можно заново зарегистрировать (`enroll`).

---

## English
Console node for [d256-panel](https://github.com/dest1256/d256-panel). Runs Xray-core as a Go library, applies the panel's config and reports statistics. No GUI.
Derived from ideas of [3x-ui](https://github.com/MHSanaei/3x-ui); changes © dest1256; GPL-3.0.

> Status: work in progress (stage 5 of 8): Xray is embedded; without the panel the node runs from an encrypted cache, journals usage on disk and enforces client limits itself.

**Commands:** `run`, `enroll <panel-url> <token>`, `status`, `logs`, `reload`, `reset`, `version`.
**Development (Docker only):** keep `d256-node` and `d256-panel` side by side, then `./dev make verify`.
**Run:** `cp .env.example .env`, `make image && docker compose up -d`.
**Update:** rebuild the image and `docker compose up -d`. **Backup:** nothing essential — re-enroll the node if the volume is lost.
