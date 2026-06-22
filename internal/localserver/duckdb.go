// Package localserver manages a local DuckDB CLI process with the Quack
// extension, allowing the driver to auto-start a server when the DSN uses
// the "local" scheme.
package localserver

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// MinVersion is the minimum DuckDB version required for Quack support.
var MinVersion = [3]int{1, 5, 3}

// FindBinary returns the path to the duckdb binary.
// It checks DUCKDB_BIN first, then falls back to exec.LookPath.
func FindBinary() (string, error) {
	if bin := os.Getenv("DUCKDB_BIN"); bin != "" {
		if _, err := exec.LookPath(bin); err != nil {
			return "", fmt.Errorf("localserver: DUCKDB_BIN=%q not found: %w", bin, err)
		}
		return bin, nil
	}
	path, err := exec.LookPath("duckdb")
	if err != nil {
		return "", fmt.Errorf("localserver: duckdb not found on PATH (install DuckDB v%d.%d.%d+ or set DUCKDB_BIN)",
			MinVersion[0], MinVersion[1], MinVersion[2])
	}
	return path, nil
}

// CheckVersion runs "duckdb --version" and returns the parsed semver
// components. It returns an error if the version is below MinVersion.
func CheckVersion(binary string) (major, minor, patch int, err error) {
	out, err := exec.Command(binary, "--version").Output()
	if err != nil {
		return 0, 0, 0, fmt.Errorf("localserver: failed to get duckdb version: %w", err)
	}
	major, minor, patch, err = parseVersion(string(out))
	if err != nil {
		return 0, 0, 0, err
	}
	if !versionAtLeast(major, minor, patch, MinVersion) {
		return major, minor, patch, fmt.Errorf(
			"localserver: duckdb v%d.%d.%d is below minimum v%d.%d.%d",
			major, minor, patch, MinVersion[0], MinVersion[1], MinVersion[2])
	}
	return major, minor, patch, nil
}

// parseVersion extracts major.minor.patch from "v1.5.3 ..." style output.
func parseVersion(output string) (major, minor, patch int, err error) {
	s := strings.TrimSpace(output)
	s = strings.TrimPrefix(s, "v")
	// Take only the first whitespace-delimited token.
	if idx := strings.IndexAny(s, " \t\n"); idx >= 0 {
		s = s[:idx]
	}
	parts := strings.SplitN(s, ".", 3)
	if len(parts) < 3 {
		return 0, 0, 0, fmt.Errorf("localserver: cannot parse version %q", output)
	}
	major, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, 0, fmt.Errorf("localserver: cannot parse version %q", output)
	}
	minor, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, 0, fmt.Errorf("localserver: cannot parse version %q", output)
	}
	patch, err = strconv.Atoi(parts[2])
	if err != nil {
		return 0, 0, 0, fmt.Errorf("localserver: cannot parse version %q", output)
	}
	return major, minor, patch, nil
}

func versionAtLeast(major, minor, patch int, min [3]int) bool {
	if major != min[0] {
		return major > min[0]
	}
	if minor != min[1] {
		return minor > min[1]
	}
	return patch >= min[2]
}
