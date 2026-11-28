# agentd

<img src="https://raw.githubusercontent.com/go-vcs/brand/main/avatar/go-vcs-agentd.png" width="96" alt="go-vcs agentd" align="right">

[![Go Reference](https://pkg.go.dev/badge/github.com/go-vcs/agentd.svg)](https://pkg.go.dev/github.com/go-vcs/agentd)
[![License](https://img.shields.io/badge/license-BSD--3--Clause-blue.svg)](LICENSE)
[![CI](https://github.com/go-vcs/agentd/actions/workflows/ci.yml/badge.svg)](https://github.com/go-vcs/agentd/actions/workflows/ci.yml)
[![Landing](https://img.shields.io/badge/landing-go--vcs.github.io-0079A8.svg)](https://go-vcs.github.io) [![Docs](https://img.shields.io/badge/docs-go--vcs.github.io%2Fdocs-0079A8.svg)](https://go-vcs.github.io/docs/main/)

`forge-agentd` is the local daemon of the go-vcs agent-VCS design
(milestone A0, see `docs/agent-vcs.md` in go-vcs/forge). It sits between many
agent sessions on one machine and GitHub, so that:

- agents never hold a token (the daemon reads one from a file, never from argv,
  environment or URL);
- thirteen agents waiting on one commit cost one upstream poller;
- a verdict is bound to one exact, full commit id, and a merge happens only
  if that commit is still the pull request's head and is green.

`forge-agent` is the small client agents call.

## Usage

    forge-agentd --token-file ~/.github-token          # socket under the user config dir
    forge-agent --label build-7 verdict go-vcs/forge <40-hex-commit>
    forge-agent --label build-7 wait    go-vcs/forge <40-hex-commit> --timeout 20m
    forge-agent --label build-7 events  go-vcs/forge <40-hex-commit> --timeout 30m
    forge-agent --label build-7 merge   go-vcs/forge 6 <40-hex-commit>
    forge-agent --label build-7 blob    go-vcs/forge <40-hex-blob-id> > file
    forge-agent --label build-7 tree    go-vcs/forge <40-hex-tree-id>
    forge-agent --label build-7 materialize go-vcs/forge <40-hex-commit> work/1 --path cmd

Exit codes of `forge-agent`: 0 green / done, 1 error, 3 red or refused by the
gate, 4 pending or wait timed out. Errors are JSON on stderr:
`{"error":{"reason":"REF_MOVED","retryable":false,"current_commit":"..."}}`.
`merge` prints its request id on stderr; reuse it with `--request-id` to retry
safely after a timeout.

## Rules the code enforces

- Object ids are full (40 or 64 lowercase hex). An abbreviated id is refused
  (`ABBREVIATED_ID`), never expanded.
- A run with zero jobs is never green; a null conclusion or an unknown job count
  is pending; observations whose head sha differs from the commit are refused;
  only the newest run per (workflow, event) counts.
- The socket directory is 0700, the socket 0600, and a connection whose peer uid
  is not the daemon's is dropped. Where peer credentials are unavailable the
  daemon refuses to serve.
- The token file must be a regular file, mode 0600, one token, at most 1 KiB.
  `--token-keychain <item>` reads it from the system keychain instead (see
  below); giving both is refused.
- Writes carry a 32-hex request id; the same id with a different body is
  `REQUEST_ID_REUSED`, with the same body it replays the first answer.
- Sessions are per label with their own identity; a change set holds at most 64
  repos.

## Keychain credential

    security add-generic-password -s forge-agentd.github -a "$USER" -w   # macOS, prompts for the token
    forge-agentd --token-keychain forge-agentd.github

The token can live in the system keychain instead of a file: a macOS generic
password, or on Linux a Secret Service item with the attribute
`service=<item>` (read with `secret-tool`). The daemon runs `/usr/bin/security
find-generic-password -s <item> -w` or `/usr/bin/secret-tool lookup service
<item>` by absolute path, so only the item name is on a command line; the secret
comes back on a pipe, never through argv, the environment or a file. The program
gets a minimal environment (HOME, USER, LOGNAME, the session bus address and
XDG_RUNTIME_DIR, nothing else), runs for at most 30 s (a locked keychain may
wait for a person), and its output is read up to 1 KiB, after which it is
killed. The answer must be one token, as for the token file. Its standard error
is dropped and no error text carries what it printed. The item name is 1 to 128
letters, digits, dots, underscores or hyphens, not starting with a hyphen. Other
platforms are refused. Each case has a test run against a helper program in
place of the real keychain.

## Bounds (each has a refusing test)

Body 64 KiB, 64 requests in flight, 64 sessions, 16 waits per session, 256
watched commits, wait at most 1 h, 100000 idempotency rows for 24 h, upstream
reply 8 MiB, 10 pages. Exceeding one gives `BOUND_EXCEEDED` or `RATE_LIMITED`.

## Object cache

`blob` and `tree` read git objects through a cache shared by every session.
An object is admitted only after the daemon has recomputed its id from the
bytes (git's `<kind> <len>\0<content>` hash, SHA-1 or SHA-256 by id length), so
a corrupted or malicious GitHub answer cannot poison it: the answer is
`OBJECT_MISMATCH` and nothing is kept. GitHub's tree JSON is rebuilt into git's
raw tree (a truncated listing is refused), so a tree is verified the same way.
Concurrent misses of one id share one upstream fetch.

Bounds: 256 MiB (`--cache-mb`), 100000 entries (`--cache-entries`), 2 MiB per
object; the least recently used is evicted; an object larger than the bound is
`BOUND_EXCEEDED`. The memory tier is never offered to clones as alternates.
The cache is keyed by id alone: a session that knows the id of an object in a
repository the daemon can read, but another session asked about, can read it
too. All sessions are the same uid and share the one credential.

### Disk tier

A second tier under `--cache-dir` (default `forge-agentd/objects` under the user
cache dir; empty means memory only) lets a restarted daemon, or an object the
memory tier evicted, be served without an upstream request. The directory is
created 0700 and is refused when it is under the temporary directory, relative,
or accessible to group or others; files are 0600, written to a temporary name
and renamed, so a reader sees all of an object or none of it. An object is
recomputed against its id when written and again on every read: a file that was
edited, truncated, swapped for a link or directory, or planted is a miss, is
removed and counted (`Corrupt`), and the upstream is asked again. A file whose
size is over the per-object bound is refused from its size, never read. Bounds:
10 GiB (`--cache-disk-mb`), 1000000 objects, 2 MiB per object; the oldest is
evicted, and at start the directory is indexed, trimmed to the bounds and
cleaned of strays and interrupted writes. A full or failing disk costs a refetch,
never the answer (`WriteErrors` counts it). The record parser is fuzzed
(`FuzzDecodeRecord`, corpus committed); the oversized-file bomb is a test under a
64 MiB memory limit. Like the memory tier it is keyed by id alone.

Windows gap: Go reports every Windows directory's mode bits as 0777, so the
owner-only (0700) refusal is Unix only and is skipped on Windows. The
directory's access control list is not checked there: on Windows the cache
directory's privacy is not verified, it rests on the user's profile directory.

Benchmark row 4 (`BenchmarkVsGitCatFileBatch`, objcache) compares a cache hit
with a long-lived `git cat-file --batch`. A cache hit allocates exactly 0 times
(asserted). Timings are not recorded yet: the benchmark only runs when the
1-minute and 5-minute loads are both at most 8.

## Materialize

    forge-agent --label build-7 materialize go-vcs/forge <40-hex-commit> work/1 --path cmd --path go.mod

Writes a sparse checkout of a commit into the new directory `work/1`, relative
to the daemon's `--materialize-root` (default: `forge-agentd/checkouts` under
the user config dir, created 0700; a root under the temporary directory is
refused, and an empty `--materialize-root` disables the call:
`NOT_PERMITTED`). The result is a plain directory, not a git worktree. Without
`--path` the whole tree is written; a path may name a file or a directory.
Every commit, tree and blob is verified against its id by the object cache
before it is written (GitHub's commit JSON is rebuilt into git's raw commit;
a commit it cannot rebuild exactly, for example one with extra headers, is
refused as `OBJECT_MISMATCH` rather than trusted).

What is refused, with a test each: a destination that exists (`CONFLICT`, the
existing directory is never touched); a destination or path with `..`, an
absolute or empty element, a backslash, or a name of `.git` or `git~1` in any
case; a tree that lists a name twice or holds a `.git` entry; a symbolic link
whose target is absolute, leaves the checkout or reaches `.git`; more than
20000 files, 256 MiB, 20000 trees, depth 32 or 256 paths (`BOUND_EXCEEDED`).
All writes go through an `os.Root` scoped to the new directory, so a link
planted in the root cannot carry a write outside it. A failure removes what
was written. A submodule becomes an empty directory, as in git. At most 4
materializations run at once (`RATE_LIMITED`), each for at most 5 minutes.

Blobs are fetched in bulk: the tree walk collects every file first, then the
blobs are prefetched concurrently, in chunks of 32 ids, by 8 workers
(`Limits.FetchWorkers`, capped at 32; one worker is sequential) through the
object cache, so each blob is still verified against its id once and concurrent
requests for one id share a fetch. The first error cancels the rest and the
destination is removed. The file bound is checked before any blob is fetched,
and the 5-minute limit is unchanged. Trees are still read one REST call each,
and each blob is one call: the gain is overlap, not fewer requests, so a large
checkout costs one API request per object. Timings are not recorded: the
benchmark against `git clone` and sparse checkout only runs when the 1-minute
and 5-minute loads are both at most 8.

## Events

    forge-agent --label build-7 events go-vcs/forge <40-hex-commit> --timeout 30m

prints one JSON line per change of the commit's verdict, then ends on the final
one with the exit code `wait` would give. Behind it, `POST /v1/events` takes a
`since` cursor (empty for the first call) and returns the next observation that
differs from it, with a new `cursor`; `final` marks a verdict that is not
`PENDING`, `timed_out` a quiet call (the cursor is then unchanged).

The stream rides the hub's poll loop, the one `wait` uses: any number of
streams and waiters on one commit cost one stream of upstream reads. GitHub
offers no event feed, so the semantics are level-triggered, not a queue:
observations between two polls coalesce, a slow caller sees the latest state and
never a backlog, and the final verdict is never skipped, whatever cursor the
caller holds. A cursor is the first 16 bytes of the SHA-256 of the observed
verdict, so it needs no state in the daemon and survives a daemon restart.
Streams count against the same 16-per-session bound as waits.

Counted, not timed (benchmark row 6, `TestThirteenWaitersAndThirteenStreamsCostOnePoller`,
400 ms window, 20 ms interval):

| Clients on one commit | Upstream reads through the daemon | Upstream reads, independent pollers |
|---|---|---|
| 13 waiters + 13 event streams | 21 | 520 |

At the default 15 s interval that is 240 reads an hour through the daemon,
against 240 per poller (6240 an hour for these 26) without it.

## A0 status

Done: verdicts, wait with one shared poller per commit, event stream over the
same poller, gated merge, idempotent writes, session identities, peer-checked
socket, structured errors, fuzzed parsers, verified object cache (memory and
disk), `Materialize`. Not done: a true push feed from GitHub (none exists; the
hub polls). Keychain credential option: done (`--token-keychain`). Leases, changes and change sets
answer `UNSUPPORTED_BY_BACKEND` (milestone A3 and the forge backend, A4).

The verdict logic in `internal/verdict` mirrors prwait's; extracting prwait's
into an importable package would remove the duplication.
