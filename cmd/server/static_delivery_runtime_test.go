package main

import (
	"context"
	sd "github.com/metavert-io/lightcms/v7/internal/product/staticdelivery"
	"testing"
)

func TestStaticDeliveryMissingDBIsIsolated(t *testing.T) {
	rt := buildStaticDeliveryAdminRuntime(context.Background(), nil, sd.Config{})
	if rt == nil || rt.Unavailable == "" {
		t.Fatal("module failure must be isolated in new runtime")
	}
}
