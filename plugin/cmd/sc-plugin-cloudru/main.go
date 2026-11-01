// SPDX-License-Identifier: MIT
// Copyright (c) Simple Container

// sc-plugin-cloudru is the Cloud.ru driver plugin for Simple Container,
// extracted from pkg/clouds/{cloudru,pulumi/cloudru} and pkg/cmd/cmd_cloudru per
// the Phase 3 extraction in forge/docs/design/2026-10-24/sc-plugin-architecture/.
//
// Shipped scope:
//   - cloudru-dns registrar   (ProvisionRegistrar + NewDnsRecord +
//     NewOverrideHeaderRule + ProvisionDomainForEndpoint)
//   - cloudru-evolution-vm    (VmHosts RPC — Phase 3d-ii, VmHosts seam from
//     api PR #488)
//
// The Container App template (`cloudru-container-app`) is a later slice; the
// config tier is already in ./api so a server.yaml naming it still parses —
// the provisioning half is not advertised here yet.
//
// # This binary has two modes, which the other plugins do not
//
// Cloud.ru's resources are not provider resources. Cloud DNS and Container Apps
// appear in no Terraform or Pulumi provider — not in Cloud.ru's own evo-terraform —
// and Pulumi's Go SDK has no dynamic providers, so the only way to put either
// inside Pulumi's resource model is a pulumi-command `local.Command` that shells out
// to a CLI which does the REST calls. In-tree that CLI was `sc cloudru …`.
//
// So this binary serves BOTH halves:
//
//   - **plugin mode** (no arguments): the go-plugin gRPC server below, which SC core
//     launches and which registers the `command:local:Command` resources.
//   - **CLI mode** (any arguments): the cobra tree from ./plugin/internal/cli, so
//     `sc-plugin-cloudru cloudru dns-record apply` is exactly what `sc cloudru
//     dns-record apply` was. This is what the rendered command text invokes, by
//     absolute path — see internal/scripts.go for why, and for what that costs.
//
// Every RPC opens a short-lived pulumi.Context through the shared
// api/pkg/plugin/runtime package and defers runtime.Settle to drain
// RegisterResource goroutines before the response is sent. That is what keeps a
// plugin that returns from a handler from dropping its own URN registrations
// (Addendum U/V of forge/docs/design/2026-10-24/sc-plugin-architecture/
// 04-no-destroy-invariant.md).
package main

import (
	"context"
	"fmt"
	"os"
	"sync"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/pkg/errors"
	sdk "github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/simple-container-com/api/pkg/api"
	"github.com/simple-container-com/api/pkg/api/logger"
	pApi "github.com/simple-container-com/api/pkg/clouds/pulumi/api"
	"github.com/simple-container-com/api/pkg/clouds/vm"
	scPlugin "github.com/simple-container-com/api/pkg/plugin"
	scpluginv1 "github.com/simple-container-com/api/pkg/plugin/proto/v1"
	"github.com/simple-container-com/api/pkg/plugin/runtime"

	cruApi "github.com/simple-container-com/sc-plugin-cloudru/api"
	cruInternal "github.com/simple-container-com/sc-plugin-cloudru/plugin/internal"
	cruCli "github.com/simple-container-com/sc-plugin-cloudru/plugin/internal/cli"
)

type cruPlugin struct {
	scpluginv1.UnimplementedSCCloudPluginServer

	mu        sync.Mutex
	registrar pApi.Registrar
	config    *cruApi.DnsRegistrarConfig
	progress  *scPlugin.Publisher
}

func (p *cruPlugin) Schema(_ context.Context, _ *scpluginv1.SchemaRequest) (*scpluginv1.SchemaResponse, error) {
	return &scpluginv1.SchemaResponse{
		PluginVersion:  "cloudru-0.0.1",
		RegistrarTypes: []string{cruApi.RegistrarTypeCloudruDns},
		// Intentionally NOT advertising ProviderTypes, for the same reason
		// sc-plugin-yandex does not: SC core would create the provider in its own
		// pctx, and a ProviderResource handle does not cross pctx boundaries. A
		// resource slice that needs one (the Container App) must therefore build it
		// inside the handler that registers the resource, in that handler's own
		// pctx. The registrar needs none at all — see ProvisionRegistrar.
		//
		// Two things the Container App slice will need and should not re-derive,
		// both settled in sc-plugin-yandex V6 (d6f7397) + api 787fb0fe:
		//   - Core skips its own initProvider for any resource type a plugin
		//     advertises here (pApi.PluginResourceTypes / IsPluginType), so the
		//     plugin owning the provider is the supported shape rather than a
		//     workaround — that is what the api pin in go.mod is for.
		//   - The provider's Pulumi name must be core's own formula,
		//     `<stack>--<resType>--<resName>--<env>--provider`, because the lifted
		//     driver then appends `--<env>` through input.ToResName and the URN
		//     comes out byte-identical to a pre-split deploy. Get it wrong and the
		//     state of a live stack reads as a provider replacement, which on a
		//     local.Command means every record and app is replaced with it.
		//
		// No ResourceTypes and no ComputeProcessorTypes yet: the Container App is
		// a later slice. An empty list is the honest answer — advertising a type
		// this binary cannot provision would have SC core route the resource here
		// and fail at dispatch instead of at config parse. The Evolution VM
		// provisions via VmDrivers below, not ResourceTypes — SC core routes
		// `type: vm` resources with `provider: cloudru-evolution-vm` to the
		// VmHosts RPC rather than to Provision.
		PulumiPlugins: []*scpluginv1.PulumiPluginRequirement{
			// pulumi-command — every Cloud.ru resource is a local.Command, so this
			// is the only provider plugin the stack needs. Matches the version in
			// api/go.mod (pulumi/pulumi-command/sdk v1.2.1); its own replacement
			// semantics are what the Triggers note in internal/registrar.go is about.
			{Package: "command", Version: "1.2.1"},
			// pulumi-tls — the Evolution VM driver mints the deploy key as a
			// tls.PrivateKey (ED25519). Matches api/go.mod. The remote.Command for
			// the docker bootstrap is `command`.
			{Package: "tls", Version: "5.3.0"},
		},
		VmDrivers: []*scpluginv1.VmDriverSpec{
			// Advertise the cloudru-evolution-vm driver so core routes a
			// `type: vm` / `provider: cloudru-evolution-vm` resource to this
			// plugin's VmHosts RPC. ToProto echoes the capability row verbatim
			// from the lifted api package — SC core and the plugin always agree
			// on what the driver can do, because they read the same source.
			vm.Drivers[vm.DriverCloudruEvolution].ToProto(vm.DriverCloudruEvolution),
		},
	}, nil
}

func (p *cruPlugin) ProvisionRegistrar(ctx context.Context, req *scpluginv1.ProvisionRegistrarRequest) (resp *scpluginv1.ProvisionRegistrarResponse, err error) {
	p.progress.Info("provision-registrar", fmt.Sprintf("initialising cloud.ru DNS registrar for zone %q", strVal(req.GetConfig()["zoneName"])))
	rctx, err := runtime.NewContext(ctx, req.GetHeader(), runtime.WithLocalEngineFallback(p.progress))
	if err != nil {
		return nil, err
	}
	defer runtime.Settle(rctx, &resp, &err)

	cfg := inputsToRegistrarConfig(req.GetConfig())
	desc := api.RegistrarDescriptor{
		Type:   cruApi.RegistrarTypeCloudruDns,
		Config: api.Config{Config: cfg},
	}
	// NO Provider, deliberately, and this is the one place in the plugin where that
	// is load-bearing rather than merely tidy.
	//
	// A local.Command needs no provider (recordOptions in internal/registrar.go
	// omits the option when there is none, which is also what the in-tree driver
	// gets from initRegistrar). Supplying one anyway cannot work across the bus: the
	// registrar is constructed in THIS pctx and cached on p, but each record is
	// registered from NewDnsRecord's own pctx, so sdk.Provider() would carry a
	// handle whose URN and ID are Outputs belonging to a context that has already
	// been closed. They never resolve, and the symptom is not an error — the
	// RegisterResource for the record simply never returns, so `sc provision`
	// prints "creating A record" and then hangs until the execution timeout with
	// the stack still at `create`. Live-caught 2026-10-25 on the first smoke of
	// this plugin; the preceding build did pass a provider and deadlocked exactly
	// there.
	r, err := cruInternal.Registrar(rctx.Pulumi, desc, pApi.ProvisionParams{Log: logger.New()})
	if err != nil {
		p.progress.Error("provision-registrar", "init failed: "+err.Error())
		return nil, errors.Wrap(err, "init cloud.ru DNS registrar")
	}
	p.mu.Lock()
	p.registrar = r
	p.config = cfg
	p.mu.Unlock()
	p.progress.Info("provision-registrar", fmt.Sprintf("ready; main domain is %q", r.MainDomain()))
	return &scpluginv1.ProvisionRegistrarResponse{MainDomain: r.MainDomain()}, nil
}

func (p *cruPlugin) NewDnsRecord(ctx context.Context, req *scpluginv1.NewDnsRecordRequest) (resp *scpluginv1.NewDnsRecordResponse, err error) {
	rctx, err := runtime.NewContext(ctx, req.GetHeader(), runtime.WithLocalEngineFallback(p.progress))
	if err != nil {
		return nil, err
	}
	defer runtime.Settle(rctx, &resp, &err)

	p.mu.Lock()
	r := p.registrar
	p.mu.Unlock()
	if r == nil {
		return nil, errors.New("NewDnsRecord called before ProvisionRegistrar")
	}
	p.progress.Info("dns-record", fmt.Sprintf("creating %s record %q → %q", req.GetRecordType(), req.GetRecordName(), req.GetValueResolved()))
	out, err := r.NewRecord(rctx.Pulumi, api.DnsRecord{
		Name:  req.GetRecordName(),
		Type:  req.GetRecordType(),
		Value: req.GetValueResolved(),
		// Proxied is deliberately dropped rather than forwarded: Cloud.ru has no
		// programmable edge, so a record is only ever a record. The registrar's own
		// comment in NewRecord says the same thing; passing it through would imply
		// it does something.
	})
	if err != nil {
		p.progress.Error("dns-record", fmt.Sprintf("record %q failed: %v", req.GetRecordName(), err))
		return nil, err
	}
	urn := waitForRef(out.Ref)
	p.progress.Info("dns-record", fmt.Sprintf("record %q registered (id=%s)", req.GetRecordName(), urn))
	return &scpluginv1.NewDnsRecordResponse{Urn: urn}, nil
}

// NewOverrideHeaderRule and ProvisionDomainForEndpoint are deliberate refusals in
// the lifted driver — Cloud.ru has no programmable edge, and a custom domain on a
// Container App needs a certificate id SC does not manage yet. They are wired
// anyway, because a refusal is only reachable if the RPC exists: an unimplemented
// RPC answers `Unimplemented`, which reads like a plugin-version mismatch instead
// of like the explanation the driver has written out.
func (p *cruPlugin) NewOverrideHeaderRule(ctx context.Context, req *scpluginv1.NewOverrideHeaderRuleRequest) (resp *scpluginv1.NewOverrideHeaderRuleResponse, err error) {
	rctx, err := runtime.NewContext(ctx, req.GetHeader(), runtime.WithLocalEngineFallback(p.progress))
	if err != nil {
		return nil, err
	}
	defer runtime.Settle(rctx, &resp, &err)

	p.mu.Lock()
	r := p.registrar
	p.mu.Unlock()
	if r == nil {
		return nil, errors.New("NewOverrideHeaderRule called before ProvisionRegistrar")
	}
	rule := pApi.OverrideHeaderRule{
		Name:       req.GetRuleName(),
		FromHost:   req.GetFromHost(),
		ToHost:     sdk.String(req.GetToHostResolved()),
		PathPrefix: req.GetPathPrefix(),
	}
	if ba := req.GetBasicAuth(); ba != nil {
		rule.BasicAuth = &pApi.BasicAuth{Username: ba.GetUsername(), Password: ba.GetPassword(), Realm: ba.GetRealm()}
	}
	if op := req.GetOverridePages(); op != nil {
		rule.OverridePages = &pApi.OverridePagesRule{IndexPage: op.GetIndexPage(), NotFoundPage: op.GetNotFoundPage()}
	}
	out, err := r.NewOverrideHeaderRule(rctx.Pulumi, api.Stack{Name: req.GetStackName()}, rule)
	if err != nil {
		p.progress.Error("worker-route", fmt.Sprintf("rule %q failed: %v", rule.Name, err))
		return nil, err
	}
	return &scpluginv1.NewOverrideHeaderRuleResponse{Urn: waitForRef(out.Ref)}, nil
}

func (p *cruPlugin) ProvisionDomainForEndpoint(ctx context.Context, req *scpluginv1.ProvisionDomainForEndpointRequest) (resp *scpluginv1.ProvisionDomainForEndpointResponse, err error) {
	rctx, err := runtime.NewContext(ctx, req.GetHeader(), runtime.WithLocalEngineFallback(p.progress))
	if err != nil {
		return nil, err
	}
	defer runtime.Settle(rctx, &resp, &err)

	p.mu.Lock()
	r := p.registrar
	p.mu.Unlock()
	if r == nil {
		return nil, errors.New("ProvisionDomainForEndpoint called before ProvisionRegistrar")
	}
	endpoint := pApi.DomainEndpoint{
		Name:       req.GetEndpointName(),
		Domain:     req.GetDomain(),
		TargetHost: sdk.String(req.GetTargetHostResolved()),
		WebSocket:  req.GetWebsocket(),
	}
	out, err := r.ProvisionDomainForEndpoint(rctx.Pulumi, api.Stack{Name: req.GetStackName()}, endpoint)
	if err != nil {
		p.progress.Error("endpoint", fmt.Sprintf("endpoint %q failed: %v", endpoint.Name, err))
		return nil, err
	}
	return &scpluginv1.ProvisionDomainForEndpointResponse{Urn: waitForRef(out.Ref)}, nil
}

// VmHosts converges a Cloud.ru Evolution VM into HostFacts the core-side
// stack code (compose/k3s/readiness) connects to over SSH.
func (p *cruPlugin) VmHosts(ctx context.Context, req *scpluginv1.VmHostsRequest) (resp *scpluginv1.VmHostsResponse, err error) {
	p.progress.Info("vm-hosts", fmt.Sprintf("provisioning cloudru-evolution-vm %q (driver=%s)", req.GetResourceName(), req.GetDriver()))
	rctx, err := runtime.NewContext(ctx, req.GetHeader(), runtime.WithLocalEngineFallback(p.progress))
	if err != nil {
		return nil, err
	}
	defer runtime.Settle(rctx, &resp, &err)

	resp, err = cruInternal.VmHosts(rctx.Pulumi, req)
	if err != nil {
		p.progress.Error("vm-hosts", fmt.Sprintf("vm %q failed: %v", req.GetResourceName(), err))
		return nil, err
	}
	p.progress.Info("vm-hosts", fmt.Sprintf("vm %q ready, %d host(s)", req.GetResourceName(), len(resp.GetHosts())))
	return resp, nil
}

func (p *cruPlugin) LogStream(_ *scpluginv1.LogStreamRequest, stream scpluginv1.SCCloudPlugin_LogStreamServer) error {
	ctx := stream.Context()
	for {
		batch, ok := p.progress.Drain()
		if !ok {
			return nil
		}
		for _, ev := range batch {
			if err := stream.Send(ev); err != nil {
				return err
			}
		}
		if ctx.Err() != nil {
			return nil
		}
	}
}

// inputsToRegistrarConfig lifts the SC-plugin-bus input map into the typed
// DnsRegistrarConfig.
//
// `credentials` is the opaque `${auth:cloudru-evolution}` blob; the key pair and the
// project id live inside it and are unpacked by api.ConvertAuth inside Registrar(),
// not here. The per-record `dnsRecords:` list is NOT read here either — SC core
// reads it from the config tier via api.RegistrarConfig's DnsRecords() and calls
// NewDnsRecord once per record (pkg/clouds/pulumi/plugin_sidecars.go).
func inputsToRegistrarConfig(m map[string]*scpluginv1.InputValue) *cruApi.DnsRegistrarConfig {
	return &cruApi.DnsRegistrarConfig{
		EvolutionAccountConfig: cruApi.EvolutionAccountConfig{
			Credentials: api.Credentials{Credentials: strVal(m["credentials"])},
			ProjectID:   strVal(m["projectId"]),
		},
		ZoneName:  strVal(m["zoneName"]),
		ZoneID:    strVal(m["zoneId"]),
		RecordTtl: intVal(m["recordTtl"]),
	}
}

func strVal(v *scpluginv1.InputValue) string {
	if v == nil {
		return ""
	}
	return v.GetStringValue()
}

// intVal reads a numeric input. The bus carries numbers as doubles, and the YAML
// loader on the SC-core side may have produced either a number or a string for an
// int field, so both are accepted — a recordTtl silently read as 0 would write a
// literal zero TTL into the operator's zone, which is the mistake
// EffectiveRecordTtl exists to prevent.
func intVal(v *scpluginv1.InputValue) int {
	if v == nil {
		return 0
	}
	switch x := v.GetValue().(type) {
	case *scpluginv1.InputValue_DoubleValue:
		return int(x.DoubleValue)
	case *scpluginv1.InputValue_StringValue:
		var n int
		if _, err := fmt.Sscanf(x.StringValue, "%d", &n); err != nil {
			return 0
		}
		return n
	default:
		return 0
	}
}

// waitForRef blocks until a Pulumi Output in a resource ref resolves. Settle
// already drains the pctx before the RPC response is sent, but each handler
// still needs a concrete string for the Urn field of its response — this is
// how we pluck one out of a resource ref. Same function in every plugin.
func waitForRef(ref any) string {
	if ref == nil {
		return ""
	}
	switch r := ref.(type) {
	case string:
		return r
	case sdk.IDOutput:
		ch := make(chan string, 1)
		r.ApplyT(func(id sdk.ID) sdk.ID {
			select {
			case ch <- string(id):
			default:
			}
			return id
		})
		return <-ch
	case sdk.URNOutput:
		ch := make(chan string, 1)
		r.ApplyT(func(u sdk.URN) sdk.URN {
			select {
			case ch <- string(u):
			default:
			}
			return u
		})
		return <-ch
	case sdk.StringOutput:
		ch := make(chan string, 1)
		r.ApplyT(func(s string) string {
			select {
			case ch <- s:
			default:
			}
			return s
		})
		return <-ch
	default:
		return fmt.Sprintf("%v", r)
	}
}

func main() {
	// CLI mode. go-plugin launches this binary with no arguments and talks gRPC over
	// stdio, so any argument at all means a human — or, far more often, the
	// local.Command this plugin itself created — is invoking a `cloudru` verb.
	if len(os.Args) > 1 {
		root := cruCli.NewCloudruCmd()
		// `cloudru` is kept as the command group so a hand-run reads exactly like the
		// in-tree `sc cloudru dns-record apply` it replaces, and so the rendered
		// script text in internal/scripts.go is unchanged from the monolith's.
		if os.Args[1] == root.Use {
			root.SetArgs(os.Args[2:])
		} else {
			root.SetArgs(os.Args[1:])
		}
		if err := root.Execute(); err != nil {
			// Never prints the request: it arrived on stdin and holds the IAM key
			// secret. cobra has already written the error to stderr.
			os.Exit(1)
		}
		return
	}

	impl := &cruPlugin{progress: scPlugin.NewPublisher(0)}
	scPlugin.Serve(scPlugin.ServerConfig{
		Impl: impl,
		Logger: hclog.New(&hclog.LoggerOptions{
			Name:   "sc-plugin-cloudru",
			Level:  hclog.Warn,
			Output: os.Stderr,
		}),
	})
}
