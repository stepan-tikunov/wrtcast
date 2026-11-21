package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A fake DLE site: the title 7 is for signed-in users only, the sign-in
// takes "viewer"/"secret".
func registeredOnlySite(t *testing.T, signIns *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/" && request.Method == http.MethodPost:
			signIns.Add(1)
			if err := request.ParseForm(); err != nil {
				t.Error(err)
			}
			if request.Form.Get("login") != "submit" {
				t.Errorf("unexpected sign-in form: %v", request.Form)
			}
			if request.Form.Get("login_name") == "viewer" && request.Form.Get("login_password") == "secret" {
				http.SetCookie(response, &http.Cookie{Name: "PHPSESSID", Value: "s1", Path: "/"})
				http.SetCookie(response, &http.Cookie{Name: "dle_user_id", Value: "42", Path: "/"})
				http.SetCookie(response, &http.Cookie{Name: "dle_password", Value: "hash", Path: "/"})
			}
			_, _ = response.Write([]byte("ready"))
		case request.URL.Path == "/":
			_, _ = response.Write([]byte("ready"))
		case request.URL.Path == "/index.php":
			cookie, _ := request.Cookie("dle_user_id")
			if cookie == nil || cookie.Value != "42" {
				response.WriteHeader(http.StatusForbidden)
				_, _ = response.Write([]byte("Просмотр доступен только зарегистрированным пользователям"))
				return
			}
			_, _ = response.Write([]byte(`<iframe src="//api.example/embed/movie/7"></iframe>`))
		default:
			response.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestRegisteredOnlyTitleSignsIn(t *testing.T) {
	t.Parallel()
	var signIns atomic.Int32
	server := registeredOnlySite(t, &signIns)
	defer server.Close()

	client, err := NewClient(Config{BaseURL: server.URL, RefreshInterval: time.Hour, Login: "viewer", Password: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, page, _, err := client.movieDocument(ctx, client.ExternalTitleID("7")); err != nil || !strings.Contains(string(page), "embed/movie/7") {
		t.Fatalf("got %q, %v", page, err)
	}
	// A session made anew stays signed in.
	if err := client.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := client.movieDocument(ctx, client.ExternalTitleID("7")); err != nil {
		t.Fatal(err)
	}
	if got := signIns.Load(); got != 1 {
		t.Fatalf("signed in %d times, want 1", got)
	}
}

func TestRegisteredOnlyTitleWrongPassword(t *testing.T) {
	t.Parallel()
	var signIns atomic.Int32
	server := registeredOnlySite(t, &signIns)
	defer server.Close()

	client, err := NewClient(Config{BaseURL: server.URL, RefreshInterval: time.Hour, Login: "viewer", Password: "wrong"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, _, _, err := client.movieDocument(ctx, client.ExternalTitleID("7")); !errors.Is(err, ErrRegistrationRequired) {
			t.Fatalf("got %v, want ErrRegistrationRequired", err)
		}
	}
	// The failed sign-in is not tried again for every title asked for.
	if got := signIns.Load(); got != 1 {
		t.Fatalf("signed in %d times, want 1", got)
	}
}

func TestRegisteredOnlyTitleWithoutAccount(t *testing.T) {
	t.Parallel()
	var signIns atomic.Int32
	server := registeredOnlySite(t, &signIns)
	defer server.Close()

	client, err := NewClient(Config{BaseURL: server.URL, RefreshInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := client.movieDocument(ctx, client.ExternalTitleID("7")); !errors.Is(err, ErrRegistrationRequired) {
		t.Fatalf("got %v, want ErrRegistrationRequired", err)
	}
	if signIns.Load() != 0 {
		t.Fatal("signed in without an account")
	}
}
