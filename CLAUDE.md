# sc-plugin-cloudru — notes for AI coding agents

If you're working here, you've been asked to extend the Cloud.ru plugin for Simple Container. The context you need in one page:

## What this repo is

The **third** out-of-tree SC cloud driver (after `sc-plugin-cloudflare` and `sc-plugin-yandex`), following the pattern defined in [forge/docs/design/2026-10-24/sc-plugin-architecture](https://github.com/simple-container-com/forge/tree/main/docs/design/2026-10-24/sc-plugin-architecture). Lifted out of the monolith's `pkg/clouds/cloudru`, `pkg/clouds/pulumi/cloudru` and `pkg/cmd/cmd_cloudru`.

Two Go modules:
- `/api` — config structs. Imports SC core's `pkg/api` only, no pulumi. SC core imports THIS module at build time to validate `type: cloudru-*` configs and generate schemas.
- `/plugin` — the subprocess binary. Imports SC core's plugin runtime AND the `pulumi-command` SDK. SC core NEVER imports this module.

One deviation from the other two plugins that is deliberate and will look wrong if you don't know why: **`api/evolution/` (the Cloud.ru REST client) lives under `/api`, not under `plugin/internal/`.** `/api`'s own `registrar_dns.go` and `container_app.go` import it, and an api module cannot reach into a plugin module's `internal`. It carries no SC-core dependencies, so core pays nothing for it.

## Read this before you change anything: the binary has two modes

Cloud.ru's resources exist in **no** Terraform or Pulumi provider — not even in Cloud.ru's own `evo-terraform` — and Pulumi's Go SDK has no dynamic providers. The only way to get Cloud DNS or a Container App inside Pulumi's resource model is a pulumi-command `local.Command` shelling out to a CLI that makes the REST calls. In-tree that CLI was `sc cloudru …`.

Extraction broke both halves of that: a core `sc` has no `cloudru` verbs any more, and this plugin's binary is not on any `PATH` — it lives in `~/.sc/plugins/<repo>/<version>/`. So the one released binary is both:

| invocation | mode |
|---|---|
| no args | the `hashicorp/go-plugin` gRPC server; SC core launches it, it registers `command:local:Command` against core's shared monitor |
| any args | the lifted `cloudru` cobra tree (`plugin/internal/cli/`) — `sc-plugin-cloudru cloudru dns-record apply` IS `sc cloudru dns-record apply` |

`plugin/internal/scripts.go` renders what the `local.Command` executes: `os.Executable()` by **absolute path**, plus the verb. Consequences that make this file the most dangerous one in the repo (and why it has its own CODEOWNERS line):

- The command text is a Pulumi **input property**, so it is **persisted in the operator's state**, and `delete` is executed *from state* at destroy time. A release that builds a working plugin but a broken CLI mode breaks destroy on a stack deployed from an *earlier, healthy* release. That is what the `Verify CLI mode is reachable` step in `release.yml` exists for — keep it, and keep the string it greps in sync with the cobra `Short`.
- A plugin **version bump changes that path**, hence a diff. Measured 2026-10-25, not assumed: `~ command:local:Command: (update)` / `1 to update, 1 unchanged`. An **update, not a replace** — which is the outcome that matters, because pulumi-command replaces *create-before-delete* and on a name+type-keyed DNS record the delete would remove what the create just wrote. Re-measure if `pulumi-command` is ever bumped past v1.2.1; that provider already does not honour its own documentation about `triggers`.

## V1 scope

**Shipped** (smoked end to end against a live Evolution zone, 2026-10-25 — provision wrote a publicly-resolvable A record, re-provision was a clean no-op, destroy removed it from both the authoritative NS and the zone API):
- Registrar surface (`type: cloudru-dns`). `ProvisionRegistrar`, `NewDnsRecord`, `NewOverrideHeaderRule`, `ProvisionDomainForEndpoint` all wired through the plugin bus with the `waitForRef` pattern that makes Pulumi's async `RegisterResource` effectively synchronous plugin-side. Return before the outputs resolve and `defer pctx.Close()` silently drops the registration.
- The last two are **deliberate refusals**, not stubs: Cloud.ru has no programmable edge, and a custom domain on a Container App needs a certificate id SC does not manage. They're wired so the operator sees the driver's own explanation rather than a gRPC `Unimplemented` that reads like a version mismatch.
- `LogStream` with progress events tagged by phase (`provision-registrar`, `dns-record`, `worker-route`, `endpoint`).

**Lifted but NOT advertised in `Schema`**:
- `cloudru-container-app` (Provision RPC). The config struct is in `api/container_app.go` and the Evolution client half is in `api/evolution/container_app.go`, so a `server.yaml` naming it parses today — only the provisioning half is unclaimed. Advertising a type this binary cannot provision would make core route the resource here and fail at *dispatch* instead of at config parse.
- The Evolution VM (`api/evolution/compute*.go`, `plugin/internal/cli/compute_vm.go`). Needs the `VmHosts` seam on the bus, which does not exist yet — see Phase 1d in the design doc.
- Artifact Registry (`api/evolution/registry.go`, `plugin/internal/cli/registry.go`).

`plugin/internal/provider.go` (`Provider()` → `command.NewProvider`) is lifted and compiled but **unused by the registrar**. Read the next section before you reach for it.

## The trap that cost this plugin its first smoke

A Pulumi `ProviderResource` **does not cross `pulumi.Context` boundaries.** Each plugin RPC opens its own short-lived pctx and closes it on return (`pulumiContextFromHeader` + `defer done()`). A provider built in `ProvisionRegistrar`'s pctx has URN and ID Outputs owned by that now-closed context; the registrar is cached on `p`, so each record is registered from `NewDnsRecord`'s *different* pctx, and `sdk.Provider(thatHandle)` waits on Outputs that can never resolve.

**The symptom is not an error.** `sc provision` logs `creating A record …` and then hangs with the stack still at `create` until the execution timeout, naming neither the provider nor the plugin. Live-caught 2026-10-25.

So `ProvisionRegistrar` passes **no** Provider, which is both correct and sufficient: a `local.Command` needs none (`recordOptions()` in `plugin/internal/registrar.go` omits the option when there is none, pinned by `TestRecordOptionsOmitsANilProvider`), it is what the in-tree driver got from `initRegistrar`, and the engine auto-boots `pulumi:providers:command::default_1_2_1` from the `PulumiPlugins` requirement in `Schema`. `sc-plugin-cloudflare` has always had this shape.

**A later slice that genuinely needs a provider** (the Container App) must build it inside the handler that registers the resource, in that handler's own pctx, and name it with SC core's own formula — `<stack>--<resType>--<resName>--<env>--provider` — because the lifted driver then appends `--<env>` through `input.ToResName` and the URN must come out byte-identical to a pre-split deploy. Core already cooperates: api `787fb0fe` added `pApi.PluginResourceTypes` / `RegisterPluginType` / `IsPluginType` and has `getProvisionParams` skip its own `initProvider` for any resource type a plugin advertises, so plugin-owned providers are the supported shape rather than a workaround. That api pin in both `go.mod`s is what buys this. Prior art: `sc-plugin-yandex` V6 (`d6f7397`).

## Known Cloud.ru quirks worth re-reading

All measured against the live API. The detail is in `README.md` §"Cloud.ru Cloud DNS caveats"; the headlines:

- **Cloud.ru is two unrelated clouds under one brand.** Evolution (what this plugin targets) and Advanced/ex-SberCloud. Credentials are not interchangeable, and the docs rarely say which estate a page is about — two agreeing pages are often one source twice. Prefer what the resource says about itself: Evolution zones serve `evo-cns01/02.cloud.ru`, not the documented `cdns-ns01/02.sbercloud.ru`.
- **The DNS REST path is `/v1/publicZones` on `https://dns.api.cloud.ru`.** `/v1/zones` is a real backend answering `{"code":5,"message":"Not Found"}`, and a `403 invalid scopes` almost always means you reached the console BFF rather than that you lack the `cdns.admin` role.
- **A record is not addressable.** Applying one is a read-modify-write of the zone's whole record array, so concurrent records clobber each other — the registrar chains every record behind the previous one via `DependsOn` for exactly this reason, because Pulumi runs siblings concurrently and without the chain a deploy reports green having written one record.
- **Writes are async and the zone reads back empty mid-flight.** `done: true` does not mean converged; a write inside the settling window silently empties the zone and reports success.
- The zone is **looked up, never created**, there is no `valid` field to gate on, and an omitted `ttl` is stored as a literal `0` — there is no "automatic" sentinel the way Cloudflare's `ttl: 1` is.
- `${…}` placeholders are **not expanded inside an `auth:` entry** (it already lives in the encrypted store), so a `${secret:…}` written there reaches IAM literally and comes back as `key_id does not match required format`, which reads like a revoked key. `Validate()` refuses it instead. Live-caught 2026-10-24, mid-deploy.
- `sc destroy` **has no `--yes` and exits 0 unanswered** — pipe `yes Y |` and verify by cloud API, not by the Pulumi log.

## Local dev loop

```sh
# Build
cd plugin && GOFLAGS=-mod=mod go build -o /tmp/sc-plugin-cloudru-dbg ./cmd/sc-plugin-cloudru

# Point a smoke stack at the local binary
# .sc/stacks/.../server.yaml:
#   plugins:
#     cloudru:
#       path: /tmp/sc-plugin-cloudru-dbg
```

Tests:
```sh
cd plugin && GOFLAGS=-mod=mod go test ./... -count=1
cd api    && GOFLAGS=-mod=mod go test ./... -count=1
```

Exercise the CLI half directly — it needs no Pulumi and no stack:
```sh
/tmp/sc-plugin-cloudru-dbg cloudru dns-record apply --help
```

Keep secrets out of the smoke stack's `server.yaml`: use `${env:}` for the credential blob and the zone id. Both the plugin and the CLI read the Evolution key as a JSON object `{keyId, keySecret, projectId}`.

## Commit discipline

- DCO sign-off required (`git commit -s`).
- Do NOT add `Co-Authored-By` trailers.
- Do NOT paste a `Claude-Session:` or claude.ai link in commits or PR descriptions. This repo's rules match the forge umbrella's.
- Push to `main` triggers an auto-release workflow that cuts a GitHub Release tagged `vYYYY.MM.DD-<short-sha>` with linux/amd64, linux/arm64, darwin/arm64 binaries + `sha256sums.txt`. The `test` job gates `build`, so a red test blocks the release rather than publishing a broken binary that a stack will then pin.
