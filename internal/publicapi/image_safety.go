package publicapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
)

// ImageSafetyVerdict is what Kaana's own content check answered about a
// prompt or about generated images (ADR 0013). The zero value is "no answer":
// a check that returns it without an error has not allowed anything.
type ImageSafetyVerdict int

const (
	// ImageSafetyUnanswered is the zero value: the check did not answer, and
	// what it was asked about is refused.
	ImageSafetyUnanswered ImageSafetyVerdict = iota
	// ImageSafetyAllowed is the only verdict that lets a prompt reach a
	// provider or an image reach the caller.
	ImageSafetyAllowed
	// ImageSafetyRefused is the check's own refusal of the content.
	ImageSafetyRefused
)

// ImageSafety is Kaana's own content check on image generation (ADR 0013):
// one question about the prompt before anything is granted, one about the
// generated images before the caller receives them. The provider's filter is
// not this check and never stands in for it.
//
// An implementation answers ImageSafetyAllowed only when it judged the content
// and found it acceptable. Anything it could not judge is an error: an image
// it holds no bytes of, a classifier that timed out, an answer it could not
// read. The handler refuses on every error and on every verdict other than
// ImageSafetyAllowed, so no failure of the check serves an image.
//
// The prompt and the images are customer content. An implementation never
// persists or logs them, and returns no error that carries them.
type ImageSafety interface {
	CheckPrompt(ctx context.Context, prompt string) (ImageSafetyVerdict, error)
	CheckImages(ctx context.Context, prompt string, images []provider.GeneratedImage) (ImageSafetyVerdict, error)
}

// Image safety vocabularies: modelSafetyMetadataSchema's two enums.
var (
	contentFilteringDefaults = []string{"none", "provider_default", "strict"}
	provenanceMarkings       = []string{"none", "visible_watermark", "invisible_watermark", "c2pa"}
)

const (
	contentFilteringStrict = "strict"
	provenanceMarkingNone  = "none"
)

// declaredSafety is a declared model's safety member (modelSafetyMetadataSchema).
type declaredSafety struct {
	SafetyCardURL           *string  `json:"safetyCardUrl"`
	ContentFilteringDefault string   `json:"contentFilteringDefault"`
	KnownLimitations        []string `json:"knownLimitations"`
	ProvenanceMarking       string   `json:"provenanceMarking"`
}

// parseDeclaredSafety holds a declared safety member to the contract's
// vocabulary. A word outside it would otherwise read as "not strict" or, for
// the marking, as "not none": the second would open image generation on a
// typo.
func parseDeclaredSafety(raw json.RawMessage) (*declaredSafety, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var safety declaredSafety
	if err := decoder.Decode(&safety); err != nil {
		return nil, errors.New("is not modelSafetyMetadataSchema")
	}
	if !slices.Contains(contentFilteringDefaults, safety.ContentFilteringDefault) {
		return nil, errors.New("contentFilteringDefault is none, provider_default or strict")
	}
	if !slices.Contains(provenanceMarkings, safety.ProvenanceMarking) {
		return nil, errors.New("provenanceMarking is none, visible_watermark, invisible_watermark or c2pa")
	}
	return &safety, nil
}

// DeclaresImageGenerationSafety is whether the declared catalogue states, for
// a model line, the two facts image generation is served on (ADR 0013): strict
// content filtering and a provenance marking other than none. An undeclared
// line, or one that declares no safety member, states neither.
func (c *PublicCatalogue) DeclaresImageGenerationSafety(line contract.ModelID) bool {
	if c == nil {
		return false
	}
	model, declared := c.models[line]
	if !declared || model.safety == nil {
		return false
	}
	return model.safety.ContentFilteringDefault == contentFilteringStrict && model.safety.ProvenanceMarking != provenanceMarkingNone
}

// ExecutesFamily is whether a route could perform a request family: its
// adapter declares the format and its upstream model, where the id names its
// family, is of that family.
func ExecutesFamily(formats FormatKinds, route provider.Route, format contract.APIFormat) bool {
	if executes, _ := formats.ExecutesFormat(route.Provider, format); !executes {
		return false
	}
	if family, classified, _ := providerconfig.ClassifyModel(route.Provider, route.UpstreamModelID); classified && family.Format != format {
		return false
	}
	return true
}

// Public messages of the image content check. They name the check and the
// part, never the content or what the check found in it.
const (
	imagePromptRefusedMessage    = "the prompt is outside Kaana's content policy for image generation"
	imageOutputRefusedMessage    = "the generated image is outside Kaana's content policy for image generation and is withheld"
	imageCheckUnavailableMessage = "Kaana's content check for image generation could not answer; nothing is served without it"
)

// imageVerdict turns one answer of the content check into the request's fate:
// nil only for an explicit ImageSafetyAllowed without an error. A refusal is
// policy_violation, naming the request field at fault when there is one; no
// answer is service_unavailable, retryable, because the same request may pass
// once the check answers. Neither names the content.
func (s *Server) imageVerdict(requestID contract.RequestID, stage string, verdict ImageSafetyVerdict, err error, refused, param string) *contract.PublicError {
	switch {
	case err == nil && verdict == ImageSafetyAllowed:
		return nil
	case err == nil && verdict == ImageSafetyRefused:
		s.logger.Info("image content check refused", "requestId", requestID, "stage", stage)
		failure := contract.NewPublicError(requestID, "policy_violation", refused)
		if param != "" {
			failure = failure.WithParam(param)
		}
		return failure
	}
	// The error is never logged: an implementation's error may quote what it
	// was asked about.
	s.logger.Warn("image content check did not answer; the request is refused", "requestId", requestID, "stage", stage)
	return contract.NewPublicError(requestID, "service_unavailable", imageCheckUnavailableMessage)
}

// checkImagePrompt asks the content check about the prompt.
func (s *Server) checkImagePrompt(ctx context.Context, requestID contract.RequestID, prompt string) *contract.PublicError {
	verdict, err := s.imageSafety.CheckPrompt(ctx, prompt)
	return s.imageVerdict(requestID, "prompt", verdict, err, imagePromptRefusedMessage, "prompt")
}

// checkGeneratedImages asks the content check about what a provider drew.
func (s *Server) checkGeneratedImages(ctx context.Context, requestID contract.RequestID, prompt string, images []provider.GeneratedImage) *contract.PublicError {
	verdict, err := s.imageSafety.CheckImages(ctx, prompt, images)
	return s.imageVerdict(requestID, "output", verdict, err, imageOutputRefusedMessage, "")
}
