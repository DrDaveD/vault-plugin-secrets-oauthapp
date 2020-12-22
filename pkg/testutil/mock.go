package testutil

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/puppetlabs/vault-plugin-secrets-oauthapp/pkg/provider"
	"golang.org/x/oauth2"
)

/* #nosec G101 */
const (
	MockAuthCodeURL = "http://localhost/authorize"
	MockTokenURL    = "http://localhost/token"
)

type MockRoundTripper struct {
	Handler http.Handler
}

func (mrt *MockRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	w := httptest.NewRecorder()
	mrt.Handler.ServeHTTP(w, r)
	return w.Result(), nil
}

var MockEndpoint = oauth2.Endpoint{
	AuthURL:  MockAuthCodeURL,
	TokenURL: MockTokenURL,
}

type MockClient struct {
	ID     string
	Secret string
}

type MockExchangeFunc func(code string) (*oauth2.Token, error)

func StaticMockExchange(token *oauth2.Token) MockExchangeFunc {
	return func(_ string) (*oauth2.Token, error) {
		return token, nil
	}
}

func AmendTokenMockExchange(get MockExchangeFunc, amend func(token *oauth2.Token) error) MockExchangeFunc {
	return func(candidate string) (*oauth2.Token, error) {
		token, err := get(candidate)
		if err != nil {
			return nil, err
		}

		if err := amend(token); err != nil {
			return nil, err
		}

		return token, nil
	}
}

func ExpiringMockExchange(fn MockExchangeFunc, duration time.Duration) MockExchangeFunc {
	return AmendTokenMockExchange(fn, func(t *oauth2.Token) error {
		t.Expiry = time.Now().Add(duration)
		return nil
	})
}

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}

	return hex.EncodeToString(b)
}

func RefreshableMockExchange(fn MockExchangeFunc, step func(i int) (time.Duration, error)) MockExchangeFunc {
	refreshToken := randomToken(40)
	var i int32

	return AmendTokenMockExchange(fn, func(t *oauth2.Token) error {
		exp, err := step(int(atomic.AddInt32(&i, 1)))
		if err != nil {
			return err
		}

		t.RefreshToken = refreshToken
		t.Expiry = time.Now().Add(exp)
		return nil
	})
}

func RandomMockExchange(_ string) (*oauth2.Token, error) {
	t := &oauth2.Token{
		AccessToken: randomToken(10),
	}
	return t, nil
}

func IncrementMockExchange(prefix string) MockExchangeFunc {
	var i int32

	return func(_ string) (*oauth2.Token, error) {
		t := &oauth2.Token{
			AccessToken: fmt.Sprintf("%s%d", prefix, atomic.AddInt32(&i, 1)),
		}
		return t, nil
	}
}

func ErrorMockExchange(_ string) (*oauth2.Token, error) {
	return nil, &oauth2.RetrieveError{}
}

func RestrictMockExchange(m map[string]MockExchangeFunc) MockExchangeFunc {
	return func(token string) (*oauth2.Token, error) {
		fn, found := m[token]
		if !found {
			return nil, &oauth2.RetrieveError{}
		}

		return fn(token)
	}
}

type mockExchangeConfig struct {
	owner *mock
	fn    MockExchangeFunc
}

func (c *mockExchangeConfig) Exchange(ctx context.Context, code string, opts ...oauth2.AuthCodeOption) (*provider.Token, error) {
	if c.fn == nil {
		return nil, &oauth2.RetrieveError{}
	}

	tok, err := c.fn(code)
	if err != nil {
		return nil, err
	}

	if tok.RefreshToken != "" {
		c.owner.putRefreshTokenCode(tok.RefreshToken, code)
	}

	return &provider.Token{Token: tok}, nil
}

func (c *mockExchangeConfig) Refresh(ctx context.Context, t *provider.Token) (*provider.Token, error) {
	if t.RefreshToken == "" || c.fn == nil {
		return t, nil
	}

	code, ok := c.owner.getRefreshTokenCode(t.RefreshToken)
	if !ok {
		return t, nil
	}

	if t.Valid() {
		return t, nil
	}

	tok, err := c.fn(code)
	if err != nil {
		return nil, err
	}

	return &provider.Token{Token: tok}, nil
}

type mockExchangeConfigBuilder struct {
	owner  *mock
	client MockClient
}

func (cb *mockExchangeConfigBuilder) WithOption(name, value string) provider.ExchangeConfigBuilder {
	return cb
}

func (cb *mockExchangeConfigBuilder) WithRedirectURL(_ string) provider.ExchangeConfigBuilder {
	return cb
}

func (cb *mockExchangeConfigBuilder) Build() provider.ExchangeConfig {
	return &mockExchangeConfig{
		owner: cb.owner,
		fn:    cb.owner.exchanges[cb.client],
	}
}

type mockProvider struct {
	owner *mock
}

func (mp *mockProvider) Version() int {
	return mp.owner.vsn
}

func (mp *mockProvider) NewAuthCodeURLConfigBuilder(clientID string) provider.AuthCodeURLConfigBuilder {
	return provider.NewConformingAuthCodeURLConfigBuilder(MockEndpoint, clientID)
}

func (mp *mockProvider) NewExchangeConfigBuilder(clientID, clientSecret string) provider.ExchangeConfigBuilder {
	return &mockExchangeConfigBuilder{
		client: MockClient{
			ID:     clientID,
			Secret: clientSecret,
		},
		owner: mp.owner,
	}
}

type mock struct {
	vsn          int
	expectedOpts map[string]string
	exchanges    map[MockClient]MockExchangeFunc
	refresh      map[string]string
	refreshMut   sync.RWMutex
}

func (m *mock) factory(ctx context.Context, vsn int, options map[string]string) (provider.Provider, error) {
	switch vsn {
	case -1, m.vsn:
	default:
		return nil, provider.ErrNoProviderWithVersion
	}

	for k, ev := range m.expectedOpts {
		av, found := options[k]
		if !found {
			return nil, &provider.OptionError{Option: k, Message: "not found"}
		}

		if av != ev {
			return nil, &provider.OptionError{Option: k, Message: fmt.Sprintf("expected %q, got %q", ev, av)}
		}

		delete(options, k)
	}

	for k := range options {
		return nil, &provider.OptionError{Option: k, Message: "unexpected"}
	}

	p := &mockProvider{
		owner: m,
	}
	return p, nil
}

func (m *mock) putRefreshTokenCode(refreshToken, code string) {
	m.refreshMut.Lock()
	defer m.refreshMut.Unlock()

	m.refresh[refreshToken] = code
}

func (m *mock) getRefreshTokenCode(refreshToken string) (string, bool) {
	m.refreshMut.RLock()
	defer m.refreshMut.RUnlock()

	code, found := m.refresh[refreshToken]
	return code, found
}

type MockOption func(m *mock)

func MockWithVersion(vsn int) MockOption {
	return func(m *mock) {
		m.vsn = vsn
	}
}

func MockWithExpectedOptionValue(opt, value string) MockOption {
	return func(m *mock) {
		m.expectedOpts[opt] = value
	}
}

func MockWithExchange(client MockClient, fn MockExchangeFunc) MockOption {
	return func(m *mock) {
		m.exchanges[client] = fn
	}
}

func MockFactory(opts ...MockOption) provider.FactoryFunc {
	m := &mock{
		expectedOpts: make(map[string]string),
		exchanges:    make(map[MockClient]MockExchangeFunc),
		refresh:      make(map[string]string),
	}

	MockWithVersion(1)(m)
	for _, opt := range opts {
		opt(m)
	}

	return m.factory
}

// tokenProvider is a local server that mocks RFC8693 token exchange
type tokenProvider struct {
	server       *httptest.Server
}

func NewMockTokenProvider() *tokenProvider {
	tp := new(tokenProvider)
	tp.server = httptest.NewServer(tp)

	return tp
}

func (tp *tokenProvider) GetServerURL() string {
	return tp.server.URL
}

func (tp *tokenProvider) Close() {
	tp.server.Close()
}

func (tp *tokenProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	errmsg := ""

	switch r.URL.Path {
	case "/token":
		grant_type := r.FormValue("grant_type")
		tok := url.Values{}
		if grant_type != "urn:ietf:params:oauth:grant-type:token-exchange" {
			tok["scopes"] = []string{"scopea scopeb scopec"}
			tok["audience"] = []string{"any"}
		} else {
			subject_token := r.FormValue("subject_token")
			if subject_token == "" {
				errmsg = "no subject_token"
				break
			}
			params, err := url.ParseQuery(subject_token)
			if err != nil {
				errmsg = "could not parse subject_token"
				break
			}
			scopes := r.FormValue("scope")
			if scopes != "" {
				tok["scopes"] = []string{scopes}
			} else {
				tok["scopes"] = params["scopes"]
			}
			audience := r.FormValue("audience")
			if audience != "" {
				tok["audience"] = []string{audience}
			} else {
				tok["audience"] = params["audience"]
			}
		}

		// simple manual json encoding
		w.Write([]byte(`{"access_token": "` + tok.Encode() + `"}`))
		return
	default:
		errmsg = fmt.Sprintf("unexpected path: %q", r.URL.Path)
	}
	w.Write([]byte(`{"error": "` + errmsg + `"}`))
}

