// Command basalt runs the basalt database server.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/useless-husband/basalt/internal/engine"
	"github.com/useless-husband/basalt/internal/pgwire"
	"github.com/useless-husband/basalt/internal/vfs"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "--version") {
		fmt.Printf("basalt %s (speaks PostgreSQL %s wire protocol v3)\n", engine.Version, engine.ServerVersion)
		return
	}
	dir := flag.String("D", "./basalt-data", "data directory")
	listen := flag.String("listen", "127.0.0.1:5433", "address to listen on (use port 0 for an ephemeral port)")
	password := flag.String("password", os.Getenv("BASALT_PASSWORD"), "require this password (cleartext authentication); empty means trust")
	poolMB := flag.Int("pool-mb", 128, "buffer pool size in MiB")
	ckpt := flag.Duration("checkpoint-interval", 30*time.Second, "time between automatic checkpoints")
	autovac := flag.Duration("autovacuum-interval", 10*time.Second, "how often autovacuum looks for work (0 disables)")
	verbose := flag.Bool("v", false, "log connection errors")
	noFsync := flag.Bool("unsafe-no-fsync", false, "never fsync (benchmarks only: a crash or power loss can lose or corrupt data)")
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	start := time.Now()
	var fs vfs.FS = vfs.OS{}
	if *noFsync {
		fs = vfs.NoSync{FS: vfs.OS{}}
		log.Warn("running with -unsafe-no-fsync: commits are not durable")
	}
	db, err := engine.Open(engine.Options{
		Dir:                *dir,
		FS:                 fs,
		PoolPages:          *poolMB * 1024 * 1024 / 8192,
		CheckpointInterval: *ckpt,
		AutovacuumInterval: *autovac,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "basalt: cannot open database in %s: %v\n", *dir, err)
		os.Exit(1)
	}
	if n := db.Store().RecoveredRecords; n > 0 {
		log.Info("crash recovery replayed WAL", "records", n, "took", time.Since(start).Round(time.Millisecond))
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		db.Close()
		fmt.Fprintf(os.Stderr, "basalt: %v\n", err)
		os.Exit(1)
	}
	srv := &pgwire.Server{DB: db, Password: *password, Log: log}
	// The address line is parsed by tests and scripts; keep its format.
	fmt.Printf("basalt %s listening on %s (data directory %s)\n", engine.Version, ln.Addr(), *dir)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case s := <-sig:
		log.Info("shutting down", "signal", s.String())
	case err := <-errc:
		if err != nil {
			log.Error("server stopped", "err", err)
		}
	}
	srv.Close()
	if err := db.Close(); err != nil {
		log.Error("checkpoint at shutdown failed", "err", err)
		os.Exit(1)
	}
}
