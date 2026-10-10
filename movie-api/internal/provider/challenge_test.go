package provider

import (
	"context"
	"testing"
)

func TestChallengeHash(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		code int64
		want int
	}{
		{code: 127, want: 155},
		{code: 161, want: 61},
		{code: 266, want: 910},
	} {
		got, err := challengeHash(context.Background(), test.code)
		if err != nil {
			t.Fatalf("challengeHash(%d): %v", test.code, err)
		}
		if got != test.want {
			t.Errorf("challengeHash(%d) = %d, want %d", test.code, got, test.want)
		}
	}
}

func TestEncodeURIComponent(t *testing.T) {
	t.Parallel()
	const input = "Mozilla/5.0 (X11; Linux)"
	const want = "Mozilla%2F5.0%20(X11%3B%20Linux)"
	if got := encodeURIComponent(input); got != want {
		t.Fatalf("encodeURIComponent() = %q, want %q", got, want)
	}
}

func TestExternalMovieID(t *testing.T) {
	t.Parallel()
	client, err := NewClient(Config{BaseURL: "https://provider.example"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := client.ExternalMovieID("42185"), "2d1de35742185"; got != want {
		t.Fatalf("ExternalMovieID() = %q, want %q", got, want)
	}
	if got, err := client.parseExternalMovieID("2d1de35742185"); err != nil || got != "42185" {
		t.Fatalf("parseExternalMovieID() = %q, %v", got, err)
	}
}
