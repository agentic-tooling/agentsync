// Package optimize asks an OpenRouter decision model which Claude model and
// effort each agent should run on, and decides which answers are confident
// enough to suggest. It writes nothing.
package optimize

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/agentic-tooling/agentsync/internal/discover"
	"github.com/agentic-tooling/agentsync/internal/openrouter"
)

// NotSet is shown for a model or effort key the frontmatter lacks.
const NotSet = "(not set)"

// stateCap keeps the agent file under Jev's long-context threshold of ~32 KB.
const stateCap = 24 << 10

const framing = "A Claude Code subagent definition follows. Decide which Claude model and reasoning effort it should run on: the cheapest choice that keeps quality for its role."

// Family is a Claude Code model alias, which Claude Code resolves to the
// newest model of that family, so advice never names a stale version.
type Family struct {
	Alias       string
	Description string
}

// Families are the aliases optimize offers and override accepts.
var Families = []Family{
	{"haiku", "Claude Haiku: fastest and cheapest, for simple, mechanical or high-volume work"},
	{"sonnet", "Claude Sonnet: balanced quality and cost, strong at most coding and review work"},
	{"opus", "Claude Opus: highest-quality reasoning for hard, high-stakes judgement"},
	{"fable", "Claude Fable: frontier model for long autonomous knowledge work, most expensive"},
}

// Efforts are Claude Code's effort levels in order; a score answer indexes it.
var Efforts = []string{"low", "medium", "high", "xhigh", "max"}

// Status is a row's outcome. Only StatusChange suggests anything; the zero
// value is none of these and suggests nothing.
type Status string

const (
	StatusChange     Status = "change"
	StatusKeep       Status = "keep"
	StatusUnresolved Status = "unresolved"
	StatusSkipped    Status = "skipped"
)

// Agent is one agent file to ask about.
type Agent struct {
	Label, Path, Content string
}

// Pick is one answer and the probability the advisor gave it; for effort, the
// probability of the level nearest the score.
type Pick struct {
	Value       string  `json:"value"`
	Probability float64 `json:"probability"`
}

// Row is the advice for one agent. Suggested fields are set only for the
// parts that change; a LowConfidence flag marks a differing pick that was
// dropped for falling under the threshold.
type Row struct {
	Agent               string  `json:"agent"`
	Path                string  `json:"path"`
	Model               string  `json:"model"`
	Effort              string  `json:"effort"`
	Status              Status  `json:"status"`
	SuggestedModel      string  `json:"suggested_model,omitempty"`
	SuggestedEffort     string  `json:"suggested_effort,omitempty"`
	ModelPick           *Pick   `json:"model_pick,omitempty"`
	EffortPick          *Pick   `json:"effort_pick,omitempty"`
	ModelLowConfidence  bool    `json:"model_low_confidence,omitempty"`
	EffortLowConfidence bool    `json:"effort_low_confidence,omitempty"`
	Reason              string  `json:"reason,omitempty"`
	Cost                float64 `json:"cost_usd"`
}

// Report is one run's rows and the spend OpenRouter reported for them.
// Overrides names the set environment variables Claude Code uses over the
// frontmatter; the caller fills it.
type Report struct {
	Advisor   string   `json:"advisor"`
	Rows      []Row    `json:"rows"`
	Spent     float64  `json:"spent_usd"`
	Overrides []string `json:"overrides,omitempty"`
}

// Count returns how many rows have status s.
func (r Report) Count(s Status) int {
	n := 0
	for _, row := range r.Rows {
		if row.Status == s {
			n++
		}
	}
	return n
}

// Config is what Run needs beyond the agents. Price is the advisor's input
// price in USD per token.
type Config struct {
	Advisor       string
	Price         float64
	MaxCost       float64
	MinConfidence float64
	Families      []Family
}

// Field returns frontmatter key k as written, without a trailing YAML comment
// or quotes, or "".
func Field(content, k string) string {
	fm, _ := discover.Frontmatter(content)
	v := " " + fm[k]
	for _, sep := range []string{" #", "\t#"} {
		v, _, _ = strings.Cut(v, sep)
	}
	return strings.Trim(strings.TrimSpace(v), `"'`)
}

// Current returns an agent's model and effort as Field reads them, or NotSet.
func Current(content string) (model, effort string) {
	get := func(k string) string {
		if v := Field(content, k); v != "" {
			return v
		}
		return NotSet
	}
	return get("model"), get("effort")
}

// Available keeps the aliases OpenRouter lists an anthropic/claude-<family>-<version> model for.
func Available(models []openrouter.Model) []Family {
	var out []Family
	for _, f := range Families {
		for _, m := range models {
			v, ok := strings.CutPrefix(m.ID, "anthropic/claude-"+f.Alias+"-")
			if ok && v != "" && v[0] >= '0' && v[0] <= '9' {
				out = append(out, f)
				break
			}
		}
	}
	return out
}

// InputPrice is the advisor's input price per token. A missing, variable
// ("-1") or unparseable price refuses: the cost cap cannot be kept without it.
func InputPrice(models []openrouter.Model, advisor string) (float64, error) {
	for _, m := range models {
		if m.ID != advisor {
			continue
		}
		p, err := strconv.ParseFloat(m.Pricing.Prompt, 64)
		if err != nil || !(p >= 0) || math.IsInf(p, 1) {
			return 0, fmt.Errorf("advisor %s has no fixed input price in the OpenRouter models list (got %q), so the cost cap cannot be kept", advisor, m.Pricing.Prompt)
		}
		return p, nil
	}
	return 0, fmt.Errorf("advisor %s is not in the OpenRouter models list", advisor)
}

// Body encodes the Decisions request for one agent file.
func Body(advisor, content string, fams []Family) []byte {
	if len(content) > stateCap {
		content = strings.ToValidUTF8(content[:stateCap], "")
	}
	criteria := make(map[string]string, len(fams))
	for _, f := range fams {
		criteria[f.Alias] = f.Description
	}
	// Strings, string maps and slices always encode; there is no error to handle.
	b, _ := json.Marshal(openrouter.DecisionRequest{
		Model: advisor,
		State: []string{framing, content},
		Questions: map[string]openrouter.Question{
			"model":  {Type: "choice", Instructions: "Which Claude model family should this subagent run on?", Criteria: criteria},
			"effort": {Type: "score", Instructions: "How much reasoning effort does this subagent need?", Criteria: Efforts},
		},
	})
	return b
}

// Run asks about each agent in turn, one request each and no retries. A
// request whose estimate would take the reported spend past MaxCost is
// skipped. Anything that may have billed without a usable usage.cost stops
// the run, because spend can no longer be tracked.
func Run(ctx context.Context, c *openrouter.Client, agents []Agent, cfg Config) Report {
	rep := Report{Advisor: cfg.Advisor, Rows: make([]Row, 0, len(agents))}
	stopped := ""
	// Listed price at bytes/4 tokens, raised to the highest cost per byte
	// OpenRouter has reported, so an advisor billed above its listed price is
	// underestimated mostly on the first request.
	rate := cfg.Price / 4
	for _, a := range agents {
		row := Row{Agent: a.Label, Path: a.Path}
		row.Model, row.Effort = Current(a.Content)
		body := Body(cfg.Advisor, a.Content, cfg.Families)
		est := float64(len(body)) * rate
		switch {
		case stopped != "":
			row.Status, row.Reason = StatusSkipped, stopped
		case rep.Spent+est > cfg.MaxCost:
			row.Status = StatusSkipped
			row.Reason = fmt.Sprintf("estimated $%.6f would pass the $%.2f cap", est, cfg.MaxCost)
		default:
			d, err := c.Decide(ctx, body)
			var apiErr *openrouter.APIError
			switch {
			case errors.As(err, &apiErr):
				// OpenRouter does not bill a request it answers with an error status.
				row.Status, row.Reason = StatusUnresolved, err.Error()
			case err != nil:
				stopped = "run stopped: spend can no longer be tracked"
				row.Status, row.Reason = StatusUnresolved, err.Error()+"; the request may have been billed, so spend can no longer be tracked"
			case d.Usage.Cost == nil || !(*d.Usage.Cost >= 0) || math.IsInf(*d.Usage.Cost, 1):
				stopped = "run stopped: spend can no longer be tracked"
				row.Status, row.Reason = StatusUnresolved, "response reported no valid usage.cost, so spend can no longer be tracked"
			default:
				row.Cost = *d.Usage.Cost
				rep.Spent += row.Cost
				rate = max(rate, row.Cost/float64(len(body)))
				row = judge(row, d, cfg)
			}
		}
		rep.Rows = append(rep.Rows, row)
	}
	return rep
}

// judge maps the answers onto row, refusing anything not offered.
func judge(row Row, d openrouter.Decision, cfg Config) Row {
	m, err := modelPick(d.Answers, cfg.Families)
	if err == nil {
		var e Pick
		if e, err = effortPick(d.Answers); err == nil {
			row.ModelPick, row.EffortPick = &m, &e
		}
	}
	if err != nil {
		row.Status, row.Reason = StatusUnresolved, err.Error()
		return row
	}
	row.SuggestedModel, row.ModelLowConfidence = suggest(row.Model, m, cfg.MinConfidence)
	row.SuggestedEffort, row.EffortLowConfidence = suggest(row.Effort, *row.EffortPick, cfg.MinConfidence)
	row.Status = StatusKeep
	if row.SuggestedModel != "" || row.SuggestedEffort != "" {
		row.Status = StatusChange
	}
	return row
}

// suggest returns the pick when it differs from current and clears min, and
// whether a differing pick was dropped for low confidence.
func suggest(current string, p Pick, min float64) (string, bool) {
	switch {
	case p.Value == current:
		return "", false
	case p.Probability < min:
		return "", true
	}
	return p.Value, false
}

func modelPick(answers map[string]openrouter.Answer, fams []Family) (Pick, error) {
	a, ok := answers["model"]
	if !ok || a.Type != "choice" {
		return Pick{}, fmt.Errorf("no choice answer for model (got type %q)", a.Type)
	}
	offered := false
	for _, f := range fams {
		offered = offered || f.Alias == a.Choice
	}
	if !offered {
		return Pick{}, fmt.Errorf("model answer %q is not an offered option", a.Choice)
	}
	p, ok := a.Probabilities[a.Choice]
	if !ok {
		return Pick{}, fmt.Errorf("model answer %q has no probability", a.Choice)
	}
	return Pick{a.Choice, p}, probability("model", p)
}

func effortPick(answers map[string]openrouter.Answer) (Pick, error) {
	a, ok := answers["effort"]
	if !ok || a.Type != "score" || a.Score == nil {
		return Pick{}, fmt.Errorf("no score answer for effort (got type %q)", a.Type)
	}
	i := math.Round(*a.Score)
	if !(i >= 0 && i < float64(len(Efforts))) {
		return Pick{}, fmt.Errorf("effort score %v is outside 0..%d", *a.Score, len(Efforts)-1)
	}
	level := Efforts[int(i)]
	p, ok := a.Probabilities[strconv.Itoa(int(i))]
	if !ok {
		return Pick{}, fmt.Errorf("effort level %s has no probability", level)
	}
	return Pick{level, p}, probability("effort", p)
}

// probability refuses a value outside [0, 1], which would otherwise clear any
// threshold.
func probability(q string, p float64) error {
	if !(p >= 0 && p <= 1) {
		return fmt.Errorf("%s probability %v is outside 0..1", q, p)
	}
	return nil
}

// Now is the current setting, "model / effort".
func (r Row) Now() string { return r.Model + " / " + r.Effort }

// Suggested is the setting to move to, naming a part dropped for low
// confidence, or why there is none.
func (r Row) Suggested() string {
	switch r.Status {
	case StatusChange:
		m, e, note := r.Model, r.Effort, ""
		if r.SuggestedModel != "" {
			m = r.SuggestedModel
		}
		if r.SuggestedEffort != "" {
			e = r.SuggestedEffort
		}
		if r.ModelLowConfidence {
			note = " (model: low confidence)"
		}
		if r.EffortLowConfidence {
			note = " (effort: low confidence)"
		}
		return m + " / " + e + note
	case StatusKeep:
		if r.ModelLowConfidence || r.EffortLowConfidence {
			return "keep (low confidence)"
		}
		return "keep"
	}
	return string(r.Status)
}

// Confidence is each pick with its probability, or "-" when nothing was picked.
func (r Row) Confidence() string {
	if r.ModelPick == nil || r.EffortPick == nil {
		return "-"
	}
	return fmt.Sprintf("%s %.2f / %s %.2f", r.ModelPick.Value, r.ModelPick.Probability, r.EffortPick.Value, r.EffortPick.Probability)
}
