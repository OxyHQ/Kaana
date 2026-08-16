package contract

// Every closed vocabulary in the contract, restated as a Go named type with its
// declared member list beside it.
//
// The member lists are what contract_test.go compares against the published
// enums, so a member added, removed or renamed upstream fails the build here
// rather than becoming an unhandled default branch at runtime.

// Environment is the credential environment a request was issued into.
type Environment string

const (
	EnvironmentDevelopment Environment = "development"
	EnvironmentStaging     Environment = "staging"
	EnvironmentProduction  Environment = "production"
)

var environmentValues = []Environment{EnvironmentDevelopment, EnvironmentStaging, EnvironmentProduction}

// Scope is one of the inference capabilities the data plane is told about. A
// credential may carry many other Oxy scopes; only these cross the boundary.
type Scope string

const (
	ScopeInvoke         Scope = "inference:invoke"
	ScopeModelsRead     Scope = "inference:models:read"
	ScopeUsageRead      Scope = "inference:usage:read"
	ScopeRoutingRead    Scope = "inference:routing:read"
	ScopeRoutingWrite   Scope = "inference:routing:write"
	ScopeProvidersRead  Scope = "inference:providers:read"
	ScopeProvidersWrite Scope = "inference:providers:write"
)

var scopeValues = []Scope{
	ScopeInvoke, ScopeModelsRead, ScopeUsageRead,
	ScopeRoutingRead, ScopeRoutingWrite, ScopeProvidersRead, ScopeProvidersWrite,
}

// Modality is what a request consumes or produces.
type Modality string

const (
	ModalityText      Modality = "text"
	ModalityImage     Modality = "image"
	ModalityAudio     Modality = "audio"
	ModalityVideo     Modality = "video"
	ModalityEmbedding Modality = "embedding"
)

var modalityValues = []Modality{ModalityText, ModalityImage, ModalityAudio, ModalityVideo, ModalityEmbedding}

// MessageRole is the role of one normalized message.
type MessageRole string

const (
	RoleSystem    MessageRole = "system"
	RoleDeveloper MessageRole = "developer"
	RoleUser      MessageRole = "user"
	RoleAssistant MessageRole = "assistant"
	RoleTool      MessageRole = "tool"
)

var messageRoleValues = []MessageRole{RoleSystem, RoleDeveloper, RoleUser, RoleAssistant, RoleTool}

// APIFormat is the public dialect the customer called. The response is rendered
// back in it, which is the only reason the data plane is told.
type APIFormat string

const (
	APIFormatResponses           APIFormat = "responses"
	APIFormatChatCompletions     APIFormat = "chat_completions"
	APIFormatEmbeddings          APIFormat = "embeddings"
	APIFormatImagesGenerations   APIFormat = "images_generations"
	APIFormatAudioTranscriptions APIFormat = "audio_transcriptions"
	APIFormatAudioSpeech         APIFormat = "audio_speech"
	APIFormatRerank              APIFormat = "rerank"
	APIFormatBatches             APIFormat = "batches"
)

var apiFormatValues = []APIFormat{
	APIFormatResponses, APIFormatChatCompletions, APIFormatEmbeddings, APIFormatImagesGenerations,
	APIFormatAudioTranscriptions, APIFormatAudioSpeech, APIFormatRerank, APIFormatBatches,
}

// ImageDetail is a provider-independent resolution hint.
type ImageDetail string

const (
	ImageDetailAuto ImageDetail = "auto"
	ImageDetailLow  ImageDetail = "low"
	ImageDetailHigh ImageDetail = "high"
)

var imageDetailValues = []ImageDetail{ImageDetailAuto, ImageDetailLow, ImageDetailHigh}

// ToolChoiceMode is the non-object half of the tool-choice union.
type ToolChoiceMode string

const (
	ToolChoiceAuto     ToolChoiceMode = "auto"
	ToolChoiceNone     ToolChoiceMode = "none"
	ToolChoiceRequired ToolChoiceMode = "required"
)

var toolChoiceModeValues = []ToolChoiceMode{ToolChoiceAuto, ToolChoiceNone, ToolChoiceRequired}

// DeltaChannel separates visible output from reasoning and refusals. A client
// that renders reasoning as answer text is a product bug, not a preference.
type DeltaChannel string

const (
	ChannelOutputText DeltaChannel = "output_text"
	ChannelReasoning  DeltaChannel = "reasoning"
	ChannelRefusal    DeltaChannel = "refusal"
)

var deltaChannelValues = []DeltaChannel{ChannelOutputText, ChannelReasoning, ChannelRefusal}

// FinishReason is why generation stopped.
type FinishReason string

const (
	FinishStop          FinishReason = "stop"
	FinishLength        FinishReason = "length"
	FinishToolCalls     FinishReason = "tool_calls"
	FinishContentFilter FinishReason = "content_filter"
	FinishCancelled     FinishReason = "cancelled"
)

var finishReasonValues = []FinishReason{FinishStop, FinishLength, FinishToolCalls, FinishContentFilter, FinishCancelled}

// RouteSwitchReason is why a route changed mid-request.
type RouteSwitchReason string

const (
	SwitchDeploymentUnavailable RouteSwitchReason = "deployment_unavailable"
	SwitchProviderError         RouteSwitchReason = "provider_error"
	SwitchProviderTimeout       RouteSwitchReason = "provider_timeout"
	SwitchProviderOverloaded    RouteSwitchReason = "provider_overloaded"
	SwitchRateLimited           RouteSwitchReason = "rate_limited"
	SwitchCapacity              RouteSwitchReason = "capacity"
	SwitchPolicyPreference      RouteSwitchReason = "policy_preference"
)

var routeSwitchReasonValues = []RouteSwitchReason{
	SwitchDeploymentUnavailable, SwitchProviderError, SwitchProviderTimeout,
	SwitchProviderOverloaded, SwitchRateLimited, SwitchCapacity, SwitchPolicyPreference,
}

// RequestOutcome is how a request ended, from the data plane's point of view.
type RequestOutcome string

const (
	OutcomeCompleted RequestOutcome = "completed"
	OutcomePartial   RequestOutcome = "partial"
	OutcomeCancelled RequestOutcome = "cancelled"
	OutcomeFailed    RequestOutcome = "failed"
)

var requestOutcomeValues = []RequestOutcome{OutcomeCompleted, OutcomePartial, OutcomeCancelled, OutcomeFailed}

// UsageUnit is a unit inference is metered in. Time is integer milliseconds so
// that no unit quantity is ever fractional.
type UsageUnit string

const (
	UnitInputTokens             UsageUnit = "input_tokens"
	UnitCachedInputTokens       UsageUnit = "cached_input_tokens"
	UnitOutputTokens            UsageUnit = "output_tokens"
	UnitReasoningTokens         UsageUnit = "reasoning_tokens"
	UnitRequests                UsageUnit = "requests"
	UnitImages                  UsageUnit = "images"
	UnitAudioInputMilliseconds  UsageUnit = "audio_input_milliseconds"
	UnitAudioOutputMilliseconds UsageUnit = "audio_output_milliseconds"
	UnitVideoMilliseconds       UsageUnit = "video_milliseconds"
	UnitCharacters              UsageUnit = "characters"
	UnitEmbeddings              UsageUnit = "embeddings"
)

var usageUnitValues = []UsageUnit{
	UnitInputTokens, UnitCachedInputTokens, UnitOutputTokens, UnitReasoningTokens,
	UnitRequests, UnitImages, UnitAudioInputMilliseconds, UnitAudioOutputMilliseconds,
	UnitVideoMilliseconds, UnitCharacters, UnitEmbeddings,
}

// Valid reports whether the unit is one the contract declares. A unit that is
// not is a unit nothing can be settled or priced against, so a configuration
// naming one is refused where it is read rather than where it is summed.
func (u UsageUnit) Valid() bool { return isMember(u, usageUnitValues) }

// UsageSource records where a metered quantity came from. An estimate that is
// indistinguishable from a reported number is an estimate nobody can reconcile.
type UsageSource string

const (
	UsageProviderReported UsageSource = "provider_reported"
	UsageOxyMeasured      UsageSource = "oxy_measured"
	UsageEstimated        UsageSource = "estimated"
)

var usageSourceValues = []UsageSource{UsageProviderReported, UsageOxyMeasured, UsageEstimated}

// UpstreamErrorCategory is Oxy's own reading of what kind of upstream failure
// occurred, in a vocabulary that is the same across every provider.
type UpstreamErrorCategory string

const (
	UpstreamRateLimit      UpstreamErrorCategory = "rate_limit"
	UpstreamQuota          UpstreamErrorCategory = "quota"
	UpstreamTimeout        UpstreamErrorCategory = "timeout"
	UpstreamOverloaded     UpstreamErrorCategory = "overloaded"
	UpstreamServerError    UpstreamErrorCategory = "server_error"
	UpstreamContentFilter  UpstreamErrorCategory = "content_filter"
	UpstreamInvalidReq     UpstreamErrorCategory = "invalid_request"
	UpstreamAuthentication UpstreamErrorCategory = "authentication"
	UpstreamUnknown        UpstreamErrorCategory = "unknown"
)

var upstreamErrorCategoryValues = []UpstreamErrorCategory{
	UpstreamRateLimit, UpstreamQuota, UpstreamTimeout, UpstreamOverloaded, UpstreamServerError,
	UpstreamContentFilter, UpstreamInvalidReq, UpstreamAuthentication, UpstreamUnknown,
}
