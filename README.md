# store-metrics

Denní collector metrik appek (App Store + Google Play) do Postgresu na domácím serveru. Jedna Go binárka, spouštěná user systemd timerem, plus CSV export pro dashboard.

## Jak běží

- Binárka `~/deploy/store-metrics/bin/store-metrics`, user systemd timer `store-metrics.timer` jednou denně (Apple vydává předchozí den ráno, Play s několikadenním zpožděním).
- Postgres `store-metrics-db` (compose v `~/deploy/store-metrics`), adresa a datový adresář přes `DB_BIND` a `DB_DIR` v `.env`, DB `store_metrics`.
- Tajemství jen na serveru: `.env` (600) a `secrets/` (700): App Store Connect API klíč (role Finance), Play service account JSON.
- Raw reporty v `RAW_DIR`, export pro dashboard v `EXPORT_CSV` (posledních 6 měsíců).
- Databáze jde kdykoli znovu postavit z raw reportů a API: smazat obsah `DB_DIR`, `docker compose up -d`, spustit collector.
- Každý běh se zapisuje do tabulky `collector_runs`.

## Zdroje

| Store | Report | kind | Poznámka |
|---|---|---|---|
| Apple | Sales SUMMARY DAILY | `sales` | stažení, opakovaná stažení, aktualizace, IAP + výnos; den, který už je načtený, se nestahuje znovu |
| Google | `stats/installs/*_country.csv` | `installs` | uživatelské instalace/odinstalace, aktivní zařízení |
| Google | `stats/store_performance/*_country.csv` | `store_performance` | návštěvy listingu (`page_views`), akvizice (`downloads`) |
| Google | `stats/store_performance/*_traffic_source.csv` | `traffic` | totéž po zdroji a UTM kampani (QR) |

`downloads` se nesčítá přes různé `kind` (u Play znamená jednou instalace, jednou akvizici z listingu).

Které appky se načítají, určuje `apps.yaml` (Apple SKU a Play package name).

## Vývoj a nasazení

```
make test
make deploy HOST=<ssh host>   # build pro linux/amd64, rsync binárky, apps.yaml, compose a unitů
```

`.env` a `secrets/` se nasazují ručně jednou (vzor `deploy/env.example`), `make deploy` je nikdy nepřepisuje.

## Ruční běh na serveru

```
cd ~/deploy/store-metrics && set -a && . ./.env && set +a && ./bin/store-metrics [-only apple|google]
journalctl --user -u store-metrics -n 50
```
