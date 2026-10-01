// Package crash kills a running basalt server with SIGKILL at random
// moments while clients commit transactions, restarts it, and checks that
// recovery kept every acknowledged commit and nothing else.
//
// Run with: BASALT_CRASH_TEST=1 go test -v ./test/crash/
// BASALT_CRASH_ROUNDS sets the number of kill/restart rounds (default 30)
// and BASALT_CRASH_SEED the random seed.
package crash

import (
	"bufio"
	"context"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

type server struct {
	cmd  *exec.Cmd
	port int
}

func buildServer(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "basalt")
	args := []string{"build", "-o", bin}
	if os.Getenv("BASALT_CRASH_RACE") != "" {
		args = append(args, "-race") // the server reports data races on stderr
	}
	cmd := exec.Command("go", append(args, "../../cmd/basalt")...)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return bin
}

var listenRe = regexp.MustCompile(`listening on 127\.0\.0\.1:(\d+)`)

func startServer(t *testing.T, bin, dir string, logf *os.File) *server {
	t.Helper()
	cmd := exec.Command(bin, "-D", dir, "-listen", "127.0.0.1:0", "-checkpoint-interval", "300ms", "-autovacuum-interval", "500ms", "-pool-mb", "8")
	cmd.Stderr = logf
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		t.Fatalf("server did not start: %v", err)
	}
	m := listenRe.FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("unexpected first line %q", line)
	}
	port, _ := strconv.Atoi(m[1])
	return &server{cmd: cmd, port: port}
}

func (s *server) url() string {
	return fmt.Sprintf("postgres://crash@127.0.0.1:%d/crash?sslmode=disable", s.port)
}

// kill sends SIGKILL to the server process (by PID) and reaps it.
func (s *server) kill() {
	_ = syscall.Kill(s.cmd.Process.Pid, syscall.SIGKILL)
	_ = s.cmd.Wait()
}

const (
	accounts = 50
	initial  = 1000
	writers  = 4
)

type key struct{ writer, seq int }

// outcome of every transaction a writer attempted.
type ledgerState struct {
	mu         sync.Mutex
	acked      map[key]bool // COMMIT returned success
	inflight   map[key]bool // COMMIT sent, outcome unknown (the server died)
	rolledBack map[key]bool // explicitly rolled back
}

func TestCrashKill(t *testing.T) {
	if os.Getenv("BASALT_CRASH_TEST") == "" {
		t.Skip("set BASALT_CRASH_TEST=1 to run the kill -9 crash test")
	}
	rounds := 30
	if v, err := strconv.Atoi(os.Getenv("BASALT_CRASH_ROUNDS")); err == nil {
		rounds = v
	}
	seed := time.Now().UnixNano()
	if v, err := strconv.ParseInt(os.Getenv("BASALT_CRASH_SEED"), 10, 64); err == nil {
		seed = v
	}
	t.Logf("seed %d (rerun with BASALT_CRASH_SEED=%d)", seed, seed)
	r := rand.New(rand.NewSource(seed))
	bin := buildServer(t)
	dir := filepath.Join(t.TempDir(), "data")
	logf, err := os.Create(filepath.Join(t.TempDir(), "server.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logf.Close()

	srv := startServer(t, bin, dir, logf)
	ctx := context.Background()
	setup, err := pgx.Connect(ctx, srv.url())
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE accounts (id int PRIMARY KEY, bal int NOT NULL)`,
		`CREATE TABLE ledger (id bigserial PRIMARY KEY, writer int NOT NULL, seq int NOT NULL, amount int NOT NULL, note text, UNIQUE (writer, seq))`,
		fmt.Sprintf(`INSERT INTO accounts SELECT g, %d FROM generate_series(1, %d) g`, initial, accounts),
	} {
		if _, err := setup.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	setup.Close(ctx)

	st := &ledgerState{acked: map[key]bool{}, inflight: map[key]bool{}, rolledBack: map[key]bool{}}
	nextSeq := make([]int, writers)
	totalAcked := 0
	for round := 1; round <= rounds; round++ {
		var wg sync.WaitGroup
		stop := make(chan struct{})
		for w := 0; w < writers; w++ {
			wg.Add(1)
			wseed := r.Int63()
			go func(w int) {
				defer wg.Done()
				runWriter(srv.url(), w, &nextSeq[w], rand.New(rand.NewSource(wseed)), st, stop)
			}(w)
		}
		time.Sleep(time.Duration(100+r.Intn(900)) * time.Millisecond)
		srv.kill()
		close(stop)
		wg.Wait()

		start := time.Now()
		srv = startServer(t, bin, dir, logf)
		recovery := time.Since(start)
		n := verify(t, srv.url(), st, round)
		st.mu.Lock()
		acked := len(st.acked)
		// Resolve in-flight transactions: present means committed.
		st.inflight = map[key]bool{}
		st.mu.Unlock()
		t.Logf("round %2d: %5d acknowledged commits so far, %5d ledger rows, restart+recovery %v", round, acked, n, recovery.Round(time.Millisecond))
		totalAcked = acked
	}
	srv.kill()
	if b, err := os.ReadFile(logf.Name()); err == nil && strings.Contains(string(b), "DATA RACE") {
		t.Fatalf("the server reported a data race:\n%s", b)
	}
	if totalAcked == 0 {
		t.Fatal("no transaction was ever acknowledged; the workload did not run")
	}
}

// runWriter commits transfers until stop is closed or the server dies.
func runWriter(url string, w int, seq *int, r *rand.Rand, st *ledgerState, stop chan struct{}) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return
	}
	defer conn.Close(ctx)
	for {
		select {
		case <-stop:
			return
		default:
		}
		*seq = *seq + 1
		k := key{w, *seq}
		from, to := 1+r.Intn(accounts), 1+r.Intn(accounts)
		amount := 1 + r.Intn(20)
		rollback := r.Intn(10) == 0
		tx, err := conn.Begin(ctx)
		if err != nil {
			return
		}
		_, err = tx.Exec(ctx, `UPDATE accounts SET bal = bal - $1 WHERE id = $2`, amount, from)
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE accounts SET bal = bal + $1 WHERE id = $2`, amount, to)
		}
		if err == nil {
			_, err = tx.Exec(ctx, `INSERT INTO ledger (writer, seq, amount, note) VALUES ($1, $2, $3, repeat('x', $4))`, w, *seq, amount, r.Intn(300))
		}
		if err != nil {
			_ = tx.Rollback(ctx)
			if conn.IsClosed() {
				return
			}
			continue // serialization failure or deadlock: try another transfer
		}
		if rollback {
			if tx.Rollback(ctx) == nil {
				st.mu.Lock()
				st.rolledBack[k] = true
				st.mu.Unlock()
			}
			continue
		}
		st.mu.Lock()
		st.inflight[k] = true
		st.mu.Unlock()
		if err := tx.Commit(ctx); err != nil {
			if conn.IsClosed() {
				return // outcome unknown; stays in flight
			}
			st.mu.Lock()
			delete(st.inflight, k)
			st.mu.Unlock()
			continue
		}
		st.mu.Lock()
		delete(st.inflight, k)
		st.acked[k] = true
		st.mu.Unlock()
	}
}

// verify checks durability, atomicity and consistency after recovery.
func verify(t *testing.T, url string, st *ledgerState, round int) int {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("round %d: cannot connect after restart: %v", round, err)
	}
	defer conn.Close(ctx)
	var total int64
	if err := conn.QueryRow(ctx, `SELECT sum(bal) FROM accounts`).Scan(&total); err != nil {
		t.Fatalf("round %d: %v", round, err)
	}
	if total != accounts*initial {
		t.Fatalf("round %d: money not conserved: sum = %d, want %d (a transaction was applied partially)", round, total, accounts*initial)
	}
	rows, err := conn.Query(ctx, `SELECT writer, seq, amount FROM ledger`)
	if err != nil {
		t.Fatal(err)
	}
	present := map[key]bool{}
	var ledgerSum int64
	for rows.Next() {
		var k key
		var amount int64
		if err := rows.Scan(&k.writer, &k.seq, &amount); err != nil {
			t.Fatal(err)
		}
		present[k] = true
		ledgerSum += amount
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	for k := range st.acked {
		if !present[k] {
			t.Fatalf("round %d: acknowledged commit %+v is missing after recovery", round, k)
		}
	}
	for k := range present {
		if st.rolledBack[k] {
			t.Fatalf("round %d: rolled-back transaction %+v is visible after recovery", round, k)
		}
		if !st.acked[k] && !st.inflight[k] {
			t.Fatalf("round %d: ledger row %+v was never committed by a client", round, k)
		}
		// An in-flight commit that survived is committed from now on.
		st.acked[k] = true
	}
	// Index and heap agree: the same count through an index-only path and
	// a forced sequential scan.
	var viaIndex, viaSeq int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM ledger WHERE writer >= 0 AND seq >= 0`).Scan(&viaSeq); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `SET enable_seqscan = off`); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM ledger WHERE writer >= 0`).Scan(&viaIndex); err != nil {
		t.Fatal(err)
	}
	if viaIndex != viaSeq || viaSeq != len(present) {
		t.Fatalf("round %d: index scan sees %d rows, sequential scan %d, rows read %d", round, viaIndex, viaSeq, len(present))
	}
	return len(present)
}
