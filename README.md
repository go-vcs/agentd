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

## A0 status

Done: verdicts, wait with one shared poller per commit, gated merge, idempotent
writes, session identities, peer-checked socket, structured errors, fuzzed
parsers. Not done: verified object cache, `Materialize`, push event stream
(the hub polls), keychain credential option. Leases, changes and change sets
answer `UNSUPPORTED_BY_BACKEND` (milestone A3 and the forge backend, A4).

The verdict logic in `internal/verdict` mirrors prwait's; extracting prwait's
into an importable package would remove the duplication.
