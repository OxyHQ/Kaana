package openaicompat

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/OxyHQ/Kaana/internal/contract"
	"github.com/OxyHQ/Kaana/internal/provider"
	"github.com/OxyHQ/Kaana/internal/providerconfig"
)

// Text-to-image on an OpenAI-compatible slug: `POST {base}/images/generations`.
// Which slugs speak it is providerconfig.ExecutableAPIFormats' answer, and
// which of a slug's models are image models is providerconfig.ClassifyModel's.
// The controls the caller named (n, size, quality, style, response_format) are
// sent as named and nothing else is ever added or defaulted: a control the
// model does not take is the provider's 400, which is not the deployment's
// fault and is not retried elsewhere.

const (
	imagesPath = "/images/generations"
	// maxImageResponseBytes bounds one buffered answer: ten base64 images.
	maxImageResponseBytes = 128 << 20
)

type imageGenerationRequest struct {
	Model          string  `json:"model"`
	Prompt         string  `json:"prompt"`
	N              *int    `json:"n,omitempty"`
	Size           *string `json:"size,omitempty"`
	Quality        *string `json:"quality,omitempty"`
	Style          *string `json:"style,omitempty"`
	ResponseFormat *string `json:"response_format,omitempty"`
}

type imageGenerationResponse struct {
	Data []struct {
		URL     string `json:"url"`
		B64JSON string `json:"b64_json"`
	} `json:"data"`
	Usage *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// isImageModel reports a deployment of this slug that only generates images.
func isImageModel(slug contract.ProviderSlug, upstreamModelID string) bool {
	family, classified, _ := providerconfig.ClassifyModel(slug, upstreamModelID)
	return classified && family.Format == contract.APIFormatImagesGenerations
}

func (a *Adapter) translateImage(request *contract.Request, route provider.Route) (*provider.Call, error) {
	if !provider.Executes(a, contract.APIFormatImagesGenerations) || route.Provider != a.config.Provider || !isImageModel(a.config.Provider, route.UpstreamModelID) {
		return nil, provider.ErrUnsupported{Code: contract.CodeUnsupportedModality, Param: "modality", Detail: "this deployment does not generate images"}
	}
	switch {
	case request.Modality != contract.ModalityImage || request.Input.Format != contract.InputText || request.Input.Text == nil:
		return nil, provider.ErrUnsupported{Code: contract.CodeInvalidRequest, Param: "input", Detail: "image generation takes a text prompt"}
	case request.Stream:
		return nil, provider.ErrUnsupported{Code: contract.CodeInvalidRequest, Param: "stream", Detail: "image generation is non-streaming"}
	}
	body := imageGenerationRequest{Model: route.UpstreamModelID, Prompt: *request.Input.Text}
	if image := request.Image; image != nil {
		body.N, body.Size, body.Quality, body.Style = image.N, image.Size, image.Quality, image.Style
		if image.ResponseFormat != nil {
			format := string(*image.ResponseFormat)
			body.ResponseFormat = &format
		}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("openaicompat: encoding image request: %w", err)
	}
	headers := make(http.Header, len(a.config.Headers)+1)
	for name, value := range a.config.Headers {
		headers.Set(name, value)
	}
	headers.Set("Content-Type", "application/json")
	return &provider.Call{Route: route, Method: http.MethodPost, URL: a.config.BaseURL + imagesPath, Body: encoded, Header: headers}, nil
}

// isImageCall reports a call translateImage built.
func isImageCall(call *provider.Call) bool {
	return !call.Stream && strings.HasSuffix(call.URL, imagesPath)
}

// noImages is the failure of an attempt that answered 200 and produced no
// image. It is the deployment's (a warming-up or degraded model answers this
// way), so it is a server error the breaker, the retry and failover read like
// any other, and the customer is not billed for it.
func noImages(slug contract.ProviderSlug) error {
	return provider.ErrUpstream{
		Code:     contract.CodeProviderError,
		Category: contract.UpstreamServerError,
		Detail:   fmt.Sprintf("%s completed the image request without producing any image", slug),
	}
}

// readImages decodes the answer. Entries with neither a url nor inline data
// are dropped; an answer left with none is a failure, and one with fewer than
// requested is served and metered as what it is.
func (a *Adapter) readImages(body io.Reader, requested int) (provider.Outcome, error) {
	outcome := provider.Outcome{UsageSource: contract.UsageProviderReported}
	var response imageGenerationResponse
	if err := json.NewDecoder(io.LimitReader(body, maxImageResponseBytes+1)).Decode(&response); err != nil {
		return outcome, provider.ErrUpstream{Code: contract.CodeProviderError, Category: contract.UpstreamUnknown, Detail: "unreadable image response"}
	}
	images := make([]provider.GeneratedImage, 0, len(response.Data))
	for _, entry := range response.Data {
		switch {
		case entry.B64JSON != "":
			images = append(images, provider.GeneratedImage{B64JSON: entry.B64JSON})
		case entry.URL != "":
			images = append(images, provider.GeneratedImage{URL: entry.URL})
		}
	}
	if len(images) == 0 {
		return outcome, noImages(a.config.Provider)
	}
	if len(images) > requested {
		return outcome, provider.ErrUpstream{Code: contract.CodeProviderError, Category: contract.UpstreamUnknown, Detail: "the provider returned more images than were requested"}
	}
	outcome.Image = &provider.ImageResult{Images: images}
	outcome.Units = []contract.UsageQuantity{{Unit: contract.UnitImages, Quantity: len(images)}}
	if usage := response.Usage; usage != nil {
		if usage.InputTokens > 0 {
			outcome.Units = append(outcome.Units, contract.UsageQuantity{Unit: contract.UnitInputTokens, Quantity: usage.InputTokens})
		}
		if usage.OutputTokens > 0 {
			outcome.Units = append(outcome.Units, contract.UsageQuantity{Unit: contract.UnitOutputTokens, Quantity: usage.OutputTokens})
		}
	}
	outcome.FinishReason = contract.FinishStop
	return outcome, nil
}
