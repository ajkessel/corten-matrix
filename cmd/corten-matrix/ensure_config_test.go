// corten-matrix - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"maunium.net/go/mautrix/bridgev2/bridgeconfig"
	"maunium.net/go/mautrix/bridgev2/matrix/mxmain"
)

const encryptionConfig = `homeserver:
    address: https://matrix.example.com
    domain: example.com

encryption:
    # Whether to enable encryption at all.
    allow: true
    default: true
    require: false
    appservice: false
    # Whether to use MSC4190 instead of appservice login.
    msc4190: false  # keep me
    msc4392: false
    pickle_key: hunter2
`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// masServer serves the MSC2965 metadata document with the given status, so the
// probe can be exercised without touching a real homeserver.
func masServer(t *testing.T, status int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != masAuthMetadataPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"issuer":"https://auth.example.com/"}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func newBridgeMain(configPath, address string, allow, msc4190 bool) *mxmain.BridgeMain {
	br := &mxmain.BridgeMain{ConfigPath: configPath, Config: &bridgeconfig.Config{}}
	br.Config.Homeserver.Address = address
	br.Config.Encryption.Allow = allow
	br.Config.Encryption.MSC4190 = msc4190
	return br
}

func TestEnsureMASCompatibility(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		allow      bool
		msc4190    bool
		wantMemory bool
		wantOnDisk bool
	}{
		{name: "mas detected flips config", status: http.StatusOK, allow: true, wantMemory: true, wantOnDisk: true},
		{name: "already enabled is a no-op", status: http.StatusOK, allow: true, msc4190: true, wantMemory: true},
		{name: "encryption disabled is a no-op", status: http.StatusOK},
		{name: "no mas leaves config alone", status: http.StatusNotFound, allow: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, encryptionConfig)
			br := newBridgeMain(path, masServer(t, tc.status), tc.allow, tc.msc4190)

			ensureMASCompatibility(br)

			if br.Config.Encryption.MSC4190 != tc.wantMemory {
				t.Errorf("in-memory MSC4190 = %v, want %v", br.Config.Encryption.MSC4190, tc.wantMemory)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read back config: %v", err)
			}
			gotOnDisk := strings.Contains(string(data), "msc4190: true")
			if gotOnDisk != tc.wantOnDisk {
				t.Errorf("on-disk msc4190: true = %v, want %v\n%s", gotOnDisk, tc.wantOnDisk, data)
			}
			if !tc.wantOnDisk && string(data) != encryptionConfig {
				t.Errorf("config was modified when it should not have been:\n%s", data)
			}
		})
	}
}

func TestEnsureMASCompatibilityUnreachableHomeserver(t *testing.T) {
	path := writeConfig(t, encryptionConfig)
	// Port 0 is never listening, so the probe fails at the transport layer.
	br := newBridgeMain(path, "http://127.0.0.1:0", true, false)

	ensureMASCompatibility(br)

	if br.Config.Encryption.MSC4190 {
		t.Error("a failed probe must not enable MSC4190")
	}
	data, _ := os.ReadFile(path)
	if string(data) != encryptionConfig {
		t.Errorf("a failed probe must not touch the config:\n%s", data)
	}
}

func TestSetMSC4190OnDiskPreservesFormatting(t *testing.T) {
	path := writeConfig(t, encryptionConfig)
	if err := setMSC4190OnDisk(path); err != nil {
		t.Fatalf("setMSC4190OnDisk: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, "    msc4190: true  # keep me\n") {
		t.Errorf("indentation or trailing comment lost:\n%s", got)
	}
	for _, keep := range []string{
		"# Whether to use MSC4190 instead of appservice login.",
		"pickle_key: hunter2",
		"msc4392: false",
		"allow: true",
	} {
		if !strings.Contains(got, keep) {
			t.Errorf("lost %q from config:\n%s", keep, got)
		}
	}

	// Idempotent: a second run must not change the file again.
	before := got
	if err := setMSC4190OnDisk(path); err != nil {
		t.Fatalf("second setMSC4190OnDisk: %v", err)
	}
	data, _ = os.ReadFile(path)
	if string(data) != before {
		t.Errorf("second run modified the file:\n%s", data)
	}
}

func TestSetMSC4190OnDiskSplicesMissingKey(t *testing.T) {
	path := writeConfig(t, "encryption:\n    allow: true\n    default: true\n")
	if err := setMSC4190OnDisk(path); err != nil {
		t.Fatalf("setMSC4190OnDisk: %v", err)
	}
	data, _ := os.ReadFile(path)
	if want := "encryption:\n    msc4190: true\n    allow: true\n"; !strings.HasPrefix(string(data), want) {
		t.Errorf("missing key was not spliced in with matching indentation:\n%s", data)
	}
}

func TestSetMSC4190OnDiskLeavesBrokenConfigAlone(t *testing.T) {
	broken := "encryption:\n  allow: true\n :::not yaml\n\t\tnope\n"
	path := writeConfig(t, broken)
	if err := setMSC4190OnDisk(path); err == nil {
		t.Error("expected an error for an unparseable config")
	}
	data, _ := os.ReadFile(path)
	if string(data) != broken {
		t.Errorf("unparseable config was modified:\n%s", data)
	}
}

func TestSetScalarInLine(t *testing.T) {
	tests := []struct {
		line string
		want string
		ok   bool
	}{
		{line: "    msc4190: false", want: "    msc4190: true", ok: true},
		{line: "msc4190: false", want: "msc4190: true", ok: true},
		{line: "\tmsc4190: false   # note", want: "\tmsc4190: true   # note", ok: true},
		{line: "    # msc4190: false", ok: false},
		{line: "    other: false", ok: false},
	}
	for _, tc := range tests {
		got, ok := setScalarInLine(tc.line, "msc4190", "true")
		if ok != tc.ok {
			t.Errorf("setScalarInLine(%q) ok = %v, want %v", tc.line, ok, tc.ok)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("setScalarInLine(%q) = %q, want %q", tc.line, got, tc.want)
		}
	}
}
