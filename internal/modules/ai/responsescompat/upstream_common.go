package responsescompat

import (
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func joinRawArray(items [][]byte) []byte {
	if len(items) == 0 {
		return []byte("[]")
	}
	size := len(items) + 1
	for _, item := range items {
		size += len(item)
	}
	out := make([]byte, 0, size)
	out = append(out, '[')
	for i, item := range items {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, item...)
	}
	return append(out, ']')
}

func setRawArrayItems(data []byte, path string, items [][]byte) []byte {
	if len(items) == 0 {
		return data
	}
	if len(items) == 1 {
		array := gjson.GetBytes(data, path)
		if array.Raw == "[]" && array.Index >= 0 && array.Index+len(array.Raw) <= len(data) {
			out := make([]byte, 0, len(data)+len(items[0]))
			out = append(out, data[:array.Index]...)
			out = append(out, '[')
			out = append(out, items[0]...)
			out = append(out, ']')
			return append(out, data[array.Index+len(array.Raw):]...)
		}
	}
	data, _ = sjson.SetRawBytes(data, path, joinRawArray(items))
	return data
}

func sseEventData(event string, payload []byte) []byte {
	out := make([]byte, 0, len(event)+len(payload)+14)
	out = append(out, "event: "...)
	out = append(out, event...)
	out = append(out, '\n')
	out = append(out, "data: "...)
	return append(out, payload...)
}
