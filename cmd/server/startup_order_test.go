package main

import (
	"os"
	"strings"
	"testing"
)

func TestStartupWriterGatePrecedesBackgroundServices(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(raw)
	gate := strings.Index(source, "enforceSingleInstance(gateCtx")
	for _, entry := range []string{"authManager.MigrateToMultiUser", "services.NewAnalyticsService", "go schedulerService.Start", "h.EnableDependencyIndex"} {
		at := strings.Index(source, entry)
		if gate < 0 || at < 0 || gate > at {
			t.Fatalf("writer lease must precede side effect %s", entry)
		}
	}
}
