package preflight

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRenderTable_Pass(t *testing.T) {
	var sb strings.Builder
	RenderTable(&sb, Run(context.Background(), paperCfg(), healthyProbes()))
	out := sb.String()
	if !strings.Contains(out, "RESULT: PASS") {
		t.Fatalf("pass report must render a PASS verdict:\n%s", out)
	}
	for _, id := range []string{CheckPostgres, CheckDataCurrent, CheckWarmupAvailable, CheckMarketDataFund} {
		if !strings.Contains(out, id) {
			t.Errorf("rendered table missing check %s:\n%s", id, out)
		}
	}
}

func TestRenderTable_Fail(t *testing.T) {
	f := healthyProbes()
	f.opendErr = errors.New("refused")
	var sb strings.Builder
	RenderTable(&sb, Run(context.Background(), paperCfg(), f))
	out := sb.String()
	if !strings.Contains(out, "RESULT: FAIL") {
		t.Fatalf("failing report must render a FAIL verdict:\n%s", out)
	}
	if !strings.Contains(out, CheckOpenD) {
		t.Fatalf("FAIL verdict must list the failing blocker:\n%s", out)
	}
}
