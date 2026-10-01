package pgwire

import (
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/useless-husband/basalt/internal/catalog"
	"github.com/useless-husband/basalt/internal/pgerr"
	"github.com/useless-husband/basalt/internal/sql"
	"github.com/useless-husband/basalt/internal/types"
)

func (c *conn) copy(cp *sql.CopyStmt) error {
	if !cp.From {
		return c.copyOut(cp)
	}
	switch cp.Format {
	case "text", "csv":
	default:
		return pgerr.Unsupported("COPY format %s is not supported (use text or csv)", cp.Format)
	}
	t, cols, err := c.sess.CopyTarget(cp)
	if err != nil {
		return err
	}
	b := c.begin('G')
	b = append(b, 0)
	b = binary.BigEndian.AppendUint16(b, uint16(len(cols)))
	for range cols {
		b = binary.BigEndian.AppendUint16(b, 0)
	}
	c.end(b)
	if err := c.w.Flush(); err != nil {
		return err
	}
	var data []byte
	for {
		typ, msg, err := c.readMessage()
		if err != nil {
			return err
		}
		switch typ {
		case 'd':
			data = append(data, msg...)
			continue
		case 'c':
		case 'f':
			return pgerr.New(pgerr.QueryCanceled, "COPY from stdin failed: %s", strings.TrimRight(string(msg), "\x00"))
		case 'H', 'S':
			continue
		default:
			return pgerr.New(pgerr.ProtocolViolation, "unexpected message type 0x%02x during COPY from stdin", typ)
		}
		break
	}
	var rows [][]*string
	if cp.Format == "csv" {
		rows, err = parseCSV(string(data), cp.Delim, cp.Header)
	} else {
		rows, err = parseCopyText(string(data), cp.Delim)
	}
	if err != nil {
		return err
	}
	n, err := c.sess.CopyFrom(t, cols, rows)
	if err != nil {
		return err
	}
	c.commandComplete(fmt.Sprintf("COPY %d", n))
	return nil
}

// parseCopyText parses COPY's text format: one row per line, fields
// separated by the delimiter, \N for NULL, backslash escapes.
func parseCopyText(data, delim string) ([][]*string, error) {
	var rows [][]*string
	for _, line := range strings.Split(data, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == `\.` {
			break
		}
		if line == "" {
			continue
		}
		var fields []*string
		for _, f := range strings.Split(line, delim) {
			if f == `\N` {
				fields = append(fields, nil)
				continue
			}
			s := unescapeCopy(f)
			fields = append(fields, &s)
		}
		rows = append(rows, fields)
	}
	return rows, nil
}

func unescapeCopy(f string) string {
	if !strings.Contains(f, `\`) {
		return f
	}
	var b strings.Builder
	for i := 0; i < len(f); i++ {
		if f[i] != '\\' || i+1 >= len(f) {
			b.WriteByte(f[i])
			continue
		}
		i++
		switch f[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'v':
			b.WriteByte('\v')
		default:
			b.WriteByte(f[i])
		}
	}
	return b.String()
}

// parseCSV parses CSV with quoting; an unquoted empty field is NULL.
func parseCSV(data, delim string, header bool) ([][]*string, error) {
	var rows [][]*string
	d := delim[0]
	i := 0
	first := true
	for i < len(data) {
		var fields []*string
		for {
			var field strings.Builder
			quoted := false
			if i < len(data) && data[i] == '"' {
				quoted = true
				i++
				for i < len(data) {
					if data[i] == '"' {
						if i+1 < len(data) && data[i+1] == '"' {
							field.WriteByte('"')
							i += 2
							continue
						}
						i++
						break
					}
					field.WriteByte(data[i])
					i++
				}
			}
			for i < len(data) && data[i] != d && data[i] != '\n' && data[i] != '\r' {
				field.WriteByte(data[i])
				i++
			}
			s := field.String()
			if !quoted && s == "" {
				fields = append(fields, nil)
			} else {
				fields = append(fields, &s)
			}
			if i < len(data) && data[i] == d {
				i++
				continue
			}
			break
		}
		for i < len(data) && (data[i] == '\r' || data[i] == '\n') {
			i++
		}
		if len(fields) == 1 && fields[0] != nil && *fields[0] == `\.` {
			break
		}
		if first && header {
			first = false
			continue
		}
		first = false
		rows = append(rows, fields)
	}
	return rows, nil
}

func (c *conn) copyOut(cp *sql.CopyStmt) error {
	query := ""
	if cp.Query != nil {
		return pgerr.Unsupported("COPY (query) TO STDOUT is not supported; use COPY table TO STDOUT")
	}
	cols := "*"
	if len(cp.Columns) > 0 {
		var qs []string
		for _, col := range cp.Columns {
			qs = append(qs, catalog.QuoteIdent(col))
		}
		cols = strings.Join(qs, ", ")
	}
	query = fmt.Sprintf("SELECT %s FROM %s", cols, catalog.QuoteIdent(cp.Table.Name))
	results, err := c.sess.Exec(query)
	if err != nil {
		return err
	}
	res := results[0]
	b := c.begin('H')
	b = append(b, 0)
	b = binary.BigEndian.AppendUint16(b, uint16(len(res.Columns)))
	for range res.Columns {
		b = binary.BigEndian.AppendUint16(b, 0)
	}
	c.end(b)
	csv := cp.Format == "csv"
	if csv && cp.Header {
		var names []string
		for _, col := range res.Columns {
			names = append(names, col.Name)
		}
		b = c.begin('d')
		b = append(b, strings.Join(names, cp.Delim)+"\n"...)
		c.end(b)
	}
	for _, row := range res.Rows {
		var parts []string
		for i, v := range row {
			if v.IsNull() {
				if csv {
					parts = append(parts, "")
				} else {
					parts = append(parts, `\N`)
				}
				continue
			}
			s := types.ToText(v, res.Columns[i].Type)
			if csv {
				if strings.ContainsAny(s, cp.Delim+"\"\n\r") || s == "" {
					s = `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
				}
			} else {
				s = strings.NewReplacer(`\`, `\\`, "\n", `\n`, "\t", `\t`, "\r", `\r`).Replace(s)
			}
			parts = append(parts, s)
		}
		b = c.begin('d')
		b = append(b, strings.Join(parts, cp.Delim)+"\n"...)
		c.end(b)
	}
	c.end(c.begin('c'))
	c.commandComplete(fmt.Sprintf("COPY %d", len(res.Rows)))
	return nil
}
