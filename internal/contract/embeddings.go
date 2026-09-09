package contract

const EmbeddingResponseSchemaVersion = 1

type EmbeddingVector struct {
	Index     int       `json:"index"`
	Embedding []float64 `json:"embedding"`
}

type EmbeddingUsage struct {
	InputTokens int `json:"inputTokens"`
	TotalTokens int `json:"totalTokens"`
}

type EmbeddingSuccess struct {
	SchemaVersion int               `json:"schemaVersion"`
	RequestID     RequestID         `json:"requestId"`
	Model         ModelReference    `json:"model"`
	Dimension     int               `json:"dimension"`
	Data          []EmbeddingVector `json:"data"`
	Usage         EmbeddingUsage    `json:"usage"`
}

type EmbeddingFailure struct {
	SchemaVersion int       `json:"schemaVersion"`
	RequestID     RequestID `json:"requestId"`
	Error         Error     `json:"error"`
}

type EmbeddingResponse struct{}
