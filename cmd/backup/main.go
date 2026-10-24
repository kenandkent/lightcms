// Offline backup and isolated restore drill. Credentials are read from the
// existing config/environment, never command-line arguments or printed output.
package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/metavert-io/lightcms/v7/config"
	"github.com/metavert-io/lightcms/v7/internal/database"
	"github.com/metavert-io/lightcms/v7/internal/product/backup"
	"os"
	"strings"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "Backup operation failed:", err)
		os.Exit(1)
	}
}
func run() error {
	mode := flag.String("mode", "verify", "create, verify, or restore-test")
	project := flag.String("project", ".", "project root")
	location := flag.String("backup", "", "project-relative backup directory")
	roots := flag.String("roots", "", "comma-separated project-relative data/config roots to include")
	databaseName := flag.String("database", "lightcms", "source database")
	restoreName := flag.String("restore-database", "", "new isolated *-restore-test database")
	restoreFiles := flag.String("restore-files", "", "new project-relative restored files directory")
	flag.Parse()
	if *location == "" {
		return fmt.Errorf("-backup is required")
	}
	if *mode == "verify" {
		manifest, err := backup.Verify(*project, *location)
		if err != nil {
			return err
		}
		fmt.Printf("Verified %d files and %d database collections.\n", len(manifest.Files), len(manifest.Collections))
		return nil
	}
	if *mode != "create" && *mode != "restore-test" {
		return fmt.Errorf("unsupported mode")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Minute)
	defer cancel()
	db, err := database.ConnectExisting(ctx, cfg.MongoURI, *databaseName)
	if err != nil {
		return fmt.Errorf("cannot connect to configured MongoDB")
	}
	defer db.Disconnect(context.Background())
	var manifest backup.Manifest
	if *mode == "create" {
		if *roots == "" {
			return fmt.Errorf("-roots must explicitly include content, private artifacts, uploads and required configuration")
		}
		manifest, err = backup.Create(ctx, db, *project, *location, strings.Split(*roots, ","))
	} else {
		manifest, err = backup.RestoreForVerification(ctx, db.Collection("content").Database().Client(), *project, *location, *restoreName, *restoreFiles)
	}
	if err != nil {
		return err
	}
	fmt.Printf("%s completed and verified: %d files, %d collections. Do not start publication workers against a restored test database.\n", *mode, len(manifest.Files), len(manifest.Collections))
	return nil
}
