package submissions

import (
	"strings"
	"testing"
)

func validInput() CreateInput {
	return CreateInput{
		IdempotencyKey: "key-123456",
		Title:          "Rocket",
		Description:    "A thing",
		Links:          []string{"https://github.com/team/rocket"},
		FileName:       "deck.pdf",
		FileSize:       10,
		FileSHA256:     strings.Repeat("ab", 32),
	}
}

func TestValidateAcceptsAGoodVersion(t *testing.T) {
	if bad := validInput().Validate(); bad != nil {
		t.Fatalf("unexpected problems: %v", bad)
	}
}

func TestValidateRejectsBadFields(t *testing.T) {
	cases := map[string]func(*CreateInput){
		"idempotency_key": func(in *CreateInput) { in.IdempotencyKey = "short" },
		"title":           func(in *CreateInput) { in.Title = strings.Repeat("x", 121) },
		"description":     func(in *CreateInput) { in.Description = strings.Repeat("x", 5001) },
		"links":           func(in *CreateInput) { in.Links = []string{"http://insecure.example"} },
		"file.name":       func(in *CreateInput) { in.FileName = "../../etc/passwd" },
		"file.size":       func(in *CreateInput) { in.FileSize = 0 },
		"file.sha256":     func(in *CreateInput) { in.FileSHA256 = "zz" },
	}
	for field, mutate := range cases {
		in := validInput()
		mutate(&in)
		bad := in.Validate()
		if _, ok := bad[field]; !ok {
			t.Errorf("%s: no problem reported, got %v", field, bad)
		}
	}

	in := validInput()
	in.Links = []string{"https://a.example", "https://b.example", "https://c.example", "https://d.example", "https://e.example", "https://f.example"}
	if _, ok := in.Validate()["links"]; !ok {
		t.Error("six links accepted")
	}
	in = validInput()
	in.Links = []string{"https://user:pass@example.com"}
	if _, ok := in.Validate()["links"]; !ok {
		t.Error("link with credentials accepted")
	}
}

func TestPartMathCoversTheWholeFile(t *testing.T) {
	const part = 8 << 20
	cases := []struct {
		size  int64
		count int
		last  int64
	}{
		{1, 1, 1},
		{part, 1, part},
		{part + 1, 2, 1},
		{100 << 20, 13, (100 << 20) - 12*part},
	}
	for _, c := range cases {
		if got := PartCount(c.size, part); got != c.count {
			t.Errorf("PartCount(%d) = %d, want %d", c.size, got, c.count)
		}
		var total int64
		for n := 1; n <= c.count; n++ {
			total += PartLength(c.size, part, n)
		}
		if total != c.size {
			t.Errorf("size %d: parts sum to %d", c.size, total)
		}
		if got := PartLength(c.size, part, c.count); got != c.last {
			t.Errorf("size %d: last part %d, want %d", c.size, got, c.last)
		}
	}
}
