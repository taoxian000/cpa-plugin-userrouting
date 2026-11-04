package userrouting

import "strings"

// CPA cloaks non-Claude model IDs in Anthropic model lists by reversing their
// runes and prepending this marker. Claude Messages decodes the IDs before
// routing, so catalog discovery and filtering must use the same model name.
const claudeCloakedModelPrefix = "claude-fable-5-dd-"

func decodeClaudeCatalogModelID(model string) string {
	if !strings.HasPrefix(model, claudeCloakedModelPrefix) {
		return model
	}
	encoded := strings.TrimPrefix(model, claudeCloakedModelPrefix)
	if encoded == "" {
		return model
	}
	runes := []rune(encoded)
	for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
		runes[i], runes[j] = runes[j], runes[i]
	}
	return string(runes)
}
