# Benchmark: Materialize against git

What it measures: the time to put the same paths of the same commit on disk,
through `forge-agentd` (Materialize) and through git, on a real repository.
`cmd/forge-bench` runs it.

## Subject

- Repository `go-vcs/forge`, commit `4804a9911cfeadbedeaf0f6be9a1a6979016958b`
  (a real, signed squash-merge commit, the one of agentd#10).
- Paths `cmd`, `proto`, `go.mod`: every variant checks out exactly these.

## Variants

| variant | what runs |
|---|---|
| `agentd-cold` | a fresh `forge-agentd` over an empty cache, then `forge-agent materialize` |
| `agentd-warm` | the same daemon again (memory and disk cache warm) |
| `agentd-disk` | the daemon restarted over the filled disk cache (memory empty) |
| `git-full` | `git clone --no-checkout`, then `git checkout <commit> -- <paths>` |
| `git-sparse` | `git clone --filter=blob:none --sparse --no-checkout`, `sparse-checkout set --no-cone`, `git checkout <commit>` |

## Method

- One core: the daemon, the client and git run with `GOMAXPROCS=1`,
  `pack.threads=1`, `index.threads=1`, `checkout.workers=1`. macOS has no
  `taskset`, so this limits Go and git threads, not the scheduler.
- The user's git configuration is out of the way (`GIT_CONFIG_GLOBAL=/dev/null`,
  `GIT_CONFIG_NOSYSTEM=1`).
- Rounds are interleaved: each round runs the three groups (agentd, git-full,
  git-sparse), starting from a different one each time. At least 5 rounds.
- `uptime` is read before each round. A round whose 1-minute or 5-minute load is
  above 8 is dropped; after two dropped rounds in a row the run stops and says
  so. The load after each round is recorded too.
- Requests and bytes are counted by loopback reverse proxies in front of
  `api.github.com` and `github.com`. The daemon is started with
  `--api-url` and `--git-url` pointing at them, and git clones through the git
  one, so both sides are counted the same way: response body bytes as they
  crossed the wire (compression left as negotiated). Counting git by
  `GIT_TRACE_CURL` does not work: `GIT_TRACE_CURL_NO_DATA` drops every
  "Recv data" line, which is why the first harness read 0 bytes.
- The checked-out tree is walked after each sample (file count and bytes,
  `.git` left out): every variant must produce the same tree, and a row whose
  samples differ is marked INCONSISTENT.
- Median and spread (min-max) are reported, not the mean.
- Nothing else runs on the machine during a measurement.

## Run

```
forge-bench --agentd forge-agentd --agent forge-agent --token-file <file> \
  --repo go-vcs/forge --commit 4804a9911cfeadbedeaf0f6be9a1a6979016958b \
  --path cmd --path proto --path go.mod \
  --work <durable dir> --out result.json --rounds 5
```

`forge-agentd` and `forge-agent` are built from a clean clone of the release tag
under test.

## Results

not measured yet (load > 8)

| variant | rounds | median s | min-max s | requests | bytes | files | tree bytes |
|---|---|---|---|---|---|---|---|
| agentd-cold | | | | | | | |
| agentd-warm | | | | | | | |
| agentd-disk | | | | | | | |
| git-full | | | | | | | |
| git-sparse | | | | | | | |
