package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// config is everything one measurement needs.
type config struct {
	agentd, agent string // binaries, built from the release tag
	repo, commit  string
	paths         []string
	tokenFile     string // read by the daemon; never printed
	work          string // durable working directory
	out           string // JSON result file
	api, gitURL   string
	rounds        int
	maxLoad       float64
	retryWait     time.Duration
}

var variantOrder = []string{"agentd-cold", "agentd-warm", "agentd-disk", "git-full", "git-sparse"}

func rank(v string) int {
	for i, x := range variantOrder {
		if x == v {
			return i
		}
	}
	return len(variantOrder)
}

var commitRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

func (c config) validate() error {
	switch {
	case c.rounds < 5:
		return errors.New("at least 5 rounds are measured")
	case len(c.paths) == 0:
		return errors.New("at least one --path is needed: the same paths are checked out by every variant")
	case !commitRE.MatchString(c.commit):
		return errors.New("--commit must be a full lowercase 40-hex commit id")
	case strings.Count(c.repo, "/") != 1:
		return errors.New("--repo must be owner/name")
	case !filepath.IsAbs(c.work):
		return errors.New("--work must be an absolute, durable directory")
	}
	for _, f := range []string{c.agentd, c.agent, c.tokenFile} {
		if _, err := os.Stat(f); err != nil {
			return err
		}
	}
	return nil
}

// tail keeps the last part of a command's output for an error message.
func tail(s string) string {
	if len(s) > 400 {
		return "..." + s[len(s)-400:]
	}
	return s
}

// timed runs cmd to completion and returns the wall time it took.
func timed(cmd *exec.Cmd) (time.Duration, error) {
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	t := time.Now()
	err := cmd.Run()
	d := time.Since(t)
	if err != nil {
		return d, fmt.Errorf("%s: %w: %s", filepath.Base(cmd.Path), err, tail(buf.String()))
	}
	return d, nil
}

func readLoad() (l1, l5 float64, err error) {
	cmd := exec.Command("uptime")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	b, err := cmd.Output()
	if err != nil {
		return 0, 0, err
	}
	l1, l5, _, err = parseLoad(string(b))
	return l1, l5, err
}

type bench struct {
	c  config
	p  *proxy // in front of the REST API
	g  *proxy // in front of the git host
	ew io.Writer
}

// cloneURL is where git clones from: the counting proxy of the git host, so
// that git's requests and bytes are counted the way the daemon's are.
func (b *bench) cloneURL() string { return b.g.url + "/" + b.c.repo + ".git" }

func (b *bench) resetCounts() { b.p.reset(); b.g.reset() }

// upstream is what the daemon asked of GitHub since the last reset: the REST
// API and the git host together.
func (b *bench) upstream() (int, int64) {
	r1, b1 := b.p.snapshot()
	r2, b2 := b.g.snapshot()
	return r1 + r2, b1 + b2
}

type daemon struct {
	cmd *exec.Cmd
	eb  *bytes.Buffer
	// done is closed once the process has exited and been waited for.
	done chan struct{}
}

// How long the daemon is given to listen, and to stop on SIGINT before it is
// killed; variables so that a test can shorten them.
var (
	listenTimeout = 20 * time.Second
	stopGrace     = 10 * time.Second
)

func (d *daemon) stop() {
	_ = d.cmd.Process.Signal(os.Interrupt)
	select {
	case <-d.done:
	case <-time.After(stopGrace):
		_ = d.cmd.Process.Kill()
		<-d.done
	}
}

// startDaemon starts forge-agentd on one core with the given cache directory,
// its upstream being the counting proxy, and returns once it listens.
func (b *bench) startDaemon(sock, cache, mat string) (*daemon, error) {
	cmd := exec.Command(b.c.agentd, "--socket", sock, "--token-file", b.c.tokenFile, "--api-url", b.p.url, "--git-url", b.g.url,
		"--cache-dir", cache, "--materialize-root", mat)
	cmd.Env = append(os.Environ(), "GOMAXPROCS=1")
	pr, pw := io.Pipe()
	d := &daemon{cmd: cmd, eb: &bytes.Buffer{}, done: make(chan struct{})}
	cmd.Stderr, cmd.Stdout = d.eb, pw
	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		return nil, err
	}
	go func() { _ = cmd.Wait(); _ = pw.Close(); close(d.done) }()
	ready := make(chan error, 1)
	go func() {
		br := bufio.NewReader(pr)
		line, err := br.ReadString('\n')
		if err == nil && !strings.Contains(line, "listening") {
			err = fmt.Errorf("unexpected first line %q", line)
		}
		ready <- err
		_, _ = io.Copy(io.Discard, br) // never leave the daemon blocked on a full pipe
	}()
	select {
	case err := <-ready:
		if err != nil {
			d.stop()
			return nil, fmt.Errorf("forge-agentd did not start: %w: %s", err, tail(d.eb.String()))
		}
	case <-time.After(listenTimeout):
		d.stop()
		return nil, fmt.Errorf("forge-agentd did not listen within %s", listenTimeout)
	}
	return d, nil
}

// materialize asks the daemon for a checkout of the paths into dir (relative
// to its materialize root), through the client binary.
func (b *bench) materialize(sock, dir string) (time.Duration, error) {
	args := []string{"--socket", sock, "--label", "bench", "materialize", b.c.repo, b.c.commit, dir}
	for _, p := range b.c.paths {
		args = append(args, "--path", p)
	}
	cmd := exec.Command(b.c.agent, args...)
	cmd.Env = append(os.Environ(), "GOMAXPROCS=1")
	return timed(cmd)
}

func (b *bench) agentdSample(round int, variant, sock, mat, name string) (sample, error) {
	b.resetCounts()
	d, err := b.materialize(sock, name)
	if err != nil {
		return sample{}, err
	}
	reqs, by := b.upstream()
	files, fb, err := walkTree(filepath.Join(mat, name))
	if err != nil {
		return sample{}, err
	}
	return sample{Round: round, Variant: variant, Seconds: d.Seconds(), Requests: reqs, Bytes: by, Files: files, FileBytes: fb}, nil
}

// agentdGroup is the three agentd variants of one round: a cold daemon over an
// empty cache, the same daemon again (memory and disk warm), then a restarted
// daemon over the filled disk cache (memory empty).
func (b *bench) agentdGroup(round int, dir string) ([]sample, error) {
	sock := filepath.Join(dir, "s", "d.sock")
	cache, mat := filepath.Join(dir, "cache"), filepath.Join(dir, "mat")
	d, err := b.startDaemon(sock, cache, mat)
	if err != nil {
		return nil, err
	}
	var out []sample
	for _, v := range []struct{ variant, name string }{{"agentd-cold", "cold"}, {"agentd-warm", "warm"}} {
		s, err := b.agentdSample(round, v.variant, sock, mat, v.name)
		if err != nil {
			d.stop()
			return nil, err
		}
		out = append(out, s)
	}
	d.stop()
	d, err = b.startDaemon(sock, cache, mat)
	if err != nil {
		return nil, err
	}
	defer d.stop()
	s, err := b.agentdSample(round, "agentd-disk", sock, mat, "disk")
	if err != nil {
		return nil, err
	}
	return append(out, s), nil
}

var gitTune = []string{"-c", "pack.threads=1", "-c", "index.threads=1", "-c", "checkout.workers=1", "-c", "core.preloadindex=false"}

// gitVariant runs the steps in order, as one timed whole, with the user's git
// configuration out of the way, then counts the tree. Its requests and bytes
// are read off the git proxy.
func (b *bench) gitVariant(round int, variant, dest string, steps [][]string) (sample, error) {
	env := append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	b.resetCounts()
	var total time.Duration
	for _, args := range steps {
		cmd := exec.Command("git", append(append([]string{}, gitTune...), args...)...)
		cmd.Env = env
		d, err := timed(cmd)
		if err != nil {
			return sample{}, err
		}
		total += d
	}
	reqs, by := b.upstream()
	files, fb, err := walkTree(dest)
	if err != nil {
		return sample{}, err
	}
	return sample{Round: round, Variant: variant, Seconds: total.Seconds(), Requests: reqs, Bytes: by, Files: files, FileBytes: fb}, nil
}

func (b *bench) gitFull(round int, dir string) ([]sample, error) {
	dest := filepath.Join(dir, "git-full")
	steps := [][]string{
		{"clone", "--no-checkout", b.cloneURL(), dest},
		append([]string{"-C", dest, "checkout", b.c.commit, "--"}, b.c.paths...),
	}
	s, err := b.gitVariant(round, "git-full", dest, steps)
	return []sample{s}, err
}

func (b *bench) gitSparse(round int, dir string) ([]sample, error) {
	dest := filepath.Join(dir, "git-sparse")
	steps := [][]string{
		{"clone", "--filter=blob:none", "--sparse", "--no-checkout", b.cloneURL(), dest},
		append([]string{"-C", dest, "sparse-checkout", "set", "--no-cone"}, sparsePatterns(b.c.paths)...),
		{"-C", dest, "checkout", b.c.commit},
	}
	s, err := b.gitVariant(round, "git-sparse", dest, steps)
	return []sample{s}, err
}

// round runs the three groups once, starting from a different one each round,
// so that no variant always follows the same neighbour.
func (b *bench) round(r int) ([]sample, error) {
	dir := filepath.Join(b.c.work, fmt.Sprintf("r%d", r))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	groups := []func() ([]sample, error){
		func() ([]sample, error) { return b.agentdGroup(r, dir) },
		func() ([]sample, error) { return b.gitFull(r, dir) },
		func() ([]sample, error) { return b.gitSparse(r, dir) },
	}
	var out []sample
	for i := range groups {
		ss, err := groups[(i+r-1)%len(groups)]()
		if err != nil {
			return nil, err
		}
		out = append(out, ss...)
	}
	return out, nil
}

type result struct {
	Repo    string    `json:"repo"`
	Commit  string    `json:"commit"`
	Paths   []string  `json:"paths"`
	Samples []sample  `json:"samples"`
	Loads   []loadRow `json:"loads"`
}

// runBench measures until c.rounds rounds ran under the load bound, or two
// rounds in a row were dropped. It always reports what it has.
func runBench(w, ew io.Writer, c config) error {
	if err := c.validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(c.work, 0o700); err != nil {
		return err
	}
	p, err := startProxy(c.api)
	if err != nil {
		return err
	}
	defer p.close()
	gp, err := startProxy(c.gitURL)
	if err != nil {
		return err
	}
	defer gp.close()
	b := &bench{c: c, p: p, g: gp, ew: ew}
	g := &gate{max: c.maxLoad}
	res := result{Repo: c.repo, Commit: c.commit, Paths: c.paths}
	valid := 0
	var stopped bool
	for attempt := 1; valid < c.rounds; attempt++ {
		l1, l5, err := readLoad()
		if err != nil {
			return fmt.Errorf("the load could not be read, so no round is run: %w", err)
		}
		ok, stop := g.admit(l1, l5)
		row := loadRow{Attempt: attempt, Load1: l1, Load5: l5, Dropped: !ok}
		if !ok {
			res.Loads = append(res.Loads, row)
			fmt.Fprintf(ew, "attempt %d dropped: load %.2f / %.2f\n", attempt, l1, l5)
			if stop {
				stopped = true
				break
			}
			time.Sleep(c.retryWait)
			continue
		}
		valid++
		row.Round = valid
		fmt.Fprintf(ew, "round %d (attempt %d): load %.2f / %.2f\n", valid, attempt, l1, l5)
		ss, err := b.round(valid)
		if err != nil {
			return fmt.Errorf("round %d: %w", valid, err)
		}
		res.Samples = append(res.Samples, ss...)
		row.After1, row.After5, _ = readLoad()
		res.Loads = append(res.Loads, row)
	}
	rows := summarize(res.Samples)
	sort.SliceStable(rows, func(i, j int) bool { return rank(rows[i].Variant) < rank(rows[j].Variant) })
	writeTable(w, rows, res.Loads)
	if c.out != "" {
		js, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(c.out, append(js, '\n'), 0o644); err != nil {
			return err
		}
	}
	if stopped {
		return fmt.Errorf("stopped after %d of %d rounds: two rounds in a row ran above the load bound", valid, c.rounds)
	}
	return nil
}
