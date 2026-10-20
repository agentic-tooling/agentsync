package optimize

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentic-tooling/agentsync/internal/openrouter"
)

const testdata = "../openrouter/testdata/"

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(testdata + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func models(t *testing.T) []openrouter.Model {
	t.Helper()
	var out struct{ Data []openrouter.Model }
	if err := json.Unmarshal([]byte(fixture(t, "models.json")), &out); err != nil {
		t.Fatal(err)
	}
	return out.Data
}

// reply is one canned response; serve, when set, replaces status and body.
type reply struct {
	status int
	body   string
	serve  http.HandlerFunc
}

// advisor serves replies in order and counts the requests it was sent. A
// request past the last reply gets 418, so an unexpected one fails a count
// assertion instead of hanging the test.
func advisor(t *testing.T, replies ...reply) (*openrouter.Client, func() int) {
	t.Helper()
	var (
		mu sync.Mutex
		n  int
	)
	next := func() (reply, bool) {
		mu.Lock()
		defer mu.Unlock()
		n++
		if n > len(replies) {
			return reply{}, false
		}
		return replies[n-1], true
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rep, ok := next()
		switch {
		case !ok:
			w.WriteHeader(http.StatusTeapot)
		case rep.serve != nil:
			rep.serve(w, r)
		default:
			w.WriteHeader(rep.status)
			_, _ = io.WriteString(w, rep.body)
		}
	}))
	t.Cleanup(srv.Close)
	c := openrouter.New("sk-test")
	c.BaseURL = srv.URL
	return c, func() int { mu.Lock(); defer mu.Unlock(); return n }
}

func agent(label, model, effort string) Agent {
	fm := "---\nname: " + label + "\ndescription: d\n"
	if model != "" {
		fm += "model: " + model + "\n"
	}
	if effort != "" {
		fm += "effort: " + effort + "\n"
	}
	return Agent{Label: label, Path: "/p/" + label + ".md", Content: fm + "---\nbody\n"}
}

func config() Config {
	return Config{Advisor: "~typesafe/jev-latest", Price: 4.2e-8, MaxCost: 0.5, MinConfidence: 0.6, Families: Families}
}

func TestCurrent(t *testing.T) {
	for _, tc := range []struct{ content, model, effort string }{
		{"---\nname: n\nmodel: opus\neffort: xhigh\n---\n", "opus", "xhigh"},
		{"---\nmodel: \"claude-opus-4-5\"\neffort: 'low'\n---\n", "claude-opus-4-5", "low"},
		{"---\nmodel: inherit\n---\n", "inherit", NotSet},
		{"---\nmodel:\neffort: \"\"\n---\n", NotSet, NotSet},
		{"no frontmatter\nmodel: opus\n", NotSet, NotSet},
		{"---\nmodel: opus # cheap enough\neffort: \"high\"\t# for now\n---\n", "opus", "high"},
		{"---\nmodel: # none yet\n---\n", NotSet, NotSet},
	} {
		if m, e := Current(tc.content); m != tc.model || e != tc.effort {
			t.Errorf("Current(%q) = %q, %q; want %q, %q", tc.content, m, e, tc.model, tc.effort)
		}
	}
}

func TestAvailable(t *testing.T) {
	if got := Available(models(t)); !reflect.DeepEqual(got, Families) {
		t.Errorf("real list: %v, want all four", got)
	}
	got := Available([]openrouter.Model{{ID: "anthropic/claude-opus-5.5"}, {ID: "anthropic/claude-sonnet-x"}, {ID: "anthropic/claude-haiku-"}, {ID: "openai/fable-5"}})
	if len(got) != 1 || got[0].Alias != "opus" {
		t.Errorf("got %v, want opus alone", got)
	}
}

func TestInputPrice(t *testing.T) {
	ms := append(models(t), openrouter.Model{ID: "nan"}, openrouter.Model{ID: "inf"})
	ms[len(ms)-2].Pricing.Prompt, ms[len(ms)-1].Pricing.Prompt = "NaN", "+Inf"
	for _, id := range []string{"~typesafe/jev-latest", "cloudflare/clef"} {
		if p, err := InputPrice(ms, id); err != nil || p != 4.2e-8 {
			t.Errorf("%s: %v, %v; want 4.2e-8", id, p, err)
		}
	}
	for id, want := range map[string]string{
		"typesafe/jev-router": "no fixed input price",
		"nan":                 "no fixed input price",
		"inf":                 "no fixed input price",
		"openai/gpt-6-luna":   "not in the OpenRouter models list",
	} {
		if _, err := InputPrice(ms, id); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", id, err, want)
		}
	}
}

func TestBody_MatchesTheSpikeRequestAndCapsState(t *testing.T) {
	var req openrouter.DecisionRequest
	content := strings.Repeat("a", stateCap-1) + "é" + "tail"
	if err := json.Unmarshal(Body("cloudflare/clef", content, Families), &req); err != nil {
		t.Fatal(err)
	}
	if req.Model != "cloudflare/clef" || len(req.State) != 2 || req.State[0] != framing {
		t.Errorf("model %q, state %d entries", req.Model, len(req.State))
	}
	if req.State[1] != strings.Repeat("a", stateCap-1) {
		t.Errorf("state not cut cleanly at %d bytes: %d bytes, tail %q", stateCap, len(req.State[1]), req.State[1][len(req.State[1])-3:])
	}
	var got, want any
	b, _ := json.Marshal(req.Questions)
	_ = json.Unmarshal(b, &got)
	_ = json.Unmarshal([]byte(fixture(t, "decision-request-questions.json")), &want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("questions differ from the request that worked:\n%s", b)
	}
}

func TestRun_RealResponses(t *testing.T) {
	jev, clef := fixture(t, "decision-jev.json"), fixture(t, "decision-clef.json")
	for _, tc := range []struct {
		name, body, model, effort string
		min                       float64
		status                    Status
		suggested                 string
		lowM, lowE                bool
	}{
		{"jev below threshold keeps", jev, "opus", "xhigh", 0.6, StatusKeep, "keep (low confidence)", true, false},
		{"jev at threshold changes", jev, "opus", "xhigh", 0.57, StatusChange, "sonnet / xhigh", false, false},
		{"jev above threshold changes", jev, "opus", "xhigh", 0.5, StatusChange, "sonnet / xhigh", false, false},
		{"jev agrees", jev, "sonnet", "xhigh", 0.6, StatusKeep, "keep", false, false},
		{"jev both parts low", jev, "opus", "low", 0.95, StatusKeep, "keep (low confidence)", true, true},
		{"clef changes model, effort low confidence", clef, "sonnet", "xhigh", 0.6, StatusChange, "opus / xhigh (effort: low confidence)", false, true},
		{"clef effort is the nearest level's probability", clef, "opus", "", 0.56, StatusChange, "opus / high", false, false},
		{"jev changes effort, model low confidence", jev, "opus", "medium", 0.6, StatusChange, "opus / xhigh (model: low confidence)", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := advisor(t, reply{status: 200, body: tc.body})
			cfg := config()
			cfg.MinConfidence = tc.min
			rep := Run(context.Background(), c, []Agent{agent("go-merge-reviewer", tc.model, tc.effort)}, cfg)
			r := rep.Rows[0]
			if r.Status != tc.status || r.Suggested() != tc.suggested || r.ModelLowConfidence != tc.lowM || r.EffortLowConfidence != tc.lowE {
				t.Errorf("status %q suggested %q low %v/%v; want %q %q %v/%v (%s)", r.Status, r.Suggested(),
					r.ModelLowConfidence, r.EffortLowConfidence, tc.status, tc.suggested, tc.lowM, tc.lowE, r.Reason)
			}
			if rep.Spent != r.Cost || r.Cost == 0 {
				t.Errorf("spent %v, row cost %v", rep.Spent, r.Cost)
			}
		})
	}
}

func TestRun_FailsClosed(t *testing.T) {
	answers := func(model, effort string) string {
		return `{"answers":{` + model + `,` + effort + `},"usage":{"cost":0.0001}}`
	}
	goodM := `"model":{"type":"choice","choice":"opus","probabilities":{"opus":0.9}}`
	goodE := `"effort":{"type":"score","score":2,"probabilities":{"2":0.9}}`
	for _, tc := range []struct {
		name string
		r    reply
		want string
	}{
		{"unknown model 400", reply{status: 400, body: fixture(t, "error-unknown-model.json")}, "Model openai/gpt-6-luna does not exist"},
		{"5xx", reply{status: 503, body: "down"}, "HTTP 503: down"},
		{"malformed", reply{status: 200, body: `{"answers":`}, "malformed JSON"},
		{"no answers", reply{status: 200, body: `{"usage":{"cost":0.0001}}`}, "no choice answer for model"},
		{"option not offered", reply{status: 200, body: answers(`"model":{"type":"choice","choice":"gpt","probabilities":{"gpt":1}}`, goodE)}, `"gpt" is not an offered option`},
		{"wrong type", reply{status: 200, body: answers(`"model":{"type":"score","choice":"opus"}`, goodE)}, `got type "score"`},
		{"no probability", reply{status: 200, body: answers(`"model":{"type":"choice","choice":"opus","probabilities":{}}`, goodE)}, "has no probability"},
		{"probability above 1", reply{status: 200, body: answers(`"model":{"type":"choice","choice":"opus","probabilities":{"opus":1.5}}`, goodE)}, "outside 0..1"},
		{"no effort score", reply{status: 200, body: answers(goodM, `"effort":{"type":"score","probabilities":{"2":0.9}}`)}, "no score answer for effort"},
		{"no probability for the nearest level", reply{status: 200, body: answers(goodM, `"effort":{"type":"score","score":2.6,"confidence":0.9,"probabilities":{"2":0.9}}`)}, "effort level xhigh has no probability"},
		{"effort out of range", reply{status: 200, body: answers(goodM, `"effort":{"type":"score","score":4.6,"probabilities":{"4":0.9}}`)}, "outside 0..4"},
		{"negative effort probability", reply{status: 200, body: answers(goodM, `"effort":{"type":"score","score":1,"probabilities":{"1":-0.1}}`)}, "effort probability"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := advisor(t, tc.r)
			r := Run(context.Background(), c, []Agent{agent("a", "opus", "high")}, config()).Rows[0]
			if r.Status != StatusUnresolved || !strings.Contains(r.Reason, tc.want) {
				t.Errorf("status %q reason %q; want unresolved with %q", r.Status, r.Reason, tc.want)
			}
			if r.Suggested() != "unresolved" || r.Confidence() != "-" || r.SuggestedModel != "" || r.SuggestedEffort != "" {
				t.Errorf("an unresolved row suggests something: %+v", r)
			}
		})
	}
}

func TestRun_UntrackedSpendStopsTheRun(t *testing.T) {
	// The server notices the client hanging up only once the body is read.
	hang := func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}
	truncate := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = io.WriteString(w, `{"answers":`)
	}
	for _, tc := range []struct {
		name string
		r    reply
		want string
	}{
		{"missing cost", reply{status: 200, body: `{"answers":{}}`}, "no valid usage.cost"},
		{"negative cost", reply{status: 200, body: `{"answers":{},"usage":{"cost":-1}}`}, "no valid usage.cost"},
		{"malformed 200", reply{status: 200, body: `{"answers":`}, "malformed JSON"},
		{"truncated 200", reply{serve: truncate}, "reading OpenRouter response"},
		{"timeout", reply{serve: hang}, "Timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, sent := advisor(t, tc.r)
			c.HTTP.Timeout = 200 * time.Millisecond
			rep := Run(context.Background(), c, []Agent{agent("a", "", ""), agent("b", "", ""), agent("c", "", "")}, config())
			if sent() != 1 {
				t.Errorf("%d requests sent, want 1", sent())
			}
			r := rep.Rows[0]
			if r.Status != StatusUnresolved || !strings.Contains(r.Reason, tc.want) || !strings.Contains(r.Reason, "spend can no longer be tracked") {
				t.Errorf("first row %+v, want unresolved with %q", r, tc.want)
			}
			if rep.Count(StatusSkipped) != 2 || !strings.Contains(rep.Rows[2].Reason, "run stopped") {
				t.Errorf("rest %+v", rep.Rows[1:])
			}
			if rep.Spent != 0 || strings.Contains(r.Reason, "sk-test") {
				t.Errorf("spent %v; reason %q", rep.Spent, r.Reason)
			}
		})
	}
	t.Run("an error status does not stop the run", func(t *testing.T) {
		c, sent := advisor(t, reply{status: 503, body: "down"})
		rep := Run(context.Background(), c, []Agent{agent("a", "", ""), agent("b", "", "")}, config())
		if sent() != 2 || rep.Count(StatusUnresolved) != 2 {
			t.Errorf("sent %d, rows %+v", sent(), rep.Rows)
		}
	})
}

func TestRun_CostCap(t *testing.T) {
	costly := `{"answers":{"model":{"type":"choice","choice":"opus","probabilities":{"opus":0.9}},"effort":{"type":"score","score":2,"probabilities":{"2":0.9}}},"usage":{"cost":0.2}}`
	cheap := strings.Replace(costly, `"cost":0.2`, `"cost":0.1`, 1)
	agents := []Agent{agent("a", "opus", "high"), agent("b", "opus", "high"), agent("c", "opus", "high")}
	t.Run("running total of usage.cost", func(t *testing.T) {
		c, sent := advisor(t, reply{status: 200, body: cheap}, reply{status: 200, body: cheap})
		cfg := config()
		cfg.MaxCost = 0.25
		rep := Run(context.Background(), c, agents, cfg)
		// a and b fit; c's $0.10 estimate does not fit on top of $0.20 spent.
		if sent() != 2 || rep.Spent != 0.2 {
			t.Errorf("sent %d, spent %v", sent(), rep.Spent)
		}
		if rep.Count(StatusKeep) != 2 || rep.Count(StatusSkipped) != 1 {
			t.Errorf("rows %+v", rep.Rows)
		}
	})
	t.Run("estimate learns the billed rate", func(t *testing.T) {
		c, sent := advisor(t, reply{status: 200, body: costly})
		cfg := config()
		cfg.MaxCost = 0.3
		rep := Run(context.Background(), c, agents, cfg)
		// At the listed price b would look free; a's reported $0.20 says it costs as much again.
		if sent() != 1 || rep.Spent != 0.2 || rep.Count(StatusSkipped) != 2 {
			t.Errorf("sent %d, spent %v, rows %+v", sent(), rep.Spent, rep.Rows)
		}
		if !strings.Contains(rep.Rows[1].Reason, "estimated $0.2") {
			t.Errorf("b's reason %q", rep.Rows[1].Reason)
		}
	})
	t.Run("estimate alone", func(t *testing.T) {
		c, sent := advisor(t)
		cfg := config()
		cfg.MaxCost = 0
		rep := Run(context.Background(), c, []Agent{agent("a", "", "")}, cfg)
		if sent() != 0 || rep.Rows[0].Status != StatusSkipped || !strings.Contains(rep.Rows[0].Reason, "would pass the $0.00 cap") {
			t.Errorf("sent %d, row %+v", sent(), rep.Rows[0])
		}
		if rep.Rows[0].Suggested() != "skipped" {
			t.Errorf("suggested %q", rep.Rows[0].Suggested())
		}
	})
}

func TestRow_Display(t *testing.T) {
	r := Row{Model: NotSet, Effort: "high", Status: StatusChange, SuggestedModel: "opus",
		ModelPick: &Pick{"opus", 0.668}, EffortPick: &Pick{"high", 0.2615}}
	if r.Now() != "(not set) / high" || r.Suggested() != "opus / high" || r.Confidence() != "opus 0.67 / high 0.26" {
		t.Errorf("now %q suggested %q confidence %q", r.Now(), r.Suggested(), r.Confidence())
	}
	r.SuggestedModel, r.SuggestedEffort = "", "max"
	if r.Suggested() != "(not set) / max" {
		t.Errorf("effort-only change shows %q", r.Suggested())
	}
	if (Row{}).Suggested() != "" {
		t.Error("the zero row suggests something")
	}
}

func TestEdit(t *testing.T) {
	const body = "body mentions\nmodel: opus\n"
	for _, tc := range []struct {
		name, in, model, effort, want, err string
	}{
		{name: "replace both", in: "---\nname: a\nmodel: opus\neffort: high\n---\n" + body, model: "sonnet", effort: "low",
			want: "---\nname: a\nmodel: sonnet\neffort: low\n---\n" + body},
		{name: "empty leaves a key alone", in: "---\nmodel: opus\neffort: high\n---\n", effort: "max",
			want: "---\nmodel: opus\neffort: max\n---\n"},
		{name: "insert both", in: "---\nname: a\n---\n" + body, model: "haiku", effort: "medium",
			want: "---\nname: a\nmodel: haiku\neffort: medium\n---\n" + body},
		{name: "insert one beside a replace", in: "---\neffort: high\n---\n", model: "opus", effort: "low",
			want: "---\neffort: low\nmodel: opus\n---\n"},
		{name: "quotes and comment kept", in: "---\nmodel:   \"opus\"  # pinned\neffort: 'high'\t# why\n---\n", model: "sonnet", effort: "low",
			want: "---\nmodel:   \"sonnet\"  # pinned\neffort: 'low'\t# why\n---\n"},
		{name: "comment-only and empty values", in: "---\nmodel: # todo\neffort:\n---\n", model: "opus", effort: "low",
			want: "---\nmodel: opus # todo\neffort: low\n---\n"},
		{name: "CRLF", in: "---\r\nname: a\r\nmodel: opus\r\n---\r\nbody\r\n", model: "sonnet", effort: "high",
			want: "---\r\nname: a\r\nmodel: sonnet\r\neffort: high\r\n---\r\nbody\r\n"},
		{name: "closing line at end of file", in: "---\nmodel: opus\n---", model: "haiku", want: "---\nmodel: haiku\n---"},
		{name: "blank line then next key", in: "---\nmodel: opus\n\nname: a\n---\n", model: "haiku", want: "---\nmodel: haiku\n\nname: a\n---\n"},
		{name: "nested model line is not the key", in: "---\nx:\n  model: opus\n---\n", model: "haiku", want: "---\nx:\n  model: opus\nmodel: haiku\n---\n"},
		{name: "no frontmatter", in: "body\nmodel: opus\n", model: "haiku", err: "no frontmatter"},
		{name: "empty file", in: "", model: "haiku", err: "no frontmatter"},
		{name: "no closing line", in: "---\nmodel: opus\n", model: "haiku", err: "no closing --- line"},
		{name: "closing line with more on it", in: "---\nmodel: opus\n--- x\n", model: "haiku", err: "no closing --- line"},
		{name: "duplicate key", in: "---\neffort: high\neffort: low\n---\n", effort: "max", err: "sets effort: twice"},
		{name: "block scalar", in: "---\nmodel: |\n  opus\n---\n", model: "haiku", err: "model: is not a simple one-line value"},
		{name: "flow value", in: "---\nmodel: [opus]\n---\n", model: "haiku", err: "model: is not a simple one-line value"},
		{name: "indented continuation", in: "---\nmodel:\n\n  opus\n---\n", model: "haiku", err: "model: is not a simple one-line value"},
		{name: "list continuation", in: "---\neffort:\n- high\n---\n", effort: "low", err: "effort: is not a simple one-line value"},
		{name: "key-only and nested lines skipped", in: "---\n: x\n  model: opus\n- effort: low\n# model: x\n---\n", model: "haiku",
			want: "---\n: x\n  model: opus\n- effort: low\n# model: x\nmodel: haiku\n---\n"},
		{name: "other keys quoted or repeated", in: "---\n\"description\": x\ntools: a\ntools: b\nmodel: opus\n---\n", model: "haiku",
			want: "---\n\"description\": x\ntools: a\ntools: b\nmodel: haiku\n---\n"},
		{name: "duplicate of the key not edited", in: "---\nmodel: opus\nmodel: haiku\n---\n", effort: "low", err: "sets model: twice"},
		{name: "space before colon", in: "---\nmodel : opus\n---\n", model: "haiku", err: `spells model: as "model :"`},
		{name: "tab before colon", in: "---\nmodel\t: opus\n---\n", model: "haiku", err: `spells model: as "model\t:"`},
		{name: "double-quoted key", in: "---\n\"model\": opus\n---\n", model: "haiku", err: `spells model: as "\"model\":"`},
		{name: "single-quoted key of the other part", in: "---\n'effort': high\n---\n", model: "haiku", err: `spells effort: as "'effort':"`},
		{name: "anchor", in: "---\nmodel: &m opus\n---\n", model: "haiku", err: "model: is not a simple one-line value"},
		{name: "alias", in: "---\nmodel: *m\n---\n", model: "haiku", err: "model: is not a simple one-line value"},
		{name: "tag", in: "---\nmodel: !!str opus\n---\n", model: "haiku", err: "model: is not a simple one-line value"},
		{name: "tab-indented continuation", in: "---\nmodel:\n\topus\n---\n", model: "haiku", err: "model: is not a simple one-line value"},
		{name: "unclosed quote", in: "---\nmodel: \"opus\n---\n", model: "haiku", err: "model: is not a simple one-line value"},
		{name: "lone quote", in: "---\nmodel: '\n---\n", model: "haiku", err: "model: is not a simple one-line value"},
		{name: "text after a quoted value", in: "---\nmodel: \"opus\" x\n---\n", model: "haiku", err: "model: is not a simple one-line value"},
		{name: "escaped quote inside", in: "---\nmodel: 'it''s'\n---\n", model: "haiku", err: "model: is not a simple one-line value"},
		{name: "quoted value with a comment", in: "---\nmodel: 'opus' # c\n---\n", model: "haiku", want: "---\nmodel: 'haiku' # c\n---\n"},
		{name: "value that does not read back", in: "---\nmodel: opus\n---\n", model: "a #b", err: "reads back as a / (not set)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Edit(tc.in, tc.model, tc.effort)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) || got != "" {
					t.Fatalf("Edit = %q, %v; want error containing %q", got, err, tc.err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("Edit = %q, %v; want %q", got, err, tc.want)
			}
			m, e := Current(got)
			if (tc.model != "" && m != tc.model) || (tc.effort != "" && e != tc.effort) {
				t.Errorf("Current(edited) = %s / %s", m, e)
			}
		})
	}
}
