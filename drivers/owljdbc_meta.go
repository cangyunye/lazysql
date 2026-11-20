package drivers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Metadata methods for the OwlJDBC driver, dispatched by transport family:
// the postgres family (openGauss/PostgreSQL) walks pg_catalog and
// information_schema, the mysql family (OceanBase MySQL tenants/MySQL) uses
// SHOW statements and information_schema, and the oracle family
// (OceanBase Oracle tenants/Oracle) uses the all_* dictionary. Queries bind
// exact owner/table names taken from the tree, so identifier storage case
// (OB oracle lower, Oracle upper) needs no special handling.

func (db *OwlJDBC) GetDatabases(ctx context.Context) ([]string, error) {
	ctx = contextOrBackground(ctx)

	conn, cleanup, err := db.editorConnectionFor(ctx, "")
	if err != nil {
		return nil, err
	}
	defer cleanup()

	var rows *sql.Rows
	switch db.family {
	case owlFamilyMysql:
		rows, err = conn.QueryContext(ctx, "SHOW DATABASES")
	case owlFamilyOracle:
		rows, err = conn.QueryContext(ctx, "SELECT username FROM all_users ORDER BY username")
	default:
		rows, err = conn.QueryContext(ctx, "SELECT datname FROM pg_database WHERE datallowconn AND has_database_privilege(current_user, datname, 'CONNECT')")
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var databases []string
	for rows.Next() {
		var database string
		if err := rows.Scan(&database); err != nil {
			return nil, err
		}
		if db.family == owlFamilyMysql {
			switch database {
			case "information_schema", "mysql", "performance_schema", "sys":
				continue
			}
		}
		databases = append(databases, database)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return databases, nil
}

func (db *OwlJDBC) GetTables(ctx context.Context, database string) (map[string][]string, error) {
	ctx = contextOrBackground(ctx)
	if database == "" {
		return nil, errors.New("database name is required")
	}

	conn, cleanup, err := db.editorConnectionFor(ctx, database)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	tables := make(map[string][]string)

	switch db.family {
	case owlFamilyMysql:
		rows, err := conn.QueryContext(ctx, fmt.Sprintf("SHOW TABLES FROM `%s`", database))
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var table string
			if err := rows.Scan(&table); err != nil {
				return nil, err
			}
			tables[database] = append(tables[database], table)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	case owlFamilyOracle:
		rows, err := conn.QueryContext(ctx, "SELECT table_name FROM all_tables WHERE owner = ? ORDER BY table_name", database)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var table string
			if err := rows.Scan(&table); err != nil {
				return nil, err
			}
			tables[database] = append(tables[database], table)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	default:
		rows, err := conn.QueryContext(ctx, "SELECT table_name, table_schema FROM information_schema.tables WHERE table_catalog = ?", database)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				tableName   string
				tableSchema string
			)
			if err := rows.Scan(&tableName, &tableSchema); err != nil {
				return nil, err
			}
			tables[tableSchema] = append(tables[tableSchema], tableName)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	return tables, nil
}

func (db *OwlJDBC) GetTableColumns(ctx context.Context, database, table string) ([][]string, error) {
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
		rows, err = conn.QueryContext(ctx, "SHOW FULL COLUMNS FROM `"+database+"`.`"+table+"`")
	case owlFamilyOracle:
		_, tableName, splitErr := owlJdbcSplitPlainTable(table)
		if splitErr != nil {
			return nil, splitErr
		}
		rows, err = conn.QueryContext(ctx, `
			SELECT c.column_name, c.data_type, c.nullable, c.data_default, cm.comments AS "comment"
			FROM all_tab_columns c
			LEFT JOIN all_col_comments cm
				ON cm.owner = c.owner AND cm.table_name = c.table_name AND cm.column_name = c.column_name
			WHERE c.owner = ? AND c.table_name = ?
			ORDER BY c.column_id
		`, database, tableName)
	default:
		parts := strings.Split(table, ".")
		if len(parts) != 2 {
			return nil, errors.New("table must be in the format schema.table")
		}
		rows, err = conn.QueryContext(ctx, `
			SELECT c.column_name, c.data_type, c.is_nullable, c.column_default, COALESCE(pd.description, '') as comment
			FROM information_schema.columns c
			LEFT JOIN pg_class pc ON pc.relname = c.table_name
				AND pc.relnamespace = (SELECT oid FROM pg_namespace WHERE nspname = c.table_schema)
			LEFT JOIN pg_namespace pn ON pn.nspname = c.table_schema AND pn.oid = pc.relnamespace
			LEFT JOIN pg_description pd ON pd.objoid = pc.oid AND pd.objsubid = c.ordinal_position
			WHERE c.table_catalog = ? AND c.table_schema = ? AND c.table_name = ?
			ORDER BY c.ordinal_position
		`, database, parts[0], parts[1])
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return owlJdbcScanAll(rows, owlScanPlain)
}

func (db *OwlJDBC) GetConstraints(ctx context.Context, database, table string) ([][]string, error) {
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
		rows, err = conn.QueryContext(ctx, "SELECT CONSTRAINT_NAME, COLUMN_NAME, REFERENCED_TABLE_NAME, REFERENCED_COLUMN_NAME FROM information_schema.KEY_COLUMN_USAGE WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?", database, table)
	case owlFamilyOracle:
		_, tableName, splitErr := owlJdbcSplitPlainTable(table)
		if splitErr != nil {
			return nil, splitErr
		}
		rows, err = conn.QueryContext(ctx, `
			SELECT cc.constraint_name, cc.column_name, c.constraint_type
			FROM all_constraints c
			JOIN all_cons_columns cc ON cc.owner = c.owner AND cc.constraint_name = c.constraint_name
			WHERE c.owner = ? AND c.table_name = ?
			ORDER BY c.constraint_type, cc.constraint_name, cc.position
		`, database, tableName)
	default:
		parts := strings.Split(table, ".")
		if len(parts) != 2 {
			return nil, errors.New("table must be in the format schema.table")
		}
		rows, err = conn.QueryContext(ctx, `
			SELECT
				tc.constraint_name,
				kcu.column_name,
				tc.constraint_type
			FROM
				information_schema.table_constraints AS tc
				JOIN information_schema.key_column_usage AS kcu ON tc.constraint_name = kcu.constraint_name
					AND tc.table_schema = kcu.table_schema
				JOIN information_schema.constraint_column_usage AS ccu ON ccu.constraint_name = tc.constraint_name
					AND ccu.table_schema = tc.table_schema
			WHERE
				NOT tc.constraint_type = 'FOREIGN KEY'
				AND tc.table_schema = ?
				AND tc.table_name = ?
		`, parts[0], parts[1])
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return owlJdbcScanAll(rows, owlScanPlain)
}

// GetForeignKeys returns the foreign keys declared on the table.
func (db *OwlJDBC) GetForeignKeys(ctx context.Context, database, table string) ([][]string, error) {
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
		rows, err = owlJdbcMySQLForeignKeys(ctx, conn, database, table)
	case owlFamilyOracle:
		_, tableName, splitErr := owlJdbcSplitPlainTable(table)
		if splitErr != nil {
			return nil, splitErr
		}
		rows, err = conn.QueryContext(ctx, `
			SELECT cc.constraint_name, cc.column_name, rc.owner AS foreign_table_schema,
				rc.table_name AS foreign_table_name, rcc.column_name AS foreign_column_name
			FROM all_constraints c
			JOIN all_cons_columns cc ON cc.owner = c.owner AND cc.constraint_name = c.constraint_name
			JOIN all_constraints rc ON rc.owner = c.r_owner AND rc.constraint_name = c.r_constraint_name
			JOIN all_cons_columns rcc ON rcc.owner = rc.owner AND rcc.constraint_name = rc.constraint_name
				AND NVL(rcc.position, 1) = NVL(cc.position, 1)
			WHERE c.constraint_type = 'R' AND c.owner = ? AND c.table_name = ?
			ORDER BY cc.constraint_name, cc.position
		`, database, tableName)
	default:
		parts := strings.Split(table, ".")
		if len(parts) != 2 {
			return nil, errors.New("table must be in the format schema.table")
		}
		// information_schema-based FK lookup: portable across real PostgreSQL
		// and openGauss, whose pg_constraint lacks the newer LATERAL unnest
		// shape the native postgres driver relies on.
		rows, err = conn.QueryContext(ctx, `
			SELECT
				tc.constraint_name,
				kcu.column_name,
				ccu.table_schema AS foreign_table_schema,
				ccu.table_name AS foreign_table_name,
				ccu.column_name AS foreign_column_name
			FROM
				information_schema.table_constraints AS tc
				JOIN information_schema.key_column_usage AS kcu ON tc.constraint_name = kcu.constraint_name
					AND tc.table_schema = kcu.table_schema
				JOIN information_schema.constraint_column_usage AS ccu ON ccu.constraint_name = tc.constraint_name
					AND ccu.table_schema = tc.table_schema
			WHERE
				tc.constraint_type = 'FOREIGN KEY'
				AND tc.table_schema = ?
				AND tc.table_name = ?
			ORDER BY tc.constraint_name, kcu.ordinal_position
		`, parts[0], parts[1])
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return owlJdbcScanAll(rows, owlScanPlain)
}

// owlJdbcMySQLForeignKeys mirrors the native MySQL driver's InnoDB dictionary
// fast paths with the KEY_COLUMN_USAGE fallback (the OceanBase MySQL dialect
// lacks the InnoDB dictionary, so the fallback carries it).
func owlJdbcMySQLForeignKeys(ctx context.Context, conn editorConnection, database, table string) (*sql.Rows, error) {
	for _, fastPath := range mysqlForeignKeysFastPathQueries {
		rows, err := conn.QueryContext(ctx, fastPath.query, database, table)
		if err == nil {
			return rows, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return conn.QueryContext(ctx, mysqlForeignKeysFallbackQuery, database, table)
}

// GetReferencingTables returns every foreign key that points at the given
// table, i.e. the reverse direction of GetForeignKeys.
func (db *OwlJDBC) GetReferencingTables(ctx context.Context, database, table string) ([][]string, error) {
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
		rows, err = conn.QueryContext(ctx, `
			SELECT CONSTRAINT_NAME, TABLE_SCHEMA, TABLE_NAME, COLUMN_NAME, REFERENCED_COLUMN_NAME
			FROM information_schema.KEY_COLUMN_USAGE
			WHERE REFERENCED_TABLE_SCHEMA = ? AND REFERENCED_TABLE_NAME = ?
			ORDER BY TABLE_SCHEMA, TABLE_NAME, CONSTRAINT_NAME, ORDINAL_POSITION
		`, database, table)
	case owlFamilyOracle:
		_, tableName, splitErr := owlJdbcSplitPlainTable(table)
		if splitErr != nil {
			return nil, splitErr
		}
		rows, err = conn.QueryContext(ctx, `
			SELECT c.constraint_name, c.owner AS table_schema, c.table_name,
				cc.column_name, rcc.column_name AS referenced_column_name
			FROM all_constraints c
			JOIN all_constraints rc ON rc.owner = c.r_owner AND rc.constraint_name = c.r_constraint_name
			JOIN all_cons_columns cc ON cc.owner = c.owner AND cc.constraint_name = c.constraint_name
			JOIN all_cons_columns rcc ON rcc.owner = rc.owner AND rcc.constraint_name = rc.constraint_name
				AND NVL(rcc.position, 1) = NVL(cc.position, 1)
			WHERE c.constraint_type = 'R' AND c.r_owner = ? AND rc.table_name = ?
			ORDER BY c.owner, c.table_name, c.constraint_name, cc.position
		`, database, tableName)
	default:
		parts := strings.Split(table, ".")
		if len(parts) != 2 {
			return nil, errors.New("table must be in the format schema.table")
		}
		rows, err = conn.QueryContext(ctx, `
			SELECT
				tc.constraint_name,
				kcu.table_schema AS table_schema,
				kcu.table_name,
				kcu.column_name,
				ccu.column_name AS referenced_column_name
			FROM
				information_schema.table_constraints AS tc
				JOIN information_schema.key_column_usage AS kcu ON tc.constraint_name = kcu.constraint_name
					AND tc.table_schema = kcu.table_schema
				JOIN information_schema.constraint_column_usage AS ccu ON ccu.constraint_name = tc.constraint_name
					AND ccu.table_schema = tc.table_schema
			WHERE
				tc.constraint_type = 'FOREIGN KEY'
				AND ccu.table_schema = ?
				AND ccu.table_name = ?
			ORDER BY kcu.table_schema, kcu.table_name, tc.constraint_name, kcu.ordinal_position
		`, parts[0], parts[1])
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return owlJdbcScanAll(rows, owlScanPlain)
}

func (db *OwlJDBC) GetIndexes(ctx context.Context, database, table string) ([][]string, error) {
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
		rows, err = conn.QueryContext(ctx, "SHOW INDEX FROM `"+database+"`.`"+table+"`")
	case owlFamilyOracle:
		_, tableName, splitErr := owlJdbcSplitPlainTable(table)
		if splitErr != nil {
			return nil, splitErr
		}
		rows, err = conn.QueryContext(ctx, `
			SELECT i.index_name, ic.column_name, i.uniqueness AS "type"
			FROM all_indexes i
			JOIN all_ind_columns ic ON ic.index_owner = i.owner AND ic.index_name = i.index_name
			WHERE i.owner = ? AND i.table_name = ?
			ORDER BY i.index_name, ic.column_position
		`, database, tableName)
	default:
		parts := strings.Split(table, ".")
		if len(parts) != 2 {
			return nil, errors.New("table must be in the format schema.table")
		}
		rows, err = conn.QueryContext(ctx, `
			SELECT
				i.relname AS index_name,
				a.attname AS column_name,
				am.amname AS type
			FROM
				pg_namespace n,
				pg_class t,
				pg_class i,
				pg_index ix,
				pg_attribute a,
				pg_am am
			WHERE
				t.oid = ix.indrelid
				AND i.oid = ix.indexrelid
				AND a.attrelid = t.oid
				AND a.attnum = ANY(ix.indkey)
				AND t.relkind = 'r'
				AND am.oid = i.relam
				AND n.oid = t.relnamespace
				AND n.nspname = ?
				AND t.relname = ?
			ORDER BY t.relname, i.relname
		`, parts[0], parts[1])
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return owlJdbcScanAll(rows, owlScanPlain)
}

func (db *OwlJDBC) GetFunctions(ctx context.Context, database string) (map[string][]string, error) {
	return db.owlJdbcRoutines(ctx, database, "FUNCTION")
}

func (db *OwlJDBC) GetProcedures(ctx context.Context, database string) (map[string][]string, error) {
	return db.owlJdbcRoutines(ctx, database, "PROCEDURE")
}

// owlJdbcRoutines lists functions or procedures per family: information_schema
// routines on postgres/mysql wires, all_objects on the oracle wire. The result
// is keyed by database; schema-aware (postgres family) names are qualified.
func (db *OwlJDBC) owlJdbcRoutines(ctx context.Context, database, kind string) (map[string][]string, error) {
	ctx = contextOrBackground(ctx)
	if database == "" {
		return nil, errors.New("database name is required")
	}

	conn, cleanup, err := db.editorConnectionFor(ctx, database)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	result := make(map[string][]string)

	switch db.family {
	case owlFamilyMysql:
		rows, err := conn.QueryContext(ctx, `
			SELECT routine_name
			FROM information_schema.routines
			WHERE routine_schema = ? AND routine_type = ?
			ORDER BY routine_name
		`, database, kind)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return nil, err
			}
			result[database] = append(result[database], name)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	case owlFamilyOracle:
		rows, err := conn.QueryContext(ctx, `
			SELECT object_name
			FROM all_objects
			WHERE owner = ? AND object_type = ?
			ORDER BY object_name
		`, database, kind)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return nil, err
			}
			result[database] = append(result[database], name)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	default:
		rows, err := conn.QueryContext(ctx, `
			SELECT routine_schema || '.' || routine_name
			FROM information_schema.routines
			WHERE routine_schema NOT IN ('pg_catalog', 'information_schema') AND routine_type = ?
			ORDER BY routine_schema, routine_name
		`, kind)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return nil, err
			}
			result[database] = append(result[database], name)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	return result, nil
}

func (db *OwlJDBC) GetViews(ctx context.Context, database string) (map[string][]string, error) {
	ctx = contextOrBackground(ctx)
	if database == "" {
		return nil, errors.New("database name is required")
	}

	conn, cleanup, err := db.editorConnectionFor(ctx, database)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	views := make(map[string][]string)

	switch db.family {
	case owlFamilyMysql:
		rows, err := conn.QueryContext(ctx, `
			SELECT table_name
			FROM information_schema.views
			WHERE table_schema = ?
			ORDER BY table_name
		`, database)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return nil, err
			}
			views[database] = append(views[database], name)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	case owlFamilyOracle:
		rows, err := conn.QueryContext(ctx, `
			SELECT view_name
			FROM all_views
			WHERE owner = ?
			ORDER BY view_name
		`, database)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return nil, err
			}
			views[database] = append(views[database], name)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	default:
		rows, err := conn.QueryContext(ctx, `
			SELECT table_schema || '.' || table_name
			FROM information_schema.views
			WHERE table_catalog = ?
			AND table_schema NOT IN ('pg_catalog', 'information_schema')
			ORDER BY table_schema, table_name
		`, database)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				return nil, err
			}
			views[database] = append(views[database], name)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	return views, nil
}

func (db *OwlJDBC) GetFunctionDefinition(ctx context.Context, database, name string) (string, error) {
	return db.owlJdbcRoutineOrViewDefinition(ctx, database, name, owlRoutineFunction)
}

func (db *OwlJDBC) GetProcedureDefinition(ctx context.Context, database, name string) (string, error) {
	return db.owlJdbcRoutineOrViewDefinition(ctx, database, name, owlRoutineProcedure)
}

func (db *OwlJDBC) GetViewDefinition(ctx context.Context, database, name string) (string, error) {
	return db.owlJdbcRoutineOrViewDefinition(ctx, database, name, owlRoutineView)
}

type owlJdbcDefinitionKind int

const (
	owlRoutineFunction owlJdbcDefinitionKind = iota
	owlRoutineProcedure
	owlRoutineView
)

// owlJdbcRoutineOrViewDefinition resolves function/procedure/view sources per
// family: pg_get_functiondef / pg_views on the postgres wire,
// information_schema on the mysql wire, all_source (line-per-row, joined in
// order) and DBMS_METADATA.GET_DDL on the oracle wire.
func (db *OwlJDBC) owlJdbcRoutineOrViewDefinition(ctx context.Context, database, name string, kind owlJdbcDefinitionKind) (string, error) {
	ctx = contextOrBackground(ctx)
	if database == "" {
		return "", errors.New("database name is required")
	}
	if name == "" {
		return "", errors.New("object name is required")
	}

	conn, cleanup, err := db.editorConnectionFor(ctx, database)
	if err != nil {
		return "", err
	}
	defer cleanup()

	switch db.family {
	case owlFamilyMysql:
		switch kind {
		case owlRoutineView:
			var definition sql.NullString
			err = owlJdbcScanOne(ctx, conn, `
				SELECT VIEW_DEFINITION
				FROM information_schema.views
				WHERE table_schema = ? AND table_name = ?
			`, []any{&definition}, database, name)
			if err != nil {
				return "", err
			}
			if !definition.Valid {
				return "", fmt.Errorf("view %s.%s not found", database, name)
			}
			return definition.String, nil
		default:
			routineType := "FUNCTION"
			if kind == owlRoutineProcedure {
				routineType = "PROCEDURE"
			}
			var definition sql.NullString
			err = owlJdbcScanOne(ctx, conn, `
				SELECT ROUTINE_DEFINITION
				FROM information_schema.routines
				WHERE routine_schema = ? AND routine_name = ? AND routine_type = ?
			`, []any{&definition}, database, name, routineType)
			if err != nil {
				return "", err
			}
			if !definition.Valid {
				return "", fmt.Errorf("routine %s.%s has no definition", database, name)
			}
			return definition.String, nil
		}

	case owlFamilyOracle:
		switch kind {
		case owlRoutineView:
			var ddl string
			err = owlJdbcScanOne(ctx, conn, fmt.Sprintf(
				"SELECT DBMS_METADATA.GET_DDL('VIEW', '%s', '%s') FROM DUAL",
				strings.ReplaceAll(name, "'", "''"),
				strings.ReplaceAll(database, "'", "''"),
			), []any{&ddl})
			if err != nil {
				return "", err
			}
			return ddl, nil
		default:
			sourceType := "FUNCTION"
			if kind == owlRoutineProcedure {
				sourceType = "PROCEDURE"
			}
			return owlJdbcSourceText(ctx, conn, database, name, sourceType)
		}

	default:
		switch kind {
		case owlRoutineView:
			parts := strings.SplitN(name, ".", 2)
			if len(parts) != 2 {
				return "", errors.New("view name must be in format schema.name")
			}
			var definition string
			err = owlJdbcScanOne(ctx, conn, `
				SELECT definition
				FROM pg_catalog.pg_views
				WHERE schemaname = ? AND viewname = ?
			`, []any{&definition}, parts[0], parts[1])
			if err != nil {
				return "", err
			}
			return definition, nil
		default:
			parts := strings.SplitN(name, ".", 2)
			if len(parts) != 2 {
				return "", errors.New("object name must be in format schema.name")
			}
			var definition string
			query := `
				SELECT pg_get_functiondef(p.oid)
				FROM pg_catalog.pg_proc p
				JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
				WHERE n.nspname = ? AND p.proname = ?
			`
			if kind == owlRoutineProcedure {
				query += " AND p.prokind = 'p'"
			}
			query += " LIMIT 1"
			err = owlJdbcScanOne(ctx, conn, query, []any{&definition}, parts[0], parts[1])
			if err != nil {
				return "", err
			}
			return definition, nil
		}
	}
}

// owlJdbcSourceText joins all_source's line-per-row text into one source
// string (the oracle wire stores function/procedure sources line by line).
func owlJdbcSourceText(ctx context.Context, conn editorConnection, owner, name, sourceType string) (string, error) {
	rows, err := conn.QueryContext(ctx, `
		SELECT text
		FROM all_source
		WHERE owner = ? AND name = ? AND type = ?
		ORDER BY line
	`, owner, name, sourceType)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var source strings.Builder
	found := false
	for rows.Next() {
		var line sql.NullString
		if err := rows.Scan(&line); err != nil {
			return "", err
		}
		found = true
		source.WriteString(line.String)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	if !found {
		return "", fmt.Errorf("%s %s.%s not found", strings.ToLower(sourceType), owner, name)
	}
	return source.String(), nil
}

// GetTableColumnsBulk loads column metadata for several tables with one
// catalog query, matching each family's GetTableColumns shape.
func (db *OwlJDBC) GetTableColumnsBulk(ctx context.Context, database string, tables []string) (map[string][][]string, error) {
	ctx = contextOrBackground(ctx)
	if database == "" {
		return nil, errors.New("database name is required")
	}
	if len(tables) == 0 {
		return map[string][][]string{}, nil
	}

	switch db.family {
	case owlFamilyMysql:
		return db.owlJdbcColumnsBulkMySQL(ctx, database, tables)
	case owlFamilyOracle:
		return db.owlJdbcColumnsBulkOracle(ctx, database, tables)
	default:
		return db.owlJdbcColumnsBulkPostgres(ctx, database, tables)
	}
}

func (db *OwlJDBC) owlJdbcColumnsBulkMySQL(ctx context.Context, database string, tables []string) (map[string][][]string, error) {
	columnHeaders := strings.Split("Field,Type,Collation,Null,Key,Default,Extra,Privileges,Comment", ",")
	results := newBulkColumnResults(tables, columnHeaders)
	placeholders := make([]string, len(tables))
	args := make([]any, 0, len(tables)+1)
	args = append(args, database)
	for i, table := range tables {
		placeholders[i] = "?"
		args = append(args, table)
	}

	query := `SELECT TABLE_NAME, COLUMN_NAME, COLUMN_TYPE, COLLATION_NAME,
		IS_NULLABLE, COLUMN_KEY, COLUMN_DEFAULT, EXTRA, PRIVILEGES, COLUMN_COMMENT
		FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME IN (` + strings.Join(placeholders, ", ") + `)
		ORDER BY TABLE_NAME, ORDINAL_POSITION`

	conn, cleanup, err := db.editorConnectionFor(ctx, database)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			tableName, columnName, columnType, collation sql.NullString
			isNullable, columnKey, columnDefault, extra  sql.NullString
			privileges, comment                          sql.NullString
		)
		if err := rows.Scan(
			&tableName, &columnName, &columnType, &collation,
			&isNullable, &columnKey, &columnDefault, &extra,
			&privileges, &comment,
		); err != nil {
			return nil, err
		}

		key := bulkResultKey(results, tableName.String)
		if key == "" {
			continue
		}
		results[key] = append(results[key], []string{
			columnName.String,
			columnType.String,
			collation.String,
			isNullable.String,
			columnKey.String,
			columnDefault.String,
			extra.String,
			privileges.String,
			comment.String,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

func (db *OwlJDBC) owlJdbcColumnsBulkOracle(ctx context.Context, database string, tables []string) (map[string][][]string, error) {
	columnHeaders := []string{"column_name", "data_type", "nullable", "data_default", "comment"}
	results := newBulkColumnResults(tables, columnHeaders)
	placeholders := make([]string, len(tables))
	args := make([]any, 0, len(tables)+1)
	args = append(args, database)
	for i, table := range tables {
		_, tableName, splitErr := owlJdbcSplitPlainTable(table)
		if splitErr != nil {
			return nil, splitErr
		}
		placeholders[i] = "?"
		args = append(args, tableName)
	}

	query := `SELECT c.table_name, c.column_name, c.data_type, c.nullable, c.data_default, cm.comments
		FROM all_tab_columns c
		LEFT JOIN all_col_comments cm
			ON cm.owner = c.owner AND cm.table_name = c.table_name AND cm.column_name = c.column_name
		WHERE c.owner = ? AND c.table_name IN (` + strings.Join(placeholders, ", ") + `)
		ORDER BY c.table_name, c.column_id`

	conn, cleanup, err := db.editorConnectionFor(ctx, database)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			tableName, columnName, dataType sql.NullString
			nullable, dataDefault, comment  sql.NullString
		)
		if err := rows.Scan(&tableName, &columnName, &dataType, &nullable, &dataDefault, &comment); err != nil {
			return nil, err
		}

		key := bulkResultKey(results, tableName.String)
		if key == "" {
			continue
		}
		results[key] = append(results[key], []string{
			columnName.String,
			dataType.String,
			nullable.String,
			dataDefault.String,
			comment.String,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

func (db *OwlJDBC) owlJdbcColumnsBulkPostgres(ctx context.Context, database string, tables []string) (map[string][][]string, error) {
	columnHeaders := strings.Split("column_name,data_type,is_nullable,column_default,comment", ",")
	results := newBulkColumnResults(tables, columnHeaders)
	filters := make([]string, 0, len(tables))
	args := make([]any, 0, len(tables)*2+1)
	args = append(args, database)
	for _, table := range tables {
		schema, tableName, ok := strings.Cut(table, ".")
		if !ok || schema == "" || tableName == "" || strings.Contains(tableName, ".") {
			return nil, fmt.Errorf("table must be in the format schema.table: %s", table)
		}
		args = append(args, schema, tableName)
		filters = append(filters, "(c.table_schema = ? AND c.table_name = ?)")
	}

	query := `SELECT c.table_schema, c.table_name, c.column_name, c.data_type,
		c.is_nullable, c.column_default, COALESCE(pd.description, '') AS comment
		FROM information_schema.columns c
		LEFT JOIN pg_class pc ON pc.relname = c.table_name
			AND pc.relnamespace = (SELECT oid FROM pg_namespace WHERE nspname = c.table_schema)
		LEFT JOIN pg_namespace pn ON pn.nspname = c.table_schema AND pn.oid = pc.relnamespace
		LEFT JOIN pg_description pd ON pd.objoid = pc.oid AND pd.objsubid = c.ordinal_position
		WHERE c.table_catalog = ? AND (` + strings.Join(filters, " OR ") + `)
		ORDER BY c.table_schema, c.table_name, c.ordinal_position`

	conn, cleanup, err := db.editorConnectionFor(ctx, database)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			schema, tableName, columnName, dataType sql.NullString
			isNullable, columnDefault, comment      sql.NullString
		)
		if err := rows.Scan(&schema, &tableName, &columnName, &dataType, &isNullable, &columnDefault, &comment); err != nil {
			return nil, err
		}

		key := bulkResultKey(results, schema.String+"."+tableName.String)
		if key == "" {
			continue
		}
		results[key] = append(results[key], []string{
			columnName.String,
			dataType.String,
			isNullable.String,
			columnDefault.String,
			comment.String,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return results, nil
}
