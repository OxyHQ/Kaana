package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
)

// DecisionEffort is independent of generation reasoning effort.
type DecisionEffort string

var decisionEffortValues = []DecisionEffort{"instant", "low", "medium", "high", "xhigh"}

type DecisionKind string

var decisionKindValues = []DecisionKind{"choice", "score", "noul"}

type DecisionQuestion struct {
	ID       string       `json:"id"`
	Kind     DecisionKind `json:"kind"`
	Question string       `json:"question"`
	Criteria *string      `json:"criteria,omitempty"`
	Options  []string     `json:"options,omitempty"`
	Levels   []string     `json:"levels,omitempty"`
}

type DecisionInput struct {
	State        string             `json:"state"`
	Instructions *string            `json:"instructions,omitempty"`
	Questions    []DecisionQuestion `json:"questions"`
	Effort       *DecisionEffort    `json:"effort,omitempty"`
}

// DecisionReply preserves the provider's discriminated scalar verbatim.
type DecisionReply struct {
	Label *string
	Score *float64
}

func (r DecisionReply) MarshalJSON() ([]byte, error) {
	if r.Label != nil && r.Score == nil {
		return json.Marshal(*r.Label)
	}
	if r.Score != nil && r.Label == nil {
		return json.Marshal(*r.Score)
	}
	return nil, fmt.Errorf("contract: decision reply requires exactly one scalar")
}
func (r *DecisionReply) UnmarshalJSON(data []byte) error {
	*r = DecisionReply{}
	if len(data) > 0 && data[0] == '"' {
		return json.Unmarshal(data, &r.Label)
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return fmt.Errorf("contract: decision reply cannot be null")
	}
	return json.Unmarshal(data, &r.Score)
}

type DecisionAnswer struct {
	Reply      *DecisionReply `json:"reply,omitempty"`
	Confidence *float64       `json:"confidence,omitempty"`

	ID            string       `json:"id"`
	Kind          DecisionKind `json:"kind"`
	Probabilities []float64    `json:"probabilities,omitempty"`
	Mean          *float64     `json:"mean,omitempty"`
	Distribution  []float64    `json:"distribution,omitempty"`
	Probability   *float64     `json:"probability,omitempty"`
}

type DecisionResult struct {
	SchemaVersion int              `json:"schemaVersion"`
	RequestID     RequestID        `json:"requestId"`
	Model         ModelReference   `json:"model"`
	Data          []DecisionAnswer `json:"data"`
	Usage         UsageReport      `json:"usage"`
}

func (d DecisionInput) Validate() error {
	if len(d.Questions) < 1 || len(d.Questions) > 255 || utf16Length(d.State) > 65536 || (d.Effort != nil && !isMember(*d.Effort, decisionEffortValues)) {
		return fmt.Errorf("contract: invalid decisions state, questions or effort")
	}
	base := len(d.State)
	if d.Instructions != nil {
		if !decisionText(*d.Instructions) {
			return fmt.Errorf("contract: invalid decisions instructions")
		}
		base += len(*d.Instructions)
	}
	total, longest := base, 0
	seen := map[string]bool{}
	for _, q := range d.Questions {
		if q.ID == "" || utf16Length(q.ID) > 128 || seen[q.ID] || !decisionText(q.Question) {
			return fmt.Errorf("contract: invalid or repeated decision question")
		}
		seen[q.ID] = true
		size := len(q.ID) + len(q.Question)
		if q.Criteria != nil {
			if !decisionText(*q.Criteria) {
				return fmt.Errorf("contract: invalid decision criteria")
			}
			size += len(*q.Criteria)
		}
		var labels []string
		switch q.Kind {
		case "choice":
			if len(q.Options) < 2 || len(q.Options) > 255 || q.Levels != nil {
				return fmt.Errorf("contract: invalid choice options")
			}
			labels = q.Options
		case "score":
			if len(q.Levels) < 2 || len(q.Levels) > 10 || q.Options != nil {
				return fmt.Errorf("contract: invalid score levels")
			}
			labels = q.Levels
		case "noul":
			if q.Levels != nil || q.Options != nil {
				return fmt.Errorf("contract: noul carries no labels")
			}
		default:
			return fmt.Errorf("contract: unknown decision kind")
		}
		unique := map[string]bool{}
		for _, label := range labels {
			if !decisionText(label) || unique[label] {
				return fmt.Errorf("contract: invalid or repeated decision label")
			}
			unique[label] = true
			size += len(label)
		}
		total += size
		longest = max(longest, size)
	}
	if total > 65536 || base+longest > 32768 {
		return fmt.Errorf("contract: decisions exceed UTF-8 byte budget")
	}
	return nil
}

func decisionText(s string) bool { return len(s) > 0 && utf16Length(s) <= 65536 }
func decisionProbability(p float64) bool {
	return !math.IsNaN(p) && !math.IsInf(p, 0) && p >= 0 && p <= 1
}

// ValidateDecisionAnswers binds every answer to the exact question and ordered
// labels, without inventing confidence or replacing malformed probabilities.
func ValidateDecisionAnswers(input DecisionInput, answers []DecisionAnswer) error {
	if len(answers) != len(input.Questions) {
		return fmt.Errorf("contract: decision answer count differs")
	}
	questions := make(map[string]DecisionQuestion, len(input.Questions))
	for _, q := range input.Questions {
		questions[q.ID] = q
	}
	for _, a := range answers {
		q, ok := questions[a.ID]
		if !ok || a.Kind != q.Kind {
			return fmt.Errorf("contract: decision answer identity differs")
		}
		delete(questions, a.ID)
		if a.Kind != "noul" && (a.Confidence == nil || !decisionProbability(*a.Confidence) || a.Reply == nil) {
			return fmt.Errorf("contract: decisions require actual reply and confidence")
		}
		var values []float64
		switch a.Kind {
		case "choice":
			if len(a.Probabilities) != len(q.Options) || a.Mean != nil || a.Probability != nil || a.Distribution != nil {
				return fmt.Errorf("contract: invalid choice answer")
			}
			if a.Reply.Label == nil || a.Reply.Score != nil {
				return fmt.Errorf("contract: invalid choice reply")
			}
			selected := -1
			highest := 0.0
			for i, label := range q.Options {
				if label == *a.Reply.Label {
					selected = i
				}
				highest = max(highest, a.Probabilities[i])
			}
			if selected < 0 || a.Probabilities[selected]+1e-6 < highest {
				return fmt.Errorf("contract: choice reply must select a maximum probability option")
			}
			values = a.Probabilities
		case "score":
			if len(a.Distribution) != len(q.Levels) || a.Mean == nil || a.Probability != nil || a.Probabilities != nil {
				return fmt.Errorf("contract: invalid score answer")
			}
			if a.Reply.Score == nil || a.Reply.Label != nil || math.IsNaN(*a.Reply.Score) || math.IsInf(*a.Reply.Score, 0) || *a.Reply.Score < 0 || *a.Reply.Score > 9 || math.Abs(*a.Reply.Score-*a.Mean) > 1e-6 {
				return fmt.Errorf("contract: score reply must match mean")
			}
			values = a.Distribution
		case "noul":
			if a.Reply != nil || a.Confidence != nil || a.Probability == nil || !decisionProbability(*a.Probability) || a.Mean != nil || a.Probabilities != nil || a.Distribution != nil {
				return fmt.Errorf("contract: invalid noul answer")
			}
			continue
		default:
			return fmt.Errorf("contract: unknown decision answer kind")
		}
		sum, mean := 0.0, 0.0
		for i, p := range values {
			if !decisionProbability(p) {
				return fmt.Errorf("contract: invalid decision probability")
			}
			sum += p
			mean += float64(i) * p
		}
		if math.Abs(sum-1) > 1e-6 {
			return fmt.Errorf("contract: decision probabilities do not sum to one")
		}
		if a.Kind == "score" && (math.IsNaN(*a.Mean) || math.IsInf(*a.Mean, 0) || *a.Mean < 0 || *a.Mean > 9 || math.Abs(*a.Mean-mean) > 1e-6) {
			return fmt.Errorf("contract: decision score differs from distribution")
		}
	}
	return nil
}

// ValidateDecisionsEnvelope also runs in Translate, before any upstream spend.
func (r *Request) ValidateDecisionsEnvelope() error {
	if r.Client.APIFormat != APIFormatDecisions || r.Input.Format != InputDecisions || r.Input.Decisions == nil || r.Modality != ModalityText || r.Stream || r.Target.Kind != TargetModel || r.Target.ModelReference == nil || !r.Target.ModelReference.Pinned() || r.MaxOutputTokens != nil || len(r.Tools) != 0 || r.ToolChoice != nil || r.ResponseFormat != nil || r.Reasoning != nil || r.Speech != nil || r.AudioOutput != nil || r.Input.Messages != nil || r.Input.Text != nil || r.Input.Texts != nil {
		return fmt.Errorf("contract: decisions require exact-model typed nonstreaming text without generation controls")
	}
	for _, route := range r.AuthorizedRoutes {
		if route.ModelReference != *r.Target.ModelReference || route.Substitution != SubstitutionSameModel {
			return fmt.Errorf("contract: decisions cannot substitute model revisions")
		}
	}
	s := r.Sampling
	if s.Temperature != nil || s.TopP != nil || s.TopK != nil || s.FrequencyPenalty != nil || s.PresencePenalty != nil || s.Seed != nil || s.StopSequences != nil {
		return fmt.Errorf("contract: decisions carry no sampling controls")
	}
	return r.Input.Decisions.Validate()
}

// UnmarshalJSON distinguishes a required empty state from an absent/null one.
// Unknown additive fields retain the envelope's forward-compatible decoding.
func (d *DecisionInput) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	for _, field := range []string{"state", "questions"} {
		raw, present := fields[field]
		if !present || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("contract: missing decisions %s", field)
		}
	}
	for _, field := range []string{"instructions", "effort"} {
		if raw, present := fields[field]; present && bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return fmt.Errorf("contract: decisions %s cannot be null", field)
		}
	}
	type wire DecisionInput
	var decoded wire
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*d = DecisionInput(decoded)
	return nil
}
