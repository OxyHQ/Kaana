package contract

import (
	"fmt"
	"slices"
	"strings"
)

// The image rules of contract set 8.1.0 (ADR 0006), the Go statement of
// `checkImageInput`. Asserted against the published constants by
// descriptor_test.go.
const (
	// MaxInputImages is MAX_INPUT_IMAGES: the most image parts one request
	// carries across every message.
	MaxInputImages = 20
	// MaxInputImageBytes is MAX_INPUT_IMAGE_BYTES: the most decoded bytes one
	// image carries, 5 MB once base64-encoded.
	MaxInputImageBytes = 3_750_000
)

// inputImageMediaTypes is INPUT_IMAGE_MEDIA_TYPES, in the published order.
var inputImageMediaTypes = []string{"image/png", "image/jpeg", "image/webp", "image/gif"}

// ImageIssueField names the part of an image the rules refuse.
type ImageIssueField string

const (
	ImageIssueRole      ImageIssueField = "role"
	ImageIssueSource    ImageIssueField = "source"
	ImageIssueMediaType ImageIssueField = "source.mediaType"
	ImageIssueData      ImageIssueField = "source.data"
	ImageIssueCount     ImageIssueField = "count"
)

// ImageInputIssue is why one image cannot be carried. It names where and why,
// never the image: an image is payload, and payload reaches no error.
type ImageInputIssue struct {
	MessageIndex int
	// PartIndex is the image's index in its message; nil for the count.
	PartIndex *int
	Field     ImageIssueField
	Message   string
}

// Param renders the issue's location under a dialect's message list, e.g.
// `input[2].content[1].source.data` for prefix "input".
func (i ImageInputIssue) Param(prefix string) string {
	if i.PartIndex == nil {
		return prefix
	}
	return fmt.Sprintf("%s[%d].content[%d].%s", prefix, i.MessageIndex, *i.PartIndex, i.Field)
}

// CheckImageInput returns how many image parts the messages carry and every
// issue against the image rules, in message order: inline base64 only (no
// image URL is fetched, nor forwarded for a provider to fetch), one of the
// four formats, at most MaxInputImageBytes decoded, on a user or tool
// message, at most MaxInputImages a request.
func CheckImageInput(messages []Message) (int, []ImageInputIssue) {
	var issues []ImageInputIssue
	images := 0
	for messageIndex, message := range messages {
		for partIndex, part := range message.Content {
			if part.Type != ContentPartImage {
				continue
			}
			images++
			at := func(field ImageIssueField, text string) ImageInputIssue {
				index := partIndex
				return ImageInputIssue{MessageIndex: messageIndex, PartIndex: &index, Field: field, Message: text}
			}
			if message.Role != RoleUser && message.Role != RoleTool {
				issues = append(issues, at(ImageIssueRole, "an image rides on a user or tool message"))
			}
			source := part.Source
			if source == nil || source.Kind != ContentSourceInline {
				issues = append(issues, at(ImageIssueSource, "an image is sent inline as base64; no image URL is fetched"))
				continue
			}
			if source.MediaType == nil || !slices.Contains(inputImageMediaTypes, *source.MediaType) {
				issues = append(issues, at(ImageIssueMediaType, "an image is one of "+strings.Join(inputImageMediaTypes, ", ")))
			}
			var data string
			if source.Data != nil {
				data = *source.Data
			}
			switch bytes, ok := Base64DecodedLength(data); {
			case !ok:
				issues = append(issues, at(ImageIssueData, "image data is canonical base64"))
			case bytes > MaxInputImageBytes:
				issues = append(issues, at(ImageIssueData, fmt.Sprintf("an image is at most %d bytes", MaxInputImageBytes)))
			}
		}
	}
	if images > MaxInputImages {
		issues = append(issues, ImageInputIssue{
			MessageIndex: len(messages) - 1,
			Field:        ImageIssueCount,
			Message:      fmt.Sprintf("a request carries at most %d images", MaxInputImages),
		})
	}
	return images, issues
}

// Base64DecodedLength is the decoded length of canonical base64 (RFC 4648
// section 4: padded, no line breaks), counted without decoding; false when
// the text is not that.
func Base64DecodedLength(data string) (int, bool) {
	if len(data) == 0 || len(data)%4 != 0 {
		return 0, false
	}
	padding := 0
	switch {
	case strings.HasSuffix(data, "=="):
		padding = 2
	case strings.HasSuffix(data, "="):
		padding = 1
	}
	for _, character := range data[:len(data)-padding] {
		switch {
		case character >= 'A' && character <= 'Z', character >= 'a' && character <= 'z',
			character >= '0' && character <= '9', character == '+', character == '/':
		default:
			return 0, false
		}
	}
	return len(data)/4*3 - padding, true
}
