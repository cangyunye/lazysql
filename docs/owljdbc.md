# owljdbc provider: JDBC 通道接入 openGauss / OceanBase / Oracle 等

`owljdbc` 通过一个 JVM sidecar（`owl-agent.jar`）加载厂商 JDBC jar，把没有原生 Go 驱动的数据库接进 LazySQL。Go 侧走标准 `database/sql`（驱动 `github.com/cangyunye/owljdbc`），sidecar 一个 classpath 复用一个 JVM 进程，多连接只增加其上的逻辑会话。

## 支持的后端（dbType）

| dbType | 族（传输层） | 元数据方言 | 说明 |
| --- | --- | --- | --- |
| `opengauss` | postgres | postgresql | openGauss JDBC 6.x（`org.opengauss.Driver`）；owljdbc 内置还有 `opengaussdb` 同参条目 |
| `oceanbase-mysql` | mysql | mysql | OceanBase MySQL 模式租户 |
| `oceanbase-oracle` | oracle | oracle | OceanBase Oracle 模式租户（同一 jar，双租户） |
| `mysql` / `postgres` / `oracle` | mysql / postgres / oracle | 同族 | 原生 MySQL 8 / PostgreSQL / Oracle 19c 经 JDBC 接入 |
| `goldendb-*` / `dm` / `kingbase` / `timesten` | — | — | owljdbc 已登记但未实测，`owljdbc://` + `?dbType=` 可用，风险自负 |

族决定 SQL 方言假设与占位符适配（Go 侧统一用 `?` 占位符，owljdbc 桥自动适配各族）；元数据方言默认随族，可用 `mode=` 覆盖（如 openGauss mysql 兼容实例的高级用法，元数据查询本身仍按族分派）。

## 连接 URL

```
owljdbc://user:pass@host:port/database?dbType=oceanbase-mysql&jarsDir=jars
opengauss://user:pass@host:6432/og_pg            # 别名，等价 dbType=opengauss
oceanbase://root@sys:pass@host:2881/test         # 默认 oceanbase-mysql
oceanbase://u:p@host:2881/?mode=oracle           # → oceanbase-oracle
```

- scheme 别名：`opengauss`/`og` → opengauss；`oceanbase`/`ob` → oceanbase（`mode=oracle` 切 Oracle 租户）；`owljdbc` 需显式 `dbType`。
- 凭据优先级：`user=`/`password=` 查询参数 > URL userinfo。**OceanBase 租户用户名（`root@sys`）建议走查询参数**，避免 userinfo 转义（`root%40sys` 也可）。
- TOML 分字段形态（provider=`owljdbc` 且 URL 留空时由 `db_type` + Username/Password/Hostname/Port/DBName 自动拼 URL，免手工转义）：

```toml
[[database]]
Name = 'OceanBase obmysql'
Provider = 'owljdbc'
DBType = 'oceanbase-mysql'
Username = 'root@obmysql'
Password = '${env:OB_PASSWORD}'
Hostname = '172.20.214.44'
Port = '2881'
DBName = 'test'
```

## URL 参数

| 参数 | 说明 |
| --- | --- |
| `dbType` | owljdbc catalog 类型；总是优先于 scheme 别名 |
| `mode` | 元数据方言覆盖（`mysql`/`postgresql`/`oracle`）；oceanbase scheme 下兼作租户模式选择 |
| `jarsDir` | 驱动 jar + owl-agent.jar 搜索目录；回退 `OWLJDBC_JARS_DIR` 环境变量，再回退 CWD 下 `jars/` |
| `agentJar` | owl-agent.jar 显式路径；回退 `OWLJDBC_AGENT_JAR`，否则在 jarsDir 按 `owl-agent*.jar` glob |
| `java` | 可执行 java 路径（缺省 PATH 里的 `java`） |
| `jvmOpts` | 逗号分隔的 JVM 选项，如 `jvmOpts=-Xmx256m,-XX:+UseSerialGC` |
| `jdbcUrl` | 完整 JDBC URL 原样透传（覆盖 profile 拼的 URL；设置后不支持换库） |
| `user` / `password` | 凭据（优先于 userinfo） |

jar 不入库：把 `mysql-connector-j-*.jar`、`opengauss-jdbc-*.jar`、`oceanbase-client-*.jar`、`ojdbc*.jar`、`postgresql-*.jar`、`owl-agent.jar` 放进某个目录（仓库 `jars/` 已被 gitignore）。owl-agent.jar 由 owljdbc 仓库 `jvm/owl-agent` 构建产出。

## 各族能力

| 能力 | postgres 族 | mysql 族 | oracle 族 |
| --- | --- | --- | --- |
| 树/元数据 | pg_catalog + information_schema | SHOW + information_schema | all_* 字典 + all_source |
| 换库 | 重连（同 JVM 新会话） | 保留会话 `USE` | 保留会话 `ALTER SESSION SET CURRENT_SCHEMA` |
| 函数/过程/视图定义 | pg_get_functiondef / pg_views | information_schema routines | all_source / DBMS_METADATA.GET_DDL |
| 分页 | `LIMIT ? OFFSET ?` | `LIMIT ?, ?` | `OFFSET ? ROWS FETCH NEXT ? ROWS ONLY` |
| 行数估算 | pg_class.reltuples | information_schema.TABLES（OB 可能为 NULL → 降级精确计数） | all_tables.num_rows |
| 标识符引用 | `"..."` | 反引号 | `"..."` |

Oracle 族说明：元数据与编辑全部按树上的精确名（owner/table 绑定参数），OB Oracle 小写存储、真 Oracle 19c 大写存储都不需要额外配置；编辑走 `all_constraints` 主键。编辑/删除依赖主键，无主键表只能查询。

## 已知限制与注意

- **内存受限的 Windows**：JVM 默认堆 commit 可能超出页面文件容量（`os::commit_memory failed; errno=1455`，sidecar 报 "agent process closed"）。连接 URL 加 `jvmOpts=-Xmx256m` 压低堆上限即可。
- Oracle 族错误消息为服务端本地化文本（可能乱码），ORA 错误码可读。
- OB MySQL 模式缺 InnoDB 字典，外键查询自动回退 `information_schema.KEY_COLUMN_USAGE`。
- openGauss 的系统 schema（dbe_perf/coverage 等）可能拒绝读取，属正常。
- `jdbcUrl` 覆盖后 `SwitchDatabase` 会拒绝执行（URL 里没有可改写的 database 槽位）。

## 构建与 fork 说明

- 仓库以本地 replace 引入 owljdbc：`go.mod` 中 `replace github.com/cangyunye/owljdbc => ../owljdbc`。fork/CI 需保持同级目录检出 owljdbc，或把 owljdbc 发布后改回版本号。
- 本次为接入做的 owljdbc 上游改动（向后兼容）：`Config.JvmOpts` 透传 JVM 选项（`openAgent` 把它插在 `-cp` 之前）。

## 验证

```bash
# 离线单测：假 sidecar（Go 编译的协议桩），无需 JVM/真库
go test ./drivers/ -run TestOwlJdbc -count=1

# 真库 e2e：LAZYSQL_OWLJDBC_E2E 传一个或多个连接 URL（分号分隔），不可达自动 skip
LAZYSQL_OWLJDBC_E2E='owljdbc://system:SYSDBA001@localhost:1521/ORCLCDB?dbType=oracle&jarsDir=jars&jvmOpts=-Xmx256m;opengauss://ogadmin:pw@host:6432/og_pg?jarsDir=jars&jvmOpts=-Xmx256m' \
  go test ./drivers/ -run TestOwlJdbcE2E -count=1 -v
```

e2e 覆盖：三族连通与 current-database 探测、树/列/PK/行数估算、分页（lookahead）、探针查询、以及建表→插数→分页→单元格更新→事务批量更新→计数的编辑回环（结束自动清理）。
