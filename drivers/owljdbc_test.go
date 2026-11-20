package drivers

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/cangyunye/owljdbc"
	"github.com/xo/dburl"

	"github.com/jorgerojas26/lazysql/models"
)

func TestOwlJdbcSchemeDBType(t *testing.T) {
	cases := []struct {
		scheme string
		mode   string
		want   string
		wantOK bool
	}{
		{scheme: "opengauss", want: "opengauss", wantOK: true},
		{scheme: "og", want: "opengauss", wantOK: true},
		{scheme: "oceanbase", want: "oceanbase-mysql", wantOK: true},
		{scheme: "oceanbase", mode: "oracle", want: "oceanbase-oracle", wantOK: true},
		{scheme: "ob", mode: "ORACLE", want: "oceanbase-oracle", wantOK: true},
		{scheme: DriverOwlJDBC, want: "", wantOK: true},
		{scheme: "postgres", want: "", wantOK: false},
	}
	for _, tc := range cases {
		got, err := owlJdbcSchemeDBType(tc.scheme, tc.mode)
		if tc.wantOK {
			if err != nil {
				t.Fatalf("scheme %q: unexpected error: %v", tc.scheme, err)
			}
			if got != tc.want {
				t.Errorf("scheme %q mode %q: got %q, want %q", tc.scheme, tc.mode, got, tc.want)
			}
			continue
		}
		if err == nil {
			t.Errorf("scheme %q: expected an error, got dbType %q", tc.scheme, got)
		}
	}
}

func TestOwlJdbcModeFor(t *testing.T) {
	cases := []struct {
		family   string
		override string
		want     string
	}{
		{family: "mysql", want: "mysql"},
		{family: "oracle", want: "oracle"},
		{family: "postgres", want: "postgresql"},
		{family: "postgres", override: "mysql", want: "mysql"},
		{family: "postgres", override: "oracle", want: "oracle"},
		{family: "postgres", override: "postgres", want: "postgresql"},
		{family: "postgres", override: "bogus", want: "postgresql"},
	}
	for _, tc := range cases {
		if got := owlJdbcModeFor(tc.family, tc.override); got != tc.want {
			t.Errorf("owlJdbcModeFor(%q, %q) = %q, want %q", tc.family, tc.override, got, tc.want)
		}
	}
}

// fakeJarsDir creates a directory with the driver jar and agent jar files the
// profiles glob for, so BuildConfig resolves without a real vendor download.
func fakeJarsDir(t *testing.T, dbType string) string {
	t.Helper()
	dir := t.TempDir()
	globs, ok := map[string][]string{
		"mysql":            {"mysql-connector-j-8.0.33.jar"},
		"postgres":         {"postgresql-42.7.13.jar"},
		"oracle":           {"ojdbc8-23.26.3.0.0.jar"},
		"opengauss":        {"opengauss-jdbc-6.0.6.jar"},
		"oceanbase-mysql":  {"oceanbase-client-2.4.19.jar"},
		"oceanbase-oracle": {"oceanbase-client-2.4.19.jar"},
	}[dbType]
	if !ok {
		t.Fatalf("no jar globs for dbType %q", dbType)
	}
	for _, glob := range globs {
		writeEmptyFile(t, filepath.Join(dir, glob))
	}
	writeEmptyFile(t, filepath.Join(dir, "owl-agent.jar"))
	return dir
}

func writeEmptyFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("fake"), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestBuildOwlJdbcDSN(t *testing.T) {
	jars := fakeJarsDir(t, "opengauss")

	t.Run("scheme alias maps to the opengauss profile", func(t *testing.T) {
		dsn, err := buildOwlJdbcDSN(mustParseURL(t, "opengauss://ogadmin:Gmcc12%23%24@172.20.214.44:6432/og_pg?jarsDir="+queryEscape(jars)))
		if err != nil {
			t.Fatalf("buildOwlJdbcDSN: %v", err)
		}
		cfg, err := owljdbc.DecodeDSN(dsn)
		if err != nil {
			t.Fatalf("DecodeDSN: %v", err)
		}
		if cfg.DriverClass != "org.opengauss.Driver" {
			t.Errorf("driverClass = %q", cfg.DriverClass)
		}
		if cfg.Family != "postgres" {
			t.Errorf("family = %q", cfg.Family)
		}
		if cfg.URL != "jdbc:opengauss://172.20.214.44:6432/og_pg" {
			t.Errorf("url = %q", cfg.URL)
		}
		if cfg.User != "ogadmin" || cfg.Password != "Gmcc12#$" {
			t.Errorf("credentials = %q / %q", cfg.User, cfg.Password)
		}
		if !strings.HasSuffix(cfg.AgentJar, "owl-agent.jar") {
			t.Errorf("agentJar = %q", cfg.AgentJar)
		}
		if !strings.HasSuffix(cfg.Classpath[0], "opengauss-jdbc-6.0.6.jar") {
			t.Errorf("classpath = %v", cfg.Classpath)
		}
	})

	t.Run("query credentials beat userinfo and survive tenant usernames", func(t *testing.T) {
		obJars := fakeJarsDir(t, "oceanbase-mysql")
		dsn, err := buildOwlJdbcDSN(mustParseURL(t, "owljdbc://ignored:ignored@host:2881/test?dbType=oceanbase-mysql&user=root%40sys&password=AAli88%40%401688&jarsDir="+queryEscape(obJars)))
		if err != nil {
			t.Fatalf("buildOwlJdbcDSN: %v", err)
		}
		cfg, err := owljdbc.DecodeDSN(dsn)
		if err != nil {
			t.Fatalf("DecodeDSN: %v", err)
		}
		if cfg.User != "root@sys" {
			t.Errorf("user = %q, want root@sys", cfg.User)
		}
		if cfg.Password != "AAli88@@1688" {
			t.Errorf("password = %q", cfg.Password)
		}
		if cfg.Family != "mysql" {
			t.Errorf("family = %q", cfg.Family)
		}
	})

	t.Run("dbType parameter wins over the scheme default", func(t *testing.T) {
		jars := fakeJarsDir(t, "oceanbase-oracle")
		_, err := buildOwlJdbcDSN(mustParseURL(t, "oceanbase://root:pw@host:2881/test?mode=oracle&jarsDir="+queryEscape(jars)))
		if err != nil {
			t.Fatalf("oceanbase mode=oracle: %v", err)
		}
	})

	t.Run("owljdbc scheme without dbType is rejected", func(t *testing.T) {
		// the DSN generator runs inside dburl.Parse, so bad URLs fail there
		if _, err := dburl.Parse("owljdbc://u:p@host:1/db"); err == nil {
			t.Fatal("expected an error for missing dbType")
		}
	})

	t.Run("unknown dbType lists the catalog", func(t *testing.T) {
		_, err := dburl.Parse("owljdbc://u:p@host:1/db?dbType=nosuch")
		if err == nil || !strings.Contains(err.Error(), "oceanbase-mysql") {
			t.Fatalf("expected catalog listing in error, got %v", err)
		}
	})

	t.Run("jdbcUrl override replaces the built URL", func(t *testing.T) {
		dsn, err := buildOwlJdbcDSN(mustParseURL(t, "opengauss://u:p@host:6432/db?jarsDir="+queryEscape(jars)+"&jdbcUrl=jdbc:opengauss://other:5432/x"))
		if err != nil {
			t.Fatalf("buildOwlJdbcDSN: %v", err)
		}
		cfg, err := owljdbc.DecodeDSN(dsn)
		if err != nil {
			t.Fatalf("DecodeDSN: %v", err)
		}
		if cfg.URL != "jdbc:opengauss://other:5432/x" {
			t.Errorf("url = %q", cfg.URL)
		}
	})

	t.Run("env fallback for jars dir", func(t *testing.T) {
		t.Setenv("OWLJDBC_JARS_DIR", jars)
		dsn, err := buildOwlJdbcDSN(mustParseURL(t, "opengauss://u:p@host:6432/db"))
		if err != nil {
			t.Fatalf("buildOwlJdbcDSN: %v", err)
		}
		cfg, err := owljdbc.DecodeDSN(dsn)
		if err != nil {
			t.Fatalf("DecodeDSN: %v", err)
		}
		if !strings.HasSuffix(cfg.Classpath[0], "opengauss-jdbc-6.0.6.jar") {
			t.Errorf("classpath = %v", cfg.Classpath)
		}
	})
}

func TestOwlJdbcResolveURL(t *testing.T) {
	jars := fakeJarsDir(t, "mysql")
	info, err := owlJdbcResolveURL("owljdbc://u:p@host:3306/db?dbType=mysql&mode=oracle&jdbcUrl=jdbc:mysql://host:3306/other&jarsDir=" + queryEscape(jars))
	if err != nil {
		t.Fatalf("owlJdbcResolveURL: %v", err)
	}
	if info.Config.Family != "mysql" {
		t.Errorf("family = %q", info.Config.Family)
	}
	if info.DBType != "mysql" {
		t.Errorf("dbType = %q", info.DBType)
	}
	if info.Mode != "oracle" {
		t.Errorf("mode = %q", info.Mode)
	}
	if info.JdbcURL != "jdbc:mysql://host:3306/other" {
		t.Errorf("jdbcUrl = %q", info.JdbcURL)
	}
	if info.Endpoint.Host != "host" || info.Endpoint.Port != "3306" || info.Endpoint.User != "u" || info.Endpoint.Database != "db" {
		t.Errorf("endpoint = %+v", info.Endpoint)
	}

	// missing driver jars surface as an error through dburl.Parse (the DSN
	// generator resolves jars), not only at connect time
	if _, err := owlJdbcResolveURL("opengauss://u:p@host:6432/db"); err == nil {
		t.Fatal("expected jar resolution error without jars")
	}
}

func mustParseURL(t *testing.T, urlstr string) *dburl.URL {
	t.Helper()
	parsed, err := dburl.Parse(urlstr)
	if err != nil {
		t.Fatalf("dburl.Parse(%q): %v", urlstr, err)
	}
	return parsed
}

func queryEscape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, `\`, "/"), " ", "%20")
}

var owlJdbcFakeAgent = sync.OnceValues(func() (string, error) {
	dir, err := os.MkdirTemp("", "owljdbc-fake-agent-*")
	if err != nil {
		return "", err
	}
	name := "owljdbc_fake_agent"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	bin := filepath.Join(dir, name)
	cmd := exec.Command("go", "build", "-o", bin, "../testdata/owljdbc_fake_agent")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", err
	}
	_ = out
	return bin, nil
})

// newFakeAgentDriver opens an OwlJDBC connection whose sidecar JVM is the
// compiled fake agent. dbType selects the profile family; extraParams carry
// per-test URL query additions.
func newFakeAgentDriver(t *testing.T, dbType string, extraParams string) *OwlJDBC {
	t.Helper()

	agentBin, err := owlJdbcFakeAgent()
	if err != nil {
		t.Fatalf("build fake agent: %v", err)
	}

	jars := fakeJarsDir(t, dbType)
	urlstr := "owljdbc://u:p@127.0.0.1:1/db?dbType=" + dbType +
		"&jarsDir=" + queryEscape(jars) +
		"&agentJar=" + queryEscape(filepath.Join(jars, "owl-agent.jar")) +
		"&java=" + queryEscape(agentBin) +
		extraParams

	driver := &OwlJDBC{}
	if err := driver.Connect(context.Background(), urlstr); err != nil {
		t.Fatalf("Connect(%s): %v", dbType, err)
	}
	t.Cleanup(func() { _ = driver.Connection.Close() })
	return driver
}

func TestOwlJdbcConnectFamilies(t *testing.T) {
	t.Run("mysql family", func(t *testing.T) {
		db := newFakeAgentDriver(t, "mysql", "")
		if db.CurrentDatabase != "mysqldb" {
			t.Errorf("CurrentDatabase = %q, want mysqldb", db.CurrentDatabase)
		}
		if db.family != owlFamilyMysql || db.UseSchemas() {
			t.Errorf("family = %q, UseSchemas = %v", db.family, db.UseSchemas())
		}
		if !db.SupportsProgramming() {
			t.Error("SupportsProgramming = false")
		}
		if db.GetProvider() != DriverOwlJDBC {
			t.Errorf("provider = %q", db.GetProvider())
		}
		if ref := db.FormatReference("col"); ref != "`col`" {
			t.Errorf("FormatReference = %q", ref)
		}
	})

	t.Run("postgres family", func(t *testing.T) {
		db := newFakeAgentDriver(t, "opengauss", "")
		if db.CurrentDatabase != "pgdb" {
			t.Errorf("CurrentDatabase = %q, want pgdb", db.CurrentDatabase)
		}
		if db.family != owlFamilyPostgres || !db.UseSchemas() {
			t.Errorf("family = %q, UseSchemas = %v", db.family, db.UseSchemas())
		}
		if ref := db.FormatReference("col"); ref != `"col"` {
			t.Errorf("FormatReference = %q", ref)
		}
	})

	t.Run("oracle family", func(t *testing.T) {
		db := newFakeAgentDriver(t, "oceanbase-oracle", "")
		if db.CurrentDatabase != "OraUser" {
			t.Errorf("CurrentDatabase = %q, want OraUser", db.CurrentDatabase)
		}
		if db.family != owlFamilyOracle || db.UseSchemas() {
			t.Errorf("family = %q, UseSchemas = %v", db.family, db.UseSchemas())
		}
		if db.FormatPlaceholder(1) != "?" {
			t.Errorf("FormatPlaceholder = %q", db.FormatPlaceholder(1))
		}
	})
}

func TestOwlJdbcConnectMissingJars(t *testing.T) {
	agentBin, err := owlJdbcFakeAgent()
	if err != nil {
		t.Fatalf("build fake agent: %v", err)
	}
	emptyDir := t.TempDir()

	driver := &OwlJDBC{}
	err = driver.Connect(context.Background(), "owljdbc://u:p@127.0.0.1:1/db?dbType=mysql&jarsDir="+queryEscape(emptyDir)+"&java="+queryEscape(agentBin))
	if err == nil {
		t.Fatal("expected a jar resolution error")
	}
}

func TestOwlJdbcGetDatabasesFiltersMySQLSystemSchemas(t *testing.T) {
	db := newFakeAgentDriver(t, "mysql", "")
	databases, err := db.GetDatabases(context.Background())
	if err != nil {
		t.Fatalf("GetDatabases: %v", err)
	}
	if len(databases) != 2 || databases[0] != "db_one" || databases[1] != "db_two" {
		t.Errorf("databases = %v, want [db_one db_two]", databases)
	}
}

func TestOwlJdbcGetTables(t *testing.T) {
	db := newFakeAgentDriver(t, "mysql", "")
	tables, err := db.GetTables(context.Background(), "db_one")
	if err != nil {
		t.Fatalf("GetTables: %v", err)
	}
	if got := tables["db_one"]; len(got) != 2 || got[0] != "t1" || got[1] != "t2" {
		t.Errorf("tables = %v", got)
	}
}

func TestOwlJdbcGetTableColumnsShape(t *testing.T) {
	// the oracle-family query carries the "column_name" probe keyword the fake
	// agent scripts, exercising the real dispatch path end to end
	db := newFakeAgentDriver(t, "oceanbase-oracle", "")
	columns, err := db.GetTableColumns(context.Background(), "OraUser", "T1")
	if err != nil {
		t.Fatalf("GetTableColumns: %v", err)
	}
	if len(columns) != 3 {
		t.Fatalf("rows = %d, want header + 2", len(columns))
	}
	if columns[0][0] != "column_name" || columns[1][0] != "id" || columns[2][3] != "'x'" {
		t.Errorf("columns = %v", columns)
	}
	// NULL default renders as "" in the metadata shape
	if columns[1][3] != "" {
		t.Errorf("null default = %q, want empty", columns[1][3])
	}
}

func TestOwlJdbcGetRecordsSentinelsAndTypes(t *testing.T) {
	db := newFakeAgentDriver(t, "mysql", "")
	ctx := context.Background()

	result, err := db.GetRecords(ctx, "db_one", "__null_row__", "", "", 0, 10)
	if err != nil {
		t.Fatalf("GetRecords null: %v", err)
	}
	if len(result.Rows) != 2 || result.Rows[1][0] != "NULL&" {
		t.Errorf("rows = %v", result.Rows)
	}

	result, err = db.GetRecords(ctx, "db_one", "__empty_row__", "", "", 0, 10)
	if err != nil {
		t.Fatalf("GetRecords empty: %v", err)
	}
	if len(result.Rows) != 2 || result.Rows[1][0] != "EMPTY&" {
		t.Errorf("rows = %v", result.Rows)
	}

	result, err = db.GetRecords(ctx, "db_one", "__typed_row__", "", "", 0, 10)
	if err != nil {
		t.Fatalf("GetRecords typed: %v", err)
	}
	want := []string{"42", "1.5", "true", "2026-01-02 03:04:05"}
	if len(result.Rows) != 2 {
		t.Fatalf("rows = %v", result.Rows)
	}
	for i, cell := range result.Rows[1] {
		if cell != want[i] {
			t.Errorf("cell %d = %q, want %q", i, cell, want[i])
		}
	}

	// the pagination placeholders are inlined for display
	if !strings.Contains(result.Query, "LIMIT 0, 11") {
		t.Errorf("query = %q", result.Query)
	}
}

func TestOwlJdbcExecuteQueryAndDML(t *testing.T) {
	db := newFakeAgentDriver(t, "mysql", "")
	ctx := context.Background()

	rows, count, err := db.ExecuteQuery(ctx, "db_one", "SELECT * FROM t1")
	if err != nil {
		t.Fatalf("ExecuteQuery: %v", err)
	}
	if count != 1 || rows[0][0] != "X" || rows[1][0] != "hello" {
		t.Errorf("rows = %v, count = %d", rows, count)
	}

	result, err := db.ExecuteDMLStatement(ctx, "db_one", "UPDATE t1 SET a = 1")
	if err != nil {
		t.Fatalf("ExecuteDMLStatement: %v", err)
	}
	if result != "3 rows affected" {
		t.Errorf("result = %q", result)
	}

	if _, _, err := db.ExecuteQuery(ctx, "db_one", "SELECT __midstream_fail__ FROM t1"); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected midstream failure, got %v", err)
	}
}

func TestOwlJdbcRecordEdits(t *testing.T) {
	db := newFakeAgentDriver(t, "mysql", "")
	ctx := context.Background()

	if err := db.UpdateRecord(ctx, "db_one", "t1", "col", "v", "id", "1"); err != nil {
		t.Errorf("UpdateRecord: %v", err)
	}
	if err := db.DeleteRecord(ctx, "db_one", "t1", "id", "1"); err != nil {
		t.Errorf("DeleteRecord: %v", err)
	}

	pending := []models.DBDMLChange{
		{
			Database: "db_one",
			Table:    "t1",
			Type:     models.DMLInsertType,
			Values: []models.CellValue{
				{Column: "id", Value: "1", Type: models.String},
				{Column: "name", Value: nil, Type: models.Null},
			},
		},
		{
			Database:       "db_one",
			Table:          "t1",
			Type:           models.DMLUpdateType,
			Values:         []models.CellValue{{Column: "name", Value: "x", Type: models.String}},
			PrimaryKeyInfo: []models.PrimaryKeyInfo{{Name: "id", Value: "1"}},
		},
		{
			Database:       "db_one",
			Table:          "t1",
			Type:           models.DMLDeleteType,
			PrimaryKeyInfo: []models.PrimaryKeyInfo{{Name: "id", Value: "2"}},
		},
	}
	if err := db.ExecutePendingChanges(ctx, pending); err != nil {
		t.Errorf("ExecutePendingChanges: %v", err)
	}
}

func TestOwlJdbcDMLChangeToQueryString(t *testing.T) {
	db := newFakeAgentDriver(t, "oceanbase-oracle", "")

	query, err := db.DMLChangeToQueryString(models.DBDMLChange{
		Database: "OraUser",
		Table:    "T1",
		Type:     models.DMLUpdateType,
		Values:   []models.CellValue{{Column: "name", Value: "it's", Type: models.String}},
		PrimaryKeyInfo: []models.PrimaryKeyInfo{
			{Name: "id", Value: "1"},
		},
	})
	if err != nil {
		t.Fatalf("DMLChangeToQueryString: %v", err)
	}
	want := `UPDATE "OraUser"."T1" SET "name" = 'it''s' WHERE "id" = '1'`
	if query != want {
		t.Errorf("query = %q, want %q", query, want)
	}
}

func TestOwlJdbcStreamQuery(t *testing.T) {
	db := newFakeAgentDriver(t, "mysql", "")
	ctx := context.Background()

	var batches []QueryBatch
	result, err := db.StreamQuery(ctx, "db_one", "SELECT * FROM t1", 100, func(batch QueryBatch) error {
		batches = append(batches, batch)
		return nil
	})
	if err != nil {
		t.Fatalf("StreamQuery: %v", err)
	}
	if result.Rows != 1 || result.Truncated {
		t.Errorf("result = %+v", result)
	}
	if len(batches) != 1 || batches[0].Rows[0][0] != "hello" {
		t.Errorf("batches = %+v", batches)
	}
}

func TestOwlJdbcSwitchDatabase(t *testing.T) {
	db := newFakeAgentDriver(t, "opengauss", "")
	if err := db.SwitchDatabase(context.Background(), "other_db"); err != nil {
		t.Fatalf("SwitchDatabase: %v", err)
	}
	if db.CurrentDatabase != "other_db" {
		t.Errorf("CurrentDatabase = %q", db.CurrentDatabase)
	}

	if err := db.Connection.PingContext(context.Background()); err != nil {
		t.Errorf("ping after switch: %v", err)
	}
}

func TestOwlJdbcSwitchDatabaseBlockedByCustomJdbcURL(t *testing.T) {
	db := newFakeAgentDriver(t, "opengauss", "&jdbcUrl=jdbc:opengauss://127.0.0.1:1/db")
	if err := db.SwitchDatabase(context.Background(), "other_db"); err == nil {
		t.Fatal("expected switch to be rejected with a custom jdbcUrl")
	}
}

func TestOwlJdbcQualifiedTable(t *testing.T) {
	mysql := &OwlJDBC{family: owlFamilyMysql}
	got, err := mysql.qualifiedTable("db", "t")
	if err != nil || got != "`db`.`t`" {
		t.Errorf("mysql qualifiedTable = %q, %v", got, err)
	}

	oracle := &OwlJDBC{family: owlFamilyOracle}
	got, err = oracle.qualifiedTable("owner", "t")
	if err != nil || got != `"owner"."t"` {
		t.Errorf("oracle qualifiedTable = %q, %v", got, err)
	}

	pg := &OwlJDBC{family: owlFamilyPostgres}
	if _, err := pg.qualifiedTable("db", "no_schema"); err == nil {
		t.Error("postgres family requires schema.table")
	}
	got, err = pg.qualifiedTable("db", "public.t")
	if err != nil || got != `"public"."t"` {
		t.Errorf("postgres qualifiedTable = %q, %v", got, err)
	}
}

func TestOwlJdbcFormatArgForQueryString(t *testing.T) {
	db := &OwlJDBC{family: owlFamilyMysql}
	cases := []struct {
		arg  any
		want string
	}{
		{arg: "plain", want: "'plain'"},
		{arg: "it's", want: "'it''s'"},
		{arg: "NULL", want: "NULL"},
		{arg: "DEFAULT", want: "DEFAULT"},
		{arg: int64(5), want: "5"},
		{arg: 1.5, want: "1.5"},
		{arg: nil, want: "NULL"},
	}
	for _, tc := range cases {
		if got := db.FormatArgForQueryString(tc.arg); got != tc.want {
			t.Errorf("FormatArgForQueryString(%v) = %q, want %q", tc.arg, got, tc.want)
		}
	}
}
