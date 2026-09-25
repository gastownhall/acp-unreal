package acpagent

import (
	"errors"
	"fmt"
	"strings"

	acp "github.com/coder/acp-go-sdk"
)

// ErrUnsupportedContent is returned for image, audio and blob content.
var ErrUnsupportedContent = errors.New("only text, resource and resource_link prompt content is supported")

// FlattenPrompt maps ACP content blocks onto the harness's text-only
// external input (contextbuilder/builder.go:43-61 decodes a JSON string).
func FlattenPrompt(blocks []acp.ContentBlock) (string, error) {
	var parts []string
	for _, block := range blocks {
		switch {
		case block.Text != nil:
			parts = append(parts, block.Text.Text)
		case block.Resource != nil && block.Resource.Resource.TextResourceContents != nil:
			resource := block.Resource.Resource.TextResourceContents
			parts = append(parts, fmt.Sprintf("--- %s ---\n%s", resource.Uri, resource.Text))
		case block.ResourceLink != nil:
			parts = append(parts, fmt.Sprintf("[Referenced file: %s]", block.ResourceLink.Uri))
		default:
			return "", ErrUnsupportedContent
		}
	}
	if len(parts) == 0 {
		return "", errors.New("prompt is empty")
	}
	return strings.Join(parts, "\n\n"), nil
}
