// Read-only legacy content/history inventory. Output may be redirected to a
// user-chosen project file; this command never writes or repairs records.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/metavert-io/lightcms/v7/config"
	"github.com/metavert-io/lightcms/v7/internal/database"
	"github.com/metavert-io/lightcms/v7/internal/product/contentaudit"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"os"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "Content history audit failed:", err)
		os.Exit(1)
	}
}
func run() error {
	name := flag.String("database", "lightcms", "source database")
	after := flag.String("after", "", "last content ID from previous page")
	flag.Parse()
	id := primitive.NilObjectID
	var err error
	if *after != "" {
		id, err = primitive.ObjectIDFromHex(*after)
		if err != nil {
			return fmt.Errorf("invalid cursor")
		}
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	db, err := database.ConnectExisting(ctx, cfg.MongoURI, *name)
	if err != nil {
		return fmt.Errorf("configured database unavailable")
	}
	defer db.Disconnect(context.Background())
	page, err := contentaudit.Scan(ctx, db, id)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(page)
}
