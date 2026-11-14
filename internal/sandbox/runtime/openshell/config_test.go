package openshell_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/ai-openshell/sdk-go/openshell/v1"

	"github.com/agent-fox-dev/parley/internal/sandbox"
	"github.com/agent-fox-dev/parley/internal/sandbox/runtime/openshell"
)

// selfSigned returns a self-signed certificate and its key, PEM encoded; the
// certificate is its own CA.
func selfSigned(t *testing.T) (certPEM, keyPEM string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "parley-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		DNSNames:              []string{"gw.test"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kder}))
}

// issuer is an OIDC issuer for the client credentials grant: it answers
// discovery and issues the token "oidc-access" to the client parley with the
// secret oidc-SECRET, and counts the grants it issued.
type issuer struct {
	*httptest.Server
	mu     sync.Mutex
	grants int
}

func newIssuer(t *testing.T) *issuer {
	t.Helper()
	is := &issuer{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                 is.URL,
			"authorization_endpoint": is.URL + "/authorize",
			"token_endpoint":         is.URL + "/token",
		})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		id, secret, ok := r.BasicAuth()
		if !ok {
			id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
		}
		if r.PostForm.Get("grant_type") != "client_credentials" || id != "parley" || secret != "oidc-SECRET" {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_client"})
			return
		}
		is.mu.Lock()
		is.grants++
		is.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "oidc-access", "token_type": "Bearer", "expires_in": 3600})
	})
	is.Server = httptest.NewServer(mux)
	t.Cleanup(is.Close)
	return is
}

func (is *issuer) Grants() int {
	is.mu.Lock()
	defer is.mu.Unlock()
	return is.grants
}

func TestBuildConfig_ConnectsWithTheOneConfiguredWayToAuthenticate(t *testing.T) {
	t.Parallel()
	cert, key := selfSigned(t)
	is := newIssuer(t)
	files := map[string]string{"cert.pem": cert, "key.pem": key, "ca.pem": cert, "token": " tok\n", "secret": "oidc-SECRET\n"}
	rows := []struct {
		name    string
		keys    string
		address string
		// tls lists the file names TLS refers to: cert, key and CA ("" for
		// none); nil wants no TLS settings at all.
		tls []string
		// bearer is the token the authentication sends; "" wants none.
		bearer string
		// plaintext wants an authentication that may go over plain HTTP.
		plaintext bool
	}{
		{
			name:    "mutual TLS",
			keys:    "gateway_url = \"https://gw.test:8443\"\ntls_cert = \"cert.pem\"\ntls_key = \"key.pem\"",
			address: "gw.test:8443",
			tls:     []string{"cert.pem", "key.pem", ""},
		},
		{
			name:    "token_file",
			keys:    "gateway_url = \"https://gw.test:8443\"\ntoken_file = \"token\"",
			address: "gw.test:8443",
			bearer:  "tok",
		},
		{
			name:    "OIDC",
			keys:    "gateway_url = \"https://gw.test:8443\"\noidc_issuer = \"" + is.URL + "\"\noidc_client_id = \"parley\"\noidc_client_secret_file = \"secret\"",
			address: "gw.test:8443",
			bearer:  "oidc-access",
		},
		{
			name:    "token_file and tls_ca",
			keys:    "gateway_url = \"https://gw.test:8443\"\ntoken_file = \"token\"\ntls_ca = \"ca.pem\"",
			address: "gw.test:8443",
			tls:     []string{"", "", "ca.pem"},
			bearer:  "tok",
		},
		{
			name:    "the default https port",
			keys:    "gateway_url = \"https://gw.test/\"\ntoken_file = \"token\"",
			address: "gw.test:443",
			bearer:  "tok",
		},
		{
			name:      "token_file over http to a loopback gateway",
			keys:      "gateway_url = \"http://127.0.0.1:8080\"\ntoken_file = \"token\"",
			address:   "http://127.0.0.1:8080",
			bearer:    "tok",
			plaintext: true,
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			settings := loadSettings(t, row.keys, files)
			cfg, err := openshell.BuildConfig(settings)
			if err != nil {
				t.Fatalf("BuildConfig: %v", err)
			}
			if cfg.Address != row.address {
				t.Errorf("Address = %q, want %q", cfg.Address, row.address)
			}
			wantTLS(t, cfg.TLS, row.tls)
			if row.bearer == "" {
				if cfg.Auth != nil {
					md, _ := cfg.Auth.GetRequestMetadata(t.Context())
					t.Errorf("Auth = %T sending %v, want no token source", cfg.Auth, md)
				}
			} else {
				if cfg.Auth == nil {
					t.Fatal("Auth = nil, want a token source")
				}
				md, err := cfg.Auth.GetRequestMetadata(t.Context())
				if err != nil {
					t.Fatalf("GetRequestMetadata: %v", err)
				}
				if got := md["authorization"]; got != "Bearer "+row.bearer {
					t.Errorf("authorization = %q, want %q", got, "Bearer "+row.bearer)
				}
				if got := cfg.Auth.RequireTransportSecurity(); got == row.plaintext {
					t.Errorf("RequireTransportSecurity() = %v, want %v", got, !row.plaintext)
				}
			}
			// The SDK accepts the configuration: it loads the certificate,
			// the key and the CA, and takes the token source.
			c, err := v1.NewClient(cfg)
			if err != nil {
				t.Fatalf("v1.NewClient: %v", err)
			}
			_ = c.Close()
		})
	}
}

func TestBuildConfig_RenewsTheOIDCTokenFromTheIssuer(t *testing.T) {
	t.Parallel()
	is := newIssuer(t)
	settings := loadSettings(t,
		"gateway_url = \"https://gw.test:8443\"\noidc_issuer = \""+is.URL+"\"\noidc_client_id = \"parley\"\noidc_client_secret_file = \"secret\"",
		map[string]string{"secret": "oidc-SECRET"})
	cfg, err := openshell.BuildConfig(settings)
	if err != nil {
		t.Fatalf("BuildConfig: %v", err)
	}
	if is.Grants() != 0 {
		t.Fatalf("the issuer granted %d tokens before the first call, want none", is.Grants())
	}
	for range 3 {
		if _, err := cfg.Auth.GetRequestMetadata(t.Context()); err != nil {
			t.Fatalf("GetRequestMetadata: %v", err)
		}
	}
	if is.Grants() != 1 {
		t.Fatalf("the issuer granted %d tokens for three calls, want 1 kept until it nears its expiry", is.Grants())
	}
}

func TestBuildConfig_RefusesSettingsWithoutSandboxes(t *testing.T) {
	t.Parallel()
	for name, settings := range map[string]*sandbox.Settings{"nil": nil, "disabled": {}} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, err := openshell.BuildConfig(settings); err == nil || !strings.Contains(err.Error(), "not enabled") {
				t.Fatalf("BuildConfig = %v, want an error saying sandboxes are not enabled", err)
			}
		})
	}
}

// wantTLS checks the TLS settings against the file names cert, key and CA
// ("" for none); want nil wants none at all.
func wantTLS(t *testing.T, got *v1.TLSConfig, want []string) {
	t.Helper()
	if want == nil {
		if got != nil {
			t.Errorf("TLS = %+v, want none", got)
		}
		return
	}
	if got == nil {
		t.Fatalf("TLS = nil, want cert %q, key %q, CA %q", want[0], want[1], want[2])
	}
	for i, path := range []string{got.CertFile, got.KeyFile, got.CAFile} {
		if want[i] == "" {
			if path != "" {
				t.Errorf("TLS file %d = %q, want none", i, path)
			}
			continue
		}
		if filepath.Base(path) != want[i] || !filepath.IsAbs(path) {
			t.Errorf("TLS file %d = %q, want the absolute path of %s", i, path, want[i])
		}
	}
	if got.Insecure {
		t.Error("TLS skips verifying the gateway's certificate")
	}
}
