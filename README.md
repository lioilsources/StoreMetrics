# store-metrics

Denní collector metrik appek (App Store + Google Play) do Postgresu na domácím serveru. Jedna Go binárka, spouštěná user systemd timerem, plus CSV export pro dashboard.

Vedle toho příjemce anonymního měření z appek (`telemetry-ingest`, sekce [Měření](#měření)): události jdou do téže databáze, takže jeden dotaz spojí stažení z obchodu s prvním spuštěním.

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

Které appky se načítají, určuje `apps.yaml` (Apple SKU a Play package name). Appka, která ještě není v obchodě, tam je jen s `id` a `name`: collector pro ni nic nestahuje, ale id slouží pro klíče měření. Store identifikátory se doplní po vydání.

## Vývoj a nasazení

```
make test                     # go vet + go test
make deploy HOST=<ssh host>   # build pro linux/amd64, rsync obou binárek, apps.yaml, compose, Caddyfile a unitů
```

Když už běží služby měření, `make deploy` je po nahrání restartuje (`--no-deps`, databáze zůstane, jak je).

`.env` a `secrets/` se nasazují ručně jednou (vzor `deploy/env.example`), `make deploy` je nikdy nepřepisuje.

## Ruční běh na serveru

```
cd ~/deploy/store-metrics && set -a && . ./.env && set +a && ./bin/store-metrics [-only apple|google]
journalctl --user -u store-metrics -n 50
```

## Měření

Appky posílají anonymní události (protokol v1) na vlastní adresu. Cesta:

```
appka ─ HTTPS ─► Cloudflare ─ tunel ─► cloudflared ─► caddy:80 ─► ingest:8080 ─► db (schéma telemetry)
```

Všechno běží v `docker-compose.yml` pod profilem `telemetry`, bez portů na hostu. Zásady: žádná IP (ani v DB, ani v lozích), žádný trvalý identifikátor, žádný volný text. ID relace je náhodné a žije jen v paměti appky.

### Protokol v1

Specifikace je v repu klientského balíčku [`lioilsources/Ol1nTelemetry`](https://github.com/lioilsources/Ol1nTelemetry), `docs/protocol.md`. Tady jen to, co příjemce vynucuje (`internal/telemetry/protocol.go`):

```
POST /v1/batch
Content-Type: application/json
X-Telemetry-Key: <klíč appky>

{"v":1,"app":"lexify","events":[{"n":"app_open","t":"2026-11-14T10:00:00.000Z","s":"<32 hex>","i":1,"p":{…}}]}
```

| Pravidlo | Hodnota |
|---|---|
| Tělo | do 64 kB (Caddy i Go) |
| `app` | `^[a-z][a-z0-9-]{1,31}$`, id z `apps.yaml`, musí sedět s klíčem |
| Události v dávce | 1 až 100 (prázdné `events` je 400) |
| Název `n` a klíče vlastností | `^[a-z][a-z0-9_]{1,39}$` |
| Relace `s` | 32 hex znaků |
| Pořadí `i` | nepovinné, celé číslo 1 až 10⁹; s ním se duplicita `(app, s, i)` přeskočí (i uvnitř jedné dávky), bez něj se nekontroluje |
| Vlastnosti `p` | povinný objekt, do 32 položek; hodnota je `true`/`false`, celé číslo 0 až 10⁹ nebo řetězec `^[A-Za-z0-9_.+-]{1,40}$` |
| Čas `t` | RFC 3339 v UTC (`…Z`); víc než 10 minut v budoucnosti se nahradí časem příjmu, starší než 30 dní se zahodí |
| `campaign` u `app_open` | musí být v `qr_placements`, jinak se uloží `other` |
| Neznámá pole obálky a události | ignorují se (dopředná kompatibilita) |

Odpovědi:

| Kód | Kdy | Klient |
|---|---|---|
| 204 | přijato (i když se jednotlivé vadné události zahodily) | hotovo |
| 400 | nečitelný JSON, špatné `v` nebo `app`, `events` chybí, je prázdné nebo má víc než 100 položek, `Content-Type` není JSON | zahodí dávku |
| 401 | chybí klíč, neznámý nebo zneplatněný klíč, `app` nesedí s klíčem | zahodí dávku |
| 413 | tělo nad 64 kB | zahodí dávku |
| 429 | omezení četnosti na IP (v paměti, 60/min) nebo denní strop appky (200 000 událostí) | zkusí později |
| 503 | databáze nedostupná | zkusí později |

Vadná událost uvnitř jinak platné dávky (volný text, objekt, desetinné číslo, špatný název, starý čas) se zahodí sama, zbytek dávky se uloží. Kolik čeho přišlo a proč se odmítlo, je po dnech v `telemetry.ingest_stats`.

Testovací dávky: `internal/telemetry/testdata/protocol/` je kopie `testdata/` z Ol1nTelemetry v0.1.0 včetně `cases.json` s očekávanými výsledky, test `TestProtocolCases` je proti příjemci ověřuje. Při změně protokolu se kopie obnoví z balíčku. Vlastní dávky příjemce (cizí klíč, volný text, posun času, duplicity, kampaň) jsou o úroveň výš. Testy nesahají na síť ani na databázi (úložiště je za rozhraním `Store`).

### Data

Schéma `telemetry` v databázi `store_metrics` (`internal/db/telemetry.sql`, idempotentní, spouští se při každém startu stejně jako `schema.sql`):

| Tabulka | Obsah |
|---|---|
| `telemetry.apps` | id appky, otisk klíče (SHA-256), ukládat zemi ano/ne, doba uchování (výchozí 365 dní) |
| `telemetry.events` | app, čas příjmu, čas události, den, relace, pořadí, název, země (`CF-IPCountry`), `props jsonb` |
| `telemetry.ingest_stats` | denní počty požadavků a událostí podle výsledku (`accepted`, `duplicate`, `bad_props`, `bad_key`, …) |
| `telemetry.events_daily` | denní agregáty smazaných událostí (název, platforma, verze, země, tester) bez ID relace; zůstávají napořád |

`app_open` s `first_session: true` (ne od testerů) přičte jedničku do `first_open` (den, appka, platforma, kampaň).

Uchování: `ingest` jednou denně smaže syrové události starší než `retention_days` appky a sečte je do `events_daily`. Ručně: `store-metrics telemetry prune`. Změna uchování nebo vypnutí země pro appku: `UPDATE telemetry.apps SET retention_days = …, store_country = false WHERE id = '…'`.

### Klíče

Klíč odděluje appky, drží forky a lokální buildy tiché a jde zneplatnit. Není to silné tajemství (je v binárce appky). V databázi je jen jeho otisk; `app` musí být id z `apps.yaml`.

```
./bin/store-metrics telemetry key new <app>      # vypíše klíč jednou na stdout; existující klíč nahradí
./bin/store-metrics telemetry key revoke <app>   # všechny vydané verze přestanou posílat do minuty
```

Klíč jde do GitHubu jen rourou, nikdy do chatu ani do souboru (viz níže).

### Report

```
./bin/store-metrics report <app> [--since 7d|36h|2026-11-01]
```

Vypíše Markdown tabulky pro osm otázek plánu měření: trychtýř první relace, čas na obrazovkách (medián, p90), kde a po jak dlouhé době relace končí, milníky návratu (2., 3., 6., 11. relace; den 1+, 2+, 7+, 30+), toky `<x>_start`/`<x>_end`, chyby, klepnutí na nedostupné, dlouhé pobyty bez akce a přeskakování A → B → A, přijetí funkcí (`feature_snapshot`, `feature_use`), čas v pásmech relace 6–10 a 11+, stažení z obchodů proti prvním spuštěním a první spuštění podle kampaně. Testerské buildy (`tester: true`) jsou venku. Pod 100 položek ukazuje jen počty, ne procenta. SQL je v `internal/telemetry/report.go`.

Ze stroje s přístupem:

```
ssh <server> 'cd ~/deploy/store-metrics && set -a && . ./.env && set +a && ./bin/store-metrics report lexify --since 7d'
```

### Zapnutí (jednou, ručně)

1. **`.env` na serveru doplnit o `DB_DIR` a `DB_BIND`** s hodnotami, které dnes stojí natvrdo v nasazeném `docker-compose.yml` (datový adresář a adresa portu 5441). Repo verze compose je čte z `.env`; bez nich by po `make deploy` další `docker compose up` založil prázdnou databázi v `./db`.
2. **Tunel a DNS (Cloudflare Zero Trust):** nový tunel pro měření, public hostname na vlastní doméně, služba `http://caddy:80`. Token tunelu do `.env` jako `TELEMETRY_TUNNEL_TOKEN`.
3. **`.env`:** `COMPOSE_PROFILES=telemetry` (případně `TELEMETRY_RATE_PER_MIN`, `TELEMETRY_DAILY_CAP`). `POSTGRES_PASSWORD` musí být bez znaků, které se v URL escapují.
4. **Nasazení:** `make deploy HOST=<server>`, pak na serveru `cd ~/deploy/store-metrics && docker compose up -d --no-deps ingest caddy cloudflared`.
5. **Kontrola:** `docker compose ps` (ingest `healthy`), `curl -fsS https://<adresa>/healthz` vrátí `ok`, `docker stats --no-stream` u tří nových kontejnerů dohromady pod 120 MB.
6. **Klíč appky do GitHubu** (až po zveřejnění nové politiky soukromí appky a vyplnění formulářů v obchodech):

   ```
   ssh <server> 'cd ~/deploy/store-metrics && set -a && . ./.env && set +a && ./bin/store-metrics telemetry key new <app>' \
     | gh secret set TELEMETRY_KEY -R lioilsources/<repo appky>
   printf '%s' 'https://<adresa>' | gh secret set TELEMETRY_HOST -R lioilsources/<repo appky>
   ```

7. **Zkouška:** dávka s klíčem testovací appky dá 204 a je v `telemetry.events`; špatný klíč 401, špatný tvar 400; `SELECT count(*) FROM telemetry.events WHERE props::text ~ '\d+\.\d+\.\d+\.\d+'` je 0.

Rozpočet paměti (limity v compose): ingest 40 MB (`GOMEMLIMIT` 28 MiB), caddy 32 MB, cloudflared 48 MB, celkem 120 MB. Podobné dvojice caddy + cloudflared na serveru dnes berou kolem 8–10 a 13–16 MB.

Vypnutí: `store-metrics telemetry key revoke <app>` pro jednu appku, `docker compose stop ingest caddy cloudflared` pro všechno (klienti drží frontu a pošlou později).
