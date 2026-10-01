package pgwire_test

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/useless-husband/basalt/internal/engine"
	"github.com/useless-husband/basalt/internal/pgwire"
)

// startServer runs basalt in-process on an ephemeral port.
func startServer(t *testing.T) string {
	t.Helper()
	db, err := engine.Open(engine.Options{Dir: t.TempDir(), PoolPages: 1024, CheckpointInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &pgwire.Server{DB: db}
	go srv.Serve(ln)
	t.Cleanup(func() {
		srv.Close()
		db.Close()
	})
	return fmt.Sprintf("postgres://test@%s/test?sslmode=disable", ln.Addr())
}

func connect(t *testing.T, url string) *pgx.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(context.Background()) })
	return c
}

func exec(t *testing.T, c *pgx.Conn, sql string, args ...any) {
	t.Helper()
	if _, err := c.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func sqlState(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func TestPgxTypesRoundTrip(t *testing.T) {
	c := connect(t, startServer(t))
	ctx := context.Background()
	exec(t, c, `CREATE TABLE all_types (
		id bigserial PRIMARY KEY, b bool, i2 smallint, i4 int, i8 bigint, f4 real, f8 double precision,
		n numeric(12,3), t text, vc varchar(10), d date, ts timestamp, tz timestamptz, by bytea, iv interval)`)
	ts := time.Date(2024, 2, 29, 13, 45, 1, 500000000, time.UTC)
	var num pgtype.Numeric
	if err := num.Scan("12345.678"); err != nil {
		t.Fatal(err)
	}
	var id int64
	err := c.QueryRow(ctx, `INSERT INTO all_types (b, i2, i4, i8, f4, f8, n, t, vc, d, ts, tz, by, iv)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14) RETURNING id`,
		true, int16(-7), int32(42), int64(1)<<40, float32(1.5), 2.25, num, "héllo", "short",
		time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC), ts, ts, []byte{0, 1, 0xff}, 90*time.Minute).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	var (
		b   bool
		i2  int16
		i4  int32
		i8  int64
		f4  float32
		f8  float64
		n   pgtype.Numeric
		s   string
		vc  string
		d   time.Time
		gts time.Time
		gtz time.Time
		by  []byte
		iv  pgtype.Interval
	)
	err = c.QueryRow(ctx, `SELECT b, i2, i4, i8, f4, f8, n, t, vc, d, ts, tz, by, iv FROM all_types WHERE id = $1`, id).
		Scan(&b, &i2, &i4, &i8, &f4, &f8, &n, &s, &vc, &d, &gts, &gtz, &by, &iv)
	if err != nil {
		t.Fatal(err)
	}
	if !b || i2 != -7 || i4 != 42 || i8 != 1<<40 || f4 != 1.5 || f8 != 2.25 || s != "héllo" || vc != "short" {
		t.Fatalf("scalar mismatch: %v %v %v %v %v %v %q %q", b, i2, i4, i8, f4, f8, s, vc)
	}
	nv, _ := n.Value()
	if nv != "12345.678" {
		t.Fatalf("numeric: %v", nv)
	}
	if !d.Equal(time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC)) || !gts.Equal(ts) || !gtz.Equal(ts) {
		t.Fatalf("time mismatch: %v %v %v", d, gts, gtz)
	}
	if string(by) != "\x00\x01\xff" {
		t.Fatalf("bytea: %x", by)
	}
	if iv.Microseconds != 90*60*1_000_000 {
		t.Fatalf("interval: %+v", iv)
	}
	// NULLs.
	exec(t, c, `INSERT INTO all_types (b) VALUES (NULL)`)
	var nb *bool
	var nn *int32
	if err := c.QueryRow(ctx, `SELECT b, i4 FROM all_types WHERE b IS NULL`).Scan(&nb, &nn); err != nil || nb != nil || nn != nil {
		t.Fatalf("nulls: %v %v %v", nb, nn, err)
	}
	// Big numeric arithmetic through binary format.
	var big1 pgtype.Numeric
	if err := c.QueryRow(ctx, `SELECT 123456789012345678901234567890::numeric * 10`).Scan(&big1); err != nil {
		t.Fatal(err)
	}
	want, _ := new(big.Int).SetString("1234567890123456789012345678900", 10)
	gotBig := new(big.Int).Mul(big1.Int, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(big1.Exp)), nil))
	if big1.Exp < 0 || gotBig.Cmp(want) != 0 {
		t.Fatalf("big numeric: %v e%d", big1.Int, big1.Exp)
	}
}

func TestPgxQueriesAndErrors(t *testing.T) {
	c := connect(t, startServer(t))
	ctx := context.Background()
	exec(t, c, `CREATE TABLE kv (k int PRIMARY KEY, v text NOT NULL)`)
	batch := &pgx.Batch{}
	for i := 0; i < 100; i++ {
		batch.Queue(`INSERT INTO kv VALUES ($1, $2)`, i, fmt.Sprintf("v%d", i))
	}
	if err := c.SendBatch(ctx, batch).Close(); err != nil {
		t.Fatal(err)
	}
	rows, err := c.Query(ctx, `SELECT k, v FROM kv WHERE k >= $1 AND k < $2 ORDER BY k`, 10, 15)
	if err != nil {
		t.Fatal(err)
	}
	got, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct {
		K int
		V string
	}])
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 || got[0].K != 10 || got[4].V != "v14" {
		t.Fatalf("got %+v", got)
	}
	_, err = c.Exec(ctx, `INSERT INTO kv VALUES (1, 'dup')`)
	if sqlState(err) != "23505" {
		t.Fatalf("want unique violation, got %v", err)
	}
	var pe *pgconn.PgError
	errors.As(err, &pe)
	if pe.ConstraintName != "kv_pkey" || !strings.Contains(pe.Detail, "(k)=(1)") {
		t.Fatalf("error fields: %+v", pe)
	}
	_, err = c.Exec(ctx, `SELECT * FROM missing_table`)
	if sqlState(err) != "42P01" {
		t.Fatalf("want undefined table, got %v", err)
	}
	// The connection is still usable after errors.
	var n int
	if err := c.QueryRow(ctx, `SELECT count(*) FROM kv`).Scan(&n); err != nil || n != 100 {
		t.Fatalf("count %d %v", n, err)
	}
	// Transactions through pgx.
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM kv WHERE k < 50`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.QueryRow(ctx, `SELECT count(*) FROM kv`).Scan(&n); err != nil || n != 100 {
		t.Fatalf("after rollback %d %v", n, err)
	}
	// Simple protocol mode.
	if err := c.QueryRow(ctx, `SELECT v FROM kv WHERE k = $1`, pgx.QueryExecModeSimpleProtocol, 7).Scan(new(string)); err != nil {
		t.Fatal(err)
	}
	// COPY FROM through pgx (text format).
	if _, err := c.Exec(ctx, `CREATE TABLE copied (a int, b text)`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PgConn().CopyFrom(ctx, strings.NewReader("1\tone\n2\t\\N\n"), `COPY copied FROM STDIN`); err != nil {
		t.Fatal(err)
	}
	if err := c.QueryRow(ctx, `SELECT count(*) FROM copied WHERE b IS NULL`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("copy: %d %v", n, err)
	}
}

// TestPgxCancel cancels a long query with a CancelRequest sent on a
// separate connection, the way psql's Ctrl-C does.
func TestPgxCancel(t *testing.T) {
	url := startServer(t)
	c := connect(t, url)
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = c.PgConn().CancelRequest(context.Background())
	}()
	start := time.Now()
	_, err := c.Exec(context.Background(), `SELECT count(*) FROM generate_series(1, 2000000000)`)
	if sqlState(err) != "57014" {
		t.Fatalf("want query_canceled (57014), got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("cancel took %v", time.Since(start))
	}
	// The session survives the cancellation.
	var one int
	if err := c.QueryRow(context.Background(), `SELECT 1`).Scan(&one); err != nil || one != 1 {
		t.Fatalf("after cancel: %v", err)
	}
}

// TestPgxConcurrentClients checks that many clients can run at once.
func TestPgxConcurrentClients(t *testing.T) {
	url := startServer(t)
	c := connect(t, url)
	exec(t, c, `CREATE TABLE counter (id int PRIMARY KEY, n int)`)
	exec(t, c, `INSERT INTO counter VALUES (1, 0)`)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cc, err := pgx.Connect(context.Background(), url)
			if err != nil {
				errs <- err
				return
			}
			defer cc.Close(context.Background())
			for i := 0; i < 25; i++ {
				for {
					_, err := cc.Exec(context.Background(), `UPDATE counter SET n = n + 1 WHERE id = 1`)
					if err == nil {
						break
					}
					if s := sqlState(err); s != "40001" && s != "40P01" {
						errs <- err
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var n int
	if err := c.QueryRow(context.Background(), `SELECT n FROM counter`).Scan(&n); err != nil || n != 200 {
		t.Fatalf("lost updates: n=%d err=%v", n, err)
	}
}
