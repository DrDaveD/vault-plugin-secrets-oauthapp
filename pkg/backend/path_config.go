package backend

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/hashicorp/vault/sdk/framework"
	"github.com/hashicorp/vault/sdk/logical"
	"github.com/puppetlabs/leg/errmap/pkg/errmark"
	"github.com/puppetlabs/vault-plugin-secrets-oauthapp/v2/pkg/persistence"
	"github.com/puppetlabs/vault-plugin-secrets-oauthapp/v2/pkg/provider"
)

func (b *backend) configReadOperation(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	c, err := b.getCache(ctx, req.Storage)
	if err != nil {
		return nil, err
	} else if c == nil {
		return nil, nil
	}

	resp := &logical.Response{
		Data: map[string]interface{}{
			"client_id":        c.Config.ClientID,
			"auth_url_params":  c.Config.AuthURLParams,
			"provider":         c.Config.ProviderName,
			"provider_version": c.Config.ProviderVersion,
			"provider_options": c.Config.ProviderOptions,

			"tune_provider_timeout_seconds":              c.Config.Tuning.ProviderTimeoutSeconds,
			"tune_provider_timeout_expiry_leeway_factor": c.Config.Tuning.ProviderTimeoutExpiryLeewayFactor,

			"tune_refresh_check_interval_seconds": c.Config.Tuning.RefreshCheckIntervalSeconds,
			"tune_refresh_expiry_delta_factor":    c.Config.Tuning.RefreshExpiryDeltaFactor,

			"tune_reap_check_interval_seconds":   c.Config.Tuning.ReapCheckIntervalSeconds,
			"tune_reap_dry_run":                  c.Config.Tuning.ReapDryRun,
			"tune_reap_non_refreshable_seconds":  c.Config.Tuning.ReapNonRefreshableSeconds,
			"tune_reap_revoked_seconds":          c.Config.Tuning.ReapRevokedSeconds,
			"tune_reap_transient_error_attempts": c.Config.Tuning.ReapTransientErrorAttempts,
			"tune_reap_transient_error_seconds":  c.Config.Tuning.ReapTransientErrorSeconds,
		},
	}
	return resp, nil
}

func (b *backend) configUpdateOperation(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	clientID, ok := data.GetOk("client_id")
	if !ok {
		return logical.ErrorResponse("missing client ID"), nil
	}

	providerName, ok := data.GetOk("provider")
	if !ok {
		return logical.ErrorResponse("missing provider"), nil
	}

	providerOptions := data.Get("provider_options").(map[string]string)

	p, err := b.providerRegistry.New(ctx, providerName.(string), providerOptions)
	if errors.Is(err, provider.ErrNoSuchProvider) {
		return logical.ErrorResponse("provider %q does not exist", providerName), nil
	} else if errmark.MarkedUser(err) {
		return logical.ErrorResponse(errmark.MarkShort(err).Error()), nil
	} else if err != nil {
		return nil, err
	}

	c := &persistence.ConfigEntry{
		Version:         persistence.ConfigVersionLatest,
		ClientID:        clientID.(string),
		ClientSecret:    data.Get("client_secret").(string),
		AuthURLParams:   data.Get("auth_url_params").(map[string]string),
		ProviderName:    providerName.(string),
		ProviderVersion: p.Version(),
		ProviderOptions: providerOptions,
		Tuning: persistence.ConfigTuningEntry{
			ProviderTimeoutSeconds:            data.Get("tune_provider_timeout_seconds").(int),
			ProviderTimeoutExpiryLeewayFactor: data.Get("tune_provider_timeout_expiry_leeway_factor").(float64),
			RefreshCheckIntervalSeconds:       data.Get("tune_refresh_check_interval_seconds").(int),
			RefreshExpiryDeltaFactor:          data.Get("tune_refresh_expiry_delta_factor").(float64),
			ReapCheckIntervalSeconds:          data.Get("tune_reap_check_interval_seconds").(int),
			ReapDryRun:                        data.Get("tune_reap_dry_run").(bool),
			ReapNonRefreshableSeconds:         data.Get("tune_reap_non_refreshable_seconds").(int),
			ReapRevokedSeconds:                data.Get("tune_reap_revoked_seconds").(int),
			ReapTransientErrorAttempts:        data.Get("tune_reap_transient_error_attempts").(int),
			ReapTransientErrorSeconds:         data.Get("tune_reap_transient_error_seconds").(int),
		},
	}

	// Sanity checks for tuning options.
	switch {
	case c.Tuning.ProviderTimeoutExpiryLeewayFactor < 1:
		return logical.ErrorResponse("provider timeout expiry leeway factor must be at least 1.0"), nil
	case c.Tuning.RefreshCheckIntervalSeconds > int((90 * 24 * time.Hour).Seconds()):
		return logical.ErrorResponse("refresh check interval can be at most 90 days"), nil
	case c.Tuning.RefreshExpiryDeltaFactor < 1:
		return logical.ErrorResponse("refresh expiry delta factor must be at least 1.0"), nil
	case c.Tuning.ReapCheckIntervalSeconds > int((180 * 24 * time.Hour).Seconds()):
		return logical.ErrorResponse("reap check interval can be at most 180 days"), nil
	case c.Tuning.ReapTransientErrorAttempts < 0:
		return logical.ErrorResponse("reap transient error attempts cannot be negative"), nil
	}

	if err := b.data.Managers(req.Storage).Config().WriteConfig(ctx, c); err != nil {
		return nil, err
	}

	b.reset()

	return nil, nil
}

func (b *backend) configDeleteOperation(ctx context.Context, req *logical.Request, _ *framework.FieldData) (*logical.Response, error) {
	if err := b.data.Managers(req.Storage).Config().DeleteConfig(ctx); err != nil {
		return nil, err
	}

	b.reset()

	return nil, nil
}

func (b *backend) configAuthCodeURLUpdateOperation(ctx context.Context, req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	c, err := b.getCache(ctx, req.Storage)
	if err != nil {
		return nil, err
	} else if c == nil {
		return logical.ErrorResponse("not configured"), nil
	}

	state, ok := data.GetOk("state")
	if !ok {
		return logical.ErrorResponse("missing state"), nil
	}

	url, ok := c.Provider.Public(c.Config.ClientID).AuthCodeURL(
		state.(string),
		provider.WithRedirectURL(data.Get("redirect_url").(string)),
		provider.WithScopes(data.Get("scopes").([]string)),
		provider.WithURLParams(data.Get("auth_url_params").(map[string]string)),
		provider.WithURLParams(c.Config.AuthURLParams),
		provider.WithProviderOptions(data.Get("provider_options").(map[string]string)),
	)
	if !ok {
		return logical.ErrorResponse("authorization code URL not available"), nil
	}

	resp := &logical.Response{
		Data: map[string]interface{}{
			"url": url,
		},
	}
	return resp, nil
}

const (
	ConfigPath            = "config"
	ConfigPathPrefix      = ConfigPath + "/"
	ConfigAuthCodeURLPath = ConfigPathPrefix + "auth_code_url"
)

var configFields = map[string]*framework.FieldSchema{
	"client_id": {
		Type:        framework.TypeString,
		Description: "Specifies the OAuth 2 client ID.",
	},
	"client_secret": {
		Type:        framework.TypeString,
		Description: "Specifies the OAuth 2 client secret.",
	},
	"auth_url_params": {
		Type:        framework.TypeKVPairs,
		Description: "Specifies the additional query parameters to add to the authorization code URL.",
	},
	"provider": {
		Type:        framework.TypeString,
		Description: "Specifies the OAuth 2 provider.",
	},
	"provider_options": {
		Type:        framework.TypeKVPairs,
		Description: "Specifies any provider-specific options.",
	},
	"tune_provider_timeout_seconds": {
		Type:        framework.TypeDurationSecond,
		Description: "Specifies the maximum time to wait for a provider response in seconds. Infinite if 0.",
		Default:     persistence.DefaultConfigTuningEntry.ProviderTimeoutSeconds,
	},
	"tune_provider_timeout_expiry_leeway_factor": {
		Type:        framework.TypeFloat,
		Description: "Specifies a multiplier for the provider timeout when a credential is about to expire. Must be at least 1.",
		Default:     persistence.DefaultConfigTuningEntry.ProviderTimeoutExpiryLeewayFactor,
	},
	"tune_refresh_check_interval_seconds": {
		Type:        framework.TypeDurationSecond,
		Description: "Specifies the interval in seconds between invocations of the credential refresh background process. Disabled if 0.",
		Default:     persistence.DefaultConfigTuningEntry.RefreshCheckIntervalSeconds,
	},
	"tune_refresh_expiry_delta_factor": {
		Type:        framework.TypeFloat,
		Description: "Specifies a multipler for the refresh check interval to use to detect tokens that will expire soon after a background refresh process is invoked. Must be at least 1.",
		Default:     persistence.DefaultConfigTuningEntry.RefreshExpiryDeltaFactor,
	},
	"tune_reap_check_interval_seconds": {
		Type:        framework.TypeDurationSecond,
		Description: "Specifies the interval in seconds between invocations of the expired credential reaper background process. Disabled if 0.",
		Default:     persistence.DefaultConfigTuningEntry.ReapCheckIntervalSeconds,
	},
	"tune_reap_dry_run": {
		Type:        framework.TypeBool,
		Description: "Specifies whether the expired credential reaper should merely report on what it would delete.",
		Default:     persistence.DefaultConfigTuningEntry.ReapDryRun,
	},
	"tune_reap_non_refreshable_seconds": {
		Type:        framework.TypeDurationSecond,
		Description: "Specifies the minimum additional time to wait before automatically deleting an expired credential that does not have a refresh token. Set to 0 to disable this reaping criterion.",
		Default:     persistence.DefaultConfigTuningEntry.ReapNonRefreshableSeconds,
	},
	"tune_reap_revoked_seconds": {
		Type:        framework.TypeDurationSecond,
		Description: "Specifies the minimum additional time to wait before automatically deleting an expired credential that has a revoked refresh token. Set to 0 to disable this reaping criterion.",
		Default:     persistence.DefaultConfigTuningEntry.ReapRevokedSeconds,
	},
	"tune_reap_transient_error_attempts": {
		Type:        framework.TypeInt,
		Description: "Specifies the minimum number of refresh attempts to make before automatically deleting an expired credential. Set to 0 to disable this reaping criterion.",
		Default:     persistence.DefaultConfigTuningEntry.ReapTransientErrorAttempts,
	},
	"tune_reap_transient_error_seconds": {
		Type:        framework.TypeDurationSecond,
		Description: "Specifies the minimum additional time to wait before automatically deleting an expired credential that cannot be refreshed because of a transient problem like network connectivity issues. Set to 0 to disable this reaping criterion.",
		Default:     persistence.DefaultConfigTuningEntry.ReapTransientErrorSeconds,
	},
}

const configHelpSynopsis = `
Configures OAuth 2.0 client information.
`

const configHelpDescription = `
This endpoint configures the endpoint, client ID, and secret for
authorization code exchange. The endpoint is selected by the given
provider. Additionally, you may specify URL parameters to add to the
authorization code endpoint.
`

func pathConfig(b *backend) *framework.Path {
	return &framework.Path{
		Pattern: ConfigPath + `$`,
		Fields:  configFields,
		Operations: map[logical.Operation]framework.OperationHandler{
			logical.ReadOperation: &framework.PathOperation{
				Callback: b.configReadOperation,
				Summary:  "Return the current configuration for this mount.",
			},
			logical.UpdateOperation: &framework.PathOperation{
				Callback: b.configUpdateOperation,
				Summary:  "Create a new client configuration or replace the configuration with new client information.",
			},
			logical.DeleteOperation: &framework.PathOperation{
				Callback: b.configDeleteOperation,
				Summary:  "Delete the client configuration, invalidating all credentials.",
			},
		},
		HelpSynopsis:    strings.TrimSpace(configHelpSynopsis),
		HelpDescription: strings.TrimSpace(configHelpDescription),
	}
}

var configAuthCodeURLFields = map[string]*framework.FieldSchema{
	"auth_url_params": {
		Type:        framework.TypeKVPairs,
		Description: "Specifies the additional query parameters to add to the authorization code URL.",
	},
	"redirect_url": {
		Type:        framework.TypeString,
		Description: "The URL to redirect to after the authorization flow completes.",
	},
	"scopes": {
		Type:        framework.TypeCommaStringSlice,
		Description: "The scopes to request for authorization.",
	},
	"state": {
		Type:        framework.TypeString,
		Description: "Specifies the state to set in the authorization code URL.",
	},
	"provider_options": {
		Type:        framework.TypeKVPairs,
		Description: "Specifies any provider-specific options.",
	},
}

const configAuthCodeURLHelpSynopsis = `
Generates authorization code URLs for the current configuration.
`

const configAuthCodeURLHelpDescription = `
This endpoint merges the configuration data with requested parameters
like a redirect URL and scopes to create an authorization code URL.
The code returned in the response should be written to a credential
endpoint to start managing authentication tokens.
`

func pathConfigAuthCodeURL(b *backend) *framework.Path {
	return &framework.Path{
		Pattern: ConfigAuthCodeURLPath + `$`,
		Fields:  configAuthCodeURLFields,
		Operations: map[logical.Operation]framework.OperationHandler{
			logical.UpdateOperation: &framework.PathOperation{
				Callback: b.configAuthCodeURLUpdateOperation,
				Summary:  "Generate an initial authorization code URL.",
			},
		},
		HelpSynopsis:    strings.TrimSpace(configAuthCodeURLHelpSynopsis),
		HelpDescription: strings.TrimSpace(configAuthCodeURLHelpDescription),
	}
}
