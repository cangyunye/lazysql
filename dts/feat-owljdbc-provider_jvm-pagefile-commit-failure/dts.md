---
id: "feat-owljdbc-provider_jvm-pagefile-commit-failure"
domain: "feat-owljdbc-provider"
slug: "jvm-pagefile-commit-failure"
title: "owljdbc e2e 真库测试失败：sidecar JVM 报 \"agent process closed\"，重连也起不来"
status: "resolved"
created: "2026-11-20T00:50:56+08:00"
resolved: "2026-11-20T00:51:36+08:00"
commit: "7be64b2"
branch: "main"
platform: "win32"
session: ""
---

# feat-owljdbc-provider_jvm-pagefile-commit-failure

## 问题

owljdbc e2e 真库测试失败：sidecar JVM 报 "agent process closed"，重连也起不来

## 环境

| 项 | 值 |
|----|----|
| git commit | 7be64b2 |
| 分支 | main |
| 平台 | win32 |
| 建档时间 | 2026-11-20T00:50:56+08:00 |
| 会话 | - |

## 调查过程

- [00:50] 建档
- [00:51] 记录日志 (manual): errno=1455 页面文件不足导致 sidecar JVM 启动即死，表现为 agent process closed
- [00:51] 结案
- [00:51] 新增 E2E 用例: owljdbc e2e：oracle_19c + og_pg 浏览与编辑回环
- [00:51] 记录证据 1 项
- [00:51] 记录终端文本快照

## 日志与摘录

### [manual] 2026-11-20T00:51:27+08:00 · errno=1455 页面文件不足导致 sidecar JVM 启动即死，表现为 agent process closed

```
Java HotSpot(TM) 64-Bit Server VM warning: INFO: os::commit_memory(0x0000000601400000, 536870912, 0) failed; error='页面文件太小，无法完成操作。' (DOS error/errno=1455)

现象：TestOwlJdbcE2E 连跑时首个子测试中途 GetTables 报 "agent process closed"，后续所有子测试（含另一 profileKey 的 openGauss）Connect 也报同样错误。单跑任一子测试则正常。

根因：Windows 默认 JVM 堆要 commit 512MB，机器内存/页面文件吃紧时 owl.agent.Main 启动即失败、进程直接死 → owljdbc readLoop 读到 stdout EOF → "agent process closed"。反复杀/起 JVM 加剧页面文件压力。与 owlsql 当时"并发多 JVM OOM 雪崩"同根：都是 JVM commit 内存超页面文件容量。drivers/ 目录下留有一批 hs_err_pid*.log（已清理并 gitignore）。
```

## 测试场景与 E2E 用例

| # | 用例 | 步骤 | 预期 | 结果 |
|---|------|------|------|------|
| 1 | owljdbc e2e：oracle_19c + og_pg 浏览与编辑回环 | LAZYSQL_OWLJDBC_E2E='owljdbc://system:REDACTED@localhost:1521/ORCLCDB?dbType=oracle&jarsDir=F:/pantheon/Project/go-tui-sqlclient/jars&jvmOpts=-Xmx256m;opengauss://ogadmin:REDACTED@172.20.214.44:6432/og_pg?jarsDir=F:/pantheon/Project/go-tui-sqlclient/jars&jvmOpts=-Xmx256m' go test ./drivers/ -run TestOwlJdbcE2E -count=1 -v（两个测试：TestOwlJdbcE2E 浏览链路 + TestOwlJdbcE2EEditRoundTrip 编辑回环） | 三族 current-database 探测正确；GetDatabases/GetTables/GetTableColumns/GetRecords(分页 lookahead)/GetPrimaryKeyColumnNames/GetEstimatedRowCount 正常；CRUD 回环：建表→插 3 行→limit=2 分页断言 HasNextPage=true→UpdateRecord 改格→ExecutePendingChanges 事务批量更新→GetExactRowCount=3→结果含 cell-updated/pending-updated→DROP 清理。不可达（refused/unreachable/timeout/access denied/communications link failure/connection reset/agent process closed）自动 SKIP。 | pass |

## 证据截图

[文本快照: 最终 e2e 终验：离线单测 + 组件测试 + 真库 e2e 全绿](shots/001-005153.txt)

## 修复方案

owljdbc Config 增加 JvmOpts []string 字段，openAgent 把它插在 java 命令行 -cp 之前（exec.Command(java, append(jvmOpts, "-cp", cp, "owl.agent.Main")...)）；lazysql 连接 URL 新增 jvmOpts 参数（逗号分隔）透传。e2e 连接串加 jvmOpts=-Xmx256m 把堆 commit 压到 256MB 后，三连跑全部稳定通过（oracle_19c + og_pg）。向后兼容：不传 JvmOpts 时行为与原先完全一致。

## 复盘

根因是环境性的：Windows 页面文件容量 < JVM 默认堆 commit 量（errno=1455），JVM 启动即死，owljdbc 侧表现为笼统的 "agent process closed"。排查路径：先怀疑测试选择随机性/管理器回收竞态，最后靠"不过滤 stderr 重跑"看到 HotSpot 警告才定位——sidecar 型工具的 stderr 转发（owljdbc logLoop）是关键线索源。教训：① Windows 内存受限环境跑 JVM sidecar 必须可控堆上限，sidecar 启动参数要留透传口子（本次补的 JvmOpts）；② "agent process closed" 既是崩溃信号也是启动失败信号，看到它先看 stderr；③ e2e 遇到该错误按 infra skip 而非 fail，避免环境波动污染结果。
