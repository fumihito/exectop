package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAPTNoUpgradableIgnoresPhasedPackages(t *testing.T) {
	runAPTCheckFixture(t, "Calculating upgrade...\nThe following upgrades have been deferred due to phasing:\n  libaudit1\n0 upgraded, 0 newly installed, 0 to remove and 1 not upgraded.\n", 0, "no upgradable packages", false)
}

func TestAPTNoUpgradableReportsAppliedPackages(t *testing.T) {
	runAPTCheckFixture(t, "Calculating upgrade...\nInst curl [1.0] (2.0 repo)\n0 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.\n", 1, "upgradable packages: 1", false)
}

func TestAPTNoUpgradableReportsSimulationFailure(t *testing.T) {
	runAPTCheckFixture(t, "E: Could not get lock /var/lib/dpkg/lock\n", 1, "apt upgrade simulation failed", true)
}

func runAPTCheckFixture(t *testing.T, output string, wantExit int, wantOutput string, aptFailure bool) {
	t.Helper()
	binDir := t.TempDir()
	fakeAPT := filepath.Join(binDir, "apt-get")
	script := "#!/bin/sh\nprintf '%s' '" + strings.ReplaceAll(output, "'", "'\\''") + "'\n"
	if aptFailure {
		script += "exit 1\n"
	}
	if err := os.WriteFile(fakeAPT, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	check := filepath.Join("..", "..", "..", "exectop.d", "examples", "host", "software", "02-apt-no-upgradable.sh")
	cmd := exec.Command("sh", check)
	cmd.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if exitCode(err) != wantExit {
		t.Fatalf("exit=%d, want %d, output=%s, err=%v", exitCode(err), wantExit, out, err)
	}
	if !strings.Contains(string(out), wantOutput) {
		t.Fatalf("output=%q, want %q", out, wantOutput)
	}
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode()
	}
	return -1
}
