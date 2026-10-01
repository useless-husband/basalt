package engine

import (
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/useless-husband/basalt/internal/pgerr"
)

// These tests demonstrate which anomalies snapshot isolation prevents
// (dirty read, non-repeatable read, phantom, lost update, read skew) and
// the one it allows (write skew), using two sessions with interleaved
// statements.

func setupKV(t *testing.T) (*DB, *Session, *Session) {
	t.Helper()
	db, _ := openMem(t)
	t.Cleanup(func() { db.Close() })
	a, b := db.NewSession("a", "test"), db.NewSession("b", "test")
	q(t, a, `CREATE TABLE kv (k text PRIMARY KEY, v int)`)
	q(t, a, `INSERT INTO kv VALUES ('x', 10), ('y', 20)`)
	return db, a, b
}

func TestNoDirtyRead(t *testing.T) {
	_, a, b := setupKV(t)
	q(t, a, `BEGIN`)
	q(t, a, `UPDATE kv SET v = 99 WHERE k = 'x'`)
	expect(t, b, `SELECT v FROM kv WHERE k = 'x'`, "10")
	q(t, a, `ROLLBACK`)
	expect(t, b, `SELECT v FROM kv WHERE k = 'x'`, "10")
}

func TestNoNonRepeatableRead(t *testing.T) {
	_, a, b := setupKV(t)
	q(t, a, `BEGIN`)
	expect(t, a, `SELECT v FROM kv WHERE k = 'x'`, "10")
	q(t, b, `UPDATE kv SET v = 11 WHERE k = 'x'`)
	expect(t, a, `SELECT v FROM kv WHERE k = 'x'`, "10")
	q(t, a, `COMMIT`)
	expect(t, a, `SELECT v FROM kv WHERE k = 'x'`, "11")
}

func TestNoPhantom(t *testing.T) {
	_, a, b := setupKV(t)
	q(t, a, `BEGIN`)
	expect(t, a, `SELECT count(*) FROM kv WHERE v > 5`, "2")
	q(t, b, `INSERT INTO kv VALUES ('z', 30)`)
	expect(t, a, `SELECT count(*) FROM kv WHERE v > 5`, "2")
	q(t, a, `COMMIT`)
	expect(t, a, `SELECT count(*) FROM kv WHERE v > 5`, "3")
}

func TestNoLostUpdate(t *testing.T) {
	_, a, b := setupKV(t)
	q(t, a, `BEGIN`)
	q(t, b, `BEGIN`)
	expect(t, a, `SELECT v FROM kv WHERE k = 'x'`, "10")
	expect(t, b, `SELECT v FROM kv WHERE k = 'x'`, "10")
	q(t, a, `UPDATE kv SET v = v + 1 WHERE k = 'x'`)
	// b's update must wait for a; when a commits, b fails instead of
	// overwriting a's change (first updater wins).
	done := make(chan error, 1)
	go func() {
		_, err := b.Exec(`UPDATE kv SET v = v + 1 WHERE k = 'x'`)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("second updater did not wait: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	q(t, a, `COMMIT`)
	err := <-done
	if pgerr.Code(err) != pgerr.SerializationFailure {
		t.Fatalf("want 40001, got %v", err)
	}
	q(t, b, `ROLLBACK`)
	expect(t, b, `SELECT v FROM kv WHERE k = 'x'`, "11")
}

func TestWaiterProceedsWhenHolderAborts(t *testing.T) {
	_, a, b := setupKV(t)
	q(t, a, `BEGIN`)
	q(t, a, `UPDATE kv SET v = 0 WHERE k = 'x'`)
	done := make(chan error, 1)
	go func() {
		_, err := b.Exec(`UPDATE kv SET v = v + 5 WHERE k = 'x'`)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	q(t, a, `ROLLBACK`)
	if err := <-done; err != nil {
		t.Fatalf("update after the holder aborted failed: %v", err)
	}
	expect(t, a, `SELECT v FROM kv WHERE k = 'x'`, "15")
}

func TestNoReadSkew(t *testing.T) {
	_, a, b := setupKV(t)
	// Invariant x + y = 30. b moves 5 from y to x while a reads.
	q(t, a, `BEGIN`)
	expect(t, a, `SELECT v FROM kv WHERE k = 'x'`, "10")
	q(t, b, `BEGIN`)
	q(t, b, `UPDATE kv SET v = v + 5 WHERE k = 'x'`)
	q(t, b, `UPDATE kv SET v = v - 5 WHERE k = 'y'`)
	q(t, b, `COMMIT`)
	expect(t, a, `SELECT v FROM kv WHERE k = 'y'`, "20") // same snapshot as x
	q(t, a, `COMMIT`)
}

// TestWriteSkewIsPossible documents the anomaly snapshot isolation does
// not prevent: two transactions read overlapping data and write disjoint
// rows, together breaking a constraint neither broke alone. PostgreSQL's
// REPEATABLE READ behaves the same way; SERIALIZABLE (SSI) would reject
// one of them, which basalt does not implement.
func TestWriteSkewIsPossible(t *testing.T) {
	db, a, b := setupKV(t)
	_ = db
	q(t, a, `CREATE TABLE oncall (doctor text PRIMARY KEY, on_duty bool)`)
	q(t, a, `INSERT INTO oncall VALUES ('alice', true), ('bob', true)`)
	q(t, a, `BEGIN`)
	q(t, b, `BEGIN`)
	expect(t, a, `SELECT count(*) FROM oncall WHERE on_duty`, "2")
	expect(t, b, `SELECT count(*) FROM oncall WHERE on_duty`, "2")
	q(t, a, `UPDATE oncall SET on_duty = false WHERE doctor = 'alice'`)
	q(t, b, `UPDATE oncall SET on_duty = false WHERE doctor = 'bob'`)
	q(t, a, `COMMIT`)
	q(t, b, `COMMIT`)
	expect(t, a, `SELECT count(*) FROM oncall WHERE on_duty`, "0")
}

func TestDeadlockDetected(t *testing.T) {
	_, a, b := setupKV(t)
	q(t, a, `BEGIN`)
	q(t, b, `BEGIN`)
	q(t, a, `UPDATE kv SET v = 1 WHERE k = 'x'`)
	q(t, b, `UPDATE kv SET v = 2 WHERE k = 'y'`)
	errA := make(chan error, 1)
	go func() {
		_, err := a.Exec(`UPDATE kv SET v = 1 WHERE k = 'y'`) // waits for b
		errA <- err
	}()
	time.Sleep(50 * time.Millisecond)
	_, errB := b.Exec(`UPDATE kv SET v = 2 WHERE k = 'x'`) // closes the cycle
	if pgerr.Code(errB) != pgerr.DeadlockDetected {
		t.Fatalf("want 40P01 for the transaction closing the cycle, got %v", errB)
	}
	q(t, b, `ROLLBACK`)
	if err := <-errA; err != nil {
		t.Fatalf("survivor should proceed: %v", err)
	}
	q(t, a, `COMMIT`)
	expect(t, a, `SELECT k, v FROM kv ORDER BY k`, "x|1\ny|1")
}

func TestUniqueConflictWaitsForInserter(t *testing.T) {
	_, a, b := setupKV(t)
	q(t, a, `BEGIN`)
	q(t, a, `INSERT INTO kv VALUES ('new', 1)`)
	done := make(chan error, 1)
	go func() {
		_, err := b.Exec(`INSERT INTO kv VALUES ('new', 2)`)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	q(t, a, `COMMIT`)
	if err := <-done; pgerr.Code(err) != pgerr.UniqueViolation {
		t.Fatalf("want 23505 after the first inserter committed, got %v", err)
	}
	// And if the first inserter aborts, the second succeeds.
	q(t, a, `BEGIN`)
	q(t, a, `INSERT INTO kv VALUES ('other', 1)`)
	go func() {
		_, err := b.Exec(`INSERT INTO kv VALUES ('other', 2)`)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	q(t, a, `ROLLBACK`)
	if err := <-done; err != nil {
		t.Fatalf("insert after the conflicting inserter aborted: %v", err)
	}
	expect(t, a, `SELECT v FROM kv WHERE k = 'other'`, "2")
}

// TestBankTransfers runs many concurrent transfer transactions (retried on
// serialization failure and deadlock) while auditors check, inside their
// own snapshots, that the total never changes.
func TestBankTransfers(t *testing.T) {
	db, _ := openMem(t)
	defer db.Close()
	setup := db.NewSession("setup", "test")
	const accounts, clients, perClient, initial = 20, 16, 150, 1000
	q(t, setup, `CREATE TABLE bank (id int PRIMARY KEY, bal int NOT NULL CHECK (bal >= 0))`)
	q(t, setup, fmt.Sprintf(`INSERT INTO bank SELECT g, %d FROM generate_series(1, %d) g`, initial, accounts))
	seed := time.Now().UnixNano()
	t.Logf("seed %d", seed)
	var retries, overdrawn, committed atomic.Int64
	errs := make(chan error, clients+4)
	var writers sync.WaitGroup
	for c := 0; c < clients; c++ {
		writers.Add(1)
		go func(c int) {
			defer writers.Done()
			s := db.NewSession(fmt.Sprintf("c%d", c), "test")
			defer s.Close()
			r := rand.New(rand.NewSource(seed + int64(c)))
			for i := 0; i < perClient; i++ {
				from, to := 1+r.Intn(accounts), 1+r.Intn(accounts)
				amt := 1 + r.Intn(100)
				for {
					_, err := s.Exec(fmt.Sprintf(`BEGIN; UPDATE bank SET bal = bal - %d WHERE id = %d; UPDATE bank SET bal = bal + %d WHERE id = %d; COMMIT`, amt, from, amt, to))
					if err == nil {
						committed.Add(1)
						break
					}
					s.Exec(`ROLLBACK`)
					switch pgerr.Code(err) {
					case pgerr.SerializationFailure, pgerr.DeadlockDetected:
						retries.Add(1)
						continue
					case pgerr.CheckViolation:
						overdrawn.Add(1)
					default:
						errs <- err
						return
					}
					break
				}
			}
		}(c)
	}
	stop := make(chan struct{})
	var audits atomic.Int64
	var auditors sync.WaitGroup
	for a := 0; a < 2; a++ {
		auditors.Add(1)
		go func() {
			defer auditors.Done()
			s := db.NewSession("auditor", "test")
			defer s.Close()
			for {
				select {
				case <-stop:
					return
				default:
				}
				res, err := s.Exec(`SELECT sum(bal), count(*) FROM bank`)
				if err != nil {
					errs <- err
					return
				}
				if got := render(res[0]); got != fmt.Sprintf("%d|%d", accounts*initial, accounts) {
					errs <- fmt.Errorf("auditor saw an inconsistent total: %s", got)
					return
				}
				audits.Add(1)
			}
		}()
	}
	writers.Wait()
	close(stop)
	auditors.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if committed.Load()+overdrawn.Load() != clients*perClient {
		t.Fatalf("committed %d + rejected %d != %d transfers", committed.Load(), overdrawn.Load(), clients*perClient)
	}
	expect(t, setup, `SELECT sum(bal), min(bal) >= 0 FROM bank`, fmt.Sprintf("%d|t", accounts*initial))
	t.Logf("%d transfers committed, %d retries after 40001/40P01, %d rejected by CHECK, %d consistent audits",
		committed.Load(), retries.Load(), overdrawn.Load(), audits.Load())
}
