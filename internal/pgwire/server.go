// Package pgwire implements the server side of the PostgreSQL
// frontend/backend protocol, version 3: startup, simple and extended
// query, COPY, cancellation and error reporting with SQLSTATE codes.
package pgwire

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"

	"github.com/useless-husband/basalt/internal/engine"
	"github.com/useless-husband/basalt/internal/pgerr"
	"github.com/useless-husband/basalt/internal/sql"
	"github.com/useless-husband/basalt/internal/types"
)

const (
	protoV3        = 196608
	sslRequest     = 80877103
	gssRequest     = 80877104
	cancelRequest  = 80877102
	maxMessageSize = 1 << 30
)

// Server accepts PostgreSQL client connections.
type Server struct {
	DB *engine.DB
	// Password, if set, is required (cleartext password authentication).
	Password string
	Log      *slog.Logger

	mu    sync.Mutex
	ln    net.Listener
	conns map[net.Conn]struct{}
	wg    sync.WaitGroup
	done  bool
}

// Serve accepts connections on ln until Close.
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	s.ln = ln
	if s.conns == nil {
		s.conns = map[net.Conn]struct{}{}
	}
	s.mu.Unlock()
	if s.Log == nil {
		s.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			done := s.done
			s.mu.Unlock()
			if done {
				return nil
			}
			return err
		}
		s.mu.Lock()
		s.conns[c] = struct{}{}
		s.mu.Unlock()
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer func() {
				s.mu.Lock()
				delete(s.conns, c)
				s.mu.Unlock()
				c.Close()
			}()
			s.handle(c)
		}()
	}
}

// Close stops accepting connections and closes open ones.
func (s *Server) Close() {
	s.mu.Lock()
	s.done = true
	if s.ln != nil {
		s.ln.Close()
	}
	for c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// conn is one client connection.
type conn struct {
	srv     *Server
	c       net.Conn
	r       *bufio.Reader
	w       *bufio.Writer
	sess    *engine.Session
	portals map[string]*portal
	buf     []byte
}

type portal struct {
	stmt       *engine.Prepared
	params     []types.Value
	resFormats []int16
	result     *engine.Result
	pos        int
	done       bool
}

func (s *Server) handle(nc net.Conn) {
	c := &conn{srv: s, c: nc, r: bufio.NewReaderSize(nc, 64<<10), w: bufio.NewWriterSize(nc, 64<<10), portals: map[string]*portal{}}
	if err := c.startup(); err != nil {
		if !errors.Is(err, io.EOF) && !errors.Is(err, errCancel) {
			s.Log.Debug("startup failed", "remote", nc.RemoteAddr(), "err", err)
		}
		return
	}
	defer c.sess.Close()
	if err := c.loop(); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
		s.Log.Debug("connection ended", "remote", nc.RemoteAddr(), "err", err)
	}
}

var errCancel = errors.New("cancel request")

func (c *conn) readStartup() (uint32, []byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(c.r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n < 8 || n > 10000 {
		return 0, nil, fmt.Errorf("invalid startup packet length %d", n)
	}
	body := make([]byte, n-4)
	if _, err := io.ReadFull(c.r, body); err != nil {
		return 0, nil, err
	}
	return binary.BigEndian.Uint32(body), body[4:], nil
}

func (c *conn) startup() error {
	for {
		code, body, err := c.readStartup()
		if err != nil {
			return err
		}
		switch code {
		case sslRequest, gssRequest:
			if _, err := c.c.Write([]byte{'N'}); err != nil {
				return err
			}
			continue
		case cancelRequest:
			if len(body) >= 8 {
				pid := int64(binary.BigEndian.Uint32(body))
				secret := int32(binary.BigEndian.Uint32(body[4:]))
				c.srv.DB.Cancel(pid, secret)
			}
			return errCancel
		}
		major := code >> 16
		if major != 3 {
			c.sendError(pgerr.New(pgerr.ProtocolViolation, "unsupported frontend protocol %d.%d: server supports 3.0", major, code&0xffff))
			c.w.Flush()
			return fmt.Errorf("unsupported protocol %d", code)
		}
		params := map[string]string{}
		var unrecognized []string
		parts := strings.Split(string(body), "\x00")
		for i := 0; i+1 < len(parts); i += 2 {
			if parts[i] == "" {
				break
			}
			if strings.HasPrefix(parts[i], "_pq_.") {
				unrecognized = append(unrecognized, parts[i])
				continue
			}
			params[parts[i]] = parts[i+1]
		}
		if minor := code & 0xffff; minor > 0 || len(unrecognized) > 0 {
			// We speak 3.0 only.
			b := c.begin('v')
			b = binary.BigEndian.AppendUint32(b, 0)
			b = binary.BigEndian.AppendUint32(b, uint32(len(unrecognized)))
			for _, u := range unrecognized {
				b = append(append(b, u...), 0)
			}
			c.end(b)
		}
		user := params["user"]
		if user == "" {
			c.sendError(pgerr.New("28000", "no PostgreSQL user name specified in startup packet"))
			c.w.Flush()
			return errors.New("no user")
		}
		dbname := params["database"]
		if dbname == "" {
			dbname = user
		}
		if c.srv.Password != "" {
			b := c.begin('R')
			b = binary.BigEndian.AppendUint32(b, 3) // cleartext password
			c.end(b)
			if err := c.w.Flush(); err != nil {
				return err
			}
			typ, msg, err := c.readMessage()
			if err != nil {
				return err
			}
			if typ != 'p' || strings.TrimRight(string(msg), "\x00") != c.srv.Password {
				c.sendError(pgerr.New("28P01", "password authentication failed for user \"%s\"", user))
				c.w.Flush()
				return errors.New("bad password")
			}
		}
		c.sess = c.srv.DB.NewSession(user, dbname)
		for k, v := range params {
			c.sess.SetStartupParameter(k, v)
		}
		b := c.begin('R')
		b = binary.BigEndian.AppendUint32(b, 0)
		c.end(b)
		for _, name := range engine.ReportedParameters {
			v, _ := c.sess.Setting(name)
			c.parameterStatus(name, v)
		}
		pid, secret := c.sess.BackendKey()
		b = c.begin('K')
		b = binary.BigEndian.AppendUint32(b, uint32(pid))
		b = binary.BigEndian.AppendUint32(b, uint32(secret))
		c.end(b)
		c.ready()
		return c.w.Flush()
	}
}

func (c *conn) parameterStatus(k, v string) {
	b := c.begin('S')
	b = append(append(b, k...), 0)
	b = append(append(b, v...), 0)
	c.end(b)
}

// begin starts an outgoing message in c.buf.
func (c *conn) begin(typ byte) []byte {
	c.buf = append(c.buf[:0], typ, 0, 0, 0, 0)
	return c.buf
}

// end fills in the length and writes the message.
func (c *conn) end(b []byte) {
	binary.BigEndian.PutUint32(b[1:], uint32(len(b)-1))
	c.w.Write(b)
	c.buf = b
}

func (c *conn) readMessage() (byte, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(c.r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n < 4 || n > maxMessageSize {
		return 0, nil, fmt.Errorf("invalid message length %d", n)
	}
	body := make([]byte, n-4)
	if _, err := io.ReadFull(c.r, body); err != nil {
		return 0, nil, err
	}
	return hdr[0], body, nil
}

func (c *conn) ready() {
	b := c.begin('Z')
	b = append(b, byte(c.sess.Status()))
	c.end(b)
}

func (c *conn) sendError(err error) {
	pe := pgerr.As(err)
	sev := pe.Severity
	if sev == "" {
		sev = "ERROR"
	}
	b := c.begin('E')
	field := func(code byte, v string) {
		if v != "" {
			b = append(b, code)
			b = append(append(b, v...), 0)
		}
	}
	field('S', sev)
	field('V', sev)
	field('C', pe.Code)
	field('M', pe.Message)
	field('D', pe.Detail)
	field('H', pe.Hint)
	if pe.Position > 0 {
		field('P', fmt.Sprint(pe.Position))
	}
	field('t', pe.Table)
	field('c', pe.Column)
	field('n', pe.Constraint)
	b = append(b, 0)
	c.end(b)
}

func (c *conn) sendNotices() {
	for _, n := range c.sess.Notices {
		b := c.begin('N')
		b = append(b, 'S')
		b = append(append(b, "NOTICE"...), 0)
		b = append(b, 'V')
		b = append(append(b, "NOTICE"...), 0)
		b = append(b, 'C')
		b = append(append(b, "00000"...), 0)
		b = append(b, 'M')
		b = append(append(b, n...), 0)
		b = append(b, 0)
		c.end(b)
	}
	c.sess.Notices = nil
}

func (c *conn) loop() error {
	ignoreTillSync := false
	for {
		typ, msg, err := c.readMessage()
		if err != nil {
			return err
		}
		if ignoreTillSync && typ != 'S' && typ != 'X' {
			continue
		}
		var herr error
		switch typ {
		case 'Q':
			herr = c.simpleQuery(strings.TrimRight(string(msg), "\x00"))
			if herr == nil {
				c.ready()
				if err := c.w.Flush(); err != nil {
					return err
				}
			}
		case 'P':
			herr = c.parse(msg)
		case 'B':
			herr = c.bind(msg)
		case 'D':
			herr = c.describe(msg)
		case 'E':
			herr = c.execute(msg)
		case 'C':
			herr = c.closeMsg(msg)
		case 'S':
			ignoreTillSync = false
			if err := c.sess.Sync(); err != nil {
				c.sendError(err)
			}
			c.ready()
			if err := c.w.Flush(); err != nil {
				return err
			}
		case 'H':
			if err := c.w.Flush(); err != nil {
				return err
			}
		case 'X':
			return nil
		case 'd', 'c', 'f':
			// Stray COPY data after an error: ignore.
		default:
			c.sendError(pgerr.New(pgerr.ProtocolViolation, "invalid frontend message type %d", typ))
			c.w.Flush()
			return fmt.Errorf("invalid message type %q", typ)
		}
		if herr != nil {
			var ioErr *net.OpError
			if errors.As(herr, &ioErr) {
				return herr
			}
			c.sendNotices()
			c.sendError(herr)
			if typ == 'Q' {
				c.ready()
			} else {
				ignoreTillSync = true
			}
			if err := c.w.Flush(); err != nil {
				return err
			}
		}
	}
}

// ---- simple query ----

func (c *conn) simpleQuery(query string) error {
	// COPY needs the sub-protocol, so handle it here.
	if stmts, err := sql.Parse(query); err == nil && len(stmts) == 1 {
		if cp, ok := stmts[0].Stmt.(*sql.CopyStmt); ok {
			return c.copy(cp)
		}
	}
	results, err := c.sess.Exec(query)
	for _, r := range results {
		c.sendNotices()
		if r.Empty {
			c.end(c.begin('I'))
			continue
		}
		if r.HasRows {
			c.rowDescription(r.Columns, nil)
			if err := c.dataRows(r.Rows, r.Columns, nil); err != nil {
				return err
			}
		}
		c.commandComplete(r.Tag)
	}
	return err
}

func (c *conn) commandComplete(tag string) {
	b := c.begin('C')
	b = append(append(b, tag...), 0)
	c.end(b)
}

func formatFor(formats []int16, i int) int16 {
	switch len(formats) {
	case 0:
		return 0
	case 1:
		return formats[0]
	}
	if i < len(formats) {
		return formats[i]
	}
	return 0
}

func binaryOK(t types.T) bool {
	switch t.Oid {
	case types.OidRegclass, types.OidRegtype, types.OidRegproc, types.OidRegnamespace, types.OidRegrole, types.OidUnknown:
		return false
	}
	return types.HasBinary(t)
}

func (c *conn) rowDescription(cols []engine.Column, formats []int16) {
	b := c.begin('T')
	b = binary.BigEndian.AppendUint16(b, uint16(len(cols)))
	for i, col := range cols {
		b = append(append(b, col.Name...), 0)
		b = binary.BigEndian.AppendUint32(b, col.TableOid)
		b = binary.BigEndian.AppendUint16(b, uint16(col.Attnum))
		oid := col.Type.Oid
		if oid == types.OidUnknown {
			oid = types.OidText
		}
		b = binary.BigEndian.AppendUint32(b, oid)
		b = binary.BigEndian.AppendUint16(b, uint16(col.Type.Len()))
		b = binary.BigEndian.AppendUint32(b, uint32(col.Type.Mod))
		f := formatFor(formats, i)
		if f == 1 && !binaryOK(col.Type) {
			f = 0
		}
		b = binary.BigEndian.AppendUint16(b, uint16(f))
	}
	c.end(b)
}

func (c *conn) dataRows(rows [][]types.Value, cols []engine.Column, formats []int16) error {
	for _, row := range rows {
		b := c.begin('D')
		b = binary.BigEndian.AppendUint16(b, uint16(len(row)))
		for i, v := range row {
			if v.IsNull() {
				b = binary.BigEndian.AppendUint32(b, 0xFFFFFFFF)
				continue
			}
			lenPos := len(b)
			b = append(b, 0, 0, 0, 0)
			t := cols[i].Type
			if formatFor(formats, i) == 1 && binaryOK(t) {
				var err error
				if b, err = types.AppendBinary(b, v, t); err != nil {
					return err
				}
			} else {
				b = append(b, types.ToText(v, t)...)
			}
			binary.BigEndian.PutUint32(b[lenPos:], uint32(len(b)-lenPos-4))
		}
		c.end(b)
		if c.w.Buffered() > 256<<10 {
			if err := c.w.Flush(); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---- extended query ----

func cstring(b []byte) (string, []byte, error) {
	i := 0
	for i < len(b) && b[i] != 0 {
		i++
	}
	if i >= len(b) {
		return "", nil, pgerr.New(pgerr.ProtocolViolation, "invalid string in message")
	}
	return string(b[:i]), b[i+1:], nil
}

func errProto() error { return pgerr.New(pgerr.ProtocolViolation, "insufficient data left in message") }

func (c *conn) parse(msg []byte) error {
	name, rest, err := cstring(msg)
	if err != nil {
		return err
	}
	query, rest, err := cstring(rest)
	if err != nil {
		return err
	}
	if len(rest) < 2 {
		return errProto()
	}
	n := int(binary.BigEndian.Uint16(rest))
	rest = rest[2:]
	if len(rest) < 4*n {
		return errProto()
	}
	oids := make([]uint32, n)
	for i := range oids {
		oids[i] = binary.BigEndian.Uint32(rest[4*i:])
	}
	if name != "" {
		if _, exists := c.sess.Lookup(name); exists {
			return pgerr.New("42P05", "prepared statement \"%s\" already exists", name)
		}
	}
	if _, err := c.sess.Prepare(name, query, oids); err != nil {
		return err
	}
	c.end(c.begin('1'))
	return nil
}

func readInt16s(b []byte) ([]int16, []byte, error) {
	if len(b) < 2 {
		return nil, nil, errProto()
	}
	n := int(binary.BigEndian.Uint16(b))
	b = b[2:]
	if len(b) < 2*n {
		return nil, nil, errProto()
	}
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(binary.BigEndian.Uint16(b[2*i:]))
	}
	return out, b[2*n:], nil
}

func (c *conn) bind(msg []byte) error {
	pname, rest, err := cstring(msg)
	if err != nil {
		return err
	}
	sname, rest, err := cstring(rest)
	if err != nil {
		return err
	}
	stmt, ok := c.sess.Lookup(sname)
	if !ok {
		return pgerr.New(pgerr.InvalidSQLStatementName, "prepared statement \"%s\" does not exist", sname)
	}
	pfmts, rest, err := readInt16s(rest)
	if err != nil {
		return err
	}
	if len(rest) < 2 {
		return errProto()
	}
	n := int(binary.BigEndian.Uint16(rest))
	rest = rest[2:]
	if n != len(stmt.ParamTypes) {
		return pgerr.New(pgerr.ProtocolViolation, "bind message supplies %d parameters, but prepared statement \"%s\" requires %d", n, sname, len(stmt.ParamTypes))
	}
	params := make([]types.Value, n)
	for i := 0; i < n; i++ {
		if len(rest) < 4 {
			return errProto()
		}
		l := int32(binary.BigEndian.Uint32(rest))
		rest = rest[4:]
		if l < 0 {
			params[i] = types.Null
			continue
		}
		if int(l) > len(rest) {
			return errProto()
		}
		data := rest[:l]
		rest = rest[l:]
		t := stmt.ParamTypes[i]
		var v types.Value
		if formatFor(pfmts, i) == 1 {
			v, err = types.DecodeBinary(data, t)
		} else {
			v, err = types.Parse(string(data), t)
		}
		if err != nil {
			return err
		}
		params[i] = v
	}
	rfmts, _, err := readInt16s(rest)
	if err != nil {
		return err
	}
	c.portals[pname] = &portal{stmt: stmt, params: params, resFormats: rfmts}
	c.end(c.begin('2'))
	return nil
}

func (c *conn) describe(msg []byte) error {
	if len(msg) < 2 {
		return errProto()
	}
	kind := msg[0]
	name, _, err := cstring(msg[1:])
	if err != nil {
		return err
	}
	switch kind {
	case 'S':
		stmt, ok := c.sess.Lookup(name)
		if !ok {
			return pgerr.New(pgerr.InvalidSQLStatementName, "prepared statement \"%s\" does not exist", name)
		}
		b := c.begin('t')
		b = binary.BigEndian.AppendUint16(b, uint16(len(stmt.ParamTypes)))
		for _, t := range stmt.ParamTypes {
			b = binary.BigEndian.AppendUint32(b, t.Oid)
		}
		c.end(b)
		if stmt.HasRows {
			c.rowDescription(stmt.Columns, nil)
		} else {
			c.end(c.begin('n'))
		}
	case 'P':
		p, ok := c.portals[name]
		if !ok {
			return pgerr.New(pgerr.InvalidCursorName, "portal \"%s\" does not exist", name)
		}
		if p.stmt.HasRows {
			c.rowDescription(p.stmt.Columns, p.resFormats)
		} else {
			c.end(c.begin('n'))
		}
	default:
		return pgerr.New(pgerr.ProtocolViolation, "invalid DESCRIBE message subtype %d", kind)
	}
	return nil
}

func (c *conn) execute(msg []byte) error {
	name, rest, err := cstring(msg)
	if err != nil {
		return err
	}
	if len(rest) < 4 {
		return errProto()
	}
	maxRows := int(int32(binary.BigEndian.Uint32(rest)))
	p, ok := c.portals[name]
	if !ok {
		return pgerr.New(pgerr.InvalidCursorName, "portal \"%s\" does not exist", name)
	}
	if p.done {
		c.commandComplete(p.result.Tag)
		return nil
	}
	if p.result == nil {
		res, err := c.sess.ExecPrepared(p.stmt, p.params)
		c.sendNotices()
		if err != nil {
			return err
		}
		p.result = res
		if res.Empty {
			p.done = true
			c.end(c.begin('I'))
			return nil
		}
	}
	res := p.result
	if res.HasRows {
		rows := res.Rows[p.pos:]
		if maxRows > 0 && len(rows) > maxRows {
			if err := c.dataRows(rows[:maxRows], res.Columns, p.resFormats); err != nil {
				return err
			}
			p.pos += maxRows
			c.end(c.begin('s')) // PortalSuspended
			return nil
		}
		if err := c.dataRows(rows, res.Columns, p.resFormats); err != nil {
			return err
		}
	}
	p.done = true
	c.commandComplete(res.Tag)
	return nil
}

func (c *conn) closeMsg(msg []byte) error {
	if len(msg) < 2 {
		return errProto()
	}
	name, _, err := cstring(msg[1:])
	if err != nil {
		return err
	}
	if msg[0] == 'S' {
		c.sess.ClosePrepared(name)
	} else {
		delete(c.portals, name)
	}
	c.end(c.begin('3'))
	return nil
}
