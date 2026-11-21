package provider

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// account is the provider's user title-api signs in as, for the titles the
// provider shows to signed-in users only. It signs in only when such a
// title is asked for, and keeps the cookies of the sign-in for every
// session after (see carry): the anti-bot sessions are made anew every
// few minutes, the sign-in is not.
//
// The site is a DataLife Engine one: its form posts login_name,
// login_password and login=submit to the site's root, and a signed-in user
// is told by its dle_user_id and dle_password cookies.
type account struct {
	login    string
	password string

	mu      sync.Mutex
	cookies [][2]string
	// No signing in again before then, after one that failed.
	pausedUntil time.Time

	flight singleflight.Group
}

// accountPause is how long a failed sign-in keeps the next one away, so a
// wrong password does not post the form for every title asked for.
const accountPause = 10 * time.Minute

var errSignInFailed = errors.New("provider sign-in failed")

func newAccount(login, password string) *account {
	if login == "" || password == "" {
		return nil
	}
	return &account{login: login, password: password}
}

// The cookies of the anti-bot challenge, which a session has its own of.
func challengeCookie(name string) bool {
	return strings.HasPrefix(name, "__js_p_") || strings.HasPrefix(name, "__jhash_") || strings.HasPrefix(name, "__jua_")
}

// carry hands the cookies of the sign-in to a session the client made
// since.
func (a *account) carry(current *session, host string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	cookies := a.cookies
	a.mu.Unlock()
	for _, cookie := range cookies {
		current.setCookie(host, cookie[0], cookie[1])
	}
}

// signIn signs the session in anew, its cookies kept for the sessions
// after; the titles asked for meanwhile wait for the same one.
func (c *Client) signIn(ctx context.Context, current *session) error {
	if c.account == nil {
		return ErrRegistrationRequired
	}
	_, err, _ := c.account.flight.Do("sign-in", func() (any, error) {
		return nil, c.signInOnce(ctx, current)
	})
	return err
}

func (c *Client) signInOnce(ctx context.Context, current *session) error {
	a := c.account
	a.mu.Lock()
	if wait := time.Until(a.pausedUntil); wait > 0 {
		a.mu.Unlock()
		return fmt.Errorf("%w: the last one failed, next in %s", errSignInFailed, wait.Round(time.Second))
	}
	a.mu.Unlock()

	base := c.providerURL()
	host := base.Host
	root := base.ResolveReference(&url.URL{Path: "/"}).String()
	form := url.Values{"login_name": {a.login}, "login_password": {a.password}, "login": {"submit"}}
	body, final, err := c.postForm(ctx, current, root, form, root)
	if err == nil && isChallenge(body) {
		err = errAntiBotChallenge
	}
	if err == nil && final != nil {
		host = final.Host
	}
	if err == nil {
		if id := current.cookieValue(host, "dle_user_id"); id == "" || id == "0" || id == "deleted" ||
			current.cookieValue(host, "dle_password") == "" {
			err = errors.New("no dle_user_id/dle_password cookies in the reply, a wrong login or password?")
		}
	}
	if err != nil {
		a.mu.Lock()
		a.pausedUntil = time.Now().Add(accountPause)
		a.mu.Unlock()
		log.Printf("provider sign-in as %q failed: %v", a.login, err)
		return fmt.Errorf("%w: %v", errSignInFailed, err)
	}

	current.mu.RLock()
	cookies := make([][2]string, 0, len(current.cookieOrder[host]))
	for _, name := range current.cookieOrder[host] {
		if !challengeCookie(name) {
			cookies = append(cookies, [2]string{name, current.cookies[host][name]})
		}
	}
	current.mu.RUnlock()
	a.mu.Lock()
	a.cookies = cookies
	a.mu.Unlock()
	log.Printf("provider sign-in as %q: signed in, user id %s", a.login, current.cookieValue(host, "dle_user_id"))
	return nil
}
