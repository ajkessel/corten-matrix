// corten-matrix - A Matrix-iMessage puppeting bridge.
// Copyright (C) 2024 Ludvig Rhodin
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.

package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	"maunium.net/go/mautrix/bridgev2/matrix/mxmain"

	"github.com/lrhodin/corten-matrix/pkg/imconfig"
)

// configPathFromArgs mirrors mxmain's -c/--config flag (default "config.yaml")
// so the config can be located before PreInit parses flags.
func configPathFromArgs() string {
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-c" || args[i] == "--config":
			if i+1 < len(args) {
				return args[i+1]
			}
		case strings.HasPrefix(args[i], "-c="):
			return strings.TrimPrefix(args[i], "-c=")
		case strings.HasPrefix(args[i], "--config="):
			return strings.TrimPrefix(args[i], "--config=")
		}
	}
	return "config.yaml"
}

// ensureNetworkConfigKeys appends any network keys the current build knows
// about but that are missing from the on-disk config, copying their documented
// defaults and comments from the embedded example. It runs before the bridge
// loads its config, so a `git pull` + rebuild + restart always lands a
// complete network block — independent of whether the framework's own config
// upgrade writes back (e.g. when the bridge is launched with --no-update).
//
// It is intentionally conservative — NO breaking changes:
//   - Keys that already exist are NEVER touched. The existing file is never
//     re-serialized; only the new key lines are spliced in, so every existing
//     value, comment and byte of formatting is preserved exactly.
//   - A config that does not parse is left completely untouched — a
//     structurally broken file is for manual repair; we must not make it worse.
//   - It writes only when something is actually missing, and writes atomically
//     (temp file + rename, mode 0600).
//   - It is idempotent: once the keys exist, every later run is a no-op.
func ensureNetworkConfigKeys(configPath string) {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return
	}

	// Parse ONLY to detect which keys are present and to locate the end of the
	// network block. We never marshal this tree back out.
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return // unparseable: leave for manual repair
	}
	if len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return
	}
	network := mappingValue(root.Content[0], "network")
	if network == nil || network.Kind != yaml.MappingNode || len(network.Content) == 0 {
		return
	}

	present := make(map[string]bool, len(network.Content)/2)
	endLine := network.Line
	for i := 0; i+1 < len(network.Content); i += 2 {
		present[network.Content[i].Value] = true
		if l := maxNodeLine(network.Content[i+1]); l > endLine {
			endLine = l
		}
		if network.Content[i].Line > endLine {
			endLine = network.Content[i].Line
		}
	}

	// Build the text to splice in: each missing key's comment block + key line
	// (+ any nested lines), taken verbatim from the example and indented one
	// level to sit under `network:`.
	var additions []string
	added := 0
	for _, blk := range exampleNetworkBlocks() {
		if present[blk.key] {
			continue
		}
		additions = append(additions, "")
		for _, line := range blk.lines {
			if line == "" {
				additions = append(additions, "")
			} else {
				additions = append(additions, "    "+line)
			}
		}
		added++
	}
	if added == 0 {
		return
	}

	lines := strings.Split(string(data), "\n")
	if endLine < 1 || endLine > len(lines) {
		return // line numbers out of range — bail rather than risk corruption
	}
	out := make([]string, 0, len(lines)+len(additions))
	out = append(out, lines[:endLine]...) // up to and including the last network line
	out = append(out, additions...)
	out = append(out, lines[endLine:]...) // the rest of the file, untouched

	if err := atomicWriteConfig(configPath, []byte(strings.Join(out, "\n"))); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not add missing config keys to %s: %v\n", configPath, err)
		return
	}
	fmt.Fprintf(os.Stderr, "Added %d missing network config key(s) to %s\n", added, configPath)
}

// mappingValue returns the value node for key in a YAML mapping node, or nil.
func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

// maxNodeLine returns the largest source line number anywhere in the subtree,
// so the end of a (possibly nested) value block can be located.
func maxNodeLine(n *yaml.Node) int {
	max := n.Line
	for _, c := range n.Content {
		if l := maxNodeLine(c); l > max {
			max = l
		}
	}
	return max
}

type exampleBlock struct {
	key   string
	lines []string // leading comment block + key line (+ nested lines), at column 0
}

// exampleNetworkBlocks splits the embedded example into per-top-level-key text
// blocks (each with its leading comment block and any nested lines), in file
// order. Working from the example text keeps additions clean and documented
// and keeps the example the single source of truth.
func exampleNetworkBlocks() []exampleBlock {
	var blocks []exampleBlock
	var pending []string
	cur := -1
	for _, line := range strings.Split(strings.TrimRight(imconfig.NetworkExampleConfig, "\n"), "\n") {
		isTopKey := len(line) > 0 && line[0] != ' ' && line[0] != '\t' && line[0] != '#' && strings.Contains(line, ":")
		isIndented := len(line) > 0 && (line[0] == ' ' || line[0] == '\t')
		switch {
		case isTopKey:
			blk := exampleBlock{key: strings.SplitN(strings.TrimSpace(line), ":", 2)[0]}
			blk.lines = append(blk.lines, trailingCommentBlock(pending)...)
			blk.lines = append(blk.lines, line)
			blocks = append(blocks, blk)
			cur = len(blocks) - 1
			pending = nil
		case isIndented && cur >= 0:
			blocks[cur].lines = append(blocks[cur].lines, line)
		default:
			pending = append(pending, line)
			cur = -1
		}
	}
	return blocks
}

// trailingCommentBlock returns the run of lines at the end of pending that
// immediately precede a key (i.e. everything after the last blank separator),
// so a key's own documentation comments travel with it.
func trailingCommentBlock(pending []string) []string {
	start := 0
	for i, line := range pending {
		if strings.TrimSpace(line) == "" {
			start = i + 1
		}
	}
	return pending[start:]
}

// atomicWriteConfig writes data to path via a temp file + rename so a partial
// write can never leave a truncated config behind.
func atomicWriteConfig(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "imessage-config-*.yaml")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// masAuthMetadataPath is the MSC2965 authentication metadata document. Synapse
// only serves it when authentication is delegated to Matrix Authentication
// Service (next-gen auth / MSC3861), which makes it a reliable MAS probe.
const masAuthMetadataPath = "/_matrix/client/v1/auth_metadata"

// ensureMASCompatibility turns on encryption.msc4190 when the homeserver has
// moved to next-gen auth (Matrix Authentication Service).
//
// Under MAS, Synapse stops serving /login altogether, so the framework's legacy
// bridge-bot device creation (m.login.application_service) fails and the bridge
// dies with "homeserver does not support appservice login" on every start.
// mautrix already ships the replacement — MSC4190's
// PUT /_matrix/client/v3/devices/{deviceID} — but only behind
// encryption.msc4190, which nothing in our setup flow used to set. So a working
// bridge breaks the moment the operator migrates their homeserver to MAS, with
// an error that doesn't say what to change.
//
// Detection is one GET of the MSC2965 metadata document. Anything other than a
// 200 — including any network error — is treated as "not MAS" and leaves the
// config completely untouched: a probe must never be able to block startup.
//
// Same conservative rules as ensureNetworkConfigKeys: a config that doesn't
// parse is left alone, the YAML tree is never re-serialized (only the one
// scalar line is rewritten, so comments and formatting survive), the write is
// atomic, and it is idempotent.
//
// The matching homeserver-side flag (io.element.msc4190: true in the appservice
// registration) lives on the homeserver and can't be set from here, so we print
// what the operator still has to do.
func ensureMASCompatibility(br *mxmain.BridgeMain) {
	if br.Config == nil || !br.Config.Encryption.Allow || br.Config.Encryption.MSC4190 {
		return
	}
	if !homeserverUsesMAS(br.Config.Homeserver.Address) {
		return
	}

	// Apply in memory too, so the fix takes effect on this run and not only
	// after the next restart.
	br.Config.Encryption.MSC4190 = true
	fmt.Fprintf(os.Stderr, "Homeserver uses Matrix Authentication Service (next-gen auth) — "+
		"enabled encryption.msc4190 so the bridge bot device is created via MSC4190 "+
		"instead of appservice login\n")
	if br.ConfigPath != "" {
		if err := setMSC4190OnDisk(br.ConfigPath); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not persist encryption.msc4190 to %s: %v\n",
				br.ConfigPath, err)
		}
	}
	fmt.Fprintf(os.Stderr, "ACTION REQUIRED: add `io.element.msc4190: true` to this bridge's appservice "+
		"registration on the homeserver and restart the homeserver, otherwise creating the bot "+
		"device will fail. See https://docs.mau.fi/bridges/general/end-to-bridge-encryption.html\n")
}

// homeserverUsesMAS reports whether the homeserver delegates authentication to
// Matrix Authentication Service. Errors mean "unknown", which is reported as
// false so a transient network problem can never flip the config.
func homeserverUsesMAS(address string) bool {
	if address == "" {
		return false
	}
	base, err := url.Parse(address)
	if err != nil || base.Host == "" {
		return false
	}
	base.Path = strings.TrimSuffix(base.Path, "/") + masAuthMetadataPath
	base.RawQuery = ""
	base.Fragment = ""

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.String(), nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}

// setMSC4190OnDisk sets encryption.msc4190 to true in the config file, editing
// only that one line (or splicing the key in if an older config predates it).
// The parsed tree is used solely to locate the line and is never marshalled
// back out.
func setMSC4190OnDisk(configPath string) error {
	data, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("config does not parse, leaving it untouched: %w", err)
	}
	if len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return fmt.Errorf("unexpected config structure")
	}
	encryption := mappingValue(root.Content[0], "encryption")
	if encryption == nil || encryption.Kind != yaml.MappingNode || len(encryption.Content) == 0 {
		return fmt.Errorf("no encryption block in config")
	}
	lines := strings.Split(string(data), "\n")

	if node := mappingValue(encryption, "msc4190"); node != nil {
		if node.Kind != yaml.ScalarNode {
			return fmt.Errorf("encryption.msc4190 is not a scalar")
		}
		if node.Value == "true" {
			return nil // already set on disk
		}
		if node.Line < 1 || node.Line > len(lines) {
			return fmt.Errorf("encryption.msc4190 line %d out of range", node.Line)
		}
		updated, ok := setScalarInLine(lines[node.Line-1], "msc4190", "true")
		if !ok {
			return fmt.Errorf("could not rewrite line %d (%q)", node.Line, lines[node.Line-1])
		}
		lines[node.Line-1] = updated
		return atomicWriteConfig(configPath, []byte(strings.Join(lines, "\n")))
	}

	// Key absent (config predates the framework option, e.g. when the bridge
	// runs with --no-update): splice it in as the encryption block's first key,
	// matching that block's existing indentation.
	indent := strings.Repeat(" ", encryption.Content[0].Column-1)
	after := encryption.Content[0].Line - 1 // insert above the first existing key
	if after < 1 || after > len(lines) {
		return fmt.Errorf("encryption block line %d out of range", encryption.Content[0].Line)
	}
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:after]...)
	out = append(out, indent+"msc4190: true")
	out = append(out, lines[after:]...)
	return atomicWriteConfig(configPath, []byte(strings.Join(out, "\n")))
}

// setScalarInLine replaces the value of a `key: value` config line. Everything
// around the value — indentation, the spacing after the colon, and any trailing
// comment together with the whitespace in front of it — is preserved byte for
// byte, so rewriting a value never reflows the file.
func setScalarInLine(line, key, value string) (string, bool) {
	idx := strings.Index(line, key+":")
	if idx < 0 || strings.TrimSpace(line[:idx]) != "" {
		return "", false
	}
	rest := line[idx+len(key)+1:]
	lead := rest[:len(rest)-len(strings.TrimLeft(rest, " \t"))]
	if lead == "" {
		lead = " "
	}
	gap, comment := "", ""
	if c := strings.Index(rest, "#"); c >= 0 {
		beforeComment := rest[:c]
		gap = beforeComment[len(strings.TrimRight(beforeComment, " \t")):]
		comment = rest[c:]
	}
	return line[:idx] + key + ":" + lead + value + gap + comment, true
}
