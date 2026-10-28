package contract

import (
	"encoding/base64"
	"strings"
	"testing"
)

const pixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="

func inlineImage(mediaType, data string) ContentPart {
	return ContentPart{Type: ContentPartImage, Source: &ContentSource{Kind: ContentSourceInline, MediaType: &mediaType, Data: &data}}
}

func TestCheckImageInputAdmitsInlineImagesOnUserAndToolMessages(t *testing.T) {
	text := "what does the tab show?"
	call := "call_1"
	messages := []Message{
		{Role: RoleUser, Content: []ContentPart{{Type: ContentPartText, Text: &text}, inlineImage("image/png", pixelPNG), inlineImage("image/jpeg", pixelPNG), inlineImage("image/webp", pixelPNG), inlineImage("image/gif", pixelPNG)}},
		{Role: RoleTool, ToolCallID: &call, Content: []ContentPart{inlineImage("image/jpeg", pixelPNG)}},
	}
	images, issues := CheckImageInput(messages)
	if images != 5 || len(issues) != 0 {
		t.Fatalf("got %d images and %v; want 5 and none", images, issues)
	}
	if images, issues := CheckImageInput([]Message{{Role: RoleUser, Content: []ContentPart{{Type: ContentPartText, Text: &text}}}}); images != 0 || issues != nil {
		t.Fatalf("a text conversation: got %d images and %v", images, issues)
	}
}

func TestCheckImageInputRefusesWhatNoRouteMayCarry(t *testing.T) {
	url := "http://169.254.169.254/latest/meta-data/"
	atBound := strings.Repeat("A", MaxInputImageBytes/3*4)
	overBound := strings.Repeat("A", (MaxInputImageBytes/3+1)*4)
	messages := []Message{
		{Role: RoleUser, Content: []ContentPart{
			{Type: ContentPartImage, Source: &ContentSource{Kind: ContentSourceURL, URL: &url}},
			inlineImage("image/tiff", pixelPNG),
			inlineImage("image/png", "not base64!"),
			inlineImage("image/png", pixelPNG+"\n"),
			inlineImage("image/png", overBound),
			inlineImage("image/png", atBound),
		}},
		{Role: RoleAssistant, Content: []ContentPart{inlineImage("image/png", pixelPNG)}},
	}
	_, issues := CheckImageInput(messages)
	got := make([]string, 0, len(issues))
	for _, issue := range issues {
		got = append(got, issue.Param("input"))
	}
	want := []string{
		"input[0].content[0].source",
		"input[0].content[1].source.mediaType",
		"input[0].content[2].source.data",
		"input[0].content[3].source.data",
		"input[0].content[4].source.data",
		"input[1].content[0].role",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("issues at\n  %v\nwant\n  %v", got, want)
	}
}

func TestCheckImageInputCountsAcrossTheConversation(t *testing.T) {
	many := make([]Message, 0, MaxInputImages+1)
	for range MaxInputImages {
		many = append(many, Message{Role: RoleUser, Content: []ContentPart{inlineImage("image/png", pixelPNG)}})
	}
	if images, issues := CheckImageInput(many); images != MaxInputImages || len(issues) != 0 {
		t.Fatalf("%d images: %v", images, issues)
	}
	many = append(many, Message{Role: RoleUser, Content: []ContentPart{inlineImage("image/png", pixelPNG)}})
	images, issues := CheckImageInput(many)
	if images != MaxInputImages+1 || len(issues) != 1 || issues[0].Field != ImageIssueCount || issues[0].Param("input") != "input" {
		t.Fatalf("one image over: %d images, %v", images, issues)
	}
}

func TestImageIssuesNeverCarryTheImage(t *testing.T) {
	marker := base64.StdEncoding.EncodeToString([]byte("MARKER-IMAGE-BYTES-0123456789"))
	url := "https://example.test/" + marker
	_, issues := CheckImageInput([]Message{
		{Role: RoleSystem, Content: []ContentPart{inlineImage("image/bmp", marker)}},
		{Role: RoleUser, Content: []ContentPart{{Type: ContentPartImage, Source: &ContentSource{Kind: ContentSourceURL, URL: &url}}}},
	})
	if len(issues) == 0 {
		t.Fatal("no issue: the positive control failed")
	}
	for _, issue := range issues {
		if strings.Contains(issue.Message, marker) || strings.Contains(issue.Param("input"), marker) {
			t.Fatalf("an issue carries the image: %+v", issue)
		}
	}
}

func TestBase64DecodedLength(t *testing.T) {
	for _, value := range []string{"", "QQ==", "QUI=", "QUJD", pixelPNG} {
		decoded, err := base64.StdEncoding.DecodeString(value)
		length, ok := Base64DecodedLength(value)
		if value == "" {
			if ok {
				t.Fatal("the empty string is no image")
			}
			continue
		}
		if err != nil || !ok || length != len(decoded) {
			t.Fatalf("%q: counted %d (%v), decoded %d (%v)", value, length, ok, len(decoded), err)
		}
	}
	for _, value := range []string{"QQ", "QQ=", "Q===", "QU-D", "QU_D", "QUJD\n"} {
		if _, ok := Base64DecodedLength(value); ok {
			t.Fatalf("%q is not canonical base64", value)
		}
	}
}

func TestEnvelopeRefusesAnImageURL(t *testing.T) {
	request := validRequestWithImage(t)
	if err := request.Validate(); err != nil {
		t.Fatalf("an inline image is valid: %v", err)
	}
	url := "https://example.test/a.png"
	request.Input.Messages[0].Content[0] = ContentPart{Type: ContentPartImage, Source: &ContentSource{Kind: ContentSourceURL, URL: &url}}
	err := request.Validate()
	if err == nil || !strings.Contains(err.Error(), "input.messages[0].content[0].source") {
		t.Fatalf("an image URL: %v", err)
	}
}

func validRequestWithImage(t *testing.T) Request {
	t.Helper()
	request := validAuthorizedRouteRequest()
	request.Input.Messages[0].Content = []ContentPart{inlineImage("image/png", pixelPNG)}
	return request
}
