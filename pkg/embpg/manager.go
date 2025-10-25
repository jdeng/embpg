package embpg

import (
	"archive/zip"
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// LoggerFunc allows callers to observe manager activity.
type LoggerFunc func(format string, args ...interface{})

// Config controls Manager behaviour.
type Config struct {
	BundlePath      string            // Path to static bundle root (e.g. dist/postgresql-darwin-arm64-...).
	DataDir         string            // PostgreSQL data directory to initialise/use.
	Port            int               // TCP port for postgres - required.
	ListenAddress   string            // Defaults to 127.0.0.1.
	Username        string            // Superuser name (default postgres).
	Password        string            // Superuser password (default postgres).
	Database        string            // Database to create; defaults to Username.
	InitDBArgs      []string          // Extra args appended to initdb.
	StartParameters map[string]string // Runtime parameters (-c key=value) passed via pg_ctl -o.
	PostgresArgs    []string          // Extra args appended via pg_ctl -o after StartParameters.
	Env             map[string]string // Extra environment variables for subprocesses.
	LogPath         string            // Optional pg_ctl -l path. Defaults to <dataDir>/log/server.log.
	StartTimeout    time.Duration     // How long pg_ctl waits for start (default 30s).
	StopTimeout     time.Duration     // How long pg_ctl waits for stop (default 30s).
	Logger          LoggerFunc        // Optional logger (defaults to log.Printf).
	Output          io.Writer         // Where to pipe stdout/stderr from subprocesses (default logger-backed).
}

// Manager bootstraps and controls a bundled PostgreSQL instance.
type Manager struct {
	cfg    Config
	binDir string
	libDir string
	env    []string
	logger LoggerFunc
	output io.Writer
	mu     sync.Mutex
}

var errBundleNotFound = errors.New("embpg: bundle not found in archive")

// EnsureBundle verifies that bundleDir contains a usable PostgreSQL bundle. If the
// directory is missing (or incomplete) it downloads the archive from bundleURL,
// extracts it, and returns the absolute bundle path ready for use with New.
func EnsureBundle(ctx context.Context, bundleURL, bundleDir string) (string, error) {
	if bundleURL == "" {
		return "", errors.New("embpg: bundle URL is required")
	}
	if bundleDir == "" {
		return "", errors.New("embpg: bundle directory is required")
	}
	absDir, err := filepath.Abs(bundleDir)
	if err != nil {
		return "", fmt.Errorf("embpg: resolve bundle directory: %w", err)
	}

	info, statErr := os.Stat(absDir)
	switch {
	case statErr == nil:
		if !info.IsDir() {
			return "", fmt.Errorf("embpg: bundle path %s exists but is not a directory", absDir)
		}
		if err := verifyBundleContents(absDir); err == nil {
			return absDir, nil
		}
	case os.IsNotExist(statErr):
		// ok
	default:
		return "", fmt.Errorf("embpg: inspect bundle directory %s: %w", absDir, statErr)
	}

	log.Printf("embpg: downloading bundle from %s", bundleURL)
	if err := downloadAndExtractBundle(ctx, bundleURL, absDir); err != nil {
		return "", err
	}
	if err := verifyBundleContents(absDir); err != nil {
		return "", err
	}
	return absDir, nil
}

// New validates Config and prepares a Manager.
func New(cfg Config) (*Manager, error) {
	if cfg.BundlePath == "" {
		return nil, errors.New("embpg: bundle path is required")
	}
	if cfg.DataDir == "" {
		return nil, errors.New("embpg: data directory is required")
	}
	if cfg.Port <= 0 {
		return nil, errors.New("embpg: port must be > 0")
	}
	if cfg.ListenAddress == "" {
		cfg.ListenAddress = "127.0.0.1"
	}
	if cfg.Username == "" {
		cfg.Username = "postgres"
	}
	if cfg.Password == "" {
		cfg.Password = "postgres"
	}
	if cfg.Database == "" {
		cfg.Database = cfg.Username
	}
	if cfg.StartTimeout <= 0 {
		cfg.StartTimeout = 30 * time.Second
	}
	if cfg.StopTimeout <= 0 {
		cfg.StopTimeout = 30 * time.Second
	}
	absBundle, err := filepath.Abs(cfg.BundlePath)
	if err != nil {
		return nil, fmt.Errorf("embpg: resolve bundle path: %w", err)
	}
	cfg.BundlePath = absBundle
	absData, err := filepath.Abs(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("embpg: resolve data dir: %w", err)
	}
	cfg.DataDir = absData
	if cfg.LogPath == "" {
		cfg.LogPath = filepath.Join(cfg.DataDir, "log", "postgres.log")
	} else if !filepath.IsAbs(cfg.LogPath) {
		cfg.LogPath = filepath.Join(cfg.DataDir, cfg.LogPath)
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Printf
	}

	startParams := make(map[string]string, len(cfg.StartParameters)+1)
	for k, v := range cfg.StartParameters {
		startParams[k] = v
	}
	if _, ok := startParams["max_connections"]; !ok {
		startParams["max_connections"] = "101"
	}
	cfg.StartParameters = startParams

	m := &Manager{
		cfg:    cfg,
		binDir: filepath.Join(cfg.BundlePath, "pgsql", "bin"),
		libDir: filepath.Join(cfg.BundlePath, "pgsql", "lib"),
		logger: cfg.Logger,
	}
	if cfg.Output != nil {
		m.output = cfg.Output
	} else {
		m.output = newLoggerWriter(cfg.Logger)
	}

	if err := m.verifyBundle(); err != nil {
		return nil, err
	}

	m.env = m.buildEnv()
	return m, nil
}

// Prepare initialises the data directory (idempotent).
func (m *Manager) Prepare(ctx context.Context) error {
	if err := os.MkdirAll(m.cfg.DataDir, 0o700); err != nil {
		return fmt.Errorf("embpg: create data dir: %w", err)
	}
	if m.isInitialised() {
		return nil
	}
	if err := m.runInitDB(ctx); err != nil {
		return err
	}
	m.logf("Initialised PostgreSQL cluster (user=%s password=%s database=%s)", m.cfg.Username, m.cfg.Password, m.cfg.Database)
	return nil
}

// Start ensures the cluster is ready and runs pg_ctl start.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.Prepare(ctx); err != nil {
		return err
	}
	if running, err := m.isRunning(ctx); err != nil {
		return err
	} else if running {
		m.logf("PostgreSQL already running (dataDir=%s)", m.cfg.DataDir)
		return nil
	}

	if err := ensurePortAvailable(m.cfg.ListenAddress, m.cfg.Port); err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(m.cfg.LogPath), 0o755); err != nil {
		return fmt.Errorf("embpg: ensure log dir: %w", err)
	}

	args := []string{"start", "-D", m.cfg.DataDir, "-l", m.cfg.LogPath, "-w"}
	if secs := int(m.cfg.StartTimeout.Seconds()); secs > 0 {
		args = append(args, "-t", strconv.Itoa(secs))
	}

	startOpts := []string{
		fmt.Sprintf("-p %d", m.cfg.Port),
		fmt.Sprintf("-h %s", m.cfg.ListenAddress),
	}
	startOpts = append(startOpts, encodeStartParameters(m.cfg.StartParameters)...)
	startOpts = append(startOpts, m.cfg.PostgresArgs...)
	if len(startOpts) > 0 {
		args = append(args, "-o", strings.Join(startOpts, " "))
	}

	m.logf("Starting PostgreSQL (dataDir=%s port=%d)", m.cfg.DataDir, m.cfg.Port)
	if err := m.runCommand(ctx, m.pgCtlPath(), args, map[string]string{}); err != nil {
		return fmt.Errorf("embpg: pg_ctl start: %w", err)
	}
	if err := m.ensureDatabase(ctx); err != nil {
		return err
	}
	return nil
}

// Stop shuts down the instance if it is running.
func (m *Manager) Stop(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	running, err := m.isRunning(ctx)
	if err != nil {
		return err
	}
	if !running {
		m.logf("PostgreSQL not running (dataDir=%s)", m.cfg.DataDir)
		return nil
	}

	args := []string{"stop", "-D", m.cfg.DataDir, "-m", "fast", "-w"}
	if secs := int(m.cfg.StopTimeout.Seconds()); secs > 0 {
		args = append(args, "-t", strconv.Itoa(secs))
	}

	m.logf("Stopping PostgreSQL (dataDir=%s)", m.cfg.DataDir)
	if err := m.runCommand(ctx, m.pgCtlPath(), args, map[string]string{}); err != nil {
		return fmt.Errorf("embpg: pg_ctl stop: %w", err)
	}
	return nil
}

// IsRunning reports whether pg_ctl thinks the instance is up.
func (m *Manager) IsRunning(ctx context.Context) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.isRunning(ctx)
}

// Port returns the configured port.
func (m *Manager) Port() int { return m.cfg.Port }

// DataDir returns the absolute data directory.
func (m *Manager) DataDir() string { return m.cfg.DataDir }

// BundlePath returns the bundle root.
func (m *Manager) BundlePath() string { return m.cfg.BundlePath }

func (m *Manager) isInitialised() bool {
	if _, err := os.Stat(filepath.Join(m.cfg.DataDir, "PG_VERSION")); err == nil {
		return true
	}
	return false
}

func (m *Manager) verifyBundle() error {
	return verifyBundleContents(m.cfg.BundlePath)
}

func verifyBundleContents(bundlePath string) error {
	required := []string{"postgres", "initdb", "pg_ctl"}
	for _, name := range required {
		path := filepath.Join(bundlePath, "pgsql", "bin", name)
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("embpg: expected %s in bundle (looked in %s): %w", name, path, err)
		}
		if info.IsDir() {
			return fmt.Errorf("embpg: expected %s in bundle, found directory", path)
		}
	}
	return nil
}

func (m *Manager) runInitDB(ctx context.Context) error {
	m.logf("Initialising data directory %s", m.cfg.DataDir)
	pwFile, err := writePasswordFile(m.cfg.DataDir, m.cfg.Password)
	if err != nil {
		return err
	}
	defer func() {
		_ = os.Remove(pwFile)
	}()

	args := []string{
		"--pgdata", m.cfg.DataDir,
		"--encoding", "UTF8",
		"--locale", "C",
		"--username", m.cfg.Username,
		"--pwfile", pwFile,
		"-A", "password",
	}
	args = append(args, m.cfg.InitDBArgs...)
	if err := m.runCommand(ctx, m.initDBPath(), args, map[string]string{}); err != nil {
		return fmt.Errorf("embpg: initdb: %w", err)
	}
	return nil
}

func (m *Manager) isRunning(ctx context.Context) (bool, error) {
	args := []string{"status", "-D", m.cfg.DataDir}
	cmd := m.command(ctx, m.pgCtlPath(), args, map[string]string{})
	if m.output != nil {
		statusLogs := newCommandLogger(m.output)
		cmd.Stdout = statusLogs.Writer()
		cmd.Stderr = statusLogs.Writer()
	}
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			switch exitErr.ExitCode() {
			case 0:
				return true, nil
			case 3:
				return false, nil
			default:
				return false, fmt.Errorf("embpg: pg_ctl status failed: %w", err)
			}
		}
		return false, fmt.Errorf("embpg: pg_ctl status error: %w", err)
	}
	return true, nil
}

func (m *Manager) runCommand(ctx context.Context, path string, args []string, extraEnv map[string]string) error {
	cmd := m.command(ctx, path, args, extraEnv)
	commandLogs := newCommandLogger(m.output)
	cmd.Stdout = commandLogs.Writer()
	cmd.Stderr = commandLogs.Writer()
	m.logf("Run: %s %s", path, strings.Join(args, " "))
	if err := cmd.Run(); err != nil {
		if logTail := strings.TrimSpace(commandLogs.Logs()); logTail != "" {
			return fmt.Errorf("%w\n%s", err, logTail)
		}
		return err
	}
	return nil
}

func (m *Manager) command(ctx context.Context, path string, args []string, extraEnv map[string]string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = m.mergeEnv(extraEnv)
	return cmd
}

func (m *Manager) mergeEnv(extra map[string]string) []string {
	envMap := make(map[string]string, len(m.env))
	for _, entry := range m.env {
		parts := strings.SplitN(entry, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}
	for k, v := range extra {
		envMap[k] = v
	}
	merged := make([]string, 0, len(envMap))
	for k, v := range envMap {
		merged = append(merged, fmt.Sprintf("%s=%s", k, v))
	}
	return merged
}

func (m *Manager) buildEnv() []string {
	envMap := make(map[string]string)
	for _, entry := range os.Environ() {
		parts := strings.SplitN(entry, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}
	for k, v := range m.cfg.Env {
		envMap[k] = v
	}

	envMap["PGDATA"] = m.cfg.DataDir
	path := filepath.Join(m.binDir)
	envMap["PATH"] = prependPath(path, envMap["PATH"])

	libKey := ""
	switch runtime.GOOS {
	case "darwin":
		libKey = "DYLD_LIBRARY_PATH"
	case "linux":
		libKey = "LD_LIBRARY_PATH"
	}
	if libKey != "" {
		envMap[libKey] = prependPath(m.libDir, envMap[libKey])
	}
	envMap["PGPORT"] = strconv.Itoa(m.cfg.Port)

	result := make([]string, 0, len(envMap))
	for k, v := range envMap {
		result = append(result, fmt.Sprintf("%s=%s", k, v))
	}
	return result
}

func prependPath(prefix, existing string) string {
	if existing == "" {
		return prefix
	}
	return fmt.Sprintf("%s:%s", prefix, existing)
}

func encodeStartParameters(params map[string]string) []string {
	if len(params) == 0 {
		return nil
	}

	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	encoded := make([]string, 0, len(params))
	for _, k := range keys {
		encoded = append(encoded, fmt.Sprintf("-c %s=\"%s\"", k, params[k]))
	}

	return encoded
}

func ensurePortAvailable(address string, port int) error {
	hostPort := net.JoinHostPort(address, strconv.Itoa(port))
	listener, err := net.Listen("tcp", hostPort)
	if err != nil {
		return fmt.Errorf("embpg: port %d unavailable on %s: %w", port, address, err)
	}
	if err := listener.Close(); err != nil {
		return fmt.Errorf("embpg: release port %d on %s: %w", port, address, err)
	}
	return nil
}

const commandLogLimit = 64 * 1024

type commandLogger struct {
	writer io.Writer
	buf    *logBuffer
}

func newCommandLogger(dst io.Writer) *commandLogger {
	buf := newLogBuffer(commandLogLimit)
	var writer io.Writer = buf
	if dst != nil {
		writer = io.MultiWriter(dst, buf)
	}
	return &commandLogger{writer: writer, buf: buf}
}

func (c *commandLogger) Writer() io.Writer { return c.writer }

func (c *commandLogger) Logs() string { return c.buf.String() }

type logBuffer struct {
	data []byte
	max  int
}

func newLogBuffer(max int) *logBuffer {
	return &logBuffer{max: max}
}

func (b *logBuffer) Write(p []byte) (int, error) {
	if b.max <= 0 {
		b.data = append(b.data, p...)
		return len(p), nil
	}

	if len(p) >= b.max {
		b.data = append(b.data[:0], p[len(p)-b.max:]...)
		return len(p), nil
	}

	overflow := len(b.data) + len(p) - b.max
	if overflow > 0 {
		b.data = append(b.data[overflow:], p...)
		return len(p), nil
	}

	b.data = append(b.data, p...)
	return len(p), nil
}

func (b *logBuffer) String() string {
	return string(b.data)
}

func downloadAndExtractBundle(ctx context.Context, bundleURL, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("embpg: ensure bundle parent directory: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, bundleURL, nil)
	if err != nil {
		return fmt.Errorf("embpg: create download request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("embpg: download bundle: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("embpg: download bundle: unexpected status %s", resp.Status)
	}

	tmpFile, err := os.CreateTemp("", "embpg-bundle-*.zip")
	if err != nil {
		return fmt.Errorf("embpg: create temporary archive: %w", err)
	}
	defer func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpFile.Name())
	}()

	if _, err := io.Copy(tmpFile, resp.Body); err != nil {
		return fmt.Errorf("embpg: download bundle: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("embpg: close bundle archive: %w", err)
	}

	tmpDir, err := os.MkdirTemp(filepath.Dir(dest), "embpg-bundle-")
	if err != nil {
		tmpDir, err = os.MkdirTemp("", "embpg-bundle-")
		if err != nil {
			return fmt.Errorf("embpg: prepare temporary directory: %w", err)
		}
	}
	defer func() {
		_ = os.RemoveAll(tmpDir)
	}()

	if err := unzipArchive(tmpFile.Name(), tmpDir); err != nil {
		return err
	}

	bundleRoot, err := locateBundleRoot(tmpDir)
	if err != nil {
		return err
	}

	if err := os.RemoveAll(dest); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("embpg: clear bundle directory: %w", err)
	}
	if err := ensureMove(bundleRoot, dest); err != nil {
		return err
	}
	return nil
}

func unzipArchive(zipPath, dest string) error {
	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("embpg: open bundle archive: %w", err)
	}
	defer reader.Close()

	for _, f := range reader.File {
		if err := extractZipEntry(dest, f); err != nil {
			return err
		}
	}
	return nil
}

func extractZipEntry(dest string, f *zip.File) error {
	name := filepath.Clean(f.Name)
	name = strings.TrimPrefix(name, "./")
	if name == "." || name == "" {
		return nil
	}
	target := filepath.Join(dest, filepath.FromSlash(name))
	rel, err := filepath.Rel(dest, target)
	if err != nil {
		return fmt.Errorf("embpg: normalise archive entry %s: %w", f.Name, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("embpg: archive entry %s escapes destination", f.Name)
	}

	mode := f.Mode()
	if f.FileInfo().IsDir() {
		if mode == 0 {
			mode = 0o755
		}
		return os.MkdirAll(target, mode)
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("embpg: prepare directory for %s: %w", target, err)
	}
	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("embpg: open archive entry %s: %w", f.Name, err)
	}
	defer rc.Close()

	if mode == 0 {
		mode = 0o644
	}
	dst, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("embpg: create file %s: %w", target, err)
	}
	if _, err := io.Copy(dst, rc); err != nil {
		_ = dst.Close()
		return fmt.Errorf("embpg: extract file %s: %w", target, err)
	}
	if err := dst.Close(); err != nil {
		return fmt.Errorf("embpg: close file %s: %w", target, err)
	}
	return nil
}

func locateBundleRoot(root string) (string, error) {
	candidate, err := findBundleRoot(root, 4)
	if err != nil {
		if errors.Is(err, errBundleNotFound) {
			return "", fmt.Errorf("embpg: downloaded archive did not contain a valid bundle")
		}
		return "", err
	}
	return candidate, nil
}

func findBundleRoot(path string, depth int) (string, error) {
	if err := verifyBundleContents(path); err == nil {
		return path, nil
	}
	if depth == 0 {
		return "", errBundleNotFound
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return "", fmt.Errorf("embpg: inspect archive directory %s: %w", path, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		candidate, err := findBundleRoot(filepath.Join(path, entry.Name()), depth-1)
		if err == nil {
			return candidate, nil
		}
		if !errors.Is(err, errBundleNotFound) {
			return "", err
		}
	}
	return "", errBundleNotFound
}

func ensureMove(src, dest string) error {
	if err := os.Rename(src, dest); err == nil {
		return nil
	} else if copyErr := copyDirectory(src, dest); copyErr != nil {
		return fmt.Errorf("embpg: move bundle into place: %v (copy fallback failed: %w)", err, copyErr)
	}
	return nil
}

func copyDirectory(src, dest string) error {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return fmt.Errorf("embpg: create directory %s: %w", dest, err)
	}
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dest, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			mode := info.Mode()
			if mode == 0 {
				mode = 0o755
			}
			return os.MkdirAll(target, mode)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return copyFileWithMode(path, target, info.Mode())
	})
}

func copyFileWithMode(src, dest string, mode os.FileMode) error {
	if mode == 0 {
		mode = 0o644
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

func writePasswordFile(dataDir, password string) (string, error) {
	candidates := make([]string, 0, 2)
	if cleaned := filepath.Clean(dataDir); cleaned != "" && cleaned != "." {
		if parent := filepath.Dir(cleaned); parent != "" && parent != "." && parent != cleaned {
			candidates = append(candidates, parent)
		}
	}
	candidates = append(candidates, os.TempDir())

	var lastErr error
	for _, dir := range candidates {
		file, err := os.CreateTemp(dir, "pg_pw_*")
		if err != nil {
			lastErr = err
			continue
		}
		if err := file.Chmod(0o600); err != nil {
			_ = file.Close()
			_ = os.Remove(file.Name())
			lastErr = err
			continue
		}
		if _, err := file.WriteString(password); err != nil {
			_ = file.Close()
			_ = os.Remove(file.Name())
			lastErr = err
			continue
		}
		if err := file.Close(); err != nil {
			_ = os.Remove(file.Name())
			lastErr = err
			continue
		}
		return file.Name(), nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no suitable directory for password file")
	}
	return "", fmt.Errorf("embpg: create password file: %w", lastErr)
}

func (m *Manager) ensureDatabase(ctx context.Context) error {
	if m.cfg.Database == "" || strings.EqualFold(m.cfg.Database, "postgres") {
		return nil
	}
	if strings.EqualFold(m.cfg.Database, m.cfg.Username) {
		return nil
	}
	createdb := filepath.Join(m.binDir, "createdb")
	args := []string{
		"-h", m.cfg.ListenAddress,
		"-p", strconv.Itoa(m.cfg.Port),
		"-U", m.cfg.Username,
		m.cfg.Database,
	}
	m.logf("Ensuring database %s exists", m.cfg.Database)
	env := map[string]string{
		"PGPASSWORD": m.cfg.Password,
	}
	if err := m.runCommand(ctx, createdb, args, env); err != nil {
		errText := strings.ToLower(err.Error())
		if strings.Contains(errText, "already exists") {
			m.logf("Database %s already exists", m.cfg.Database)
			return nil
		}
		return fmt.Errorf("embpg: ensure database %s: %w", m.cfg.Database, err)
	}
	return nil
}

func (m *Manager) pgCtlPath() string  { return filepath.Join(m.binDir, "pg_ctl") }
func (m *Manager) initDBPath() string { return filepath.Join(m.binDir, "initdb") }

func (m *Manager) logf(format string, args ...interface{}) {
	if m.logger != nil {
		m.logger(format, args...)
	}
}

func newLoggerWriter(logger LoggerFunc) io.Writer {
	if logger == nil {
		return io.Discard
	}
	pr, pw := io.Pipe()

	go func() {
		scanner := bufio.NewScanner(pr)
		for scanner.Scan() {
			logger("%s", scanner.Text())
		}
		_ = pr.Close()
	}()

	return pw
}
