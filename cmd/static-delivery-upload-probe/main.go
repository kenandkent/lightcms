// Diagnostic upload only: never activates a version or changes target/job state.
package main

import (
	"bufio"
	"context"
	"encoding/gob"
	"encoding/json"
	"fmt"
	sd "github.com/metavert-io/lightcms/v7/internal/product/staticdelivery"
	cf "github.com/metavert-io/lightcms/v7/internal/product/staticdelivery/cloudflare"
	"os"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("credential input unavailable")
	}
	var cfg struct{ Token, Account, Worker string }
	if json.Unmarshal(line, &cfg) != nil || cfg.Worker != "lightcms-reports-test" || cfg.Token == "" {
		return fmt.Errorf("probe restricted to dedicated test Worker")
	}
	var release sd.ReleaseManifest
	if err = gob.NewDecoder(reader).Decode(&release); err != nil {
		return fmt.Errorf("release input invalid: %w", err)
	}
	files, err := sd.ReleaseUploadFiles(release)
	if err != nil {
		return err
	}
	modules, err := sd.ReleaseModules(release)
	if err != nil {
		return err
	}
	c := cf.NewClient(cfg.Token, cf.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	before, err := c.Deployments(ctx, cfg.Account, cfg.Worker)
	if err != nil {
		return err
	}
	rawBefore, _ := json.Marshal(before)
	fmt.Printf("probe: %d assets, %d modules, mode=%s; no activation\n", len(files), len(modules), release.DeliveryMode)
	assets := make([]cf.File, len(files))
	for i, f := range files {
		assets[i] = cf.File{Path: f.PublicPath, MIME: f.MIME, Data: f.Data}
	}
	token, err := c.UploadAssets(ctx, cfg.Account, cfg.Worker, assets)
	if err != nil {
		return fmt.Errorf("asset upload: %w", err)
	}
	fmt.Println("asset upload succeeded")
	ms := make([]cf.Module, len(modules))
	for i, m := range modules {
		ms[i] = cf.Module{Name: m.Name, MIME: m.MIME, Data: m.Data}
	}
	marker := sd.OwnershipMarker{Protocol: 1, InstallationID: release.InstallationID, TargetID: release.TargetID.Hex(), ReleaseID: release.ID.Hex()}.String()
	version, uploadErr := c.UploadVersion(ctx, cfg.Account, cfg.Worker, token, ms, marker)
	after, checkErr := c.Deployments(ctx, cfg.Account, cfg.Worker)
	if checkErr != nil {
		return fmt.Errorf("post-check: %w", checkErr)
	}
	rawAfter, _ := json.Marshal(after)
	if string(rawBefore) != string(rawAfter) {
		return fmt.Errorf("active deployments changed during probe; stop for investigation")
	}
	fmt.Println("active deployments unchanged")
	if uploadErr != nil {
		return fmt.Errorf("version upload: %w", uploadErr)
	}
	fmt.Printf("version upload succeeded: %s (not activated)\n", version.ID)
	return nil
}
