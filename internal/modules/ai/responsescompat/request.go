package responsescompat

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type TranslatedRequest struct {
	Model        string
	Stream       bool
	OriginalBody []byte
	ChatBody     []byte
}

// TranslateRequest validates the supported Responses API subset and converts
// it to the OpenAI-compatible Chat Completions shape accepted by Workers AI.
func TranslateRequest(raw []byte) (TranslatedRequest, error) {
	if !gjson.ValidBytes(raw) {
		return TranslatedRequest{}, invalid("body", "The request body must be valid JSON.")
	}
	root := gjson.ParseBytes(raw)
	if !root.IsObject() {
		return TranslatedRequest{}, invalid("body", "The request body must be a JSON object.")
	}
	model := strings.TrimSpace(root.Get("model").String())
	if model == "" {
		return TranslatedRequest{}, invalid("model", "The model field is required.")
	}
	input := root.Get("input")
	if !input.Exists() {
		return TranslatedRequest{}, invalid("input", "The input field is required.")
	}
	if instructions := root.Get("instructions"); instructions.Exists() && instructions.Type != gjson.String {
		return TranslatedRequest{}, invalid("instructions", "The instructions field must be a string.")
	}
	if stream := root.Get("stream"); stream.Exists() && stream.Type != gjson.True && stream.Type != gjson.False {
		return TranslatedRequest{}, invalid("stream", "The stream field must be a boolean.")
	}
	for _, field := range []string{"max_output_tokens", "top_logprobs"} {
		if value := root.Get(field); value.Exists() {
			if value.Type != gjson.Number || value.Int() < 0 || value.Float() != float64(value.Int()) {
				return TranslatedRequest{}, invalid(field, "The %s field must be a non-negative integer.", field)
			}
		}
	}
	for _, field := range []string{"temperature", "top_p", "frequency_penalty", "presence_penalty"} {
		if value := root.Get(field); value.Exists() && value.Type != gjson.Number {
			return TranslatedRequest{}, invalid(field, "The %s field must be a number.", field)
		}
	}
	if reasoning := root.Get("reasoning"); reasoning.Exists() {
		if !reasoning.IsObject() {
			return TranslatedRequest{}, invalid("reasoning", "The reasoning field must be an object.")
		}
		if effort := reasoning.Get("effort"); effort.Exists() && effort.Type != gjson.String {
			return TranslatedRequest{}, invalid("reasoning.effort", "The reasoning effort must be a string.")
		}
	}
	if value := root.Get("background"); value.Exists() && value.Bool() {
		return TranslatedRequest{}, unsupported("background", "Background Responses jobs are not supported by Workers AI.")
	}
	if value := root.Get("store"); value.Exists() && value.Bool() {
		return TranslatedRequest{}, unsupported("store", "Stored Responses are not supported by Workers AI.")
	}
	if value := root.Get("previous_response_id"); value.Exists() && strings.TrimSpace(value.String()) != "" {
		return TranslatedRequest{}, unsupported("previous_response_id", "Server-side response history is not supported by Workers AI.")
	}
	if value := root.Get("conversation"); value.Exists() && value.Raw != "null" {
		return TranslatedRequest{}, unsupported("conversation", "Server-side conversations are not supported by Workers AI.")
	}
	if err := validateInput(input); err != nil {
		return TranslatedRequest{}, err
	}
	if err := validateTools(root.Get("tools")); err != nil {
		return TranslatedRequest{}, err
	}
	if err := validateToolChoice(root.Get("tool_choice")); err != nil {
		return TranslatedRequest{}, err
	}

	stream := root.Get("stream").Bool()
	format, err := validateTextFormat(root.Get("text.format"))
	if err != nil {
		return TranslatedRequest{}, err
	}
	if stream && format != "" && format != "text" {
		return TranslatedRequest{}, unsupported("stream", "Workers AI does not support streamed JSON mode for Responses requests.")
	}

	original := append([]byte(nil), raw...)
	chat := ConvertOpenAIResponsesRequestToOpenAIChatCompletions(model, original, stream)
	chat = applyGenerationParameters(root, chat)
	chat, err = applyStructuredOutput(root.Get("text.format"), chat)
	if err != nil {
		return TranslatedRequest{}, err
	}
	chat = normalizeToolChoice(root.Get("tool_choice"), chat)

	return TranslatedRequest{
		Model:        model,
		Stream:       stream,
		OriginalBody: original,
		ChatBody:     chat,
	}, nil
}

func validateInput(input gjson.Result) error {
	if input.Type == gjson.String {
		return nil
	}
	if !input.IsArray() {
		return invalid("input", "The input field must be a string or an array.")
	}

	knownCalls := make(map[string]struct{})
	seenOutputs := make(map[string]struct{})
	var validationErr error
	input.ForEach(func(index, item gjson.Result) bool {
		path := fmt.Sprintf("input[%d]", index.Int())
		if !item.IsObject() {
			validationErr = invalid(path, "Each input item must be an object.")
			return false
		}
		itemType := strings.TrimSpace(item.Get("type").String())
		if itemType == "" && item.Get("role").Exists() {
			itemType = "message"
		}
		switch itemType {
		case "", "message":
			validationErr = validateMessage(item, path)
		case "function_call":
			callID := strings.TrimSpace(item.Get("call_id").String())
			if callID == "" {
				validationErr = invalid(path+".call_id", "Function calls require a call_id.")
				break
			}
			if _, exists := knownCalls[callID]; exists {
				validationErr = invalid(path+".call_id", "Function call IDs must be unique.")
				break
			}
			if strings.TrimSpace(item.Get("name").String()) == "" {
				validationErr = invalid(path+".name", "Function calls require a name.")
				break
			}
			if arguments := item.Get("arguments"); !arguments.Exists() || arguments.Type != gjson.String {
				validationErr = invalid(path+".arguments", "Function call arguments must be a JSON-encoded string.")
				break
			}
			knownCalls[callID] = struct{}{}
		case "function_call_output":
			callID := strings.TrimSpace(item.Get("call_id").String())
			if _, exists := knownCalls[callID]; callID == "" || !exists {
				validationErr = invalid(path+".call_id", "Function outputs must reference a preceding function call.")
				break
			}
			if _, duplicate := seenOutputs[callID]; duplicate {
				validationErr = invalid(path+".call_id", "Each function call can have only one output.")
				break
			}
			if output := item.Get("output"); !output.Exists() {
				validationErr = invalid(path+".output", "Function outputs require an output value.")
				break
			}
			seenOutputs[callID] = struct{}{}
		case "reasoning":
			// Reasoning history is converted by the pinned upstream translator.
		case "input_file":
			validationErr = unsupported(path+".type", "Hosted file inputs are not supported by Workers AI.")
		default:
			validationErr = unsupported(path+".type", "Input item type %q is not supported by Workers AI.", itemType)
		}
		return validationErr == nil
	})
	return validationErr
}

func validateMessage(item gjson.Result, path string) error {
	role := strings.TrimSpace(item.Get("role").String())
	switch role {
	case "user", "assistant", "system", "developer":
	case "":
		return invalid(path+".role", "Message input items require a role.")
	default:
		return invalid(path+".role", "Message role %q is invalid.", role)
	}
	content := item.Get("content")
	if !content.Exists() {
		return invalid(path+".content", "Message input items require content.")
	}
	if content.Type == gjson.String {
		return nil
	}
	if !content.IsArray() {
		return invalid(path+".content", "Message content must be a string or an array.")
	}
	var validationErr error
	content.ForEach(func(index, part gjson.Result) bool {
		partPath := fmt.Sprintf("%s.content[%d]", path, index.Int())
		partType := strings.TrimSpace(part.Get("type").String())
		switch partType {
		case "input_text", "output_text":
			if text := part.Get("text"); !text.Exists() || text.Type != gjson.String {
				validationErr = invalid(partPath+".text", "Text content parts require a text string.")
			}
		case "input_image":
			if strings.TrimSpace(part.Get("image_url").String()) == "" {
				validationErr = unsupported(partPath+".image_url", "Hosted image files are not supported; provide an image_url.")
			} else if detail := part.Get("detail"); detail.Exists() && detail.Type != gjson.String {
				validationErr = invalid(partPath+".detail", "Image detail must be a string.")
			}
		case "input_file":
			validationErr = unsupported(partPath+".type", "Hosted file inputs are not supported by Workers AI.")
		default:
			validationErr = unsupported(partPath+".type", "Content part type %q is not supported by Workers AI.", partType)
		}
		return validationErr == nil
	})
	return validationErr
}

func validateTools(tools gjson.Result) error {
	if !tools.Exists() {
		return nil
	}
	if !tools.IsArray() {
		return invalid("tools", "The tools field must be an array.")
	}
	var validationErr error
	tools.ForEach(func(index, tool gjson.Result) bool {
		path := fmt.Sprintf("tools[%d]", index.Int())
		toolType := strings.TrimSpace(tool.Get("type").String())
		if toolType == "" {
			toolType = "function"
		}
		if toolType != "function" {
			validationErr = unsupported(path+".type", "Hosted tool type %q is not supported by Workers AI.", toolType)
			return false
		}
		name := strings.TrimSpace(tool.Get("name").String())
		if name == "" {
			name = strings.TrimSpace(tool.Get("function.name").String())
		}
		if name == "" {
			validationErr = invalid(path+".name", "Function tools require a name.")
			return false
		}
		parameters := responsesToolParameters(tool)
		if !parameters.Exists() || !parameters.IsObject() {
			validationErr = invalid(path+".parameters", "Function tools require an object parameters schema.")
			return false
		}
		if strict := tool.Get("strict"); strict.Exists() && strict.Type != gjson.True && strict.Type != gjson.False {
			validationErr = invalid(path+".strict", "Function tool strict must be a boolean.")
			return false
		}
		return true
	})
	return validationErr
}

func validateToolChoice(choice gjson.Result) error {
	if !choice.Exists() {
		return nil
	}
	if choice.Type == gjson.String {
		switch choice.String() {
		case "auto", "none", "required":
			return nil
		default:
			return invalid("tool_choice", "The tool_choice value is invalid.")
		}
	}
	if !choice.IsObject() || choice.Get("type").String() != "function" {
		return invalid("tool_choice", "tool_choice must select a function or use auto, none, or required.")
	}
	name := strings.TrimSpace(choice.Get("name").String())
	if name == "" {
		name = strings.TrimSpace(choice.Get("function.name").String())
	}
	if name == "" {
		return invalid("tool_choice.name", "A function tool choice requires a name.")
	}
	return nil
}

func validateTextFormat(format gjson.Result) (string, error) {
	if !format.Exists() {
		return "", nil
	}
	if !format.IsObject() {
		return "", invalid("text.format", "text.format must be an object.")
	}
	formatType := strings.TrimSpace(format.Get("type").String())
	switch formatType {
	case "", "text":
		return formatType, nil
	case "json_object":
		return formatType, nil
	case "json_schema":
		if strings.TrimSpace(format.Get("name").String()) == "" {
			return "", invalid("text.format.name", "JSON Schema output requires a name.")
		}
		if schema := format.Get("schema"); !schema.Exists() || !schema.IsObject() {
			return "", invalid("text.format.schema", "JSON Schema output requires an object schema.")
		}
		return formatType, nil
	default:
		return "", invalid("text.format.type", "The text format type %q is invalid.", formatType)
	}
}

func applyGenerationParameters(root gjson.Result, chat []byte) []byte {
	for _, mapping := range []struct {
		from string
		to   string
	}{
		{"max_output_tokens", "max_tokens"},
		{"temperature", "temperature"},
		{"top_p", "top_p"},
		{"frequency_penalty", "frequency_penalty"},
		{"presence_penalty", "presence_penalty"},
		{"top_logprobs", "top_logprobs"},
	} {
		if value := root.Get(mapping.from); value.Exists() {
			chat, _ = sjson.SetRawBytes(chat, mapping.to, []byte(value.Raw))
		}
	}
	if root.Get("top_logprobs").Exists() {
		chat, _ = sjson.SetBytes(chat, "logprobs", true)
	}
	return chat
}

func applyStructuredOutput(format gjson.Result, chat []byte) ([]byte, error) {
	formatType, err := validateTextFormat(format)
	if err != nil || formatType == "" || formatType == "text" {
		return chat, err
	}
	if formatType == "json_object" {
		chat, _ = sjson.SetRawBytes(chat, "response_format", []byte(`{"type":"json_object"}`))
		return chat, nil
	}
	responseFormat := []byte(`{"type":"json_schema","json_schema":{"name":"","schema":{}}}`)
	responseFormat, _ = sjson.SetBytes(responseFormat, "json_schema.name", format.Get("name").String())
	responseFormat, _ = sjson.SetRawBytes(responseFormat, "json_schema.schema", []byte(format.Get("schema").Raw))
	if description := format.Get("description"); description.Exists() {
		responseFormat, _ = sjson.SetBytes(responseFormat, "json_schema.description", description.String())
	}
	if strict := format.Get("strict"); strict.Exists() {
		responseFormat, _ = sjson.SetBytes(responseFormat, "json_schema.strict", strict.Bool())
	}
	chat, _ = sjson.SetRawBytes(chat, "response_format", responseFormat)
	return chat, nil
}

func normalizeToolChoice(choice gjson.Result, chat []byte) []byte {
	if !choice.IsObject() || choice.Get("type").String() != "function" {
		return chat
	}
	if choice.Get("function.name").Exists() {
		return chat
	}
	name := strings.TrimSpace(choice.Get("name").String())
	if name == "" {
		return chat
	}
	normalized := []byte(`{"type":"function","function":{"name":""}}`)
	normalized, _ = sjson.SetBytes(normalized, "function.name", name)
	chat, _ = sjson.SetRawBytes(chat, "tool_choice", normalized)
	return chat
}
