package internal

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Task is the unit of work the worker drains. Mirrors the tasks row
// shape so the worker can write back without duplicating column
// names elsewhere.
type Task struct {
	ID          int64
	ActionID    string
	ActionLabel string
	Status      string
	ArgsJSON    string
	Username    string
}

// LogEvent is broadcast on the WebSocket. status="" for log lines,
// otherwise a status transition (running, success, error). cancelled
// is also a final status.
type LogEvent struct {
	TaskID int64  `json:"task_id"`
	Status string `json:"status,omitempty"`
	Line   string `json:"line,omitempty"`
	At     int64  `json:"at"` // unix millis
}

// Runner owns the work queue + log fan-out. Single worker for now —
// playbooks mutate hosts.yml and the same git tree, so parallel runs
// would race.
type Runner struct {
	cfg     *Config
	db      *sql.DB
	cat     *Catalog
	queue   chan int64       // task IDs
	cancel  sync.Map         // taskID → chan struct{} (cancellation)
	subsMu  sync.RWMutex
	subs    map[int64][]chan LogEvent
}

func NewRunner(cfg *Config, db *sql.DB, cat *Catalog) *Runner {
	return &Runner{
		cfg:   cfg,
		db:    db,
		cat:   cat,
		queue: make(chan int64, 64),
		subs:  map[int64][]chan LogEvent{},
	}
}

// Start launches the worker goroutine. Returns immediately.
func (r *Runner) Start(ctx context.Context) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case id := <-r.queue:
				r.run(ctx, id)
			}
		}
	}()
	// On boot, mark anything stuck in "running" as errored — it's
	// from a previous process that died, can't be resumed.
	go r.adoptStuckTasks(ctx)
}

func (r *Runner) adoptStuckTasks(ctx context.Context) {
	_, _ = r.db.ExecContext(ctx,
		`UPDATE tasks SET status='error', ended_at=CURRENT_TIMESTAMP, exit_code=-1
		 WHERE status IN ('running','queued')`)
}

// Enqueue inserts a tasks row and pushes its id onto the worker
// channel. Returns the new task id.
func (r *Runner) Enqueue(ctx context.Context, action Action, args map[string]string, user string) (int64, error) {
	maskedArgs := map[string]string{}
	for k, v := range args {
		// Mask anything that looks like a secret. Cheap and good
		// enough for an audit log; the plaintext goes to ansible
		// only via --extra-vars which is also captured in the live
		// log (operator can redact later if needed).
		if isSecretField(action, k) {
			maskedArgs[k] = "<redacted>"
		} else {
			maskedArgs[k] = v
		}
	}
	argsJSON, _ := json.Marshal(maskedArgs)
	res, err := r.db.ExecContext(ctx,
		`INSERT INTO tasks(action_id, action_label, status, args_json, username)
		 VALUES(?,?,?,?,?)`,
		action.ID, action.Label, "queued", string(argsJSON), user)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	// Stash the *real* args in a sidecar file the worker can read —
	// keeps secrets out of the DB but available to the runner.
	if err := os.MkdirAll(r.cfg.LogDir, 0o755); err != nil {
		return 0, err
	}
	realArgs, _ := json.Marshal(args)
	if err := os.WriteFile(filepath.Join(r.cfg.LogDir, fmt.Sprintf("task-%d.args", id)), realArgs, 0o600); err != nil {
		return 0, err
	}
	r.queue <- id
	return id, nil
}

// Cancel signals an in-flight task to terminate. No-op on completed.
func (r *Runner) Cancel(id int64) {
	if v, ok := r.cancel.Load(id); ok {
		select {
		case v.(chan struct{}) <- struct{}{}:
		default:
		}
	}
}

func isSecretField(a Action, name string) bool {
	for _, f := range a.Fields {
		if f.Name == name && f.Type == "secret" {
			return true
		}
	}
	return false
}

// ──────────────────────────────────────────────────────────────────────
// Subscriptions (WebSocket fan-out)
// ──────────────────────────────────────────────────────────────────────

// Subscribe returns a buffered channel of events for a specific task.
// Caller must call Unsubscribe when done. After-the-fact subscribers
// won't see lines that already streamed — they should fetch the log
// file separately for replay.
func (r *Runner) Subscribe(taskID int64) chan LogEvent {
	ch := make(chan LogEvent, 256)
	r.subsMu.Lock()
	r.subs[taskID] = append(r.subs[taskID], ch)
	r.subsMu.Unlock()
	return ch
}

func (r *Runner) Unsubscribe(taskID int64, ch chan LogEvent) {
	r.subsMu.Lock()
	defer r.subsMu.Unlock()
	subs := r.subs[taskID]
	for i, c := range subs {
		if c == ch {
			r.subs[taskID] = append(subs[:i], subs[i+1:]...)
			break
		}
	}
	if len(r.subs[taskID]) == 0 {
		delete(r.subs, taskID)
	}
	close(ch)
}

func (r *Runner) broadcast(taskID int64, ev LogEvent) {
	r.subsMu.RLock()
	subs := r.subs[taskID]
	r.subsMu.RUnlock()
	for _, ch := range subs {
		// Non-blocking send: if a subscriber is slow, drop the line
		// for that subscriber rather than wedging the whole task.
		select {
		case ch <- ev:
		default:
		}
	}
}

// ──────────────────────────────────────────────────────────────────────
// Actual run
// ──────────────────────────────────────────────────────────────────────

func (r *Runner) run(ctx context.Context, taskID int64) {
	// Cancellation signal for THIS task. Cleared on exit.
	cancelCh := make(chan struct{}, 1)
	r.cancel.Store(taskID, cancelCh)
	defer r.cancel.Delete(taskID)

	// Log file: one per task, lives under LogDir, kept forever for
	// audit. Streamed live to subscribers as well.
	logPath := filepath.Join(r.cfg.LogDir, fmt.Sprintf("task-%d.log", taskID))
	logFile, err := os.Create(logPath)
	if err != nil {
		r.fail(ctx, taskID, "create log file: "+err.Error())
		return
	}
	defer logFile.Close()

	if _, err := r.db.ExecContext(ctx,
		`UPDATE tasks SET status='running', started_at=CURRENT_TIMESTAMP, log_path=? WHERE id=?`,
		logPath, taskID); err != nil {
		r.fail(ctx, taskID, "mark running: "+err.Error())
		return
	}
	r.broadcast(taskID, LogEvent{TaskID: taskID, Status: "running", At: nowMS()})

	// Read action + args.
	var actionID, argsJSONmasked string
	if err := r.db.QueryRowContext(ctx, `SELECT action_id, args_json FROM tasks WHERE id=?`, taskID).
		Scan(&actionID, &argsJSONmasked); err != nil {
		r.fail(ctx, taskID, "load task: "+err.Error())
		return
	}
	action, ok := r.cat.ByID(actionID)
	if !ok {
		r.fail(ctx, taskID, "unknown action_id "+actionID)
		return
	}
	argsBytes, err := os.ReadFile(filepath.Join(r.cfg.LogDir, fmt.Sprintf("task-%d.args", taskID)))
	if err != nil {
		r.fail(ctx, taskID, "read args sidecar: "+err.Error())
		return
	}
	var realArgs map[string]string
	if err := json.Unmarshal(argsBytes, &realArgs); err != nil {
		r.fail(ctx, taskID, "parse args sidecar: "+err.Error())
		return
	}

	// Step 1: refresh repo. Hard-reset to remote so a left-behind
	// half-edited tree from a previous run never poisons this one.
	r.line(taskID, logFile, "$ git fetch && git reset --hard origin/"+r.cfg.RepoBranch)
	if err := r.runCmd(ctx, cancelCh, taskID, logFile,
		exec.Command("git", "-C", r.cfg.RepoPath, "fetch", "--quiet", "origin", r.cfg.RepoBranch)); err != nil {
		r.finish(ctx, taskID, "error", -1)
		return
	}
	if err := r.runCmd(ctx, cancelCh, taskID, logFile,
		exec.Command("git", "-C", r.cfg.RepoPath, "reset", "--hard", "origin/"+r.cfg.RepoBranch)); err != nil {
		r.finish(ctx, taskID, "error", -1)
		return
	}
	commitHash := strings.TrimSpace(captureCmd(r.cfg.RepoPath, "git", "rev-parse", "HEAD"))
	_, _ = r.db.ExecContext(ctx, `UPDATE tasks SET commit_hash=? WHERE id=?`, commitHash, taskID)

	// Step 2: ansible-playbook.
	// Inject runner_task_id as an extra-var so playbooks (e.g. e2e-full)
	// can derive unique scratch resource names per run — eliminates the
	// "leftover state from a previous failed run blocks the next run"
	// class of problems.
	args := []string{
		"-i", filepath.Join("inventories", r.cfg.Env, "hosts.yml"),
		"--vault-id", r.cfg.VaultLabel + "@" + r.cfg.VaultPasswordFile,
		"-e", "env=" + r.cfg.Env,
		"-e", fmt.Sprintf("runner_task_id=%d", taskID),
	}
	for k, v := range realArgs {
		args = append(args, "-e", k+"="+v)
	}
	args = append(args, action.ExtraArgs...)
	args = append(args, action.Playbook)

	cmd := exec.Command("ansible-playbook", args...)
	cmd.Dir = r.cfg.RepoPath
	cmd.Env = append(os.Environ(),
		"ANSIBLE_FORCE_COLOR=0",       // log file readability
		"ANSIBLE_HOST_KEY_CHECKING=False",
		// Export the vault password file path so any ansible-playbook
		// subprocess the playbook itself spawns (e.g. preflight's
		// --syntax-check sweep) can decrypt the vault without us
		// having to forward the --vault-id CLI flag explicitly.
		"ANSIBLE_VAULT_PASSWORD_FILE=" + r.cfg.VaultPasswordFile,
	)
	r.line(taskID, logFile, "$ ansible-playbook "+strings.Join(redactSecrets(args, action), " "))
	err = r.runCmd(ctx, cancelCh, taskID, logFile, cmd)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			r.finish(ctx, taskID, "error", exitErr.ExitCode())
		} else {
			r.finish(ctx, taskID, "error", -1)
		}
		return
	}
	r.finish(ctx, taskID, "success", 0)
}

func (r *Runner) runCmd(ctx context.Context, cancelCh chan struct{}, taskID int64, logFile *os.File, cmd *exec.Cmd) error {
	// Same process group so we can SIGTERM the whole tree on cancel.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		r.line(taskID, logFile, "[error] start: "+err.Error())
		return err
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go r.tail(taskID, logFile, stdout, &wg)
	go r.tail(taskID, logFile, stderr, &wg)

	// Watch for cancellation while the command is alive.
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case <-cancelCh:
		r.line(taskID, logFile, "[runner] cancel requested → SIGTERM")
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			r.line(taskID, logFile, "[runner] still alive → SIGKILL")
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
		}
		wg.Wait()
		return errors.New("cancelled")
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		<-done
		wg.Wait()
		return ctx.Err()
	case err := <-done:
		wg.Wait()
		return err
	}
}

func (r *Runner) tail(taskID int64, logFile *os.File, rc io.ReadCloser, wg *sync.WaitGroup) {
	defer wg.Done()
	defer rc.Close()
	scanner := bufio.NewScanner(rc)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		r.line(taskID, logFile, line)
	}
}

// line writes one log line to disk + fans out via WebSocket. Errors
// here are intentionally ignored — losing a single line on a flaky
// disk shouldn't crash the runner.
func (r *Runner) line(taskID int64, logFile *os.File, s string) {
	_, _ = io.WriteString(logFile, s+"\n")
	r.broadcast(taskID, LogEvent{TaskID: taskID, Line: s, At: nowMS()})
}

func (r *Runner) finish(ctx context.Context, taskID int64, status string, exitCode int) {
	_, _ = r.db.ExecContext(ctx,
		`UPDATE tasks SET status=?, ended_at=CURRENT_TIMESTAMP, exit_code=? WHERE id=?`,
		status, exitCode, taskID)
	r.broadcast(taskID, LogEvent{TaskID: taskID, Status: status, At: nowMS()})
	// Close any subscribers — they read EOF when their channel closes.
	r.subsMu.Lock()
	for _, ch := range r.subs[taskID] {
		close(ch)
	}
	delete(r.subs, taskID)
	r.subsMu.Unlock()
}

func (r *Runner) fail(ctx context.Context, taskID int64, msg string) {
	_, _ = r.db.ExecContext(ctx,
		`UPDATE tasks SET status='error', ended_at=CURRENT_TIMESTAMP, exit_code=-2 WHERE id=?`,
		taskID)
	r.broadcast(taskID, LogEvent{TaskID: taskID, Line: "[runner] " + msg, At: nowMS()})
	r.broadcast(taskID, LogEvent{TaskID: taskID, Status: "error", At: nowMS()})
}

func captureCmd(dir string, name string, args ...string) string {
	c := exec.Command(name, args...)
	c.Dir = dir
	out, _ := c.Output()
	return string(out)
}

func redactSecrets(args []string, action Action) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = a
		if !strings.HasPrefix(a, "-e") && i > 0 && args[i-1] == "-e" {
			parts := strings.SplitN(a, "=", 2)
			if len(parts) == 2 && isSecretField(action, parts[0]) {
				out[i] = parts[0] + "=<redacted>"
			}
		}
	}
	return out
}

func nowMS() int64 { return time.Now().UnixMilli() }
