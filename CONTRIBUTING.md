# Contributing

Thanks for helping. Bug reports, measurements on other Macs and pull requests are all welcome.

## Reporting a bug

[Open an issue](https://github.com/piotr-graczyk-dev/agentic-emulator/issues/new/choose) with
the output of `agemu version` and `agemu doctor --json`, the commands or MCP calls that failed, and the relevant lines of `~/.agemu/daemon.log`. A
security problem goes to [SECURITY.md](SECURITY.md) instead.

## Building

You need Go (the version in `go.mod`) and, for each platform you work on, its SDK: on an Apple
Silicon Mac, Xcode or the Command Line Tools for the Swift helper and Xcode with an iOS
Simulator runtime for iOS; the Android SDK for Android, which you can also work on from Linux or
Windows (`agemu doctor` lists what is missing). OS-specific calls go in `internal/osproc` (with a
`_unix.go` and a `_windows.go` side) or `internal/toolchain`, so the rest stays portable.

```sh
make build     # bin/agemu, bin/agemud, bin/agemu-ios-helper, bin/agemu-bench
make test      # go vet + unit tests; no devices needed
```

A daemon that is already running keeps serving the old build after `make build`: run
`bin/agemu daemon restart` (devices keep running), or use a separate state directory with
`AGEMU_HOME=$(mktemp -d)`, which also keeps your work away from a daemon other sessions use.

`make android-build` and the `noios` / `noandroid` build tags compile one platform out. The
on-device Android agent's jar (`internal/android/assets/agemu-agent.jar`) is committed;
`make android-agent` rebuilds it from `android-agent/src` with a JDK and the Android SDK.

## Testing on devices

Unit tests run in CI. Anything that drives a device runs locally:

```sh
make android-e2e    # scripts/e2e-android.sh against an agemu-* emulator
make ios-e2e        # scripts/e2e-ios.sh against agemu-* simulators
```

The scripts only create and boot `agemu-*` devices, but they reset them: the iOS script resets
`agemu-iphone-17-pro`, which uninstalls apps someone else installed on it.

## Pull requests

- One change per pull request, with a description of what it fixes and how you checked it.
- `gofmt`, `go vet ./...` and `go test ./...` must pass; CI runs them on macOS, Linux and
  Windows, and the Android end-to-end test on a Linux KVM emulator.
- Keep the conventions in [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md): device ids with a
  platform prefix, coordinates in screenshot pixels, calls that never hang silently, and
  "not implemented" (exit status 3) instead of a faked capability.
- **Numbers need a measurement.** The README and docs only state speeds, memory and failure
  rates that were measured: give n, the host, its load and the method, and say what the
  baseline was (the standard tool, another agent tool, or an earlier agemu build). Put the
  details in `docs/` and the summary in the README. A change that makes something slower says
  by how much.
- A new CLI command or MCP tool needs its entry in `agemu help`, the README and, for test
  capabilities, [docs/CAPABILITIES.md](docs/CAPABILITIES.md).

## Releasing (maintainers)

Push a tag: `git tag v0.2.0 && git push origin v0.2.0`. The
[release workflow](.github/workflows/release.yml) builds on Apple Silicon (and cross-compiles
Linux and Windows), runs the tests and a smoke test, publishes a GitHub release with
`agemu-<version>-darwin-arm64.tar.gz`, `-linux-amd64.tar.gz`, `-linux-arm64.tar.gz`,
`-windows-amd64.zip` and `SHA256SUMS` (which `install.sh` and `install.ps1` download), and
publishes the npm package `agentic-emulator`, with every platform's binaries, with provenance. A tag with a suffix (`v0.2.0-rc.1`) is a GitHub prerelease and npm's `next`
tag. Running the workflow by hand packages a version without publishing it.

The workflow publishes to npm with trusted publishing: npm trusts `release.yml` in this
repository and authenticates the run over OIDC, so there is no npm token or repository secret
to set up or rotate. If the npm publish step fails, rerun the release job from the tag's run
(Actions > release > Re-run jobs). The rerun reuses the GitHub release and replaces its files,
and the npm step does nothing when that version is already on npm. The manual run is only a
dry run, so it can't be used to retry.

The [MCP Registry](https://registry.modelcontextprotocol.io) lists agemu from
[server.json](server.json). The registry checks that the npm package carries the same `mcpName`,
so publish the entry once the version is on npm, with
[`mcp-publisher`](https://modelcontextprotocol.io/registry/quickstart)
(`brew install mcp-publisher`):

```sh
v=0.2.1 # the version that was just released
dir="$(mktemp -d)" && sed "s/\"version\": \"0.0.0\"/\"version\": \"$v\"/" server.json > "$dir/server.json"
(cd "$dir" && mcp-publisher login github && mcp-publisher publish)
```

`scripts/package.sh <version>` builds the same files locally from `bin/`
(`make build VERSION=<version>` first). After changing `go.mod`, regenerate
`THIRD_PARTY_NOTICES.md` with `scripts/third-party-notices.sh > THIRD_PARTY_NOTICES.md`.

By contributing you agree that your contribution is licensed under the [MIT License](LICENSE).
Please follow the [code of conduct](CODE_OF_CONDUCT.md).
