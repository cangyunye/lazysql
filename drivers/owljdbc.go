package drivers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cangyunye/owljdbc"

	"github.com/jorgerojas26/lazysql/models"
)

// OwlJDBC transport families (owljdbc.Config.Family values).
const (
	owlFamilyMysql    = "mysql"
	owlFamilyOracle   = "oracle"
	owlFamilyPostgres = "postgres"
)

// OwlJDBC talks to JDBC-only backends (openGauss, OceanBase, Oracle, ...) via
// the owljdbc bridge: a JVM sidecar loads the vendor JDBC jar and the Go side
// speaks database/sql over the bridge's frame protocol. Dialect assumptions
// follow the transport family (mysql/oracle/postgres) that the resolved
// owljdbc profile reports, while the metadata dialect (mode) defaults to the
// family and can be overridden per URL for compatibility instances.
type OwlJDBC struct {
	Connection      *sql.DB
	Provider        string
	PoolConfig      models.ConnectionPoolConfig
	CurrentDatabase string
	Urlstr          string

	dbType     string           // owljdbc catalog type (opengauss, oceanbase-mysql, ...)
	family     string           // mysql | oracle | postgres
	mode       string           // metadata dialect: mysql | postgresql | oracle
	dsn        string           // JSON DSN of the current connection
	endpoint   owljdbc.Endpoint // resolved endpoint for SwitchDatabase rebuilds
	customJDBC bool             // a jdbcUrl override was supplied (blocks rebuilds)

	jarsDir  string
	agentJar string
	javaHome string
	jvmOpts  []string
}

func (db *OwlJDBC) TestConnection(ctx context.Context, urlstr string) error {
	return db.Connect(ctx, urlstr)
}

func (db *OwlJDBC) Connect(ctx context.Context, urlstr string) error {
	ctx = contextOrBackground(ctx)
	db.SetProvider(DriverOwlJDBC)

	info, err := owlJdbcResolveURL(urlstr)
	if err != nil {
		return err
	}

	connection, err := owlJdbcOpen(info.Config, db.PoolConfig)
	if err != nil {
		return err
	}

	if err = connection.PingContext(ctx); err != nil {
		_ = connection.Close()
		return err
	}

	db.Connection = connection
	db.Urlstr = urlstr
	db.dsn = owljdbc.EncodeDSN(info.Config)
	db.dbType = info.DBType
	db.family = info.Config.Family
	db.mode = info.Mode
	db.endpoint = info.Endpoint
	db.customJDBC = info.JdbcURL != ""
	db.jarsDir = info.JarsDir
	db.agentJar = info.AgentJar
	db.javaHome = info.JavaHome
	db.jvmOpts = info.JvmOpts

	database, err := db.currentDatabase(ctx)
	if err != nil {
		_ = db.Connection.Close()
		db.Connection = nil
		return err
	}
	db.CurrentDatabase = database

	return nil
}

// owlJdbcOpen opens an owljdbc pool from a decoded config and applies the
// connection pool limits.
func owlJdbcOpen(cfg owljdbc.Config, poolConfig models.ConnectionPoolConfig) (*sql.DB, error) {
	connection, err := sql.Open(DriverOwlJDBC, owljdbc.EncodeDSN(cfg))
	if err != nil {
		return nil, err
	}
	if err := applyConnectionPoolConfig(connection, poolConfig); err != nil {
		_ = connection.Close()
		return nil, err
	}
	return connection, nil
}

// currentDatabase reports the login database per family so the tree and the
// editor default to the connection's own context.
func (db *OwlJDBC) currentDatabase(ctx context.Context) (string, error) {
	switch db.family {
	case owlFamilyMysql:
		var database sql.NullString
		if err := db.Connection.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&database); err != nil {
			return "", err
		}
		return database.String, nil
	case owlFamilyOracle:
		var user string
		if err := db.Connection.QueryRowContext(ctx, "SELECT USER FROM DUAL").Scan(&user); err != nil {
			return "", err
		}
		return user, nil
	default:
		var database string
		if err := db.Connection.QueryRowContext(ctx, "SELECT current_database()").Scan(&database); err != nil {
			return "", err
		}
		return database, nil
	}
}

// connectToDatabase opens a new pool for the given database without mutating
// the receiver; the caller must close it. It rebuilds the JSON DSN from the
// stored endpoint, so the owljdbc sidecar JVM (keyed by classpath) is reused
// and the switch only costs one extra bridge session.
func (db *OwlJDBC) connectToDatabase(ctx context.Context, database string) (*sql.DB, error) {
	ctx = contextOrBackground(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if db.customJDBC {
		return nil, errors.New("cannot switch database: connection uses a custom jdbcUrl override")
	}

	endpoint := db.endpoint
	endpoint.Database = database
	cfg, err := owljdbc.BuildConfig(db.dbType, endpoint, "", db.jarsDir, db.agentJar, db.javaHome)
	if err != nil {
		return nil, err
	}
	cfg.JvmOpts = db.jvmOpts
	return owlJdbcOpen(cfg, db.PoolConfig)
}

// connectionFor returns a pool scoped to the given database (postgres family
// contract): the current one when it matches, otherwise a temporary pool the
// caller MUST close.
func (db *OwlJDBC) connectionFor(ctx context.Context, database string) (*sql.DB, bool, error) {
	ctx = contextOrBackground(ctx)
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if database == "" || database == db.CurrentDatabase {
		return db.Connection, false, nil
	}
	conn, err := db.connectToDatabase(ctx, database)
	if err != nil {
		return nil, false, err
	}
	return conn, true, nil
}

func (db *OwlJDBC) SwitchDatabase(ctx context.Context, database string) error {
	ctx = contextOrBackground(ctx)
	conn, err := db.connectToDatabase(ctx, database)
	if err != nil {
		return err
	}

	if err := db.Connection.Close(); err != nil {
		_ = conn.Close()
		return err
	}

	db.Connection = conn
	db.CurrentDatabase = database
	return nil
}

// editorConnectionFor returns the connection an editor operation should run
// on, mirroring each family's database-selection contract: postgres family
// opens a temporary pool, mysql family issues USE on a reserved session, and
// oracle family moves current_schema on a reserved session (both reserved
// sessions are discarded after use so they cannot leak into other queries).
func (db *OwlJDBC) editorConnectionFor(ctx context.Context, database string) (editorConnection, func(), error) {
	switch db.family {
	case owlFamilyMysql:
		if database == "" || database == db.CurrentDatabase {
			return db.Connection, func() {}, nil
		}
		return switchedEditorConnection(ctx, db.Connection, "USE `"+strings.ReplaceAll(database, "`", "``")+"`")
	case owlFamilyOracle:
		if database == "" || strings.EqualFold(database, db.CurrentDatabase) {
			return db.Connection, func() {}, nil
		}
		return switchedEditorConnection(ctx, db.Connection, "ALTER SESSION SET CURRENT_SCHEMA = "+db.FormatReference(database))
	default:
		conn, needsClose, err := db.connectionFor(ctx, database)
		if err != nil {
			return nil, nil, err
		}
		if needsClose {
			return conn, func() { _ = conn.Close() }, nil
		}
		return conn, func() {}, nil
	}
}

func (db *OwlJDBC) GetProvider() string {
	return db.Provider
}

// Mode reports the metadata dialect (mysql | postgresql | oracle). Metadata
// dispatch itself follows the transport family; Mode is informational and
// exists for logs and the openGauss compatibility-instance use case.
func (db *OwlJDBC) Mode() string {
	return db.mode
}

func (db *OwlJDBC) SetProvider(provider string) {
	db.Provider = provider
}

// SupportsProgramming reports functions/procedures/views support. The mysql
// family implements these over information_schema.routines, which the native
// MySQL driver does not; the bridge has no reason to skip them.
func (db *OwlJDBC) SupportsProgramming() bool {
	return true
}

// UseSchemas reports whether the tree groups tables by schema. Only the
// postgres family does (openGauss keeps the postgresql catalog shape); mysql
// and oracle families scope everything by the database/owner parameter.
func (db *OwlJDBC) UseSchemas() bool {
	return db.family == owlFamilyPostgres
}

// qualifiedTable renders the table reference a family's SQL expects: backticks
// for mysql, quoted identifiers elsewhere. The postgres family follows the
// native postgres driver's "schema.table" convention; mysql and oracle
// families keep database and table separate.
func (db *OwlJDBC) qualifiedTable(database, table string) (string, error) {
	switch db.family {
	case owlFamilyMysql:
		return "`" + database + "`.`" + table + "`", nil
	case owlFamilyOracle:
		return `"` + database + `"."` + table + `"`, nil
	default:
		parts := strings.Split(table, ".")
		if len(parts) != 2 {
			return "", errors.New("table must be in the format schema.table")
		}
		return `"` + parts[0] + `"."` + parts[1] + `"`, nil
	}
}

// paginationClause returns the LIMIT/OFFSET suffix with two ? placeholders, in
// the argument order paginationArgs binds them.
func (db *OwlJDBC) paginationClause() string {
	switch db.family {
	case owlFamilyMysql:
		return " LIMIT ?, ?"
	case owlFamilyOracle:
		return " OFFSET ? ROWS FETCH NEXT ? ROWS ONLY"
	default:
		return " LIMIT ? OFFSET ?"
	}
}

func (db *OwlJDBC) paginationArgs(offset, fetchLimit int) []any {
	if db.family == owlFamilyPostgres {
		return []any{fetchLimit, offset}
	}
	return []any{offset, fetchLimit}
}

// displayQuery inlines the pagination values for the query string the UI
// stores and shows.
func (db *OwlJDBC) displayQuery(query string, offset, fetchLimit int) string {
	first, second := strconv.Itoa(offset), strconv.Itoa(fetchLimit)
	if db.family == owlFamilyPostgres {
		first, second = strconv.Itoa(fetchLimit), strconv.Itoa(offset)
	}
	query = strings.Replace(query, "?", first, 1)
	query = strings.Replace(query, "?", second, 1)
	return query
}

func (db *OwlJDBC) GetRecords(ctx context.Context, database, table, where, sort string, offset, limit int) (PageResult, error) {
	ctx = contextOrBackground(ctx)
	if database == "" {
		return PageResult{}, errors.New("database name is required")
	}
	if table == "" {
		return PageResult{}, errors.New("table name is required")
	}

	formattedTable, err := db.qualifiedTable(database, table)
	if err != nil {
		return PageResult{}, err
	}

	conn, cleanup, err := db.editorConnectionFor(ctx, database)
	if err != nil {
		return PageResult{}, err
	}
	defer cleanup()

	pageSize, fetchLimit := pageSizeAndFetchLimit(limit)
	queryString := "SELECT * FROM " + formattedTable

	if where != "" {
		queryString += fmt.Sprintf(" %s", where)
	}
	if sort != "" {
		queryString += fmt.Sprintf(" ORDER BY %s", sort)
	}
	queryString += db.paginationClause()

	paginatedRows, err := conn.QueryContext(ctx, queryString, db.paginationArgs(offset, fetchLimit)...)
	if err != nil {
		return PageResult{Query: queryString}, err
	}
	defer paginatedRows.Close()

	columns, err := paginatedRows.Columns()
	if err != nil {
		return PageResult{Query: queryString}, err
	}

	records := [][]string{columns}
	for paginatedRows.Next() {
		row, err := owlJdbcScanRow(paginatedRows, len(columns), owlScanPage)
		if err != nil {
			return PageResult{Query: queryString}, err
		}
		records = append(records, row)
	}
	if err := paginatedRows.Err(); err != nil {
		return PageResult{Query: queryString}, err
	}
	if err := paginatedRows.Close(); err != nil {
		return PageResult{Query: queryString}, err
	}

	return newPageResult(records, db.displayQuery(queryString, offset, fetchLimit), pageSize), nil
}

func (db *OwlJDBC) GetEstimatedRowCount(ctx context.Context, database, table string) (*int64, error) {
	ctx = contextOrBackground(ctx)
	if database == "" {
		return nil, errors.New("database name is required")
	}
	if table == "" {
		return nil, errors.New("table name is required")
	}

	conn, cleanup, err := db.editorConnectionFor(ctx, database)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	var estimate sql.NullInt64
	var scanErr error
	switch db.family {
	case owlFamilyMysql:
		_, tableName, tableErr := owlJdbcSplitPlainTable(table)
		if tableErr != nil {
			return nil, tableErr
		}
		scanErr = owlJdbcScanOne(ctx, conn, `
			SELECT TABLE_ROWS
			FROM information_schema.TABLES
			WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?
		`, []any{&estimate}, database, tableName)
	case owlFamilyOracle:
		_, tableName, tableErr := owlJdbcSplitPlainTable(table)
		if tableErr != nil {
			return nil, tableErr
		}
		scanErr = owlJdbcScanOne(ctx, conn, `
			SELECT num_rows
			FROM all_tables
			WHERE owner = ? AND table_name = ?
		`, []any{&estimate}, database, tableName)
	default:
		parts := strings.SplitN(table, ".", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, errors.New("table must be in the format schema.table")
		}
		scanErr = owlJdbcScanOne(ctx, conn, `
			SELECT c.reltuples::bigint
			FROM pg_catalog.pg_class c
			JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
			WHERE n.nspname = ? AND c.relname = ?
		`, []any{&estimate}, parts[0], parts[1])
	}

	if errors.Is(scanErr, sql.ErrNoRows) {
		return nil, nil
	}
	if scanErr != nil {
		return nil, scanErr
	}
	if !estimate.Valid || estimate.Int64 < 0 {
		return nil, nil
	}
	return &estimate.Int64, nil
}

// owlJdbcSplitPlainTable splits the "database.table" references the mysql and
// oracle families receive, tolerating a bare table name that lost its prefix.
func owlJdbcSplitPlainTable(table string) (database, tableName string, err error) {
	parts := strings.SplitN(table, ".", 2)
	if len(parts) == 2 {
		return parts[0], parts[1], nil
	}
	return "", table, nil
}

func (db *OwlJDBC) GetExactRowCount(ctx context.Context, database, table, where string) (int64, error) {
	ctx = contextOrBackground(ctx)
	if database == "" {
		return 0, errors.New("database name is required")
	}
	if table == "" {
		return 0, errors.New("table name is required")
	}

	formattedTable, err := db.qualifiedTable(database, table)
	if err != nil {
		return 0, err
	}

	conn, cleanup, err := db.editorConnectionFor(ctx, database)
	if err != nil {
		return 0, err
	}
	defer cleanup()

	query := "SELECT COUNT(*) FROM " + formattedTable
	if where != "" {
		query += " " + where
	}

	var count int64
	if err := owlJdbcScanOne(ctx, conn, query, []any{&count}); err != nil {
		return 0, err
	}
	return count, nil
}

func (db *OwlJDBC) UpdateRecord(ctx context.Context, database, table, column, value, primaryKeyColumnName, primaryKeyValue string) error {
	ctx = contextOrBackground(ctx)
	if database == "" {
		return errors.New("database name is required")
	}
	if table == "" {
		return errors.New("table name is required")
	}
	if column == "" {
		return errors.New("column name is required")
	}
	if primaryKeyColumnName == "" {
		return errors.New("primary key column name is required")
	}

	formattedTable, err := db.qualifiedTable(database, table)
	if err != nil {
		return err
	}

	conn, cleanup, err := db.editorConnectionFor(ctx, database)
	if err != nil {
		return err
	}
	defer cleanup()

	query := "UPDATE " + formattedTable + " SET " + db.FormatReference(column) + " = ? WHERE " + db.FormatReference(primaryKeyColumnName) + " = ?"

	_, err = conn.ExecContext(ctx, query, value, primaryKeyValue)
	return err
}

func (db *OwlJDBC) DeleteRecord(ctx context.Context, database, table, primaryKeyColumnName, primaryKeyValue string) error {
	ctx = contextOrBackground(ctx)
	if database == "" {
		return errors.New("database name is required")
	}
	if table == "" {
		return errors.New("table name is required")
	}
	if primaryKeyColumnName == "" {
		return errors.New("primary key column name is required")
	}

	formattedTable, err := db.qualifiedTable(database, table)
	if err != nil {
		return err
	}

	conn, cleanup, err := db.editorConnectionFor(ctx, database)
	if err != nil {
		return err
	}
	defer cleanup()

	query := "DELETE FROM " + formattedTable + " WHERE " + db.FormatReference(primaryKeyColumnName) + " = ?"

	_, err = conn.ExecContext(ctx, query, primaryKeyValue)
	return err
}

func (db *OwlJDBC) ExecuteDMLStatement(ctx context.Context, database, query string) (string, error) {
	conn, cleanup, err := db.editorConnectionFor(ctx, database)
	if err != nil {
		return "", err
	}
	defer cleanup()

	ctx = contextOrBackground(ctx)
	res, err := conn.ExecContext(ctx, query)
	if err != nil {
		return "", err
	}

	rowsAffected, err := res.RowsAffected()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d rows affected", rowsAffected), nil
}

// StreamQuery incrementally emits interactive SQL results and honors context
// cancellation through database/sql.
func (db *OwlJDBC) StreamQuery(ctx context.Context, database, query string, maxRows int, onBatch func(QueryBatch) error) (QueryStreamResult, error) {
	conn, cleanup, err := db.editorConnectionFor(ctx, database)
	if err != nil {
		return QueryStreamResult{}, err
	}
	defer cleanup()

	return streamQueryWithScanner(ctx, conn, query, maxRows, onBatch, owlJdbcStreamScanRow)
}

func (db *OwlJDBC) ExecuteQuery(ctx context.Context, database, query string) ([][]string, int, error) {
	conn, cleanup, err := db.editorConnectionFor(ctx, database)
	if err != nil {
		return nil, 0, err
	}
	defer cleanup()

	ctx = contextOrBackground(ctx)
	rows, err := conn.QueryContext(ctx, query)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	columns, err := rows.Columns()
	if err != nil {
		return nil, 0, err
	}

	records := make([][]string, 0)
	for rows.Next() {
		row, err := owlJdbcScanRow(rows, len(columns), owlScanPlain)
		if err != nil {
			return nil, 0, err
		}
		records = append(records, row)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	return append([][]string{columns}, records...), len(records), nil
}

func (db *OwlJDBC) ExecutePendingChanges(ctx context.Context, changes []models.DBDMLChange) error {
	ctx = contextOrBackground(ctx)
	if len(changes) == 0 {
		return nil
	}

	var queries []models.Query
	var target string
	if db.family == owlFamilyPostgres {
		target = changes[0].Database
		if target == "" {
			target = db.CurrentDatabase
		}
	}

	for _, change := range changes {
		if db.family == owlFamilyPostgres {
			changeTarget := change.Database
			if changeTarget == "" {
				changeTarget = db.CurrentDatabase
			}
			if changeTarget != target {
				return errors.New("cannot atomically apply PostgreSQL changes across databases; apply each database separately")
			}
		}

		formattedTable, err := db.qualifiedTable(change.Database, change.Table)
		if err != nil {
			return err
		}

		switch change.Type {
		case models.DMLInsertType:
			queries = append(queries, buildInsertQuery(formattedTable, change.Values, db))
		case models.DMLUpdateType:
			queries = append(queries, buildUpdateQuery(formattedTable, change.Values, change.PrimaryKeyInfo, db))
		case models.DMLDeleteType:
			queries = append(queries, buildDeleteQuery(formattedTable, change.PrimaryKeyInfo, db))
		}
	}

	if db.family == owlFamilyPostgres {
		conn, cleanup, err := db.editorConnectionFor(ctx, target)
		if err != nil {
			return err
		}
		defer cleanup()
		pool, ok := conn.(*sql.DB)
		if !ok {
			return errors.New("transactional changes require a pool connection")
		}
		return queriesInTransaction(ctx, pool, queries)
	}

	return queriesInTransaction(ctx, db.Connection, queries)
}

func (db *OwlJDBC) GetPrimaryKeyColumnNames(ctx context.Context, database, table string) ([]string, error) {
	ctx = contextOrBackground(ctx)
	if database == "" {
		return nil, errors.New("database name is required")
	}
	if table == "" {
		return nil, errors.New("table name is required")
	}

	conn, cleanup, err := db.editorConnectionFor(ctx, database)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	var rows *sql.Rows
	switch db.family {
	case owlFamilyMysql:
		_, tableName, tableErr := owlJdbcSplitPlainTable(table)
		if tableErr != nil {
			return nil, tableErr
		}
		rows, err = conn.QueryContext(ctx, "SELECT column_name FROM information_schema.key_column_usage WHERE table_schema = ? AND table_name = ? AND constraint_name = ?", database, tableName, "PRIMARY")
	case owlFamilyOracle:
		_, tableName, tableErr := owlJdbcSplitPlainTable(table)
		if tableErr != nil {
			return nil, tableErr
		}
		rows, err = conn.QueryContext(ctx, `
			SELECT cc.column_name
			FROM all_constraints c
			JOIN all_cons_columns cc ON cc.owner = c.owner AND cc.constraint_name = c.constraint_name
			WHERE c.constraint_type = 'P' AND c.owner = ? AND c.table_name = ?
			ORDER BY cc.position
		`, database, tableName)
	default:
		parts := strings.SplitN(table, ".", 2)
		if len(parts) != 2 {
			return nil, errors.New("table must be in the format schema.table")
		}
		rows, err = conn.QueryContext(ctx, `
			SELECT a.attname AS column_name
			FROM pg_index i
			JOIN pg_class c ON c.oid = i.indrelid
			JOIN pg_attribute a ON a.attrelid = c.oid AND a.attnum = ANY (i.indkey)
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE relname = ? AND nspname = ? AND indisprimary
		`, parts[1], parts[0])
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var primaryKeyColumnNames []string
	for rows.Next() {
		var colName string
		if err := rows.Scan(&colName); err != nil {
			return nil, err
		}
		primaryKeyColumnNames = append(primaryKeyColumnNames, colName)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return primaryKeyColumnNames, nil
}

func (db *OwlJDBC) FormatArg(arg any, colType models.CellValueType) any {
	if colType == models.Null {
		return sql.NullString{String: "", Valid: false}
	}
	if colType == models.Empty {
		return ""
	}
	if colType == models.String {
		switch v := arg.(type) {
		case int, int64:
			return fmt.Sprintf("%d", v)
		case float64, float32:
			s := fmt.Sprintf("%f", v)
			trimmed := strings.TrimRight(s, "0")
			if strings.HasSuffix(trimmed, ".") {
				trimmed += "0"
			}
			return trimmed
		case string:
			return v
		case []byte:
			return string(v)
		case nil:
			return sql.NullString{String: "", Valid: false}
		default:
			return fmt.Sprintf("%v", v)
		}
	}
	return fmt.Sprintf("%v", arg)
}

func (db *OwlJDBC) FormatArgForQueryString(arg any) string {
	switch v := arg.(type) {
	case string:
		if v == "NULL" || v == "DEFAULT" {
			return v
		}
		escaped := strings.ReplaceAll(v, "'", "''")
		return "'" + escaped + "'"
	case sql.NullString:
		if !v.Valid {
			return "NULL"
		}
		escaped := strings.ReplaceAll(v.String, "'", "''")
		return "'" + escaped + "'"
	case int, int64:
		return fmt.Sprintf("%d", v)
	case float64, float32:
		s := fmt.Sprintf("%f", v)
		trimmed := strings.TrimRight(s, "0")
		if strings.HasSuffix(trimmed, ".") {
			trimmed += "0"
		}
		return trimmed
	case nil:
		return "NULL"
	default:
		return fmt.Sprintf("%v", v)
	}
}

// FormatReference quotes identifiers per family: backticks on the mysql wire,
// double quotes on postgres and oracle wires.
func (db *OwlJDBC) FormatReference(reference string) string {
	if db.family == owlFamilyMysql {
		return "`" + reference + "`"
	}
	return `"` + reference + `"`
}

// FormatPlaceholder returns the JDBC-standard ? placeholder for every family:
// the owljdbc bridge binds arguments positionally and rewrites family-specific
// placeholder syntax itself.
func (db *OwlJDBC) FormatPlaceholder(_ int) string {
	return "?"
}

func (db *OwlJDBC) DMLChangeToQueryString(change models.DBDMLChange) (string, error) {
	formattedTable, err := db.qualifiedTable(change.Database, change.Table)
	if err != nil {
		return "", err
	}

	columnNames, values := getColNamesAndArgsAsString(change.Values)

	switch change.Type {
	case models.DMLInsertType:
		return buildInsertQueryString(formattedTable, columnNames, values, db), nil
	case models.DMLUpdateType:
		return buildUpdateQueryString(formattedTable, columnNames, values, change.PrimaryKeyInfo, db), nil
	case models.DMLDeleteType:
		return buildDeleteQueryString(formattedTable, change.PrimaryKeyInfo, db), nil
	}
	return "", nil
}

// owlJdbcScanOne runs a single-row query over an editorConnection (which only
// exposes QueryContext/ExecContext) and scans the first row. Returns
// sql.ErrNoRows when the query has no rows.
func owlJdbcScanOne(ctx context.Context, conn editorConnection, query string, dest []any, args ...any) error {
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return sql.ErrNoRows
	}
	if err := rows.Scan(dest...); err != nil {
		return err
	}
	return rows.Err()
}

// owlJdbcScanMode controls the NULL/empty-string sentinels a result shape
// expects, matching the conventions of the native drivers.
type owlJdbcScanMode int

const (
	owlScanPlain  owlJdbcScanMode = iota // NULL renders as ""
	owlScanPage                          // NULL renders as "NULL&", "" as "EMPTY&"
	owlScanStream                        // NULL renders as "NULL"
)

// owlJdbcScanRow scans one row of typed bridge values into display strings.
// Unlike the native text-protocol drivers, owljdbc returns real Go types
// (int64, float64, time.Time, ...), so RawBytes scanning is not an option.
func owlJdbcScanRow(rows *sql.Rows, columnCount int, mode owlJdbcScanMode) ([]string, error) {
	values := make([]any, columnCount)
	pointers := make([]any, columnCount)
	for i := range values {
		pointers[i] = &values[i]
	}

	if err := rows.Scan(pointers...); err != nil {
		return nil, err
	}

	row := make([]string, columnCount)
	for i, value := range values {
		row[i] = owlJdbcValueString(value, mode)
	}
	return row, nil
}

// owlJdbcStreamScanRow adapts owlJdbcScanRow to the shared query streamer's
// scanRow contract.
func owlJdbcStreamScanRow(rows *sql.Rows, columnCount int) ([]string, error) {
	return owlJdbcScanRow(rows, columnCount, owlScanStream)
}

// owlJdbcScanAll scans a full result into the [][]string header + rows shape
// the table metadata getters return.
func owlJdbcScanAll(rows *sql.Rows, mode owlJdbcScanMode) ([][]string, error) {
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}

	results := [][]string{columns}
	for rows.Next() {
		row, err := owlJdbcScanRow(rows, len(columns), mode)
		if err != nil {
			return nil, err
		}
		results = append(results, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

func owlJdbcValueString(value any, mode owlJdbcScanMode) string {
	if value == nil {
		switch mode {
		case owlScanPage:
			return "NULL&"
		case owlScanStream:
			return "NULL"
		default:
			return ""
		}
	}

	switch v := value.(type) {
	case string:
		if mode == owlScanPage && v == "" {
			return "EMPTY&"
		}
		return v
	case []byte:
		return string(v)
	case time.Time:
		return v.Format("2006-01-02 15:04:05.999999999")
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(v), 'f', -1, 32)
	case bool:
		if v {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprintf("%v", value)
	}
}

var (
	_ Driver                = (*OwlJDBC)(nil)
	_ BulkTableColumnLoader = (*OwlJDBC)(nil)
	_ QueryStreamer         = (*OwlJDBC)(nil)
)
