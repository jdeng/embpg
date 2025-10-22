package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	componentZlib     = "zlib"
	componentReadline = "readline"
	componentOpenssl  = "openssl"
	componentICU      = "icu"
	componentPostgres = "postgresql"
)

type component struct {
	Version   string `json:"version"`
	URL       string `json:"url"`
	SHA256    string `json:"sha256"`
	SourceDir string `json:"sourceDir"`
}

type dependencies struct {
	Zlib     component `json:"zlib"`
	Readline component `json:"readline"`
	Openssl  component `json:"openssl"`
	ICU      component `json:"icu"`
}

type config struct {
	RootDir      string       `json:"rootDir"`
	BundleDir    string       `json:"bundleDir"`
	CPUCount     int          `json:"cpuCount"`
	Dependencies dependencies `json:"dependencies"`
	Postgres     component    `json:"postgres"`
}

type builder struct {
	cfg            config
	rootDir        string
	srcDir         string
	prefixDir      string
	pgPrefixDir    string
	bundleDir      string
	cpuCount       int
	osName         string
	arch           string
	opensslTarget  string
	rpathFlag      string
	platformCFlags string
	baseEnv        map[string]string
}

func main() {
	cfgPath := flag.String("config", "build-config.json", "path to build configuration file")
	action := flag.String("action", "build", "action to perform: build or clean")
	rootOverride := flag.String("root", "", "override build root directory")
	buildDirFlag := flag.String("build-dir", "", "path to build workspace (default ./build)")
	outputDirFlag := flag.String("output-dir", "", "path to final bundle directory (default ./dist/postgresql-<os>-<arch>-<version>)")
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fail(err)
	}

	cwd, err := os.Getwd()
	if err != nil {
		fail(err)
	}

	if *rootOverride != "" {
		cfg.RootDir = *rootOverride
	}
	if *buildDirFlag != "" {
		cfg.RootDir = *buildDirFlag
	}

	applyDefaults(&cfg, cwd)
	if err := validateConfig(cfg); err != nil {
		fail(err)
	}
	bld, err := newBuilder(cfg, cwd, *outputDirFlag)
	if err != nil {
		fail(err)
	}

	ctx := context.Background()
	switch strings.ToLower(*action) {
	case "build":
		if err := bld.Build(ctx); err != nil {
			fail(err)
		}
	case "clean":
		if err := bld.Clean(); err != nil {
			fail(err)
		}
	default:
		fail(fmt.Errorf("unknown action %q", *action))
	}
	logf("All done - test with: cd %s && ./init-database.sh", filepath.Base(bld.bundleDir))
}

func loadConfig(path string) (config, error) {
	file, err := os.Open(path)
	if err != nil {
		return config{}, err
	}
	defer file.Close()

	var cfg config
	if err := json.NewDecoder(file).Decode(&cfg); err != nil {
		return config{}, err
	}
	return cfg, nil
}

func applyDefaults(cfg *config, cwd string) {
	if cfg.RootDir == "" {
		if v := os.Getenv("ROOT_DIR"); v != "" {
			cfg.RootDir = v
		} else {
			cfg.RootDir = filepath.Join(cwd, "build")
		}
	}
	if cfg.Dependencies.Zlib.SourceDir == "" && cfg.Dependencies.Zlib.Version != "" {
		cfg.Dependencies.Zlib.SourceDir = fmt.Sprintf("zlib-%s", cfg.Dependencies.Zlib.Version)
	}
	if cfg.Dependencies.Readline.SourceDir == "" && cfg.Dependencies.Readline.Version != "" {
		cfg.Dependencies.Readline.SourceDir = fmt.Sprintf("readline-%s", cfg.Dependencies.Readline.Version)
	}
	if cfg.Dependencies.Openssl.SourceDir == "" && cfg.Dependencies.Openssl.Version != "" {
		cfg.Dependencies.Openssl.SourceDir = fmt.Sprintf("openssl-%s", cfg.Dependencies.Openssl.Version)
	}
	if cfg.Dependencies.ICU.SourceDir == "" {
		if cfg.Dependencies.ICU.Version != "" {
			cfg.Dependencies.ICU.SourceDir = "icu/source"
		}
	}
	if cfg.Postgres.SourceDir == "" && cfg.Postgres.Version != "" {
		cfg.Postgres.SourceDir = fmt.Sprintf("postgresql-%s", cfg.Postgres.Version)
	}
}

func validateConfig(cfg config) error {
	if cfg.RootDir == "" {
		return errors.New("rootDir must be specified in config or via --root")
	}
	if cfg.Postgres.URL == "" {
		return errors.New("postgres url must be specified")
	}
	if cfg.Postgres.Version == "" {
		return errors.New("postgres version must be specified")
	}
	if cfg.Postgres.SourceDir == "" {
		return errors.New("postgres sourceDir must be specified or derivable")
	}
	return nil
}

func newBuilder(cfg config, cwd, outputBase string) (*builder, error) {
	osName := runtime.GOOS
	if osName != "darwin" && osName != "linux" {
		return nil, fmt.Errorf("unsupported OS: %s", osName)
	}
	arch, err := detectArch()
	if err != nil {
		return nil, err
	}

	cpuCount := cfg.CPUCount
	if cpuCount <= 0 {
		cpuCount = runtime.NumCPU()
	}
	rootDir := cfg.RootDir
	if !filepath.IsAbs(rootDir) {
		rootDir = filepath.Join(cwd, rootDir)
	}
	srcDir := filepath.Join(rootDir, "src")
	prefixDir := filepath.Join(rootDir, "build")
	pgPrefixDir := filepath.Join(rootDir, "pgsql")

	opensslTarget, platformCFlags, rpathFlag, err := platformSettings(osName, arch)
	if err != nil {
		return nil, err
	}

	bundleName := fmt.Sprintf("postgresql-%s-%s-%s", osName, arch, cfg.Postgres.Version)
	if outputBase != "" {
		if !filepath.IsAbs(outputBase) {
			outputBase = filepath.Join(cwd, outputBase)
		}
		cfg.BundleDir = filepath.Join(outputBase, bundleName)
	} else if cfg.BundleDir == "" {
		cfg.BundleDir = filepath.Join(cwd, "dist", bundleName)
	} else if !filepath.IsAbs(cfg.BundleDir) {
		cfg.BundleDir = filepath.Join(cwd, cfg.BundleDir)
	}

	ldflagsParts := []string{fmt.Sprintf("-L%s", filepath.Join(prefixDir, "lib"))}
	if rpathFlag != "" {
		ldflagsParts = append(ldflagsParts, rpathFlag)
	}
	switch osName {
	case "darwin":
		ldflagsParts = append(ldflagsParts, "-lc++")
	}
	ldflags := strings.Join(ldflagsParts, " ")

	baseEnv := map[string]string{
		"CFLAGS":          fmt.Sprintf("-O2 -fPIC %s", platformCFlags),
		"CXXFLAGS":        fmt.Sprintf("-O2 -fPIC %s", platformCFlags),
		"CPPFLAGS":        fmt.Sprintf("-I%s", filepath.Join(prefixDir, "include")),
		"LDFLAGS":         ldflags,
		"PKG_CONFIG_PATH": filepath.Join(prefixDir, "lib", "pkgconfig"),
	}

	return &builder{
		cfg:            cfg,
		rootDir:        rootDir,
		srcDir:         srcDir,
		prefixDir:      prefixDir,
		pgPrefixDir:    pgPrefixDir,
		bundleDir:      cfg.BundleDir,
		cpuCount:       cpuCount,
		osName:         osName,
		arch:           arch,
		opensslTarget:  opensslTarget,
		rpathFlag:      rpathFlag,
		platformCFlags: platformCFlags,
		baseEnv:        baseEnv,
	}, nil
}

func platformSettings(osName, arch string) (string, string, string, error) {
	switch osName {
	case "darwin":
		return fmt.Sprintf("darwin64-%s-cc", arch), "-mmacosx-version-min=15.4", "-Wl,-rpath,@loader_path/../lib", nil
	case "linux":
		var opensslArch string
		switch arch {
		case "x86_64", "amd64":
			opensslArch = "x86_64"
		default:
			opensslArch = arch
		}
		return fmt.Sprintf("linux-%s", opensslArch), "", "-Wl,-rpath,\\$ORIGIN/../lib", nil
	default:
		return "", "", "", fmt.Errorf("unsupported OS: %s", osName)
	}
}

func detectArch() (string, error) {
	out, err := exec.Command("uname", "-m").Output()
	if err != nil {
		return "", fmt.Errorf("detect architecture: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (b *builder) Build(ctx context.Context) error {
	logf("Building dependencies -> PostgreSQL")
	if err := os.MkdirAll(b.srcDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(b.prefixDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(b.pgPrefixDir, 0o755); err != nil {
		return err
	}

	if err := b.buildZlib(ctx); err != nil {
		return err
	}
	if err := b.buildReadline(ctx); err != nil {
		return err
	}
	if err := b.buildOpenssl(ctx); err != nil {
		return err
	}
	if err := b.buildICU(ctx); err != nil {
		return err
	}
	if err := b.buildPostgres(ctx); err != nil {
		return err
	}
	return b.packageBundle()
}

func (b *builder) Clean() error {
	logf("Cleaning build artifacts")
	if err := os.RemoveAll(b.rootDir); err != nil {
		return err
	}
	if err := os.RemoveAll(b.bundleDir); err != nil {
		return err
	}
	return nil
}

func (b *builder) buildZlib(ctx context.Context) error {
	comp := b.cfg.Dependencies.Zlib
	if comp.URL == "" {
		logf("Skip zlib: missing URL")
		return nil
	}
	dir, err := b.prepareComponent(componentZlib, comp)
	if err != nil {
		return err
	}
	return b.buildWithStamp(componentZlib, func() error {
		if err := b.run(ctx, dir, "./configure", []string{fmt.Sprintf("--prefix=%s", b.prefixDir), "--static"}, nil); err != nil {
			return err
		}
		if err := b.run(ctx, dir, "make", []string{fmt.Sprintf("-j%d", b.cpuCount)}, nil); err != nil {
			return err
		}
		return b.run(ctx, dir, "make", []string{"install"}, nil)
	})
}

func (b *builder) buildReadline(ctx context.Context) error {
	comp := b.cfg.Dependencies.Readline
	if comp.URL == "" {
		logf("Skip readline: missing URL")
		return nil
	}
	dir, err := b.prepareComponent(componentReadline, comp)
	if err != nil {
		return err
	}
	return b.buildWithStamp(componentReadline, func() error {
		if err := b.run(ctx, dir, "./configure", []string{
			fmt.Sprintf("--prefix=%s", b.prefixDir),
			"--disable-shared",
			"--enable-static",
			"--with-curses",
		}, nil); err != nil {
			return err
		}
		env := map[string]string{"SHLIB_LIBS": ""}
		if err := b.run(ctx, dir, "make", []string{fmt.Sprintf("-j%d", b.cpuCount)}, env); err != nil {
			return err
		}
		return b.run(ctx, dir, "make", []string{"install"}, env)
	})
}

func (b *builder) buildOpenssl(ctx context.Context) error {
	comp := b.cfg.Dependencies.Openssl
	if comp.URL == "" {
		logf("Skip openssl: missing URL")
		return nil
	}
	dir, err := b.prepareComponent(componentOpenssl, comp)
	if err != nil {
		return err
	}
	return b.buildWithStamp(componentOpenssl, func() error {
		args := []string{
			b.opensslTarget,
			"no-shared",
			"no-tests",
			"enable-static-engine",
			fmt.Sprintf("--prefix=%s", b.prefixDir),
			fmt.Sprintf("--openssldir=%s", filepath.Join(b.prefixDir, "ssl")),
		}
		if b.platformCFlags != "" {
			args = append(args, b.platformCFlags)
		}
		env := map[string]string{
			"CFLAGS":   b.baseEnv["CFLAGS"] + " -DOPENSSL_NO_ATEXIT",
			"CPPFLAGS": b.baseEnv["CPPFLAGS"] + " -DOPENSSL_NO_ATEXIT",
		}
		if err := b.run(ctx, dir, "./Configure", args, env); err != nil {
			return err
		}
		if err := b.run(ctx, dir, "make", []string{fmt.Sprintf("-j%d", b.cpuCount)}, env); err != nil {
			return err
		}
		return b.run(ctx, dir, "make", []string{"install_sw"}, env)
	})
}

func (b *builder) buildICU(ctx context.Context) error {
	comp := b.cfg.Dependencies.ICU
	if comp.URL == "" {
		logf("Skip icu: missing URL")
		return nil
	}
	dir, err := b.prepareComponent(componentICU, comp)
	if err != nil {
		return err
	}
	return b.buildWithStamp(componentICU, func() error {
		env := map[string]string{}
		if b.osName == "linux" {
			env["CXX"] = "g++"
		}
		if err := b.run(ctx, dir, "./configure", []string{
			fmt.Sprintf("--prefix=%s", b.prefixDir),
			"--enable-static",
			"--disable-shared",
			"--disable-samples",
			"--disable-tests",
		}, env); err != nil {
			return err
		}
		if err := b.run(ctx, dir, "make", []string{fmt.Sprintf("-j%d", b.cpuCount)}, env); err != nil {
			return err
		}
		return b.run(ctx, dir, "make", []string{"install"}, env)
	})
}

func (b *builder) buildPostgres(ctx context.Context) error {
	comp := b.cfg.Postgres
	dir, err := b.prepareComponent(componentPostgres, comp)
	if err != nil {
		return err
	}
	if err := relaxLibpqCheck(dir); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(b.pgPrefixDir, "bin", "postgres")); err == nil {
		logf("PostgreSQL already built")
		return nil
	}

	logf("Configure PostgreSQL %s", comp.Version)
	extraEnv := map[string]string{}
	if b.osName == "linux" {
		extraEnv["LIBS"] = "-Wl,--no-as-needed -lstdc++ -Wl,--as-needed"
	}
	if err := b.run(ctx, dir, "./configure", []string{
		fmt.Sprintf("--prefix=%s", b.pgPrefixDir),
		fmt.Sprintf("--with-includes=%s", filepath.Join(b.prefixDir, "include")),
		fmt.Sprintf("--with-libraries=%s", filepath.Join(b.prefixDir, "lib")),
		"--with-openssl",
		"--with-readline",
		"--with-zlib",
		"--with-icu",
		"--disable-rpath",
	}, extraEnv); err != nil {
		return err
	}
	if b.osName == "linux" {
		if err := ensureLinuxStdlibOrder(dir); err != nil {
			return err
		}
	}
	logf("Build + install PostgreSQL")
	if err := b.run(ctx, dir, "make", []string{fmt.Sprintf("-j%d", b.cpuCount)}, extraEnv); err != nil {
		return err
	}
	return b.run(ctx, dir, "make", []string{"install"}, extraEnv)
}

func (b *builder) packageBundle() error {
	target := filepath.Join(b.bundleDir, "pgsql", "bin")
	if _, err := os.Stat(target); err == nil {
		logf("Bundle exists (%s)", b.bundleDir)
		return nil
	}

	logf("Assembling bundle -> %s", b.bundleDir)
	if err := os.MkdirAll(b.bundleDir, 0o755); err != nil {
		return err
	}
	dest := filepath.Join(b.bundleDir, "pgsql")
	if err := copyDir(b.pgPrefixDir, dest); err != nil {
		return err
	}
	if err := writeScript(filepath.Join(b.bundleDir, "init-database.sh"), initDatabaseScript); err != nil {
		return err
	}
	if err := writeScript(filepath.Join(b.bundleDir, "start-server.sh"), startServerScript); err != nil {
		return err
	}
	if err := writeScript(filepath.Join(b.bundleDir, "stop-server.sh"), stopServerScript); err != nil {
		return err
	}
	if err := writeScript(filepath.Join(b.bundleDir, "connect.sh"), connectScript); err != nil {
		return err
	}
	logf("Bundle ready: %s", b.bundleDir)
	return nil
}

func (b *builder) prepareComponent(name string, comp component) (string, error) {
	if comp.URL == "" {
		return "", fmt.Errorf("%s url missing", name)
	}
	if comp.SourceDir == "" {
		return "", fmt.Errorf("%s sourceDir missing", name)
	}
	archivePath := filepath.Join(b.srcDir, filepath.Base(comp.URL))
	if err := b.ensureArchive(comp, archivePath); err != nil {
		return "", err
	}
	sourceDir := filepath.Join(b.srcDir, comp.SourceDir)
	if _, err := os.Stat(sourceDir); err == nil {
		logf("Reuse %s", sourceDir)
		return sourceDir, nil
	}
	if err := b.extractArchive(archivePath); err != nil {
		return "", err
	}
	if _, err := os.Stat(sourceDir); err != nil {
		return "", fmt.Errorf("expected source directory %s after extracting %s", sourceDir, filepath.Base(comp.URL))
	}
	return sourceDir, nil
}

func (b *builder) ensureArchive(comp component, archivePath string) error {
	needDownload := true
	if info, err := os.Stat(archivePath); err == nil && info.Size() > 0 {
		needDownload = false
		if comp.SHA256 != "" {
			ok, err := verifySHA256(archivePath, comp.SHA256)
			if err != nil {
				return err
			}
			if !ok {
				logf("Checksum mismatch for %s; re-download", filepath.Base(archivePath))
				if err := os.Remove(archivePath); err != nil {
					return err
				}
				needDownload = true
			}
		} else {
			logf("Reuse %s (no checksum)", filepath.Base(archivePath))
		}
	}
	if needDownload {
		if err := downloadFile(comp.URL, archivePath); err != nil {
			return err
		}
	}
	if comp.SHA256 != "" {
		ok, err := verifySHA256(archivePath, comp.SHA256)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("checksum validation failed for %s", filepath.Base(archivePath))
		}
	} else {
		logf("WARNING: no checksum for %s; skipped integrity check", filepath.Base(archivePath))
	}
	return nil
}

func downloadFile(url, dest string) error {
	logf("Download %s", filepath.Base(dest))
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: unexpected status %s", url, resp.Status)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dest), "download-*")
	if err != nil {
		return err
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}()
	if _, err := io.Copy(tmp, resp.Body); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dest)
}

func verifySHA256(path, expected string) (bool, error) {
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()

	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return false, err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	return strings.EqualFold(sum, expected), nil
}

func (b *builder) extractArchive(archivePath string) error {
	logf("Extract %s", filepath.Base(archivePath))
	args, err := tarArgs(archivePath, b.srcDir)
	if err != nil {
		return err
	}
	return b.run(context.Background(), "", "tar", args, nil)
}

func tarArgs(archivePath, destDir string) ([]string, error) {
	switch {
	case strings.HasSuffix(archivePath, ".tar.gz"), strings.HasSuffix(archivePath, ".tgz"):
		return []string{"-xzf", archivePath, "-C", destDir}, nil
	case strings.HasSuffix(archivePath, ".tar.xz"):
		return []string{"-xJf", archivePath, "-C", destDir}, nil
	default:
		return nil, fmt.Errorf("unsupported archive format: %s", archivePath)
	}
}

func (b *builder) run(ctx context.Context, dir, executable string, args []string, extraEnv map[string]string) error {
	display := executable
	if len(args) > 0 {
		display = display + " " + strings.Join(args, " ")
	}
	if dir != "" {
		logf("Run (%s): %s", dir, display)
	} else {
		logf("Run: %s", display)
	}

	cmd := exec.CommandContext(ctx, executable, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = mergeEnv(os.Environ(), b.baseEnv, extraEnv)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (b *builder) buildWithStamp(name string, fn func() error) error {
	stamp := filepath.Join(b.prefixDir, fmt.Sprintf(".%s-built", name))
	if _, err := os.Stat(stamp); err == nil {
		logf("%s already built", name)
		return nil
	}
	if err := fn(); err != nil {
		return err
	}
	return os.WriteFile(stamp, []byte(time.Now().Format(time.RFC3339)), 0o644)
}

func mergeEnv(base []string, overrides map[string]string, extra map[string]string) []string {
	result := make(map[string]string, len(base))
	for _, entry := range base {
		if idx := strings.Index(entry, "="); idx >= 0 {
			result[entry[:idx]] = entry[idx+1:]
		}
	}
	for k, v := range overrides {
		result[k] = v
	}
	for k, v := range extra {
		result[k] = v
	}
	out := make([]string, 0, len(result))
	for k, v := range result {
		out = append(out, fmt.Sprintf("%s=%s", k, v))
	}
	return out
}

func copyDir(src, dest string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dest, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		if err := copyFile(path, target, info.Mode()); err != nil {
			return err
		}
		return nil
	})
}

func copyFile(src, dest string, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	success := false
	defer func() {
		out.Close()
		if !success {
			os.Remove(dest)
		}
	}()
	if _, err = io.Copy(out, in); err != nil {
		return err
	}
	if err = out.Close(); err != nil {
		return err
	}
	if err = os.Chmod(dest, mode); err != nil {
		return err
	}
	success = true
	return nil
}

func relaxLibpqCheck(pgSourceDir string) error {
	makefile := filepath.Join(pgSourceDir, "src", "interfaces", "libpq", "Makefile")
	data, err := os.ReadFile(makefile)
	if err != nil {
		return err
	}
	text := string(data)
	const needle = "grep -v __cxa_atexit"
	const replacement = "grep -v __cxa_atexit | grep -v _atexit | grep -v pthread_exit"
	if strings.Contains(text, "| grep -v _atexit") {
		return nil
	}
	if !strings.Contains(text, needle) {
		return nil
	}
	updated := strings.Replace(text, needle, replacement, 1)
	if updated == text {
		return nil
	}
	return os.WriteFile(makefile, []byte(updated), 0o644)
}

func ensureLinuxStdlibOrder(pgSourceDir string) error {
	makefile := filepath.Join(pgSourceDir, "src", "Makefile.global")
	data, err := os.ReadFile(makefile)
	if err != nil {
		return err
	}
	text := string(data)
	const suffix = " -Wl,--no-as-needed -lstdc++ -Wl,--as-needed"
	if strings.Contains(text, suffix) {
		return nil
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "ICU_LIBS") {
			if strings.Contains(line, suffix) {
				return nil
			}
			lines[i] = line + suffix
			updated := strings.Join(lines, "\n")
			return os.WriteFile(makefile, []byte(updated), 0o644)
		}
	}
	return fmt.Errorf("ICU_LIBS entry not found in %s", makefile)
}

func writeScript(path, content string) error {
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		return err
	}
	return nil
}

func fail(err error) {
	logf("ERROR: %v", err)
	os.Exit(1)
}

func logf(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	fmt.Printf("[%s] %s\n", time.Now().Format("15:04:05"), msg)
}

const initDatabaseScript = `#!/usr/bin/env bash
set -e
DIR="$(cd "$(dirname "$0")" && pwd)"
if [[ -d "$DIR/data" ]]; then echo "data directory exists -- remove first."; exit 1; fi
"$DIR/pgsql/bin/initdb" -D "$DIR/data" --locale=C --encoding=UTF8 "$@"
echo "Cluster initialised => $DIR/data"
`

const startServerScript = `#!/usr/bin/env bash
set -e
DIR="$(cd "$(dirname "$0")" && pwd)"
"$DIR/pgsql/bin/pg_ctl" -D "$DIR/data" -l "$DIR/data/log" start "$@"
`

const stopServerScript = `#!/usr/bin/env bash
set -e
DIR="$(cd "$(dirname "$0")" && pwd)"
"$DIR/pgsql/bin/pg_ctl" -D "$DIR/data" stop -m fast "$@"
`

const connectScript = `#!/usr/bin/env bash
DIR="$(cd "$(dirname "$0")" && pwd)"
exec "$DIR/pgsql/bin/psql" postgres "$@"
`
