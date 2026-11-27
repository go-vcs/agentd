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
- Writes carry a 32-hex request id; the same id with a different body is
  `REQUEST_ID_REUSED`, with the same body it replays the first answer.
- Sessions are per label with their own identity; a change set holds at most 64
  repos.

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
`BOUND_EXCEEDED`. The cache is in memory (the design's 10 GiB default belongs to
a disk-backed cache, not done) and is never offered to clones as alternates.
The cache is keyed by id alone: a session that knows the id of an object in a
repository the daemon can read, but another session asked about, can read it
too. All sessions are the same uid and share the one credential.

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

Known limit: objects are fetched one at a time (one REST call per tree and per
blob); there is no bulk path yet, so a large checkout is slower than
`git clone` and costs one API request per object. Timings are not recorded:
the benchmark against `git clone` and sparse checkout only runs when the
1-minute and 5-minute loads are both at most 8.

## A0 status

Done: verdicts, wait with one shared poller per commit, gated merge, idempotent
writes, session identities, peer-checked socket, structured errors, fuzzed
parsers, verified object cache, `Materialize`. Not done: push event stream
(the hub polls), keychain credential option. Leases, changes and change sets
answer `UNSUPPORTED_BY_BACKEND` (milestone A3 and the forge backend, A4).

The verdict logic in `internal/verdict` mirrors prwait's; extracting prwait's
into an importable package would remove the duplication.
