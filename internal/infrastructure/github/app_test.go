package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// fakeGitHub serves the installation-token endpoint, checking the app JWT
// the way GitHub does, and counts how many tokens it minted.
func fakeGitHub(t *testing.T, key *rsa.PrivateKey, appID string, expiresIn time.Duration) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var minted atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/app/installations/42/access_tokens" {
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		jwt := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if err := verifyAppJWT(jwt, &key.PublicKey, appID); err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		n := minted.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "ghs_installation_" + strconv.Itoa(int(n)),
			"expires_at": time.Now().Add(expiresIn).UTC().Format(time.RFC3339),
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &minted
}

func verifyAppJWT(jwt string, pub *rsa.PublicKey, appID string) error {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return errString("jwt has " + strconv.Itoa(len(parts)) + " parts")
	}
	var header struct{ Alg string }
	var claims struct {
		Iss      string
		Iat, Exp int64
	}
	for i, dst := range []any{&header, &claims} {
		raw, err := base64.RawURLEncoding.DecodeString(parts[i])
		if err != nil {
			return err
		}
		if err := json.Unmarshal(raw, dst); err != nil {
			return err
		}
	}
	if header.Alg != "RS256" {
		return errString("alg " + header.Alg)
	}
	if claims.Iss != appID {
		return errString("iss " + claims.Iss)
	}
	// GitHub rejects an app JWT that lives longer than ten minutes.
	if claims.Exp-claims.Iat > 600 || claims.Exp <= time.Now().Unix() {
		return errString("bad lifetime")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	return rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig)
}

type errString string

func (e errString) Error() string { return string(e) }

func TestAppTokens_MintsAnInstallationTokenWithAValidAppJWT(t *testing.T) {
	key := testKey(t)
	srv, minted := fakeGitHub(t, key, "1234", time.Hour)
	src := &AppTokens{AppID: "1234", InstallationID: "42", Key: key, BaseURL: srv.URL}

	tok, err := src.Token(t.Context())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "ghs_installation_1" || minted.Load() != 1 {
		t.Errorf("token %q after %d mints", tok, minted.Load())
	}
}

// An installation token lasts an hour. Minting one per request would spend
// the app's rate limit on credentials; minting once per process would leave
// a daemon or an --every loop holding a dead token after an hour.
func TestAppTokens_ReusesATokenUntilItIsNearExpiry(t *testing.T) {
	key := testKey(t)
	srv, minted := fakeGitHub(t, key, "1234", time.Hour)
	now := time.Now()
	src := &AppTokens{AppID: "1234", InstallationID: "42", Key: key, BaseURL: srv.URL, now: func() time.Time { return now }}

	first, _ := src.Token(t.Context())
	again, _ := src.Token(t.Context())
	if again != first || minted.Load() != 1 {
		t.Fatalf("a fresh token was not reused: %q then %q, %d mints", first, again, minted.Load())
	}

	now = now.Add(56 * time.Minute)
	renewed, err := src.Token(t.Context())
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if renewed == first || minted.Load() != 2 {
		t.Errorf("a token five minutes from expiry was reused: %q, %d mints", renewed, minted.Load())
	}
}

func TestAppTokens_ReportsARejectedApp(t *testing.T) {
	key := testKey(t)
	srv, _ := fakeGitHub(t, key, "1234", time.Hour)
	src := &AppTokens{AppID: "9999", InstallationID: "42", Key: key, BaseURL: srv.URL}

	if _, err := src.Token(t.Context()); err == nil {
		t.Fatal("a JWT for the wrong app produced a token")
	}
}

func TestParseAppKey_AcceptsBothPEMEncodingsGitHubHasIssued(t *testing.T) {
	key := testKey(t)
	pkcs1 := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8 := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	for name, data := range map[string][]byte{"pkcs1": pkcs1, "pkcs8": pkcs8} {
		got, err := ParseAppKey(data)
		if err != nil || !got.Equal(key) {
			t.Errorf("%s: key %v, err %v", name, got != nil, err)
		}
	}
	if _, err := ParseAppKey([]byte("not a key")); err == nil {
		t.Error("garbage parsed as a key")
	}
}

// The client asks the source on every request, so a long-lived process
// picks up the renewed token rather than the one it booted with.
func TestClient_AuthenticatesWithTheTokenSource(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	c := NewClient("", Repo{Owner: "o", Name: "r"}, nil)
	c.BaseURL = srv.URL
	c.Tokens = TokenFunc(func(context.Context) (string, error) { return "ghs_live", nil })

	if !c.Enabled() {
		t.Fatal("a client with a token source reported itself disabled")
	}
	if _, err := c.once(t.Context(), http.MethodGet, "/repos/o/r", nil); err != nil {
		t.Fatalf("request: %v", err)
	}
	if got != "Bearer ghs_live" {
		t.Errorf("Authorization = %q", got)
	}
}
