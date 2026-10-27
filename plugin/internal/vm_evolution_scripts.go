// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package cloudru

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pkg/errors"
	sdk "github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/simple-container-com/api/pkg/clouds/cloudru/evolution"
	"github.com/simple-container-com/api/pkg/clouds/vm"
	scPlugin "github.com/simple-container-com/api/pkg/plugin"
)

// cloudruEvolutionVmSpec turns the cloud-independent VmConfig into the request
// `sc-plugin-cloudru cloudru compute-vm apply` reads.
//
// Resolved by cores+memory and NEVER by flavor name: the catalogue renames
// sizes under us — the 2 vCPU / 4 GiB flavor was `low-2-4` on 2026-10-23 and
// `lowcost10-2-4` a day later, with no `low-2-4` left in the catalogue. A name
// compiled in would have failed a deploy on Cloud.ru's schedule.
func cloudruEvolutionVmSpec(
	cfg *vm.VmConfig,
	hostName, deployPublicKey string,
	ports []vm.PortRange,
) (evolution.VmSpec, error) {
	size := cfg.EffectiveSize()
	memGiB, err := scPlugin.Gibibytes(size.Memory, "size.memory")
	if err != nil {
		return evolution.VmSpec{}, err
	}
	spec := evolution.VmSpec{
		Name:         hostName,
		Cores:        size.Cores,
		MemoryGiB:    memGiB,
		Image:        cfg.EffectiveImage(),
		SSHUser:      cfg.EffectiveSshUser(),
		PublicKey:    strings.TrimSpace(deployPublicKey),
		PublicIP:     true,
		IngressRules: evolutionIngressRules(hostName, ports),
	}
	if len(cfg.Zones) > 0 && cfg.Zones[0] != "" {
		spec.Zone = cfg.Zones[0]
	}
	if size.Disk != "" {
		disk, err := scPlugin.Gibibytes(size.Disk, "size.disk")
		if err != nil {
			return evolution.VmSpec{}, err
		}
		spec.DiskGiB = disk
	}
	// Left empty when unset: lets the client pick the project's default disk
	// type by name. An SC-invented default here would be a guess about an
	// account's quota, and the type names are not the same across projects.
	spec.DiskType = size.DiskType
	return spec, nil
}

// evolutionIngressRules maps the resolved port list (core's firewall.go) onto
// this cloud's rule shape.
//
// Two decisions worth keeping:
//
//   - The description carries the VM's name, not just the label. On this cloud
//     the VM joins the project's SHARED default security group, so a destroy
//     has to tell SC's rules apart from the siblings' by description. A sweep
//     keyed on a bare `simple-container ` prefix would close another VM's
//     ports.
//   - A node-to-node port becomes the private subnet's CIDR, filled in by the
//     CLI once it knows which subnet the machine actually landed on (SC does
//     not get to choose that here). Expressed as empty CIDR + Internal, not
//     guessed at, because the provisioning tier never learns the subnet.
func evolutionIngressRules(hostName string, ports []vm.PortRange) []evolution.IngressRule {
	rules := make([]evolution.IngressRule, 0, len(ports))
	for _, p := range ports {
		rules = append(rules, evolution.IngressRule{
			Protocol:    p.Protocol,
			PortRange:   fmt.Sprintf("%d:%d", p.From, p.To),
			Internal:    p.Internal,
			Description: evolution.RuleDescription(hostName, p.Label),
		})
	}
	return rules
}

// dockerBootstrapScript is the remote half of the docker install.
//
// apt from download.docker.com, chosen for a measured reason: the smoke
// machine inside Cloud.ru Evolution reaches `download.docker.com`,
// `archive.ubuntu.com` and `security.ubuntu.com` (verified 2026-10-23,
// docker-ce 5:29.9.2-1~ubuntu.24.04~noble installed that way). Deliberately
// NOT the `get.docker.com` convenience script: it pipes a remote shell script
// into root over a link this market cannot always make, and re-runs its whole
// repo setup every time.
//
// DEBIAN_FRONTEND and -o DPkg::Lock::Timeout matter on a machine this young:
// unattended-upgrades holds the dpkg lock for the first minutes after boot,
// and apt without a lock timeout fails instantly. The timeout alone is not
// enough — `apt-get update` also takes /var/lib/apt/lists/lock, and THAT one
// it tries once and abandons:
//
//	E: Could not get lock /var/lib/apt/lists/lock. It is held by process 911 (apt-get)
//
// Process 911 is the image's own first-boot apt run — the collision is the
// normal state when we connect the moment sshd answers, not an edge case. So
// every apt invocation retries. `cloud-init status --wait` would be the tidier
// gate and is not available on this image; polling `fuser` needs psmisc,
// which is a package install, which needs apt. A retry loop needs nothing
// that is not already there.
func dockerBootstrapScript(user string) string {
	return strings.Join([]string{
		`set -eu`,
		`export DEBIAN_FRONTEND=noninteractive`,
		`APT="sudo -E apt-get -y -o DPkg::Lock::Timeout=300"`,
		// 40 × 15s = 10 minutes, generously past the ~2 minutes an Ubuntu cloud
		// image's first-boot apt actually takes here. A failure after that is a
		// real one and must surface: a bootstrap that gave up quietly would hand
		// the compose step a machine with no docker on it.
		`apt_retry() {`,
		`  n=0`,
		`  until $APT "$@"; do`,
		`    n=$((n+1))`,
		`    if [ "$n" -ge 40 ]; then`,
		`      echo "apt is still locked after 10 minutes, giving up on: $*" >&2`,
		`      exit 1`,
		`    fi`,
		`    echo "apt is locked (the image's own first-boot run?), retry $n/40: $*" >&2`,
		`    sleep 15`,
		`  done`,
		`}`,
		// The guard, checked AS THE LOGIN USER: "docker is installed" is not the
		// property the compose path needs — it needs this user to be able to talk
		// to the daemon, which is group membership, and that is the half that
		// silently goes missing when a machine is rebuilt.
		`if command -v docker >/dev/null 2>&1 && docker version >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then`,
		`  echo docker-already-installed`,
		`  exit 0`,
		`fi`,
		`apt_retry update`,
		`apt_retry install ca-certificates curl`,
		`sudo install -m 0755 -d /etc/apt/keyrings`,
		`sudo curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc`,
		`sudo chmod a+r /etc/apt/keyrings/docker.asc`,
		`. /etc/os-release`,
		`echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] ` +
			`https://download.docker.com/linux/ubuntu ${VERSION_CODENAME} stable" | ` +
			`sudo tee /etc/apt/sources.list.d/docker.list >/dev/null`,
		`apt_retry update`,
		`apt_retry install docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin`,
		`sudo systemctl enable --now docker`,
		// The group, then verification AS ROOT. The login shell of THIS session
		// predates the usermod, so `docker version` as the user would fail here
		// even though it will succeed on the next connection — pulumi-command
		// dials afresh for each command.
		fmt.Sprintf(`sudo usermod -aG docker %s`, user),
		shellFail(`sudo docker version >/dev/null 2>&1`, "docker was installed but the daemon is not answering"),
		shellFail(`sudo docker compose version >/dev/null 2>&1`, "docker was installed without the compose v2 plugin"),
		`echo docker-installed`,
	}, "\n") + "\n"
}

// shellFail wraps a condition in the shell idiom that fails the whole script
// with a readable message. Inline from the in-tree driver's `fail` — three
// lines, no invariant worth sharing.
func shellFail(cond, msg string) string {
	return cond + ` || { echo "` + msg + `" >&2; exit 1; }`
}

// scComputeVmResultScript renders the shell `compute-vm apply` runs on the
// local half. Uses scScript — which invokes THIS plugin binary by absolute
// path from the plugin cache — plus a shell-level check that a result document
// came back.
//
// The check is here, and not only in parseEvolutionResult, because of HOW an
// older plugin binary fails. It does not error: `cloudru compute-vm` with an
// unknown subcommand prints the group's help and exits 0, so pulumi-command
// records a successful create whose stdout is that help text. From then on the
// stack is wedged in a way very hard to read — the parse happens against the
// STORED stdout, so the next `pulumi preview` fails before it plans anything,
// and the stack can be neither re-deployed nor destroyed without hand-editing
// the checkpoint. Failing the create instead leaves nothing in state.
//
// The captured output is echoed back verbatim so pulumi-command still sees
// exactly what the command wrote; nothing is lost by buffering, because
// progress goes to stderr and stdout is contractually the single JSON line.
// Matched loosely (an object mentioning `name`) rather than parsed: the
// shell's job is only to tell a result document from a help screen.
func scComputeVmResultScript(args ...string) string {
	invocation := shellQuote(pluginExecutable()) + " cloudru " + strings.Join(args, " ")
	label := "cloudru " + strings.Join(args, " ")
	return strings.Join([]string{
		"set -eu",
		"out=$(" + invocation + ")",
		`printf '%s\n' "$out"`,
		`printf '%s\n' "$out" | grep -q '^[[:space:]]*{.*"name"' || {`,
		`  echo '` + label + ` printed no result document: this sc-plugin-cloudru is older than the deploy' >&2`,
		"  exit 1",
		"}",
	}, "\n") + "\n"
}

// evolutionResultField reads one field out of the JSON document `cloudru
// compute-vm apply` printed, so the stack can use facts only the API knew —
// the floating IP above all, which is what every later SSH connection dials.
//
// Scanned from the END for a parseable object rather than decoded whole:
// stdout is contractually one JSON document, but the CLI's own logger writes
// to the same stream and an operator debugging a deploy by hand will have the
// progress lines in front of them.
func evolutionResultField(stdout sdk.StringOutput, pick func(evolution.Result) string) sdk.StringOutput {
	return stdout.ApplyT(func(out string) (string, error) {
		// Empty during a preview: the command has not run, and an error here would
		// fail the plan rather than the deploy.
		if strings.TrimSpace(out) == "" {
			return "", nil
		}
		res, err := parseEvolutionResult(out)
		if err != nil {
			return "", err
		}
		return pick(*res), nil
	}).(sdk.StringOutput)
}

func parseEvolutionResult(stdout string) (*evolution.Result, error) {
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var res evolution.Result
		if err := json.Unmarshal([]byte(line), &res); err == nil && res.Name != "" {
			return &res, nil
		}
	}
	// Does not echo stdout: LoggingNone keeps the command's output out of the log
	// on purpose, and repeating it in an error would undo that.
	return nil, errors.Errorf("could not find a result document in the output of `cloudru compute-vm apply` " +
		"(expected one JSON object on its own line); the plugin binary on this runner may be older than this deploy")
}
