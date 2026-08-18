# aiproxy — in-app updates

**Date:** 2026-08-18
**Status:** approved, pre-implementation
**Depends on:** the release pipeline in `.github/workflows/release.yml`, whose
asset naming this design consumes.

## 1. Purpose

`aiproxy` ships as a single binary installed by `curl | sh` from a GitHub
release. Nothing about that install path tells the operator when a newer
release exists, and nothing updates it.

This adds two things:

- **Show.** The TUI header says when a newer release exists.
- **Update.** One key in the TUI, or `aiproxy update` from a shell, replaces
  the installed binary with the latest release.

### In scope

- A version check against the project's GitHub releases, on a long cadence,
  cached, opt-out.
- Download, checksum verification, and atomic replacement of the running
  binary.
- Surfacing both through the existing presentation seam so the TUI, the
  control API, and a future dashboard see one answer.
- A `update` subcommand for headless and service installs.

### Out of scope

- Restarting the proxy. A successful update reports "restart to apply" and
  the running process continues serving the old code until the operator
  quits it. Draining in-flight streams to re-exec in place is a separate
  feature with its own failure modes (§14).
- Windows. Renaming over a running `.exe` is not possible there, and the
  release pipeline ships darwin and linux only.
- Codesigning and notarization. Out of scope for the release pipeline, so
  out of scope here.
- Downgrades and channel selection (beta/nightly). One channel: latest
  release.

## 2. Non-negotiable properties

Each has a test (§12).

1. **A failed update never damages the installed binary.** A checksum
   mismatch, a truncated download, a missing asset, or a disappearing
   network leaves the on-disk binary byte-for-byte identical. Verification
   happens before anything is moved into place, never after.
2. **No unverified bytes are ever made executable.** The downloaded archive
   is checked against the release's `checksums.txt` before it is unpacked,
   and the unpacked binary is renamed into place in one step.
3. **An update never disturbs in-flight requests.** Replacement is
   `os.Rename` over the executable's path; the running process keeps its
   open inode. A streamed completion in progress during an update completes
   normally.
4. **The check never blocks the request path or the render path.** It runs
   on its own goroutine on a long cadence and writes to a cache;
   `ServerStatus` reads that cache. No proxied request and no TUI frame ever
   waits on network I/O to github.com.
5. **The check is opt-out, and honours the opt-out.** With
   `update.checkEnabled` false, the process makes no outbound request to
   github.com at all.

## 3. Architecture

```
cmd/aiproxy ──> internal/updater ──> github.com (releases)
     │                  ▲
     │                  │ (read cache)
     └──> internal/view.Local ──> Status.Update
                        ▲
                        │
              internal/tui, control API
```

`internal/updater` is a leaf: it imports the standard library and nothing
from this module. It knows about versions, releases, and files on disk. It
does not know about `view`, `config`, `account`, or the TUI, which is what
lets it be tested end to end against an `httptest.Server` with no proxy
running.

`view.Local` owns the wiring, exactly as it already does for the prober.
`internal/tui` continues to import only `internal/view`
(`TestTUIImportsOnlyTheViewSeam` keeps this honest).

### 3.1 Why availability rides on `Status`

`view.Status` already carries `Probe ProbeStatus`, on the stated grounds that
"a throttled probe must be visible in the UI, not silently stale". Update
availability is the same kind of fact about the running instance, arriving on
the same kind of background cadence, consumed by the same header. It gets the
same treatment: a field on `Status`, not a new read method.

The consequence is that the TUI's existing two-second status poll renders it
for free, with no second poll cycle and no new route. The check's real
cadence is decoupled from the poll entirely, because the poll reads a cache.

## 4. Version identity

The release workflow stamps `main.version` with the tag minus its leading
`v` (`v0.2.0` → `0.2.0`). An unstamped build is `dev`.

`updater.Compare(a, b string) int` parses `major.minor.patch` with an
optional `-prerelease` suffix, ignoring a leading `v` on either side:

- Numeric fields compare numerically, so `0.10.0` > `0.9.0`.
- A prerelease sorts *below* its release: `1.0.0-rc1` < `1.0.0`.
- Anything unparseable compares as lower than anything parseable, and two
  unparseable values compare equal.

This is a bounded comparator for versions this project produces, not a
general semver implementation, and it takes no dependency.

**`dev` is never updated.** A build with no stamped version has no defensible
comparison point, so `Check` returns `ErrDevBuild` (§11) and offers nothing.
The TUI says so rather than showing a misleading "up to date".

## 5. The check

`Check(ctx, current string) (Release, error)`.

Resolution uses the redirect, not the API:

```
GET https://github.com/nicko170/aiproxy/releases/latest
    (redirects suppressed; read the Location header)
→ https://github.com/nicko170/aiproxy/releases/tag/v0.2.0
```

The client sets `CheckRedirect` to `http.ErrUseLastResponse`, so this is one
request, no body, and no redirect chase. This is the same mechanism
`install.sh` uses, chosen for the same reasons: no token, no 60-request/hour
unauthenticated rate limit, and no coupling to the shape of GitHub's release
JSON.

A repository with no releases redirects to `/releases` with no `/tag/`
segment; that is reported as "no releases published", not as an error state
the UI must render red.

`Check` reports what the latest release *is*; it never returns `ErrUpToDate`.
Comparing it against the running version is the caller's job — the `Checker`
for the header, `Apply` for the install path — so the same lookup serves both
without one of them having to interpret an error as a success.

`Release` carries the version, the page URL, and the derived asset and
checksum URLs:

```
https://github.com/nicko170/aiproxy/releases/download/v0.2.0/aiproxy_0.2.0_darwin_arm64.tar.gz
https://github.com/nicko170/aiproxy/releases/download/v0.2.0/checksums.txt
```

`runtime.GOOS`/`runtime.GOARCH` name the asset. That string is constructed
identically in `install.sh` and in the release workflow; the workflow carries
a comment saying so.

### 5.1 The background checker

`updater.Checker` has the same lifecycle shape as `metrics.Roller`,
`metrics.Pruner`, and `prober.Prober`: constructed in `buildHandler`,
`Start()`/`Stop()` from `run()`.

- One check on start, after a short delay so it never competes with the
  proxy's own startup, then every `update.checkIntervalHours`.
- The result — or the error — is cached behind a mutex. Readers get the last
  known answer instantly and always.
- A failed check does not clear a previous good answer; it records the error
  alongside it. A transient network failure must not make an available
  update disappear from the header.
- `checkEnabled == false` means `Start()` starts nothing. Property 5.

## 6. The apply

`Apply(ctx, rel Release) (Result, error)`, in this order:

0. Refuse `ErrDevBuild` immediately if the current version is `dev`. Nothing
   below runs: no request, no temp file.
1. `os.Executable()`, then `filepath.EvalSymlinks` — update what is actually
   running, not a symlink pointing at it.
2. `dir := filepath.Dir(exe)`. Everything below happens inside `dir`, because
   `os.Rename` is only atomic within one filesystem.
3. **Writability probe.** `os.CreateTemp(dir, ".aiproxy-update-*")`. Failure
   here returns `ErrNotWritable` *before* anything is downloaded, carrying
   the install path so the message can say what to do instead (re-run the
   installer, or use the package manager that owns it). This is how a
   Homebrew-owned or root-owned install is handled — by probing, not by
   guessing from the path.
4. Stream the archive into that temp file, with a size cap.
5. Fetch `checksums.txt` into memory, with a size cap.
6. Compute the archive's sha256; compare against the line naming this asset.
   A mismatch, or an asset absent from the file, aborts here. **Nothing has
   been moved; the installed binary is untouched.** Property 1 and 2.
7. Unpack the `aiproxy` member into a second temp file in `dir`, capping the
   decompressed size and refusing any archive member whose name is not
   exactly `aiproxy`.
8. `chmod` the new file to the existing binary's mode, defaulting to `0755`.
9. `os.Rename(newTmp, exe)`. The running process keeps its inode.
   Property 3.
10. Remove the archive temp.

Every temp file is removed on every error path via `defer`. A crash mid-way
leaves at most a dot-prefixed temp file beside the binary, never a
partially-written `aiproxy`.

`Result` reports the version installed and the path written, so both the TUI
flash and the CLI print the same two facts.

## 7. Seam changes

`view.Status` gains one field:

```go
// Update reports whether a newer release exists, read from the background
// checker's cache (never a live network call — see spec §3.1).
Update UpdateStatus `json:"update"`
```

```go
type UpdateStatus struct {
    CurrentVersion  string `json:"currentVersion"`
    LatestVersion   string `json:"latestVersion,omitempty"`
    Available       bool   `json:"available"`
    ReleaseURL      string `json:"releaseUrl,omitempty"`
    CheckedAt       int64  `json:"checkedAt,omitempty"`
    CheckError      string `json:"checkError,omitempty"`
    Disabled        bool   `json:"disabled"`
    DevBuild        bool   `json:"devBuild"`
}
```

`view.Source` gains exactly one method:

```go
// ApplyUpdate downloads, verifies, and installs the latest release over the
// running binary. It does not restart anything: the caller reports the
// returned version and tells the operator to restart (spec §1).
ApplyUpdate(ctx context.Context) (UpdateResult, error)
```

```go
type UpdateResult struct {
    Updated        bool   `json:"updated"`
    PreviousVersion string `json:"previousVersion"`
    Version        string `json:"version"`
    Path           string `json:"path"`
    // Message is what the TUI flashes and the CLI prints — the one place
    // "restart to apply" is worded, so both say it identically.
    Message string `json:"message"`
}
```

`Updated` is false, with no error, for the two "nothing to do" outcomes:
already latest, and no releases published. Those are states, not failures,
and neither should render red.

Routed at `POST /_aiproxy/api/v1/update`, with its entry added to
`routeFor` in `internal/proxy` so `TestEveryViewSourceMethodHasAControlRoute`
stays green. The route returns 409 when an update is already running, 412
when the build is `dev`, and 403 when the install directory is not writable
— distinguishable states, not one opaque 500.

Concurrent `ApplyUpdate` calls are serialized by a mutex in `view.Local`; the
second caller gets `ErrUpdateInProgress` rather than two downloads racing to
rename over the same path.

## 8. Config and settings

```json
"update": {
  "checkEnabled": true,
  "checkIntervalHours": 24
}
```

Defaults: enabled, 24 hours. `Default()` gains them; an existing config
without the block decodes to the zero value, so `Store.Load` fills unset
fields with the defaults exactly as it already does for other blocks.

Both fields join `view.Settings` and the Settings screen.
`checkEnabled` is live-tunable (it starts or stops the checker's loop);
`checkIntervalHours` is reported under `Applied.NeedsRestart`, matching how
the quota probe interval is already handled.

Opt-out is a first-class setting rather than an env var because the check is
an outbound request that tells github.com the operator's IP and version. The
README says so plainly.

## 9. TUI

**Header.** When `status.Update.Available`, a segment is appended:
`↑ 0.2.0 available`, in the accent colour, degrading to plain `^ 0.2.0
available` under `modeNone` exactly as the lamp glyph already degrades. It
is appended after the existing segments and is subject to the same
width-driven shedding, so a narrow terminal drops it rather than wrapping.

**Applying.** `u` triggers `ApplyUpdate` from any screen, alongside the
existing global `l`, `p`, and `o`. `u` is unbound today, globally and on every
screen. It is a `tea.Cmd` like every other seam
call, so the render path never blocks (§2, property 4). The flash line
reports, in order: `updating…`, then either
`updated to 0.2.0 — restart to apply` or the specific failure. `u` is added
to the Overview footer hints and to the help screen.

Pressing `u` with no update available is a no-op with an explanatory flash,
not an error.

**Settings.** The two new fields render and edit like the existing ones.

## 10. CLI

`aiproxy update` becomes the first subcommand. Dispatch is a check of
`flag.Arg(0)` before the server boots — no subcommand framework, no new
dependency:

- `aiproxy update` — check, and install if newer. Prints the version
  installed and the path written, or the reason it did nothing.
- `aiproxy update --check` — report only; exit 0 when up to date, exit 1
  when an update is available, so a script can branch on it.

It shares `internal/updater` with the TUI path, so there is one
download-and-verify implementation. It does not need a running proxy, does
not open the config store beyond reading `update.checkEnabled`, and never
starts a listener.

Any other first argument is an error naming the valid subcommands, so a typo
does not silently boot a proxy.

## 11. Error handling

`internal/updater` returns typed sentinels, and every layer above maps them
to its own idiom rather than re-deriving the cause from a string:

The HTTP column is `POST /_aiproxy/api/v1/update`. On the check path the same
sentinels never reach HTTP at all: they land in `UpdateStatus.CheckError`, or
in `Available == false`, and `GET /status` stays 200.

| Sentinel | Means | TUI flash | HTTP |
|---|---|---|---|
| `ErrDevBuild` | version is `dev` | "dev build — install a release to update" | 412 |
| `ErrNoReleases` | repo has no releases | "no releases published yet" | 200, `updated:false` |
| `ErrUpToDate` | already latest | "already on 0.2.0" | 200, `updated:false` |
| `ErrNotWritable` | install dir read-only | names the path and the fix | 403 |
| `ErrChecksumMismatch` | asset failed verification | "download failed verification — nothing changed" | 502 |
| `ErrUpdateInProgress` | concurrent apply | "an update is already running" | 409 |

A checksum mismatch is logged at `error` with the expected and actual
digests. It is the one failure that could indicate something other than a
bad day on the network, and it must not be summarized away.

## 12. Testing

`internal/updater`, against an `httptest.Server` serving a genuine gzipped
tar and a genuine `checksums.txt`:

- **Happy path.** A temp "installed binary" is replaced; content and mode are
  asserted afterwards.
- **Checksum mismatch leaves the original byte-identical.** Property 1. The
  same assertion covers a truncated archive and an asset missing from
  `checksums.txt`.
- **Unwritable directory fails before any download**, asserted by the test
  server recording that it was never asked for the asset. Property 1.
- **Archive containing an unexpected member** is refused.
- **Decompressed size cap** is enforced.
- **Same version is a no-op** returning `ErrUpToDate`.
- **Redirect parsing**: a `/tag/v1.2.3` Location, and a `/releases` Location
  meaning no releases.
- **`Compare`** as a table test, including `0.10.0 > 0.9.0`, prerelease
  ordering, and unparseable input.
- **Checker caching**: a failing check after a good one retains the good
  answer and records the error; a disabled checker issues zero requests
  (property 5, asserted by request count on the test server).

`internal/proxy`: the route-mapping test gains `ApplyUpdate`; handler tests
cover the 409/412/403 mappings.

`internal/tui`: a golden frame with an update available at each existing
width, since the header is width-sensitive and this adds a segment; and a
behaviour test that `u` calls `ApplyUpdate` exactly once and renders the
returned version in the flash.

Property 3 (in-flight requests survive) is tested in `internal/updater` at
the level it is true: a file handle opened on the "binary" before the rename
still reads the original bytes afterwards. Asserting it through a live proxy
would test the operating system's rename semantics, not this code.

## 13. Delivery order

1. `internal/updater`: `Compare`, `Check`, `Apply`, `Checker`, with the full
   test suite. Nothing is wired; the package stands alone.
2. Config block, defaults, and the `view.Settings` fields.
3. Seam: `Status.Update`, `ApplyUpdate`, the control route, the route-test
   entry, and the error mapping.
4. `Checker` lifecycle in `run()`, alongside the roller, pruner, and prober.
5. TUI: header segment, `u`, footer and help, golden frames.
6. `aiproxy update` subcommand.
7. README: an Updating section, and the opt-out documented.

Each step ends somewhere the suite is green.

## 14. Deferred, deliberately

- **Restart in place.** Draining in-flight streams and re-execing is a real
  feature with its own timeout and terminal-teardown questions. "Restart to
  apply" is the honest interim.
- **Update channels.** One channel — latest release. Prereleases are
  comparable (§4) but never offered.
- **Signature verification.** `checksums.txt` is fetched over TLS from the
  same origin as the asset, so it defends against corruption and a bad
  mirror, not against a compromised release. Signing needs a key and a place
  to keep it; it slots in at §6 step 6 when there is one.
- **Rollback.** The previous binary is not retained. Reinstalling a known
  version is `AIPROXY_VERSION=0.1.0 curl … | sh`.
