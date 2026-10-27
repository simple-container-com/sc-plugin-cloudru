// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

package cloudru

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pkg/errors"
	"github.com/pulumi/pulumi-command/sdk/go/command/local"
	"github.com/pulumi/pulumi-command/sdk/go/command/remote"
	"github.com/pulumi/pulumi-tls/sdk/v5/go/tls"
	sdk "github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/simple-container-com/api/pkg/api"
	"github.com/simple-container-com/api/pkg/clouds/cloudru"
	"github.com/simple-container-com/api/pkg/clouds/cloudru/evolution"
	cloudsvm "github.com/simple-container-com/api/pkg/clouds/pulumi/vm"
	"github.com/simple-container-com/api/pkg/clouds/vm"
	scPlugin "github.com/simple-container-com/api/pkg/plugin"
	scpluginv1 "github.com/simple-container-com/api/pkg/plugin/proto/v1"
)

// VmHosts converges one Cloud.ru Evolution VM for a `type: vm` resource whose
// `provider:` is cloudru-evolution-vm.
//
// Pure port of pkg/clouds/pulumi/vm/cloudruEvolutionDriver.Hosts in api — the
// same three Pulumi resources per host (tls.PrivateKey deploy key,
// local.Command apply/destroy, remote.Command docker bootstrap), the same
// find-or-create convergence, the same refusal of `network:`. One meaningful
// semantic change: the apply/destroy scripts invoke THIS plugin binary by
// absolute path out of the plugin cache (via scScript) instead of the in-tree
// `sc cloudru compute-vm …` on $PATH, which is gone from a core `sc` built
// after the Cloud.ru extraction.
//
// Three things this cloud refuses, measured 2026-10-24 against the live API,
// each shaping what is below:
//   - No user-data: POST /vms answers `extra_forbidden`. So docker installs
//     over SSH afterwards, not via cloud-init.
//   - No networking at create: subnet_id / security_group_ids / interfaces /
//     networks / nics / floating_ip all refused. A configured `network:` is
//     a hard refusal here rather than a silent drop.
//   - No pre-seeded SSH host key (follows from no user-data). First connection
//     is trust-on-first-use; `hostKey:` is the operator's escape hatch.
//
// Design: forge/docs/design/2026-10-13/sc-russia-market-readiness/ (the
// 2026-10-24 amendment) and docs/design/2026-10-24/sc-plugin-architecture/
// 07-plugin-first-for-new-clouds.md §3 (the VM seam).
func VmHosts(pctx *sdk.Context, req *scpluginv1.VmHostsRequest) (*scpluginv1.VmHostsResponse, error) {
	resourceName := req.GetResourceName()
	hostName := fmt.Sprintf("%s-host-0", resourceName)
	env := req.GetHeader().GetEnvironment()

	var cfg vm.VmConfig
	if err := json.Unmarshal(req.GetConfigJson(), &cfg); err != nil {
		return nil, errors.Wrapf(err, "decode VmConfig for vm %q", resourceName)
	}
	ports := portsFromProto(req.GetPorts())

	if cfg.Network != nil {
		return nil, errors.Errorf(
			"`network` is not supported with `provider: %s`: a Cloud.ru Evolution VM create accepts no "+
				"`subnet_id`, `security_group_ids`, `interfaces`, `networks` or `nics` (measured 2026-10-24, the "+
				"API answers `extra_forbidden`), so the machine always lands on the availability zone's default "+
				"subnet and default security group. Remove `network`, or place the machine by hand in the console "+
				"and declare it with `provider: %s` and `hosts:`", vm.DriverCloudruEvolution, vm.DriverByo,
		)
	}

	var account cloudru.EvolutionAccountConfig
	if err := api.ConvertAuth(&cfg, &account); err != nil {
		return nil, errors.Wrapf(err,
			"failed to read Cloud.ru Evolution credentials of vm %q; `credentials:` must reference an `auth:` "+
				"entry of type %q", resourceName, cloudru.AuthTypeCloudruEvolutionKey)
	}
	if err := account.Validate(); err != nil {
		return nil, errors.Wrapf(err, "invalid Cloud.ru Evolution credentials of vm %q", resourceName)
	}

	// The deploy key is a STACK RESOURCE, not an operator secret: SC generates it,
	// publishes the public half into the VM's `public_key` metadata slot, and rotates
	// it by recreating this resource. A key minted in Go would change the VM spec on
	// every provision, and the spec is what triggers the converge.
	deployKey, err := tls.NewPrivateKey(pctx, toResName(resourceName+"-deploy-key", env), &tls.PrivateKeyArgs{
		Algorithm: sdk.String("ED25519"),
	})
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create deploy key for vm %q", resourceName)
	}

	// Two documents from one spec: the request carries the key secret and goes to
	// Stdin (secret, encrypted in the checkpoint); the trigger is a hash of the
	// spec alone, because triggers are rendered verbatim in a Pulumi diff and must
	// not hold credentials.
	buildSpec := func(publicKey string) (evolution.VmSpec, error) {
		return cloudruEvolutionVmSpec(&cfg, hostName, publicKey, ports)
	}
	requestJSON := deployKey.PublicKeyOpenssh.ApplyT(func(publicKey string) (string, error) {
		spec, err := buildSpec(publicKey)
		if err != nil {
			return "", err
		}
		encoded, err := json.Marshal(evolution.Request{
			Credentials: evolution.Credentials{
				KeyID:     account.KeyID,
				Secret:    account.KeySecret,
				ProjectID: account.ProjectID,
			},
			Name:   spec.Name,
			VmSpec: &spec,
		})
		if err != nil {
			return "", errors.Wrapf(err, "failed to encode Cloud.ru Evolution vm request for %q", resourceName)
		}
		return string(encoded), nil
	}).(sdk.StringOutput)

	specHash := deployKey.PublicKeyOpenssh.ApplyT(func(publicKey string) (string, error) {
		spec, err := buildSpec(publicKey)
		if err != nil {
			return "", err
		}
		encoded, err := json.Marshal(spec)
		if err != nil {
			return "", errors.Wrapf(err, "failed to hash Cloud.ru Evolution vm spec for %q", resourceName)
		}
		sum := sha256.Sum256(encoded)
		return hex.EncodeToString(sum[:]), nil
	}).(sdk.StringOutput)

	applyScript := scComputeVmResultScript("compute-vm", "apply")
	destroyScript := scScript("cloudru", "compute-vm", "destroy")

	vmCmd, err := local.NewCommand(pctx, toResName(hostName+"-compute-vm", env), &local.CommandArgs{
		Create: sdk.String(applyScript),
		// Update is the same script, not absent: without it pulumi-command turns any
		// input change into a REPLACEMENT, performed as delete-then-create — which on
		// this resource means destroying the machine, and with it the boot disk and
		// every stack deployed onto it, because one field of the spec moved. `apply`
		// is a find-or-create converge and refuses the changes it cannot make in
		// place (a resize) instead of taking that decision itself.
		Update: sdk.String(applyScript),
		Delete: sdk.StringPtr(destroyScript),
		// The spec hash, and deliberately NOT a per-run nonce: this command CHANGES
		// the cloud, and `apply` cannot tell before it runs whether there is
		// anything to change — it is cheap to re-run, but it is not free, and on a
		// deploy that changed nothing a nonce would re-converge the machine every
		// time. The script text is constant, so the hash is the only thing that can
		// move.
		Triggers:               sdk.Array{specHash},
		Stdin:                  sdk.ToSecret(requestJSON).(sdk.StringOutput).ToStringPtrOutput(),
		Logging:                local.LoggingNone.ToLoggingPtrOutput(),
		AddPreviousOutputInEnv: sdk.Bool(false),
	})
	if err != nil {
		return nil, errors.Wrapf(err, "failed to converge Cloud.ru Evolution vm %q", resourceName)
	}

	// Parse the floating IP out of the apply command's stdout — this cloud has no
	// Pulumi provider for the compute resource, so the IP is not a provider
	// property we can OutputRefTo. AddressOutput awaits in-plugin and emits a
	// literal; on preview the value is unknown and lands as a known empty string,
	// which core's downstream SSH gating treats correctly (its own deps are also
	// unknown on preview and it never dials).
	address := evolutionResultField(vmCmd.Stdout, func(r evolution.Result) string { return r.Address })

	// Docker, installed over SSH because this cloud has no user-data. Done inside
	// the plugin — not left to the stack above — so that the readiness gate and
	// the compose preflight (which asserts exactly what this installs) cannot run
	// first and fail on a machine that is merely young.
	hostFacts := vmHostFactsForBootstrap{
		Address: address,
		User:    cfg.EffectiveSshUser(),
		Port:    vm.DefaultSshPort,
		// tls.PrivateKey already marks its private outputs secret; wrapping again is
		// idempotent and keeps the guarantee local to the line that reads it.
		PrivateKey: sdk.ToSecret(deployKey.PrivateKeyOpenssh).(sdk.StringOutput),
		// Empty, but secret-wrapped: ConnectionArgs .ApplyT's on this field and the
		// zero value panics. An SC-generated key is never an encrypted PEM.
		PrivateKeyPassword: sdk.ToSecret(sdk.String("")).(sdk.StringOutput),
		HostKey:            sdk.String(strings.TrimSpace(cfg.HostKey)).ToStringOutput(),
	}
	dockerCmd, err := cloudruEvolutionDockerBootstrap(pctx, resourceName, hostName, env, hostFacts, vmCmd)
	if err != nil {
		return nil, err
	}

	host := scPlugin.NewVmHost(pctx.Context(), hostName).
		User(cfg.EffectiveSshUser()).
		Port(vm.DefaultSshPort).
		Role(string(vm.RoleCompose)).
		AddressOutput(address).
		PrivateKeyOf(deployKey, "privateKeyOpenssh").
		DependsOn(vmCmd, dockerCmd)

	// Whatever the operator pinned, or TrustOnFirstUse. Unlike every other cloud
	// driver this one cannot generate the machine's identity (see the file
	// header), so HostKeyPinned is a property of the CONFIG rather than of this
	// driver, and the loud warning for the unpinned case is what core's own
	// unpinned-host-key handling already emits.
	trimmedHostKey := strings.TrimSpace(cfg.HostKey)
	if trimmedHostKey != "" {
		host = host.HostKey(trimmedHostKey)
	} else {
		host = host.TrustOnFirstUse()
	}

	return scPlugin.BuildVmHosts(host)
}

// vmHostFactsForBootstrap is the subset of HostFacts the docker bootstrap step
// needs — carrying it as a struct keeps the two call sites' arguments in sync.
//
// PrivateKeyPassword is deliberately an EMPTY SECRET sdk.StringOutput rather
// than left zero. ConnectionArgs calls .ApplyT on this field, and the zero
// value is a nil OutputState that panics on .ApplyT; the empty-but-marked-
// secret shape is what every self-keying driver means by "no passphrase".
type vmHostFactsForBootstrap struct {
	Address            sdk.StringOutput
	User               string
	Port               int
	PrivateKey         sdk.StringOutput
	PrivateKeyPassword sdk.StringOutput
	HostKey            sdk.StringOutput
}

func cloudruEvolutionDockerBootstrap(
	pctx *sdk.Context,
	resourceName, hostName, env string,
	h vmHostFactsForBootstrap,
	vmCmd *local.Command,
) (*remote.Command, error) {
	script := dockerBootstrapScript(h.User)

	// cloudsvm.ConnectionArgs is used by the in-tree k3s driver too — "nil-not-empty"
	// host-key handling and dial tuning that is load-bearing, not stylistic.
	hostFacts := cloudsvm.HostFacts{
		Name:               hostName,
		Address:            h.Address,
		User:               h.User,
		Port:               h.Port,
		PrivateKey:         h.PrivateKey,
		PrivateKeyPassword: h.PrivateKeyPassword,
		HostKey:            h.HostKey,
	}

	cmd, err := remote.NewCommand(pctx, toResName(hostName+"-docker", env), &remote.CommandArgs{
		Connection: cloudsvm.ConnectionArgs(hostFacts),
		Create:     sdk.String(script),
		Update:     sdk.String(script),
		// Guarded by the script's internal docker-already-installed check; the nonce
		// is what makes the guarded command repair drift when a machine is rebuilt
		// underneath the stack. All three preconditions in cloudsvm/nonce.go hold:
		// the no-op is observable nowhere, costs one SSH round-trip, and the command
		// carries no Delete (SC does not uninstall docker from a machine it is
		// destroying anyway).
		Triggers: cloudsvm.TriggersForGuardedChange(
			h.Address,
			sdk.String(h.User),
			sdk.String(script),
		),
		Logging:                remote.LoggingNone.ToLoggingPtrOutput(),
		AddPreviousOutputInEnv: sdk.Bool(false),
	}, sdk.DependsOn([]sdk.Resource{vmCmd}))
	if err != nil {
		return nil, errors.Wrapf(err, "failed to install docker on host %q of vm %q", hostName, resourceName)
	}
	return cmd, nil
}

// portsFromProto converts the wire port list onto this cloud's internal shape.
func portsFromProto(ports []*scpluginv1.VmPortRange) []vm.PortRange {
	out := make([]vm.PortRange, 0, len(ports))
	for _, p := range ports {
		out = append(out, vm.PortRange{
			Label:    p.GetLabel(),
			Protocol: p.GetProtocol(),
			From:     int(p.GetFrom()),
			To:       int(p.GetTo()),
			Internal: p.GetInternal(),
		})
	}
	return out
}

// toResName mirrors api.ResourceInput.ToResName: `<name>` or `<name>--<env>` so
// the plugin's registered URNs match the formula SC core expects, and the
// provider name formula on which the no-destroy invariant hangs.
func toResName(name, env string) string {
	if env == "" {
		return name
	}
	return name + "--" + env
}
