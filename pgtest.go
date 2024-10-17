// Spawns a PostgreSQL server with a single database configured. Ideal for unit
// tests where you want a clean instance each time. Then clean up afterwards.
//
// Requires PostgreSQL to be installed on your system (but it doesn't have to be running).
package pgtest

import (
	"database/sql"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "github.com/lib/pq"
)

type PG struct {
	dir string
	cmd *exec.Cmd
	DB  *sql.DB

	Host string
	User string
	Name string

	persistent bool

	stderr io.ReadCloser
	stdout io.ReadCloser
}

// Start a new PostgreSQL database, on temporary storage.
//
// This database has fsync disabled for performance, so it might run faster
// than your production database. This makes it less reliable in case of system
// crashes, but we don't care about that anyway during unit testing.
//
// Use the DB field to access the database connection
func Start() (*PG, error) {
	return start(New())
}

// Starts a new PostgreSQL database
//
// Will listen on a unix socket and initialize the database in the given
// folder, if needed. Data isn't removed when calling Stop(), so this database
// can be used multiple times. Allows using PostgreSQL as an embedded databases
// (such as SQLite). Not for production usage!
func StartPersistent(folder string) (*PG, error) {
	return start(New().DataDir(folder).Persistent())
}

// start Starts a new PostgreSQL database
//
// Will listen on a unix socket and initialize the database in the given
// folder (config.Dir), if needed.
// Data isn't removed when calling Stop() if config.Persistent == true,
// so this database
// can be used multiple times. Allows using PostgreSQL as an embedded databases
// (such as SQLite). Not for production usage!
func start(config *PGConfig) (*PG, error) {
	// Find executables root path
	binPath, err := findBinPath(config.BinDir)
	if err != nil {
		return nil, err
	}

	// Handle dropping permissions when running as root
	me, err := user.Current()
	if err != nil {
		return nil, err
	}
	isRoot := me.Username == "root"

	pgUID := int(0)
	pgGID := int(0)
	if isRoot {
		pgUser, err := user.Lookup("postgres")
		if err != nil {
			return nil, fmt.Errorf("Could not find postgres user, which is required when running as root: %s", err)
		}

		uid, err := strconv.ParseInt(pgUser.Uid, 10, 64)
		if err != nil {
			return nil, err
		}
		pgUID = int(uid)

		gid, err := strconv.ParseInt(pgUser.Gid, 10, 64)
		if err != nil {
			return nil, err
		}
		pgGID = int(gid)
	}

	// Prepare data directory
	dir := config.Dir
	if config.Dir == "" {
		d, err := os.MkdirTemp("", "pgtest")
		if err != nil {
			return nil, err
		}
		dir = d
	}

	dataDir := filepath.Join(dir, "data")
	sockDir := filepath.Join(dir, "sock")

	err = os.MkdirAll(dataDir, 0711)
	if err != nil {
		return nil, err
	}

	err = os.MkdirAll(sockDir, 0711)
	if err != nil {
		return nil, err
	}

	if isRoot {
		err = os.Chmod(dir, 0711)
		if err != nil {
			return nil, err
		}

		err = os.Chown(dataDir, pgUID, pgGID)
		if err != nil {
			return nil, err
		}

		err = os.Chown(sockDir, pgUID, pgGID)
		if err != nil {
			return nil, err
		}
	}

	// Initialize PostgreSQL data directory
	_, err = os.Stat(filepath.Join(dataDir, "postgresql.conf"))
	if os.IsNotExist(err) {
		init := prepareCommand(isRoot, filepath.Join(binPath, "initdb"),
			"-D", dataDir,
			"--no-sync",
		)
		out, err := init.CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("Failed to initialize DB: %w -> %s", err, string(out))
		}
	}

	// Start PostgreSQL
	args := []string{
		"-D", dataDir, // Data directory
		"-k", sockDir, // Location for the UNIX socket
		"-h", "", // Disable TCP listening
	}

	if config.FSync == false {
		args = append(args, "-F")
	}

	if len(config.AdditionalArgs) > 0 {
		args = append(args, config.AdditionalArgs...)
	}
	cmd := prepareCommand(isRoot, filepath.Join(binPath, "postgres"),
		args...,
	)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stderr.Close()
		return nil, err
	}

	err = cmd.Start()
	if err != nil {
		return nil, abort("Failed to start PostgreSQL", cmd, stderr, stdout, err)
	}

	// Connect to DB
	dsn := makeDSN(sockDir, "postgres", isRoot)
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, abort("Failed to connect to DB", cmd, stderr, stdout, err)
	}

	// Prepare test database
	err = retry(func() error {
		var exists bool
		err = db.QueryRow("SELECT 1 FROM pg_database WHERE datname = 'test'").Scan(&exists)
		if exists {
			return nil
		}

		_, err := db.Exec("CREATE DATABASE test")
		return err
	}, 1000, 10*time.Millisecond)
	if err != nil {
		return nil, abort("Failed to prepare test DB", cmd, stderr, stdout, err)
	}

	err = db.Close()
	if err != nil {
		return nil, abort("Failed to disconnect", cmd, stderr, stdout, err)
	}

	// Connect to it properly
	dsn = makeDSN(sockDir, "test", isRoot)
	db, err = sql.Open("postgres", dsn)
	if err != nil {
		return nil, abort("Failed to connect to test DB", cmd, stderr, stdout, err)
	}

	pg := &PG{
		cmd: cmd,
		dir: dir,

		DB: db,

		Host: sockDir,
		User: pgUser(isRoot),
		Name: "test",

		persistent: config.IsPersistent,

		stderr: stderr,
		stdout: stdout,
	}

	return pg, nil
}

// Stop the database and remove storage files.
func (p *PG) Stop() error {
	if p == nil {
		return nil
	}

	if !p.persistent {
		defer func() {
			// Always try to remove it
			os.RemoveAll(p.dir)
		}()
	}

	err := p.cmd.Process.Signal(os.Interrupt)
	if err != nil {
		return err
	}

	// Doesn't matter if the server exists with an error
	err = p.cmd.Wait()
	if err != nil {
		_ = p.cmd.Process.Signal(os.Kill)

		// Remove UNIX sockets
		files, err := os.ReadDir(p.Host)
		if err == nil {
			for _, file := range files {
				_ = os.Remove(filepath.Join(p.Host, file.Name()))
			}
		}
	}

	if p.stderr != nil {
		p.stderr.Close()
	}

	if p.stdout != nil {
		p.stdout.Close()
	}

	return nil
}

// Needed because Ubuntu doesn't put initdb in $PATH
// binDir a path to a directory that contains postgresql binaries
func findBinPath(binDir string) (string, error) {
	// In $PATH (e.g. Fedora) great!
	if binDir == "" {
		p, err := exec.LookPath("initdb")
		if err == nil {
			return path.Dir(p), nil
		}
	}

	// Look for a PostgreSQL in one of the folders Ubuntu uses
	folders := []string{
		binDir,
		"/usr/lib/postgresql/",
	}
	for _, folder := range folders {
		f, err := os.Stat(folder)
		if os.IsNotExist(err) {
			continue
		}
		if !f.IsDir() {
			continue
		}

		files, err := os.ReadDir(folder)
		if err != nil {
			return "", err
		}
		for _, fi := range files {
			if !fi.IsDir() && "initdb" == fi.Name() {
				return filepath.Join(folder), nil
			}

			if !fi.IsDir() {
				continue
			}

			binPath := filepath.Join(folder, fi.Name(), "bin")
			_, err := os.Stat(filepath.Join(binPath, "initdb"))
			if err == nil {
				return binPath, nil
			}
		}
	}

	return "", fmt.Errorf("Did not find PostgreSQL executables installed")
}

func pgUser(isRoot bool) string {
	user := ""
	if isRoot {
		user = "postgres"
	}
	return user
}

func makeDSN(sockDir, dbname string, isRoot bool) string {
	dsnUser := ""
	user := pgUser(isRoot)
	if user != "" {
		dsnUser = fmt.Sprintf("user=%s", user)
	}
	return fmt.Sprintf("host=%s dbname=%s %s", sockDir, dbname, dsnUser)
}

func retry(fn func() error, attempts int, interval time.Duration) error {
	for {
		err := fn()
		if err == nil {
			return nil
		}

		attempts -= 1
		if attempts <= 0 {
			return err
		}

		time.Sleep(interval)
	}
}

func prepareCommand(isRoot bool, command string, args ...string) *exec.Cmd {
	var cmd *exec.Cmd
	if !isRoot {
		cmd = exec.Command(command, args...)
	} else {
		for i, a := range args {
			if a == "" {
				args[i] = "''"
			}
		}

		cmd = exec.Command("su",
			"-",
			"postgres",
			"-c",
			strings.Join(append([]string{command}, args...), " "),
		)
	}

	cmd.Env = append(
		os.Environ(),
		"LC_ALL=en_US.UTF-8", // Fix for https://github.com/Homebrew/homebrew-core/issues/124215 in Mac OS X
	)

	return cmd
}

func abort(msg string, cmd *exec.Cmd, stderr, stdout io.ReadCloser, err error) error {
	_ = cmd.Process.Signal(os.Interrupt)
	_ = cmd.Wait()

	serr, _ := io.ReadAll(stderr)
	sout, _ := io.ReadAll(stdout)
	_ = stderr.Close()
	_ = stdout.Close()
	return fmt.Errorf("%s: %s\nOUT: %s\nERR: %s", msg, err, string(sout), string(serr))
}
