package telemetry

import (
	"context"
	"fmt"
	"io"
	"math"
	"math/big"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// Querier runs one SQL query and returns column names and rows. PG implements it.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (cols []string, rows [][]any, err error)
}

// Section answers one question of PLAN-analytika §1.
type Section struct {
	Title  string
	Note   string
	Tables []Table
}

// Table is one query. Every query takes $1 = app id, $2 = start of the window.
type Table struct {
	Title string
	SQL   string
}

// smallBase: below this many items a share in % is noise, so only counts are shown.
const smallBase = 100

// eCTE: the app's events in the window, tester builds excluded.
const eCTE = `WITH e AS (
  SELECT session, seq, ts, name, props FROM telemetry.events
  WHERE app = $1 AND ts >= $2 AND NOT props @> '{"tester": true}'
)`

// segment: new (first session), advanced (session 6+ or day 7+), other.
const segment = `CASE WHEN props->>'session_band' IN ('6-10', '11+') OR props->>'days_band' IN ('7-29', '30+') THEN 'pokročilý'
  WHEN props->>'session_band' = '1' THEN 'nový' ELSE 'ostatní' END`

// bandOrder sorts session bands 1, 2, 3-5, 6-10, 11+ (text order would put 11+ second).
const bandOrder = `array_position(ARRAY['1', '2', '3-5', '6-10', '11+'], props->>'session_band')`

// num is a numeric property, NULL when missing or not a number (percentiles skip NULLs).
func num(key string) string {
	return fmt.Sprintf(`(CASE WHEN jsonb_typeof(props->'%[1]s') = 'number' THEN (props->>'%[1]s')::numeric END)`, key)
}

// share renders n/base in % only when base is big enough.
func share(n, base string) string {
	return fmt.Sprintf(`CASE WHEN %[2]s >= %[3]d THEN round(100.0 * %[1]s / %[2]s) || ' %%' ELSE '' END`, n, base, smallBase)
}

// Sections are the eight questions of §1, in order.
var Sections = []Section{
	{
		Title: "1. Nový uživatel: co udělá po prvním spuštění",
		Note:  "Trychtýř první relace. „Došel k 2. relaci“ počítá app_open se session_band_first v okně, ne stejné instalace.",
		Tables: []Table{
			{"Trychtýř", eCTE + `,
fst AS (SELECT DISTINCT session FROM e WHERE name = 'app_open' AND props @> '{"first_session": true}'),
fs AS (
  SELECT f.session, count(*) FILTER (WHERE e.name = 'screen_view') AS screens, bool_or(e.name = 'activation') AS activated
  FROM fst f JOIN e USING (session) GROUP BY f.session
),
steps(o, krok, n) AS (
  SELECT 1, 'první relace', count(*) FROM fs
  UNION ALL SELECT 2, 'aspoň 1 obrazovka', count(*) FILTER (WHERE screens >= 1) FROM fs
  UNION ALL SELECT 3, 'aspoň 3 obrazovky', count(*) FILTER (WHERE screens >= 3) FROM fs
  UNION ALL SELECT 4, 'aktivace v první relaci', count(*) FILTER (WHERE activated) FROM fs
  UNION ALL SELECT 5, 'došel k 2. relaci', (SELECT count(*) FROM e WHERE name = 'app_open' AND props @> '{"session_band": "2", "session_band_first": true}')
),
b AS (SELECT n FROM steps WHERE o = 1)
SELECT krok, steps.n AS "počet", ` + share("steps.n", "b.n") + ` AS "podíl" FROM steps, b ORDER BY o`},
			{"Obrazovky v první relaci", eCTE + `
SELECT props->>'screen' AS obrazovka, count(DISTINCT session) AS "relací", count(*) AS "zobrazení"
FROM e WHERE name = 'screen_view' AND props @> '{"first_session": true}'
GROUP BY 1 ORDER BY 2 DESC, 1 LIMIT 20`},
		},
	},
	{
		Title: "2. Jak dlouho je na které obrazovce",
		Tables: []Table{
			{"Čas na obrazovce (screen_leave.secs)", eCTE + `
SELECT props->>'screen' AS obrazovka, count(*) AS "odchodů",
  percentile_cont(0.5) WITHIN GROUP (ORDER BY ` + num("secs") + `) AS "medián s",
  percentile_cont(0.9) WITHIN GROUP (ORDER BY ` + num("secs") + `) AS "p90 s"
FROM e WHERE name = 'screen_leave' GROUP BY 1 ORDER BY 2 DESC, 1 LIMIT 30`},
		},
	},
	{
		Title: "3. Kdy a kde to vypne",
		Note:  "Poslední session_pause každé relace. active_secs je souhrn aktivního času relace.",
		Tables: []Table{
			{"Poslední obrazovka relace", eCTE + `,
last AS (SELECT DISTINCT ON (session) session, props FROM e WHERE name = 'session_pause' ORDER BY session, ts DESC, seq DESC NULLS LAST)
SELECT coalesce(props->>'screen', '?') AS obrazovka, count(*) AS "relací",
  percentile_cont(0.5) WITHIN GROUP (ORDER BY ` + num("active_secs") + `) AS "medián aktivních s",
  percentile_cont(0.5) WITHIN GROUP (ORDER BY ` + num("screen_secs") + `) AS "medián s na obrazovce"
FROM last GROUP BY 1 ORDER BY 2 DESC, 1 LIMIT 20`},
			{"Délka relace", eCTE + `,
s AS (SELECT session, bool_or(props @> '{"first_session": true}') AS is_first, max(` + num("active_secs") + `) AS secs
      FROM e WHERE name = 'session_pause' GROUP BY session),
b(o, pasmo, lo, hi) AS (VALUES (1, '< 10 s', 0, 10), (2, '10–60 s', 10, 60), (3, '1–5 min', 60, 300), (4, '5–15 min', 300, 900), (5, '15+ min', 900, 1000000000))
SELECT b.pasmo AS "délka", count(s.session) FILTER (WHERE s.is_first) AS "první relace", count(s.session) FILTER (WHERE NOT s.is_first) AS "ostatní"
FROM b LEFT JOIN s ON s.secs >= b.lo AND s.secs < b.hi GROUP BY b.o, b.pasmo ORDER BY b.o`},
		},
	},
	{
		Title: "4. Vrátí se",
		Note:  "Milníky z session_band_first a days_band_first: přesné počty instalací bez ID. Podíl je vůči prvním relacím ve stejném okně.",
		Tables: []Table{
			{"Milníky návratu", eCTE + `,
m(o, milnik, f) AS (VALUES
  (0, 'první relace', '{"first_session": true}'::jsonb),
  (1, '2. relace', '{"session_band": "2", "session_band_first": true}'),
  (2, '3. relace', '{"session_band": "3-5", "session_band_first": true}'),
  (3, '6. relace', '{"session_band": "6-10", "session_band_first": true}'),
  (4, '11. relace', '{"session_band": "11+", "session_band_first": true}'),
  (5, 'den 1+', '{"days_band": "1", "days_band_first": true}'),
  (6, 'den 2+', '{"days_band": "2-6", "days_band_first": true}'),
  (7, 'den 7+', '{"days_band": "7-29", "days_band_first": true}'),
  (8, 'den 30+', '{"days_band": "30+", "days_band_first": true}')),
c AS (SELECT m.o, m.milnik, count(e.session) AS n FROM m LEFT JOIN e ON e.name = 'app_open' AND e.props @> m.f GROUP BY m.o, m.milnik),
b AS (SELECT n FROM c WHERE o = 0)
SELECT milnik AS "milník", c.n AS "instalací", ` + share("c.n", "b.n") + ` AS "podíl" FROM c, b ORDER BY o`},
		},
	},
	{
		Title: "5. Pokročilý: kde bojuje",
		Note:  "Skupiny: nový = 1. relace, pokročilý = relace 6+ nebo den 7+, ostatní = zbytek.",
		Tables: []Table{
			{"Toky <x>_start / <x>_end", eCTE + `
SELECT regexp_replace(name, '_(start|end)$', '') AS tok, ` + segment + ` AS skupina,
  count(*) FILTER (WHERE name ~ '_start$') AS "začátků",
  count(*) FILTER (WHERE name ~ '_end$') AS "konců",
  count(*) FILTER (WHERE name ~ '_end$' AND props->>'result' = 'ok') AS ok,
  count(*) FILTER (WHERE name ~ '_end$' AND props->>'result' = 'cancel') AS "zrušeno",
  count(*) FILTER (WHERE name ~ '_end$' AND props->>'result' = 'error') AS "chyba",
  count(*) FILTER (WHERE name ~ '_end$' AND props->>'result' = 'background') AS "pozadí",
  count(*) FILTER (WHERE name ~ '_end$' AND coalesce(props->>'result', '') NOT IN ('ok', 'cancel', 'error', 'background')) AS "jiné",
  percentile_cont(0.5) WITHIN GROUP (ORDER BY ` + num("secs") + `) FILTER (WHERE name ~ '_end$') AS "medián s"
FROM e WHERE name ~ '_(start|end)$'
GROUP BY 1, 2 ORDER BY count(*) FILTER (WHERE name ~ '_end$' AND props->>'result' IN ('cancel', 'error')) DESC, 1, 2 LIMIT 40`},
			{"Chyby (error_shown)", eCTE + `
SELECT props->>'kind' AS druh, props->>'screen' AS obrazovka, ` + segment + ` AS skupina, count(*) AS "počet", count(DISTINCT session) AS "relací"
FROM e WHERE name = 'error_shown' GROUP BY 1, 2, 3 ORDER BY 4 DESC, 1, 2, 3 LIMIT 30`},
			{"Klepnutí na nedostupné (locked_tap)", eCTE + `
SELECT props->>'target' AS cil, ` + segment + ` AS skupina, count(*) AS "počet", count(DISTINCT session) AS "relací"
FROM e WHERE name = 'locked_tap' GROUP BY 1, 2 ORDER BY 3 DESC, 1, 2 LIMIT 30`},
			{"Dlouho bez akce (screen_leave, actions = 0, 30 s a víc)", eCTE + `
SELECT props->>'screen' AS obrazovka, count(*) AS "pobytů",
  percentile_cont(0.5) WITHIN GROUP (ORDER BY ` + num("secs") + `) AS "medián s"
FROM e WHERE name = 'screen_leave' AND props @> '{"actions": 0}' AND ` + num("secs") + ` >= 30
GROUP BY 1 ORDER BY 2 DESC, 1 LIMIT 20`},
			{"Přeskakování A → B → A", eCTE + `,
v AS (
  SELECT session, props->>'screen' AS s,
    lag(props->>'screen', 1) OVER w AS p1, lag(props->>'screen', 2) OVER w AS p2
  FROM e WHERE name = 'screen_view'
  WINDOW w AS (PARTITION BY session ORDER BY ts, seq)
)
SELECT p2 || ' → ' || p1 || ' → ' || s AS cesta, count(*) AS "počet", count(DISTINCT session) AS "relací"
FROM v WHERE p2 = s AND p1 <> s GROUP BY 1 ORDER BY 2 DESC, 1 LIMIT 15`},
		},
	},
	{
		Title: "6. Pokročilý: co nepoužívá",
		Note:  "feature_snapshot se posílá při 3., 6. a 11. relaci; f_<funkce> říká, jestli zařízení funkci kdy použilo.",
		Tables: []Table{
			{"Snímky funkcí", eCTE + `
SELECT e.props->>'session_band' AS "pásmo", substr(j.k, 3) AS funkce,
  count(*) FILTER (WHERE j.v = 'true'::jsonb) AS "použilo", count(*) AS "snímků",
  ` + share("count(*) FILTER (WHERE j.v = 'true'::jsonb)", "count(*)") + ` AS "podíl"
FROM e, jsonb_each(e.props) AS j(k, v)
WHERE e.name = 'feature_snapshot' AND left(j.k, 2) = 'f_'
GROUP BY 1, 2 ORDER BY array_position(ARRAY['1', '2', '3-5', '6-10', '11+'], e.props->>'session_band'), 3, 2`},
			{"První použití v relaci (feature_use)", eCTE + `
SELECT props->>'feature' AS funkce, ` + segment + ` AS skupina, count(DISTINCT session) AS "relací"
FROM e WHERE name = 'feature_use' GROUP BY 1, 2 ORDER BY 1, 2`},
		},
	},
	{
		Title: "7. Kolik času a kde tráví (relace 6–10 a 11+)",
		Tables: []Table{
			{"Obrazovky v pásmech 6–10 a 11+", eCTE + `
SELECT props->>'session_band' AS "pásmo", props->>'screen' AS obrazovka, count(*) AS "odchodů",
  round(sum(` + num("secs") + `) / 60.0, 1) AS "celkem min",
  percentile_cont(0.5) WITHIN GROUP (ORDER BY ` + num("secs") + `) AS "medián s"
FROM e WHERE name = 'screen_leave' AND props->>'session_band' IN ('6-10', '11+')
GROUP BY 1, 2 ORDER BY ` + bandOrder + `, 4 DESC NULLS LAST, 2 LIMIT 40`},
			{"Aktivní čas relace podle pásma", eCTE + `,
s AS (SELECT session, max(props->>'session_band') AS band, max(` + num("active_secs") + `) AS secs FROM e WHERE name = 'session_pause' GROUP BY session)
SELECT s.band AS "pásmo", count(*) AS "relací",
  round(percentile_cont(0.5) WITHIN GROUP (ORDER BY s.secs)::numeric / 60.0, 1) AS "medián min",
  round(percentile_cont(0.9) WITHIN GROUP (ORDER BY s.secs)::numeric / 60.0, 1) AS "p90 min",
  round(sum(s.secs) / 3600.0, 1) AS "celkem h"
FROM s GROUP BY s.band ORDER BY array_position(ARRAY['1', '2', '3-5', '6-10', '11+'], s.band) NULLS LAST`},
		},
	},
	{
		Title: "8. Kolik stažení se promění ve spuštění",
		Note:  "Stažení z obchodů (Apple Sales `sales`, Play `installs`; Play má zpoždění několik dní) proti prvním spuštěním z měření (first_open, bez testerů). Apple zahrnuje i macOS.",
		Tables: []Table{
			{"Obchod → appka", `WITH d AS (
  SELECT CASE WHEN store = 'apple' THEN 'ios' ELSE 'android' END AS platforma, sum(downloads) AS n
  FROM metrics_daily WHERE app_id = $1 AND date >= $2::date AND ((store = 'apple' AND kind = 'sales') OR (store = 'google' AND kind = 'installs'))
  GROUP BY 1
), f AS (
  SELECT CASE WHEN platform = 'macos' THEN 'ios' ELSE platform END AS platforma, sum(count) AS n
  FROM first_open WHERE app_id = $1 AND date >= $2::date GROUP BY 1
)
SELECT platforma, coalesce(d.n, 0) AS "stažení", coalesce(f.n, 0) AS "první spuštění",
  ` + share("coalesce(f.n, 0)", "coalesce(d.n, 0)") + ` AS "podíl"
FROM d FULL JOIN f USING (platforma) ORDER BY 1`},
			{"První spuštění podle kampaně (QR)", `SELECT CASE WHEN campaign = '' THEN '(bez kampaně)' ELSE campaign END AS kampan, platform AS platforma, sum(count) AS "první spuštění"
FROM first_open WHERE app_id = $1 AND date >= $2::date GROUP BY 1, 2 ORDER BY 3 DESC, 1, 2`},
		},
	},
}

// Report writes the Markdown answer to §1 for app since `since`. now only labels the window.
func Report(ctx context.Context, q Querier, w io.Writer, app string, since, now time.Time) error {
	fmt.Fprintf(w, "# Měření: %s\n\nOkno: %s až %s (UTC). Testerské buildy vynechané. Pod %d položek jen počty, bez procent.\n\n",
		app, since.UTC().Format("2006-01-02 15:04"), now.UTC().Format("2006-01-02 15:04"), smallBase)
	cols, rows, err := q.Query(ctx, eCTE+`
SELECT count(*) AS "událostí", count(DISTINCT session) AS "relací", count(DISTINCT props->>'app_version') AS "verzí",
  min(ts)::date::text AS "první", max(ts)::date::text AS "poslední" FROM e`, app, since)
	if err != nil {
		return fmt.Errorf("summary: %w", err)
	}
	Markdown(w, cols, rows)
	for _, s := range Sections {
		fmt.Fprintf(w, "\n## %s\n\n", s.Title)
		if s.Note != "" {
			fmt.Fprintf(w, "%s\n\n", s.Note)
		}
		for _, t := range s.Tables {
			cols, rows, err := q.Query(ctx, t.SQL, app, since)
			if err != nil {
				return fmt.Errorf("%s / %s: %w", s.Title, t.Title, err)
			}
			fmt.Fprintf(w, "### %s\n\n", t.Title)
			Markdown(w, cols, rows)
			fmt.Fprintln(w)
		}
	}
	return nil
}

// Markdown writes one table. No rows prints a line saying so.
func Markdown(w io.Writer, cols []string, rows [][]any) {
	if len(rows) == 0 {
		fmt.Fprintln(w, "_žádná data_")
		return
	}
	fmt.Fprintf(w, "| %s |\n", strings.Join(cols, " | "))
	sep := make([]string, len(cols))
	for i := range sep {
		sep[i] = "---"
	}
	fmt.Fprintf(w, "| %s |\n", strings.Join(sep, " | "))
	for _, r := range rows {
		cells := make([]string, len(r))
		for i, v := range r {
			cells[i] = strings.ReplaceAll(Format(v), "|", `\|`)
		}
		fmt.Fprintf(w, "| %s |\n", strings.Join(cells, " | "))
	}
}

// Format renders one database value for a report cell.
func Format(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		if x {
			return "ano"
		}
		return "ne"
	case float64:
		return formatFloat(x)
	case float32:
		return formatFloat(float64(x))
	case pgtype.Numeric:
		if !x.Valid || x.NaN {
			return ""
		}
		f, err := x.Float64Value()
		if err != nil || !f.Valid {
			return ""
		}
		return formatFloat(f.Float64)
	case *big.Int:
		return x.String()
	case time.Time:
		return x.UTC().Format("2006-01-02")
	default:
		return fmt.Sprint(x)
	}
}

func formatFloat(f float64) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return ""
	}
	if f == math.Trunc(f) && math.Abs(f) < 1e15 {
		return fmt.Sprintf("%d", int64(f))
	}
	return fmt.Sprintf("%.1f", f)
}

// ParseSince reads --since: "7d", "36h" or a date "2026-11-01". The window starts that long before now.
func ParseSince(s string, now time.Time) (time.Time, error) {
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	if strings.HasSuffix(s, "d") {
		var n int
		if _, err := fmt.Sscanf(s, "%dd", &n); err == nil && n > 0 {
			return now.AddDate(0, 0, -n), nil
		}
		return time.Time{}, fmt.Errorf("bad --since %q", s)
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return time.Time{}, fmt.Errorf("bad --since %q (use 7d, 36h or 2026-11-01)", s)
	}
	return now.Add(-d), nil
}
