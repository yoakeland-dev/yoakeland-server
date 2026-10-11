package provider

import (
	"strings"
	"sync"

	"yoakeland/internal/agent"
)

// Picture support is a property of the pair (provider, model): an API provider
// can send an image inside a tool result, a CLI agent's subprocess can't take
// one at all. Providers that can implement agent.VisionProvider (SupportsImages
// below), and the agent offers the browser tool's `screenshot` action only when
// it does — a model that can't look at a picture shouldn't be told about an
// action that makes one (SPEC.md §4e).

// droppedImageNote stands in for a picture that wasn't sent because the model
// can't see pictures. It keeps a tool result from being empty once the only
// thing it carried is gone.
const droppedImageNote = "[the picture attached here wasn't sent: this model can't see pictures]"

// withoutImages returns history with the pictures removed from every message,
// when keeps is false, so a blind model's request never carries one. Stories
// that make this necessary even though the screenshot action is hidden from
// such a model: history keeps its pictures when the session switches to
// another model mid-conversation, and text-only OpenAI models reject a
// request that arrives with an image part (rather than ignoring it), which
// would wedge every later turn of that session.
func withoutImages(history []agent.Message, keeps bool) []agent.Message {
	if keeps {
		return history
	}
	copied := false
	for i := range history {
		if len(history[i].Images) == 0 {
			continue
		}
		if !copied {
			history = append([]agent.Message(nil), history...)
			copied = true
		}
		history[i].Images = nil
		if history[i].Content == "" {
			history[i].Content = droppedImageNote
		}
	}
	return history
}

// visionFromCatalog remembers, per lowercased model id, whether the last
// OpenRouter catalog listed the model as taking images
// (architecture.input_modalities contains "image"). Same lazy side table as
// openRouterContext, for the same reason: the question is asked long after a
// client listed models, and the catalog is the only source that actually
// reports this.
var visionFromCatalog sync.Map

// rememberVision notes what a catalog said about a model.
func rememberVision(model string, takesImages bool) {
	if id := strings.ToLower(strings.TrimSpace(model)); id != "" {
		visionFromCatalog.Store(id, takesImages)
	}
}

// takesImages reads an OpenRouter model's architecture object
// ({"input_modalities": ["text", "image"], ...}) for whether it takes pictures.
func takesImages(architecture any) bool {
	arch, ok := architecture.(map[string]any)
	if !ok {
		return false
	}
	modalities, ok := arch["input_modalities"].([]any)
	if !ok {
		return false
	}
	for _, m := range modalities {
		if s, _ := m.(string); strings.EqualFold(s, "image") {
			return true
		}
	}
	return false
}

// SupportsImages reports whether model can be sent a picture. A catalog that
// has said so decides; otherwise visionModelPrefixes below is the only
// knowledge we have, and an unlisted model is treated as blind — the cost of a
// gap is a screenshot action that isn't offered, where the opposite mistake
// (claiming a text-only model can see) means a tool result with an image the
// provider rejects.
func SupportsImages(model string) bool {
	id := strings.ToLower(strings.TrimSpace(model))
	if id == "" {
		return false
	}
	if takesImages, ok := visionFromCatalog.Load(id); ok {
		return takesImages.(bool)
	}
	// OpenRouter ids carry the vendor in front of the model's own name
	// ("anthropic/claude-sonnet-5", "meta-llama/llama-3.2-90b-vision-instruct"),
	// so the tables below match the name part.
	name := id
	if slash := strings.LastIndex(id, "/"); slash >= 0 {
		name = id[slash+1:]
	}
	for _, exception := range visionExceptions {
		if strings.Contains(name, exception) {
			return false
		}
	}
	// Some names say it themselves: llava, pixtral, qwen2.5-vl, and the rest of
	// the open-weight models that spell out "vision" or a -vl suffix.
	if strings.Contains(name, "vision") || strings.Contains(name, "-vl") {
		return true
	}
	for _, prefix := range visionModelPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// visionModelPrefixes are the model families this build knows take pictures.
// Hand-maintained, like openAIReasoningModels: Anthropic's and OpenAI's
// /v1/models endpoints don't report modality (OpenRouter's catalog does, see
// visionFromCatalog), so the families have to be spelled out.
var visionModelPrefixes = []string{
	// Anthropic: Claude 3 and everything after it.
	"claude-3", "claude-4", "claude-5", "claude-opus", "claude-sonnet", "claude-haiku",
	// OpenAI (gpt-4.5 and o1/o3/o4 are the ones that take images; plain gpt-4
	// and gpt-3.5 are text-only).
	"gpt-4o", "gpt-4.1", "gpt-4.5", "gpt-4-turbo", "gpt-4-vision", "gpt-5", "chatgpt-4o",
	"o1", "o3", "o4",
	// Google
	"gemini-",
	// Open-weight families served through a gateway or a local server
	"pixtral", "llava", "moondream", "internvl",
}

// visionExceptions are names a prefix above would claim but that in fact have
// no image input: the small OpenAI reasoning models, and the Codex ones.
var visionExceptions = []string{"o1-mini", "o1-preview", "o3-mini", "codex"}
