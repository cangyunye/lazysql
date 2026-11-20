package drivers

import (
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/cangyunye/owljdbc"
	"github.com/xo/dburl"
)

// owljdbc scheme wiring. The owljdbc driver talks to JDBC-only backends
// (openGauss, OceanBase, Oracle, ...) through a JVM sidecar that loads the
// vendor JDBC jar. Registering a dburl scheme lets "owljdbc" URLs flow through
// the same dburl.Parse -> sql.Open("owljdbc", dsn) path as every other driver;
// the generator turns the URL into the JSON DSN owljdbc expects at parse time.
func init() {
	// owljdbc's builtin profile for openGauss is named "opengaussdb"; register
	// the friendlier "opengauss" name, mirroring owlsql's real-instance setup.
	if err := owljdbc.RegisterSpec("opengauss", owljdbc.ProfileSpec{
		DriverClass: "org.opengauss.Driver",
		JarGlobs:    []string{"opengauss-jdbc-*.jar"},
		Family:      "postgres",
		URLTemplate: "jdbc:opengauss://{host}:{port}/{database}",
	}); err != nil {
		panic(fmt.Sprintf("owljdbc: register opengauss profile: %v", err))
	}

	dburl.Register(dburl.Scheme{
		Driver: DriverOwlJDBC,
		// Aliases preselect a dbType: opengauss:// -> opengauss, oceanbase:// ->
		// oceanbase-mysql (mode=oracle -> oceanbase-oracle). The generic
		// owljdbc:// scheme always requires an explicit ?dbType=.
		Aliases:   []string{"opengauss", "og", "oceanbase", "ob"},
		Override:  DriverOwlJDBC,
		Transport: dburl.TransportTCP,
		Generator: func(u *dburl.URL) (string, string, error) {
			dsn, err := buildOwlJdbcDSN(u)
			return dsn, "", err
		},
	})
}

// owlJdbcSchemeDBType maps an owljdbc scheme alias to its default dbType.
// OceanBase's single jar serves both tenant modes, so mode=oracle selects the
// Oracle-mode profile while everything else defaults to the MySQL mode.
func owlJdbcSchemeDBType(scheme, mode string) (string, error) {
	switch strings.ToLower(scheme) {
	case "opengauss", "og":
		return "opengauss", nil
	case "oceanbase", "ob":
		if strings.EqualFold(mode, "oracle") {
			return "oceanbase-oracle", nil
		}
		return "oceanbase-mysql", nil
	case DriverOwlJDBC:
		return "", nil
	}
	return "", fmt.Errorf("owljdbc: scheme %q is not an owljdbc alias", scheme)
}

// buildOwlJdbcDSN converts a parsed owljdbc URL into the JSON DSN the owljdbc
// database/sql driver expects.
//
//	owljdbc://user:pass@host:port/db?dbType=oceanbase-oracle
//	opengauss://user:pass@host:6432/og_db
//	oceanbase://root%40sys:pass@host:2881/test?mode=oracle
//
// Credentials may ride in the URL userinfo or in user=/password= query
// parameters (query wins), which spares OceanBase tenant usernames such as
// root@sys any escaping. Jars resolve from jarsDir/agentJar params, then
// OWLJDBC_JARS_DIR/OWLJDBC_AGENT_JAR, then a "jars" directory in the working
// directory (owljdbc globs owl-agent*.jar and the profile's driver jars there).
func buildOwlJdbcDSN(u *dburl.URL) (string, error) {
	q := u.Query()
	mode := owlJdbcQueryValue(q, "mode", "dialect")

	dbType := owlJdbcQueryValue(q, "dbType", "db_type", "type")
	if dbType == "" {
		var err error
		if dbType, err = owlJdbcSchemeDBType(u.Scheme, mode); err != nil {
			return "", err
		}
	}
	if dbType == "" {
		return "", fmt.Errorf("owljdbc: URL needs ?dbType= (e.g. dbType=oceanbase-mysql) or an owljdbc scheme alias (opengauss://, oceanbase://)")
	}
	if !owljdbc.HasProfile(dbType) {
		return "", fmt.Errorf("owljdbc: unknown dbType %q (known: %s)", dbType, strings.Join(owljdbc.ProfileTypes(), ", "))
	}

	user := owlJdbcQueryValue(q, "user", "username")
	password := owlJdbcQueryValue(q, "password", "pass")
	if u.User != nil {
		if user == "" {
			user = u.User.Username()
		}
		if password == "" {
			if p, ok := u.User.Password(); ok {
				password = p
			}
		}
	}

	jarsDir := firstNonEmpty(
		owlJdbcQueryValue(q, "jarsDir", "jars_dir", "jars"),
		os.Getenv("OWLJDBC_JARS_DIR"),
		"jars",
	)
	agentJar := firstNonEmpty(
		owlJdbcQueryValue(q, "agentJar", "agent_jar", "agent"),
		os.Getenv("OWLJDBC_AGENT_JAR"),
	)
	javaHome := owlJdbcQueryValue(q, "java", "javaHome", "java_home")
	jdbcURL := owlJdbcQueryValue(q, "jdbcUrl", "jdbc_url", "jdbcurl")

	cfg, err := owljdbc.BuildConfig(dbType, owljdbc.Endpoint{
		Host:     u.Hostname(),
		Port:     u.Port(),
		User:     user,
		Password: password,
		Database: strings.TrimPrefix(u.Path, "/"),
	}, "", jarsDir, agentJar, javaHome)
	if err != nil {
		return "", err
	}
	if jdbcURL != "" {
		cfg.URL = jdbcURL
	}
	return owljdbc.EncodeDSN(cfg), nil
}

// owlJdbcURLInfo carries what the driver needs beyond the standard DSN: the
// dialect hints and jar settings that ride in the URL query, plus the endpoint
// pieces the generator consumed (needed again for SwitchDatabase rebuilds).
type owlJdbcURLInfo struct {
	Config   owljdbc.Config   // decoded JSON DSN (family, credentials, jars)
	Endpoint owljdbc.Endpoint // host/port/user/password/database from the URL
	DBType   string           // resolved catalog type (opengauss, oceanbase-mysql, ...)
	Mode     string           // metadata dialect: mysql | postgresql | oracle
	JdbcURL  string           // custom JDBC URL override ("" = profile-built URL)
	JarsDir  string
	AgentJar string
	JavaHome string
	JvmOpts  []string // sidecar JVM options (e.g. -Xmx256m) from the jvmOpts param
}

// owlJdbcResolveURL parses an owljdbc URL and decodes every piece the driver
// side needs. It is called per Connect, so jar re-resolution tracks whatever
// URL is handed in.
func owlJdbcResolveURL(urlstr string) (owlJdbcURLInfo, error) {
	parsed, err := dburl.Parse(urlstr)
	if err != nil {
		return owlJdbcURLInfo{}, err
	}
	cfg, err := owljdbc.DecodeDSN(parsed.DSN)
	if err != nil {
		return owlJdbcURLInfo{}, err
	}

	q := parsed.Query()
	mode := owlJdbcModeFor(cfg.Family, owlJdbcQueryValue(q, "mode", "dialect"))

	dbType := owlJdbcQueryValue(q, "dbType", "db_type", "type")
	if dbType == "" {
		var err error
		if dbType, err = owlJdbcSchemeDBType(parsed.Scheme, mode); err != nil {
			return owlJdbcURLInfo{}, err
		}
	}

	user, password := cfg.User, cfg.Password
	if u := parsed.User; u != nil {
		if user == "" {
			user = u.Username()
		}
		if password == "" {
			if p, ok := u.Password(); ok {
				password = p
			}
		}
	}

	return owlJdbcURLInfo{
		Config: cfg,
		Endpoint: owljdbc.Endpoint{
			Host:     parsed.Hostname(),
			Port:     parsed.Port(),
			User:     user,
			Password: password,
			Database: strings.TrimPrefix(parsed.Path, "/"),
		},
		DBType:   dbType,
		Mode:     mode,
		JdbcURL:  owlJdbcQueryValue(q, "jdbcUrl", "jdbc_url", "jdbcurl"),
		JarsDir:  firstNonEmpty(owlJdbcQueryValue(q, "jarsDir", "jars_dir", "jars"), os.Getenv("OWLJDBC_JARS_DIR"), "jars"),
		AgentJar: firstNonEmpty(owlJdbcQueryValue(q, "agentJar", "agent_jar", "agent"), os.Getenv("OWLJDBC_AGENT_JAR")),
		JavaHome: owlJdbcQueryValue(q, "java", "javaHome", "java_home"),
		JvmOpts:  owlJdbcSplitJvmOpts(owlJdbcQueryValue(q, "jvmOpts", "jvm_opts")),
	}, nil
}

// owlJdbcSplitJvmOpts parses the comma-separated jvmOpts parameter
// ("-Xmx256m,-XX:+UseSerialGC"); values are passed verbatim to the sidecar JVM.
func owlJdbcSplitJvmOpts(raw string) []string {
	if raw == "" {
		return nil
	}
	var opts []string
	for _, part := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			opts = append(opts, trimmed)
		}
	}
	return opts
}

// owlJdbcModeFor maps the transport family to its metadata dialect, honoring
// an explicit override. The override exists for openGauss compatibility
// instances, where a postgres-wire connection can speak the mysql dialect.
func owlJdbcModeFor(family, override string) string {
	switch strings.ToLower(override) {
	case "mysql":
		return "mysql"
	case "postgresql", "postgres":
		return "postgresql"
	case "oracle":
		return "oracle"
	}
	switch family {
	case "mysql":
		return "mysql"
	case "oracle":
		return "oracle"
	default:
		return "postgresql"
	}
}

func owlJdbcQueryValue(q url.Values, names ...string) string {
	for _, name := range names {
		if v := q.Get(name); v != "" {
			return v
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
