package enforce

import "testing"

func TestAttachmentIdentityIncludesBytes(t *testing.T) {
	a := CheckRequest{Content: Content{Full: "same", Attachments: []Attachment{{ID: "a", MediaType: "image/png", DataBase64: "first"}}}}
	b := CheckRequest{Content: Content{Full: "same", Attachments: []Attachment{{ID: "a", MediaType: "image/png", DataBase64: "second"}}}}
	a.fillDefaults()
	b.fillDefaults()
	if a.Content.Hash == b.Content.Hash {
		t.Fatal("different images inherited text-only identity")
	}
}
