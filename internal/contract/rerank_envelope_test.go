package contract

import (
	"strings"
	"testing"
)

func validRerankEnvelope() Request {
	request := validAuthorizedRouteRequest()
	request.Modality = ModalityText
	request.Client.APIFormat = APIFormatRerank
	request.Input = Input{Format: InputText, Text: pointerTo("which is about foxes")}
	request.Rerank = &RerankParameters{Documents: []string{"a fox", "a dog"}, TopN: pointerTo(1)}
	return request
}

// Contract set 8.15.0: the rerank member is valid only as a whole rerank
// request, and a rerank request is valid only with its member.
func TestARerankEnvelopeNeedsItsDocumentsAndNothingElseHasThem(t *testing.T) {
	valid := validRerankEnvelope()
	if err := valid.Validate(); err != nil {
		t.Fatalf("a rerank envelope is invalid: %v", err)
	}
	many := make([]string, MaxRerankDocuments+1)
	for index := range many {
		many[index] = "d"
	}
	for name, mutate := range map[string]func(*Request){
		"no member":                     func(r *Request) { r.Rerank = nil },
		"member on a chat request":      func(r *Request) { r.Client.APIFormat = APIFormatChatCompletions },
		"empty query":                   func(r *Request) { r.Input.Text = pointerTo("") },
		"query as messages":             func(r *Request) { r.Input = Input{Format: InputTextBatch, Texts: []string{"q"}} },
		"streamed":                      func(r *Request) { r.Stream = true },
		"embedding modality":            func(r *Request) { r.Modality = ModalityEmbedding },
		"no documents":                  func(r *Request) { r.Rerank.Documents = nil },
		"too many documents":            func(r *Request) { r.Rerank.Documents = many },
		"an empty document":             func(r *Request) { r.Rerank.Documents = []string{"a", ""} },
		"top_n of zero":                 func(r *Request) { r.Rerank.TopN = pointerTo(0) },
		"member on a decisions request": func(r *Request) { r.Client.APIFormat = APIFormatDecisions; r.Input = Input{Format: InputDecisions} },
	} {
		request := validRerankEnvelope()
		mutate(&request)
		err := request.Validate()
		if err == nil {
			t.Errorf("%s: validated", name)
			continue
		}
		if strings.Contains(err.Error(), "a fox") {
			t.Errorf("%s: the error repeats customer text: %v", name, err)
		}
	}
}
