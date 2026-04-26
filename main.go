package main

import (
	"context"
	"embed"
	"fmt"
	"log"
	"net/http"
	"os"
	osexec "os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/keeppio/keeppio-runner/internal"
)

//go:embed templates/*
var tplFS embed.FS

//go:embed static/*
var staticFS embed.FS

func main() {
	cfg, err := internal.LoadConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o755); err != nil {
		log.Fatalf("ensure db dir: %v", err)
	}
	if err := os.MkdirAll(cfg.LogDir, 0o755); err != nil {
		log.Fatalf("ensure log dir: %v", err)
	}

	db, err := internal.OpenDB(cfg.DBPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := internal.Migrate(ctx, db); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	if err := internal.EnsureAdmin(ctx, db, cfg.InitialAdminPassword); err != nil {
		log.Fatalf("seed admin: %v", err)
	}
	if _, err := internal.EnsureSessionKey(ctx, db); err != nil {
		log.Fatalf("session key: %v", err)
	}

	// Clone the repo if missing. Subsequent fetches happen per-task.
	if _, err := os.Stat(filepath.Join(cfg.RepoPath, ".git")); err != nil {
		log.Printf("cloning %s into %s …", cfg.RepoURL, cfg.RepoPath)
		if err := os.MkdirAll(cfg.RepoPath, 0o755); err != nil {
			log.Fatalf("mkdir repo path: %v", err)
		}
		out, cerr := exec("git", "clone", "--depth=1", "--branch", cfg.RepoBranch, cfg.RepoURL, cfg.RepoPath)
		if cerr != nil {
			log.Fatalf("git clone: %v\n%s", cerr, out)
		}
	}

	cat := &internal.Catalog{}
	// First sync — also picks up any commits that landed since clone.
	if err := pullRepo(cfg); err != nil {
		log.Printf("initial git pull: %v", err)
	}
	if err := loadActions(cfg.RepoPath, cat); err != nil {
		log.Fatalf("load actions.yml: %v", err)
	}
	// Refresh the repo + actions every 5 min. Each task run already
	// fetches+resets, so the periodic loop only catches *out-of-band*
	// pushes (e.g. someone editing actions.yml directly via git). 30 s
	// was overkill for a once-a-week event; 5 min still feels live.
	go func() {
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := pullRepo(cfg); err != nil {
					log.Printf("periodic git pull: %v", err)
				}
				if err := loadActions(cfg.RepoPath, cat); err != nil {
					log.Printf("reload actions.yml: %v", err)
				}
			}
		}
	}()

	// Hourly housekeeping.
	go func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = internal.PurgeExpiredSessions(ctx, db)
			}
		}
	}()

	runner := internal.NewRunner(cfg, db, cat)
	runner.Start(ctx)

	server, err := internal.NewServer(cfg, db, cat, runner, tplFS, staticFS)
	if err != nil {
		log.Fatalf("server: %v", err)
	}

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           server.Mux(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("keeppio-runner (%s) listening on %s", cfg.Env, cfg.Addr)

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down …")
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	_ = srv.Shutdown(shutCtx)
}

// loadActions reads actions.yml from the cloned repo. If absent (e.g.
// brand-new branch), the catalog is left intact.
func loadActions(repo string, cat *internal.Catalog) error {
	path := filepath.Join(repo, "actions.yml")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	return cat.Load(path)
}

// exec is a thin wrapper over os/exec for one-shot commands during
// boot. Returns combined output for diagnostics.
func exec(name string, args ...string) (string, error) {
	cmd := osexec.Command(name, args...)
	b, err := cmd.CombinedOutput()
	return string(b), err
}

// pullRepo fetches + hard-resets the cloned repo to origin/<branch>.
// Same shape the task runner does per-task, lifted out so the boot
// path and the periodic refresher share a code path.
func pullRepo(cfg *internal.Config) error {
	if out, err := exec("git", "-C", cfg.RepoPath, "fetch", "--quiet", "origin", cfg.RepoBranch); err != nil {
		return fmt.Errorf("git fetch: %w: %s", err, out)
	}
	if out, err := exec("git", "-C", cfg.RepoPath, "reset", "--hard", "origin/"+cfg.RepoBranch); err != nil {
		return fmt.Errorf("git reset: %w: %s", err, out)
	}
	return nil
}
