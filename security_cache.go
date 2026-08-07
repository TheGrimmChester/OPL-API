package main

import (
	"context"
	"log"
	"os"
	"strings"
	"time"

	opencache "github.com/TheGrimmChester/open-cache-go"
)

const runNotifyDedupTTL = 24 * time.Hour

var secCache *opencache.Layered

func initSecurityCache() {
	l1 := clampInt(atoiDefault(envOr("OPL_SEC_L1_CACHE", "20000"), 20000), 1000, 100000)
	prefix := stringsTrim(envOr("OPL_SEC_KEY_PREFIX", "opl:sec:"))
	lc, err := opencache.NewLayered(opencache.Config{RedisURL: os.Getenv("REDIS_URL"), L1Max: l1, KeyPrefix: prefix})
	if err != nil {
		log.Printf("[WARN] security cache: %v; memory-only", err)
		lc, _ = opencache.NewLayered(opencache.Config{L1Max: l1, KeyPrefix: prefix})
	}
	secCache = lc
}

func runNotifyDedupSeen(ctx context.Context, runID, status string) bool {
	if secCache == nil || runID == "" {
		return false
	}
	key := "run-notify:" + opencache.HashKey(runID, status)
	if !secCache.SetNX(ctx, key, []byte("1"), runNotifyDedupTTL) {
		return true
	}
	return false
}

func stringsTrim(s string) string {
	return strings.TrimSpace(s)
}
