package provider_test

import (
	"context"
	"io/ioutil"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/puppetlabs/vault-plugin-secrets-oauthapp/pkg/provider"
	"github.com/puppetlabs/vault-plugin-secrets-oauthapp/pkg/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

var basicTestFactory = provider.BasicFactory(oauth2.Endpoint{
	AuthURL:   "http://localhost/authorize",
	TokenURL:  "http://localhost/token",
	AuthStyle: oauth2.AuthStyleInParams,
})

func TestBasicAuthCodeURLConfig(t *testing.T) {
	ctx := context.Background()

	r := provider.NewRegistry()
	r.MustRegister("basic", basicTestFactory)

	basicTest, err := r.New(ctx, "basic", map[string]string{})
	require.NoError(t, err)

	conf := basicTest.NewAuthCodeURLConfigBuilder("foo").
		WithRedirectURL("http://example.com/redirect").
		WithScopes("a", "b", "c").
		Build()

	u, err := url.Parse(conf.AuthCodeURL("state", oauth2.SetAuthURLParam("baz", "quux")))
	require.NoError(t, err)

	assert.Equal(t, "http", u.Scheme)
	assert.Equal(t, "localhost", u.Host)
	assert.Equal(t, "/authorize", u.Path)

	qs := u.Query()
	assert.Equal(t, "code", qs.Get("response_type"))
	assert.Equal(t, "foo", qs.Get("client_id"))
	assert.Equal(t, "http://example.com/redirect", qs.Get("redirect_uri"))
	assert.Equal(t, "state", qs.Get("state"))
	assert.Equal(t, "a b c", qs.Get("scope"))
	assert.Equal(t, "quux", qs.Get("baz"))
}

func TestBasicExchangeConfig(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	r := provider.NewRegistry()
	r.MustRegister("basic", basicTestFactory)

	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			b, err := ioutil.ReadAll(r.Body)
			require.NoError(t, err)

			data, err := url.ParseQuery(string(b))
			require.NoError(t, err)

			assert.Equal(t, "foo", data.Get("client_id"))
			assert.Equal(t, "bar", data.Get("client_secret"))

			switch data.Get("grant_type") {
			case "authorization_code":
				assert.Equal(t, "authorization_code", data.Get("grant_type"))
				assert.Equal(t, "123456", data.Get("code"))
				assert.Equal(t, "http://example.com/redirect", data.Get("redirect_uri"))
				assert.Equal(t, "quux", data.Get("baz"))

				_, _ = w.Write([]byte(`access_token=abcd&refresh_token=efgh&token_type=bearer&expires_in=5`))
			case "refresh_token":
				assert.Equal(t, "efgh", data.Get("refresh_token"))

				_, _ = w.Write([]byte(`access_token=ijkl&refresh_token=efgh&token_type=bearer&expires_in=3600`))
			default:
				assert.Fail(t, "unexpected `grant_type` value: %q", data.Get("grant_type"))
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	c := &http.Client{Transport: &testutil.MockRoundTripper{Handler: h}}
	ctx = context.WithValue(ctx, oauth2.HTTPClient, c)

	basicTest, err := r.New(ctx, "basic", map[string]string{})
	require.NoError(t, err)

	conf := basicTest.NewExchangeConfigBuilder("foo", "bar").
		WithRedirectURL("http://example.com/redirect").
		Build()

	token, err := conf.Exchange(ctx, "123456", oauth2.SetAuthURLParam("baz", "quux"))
	require.NoError(t, err)
	require.NotNil(t, token)
	require.Equal(t, "abcd", token.AccessToken)
	require.Equal(t, "Bearer", token.Type())
	require.Equal(t, "efgh", token.RefreshToken)
	require.NotEmpty(t, token.Expiry)

	// This token is already invalid, so let's try to refresh it.
	require.False(t, token.Valid())

	token, err = conf.Refresh(ctx, token)
	require.NoError(t, err)
	require.NotNil(t, token)

	// Our refreshed response is good for an hour.
	require.Equal(t, "ijkl", token.AccessToken)
	require.True(t, token.Valid())
}
