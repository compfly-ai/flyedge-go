package main

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestUploadValidation(t *testing.T) {
	valid := attachment{Name: "demo.pdf", MediaType: "application/pdf", Data: base64.StdEncoding.EncodeToString([]byte("synthetic-pdf"))}
	if _, err := inputContents(chatInput{Attachments: []attachment{valid}}); err != nil {
		t.Fatal(err)
	}
	for _, input := range []chatInput{{}, {Message: strings.Repeat("x", 16001)}, {Attachments: make([]attachment, 11)}, {Attachments: []attachment{{MediaType: "text/plain", Data: valid.Data}}}, {Attachments: []attachment{{MediaType: "image/png", Data: "invalid!"}}}, {Attachments: []attachment{{MediaType: "application/pdf", Data: strings.Repeat("A", 14<<20)}}}} {
		if _, err := inputContents(input); err == nil {
			t.Fatal("invalid upload accepted")
		}
	}
}
