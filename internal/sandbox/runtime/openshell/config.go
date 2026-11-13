package openshell

import (
	"errors"
	"fmt"
	"net"
	"net/url"

	v1 "github.com/ai-openshell/sdk-go/openshell/v1"
	"github.com/ai-openshell/sdk-go/openshell/v1/oidc"

	"github.com/agent-fox-dev/parley/internal/sandbox"
)

// BuildConfig returns the SDK configuration of a checked [sandbox] section:
// the gateway's address and the one way to authenticate it configures.
//
// The address is gateway_url's host and port (443 when the URL gives none).
// An http gateway_url, which the section allows only for a loopback host,
// keeps its scheme, which is how the SDK is told to connect without TLS.
//
// Mutual TLS gives the client certificate and its key and no token source;
// token_file gives a static bearer token, the file's content without
// surrounding white space; the OIDC keys give a token source that obtains a
// token with the client credentials grant and renews it before it expires.
// tls_ca, when set, is the only CA the gateway's certificate is checked
// against. The SDK reads the certificate, key and CA files when the client
// is built.
func BuildConfig(settings *sandbox.Settings) (v1.Config, error) {
	if settings == nil || !settings.Enabled {
		return v1.Config{}, errors.New("sandbox runtime: sandboxes are not enabled")
	}
	u, err := url.Parse(settings.GatewayURL)
	if err != nil || u.Hostname() == "" {
		return v1.Config{}, errors.New("sandbox runtime: sandbox.gateway_url is not a URL")
	}
	plaintext := u.Scheme == "http"
	port := u.Port()
	if port == "" {
		port = "443"
		if plaintext {
			port = "80"
		}
	}
	cfg := v1.Config{Address: net.JoinHostPort(u.Hostname(), port)}
	if plaintext {
		cfg.Address = "http://" + cfg.Address
	}

	if settings.TLSCert != "" || settings.TLSKey != "" || settings.TLSCA != "" {
		cfg.TLS = &v1.TLSConfig{CertFile: settings.TLSCert, KeyFile: settings.TLSKey, CAFile: settings.TLSCA}
	}
	switch {
	case settings.TokenFile != "":
		cfg.Auth = v1.StaticToken(settings.Token())
	case settings.OIDCIssuer != "":
		auth, err := oidc.NewClientCredentialsAuth(
			oidc.WithIssuer(settings.OIDCIssuer),
			oidc.WithClientID(settings.OIDCClientID),
			oidc.WithClientSecret(settings.OIDCClientSecret()),
		)
		if err != nil {
			return v1.Config{}, fmt.Errorf("sandbox runtime: sandbox.oidc_issuer: %w", err)
		}
		cfg.Auth = auth
	}
	if plaintext && cfg.Auth != nil {
		// The SDK refuses to send a token without TLS. The section allows an
		// http gateway_url only for a loopback host, where the token does
		// not leave the machine.
		cfg.Auth = loopbackAuth{cfg.Auth}
	}
	return cfg, nil
}

// loopbackAuth is a token source that may be used without TLS, for a
// gateway on a loopback address.
type loopbackAuth struct{ v1.AuthProvider }

func (loopbackAuth) RequireTransportSecurity() bool { return false }
