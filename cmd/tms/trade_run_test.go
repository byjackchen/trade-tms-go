package main

import (
	"testing"

	"github.com/byjackchen/trade-tms-go/internal/domain"
)

// TestResolveRun locks the 2D run selector — and in particular that a SIGNAL run
// may carry --env paper|real to designate a READ-ONLY broker-sync account
// (DIRECTION 2) while the run word stays "signal" (no auto orders), and that
// signal with no env / --env simu binds no broker account (env stays empty).
func TestResolveRun(t *testing.T) {
	cases := []struct {
		name, exec, env string
		wantExec        domain.ExecutionPolicy
		wantEnv         domain.BrokerEnv
		wantWord        string
		wantErr         bool
	}{
		{"signal no env", "signal", "", domain.ExecSignal, "", "signal", false},
		{"signal simu = no sync acct", "signal", "simu", domain.ExecSignal, "", "signal", false},
		{"signal paper = paper sync", "signal", "paper", domain.ExecSignal, domain.EnvPaper, "signal", false},
		{"signal real = live sync", "signal", "real", domain.ExecSignal, domain.EnvReal, "signal", false},
		{"signal bogus env", "signal", "bogus", "", "", "", true},
		{"auto needs env", "auto", "", "", "", "", true},
		{"auto paper = paper", "auto", "paper", domain.ExecAuto, domain.EnvPaper, "paper", false},
		{"auto real = live", "auto", "real", domain.ExecAuto, domain.EnvReal, "live", false},
		{"bogus exec", "bogus", "", "", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			exec, env, word, err := resolveRun(c.exec, c.env)
			if c.wantErr {
				if err == nil {
					t.Fatalf("resolveRun(%q,%q): want error, got (%v,%v,%q)", c.exec, c.env, exec, env, word)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveRun(%q,%q): unexpected error %v", c.exec, c.env, err)
			}
			if exec != c.wantExec || env != c.wantEnv || word != c.wantWord {
				t.Fatalf("resolveRun(%q,%q) = (%v,%v,%q); want (%v,%v,%q)",
					c.exec, c.env, exec, env, word, c.wantExec, c.wantEnv, c.wantWord)
			}
		})
	}
}
