package main

import (
	"fmt"
	"math/rand"
	"os"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/useless-husband/basalt/internal/engine"
	"github.com/useless-husband/basalt/internal/pgerr"
	"github.com/useless-husband/basalt/internal/types"
	"github.com/useless-husband/basalt/internal/vfs"
)

// runEmbedded runs the TPC-B transaction through the engine API directly
// (prepared statements, no wire protocol, no fsync), which isolates the
// engine's CPU cost per transaction.
func runEmbedded(scale int, clientList string, d time.Duration, cpuprofile, memprofile string, settings []string) {
	dir, err := os.MkdirTemp("", "basalt-tpcb-emb-")
	if err != nil {
		fail(err)
	}
	defer os.RemoveAll(dir)
	db, err := engine.Open(engine.Options{Dir: dir, FS: vfs.NoSync{FS: vfs.OS{}}, PoolPages: 65536, AutovacuumInterval: 10 * time.Second})
	if err != nil {
		fail(err)
	}
	defer db.Close()
	s := db.NewSession("bench", "bench")
	for _, q := range []string{
		`CREATE TABLE pgbench_branches (bid int PRIMARY KEY, bbalance int, filler char(88))`,
		`CREATE TABLE pgbench_tellers (tid int PRIMARY KEY, bid int, tbalance int, filler char(84))`,
		`CREATE TABLE pgbench_accounts (aid int PRIMARY KEY, bid int, abalance int, filler char(84))`,
		`CREATE TABLE pgbench_history (tid int, bid int, aid int, delta int, mtime timestamp, filler char(22))`,
		fmt.Sprintf(`INSERT INTO pgbench_branches SELECT g, 0, '' FROM generate_series(1, %d) g`, scale),
		fmt.Sprintf(`INSERT INTO pgbench_tellers SELECT g, (g - 1) / 10 + 1, 0, '' FROM generate_series(1, %d) g`, 10*scale),
		fmt.Sprintf(`INSERT INTO pgbench_accounts SELECT g, (g - 1) / 100000 + 1, 0, '' FROM generate_series(1, %d) g`, 100000*scale),
		`ANALYZE`,
	} {
		if _, err := s.Exec(q); err != nil {
			fail(err)
		}
	}
	for _, kv := range settings {
		if _, err := s.Exec("SET " + strings.Replace(kv, "=", " = ", 1)); err != nil {
			fail(err)
		}
	}
	res, err := s.Exec(`EXPLAIN UPDATE pgbench_branches SET bbalance = bbalance + 1 WHERE bid = 1`)
	if err != nil {
		fail(err)
	}
	var plan []string
	for _, r := range res[0].Rows {
		plan = append(plan, strings.TrimSpace(r[0].S))
	}
	fmt.Printf("# settings %v; branch update plan: %s\n", settings, strings.Join(plan, " / "))
	s.Close()
	if cpuprofile != "" {
		f, err := os.Create(cpuprofile)
		if err != nil {
			fail(err)
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}
	if memprofile != "" {
		defer func() {
			f, err := os.Create(memprofile)
			if err == nil {
				pprof.Lookup("allocs").WriteTo(f, 0)
				f.Close()
			}
		}()
	}
	fmt.Printf("# basalt %s embedded (no network, no fsync), scale %d\n", engine.Version, scale)
	fmt.Printf("%-8s %10s %10s %10s %10s\n", "clients", "tps", "p50 ms", "p99 ms", "retries")
	for _, cs := range strings.Split(clientList, ",") {
		clients, _ := strconv.Atoi(strings.TrimSpace(cs))
		var committed, retries atomic.Int64
		var mu sync.Mutex
		var lat []time.Duration
		var wg sync.WaitGroup
		deadline := time.Now().Add(d)
		for c := 0; c < clients; c++ {
			wg.Add(1)
			go func(c int) {
				defer wg.Done()
				s := db.NewSession("bench", "bench")
				defer s.Close()
				for _, kv := range settings {
					if _, err := s.Exec("SET " + strings.Replace(kv, "=", " = ", 1)); err != nil {
						fail(err)
					}
				}
				prep := func(q string) *engine.Prepared {
					p, err := s.Prepare("", q, nil)
					if err != nil {
						fail(err)
					}
					return p
				}
				upA := prep(`UPDATE pgbench_accounts SET abalance = abalance + $1 WHERE aid = $2`)
				selA := prep(`SELECT abalance FROM pgbench_accounts WHERE aid = $1`)
				upT := prep(`UPDATE pgbench_tellers SET tbalance = tbalance + $1 WHERE tid = $2`)
				upB := prep(`UPDATE pgbench_branches SET bbalance = bbalance + $1 WHERE bid = $2`)
				ins := prep(`INSERT INTO pgbench_history (tid, bid, aid, delta, mtime) VALUES ($1, $2, $3, $4, CURRENT_TIMESTAMP)`)
				r := rand.New(rand.NewSource(int64(c) + 1))
				var mine []time.Duration
				iv := func(i int) types.Value { return types.NewInt(int64(i)) }
				for time.Now().Before(deadline) {
					aid, bid, tid := 1+r.Intn(100000*scale), 1+r.Intn(scale), 1+r.Intn(10*scale)
					delta := r.Intn(10001) - 5000
					t0 := time.Now()
					for {
						err := func() error {
							if _, err := s.Exec("BEGIN"); err != nil {
								return err
							}
							for _, st := range []struct {
								p    *engine.Prepared
								args []types.Value
							}{
								{upA, []types.Value{iv(delta), iv(aid)}},
								{selA, []types.Value{iv(aid)}},
								{upT, []types.Value{iv(delta), iv(tid)}},
								{upB, []types.Value{iv(delta), iv(bid)}},
								{ins, []types.Value{iv(tid), iv(bid), iv(aid), iv(delta)}},
							} {
								if _, err := s.ExecPrepared(st.p, st.args); err != nil {
									return err
								}
							}
							_, err := s.Exec("COMMIT")
							return err
						}()
						if err == nil {
							break
						}
						s.Exec("ROLLBACK")
						if c := pgerr.Code(err); c != pgerr.SerializationFailure && c != pgerr.DeadlockDetected {
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
		p := func(f float64) float64 { return float64(lat[int(f*float64(len(lat)-1))].Microseconds()) / 1000 }
		fmt.Printf("%-8d %10.0f %10.3f %10.3f %10d\n", clients, float64(committed.Load())/d.Seconds(), p(0.5), p(0.99), retries.Load())
		// TPC-B consistency: every delta was applied to an account, a
		// teller and a branch, and logged once in the history.
		chk := db.NewSession("check", "bench")
		res, err := chk.Exec(`SELECT (SELECT sum(abalance) FROM pgbench_accounts), (SELECT sum(tbalance) FROM pgbench_tellers),
			(SELECT sum(bbalance) FROM pgbench_branches), (SELECT sum(delta) FROM pgbench_history), (SELECT count(*) FROM pgbench_history)`)
		if err != nil {
			fail(err)
		}
		row := res[0].Rows[0]
		if !(types.Equal(row[0], row[1]) && types.Equal(row[1], row[2]) && types.Equal(row[2], row[3])) {
			fail(fmt.Errorf("TPC-B consistency check failed: %v", row))
		}
		chk.Close()
		fmt.Printf("#   consistent: balances and history sum to %s, %s history rows\n", types.ToText(row[0], types.Int8), types.ToText(row[4], types.Int8))
		w := db.Store().WAL()
		fmt.Printf("#   WAL: %d flushes, %.1f MB written (%.0f bytes per transaction), %d checkpoints\n", w.Syncs.Load(), float64(w.Written.Load())/1e6, float64(w.Written.Load())/float64(committed.Load()), db.Store().Checkpoints.Load())

	}
}
