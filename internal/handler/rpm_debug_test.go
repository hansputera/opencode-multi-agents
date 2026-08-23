package handler

import (
	"testing"
	"time"

	"github.com/hansputera/opencode-multi-agents/internal/config"
)

func TestDebugRPM(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.PowStorePath = t.TempDir() + "/pow.db"
	svc, err := newPoWService(cfg, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	kh := "debughash"
	for i := 0; i < 6; i++ {
		retry, code := svc.checkKeyLimits(kh, 2)
		t.Logf("call %d: code=%q retry=%v", i, code, retry)
		time.Sleep(50 * time.Millisecond)
	}
}
