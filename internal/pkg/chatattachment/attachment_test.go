package chatattachment

import "testing"

func TestValidateSupportedAttachments(t *testing.T) {
	for _, test := range []struct {
		name, declared string
		data           []byte
		wantType       string
	}{
		{"report.pdf", "application/pdf", []byte("%PDF-1.7\nbody"), "application/pdf"},
		{"notes.txt", "text/plain", []byte("hello"), "text/plain"},
		{"readme.md", "text/plain", []byte("# title"), "text/markdown"},
		{"rows.csv", "application/vnd.ms-excel", []byte("a,b\n1,2"), "text/csv"},
	} {
		file, err := Validate(test.data, test.name, test.declared)
		if err != nil || file.Name != test.name || file.ContentType != test.wantType {
			t.Fatalf("%s file=%+v err=%v", test.name, file, err)
		}
	}
}

func TestValidateRejectsUnsafeOrUnsupportedAttachments(t *testing.T) {
	for _, test := range []struct {
		name string
		data []byte
	}{
		{"script.exe", []byte("MZ")},
		{"fake.pdf", []byte("not pdf")},
		{"binary.txt", []byte{'a', 0, 'b'}},
	} {
		if _, err := Validate(test.data, test.name, "application/octet-stream"); err == nil {
			t.Fatalf("%s was accepted", test.name)
		}
	}
}
