package main

import (
	"strings"
	"testing"
)

func TestReadStreamUsage(t *testing.T) {
	s := "data: {\"id\":\"call\",\"model\":\"model\",\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":4,\"total_tokens\":14}}\n\ndata: [DONE]\n\n"
	r, err := readStream(strings.NewReader(s))
	if err != nil {
		t.Fatal(err)
	}
	if r.Text != "ok" || r.Usage.Input != 10 || r.Usage.Output != 4 || r.Usage.Total != 14 {
		t.Fatalf("%+v", r)
	}
	if _, err := readStream(strings.NewReader(strings.ReplaceAll(s, "data: [DONE]", ""))); err == nil {
		t.Fatal("accepted truncated stream")
	}
	if _, err := readStream(strings.NewReader("data: [DONE]\n")); err == nil {
		t.Fatal("accepted missing usage")
	}
}
