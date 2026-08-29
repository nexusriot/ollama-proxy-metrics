package upstream

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return u
}

// order renders Pick's result as a comparable slice of URL strings.
func order(urls []*url.URL) []string {
	out := make([]string, 0, len(urls))
	for _, u := range urls {
		out = append(out, u.String())
	}
	return out
}

// tagsServer serves an /api/tags inventory of the given model names.
func tagsServer(t *testing.T, names ...string) *httptest.Server {
	t.Helper()
	body := `{"models":[`
	for i, n := range names {
		if i > 0 {
			body += ","
		}
		body += `{"name":"` + n + `","model":"` + n + `"}`
	}
	body += `]}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPool_PicksEveryUpstream(t *testing.T) {
	p := New(Options{URLs: []*url.URL{mustURL(t, "http://a"), mustURL(t, "http://b")}})
	if got := p.Pick("llama3"); len(got) != 2 {
		t.Fatalf("Pick returned %d candidates, want 2", len(got))
	}
}

func TestPool_RoundRobinsWithoutPolling(t *testing.T) {
	p := New(Options{URLs: []*url.URL{mustURL(t, "http://a"), mustURL(t, "http://b")}})
	first := order(p.Pick("m"))[0]
	second := order(p.Pick("m"))[0]
	if first == second {
		t.Fatalf("Pick returned %q first twice; expected round-robin rotation", first)
	}
}

func TestPool_PrefersUpstreamThatHasTheModel(t *testing.T) {
	withModel := tagsServer(t, "qwen3:8b")
	without := tagsServer(t, "llama3:latest")

	p := New(Options{URLs: []*url.URL{mustURL(t, without.URL), mustURL(t, withModel.URL)}})
	p.Refresh(context.Background())

	// Repeated picks must keep choosing the node that actually has the model,
	// even as the round-robin cursor advances.
	for i := 0; i < 4; i++ {
		if got := order(p.Pick("qwen3:8b"))[0]; got != withModel.URL {
			t.Fatalf("Pick[0] = %q, want the upstream serving the model (%q)", got, withModel.URL)
		}
	}
	// The other upstream is deprioritized, never dropped.
	if got := len(p.Pick("qwen3:8b")); got != 2 {
		t.Fatalf("Pick returned %d candidates, want both", got)
	}
}

func TestPool_MatchesUntaggedModelName(t *testing.T) {
	srv := tagsServer(t, "llama3:latest")
	other := tagsServer(t, "mistral:7b")

	p := New(Options{URLs: []*url.URL{mustURL(t, other.URL), mustURL(t, srv.URL)}})
	p.Refresh(context.Background())

	if got := order(p.Pick("llama3"))[0]; got != srv.URL {
		t.Fatalf("Pick[0] for untagged name = %q, want %q", got, srv.URL)
	}
}

func TestPool_DemotesUnreachableUpstream(t *testing.T) {
	healthy := tagsServer(t, "llama3:latest")
	p := New(Options{URLs: []*url.URL{mustURL(t, "http://127.0.0.1:1"), mustURL(t, healthy.URL)}})
	p.Refresh(context.Background())

	if got := order(p.Pick("anything"))[0]; got != healthy.URL {
		t.Fatalf("Pick[0] = %q, want the reachable upstream", got)
	}
	states := p.Snapshot()
	if states[0].Up {
		t.Fatal("unreachable upstream reported Up")
	}
	if states[0].LastError == "" {
		t.Fatal("unreachable upstream recorded no error")
	}
	if len(states[1].Models) != 1 || states[1].Models[0] != "llama3:latest" {
		t.Fatalf("healthy upstream models = %v", states[1].Models)
	}
}

func TestPool_CircuitOpensAfterThresholdAndSinksUpstream(t *testing.T) {
	a, b := mustURL(t, "http://a"), mustURL(t, "http://b")
	p := New(Options{URLs: []*url.URL{a, b}, FailureThreshold: 2, Cooldown: time.Minute})
	now := time.Unix(1_700_000_000, 0)
	p.now = func() time.Time { return now }

	p.Failure(a)
	if p.Snapshot()[0].CircuitOpen {
		t.Fatal("circuit opened before reaching the threshold")
	}
	p.Failure(a)
	if !p.Snapshot()[0].CircuitOpen {
		t.Fatal("circuit did not open at the threshold")
	}
	if got := order(p.Pick("m")); got[len(got)-1] != "http://a" {
		t.Fatalf("Pick = %v, want the open-circuit upstream last", got)
	}

	// The circuit closes on its own once the cooldown elapses.
	now = now.Add(2 * time.Minute)
	if p.Snapshot()[0].CircuitOpen {
		t.Fatal("circuit stayed open past its cooldown")
	}
}

func TestPool_SuccessClearsFailureStreak(t *testing.T) {
	a := mustURL(t, "http://a")
	p := New(Options{URLs: []*url.URL{a}, FailureThreshold: 2, Cooldown: time.Minute})

	p.Failure(a)
	p.Success(a)
	p.Failure(a)
	if p.Snapshot()[0].CircuitOpen {
		t.Fatal("circuit opened even though Success reset the streak")
	}
	if got := p.Snapshot()[0].Failures; got != 1 {
		t.Fatalf("Failures = %d, want 1", got)
	}
}

func TestPool_ModelElsewhere(t *testing.T) {
	withModel := tagsServer(t, "qwen3:8b")
	without := tagsServer(t, "llama3:latest")

	uWithout, uWith := mustURL(t, without.URL), mustURL(t, withModel.URL)
	p := New(Options{URLs: []*url.URL{uWithout, uWith}})
	p.Refresh(context.Background())

	if !p.ModelElsewhere(uWithout, "qwen3:8b") {
		t.Fatal("ModelElsewhere = false, but another upstream serves the model")
	}
	if p.ModelElsewhere(uWith, "qwen3:8b") {
		t.Fatal("ModelElsewhere = true for the upstream that has the model")
	}
	if p.ModelElsewhere(uWithout, "nobody-has-this") {
		t.Fatal("ModelElsewhere = true for a model no upstream serves")
	}
}

func TestPool_ModelElsewhereFalseBeforePolling(t *testing.T) {
	a, b := mustURL(t, "http://a"), mustURL(t, "http://b")
	p := New(Options{URLs: []*url.URL{a, b}})
	if p.ModelElsewhere(a, "llama3") {
		t.Fatal("ModelElsewhere = true with no inventory polled yet")
	}
}

func TestPool_StartPollsUntilContextCancelled(t *testing.T) {
	srv := tagsServer(t, "llama3:latest")
	refreshed := make(chan []State, 4)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p := New(Options{
		URLs:         []*url.URL{mustURL(t, srv.URL)},
		PollInterval: 10 * time.Millisecond,
		OnRefresh: func(s []State) {
			select {
			case refreshed <- s:
			default:
			}
		},
	})
	p.Start(ctx)

	select {
	case states := <-refreshed:
		if !states[0].Up || !states[0].Polled {
			t.Fatalf("polled state = %+v", states[0])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start never refreshed the inventory")
	}
}

func TestPool_StartIsNoOpWithoutInterval(t *testing.T) {
	p := New(Options{URLs: []*url.URL{mustURL(t, "http://127.0.0.1:1")}})
	p.Start(context.Background())
	if p.Snapshot()[0].Polled {
		t.Fatal("Start polled even though the interval is zero")
	}
}

func TestPool_URLsPreservesOrder(t *testing.T) {
	a, b := mustURL(t, "http://a"), mustURL(t, "http://b")
	p := New(Options{URLs: []*url.URL{a, b}})
	got := order(p.URLs())
	if len(got) != 2 || got[0] != "http://a" || got[1] != "http://b" {
		t.Fatalf("URLs = %v", got)
	}
}
