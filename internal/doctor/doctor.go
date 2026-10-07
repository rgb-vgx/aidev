// Package doctor checks whether aidev can work on this machine and says, in
// plain words, what to do about each problem it finds.
package doctor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"aidev/internal/config"
)

// Status is the outcome of one check.
type Status string

const (
	StatusOK      Status = "ok"
	StatusWarn    Status = "warn"
	StatusFail    Status = "fail"
	StatusSkipped Status = "skipped"
)

// Check names, in the order Run reports them.
const (
	CheckConfig     = "config"
	CheckGit        = "git"
	CheckAgent      = "agent"
	CheckDatabase   = "database"
	CheckMigrations = "migrations"
	CheckStuckTasks = "stuck tasks"
	CheckWorkspace  = "workspace"
	CheckDisk       = "disk"
)

// Free-space thresholds for the disk check, on the filesystem that holds the
// workspace. Every task copies the repository into a worktree, its checks may
// build there, and git fails half-way when the disk fills — a run that dies
// in the middle of a commit is harder to read than one that never started.
const (
	diskWarnBytes = 5 << 30
	diskFailBytes = 1 << 30
)

// Result is one check's outcome. Summary says what was found; Fix, set whenever
// Status is warn or fail, says what to do about it.
type Result struct {
	Name    string `json:"name"`
	Status  Status `json:"status"`
	Summary string `json:"summary"`
	Fix     string `json:"fix,omitempty"`
}

// Deps is everything Run needs from the outside world, so that each check can be
// exercised without a real machine.
type Deps struct {
	// LoadConfig resolves aidev's configuration (config.Load in production).
	LoadConfig func() (config.Config, error)
	// LookPath finds an executable (exec.LookPath in production).
	LookPath func(name string) (string, error)
	// PingDatabase connects to the database and returns nil if it answers.
	PingDatabase func(ctx context.Context, databaseURL string) error
	// PendingMigrations returns the versions of migrations not yet applied.
	PendingMigrations func(ctx context.Context, databaseURL string) ([]string, error)
	// StuckTasks returns a short description of each task stuck in RUNNING or
	// VERIFYING with an expired lease (store.StuckLeases in production).
	StuckTasks func(ctx context.Context, databaseURL string) ([]string, error)
	// CheckWorkspace returns nil if dir exists or can be created, and is writable.
	CheckWorkspace func(dir string) error
	// FreeSpace returns the bytes available to this user on the filesystem
	// holding dir. Nil, or errors.ErrUnsupported, skips the disk check.
	FreeSpace func(dir string) (uint64, error)
	// Docker reports the state of the Docker daemon and of the PostgreSQL
	// container `aidev setup` creates. It is asked only when the database
	// does not answer, to say which of several look-alike problems it is.
	// Nil falls back to the generic advice.
	Docker func(ctx context.Context) DockerState
	// Sleep waits between pings while PostgreSQL starts in a Docker the
	// database check has just woken. Nil means time.Sleep.
	Sleep func(time.Duration)
}

// DockerState is what the database check learns from Docker.
type DockerState struct {
	// Daemon is DaemonUp, DaemonDown or DaemonDenied.
	Daemon string
	// Container is the setup container's status as docker reports it
	// (running, exited, created, paused…), or "" when there is none.
	Container string
	// Name is the container that was looked up.
	Name string

	// The Docker systemd units as systemctl reports them, or "" where there
	// is no systemd to ask. They are read before any docker command runs,
	// because a docker command can itself start the daemon through
	// docker.socket: ServiceActive is how the check knows Docker was not
	// running until it looked.
	ServiceActive  string // docker.service: active, inactive, failed…
	ServiceEnabled string // docker.service: enabled, disabled, masked…
	SocketEnabled  string // docker.socket: enabled, disabled…
}

// startedByProbe reports whether Docker was not running until the check's own
// docker command woke it through docker.socket.
func (st DockerState) startedByProbe() bool {
	return st.ServiceActive != "" && st.ServiceActive != "active" && st.Daemon == DaemonUp
}

// bootAdvice says how to make Docker start at boot when systemd reports that
// it does not, and is empty otherwise — including where there is no systemd.
func (st DockerState) bootAdvice() string {
	if st.ServiceEnabled != "disabled" {
		return ""
	}
	if st.SocketEnabled == "enabled" {
		return "Docker does not start at boot on this machine: docker.service is disabled and only docker.socket is enabled, and aidev reaches PostgreSQL over TCP, which does not wake it. Enable it with `sudo systemctl enable docker.service`."
	}
	return "Docker does not start at boot on this machine (docker.service is disabled). Enable it with `sudo systemctl enable docker.service`."
}

// databaseWakeWait bounds how long the database check waits for PostgreSQL in
// a Docker it has just woken, and databaseWakePoll how often it asks.
const (
	databaseWakeWait = 20 * time.Second
	databaseWakePoll = time.Second
)

// Docker daemon states.
const (
	DaemonUp     = "up"
	DaemonDown   = "down"
	DaemonDenied = "denied"
)

// Run performs every check in order and returns one Result per check.
//
// It starts with the configuration, then git, the configured agent command,
// the database, pending migrations, tasks stuck with an expired lease and the
// workspace directory, then the free space under it. When the configuration
// cannot be read, the checks that need it are skipped. When the database does
// not answer, the migrations check is skipped; when migrations are pending or unreadable, the stuck-task check
// is skipped, because the lease columns it queries may not exist yet.
func Run(ctx context.Context, deps Deps) []Result {
	results := make([]Result, 0, 8)

	cfg, err := deps.LoadConfig()
	if err != nil {
		results = append(results, Result{
			Name:    CheckConfig,
			Status:  StatusFail,
			Summary: fmt.Sprintf("Cannot read the configuration: %s.", err.Error()),
			Fix:     "Run `aidev setup` to create a configuration and a database, or set AIDEV_CONFIG to the absolute path of your conf.json (copy conf/conf.example.json to start).",
		})
		results = append(results, gitResult(deps))
		skipped := "Skipped: it needs a readable configuration."
		results = append(results,
			Result{Name: CheckAgent, Status: StatusSkipped, Summary: skipped},
			Result{Name: CheckDatabase, Status: StatusSkipped, Summary: skipped},
			Result{Name: CheckMigrations, Status: StatusSkipped, Summary: skipped},
			Result{Name: CheckStuckTasks, Status: StatusSkipped, Summary: skipped},
			Result{Name: CheckWorkspace, Status: StatusSkipped, Summary: skipped},
			Result{Name: CheckDisk, Status: StatusSkipped, Summary: skipped},
		)
		return results
	}

	results = append(results, Result{
		Name:    CheckConfig,
		Status:  StatusOK,
		Summary: "Configuration is readable.",
	})

	results = append(results, gitResult(deps))
	results = append(results, agentResult(deps, cfg))

	dbErr := databaseResult(ctx, deps, cfg, &results)
	if dbErr != nil {
		results = append(results,
			Result{
				Name:    CheckMigrations,
				Status:  StatusSkipped,
				Summary: "Skipped: the database did not answer, so migrations were not checked.",
			},
			Result{
				Name:    CheckStuckTasks,
				Status:  StatusSkipped,
				Summary: "Skipped: the database did not answer, so stuck tasks were not checked.",
			},
		)
	} else {
		migrations := migrationsResult(ctx, deps, cfg)
		results = append(results, migrations)
		if migrations.Status != StatusOK {
			results = append(results, Result{
				Name:    CheckStuckTasks,
				Status:  StatusSkipped,
				Summary: "Skipped: migrations are pending or unreadable, so the lease columns may not exist.",
			})
		} else {
			results = append(results, stuckTasksResult(ctx, deps, cfg))
		}
	}

	results = append(results, workspaceResult(deps, cfg))
	results = append(results, diskResult(deps, cfg))

	return results
}

// Failed reports whether any result has StatusFail.
//
// Warnings and skipped checks are not failures.
func Failed(results []Result) bool {
	for _, r := range results {
		if r.Status == StatusFail {
			return true
		}
	}
	return false
}

// gitResult checks that git is installed, since aidev manages task worktrees with it.
func gitResult(deps Deps) Result {
	path, err := deps.LookPath("git")
	if err != nil {
		return Result{
			Name:    CheckGit,
			Status:  StatusFail,
			Summary: "git was not found on your PATH.",
			Fix:     "Install git and make sure it is on your PATH.",
		}
	}
	return Result{
		Name:    CheckGit,
		Status:  StatusOK,
		Summary: fmt.Sprintf("Found git at %s.", path),
	}
}

// agentResult checks that the command for the configured agent backend can be found.
func agentResult(deps Deps, cfg config.Config) Result {
	command := cfg.OpenCodeCommand
	isCodex := cfg.AgentBackend == config.BackendCodex
	if isCodex {
		command = cfg.CodexCommand
	}
	path, err := deps.LookPath(command)
	if err != nil {
		if isCodex {
			return Result{
				Name:    CheckAgent,
				Status:  StatusFail,
				Summary: fmt.Sprintf("The codex command %q was not found.", command),
				Fix:     "Install Codex or set agent.codex.command to the Codex executable.",
			}
		}
		return Result{
			Name:    CheckAgent,
			Status:  StatusFail,
			Summary: fmt.Sprintf("The opencode command %q was not found.", command),
			Fix:     "Install OpenCode or set agent.opencode.command to the OpenCode executable.",
		}
	}
	return Result{
		Name:    CheckAgent,
		Status:  StatusOK,
		Summary: fmt.Sprintf("Found %s at %s.", command, path),
	}
}

// databaseResult appends the database check and returns the ping error, if any,
// so Run can skip the migrations check when the database does not answer.
func databaseResult(ctx context.Context, deps Deps, cfg config.Config, results *[]Result) error {
	err := deps.PingDatabase(ctx, cfg.DatabaseURL)
	if err == nil {
		*results = append(*results, Result{
			Name:    CheckDatabase,
			Status:  StatusOK,
			Summary: "Database is reachable.",
		})
		return nil
	}

	summary := fmt.Sprintf("Cannot reach the database: %s.", redactDatabaseURL(err.Error(), cfg.DatabaseURL))
	if _, dockerErr := deps.LookPath("docker"); dockerErr != nil {
		*results = append(*results, Result{
			Name:    CheckDatabase,
			Status:  StatusFail,
			Summary: summary,
			Fix:     "Install Docker and run `aidev setup`, or check that database.url in conf.json points at a running PostgreSQL.",
		})
		return err
	}

	var st DockerState
	known := deps.Docker != nil
	if known {
		st = deps.Docker(ctx)
	}
	// The probe woke Docker and the container is coming back under its
	// restart policy: PostgreSQL is starting, not misconfigured, so give it
	// a moment before calling anything broken.
	if known && st.startedByProbe() && (st.Container == "running" || st.Container == "restarting") {
		if waitForDatabase(ctx, deps, cfg) == nil {
			fix := st.bootAdvice()
			if fix == "" {
				fix = "Make Docker start at boot, so aidev finds PostgreSQL after a restart without this check waking it."
			}
			*results = append(*results, Result{
				Name:    CheckDatabase,
				Status:  StatusWarn,
				Summary: "The database did not answer at first: Docker was not running, and this check started it (a docker command wakes docker.socket). PostgreSQL answers now.",
				Fix:     fix,
			})
			return nil
		}
	}

	fix := "Run `aidev setup` to start PostgreSQL in Docker (safe to run again), or check that database.url in conf.json points at a running PostgreSQL."
	if known {
		fix = dockerFix(st)
	}
	*results = append(*results, Result{
		Name:    CheckDatabase,
		Status:  StatusFail,
		Summary: summary,
		Fix:     fix,
	})
	return err
}

// waitForDatabase pings until the database answers or databaseWakeWait runs out.
func waitForDatabase(ctx context.Context, deps Deps, cfg config.Config) error {
	sleep := deps.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	var err error
	for waited := time.Duration(0); waited < databaseWakeWait; waited += databaseWakePoll {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		sleep(databaseWakePoll)
		if err = deps.PingDatabase(ctx, cfg.DatabaseURL); err == nil {
			return nil
		}
	}
	return err
}

// dockerFix tells apart the problems that all look like "the database does
// not answer" when Docker is installed: the daemon is not running (common
// after a reboot where Docker does not start at boot), this user may not talk
// to it, the check itself just started Docker and PostgreSQL is not up yet,
// the setup container is stopped, or it runs and still does not answer —
// which points at database.url rather than at Docker.
func dockerFix(st DockerState) string {
	withBoot := func(fix string) string {
		if advice := st.bootAdvice(); advice != "" {
			return fix + " " + advice
		}
		return fix
	}
	switch {
	case st.Daemon == DaemonDown:
		return withBoot("Docker is installed but its daemon is not running. Start it (`sudo systemctl start docker` on Linux, or open Docker Desktop), then run `aidev doctor` again.")
	case st.Daemon == DaemonDenied:
		return "Docker is running but this user may not talk to it (permission denied). Add the user to the docker group (`sudo usermod -aG docker $USER`, then log in again), or use rootless Docker."
	case st.startedByProbe() && st.Container != "":
		return withBoot(fmt.Sprintf("Docker was not running; this check started it, and the %s container is %s but PostgreSQL has not answered yet. Give it a few seconds and run `aidev doctor` again.", st.Name, st.Container))
	case st.Container == "running":
		return fmt.Sprintf("The %s container is running but the database does not answer at database.url. Check the host, port, user and password in conf.json against the container (`docker port %s`).", st.Name, st.Name)
	case st.Container != "":
		return fmt.Sprintf("The %s container exists but is %s. Start it with `docker start %s` (or `aidev setup`, which does the same and waits until it is ready).", st.Name, st.Container, st.Name)
	default:
		return "Run `aidev setup` to start PostgreSQL in Docker (safe to run again), or check that database.url in conf.json points at a running PostgreSQL."
	}
}

// migrationsResult checks that no database migration is still waiting to be applied.
func migrationsResult(ctx context.Context, deps Deps, cfg config.Config) Result {
	pending, err := deps.PendingMigrations(ctx, cfg.DatabaseURL)
	if err != nil {
		return Result{
			Name:    CheckMigrations,
			Status:  StatusFail,
			Summary: fmt.Sprintf("Cannot read migrations: %s.", redactDatabaseURL(err.Error(), cfg.DatabaseURL)),
			Fix:     "Check the database and the migration files, then run `aidev migrate`.",
		}
	}
	if len(pending) > 0 {
		return Result{
			Name:    CheckMigrations,
			Status:  StatusFail,
			Summary: fmt.Sprintf("%s pending: %s.", countMigrations(len(pending)), strings.Join(pending, ", ")),
			Fix:     "Run `aidev migrate` to apply them.",
		}
	}
	return Result{
		Name:    CheckMigrations,
		Status:  StatusOK,
		Summary: "Migrations are up to date.",
	}
}

// stuckTasksResult reports tasks whose process stopped heartbeating. Listing
// them is the finding; failing to list them is a failure of the check, not a
// verdict on the tasks.
func stuckTasksResult(ctx context.Context, deps Deps, cfg config.Config) Result {
	stuck, err := deps.StuckTasks(ctx, cfg.DatabaseURL)
	if err != nil {
		return Result{
			Name:    CheckStuckTasks,
			Status:  StatusFail,
			Summary: fmt.Sprintf("Cannot list tasks with an expired lease: %s.", redactDatabaseURL(err.Error(), cfg.DatabaseURL)),
			Fix:     "Check the database, then run `aidev task recover` to clear whatever is stuck.",
		}
	}
	if len(stuck) == 0 {
		return Result{
			Name:    CheckStuckTasks,
			Status:  StatusOK,
			Summary: "No task is stuck with an expired lease.",
		}
	}
	return Result{
		Name:    CheckStuckTasks,
		Status:  StatusWarn,
		Summary: fmt.Sprintf("%s stuck with an expired lease: %s.", countStuck(len(stuck)), strings.Join(stuck, ", ")),
		Fix:     "Run `aidev task recover` to cancel them (`--dry-run` first to see what would be cancelled). Worktrees are kept.",
	}
}

// countStuck says "1 task is" or "N tasks are".
func countStuck(n int) string {
	if n == 1 {
		return "1 task is"
	}
	return fmt.Sprintf("%d tasks are", n)
}

// workspaceResult checks that the workspace directory exists or can be created and is writable.
func workspaceResult(deps Deps, cfg config.Config) Result {
	if err := deps.CheckWorkspace(cfg.WorkspaceRoot); err != nil {
		return Result{
			Name:    CheckWorkspace,
			Status:  StatusFail,
			Summary: fmt.Sprintf("Workspace directory is unusable: %s.", err.Error()),
			Fix:     "Check the workspace_root setting in conf.json and make sure the directory exists and is writable.",
		}
	}
	return Result{
		Name:    CheckWorkspace,
		Status:  StatusOK,
		Summary: fmt.Sprintf("Workspace directory %s is usable.", cfg.WorkspaceRoot),
	}
}

// diskResult reports the free space where task worktrees are created.
func diskResult(deps Deps, cfg config.Config) Result {
	if deps.FreeSpace == nil {
		return Result{Name: CheckDisk, Status: StatusSkipped, Summary: "Skipped: free space cannot be measured on this system."}
	}
	free, err := deps.FreeSpace(cfg.WorkspaceRoot)
	if errors.Is(err, errors.ErrUnsupported) {
		return Result{Name: CheckDisk, Status: StatusSkipped, Summary: "Skipped: free space cannot be measured on this system."}
	}
	if err != nil {
		return Result{
			Name:    CheckDisk,
			Status:  StatusWarn,
			Summary: fmt.Sprintf("Cannot measure free space for %s: %s.", cfg.WorkspaceRoot, err.Error()),
			Fix:     "Check the disk holding workspace_root by hand (df -h).",
		}
	}
	const fix = "Free space before running tasks: `aidev worktree list` shows what retained worktrees hold and " +
		"`aidev worktree remove <task>` reclaims one; `aidev prune --logs-older-than 30d` shrinks the database; " +
		"`docker system df` shows what Docker keeps."
	switch {
	case free < diskFailBytes:
		return Result{Name: CheckDisk, Status: StatusFail,
			Summary: fmt.Sprintf("Only %s free where worktrees are created (%s); a task will likely fail half-way.", humanSize(free), cfg.WorkspaceRoot),
			Fix:     fix}
	case free < diskWarnBytes:
		return Result{Name: CheckDisk, Status: StatusWarn,
			Summary: fmt.Sprintf("%s free where worktrees are created (%s); that is little for a repository copy and its build.", humanSize(free), cfg.WorkspaceRoot),
			Fix:     fix}
	default:
		return Result{Name: CheckDisk, Status: StatusOK,
			Summary: fmt.Sprintf("%s free where worktrees are created.", humanSize(free))}
	}
}

// humanSize renders a byte count the way df -h would.
func humanSize(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	for _, u := range []string{"KiB", "MiB", "GiB", "TiB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, u)
		}
	}
	return fmt.Sprintf("%.1f PiB", value/unit)
}

// redactDatabaseURL replaces every occurrence of the connection string in a
// message with its redacted form, so a password never reaches the reader.
func redactDatabaseURL(msg, databaseURL string) string {
	if databaseURL == "" {
		return msg
	}
	return strings.ReplaceAll(msg, databaseURL, config.RedactURL(databaseURL))
}

// countMigrations says "1 migration is" or "N migrations are".
func countMigrations(n int) string {
	if n == 1 {
		return "1 migration is"
	}
	return fmt.Sprintf("%d migrations are", n)
}
