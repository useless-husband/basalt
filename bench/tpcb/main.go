// Command tpcb runs a pgbench-style TPC-B workload against basalt over the
// PostgreSQL wire protocol (pgx) and prints throughput and latency.
//
// It starts the basalt binary given with -basalt on a fresh data directory
// (or uses -url), creates pgbench's four tables at the given scale, and
// runs pgbench's built-in transaction for each client count:
//
//	UPDATE pgbench_accounts SET abalance = abalance + :delta WHERE aid = :aid;
//	SELECT abalance FROM pgbench_accounts WHERE aid = :aid;
//	UPDATE pgbench_tellers  SET tbalance = tbalance + :delta WHERE tid = :tid;
//	UPDATE pgbench_branches SET bbalance = bbalance + :delta WHERE bid = :bid;
//	INSERT INTO pgbench_history (tid, bid, aid, delta, mtime) VALUES (...);
//
// Transactions that fail with a serialization failure or deadlock are
// retried and counted.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func main() {
	bin := flag.String("basalt", "bin/basalt", "basalt binary to start")
	url := flag.String("url", "", "connect to this server instead of starting one")
	scale := flag.Int("scale", 1, "scale factor (100,000 accounts per unit)")
	clientList := flag.String("clients", "1,4,8,16", "client counts to run")
	duration := flag.Duration("duration", 20*time.Second, "duration of each run")
	noSync := flag.Bool("no-fsync", false, "start basalt with -unsafe-no-fsync (measures CPU cost without durability)")
	embedded := flag.Bool("embedded", false, "run basalt in-process (no network, no fsync) to measure engine CPU cost")
	cpuprofile := flag.String("cpuprofile", "", "write a CPU profile (embedded mode)")
	memprofile := flag.String("memprofile", "", "write an allocation profile (embedded mode)")
	flag.Parse()
	if *embedded {
		runEmbedded(*scale, *clientList, *duration, *cpuprofile, *memprofile)
		return
	}

	ctx := context.Background()
	if *url == "" {
		dir, err := os.MkdirTemp("", "basalt-tpcb-")
		if err != nil {
			fail(err)
		}
		defer os.RemoveAll(dir)
		args := []string{"-D", filepath.Join(dir, "data"), "-listen", "127.0.0.1:0", "-pool-mb", "512"}
		if *noSync {
			args = append(args, "-unsafe-no-fsync")
		}
		cmd := exec.Command(*bin, args...)
		cmd.Stderr = os.Stderr
		out, err := cmd.StdoutPipe()
		if err != nil {
			fail(err)
		}
		if err := cmd.Start(); err != nil {
			fail(err)
		}
		defer func() {
			_ = cmd.Process.Signal(os.Interrupt)
			_ = cmd.Wait()
		}()
		line, _ := bufio.NewReader(out).ReadString('\n')
		m := regexp.MustCompile(`listening on (\S+)`).FindStringSubmatch(line)
		if m == nil {
			fail(fmt.Errorf("server did not start: %q", line))
		}
		*url = "postgres://bench@" + m[1] + "/bench?sslmode=disable"
	}
	setup, err := pgx.Connect(ctx, *url)
	if err != nil {
		fail(err)
	}
	start := time.Now()
	n := *scale
	for _, q := range []string{
		`DROP TABLE IF EXISTS pgbench_history`, `DROP TABLE IF EXISTS pgbench_accounts`,
		`DROP TABLE IF EXISTS pgbench_tellers`, `DROP TABLE IF EXISTS pgbench_branches`,
		`CREATE TABLE pgbench_branches (bid int PRIMARY KEY, bbalance int, filler char(88))`,
		`CREATE TABLE pgbench_tellers (tid int PRIMARY KEY, bid int, tbalance int, filler char(84))`,
		`CREATE TABLE pgbench_accounts (aid int PRIMARY KEY, bid int, abalance int, filler char(84))`,
		`CREATE TABLE pgbench_history (tid int, bid int, aid int, delta int, mtime timestamp, filler char(22))`,
		fmt.Sprintf(`INSERT INTO pgbench_branches SELECT g, 0, '' FROM generate_series(1, %d) g`, n),
		fmt.Sprintf(`INSERT INTO pgbench_tellers SELECT g, (g - 1) / 10 + 1, 0, '' FROM generate_series(1, %d) g`, 10*n),
	} {
		if _, err := setup.Exec(ctx, q); err != nil {
			fail(fmt.Errorf("%s: %w", q, err))
		}
	}
	for b := 0; b < n; b++ {
		q := fmt.Sprintf(`INSERT INTO pgbench_accounts SELECT g, %d, 0, '' FROM generate_series(%d, %d) g`, b+1, b*100000+1, (b+1)*100000)
		if _, err := setup.Exec(ctx, q); err != nil {
			fail(err)
		}
	}
	if _, err := setup.Exec(ctx, `ANALYZE`); err != nil {
		fail(err)
	}
	var version string
	_ = setup.QueryRow(ctx, `SELECT version()`).Scan(&version)
	setup.Close(ctx)
	fmt.Printf("# %s, scale %d (%d accounts), loaded in %v\n", version, n, 100000*n, time.Since(start).Round(time.Millisecond))
	fmt.Printf("%-8s %10s %10s %10s %10s %10s\n", "clients", "tps", "p50 ms", "p99 ms", "retries", "retry %")
	for _, cs := range strings.Split(*clientList, ",") {
		clients, err := strconv.Atoi(strings.TrimSpace(cs))
		if err != nil {
			fail(err)
		}
		run(ctx, *url, n, clients, *duration)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "tpcb:", err)
	os.Exit(1)
}

func retryable(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && (pe.Code == "40001" || pe.Code == "40P01")
}

func run(ctx context.Context, url string, scale, clients int, d time.Duration) {
	var committed, retries atomic.Int64
	var mu sync.Mutex
	var lat []time.Duration
	var wg sync.WaitGroup
	deadline := time.Now().Add(d)
	for c := 0; c < clients; c++ {
		wg.Add(1)
		go func(c int) {
			defer wg.Done()
			conn, err := pgx.Connect(ctx, url)
			if err != nil {
				fail(err)
			}
			defer conn.Close(ctx)
			r := rand.New(rand.NewSource(int64(c) + 1))
			var mine []time.Duration
			for time.Now().Before(deadline) {
				aid := 1 + r.Intn(100000*scale)
				bid := 1 + r.Intn(scale)
				tid := 1 + r.Intn(10*scale)
				delta := r.Intn(10001) - 5000
				t0 := time.Now()
				for {
					err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
						if _, err := tx.Exec(ctx, `UPDATE pgbench_accounts SET abalance = abalance + $1 WHERE aid = $2`, delta, aid); err != nil {
							return err
						}
						var bal int
						if err := tx.QueryRow(ctx, `SELECT abalance FROM pgbench_accounts WHERE aid = $1`, aid).Scan(&bal); err != nil {
							return err
						}
						if _, err := tx.Exec(ctx, `UPDATE pgbench_tellers SET tbalance = tbalance + $1 WHERE tid = $2`, delta, tid); err != nil {
							return err
						}
						if _, err := tx.Exec(ctx, `UPDATE pgbench_branches SET bbalance = bbalance + $1 WHERE bid = $2`, delta, bid); err != nil {
							return err
						}
						_, err := tx.Exec(ctx, `INSERT INTO pgbench_history (tid, bid, aid, delta, mtime) VALUES ($1, $2, $3, $4, CURRENT_TIMESTAMP)`, tid, bid, aid, delta)
						return err
					})
					if err == nil {
						break
					}
					if !retryable(err) {
						fail(err)
					}
					retries.Add(1)
				}
				committed.Add(1)
				mine = append(mine, time.Since(t0))
			}
			mu.Lock()
			lat = append(lat, mine...)
			mu.Unlock()
		}(c)
	}
	wg.Wait()
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	pct := func(p float64) float64 {
		if len(lat) == 0 {
			return 0
		}
		return float64(lat[int(p*float64(len(lat)-1))].Microseconds()) / 1000
	}
	tps := float64(committed.Load()) / d.Seconds()
	retryPct := 100 * float64(retries.Load()) / float64(max(1, committed.Load()+retries.Load()))
	fmt.Printf("%-8d %10.0f %10.2f %10.2f %10d %9.1f%%\n", clients, tps, pct(0.5), pct(0.99), retries.Load(), retryPct)
}
