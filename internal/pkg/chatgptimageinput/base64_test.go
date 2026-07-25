package imageinput

import "testing"

func TestDecodeBase64Images(t *testing.T) {
	images, err := DecodeBase64Images([]string{"aGVsbG8=", "data:image/png;base64,d29ybGQ="})
	if err != nil {
		t.Fatal(err)
	}
	if string(images[0]) != "hello" || string(images[1]) != "world" {
		t.Fatalf("decoded=%q", images)
	}
}

func TestDecodeBase64ImagesRejectsInvalidInput(t *testing.T) {
	for _, input := range []string{"", "data:image/png,hello", "%%%"} {
		if _, err := DecodeBase64Images([]string{input}); err == nil {
			t.Fatalf("input %q unexpectedly succeeded", input)
		}
	}
}
