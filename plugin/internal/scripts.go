// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package cloudru

import (
	"os"
	"strings"
	"sync"
)

// executable is the absolute path of THIS plugin binary, and it is what a record's
// local.Command runs.
//
// In-tree, the same scripts said `sc cloudru dns-record apply` and relied on `sc`
// being on the runner's PATH. Out of tree that is no longer true in either
// direction: a core `sc` built from `api` after the Cloud.ru extraction has no
// `cloudru` verbs at all, and the plugin's own binary — which does — is not on any
// PATH, it sits in the plugin cache (`~/.sc/plugins/cloudru/<version>/`). So the
// script names the running binary by absolute path.
//
// The cost of that choice is worth stating where it will be read. The command text
// is an INPUT PROPERTY of the local.Command and is stored in Pulumi state, so the
// path appears in the state file and a change to it is a diff. Within one pinned
// plugin version the path is stable, so there is no diff; a version bump moves the
// cache directory and therefore changes `create`/`delete`/`update`.
//
// That diff is an UPDATE, not a replace — measured, not assumed. Moving a live
// record's binary to a second path and previewing gives
// `~ command:local:Command: (update)` with all three scripts in the diff and
// `1 to update, 1 unchanged` (smoke on atriumdev.online, 2026-10-25). Which is the
// outcome that matters: a replace on these records is the destructive case the
// Triggers note in registrar.go describes, because pulumi-command replaces
// create-before-delete and the delete would then remove the record the create just
// wrote. Worth re-checking if pulumi-command is ever bumped past v1.2.1 — the
// provider already does not honour its own documentation about `triggers`.
//
// Resolved lazily rather than at init: a plugin exec'd only to answer `Schema`
// should not fail to start because os.Executable did.
var (
	executableMu   sync.Mutex
	executablePath string
)

// SetExecutable pins the binary the rendered scripts invoke, overriding
// os.Executable. Exists for tests, which must not bake the test binary's own path
// into an assertion.
func SetExecutable(path string) {
	executableMu.Lock()
	defer executableMu.Unlock()
	executablePath = path
}

func pluginExecutable() string {
	executableMu.Lock()
	defer executableMu.Unlock()
	if executablePath != "" {
		return executablePath
	}
	exe, err := os.Executable()
	if err != nil || exe == "" {
		// Last resort: a PATH lookup, which fails loudly in the command's stderr
		// rather than silently doing nothing.
		executablePath = "sc-plugin-cloudru"
	} else {
		executablePath = exe
	}
	return executablePath
}

// scScript renders the shell a local.Command runs to invoke one `cloudru`
// subcommand of this binary.
//
// Nothing is interpolated into it: the whole request — credentials, the stack's
// secrets, the spec — arrives on the command's Stdin, which is why there is no
// mktemp/trap dance here as there is in the vm driver's shipScript (that one has to
// materialise an SSH key as a *file*, because `--ssh-key` takes a path). An
// argument would be readable in `ps` by every other process on the runner, and an
// environment variable would be readable by every other step of the job.
func scScript(args ...string) string {
	return strings.Join([]string{
		"set -eu",
		shellQuote(pluginExecutable()) + " " + strings.Join(args, " "),
	}, "\n") + "\n"
}

// shellQuote single-quotes a path for the `sh -c` the provider runs. Plugin cache
// paths are tame, but they contain the plugin version, which is operator-supplied
// through `plugins:` in server.yaml.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
