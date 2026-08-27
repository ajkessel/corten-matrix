// corten-matrix - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The MAS block sets encryption.msc4190 in config.yaml and io.element.msc4190 in
// the appservice registration. It has to be inline in each installer because
// runSetupScript writes the chosen script to a single temp file, so a sibling
// .py would not exist at run time — which means the same Python lives in two
// scripts and can drift. These tests pin the copies together and exercise the
// payload directly, since it edits the same file setMSC4190OnDisk does (see
// cmd/corten-matrix/ensure_config.go) and the two must agree about it.

const (
	payloadStart = "python3 - \"$CONFIG\" \"$REGISTRATION\" <<'PYMAS'\n"
	payloadEnd   = "\nPYMAS\n"
)

func masPayload(t *testing.T, script string) string {
	t.Helper()
	data, err := Files.ReadFile(script)
	if err != nil {
		t.Fatalf("read %s: %v", script, err)
	}
	s := string(data)
	i := strings.Index(s, payloadStart)
	if i < 0 {
		t.Fatalf("%s: MAS payload start marker not found", script)
	}
	rest := s[i+len(payloadStart):]
	j := strings.Index(rest, payloadEnd)
	if j < 0 {
		t.Fatalf("%s: MAS payload end marker not found", script)
	}
	return rest[:j]
}

// Both installers must carry byte-identical Python. Two rounds of review found
// indentation bugs in this payload; a divergent copy would mean fixing one and
// shipping the other.
func TestMASPayloadIdenticalAcrossInstallers(t *testing.T) {
	mac := masPayload(t, "install.sh")
	linux := masPayload(t, "install-linux.sh")
	if mac != linux {
		t.Errorf("install.sh and install-linux.sh MAS payloads differ\n--- install.sh ---\n%s\n--- install-linux.sh ---\n%s", mac, linux)
	}
	if !strings.Contains(mac, "def set_block_flag") || !strings.Contains(mac, "def set_top_level_flag") {
		t.Errorf("extracted payload does not look like the MAS block:\n%s", mac)
	}
}

func runPayload(t *testing.T, payload, config, registration string) (stdout string, exitCode int) {
	t.Helper()
	cmd := exec.Command("python3", "-c", payload, config, registration)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return string(out), ee.ExitCode()
		}
		t.Fatalf("run payload: %v", err)
	}
	return string(out), 0
}

func requirePython(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
}

const validRegistration = "id: corten\nrate_limited: false\n"

func TestMASPayloadFixtures(t *testing.T) {
	requirePython(t)
	payload := masPayload(t, "install.sh")

	tests := []struct {
		name         string
		config       string
		registration string // "" means do not create the file
		wantStdout   string
		wantExit     int
		wantChanged  bool
	}{{
		name:         "key present, comment and trailing comment preserved",
		config:       "homeserver:\n    address: https://x\n\nencryption:\n    allow: true\n    # doc\n    msc4190: false  # keep\n",
		registration: validRegistration,
		wantStdout:   "set set", wantChanged: true,
	}, {
		name:         "key absent, four-space block",
		config:       "encryption:\n    allow: true\n    default: true\n",
		registration: validRegistration,
		wantStdout:   "set set", wantChanged: true,
	}, {
		name:         "key absent, two-space block: indent must be derived",
		config:       "homeserver:\n  address: https://x\nencryption:\n  allow: true\nlogging:\n  min_level: debug\n",
		registration: validRegistration,
		wantStdout:   "set set", wantChanged: true,
	}, {
		// Regression: the indent used to come from the first non-blank line
		// after the header, which can be a comment indented differently from
		// the keys. That produced a config that no longer parsed.
		name:         "comment between header and first key must not define the indent",
		config:       "encryption:\n  # whether to enable encryption\n    allow: true\n",
		registration: validRegistration,
		wantStdout:   "set set", wantChanged: true,
	}, {
		// A column-0 comment must not be mistaken for the end of the block, or
		// an existing key below it is missed and a duplicate gets spliced in.
		name:         "column-zero comment does not end the block",
		config:       "encryption:\n    allow: true\n# a comment at column zero\n    msc4190: false\n",
		registration: validRegistration,
		wantStdout:   "set set", wantChanged: true,
	}, {
		name:         "header with trailing comment is still an anchor",
		config:       "encryption:  # e2be\n    allow: true\n",
		registration: validRegistration,
		wantStdout:   "set set", wantChanged: true,
	}, {
		name:         "already enabled in both files",
		config:       "encryption:\n    msc4190: true\n",
		registration: "id: corten\nio.element.msc4190: true\n",
		wantStdout:   "already already",
	}, {
		name:       "missing registration is reported, not glossed over",
		config:     "encryption:\n    allow: true\n",
		wantStdout: "set registration missing", wantChanged: true,
	}, {
		name:         "flow-style block is refused",
		config:       "homeserver:\n    address: https://x\nencryption: {allow: true}\n",
		registration: validRegistration,
		wantStdout:   "config failed", wantExit: 1,
	}, {
		name:         "no encryption block is refused",
		config:       "homeserver:\n    address: https://x\n",
		registration: validRegistration,
		wantStdout:   "config failed", wantExit: 1,
	}, {
		name:         "empty encryption block is refused",
		config:       "encryption:\nlogging:\n    min_level: debug\n",
		registration: validRegistration,
		wantStdout:   "config failed", wantExit: 1,
	}, {
		// Tabs are already invalid YAML, and there is no parser on the shell
		// side to check with, so there is no safe edit to make.
		name:         "tab indentation is refused",
		config:       "encryption:\n\tallow: true\n",
		registration: validRegistration,
		wantStdout:   "config failed", wantExit: 1,
	}}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(cfg, []byte(tc.config), 0o600); err != nil {
				t.Fatal(err)
			}
			reg := filepath.Join(dir, "registration.yaml")
			if tc.registration != "" {
				if err := os.WriteFile(reg, []byte(tc.registration), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			stdout, code := runPayload(t, payload, cfg, reg)
			if got := strings.TrimSpace(stdout); got != tc.wantStdout {
				t.Errorf("stdout = %q, want %q", got, tc.wantStdout)
			}
			if code != tc.wantExit {
				t.Errorf("exit = %d, want %d", code, tc.wantExit)
			}

			after, err := os.ReadFile(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if changed := string(after) != tc.config; changed != tc.wantChanged {
				t.Errorf("config changed = %v, want %v\n%s", changed, tc.wantChanged, after)
			}

			// Anything we wrote must still parse. A config we declined to touch
			// is exempt: it may have been invalid on the way in (the tab case).
			if tc.wantChanged {
				var probe yaml.Node
				if err := yaml.Unmarshal(after, &probe); err != nil {
					t.Errorf("payload produced unparseable YAML: %v\n%s", err, after)
				}
				if !strings.Contains(string(after), "msc4190: true") {
					t.Errorf("msc4190 was not enabled:\n%s", after)
				}
			}

			// Idempotent: a second run must not touch either file again.
			before := string(after)
			var regBefore []byte
			if tc.registration != "" {
				regBefore, _ = os.ReadFile(reg)
			}
			runPayload(t, payload, cfg, reg)
			if again, _ := os.ReadFile(cfg); string(again) != before {
				t.Errorf("second run modified the config:\n%s", again)
			}
			if tc.registration != "" {
				if again, _ := os.ReadFile(reg); string(again) != string(regBefore) {
					t.Errorf("second run modified the registration:\n%s", again)
				}
			}
		})
	}
}

// If the config write lands and the registration write fails, the operator must
// be told that — not that nothing was written. The installer's warning branch
// keys off this string to decide which message to print.
func TestMASPayloadReportsRegistrationWriteFailure(t *testing.T) {
	requirePython(t)
	if os.Geteuid() == 0 {
		t.Skip("running as root: file mode would not block the write")
	}
	payload := masPayload(t, "install.sh")

	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	config := "encryption:\n    allow: true\n"
	if err := os.WriteFile(cfg, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := filepath.Join(dir, "registration.yaml")
	if err := os.WriteFile(reg, []byte(validRegistration), 0o400); err != nil {
		t.Fatal(err)
	}

	stdout, code := runPayload(t, payload, cfg, reg)
	if code == 0 {
		t.Errorf("expected a non-zero exit, got 0 (stdout %q)", stdout)
	}
	if !strings.HasPrefix(stdout, "set registration failed") {
		t.Errorf("stdout = %q, want it to start with %q so the installer can tell the\n"+
			"config half succeeded", strings.TrimSpace(stdout), "set registration failed")
	}
	after, _ := os.ReadFile(cfg)
	if !strings.Contains(string(after), "msc4190: true") {
		t.Errorf("config should have been written before the registration was attempted:\n%s", after)
	}
}
