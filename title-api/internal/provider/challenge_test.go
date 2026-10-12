package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type memoryURLStore struct {
	mu    sync.Mutex
	value string
}

func (s *memoryURLStore) Load(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.value, nil
}

func (s *memoryURLStore) Save(_ context.Context, value string) error {
	s.mu.Lock()
	s.value = value
	s.mu.Unlock()
	return nil
}

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
	const input = "Mozilla/5.0 (X11; Linux)*"
	const want = "Mozilla%2F5.0%20%28X11%3B%20Linux%29%2a"
	if got := encodeURIComponent(input); got != want {
		t.Fatalf("encodeURIComponent() = %q, want %q", got, want)
	}
}

func TestClientRemembersRedirectedProviderURL(t *testing.T) {
	t.Parallel()

	const code = int64(127)
	hash, err := challengeHash(context.Background(), code)
	if err != nil {
		t.Fatal(err)
	}
	actual := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		cookie := request.Header.Get("Cookie")
		valid := containsAll(cookie,
			fmt.Sprintf("__js_p_=%d,1800,0,0,0", code),
			"__jhash_="+strconv.Itoa(hash),
			"__jua_=Mozilla%2F5.0%20%28X11%3B%20Linux%29",
		)
		if !valid {
			response.Header().Set("Set-Cookie", fmt.Sprintf("__js_p_=%d,1800,0,0,0; Path=/", code))
			_, _ = response.Write([]byte(`<script>function get_jhash(){}; var cookie = "__js_p_";</script>`))
			return
		}
		_, _ = response.Write([]byte("ready"))
	}))
	defer actual.Close()

	var bootstrapRequests atomic.Int64
	bootstrap := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		bootstrapRequests.Add(1)
		http.Redirect(response, request, actual.URL+"/", http.StatusFound)
	}))
	store := &memoryURLStore{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client, err := NewClient(Config{
		BaseURL:         bootstrap.URL,
		UserAgent:       "Mozilla/5.0 (X11; Linux)",
		RefreshInterval: time.Hour,
		URLStore:        store,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if got := client.providerURL().String(); got != actual.URL {
		t.Fatalf("providerURL() = %q, want %q", got, actual.URL)
	}
	if stored, _ := store.Load(context.Background()); stored != actual.URL {
		t.Fatalf("stored URL = %q, want %q", stored, actual.URL)
	}

	requestsBeforeRestart := bootstrapRequests.Load()
	restartCtx, cancelRestart := context.WithCancel(context.Background())
	defer cancelRestart()
	restarted, err := NewClient(Config{
		BaseURL:         bootstrap.URL,
		UserAgent:       "Mozilla/5.0 (X11; Linux)",
		RefreshInterval: time.Hour,
		URLStore:        store,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Start(restartCtx); err != nil {
		t.Fatal(err)
	}
	if bootstrapRequests.Load() != requestsBeforeRestart {
		t.Fatal("restarted client contacted bootstrap URL instead of remembered URL")
	}
}

func containsAll(value string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(value, part) {
			return false
		}
	}
	return true
}

func TestExternalTitleID(t *testing.T) {
	t.Parallel()
	client, err := NewClient(Config{BaseURL: "https://provider.example"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := client.ExternalTitleID("42185"), "2d1de35742185"; got != want {
		t.Fatalf("ExternalTitleID() = %q, want %q", got, want)
	}
	if got, err := client.parseExternalTitleID("2d1de35742185"); err != nil || got != "42185" {
		t.Fatalf("parseExternalTitleID() = %q, %v", got, err)
	}
}
