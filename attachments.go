package flyedge

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

const mediaTransportLimit = 16 << 20

var mediaURLPattern = regexp.MustCompile(`(?i)data:(?:image/[\w.+-]+|application/pdf);base64,[A-Za-z0-9+/=\r\n\t ]+`)
var mediaMIMEPattern = regexp.MustCompile(`(?i)["'](?:media_type|mime_type|mimeType)["']\s*:\s*["'](?:image/[^"']+|application/pdf)["']`)
var mediaFieldPattern = regexp.MustCompile(`(["'](?:data|base64|file_data)["']\s*:\s*)["'][A-Za-z0-9+/=\r\n\t ]+["']`)

func mediaAuditText(text string) string {
	text = mediaURLPattern.ReplaceAllString(text, "[inline media omitted]")
	if mediaMIMEPattern.MatchString(text) {
		text = mediaFieldPattern.ReplaceAllString(text, `${1}"[inline media omitted]"`)
	}
	return text
}

// ExtractAttachments normalizes recognized inline provider content, without URL fetches or file reads.
// Callers must check per-agent permission first. Traversal and encoded transport size are bounded.
func ExtractAttachments(value any) []Attachment {
	var items []Attachment
	nodes, size := 0, 0
	incomplete, oversize := false, false
	add := func(mime, data string) {
		if len(mime) > 128 {
			mime = "unresolved"
		}
		size += len(data) + len(mime) + 100
		if size > mediaTransportLimit {
			oversize = true
			return
		}
		items = append(items, Attachment{ID: fmt.Sprintf("attachment-%d", len(items)+1), MediaType: mime, DataBase64: data})
	}
	url := func(value any) {
		if m, ok := value.(map[string]any); ok {
			value = m["url"]
		}
		s, _ := value.(string)
		if at := strings.Index(s, ";base64,"); strings.HasPrefix(s, "data:") && at > 5 && at < 150 {
			add(s[5:at], s[at+8:])
			return
		}
		add("unresolved", "")
	}
	var walk func(any, int)
	walk = func(v any, depth int) {
		if oversize {
			return
		}
		nodes++
		if depth > 32 || nodes > 10000 {
			incomplete = true
			return
		}
		switch x := v.(type) {
		case []any:
			for _, child := range x {
				walk(child, depth+1)
			}
		case map[string]any:
			typ, _ := x["type"].(string)
			switch typ {
			case "image_url", "input_image":
				url(x["image_url"])
				return
			case "input_file":
				url(x["file_data"])
				return
			case "file":
				if f, ok := x["file"].(map[string]any); ok {
					url(f["file_data"])
				} else {
					add("unresolved", "")
				}
				return
			case "image", "document":
				if src, ok := x["source"].(map[string]any); ok {
					mime, _ := src["media_type"].(string)
					data, _ := src["data"].(string)
					add(mime, data)
				} else {
					add("unresolved", "")
				}
				return
			}
			for _, key := range []string{"inlineData", "inline_data"} {
				if src, ok := x[key].(map[string]any); ok {
					mime, _ := src["mimeType"].(string)
					if mime == "" {
						mime, _ = src["mime_type"].(string)
					}
					data, _ := src["data"].(string)
					add(mime, data)
					return
				}
			}
			for _, child := range x {
				walk(child, depth+1)
			}
		}
	}
	walk(value, 0)
	if oversize {
		return []Attachment{{ID: "provider-request", MediaType: "unresolved", Oversize: map[string]any{"scope": "provider_request", "encoded_bytes": size}}}
	}
	if incomplete {
		items = append(items, Attachment{ID: "incomplete", MediaType: "unresolved"})
	}
	return items
}

// policyValue creates a text-only inspection view; original provider/tool objects remain untouched.
func policyValue(v any, depth int) any {
	if depth > 32 {
		return "[inspection depth exceeded]"
	}
	switch x := v.(type) {
	case map[string]any:
		typ, _ := x["type"].(string)
		switch typ {
		case "image_url", "input_image", "input_file", "file", "image", "document":
			return "[inline media omitted]"
		}
		if x["inlineData"] != nil || x["inline_data"] != nil {
			return "[inline media omitted]"
		}
		out := make(map[string]any, len(x))
		for k, child := range x {
			out[k] = policyValue(child, depth+1)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, child := range x {
			out[i] = policyValue(child, depth+1)
		}
		return out
	case string:
		return mediaAuditText(x)
	default:
		return v
	}
}

func (g *Guard) mediaContent(value any) Content {
	// Permission precedes normalization, hashing and attachment forwarding.
	var attachments []Attachment
	if g.NontextPayloadAllowed() {
		if encoded, ok := value.(string); ok && len(encoded) <= mediaTransportLimit {
			var decoded any
			if json.Unmarshal([]byte(encoded), &decoded) == nil {
				value = decoded
			}
		}
		attachments = ExtractAttachments(value)
	}
	return Content{Full: jsonString(policyValue(value, 0)), Attachments: attachments}
}

func mediaAcknowledgment(dec Decision) (string, bool) {
	var metadata struct {
		Status     string `json:"nontext_status"`
		Evaluation string `json:"evaluation_status"`
	}
	if encoded, ok := dec.Detail["intent_metadata_json"].(string); ok {
		_ = json.Unmarshal([]byte(encoded), &metadata)
	}
	return metadata.Status, metadata.Status == "decoded" || (metadata.Status == "skipped_oversize" && metadata.Evaluation == "skipped" && dec.Reason == "nontext_oversize_skipped")
}

// Technical media decisions are availability failures, not control denials.
func mediaInspectionUnavailable(dec Decision) bool {
	if dec.Reason == "nontext_uninspected" || dec.Reason == "nontext_limit_exceeded" || string(dec.Action) == "indeterminate" {
		return true
	}
	_, confirmed := mediaAcknowledgment(dec)
	return dec.Action != ActionDeny && !confirmed
}
