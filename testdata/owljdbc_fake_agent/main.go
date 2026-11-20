// owljdbc_fake_agent is a Go stand-in for the owl-agent.jar JVM sidecar. It
// speaks the owljdbc frame protocol (see github.com/cangyunye/owljdbc) and
// answers a small script of SQL probes, so driver tests can run without a JVM
// or a real database. Tests point the driver at it via the Config java
// executable (the owljdbc client ignores extra command line arguments, so the
// -cp/-classpath/-jar argv of a real agent launch is harmless here).
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	agent "github.com/cangyunye/owljdbc"
)

func main() {
	for {
		ft, payload, err := agent.ReadFrame(os.Stdin)
		if err != nil {
			if err == io.EOF {
				return
			}
			fmt.Fprintln(os.Stderr, err)
			return
		}
		if ft != agent.FrameRequest {
			continue
		}
		req, _, err := agent.DecodeRequestPayload(payload)
		if err != nil {
			continue
		}
		switch req.Op {
		case "CONNECT", "PING", "BEGIN", "COMMIT", "ROLLBACK", "CLOSE":
			writeResp(os.Stdout, req, agent.ControlResponse{ID: req.ID, OK: true})
		case "CANCEL":
			// best-effort in the real agent; no response expected
		case "QUERY":
			respondQuery(os.Stdout, req)
		case "EXEC":
			writeResp(os.Stdout, req, agent.ControlResponse{ID: req.ID, OK: true, Affected: 3})
		default:
			writeResp(os.Stdout, req, agent.ControlResponse{ID: req.ID, OK: false, Error: "unknown op"})
		}
	}
}

// respondQuery answers well-known SQL probes with scripted result sets so
// tests can assert the driver's per-family dispatch and value handling.
func respondQuery(w io.Writer, req agent.ControlRequest) {
	sqlText := strings.ToLower(req.SQL)

	switch {
	case strings.Contains(sqlText, "__midstream_fail__"):
		writeResp(w, req, agent.ControlResponse{ID: req.ID, OK: true, Cols: []agent.ColMeta{{Name: "X", Type: "VARCHAR"}}})
		writeRow(w, req, []any{"hello"})
		writeEndErr(w, req, "boom")
	case strings.Contains(sqlText, "current_database()"):
		writeResultSet(w, req, []agent.ColMeta{{Name: "datname", Type: "VARCHAR"}}, [][]any{{"pgdb"}})
	case strings.Contains(sqlText, "database()"):
		writeResultSet(w, req, []agent.ColMeta{{Name: "DATABASE()", Type: "VARCHAR"}}, [][]any{{"mysqldb"}})
	case strings.Contains(sqlText, "from dual"):
		writeResultSet(w, req, []agent.ColMeta{{Name: "USER", Type: "VARCHAR"}}, [][]any{{"OraUser"}})
	case strings.Contains(sqlText, "show databases"):
		writeResultSet(w, req, []agent.ColMeta{{Name: "Database", Type: "VARCHAR"}},
			[][]any{{"db_one"}, {"information_schema"}, {"mysql"}, {"performance_schema"}, {"db_two"}, {"sys"}})
	case strings.Contains(sqlText, "show tables"):
		writeResultSet(w, req, []agent.ColMeta{{Name: "Tables_in_db_one", Type: "VARCHAR"}}, [][]any{{"t1"}, {"t2"}})
	case strings.Contains(sqlText, "__null_row__"):
		writeResultSet(w, req, []agent.ColMeta{{Name: "X", Type: "VARCHAR"}}, [][]any{{nil}})
	case strings.Contains(sqlText, "__empty_row__"):
		writeResultSet(w, req, []agent.ColMeta{{Name: "X", Type: "VARCHAR"}}, [][]any{{""}})
	case strings.Contains(sqlText, "__typed_row__"):
		// exercise the typed value conversions the bridge decodes
		writeResultSet(w, req, []agent.ColMeta{
			{Name: "I", Type: "BIGINT"}, {Name: "F", Type: "DOUBLE"}, {Name: "B", Type: "BOOLEAN"}, {Name: "D", Type: "DATETIME"},
		}, [][]any{{int64(42), 1.5, true, "2026-01-02 03:04:05"}})
	case strings.Contains(sqlText, "column_name"):
		writeResultSet(w, req, []agent.ColMeta{
			{Name: "column_name", Type: "VARCHAR"}, {Name: "data_type", Type: "VARCHAR"},
			{Name: "is_nullable", Type: "VARCHAR"}, {Name: "column_default", Type: "VARCHAR"},
		}, [][]any{{"id", "int", "YES", nil}, {"name", "varchar", "YES", "'x'"}})
	default:
		writeResultSet(w, req, []agent.ColMeta{{Name: "X", Type: "VARCHAR"}}, [][]any{{"hello"}})
	}
}

func writeResultSet(w io.Writer, req agent.ControlRequest, cols []agent.ColMeta, rows [][]any) {
	writeResp(w, req, agent.ControlResponse{ID: req.ID, OK: true, Cols: cols, Rows: int64(len(rows))})
	for _, row := range rows {
		writeRow(w, req, row)
	}
	writeEnd(w, req, int64(len(rows)))
}

func writeResp(w io.Writer, req agent.ControlRequest, resp agent.ControlResponse) {
	// the client routes responses by conn-id, so echo it back
	resp.Conn = req.Conn
	b, _ := json.Marshal(resp)
	agent.WriteFrame(w, agent.FrameResponse, b)
}

func writeRow(w io.Writer, req agent.ControlRequest, row []any) {
	agent.WriteFrame(w, agent.FrameRowBatch, agent.EncodeRowBatch(req.Conn, req.ID, row))
}

func writeEnd(w io.Writer, req agent.ControlRequest, rows int64) {
	b, _ := json.Marshal(agent.ControlResponse{ID: req.ID, Conn: req.Conn, OK: true, Rows: rows})
	agent.WriteFrame(w, agent.FrameEnd, b)
}

func writeEndErr(w io.Writer, req agent.ControlRequest, msg string) {
	b, _ := json.Marshal(agent.ControlResponse{ID: req.ID, Conn: req.Conn, OK: false, Error: msg})
	agent.WriteFrame(w, agent.FrameEnd, b)
}
