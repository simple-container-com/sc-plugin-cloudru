# sc-plugin-cloudru

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

Cloud.ru driver plugin for [Simple Container](https://github.com/simple-container-com/api). Extracted from the SC monolith's `pkg/clouds/{cloudru,pulumi/cloudru}` and `pkg/cmd/cmd_cloudru` subtrees as the **third out-of-tree sc-plugin** per the [`sc-plugin-architecture`](https://github.com/simple-container-com/forge/tree/main/docs/design/2026-10-24/sc-plugin-architecture) design.

The plugin talks to SC core's running Pulumi engine over the SC plugin bus, registers resources against the SAME monitor, and shows up in state exactly as the in-tree driver did.

## What this plugin does

**V1 — the registrar slice**:
- **DNS registrar** (`type: cloudru-dns`): per-record writes into a Cloud.ru **Evolution** Cloud DNS public zone, with the zone looked up and never created.
- `NewOverrideHeaderRule` and `ProvisionDomainForEndpoint` are wired, and both are **deliberate refusals** — Cloud.ru has no programmable edge, and a custom domain on a Container App needs a certificate id SC does not manage yet. They are wired anyway so the driver's own explanation is what the operator sees, instead of a gRPC `Unimplemented` that reads like a version mismatch.
- `LogStream` with progress events tagged by phase (`provision-registrar`, `dns-record`, `worker-route`, `endpoint`).

**Config tier present, provisioning not yet advertised**: the Container App template (`cloudru-container-app`) and the Evolution VM are later slices. Their config structs already live in `/api`, so a `server.yaml` naming them still parses and still generates schemas — it is the provisioning half that this binary does not claim in `Schema` yet. Advertising a type the binary cannot provision would make SC route the resource here and fail at dispatch rather than at config parse.

## This binary has two modes, which the other plugins do not

Cloud.ru's resources are **not provider resources**. Cloud DNS and Container Apps appear in no Terraform or Pulumi provider — not in Cloud.ru's own `evo-terraform` — and Pulumi's Go SDK has no dynamic providers. So the only way to put either inside Pulumi's resource model is a pulumi-command `local.Command` that shells out to a CLI which makes the REST calls. In-tree that CLI was `sc cloudru …`.

After extraction neither half of that is true any more: a core `sc` built from `api` without the in-tree driver has no `cloudru` verbs, and this plugin's binary — which does — is not on any `PATH`, it lives in the plugin cache. So the released binary serves **both** halves:

| invocation | mode |
|---|---|
| no arguments | the go-plugin gRPC server; SC core launches it and it registers `command:local:Command` resources |
| any arguments | the `cloudru` cobra tree — `sc-plugin-cloudru cloudru dns-record apply` is exactly what `sc cloudru dns-record apply` was |

Everything else is the same bus every other plugin speaks. Two consequences worth knowing before you operate this:

- **The rendered command text is a Pulumi input property, stored in state**, and `delete` is executed *from state* at destroy time. So the state of a live stack contains the absolute path of the plugin binary that created it.
- A **version bump therefore shows as a diff**. Measured, not assumed: moving a live record's binary to a different path previews as `~ command:local:Command: (update)` with `1 to update, 1 unchanged` — an update, not a replace. That distinction matters because pulumi-command replaces *create-before-delete*, and on a name+type-keyed DNS record the delete would remove what the create just wrote.

## Install

### Download a release binary

Pre-built binaries for each platform are attached to each GitHub Release:

| GOOS   | GOARCH | Binary filename                      |
|--------|--------|--------------------------------------|
| linux  | amd64  | `sc-plugin-cloudru-linux-amd64`      |
| linux  | arm64  | `sc-plugin-cloudru-linux-arm64`      |
| darwin | arm64  | `sc-plugin-cloudru-darwin-arm64`     |

darwin/amd64 (Intel Macs) and Windows are intentionally unsupported.

### Let SC fetch it for you

SC core v2026.10+ resolves plugins directly from a GitHub Release — specify a `source:` block in `plugins:` instead of hand-placing a binary. SC caches the binary under `~/.sc/plugins/<repo>/<version>/` and sha256-verifies on every spawn.

## Configure in `server.yaml`

```yaml
schemaVersion: 1.0

provisioner:
  type: pulumi
  config:
    organization: your-org
    state-storage: { type: s3-bucket, config: {...} }
    secrets-provider: { type: awskms, config: {...} }

plugins:
  cloudru:
    source:
      type: github-release
      repository: simple-container-com/sc-plugin-cloudru
      version: v2026.10.25-<short-sha>   # pin a specific release

resources:
  registrar:
    type: cloudru-dns
    config:
      credentials: ${auth:cloudru-evolution}   # the opaque auth blob
      zoneName:  your-zone.example.com
      zoneId:    ${env:CRU_ZONE_ID}            # optional; see below
      recordTtl: 300
      dnsRecords:
        - { name: api, type: A, value: "203.0.113.10" }

  resources:
    staging:
      resources: {}
```

The credential is a Cloud.ru **Evolution** service-account key (`auth` type `cloudru-evolution-key`): a `keyId` + `keySecret` pair plus the `projectId`. It is not interchangeable with the Advanced (ex-SberCloud) access key — Cloud.ru sells two unrelated clouds under one brand and a credential for one does not authenticate against the other. The service account needs **`cdns.admin`** for DNS.

Write both halves of the key **verbatim** into `secrets.yaml`. An `auth:` entry lives in the encrypted store itself, and SC does **not** expand placeholders there, so a `${secret:…}` written in an auth entry reaches Cloud.ru literally. `Validate()` refuses that rather than letting IAM answer `key_id does not match required format` — a message that reads like a revoked key instead of an unexpanded config. (Live-caught 2026-10-24, mid-deploy, after resources had already been created.)

### Cloud.ru Cloud DNS caveats worth reading before you deploy

All measured against the live API, not inferred from docs:

- **The zone is looked up, never created.** A public zone is authoritative for a real domain; creating one from a deploy either collides with the existing delegation or produces a second zone nobody's nameservers point at. Delegate the domain to `evo-cns01.cloud.ru` / `evo-cns02.cloud.ru` first.
- **There is no verification step and no `valid` field.** A zone is authoritative as soon as it exists. An earlier version of this driver gated on a `valid` flag the API does not have, which would have stayed false forever.
- **`zoneId` is optional but worth setting.** A lookup scoped to the wrong `projectId` does not error — it finds no zones, which is indistinguishable from having none.
- **A record is not addressable.** Applying one is a read-modify-write of the zone's *entire* record array, so two records provisioned concurrently each write the array they read and the loser's record vanishes. The registrar serialises every record behind the previous one for exactly this reason; Pulumi runs siblings concurrently by default, and without the chain a deploy reports green having written only one record.
- **Writes are asynchronous.** The zone reads back empty mid-flight; poll `/v1/operations/{id}`.
- **The REST path is `/v1/publicZones`**, on host `https://dns.api.cloud.ru`. `/v1/zones` is a real backend answering `{"code":5,"message":"Not Found"}`. A `403 invalid scopes` almost always means you reached the console BFF (`console.cloud.ru/api/clouddns`) rather than a missing role.
- Record names are **relative to the zone**, types are **lowercase on the wire**, and the apex is the empty string. `proxied` is meaningless here.
- An omitted `ttl` is stored as a literal **0**, not replaced by a platform default — there is no "automatic" sentinel the way Cloudflare's `ttl: 1` is. `recordTtl` therefore always resolves to *some* number.

The driver refuses to touch a zone's own `NS`/`SOA` records or `_acme-challenge` — the first would break delegation, the second renewal.

## Repository layout

```
sc-plugin-cloudru/
  api/                              # Go module — config structs for SC core
    auth.go, auth_evolution.go      # both Cloud.ru auth types (Advanced + Evolution)
    registrar_dns.go                # cloudru-dns registrar config
    container_app.go                # cloudru-container-app template config
    init.go                         # api.RegisterProviderConfig + config readers
    evolution/                      # the Cloud.ru REST client (IAM, DNS, apps, registry)
    go.mod                          # imports: SC's pkg/api, errors, lo. NO pulumi.
  plugin/                           # Go module — the plugin binary
    cmd/sc-plugin-cloudru/
      main.go                       # SCCloudPluginServer impl + the CLI entrypoint
    internal/
      registrar.go                  # lifted driver — the Pulumi DNS registrar
      scripts.go                    # renders what a local.Command executes
      provider.go                   # lifted pulumi-command provider (for later slices)
      cli/                          # lifted `sc cloudru …` cobra tree
    go.mod                          # imports: pulumi-command SDK + SC plugin runtime
  LICENSE                           # MIT
  CODEOWNERS
  README.md                         # this file
  .github/workflows/release.yml     # auto-release on push to main (CalVer)
```

**Two Go modules.** SC core imports `/api` at build time to register the `cloudru*` types in its config-tier map and generate schemas; the `/plugin` module never crosses that boundary. Note that `evolution/` lives under `/api` rather than under `plugin/internal/` — not an accident: `/api`'s own `registrar_dns.go` and `container_app.go` import it, and an api module cannot import a plugin module's `internal`. It has zero SC-core dependencies, so this costs core nothing.

## A trap this plugin found, for anyone extending it

A Pulumi `ProviderResource` **does not cross `pulumi.Context` boundaries**, and every plugin RPC opens its own short-lived context. Building a provider in `ProvisionRegistrar` and handing it to the registrar — which is cached on the plugin struct and used from `NewDnsRecord`'s *different* context — waits on Outputs owned by a context that has already closed. They never resolve.

**The symptom is not an error.** `sc provision` logs `creating A record …` and then hangs with the stack still at `create` until the execution timeout, naming neither the provider nor the plugin. Live-caught on this plugin's first smoke, 2026-10-25.

So: the registrar passes **no** provider, which is correct — a `local.Command` needs none, and the engine auto-boots `pulumi:providers:command::default_1_2_1` from the `PulumiPlugins` requirement in `Schema`. A later slice that does need a real provider (the Container App) must build it inside the handler that registers the resource, named by SC core's own formula `<stack>--<resType>--<resName>--<env>--provider`, so the URN stays byte-identical to a pre-split deploy. See `sc-plugin-yandex` `d6f7397` and api `787fb0fe`.

## Compatibility

| sc-plugin-cloudru | SC core (`simple-container-com/api`) | Pulumi SDK | pulumi-command |
|---|---|---|---|
| v0.0.x (initial) | `v0.0.0-20261025132958-787fb0fe2efd` or later | sdk/v3 v3.245.0 | v1.2.1 |

SC core's `pulumi/sdk/v3` version MUST match the plugin's — a mismatch breaks the shared monitor gRPC protocol. Bumping `pulumi-command` past v1.2.1 warrants re-measuring the update-vs-replace behaviour above; that provider already does not honour its own documentation about `triggers`.

## Status

The registrar slice was smoked end to end against a live Cloud.ru Evolution zone on 2026-10-25: provision wrote a publicly-resolvable A record, a re-provision was a clean no-op, and destroy removed it from both the authoritative nameservers and the zone API.

## License

[MIT](./LICENSE).
