package main

import (
	"context"
	"flag"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/jdeng/embpg/pkg/embpg"
)

func main() {
	bundle := flag.String("bundle", defaultBundle(), "path to the postgres bundle")
	dataDir := flag.String("data", filepath.Join(os.TempDir(), "embedded-pg-data"), "data directory")
	port := flag.Int("port", defaultPort(), "port to listen on")
	user := flag.String("user", defaultUser(), "postgres superuser name")
	pass := flag.String("password", defaultPassword(), "postgres superuser password")
	database := flag.String("database", "", "database to create (defaults to user)")
	flag.Parse()

	mgr, err := embpg.New(embpg.Config{
		BundlePath:    *bundle,
		DataDir:       *dataDir,
		Port:          *port,
		ListenAddress: "127.0.0.1",
		Username:      *user,
		Password:      *pass,
		Database:      *database,
		StartParameters: map[string]string{
			"shared_buffers": "16MB",
		},
	})
	if err != nil {
		log.Fatalf("create manager: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := mgr.Start(ctx); err != nil {
		log.Fatalf("start postgres: %v", err)
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer stopCancel()
		if err := mgr.Stop(stopCtx); err != nil {
			log.Printf("stop postgres: %v", err)
		}
	}()

	log.Printf("PostgreSQL running from bundle %s on port %d using data dir %s", mgr.BundlePath(), mgr.Port(), mgr.DataDir())
	log.Printf("Connect with: psql -h 127.0.0.1 -p %d postgres", mgr.Port())

	time.Sleep(500 * time.Second)
}

func defaultBundle() string {
	if val := os.Getenv("EMBPG_BUNDLE"); val != "" {
		return val
	}
	return "dist/postgresql-darwin-arm64-16.2"
}

func defaultPort() int {
	if val := os.Getenv("EMBPG_PORT"); val != "" {
		if p, err := strconv.Atoi(val); err == nil {
			return p
		}
	}
	return 55432
}

func defaultUser() string {
	if val := os.Getenv("EMBPG_USER"); val != "" {
		return val
	}
	return "postgres"
}

func defaultPassword() string {
	if val := os.Getenv("EMBPG_PASSWORD"); val != "" {
		return val
	}
	return "postgres"
}
