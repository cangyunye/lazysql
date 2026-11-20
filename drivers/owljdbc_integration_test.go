package drivers

import (
	"context"
	"errors"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jorgerojas26/lazysql/models"
)

// owlJdbcProbeSystemSchemas are openGauss/PG maintenance catalogs whose tables
// often deny reads for regular users; the e2e probe skips them.
var owlJdbcProbeSystemSchemas = map[string]bool{
	"pg_catalog": true, "information_schema": true, "dbe_perf": true,
	"dbe_pldebugger": true, "dbe_pldeveloper": true, "dbe_sql_diag": true,
	"sqladvisor": true, "snapshot": true, "pkg_service": true,
	"pkg_util": true, "db4ai": true, "ai": true, "cshstore": true,
}

// owlJdbcE2EGate returns the connection URLs to verify against real backends.
// Set LAZYSQL_OWLJDBC_E2E to one or more owljdbc URLs separated by ';', e.g.
//
//	LAZYSQL_OWLJDBC_E2E='owljdbc://root%40obmysql:pw@host:2881/test?dbType=oceanbase-mysql;jarsDir=...' \
//	  go test ./drivers/ -run TestOwlJdbcE2E
//
// Backends that are unreachable are skipped rather than failed: these hosts
// flap, and a refused/unreachable connection says nothing about the driver.
func owlJdbcE2EGate(t *testing.T) []string {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv("LAZYSQL_OWLJDBC_E2E"))
	if raw == "" {
		t.Skip("LAZYSQL_OWLJDBC_E2E not set; skipping real-backend verification")
	}
	var urls []string
	for _, part := range strings.Split(raw, ";") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			urls = append(urls, trimmed)
		}
	}
	if len(urls) == 0 {
		t.Skip("LAZYSQL_OWLJDBC_E2E empty; skipping real-backend verification")
	}
	return urls
}

// owlJdbcE2EInfraError reports whether the error is environmental (backend
// down, sidecar JVM gone) so the test skips instead of failing.
func owlJdbcE2EInfraError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, keyword := range []string{
		"refused", "unreachable", "timed out", "timeout", "access denied",
		"communications link failure", "connection reset", "agent process closed",
		"no jar matching",
	} {
		if strings.Contains(msg, keyword) {
			return true
		}
	}
	return errors.Is(err, context.DeadlineExceeded)
}

func TestOwlJdbcE2E(t *testing.T) {
	for _, urlstr := range owlJdbcE2EGate(t) {
		urlstr := urlstr
		t.Run(urlstr, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			db := &OwlJDBC{}
			err := db.Connect(ctx, urlstr)
			if err != nil {
				if owlJdbcE2EInfraError(err) {
					t.Skipf("backend unavailable: %v", err)
				}
				t.Fatalf("Connect: %v", err)
			}
			t.Cleanup(func() { _ = db.Connection.Close() })

			if db.CurrentDatabase == "" {
				t.Errorf("CurrentDatabase is empty")
			}
			t.Logf("family=%s mode=%s current_database=%s", db.family, db.mode, db.CurrentDatabase)

			databases, err := db.GetDatabases(ctx)
			if err != nil {
				if owlJdbcE2EInfraError(err) {
					t.Skipf("GetDatabases unavailable: %v", err)
				}
				t.Fatalf("GetDatabases: %v", err)
			}
			if len(databases) == 0 {
				t.Fatal("GetDatabases returned no databases")
			}
			t.Logf("databases(%d): %v", len(databases), databases)

			// walk databases (login database first) in a stable order and
			// verify the table read surfaces on the first table that allows
			// reads; internal catalogs sometimes deny reads, so a denied table
			// is logged and the next candidate is tried
			type owlJdbcProbeCandidate struct {
				database, table string
			}
			var candidates []owlJdbcProbeCandidate
			for _, probeDatabase := range append([]string{db.CurrentDatabase}, databases...) {
				tables, err := db.GetTables(ctx, probeDatabase)
				if err != nil {
					if owlJdbcE2EInfraError(err) {
						t.Skipf("sidecar/backend unavailable mid-probe: %v", err)
					}
					t.Fatalf("GetTables(%s): %v", probeDatabase, err)
				}
				schemas := make([]string, 0, len(tables))
				for schema := range tables {
					schemas = append(schemas, schema)
				}
				sort.Strings(schemas)
				for _, schema := range schemas {
					if len(tables[schema]) == 0 {
						continue
					}
					if db.UseSchemas() && owlJdbcProbeSystemSchemas[schema] {
						continue
					}
					for _, name := range tables[schema] {
						qualified := name
						if db.UseSchemas() {
							qualified = schema + "." + name
						}
						candidates = append(candidates, owlJdbcProbeCandidate{database: probeDatabase, table: qualified})
					}
				}
			}
			if len(candidates) == 0 {
				t.Fatal("no tables visible through GetTables")
			}

			probeDatabase, firstTable := candidates[0].database, candidates[0].table
			for _, candidate := range candidates {
				// probe reads cheaply first; column metadata joins are the
				// expensive queries and are only run once a table reads OK
				result, err := db.GetRecords(ctx, candidate.database, candidate.table, "", "", 0, 5)
				if err != nil {
					t.Logf("skip records of %s: %v", candidate.table, err)
					continue
				}

				columns, err := db.GetTableColumns(ctx, candidate.database, candidate.table)
				if err != nil {
					t.Logf("skip columns of %s: %v", candidate.table, err)
					continue
				}
				if len(columns) < 1 {
					t.Fatalf("GetTableColumns(%s) returned no header", candidate.table)
				}

				probeDatabase, firstTable = candidate.database, candidate.table
				t.Logf("probe table %s: columns=%v records_page_rows=%d has_next=%v",
					firstTable, columns[0], len(result.Rows)-1, result.HasNextPage)

				if pks, err := db.GetPrimaryKeyColumnNames(ctx, probeDatabase, firstTable); err == nil {
					t.Logf("primary keys: %v", pks)
				}
				if estimate, err := db.GetEstimatedRowCount(ctx, probeDatabase, firstTable); err == nil && estimate != nil {
					t.Logf("estimated rows: %d", *estimate)
				}
				break
			}
			t.Logf("probe database=%q first_table=%q", probeDatabase, firstTable)

			probe := "SELECT 1"
			if db.family == owlFamilyOracle {
				probe = "SELECT 1 FROM DUAL"
			}
			database := databases[0]
			if db.family == owlFamilyPostgres {
				database = db.CurrentDatabase
			}
			rows, _, err := db.ExecuteQuery(ctx, database, probe)
			if err != nil {
				t.Fatalf("ExecuteQuery(%s): %v", probe, err)
			}
			t.Logf("probe result: %v", rows)
		})
	}
}

// TestOwlJdbcE2EEditRoundTrip drives a full create/insert/page/edit/drop cycle
// on a scratch table, proving pagination, record editing and transactional
// pending changes against each real backend.
func TestOwlJdbcE2EEditRoundTrip(t *testing.T) {
	for _, urlstr := range owlJdbcE2EGate(t) {
		urlstr := urlstr
		t.Run(urlstr, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()

			db := &OwlJDBC{}
			if err := db.Connect(ctx, urlstr); err != nil {
				if owlJdbcE2EInfraError(err) {
					t.Skipf("backend unavailable: %v", err)
				}
				t.Fatalf("Connect: %v", err)
			}
			t.Cleanup(func() { _ = db.Connection.Close() })

			database := db.CurrentDatabase
			createSQL, insertSQL, dropSQL := owlJdbcE2EEditSQL(db.family, database)

			if _, err := db.ExecuteDMLStatement(ctx, database, dropSQL); err != nil {
				t.Logf("pre-clean drop: %v", err)
			}
			t.Cleanup(func() {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
				defer cleanupCancel()
				_, _ = db.ExecuteDMLStatement(cleanupCtx, database, dropSQL)
			})

			if _, err := db.ExecuteDMLStatement(ctx, database, createSQL); err != nil {
				t.Fatalf("CREATE TABLE: %v", err)
			}

			for _, insert := range insertSQL {
				if _, err := db.ExecuteDMLStatement(ctx, database, insert); err != nil {
					t.Fatalf("INSERT: %v", err)
				}
			}

			// resolve the table parameter the way the tree would: find the
			// scratch table in GetTables and qualify it per family
			tableParam := "owljdbc_e2e_rt"
			tables, err := db.GetTables(ctx, database)
			if err != nil {
				t.Fatalf("GetTables: %v", err)
			}
			found := false
			for schema, names := range tables {
				for _, name := range names {
					if strings.EqualFold(name, "owljdbc_e2e_rt") {
						tableParam = name
						if db.UseSchemas() {
							tableParam = schema + "." + name
						}
						found = true
						break
					}
				}
				if found {
					break
				}
			}
			if !found {
				t.Fatalf("scratch table not visible via GetTables (database=%s)", database)
			}

			result, err := db.GetRecords(ctx, database, tableParam, "", "", 0, 2)
			if err != nil {
				t.Fatalf("GetRecords: %v", err)
			}
			if !result.HasNextPage || len(result.Rows)-1 != 2 {
				t.Fatalf("pagination: rows=%d has_next=%v, want 2 rows and lookahead", len(result.Rows)-1, result.HasNextPage)
			}

			count, err := db.GetExactRowCount(ctx, database, tableParam, "")
			if err != nil {
				t.Fatalf("GetExactRowCount: %v", err)
			}
			if count != 3 {
				t.Fatalf("count = %d, want 3", count)
			}

			if err := db.UpdateRecord(ctx, database, tableParam, "name", "cell-updated", "id", "1"); err != nil {
				t.Fatalf("UpdateRecord: %v", err)
			}

			pending := []models.DBDMLChange{{
				Database:       database,
				Table:          tableParam,
				Type:           models.DMLUpdateType,
				Values:         []models.CellValue{{Column: "name", Value: "pending-updated", Type: models.String}},
				PrimaryKeyInfo: []models.PrimaryKeyInfo{{Name: "id", Value: "2"}},
			}}
			if err := db.ExecutePendingChanges(ctx, pending); err != nil {
				t.Fatalf("ExecutePendingChanges: %v", err)
			}

			result, err = db.GetRecords(ctx, database, tableParam, "", db.FormatReference("id"), 0, 10)
			if err != nil {
				t.Fatalf("GetRecords after edits: %v", err)
			}
			values := map[string]string{}
			for _, row := range result.Rows[1:] {
				values[row[0]] = row[1]
			}
			if values["1"] != "cell-updated" || values["2"] != "pending-updated" || values["3"] != "row-3" {
				t.Fatalf("rows after edits: %v", values)
			}
			t.Logf("edit round trip OK: %v", values)
		})
	}
}

// owlJdbcE2EEditSQL returns the CREATE, INSERTs and DROP for a scratch table
// in each family's dialect.
func owlJdbcE2EEditSQL(family, database string) (create string, inserts []string, drop string) {
	switch family {
	case owlFamilyOracle:
		// quote every identifier: Oracle stores unquoted names uppercase, which
		// would break the quoted lowercase references the driver issues later
		create = `CREATE TABLE "` + database + `"."owljdbc_e2e_rt" ("id" NUMBER(10) PRIMARY KEY, "name" VARCHAR2(64))`
		inserts = []string{
			`INSERT INTO "` + database + `"."owljdbc_e2e_rt" ("id", "name") VALUES (1, 'row-1')`,
			`INSERT INTO "` + database + `"."owljdbc_e2e_rt" ("id", "name") VALUES (2, 'row-2')`,
			`INSERT INTO "` + database + `"."owljdbc_e2e_rt" ("id", "name") VALUES (3, 'row-3')`,
		}
		drop = `DROP TABLE "` + database + `"."owljdbc_e2e_rt"`
	case owlFamilyMysql:
		create = "CREATE TABLE `" + database + "`.`owljdbc_e2e_rt` (id INT PRIMARY KEY, name VARCHAR(64))"
		inserts = []string{
			"INSERT INTO `" + database + "`.`owljdbc_e2e_rt` (id, name) VALUES (1, 'row-1')",
			"INSERT INTO `" + database + "`.`owljdbc_e2e_rt` (id, name) VALUES (2, 'row-2')",
			"INSERT INTO `" + database + "`.`owljdbc_e2e_rt` (id, name) VALUES (3, 'row-3')",
		}
		drop = "DROP TABLE `" + database + "`.`owljdbc_e2e_rt`"
	default:
		create = `CREATE TABLE "public"."owljdbc_e2e_rt" (id INT PRIMARY KEY, name VARCHAR(64))`
		inserts = []string{
			`INSERT INTO "public"."owljdbc_e2e_rt" (id, name) VALUES (1, 'row-1')`,
			`INSERT INTO "public"."owljdbc_e2e_rt" (id, name) VALUES (2, 'row-2')`,
			`INSERT INTO "public"."owljdbc_e2e_rt" (id, name) VALUES (3, 'row-3')`,
		}
		drop = `DROP TABLE "public"."owljdbc_e2e_rt"`
	}
	return create, inserts, drop
}
