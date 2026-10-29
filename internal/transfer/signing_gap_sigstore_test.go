package transfer

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/strio-dev/coopr/internal/imagestore"
	"github.com/strio-dev/coopr/internal/localstore"
	"github.com/strio-dev/coopr/internal/oci"
	imagecopy "go.podman.io/image/v5/copy"
	"go.podman.io/image/v5/docker"
	"go.podman.io/image/v5/image"
	sigstorecli "go.podman.io/image/v5/pkg/cli/sigstore"
	"go.podman.io/image/v5/signature"
	"go.podman.io/image/v5/signature/signer"
	"go.podman.io/image/v5/types"
)

// This fixture exercises real HTTP protocol clients and cryptography, not the
// public Sigstore service. It validates OIDC claims/key ownership before issuing
// certificates and validates signatures before returning a signed Rekor SET.
type signingGapServices struct {
	server                       *httptest.Server
	oidcKey, caKey, rekorKey     *ecdsa.PrivateKey
	ca                           *x509.Certificate
	caPEM, rekorPEM              []byte
	challenge                    atomic.Value
	device, token, fulcio, rekor atomic.Int32
}

func signingGapJSON(w io.Writer, value any) error { return json.NewEncoder(w).Encode(value) }

func newSigningGapServices(t *testing.T) *signingGapServices {
	t.Helper()
	f := &signingGapServices{}
	var err error
	for _, target := range []**ecdsa.PrivateKey{&f.oidcKey, &f.caKey, &f.rekorKey} {
		*target, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Private test Fulcio"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &f.caKey.PublicKey, f.caKey)
	if err != nil {
		t.Fatal(err)
	}
	f.ca, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	f.caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	pub, err := x509.MarshalPKIXPublicKey(&f.rekorKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	f.rekorPEM = pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub})
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := f.handle(w, r); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *signingGapServices) jwt() (string, error) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"ES256","kid":"fixture"}`))
	claims, err := json.Marshal(map[string]any{"iss": f.server.URL, "sub": "fixture-user", "aud": "sigstore", "email": "signer@example.invalid", "email_verified": true, "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix()})
	if err != nil {
		return "", err
	}
	body := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	h := sha256.Sum256([]byte(body))
	r, s, err := ecdsa.Sign(rand.Reader, f.oidcKey, h[:])
	if err != nil {
		return "", err
	}
	raw := append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	return body + "." + base64.RawURLEncoding.EncodeToString(raw), nil
}

func (f *signingGapServices) validateToken(token string) error {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return fmt.Errorf("invalid token")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		return fmt.Errorf("invalid token signature")
	}
	h := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(&f.oidcKey.PublicKey, h[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		return fmt.Errorf("untrusted token")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return err
	}
	var claims struct {
		Issuer   string `json:"iss"`
		Audience string `json:"aud"`
		Email    string `json:"email"`
		Verified bool   `json:"email_verified"`
		Expires  int64  `json:"exp"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return err
	}
	if claims.Issuer != f.server.URL || claims.Audience != "sigstore" || claims.Email != "signer@example.invalid" || !claims.Verified || claims.Expires <= time.Now().Unix() {
		return fmt.Errorf("invalid token claims")
	}
	return nil
}

func (f *signingGapServices) handle(w http.ResponseWriter, r *http.Request) error {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/.well-known/openid-configuration":
		return signingGapJSON(w, map[string]any{"issuer": f.server.URL, "authorization_endpoint": f.server.URL + "/authorize", "token_endpoint": f.server.URL + "/token", "jwks_uri": f.server.URL + "/keys", "device_authorization_endpoint": f.server.URL + "/device", "id_token_signing_alg_values_supported": []string{"ES256"}, "code_challenge_methods_supported": []string{"S256"}})
	case "/keys":
		return signingGapJSON(w, map[string]any{"keys": []any{map[string]string{"kty": "EC", "crv": "P-256", "kid": "fixture", "alg": "ES256", "use": "sig", "x": base64.RawURLEncoding.EncodeToString(f.oidcKey.X.FillBytes(make([]byte, 32))), "y": base64.RawURLEncoding.EncodeToString(f.oidcKey.Y.FillBytes(make([]byte, 32)))}}})
	case "/device":
		if err := r.ParseForm(); err != nil {
			return err
		}
		if r.Form.Get("client_id") != "sigstore" || r.Form.Get("code_challenge_method") != "S256" || r.Form.Get("scope") != "openid email" {
			return fmt.Errorf("invalid device authorization request")
		}
		f.challenge.Store(r.Form.Get("code_challenge"))
		f.device.Add(1)
		return signingGapJSON(w, map[string]any{"device_code": "fixture-device", "user_code": "TEST", "verification_uri": f.server.URL + "/verify", "expires_in": 60, "interval": 1})
	case "/token":
		if err := r.ParseForm(); err != nil {
			return err
		}
		h := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" || r.Form.Get("device_code") != "fixture-device" || r.Form.Get("client_id") != "sigstore" || base64.RawURLEncoding.EncodeToString(h[:]) != f.challenge.Load() {
			return fmt.Errorf("invalid device code or PKCE verifier")
		}
		token, err := f.jwt()
		if err != nil {
			return err
		}
		f.token.Add(1)
		return signingGapJSON(w, map[string]string{"id_token": token, "token_type": "Bearer", "access_token": "unused"})
	case "/api/v1/signingCert":
		if err := f.validateToken(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")); err != nil {
			return err
		}
		var req struct {
			PublicKey struct {
				Content   []byte `json:"content"`
				Algorithm string `json:"algorithm"`
			} `json:"publicKey"`
			SignedEmailAddress []byte `json:"signedEmailAddress"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return err
		}
		if req.PublicKey.Algorithm != "ecdsa" {
			return fmt.Errorf("unexpected certificate key algorithm")
		}
		pub, err := x509.ParsePKIXPublicKey(req.PublicKey.Content)
		if err != nil {
			return err
		}
		ec, ok := pub.(*ecdsa.PublicKey)
		if !ok {
			return fmt.Errorf("not an EC key")
		}
		h := sha256.Sum256([]byte("signer@example.invalid"))
		if !ecdsa.VerifyASN1(ec, h[:], req.SignedEmailAddress) {
			return fmt.Errorf("invalid key ownership proof")
		}
		issuer, err := asn1.Marshal(f.server.URL)
		if err != nil {
			return err
		}
		leaf := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(10 * time.Minute), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning}, EmailAddresses: []string{"signer@example.invalid"}, ExtraExtensions: []pkix.Extension{{Id: asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 8}, Value: issuer}}}
		der, err := x509.CreateCertificate(rand.Reader, leaf, f.ca, ec, f.caKey)
		if err != nil {
			return err
		}
		f.fulcio.Add(1)
		w.WriteHeader(http.StatusCreated)
		_, err = w.Write(append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), f.caPEM...))
		return err
	case "/api/v1/log/entries":
		var entry struct {
			Kind       string `json:"kind"`
			APIVersion string `json:"apiVersion"`
			Spec       struct {
				Data struct {
					Hash struct{ Algorithm, Value string } `json:"hash"`
				} `json:"data"`
				Signature struct {
					Content   []byte `json:"content"`
					PublicKey struct {
						Content []byte `json:"content"`
					} `json:"publicKey"`
				} `json:"signature"`
			} `json:"spec"`
		}
		if err := json.NewDecoder(r.Body).Decode(&entry); err != nil {
			return err
		}
		if entry.Kind != "hashedrekord" || entry.APIVersion != "0.0.1" || entry.Spec.Data.Hash.Algorithm != "sha256" {
			return fmt.Errorf("invalid Rekor entry")
		}
		block, _ := pem.Decode(entry.Spec.Signature.PublicKey.Content)
		if block == nil {
			return fmt.Errorf("missing certificate")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return err
		}
		roots := x509.NewCertPool()
		roots.AddCert(f.ca)
		if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning}}); err != nil {
			return err
		}
		ec, ok := cert.PublicKey.(*ecdsa.PublicKey)
		if !ok {
			return fmt.Errorf("non-EC certificate")
		}
		hash, err := hex.DecodeString(entry.Spec.Data.Hash.Value)
		if err != nil {
			return err
		}
		if !ecdsa.VerifyASN1(ec, hash, entry.Spec.Signature.Content) {
			return fmt.Errorf("invalid logged image signature")
		}
		body, err := json.Marshal(entry)
		if err != nil {
			return err
		}
		publicDER, err := x509.MarshalPKIXPublicKey(&f.rekorKey.PublicKey)
		if err != nil {
			return err
		}
		logID := sha256.Sum256(publicDER)
		index := f.rekor.Add(1) - 1
		integrated := time.Now().Unix()
		payload := map[string]any{"body": base64.StdEncoding.EncodeToString(body), "integratedTime": integrated, "logIndex": index, "logID": hex.EncodeToString(logID[:])}
		canonical, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		h := sha256.Sum256(canonical)
		set, err := ecdsa.SignASN1(rand.Reader, f.rekorKey, h[:])
		if err != nil {
			return err
		}
		payload["verification"] = map[string]any{"signedEntryTimestamp": set}
		w.WriteHeader(http.StatusCreated)
		return signingGapJSON(w, map[string]any{"private-entry": payload})
	default:
		return fmt.Errorf("unexpected service path: %s", r.URL.Path)
	}
}

func (f *signingGapServices) parameters(t *testing.T, mode string) string {
	t.Helper()
	fulcio := map[string]string{"fulcioURL": f.server.URL, "oidcMode": mode}
	if mode == "staticToken" {
		token, err := f.jwt()
		if err != nil {
			t.Fatal(err)
		}
		fulcio["oidcIDToken"] = token
	} else {
		fulcio["oidcIssuerURL"] = f.server.URL
		fulcio["oidcClientID"] = "sigstore"
	}
	path := filepath.Join(t.TempDir(), "params.json")
	raw, err := json.Marshal(map[string]any{"fulcio": fulcio, "rekorURL": f.server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func (f *signingGapServices) verify(t *testing.T, ctx context.Context, name string, want bool, wrong string) {
	t.Helper()
	issuer := f.server.URL
	if wrong == "issuer" {
		issuer += "/wrong"
	}
	email := "signer@example.invalid"
	if wrong == "email" {
		email = "wrong@example.invalid"
	}
	rekorPEM := f.rekorPEM
	if wrong == "rekor" {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
		if err != nil {
			t.Fatal(err)
		}
		rekorPEM = pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: raw})
	}
	fulcio, err := signature.NewPRSigstoreSignedFulcio(signature.PRSigstoreSignedFulcioWithCAData(f.caPEM), signature.PRSigstoreSignedFulcioWithOIDCIssuer(issuer), signature.PRSigstoreSignedFulcioWithSubjectEmail(email))
	if err != nil {
		t.Fatal(err)
	}
	identity := signature.NewPRMMatchRepoDigestOrExact()
	if wrong == "identity" {
		identity, err = signature.NewPRMExactReference(name + "-wrong")
		if err != nil {
			t.Fatal(err)
		}
	}
	requirement, err := signature.NewPRSigstoreSigned(signature.PRSigstoreSignedWithFulcio(fulcio), signature.PRSigstoreSignedWithRekorPublicKeyData(rekorPEM), signature.PRSigstoreSignedWithSignedIdentity(identity))
	if err != nil {
		t.Fatal(err)
	}
	policy, err := signature.NewPolicyContext(&signature.Policy{Default: signature.PolicyRequirements{requirement}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := policy.Destroy(); err != nil {
			t.Error(err)
		}
	}()
	ref, err := docker.ParseReference("//" + name)
	if err != nil {
		t.Fatal(err)
	}
	system := &types.SystemContext{DockerInsecureSkipTLSVerify: types.OptionalBoolTrue}
	source, err := ref.NewImageSource(ctx, system)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := source.Close(); err != nil {
			t.Error(err)
		}
	}()
	allowed, err := policy.IsRunningImageAllowed(ctx, image.UnparsedInstance(source, nil))
	if allowed != want || (want && err != nil) {
		t.Fatalf("Fulcio/Rekor policy allowed=%t want=%t err=%v", allowed, want, err)
	}
}

func TestSigningGapFulcioOIDCRekor(t *testing.T) {
	if testing.Short() {
		t.Skip("local OIDC/Fulcio/Rekor service integration")
	}
	for _, mode := range []string{"staticToken", "deviceGrant"} {
		for _, backend := range []string{"coopr", "native", "worker"} {
			t.Run(mode+"/"+backend, func(t *testing.T) {
				configureTestSignatureLookaside(t, t.TempDir())
				if err := os.WriteFile(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "containers", "registries.d", "coopr-test.yaml"), []byte("default-docker:\n  use-sigstore-attachments: true\n"), 0600); err != nil {
					t.Fatal(err)
				}
				f := newSigningGapServices(t)
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				source, root, members := multiPlatformImageFixture(t, ctx)
				store := t.TempDir()
				if err := localstore.Put(ctx, store, source, root, "sigstore-gap:latest"); err != nil {
					t.Fatal(err)
				}
				workerStore := nativeTestStore(t.TempDir())
				if backend == "worker" {
					selected := nativeEmptyImageFixture(t, workerStore, v1.Platform{OS: "linux", Architecture: runtime.GOARCH}, "signing-gap")
					if err := nameNativeTestImage(ctx, workerStore, "signing-gap:latest", selected); err != nil {
						t.Fatal(err)
					}
					root = selected.Manifest
					members = nil
				}
				parameters := f.parameters(t, mode)
				registry := httptest.NewServer(registry.New())
				defer registry.Close()
				name := strings.TrimPrefix(registry.URL, "http://") + "/coopr/fulcio:test"
				switch backend {
				case "worker":
					_, err := Copy(ctx, oci.Image, "signing-gap:latest", Destination{Transport: "registry", Name: name}, Options{BuildStore: workerStore, TLSVerify: new(false), Signing: SigningOptions{SigstoreParameterFile: parameters}})
					if err != nil {
						t.Fatal(err)
					}
				case "coopr":
					_, err := CopyRoot(ctx, oci.Image, store, root, Destination{Transport: "registry", Name: name}, Options{TLSVerify: new(false), Signing: SigningOptions{SigstoreParameterFile: parameters}})
					if err != nil {
						t.Fatal(err)
					}
				default:
					signing, err := sigstorecli.NewSignerFromParameterFile(parameters, &sigstorecli.Options{Stdin: strings.NewReader(""), Stdout: io.Discard})
					if err != nil {
						t.Fatal(err)
					}
					defer func() {
						if err := signing.Close(); err != nil {
							t.Error(err)
						}
					}()
					src, err := imagestore.LayoutReference(store, root)
					if err != nil {
						t.Fatal(err)
					}
					dst, err := docker.ParseReference("//" + name)
					if err != nil {
						t.Fatal(err)
					}
					policy, err := signature.NewPolicyContext(&signature.Policy{Default: signature.PolicyRequirements{signature.NewPRInsecureAcceptAnything()}})
					if err != nil {
						t.Fatal(err)
					}
					defer func() {
						if err := policy.Destroy(); err != nil {
							t.Error(err)
						}
					}()
					dir := t.TempDir()
					if err := os.WriteFile(filepath.Join(dir, "sigstore.yaml"), []byte("default-docker:\n  use-sigstore-attachments: true\n"), 0600); err != nil {
						t.Fatal(err)
					}
					system := &types.SystemContext{DockerInsecureSkipTLSVerify: types.OptionalBoolTrue, RegistriesDirPath: dir}
					_, err = imagecopy.Image(ctx, policy, dst, src, &imagecopy.Options{SourceCtx: system, DestinationCtx: system, Signers: []*signer.Signer{signing}, ImageListSelection: imagecopy.CopyAllImages, PreserveDigests: true})
					if err != nil {
						t.Fatal(err)
					}
				}
				assertRemoteManifestDigests(t, ctx, name, root, members)
				f.verify(t, ctx, name, true, "")
				for _, member := range members {
					f.verify(t, ctx, strings.TrimSuffix(name, ":test")+"@"+member.Digest.String(), true, "")
				}
				for _, wrong := range []string{"issuer", "email", "rekor", "identity"} {
					f.verify(t, ctx, name, false, wrong)
				}
				if f.fulcio.Load() != 1 || f.rekor.Load() != int32(1+len(members)) {
					t.Fatalf("services calls Fulcio=%d Rekor=%d", f.fulcio.Load(), f.rekor.Load())
				}
				if mode == "deviceGrant" && (f.device.Load() != 1 || f.token.Load() != 1) {
					t.Fatalf("device exchange not exercised: authorization=%d token=%d", f.device.Load(), f.token.Load())
				}
				t.Logf("verified %s %s: signed OIDC token, certificate ownership, root/member signatures, cryptographic SET and rejecting wrong issuer/email/Rekor key/identity", mode, backend)
			})
		}
	}
}

func TestSigningGapRejectsUntrustedOIDCToken(t *testing.T) {
	if testing.Short() {
		t.Skip("local Fulcio authenticated request")
	}
	for _, backend := range []string{"coopr", "native"} {
		t.Run(backend, func(t *testing.T) {
			configureTestSignatureLookaside(t, t.TempDir())
			f := newSigningGapServices(t)
			parameters := f.parameters(t, "staticToken")
			raw, err := os.ReadFile(parameters)
			if err != nil {
				t.Fatal(err)
			}
			var params map[string]any
			if err := json.Unmarshal(raw, &params); err != nil {
				t.Fatal(err)
			}
			fulcio := params["fulcio"].(map[string]any)
			parts := strings.Split(fulcio["oidcIDToken"].(string), ".")
			sig, err := base64.RawURLEncoding.DecodeString(parts[2])
			if err != nil {
				t.Fatal(err)
			}
			sig[0] ^= 1
			parts[2] = base64.RawURLEncoding.EncodeToString(sig)
			fulcio["oidcIDToken"] = strings.Join(parts, ".")
			raw, err = json.Marshal(params)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(parameters, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if backend == "native" {
				signing, signErr := sigstorecli.NewSignerFromParameterFile(parameters, &sigstorecli.Options{Stdin: strings.NewReader(""), Stdout: io.Discard})
				if signing != nil {
					if err := signing.Close(); err != nil {
						t.Error(err)
					}
				}
				err = signErr
			} else {
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				source, root := imageFixture(t, ctx)
				store := t.TempDir()
				if err := localstore.Put(ctx, store, source, root, "untrusted:latest"); err != nil {
					t.Fatal(err)
				}
				registry := httptest.NewServer(registry.New())
				defer registry.Close()
				_, err = CopyRoot(ctx, oci.Image, store, root, Destination{Transport: "registry", Name: strings.TrimPrefix(registry.URL, "http://") + "/coopr/untrusted:test"}, Options{TLSVerify: new(false), Signing: SigningOptions{SigstoreParameterFile: parameters}})
			}
			if err == nil || !strings.Contains(err.Error(), "obtaining certificate from Fulcio") || !strings.Contains(err.Error(), "untrusted token") {
				t.Fatalf("untrusted OIDC credential not rejected by Fulcio: %v", err)
			}
			if f.fulcio.Load() != 0 || f.rekor.Load() != 0 {
				t.Fatal("rejected credential produced a certificate or log entry")
			}
		})
	}
}
