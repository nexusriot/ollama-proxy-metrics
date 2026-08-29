// Package upstream tracks the health and model inventory of the Ollama servers
// behind the proxy, and orders them for each request.
//
// Plain round-robin is blind in two ways that matter. An upstream that has not
// pulled the requested model still receives its share of the traffic and answers
// 404, and an upstream that has started failing keeps receiving traffic until
// someone notices. The pool fixes both: it polls each upstream's /api/tags to
// learn which models it actually serves, and it opens a short circuit on one
// that fails repeatedly so the next request skips it.
//
// Ordering is a preference, never a restriction — every upstream stays in the
// returned candidate list, just further down it. A stale inventory or a wrongly
// tripped circuit therefore degrades to the old round-robin behaviour instead of
// failing the request.
package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nexusriot/ollama-proxy-metrics/internal/modelname"
)

// Defaults applied when Options leaves a field zero.
const (
	DefaultPollInterval     = 30 * time.Second
	DefaultFailureThreshold = 3
	DefaultCooldown         = 30 * time.Second
	pollTimeout             = 5 * time.Second
)

// Options configures a Pool.
type Options struct {
	// URLs are the upstream base URLs, in the order given on the command line.
	URLs []*url.URL
	// PollInterval is how often the model inventory is refreshed. Zero disables
	// polling, leaving the pool as round-robin plus the circuit breaker.
	PollInterval time.Duration
	// FailureThreshold is how many consecutive failures open an upstream's circuit.
	FailureThreshold int
	// Cooldown is how long a circuit stays open.
	Cooldown time.Duration
	// Client performs the inventory polls. Defaults to a short-timeout client.
	Client *http.Client
	// Logger receives health transitions. Optional.
	Logger *slog.Logger
	// OnRefresh, when set, is called with a fresh snapshot after every poll round.
	OnRefresh func([]State)
}

// State is a point-in-time view of one upstream, as reported by Snapshot.
type State struct {
	URL         string    `json:"url"`
	Up          bool      `json:"up"`
	Polled      bool      `json:"polled"`
	Models      []string  `json:"models"`
	Failures    int       `json:"failures"`
	CircuitOpen bool      `json:"circuit_open"`
	OpenUntil   time.Time `json:"open_until,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
}

// node is one upstream's mutable state.
type node struct {
	url *url.URL
	// up starts optimistic: an upstream is assumed healthy until a poll or a
	// real request proves otherwise, so a pool with polling disabled behaves
	// exactly like the round-robin it replaced.
	up        bool
	polled    bool
	models    map[string]struct{}
	failures  int
	openUntil time.Time
	lastErr   string
}

// Pool orders upstreams per request and tracks their health.
type Pool struct {
	nodes     []*node
	threshold int
	cooldown  time.Duration
	interval  time.Duration
	client    *http.Client
	logger    *slog.Logger
	onRefresh func([]State)
	now       func() time.Time

	rr atomic.Uint64
	mu sync.RWMutex
}

// New creates a Pool over opts.URLs, which must not be empty.
func New(opts Options) *Pool {
	p := &Pool{
		threshold: opts.FailureThreshold,
		cooldown:  opts.Cooldown,
		interval:  opts.PollInterval,
		client:    opts.Client,
		logger:    opts.Logger,
		onRefresh: opts.OnRefresh,
		now:       time.Now,
	}
	if p.threshold <= 0 {
		p.threshold = DefaultFailureThreshold
	}
	if p.cooldown <= 0 {
		p.cooldown = DefaultCooldown
	}
	if p.client == nil {
		p.client = &http.Client{Timeout: pollTimeout}
	}
	if p.logger == nil {
		p.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	for _, u := range opts.URLs {
		p.nodes = append(p.nodes, &node{url: u, up: true})
	}
	return p
}

// URLs returns the configured upstreams in their original order.
func (p *Pool) URLs() []*url.URL {
	out := make([]*url.URL, 0, len(p.nodes))
	for _, n := range p.nodes {
		out = append(out, n.url)
	}
	return out
}

// Start refreshes the model inventory immediately and then on every tick until
// ctx is cancelled. It is a no-op when polling is disabled.
func (p *Pool) Start(ctx context.Context) {
	if p.interval <= 0 {
		return
	}
	go func() {
		p.Refresh(ctx)
		ticker := time.NewTicker(p.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				p.Refresh(ctx)
			}
		}
	}()
}

// Refresh polls every upstream's model inventory once, in parallel.
func (p *Pool) Refresh(ctx context.Context) {
	var wg sync.WaitGroup
	for _, n := range p.nodes {
		wg.Add(1)
		go func(n *node) {
			defer wg.Done()
			models, err := p.fetchModels(ctx, n.url)
			p.applyPoll(n, models, err)
		}(n)
	}
	wg.Wait()
	if p.onRefresh != nil {
		p.onRefresh(p.Snapshot())
	}
}

// tagsResponse is the shape of Ollama's GET /api/tags.
type tagsResponse struct {
	Models []struct {
		Name  string `json:"name"`
		Model string `json:"model"`
	} `json:"models"`
}

// fetchModels reads one upstream's model inventory.
func (p *Pool) fetchModels(ctx context.Context, u *url.URL) (map[string]struct{}, error) {
	target := *u
	target.Path = strings.TrimRight(target.Path, "/") + "/api/tags"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /api/tags: %s", resp.Status)
	}

	var tags tagsResponse
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return nil, fmt.Errorf("decode /api/tags: %w", err)
	}
	models := make(map[string]struct{}, len(tags.Models))
	for _, m := range tags.Models {
		for _, name := range []string{m.Name, m.Model} {
			if name = strings.TrimSpace(name); name != "" {
				models[name] = struct{}{}
				models[modelname.Normalize(name, nil)] = struct{}{}
			}
		}
	}
	return models, nil
}

// applyPoll records the outcome of one inventory poll.
func (p *Pool) applyPoll(n *node, models map[string]struct{}, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	was := n.up
	n.polled = true
	if err != nil {
		n.up = false
		n.lastErr = err.Error()
	} else {
		n.up = true
		n.lastErr = ""
		n.models = models
	}
	if was != n.up {
		p.logger.Info("upstream health changed",
			"upstream", n.url.String(), "up", n.up, "error", n.lastErr)
	}
}

// Pick returns every upstream ordered by how suitable it is for model, best
// first. Upstreams known to serve the model come first, then those whose
// inventory is unknown, then the rest; a tripped circuit sinks an upstream to
// the bottom but never removes it.
func (p *Pool) Pick(model string) []*url.URL {
	p.mu.RLock()
	defer p.mu.RUnlock()

	type ranked struct {
		url  *url.URL
		rank int
		pos  int
	}
	now := p.now()
	offset := int(p.rr.Add(1) - 1)

	list := make([]ranked, 0, len(p.nodes))
	for i, n := range p.nodes {
		// Rotate the tie-break position so equally ranked upstreams still share
		// traffic round-robin.
		list = append(list, ranked{
			url:  n.url,
			rank: p.rank(n, model, now),
			pos:  (i + offset) % len(p.nodes),
		})
	}
	sort.SliceStable(list, func(a, b int) bool {
		if list[a].rank != list[b].rank {
			return list[a].rank < list[b].rank
		}
		return list[a].pos < list[b].pos
	})

	out := make([]*url.URL, 0, len(list))
	for _, r := range list {
		out = append(out, r.url)
	}
	return out
}

// rank scores a node for model: lower is better. Caller must hold at least RLock.
func (p *Pool) rank(n *node, model string, now time.Time) int {
	switch {
	case now.Before(n.openUntil):
		return 4
	case !n.up:
		return 3
	case has(n, model):
		return 0
	case !n.polled || len(n.models) == 0:
		return 1
	default:
		return 2
	}
}

// has reports whether n is known to serve model, accepting either the name as
// written or its normalized form.
func has(n *node, model string) bool {
	if model == "" || model == modelname.Unknown || len(n.models) == 0 {
		return false
	}
	if _, ok := n.models[model]; ok {
		return true
	}
	_, ok := n.models[modelname.Normalize(model, nil)]
	return ok
}

// ModelElsewhere reports whether an upstream other than u is known to serve
// model. It is what tells a 404 from "wrong node" apart from a 404 that every
// node would return. It deliberately does not ask whether u claims the model
// too: an inventory up to one poll interval stale is exactly the case where a
// node that used to have a model answers 404 for it.
func (p *Pool) ModelElsewhere(u *url.URL, model string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()

	target := u.String()
	var current *node
	for _, n := range p.nodes {
		if n.url.String() == target {
			current = n
			break
		}
	}
	if current == nil {
		return false
	}
	for _, n := range p.nodes {
		if n.url.String() != target && has(n, model) {
			return true
		}
	}
	return false
}

// Success clears the failure streak for u.
func (p *Pool) Success(u *url.URL) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n := p.find(u); n != nil {
		n.failures = 0
		n.openUntil = time.Time{}
		n.up = true
	}
}

// Failure records a failed request against u, opening its circuit once the
// failure threshold is reached.
func (p *Pool) Failure(u *url.URL) {
	p.mu.Lock()
	defer p.mu.Unlock()

	n := p.find(u)
	if n == nil {
		return
	}
	n.failures++
	if n.failures >= p.threshold && !p.now().Before(n.openUntil) {
		n.openUntil = p.now().Add(p.cooldown)
		p.logger.Warn("upstream circuit opened",
			"upstream", n.url.String(), "failures", n.failures, "cooldown", p.cooldown)
	}
}

// find returns the node for u, or nil. Caller must hold the lock.
func (p *Pool) find(u *url.URL) *node {
	target := u.String()
	for _, n := range p.nodes {
		if n.url.String() == target {
			return n
		}
	}
	return nil
}

// Snapshot returns the current state of every upstream, for reporting.
func (p *Pool) Snapshot() []State {
	p.mu.RLock()
	defer p.mu.RUnlock()

	now := p.now()
	out := make([]State, 0, len(p.nodes))
	for _, n := range p.nodes {
		s := State{
			URL:         n.url.String(),
			Up:          n.up,
			Polled:      n.polled,
			Failures:    n.failures,
			CircuitOpen: now.Before(n.openUntil),
			LastError:   n.lastErr,
		}
		if s.CircuitOpen {
			s.OpenUntil = n.openUntil
		}
		// Report the names as pulled, not the normalized duplicates kept for lookup.
		for m := range n.models {
			if strings.Contains(m[strings.LastIndex(m, "/")+1:], ":") {
				s.Models = append(s.Models, m)
			}
		}
		sort.Strings(s.Models)
		out = append(out, s)
	}
	return out
}
